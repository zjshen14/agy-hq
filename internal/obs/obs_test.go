package obs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
)

func writeTranscriptFixture(t *testing.T, brainDir, convID string, lines []string) string {
	t.Helper()
	logDir := filepath.Join(brainDir, convID, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", logDir, err)
	}
	p := filepath.Join(logDir, "transcript.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write transcript %s: %v", p, err)
	}
	return p
}

func TestParseTranscriptTurnsLatencyAndCheckpoint(t *testing.T) {
	tmp := t.TempDir()
	brainDir := filepath.Join(tmp, "brain")
	convID := "11111111-2222-3333-4444-555555555555"

	lines := []string{
		`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-10T08:00:00Z","content":"<USER_REQUEST>Check status and ask me</USER_REQUEST>"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-10T08:00:03Z","input_tokens":20000,"cache_read_tokens":150000,"output_tokens":500,"tool_calls":[{"id":"1","name":"run_command","args":{"CommandLine":"./bin/hq status"}}]}`,
		`{"step_index":2,"type":"GENERIC","source":"SYSTEM","created_at":"2026-11-10T08:00:05Z","content":"Created At: 2026-11-10T08:00:03Z\nCompleted At: 2026-11-10T08:00:05Z\nOutput: ok"}`,
		`{"step_index":3,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-10T08:00:07Z","input_tokens":5000,"cache_read_tokens":170000,"output_tokens":400,"tool_calls":[{"id":"2","name":"ask_question","args":{"questions":[{"question":"Proceed?"}]}}]}`,
		`{"step_index":4,"type":"GENERIC","source":"SYSTEM","created_at":"2026-11-10T08:01:07Z","content":"Created At: 2026-11-10T08:00:07Z\nCompleted At: 2026-11-10T08:01:07Z\nAnswer: Yes"}`,
		`{"step_index":5,"type":"CHECKPOINT","source":"SYSTEM","created_at":"2026-11-10T08:01:08Z","input_tokens":10000,"cache_read_tokens":246000,"output_tokens":1200}`,
		`{"step_index":6,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-10T08:01:10Z","input_tokens":4000,"cache_read_tokens":60000,"output_tokens":300,"content":"All done."}`,
		`{"step_index":7,"type":"SYSTEM_MESSAGE","source":"SYSTEM","created_at":"2026-11-10T08:30:00Z","content":"[Auto] Morning Brief scheduled wakeup"}`,
		`{"step_index":8,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-10T08:30:04Z","input_tokens":64000,"cache_read_tokens":0,"output_tokens":600,"content":"Brief complete."}`,
	}
	tPath := writeTranscriptFixture(t, brainDir, convID, lines)

	cm, err := ParseTranscriptFile(convID, tPath, "gemini-4-argon-high", "deep")
	if err != nil {
		t.Fatalf("ParseTranscriptFile failed: %v", err)
	}
	if len(cm.Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(cm.Turns))
	}

	t1 := cm.Turns[0]
	if t1.TriggerKind != "user" {
		t.Errorf("turn 1 triggerKind = %q, want user", t1.TriggerKind)
	}
	if t1.CompactionCount != 1 {
		t.Errorf("turn 1 compactionCount = %d, want 1", t1.CompactionCount)
	}
	if t1.PeakContextTokens != 256000 {
		t.Errorf("turn 1 peakContextTokens = %d, want 256000", t1.PeakContextTokens)
	}
	if t1.CurrentContextTokens != 64000 {
		t.Errorf("turn 1 currentContextTokens = %d, want 64000 (post-checkpoint)", t1.CurrentContextTokens)
	}
	if t1.ToolLatencyMs != 2000 {
		t.Errorf("turn 1 toolLatencyMs = %d, want 2000", t1.ToolLatencyMs)
	}
	if t1.HumanWaitMs != 60000 {
		t.Errorf("turn 1 humanWaitMs = %d, want 60000 (ask_question wait)", t1.HumanWaitMs)
	}

	t2 := cm.Turns[1]
	if t2.TriggerKind != "schedule" {
		t.Errorf("turn 2 triggerKind = %q, want schedule", t2.TriggerKind)
	}
	if t2.CacheHitRatio != 0 {
		t.Errorf("turn 2 cacheHitRatio = %v, want 0 (cold wakeup)", t2.CacheHitRatio)
	}
	if cm.Self.CompactionCount != 1 {
		t.Errorf("cm.Self.CompactionCount = %d, want 1", cm.Self.CompactionCount)
	}
}

func TestDAGTreeRollupsAndSubagentAttributionReconcile(t *testing.T) {
	tmp := t.TempDir()
	vaultDir := filepath.Join(tmp, "local")
	brainDir := filepath.Join(tmp, "brain")
	t.Setenv("HQ_BRAIN_DIR", brainDir)

	fdID := "aaaaaaaa-0000-0000-0000-000000000001"
	sub1ID := "bbbbbbbb-0000-0000-0000-000000000002"
	nestedID := "cccccccc-0000-0000-0000-000000000003"

	if err := kernel.SetFrontDesk(vaultDir, fdID, filepath.Join(brainDir, fdID)); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}

	// Register sub1ID in tasks.json with a FIFO-drifted wrong title/slug to verify ground-truth repair
	_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:       "subagent-wrong-fifo-slug",
		Title:          "[Subagent:wrong] FIFO Drifted Worker [quick]",
		Kind:           "subagent",
		Scope:          "wrong",
		Tier:           "quick",
		Model:          "gemini-3.8-flash-high",
		ConversationID: sub1ID,
		Status:         "completed",
	})

	// 1. Front Desk transcript spawns sub1ID via invoke_subagent
	writeTranscriptFixture(t, brainDir, fdID, []string{
		`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-10T09:00:00Z","content":"Fix opencli PR #279"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-10T09:00:02Z","input_tokens":30000,"cache_read_tokens":100000,"output_tokens":500,"tool_calls":[{"id":"c1","name":"invoke_subagent","args":{"Role":"[standard] opencli: PR 279 Fixer","Model":"flash","TypeName":"self","Prompt":"Fix PR 279 in worktree"}}]}`,
		fmt.Sprintf(`{"step_index":2,"type":"GENERIC","source":"SYSTEM","created_at":"2026-11-10T09:00:03Z","content":"Created the following subagent:\n{\n  \"conversationId\": \"%s\"\n}"}`, sub1ID),
	})

	// 2. Layer-1 subagent transcript spawns nestedID via invoke_subagent
	writeTranscriptFixture(t, brainDir, sub1ID, []string{
		`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-10T09:00:04Z","content":"Fix PR 279 in worktree"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-10T09:00:06Z","input_tokens":10000,"cache_read_tokens":40000,"output_tokens":1000,"tool_calls":[{"id":"n1","name":"invoke_subagent","args":{"Role":"[quick] opencli: Test Runner","Model":"flash_lite","TypeName":"self","Prompt":"Run unit tests"}}]}`,
		fmt.Sprintf(`{"step_index":2,"type":"GENERIC","source":"SYSTEM","created_at":"2026-11-10T09:00:07Z","content":"Created the following subagent:\n{\n  \"conversationId\": \"%s\"\n}"}`, nestedID),
	})

	// 3. Nested subagent transcript
	writeTranscriptFixture(t, brainDir, nestedID, []string{
		`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-10T09:00:08Z","content":"Run unit tests"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-10T09:00:10Z","input_tokens":5000,"cache_read_tokens":15000,"output_tokens":500,"content":"Tests passed."}`,
	})

	idx, err := LoadOrRefreshIndex(vaultDir)
	if err != nil {
		t.Fatalf("LoadOrRefreshIndex: %v", err)
	}

	fdCM := idx.Conversations[fdID]
	sub1CM := idx.Conversations[sub1ID]
	nestedCM := idx.Conversations[nestedID]

	// Verify ground-truth attribution overrode the FIFO drift
	if sub1CM.Title != "[Subagent:opencli] PR 279 Fixer [standard]" {
		t.Errorf("sub1CM.Title = %q, want [Subagent:opencli] PR 279 Fixer [standard]", sub1CM.Title)
	}
	if sub1CM.ParentConversationID != fdID {
		t.Errorf("sub1CM.ParentConversationID = %q, want %q", sub1CM.ParentConversationID, fdID)
	}
	if nestedCM.Kind != "nested_subagent" {
		t.Errorf("nestedCM.Kind = %q, want nested_subagent", nestedCM.Kind)
	}
	if nestedCM.RootConversationID != fdID {
		t.Errorf("nestedCM.RootConversationID = %q, want %q", nestedCM.RootConversationID, fdID)
	}

	// Verify Tree rollups:
	// fd Self uncached = 30500, sub1 Self uncached = 11000, nested Self uncached = 5500
	if sub1CM.Self.UncachedTokens != 11000 {
		t.Errorf("sub1CM.Self.UncachedTokens = %d, want 11000", sub1CM.Self.UncachedTokens)
	}
	if sub1CM.Tree.UncachedTokens != 16500 {
		t.Errorf("sub1CM.Tree.UncachedTokens = %d, want 16500 (sub1 + nested)", sub1CM.Tree.UncachedTokens)
	}
	if fdCM.Tree.UncachedTokens != 47000 {
		t.Errorf("fdCM.Tree.UncachedTokens = %d, want 47000 (fd + sub1 + nested)", fdCM.Tree.UncachedTokens)
	}

	// Verify ReconcileSubagentAttribution repairs tasks.json on disk
	repaired, err := ReconcileSubagentAttribution(vaultDir)
	if err != nil {
		t.Fatalf("ReconcileSubagentAttribution: %v", err)
	}
	if repaired != 1 {
		t.Errorf("repaired = %d, want 1", repaired)
	}
	tasks, _ := kernel.ListTasks(vaultDir)
	if len(tasks) != 1 || tasks[0].Title != "[Subagent:opencli] PR 279 Fixer [standard]" {
		t.Errorf("tasks.json after repair = %+v", tasks)
	}
}

func TestAnomaliesBudgetCircuitBreakerAndFrontDeskNudge(t *testing.T) {
	tmp := t.TempDir()
	vaultDir := filepath.Join(tmp, "local")
	brainDir := filepath.Join(tmp, "brain")
	t.Setenv("HQ_BRAIN_DIR", brainDir)

	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	fdID := "dddddddd-0000-0000-0000-000000000001"
	subID := "eeeeeeee-0000-0000-0000-000000000002"

	_ = kernel.SetFrontDesk(vaultDir, fdID, filepath.Join(brainDir, fdID))
	_ = SaveBudgetConfig(vaultDir, BudgetConfig{
		DailyUncachedBudget:     1_000_000,
		DailyDeepUncachedBudget: 800_000,
		WarnContextTokens:       120_000,
		NudgeContextTokens:      160_000,
		CritContextTokens:       180_000,
		MaxTurnSteps:            60,
		MaxTurnUncachedTokens:   500_000,
		MaxTurnOutputTokens:     50_000,
	})

	// Front Desk with 190k context (triggers A1 CRITICAL + 160k SessionStart nudge) and 600k uncached turn (triggers A3)
	fdPath := writeTranscriptFixture(t, brainDir, fdID, []string{
		`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-10T10:00:00Z","content":"Run heavy turn"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-10T10:00:05Z","input_tokens":550000,"cache_read_tokens":0,"output_tokens":55000,"content":" Step 1"}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-10T10:00:10Z","input_tokens":190000,"cache_read_tokens":0,"output_tokens":5000,"content":"Done"}`,
	})

	// Deep subagent burning 350k uncached tokens (triggers A4 + pushes daily total over 1.0M budget -> triggers A5 CRITICAL)
	writeTranscriptFixture(t, brainDir, subID, []string{
		`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-10T11:00:00Z","content":"Subagent task"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-10T11:00:05Z","input_tokens":340000,"cache_read_tokens":100000,"output_tokens":10000,"content":"Subagent done"}`,
	})
	_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:             "subagent-general-heavy",
		Title:                "[Subagent:general] Heavy Worker [deep]",
		Kind:                 "subagent",
		Tier:                 "deep",
		Model:                "gemini-4-argon-high",
		ParentConversationID: fdID,
		ConversationID:       subID,
		Status:               "completed",
	})

	idx, err := LoadOrRefreshIndex(vaultDir)
	if err != nil {
		t.Fatalf("LoadOrRefreshIndex: %v", err)
	}
	cfg := LoadBudgetConfig(vaultDir)
	anomalies := EvaluateAnomalies(vaultDir, idx, cfg, now)

	codes := make(map[string]bool)
	for _, a := range anomalies {
		codes[a.Code] = true
	}
	for _, want := range []string{"A1", "A3", "A4", "A5"} {
		if !codes[want] {
			t.Errorf("expected anomaly code %s in %+v", want, anomalies)
		}
	}

	// Verify SessionStart Front Desk context nudge at >= 160k
	curCtx, _, nudge := GetFrontDeskContextStatus(vaultDir, fdPath)
	if curCtx != 190000 {
		t.Errorf("curCtx = %d, want 190000", curCtx)
	}
	if !strings.Contains(nudge, "/compact") {
		t.Errorf("expected /compact nudge banner, got %q", nudge)
	}

	// Verify DailyTokenBudgetGuard circuit breaker trips for kind:auto spawns
	tripped, reason := SyncDailyBudgetCircuitBreaker(vaultDir, now)
	if !tripped {
		t.Fatalf("expected SyncDailyBudgetCircuitBreaker to trip")
	}
	q := kernel.LoadQuota(vaultDir)
	if !q.CircuitOpen || !strings.Contains(q.CircuitReason, "budget reached") {
		t.Errorf("expected QuotaState circuit open with budget reason, got %+v (reason=%q)", q, reason)
	}

	// Verify ResetBudgetCircuitBreaker clears it
	if err := ResetBudgetCircuitBreaker(vaultDir); err != nil {
		t.Fatalf("ResetBudgetCircuitBreaker: %v", err)
	}
	qAfter := kernel.LoadQuota(vaultDir)
	if qAfter.CircuitOpen {
		t.Errorf("expected circuit breaker closed after reset, got %+v", qAfter)
	}
}
