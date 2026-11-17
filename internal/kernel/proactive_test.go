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

	"github.com/zjshen14/agy-hq/internal/connector"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/outbox"
)

func TestDaemonGatekeeperDeduplicationAndCronMatcher(t *testing.T) {
	vaultDir := t.TempDir()
	stateFile := filepath.Join(vaultDir, "feed_state.txt")
	_ = os.WriteFile(stateFile, []byte(""), 0o600)

	trigYAML := `schedules:
  - id: edgar-close-watch
    cron: "CRON_TZ=UTC 30 13 * * 1-5"
    mode: daemon_gatekeeper
    model: flash
    precheck_cmd: ["cat", "` + stateFile + `"]
    dedup_mode: stdout_sha256
    delivery: interrupt_on_breach
    skill: thesis-check
`
	if err := os.WriteFile(filepath.Join(vaultDir, "triggers.yaml"), []byte(trigYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	// Thursday 13:30 UTC matches "30 13 * * 1-5"
	slot1 := time.Date(2026, 11, 12, 13, 30, 0, 0, time.UTC)
	if ok, _ := MatchesCron("CRON_TZ=UTC 30 13 * * 1-5", slot1); !ok {
		t.Fatalf("expected Thursday 13:30 UTC to match weekday cron")
	}
	// Saturday 13:30 UTC must NOT match "30 13 * * 1-5"
	sat := time.Date(2026, 11, 14, 13, 30, 0, 0, time.UTC)
	if ok, _ := MatchesCron("CRON_TZ=UTC 30 13 * * 1-5", sat); ok {
		t.Fatalf("expected Saturday 13:30 UTC NOT to match weekday cron")
	}

	// 1. Empty stdout -> 0-token skip (savesToday = 1, spawnsToday = 0)
	actions, saves, err := EvaluateDaemonGatekeepers(vaultDir, slot1, false, true)
	if err != nil || len(actions) != 0 || saves != 1 {
		t.Fatalf("expected 0 spawns and 1 zero-token save on empty output, got actions=%v saves=%d err=%v", actions, saves, err)
	}
	if q := LoadQuota(vaultDir); q.SpawnsToday != 0 || q.GatekeeperSavesToday != 1 {
		t.Fatalf("unexpected quota after 0-token skip: %+v", q)
	}

	// 2. Write new filing state -> next weekday slot triggers 1 dry-run Flash spawn
	_ = os.WriteFile(stateFile, []byte("ACME 8-K filed 2026-11-13"), 0o600)
	slot2 := time.Date(2026, 11, 13, 13, 30, 0, 0, time.UTC)
	actions2, saves2, err := EvaluateDaemonGatekeepers(vaultDir, slot2, false, true)
	if err != nil || len(actions2) != 1 || saves2 != 0 {
		t.Fatalf("expected 1 gatekeeper spawn on new SHA256 delta, got actions=%v saves=%d err=%v", actions2, saves2, err)
	}
	if !strings.Contains(actions2[0], "edgar-close-watch") {
		t.Fatalf("unexpected action string: %s", actions2[0])
	}

	// 3. Unchanged filing state on Monday -> 0-token skip (savesToday increments, no duplicate spawn!)
	slot3 := time.Date(2026, 11, 16, 13, 30, 0, 0, time.UTC)
	actions3, saves3, err := EvaluateDaemonGatekeepers(vaultDir, slot3, false, true)
	if err != nil || len(actions3) != 0 || saves3 != 1 {
		t.Fatalf("expected 0 spawns and 1 zero-token save when SHA256 unchanged, got actions=%v saves=%d err=%v", actions3, saves3, err)
	}
}

func TestGoalsAndWaitingOnSLASweep(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

	g, err := AddGoal(vaultDir, "acme-q4-contract", "Close ACME Q4 Partnership", "ACME", "2026-11-15", now)
	if err != nil || g.Slug != "acme-q4-contract" {
		t.Fatalf("AddGoal: %v (%+v)", err, g)
	}
	if _, ms, err := AddMilestone(vaultDir, g.Slug, "Draft redlines", now); err != nil || ms.ID != "ms-1" {
		t.Fatalf("AddMilestone: %v", err)
	}
	if gChecked, err := CheckMilestone(vaultDir, g.Slug, "ms-1", now); err != nil || !gChecked.Milestones[0].Done {
		t.Fatalf("CheckMilestone: %v", err)
	}

	if _, w, err := AddWaitingOn(vaultDir, g.Slug, "Alice Chen", "gmail", "thread-acme-1", "Signed redlines", 24*time.Hour, now); err != nil || w.ID != "wait-1" {
		t.Fatalf("AddWaitingOn: %v", err)
	}

	// At +12h: SLA not yet expired
	sweptEarly, err := SweepWaitingOnObligations(vaultDir, now.Add(12*time.Hour))
	if err != nil || len(sweptEarly) != 0 {
		t.Fatalf("expected 0 overdue obligations at +12h, got %v (%v)", sweptEarly, err)
	}

	// At +25h: SLA expired -> transitions to nudge_queued and writes outbox item
	sweptLate, err := SweepWaitingOnObligations(vaultDir, now.Add(25*time.Hour))
	if err != nil || len(sweptLate) != 1 {
		t.Fatalf("expected 1 overdue obligation swept at +25h, got %v (%v)", sweptLate, err)
	}
	items, _ := outbox.List(vaultDir, outbox.TierBriefRollup, outbox.StatusPending, now.Add(25*time.Hour))
	if len(items) != 1 || !strings.Contains(items[0].Title, "Alice Chen") {
		t.Fatalf("expected 1 pending brief_rollup follow-up nudge for Alice Chen, got %+v", items)
	}

	// Mark replied -> DueGoals should no longer report overdue wait
	if _, err := MarkWaitingReplied(vaultDir, g.Slug, "Alice Chen", now.Add(26*time.Hour)); err != nil {
		t.Fatalf("MarkWaitingReplied: %v", err)
	}
	_, dueWaits, _ := DueGoals(vaultDir, 48*time.Hour, now.Add(26*time.Hour))
	if len(dueWaits) != 0 {
		t.Fatalf("expected 0 due waits after MarkWaitingReplied, got %+v", dueWaits)
	}
}

func TestCoalescedTriageBatching(t *testing.T) {
	vaultDir := t.TempDir()
	t0 := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

	// Window = 15m -> first two bursts at t0 and t0+5m buffer without flushing
	r1 := EnqueueOrFlushCoalescedTriage(vaultDir, "gmail", "primary", "b1", []string{"th-1"}, 15*time.Minute, t0)
	if len(r1) != 0 {
		t.Fatalf("expected buffer hold at t0, got %+v", r1)
	}
	r2 := EnqueueOrFlushCoalescedTriage(vaultDir, "gmail", "primary", "b2", []string{"th-2", "th-3"}, 15*time.Minute, t0.Add(5*time.Minute))
	if len(r2) != 0 {
		t.Fatalf("expected buffer hold at t0+5m, got %+v", r2)
	}
	// At t0+16m -> flushes single consolidated entry with both batches and all 3 threads!
	r3 := EnqueueOrFlushCoalescedTriage(vaultDir, "gmail", "primary", "", nil, 15*time.Minute, t0.Add(16*time.Minute))
	if len(r3) != 1 || len(r3[0].BatchIDs) != 2 || len(r3[0].Targets) != 3 {
		t.Fatalf("expected 1 coalesced flush with 2 batches and 3 threads, got %+v", r3)
	}
}

func TestGatekeeperPrecheckFailureHealthAndStrandedInboxRecovery(t *testing.T) {
	vaultDir := t.TempDir()
	trigYAML := `schedules:
  - id: broken-watcher
    cron: "CRON_TZ=UTC 30 13 * * 1-5"
    mode: daemon_gatekeeper
    model: flash
    precheck_cmd: ["false"]
    dedup_mode: stdout_sha256
    skill: swe-pulse
`
	if err := os.WriteFile(filepath.Join(vaultDir, "triggers.yaml"), []byte(trigYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	slot := time.Date(2026, 11, 12, 13, 30, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		actions, saves, err := EvaluateDaemonGatekeepers(vaultDir, slot, false, true)
		if err != nil || len(actions) != 0 || saves != 0 {
			t.Fatalf("attempt %d: broken precheck must not save or spawn, got actions=%v saves=%d err=%v", i, actions, saves, err)
		}
	}
	cursors := LoadTriggerCursors(vaultDir)
	cur := cursors["broken-watcher"]
	if cur.ConsecutiveErrors != 3 || cur.SkipsCount != 0 || cur.LastCronSlot != "" {
		t.Fatalf("unexpected cursor health after 3 precheck errors: %+v", cur)
	}
	alerts, _ := outbox.List(vaultDir, outbox.TierBriefRollup, outbox.StatusPending, slot)
	if len(alerts) != 1 || !strings.Contains(alerts[0].Title, "broken-watcher") {
		t.Fatalf("expected 1 brief_rollup alert after 3 consecutive precheck failures, got %+v", alerts)
	}

	// Stranded inbox file in local/inbox/queued/ while CircuitOpen -> deferred without loss, then completed -> moved to processed/
	// Install ledger-ingest starter skill so stranded-2026.csv routes to /ledger-ingest
	ledgerSkillDir := filepath.Join(vaultDir, "skills", "ledger-ingest")
	_ = os.MkdirAll(ledgerSkillDir, 0o755)
	_ = os.WriteFile(filepath.Join(ledgerSkillDir, "SKILL.md"), []byte("---\nname: ledger-ingest\ndescription: test\n---\n"), 0o644)

	queuedDir := filepath.Join(vaultDir, "inbox", "queued")
	_ = os.MkdirAll(queuedDir, 0o755)
	strandedFile := filepath.Join(queuedDir, "stranded-2026.csv")
	_ = os.WriteFile(strandedFile, []byte("date,amount\n2026-11-12,42.00\n"), 0o600)

	qOpen := LoadQuota(vaultDir)
	qOpen.CircuitOpen = true
	qOpen.CircuitReason = "test circuit open"
	_ = SaveQuota(vaultDir, qOpen)

	defActions, err := ProcessInbox(vaultDir, false)
	if err != nil || len(defActions) != 1 || !strings.Contains(defActions[0], "deferred") {
		t.Fatalf("expected stranded inbox file to enqueue and defer under CircuitOpen, got actions=%v err=%v", defActions, err)
	}

	// Now create a synthetic mock agentapi binary returning a canonical UUID, clear CircuitOpen, and advance past 30s quota deferral
	binDir := t.TempDir()
	fakeAgentAPI := filepath.Join(binDir, "agentapi")
	script := "#!/bin/sh\necho \"00000000-0000-0000-0000-000000000077\"\n"
	if err := os.WriteFile(fakeAgentAPI, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	qClosed := LoadQuota(vaultDir)
	qClosed.CircuitOpen = false
	qClosed.CircuitReason = ""
	_ = SaveQuota(vaultDir, qClosed)

	dispatched, err := DispatchPendingEvents(vaultDir, time.Now().Add(45*time.Second), false)
	if err != nil || len(dispatched) != 1 || !strings.Contains(dispatched[0], "stranded-2026.csv -> /ledger-ingest") {
		t.Fatalf("expected deferred stranded file to dispatch cleanly once circuit closed, got %v (%v)", dispatched, err)
	}

	// Simulate worker Stop hook completing bound events -> moves queued file to processed/
	compCount, err := CompleteConversationEvents(vaultDir, "00000000-0000-0000-0000-000000000077", slot)
	if err != nil || compCount != 1 {
		t.Fatalf("expected CompleteConversationEvents to complete 1 event, got %d (%v)", compCount, err)
	}
	if _, err := os.Stat(strandedFile); !os.IsNotExist(err) {
		t.Fatalf("expected queued file to be moved out of inbox/queued/, stat err=%v", err)
	}
	procFile := filepath.Join(vaultDir, "inbox", "processed", "2026-11-12_stranded-2026.csv")
	if _, err := os.Stat(procFile); err != nil {
		t.Fatalf("expected processed file at %s: %v", procFile, err)
	}
}

func TestLSSessionPersistenceHydrationAndPrecheckEnqueues(t *testing.T) {
	wsRoot := t.TempDir()
	vaultDir := filepath.Join(wsRoot, "local")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatalf("mkdir vault: %v", err)
	}
	shimDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("ANTIGRAVITY_PROJECT_ID", "")
	if err := SetFrontDesk(vaultDir, "conv-fd-ls-test", ""); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}

	rawAddr := strings.TrimPrefix(srv.URL, "http://")
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "conv-fd-ls-test")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "proj-hq-ls-test")
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", rawAddr)
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-token-live")
	t.Setenv("ANTIGRAVITY_AGENTAPI_EXE", "/opt/fake/language_server")
	t.Setenv("ANTIGRAVITY_PLUGIN_SHIM_DIR", shimDir)
	t.Setenv("ANTIGRAVITY_CREDENTIAL_SCOPE", "scope-live-desktop")

	if err := RecordLiveLSSession(vaultDir); err != nil {
		t.Fatalf("RecordLiveLSSession: %v", err)
	}
	sessBytes, err := os.ReadFile(lsSessionFilePath(vaultDir))
	if err != nil {
		t.Fatalf("read ls_session.json: %v", err)
	}
	var recorded lsCredentials
	if err := json.Unmarshal(sessBytes, &recorded); err != nil {
		t.Fatalf("unmarshal ls_session.json: %v", err)
	}
	if recorded.RawAddress != rawAddr ||
		recorded.BaseURL != srv.URL ||
		recorded.CSRFToken != "csrf-token-live" ||
		recorded.AgentAPIExe != "/opt/fake/language_server" ||
		recorded.PluginShimDir != shimDir ||
		recorded.CredentialScope != "scope-live-desktop" ||
		recorded.ProjectID != "proj-hq-ls-test" {
		t.Fatalf("unexpected persisted LS credentials: %+v", recorded)
	}

	// Sentinel/quarantined invocation without desktop scope must NEVER clobber persisted desktop coords.
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "")
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", "")
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "")
	t.Setenv("ANTIGRAVITY_AGENTAPI_EXE", "")
	t.Setenv("ANTIGRAVITY_PLUGIN_SHIM_DIR", "/nonexistent-hq-sentinel")
	t.Setenv("ANTIGRAVITY_CREDENTIAL_SCOPE", "")
	if err := RecordLiveLSSession(vaultDir); err != nil {
		t.Fatalf("RecordLiveLSSession(headless/sentinel): %v", err)
	}
	if !HasReachableLSSession(vaultDir) {
		t.Fatalf("expected HasReachableLSSession=true while test server listening")
	}

	// Desktop plugin env hydration must work after desktop env vars are unset in process env.
	t.Setenv("ANTIGRAVITY_PLUGIN_SHIM_DIR", "")
	t.Setenv("ANTIGRAVITY_CREDENTIAL_SCOPE", "")
	hydrated, ok := connector.LoadReachableDesktopPluginEnv(vaultDir)
	if !ok || hydrated["ANTIGRAVITY_PLUGIN_SHIM_DIR"] != shimDir || hydrated["ANTIGRAVITY_CREDENTIAL_SCOPE"] != "scope-live-desktop" {
		t.Fatalf("expected LoadReachableDesktopPluginEnv to succeed, ok=%v hydrated=%+v", ok, hydrated)
	}
	if !connector.HasActiveDesktopPluginScope(vaultDir) {
		t.Fatalf("expected HasActiveDesktopPluginScope=true from persisted reachable LS session")
	}

	// configureSpawnCommand still injects LS credentials and primary ProjectID when process env lacks them.
	cmd := exec.Command("true")
	configureSpawnCommand(cmd, vaultDir, "cron:test")
	envMap := make(map[string]string)
	for _, kv := range cmd.Env {
		if k, v, cutOK := strings.Cut(kv, "="); cutOK {
			envMap[k] = v
		}
	}
	if envMap["ANTIGRAVITY_LS_ADDRESS"] != rawAddr ||
		envMap["ANTIGRAVITY_CSRF_TOKEN"] != "csrf-token-live" ||
		envMap["ANTIGRAVITY_AGENTAPI_EXE"] != "/opt/fake/language_server" ||
		envMap["ANTIGRAVITY_PROJECT_ID"] != "proj-hq-ls-test" {
		t.Fatalf("expected configureSpawnCommand to inject persisted LS/project env, got %+v", envMap)
	}

	// Simulate Antigravity cleaning up the per-run_command ephemeral /tmp/agy-plugin-cli-* dir
	// while an executable language_server (agentApiExe) and reachable LS remain alive:
	// desktop plugin scope hydration must STILL succeed!
	fakeHostExe := filepath.Join(t.TempDir(), "language_server")
	if err := os.WriteFile(fakeHostExe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fakeHostExe: %v", err)
	}
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", rawAddr)
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-token-live")
	t.Setenv("ANTIGRAVITY_AGENTAPI_EXE", fakeHostExe)
	t.Setenv("ANTIGRAVITY_PLUGIN_SHIM_DIR", "") // hook invocation without shim dir
	t.Setenv("ANTIGRAVITY_CREDENTIAL_SCOPE", "scope-hook-persisted")
	_ = os.RemoveAll(shimDir)
	if err := RecordLiveLSSession(vaultDir); err != nil {
		t.Fatalf("RecordLiveLSSession(post-shim-cleanup): %v", err)
	}
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", "")
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "")
	t.Setenv("ANTIGRAVITY_AGENTAPI_EXE", "")
	t.Setenv("ANTIGRAVITY_CREDENTIAL_SCOPE", "")
	hydratedNoShim, okNoShim := connector.LoadReachableDesktopPluginEnv(vaultDir)
	if !okNoShim || hydratedNoShim["ANTIGRAVITY_CREDENTIAL_SCOPE"] != "scope-hook-persisted" {
		t.Fatalf("expected LoadReachableDesktopPluginEnv to succeed after ephemeral shim cleanup when agentApiExe is executable, ok=%v hydrated=%+v", okNoShim, hydratedNoShim)
	}
	if _, hasDeadShim := hydratedNoShim["ANTIGRAVITY_PLUGIN_SHIM_DIR"]; hasDeadShim {
		t.Fatalf("expected dead shimDir not to be injected into hydrated env: %+v", hydratedNoShim)
	}

	// Verify ScheduleTrigger.EffectiveDedupMode accepts precheck_enqueues cleanly.
	if got := (ScheduleTrigger{ID: "gmail-inbox-watch", DedupMode: "precheck_enqueues"}).EffectiveDedupMode(); got != "precheck_enqueues" {
		t.Fatalf("expected EffectiveDedupMode()=precheck_enqueues, got %q", got)
	}

	// Closing LS server must flip HasReachableLSSession and desktop plugin scope to false cleanly.
	srv.Close()
	if HasReachableLSSession(vaultDir) {
		t.Fatalf("expected HasReachableLSSession=false after LS server closed")
	}
	if connector.HasActiveDesktopPluginScope(vaultDir) {
		t.Fatalf("expected HasActiveDesktopPluginScope=false after LS server closed")
	}
}

func TestPrecheckEnqueuesDoesNotDuplicateGatekeeperEventAndAdvancesSlot(t *testing.T) {
	vaultDir := t.TempDir()
	outFile := filepath.Join(vaultDir, "precheck_out.json")
	_ = os.WriteFile(outFile, []byte(`{"enqueued_total":2}`+"\n"), 0o600)
	trigYAML := `schedules:
  - id: gmail-inbox-watch
    cron: "CRON_TZ=America/Los_Angeles */15 7-22 * * *"
    mode: daemon_gatekeeper
    model: gemini-4-hydrogen-low
    precheck_cmd: ["cat", "` + outFile + `"]
    dedup_mode: precheck_enqueues
    delivery: brief_rollup
    skill: inbox-triage
`
	_ = os.WriteFile(filepath.Join(vaultDir, "triggers.yaml"), []byte(trigYAML), 0o600)

	loc, _ := time.LoadLocation("America/Los_Angeles")
	slot1 := time.Date(2026, 11, 15, 14, 15, 0, 0, loc)
	actions1, zero1, err1 := EvaluateDaemonGatekeepers(vaultDir, slot1, false, true)
	if err1 != nil {
		t.Fatalf("EvaluateDaemonGatekeepers slot1: %v", err1)
	}
	if zero1 != 0 || len(actions1) != 1 || actions1[0] != "precheck_enqueued:gmail-inbox-watch=2" {
		t.Fatalf("unexpected slot1 outcome: zero=%d actions=%v", zero1, actions1)
	}
	if pending, _ := events.List(vaultDir, "pending", false, 0); len(pending) != 0 {
		t.Fatalf("expected precheck_enqueues NOT to enqueue duplicate gatekeeper event, got %+v", pending)
	}
	cursors := LoadTriggerCursors(vaultDir)
	firstSlotKey := cursors["gmail-inbox-watch"].LastCronSlot
	if firstSlotKey == "" {
		t.Fatalf("expected LastCronSlot populated after precheck_enqueues hit")
	}

	// Second tick in the SAME cron slot must be skipped via slot dedup without re-running precheck_cmd.
	actionsRepeat, zeroRepeat, errRepeat := EvaluateDaemonGatekeepers(vaultDir, slot1.Add(30*time.Second), false, true)
	if errRepeat != nil || zeroRepeat != 0 || len(actionsRepeat) != 0 {
		t.Fatalf("expected same cron slot to be skipped cleanly, err=%v zero=%d actions=%v", errRepeat, zeroRepeat, actionsRepeat)
	}

	// Slot with enqueued_total=0 increments zeroTokenSaves and still advances LastCronSlot.
	_ = os.WriteFile(outFile, []byte(`{"enqueued_total":0}`+"\n"), 0o600)
	slot2 := time.Date(2026, 11, 15, 14, 30, 0, 0, loc)
	actions2, zero2, err2 := EvaluateDaemonGatekeepers(vaultDir, slot2, false, true)
	if err2 != nil || zero2 != 1 || len(actions2) != 0 {
		t.Fatalf("expected enqueued_total=0 to count 1 zero-token save, err=%v zero=%d actions=%v", err2, zero2, actions2)
	}
	cursors2 := LoadTriggerCursors(vaultDir)
	if cursors2["gmail-inbox-watch"].LastCronSlot == "" || cursors2["gmail-inbox-watch"].LastCronSlot == firstSlotKey {
		t.Fatalf("expected LastCronSlot to advance to slot2, got %q (was %q)", cursors2["gmail-inbox-watch"].LastCronSlot, firstSlotKey)
	}
}
