package kernel

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/connector"
)

func TestBuildMonitorHealthSummaryDeadManSwitch(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 24, 8, 20, 0, 0, time.UTC)

	// Record live daemon PID (current test process) and a recent heartbeat
	if err := RecordDaemonStart(vaultDir, "127.0.0.1:9999"); err != nil {
		t.Fatalf("RecordDaemonStart: %v", err)
	}
	t.Cleanup(func() { _ = RecordDaemonStop(vaultDir) })
	if err := UpdateQuota(vaultDir, func(st *QuotaState) (bool, error) {
		st.LastHeartbeatAt = now.Add(-5 * time.Minute).Format(time.RFC3339)
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateQuota: %v", err)
	}

	// Healthy gmail watermark @ 08:15
	gmailWM := connector.LoadAccountWatermark(vaultDir, "gmail", "primary")
	gmailWM.RecordPrecheckSuccess(now.Add(-5 * time.Minute))
	if err := connector.SaveAccountWatermark(vaultDir, "gmail", gmailWM); err != nil {
		t.Fatalf("save gmail watermark: %v", err)
	}

	// Auth-expired gchat watermark (last ok 22:10)
	gchatWM := connector.LoadAccountWatermark(vaultDir, "gchat", "personal")
	gchatWM.RecordPrecheckSuccess(time.Date(2026, 11, 23, 22, 10, 0, 0, time.UTC))
	gchatWM.RecordPrecheckFailure(errors.New("OAuth token expired (401)"), now.Add(-2*time.Minute))
	if err := connector.SaveAccountWatermark(vaultDir, "gchat", gchatWM); err != nil {
		t.Fatalf("save gchat watermark: %v", err)
	}

	sum := BuildMonitorHealthSummary(vaultDir, now)
	if !sum.DaemonAlive || sum.HeartbeatStale {
		t.Fatalf("expected live non-stale daemon, got %+v", sum)
	}
	line := sum.FormatLine(now)
	for _, want := range []string{"daemon: alive (heartbeat @ 08:15)", "gmail: ok @ 08:15", "gchat: AUTH_EXPIRED (last ok 22:10)"} {
		if !strings.Contains(line, want) {
			t.Fatalf("expected %q in health line %q", want, line)
		}
	}

	// Stale heartbeat detection (>30m old while daemon alive)
	staleSum := BuildMonitorHealthSummary(vaultDir, now.Add(45*time.Minute))
	if !staleSum.HeartbeatStale || !strings.Contains(staleSum.FormatLine(now.Add(45*time.Minute)), "STALE") {
		t.Fatalf("expected stale heartbeat warning, got %+v (%q)", staleSum, staleSum.FormatLine(now.Add(45*time.Minute)))
	}
}
