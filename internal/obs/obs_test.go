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

func TestTokenCircuitDisabledSuppressesBreakerAndA5(t *testing.T) {
	tmp := t.TempDir()
	vaultDir := filepath.Join(tmp, "local")
	brainDir := filepath.Join(tmp, "brain")
	t.Setenv("HQ_BRAIN_DIR", brainDir)

	now := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	cid := "cccccccc-0000-0000-0000-000000000099"
	writeTranscriptFixture(t, brainDir, cid, []string{
		`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-19T12:00:00Z","content":"Run eval"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-19T12:00:05Z","input_tokens":2000000,"cache_read_tokens":0,"output_tokens":10000,"content":"Done"}`,
	})

	// 1. Enabled budget trips circuit and emits A5.
	if err := SaveBudgetConfig(vaultDir, BudgetConfig{DailyUncachedBudget: 1_000_000, DailyDeepUncachedBudget: 500_000}); err != nil {
		t.Fatalf("SaveBudgetConfig: %v", err)
	}
	if tripped, _ := SyncDailyBudgetCircuitBreaker(vaultDir, now); !tripped {
		t.Fatalf("expected enabled token budget to trip circuit breaker")
	}
	if !kernel.LoadQuota(vaultDir).CircuitOpen {
		t.Fatalf("expected QuotaState.CircuitOpen = true after trip")
	}

	// 2. Disabling token circuit persists across LoadBudgetConfig, auto-closes the budget circuit, and suppresses A5.
	cfg := LoadBudgetConfig(vaultDir)
	cfg.TokenCircuitDisabled = true
	if err := SaveBudgetConfig(vaultDir, cfg); err != nil {
		t.Fatalf("SaveBudgetConfig disabled: %v", err)
	}
	reloaded := LoadBudgetConfig(vaultDir)
	if !reloaded.TokenCircuitDisabled {
		t.Fatalf("expected TokenCircuitDisabled to persist in LoadBudgetConfig")
	}
	if tripped, reason := SyncDailyBudgetCircuitBreaker(vaultDir, now); tripped || reason != "" {
		t.Fatalf("expected disabled token circuit not to trip, got tripped=%v reason=%q", tripped, reason)
	}
	if q := kernel.LoadQuota(vaultDir); q.CircuitOpen {
		t.Fatalf("expected SyncDailyBudgetCircuitBreaker to auto-close budget circuit when disabled, got %+v", q)
	}
	sum := BuildDashboardSummary(vaultDir, now)
	if sum.DailyBudgetPct != 0 || sum.DeepBudgetPct != 0 {
		t.Fatalf("expected zero budget pct when disabled, got daily=%v deep=%v", sum.DailyBudgetPct, sum.DeepBudgetPct)
	}
	for _, a := range sum.Anomalies {
		if a.Code == "A5" {
			t.Fatalf("expected A5 daily budget anomaly suppressed when disabled, got %+v", a)
		}
	}
}

func TestEnrichAndBuildDAGUsesBatchNestedParents(t *testing.T) {
	vaultDir := t.TempDir()
	parentID := "eeee0001-0000-0000-0000-000000000001"
	childID := "eeee0001-0000-0000-0000-000000000002"

	if err := kernel.RecordNestedConversation(vaultDir, strings.ToUpper(childID), strings.ToUpper(parentID)); err != nil {
		t.Fatalf("RecordNestedConversation: %v", err)
	}

	idx := &ObsIndex{
		Version: currentObsIndexVersion,
		Conversations: map[string]ConversationMetrics{
			parentID: {
				ConversationID: parentID,
				Kind:           "task",
				Title:          "[Task:core] Parent",
				TaskSlug:       "core-parent",
				Turns: []TurnMetrics{
					{InputTokens: 1000, OutputTokens: 200, UncachedTokens: 1200, GrossTokens: 1200},
				},
			},
			childID: {
				ConversationID: childID,
				Turns: []TurnMetrics{
					{InputTokens: 500, OutputTokens: 100, UncachedTokens: 600, GrossTokens: 600},
				},
			},
		},
	}

	enrichAndBuildDAG(vaultDir, idx)

	child := idx.Conversations[childID]
	if child.Kind != "nested_subagent" {
		t.Fatalf("expected child Kind=nested_subagent, got %q", child.Kind)
	}
	if child.ParentConversationID != parentID {
		t.Fatalf("expected child ParentConversationID=%s, got %q", parentID, child.ParentConversationID)
	}
	parent := idx.Conversations[parentID]
	if parent.Tree.UncachedTokens != 1800 {
		t.Fatalf("expected parent Tree.UncachedTokens=1800 (1200+600), got %d", parent.Tree.UncachedTokens)
	}
}

func TestSummaryCacheHitExpiryAndBudgetRefresh(t *testing.T) {
	vaultDir := t.TempDir()
	brainDir := filepath.Join(t.TempDir(), "brain")
	now := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)

	if err := SaveBudgetConfig(vaultDir, BudgetConfig{
		DailyUncachedBudget:     2_000_000,
		DailyDeepUncachedBudget: 1_000_000,
	}); err != nil {
		t.Fatalf("SaveBudgetConfig: %v", err)
	}

	seed := DashboardSummary{
		UpdatedAt:               now.Format("15:04:05"),
		TodayUncachedTokens:     500_000,
		TodayDeepUncachedTokens: 250_000,
		FrontDeskContextStatus:  "OK",
		TaskBadges: map[string]TaskTokenBadge{
			"conv-1": {ConversationID: "conv-1", TreeUncachedTokens: 500_000},
		},
	}
	saveSummaryCache(vaultDir, brainDir, now, seed)

	// 1. Fresh hit recomputes budget percentages against current budget config.
	got, ok := loadSummaryCache(vaultDir, brainDir, now.Add(30*time.Second), time.Minute)
	if !ok {
		t.Fatalf("expected fresh summary cache hit")
	}
	if got.TodayUncachedTokens != 500_000 || got.DailyBudgetPct != 25.0 || got.DeepBudgetPct != 25.0 {
		t.Fatalf("unexpected cached summary values: %+v", got)
	}

	// 2. Updating budget config immediately reflects on next cache hit.
	if err := SaveBudgetConfig(vaultDir, BudgetConfig{
		DailyUncachedBudget:     1_000_000,
		DailyDeepUncachedBudget: 500_000,
	}); err != nil {
		t.Fatalf("SaveBudgetConfig update: %v", err)
	}
	got2, ok := loadSummaryCache(vaultDir, brainDir, now.Add(45*time.Second), time.Minute)
	if !ok || got2.DailyBudgetPct != 50.0 || got2.DeepBudgetPct != 50.0 {
		t.Fatalf("expected updated budget pct (50/50) on cache hit, got ok=%v daily=%v deep=%v", ok, got2.DailyBudgetPct, got2.DeepBudgetPct)
	}

	// 3. Expired TTL, mismatched brainDir, and date rollover all miss cleanly.
	if _, ok := loadSummaryCache(vaultDir, brainDir, now.Add(2*time.Minute), time.Minute); ok {
		t.Fatalf("expected TTL-expired summary cache miss")
	}
	if _, ok := loadSummaryCache(vaultDir, brainDir+"-other", now.Add(10*time.Second), time.Minute); ok {
		t.Fatalf("expected mismatched brainDir cache miss")
	}
	if _, ok := loadSummaryCache(vaultDir, brainDir, now.Add(24*time.Hour), 48*time.Hour); ok {
		t.Fatalf("expected date-rollover cache miss")
	}
}

func TestPostCheckpointWatermarkAndRecovery(t *testing.T) {
	tmp := t.TempDir()
	vaultDir := filepath.Join(tmp, "local")
	brainDir := filepath.Join(tmp, "brain")
	t.Setenv("HQ_BRAIN_DIR", brainDir)

	fdID := "dddd0001-0000-0000-0000-000000000001"
	fdArtDir := filepath.Join(brainDir, fdID)
	if err := kernel.SetFrontDesk(vaultDir, fdID, fdArtDir); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	// 1. Pre-compaction Front Desk at 205k tokens -> CRITICAL + A1 anomaly + /compact nudge.
	preLines := []string{
		`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-22T09:50:00Z","content":"Status?"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-22T09:50:03Z","input_tokens":20000,"cache_read_tokens":185000,"output_tokens":400,"content":"All green."}`,
	}
	fdPath := writeTranscriptFixture(t, brainDir, fdID, preLines)

	sum1 := BuildDashboardSummary(vaultDir, now)
	if sum1.FrontDeskContextTokens != 205000 || sum1.FrontDeskContextStatus != "CRITICAL" || sum1.FrontDeskCompactedPending {
		t.Fatalf("pre-compaction summary = (tokens=%d status=%s compacted=%v), want (205000 CRITICAL false)",
			sum1.FrontDeskContextTokens, sum1.FrontDeskContextStatus, sum1.FrontDeskCompactedPending)
	}
	hasA1 := false
	for _, a := range sum1.Anomalies {
		if a.Code == "A1" {
			hasA1 = true
		}
	}
	if !hasA1 {
		t.Fatalf("expected A1 FrontDeskContextBloat anomaly pre-compaction, got %+v", sum1.Anomalies)
	}
	if curTok, _, nudge := GetFrontDeskContextStatus(vaultDir, fdPath); curTok != 205000 || nudge == "" {
		t.Fatalf("expected pre-compaction nudge at 205k, got curTok=%d nudge=%q", curTok, nudge)
	}

	// 2. Append /compact CHECKPOINT with no subsequent PLANNER_RESPONSE yet.
	postCompactLines := append(append([]string{}, preLines...),
		`{"step_index":2,"type":"CHECKPOINT","source":"SYSTEM","created_at":"2026-11-22T09:55:00Z","input_tokens":10000,"cache_read_tokens":200000,"output_tokens":1800}`,
	)
	writeTranscriptFixture(t, brainDir, fdID, postCompactLines)

	cm, err := ParseTranscriptFile(fdID, fdPath, "gemini-4-hydrogen-medium", "standard")
	if err != nil {
		t.Fatalf("ParseTranscriptFile post-compact: %v", err)
	}
	if cm.CurrentContextTokens != 0 || !cm.CompactedPendingNextTurn {
		t.Fatalf("post-checkpoint cm = (cur=%d compacted=%v), want (0 true)", cm.CurrentContextTokens, cm.CompactedPendingNextTurn)
	}
	if cm.Self.PeakContextTokens != 210000 || cm.Self.CompactionCount != 1 {
		t.Fatalf("post-checkpoint peak/compactions = (%d, %d), want (210000, 1)", cm.Self.PeakContextTokens, cm.Self.CompactionCount)
	}
	if len(cm.Turns) != 1 || !cm.Turns[0].CompactedPendingNextTurn || cm.Turns[0].CurrentContextTokens != 0 || cm.Turns[0].PeakContextTokens != 210000 {
		t.Fatalf("unexpected post-checkpoint turn metrics: %+v", cm.Turns)
	}

	sliceTM := ParseSingleTurnSlice(fdPath, 0)
	if sliceTM.CurrentContextTokens != 0 || !sliceTM.CompactedPendingNextTurn || sliceTM.PeakContextTokens != 210000 || sliceTM.CompactionCount != 1 {
		t.Fatalf("unexpected ParseSingleTurnSlice post-checkpoint: %+v", sliceTM)
	}

	sum2 := BuildDashboardSummary(vaultDir, now)
	if sum2.FrontDeskContextTokens != 0 || !sum2.FrontDeskCompactedPending || sum2.FrontDeskContextStatus != "OK" {
		t.Fatalf("post-compaction summary = (tokens=%d compacted=%v status=%s), want (0 true OK)",
			sum2.FrontDeskContextTokens, sum2.FrontDeskCompactedPending, sum2.FrontDeskContextStatus)
	}
	if sum2.FrontDeskPeakTokens != 210000 || sum2.FrontDeskCompactions != 1 {
		t.Fatalf("post-compaction peak/compactions = (%d, %d), want (210000, 1)", sum2.FrontDeskPeakTokens, sum2.FrontDeskCompactions)
	}
	for _, a := range sum2.Anomalies {
		if a.Code == "A1" {
			t.Fatalf("expected no A1 anomaly right after CHECKPOINT, got %+v", a)
		}
	}
	if curTok, _, nudge := GetFrontDeskContextStatus(vaultDir, fdPath); curTok != 0 || nudge != "" {
		t.Fatalf("expected no /compact nudge right after CHECKPOINT, got curTok=%d nudge=%q", curTok, nudge)
	}

	// 3. Recovery on next user turn's PLANNER_RESPONSE (42k context).
	recoveredLines := append(append([]string{}, postCompactLines...),
		`{"step_index":3,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-22T09:58:00Z","content":"Next task"}`,
		`{"step_index":4,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-22T09:58:02Z","input_tokens":12000,"cache_read_tokens":30000,"output_tokens":300,"content":"Ready."}`,
	)
	writeTranscriptFixture(t, brainDir, fdID, recoveredLines)

	sum3 := BuildDashboardSummary(vaultDir, now)
	if sum3.FrontDeskContextTokens != 42000 || sum3.FrontDeskCompactedPending || sum3.FrontDeskContextStatus != "OK" {
		t.Fatalf("recovered summary = (tokens=%d compacted=%v status=%s), want (42000 false OK)",
			sum3.FrontDeskContextTokens, sum3.FrontDeskCompactedPending, sum3.FrontDeskContextStatus)
	}
	if sum3.FrontDeskPeakTokens != 210000 || sum3.FrontDeskCompactions != 1 {
		t.Fatalf("recovered peak/compactions = (%d, %d), want (210000, 1)", sum3.FrontDeskPeakTokens, sum3.FrontDeskCompactions)
	}
}

func TestSummaryCacheInvalidatesOnFrontDeskTranscriptChange(t *testing.T) {
	tmp := t.TempDir()
	vaultDir := filepath.Join(tmp, "local")
	brainDir := filepath.Join(tmp, "brain")
	t.Setenv("HQ_BRAIN_DIR", brainDir)

	fdID := "dddd0002-0000-0000-0000-000000000002"
	if err := kernel.SetFrontDesk(vaultDir, fdID, filepath.Join(brainDir, fdID)); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	preLines := []string{
		`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-22T09:50:00Z","content":"Hi"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-22T09:50:02Z","input_tokens":25000,"cache_read_tokens":180000,"output_tokens":200,"content":"Hello"}`,
	}
	writeTranscriptFixture(t, brainDir, fdID, preLines)

	sum1 := BuildDashboardSummary(vaultDir, now)
	if sum1.FrontDeskContextStatus != "CRITICAL" {
		t.Fatalf("expected CRITICAL pre-compact, got %s", sum1.FrontDeskContextStatus)
	}
	if got, ok := loadSummaryCache(vaultDir, brainDir, now.Add(10*time.Second), 5*time.Minute); !ok || got.FrontDeskContextStatus != "CRITICAL" {
		t.Fatalf("expected fresh summary cache hit before transcript change, got ok=%v status=%s", ok, got.FrontDeskContextStatus)
	}

	// Append CHECKPOINT to Front Desk transcript -> cache MUST miss immediately even 15s later (inside 5m TTL).
	postLines := append(append([]string{}, preLines...),
		`{"step_index":2,"type":"CHECKPOINT","source":"SYSTEM","created_at":"2026-11-22T09:55:00Z","input_tokens":5000,"cache_read_tokens":200000,"output_tokens":1500}`,
	)
	writeTranscriptFixture(t, brainDir, fdID, postLines)

	if _, ok := loadSummaryCache(vaultDir, brainDir, now.Add(15*time.Second), 5*time.Minute); ok {
		t.Fatalf("expected summary cache miss immediately after Front Desk transcript CHECKPOINT change")
	}

	// Rebuilding summary updates cache stamp; subsequent load hits with OK + compacted state.
	sum2 := BuildDashboardSummary(vaultDir, now.Add(15*time.Second))
	if !sum2.FrontDeskCompactedPending || sum2.FrontDeskContextStatus != "OK" {
		t.Fatalf("expected OK compacted summary after rebuild, got %+v", sum2)
	}
	if got2, ok := loadSummaryCache(vaultDir, brainDir, now.Add(20*time.Second), 5*time.Minute); !ok || !got2.FrontDeskCompactedPending || got2.FrontDeskContextStatus != "OK" {
		t.Fatalf("expected fresh cache hit with compacted state, got ok=%v compacted=%v status=%s", ok, got2.FrontDeskCompactedPending, got2.FrontDeskContextStatus)
	}

	// Explicit InvalidateSummaryCache forces miss immediately.
	InvalidateSummaryCache(vaultDir)
	if _, ok := loadSummaryCache(vaultDir, brainDir, now.Add(21*time.Second), 5*time.Minute); ok {
		t.Fatalf("expected cache miss after InvalidateSummaryCache")
	}
}

func TestBuildSummaryReportPerRunMeanAndMedian(t *testing.T) {
	tmp := t.TempDir()
	vaultDir := filepath.Join(tmp, "local")
	brainDir := filepath.Join(tmp, "brain")
	t.Setenv("HQ_BRAIN_DIR", brainDir)

	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	// 5 [Auto] runs: 4 routine runs (10k, 20k, 25k, 30k uncached) + 1 heavy nightly run (265k uncached)
	// Total uncached = 350k -> Mean = 70k, Median (p50) = 25k.
	runs := []struct {
		id     string
		input  int64
		cache  int64
		output int64
	}{
		{"22222222-2222-4222-8222-222222222201", 8000, 10000, 2000},     // 10k uncached, 20k gross
		{"22222222-2222-4222-8222-222222222202", 16000, 20000, 4000},    // 20k uncached, 40k gross
		{"22222222-2222-4222-8222-222222222203", 20000, 25000, 5000},    // 25k uncached, 50k gross
		{"22222222-2222-4222-8222-222222222204", 24000, 30000, 6000},    // 30k uncached, 60k gross
		{"22222222-2222-4222-8222-222222222205", 200000, 400000, 65000}, // 265k uncached, 665k gross
	}
	for i, r := range runs {
		_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
			TaskSlug:       fmt.Sprintf("auto-run-%d", i+1),
			Title:          fmt.Sprintf("[Auto] Worker %d", i+1),
			Kind:           "auto",
			Scope:          "ops",
			Tier:           "standard",
			ConversationID: r.id,
			Status:         "done",
		})
		writeTranscriptFixture(t, brainDir, r.id, []string{
			`{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","created_at":"2026-11-24T08:00:00Z","content":"run auto job"}`,
			fmt.Sprintf(`{"step_index":1,"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-11-24T08:00:05Z","input_tokens":%d,"cache_read_tokens":%d,"output_tokens":%d,"content":"done"}`, r.input, r.cache, r.output),
		})
	}

	rep, err := BuildSummaryReport(vaultDir, "7d", "kind", now)
	if err != nil {
		t.Fatalf("BuildSummaryReport: %v", err)
	}
	var autoBucket *BucketMetrics
	for i := range rep.Buckets {
		if rep.Buckets[i].Key == "auto" {
			autoBucket = &rep.Buckets[i]
			break
		}
	}
	if autoBucket == nil {
		t.Fatalf("expected 'auto' bucket in %+v", rep.Buckets)
	}
	if autoBucket.ConvCount != 5 {
		t.Fatalf("expected ConvCount=5, got %d", autoBucket.ConvCount)
	}
	if autoBucket.AvgUncachedPerRun != 70000 {
		t.Fatalf("expected AvgUncachedPerRun=70000, got %d", autoBucket.AvgUncachedPerRun)
	}
	if autoBucket.P50UncachedPerRun != 25000 {
		t.Fatalf("expected P50UncachedPerRun=25000, got %d", autoBucket.P50UncachedPerRun)
	}
	if autoBucket.AvgGrossPerRun != 167000 {
		t.Fatalf("expected AvgGrossPerRun=167000, got %d", autoBucket.AvgGrossPerRun)
	}
	if autoBucket.P50GrossPerRun != 50000 {
		t.Fatalf("expected P50GrossPerRun=50000, got %d", autoBucket.P50GrossPerRun)
	}
}
