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
	"testing"
	"time"
)

func TestClassifyInboxAndHeartbeatAndCircuitBreaker(t *testing.T) {
	if got := ClassifyInboxFile("chase-2026-11.csv"); got != "ledger-ingest" {
		t.Fatalf("expected ledger-ingest, got %s", got)
	}
	if got := ClassifyInboxFile("2026-W2-employer.pdf"); got != "tax-prep" {
		t.Fatalf("expected tax-prep, got %s", got)
	}
	if got := ClassifyInboxFile("NVDA-10-Q-Q3.pdf"); got != "thesis-check" {
		t.Fatalf("expected thesis-check, got %s", got)
	}
	if got := ClassifyInboxFile("random-article-notes.md"); got != "inbox-triage" {
		t.Fatalf("expected inbox-triage fallback, got %s", got)
	}

	vaultDir := t.TempDir()
	// 1. Empty heartbeat should cost 0 tokens and record HEARTBEAT_OK
	status, err := RunHeartbeat(vaultDir, true)
	if err != nil || !strings.Contains(status, "HEARTBEAT_OK (0 LLM tokens)") {
		t.Fatalf("unexpected empty heartbeat status: %q (%v)", status, err)
	}

	// 2. Drop a CSV file in local/inbox/ and run heartbeat in dry-run mode
	inboxDir := filepath.Join(vaultDir, "inbox")
	_ = os.MkdirAll(inboxDir, 0o755)
	_ = os.WriteFile(filepath.Join(inboxDir, "stmt.csv"), []byte("date,amount\n"), 0o644)

	status2, err := RunHeartbeat(vaultDir, true)
	if err != nil || !strings.Contains(status2, "stmt.csv -> /ledger-ingest") {
		t.Fatalf("expected stmt.csv routed to /ledger-ingest, got: %q (%v)", status2, err)
	}

	q := LoadQuota(vaultDir)
	if q.SpawnsToday != 1 {
		t.Fatalf("expected SpawnsToday == 1, got %d", q.SpawnsToday)
	}

	// 3. Verify smart model + depth routing and user overrides
	if m, tier, sub := ResolveModel("Summarize the status of open PRs", "", "", ""); m != "gemini-4-hydrogen-low" || tier != "quick" || sub != "flash_lite" {
		t.Fatalf("unexpected quick tier routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("Implement the CSV parser for bank statements", "", "", ""); m != "gemini-4-hydrogen-high" || tier != "standard" || sub != "flash" {
		t.Fatalf("unexpected standard tier routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("Perform a security audit and architecture review", "", "", ""); m != "gemini-4-carbon-high" || tier != "deep" || sub != "pro" {
		t.Fatalf("unexpected deep tier routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, _, _ := ResolveModel("Fix a bug", "opus", "medium", ""); m != "claude-opus-5-5-medium" {
		t.Fatalf("unexpected explicit user override: got %s", m)
	}
	if m, _, _ := ResolveModel("Fix a bug", "hydrogen", "xhigh", ""); m != "gemini-4-hydrogen-xhigh" {
		t.Fatalf("unexpected hydrogen override: got %s", m)
	}
	if m, _, _ := ResolveModel("Fix a bug", "flash", "high", ""); m != "gemini-4-hydrogen-high" {
		t.Fatalf("unexpected flash alias override: got %s", m)
	}
	if m, _, _ := ResolveModel("Architecture design", "carbon", "high", ""); m != "gemini-4-carbon-high" {
		t.Fatalf("unexpected carbon override: got %s", m)
	}
	if m, _, _ := ResolveModel("Architecture design", "argon", "high", ""); m != "gemini-4-carbon-high" {
		t.Fatalf("unexpected argon alias override: got %s", m)
	}
}

func TestSubagentRoleParsingAndLifecycle(t *testing.T) {
	// 1. Role parsing
	tier, scope, roleName, title, slug := ParseSubagentRole("[standard] agy-hq: Bootstrap Lease Fixer", "flash")
	if tier != "standard" || scope != "agy-hq" || roleName != "Bootstrap Lease Fixer" ||
		title != "[Subagent:agy-hq] Bootstrap Lease Fixer [standard]" ||
		slug != "subagent-agy-hq-bootstrap-lease-fixer" {
		t.Fatalf("unexpected parse result for [standard] agy-hq: tier=%s scope=%s role=%s title=%s slug=%s", tier, scope, roleName, title, slug)
	}

	tier2, scope2, roleName2, title2, slug2 := ParseSubagentRole("[deep] NVDA: 10-Q Auditor", "pro")
	if tier2 != "deep" || scope2 != "NVDA" || roleName2 != "10-Q Auditor" ||
		title2 != "[Subagent:NVDA] 10-Q Auditor [deep]" ||
		slug2 != "subagent-nvda-10-q-auditor" {
		t.Fatalf("unexpected parse result for [deep] NVDA: tier=%s scope=%s role=%s title=%s slug=%s", tier2, scope2, roleName2, title2, slug2)
	}

	// Scope only (no tier bracket)
	tier3, scope3, _, title3, slug3 := ParseSubagentRole("opencli: Fix PR #279", "flash")
	if tier3 != "standard" || scope3 != "opencli" ||
		title3 != "[Subagent:opencli] Fix PR #279 [standard]" ||
		slug3 != "subagent-opencli-fix-pr-279" {
		t.Fatalf("unexpected parse result for opencli: tier=%s scope=%s title=%s slug=%s", tier3, scope3, title3, slug3)
	}

	// 2. Queue and Pop
	vaultDir := t.TempDir()
	item, err := QueuePendingSubagent(vaultDir, "[standard] agy-hq: Test Fixer", "flash", "fix bug")
	if err != nil {
		t.Fatalf("QueuePendingSubagent error: %v", err)
	}
	if item.Title != "[Subagent:agy-hq] Test Fixer [standard]" {
		t.Fatalf("unexpected item title: %s", item.Title)
	}

	popped, ok := PopPendingSubagent(vaultDir, 10*time.Minute)
	if !ok || popped.TaskSlug != "subagent-agy-hq-test-fixer" {
		t.Fatalf("expected to pop pending subagent, got ok=%v, item=%+v", ok, popped)
	}
	// Second pop should be empty
	_, ok2 := PopPendingSubagent(vaultDir, 10*time.Minute)
	if ok2 {
		t.Fatalf("expected queue to be empty on second pop")
	}

	// 3. RecordSubagentSpawn increments quota
	if err := RecordSubagentSpawn(vaultDir, "[standard] agy-hq: Test Fixer", "flash", "prompt"); err != nil {
		t.Fatalf("RecordSubagentSpawn error: %v", err)
	}
	q := LoadQuota(vaultDir)
	if q.SpawnsToday != 1 {
		t.Fatalf("expected SpawnsToday == 1, got %d", q.SpawnsToday)
	}

	// 4. CancelTasks and CancelAllSubagentTasks
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "subagent-agy-hq-test-fixer",
		Title:          "[Subagent:agy-hq] Test Fixer [standard]",
		ConversationID: "conv-sub-1",
		Status:         "active",
	})
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "subagent-agy-hq-other-worker",
		Title:          "[Subagent:agy-hq] Other Worker [standard]",
		ConversationID: "conv-sub-2",
		Status:         "active",
	})

	if err := CancelTasks(vaultDir, []string{"conv-sub-1"}, "killed"); err != nil {
		t.Fatalf("CancelTasks: %v", err)
	}
	tasks, _ := ListTasks(vaultDir)
	for _, tsk := range tasks {
		if tsk.ConversationID == "conv-sub-1" && tsk.Status != "cancelled" {
			t.Fatalf("expected conv-sub-1 to be cancelled, got %s", tsk.Status)
		}
		if tsk.ConversationID == "conv-sub-2" && tsk.Status != "active" {
			t.Fatalf("expected conv-sub-2 to remain active, got %s", tsk.Status)
		}
	}

	if err := CancelAllSubagentTasks(vaultDir, "kill all"); err != nil {
		t.Fatalf("CancelAllSubagentTasks: %v", err)
	}
	tasks2, _ := ListTasks(vaultDir)
	for _, tsk := range tasks2 {
		if tsk.Status != "cancelled" {
			t.Fatalf("expected all subagent tasks cancelled, got %+v", tsk)
		}
	}
}

func TestResolveModelEnumAndSpawnSubagent(t *testing.T) {
	// 1. Static fallback resolution
	enumVal, err := ResolveModelEnum("", "", "claude-opus-5-5-high")
	if err != nil || enumVal != "MODEL_PLACEHOLDER_M402" {
		t.Fatalf("expected MODEL_PLACEHOLDER_M402, got %q (%v)", enumVal, err)
	}
	if hVal, hErr := ResolveModelEnum("", "", "gemini-4-hydrogen-high"); hErr != nil || hVal != "MODEL_PLACEHOLDER_M525" {
		t.Fatalf("expected MODEL_PLACEHOLDER_M525 for gemini-4-hydrogen-high, got %q (%v)", hVal, hErr)
	}

	// 2. Mock LanguageServerService Connect JSON endpoints
	var gotStartReq map[string]interface{}
	var gotSendReq map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-codeium-csrf-token") != "test-csrf" {
			http.Error(w, "missing csrf", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/exa.language_server_pb.LanguageServerService/GetCascadeModelConfigData":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"clientModelConfigs": []map[string]interface{}{
					{
						"modelId": "claude-opus-5-5-high",
						"modelOrAlias": map[string]interface{}{
							"model": "MODEL_PLACEHOLDER_M402",
						},
					},
				},
			})
		case "/exa.language_server_pb.LanguageServerService/StartCascade":
			_ = json.NewDecoder(r.Body).Decode(&gotStartReq)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"cascadeId": "11111111-2222-3333-4444-555555555555",
			})
		case "/exa.language_server_pb.LanguageServerService/SendUserCascadeMessage":
			_ = json.NewDecoder(r.Body).Decode(&gotSendReq)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	lsAddr := strings.TrimPrefix(srv.URL, "http://")
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", lsAddr)
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "test-csrf")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "proj-test-123")

	vaultDir := t.TempDir()
	parentID := "00000000-0000-0000-0000-000000000001"
	_ = SetFrontDesk(vaultDir, parentID, "")
	role := "[deep] rfc-0005: Opus 5.5 Reviewer"
	out, err := SpawnSubagent(vaultDir, parentID, role, "Review RFC-0005", "claude-opus-5-5-high", "", false)
	if err != nil {
		t.Fatalf("SpawnSubagent failed: %v", err)
	}
	if !strings.Contains(out, "11111111-2222-3333-4444-555555555555") {
		t.Fatalf("unexpected SpawnSubagent output: %s", out)
	}
	if gotStartReq["parentConversationId"] != parentID {
		t.Fatalf("expected parentConversationId=%s, got %v", parentID, gotStartReq["parentConversationId"])
	}
	if gotStartReq["requestedModel"] != "MODEL_PLACEHOLDER_M402" {
		t.Fatalf("expected requestedModel=MODEL_PLACEHOLDER_M402, got %v", gotStartReq["requestedModel"])
	}
	subDesc, _ := gotStartReq["subagentDescriptor"].(map[string]interface{})
	if subDesc["typeName"] != "self" || subDesc["role"] != role {
		t.Fatalf("unexpected subagentDescriptor: %+v", subDesc)
	}
	scriptItem, _ := gotStartReq["agentScriptItem"].(map[string]interface{})
	if scriptItem["name"] != "self" {
		t.Fatalf("unexpected agentScriptItem: %+v", scriptItem)
	}
	if gotSendReq["cascadeId"] != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("expected fallback SendUserCascadeMessage for cascadeId, got %+v", gotSendReq)
	}

	tasks, err := ListTasks(vaultDir)
	if err != nil || len(tasks) != 1 || tasks[0].ConversationID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("expected subagent registered in tasks.json, got %+v (%v)", tasks, err)
	}
}

func TestArchiveAndReopenTask(t *testing.T) {
	vaultDir := t.TempDir()
	baseRepo := filepath.Join(t.TempDir(), "repo")
	_ = os.MkdirAll(baseRepo, 0o755)

	// Init git repo
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@agy-hq.local"},
		{"config", "user.name", "Test"},
	} {
		c := exec.Command("git", args...)
		c.Dir = baseRepo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	_ = os.WriteFile(filepath.Join(baseRepo, "README.md"), []byte("# Base\n"), 0o644)
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-m", "initial commit"},
	} {
		c := exec.Command("git", args...)
		c.Dir = baseRepo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}

	// Create worktree for task
	wtDir := filepath.Join(vaultDir, "worktrees", "repo-my-task")
	_ = os.MkdirAll(filepath.Dir(wtDir), 0o755)
	c := exec.Command("git", "-C", baseRepo, "worktree", "add", "-b", "hq/my-task", wtDir)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v (%s)", err, string(out))
	}

	taskSlug := "my-task"
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:    taskSlug,
		Title:       "[Task:repo] my-task",
		WorktreeDir: wtDir,
		RepoRoot:    baseRepo,
		Status:      "completed",
	})

	// 1. Uncommitted file in worktree -> ArchiveTask without force must fail
	dirtyFile := filepath.Join(wtDir, "dirty.txt")
	_ = os.WriteFile(dirtyFile, []byte("dirty"), 0o644)

	err := ArchiveTask(vaultDir, taskSlug, false)
	if err == nil {
		t.Fatalf("expected error archiving dirty worktree without force")
	}
	expectedErrMsg := fmt.Sprintf("refusing to archive %q: uncommitted changes in worktree (pass --force to override)", taskSlug)
	if err.Error() != expectedErrMsg {
		t.Fatalf("expected error %q, got %q", expectedErrMsg, err.Error())
	}

	// 2. ArchiveTask with force must succeed and remove worktree dir
	if err := ArchiveTask(vaultDir, taskSlug, true); err != nil {
		t.Fatalf("ArchiveTask with force failed: %v", err)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("expected worktree dir %q to be deleted, stat err: %v", wtDir, err)
	}
	tasks, _ := ListTasks(vaultDir)
	if len(tasks) != 1 || tasks[0].Status != "archived" {
		t.Fatalf("expected task status archived, got %+v", tasks)
	}

	// 3. ReopenTask must restore worktree dir and set status to active
	if err := ReopenTask(vaultDir, taskSlug); err != nil {
		t.Fatalf("ReopenTask failed: %v", err)
	}
	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("expected worktree dir %q to be restored, stat err: %v", wtDir, err)
	}
	tasks, _ = ListTasks(vaultDir)
	if len(tasks) != 1 || tasks[0].Status != "active" {
		t.Fatalf("expected task status active, got %+v", tasks)
	}
}

func TestLayer1TaskInvariantAndNestedSubagents(t *testing.T) {
	vaultDir := t.TempDir()
	deskConv := "conv-desk-12345"
	if err := SetFrontDesk(vaultDir, deskConv, ""); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}

	// 1. IsFrontDeskConversation check
	if !IsFrontDeskConversation(vaultDir, deskConv) {
		t.Fatalf("expected IsFrontDeskConversation == true for %s", deskConv)
	}
	if IsFrontDeskConversation(vaultDir, "conv-other") {
		t.Fatalf("expected IsFrontDeskConversation == false for conv-other")
	}

	// 2. IsLayer1Task check
	task1 := ActiveTask{Kind: "task", TaskSlug: "test-task"}
	auto1 := ActiveTask{Kind: "auto", TaskSlug: "auto-morning"}
	subLayer1 := ActiveTask{Kind: "subagent", TaskSlug: "sub-layer1", ParentConversationID: deskConv}
	subLegacy := ActiveTask{Kind: "subagent", TaskSlug: "sub-legacy"}
	subNested := ActiveTask{Kind: "subagent", TaskSlug: "sub-nested", ParentConversationID: "conv-other"}

	if !IsLayer1Task(task1, deskConv) {
		t.Fatalf("expected task1 to be Layer 1")
	}
	if !IsLayer1Task(auto1, deskConv) {
		t.Fatalf("expected auto1 to be Layer 1")
	}
	if !IsLayer1Task(subLayer1, deskConv) {
		t.Fatalf("expected subLayer1 to be Layer 1")
	}
	if !IsLayer1Task(subLegacy, deskConv) {
		t.Fatalf("expected legacy subagent with empty parent to be Layer 1")
	}
	if IsLayer1Task(subNested, deskConv) {
		t.Fatalf("expected subNested with non-desk parent to NOT be Layer 1")
	}

	// 3. QueuePendingNestedSubagent & PopPendingNestedSubagent
	item, err := QueuePendingNestedSubagent(vaultDir, "conv-secondary-task", "[standard] test: Nested Worker", "flash", "nested prompt")
	if err != nil {
		t.Fatalf("QueuePendingNestedSubagent: %v", err)
	}
	if item.ParentConversationID != "conv-secondary-task" {
		t.Fatalf("unexpected parentConvID: %s", item.ParentConversationID)
	}

	popped, ok := PopPendingNestedSubagent(vaultDir, 10*time.Minute)
	if !ok || popped.ParentConversationID != "conv-secondary-task" {
		t.Fatalf("expected popped nested subagent, got %+v (ok=%v)", popped, ok)
	}

	// 4. RecordNestedConversation & IsNestedSubagentConversation
	nestedConvID := "conv-nested-child-999"
	if err := RecordNestedConversation(vaultDir, nestedConvID, "conv-secondary-task"); err != nil {
		t.Fatalf("RecordNestedConversation: %v", err)
	}
	if !IsNestedSubagentConversation(vaultDir, nestedConvID) {
		t.Fatalf("expected IsNestedSubagentConversation == true for %s", nestedConvID)
	}
	if IsNestedSubagentConversation(vaultDir, "conv-non-nested") {
		t.Fatalf("expected IsNestedSubagentConversation == false for conv-non-nested")
	}

	// 5. FilterLayer1Tasks
	allTasks := []ActiveTask{
		task1,
		subLayer1,
		subNested,
		{Kind: "subagent", TaskSlug: "sub-recorded-nested", ConversationID: nestedConvID},
	}
	filtered := FilterLayer1Tasks(vaultDir, allTasks)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 Layer-1 tasks, got %d: %+v", len(filtered), filtered)
	}
	if filtered[0].TaskSlug != "test-task" || filtered[1].TaskSlug != "sub-layer1" {
		t.Fatalf("unexpected filtered tasks: %+v", filtered)
	}
}

func TestCleanNestedTasks(t *testing.T) {
	vaultDir := t.TempDir()
	deskConv := "conv-desk-999"
	_ = SetFrontDesk(vaultDir, deskConv, "")

	// Set initial quota with 3 spawns
	q := LoadQuota(vaultDir)
	q.SpawnsToday = 3
	_ = SaveQuota(vaultDir, q)

	// Register 1 top-level task, 1 Layer-1 subagent, and 1 nested subagent
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug: "top-level-task",
		Title:    "[Task:general] Top Level",
		Kind:     "task",
		Status:   "active",
	})
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:             "sub-layer1",
		Title:                "[Subagent:general] Layer 1 Worker",
		Kind:                 "subagent",
		ParentConversationID: deskConv,
		Status:               "completed",
	})
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:             "sub-nested-child",
		Title:                "[Subagent:general] Nested Worker",
		Kind:                 "subagent",
		ParentConversationID: "conv-secondary-task",
		Status:               "completed",
	})

	removed, err := CleanNestedTasks(vaultDir)
	if err != nil {
		t.Fatalf("CleanNestedTasks failed: %v", err)
	}
	if len(removed) != 1 || removed[0] != "sub-nested-child" {
		t.Fatalf("expected sub-nested-child to be removed, got: %v", removed)
	}

	// Verify tasks.json now only has 2 tasks
	remaining, err := ListTasks(vaultDir)
	if err != nil || len(remaining) != 2 {
		t.Fatalf("expected 2 remaining tasks in tasks.json, got %d (%v)", len(remaining), err)
	}

	// Verify quota SpawnsToday decremented from 3 to 2
	afterQuota := LoadQuota(vaultDir)
	if afterQuota.SpawnsToday != 2 {
		t.Fatalf("expected SpawnsToday == 2, got %d", afterQuota.SpawnsToday)
	}

	// Verify cold archive received the removed nested subagent
	cold, coldCount, _ := ListArchivedTasks(vaultDir, 0)
	if coldCount != 1 || len(cold) != 1 || cold[0].TaskSlug != "sub-nested-child" {
		t.Fatalf("expected cold archive to have sub-nested-child, got count=%d, list=%+v", coldCount, cold)
	}
}

func TestIsSubagentConversationAndSetFrontDeskGuard(t *testing.T) {
	vaultDir := t.TempDir()

	// 1. Unregistered conversation is NOT a subagent
	if IsSubagentConversation(vaultDir, "conv-user-1") {
		t.Fatalf("expected conv-user-1 to NOT be a subagent")
	}

	// 2. Normal task is NOT a subagent
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "headquarter-normal-task",
		Title:          "[Task:headquarter] Normal Task",
		Kind:           "task",
		ConversationID: "conv-task-1",
		Status:         "active",
	})
	if IsSubagentConversation(vaultDir, "conv-task-1") {
		t.Fatalf("expected conv-task-1 to NOT be a subagent")
	}

	// 3. Layer-1 subagent in tasks.json IS a subagent
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:             "subagent-agy-hq-fixer",
		Title:                "[Subagent:agy-hq] Fixer [standard]",
		Kind:                 "subagent",
		ParentConversationID: "conv-desk-1",
		ConversationID:       "conv-sub-1",
		Status:               "active",
	})
	if !IsSubagentConversation(vaultDir, "conv-sub-1") {
		t.Fatalf("expected conv-sub-1 to be recognized as a subagent")
	}

	// 4. Nested subagent in nested_conversations.json IS a subagent
	_ = RecordNestedConversation(vaultDir, "conv-nested-sub-2", "conv-sub-1")
	if !IsSubagentConversation(vaultDir, "conv-nested-sub-2") {
		t.Fatalf("expected conv-nested-sub-2 to be recognized as a subagent")
	}

	// 5. SetFrontDesk must reject child subagents
	if err := SetFrontDesk(vaultDir, "conv-sub-1", ""); err == nil {
		t.Fatalf("expected SetFrontDesk for conv-sub-1 to fail, got nil")
	} else if !strings.Contains(err.Error(), "cannot claim Front Desk lease for a child subagent") {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := SetFrontDesk(vaultDir, "conv-nested-sub-2", ""); err == nil {
		t.Fatalf("expected SetFrontDesk for conv-nested-sub-2 to fail, got nil")
	} else if !strings.Contains(err.Error(), "cannot claim Front Desk lease for a child subagent") {
		t.Fatalf("unexpected error: %v", err)
	}

	// 6. Normal conversation CAN claim Front Desk lease
	if err := SetFrontDesk(vaultDir, "conv-desk-1", ""); err != nil {
		t.Fatalf("expected SetFrontDesk for conv-desk-1 to succeed, got %v", err)
	}
}
