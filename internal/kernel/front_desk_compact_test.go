package kernel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/outbox"
)

func TestParseTriggersCompactFields(t *testing.T) {
	raw := `schedules:
  - id: morning-brief
    cron: "CRON_TZ=UTC 30 7 * * *"
    mode: front_desk_cron
    tier: routine
    compact_before_run: true
    compact_context_tokens: 135000
    compact_min_planner_steps: 3
    delivery: interrupt
    skill: morning-brief
  - id: ping-only
    cron: "0 * * * *"
    mode: front_desk_cron
    tier: quick
    delivery: silent_ledger
    skill: inbox-triage
`
	cfg, err := ParseTriggersContent(raw)
	if err != nil {
		t.Fatalf("ParseTriggersContent: %v", err)
	}
	if len(cfg.Schedules) != 2 {
		t.Fatalf("expected 2 schedules, got %d", len(cfg.Schedules))
	}
	mb := cfg.Schedules[0]
	if !mb.CompactBeforeRun || mb.CompactContextTokens != 135000 || mb.CompactMinPlannerSteps != 3 {
		t.Fatalf("unexpected morning-brief compact config: %+v", mb)
	}
	if cfg.Schedules[1].CompactBeforeRun {
		t.Fatalf("expected compact_before_run to default false when omitted")
	}

	// Verify ParseTriggersContent handles PackSchedule-equivalent fields consistently when empty/defaulted.
	var psTest struct {
		CompactBeforeRun       bool  `json:"compact_before_run"`
		CompactContextTokens   int64 `json:"compact_context_tokens"`
		CompactMinPlannerSteps int   `json:"compact_min_planner_steps"`
	}
	rawJSON := `{"compact_before_run":true,"compact_context_tokens":140000,"compact_min_planner_steps":3}`
	if err := json.Unmarshal([]byte(rawJSON), &psTest); err != nil || !psTest.CompactBeforeRun || psTest.CompactContextTokens != 140000 || psTest.CompactMinPlannerSteps != 3 {
		t.Fatalf("unexpected pack compact JSON unmarshal: %+v err=%v", psTest, err)
	}
}

type capturedSendCascadeReq struct {
	ConnectProto string
	CSRFToken    string
	RawBody      map[string]interface{}
}

func startMockSendCascadeServer(t *testing.T, csrf string, statusCode int, captured *[]capturedSendCascadeReq, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/exa.language_server_pb.LanguageServerService/SendUserCascadeMessage" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("x-codeium-csrf-token") != csrf {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		*captured = append(*captured, capturedSendCascadeReq{
			ConnectProto: r.Header.Get("Connect-Protocol-Version"),
			CSRFToken:    r.Header.Get("x-codeium-csrf-token"),
			RawBody:      body,
		})
		mu.Unlock()
		if statusCode >= 300 {
			// Simulate an LS rejecting slashCommand while accepting plain prompt requests.
			if items, ok := body["items"].([]interface{}); ok && len(items) > 1 {
				http.Error(w, "unknown field slashCommand", statusCode)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
}

func seedFrontDeskCompactTriggers(t *testing.T, vaultDir string, compactBeforeRun bool) {
	t.Helper()
	compactLine := ""
	if compactBeforeRun {
		compactLine = "    compact_before_run: true\n"
	}
	trigYAML := "schedules:\n" +
		"  - id: morning-brief\n" +
		"    cron: \"CRON_TZ=UTC 30 7 * * *\"\n" +
		"    mode: front_desk_cron\n" +
		"    tier: routine\n" +
		compactLine +
		"    delivery: interrupt\n" +
		"    skill: morning-brief\n"
	if err := os.WriteFile(filepath.Join(vaultDir, "triggers.yaml"), []byte(trigYAML), 0o600); err != nil {
		t.Fatalf("write triggers.yaml: %v", err)
	}
}

func TestGatedFrontDeskCompactThenContinueDecisionAndPayload(t *testing.T) {
	dueTime := time.Date(2026, 11, 21, 7, 30, 0, 0, time.UTC)
	fdID := "fd000001-1111-4111-8111-111111111111"
	const csrf = "csrf-compact-test"

	var snapMu sync.Mutex
	var activeSnap FrontDeskContextSnapshot
	snapPresent := true
	RegisterFrontDeskContextInspector(func(string) (FrontDeskContextSnapshot, bool) {
		snapMu.Lock()
		defer snapMu.Unlock()
		return activeSnap, snapPresent
	})
	t.Cleanup(func() { RegisterFrontDeskContextInspector(nil) })

	var reqMu sync.Mutex
	var captured []capturedSendCascadeReq
	srv := startMockSendCascadeServer(t, csrf, http.StatusOK, &captured, &reqMu)
	defer srv.Close()

	lsEnv := func(vDir string) {
		t.Setenv("ANTIGRAVITY_LS_ADDRESS", strings.TrimPrefix(srv.URL, "http://"))
		t.Setenv("ANTIGRAVITY_CSRF_TOKEN", csrf)
		t.Setenv("ANTIGRAVITY_PROJECT_ID", "")
		if err := SetFrontDesk(vDir, fdID, ""); err != nil {
			t.Fatalf("SetFrontDesk: %v", err)
		}
	}

	// 1. Below context token watermark -> skips compact, still uses MESSAGE_DELIVERY_STRATEGY_WHEN_IDLE
	t.Run("BelowTokenWatermark", func(t *testing.T) {
		vDir := t.TempDir()
		lsEnv(vDir)
		seedFrontDeskCompactTriggers(t, vDir, true)
		snapMu.Lock()
		activeSnap = FrontDeskContextSnapshot{
			ConversationID:                  fdID,
			CurrentContextTokens:            90_000,
			AutoCompactContextTokens:        120_000,
			PlannerResponsesSinceCheckpoint: 6,
		}
		snapPresent = true
		snapMu.Unlock()

		acts, skipReason, err := TickScheduleOnce(vDir, "core/morning-brief", false, dueTime)
		if err != nil || skipReason != "" || len(acts) != 1 {
			t.Fatalf("TickScheduleOnce failed: acts=%v skip=%q err=%v", acts, skipReason, err)
		}
		if !strings.Contains(acts[0], "skip-compact=below_token_watermark") {
			t.Fatalf("expected skip-compact=below_token_watermark in action, got %q", acts[0])
		}
		reqMu.Lock()
		got := captured[len(captured)-1]
		reqMu.Unlock()
		items := got.RawBody["items"].([]interface{})
		if len(items) != 1 {
			t.Fatalf("expected 1 plain item when below threshold, got %#v", items)
		}
		if got.RawBody["deliveryStrategy"] != LSDeliveryStrategyWhenIdle || got.RawBody["blocking"] != false {
			t.Fatalf("expected blocking=false and WHEN_IDLE strategy, got %#v", got.RawBody)
		}
		if _, hasCfg := got.RawBody["cascadeConfig"]; hasCfg {
			t.Fatalf("cascadeConfig must be omitted on SendUserCascadeMessage")
		}
	})

	// 2. High context immediately after DONE CHECKPOINT (0 uncompacted planner steps) -> skips compact!
	t.Run("ZeroPlannerStepsAfterCheckpoint", func(t *testing.T) {
		vDir := t.TempDir()
		lsEnv(vDir)
		seedFrontDeskCompactTriggers(t, vDir, true)
		snapMu.Lock()
		activeSnap = FrontDeskContextSnapshot{
			ConversationID:                  fdID,
			CurrentContextTokens:            180_000,
			AutoCompactContextTokens:        120_000,
			PlannerResponsesSinceCheckpoint: 0,
			LastCheckpointStepIdx:           12,
		}
		snapPresent = true
		snapMu.Unlock()

		beforeCount := len(captured)
		acts, skipReason, err := TickScheduleOnce(vDir, "core/morning-brief", false, dueTime)
		if err != nil || skipReason != "" || len(acts) != 1 {
			t.Fatalf("TickScheduleOnce failed: acts=%v skip=%q err=%v", acts, skipReason, err)
		}
		if !strings.Contains(acts[0], "skip-compact=insufficient_planner_activity") {
			t.Fatalf("expected insufficient_planner_activity skip, got %q", acts[0])
		}
		got := captured[beforeCount]
		if items := got.RawBody["items"].([]interface{}); len(items) != 1 {
			t.Fatalf("expected plain 1-item prompt right after checkpoint, got %#v", items)
		}
	})

	// 3. Dry-run annotation when both conditions pass
	t.Run("DryRunReportsCompactGate", func(t *testing.T) {
		vDir := t.TempDir()
		lsEnv(vDir)
		seedFrontDeskCompactTriggers(t, vDir, true)
		snapMu.Lock()
		activeSnap = FrontDeskContextSnapshot{
			ConversationID:                  fdID,
			CurrentContextTokens:            140_000,
			AutoCompactContextTokens:        120_000,
			PlannerResponsesSinceCheckpoint: 3,
		}
		snapPresent = true
		snapMu.Unlock()

		beforeCount := len(captured)
		acts, skipReason, err := TickScheduleOnce(vDir, "core/morning-brief", true, dueTime)
		if err != nil || skipReason != "" || len(acts) != 1 {
			t.Fatalf("TickScheduleOnce dry-run failed: acts=%v skip=%q err=%v", acts, skipReason, err)
		}
		if !strings.Contains(acts[0], "compact+when_idle") {
			t.Fatalf("expected dry-run compact+when_idle note, got %q", acts[0])
		}
		if len(captured) != beforeCount {
			t.Fatalf("dry-run must not send HTTP requests")
		}
	})

	// 4. Both watermark (>=120k) AND uncompacted planner activity (>=2) met -> exact CompactThenContinue payload!
	t.Run("CompactThenContinueSchema", func(t *testing.T) {
		vDir := t.TempDir()
		lsEnv(vDir)
		seedFrontDeskCompactTriggers(t, vDir, true)
		snapMu.Lock()
		activeSnap = FrontDeskContextSnapshot{
			ConversationID:                  fdID,
			CurrentContextTokens:            150_000,
			AutoCompactContextTokens:        120_000,
			PlannerResponsesSinceCheckpoint: 4,
		}
		snapPresent = true
		snapMu.Unlock()

		beforeCount := len(captured)
		acts, skipReason, err := TickScheduleOnce(vDir, "core/morning-brief", false, dueTime)
		if err != nil || skipReason != "" || len(acts) != 1 {
			t.Fatalf("TickScheduleOnce failed: acts=%v skip=%q err=%v", acts, skipReason, err)
		}
		if !strings.Contains(acts[0], "compact+when_idle") {
			t.Fatalf("expected compact+when_idle action, got %q", acts[0])
		}
		got := captured[beforeCount]
		if got.ConnectProto != "1" {
			t.Fatalf("expected Connect-Protocol-Version: 1, got %q", got.ConnectProto)
		}
		if got.RawBody["cascadeId"] != fdID || got.RawBody["blocking"] != false || got.RawBody["deliveryStrategy"] != LSDeliveryStrategyWhenIdle {
			t.Fatalf("unexpected top-level SendUserCascadeMessage fields: %#v", got.RawBody)
		}
		if _, hasCfg := got.RawBody["cascadeConfig"]; hasCfg {
			t.Fatalf("cascadeConfig must NEVER be sent on SendUserCascadeMessage")
		}
		items := got.RawBody["items"].([]interface{})
		if len(items) != 2 {
			t.Fatalf("expected 2 items ([compact slashCommand, text]), got %#v", items)
		}
		first := items[0].(map[string]interface{})["item"].(map[string]interface{})["slashCommand"].(map[string]interface{})["info"].(map[string]interface{})
		if first["name"] != "compact" || first["type"] != "SLASH_COMMAND_TYPE_SYSTEM" {
			t.Fatalf("unexpected compact slashCommand info: %#v", first)
		}
		secondText, _ := items[1].(map[string]interface{})["text"].(string)
		if !strings.Contains(secondText, "morning-brief") {
			t.Fatalf("expected prompt text in items[1].text, got %q", secondText)
		}
	})

	// 5. Regular outbox interrupt alerts and non-opt-in front_desk_cron NEVER compact even at 180k / 10 steps!
	t.Run("NonOptInAlertsNeverCompact", func(t *testing.T) {
		vDir := t.TempDir()
		lsEnv(vDir)
		seedFrontDeskCompactTriggers(t, vDir, false) // compact_before_run NOT set
		snapMu.Lock()
		activeSnap = FrontDeskContextSnapshot{
			ConversationID:                  fdID,
			CurrentContextTokens:            185_000,
			AutoCompactContextTokens:        120_000,
			PlannerResponsesSinceCheckpoint: 10,
		}
		snapPresent = true
		snapMu.Unlock()

		beforeCount := len(captured)
		if _, _, err := outbox.Add(vDir, outbox.OutboxItem{
			SourceSkill: "inbox-triage",
			Tier:        outbox.TierInterrupt,
			Title:       "Urgent alert",
			Summary:     "Check mail",
		}, dueTime); err != nil {
			t.Fatalf("outbox.Add: %v", err)
		}
		if n, err := DispatchOutboxToFrontDesk(vDir, dueTime, false); err != nil || n != 1 {
			t.Fatalf("DispatchOutboxToFrontDesk: n=%d err=%v", n, err)
		}
		if _, skip, err := TickScheduleOnce(vDir, "core/morning-brief", false, dueTime); err != nil || skip != "" {
			t.Fatalf("non-opt-in TickScheduleOnce failed: skip=%q err=%v", skip, err)
		}
		if _, _, err := events.Enqueue(vDir, events.Event{
			Source:    "timer",
			DedupKey:  "timer-noncompact",
			ScopeKey:  "timer:1",
			SessionID: fdID,
			Title:     "[Follow-Up]",
			Prompt:    "Resume check",
		}, dueTime); err != nil {
			t.Fatalf("events.Enqueue: %v", err)
		}
		if _, err := DispatchPendingEvents(vDir, dueTime, false); err != nil {
			t.Fatalf("DispatchPendingEvents: %v", err)
		}

		for i, req := range captured[beforeCount:] {
			items := req.RawBody["items"].([]interface{})
			if len(items) != 1 {
				t.Fatalf("non-opt-in delivery #%d unexpectedly attached compact item: %#v", i, items)
			}
		}
	})
}

func TestCompactFallbacksNeverSendRawSlashCompact(t *testing.T) {
	dueTime := time.Date(2026, 11, 21, 7, 30, 0, 0, time.UTC)
	fdID := "fd000002-1111-4111-8111-111111111111"
	const csrf = "csrf-fallback-test"

	RegisterFrontDeskContextInspector(func(string) (FrontDeskContextSnapshot, bool) {
		return FrontDeskContextSnapshot{
			ConversationID:                  fdID,
			CurrentContextTokens:            150_000,
			AutoCompactContextTokens:        120_000,
			PlannerResponsesSinceCheckpoint: 4,
		}, true
	})
	t.Cleanup(func() { RegisterFrontDeskContextInspector(nil) })

	// A) LS returns HTTP 400 rejecting slashCommand -> automatically retries plain SendUserCascadeMessage (NO /compact)
	t.Run("LSRejectsSlashCommandFallsBackToPlainLS", func(t *testing.T) {
		vDir := t.TempDir()
		var mu sync.Mutex
		var captured []capturedSendCascadeReq
		srv := startMockSendCascadeServer(t, csrf, http.StatusBadRequest, &captured, &mu)
		defer srv.Close()

		t.Setenv("ANTIGRAVITY_LS_ADDRESS", strings.TrimPrefix(srv.URL, "http://"))
		t.Setenv("ANTIGRAVITY_CSRF_TOKEN", csrf)
		t.Setenv("ANTIGRAVITY_PROJECT_ID", "")
		_ = SetFrontDesk(vDir, fdID, "")
		seedFrontDeskCompactTriggers(t, vDir, true)

		acts, skip, err := TickScheduleOnce(vDir, "core/morning-brief", false, dueTime)
		if err != nil || skip != "" || len(acts) != 1 {
			t.Fatalf("TickScheduleOnce failed: acts=%v skip=%q err=%v", acts, skip, err)
		}
		if !strings.Contains(acts[0], "compact-fallback-plain") {
			t.Fatalf("expected compact-fallback-plain status, got %q", acts[0])
		}
		if len(captured) != 2 {
			t.Fatalf("expected 2 LS calls (rejected compact attempt + plain fallback), got %d", len(captured))
		}
		plainItems := captured[1].RawBody["items"].([]interface{})
		if len(plainItems) != 1 {
			t.Fatalf("expected plain fallback to have 1 item, got %#v", plainItems)
		}
		txt, _ := plainItems[0].(map[string]interface{})["text"].(string)
		if strings.HasPrefix(strings.TrimSpace(txt), "/compact") {
			t.Fatalf("plain fallback must NEVER inject /compact text, got %q", txt)
		}
	})

	// B) LS unreachable -> falls back to agentapi send-message with plain prompt text only (NEVER "/compact")
	t.Run("LSUnavailableAgentAPIFallbackIsPlainTextOnly", func(t *testing.T) {
		vDir := t.TempDir()
		fakeBin := t.TempDir()
		captureFile := filepath.Join(t.TempDir(), "agentapi-args.txt")
		fakeAgentAPI := filepath.Join(fakeBin, "agentapi")
		script := "#!/bin/sh\nprintf '%s\n' \"$@\" > " + captureFile + "\necho 'queued'\n"
		if err := os.WriteFile(fakeAgentAPI, []byte(script), 0o755); err != nil {
			t.Fatalf("write fake agentapi: %v", err)
		}

		t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("ANTIGRAVITY_LS_ADDRESS", "127.0.0.1:1")
		t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "dead")
		t.Setenv("ANTIGRAVITY_PROJECT_ID", "")
		_ = SetFrontDesk(vDir, fdID, "")
		seedFrontDeskCompactTriggers(t, vDir, true)

		acts, skip, err := TickScheduleOnce(vDir, "core/morning-brief", false, dueTime)
		if err != nil || skip != "" || len(acts) != 1 {
			t.Fatalf("TickScheduleOnce with agentapi fallback failed: acts=%v skip=%q err=%v", acts, skip, err)
		}
		gotBytes, rErr := os.ReadFile(captureFile)
		if rErr != nil {
			t.Fatalf("expected fake agentapi to be invoked: %v", rErr)
		}
		gotArgs := string(gotBytes)
		if !strings.Contains(gotArgs, "send-message") || !strings.Contains(gotArgs, fdID) || !strings.Contains(gotArgs, "morning-brief") {
			t.Fatalf("unexpected agentapi args:\n%s", gotArgs)
		}
		if strings.Contains(gotArgs, "/compact") {
			t.Fatalf("agentapi fallback must NEVER receive /compact text, got:\n%s", gotArgs)
		}
	})
}
