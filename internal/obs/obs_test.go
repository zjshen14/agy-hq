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

	cm, err := ParseTranscriptFile(convID, tPath, "gemini-4-carbon-high", "deep")
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
		Model:          "gemini-4-hydrogen-high",
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
		Model:                "gemini-4-carbon-high",
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

func TestIndexSkipRedundantRewriteAndDashboardSummaryBounds(t *testing.T) {
	tmp := t.TempDir()
	vaultDir := filepath.Join(tmp, "local")
	brainDir := filepath.Join(tmp, "brain")
	t.Setenv("HQ_BRAIN_DIR", brainDir)

	fdID := "ffffffff-0000-0000-0000-000000000001"
	_ = kernel.SetFrontDesk(vaultDir, fdID, filepath.Join(brainDir, fdID))
	writeTranscriptFixture(t, brainDir, fdID, []string{
		`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-12T08:00:00Z","content":"Status"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-12T08:00:02Z","input_tokens":10000,"cache_read_tokens":20000,"output_tokens":300,"content":"OK"}`,
	})

	// Seed 70 historical conversations, 30 of which exceed WarnContextTokens so >MaxDashboardAnomalies anomalies exist.
	for i := 0; i < 70; i++ {
		cid := fmt.Sprintf("10000000-0000-0000-0000-%012d", i+1)
		inTok := 15000
		if i < 30 {
			inTok = 600000 // > default 500k MaxTurnUncachedTokens -> A3 anomaly
		}
		writeTranscriptFixture(t, brainDir, cid, []string{
			`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-12T09:00:00Z","content":"Work item"}`,
			fmt.Sprintf(`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-12T09:00:02Z","input_tokens":%d,"cache_read_tokens":0,"output_tokens":200,"content":"Done"}`, inTok),
		})
		if i < 2 {
			_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
				TaskSlug:       fmt.Sprintf("hot-task-%d", i+1),
				Title:          fmt.Sprintf("[Task:core] Hot Task %d", i+1),
				Kind:           "task",
				ConversationID: cid,
				Status:         "active",
			})
		}
	}

	if _, err := LoadOrRefreshIndex(vaultDir); err != nil {
		t.Fatalf("first LoadOrRefreshIndex: %v", err)
	}
	idxPath := filepath.Join(vaultDir, "memory", "obs", "index.json")
	st1, err := os.Stat(idxPath)
	if err != nil {
		t.Fatalf("stat index.json: %v", err)
	}

	time.Sleep(15 * time.Millisecond)
	if _, err := LoadOrRefreshIndex(vaultDir); err != nil {
		t.Fatalf("second LoadOrRefreshIndex: %v", err)
	}
	st2, err := os.Stat(idxPath)
	if err != nil {
		t.Fatalf("stat index.json second time: %v", err)
	}
	if st2.ModTime().UnixNano() != st1.ModTime().UnixNano() {
		t.Fatalf("expected unchanged transcripts/metadata NOT to rewrite obs/index.json (mtime1=%d mtime2=%d)", st1.ModTime().UnixNano(), st2.ModTime().UnixNano())
	}

	sum := BuildDashboardSummary(vaultDir, time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC))
	if len(sum.Anomalies) != MaxDashboardAnomalies {
		t.Fatalf("expected Anomalies capped at %d, got %d", MaxDashboardAnomalies, len(sum.Anomalies))
	}
	if sum.TotalAnomalies < 30 {
		t.Fatalf("expected TotalAnomalies >= 30, got %d", sum.TotalAnomalies)
	}
	if len(sum.TaskBadges) > 5 {
		t.Fatalf("expected TaskBadges scoped to hot tasks + Front Desk (<=5), got %d", len(sum.TaskBadges))
	}
}

func TestParseTranscriptStreamingMultiTurn(t *testing.T) {
	brainDir := t.TempDir()
	var lines []string
	for turn := 0; turn < 50; turn++ {
		tsUser := time.Date(2026, 11, 17, 10, turn, 0, 0, time.UTC).Format(time.RFC3339)
		tsPlan := time.Date(2026, 11, 17, 10, turn, 2, 0, time.UTC).Format(time.RFC3339)
		lines = append(lines,
			fmt.Sprintf(`{"step_index":%d,"type":"USER_INPUT","source":"USER_EXPLICIT","status":"DONE","created_at":%q,"content":"turn %d prompt"}`, turn*2, tsUser, turn),
			fmt.Sprintf(`{"step_index":%d,"type":"PLANNER_RESPONSE","source":"MODEL","status":"DONE","created_at":%q,"token_usage":{"input_tokens":100,"cache_read_tokens":400,"output_tokens":50},"tool_calls":[{"id":"c1","name":"read_file","args":{"Path":"/tmp/a"}}]}`, turn*2+1, tsPlan),
		)
	}
	tPath := writeTranscriptFixture(t, brainDir, "stream-conv-1", lines)
	cm, err := ParseTranscriptFile("stream-conv-1", tPath, "gemini-4-hydrogen-high", "standard")
	if err != nil {
		t.Fatalf("ParseTranscriptFile: %v", err)
	}
	if len(cm.Turns) != 50 {
		t.Fatalf("expected 50 turns, got %d", len(cm.Turns))
	}
	if cm.Self.StepCount != 100 || cm.Self.PlannerSteps != 50 {
		t.Fatalf("unexpected step/planner counts: %+v", cm.Self)
	}
	if cm.Self.InputTokens != 5000 || cm.Self.CacheReadTokens != 20000 || cm.Self.OutputTokens != 2500 {
		t.Fatalf("unexpected token rollup: %+v", cm.Self)
	}
	if cm.CurrentContextTokens != 500 {
		t.Fatalf("expected CurrentContextTokens=500, got %d", cm.CurrentContextTokens)
	}
}

func TestParseWorkflowAndAgentAPIEdgesAndReconcileCleanup(t *testing.T) {
	tmp := t.TempDir()
	vaultDir := filepath.Join(tmp, "local")
	brainDir := filepath.Join(tmp, "brain")
	t.Setenv("HQ_BRAIN_DIR", brainDir)

	fdID := "11111111-1111-1111-1111-111111111101"
	taskID := "11111111-1111-1111-1111-111111111102"
	directSubID := "11111111-1111-1111-1111-111111111103"
	nestedSubID := "11111111-1111-1111-1111-111111111104"
	wfChild1 := "11111111-1111-1111-1111-111111111105"
	wfChild2 := "11111111-1111-1111-1111-111111111106"
	agentapiChild := "11111111-1111-1111-1111-111111111107"
	hqSpawnTask := "11111111-1111-1111-1111-111111111108"

	_ = kernel.SetFrontDesk(vaultDir, fdID, filepath.Join(brainDir, fdID))

	// Write truncated run_workflow output file in sibling steps/2/output.txt for taskID
	wfOutPath := filepath.Join(brainDir, taskID, ".system_generated", "steps", "2", "output.txt")
	if err := os.MkdirAll(filepath.Dir(wfOutPath), 0o755); err != nil {
		t.Fatalf("mkdir steps dir: %v", err)
	}
	_ = os.WriteFile(wfOutPath, []byte(fmt.Sprintf(`{"results":[{"conversation_id":%q},{"conversationId":%q}]}`, wfChild1, wfChild2)), 0o644)

	// Front Desk transcript spawns directSubID via invoke_subagent, and spawns hqSpawnTask via `hq spawn` (must NOT emit agentapi edge).
	writeTranscriptFixture(t, brainDir, fdID, []string{
		`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-18T10:00:00Z","content":"Run direct subagent"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-18T10:00:01Z","tool_calls":[{"id":"s1","name":"invoke_subagent","args":{"Role":"[standard] core: Direct Repair","Model":"flash","Prompt":"Do direct work"}}]}`,
		fmt.Sprintf(`{"step_index":2,"type":"GENERIC","source":"SYSTEM","created_at":"2026-11-18T10:00:02Z","content":"Created the following subagent:\n{\n  \"conversationId\": \"%s\"\n}"}`, directSubID),
		`{"step_index":3,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-18T10:00:03Z","tool_calls":[{"id":"h1","name":"run_command","args":{"CommandLine":"hq spawn --task=top-task --prompt=hello"}}]}`,
		fmt.Sprintf(`{"step_index":4,"type":"GENERIC","source":"SYSTEM","created_at":"2026-11-18T10:00:04Z","content":"{\"conversationId\":\"%s\"}"}`, hqSpawnTask),
	})

	// Secondary task transcript spawns nestedSubID (invoke_subagent), wfChild1/wfChild2 (async run_workflow), and agentapiChild (raw agentapi new-conversation).
	writeTranscriptFixture(t, brainDir, taskID, []string{
		`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-18T10:01:00Z","content":"Fan out work"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-18T10:01:01Z","tool_calls":[{"id":"n1","name":"invoke_subagent","args":{"Role":"[quick] core: Nested Editor","Model":"flash_lite","Prompt":"Edit file"}}]}`,
		fmt.Sprintf(`{"step_index":2,"type":"GENERIC","source":"SYSTEM","created_at":"2026-11-18T10:01:02Z","content":"Created the following subagent:\n{\n  \"conversationId\": \"%s\"\n}"}`, nestedSubID),
		`{"step_index":3,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-18T10:01:03Z","tool_calls":[{"id":"w1","name":"run_workflow","args":{"Description":"Fanout batch","Script":"parallel(...)"}}]}`,
		fmt.Sprintf(`{"step_index":4,"type":"GENERIC","source":"SYSTEM","created_at":"2026-11-18T10:01:04Z","content":"Tool is running as a background task with task id: %s/task-1"}`, taskID),
		fmt.Sprintf(`{"step_index":5,"type":"SYSTEM_MESSAGE","source":"SYSTEM","created_at":"2026-11-18T10:01:10Z","content":"Task id \"%s/task-1\" finished with result: Workflow \"Fanout batch\" completed. Output saved to file:///tmp/.system_generated/steps/2/output.txt"}`, taskID),
		`{"step_index":6,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-18T10:01:11Z","tool_calls":[{"id":"a1","name":"run_command","args":{"CommandLine":"agentapi new-conversation --json \"helper task\""}}]}`,
		fmt.Sprintf(`{"step_index":7,"type":"GENERIC","source":"SYSTEM","created_at":"2026-11-18T10:01:12Z","content":"Command exited with code 0.\n{\"conversationId\":\"%s\"}"}`, agentapiChild),
	})

	for _, cid := range []string{directSubID, nestedSubID, wfChild1, wfChild2, agentapiChild, hqSpawnTask} {
		writeTranscriptFixture(t, brainDir, cid, []string{
			`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-18T10:02:00Z","content":"worker step"}`,
			`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-18T10:02:01Z","input_tokens":100,"cache_read_tokens":0,"output_tokens":50,"content":"done"}`,
		})
	}

	// Register leaked Kind:"task" entries in tasks.json
	_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:       "core-main-task",
		Title:          "[Task:core] Main Task",
		Kind:           "task",
		ConversationID: taskID,
		Status:         "active",
	})
	_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:       "core-hq-spawn-task",
		Title:          "[Task:core] Spawned Task",
		Kind:           "task",
		ConversationID: hqSpawnTask,
		Status:         "active",
	})
	// Direct FD subagent leaked as Kind:"task" with wrong conv-* slug
	_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:       "conv-11111103",
		Title:          "[Task:general] Auto-adopted Direct Subagent",
		Kind:           "task",
		ConversationID: directSubID,
		Status:         "completed",
	})
	// Nested invoke_subagent leaked as Kind:"task" even after earlier attribution repaired slug/parent
	_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:             "subagent-core-nested-editor",
		Title:                "[Subagent:core] Nested Editor [quick]",
		Kind:                 "task",
		ParentConversationID: taskID,
		ConversationID:       nestedSubID,
		Status:               "completed",
	})
	// run_workflow & agentapi children leaked as Kind:"task"
	_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:       "conv-11111105",
		Title:          "Workflow step worker 1",
		Kind:           "task",
		ConversationID: wfChild1,
		Status:         "idle",
	})
	_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:       "conv-11111107",
		Title:          "helper task",
		Kind:           "task",
		ConversationID: agentapiChild,
		Status:         "idle",
	})

	// Check DAG classification first
	idx, err := LoadOrRefreshIndex(vaultDir)
	if err != nil {
		t.Fatalf("LoadOrRefreshIndex: %v", err)
	}
	if idx.Conversations[wfChild1].Kind != "nested_subagent" || idx.Conversations[wfChild2].Kind != "nested_subagent" {
		t.Fatalf("expected workflow children to classify as nested_subagent, got %q / %q", idx.Conversations[wfChild1].Kind, idx.Conversations[wfChild2].Kind)
	}
	if idx.Conversations[agentapiChild].Kind != "nested_subagent" {
		t.Fatalf("expected agentapiChild to classify as nested_subagent, got %q", idx.Conversations[agentapiChild].Kind)
	}
	if idx.Conversations[hqSpawnTask].ParentConversationID != "" {
		t.Fatalf("expected hq spawn to NOT create an agentapi spawn edge, got parent %q", idx.Conversations[hqSpawnTask].ParentConversationID)
	}

	repaired, err := ReconcileSubagentAttribution(vaultDir)
	if err != nil {
		t.Fatalf("ReconcileSubagentAttribution: %v", err)
	}
	if repaired != 4 {
		t.Fatalf("expected 4 repaired/pruned tasks (1 direct Kind=subagent + 3 nested/workflow/agentapi removed), got %d", repaired)
	}

	remaining, _ := kernel.ListTasks(vaultDir)
	if len(remaining) != 3 {
		t.Fatalf("expected 3 tasks left (taskID, hqSpawnTask, directSubID), got %d: %+v", len(remaining), remaining)
	}
	for _, r := range remaining {
		if r.ConversationID == directSubID {
			if r.Kind != "subagent" || r.ParentConversationID != fdID || r.Title != "[Subagent:core] Direct Repair [standard]" {
				t.Fatalf("expected directSubID repaired to Kind=subagent under fdID, got %+v", r)
			}
		}
		if r.ConversationID == nestedSubID || r.ConversationID == wfChild1 || r.ConversationID == agentapiChild {
			t.Fatalf("nested/workflow/agentapi leak survived ReconcileSubagentAttribution: %+v", r)
		}
	}
	for _, nid := range []string{nestedSubID, wfChild1, agentapiChild} {
		if !kernel.IsNestedSubagentConversation(vaultDir, nid) {
			t.Fatalf("expected %s recorded in nested_conversations.json", nid)
		}
	}

	// Explicit [Task:...] created via `agentapi new-conversation --title="[Task:...]` or adopted with an FD ParentConversationID
	// (including across Front Desk rotation) must NOT emit an agentapi edge and must survive Reconcile + CleanNestedTasks.
	if IsRawAgentAPINewConversationCommand(`env -u ANTIGRAVITY_SOURCE_METADATA agentapi new-conversation --model=auto --title="[Task:core] Delegated Feature" "handoff prompt"`) {
		t.Fatalf("expected explicit --title=\"[Task:...]\" agentapi new-conversation to NOT be classified as hidden worker")
	}
	if !IsRawAgentAPINewConversationCommand(`agentapi new-conversation --json "background helper worker"`) {
		t.Fatalf("expected raw unnamed agentapi new-conversation to be classified as hidden worker")
	}
	forkedTaskID := "11111111-1111-1111-1111-111111111109"
	oldFD := "11111111-1111-1111-1111-111111111199"
	_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:             "core-delegated-feature",
		Title:                "[Task:core] Delegated Feature",
		Kind:                 "task",
		ParentConversationID: oldFD,
		ConversationID:       forkedTaskID,
		Status:               "active",
	})
	if cleaned, cErr := kernel.CleanNestedTasks(vaultDir); cErr != nil || len(cleaned) != 0 {
		t.Fatalf("expected CleanNestedTasks to preserve explicit [Task:...] across rotated FD, got err=%v cleaned=%v", cErr, cleaned)
	}
	if layer1 := kernel.FilterLayer1Tasks(vaultDir, []kernel.ActiveTask{{
		TaskSlug:             "core-delegated-feature",
		Title:                "[Task:core] Delegated Feature",
		Kind:                 "task",
		ParentConversationID: oldFD,
		ConversationID:       forkedTaskID,
		Status:               "active",
	}}); len(layer1) != 1 {
		t.Fatalf("expected explicit [Task:...] to remain Layer-1 across Front Desk rotation, got %+v", layer1)
	}

	// Quote-aware command matching & per-turn pending cleanup:
	// 1. Quoted commit/echo strings mentioning agentapi new-conversation must NOT match.
	// 2. An unconsumed agentapi command in turn 1 must NOT hijack hq tasks --json in turn 2.
	for _, nonWorkerCmd := range []string{
		`git commit -m "fix agentapi new-conversation bug"`,
		`echo 'agentapi new-conversation --json "hi"'`,
		`env -u VAR agentapi new-conversation --title="[Task:core] Explicit" "prompt"`,
		`agentapi new-conversation "worker" && ./bin/hq adopt --conv=123 --task=foo`,
	} {
		if IsRawAgentAPINewConversationCommand(nonWorkerCmd) {
			t.Fatalf("expected non-worker command %q to return false", nonWorkerCmd)
		}
	}
	isolatedParent := "22222222-1111-1111-1111-111111111101"
	innocentTask := "22222222-1111-1111-1111-111111111102"
	isoPath := writeTranscriptFixture(t, brainDir, isolatedParent, []string{
		`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","content":"Turn 1 denied"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","tool_calls":[{"id":"c1","name":"run_command","args":{"CommandLine":"agentapi new-conversation \"denied\""}}]}`,
		`{"step_index":2,"type":"GENERIC","source":"SYSTEM","status":"ERROR","content":"tool call denied"}`,
		`{"step_index":3,"type":"USER_INPUT","source":"USER_EXPLICIT","content":"Turn 2 list tasks"}`,
		`{"step_index":4,"type":"PLANNER_RESPONSE","source":"MODEL","tool_calls":[{"id":"c2","name":"run_command","args":{"CommandLine":"hq tasks --json"}}]}`,
		fmt.Sprintf(`{"step_index":5,"type":"GENERIC","source":"SYSTEM","content":"Command exited with code 0.\n[{\"conversationId\":\"%s\",\"taskSlug\":\"innocent\"}]"}`, innocentTask),
	})
	isoCM, err := ParseTranscriptFile(isolatedParent, isoPath, "", "")
	if err != nil {
		t.Fatalf("ParseTranscriptFile isolatedParent: %v", err)
	}
	if len(isoCM.SpawnEdges) != 0 {
		t.Fatalf("expected unconsumed turn-1 agentapi call NOT to hijack turn-2 hq tasks --json, got %+v", isoCM.SpawnEdges)
	}
}
