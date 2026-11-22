package kernel

import (
	"testing"
	"time"
)

func TestGoalReadinessThreeStateAndParking(t *testing.T) {
	vaultDir := t.TempDir()
	baseNow := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

	trip, err := AddGoalWithOptions(vaultDir, AddGoalOptions{
		Slug:                "release-cutover",
		Title:               "Q4 Release Cutover",
		Scope:               "engineering",
		WindowStartAt:       "2026-11-22",
		WindowEndAt:         "2026-11-25",
		Timezone:            "America/Los_Angeles",
		BlocksAvailability:  true,
		RequiredCoverageTag: "oncall",
		SpaceSlug:           "q4-cutover",
	}, baseNow)
	if err != nil {
		t.Fatalf("AddGoalWithOptions: %v", err)
	}
	if !trip.BlocksAvailability || trip.SpaceSlug != "q4-cutover" {
		t.Fatalf("unexpected goal metadata: %+v", trip)
	}

	if _, _, err := AddChecklistItem(vaultDir, "release-cutover", ChecklistItemOptions{
		ID:     "chk-ci",
		Title:  "Green CI matrix",
		Tag:    "ci_gate",
		Status: ChecklistOpen,
	}, baseNow); err != nil {
		t.Fatalf("AddChecklistItem 1: %v", err)
	}
	if _, _, err := AddChecklistItem(vaultDir, "release-cutover", ChecklistItemOptions{
		ID:     "chk-docs",
		Title:  "Publish release notes",
		Tag:    "docs",
		Status: ChecklistOpen,
	}, baseNow); err != nil {
		t.Fatalf("AddChecklistItem 2: %v", err)
	}
	if _, _, err := AddChecklistItem(vaultDir, "release-cutover", ChecklistItemOptions{
		ID:     "chk-opt",
		Title:  "Optional blog post",
		Status: ChecklistOpen,
	}, baseNow); err != nil {
		t.Fatalf("AddChecklistItem 3: %v", err)
	}

	if _, _, err := ConfirmChecklistItem(vaultDir, "release-cutover", "chk-ci", "PR#101", "All green", baseNow); err != nil {
		t.Fatalf("ConfirmChecklistItem: %v", err)
	}
	g, skipped, err := SkipChecklistItem(vaultDir, "release-cutover", "chk-opt", "Deferred by owner", baseNow)
	if err != nil {
		t.Fatalf("SkipChecklistItem: %v", err)
	}
	if skipped.Status != ChecklistSkipped || skipped.SkipReason != "Deferred by owner" {
		t.Fatalf("unexpected skipped item: %+v", skipped)
	}
	prog := g.Readiness()
	if prog.Confirmed != 1 || prog.Open != 1 || prog.Skipped != 1 || prog.Active != 2 || prog.Percent != 50 {
		t.Fatalf("unexpected readiness progress: %+v", prog)
	}

	// Test "after:<goal-slug>" parking
	if _, err := AddGoalWithOptions(vaultDir, AddGoalOptions{
		Slug:        "post-release-cleanup",
		Title:       "Cleanup legacy flags",
		Scope:       "engineering",
		ParkedUntil: "after:release-cutover",
	}, baseNow); err != nil {
		t.Fatalf("AddGoalWithOptions parked: %v", err)
	}

	all, err := ListGoals(vaultDir)
	if err != nil {
		t.Fatalf("ListGoals: %v", err)
	}
	var parked Goal
	for _, x := range all {
		if x.Slug == "post-release-cleanup" {
			parked = x
		}
	}
	if !parked.IsParked(all, time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("expected goal parked during release-cutover window")
	}
	if parked.IsParked(all, time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("expected goal unparked after release-cutover window ends")
	}

	if _, err := UnparkGoal(vaultDir, "post-release-cleanup", baseNow); err != nil {
		t.Fatalf("UnparkGoal: %v", err)
	}
	all2, _ := ListGoals(vaultDir)
	for _, x := range all2 {
		if x.Slug == "post-release-cleanup" && x.ParkedUntil != "" {
			t.Fatalf("expected ParkedUntil cleared, got %q", x.ParkedUntil)
		}
	}
}
