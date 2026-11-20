package kernel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	if fromBlank, _, _ := ResolveVerifiedWorkspaceProjectID(vaultDir, wsGitRepo, ""); fromBlank != "proj-git-backed-hq" {
		t.Fatalf("expected ResolveVerifiedWorkspaceProjectID from blank to return proj-git-backed-hq, got %q", fromBlank)
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

func TestProjectRegistryFastPathAndCacheInvalidation(t *testing.T) {
	wsRoot := t.TempDir()
	wsSecond := t.TempDir()
	vaultDir := filepath.Join(wsRoot, "local")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatalf("mkdir vault: %v", err)
	}
	cfgDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", cfgDir)
	writeProjectRegistryFixture(t, cfgDir, "proj-fast-1", wsRoot)

	// 1. O(1) direct candidate lookup (`<dir>/<projectID>.json`) succeeds for both primary workspace and managed worktree.
	valid, regAvail := ValidateProjectMatchesWorkspace(vaultDir, "proj-fast-1", wsRoot)
	if !valid || !regAvail {
		t.Fatalf("expected O(1) ValidateProjectMatchesWorkspace=true, got valid=%v avail=%v", valid, regAvail)
	}
	wtDir := filepath.Join(vaultDir, "worktrees", "repo-feature")
	_ = os.MkdirAll(wtDir, 0o755)
	gotProj, regAvail2, repaired := ResolveVerifiedWorkspaceProjectID(vaultDir, wtDir, "proj-fast-1")
	if gotProj != "proj-fast-1" || !regAvail2 || repaired {
		t.Fatalf("expected managed worktree O(1) match for proj-fast-1, got proj=%q avail=%v repaired=%v", gotProj, regAvail2, repaired)
	}

	// 2. Cold scan populates in-process snapshot cache; adding a new project file invalidates fingerprint immediately.
	if emptyProj, _, _ := ResolveVerifiedWorkspaceProjectID(vaultDir, wsSecond, ""); emptyProj != "" {
		t.Fatalf("expected empty projectId before registering wsSecond, got %q", emptyProj)
	}
	writeProjectRegistryFixture(t, cfgDir, "proj-second-2", wsSecond)
	if resolvedSecond, _, _ := ResolveVerifiedWorkspaceProjectID(vaultDir, wsSecond, ""); resolvedSecond != "proj-second-2" {
		t.Fatalf("expected cache invalidation on new project file to return proj-second-2, got %q", resolvedSecond)
	}
}

func TestSpawnSubagentWorkspaceProjectVerificationAndConcurrentTaskLocking(t *testing.T) {
	wsParent := t.TempDir()
	wsUnregistered := t.TempDir()
	wsRegistered := t.TempDir()
	vaultDir := filepath.Join(wsParent, "local")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgDir := t.TempDir()
	t.Setenv("HQ_CONFIG_DIR", cfgDir)
	writeProjectRegistryFixture(t, cfgDir, "proj-parent-1", wsParent)
	writeProjectRegistryFixture(t, cfgDir, "proj-child-repo-2", wsRegistered)

	var mu sync.Mutex
	var capturedStartBodies []map[string]interface{}
	cascadeSeq := 0
	lsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/StartCascade"):
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			capturedStartBodies = append(capturedStartBodies, body)
			cascadeSeq++
			cid := fmt.Sprintf("00000000-0000-0000-0000-0000000009%02d", cascadeSeq)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"cascadeId": cid})
		case strings.HasSuffix(r.URL.Path, "/SendUserCascadeMessage"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer lsSrv.Close()

	t.Setenv("ANTIGRAVITY_LS_ADDRESS", lsSrv.URL)
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-test")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "proj-parent-1")
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "00000000-0000-0000-0000-000000000001")
	_ = SetFrontDesk(vaultDir, "00000000-0000-0000-0000-000000000001", "")

	// 1a. Subagent into unregistered external workspace must NOT inherit parent proj-parent-1; must pass workspaceUris.
	if _, err := SpawnSubagent(vaultDir, "", "[standard] ext: External Fix", "fix external", "flash", wsUnregistered, false); err != nil {
		t.Fatalf("SpawnSubagent unregistered workspace: %v", err)
	}
	if _, hasProj := capturedStartBodies[0]["projectEnvConfig"]; hasProj {
		t.Fatalf("expected unregistered target workspace to omit poisoned parent projectEnvConfig, got %+v", capturedStartBodies[0])
	}
	if uris, ok := capturedStartBodies[0]["workspaceUris"].([]interface{}); !ok || len(uris) != 1 {
		t.Fatalf("expected workspaceUris on unregistered target workspace, got %+v", capturedStartBodies[0]["workspaceUris"])
	}

	// 1b. Subagent into registered secondary workspace must repair proj-parent-1 -> proj-child-repo-2.
	if _, err := SpawnSubagent(vaultDir, "", "[standard] ext2: Registered Fix", "fix reg", "flash", wsRegistered, false); err != nil {
		t.Fatalf("SpawnSubagent registered workspace: %v", err)
	}
	projCfg, _ := capturedStartBodies[1]["projectEnvConfig"].(map[string]interface{})
	if gotID, _ := projCfg["projectId"].(string); gotID != "proj-child-repo-2" {
		t.Fatalf("expected repaired projectId=proj-child-repo-2, got %q (%+v)", gotID, capturedStartBodies[1])
	}

	// 2. Concurrent RegisterTask + RecordSubagentSpawn must not drop writes under RankTasks / RankQuota.
	const workers = 12
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			slug := fmt.Sprintf("conc-task-%02d", i)
			cid := fmt.Sprintf("00000000-0000-0000-0000-0000000010%02d", i)
			if err := RegisterTask(vaultDir, ActiveTask{
				TaskSlug:       slug,
				Title:          fmt.Sprintf("[Task:core] Concurrent %02d", i),
				Kind:           "task",
				ConversationID: cid,
				Status:         "active",
			}); err != nil {
				t.Errorf("RegisterTask(%s): %v", slug, err)
			}
			if err := RecordSubagentSpawn(vaultDir, fmt.Sprintf("[quick] core: Worker %02d", i), "flash_lite", fmt.Sprintf("job %02d", i)); err != nil {
				t.Errorf("RecordSubagentSpawn(%02d): %v", i, err)
			}
		}()
	}
	wg.Wait()

	allTasks, err := ListTasks(vaultDir)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	// 2 subagents from 1a/1b + 12 concurrent tasks = 14
	if len(allTasks) != workers+2 {
		t.Fatalf("expected %d tasks without lost concurrent updates, got %d", workers+2, len(allTasks))
	}
	q := LoadQuota(vaultDir)
	if q.SpawnsToday != workers+2 {
		t.Fatalf("expected SpawnsToday=%d without lost concurrent updates, got %d", workers+2, q.SpawnsToday)
	}
}

func TestSupersededConversationIDsAccumulationRollbackAndAutoAdoptRefusal(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

	u1 := "11111111-1111-4111-8111-111111111101"
	u2Fail := "22222222-2222-4222-8222-222222222202"
	u3 := "33333333-3333-4333-8333-333333333303"
	pendingSpawnSlug := "interactive-spawn-slot"

	// Register an unrelated interactive `hq spawn` slot to verify superseded/queued IDs can never steal it.
	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug: pendingSpawnSlug,
		Title:    "[Task:core] Interactive Task",
		Kind:     "task",
		Status:   "active",
	}); err != nil {
		t.Fatalf("RegisterTask interactive slot: %v", err)
	}

	spawnAttempt := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/exa.language_server_pb.LanguageServerService/GetCascadeModelConfigData":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"clientModelConfigs": []map[string]interface{}{
					{
						"modelId": "gemini-4-hydrogen-low",
						"modelOrAlias": map[string]interface{}{
							"model": "MODEL_PLACEHOLDER_TEST",
						},
					},
				},
			})
		case "/exa.language_server_pb.LanguageServerService/StartCascade":
			spawnAttempt++
			cid := u1
			switch spawnAttempt {
			case 2:
				cid = u2Fail
			case 3:
				cid = u3
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"cascadeId": cid})
		case "/exa.language_server_pb.LanguageServerService/SendUserCascadeMessage":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{})
		case "/exa.language_server_pb.LanguageServerService/GetCascadeTrajectory":
			var req struct {
				CascadeID string `json:"cascadeId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.CascadeID == u2Fail {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"status":        "CASCADE_RUN_STATUS_IDLE",
					"numTotalSteps": 2,
					"trajectory": map[string]interface{}{
						"steps": []map[string]interface{}{
							{"type": "CORTEX_STEP_TYPE_USER_INPUT", "status": "CORTEX_STEP_STATUS_DONE"},
							{
								"type":   "CORTEX_STEP_TYPE_ERROR_MESSAGE",
								"status": "CORTEX_STEP_STATUS_ERROR",
								"errorMessage": map[string]interface{}{
									"error": map[string]interface{}{"shortError": "failed to construct executor: transient error"},
								},
							},
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":        "CASCADE_RUN_STATUS_RUNNING",
				"numTotalSteps": 2,
				"trajectory": map[string]interface{}{
					"steps": []map[string]interface{}{
						{"type": "CORTEX_STEP_TYPE_USER_INPUT", "status": "CORTEX_STEP_STATUS_DONE"},
						{"type": "CORTEX_STEP_TYPE_PLANNER_RESPONSE", "status": "CORTEX_STEP_STATUS_RUNNING"},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("ANTIGRAVITY_LS_ADDRESS", strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-superseded")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "")

	ev, _, err := events.Enqueue(vaultDir, events.Event{
		Source:   "watcher",
		ScopeKey: "watch:mail:work",
		Skill:    "inbox-triage",
		Title:    "[Auto] Channel Triage",
		Prompt:   "Triage item",
		Model:    "gemini-4-hydrogen-low",
	}, now)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	autoSlug := "auto-" + slugify("watch:mail:work-"+ev.ID)

	// Attempt 1 binds u1.
	if _, err := DispatchPendingEvents(vaultDir, now, false); err != nil {
		t.Fatalf("Attempt 1 DispatchPendingEvents: %v", err)
	}

	// Lease expires -> Attempt 2 spawns u2Fail and fails at step 1 -> must restore u1 as ConversationID and record u2Fail as superseded!
	tReap := now.Add(events.DefaultLeaseTTL + time.Minute)
	_, _ = DispatchPendingEvents(vaultDir, tReap, false)
	tRetry2 := tReap.Add(2 * time.Minute)
	acts2, err := DispatchPendingEvents(vaultDir, tRetry2, false)
	if err != nil || len(acts2) != 1 || !strings.Contains(acts2[0], "deferred:") {
		t.Fatalf("expected Attempt 2 deferral on step1 error, got acts=%v err=%v", acts2, err)
	}
	afterFail2, locFail2, ok := FindTaskAnywhere(vaultDir, autoSlug)
	if !ok || locFail2 != "hot" || afterFail2.ConversationID != u1 || !afterFail2.HasSupersededConversationID(u2Fail) {
		t.Fatalf("expected Attempt 2 rollback to restore %s and keep %s superseded, got %+v (loc=%q ok=%v)", u1, u2Fail, afterFail2, locFail2, ok)
	}

	// Attempt 3 succeeds with u3 -> SupersededConversationIDs must include both u1 and u2Fail, and u1 must be queued for archive.
	tRetry3 := tRetry2.Add(2 * time.Minute)
	acts3, err := DispatchPendingEvents(vaultDir, tRetry3, false)
	if err != nil || len(acts3) != 1 || !strings.Contains(acts3[0], u3) {
		t.Fatalf("expected Attempt 3 to spawn %s, got acts=%v err=%v", u3, acts3, err)
	}
	afterAttempt3, loc3, ok := FindTaskAnywhere(vaultDir, autoSlug)
	if !ok || loc3 != "hot" || afterAttempt3.ConversationID != u3 || !afterAttempt3.HasSupersededConversationID(u1) || !afterAttempt3.HasSupersededConversationID(u2Fail) {
		t.Fatalf("expected u3 active with [u1, u2Fail] superseded, got %+v (loc=%q)", afterAttempt3, loc3)
	}

	// RegisterTask auto-adoption with empty slug or conv-* slug MUST refuse u1 and u2Fail, and must NOT steal pendingSpawnSlug!
	for _, orphanID := range []string{u1, u2Fail} {
		if err := RegisterTask(vaultDir, ActiveTask{
			Title:          "Orphan Late Stop",
			Kind:           "task",
			ConversationID: orphanID,
			Status:         "active",
		}); err != nil {
			t.Fatalf("RegisterTask(%s): %v", orphanID, err)
		}
		if err := RegisterTask(vaultDir, ActiveTask{
			TaskSlug:       "conv-" + orphanID[:8],
			Title:          "Explicit conv-* orphan",
			Kind:           "task",
			ConversationID: orphanID,
			Status:         "active",
		}); err != nil {
			t.Fatalf("RegisterTask explicit conv-*(%s): %v", orphanID, err)
		}
		if _, linked := LinkPendingSpawnTask(vaultDir, orphanID, ""); linked {
			t.Fatalf("expected LinkPendingSpawnTask to refuse superseded/queued %s", orphanID)
		}
	}
	if slot, locSlot, sOk := FindTaskAnywhere(vaultDir, pendingSpawnSlug); !sOk || locSlot != "hot" || slot.ConversationID != "" {
		t.Fatalf("expected interactive pending slot untouched, got %+v (loc=%q)", slot, locSlot)
	}
	if all, _ := ListTasks(vaultDir); len(all) != 2 {
		t.Fatalf("expected only pendingSpawnSlug + autoSlug (no conv-* orphans), got %+v", all)
	}

	// Archive autoSlug -> compact to cold tasks_archive.jsonl -> remove queue -> RegisterTask with u1 must STILL refuse!
	RemoveSidebarSyncQueueItem(vaultDir, u1, tRetry3)
	RemoveSidebarSyncQueueItem(vaultDir, u2Fail, tRetry3)
	_ = UpdateTasks(vaultDir, func(tasks []ActiveTask) ([]ActiveTask, bool, error) {
		for i := range tasks {
			if tasks[i].TaskSlug == autoSlug {
				tasks[i].Status = "archived"
				tasks[i].ArchivedAt = tRetry3.Add(-48 * time.Hour).Format(time.RFC3339)
				tasks[i].LastUpdatedAt = tasks[i].ArchivedAt
			}
		}
		return tasks, true, nil
	})
	if _, err := CompactTasks(vaultDir, CompactTasksOptions{
		Now:                tRetry3,
		ArchivedColdMaxAge: 24 * time.Hour,
		NoSidebar:          true,
		NoWorktreePrune:    true,
	}); err != nil {
		t.Fatalf("CompactTasks: %v", err)
	}
	att, loc, sup, found := FindTaskAttemptAnywhere(vaultDir, u1)
	if !found || loc != "cold" || !sup || att.TaskSlug != autoSlug {
		t.Fatalf("expected cold superseded lookup for u1, got found=%v loc=%q sup=%v task=%+v", found, loc, sup, att)
	}
	_ = RegisterTask(vaultDir, ActiveTask{ConversationID: u1, Title: "Very late u1", Kind: "task", Status: "active"})
	_ = RegisterTask(vaultDir, ActiveTask{ConversationID: u3, Title: "Late cold u3", Kind: "task", Status: "active"})
	if allAfterCold, _ := ListTasks(vaultDir); len(allAfterCold) != 1 || allAfterCold[0].TaskSlug != pendingSpawnSlug {
		t.Fatalf("expected cold superseded u1 and cold auto u3 to never auto-adopt, got %+v", allAfterCold)
	}
}
