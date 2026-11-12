package outbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAttentionQuietHoursCooldownAndIdeasLifecycle(t *testing.T) {
	vaultDir := t.TempDir()
	cfgYAML := `attention:
  mode: balanced
  quiet_hours:
    enabled: true
    start: "22:00"
    end: "07:30"
    timezone: "UTC"
    allow_critical_breach: true
  cooldown_per_topic: "4h"
  coalesce_window: "15m"
`
	if err := os.WriteFile(filepath.Join(vaultDir, "config.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := LoadAttentionConfig(vaultDir)
	if !cfg.QuietHours.Enabled || cfg.CooldownPerTopic != 4*time.Hour || cfg.CoalesceWindow != 15*time.Minute {
		t.Fatalf("unexpected parsed AttentionConfig: %+v", cfg)
	}

	// 1. Non-critical interrupt at 23:15 UTC (inside quiet hours) -> downgraded to brief_rollup
	nightTime := time.Date(2026, 11, 12, 23, 15, 0, 0, time.UTC)
	it1, cooled, err := Add(vaultDir, OutboxItem{
		ID:          "item-1",
		SourceSkill: "thesis-check",
		Scope:       "ACME",
		Tier:        TierInterrupt,
		Title:       "ACME 8-K Minor Update",
		Summary:     "Routine exhibit filing; thesis WATCH.",
	}, nightTime)
	if err != nil || cooled {
		t.Fatalf("Add item-1 failed: err=%v cooled=%v", err, cooled)
	}
	if it1.Tier != TierBriefRollup || it1.DowngradedBy != "quiet_hours" {
		t.Fatalf("expected quiet_hours downgrade to brief_rollup, got: %+v", it1)
	}

	// 2. Critical breach at 23:20 UTC -> allowed to remain TierInterrupt
	itCrit, _, err := Add(vaultDir, OutboxItem{
		ID:          "item-crit",
		SourceSkill: "thesis-check",
		Scope:       "ACME",
		Tier:        TierInterrupt,
		Critical:    true,
		Title:       "ACME Kill Criterion Breached",
		Summary:     "Gross margin fell 620 bps below floor.",
	}, nightTime.Add(5*time.Minute))
	if err != nil || itCrit.Tier != TierInterrupt {
		t.Fatalf("expected critical breach to bypass quiet hours, got: %+v (err=%v)", itCrit, err)
	}

	// 3. Duplicate topic within 4h cooldown -> suppressed to silent_ledger
	itDup, cooledDup, err := Add(vaultDir, OutboxItem{
		ID:          "item-dup",
		SourceSkill: "thesis-check",
		Scope:       "ACME",
		Tier:        TierBriefRollup,
		Title:       "ACME 8-K Minor Update",
		Summary:     "Duplicate alert within 4h.",
	}, nightTime.Add(30*time.Minute))
	if err != nil || !cooledDup || itDup.Tier != TierSilentLedger {
		t.Fatalf("expected duplicate topic within 4h to hit cooldown, got cooled=%v item=%+v err=%v", cooledDup, itDup, err)
	}

	// 4. DrainBriefRollup returns item-1 once and marks it delivered
	drained, err := DrainBriefRollup(vaultDir, nightTime.Add(8*time.Hour))
	if err != nil || len(drained) != 1 || drained[0].ID != "item-1" {
		t.Fatalf("expected 1 brief_rollup drained, got %+v (err=%v)", drained, err)
	}
	drainedAgain, _ := DrainBriefRollup(vaultDir, nightTime.Add(9*time.Hour))
	if len(drainedAgain) != 0 {
		t.Fatalf("expected second DrainBriefRollup to be empty, got %d", len(drainedAgain))
	}

	// 5. Ideas lifecycle: add, snooze, wake after snooze, promote, dismiss, and RSI stats
	dayTime := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	idea, _, err := Add(vaultDir, OutboxItem{
		ID:            "idea-1",
		SourceSkill:   "swe-pulse",
		Scope:         "headquarter",
		Tier:          TierIdeasFeed,
		Title:         "Clean up stale feature branch",
		Summary:       "Branch hq/old-demo is 14 days behind main.",
		SuggestedSlug: "hq-cleanup-demo",
	}, dayTime)
	if err != nil {
		t.Fatalf("Add idea-1: %v", err)
	}
	if _, err := SnoozeIdea(vaultDir, idea.ID, 2*time.Hour, dayTime); err != nil {
		t.Fatalf("SnoozeIdea: %v", err)
	}
	hidden, _ := List(vaultDir, TierIdeasFeed, StatusPending, dayTime.Add(1*time.Hour))
	if len(hidden) != 0 {
		t.Fatalf("expected snoozed idea to be hidden at +1h, got %d", len(hidden))
	}
	woken, _ := List(vaultDir, TierIdeasFeed, StatusPending, dayTime.Add(3*time.Hour))
	if len(woken) != 1 || woken[0].ID != "idea-1" {
		t.Fatalf("expected snoozed idea to wake at +3h, got %+v", woken)
	}
	if promoted, err := PromoteIdea(vaultDir, idea.ID, dayTime.Add(3*time.Hour)); err != nil || promoted.Status != StatusPromoted {
		t.Fatalf("PromoteIdea failed: %+v (%v)", promoted, err)
	}

	// Seed 5 dismissed ideas for noisy-skill to verify ComputeSkillDismissalStats
	for i := 1; i <= 5; i++ {
		id := "noisy-" + string(rune('0'+i))
		_, _, _ = Add(vaultDir, OutboxItem{
			ID:          id,
			SourceSkill: "inbox-triage",
			Tier:        TierIdeasFeed,
			Title:       "Noisy newsletter suggestion " + id,
			Summary:     "Archive promo digest",
		}, dayTime.Add(time.Duration(i)*time.Minute))
		_, _ = DismissIdea(vaultDir, id, "Not actionable promo noise", dayTime.Add(time.Duration(i+10)*time.Minute))
	}
	stats, err := ComputeSkillDismissalStats(vaultDir)
	if err != nil {
		t.Fatalf("ComputeSkillDismissalStats: %v", err)
	}
	foundNoisy := false
	for _, st := range stats {
		if st.Skill == "inbox-triage" && st.TotalResolved == 5 && st.DismissRate > 0.99 {
			foundNoisy = true
		}
	}
	if !foundNoisy {
		t.Fatalf("expected inbox-triage with 100%% dismissal rate across 5 items, got: %+v", stats)
	}
}
