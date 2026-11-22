package pulse

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

func TestPulseDetectorsD1ThroughD10AndZeroTokenIdempotency(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)

	// Seed D1 calendar slot (starts in 20m)
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory", "pulse"), 0o700)
	calBytes, _ := json.Marshal([]CalendarSlot{
		{ID: "ev-1on1", Title: "1:1 Sync with Alice", StartAt: now.Add(20 * time.Minute).Format(time.RFC3339), Attendees: []string{"alice@example.com"}, Scope: "engineering"},
	})
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "pulse", "calendar_cache.json"), calBytes, 0o600)

	// Seed D10 stale draft (6h old)
	draftBytes, _ := json.Marshal([]DraftEntry{
		{ID: "dr-1", Connector: "gmail", Recipient: "bob@example.com", Subject: "Q4 Architecture Notes", UpdatedAt: now.Add(-6 * time.Hour).Format(time.RFC3339)},
	})
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "pulse", "drafts_cache.json"), draftBytes, 0o600)

	// Seed D2/D3/D5 unavailable window Goal + colliding deadline Goal + parked goal ready to wake
	_, err := kernel.AddGoalWithOptions(vaultDir, kernel.AddGoalOptions{
		Slug:                "release-freeze",
		Title:               "Q4 Release Freeze",
		Scope:               "engineering",
		WindowStartAt:       "2026-11-22",
		WindowEndAt:         "2026-11-26",
		Timezone:            "UTC",
		BlocksAvailability:  true,
		RequiredCoverageTag: "oncall",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = kernel.AddChecklistItem(vaultDir, "release-freeze", kernel.ChecklistItemOptions{
		ID:       "chk-rollout",
		Title:    "Freeze feature flags",
		Status:   kernel.ChecklistOpen,
		CutoffAt: now.Add(4 * time.Hour).Format(time.RFC3339),
	}, now)
	_, _, _ = kernel.AddChecklistItem(vaultDir, "release-freeze", kernel.ChecklistItemOptions{
		ID:            "chk-s1",
		Title:         "Canary batch A",
		Status:        kernel.ChecklistConfirmed,
		WindowStartAt: "2026-11-22T10:00:00Z",
		WindowEndAt:   "2026-11-22T11:00:00Z",
	}, now)
	_, _, _ = kernel.AddChecklistItem(vaultDir, "release-freeze", kernel.ChecklistItemOptions{
		ID:            "chk-s2",
		Title:         "Canary batch B",
		Status:        kernel.ChecklistConfirmed,
		WindowStartAt: "2026-11-22T11:15:00Z", // only 15m gap < 45m buffer!
		WindowEndAt:   "2026-11-22T12:00:00Z",
	}, now)

	// Colliding deadline Goal during freeze window (D3)
	_, _ = kernel.AddGoalWithOptions(vaultDir, kernel.AddGoalOptions{
		Slug:       "compliance-form",
		Title:      "Annual Compliance Signoff",
		Scope:      "ops",
		TargetDate: "2026-11-25",
	}, now)

	// Parked Goal whose date already passed (D8)
	_, _ = kernel.AddGoalWithOptions(vaultDir, kernel.AddGoalOptions{
		Slug:        "backlog-tidy",
		Title:       "Tidy Q3 Backlog",
		Scope:       "engineering",
		ParkedUntil: "2026-11-19",
	}, now)

	// Seed D6 expired proposal (>12h old!)
	prop, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:  "messaging",
		Summary:     "Send spec update to Alice",
		Command:     "hq gchat send --to=alice@example.com --body=Ready",
		CommandArgv: []string{"hq", "gchat", "send", "--to=alice@example.com", "--body=Ready"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = approvals.ExpireStaleProposals(vaultDir, time.Now().Add(48*time.Hour))

	// Seed D9 workspace with NeedsOwner: true
	_ = workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
		l.Put(workspace.LedgerEntry{
			Provider:    "core/git-worktree",
			WorkspaceID: "ws-pr1",
			Name:        "headquarter-foo",
			Root:        "/home/user/ws/headquarter-foo",
			Ownership:   workspace.OwnershipOwned,
			Status: workspace.LedgerStatus{
				State: workspace.StateClean,
				Changes: []workspace.ChangeRef{
					{ID: "PR#99", State: workspace.ChangeInReview, NeedsOwner: true, Title: "Address review comment"},
				},
			},
		})
		return true, nil
	})

	// Run precheck #1
	rep1, err := RunPrecheck(context.Background(), vaultDir, PrecheckOptions{Now: now})
	if err != nil {
		t.Fatalf("RunPrecheck 1: %v", err)
	}
	seenDet := make(map[string]bool)
	for _, f := range rep1.Findings {
		seenDet[f.Detector] = true
	}
	for _, want := range []string{
		"D1:pre_meeting_prep",
		"D2:horizon_countdown",
		"D2:cutoff_lead",
		"D3:blocking_window_ripple",
		"D5:interval_conflict",
		"D5:coverage_gap",
		"D6:proposal_expired",
		"D8:parked_horizon_wakeup",
		"D9:workspace_review_pulse",
		"D10:stale_unsent_draft",
	} {
		if !seenDet[want] {
			t.Errorf("expected detector %s to fire, got findings: %+v", want, rep1.Findings)
		}
	}
	_ = prop

	// Verify outbox persisted items with DecisionOptions
	items, err := outbox.LoadAll(vaultDir)
	if err != nil || len(items) < 8 {
		t.Fatalf("expected >=8 outbox items emitted, got %d (%v)", len(items), err)
	}

	// Run precheck #2 immediately -> 0 new emissions (idempotent 0-token tick!)
	rep2, err := RunPrecheck(context.Background(), vaultDir, PrecheckOptions{Now: now.Add(5 * time.Minute)})
	if err != nil {
		t.Fatalf("RunPrecheck 2: %v", err)
	}
	if rep2.EmittedCount != 0 {
		t.Fatalf("expected 0 new emissions on second identical tick, got %d: %+v", rep2.EmittedCount, rep2.Findings)
	}
}
