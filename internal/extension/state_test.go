package extension

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSchedulePauseResumeAndDispatchResolve(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)

	fqid, ov, err := SetSchedulePaused(context.Background(), vaultDir, "morning-brief", true, "PTO", "none", now)
	if err != nil {
		t.Fatalf("SetSchedulePaused: %v", err)
	}
	if fqid != "core/morning-brief" || ov.Enabled == nil || *ov.Enabled {
		t.Fatalf("unexpected pause override: fqid=%q ov=%+v", fqid, ov)
	}

	st, err := LoadVaultState(vaultDir)
	if err != nil {
		t.Fatalf("LoadVaultState: %v", err)
	}
	if st.IsScheduleEnabled("morning-brief", true) || st.IsScheduleEnabled("core/morning-brief", true) {
		t.Fatal("expected morning-brief to report disabled via both bare ID and FQID")
	}

	_, _, err = SetSchedulePaused(context.Background(), vaultDir, "core/morning-brief", false, "", "last", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	st2, _ := LoadVaultState(vaultDir)
	if !st2.IsScheduleEnabled("morning-brief", true) {
		t.Fatal("expected morning-brief to be re-enabled")
	}

	// Verify skill override resolution and non-overridable inbox-triage invariant
	packDir := filepath.Join(t.TempDir(), "acme-workspace")
	skillFile := filepath.Join(packDir, "skills", "corp-morning-brief", "SKILL.md")
	_ = os.MkdirAll(filepath.Dir(skillFile), 0o755)
	_ = os.WriteFile(skillFile, []byte("# Corp Brief\n"), 0o644)

	err = UpdateVaultState(context.Background(), vaultDir, func(v *VaultState) error {
		v.Packs["acme-workspace"] = PackActivation{
			Enabled: true,
			Path:    packDir,
		}
		v.SkillOverrides["morning-brief"] = "acme-workspace/corp-morning-brief"
		v.SkillOverrides["inbox-triage"] = "acme-workspace/evil-triage"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateVaultState: %v", err)
	}

	rBrief := ResolveSkillDispatch(vaultDir, "morning-brief")
	if !rBrief.Overridden || rBrief.ActiveTarget != "acme-workspace/corp-morning-brief" || rBrief.SkillPath != skillFile {
		t.Fatalf("unexpected morning-brief dispatch: %+v", rBrief)
	}
	rTriage := ResolveSkillDispatch(vaultDir, "inbox-triage")
	if rTriage.Overridden || rTriage.ActiveTarget != "core/inbox-triage" {
		t.Fatalf("expected inbox-triage to remain non-overridable, got %+v", rTriage)
	}
}
