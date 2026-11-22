package kernel

import (
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/timers"
)

func TestCancelTargetEventsTimersProposalsAndActivityLog(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)

	ev, _, err := events.Enqueue(vaultDir, events.Event{
		Source:   "decision_action",
		ScopeKey: "scope:release-prep",
		Skill:    "swe-pulse",
		Title:    "Check release-prep blockers",
		Prompt:   "Inspect release-prep",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	tm, err := timers.SetFollowUp(vaultDir, timers.FollowUpRequest{
		InDuration: 30 * time.Minute,
		ScopeKey:   "scope:release-prep",
		Title:      "Follow up on release-prep",
		Prompt:     "Ping",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	prop, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:  "messaging",
		Summary:     "Send release-prep update",
		Command:     "hq gchat send --to=team@example.com --body=Update",
		CommandArgv: []string{"hq", "gchat", "send", "--to=team@example.com", "--body=Update"},
	})
	if err != nil {
		t.Fatal(err)
	}

	rep, err := CancelTarget(vaultDir, "release-prep", "User changed plans", now)
	if err != nil {
		t.Fatalf("CancelTarget: %v", err)
	}
	if rep.TotalCancelled != 3 || len(rep.CancelledEvents) != 1 || len(rep.CancelledTimers) != 1 || len(rep.CancelledProposals) != 1 {
		t.Fatalf("expected 3 items cancelled (ev=%s tm=%s prop=%s), got %+v", ev.ID, tm.ID, prop.ID, rep)
	}

	act := LoadRecentActivity(vaultDir, 5)
	if len(act) != 1 || act[0].Kind != "reverted_by_user" || len(act[0].Cancelled) != 3 {
		t.Fatalf("expected 1 reverted_by_user activity record, got %+v", act)
	}
}
