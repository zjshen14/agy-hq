package kernel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/events"
)

func writeProjectRegistryFixture(t *testing.T, configDir, id, workspacePath string) {
	t.Helper()
	projDir := filepath.Join(configDir, "projects")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir projects dir: %v", err)
	}
	payload := map[string]interface{}{
		"id":   id,
		"name": id,
		"projectResources": map[string]interface{}{
			"resources": []map[string]interface{}{
				{"folderUri": "file://" + filepath.ToSlash(workspacePath)},
			},
		},
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal project fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projDir, id+".json"), raw, 0o644); err != nil {
		t.Fatalf("write project fixture: %v", err)
	}
}

func TestProjectIDPoisoningRepairAndWorkspaceSafeSpawn(t *testing.T) {
	wsVault := t.TempDir()
	wsOtherA := t.TempDir()
	wsUnregistered := t.TempDir()
	vaultDir := filepath.Join(wsVault, "local")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatalf("mkdir vault: %v", err)
	}

	cfgDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", cfgDir)
	t.Setenv("ANTIGRAVITY_SIDECAR_WEB_PORT", "")
	writeProjectRegistryFixture(t, cfgDir, "proj-vault-hq", wsVault)
	writeProjectRegistryFixture(t, cfgDir, "proj-foreign-a", wsOtherA)

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(wsVault); err != nil {
		t.Fatalf("chdir wsVault: %v", err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	// 1. Invoking SetFrontDesk / BackfillFrontDeskProjectID while cwd == wsVault
	//    with a foreign window's ANTIGRAVITY_PROJECT_ID must reject proj-foreign-a and resolve proj-vault-hq.
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "proj-foreign-a")
	if err := SetFrontDesk(vaultDir, "conv-fd-main", ""); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}
	fd, ok := GetFrontDesk(vaultDir)
	if !ok || fd.ProjectID != "proj-vault-hq" {
		t.Fatalf("expected SetFrontDesk to reject proj-foreign-a and resolve proj-vault-hq, got %+v", fd)
	}

	// 2. Poison front_desk.json on disk manually and verify BackfillFrontDeskProjectID repairs it.
	fd.ProjectID = "proj-foreign-a"
	if err := saveFrontDeskLease(vaultDir, fd); err != nil {
		t.Fatalf("saveFrontDeskLease: %v", err)
	}
	BackfillFrontDeskProjectID(vaultDir)
	fdAfter, _ := GetFrontDesk(vaultDir)
	if fdAfter.ProjectID != "proj-vault-hq" {
		t.Fatalf("expected BackfillFrontDeskProjectID to repair poisoned ProjectID to proj-vault-hq, got %q", fdAfter.ProjectID)
	}

	// 3. Poison front_desk.json again and verify resolveSpawnTarget + configureSpawnCommand self-heal both auto and manual spawns.
	fdAfter.ProjectID = "proj-foreign-a"
	_ = saveFrontDeskLease(vaultDir, fdAfter)
	workDir, projID := resolveSpawnTarget(vaultDir, "gatekeeper:sleep-consolidate")
	if filepath.Clean(workDir) != filepath.Clean(wsVault) || projID != "proj-vault-hq" {
		t.Fatalf("expected resolveSpawnTarget to return (%s, proj-vault-hq), got (%s, %s)", wsVault, workDir, projID)
	}
	if healedFD, _ := GetFrontDesk(vaultDir); healedFD.ProjectID != "proj-vault-hq" {
		t.Fatalf("expected resolveSpawnTarget to persist repaired ProjectID, got %q", healedFD.ProjectID)
	}

	manualCmd := exec.Command("true")
	configureSpawnCommand(manualCmd, vaultDir, "manual-spawn:test-task")
	gotProjEnv := ""
	for _, kv := range manualCmd.Env {
		if strings.HasPrefix(kv, "ANTIGRAVITY_PROJECT_ID=") {
			gotProjEnv = strings.TrimPrefix(kv, "ANTIGRAVITY_PROJECT_ID=")
		}
	}
	if gotProjEnv != "proj-vault-hq" {
		t.Fatalf("expected manual-spawn configureSpawnCommand to replace foreign ANTIGRAVITY_PROJECT_ID with proj-vault-hq, got %q", gotProjEnv)
	}

	// 4. Verify spawnTaskViaLS refuses silent agentapi fallback when StartCascade fails without a verified projectId.
	unregVault := filepath.Join(wsUnregistered, "local")
	_ = os.MkdirAll(unregVault, 0o755)
	_ = SetFrontDesk(unregVault, "conv-fd-unreg", "")

	binDir := t.TempDir()
	fakeAgentAPI := filepath.Join(binDir, "agentapi")
	if err := os.WriteFile(fakeAgentAPI, []byte("#!/bin/sh\necho 11111111-2222-3333-4444-555555555555\n"), 0o755); err != nil {
		t.Fatalf("write fake agentapi: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	lsFailSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/StartCascade") {
			http.Error(w, "internal cascade failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer lsFailSrv.Close()

	t.Setenv("ANTIGRAVITY_LS_ADDRESS", strings.TrimPrefix(lsFailSrv.URL, "http://"))
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-test")

	// Unregistered workspace (projectID == "") MUST NOT silently fall back to agentapi; it must surface StartCascade's error.
	if _, spawnErr := SpawnTask(unregVault, "gatekeeper:test", "morning-brief", "[Auto] test", "prompt", "gemini-4-hydrogen-medium", false); spawnErr == nil || !strings.Contains(spawnErr.Error(), "StartCascade HTTP 500") {
		t.Fatalf("expected unverified-workspace SpawnTask to surface StartCascade HTTP 500 instead of silent agentapi fallback, got %v", spawnErr)
	}

	// Registered workspace (projectID == "proj-vault-hq") MAY fall back safely via verified ANTIGRAVITY_PROJECT_ID and MUST record the fallback reason.
	out, safeErr := SpawnTask(vaultDir, "gatekeeper:test", "morning-brief", "[Auto] test", "prompt", "gemini-4-hydrogen-medium", false)
	if safeErr != nil || out != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("expected verified-project fallback to succeed, got out=%q err=%v", out, safeErr)
	}
	qState := LoadQuota(vaultDir)
	if len(qState.Events) == 0 || !strings.Contains(qState.Events[len(qState.Events)-1].Outcome, "agentapi fallback: StartCascade failed") {
		t.Fatalf("expected SpawnEvent.Outcome to record explicit agentapi fallback reason, got %+v", qState.Events)
	}
}

func TestAutoTaskReconciliationWhenStopHookMissed(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)

	// Enqueue 4 separate event scopes:
	// 1. Completed event batch -> unpinned auto task must transition to archived + sidebar sync queued
	// 2. Completed event batch -> pinned auto task must transition to completed
	// 3. Completed event batch with pending resumable approval -> auto task must remain active
	// 4. Watcher event batch acknowledged via AckWatcherEvents -> stays active within grace, then archives & completes acked event
	evDoneUnpinned, _, _ := events.Enqueue(vaultDir, events.Event{Source: "gatekeeper", ScopeKey: "s1", Skill: "s1", Prompt: "p1"}, now)
	evDonePinned, _, _ := events.Enqueue(vaultDir, events.Event{Source: "gatekeeper", ScopeKey: "s2", Skill: "s2", Prompt: "p2"}, now)
	evPendingAppr, _, _ := events.Enqueue(vaultDir, events.Event{Source: "gatekeeper", ScopeKey: "s3", Skill: "s3", Prompt: "p3"}, now)
	evAckedWatch, _, _ := events.Enqueue(vaultDir, events.Event{
		Source:   "connector:gmail",
		ScopeKey: "s4",
		Skill:    "inbox-triage",
		Prompt:   "p4",
		Metadata: map[string]string{"requires_ack": "1", "item_key": "msg-1"},
	}, now)
	evStillRunning, _, _ := events.Enqueue(vaultDir, events.Event{Source: "gatekeeper", ScopeKey: "s5", Skill: "s5", Prompt: "p5"}, now)

	batches, _, err := events.LeaseNextBatches(vaultDir, now, events.DefaultLeaseTTL)
	if err != nil || len(batches) != 5 {
		t.Fatalf("LeaseNextBatches expected 5 batches, got %d (%v)", len(batches), err)
	}

	u1 := "11111111-1111-4111-8111-111111111101"
	u2 := "11111111-1111-4111-8111-111111111102"
	u3 := "11111111-1111-4111-8111-111111111103"
	u4 := "11111111-1111-4111-8111-111111111104"
	u5 := "11111111-1111-4111-8111-111111111105"

	_ = events.BindBatchConversation(vaultDir, []string{evDoneUnpinned.ID}, u1, now, events.DefaultLeaseTTL)
	_ = events.BindBatchConversation(vaultDir, []string{evDonePinned.ID}, u2, now, events.DefaultLeaseTTL)
	_ = events.BindBatchConversation(vaultDir, []string{evPendingAppr.ID}, u3, now, events.DefaultLeaseTTL)
	_ = events.BindBatchConversation(vaultDir, []string{evAckedWatch.ID}, u4, now, events.DefaultLeaseTTL)
	_ = events.BindBatchConversation(vaultDir, []string{evStillRunning.ID}, u5, now, events.DefaultLeaseTTL)

	nowStr := now.Format(time.RFC3339)
	for _, tc := range []ActiveTask{
		{TaskSlug: "auto-s1", Title: "[Auto] s1", Kind: "auto", ConversationID: u1, EventIDs: []string{evDoneUnpinned.ID}, Status: "active", CreatedAt: nowStr, LastUpdatedAt: nowStr},
		{TaskSlug: "auto-s2", Title: "[Auto] s2", Kind: "auto", ConversationID: u2, EventIDs: []string{evDonePinned.ID}, Pinned: true, Status: "active", CreatedAt: nowStr, LastUpdatedAt: nowStr},
		{TaskSlug: "auto-s3", Title: "[Auto] s3", Kind: "auto", ConversationID: u3, EventIDs: []string{evPendingAppr.ID}, Status: "active", CreatedAt: nowStr, LastUpdatedAt: nowStr},
		{TaskSlug: "auto-s4", Title: "[Auto] s4", Kind: "auto", ConversationID: u4, EventIDs: []string{evAckedWatch.ID}, Status: "active", CreatedAt: nowStr, LastUpdatedAt: nowStr},
		{TaskSlug: "auto-s5", Title: "[Auto] s5", Kind: "auto", ConversationID: u5, EventIDs: []string{evStillRunning.ID}, Status: "active", CreatedAt: nowStr, LastUpdatedAt: nowStr},
	} {
		if err := RegisterTask(vaultDir, tc); err != nil {
			t.Fatalf("RegisterTask(%s): %v", tc.TaskSlug, err)
		}
	}

	// Stage a pending resumable approval on u3
	if _, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:     "gmail_send",
		Summary:        "Send reply",
		Command:        "hq gmail send-draft --draft=d1 --hash=12345678",
		ConversationID: u3,
		ResumeOnDecide: true,
	}); err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	// Complete events 1, 2, 3 and ack event 4 without firing HandleStop!
	_, _ = events.CompleteIDs(vaultDir, []string{evDoneUnpinned.ID, evDonePinned.ID, evPendingAppr.ID}, now.Add(10*time.Second))
	if acked, aErr := events.AckWatcherEvents(vaultDir, u4, []string{"msg-1"}, now.Add(10*time.Second)); aErr != nil || acked != 1 {
		t.Fatalf("AckWatcherEvents: acked=%d err=%v", acked, aErr)
	}

	// At t = +30s (inside DefaultAutoAckGracePeriod = 2m): s1 -> archived, s2 -> completed, s3/s4/s5 -> still active.
	reconciled1, err := ReconcileAutoTasks(vaultDir, now.Add(30*time.Second), false)
	if err != nil {
		t.Fatalf("ReconcileAutoTasks(+30s): %v", err)
	}
	if len(reconciled1) != 2 {
		t.Fatalf("expected 2 reconciled auto tasks at +30s (auto-s1, auto-s2), got %v", reconciled1)
	}
	if !HasPendingSidebarSyncQueue(vaultDir) {
		t.Fatalf("expected EnqueueSidebarSync to queue u1 for sidebar archiving")
	}

	// At t = +3m (past DefaultAutoAckGracePeriod): CompactTasks must reconcile auto-s4 -> archived and finalize evAckedWatch to StatusDone!
	compRep, err := CompactTasks(vaultDir, CompactTasksOptions{
		Now:             now.Add(3 * time.Minute),
		NoSidebar:       true,
		NoWorktreePrune: true,
	})
	if err != nil {
		t.Fatalf("CompactTasks(+3m): %v", err)
	}
	if len(compRep.AutoArchivedSlugs) != 1 || compRep.AutoArchivedSlugs[0] != "auto-s4" {
		t.Fatalf("expected CompactTasks(+3m) to auto-archive [auto-s4], got %+v", compRep.AutoArchivedSlugs)
	}

	ev4Inspected, err := events.Inspect(vaultDir, evAckedWatch.ID)
	if err != nil || ev4Inspected.Status != events.StatusDone {
		t.Fatalf("expected acked watcher event %s to be finalized as StatusDone, got %+v (%v)", evAckedWatch.ID, ev4Inspected, err)
	}

	tasksAfter, _ := ListTasks(vaultDir)
	bySlug := make(map[string]ActiveTask)
	for _, tk := range tasksAfter {
		bySlug[tk.TaskSlug] = tk
	}
	if bySlug["auto-s1"].Status != "archived" {
		t.Fatalf("expected auto-s1 status=archived, got %q", bySlug["auto-s1"].Status)
	}
	if bySlug["auto-s2"].Status != "completed" {
		t.Fatalf("expected pinned auto-s2 status=completed, got %q", bySlug["auto-s2"].Status)
	}
	if bySlug["auto-s3"].Status != "active" {
		t.Fatalf("expected approval-waiting auto-s3 status=active, got %q", bySlug["auto-s3"].Status)
	}
	if bySlug["auto-s4"].Status != "archived" {
		t.Fatalf("expected acked auto-s4 status=archived, got %q", bySlug["auto-s4"].Status)
	}
	if bySlug["auto-s5"].Status != "active" {
		t.Fatalf("expected still-leased auto-s5 status=active, got %q", bySlug["auto-s5"].Status)
	}
}

func TestGitBackedProjectRegistryFolderURI(t *testing.T) {
	wsGitRepo := t.TempDir()
	wsWorkspaceOnly := t.TempDir()
	vaultDir := filepath.Join(wsGitRepo, "local")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatalf("mkdir vault: %v", err)
	}

	cfgDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", cfgDir)
	projDir := filepath.Join(cfgDir, "projects")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}

	// Git-backed project (`isWorkspaceOnly: false`) nests folderUri under `gitFolder.folderUri`
	gitBackedJSON := map[string]interface{}{
		"id":              "proj-git-backed-hq",
		"name":            "headquarter",
		"isWorkspaceOnly": false,
		"projectResources": map[string]interface{}{
			"resources": []map[string]interface{}{
				{
					"gitFolder": map[string]interface{}{
						"folderUri":     "file://" + filepath.ToSlash(wsGitRepo),
						"defaultBranch": "main",
					},
				},
			},
		},
	}
	rawGit, _ := json.MarshalIndent(gitBackedJSON, "", "  ")
	if err := os.WriteFile(filepath.Join(projDir, "proj-git-backed-hq.json"), rawGit, 0o644); err != nil {
		t.Fatalf("write git-backed project json: %v", err)
	}
	writeProjectRegistryFixture(t, cfgDir, "proj-ws-only-other", wsWorkspaceOnly)

	valid, avail := ValidateProjectMatchesWorkspace(vaultDir, "proj-git-backed-hq", wsGitRepo)
	if !avail || !valid {
		t.Fatalf("expected Git-backed projectId to validate against gitFolder.folderUri (valid=%v, avail=%v)", valid, avail)
	}
	resolved, regAvail, repaired := ResolveVerifiedWorkspaceProjectID(vaultDir, wsGitRepo, "proj-git-backed-hq")
	if !regAvail || repaired || resolved != "proj-git-backed-hq" {
		t.Fatalf("expected ResolveVerifiedWorkspaceProjectID to preserve proj-git-backed-hq without repair, got resolved=%q regAvail=%v repaired=%v", resolved, regAvail, repaired)
	}
	if fromBlank := ResolveProjectIDForWorkspace(vaultDir, wsGitRepo); fromBlank != "proj-git-backed-hq" {
		t.Fatalf("expected ResolveProjectIDForWorkspace to return proj-git-backed-hq, got %q", fromBlank)
	}
}

func TestDispatchPendingEventsRetryPreservesPriorConvAndEnqueuesSupersededSync(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 15, 21, 0, 0, 0, time.UTC)

	// 1. RegisterTask with empty ConversationID must preserve an existing non-empty ConversationID
	uInit := "33333333-3333-4333-8333-333333333301"
	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "manual-preserve-check",
		Title:          "[Task:hq] preserve-check",
		Kind:           "task",
		ConversationID: uInit,
		Status:         "active",
	}); err != nil {
		t.Fatalf("RegisterTask initial: %v", err)
	}
	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:    "manual-preserve-check",
		LastSummary: "updated summary without convId",
	}); err != nil {
		t.Fatalf("RegisterTask partial update: %v", err)
	}
	if gotTask, ok := FindTaskByConversationID(vaultDir, uInit); !ok || gotTask.ConversationID != uInit {
		t.Fatalf("expected RegisterTask with empty ConversationID to preserve %s, got ok=%v task=%+v", uInit, ok, gotTask)
	}

	// 2. DispatchPendingEvents Attempt 1 spawns uuid1; Attempt 2 fails on quota (must NOT delete uuid1);
	//    Attempt 3 spawns uuid2 and MUST enqueue superseded_by_retry sidebar sync for uuid1!
	binDir := t.TempDir()
	counterFile := filepath.Join(binDir, "count.txt")
	_ = os.WriteFile(counterFile, []byte("0"), 0o600)
	uAttempt1 := "44444444-4444-4444-8444-444444444401"
	uAttempt3 := "44444444-4444-4444-8444-444444444402"
	fakeAgentAPI := filepath.Join(binDir, "agentapi")
	agentScript := "#!/bin/sh\n" +
		"c=$(cat " + counterFile + ")\n" +
		"if [ \"$c\" = \"0\" ]; then\n" +
		"  echo 1 > " + counterFile + "\n" +
		"  echo " + uAttempt1 + "\n" +
		"else\n" +
		"  echo " + uAttempt3 + "\n" +
		"fi\n"
	if err := os.WriteFile(fakeAgentAPI, []byte(agentScript), 0o755); err != nil {
		t.Fatalf("write fake agentapi: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", "")
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "")

	ev, _, err := events.Enqueue(vaultDir, events.Event{
		Source:   "gatekeeper",
		ScopeKey: "retry-scope",
		Skill:    "inbox-triage",
		Title:    "[Auto] Retry Test",
		Prompt:   "Process batch",
	}, now)
	if err != nil {
		t.Fatalf("events.Enqueue: %v", err)
	}
	autoSlug := "auto-" + slugify("retry-scope-"+ev.ID)

	// Attempt 1: succeeds with uAttempt1
	acts1, err := DispatchPendingEvents(vaultDir, now, false)
	if err != nil || len(acts1) != 1 || !strings.Contains(acts1[0], uAttempt1) {
		t.Fatalf("Attempt 1 DispatchPendingEvents failed: acts=%v err=%v", acts1, err)
	}

	// Advance past lease expiry + retry backoff, open quota circuit breaker -> Attempt 2 fails SpawnTask!
	qOpen := LoadQuota(vaultDir)
	qOpen.CircuitOpen = true
	qOpen.CircuitReason = "test quota open"
	_ = SaveQuota(vaultDir, qOpen)

	tAttempt2 := now.Add(events.DefaultLeaseTTL + time.Minute)
	// First call reaps expired lease into pending with retry backoff; advance past backoff to dispatch Attempt 2
	_, _ = DispatchPendingEvents(vaultDir, tAttempt2, false)
	tAttempt2Ready := tAttempt2.Add(2 * time.Minute)
	acts2, err := DispatchPendingEvents(vaultDir, tAttempt2Ready, false)
	if err != nil || len(acts2) != 1 || !strings.Contains(acts2[0], "deferred") {
		t.Fatalf("expected Attempt 2 to defer on quota circuit open, got acts=%v err=%v", acts2, err)
	}

	// Verify uAttempt1 was NOT wiped or deleted from tasks.json on failed retry!
	if preserved, ok := FindTaskByConversationID(vaultDir, uAttempt1); !ok || preserved.TaskSlug != autoSlug {
		t.Fatalf("expected failed retry NOT to wipe or delete prior conversation %s, got ok=%v task=%+v", uAttempt1, ok, preserved)
	}

	// Close circuit breaker and advance past quota deferral -> Attempt 3 spawns uAttempt3 and enqueues superseded_by_retry for uAttempt1!
	qClosed := LoadQuota(vaultDir)
	qClosed.CircuitOpen = false
	qClosed.CircuitReason = ""
	_ = SaveQuota(vaultDir, qClosed)

	tAttempt3 := tAttempt2Ready.Add(2 * time.Minute)
	acts3, err := DispatchPendingEvents(vaultDir, tAttempt3, false)
	if err != nil || len(acts3) != 1 || !strings.Contains(acts3[0], uAttempt3) {
		t.Fatalf("expected Attempt 3 to spawn %s, got acts=%v err=%v", uAttempt3, acts3, err)
	}

	qItems := LoadSidebarSyncQueue(vaultDir)
	foundSuperseded := false
	for _, item := range qItems.Items {
		if item.ConversationID == uAttempt1 && item.DesiredArchived && item.LastError == "superseded_by_retry" {
			foundSuperseded = true
			break
		}
	}
	if !foundSuperseded {
		t.Fatalf("expected superseded_by_retry sidebar archive item queued for %s, got queue=%+v", uAttempt1, qItems.Items)
	}
	if latest, ok := FindTaskByConversationID(vaultDir, uAttempt3); !ok || latest.TaskSlug != autoSlug {
		t.Fatalf("expected %s bound to %s, got ok=%v task=%+v", autoSlug, uAttempt3, ok, latest)
	}
}
