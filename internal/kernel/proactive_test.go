package kernel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
