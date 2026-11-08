package kernel

import (
	"os"
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
	if m, tier, sub := ResolveModel("Summarize the status of open PRs", "", "", ""); m != "gemini-3.8-flash-low" || tier != "quick" || sub != "flash_lite" {
		t.Fatalf("unexpected quick tier routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("Implement the CSV parser for bank statements", "", "", ""); m != "gemini-3.8-flash-high" || tier != "standard" || sub != "flash" {
		t.Fatalf("unexpected standard tier routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("Perform a security audit and architecture review", "", "", ""); m != "gemini-4-argon-high" || tier != "deep" || sub != "pro" {
		t.Fatalf("unexpected deep tier routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, _, _ := ResolveModel("Fix a bug", "opus", "medium", ""); m != "claude-opus-5-5-medium" {
		t.Fatalf("unexpected explicit user override: got %s", m)
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
