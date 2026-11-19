package kernel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

	// 2. Drop a CSV file in local/inbox/ without ledger-ingest installed -> falls back to /inbox-triage
	inboxDir := filepath.Join(vaultDir, "inbox")
	_ = os.MkdirAll(inboxDir, 0o755)
	_ = os.WriteFile(filepath.Join(inboxDir, "unmatched.csv"), []byte("date,amount\n"), 0o644)

	statusFallback, err := RunHeartbeat(vaultDir, true)
	if err != nil || !strings.Contains(statusFallback, "unmatched.csv -> /inbox-triage") {
		t.Fatalf("expected unmatched.csv without starter skill to fall back to /inbox-triage, got: %q (%v)", statusFallback, err)
	}

	// 2b. Install local/skills/ledger-ingest/SKILL.md -> CSV drop routes to /ledger-ingest
	ledgerSkillDir := filepath.Join(vaultDir, "skills", "ledger-ingest")
	_ = os.MkdirAll(ledgerSkillDir, 0o755)
	_ = os.WriteFile(filepath.Join(ledgerSkillDir, "SKILL.md"), []byte("---\nname: ledger-ingest\ndescription: test\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(inboxDir, "stmt.csv"), []byte("date,amount\n"), 0o644)

	status2, err := RunHeartbeat(vaultDir, true)
	if err != nil || !strings.Contains(status2, "stmt.csv -> /ledger-ingest") {
		t.Fatalf("expected stmt.csv routed to /ledger-ingest, got: %q (%v)", status2, err)
	}

	q := LoadQuota(vaultDir)
	if q.SpawnsToday != 2 {
		t.Fatalf("expected SpawnsToday == 2, got %d", q.SpawnsToday)
	}

	// 3. Verify smart model + depth routing and user overrides
	if m, tier, sub := ResolveModel("Summarize the status of open PRs", "", "", ""); m != "gemini-4-hydrogen-low" || tier != "quick" || sub != "flash_lite" {
		t.Fatalf("unexpected quick tier routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("High-frequency Hiive inbox polling scan", "", "", ""); m != "gemini-4-hydrogen-low" || tier != "quick" || sub != "flash_lite" {
		t.Fatalf("unexpected hiive quick polling routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("Generate Morning Brief and multi-source summary", "", "", ""); m != "gemini-4-hydrogen-medium" || tier != "standard" || sub != "flash" {
		t.Fatalf("unexpected routine medium routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("[Auto] EDGAR Close Watch: Activate the thesis-check skill to monitor new after-close 8-K filings", "", "", ""); m != "gemini-4-hydrogen-medium" || tier != "standard" || sub != "flash" {
		t.Fatalf("unexpected edgar-close-watch medium routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("Implement the CSV parser for bank statements", "", "", ""); m != "gemini-4-hydrogen-high" || tier != "standard" || sub != "flash" {
		t.Fatalf("unexpected standard tier routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("Exhaustive multi-file refactor of kernel handlers", "", "", ""); m != "gemini-4-hydrogen-xhigh" || tier != "standard" || sub != "flash" {
		t.Fatalf("unexpected heavy xhigh sweep routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("Nightly sleep consolidation and memory dreaming", "", "", ""); m != "gemini-4-carbon-medium" || tier != "deep" || sub != "pro" {
		t.Fatalf("unexpected deep-medium consolidation routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("Perform a security audit and architecture review", "", "", ""); m != "gemini-4-carbon-high" || tier != "deep" || sub != "pro" {
		t.Fatalf("unexpected deep tier routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("Audit NVDA KC-1 breach and thesis falsification", "", "", ""); m != "gemini-4-carbon-high" || tier != "deep" || sub != "pro" {
		t.Fatalf("unexpected high-stakes thesis deep routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, _, _ := ResolveModel("Fix a bug", "opus", "medium", ""); m != "claude-opus-5-5-medium" {
		t.Fatalf("unexpected explicit user override: got %s", m)
	}
	if m, _, _ := ResolveModel("Fix a bug", "hydrogen", "xhigh", ""); m != "gemini-4-hydrogen-xhigh" {
		t.Fatalf("unexpected hydrogen override: got %s", m)
	}
	if m, _, _ := ResolveModel("Fix a bug", "hydrogen", "medium", ""); m != "gemini-4-hydrogen-medium" {
		t.Fatalf("unexpected hydrogen medium override: got %s", m)
	}
	if m, _, _ := ResolveModel("Fix a bug", "flash", "high", ""); m != "gemini-4-hydrogen-high" {
		t.Fatalf("unexpected flash alias override: got %s", m)
	}
	if m, _, _ := ResolveModel("Architecture design", "carbon", "high", ""); m != "gemini-4-carbon-high" {
		t.Fatalf("unexpected carbon override: got %s", m)
	}
	if m, tier, sub := ResolveModel("Consolidate memory", "", "", "deep-medium"); m != "gemini-4-carbon-medium" || tier != "deep" || sub != "pro" {
		t.Fatalf("unexpected deep-medium tier override: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("Refactor", "", "", "heavy"); m != "gemini-4-hydrogen-xhigh" || tier != "standard" || sub != "flash" {
		t.Fatalf("unexpected heavy tier override: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("Digest", "", "", "routine"); m != "gemini-4-hydrogen-medium" || tier != "standard" || sub != "flash" {
		t.Fatalf("unexpected routine tier override: model=%s tier=%s sub=%s", m, tier, sub)
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
	item, err := QueuePendingSubagentWithParent(vaultDir, "", "[standard] agy-hq: Test Fixer", "flash", "fix bug")
	if err != nil {
		t.Fatalf("QueuePendingSubagentWithParent error: %v", err)
	}
	if item.Title != "[Subagent:agy-hq] Test Fixer [standard]" {
		t.Fatalf("unexpected item title: %s", item.Title)
	}

	popped, ok := PopPendingSubagentForCaller(vaultDir, "", "", 10*time.Minute)
	if !ok || popped.TaskSlug != "subagent-agy-hq-test-fixer" {
		t.Fatalf("expected to pop pending subagent, got ok=%v, item=%+v", ok, popped)
	}
	// Second pop should be empty
	_, ok2 := PopPendingSubagentForCaller(vaultDir, "", "", 10*time.Minute)
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

	// 3. ReopenTaskWithOptions must restore worktree dir and set status to active
	if _, err := ReopenTaskWithOptions(vaultDir, taskSlug, ReopenTaskOptions{}); err != nil {
		t.Fatalf("ReopenTaskWithOptions failed: %v", err)
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

	popped, ok := PopPendingNestedSubagentForCaller(vaultDir, "", "", 10*time.Minute)
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

func TestSpawnTaskLSAttachmentAndHubDiscovery(t *testing.T) {
	var gotStartReqs []map[string]interface{}
	var gotSendReqs []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-codeium-csrf-token") != "hub-csrf-token" {
			http.Error(w, "missing csrf", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/exa.language_server_pb.LanguageServerService/GetCascadeModelConfigData":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"clientModelConfigs": []map[string]interface{}{
					{
						"modelId": "gemini-4-carbon-medium",
						"modelOrAlias": map[string]interface{}{
							"model": "MODEL_PLACEHOLDER_M421",
						},
					},
				},
			})
		case "/exa.language_server_pb.LanguageServerService/StartCascade":
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			_, hasProj := req["projectEnvConfig"]
			_, hasURIs := req["workspaceUris"]
			if hasProj && hasURIs {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"code":    "invalid_argument",
					"message": "invalid_argument: cannot specify both workspace URIs and project environment config",
				})
				return
			}
			gotStartReqs = append(gotStartReqs, req)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"cascadeId": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			})
		case "/exa.language_server_pb.LanguageServerService/SendUserCascadeMessage":
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotSendReqs = append(gotSendReqs, req)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	parsedURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse srv URL: %v", err)
	}
	port, err := strconv.Atoi(parsedURL.Port())
	if err != nil {
		t.Fatalf("parse srv port: %v", err)
	}

	// Simulate systemd daemon environment: no ANTIGRAVITY_LS_ADDRESS / ANTIGRAVITY_CSRF_TOKEN in env,
	// only <AppDataDir>/daemon/hub_<port>.json on disk.
	rootDir := t.TempDir()
	vaultDir := filepath.Join(rootDir, "local")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	appDataDir := t.TempDir()
	daemonDir := filepath.Join(appDataDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hubData, _ := json.Marshal(map[string]interface{}{
		"pid":       os.Getpid(),
		"httpPort":  port,
		"csrfToken": "hub-csrf-token",
	})
	if err := os.WriteFile(filepath.Join(daemonDir, fmt.Sprintf("hub_%d.json", port)), hubData, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ANTIGRAVITY_LS_ADDRESS", "")
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "")
	t.Setenv("ANTIGRAVITY_SIDECAR_WEB_PORT", "")
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", appDataDir)

	// 1. Without fd.ProjectID (typical daemon case): workspaceUris must still attach to rootDir.
	if err := SetFrontDesk(vaultDir, "conv-fd-daemon", ""); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}
	out, err := SpawnTask(vaultDir, "sleep-consolidate", "sleep-consolidate", "[Auto] sleep-consolidate", "Run nightly sleep", "gemini-4-carbon-medium", false)
	if err != nil {
		t.Fatalf("SpawnTask without ProjectID failed: %v", err)
	}
	if out != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("expected cascadeId output, got %q", out)
	}
	if len(gotStartReqs) != 1 || len(gotSendReqs) != 1 {
		t.Fatalf("expected 1 StartCascade and 1 SendUserCascadeMessage, got %d and %d", len(gotStartReqs), len(gotSendReqs))
	}
	req1 := gotStartReqs[0]
	if _, hasParent := req1["parentConversationId"]; hasParent {
		t.Fatalf("top-level SpawnTask must not set parentConversationId: %+v", req1)
	}
	if _, hasSub := req1["subagentDescriptor"]; hasSub {
		t.Fatalf("top-level SpawnTask must not set subagentDescriptor: %+v", req1)
	}
	expectedURI := "file://" + filepath.ToSlash(rootDir)
	uris, _ := req1["workspaceUris"].([]interface{})
	if len(uris) != 1 || uris[0] != expectedURI {
		t.Fatalf("expected workspaceUris=[%q], got %+v", expectedURI, req1["workspaceUris"])
	}
	if _, hasProj := req1["projectEnvConfig"]; hasProj {
		t.Fatalf("expected no projectEnvConfig when fd.ProjectID is empty, got %+v", req1["projectEnvConfig"])
	}

	// 2. With fd.ProjectID populated: projectEnvConfig.projectId must be sent and workspaceUris must be omitted
	// (StartCascade rejects specifying both with HTTP 400 invalid_argument).
	fd, _ := GetFrontDesk(vaultDir)
	fd.ProjectID = "proj-headquarter-uuid"
	if err := saveFrontDeskLease(vaultDir, fd); err != nil {
		t.Fatalf("saveFrontDeskLease: %v", err)
	}
	if _, err := SpawnTask(vaultDir, "morning-brief", "morning-brief", "[Auto] morning-brief", "Run morning brief", "gemini-4-carbon-medium", false); err != nil {
		t.Fatalf("SpawnTask with ProjectID failed: %v", err)
	}
	if len(gotStartReqs) != 2 {
		t.Fatalf("expected 2 StartCascade calls, got %d", len(gotStartReqs))
	}
	req2 := gotStartReqs[1]
	projCfg, _ := req2["projectEnvConfig"].(map[string]interface{})
	if projCfg["projectId"] != "proj-headquarter-uuid" {
		t.Fatalf("expected projectEnvConfig.projectId=proj-headquarter-uuid, got %+v", req2["projectEnvConfig"])
	}
	if _, hasURIs := req2["workspaceUris"]; hasURIs {
		t.Fatalf("expected workspaceUris to be omitted when projectId is set, got %+v", req2["workspaceUris"])
	}
}

func TestBuildCascadeRequestBodyMutualExclusion(t *testing.T) {
	wsDir := t.TempDir()
	expectedURI := "file://" + filepath.ToSlash(wsDir)

	// 1. Both projectID and workspaceDir set -> projectEnvConfig only, no workspaceUris.
	reqProj := buildCascadeRequestBody("MODEL_X", "", "", wsDir, "proj-123")
	if _, hasProj := reqProj["projectEnvConfig"]; !hasProj {
		t.Fatalf("expected projectEnvConfig when projectID is set")
	}
	if _, hasURIs := reqProj["workspaceUris"]; hasURIs {
		t.Fatalf("expected workspaceUris to be omitted when projectID is set, got %+v", reqProj["workspaceUris"])
	}

	// 2. Empty/whitespace projectID and workspaceDir set -> workspaceUris only, no projectEnvConfig.
	reqWS := buildCascadeRequestBody("MODEL_X", "parent-1", "[standard] scope: Role", wsDir, "   ")
	if _, hasProj := reqWS["projectEnvConfig"]; hasProj {
		t.Fatalf("expected projectEnvConfig to be omitted when projectID is empty, got %+v", reqWS["projectEnvConfig"])
	}
	uris, _ := reqWS["workspaceUris"].([]string)
	if len(uris) != 1 || uris[0] != expectedURI {
		t.Fatalf("expected workspaceUris=[%q], got %+v", expectedURI, reqWS["workspaceUris"])
	}
	if reqWS["parentConversationId"] != "parent-1" {
		t.Fatalf("expected parentConversationId=parent-1, got %v", reqWS["parentConversationId"])
	}

	// 3. Both empty -> neither field is set.
	reqEmpty := buildCascadeRequestBody("MODEL_X", "", "", "", "")
	if _, hasProj := reqEmpty["projectEnvConfig"]; hasProj {
		t.Fatalf("expected no projectEnvConfig when both are empty")
	}
	if _, hasURIs := reqEmpty["workspaceUris"]; hasURIs {
		t.Fatalf("expected no workspaceUris when both are empty")
	}
}

func TestBackfillFrontDeskProjectID(t *testing.T) {
	vaultDir := t.TempDir()
	fdConv := "conv-front-desk-backfill"
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "")
	t.Setenv("ANTIGRAVITY_SIDECAR_WEB_PORT", "")
	if err := SetFrontDesk(vaultDir, fdConv, ""); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}

	// 1. Different conversation must NOT backfill fd.ProjectID
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "conv-other")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "proj-wrong")
	BackfillFrontDeskProjectID(vaultDir)
	if fd, _ := GetFrontDesk(vaultDir); fd.ProjectID != "" {
		t.Fatalf("expected empty ProjectID for non-FrontDesk conversation, got %q", fd.ProjectID)
	}

	// 2. Sidecar environment must NOT backfill fd.ProjectID
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", fdConv)
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "proj-sidecar")
	t.Setenv("ANTIGRAVITY_SIDECAR_WEB_PORT", "49152")
	BackfillFrontDeskProjectID(vaultDir)
	if fd, _ := GetFrontDesk(vaultDir); fd.ProjectID != "" {
		t.Fatalf("expected empty ProjectID in sidecar env, got %q", fd.ProjectID)
	}

	// 3. Matching Front Desk conversation without sidecar DOES backfill fd.ProjectID
	t.Setenv("ANTIGRAVITY_SIDECAR_WEB_PORT", "")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "proj-real-hq")
	BackfillFrontDeskProjectID(vaultDir)
	if fd, ok := GetFrontDesk(vaultDir); !ok || fd.ProjectID != "proj-real-hq" {
		t.Fatalf("expected ProjectID=proj-real-hq, got %q (ok=%v)", fd.ProjectID, ok)
	}

	// 4. Subsequent run with different ANTIGRAVITY_PROJECT_ID must NOT overwrite existing fd.ProjectID
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "proj-other")
	BackfillFrontDeskProjectID(vaultDir)
	if fd, _ := GetFrontDesk(vaultDir); fd.ProjectID != "proj-real-hq" {
		t.Fatalf("expected existing ProjectID to be preserved, got %q", fd.ProjectID)
	}
}

func TestReconcileFrontDeskFiveCronsAndDaemonMigration(t *testing.T) {
	vaultDir := t.TempDir()
	trigYAML := `schedules:
  - id: gmail-inbox-watch
    cron: "CRON_TZ=America/Los_Angeles */15 7-22 * * *"
    mode: daemon_gatekeeper
    model: gemini-4-hydrogen-low
    precheck_cmd: ["hq", "watch", "precheck", "--connector=gmail", "--json"]
    dedup_mode: precheck_enqueues
    skill: inbox-triage
  - id: morning-brief
    cron: "CRON_TZ=America/Los_Angeles 30 7 * * *"
    mode: front_desk_cron
    model: gemini-4-hydrogen-medium
    skill: morning-brief
  - id: hiive-opportunity-watch
    cron: "CRON_TZ=America/Los_Angeles 0 11,16 * * 1-5"
    mode: isolated_worker
    model: gemini-4-hydrogen-low
    skill: thesis-check
  - id: edgar-close-watch
    cron: "CRON_TZ=America/Los_Angeles 30 13 * * 1-5"
    mode: daemon_gatekeeper
    model: gemini-4-hydrogen-medium
    precheck_cmd: ["hq", "status", "--quiet"]
    skill: thesis-check
  - id: sleep-consolidate
    cron: "CRON_TZ=America/Los_Angeles 30 23 * * *"
    mode: isolated_worker
    model: gemini-4-carbon-medium
    skill: sleep-consolidate
`
	if err := os.WriteFile(filepath.Join(vaultDir, "triggers.yaml"), []byte(trigYAML), 0o600); err != nil {
		t.Fatalf("write triggers.yaml: %v", err)
	}

	fdConv := "c15ecd75-f285-4ee0-8ee9-f97cb3979324"
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "")
	if err := SetFrontDesk(vaultDir, fdConv, ""); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}

	transcriptPath := filepath.Join(vaultDir, "transcript.jsonl")
	lines := []string{
		`{"step_index":1316,"type":"PLANNER_RESPONSE","created_at":"2026-11-12T07:53:57Z","tool_calls":[{"name":"schedule","args":{"CronExpression":"\"30 23 * * *\"","IsDaemon":"true","Prompt":"\"[Auto] Nightly Sleep Consolidation: Run /sleep-consolidate to review today's turn journals.\""}},{"name":"schedule","args":{"CronExpression":"\"30 7 * * *\"","IsDaemon":"true","Prompt":"\"[Auto] Morning Brief: Run /morning-brief to generate today's morning briefing.\""}},{"name":"schedule","args":{"CronExpression":"\"30 13 * * 1-5\"","IsDaemon":"true","Prompt":"\"[Auto] EDGAR Close Watch: Run /thesis-check to monitor new 10-K, 10-Q, 8-K filings.\""}},{"name":"schedule","args":{"CronExpression":"\"30 6 * * 1\"","IsDaemon":"true","Prompt":"\"[Auto] NVDA Opening Bell Watch: Run /thesis-check on NVDA against local/notes/investing/nvda_thesis.md.\""}},{"name":"schedule","args":{"CronExpression":"\"0 11,16 * * 1-5\"","IsDaemon":"true","Prompt":"\"[Auto] Clarity (Hiive) Secondary Watch: Check for new deal emails.\""}}]}`,
		`{"step_index":1317,"type":"GENERIC","created_at":"2026-11-12T07:54:00Z","content":"Tool is running as a background task with task id: c15ecd75-f285-4ee0-8ee9-f97cb3979324/task-1317\nTask Description: Cron: 30 23 * * *, Prompt: [Auto] Nightly Sleep Consolidation: Run /sleep-consolidate to review today's turn journals."}`,
		`{"step_index":1318,"type":"GENERIC","created_at":"2026-11-12T07:54:00Z","content":"Tool is running as a background task with task id: c15ecd75-f285-4ee0-8ee9-f97cb3979324/task-1318\nTask Description: Cron: 30 7 * * *, Prompt: [Auto] Morning Brief: Run /morning-brief to generate today's morning briefing."}`,
		`{"step_index":1319,"type":"GENERIC","created_at":"2026-11-12T07:54:01Z","content":"Tool is running as a background task with task id: c15ecd75-f285-4ee0-8ee9-f97cb3979324/task-1319\nTask Description: Cron: 30 13 * * 1-5, Prompt: [Auto] EDGAR Close Watch: Run /thesis-check to monitor new 10-K, 10-Q, 8-K filings."}`,
		`{"step_index":1320,"type":"GENERIC","created_at":"2026-11-12T07:54:01Z","content":"Tool is running as a background task with task id: c15ecd75-f285-4ee0-8ee9-f97cb3979324/task-1320\nTask Description: Cron: 30 6 * * 1, Prompt: [Auto] NVDA Opening Bell Watch: Run /thesis-check on NVDA against local/notes/investing/nvda_thesis.md."}`,
		`{"step_index":1321,"type":"GENERIC","created_at":"2026-11-12T07:54:01Z","content":"Tool is running as a background task with task id: c15ecd75-f285-4ee0-8ee9-f97cb3979324/task-1321\nTask Description: Cron: 0 11,16 * * 1-5, Prompt: [Auto] Clarity (Hiive) Secondary Watch: Check for new deal emails."}`,
	}
	if err := os.WriteFile(transcriptPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript.jsonl: %v", err)
	}

	if !ReconcileFrontDeskFromTranscript(vaultDir, transcriptPath) {
		t.Fatalf("expected ReconcileFrontDeskFromTranscript to return changed=true")
	}
	fd, ok := GetFrontDesk(vaultDir)
	if !ok {
		t.Fatalf("GetFrontDesk failed")
	}
	if len(fd.ArmedTriggers) != 4 {
		t.Fatalf("expected exactly 4 triggers.yaml ArmedTriggers (5th cron stays ad-hoc), got %d: %+v", len(fd.ArmedTriggers), fd.ArmedTriggers)
	}
	expectedTaskIDs := map[string]string{
		"sleep-consolidate":       fdConv + "/task-1317",
		"morning-brief":           fdConv + "/task-1318",
		"edgar-close-watch":       fdConv + "/task-1319",
		"hiive-opportunity-watch": fdConv + "/task-1321",
	}
	for id, wantTID := range expectedTaskIDs {
		if got := fd.ArmedTriggers[id].TaskID; got != wantTID {
			t.Fatalf("ArmedTriggers[%s].TaskID = %q, want %q", id, got, wantTID)
		}
	}
	if len(fd.AdHocCrons) != 1 || fd.AdHocCrons[0].TaskID != fdConv+"/task-1320" || fd.AdHocCrons[0].Cron != "30 6 * * 1" {
		t.Fatalf("expected 1 preserved AdHocCron for NVDA task-1320, got %+v", fd.AdHocCrons)
	}

	// Simulate active hq daemon: all 5 schedules count as daemon-armed, and the 4 duplicate Front Desk timers are marked migrated_to_daemon (task-1320 untouched).
	dBytes, _ := json.Marshal(daemonState{PID: os.Getpid(), Addr: "127.0.0.1:4799", StartedAt: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(daemonStatePath(vaultDir), dBytes, 0o600); err != nil {
		t.Fatalf("write daemon.json: %v", err)
	}
	armed, unarmed := GetUnarmedTriggers(vaultDir)
	if len(armed) != 5 || len(unarmed) != 0 {
		t.Fatalf("expected 5 armed in daemon / 0 unarmed, got armed=%d unarmed=%d", len(armed), len(unarmed))
	}
	disarm := GetDisarmTriggers(vaultDir)
	if len(disarm) != 4 {
		t.Fatalf("expected 4 GetDisarmTriggers items (excluding ad-hoc task-1320), got %+v", disarm)
	}
	for _, dt := range disarm {
		if dt.Reason != "migrated_to_daemon" || dt.TaskID == "" {
			t.Fatalf("expected Reason=migrated_to_daemon and non-empty TaskID, got %+v", dt)
		}
	}

	// Killing task-1317 must remove sleep-consolidate and never resurrect it on subsequent transcript reconciliation.
	if err := RecordKilledFrontDeskTask(vaultDir, fdConv, fdConv+"/task-1317"); err != nil {
		t.Fatalf("RecordKilledFrontDeskTask: %v", err)
	}
	_ = ReconcileFrontDeskFromTranscript(vaultDir, transcriptPath)
	fdAfterKill, _ := GetFrontDesk(vaultDir)
	if _, resurrected := fdAfterKill.ArmedTriggers["sleep-consolidate"]; resurrected {
		t.Fatalf("killed task-1317 resurrected into ArmedTriggers after reconciliation")
	}
	if len(fdAfterKill.AdHocCrons) != 1 || fdAfterKill.AdHocCrons[0].TaskID != fdConv+"/task-1320" {
		t.Fatalf("expected ad-hoc task-1320 preserved after killing task-1317, got %+v", fdAfterKill.AdHocCrons)
	}

	// Simulate pre-restart task-336 superseded by post-restart task-1319, followed by killing task-1319 and removing edgar-close-watch from triggers.yaml:
	// task-336 must NOT resurrect as an ad-hoc cron.
	fPre, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	preRestartLine := fmt.Sprintf("{\"type\":\"GENERIC\",\"created_at\":\"2026-11-08T18:33:31Z\",\"content\":\"Tool is running as a background task with task id: %s/task-336\\nTask Description: Cron: 30 13 * * 1-5, Prompt: [Auto] EDGAR Close Watch: Run /thesis-check to monitor new 10-K, 10-Q, 8-K filings and earnings releases against investment watchlists.\"}\n", fdConv)
	if err := os.WriteFile(transcriptPath, append([]byte(preRestartLine), fPre...), 0o600); err != nil {
		t.Fatalf("prepend pre-restart line: %v", err)
	}
	_ = RecordKilledFrontDeskTask(vaultDir, fdConv, fdConv+"/task-1319")
	coreOnlyYAML := `schedules:
  - id: morning-brief
    cron: "CRON_TZ=America/Los_Angeles 30 7 * * *"
    mode: front_desk_cron
    skill: morning-brief
  - id: sleep-consolidate
    cron: "CRON_TZ=America/Los_Angeles 30 23 * * *"
    mode: isolated_worker
    skill: sleep-consolidate
`
	_ = os.WriteFile(filepath.Join(vaultDir, "triggers.yaml"), []byte(coreOnlyYAML), 0o600)
	_ = ReconcileFrontDeskFromTranscript(vaultDir, transcriptPath)
	fdNoGhost, _ := GetFrontDesk(vaultDir)
	for _, ac := range fdNoGhost.AdHocCrons {
		if strings.Contains(ac.TaskID, "task-336") || strings.Contains(ac.TaskID, "task-1319") {
			t.Fatalf("expected killed/superseded EDGAR cron not to resurrect as ad-hoc cron, got %+v", fdNoGhost.AdHocCrons)
		}
	}
}

func TestSubagentPopCallerGuardsAndNestedTaskPoisonRecovery(t *testing.T) {
	vaultDir := t.TempDir()
	parentID := "11111111-1111-1111-1111-111111111111"
	otherTaskID := "22222222-2222-2222-2222-222222222222"
	realChildID := "33333333-3333-3333-3333-333333333333"

	if _, err := QueuePendingNestedSubagent(vaultDir, parentID, "[quick] repo: Research Helper", "flash_lite", "Inspect module imports and return findings"); err != nil {
		t.Fatalf("QueuePendingNestedSubagent: %v", err)
	}

	// 1. Self-pop by the parent conversation itself (e.g., on Stop hook) MUST be rejected!
	if _, ok := PopPendingNestedSubagentForCaller(vaultDir, parentID, "", 60*time.Minute); ok {
		t.Fatalf("expected PopPendingNestedSubagentForCaller to reject self-pop when callerConvID == parentConvID")
	}

	// 2. Unrelated conversation with a mismatched prompt MUST NOT hijack the pending nested subagent!
	if _, ok := PopPendingNestedSubagentForCaller(vaultDir, otherTaskID, "Dedicated sidebar task conversation for roadmap RFC", 60*time.Minute); ok {
		t.Fatalf("expected PopPendingNestedSubagentForCaller to reject mismatched prompt")
	}

	// 3. Genuine child conversation with matching prompt pops cleanly.
	popped, ok := PopPendingNestedSubagentForCaller(vaultDir, realChildID, "Inspect module imports and return findings", 60*time.Minute)
	if !ok || popped.ParentConversationID != parentID {
		t.Fatalf("expected genuine child to pop pending nested subagent, got ok=%v item=%+v", ok, popped)
	}

	// 4. RecordNestedConversation rejects self-parenting, and RegisterTask(Kind="task") + CleanNestedTasks scrub poisoned task IDs.
	_ = RecordNestedConversation(vaultDir, parentID, parentID)
	if IsNestedSubagentConversation(vaultDir, parentID) {
		t.Fatalf("expected self-parented RecordNestedConversation to be ignored")
	}

	// Simulate legacy poisoned nested_conversations.json containing an explicit top-level task ID and a self-parented record.
	rawPoison := `[
  {"conversationId":"` + otherTaskID + `","parentConversationId":"` + parentID + `","createdAt":"2026-11-17T00:00:00Z"},
  {"conversationId":"` + parentID + `","parentConversationId":"` + parentID + `","createdAt":"2026-11-17T00:00:01Z"},
  {"conversationId":"` + realChildID + `","parentConversationId":"` + parentID + `","createdAt":"2026-11-17T00:00:02Z"}
]`
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755)
	_ = os.WriteFile(nestedConversationsFilePath(vaultDir), []byte(rawPoison), 0o644)

	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "roadmap-rfc",
		Title:          "[Task:core] Roadmap RFC",
		Kind:           "task",
		ConversationID: otherTaskID,
		Status:         "active",
	}); err != nil {
		t.Fatalf("RegisterTask: %v", err)
	}
	if IsNestedSubagentConversation(vaultDir, otherTaskID) {
		t.Fatalf("expected RegisterTask(Kind=task) to scrub poisoned nested_conversations.json entry")
	}
	if IsSubagentConversation(vaultDir, otherTaskID) {
		t.Fatalf("expected IsSubagentConversation=false for explicit Kind=task conversation")
	}
	if !IsSubagentConversation(vaultDir, realChildID) {
		t.Fatalf("expected IsSubagentConversation=true for genuine nested child conversation")
	}

	// Re-inject poison and verify CleanNestedTasks scrubs explicit Kind="task" and self-parented IDs while keeping genuine child.
	_ = os.WriteFile(nestedConversationsFilePath(vaultDir), []byte(rawPoison), 0o644)
	if IsSubagentConversation(vaultDir, otherTaskID) {
		t.Fatalf("expected IsSubagentConversation=false for explicit Kind=task even before CleanNestedTasks runs")
	}
	if _, err := CleanNestedTasks(vaultDir); err != nil {
		t.Fatalf("CleanNestedTasks: %v", err)
	}
	if IsNestedSubagentConversation(vaultDir, otherTaskID) || IsNestedSubagentConversation(vaultDir, parentID) {
		t.Fatalf("expected CleanNestedTasks to scrub poisoned task and self-parented IDs")
	}
	if !IsNestedSubagentConversation(vaultDir, realChildID) {
		t.Fatalf("expected CleanNestedTasks to preserve genuine nested child ID")
	}
}

func TestVaultQuotaConfigMaxDailySpawnsAndInteractiveExemption(t *testing.T) {
	vaultDir := t.TempDir()
	fdConv := "dddddddd-1111-2222-3333-444444444444"
	_ = SetFrontDesk(vaultDir, fdConv, filepath.Join(vaultDir, "art-fd"))

	// Seed legacy persisted quota_state.json with MaxDailySpawns=15, then write config.yaml with max_daily_spawns: 2.
	_ = SaveQuota(vaultDir, QuotaState{
		Date:           time.Now().Format("2006-01-02"),
		MaxDailySpawns: 15,
	})
	cfgYAML := "# Vault config\nquota:\n  max_parallel_runs: 4\n  max_daily_spawns: 2 # custom cap\n  background_model: flash\n"
	if err := os.WriteFile(filepath.Join(vaultDir, "config.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	q0 := LoadQuota(vaultDir)
	if q0.MaxDailySpawns != 2 {
		t.Fatalf("expected LoadQuota to honor config.yaml max_daily_spawns=2 over persisted 15, got %d", q0.MaxDailySpawns)
	}

	// 1. Interactive Front Desk subagents and manual `hq spawn` increment InteractiveSpawnsToday + SpawnsToday,
	//    and MUST NOT consume or trip the automated MaxDailySpawns=2 budget!
	if err := RecordSubagentSpawn(vaultDir, "[quick] repo: Lookup Helper", "flash_lite", "Check version"); err != nil {
		t.Fatalf("RecordSubagentSpawn: %v", err)
	}
	if _, err := SpawnSubagent(vaultDir, fdConv, "[standard] repo: Unit Test Runner", "Run tests", "flash", vaultDir, true); err != nil {
		t.Fatalf("SpawnSubagent dryRun: %v", err)
	}
	if _, err := SpawnTask(vaultDir, "manual-spawn:design-rfc", "swe-pulse", "[Task:core] Design RFC", "Draft RFC", "gemini-4-carbon-high", true); err != nil {
		t.Fatalf("manual SpawnTask dryRun: %v", err)
	}

	qInter := LoadQuota(vaultDir)
	if qInter.InteractiveSpawnsToday != 3 || qInter.AutoSpawnsToday != 0 || qInter.SpawnsToday != 3 || qInter.EffectiveAutoSpawns() != 0 {
		t.Fatalf("unexpected interactive/auto quota split: %+v", qInter)
	}
	if qInter.CircuitOpen {
		t.Fatalf("expected interactive spawns NOT to trip CircuitOpen, got %+v", qInter)
	}

	// 2. Two automated background spawns succeed (reaching 2/2); the 3rd automated spawn trips CircuitOpen!
	for i := 1; i <= 2; i++ {
		if _, err := SpawnTask(vaultDir, "watch:gmail:work", "inbox-triage", "[Auto] Triage", "Triage batch", "gemini-4-hydrogen-medium", true); err != nil {
			t.Fatalf("auto SpawnTask #%d failed unexpectedly: %v", i, err)
		}
	}
	if _, err := SpawnTask(vaultDir, "watch:gmail:work", "inbox-triage", "[Auto] Triage", "Triage batch 3", "gemini-4-hydrogen-medium", true); err == nil {
		t.Fatalf("expected 3rd automated SpawnTask to be blocked at MaxDailySpawns=2")
	}
	qTripped := LoadQuota(vaultDir)
	if !qTripped.CircuitOpen || qTripped.AutoSpawnsToday != 2 || qTripped.InteractiveSpawnsToday != 3 || qTripped.SpawnsToday != 5 {
		t.Fatalf("expected CircuitOpen=true with Auto=2, Interactive=3, Total=5; got %+v", qTripped)
	}

	// 3. Even while automated CircuitOpen is tripped, interactive manual-spawn and Front Desk hq subagent STILL work!
	if _, err := SpawnTask(vaultDir, "manual-spawn:hotfix", "swe-pulse", "[Task:core] Hotfix", "Fix bug", "gemini-4-hydrogen-high", true); err != nil {
		t.Fatalf("expected manual-spawn to succeed even when background CircuitOpen is true, got: %v", err)
	}
	if _, err := SpawnSubagent(vaultDir, fdConv, "[quick] core: Verifier", "Verify fix", "flash_lite", vaultDir, true); err != nil {
		t.Fatalf("expected Front Desk SpawnSubagent to succeed even when background CircuitOpen is true, got: %v", err)
	}

	// 4. Raising max_daily_spawns via SetVaultMaxDailySpawns updates config.yaml in-place AND auto-clears CircuitOpen!
	qRaised, err := SetVaultMaxDailySpawns(vaultDir, 25)
	if err != nil {
		t.Fatalf("SetVaultMaxDailySpawns: %v", err)
	}
	if qRaised.MaxDailySpawns != 25 || qRaised.CircuitOpen {
		t.Fatalf("expected MaxDailySpawns=25 and CircuitOpen=false after raising cap, got %+v", qRaised)
	}
	updatedCfg, err := os.ReadFile(filepath.Join(vaultDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read updated config.yaml: %v", err)
	}
	if !strings.Contains(string(updatedCfg), "max_daily_spawns: 25") || !strings.Contains(string(updatedCfg), "max_parallel_runs: 4") {
		t.Fatalf("expected in-place config.yaml update preserving sibling keys, got:\n%s", string(updatedCfg))
	}
}

func TestLayer1LSLineageAndCleanNestedTasksLeakFixes(t *testing.T) {
	vaultDir := t.TempDir()
	fdID := "aaaaaaaa-0000-0000-0000-000000000001"
	topTaskID := "aaaaaaaa-0000-0000-0000-000000000002"
	subDirectID := "aaaaaaaa-0000-0000-0000-000000000003"
	subNestedID := "aaaaaaaa-0000-0000-0000-000000000004"
	wfChildID := "aaaaaaaa-0000-0000-0000-000000000005"
	standaloneID := "aaaaaaaa-0000-0000-0000-000000000006"

	_ = SetFrontDesk(vaultDir, fdID, "")

	summaries := map[string]LSTrajectorySummary{
		fdID: {
			TrajectoryMetadata: LSTrajectoryMetadata{RootConversationID: fdID, NestingDepth: 0},
		},
		topTaskID: {
			TrajectoryMetadata: LSTrajectoryMetadata{RootConversationID: topTaskID, NestingDepth: 0},
		},
		subDirectID: {
			TrajectoryMetadata: LSTrajectoryMetadata{
				ParentConversationID: fdID,
				RootConversationID:   fdID,
				NestingDepth:         1,
				SubagentSpec:         &LSSubagentSpec{Role: "[standard] core: Direct Worker", Model: "flash"},
			},
		},
		subNestedID: {
			TrajectoryMetadata: LSTrajectoryMetadata{
				ParentConversationID: topTaskID,
				RootConversationID:   topTaskID,
				NestingDepth:         1,
				SubagentSpec:         &LSSubagentSpec{Role: "[quick] core: Nested Sub", Model: "flash_lite"},
			},
		},
		wfChildID: {
			TrajectoryMetadata: LSTrajectoryMetadata{
				RootConversationID: topTaskID,
				NestingDepth:       0,
				SourceMetadata: &LSSourceMetadata{
					HideFromConversationList: true,
					Tool: &LSSourceMetadataTool{
						ConversationID: topTaskID,
						Title:          "Workflow fanout",
						ToolCall:       &LSSourceMetadataToolCall{ID: "tc1", Name: "run_workflow"},
					},
				},
			},
		},
		standaloneID: {
			TrajectoryMetadata: LSTrajectoryMetadata{
				RootConversationID: standaloneID,
				NestingDepth:       0,
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-codeium-csrf-token") != "csrf-test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/GetAllCascadeTrajectories") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"trajectorySummaries": summaries})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/GetCascadeTrajectory") {
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			cid := normalizeUUID(req["cascadeId"])
			if sum, ok := summaries[cid]; ok {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"status": "CASCADE_RUN_STATUS_IDLE",
					"trajectory": map[string]interface{}{
						"metadata": sum.TrajectoryMetadata,
					},
				})
				return
			}
			http.NotFound(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", u.Host)
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-test")

	// 1. InspectLSConversationLineage checks unary metadata classification
	linWF, okWF := InspectLSConversationLineage(vaultDir, wfChildID)
	if !okWF || !linWF.IsNestedDescendant || !linWF.IsWorkflowChild || linWF.DirectParentID != topTaskID {
		t.Fatalf("expected wfChildID to be nested workflow child under %s, got ok=%v %+v", topTaskID, okWF, linWF)
	}
	linNested, okNested := InspectLSConversationLineage(vaultDir, subNestedID)
	if !okNested || !linNested.IsNestedDescendant || linNested.IsDirectFrontDeskSubagent {
		t.Fatalf("expected subNestedID to be nested descendant, got ok=%v %+v", okNested, linNested)
	}
	linDirect, okDirect := InspectLSConversationLineage(vaultDir, subDirectID)
	if !okDirect || linDirect.IsNestedDescendant || !linDirect.IsDirectFrontDeskSubagent {
		t.Fatalf("expected subDirectID to be direct FD subagent, got ok=%v %+v", okDirect, linDirect)
	}
	linStandalone, okStandalone := InspectLSConversationLineage(vaultDir, standaloneID)
	if !okStandalone || linStandalone.IsNestedDescendant || linStandalone.IsDirectFrontDeskSubagent {
		t.Fatalf("expected standaloneID to be Layer-1 standalone conversation, got ok=%v %+v", okStandalone, linStandalone)
	}

	// 2. IsLayer1Task rejects leaked Kind:"task" with non-FD parent or subagent markers
	leakedKindTaskWithParent := ActiveTask{
		TaskSlug:             "conv-aaaa0004",
		Title:                "[Task:core] Leaked Nested",
		Kind:                 "task",
		ParentConversationID: topTaskID,
		ConversationID:       subNestedID,
	}
	leakedKindTaskWithSubMarker := ActiveTask{
		TaskSlug: "subagent-core-leaked",
		Title:    "[Subagent:core] Leaked Marker [quick]",
		Kind:     "task",
	}
	if IsLayer1Task(leakedKindTaskWithParent, fdID) {
		t.Fatalf("expected IsLayer1Task to reject Kind=task with non-FD ParentConversationID")
	}
	if IsLayer1Task(leakedKindTaskWithSubMarker, fdID) {
		t.Fatalf("expected IsLayer1Task to reject Kind=task with subagent markers")
	}

	// 3. CleanNestedTasks prunes leaked Kind:"task" subagents & LS workflow children while keeping standalone & explicit tasks
	q := LoadQuota(vaultDir)
	q.SpawnsToday = 2
	_ = SaveQuota(vaultDir, q)

	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "core-explicit-task",
		Title:          "[Task:core] Explicit Task",
		Kind:           "task",
		ConversationID: topTaskID,
		Status:         "active",
	})
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "conv-aaaaaaaa-0006",
		Title:          "[Task:general] Standalone User Chat",
		Kind:           "task",
		ConversationID: standaloneID,
		Status:         "idle",
	})
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:             "subagent-core-leaked-nested",
		Title:                "[Subagent:core] Nested Sub",
		Kind:                 "task",
		ParentConversationID: topTaskID,
		ConversationID:       subNestedID,
		Status:               "completed",
	})
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "conv-aaaaaaaa-0005",
		Title:          "Workflow step worker",
		Kind:           "task",
		ConversationID: wfChildID,
		Status:         "idle",
	})

	removed, err := CleanNestedTasks(vaultDir)
	if err != nil {
		t.Fatalf("CleanNestedTasks: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("expected 2 leaked entries removed, got %d (%v)", len(removed), removed)
	}
	remaining, _ := ListTasks(vaultDir)
	if len(remaining) != 2 {
		t.Fatalf("expected 2 genuine Layer-1 tasks remaining, got %d: %+v", len(remaining), remaining)
	}
	if !IsNestedSubagentConversation(vaultDir, wfChildID) || !IsNestedSubagentConversation(vaultDir, subNestedID) {
		t.Fatalf("expected pruned workflow & nested subagent to be recorded in nested_conversations.json")
	}
	// Quota SpawnsToday should NOT decrement for leaked Kind:"task" entries.
	if afterQ := LoadQuota(vaultDir); afterQ.SpawnsToday != 2 {
		t.Fatalf("expected SpawnsToday to remain 2 when pruning leaked Kind=task entries, got %d", afterQ.SpawnsToday)
	}
	// Archived cold entries should be reclassified to Kind:"subagent".
	cold, _, _ := ListArchivedTasks(vaultDir, 0)
	for _, c := range cold {
		if c.Kind != "subagent" {
			t.Fatalf("expected archived leak %s reclassified to Kind=subagent, got %q", c.TaskSlug, c.Kind)
		}
	}
}

func TestFrontDeskRotationPreservesLayer1SubagentsAndAvoidsBatchFallback(t *testing.T) {
	vaultDir := t.TempDir()
	fd1 := "bbbbbbbb-0000-0000-0000-000000000001"
	fd2 := "bbbbbbbb-0000-0000-0000-000000000002"
	sub1 := "bbbbbbbb-0000-0000-0000-000000000003"
	hop1 := "bbbbbbbb-0000-0000-0000-000000000004"
	hop2 := "bbbbbbbb-0000-0000-0000-000000000005"
	omittedMetaID := "bbbbbbbb-0000-0000-0000-000000000006"

	_ = SetFrontDesk(vaultDir, fd1, "")
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:             "subagent-core-fd1-worker",
		Title:                "[Subagent:core] FD1 Worker [standard]",
		Kind:                 "subagent",
		ParentConversationID: fd1,
		ConversationID:       sub1,
		Status:               "active",
	})
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "core-user-named-conv-task",
		Title:          "[Task:core] User Named Conv Refactor",
		Kind:           "task",
		ConversationID: hop1,
		Status:         "active",
	})
	// Check user-named conv-refactor slug isn't misclassified when Title is [Task:...] and TaskSlug matches.
	if !IsExplicitTopLevelTask(ActiveTask{
		TaskSlug:       "conv-refactor",
		Title:          "[Task:core] Conv Refactor",
		Kind:           "task",
		ConversationID: hop1,
	}) {
		t.Fatalf("expected explicit user-named conv-refactor task to be treated as IsExplicitTopLevelTask")
	}

	// Rotate Front Desk fd1 -> vacant -> fd2
	if err := ReleaseFrontDesk(vaultDir); err != nil {
		t.Fatalf("ReleaseFrontDesk: %v", err)
	}
	// During temporary vacancy, fd1's active subagent stays Layer-1 and survives CleanNestedTasks
	if removed, err := CleanNestedTasks(vaultDir); err != nil || len(removed) != 0 {
		t.Fatalf("expected temporary Front Desk vacancy to NOT purge fd1 subagent, got err=%v removed=%v", err, removed)
	}
	_ = SetFrontDesk(vaultDir, fd2, "")
	if removed, err := CleanNestedTasks(vaultDir); err != nil || len(removed) != 0 {
		t.Fatalf("expected Front Desk rotation to fd2 to NOT purge fd1 subagent, got err=%v removed=%v", err, removed)
	}
	if layer1 := FilterLayer1Tasks(vaultDir, func() []ActiveTask { l, _ := ListTasks(vaultDir); return l }()); len(layer1) != 2 {
		t.Fatalf("expected both fd1 subagent and top-level task to remain Layer-1 after rotation, got %+v", layer1)
	}

	// Verify transitive multi-hop HasNonFrontDeskAncestry (hop1 [task] -> sub -> hop2)
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:             "subagent-core-hop",
		Title:                "[Subagent:core] Hop Sub",
		Kind:                 "subagent",
		ParentConversationID: hop1,
		ConversationID:       hop2,
		Status:               "active",
	})
	if hasAnc, _ := HasNonFrontDeskAncestry(vaultDir, "", hop2); !hasAnc {
		t.Fatalf("expected transitive child of %s to report HasNonFrontDeskAncestry=true", hop2)
	}

	// Verify InspectLSConversationLineage handles HTTP 200 omitted proto3 metadata WITHOUT calling GetAllCascadeTrajectories
	batchCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/GetAllCascadeTrajectories") {
			batchCalls++
			http.Error(w, "should not be called on HTTP 200 unary response", http.StatusInternalServerError)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/GetCascadeTrajectory") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "CASCADE_RUN_STATUS_IDLE",
				"trajectory": map[string]interface{}{
					"steps": []interface{}{},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", u.Host)
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-test")

	lin, ok := InspectLSConversationLineage(vaultDir, omittedMetaID)
	if !ok || lin.IsNestedDescendant || lin.IsDirectFrontDeskSubagent {
		t.Fatalf("expected omitted proto3 metadata HTTP 200 to return standalone lin, got ok=%v %+v", ok, lin)
	}
	if batchCalls != 0 {
		t.Fatalf("expected 0 GetAllCascadeTrajectories fallback calls on HTTP 200 GetCascadeTrajectory, got %d", batchCalls)
	}
}
