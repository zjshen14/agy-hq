package kernel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/extension"
)

func TestHeartbeatRespectsPausedAndCronGatedWatcherSchedules(t *testing.T) {
	vaultDir := t.TempDir()

	// Seed deterministic mock Gmail message + account config
	mockDir := filepath.Join(vaultDir, "memory", "gmail", "mock")
	if err := os.MkdirAll(mockDir, 0o755); err != nil {
		t.Fatalf("mkdir mockDir: %v", err)
	}
	mockMsgs := `[{"id":"msg-101","threadId":"th-101","snippet":"Board packet","payload":{"headers":[{"name":"From","value":"ceo@acme.com"},{"name":"To","value":"owner@example.com"},{"name":"Subject","value":"Action needed: Q4 packet"},{"name":"Authentication-Results","value":"mx.google.com; dkim=pass header.d=acme.com"}]}}]`
	if err := os.WriteFile(filepath.Join(mockDir, "messages.json"), []byte(mockMsgs), 0o600); err != nil {
		t.Fatalf("write mock messages: %v", err)
	}
	privGmailDir := filepath.Join(vaultDir, "private", "gmail")
	_ = os.MkdirAll(privGmailDir, 0o700)
	acctJSON := `{"defaultAccount":"primary","accounts":{"primary":{"alias":"primary","email":"owner@example.com","enabled":true}}}`
	if err := os.WriteFile(filepath.Join(privGmailDir, "accounts.json"), []byte(acctJSON), 0o600); err != nil {
		t.Fatalf("write accounts.json: %v", err)
	}

	// Disable gchat/gcalendar connectors so only gmail is poll-eligible
	_ = os.MkdirAll(filepath.Join(vaultDir, "connectors", "gchat"), 0o755)
	_ = os.WriteFile(filepath.Join(vaultDir, "connectors", "gchat", "instance.json"), []byte(`{"schema_version":1,"enabled":false}`), 0o600)

	// 1. Schedule disabled directly in triggers.yaml (`enabled: false`) -> RunHeartbeatAt must NOT poll or enqueue
	disabledTrig := `schedules:
  - id: gmail-inbox-watch
    cron: "CRON_TZ=UTC */15 7-22 * * *"
    mode: daemon_gatekeeper
    tier: quick
    enabled: false
    precheck_cmd: ["hq", "watch", "precheck", "--connector=gmail", "--json"]
    dedup_mode: precheck_enqueues
    skill: inbox-triage
`
	if err := os.WriteFile(filepath.Join(vaultDir, "triggers.yaml"), []byte(disabledTrig), 0o600); err != nil {
		t.Fatalf("write disabled triggers.yaml: %v", err)
	}
	dueSlot := time.Date(2026, 11, 15, 14, 15, 0, 0, time.UTC)
	if _, err := RunHeartbeatAt(vaultDir, dueSlot, false); err != nil {
		t.Fatalf("RunHeartbeatAt(disabled): %v", err)
	}
	if allEvs, _ := events.List(vaultDir, "", true, 50); len(allEvs) != 0 {
		t.Fatalf("expected 0 enqueued events when gmail-inbox-watch has enabled: false, got %+v", allEvs)
	}

	// 2. Enable in triggers.yaml, then pause via `hq triggers pause` (`SetSchedulePaused`) -> must NOT poll or enqueue
	enabledTrig := strings.ReplaceAll(disabledTrig, "    enabled: false\n", "")
	if err := os.WriteFile(filepath.Join(vaultDir, "triggers.yaml"), []byte(enabledTrig), 0o600); err != nil {
		t.Fatalf("write enabled triggers.yaml: %v", err)
	}
	if _, _, err := extension.SetSchedulePaused(context.Background(), vaultDir, "core/gmail-inbox-watch", true, "maintenance", "none", dueSlot); err != nil {
		t.Fatalf("SetSchedulePaused(pause): %v", err)
	}
	if _, err := RunHeartbeatAt(vaultDir, dueSlot, false); err != nil {
		t.Fatalf("RunHeartbeatAt(paused): %v", err)
	}
	if allEvs, _ := events.List(vaultDir, "", true, 50); len(allEvs) != 0 {
		t.Fatalf("expected 0 enqueued events while gmail-inbox-watch is PAUSED, got %+v", allEvs)
	}

	// 3. Resume schedule, but run heartbeat off-cron (14:07 UTC and 03:00 UTC) -> must NOT poll outside cron window
	if _, _, err := extension.SetSchedulePaused(context.Background(), vaultDir, "core/gmail-inbox-watch", false, "", "none", dueSlot); err != nil {
		t.Fatalf("SetSchedulePaused(resume): %v", err)
	}
	for _, offTime := range []time.Time{
		time.Date(2026, 11, 15, 14, 7, 0, 0, time.UTC),
		time.Date(2026, 11, 15, 3, 15, 0, 0, time.UTC),
	} {
		if _, err := RunHeartbeatAt(vaultDir, offTime, false); err != nil {
			t.Fatalf("RunHeartbeatAt(%s): %v", offTime, err)
		}
		if allEvs, _ := events.List(vaultDir, "", true, 50); len(allEvs) != 0 {
			t.Fatalf("expected 0 enqueued events outside cron window at %s, got %+v", offTime, allEvs)
		}
	}

	// 4. Run heartbeat on due cron slot (14:15 UTC) -> polls gmail once and enqueues watcher event
	status, err := RunHeartbeatAt(vaultDir, dueSlot, false)
	if err != nil {
		t.Fatalf("RunHeartbeatAt(due): %v", err)
	}
	if !strings.Contains(status, "precheck_enqueued:gmail-inbox-watch=1") {
		t.Fatalf("expected precheck_enqueued:gmail-inbox-watch=1 on due slot, got %q", status)
	}
	allEvs, _ := events.List(vaultDir, "", true, 50)
	if len(allEvs) == 0 {
		t.Fatalf("expected watcher event to be enqueued on enabled due cron slot")
	}
}

func TestTickScheduleOnceHonorsPauseCronDueFrontDeskLeaseAndTierRouting(t *testing.T) {
	vaultDir := t.TempDir()
	trigYAML := `schedules:
  - id: morning-brief
    cron: "CRON_TZ=UTC 30 7 * * *"
    mode: front_desk_cron
    tier: routine
    delivery: interrupt
    skill: morning-brief
  - id: sleep-consolidate
    cron: "CRON_TZ=UTC 30 23 * * *"
    mode: isolated_worker
    tier: deep_routine
    delivery: silent_ledger
    skill: sleep-consolidate
`
	if err := os.WriteFile(filepath.Join(vaultDir, "triggers.yaml"), []byte(trigYAML), 0o600); err != nil {
		t.Fatalf("write triggers.yaml: %v", err)
	}
	// Custom tier override in local/config.yaml to verify tier-only triggers resolve via config.yaml routing.tiers
	cfgYAML := `routing:
  tiers:
    routine:
      model: claude-sonnet-5-5-medium
      subagent_model: pro
    deep_routine:
      model: claude-opus-5-5-high
      subagent_model: pro
`
	if err := os.WriteFile(filepath.Join(vaultDir, "config.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	morningDue := time.Date(2026, 11, 15, 7, 30, 0, 0, time.UTC)
	morningOff := time.Date(2026, 11, 15, 7, 31, 0, 0, time.UTC)
	sleepDue := time.Date(2026, 11, 15, 23, 30, 0, 0, time.UTC)

	// 1. Paused schedule: TickScheduleOnce exits cleanly with skipReason="paused", 0 actions
	_, _, _ = extension.SetSchedulePaused(context.Background(), vaultDir, "core/morning-brief", true, "vacation", "none", morningDue)
	acts, skip, err := TickScheduleOnce(vaultDir, "core/morning-brief", true, morningDue)
	if err != nil || skip != "paused" || len(acts) != 0 {
		t.Fatalf("expected paused TickScheduleOnce to skip with 'paused', got acts=%v skip=%q err=%v", acts, skip, err)
	}

	// 2. Resume morning-brief; off-cron minute returns skipReason="not_due"
	_, _, _ = extension.SetSchedulePaused(context.Background(), vaultDir, "core/morning-brief", false, "", "none", morningDue)
	actsOff, skipOff, err := TickScheduleOnce(vaultDir, "core/morning-brief", true, morningOff)
	if err != nil || skipOff != "not_due" || len(actsOff) != 0 {
		t.Fatalf("expected off-minute TickScheduleOnce to skip with 'not_due', got acts=%v skip=%q err=%v", actsOff, skipOff, err)
	}

	// 3. Due minute with NO Front Desk lease claimed: both TickScheduleOnce and EvaluateDaemonGatekeepers skip cleanly
	actsNoFD, skipNoFD, err := TickScheduleOnce(vaultDir, "core/morning-brief", true, morningDue)
	if err != nil || skipNoFD != "no_front_desk_lease" || len(actsNoFD) != 0 {
		t.Fatalf("expected vacant Front Desk TickScheduleOnce to skip with 'no_front_desk_lease', got acts=%v skip=%q err=%v", actsNoFD, skipNoFD, err)
	}
	gkActsNoFD, _, err := EvaluateDaemonGatekeepers(vaultDir, morningDue, false, true)
	if err != nil || len(gkActsNoFD) != 0 {
		t.Fatalf("expected EvaluateDaemonGatekeepers to skip front_desk_cron without Front Desk lease, got %v (%v)", gkActsNoFD, err)
	}

	// 4. Claim Front Desk lease and advance to next day's 07:30 UTC -> delivers to Front Desk session!
	nextMorningDue := morningDue.Add(24 * time.Hour)
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "")
	if err := SetFrontDesk(vaultDir, "22222222-2222-4222-8222-222222222222", ""); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}
	actsFD, skipFD, err := TickScheduleOnce(vaultDir, "core/morning-brief", true, nextMorningDue)
	if err != nil || skipFD != "" || len(actsFD) != 1 || !strings.Contains(actsFD[0], "22222222-2222-4222-8222-222222222222") {
		t.Fatalf("expected claimed Front Desk TickScheduleOnce to deliver to Front Desk conv, got acts=%v skip=%q err=%v", actsFD, skipFD, err)
	}
	// Repeat tick in same minute slot skips with "already_triggered"
	actsDup, skipDup, err := TickScheduleOnce(vaultDir, "core/morning-brief", true, nextMorningDue.Add(20*time.Second))
	if err != nil || skipDup != "already_triggered" || len(actsDup) != 0 {
		t.Fatalf("expected duplicate same-minute tick to return skip='already_triggered', got acts=%v skip=%q err=%v", actsDup, skipDup, err)
	}

	// 5. Verify isolated_worker sleep-consolidate resolves custom config.yaml deep_routine model (claude-opus-5-5-high)
	actsSleep, skipSleep, err := TickScheduleOnce(vaultDir, "core/sleep-consolidate", true, sleepDue)
	if err != nil || skipSleep != "" || len(actsSleep) != 1 || !strings.Contains(actsSleep[0], "--model=claude-opus-5-5-high") {
		t.Fatalf("expected sleep-consolidate tick to resolve config.yaml deep_routine model claude-opus-5-5-high, got acts=%v skip=%q err=%v", actsSleep, skipSleep, err)
	}
}

func TestScheduleSidecarTemplatesAndTriggersYAMLHaveNoHardcodedModels(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))

	// 1. Check templates/triggers.yaml has zero hardcoded `model:` fields
	trigRaw, err := os.ReadFile(filepath.Join(repoRoot, "templates", "triggers.yaml"))
	if err != nil {
		t.Fatalf("read templates/triggers.yaml: %v", err)
	}
	for _, line := range strings.Split(string(trigRaw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "model:") || strings.HasPrefix(trimmed, "#   model:") {
			t.Fatalf("templates/triggers.yaml still contains hardcoded model field: %q", line)
		}
	}
}
