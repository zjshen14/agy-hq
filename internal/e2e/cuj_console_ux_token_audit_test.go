package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/webui"
)

// CUJ-48: 4-Tab Console UX, Per-Turn Multi-Model Token Calibration, Disjoint Tree Attribution,
// 512k Context Ceiling & Dual-Artifact Front Desk Lifecycle (RFC-0030).
func TestCUJ48_ConsoleUXTokenCalibrationAndDualArtifactSync(t *testing.T) {
	env := setupHQEnv(t)
	mustRunV3(t, env, "init")

	artDesk1 := filepath.Join(t.TempDir(), "brain-fd-1")
	if err := os.MkdirAll(artDesk1, 0o755); err != nil {
		t.Fatal(err)
	}
	deskConv1 := "conv-fd-cuj48-1"

	// 1. Vacant Front Desk auto-claim emits single {{HQ_SYNC}} write_file instruction (0 read_file steps)
	// and retires any legacy hq_console_live.html file left over from older builds.
	_ = os.WriteFile(filepath.Join(artDesk1, "hq_console_live.html"), []byte("<html>legacy</html>"), 0o644)
	_ = os.WriteFile(filepath.Join(artDesk1, "hq_console_live.html.metadata.json"), []byte(`{"userFacing":true}`), 0o644)

	ssPayload1 := fmt.Sprintf(`{"conversationId":%q,"artifactDirectoryPath":%q}`, deskConv1, artDesk1)
	ssOut1, err := env.run(ssPayload1, "hook", "session-start")
	if err != nil {
		t.Fatalf("session-start 1 failed: %v\n%s", err, ssOut1)
	}
	if !strings.Contains(ssOut1, "New [HQ] Front Desk Initialization Required") ||
		!strings.Contains(ssOut1, "{{HQ_SYNC}}") ||
		!strings.Contains(ssOut1, "hq_console.md") ||
		strings.Contains(ssOut1, "hq_console_live.html") {
		t.Fatalf("expected single-artifact {{HQ_SYNC}} initialization instructions in SessionStart, got:\n%s", ssOut1)
	}
	for _, fn := range []string{"hq_console.md", "hq_console.md.metadata.json"} {
		st, stErr := os.Stat(filepath.Join(artDesk1, fn))
		if stErr != nil {
			t.Fatalf("expected %s to be synced into artDesk1 on SessionStart: %v", fn, stErr)
		}
		if st.Mode().Perm() != 0o644 {
			t.Fatalf("expected 0644 permissions on %s, got %04o", fn, st.Mode().Perm())
		}
	}
	for _, legacyFn := range []string{"hq_console_live.html", "hq_console_live.html.metadata.json"} {
		if _, stErr := os.Stat(filepath.Join(artDesk1, legacyFn)); !os.IsNotExist(stErr) {
			t.Fatalf("expected legacy %s to be retired on SessionStart sync", legacyFn)
		}
	}

	// Pinning hq_console.md via {{HQ_SYNC}} hydrates 512k Markdown in 0 reads and (after arming triggers) suppresses Turn-2 init banner
	pinMD := fmt.Sprintf(`{"conversationId":%q,"artifactDirectoryPath":%q,"toolCall":{"name":"write_file","args":{"Path":%q,"Content":"{{HQ_SYNC}}"}}}`,
		deskConv1, artDesk1, filepath.Join(artDesk1, "hq_console.md"))
	pinMDOut, err := env.run(pinMD, "hook", "pre-tool")
	if err != nil || !strings.Contains(pinMDOut, `"decision":"allow"`) || !strings.Contains(pinMDOut, "512k") {
		t.Fatalf("expected hydrated 512k overwrite on hq_console.md {{HQ_SYNC}}, got err=%v out=%s", err, pinMDOut)
	}
	for _, s := range []struct{ cron, prompt string }{
		{"30 7 * * *", "[Auto] Morning Brief: Run the morning-brief skill and update hq_console.md."},
		{"*/15 7-22 * * *", "[Auto] Gmail Inbox Watch: Poll Gmail via hq watch precheck."},
		{"30 23 * * *", "[Auto] Sleep Consolidate: Run the sleep-consolidate nightly memory workflow."},
	} {
		schedCall := fmt.Sprintf(`{"conversationId":%q,"toolCall":{"name":"schedule","args":{"CronExpression":%q,"IsDaemon":true,"Prompt":%q}}}`, deskConv1, s.cron, s.prompt)
		if out, sErr := env.run(schedCall, "hook", "pre-tool"); sErr != nil || !strings.Contains(out, `"decision":"allow"`) {
			t.Fatalf("schedule pre-tool failed: %v (%s)", sErr, out)
		}
	}
	fdPinned, ok := kernel.GetFrontDesk(env.vaultDir)
	if !ok || !fdPinned.ConsolePinned {
		t.Fatalf("expected ConsolePinned=true, got %+v", fdPinned)
	}
	ssSuppressed, err := env.run(ssPayload1, "hook", "session-start")
	if err != nil || strings.Contains(ssSuppressed, "New [HQ] Front Desk Initialization Required") {
		t.Fatalf("expected Turn-2 initialization banner to be suppressed after single-artifact pin, got:\n%s", ssSuppressed)
	}

	// 2. Seed multi-model task (-2.json Opus vs -10.json Neon), child subagent, 3 cold-cache wakeups,
	// high-step_index / normal-PlannerSteps turn, and a >48h historical runaway turn.
	now := time.Now()
	t0, t1, t2, t3 := sameLocalDayTurnTimestamps(now)
	oldTS := now.Add(-48 * time.Hour).Format(time.RFC3339)
	todayStr := now.Format("2006-01-02")

	brainDir := filepath.Join(filepath.Dir(env.vaultDir), ".brain")
	mkTranscript := func(convID, body string) string {
		dir := filepath.Join(brainDir, convID, ".system_generated", "logs")
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			t.Fatal(mkErr)
		}
		p := filepath.Join(dir, "transcript.jsonl")
		if wErr := os.WriteFile(p, []byte(body), 0o644); wErr != nil {
			t.Fatal(wErr)
		}
		return p
	}

	// Front Desk transcript: 3 cold-cache wakeups (>=50k input, <20% first-step CHR) -> triggers [A2]
	fdTranscript := strings.Join([]string{
		fmt.Sprintf(`{"step_index":0,"type":"USER_INPUT","status":"DONE","created_at":%q,"content":"Wake 1"}`, t0),
		fmt.Sprintf(`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":%q,"completed_at":%q,"token_usage":{"input_tokens":60000,"output_tokens":1000,"cache_read_tokens":5000}}`, t0, t1),
		fmt.Sprintf(`{"step_index":2,"type":"USER_INPUT","status":"DONE","created_at":%q,"content":"Wake 2"}`, t1),
		fmt.Sprintf(`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","created_at":%q,"completed_at":%q,"token_usage":{"input_tokens":65000,"output_tokens":1000,"cache_read_tokens":5000}}`, t1, t2),
		fmt.Sprintf(`{"step_index":4,"type":"USER_INPUT","status":"DONE","created_at":%q,"content":"Wake 3"}`, t2),
		fmt.Sprintf(`{"step_index":5,"type":"PLANNER_RESPONSE","status":"DONE","created_at":%q,"completed_at":%q,"token_usage":{"input_tokens":70000,"output_tokens":1000,"cache_read_tokens":5000}}`, t2, t3),
	}, "\n") + "\n"
	fdTrPath := mkTranscript(deskConv1, fdTranscript)

	// Multi-model task transcript (conv-task-cuj48) spawning child subagent (conv-sub-cuj48),
	// plus an old >48h turn (Turn 0) and two today turns (Turn 1 = Opus, Turn 2 = Neon with high step_index=55 but only 1 PlannerStep)
	taskConv := "conv-task-cuj48"
	subConv := "conv-sub-cuj48"
	taskTranscript := strings.Join([]string{
		fmt.Sprintf(`{"step_index":0,"type":"USER_INPUT","status":"DONE","created_at":%q,"content":"Historical audit"}`, oldTS),
		fmt.Sprintf(`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":%q,"completed_at":%q,"token_usage":{"input_tokens":600000,"output_tokens":80000,"cache_read_tokens":100000}}`, oldTS, oldTS),
		fmt.Sprintf(`{"step_index":2,"type":"USER_INPUT","status":"DONE","created_at":%q,"content":"Phase 1 RFC on Opus"}`, t0),
		fmt.Sprintf(`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","created_at":%q,"completed_at":%q,"token_usage":{"input_tokens":40000,"output_tokens":10000,"cache_read_tokens":100000},"tool_calls":[{"id":"c1","name":"invoke_subagent","arguments_json":"{\"Model\":\"flash\",\"Role\":\"[standard] webui: UI Builder\",\"Prompt\":\"Build UI\",\"TypeName\":\"self\"}"}]}`, t0, t1),
		fmt.Sprintf(`{"step_index":4,"type":"GENERIC","status":"DONE","created_at":%q,"completed_at":%q,"content":"Created the following subagent:\n{\n  \"conversationId\": \"conv-sub-cuj48\"\n}"}`, t1, t1),
		fmt.Sprintf(`{"step_index":5,"type":"USER_INPUT","status":"DONE","created_at":%q,"content":"Phase 2 Code on Neon"}`, t2),
		fmt.Sprintf(`{"step_index":55,"type":"PLANNER_RESPONSE","status":"DONE","created_at":%q,"completed_at":%q,"token_usage":{"input_tokens":20000,"output_tokens":5000,"cache_read_tokens":200000},"tool_calls":[{"id":"c2","name":"edit_file","arguments_json":"{\"Path\":\"main.go\"}"}]}`, t2, t3),
	}, "\n") + "\n"
	_ = mkTranscript(taskConv, taskTranscript)

	subTranscript := strings.Join([]string{
		fmt.Sprintf(`{"step_index":0,"type":"USER_INPUT","status":"DONE","created_at":%q,"content":"Build UI"}`, t1),
		fmt.Sprintf(`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":%q,"completed_at":%q,"token_usage":{"input_tokens":15000,"output_tokens":3000,"cache_read_tokens":80000},"tool_calls":[{"id":"s1","name":"edit_file","arguments_json":"{\"Path\":\"ui.go\"}"}]}`, t1, t2),
	}, "\n") + "\n"
	_ = mkTranscript(subConv, subTranscript)

	if err := kernel.RegisterTask(env.vaultDir, kernel.ActiveTask{
		TaskSlug:       "webui-console-ux",
		Title:          "[Task:webui] Console UX Redesign",
		Kind:           "task",
		Scope:          "webui",
		ConversationID: taskConv,
		Model:          "gemini-4-neon-medium",
		Tier:           "standard",
		LastUpdatedAt:  t3,
		Status:         "active",
	}); err != nil {
		t.Fatal(err)
	}

	// Seed numerical -2.json (Opus) vs -10.json (Neon) journal entries to test numerical execNum sorting & per-turn attribution
	jDir := filepath.Join(env.vaultDir, "memory", "journal", todayStr)
	if err := os.MkdirAll(jDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJournal := func(name, convID string, execNum, stepStart, stepCount int, model, ts string) {
		raw, _ := json.Marshal(map[string]interface{}{
			"timestamp":      ts,
			"conversationId": convID,
			"executionNum":   execNum,
			"stepStartIdx":   stepStart,
			"stepCount":      stepCount,
			"modelName":      model,
		})
		if wErr := os.WriteFile(filepath.Join(jDir, name), raw, 0o644); wErr != nil {
			t.Fatal(wErr)
		}
	}
	writeJournal(taskConv+"-2.json", taskConv, 2, 2, 5, "claude-opus-5-5-high", t1)
	writeJournal(taskConv+"-10.json", taskConv, 10, 5, 56, "gemini-4-neon-medium", t3)
	writeJournal(subConv+"-1.json", subConv, 1, 0, 2, "gemini-4-neon-medium", t2)

	// Trigger hq hook stop on Front Desk so obs index + CONSOLE.md + hq_console_live.html sync
	stopFD := fmt.Sprintf(`{"conversationId":%q,"executionId":"fd-e3","executionNumber":3,"modelName":"gemini-4-hydrogen-medium","transcriptPath":%q,"finalResponseText":"Ready."}`, deskConv1, fdTrPath)
	if out, sErr := env.run(stopFD, "hook", "stop"); sErr != nil {
		t.Fatalf("hq hook stop failed: %v\n%s", sErr, out)
	}

	// 3. Verify `hq obs tokens --since=today --by=model --json`, `BuildDashboardSummary`, `hq obs top --tree`, and 24h-scoped `hq obs anomalies`
	obsJSON := mustRunV3(t, env, "obs", "tokens", "--since=today", "--by=model", "--json")
	var rep map[string]interface{}
	if err := json.Unmarshal([]byte(obsJSON), &rep); err != nil {
		t.Fatalf("unmarshal obs tokens --json: %v\n%s", err, obsJSON)
	}
	snap := obs.BuildDashboardSummary(env.vaultDir, now)
	if snap.Budget.EffectiveContextCeiling() != 512_000 {
		t.Fatalf("expected EffectiveContextCeiling=512000, got %d", snap.Budget.EffectiveContextCeiling())
	}
	todayWin := snap.Windows["today"]
	modelMap := map[string]obs.ModelBurnRow{}
	for _, mr := range todayWin.ByModel {
		modelMap[mr.Model] = mr
	}
	if modelMap["claude-opus-5-5-high"].UncachedTokens != 50_000 {
		t.Fatalf("expected Opus turn (-2.json) to own 50,000 uncached tokens, got %+v (all=%+v)", modelMap["claude-opus-5-5-high"], todayWin.ByModel)
	}
	if modelMap["gemini-4-neon-medium"].UncachedTokens != 43_000 {
		t.Fatalf("expected Neon turns (-10.json 25k + subagent 18k) to own 43,000 uncached tokens, got %+v", modelMap["gemini-4-neon-medium"])
	}

	// Verify disjoint root-tree attribution: subConv appears as a child under taskConv, NOT as a duplicate top-level root
	for _, root := range todayWin.TopRootTrees {
		if root.ConversationID == subConv {
			t.Fatalf("expected child subagent %s NOT to appear as a separate top-level root tree in %+v", subConv, todayWin.TopRootTrees)
		}
	}
	topTreeOut := mustRunV3(t, env, "obs", "top", "--tree")
	if strings.Contains(topTreeOut, "conv-sub") {
		t.Fatalf("expected disjoint hq obs top --tree to exclude child subagent from top-level rows, got:\n%s", topTreeOut)
	}

	// Verify 24h-scoped anomalies: [A2] fires on Front Desk's 3 cold-cache wakeups; 0 false-positive [A3] from step_index=55 or >48h turn
	anomOut := mustRunV3(t, env, "obs", "anomalies")
	if !strings.Contains(anomOut, "[A2]") {
		t.Fatalf("expected [A2] ColdCacheThrash on Front Desk, got:\n%s", anomOut)
	}
	if strings.Contains(anomOut, "[A3]") {
		t.Fatalf("expected 0 false-positive [A3] from step_index=55 (1 PlannerStep) or >48h turn, got:\n%s", anomOut)
	}

	// 4. Verify 4-Tab `:4242` Web Console, Smart Default Tab ("work" -> "needs-me"), `/api/state`, and Zero UI Jargon
	h := webui.NewHandler(env.vaultDir)
	reqWork := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/", nil)
	recWork := httptest.NewRecorder()
	h.ServeHTTP(recWork, reqWork)
	htmlWork := recWork.Body.String()
	if recWork.Code != http.StatusOK || !strings.Contains(htmlWork, `data-default-tab="work"`) {
		t.Fatalf("expected smart default tab 'work' when ActiveTasks>0 and ActionCount==0, code=%d", recWork.Code)
	}
	for _, want := range []string{`data-tab="needs_me"`, `data-tab="work"`, `data-tab="tokens"`, `data-tab="ops"`, `data-win="30d"`, `function hqFetch(`, `512k`} {
		if !strings.Contains(htmlWork, want) {
			t.Fatalf("missing %q in :4242 console HTML", want)
		}
	}
	for _, banned := range []string{"QBU", "Compute Share", "computeShare"} {
		if strings.Contains(htmlWork, banned) {
			t.Fatalf("unexpected banned jargon %q in :4242 console HTML", banned)
		}
	}

	// Stage an outbound proposal -> smart default tab flips to "needs_me" on the sole interactive Console
	mustRunV3(t, env, "propose", "--type=release_publish", "--summary=Publish v9.9.9 release", "--cmd=git push origin v9.9.9")
	mustRunV3(t, env, "console")

	recNeeds := httptest.NewRecorder()
	h.ServeHTTP(recNeeds, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/", nil))
	if !strings.Contains(recNeeds.Body.String(), `data-default-tab="needs_me"`) {
		t.Fatalf("expected smart default tab 'needs_me' once pending proposal exists")
	}

	recAPI := httptest.NewRecorder()
	h.ServeHTTP(recAPI, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/api/state", nil))
	if recAPI.Code != http.StatusOK || !strings.Contains(recAPI.Body.String(), `"windows"`) || !strings.Contains(recAPI.Body.String(), `"30d"`) {
		t.Fatalf("expected /api/state to include multi-window token breakdown, code=%d", recAPI.Code)
	}

	// Verify CONSOLE.md collapses PASS doctor checks, keeps 512k ceiling, links Sidecar, and emits zero QBU jargon
	mdBytes, err := os.ReadFile(filepath.Join(env.vaultDir, "CONSOLE.md"))
	if err != nil {
		t.Fatal(err)
	}
	mdStr := string(mdBytes)
	collapsedPASS := strings.Contains(mdStr, "checks passed.") || strings.Contains(mdStr, "other check(s) `PASS`")
	if !strings.Contains(mdStr, "512k") || !collapsedPASS || !strings.Contains(mdStr, "sidecar://agy-hq-console/") || strings.Contains(mdStr, "hq_console_live.html") || strings.Contains(mdStr, "QBU") {
		t.Fatalf("unexpected CONSOLE.md content:\n%s", mdStr)
	}

	// 5. Shift Handover via `hq claim-desk` removes hq_console.md (+ any legacy hq_console_live.html) from artDesk1 and passes `hq doctor` Checks #4 & #10
	_ = os.WriteFile(filepath.Join(artDesk1, "hq_console_live.html"), []byte("<html>legacy</html>"), 0o644)
	artDesk2 := filepath.Join(t.TempDir(), "brain-fd-2")
	if err := os.MkdirAll(artDesk2, 0o755); err != nil {
		t.Fatal(err)
	}
	mustRunV3(t, env, "claim-desk", "--conv=conv-fd-cuj48-2", "--artifact-dir="+artDesk2)
	for _, fn := range []string{"hq_console.md", "hq_console.md.metadata.json", "hq_console_live.html", "hq_console_live.html.metadata.json"} {
		if _, stErr := os.Stat(filepath.Join(artDesk1, fn)); !os.IsNotExist(stErr) {
			t.Fatalf("expected %s to be deleted from outgoing artDesk1 on Front Desk rotation", fn)
		}
	}
	for _, fn := range []string{"hq_console.md", "hq_console.md.metadata.json"} {
		if _, stErr := os.Stat(filepath.Join(artDesk2, fn)); stErr != nil {
			t.Fatalf("expected %s to be synced into incoming artDesk2 on claim-desk: %v", fn, stErr)
		}
	}
	docOut := mustRunV3(t, env, "doctor")
	if !strings.Contains(docOut, "[PASS] Front Desk Console Artifact") || !strings.Contains(docOut, "512k") {
		t.Fatalf("expected hq doctor PASS on Front Desk Console Artifact and 512k ceiling in Check #10, got:\n%s", docOut)
	}
}
