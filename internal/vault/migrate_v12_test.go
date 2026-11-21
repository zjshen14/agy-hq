package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateV11ToV12_MorningBriefCompactBeforeRun(t *testing.T) {
	// Case 1: Unmodified v11 morning-brief front_desk_cron gets compact_before_run: true.
	vDir1 := t.TempDir()
	_ = os.MkdirAll(filepath.Join(vDir1, "memory"), 0o755)
	_ = os.WriteFile(filepath.Join(vDir1, "memory", "schema_version.json"), []byte(`{"version":11}`), 0o644)
	v11Trig := `schedules:
  - id: morning-brief
    cron: "CRON_TZ=America/Los_Angeles 30 7 * * *"
    mode: front_desk_cron
    tier: routine
    delivery: interrupt
    skill: morning-brief
  - id: sleep-consolidate
    cron: "CRON_TZ=America/Los_Angeles 30 23 * * *"
    mode: daemon_gatekeeper
    tier: deep_routine
    precheck_cmd: ["hq", "memory", "dream-plan", "--precheck"]
    dedup_mode: exit_zero_nonempty
    delivery: silent_ledger
    skill: sleep-consolidate
`
	_ = os.WriteFile(filepath.Join(vDir1, "triggers.yaml"), []byte(v11Trig), 0o600)

	rep1, err := Migrate(vDir1, "")
	if err != nil {
		t.Fatalf("Migrate Case 1: %v", err)
	}
	if rep1.PreviousVersion != 11 || rep1.CurrentVersion != CurrentSchemaVersion {
		t.Fatalf("unexpected schema versions: %+v", rep1)
	}
	after1, _ := os.ReadFile(filepath.Join(vDir1, "triggers.yaml"))
	if !strings.Contains(string(after1), "    compact_before_run: true\n") {
		t.Fatalf("expected v12 migration to insert compact_before_run: true on morning-brief, got:\n%s", string(after1))
	}

	// Case 2: User explicitly opted out with compact_before_run: false -> migration must preserve it.
	vDir2 := t.TempDir()
	_ = os.MkdirAll(filepath.Join(vDir2, "memory"), 0o755)
	_ = os.WriteFile(filepath.Join(vDir2, "memory", "schema_version.json"), []byte(`{"version":11}`), 0o644)
	customTrig := strings.Replace(v11Trig, "    delivery: interrupt\n", "    compact_before_run: false\n    delivery: interrupt\n", 1)
	_ = os.WriteFile(filepath.Join(vDir2, "triggers.yaml"), []byte(customTrig), 0o600)

	if _, err := Migrate(vDir2, ""); err != nil {
		t.Fatalf("Migrate Case 2: %v", err)
	}
	after2, _ := os.ReadFile(filepath.Join(vDir2, "triggers.yaml"))
	if !strings.Contains(string(after2), "compact_before_run: false") || strings.Contains(string(after2), "compact_before_run: true") {
		t.Fatalf("expected explicit user compact_before_run: false preserved, got:\n%s", string(after2))
	}
}
