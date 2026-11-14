package timers

import (
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/events"
)

func TestFollowUpAndScheduleTimersFireIntoEventInbox(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)

	// 1. Horizon < 1m must be rejected
	if _, err := SetFollowUp(vaultDir, FollowUpRequest{
		InDuration: 15 * time.Second,
		Prompt:     "Too fast",
	}, now); err == nil {
		t.Fatalf("expected <1m follow-up horizon to be rejected")
	}

	// 2. Valid agent-set follow-up resuming session conv-0001
	tm, err := SetFollowUp(vaultDir, FollowUpRequest{
		InDuration:      20 * time.Minute,
		ScopeKey:        "session:00000000-0000-0000-0000-000000000001",
		SessionID:       "00000000-0000-0000-0000-000000000001",
		Skill:           "swe-pulse",
		Title:           "Check staging deploy",
		Prompt:          "Verify staging healthcheck green",
		CreatedByConvID: "00000000-0000-0000-0000-000000000001",
	}, now)
	if err != nil {
		t.Fatalf("SetFollowUp: %v", err)
	}

	// 3. Before DueAt (+10m): 0 events fired
	dueEarly, err := EvaluateDue(vaultDir, now.Add(10*time.Minute))
	if err != nil || len(dueEarly) != 0 {
		t.Fatalf("expected 0 fired timers at +10m, got %d (%v)", len(dueEarly), err)
	}

	// 4. At DueAt (+21m): follow-up fires into Event Inbox and transitions to done
	fired, err := EvaluateDue(vaultDir, now.Add(21*time.Minute))
	if err != nil || len(fired) != 1 {
		t.Fatalf("expected 1 fired follow-up event, got %d (%v)", len(fired), err)
	}
	if fired[0].SessionID != "00000000-0000-0000-0000-000000000001" || fired[0].Source != "timer" {
		t.Fatalf("unexpected fired event: %+v", fired[0])
	}
	armedAfter, _ := List(vaultDir, false)
	if len(armedAfter) != 0 {
		t.Fatalf("expected 0 armed follow-up timers after firing, got %d (%+v)", len(armedAfter), armedAfter)
	}
	allAfter, _ := List(vaultDir, true)
	if len(allAfter) != 1 || allAfter[0].ID != tm.ID || allAfter[0].Status != StatusDone {
		t.Fatalf("expected follow-up timer marked done, got %+v", allAfter)
	}

	// Verify event is sitting in internal/events as pending
	evList, err := events.List(vaultDir, "pending", false, 0)
	if err != nil || len(evList) != 1 {
		t.Fatalf("expected 1 pending event in Event Inbox, got %d (%v)", len(evList), err)
	}

	// 5. Recurring Schedule timer advances DueAt instead of marking done
	sched, err := UpsertSchedule(vaultDir, "hourly-pulse", "0 * * * *", "skill:swe-pulse", "swe-pulse", "flash", "[Auto] Hourly Pulse", "Check open PRs", now)
	if err != nil || sched.DueAt != "2026-11-14T16:00:00Z" {
		t.Fatalf("UpsertSchedule unexpected DueAt: %+v (%v)", sched, err)
	}
	firedSched, err := EvaluateDue(vaultDir, time.Date(2026, 11, 14, 16, 0, 5, 0, time.UTC))
	if err != nil || len(firedSched) != 1 {
		t.Fatalf("expected schedule timer to fire at 16:00:05Z, got %d (%v)", len(firedSched), err)
	}
	armedSched, _ := List(vaultDir, false)
	if len(armedSched) != 1 || armedSched[0].DueAt != "2026-11-14T17:00:00Z" {
		t.Fatalf("expected schedule timer to advance DueAt to 17:00:00Z, got %+v", armedSched)
	}
}
