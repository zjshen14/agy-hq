package events

import (
	"strings"
	"testing"
	"time"
)

func TestEventInboxSerializationBatchingAndCrashRetry(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)

	// 1. Enqueue 2 events sharing ScopeKey="skill:inbox-triage" and 1 duplicate by DedupKey
	ev1, dedup1, err := Enqueue(vaultDir, Event{
		Source:   "gmail",
		DedupKey: "gmail:batch-1",
		ScopeKey: "skill:inbox-triage",
		Skill:    "inbox-triage",
		Title:    "[Auto] Gmail Triage 1",
		Prompt:   "Process thread A",
	}, now)
	if err != nil || dedup1 {
		t.Fatalf("Enqueue ev1: err=%v dedup=%v", err, dedup1)
	}
	if _, dedupDup, err := Enqueue(vaultDir, Event{
		Source:   "gmail",
		DedupKey: "gmail:batch-1",
		ScopeKey: "skill:inbox-triage",
		Skill:    "inbox-triage",
		Title:    "[Auto] Gmail Triage 1 dup",
		Prompt:   "Process thread A",
	}, now); err != nil || !dedupDup {
		t.Fatalf("expected duplicate DedupKey to be suppressed, got err=%v dedup=%v", err, dedupDup)
	}

	ev2, dedup2, err := Enqueue(vaultDir, Event{
		Source:   "gchat",
		DedupKey: "gchat:batch-2",
		ScopeKey: "skill:inbox-triage",
		Skill:    "inbox-triage",
		Title:    "[Auto] GChat Triage 2",
		Prompt:   "Process space B",
	}, now.Add(time.Second))
	if err != nil || dedup2 {
		t.Fatalf("Enqueue ev2: err=%v dedup=%v", err, dedup2)
	}

	// 2. LeaseNextBatches batches ev1 + ev2 into 1 LeasedBatch
	batches, dropped, err := LeaseNextBatches(vaultDir, now.Add(2*time.Second), 5*time.Minute)
	if err != nil || len(dropped) != 0 || len(batches) != 1 {
		t.Fatalf("expected 1 batched lease, got batches=%d dropped=%d err=%v", len(batches), len(dropped), err)
	}
	if len(batches[0].Events) != 2 || !strings.Contains(batches[0].Prompt, "Batched Events (2)") {
		t.Fatalf("expected 2 coalesced events in prompt, got: %+v", batches[0])
	}

	// 3. Enqueue a 3rd event in the same busy ScopeKey while batch 1 is leased -> must NOT spawn competing run!
	ev3, _, err := Enqueue(vaultDir, Event{
		Source:   "inbox_drop",
		DedupKey: "drop:invoice.pdf",
		ScopeKey: "skill:inbox-triage",
		Skill:    "inbox-triage",
		Title:    "[Auto] Inbox Drop invoice.pdf",
		Prompt:   "Process invoice.pdf",
	}, now.Add(5*time.Second))
	if err != nil {
		t.Fatalf("Enqueue ev3: %v", err)
	}
	competing, _, err := LeaseNextBatches(vaultDir, now.Add(6*time.Second), 5*time.Minute)
	if err != nil || len(competing) != 0 {
		t.Fatalf("expected 0 competing batches while ScopeKey is leased, got %d (%v)", len(competing), err)
	}

	// 4. Bind batch 1 to conv-0001 and complete on Stop -> unblocks ev3!
	if err := BindBatchConversation(vaultDir, []string{ev1.ID, ev2.ID}, "00000000-0000-0000-0000-000000000001", now.Add(10*time.Second), 5*time.Minute); err != nil {
		t.Fatalf("BindBatchConversation: %v", err)
	}
	completed, err := CompleteByConversation(vaultDir, "00000000-0000-0000-0000-000000000001", now.Add(20*time.Second))
	if err != nil || len(completed) != 2 {
		t.Fatalf("expected 2 completed events, got %d (%v)", len(completed), err)
	}

	// Archived DedupKey must still suppress re-enqueue within 7d window
	if _, dedupAfterDone, _ := Enqueue(vaultDir, Event{
		Source:   "gmail",
		DedupKey: "gmail:batch-1",
		ScopeKey: "skill:inbox-triage",
		Title:    "Replay",
		Prompt:   "Replay",
	}, now.Add(30*time.Second)); !dedupAfterDone {
		t.Fatalf("expected completed DedupKey within archive window to deduplicate")
	}

	// 5. Now ev3 leases cleanly; simulate CircuitOpen quota deferral (must NOT burn AttemptCount)
	nextBatches, _, err := LeaseNextBatches(vaultDir, now.Add(31*time.Second), 5*time.Minute)
	if err != nil || len(nextBatches) != 1 || nextBatches[0].Events[0].ID != ev3.ID {
		t.Fatalf("expected ev3 to lease after scope release, got %+v (%v)", nextBatches, err)
	}
	if _, err := DeferBatchOnSpawnError(vaultDir, []string{ev3.ID}, "quota circuit breaker is OPEN", true, now.Add(32*time.Second)); err != nil {
		t.Fatalf("DeferBatchOnSpawnError: %v", err)
	}
	inspected, err := Inspect(vaultDir, ev3.ID)
	if err != nil || inspected.Status != StatusPending || inspected.AttemptCount != 0 {
		t.Fatalf("expected quota-deferred event to stay pending with AttemptCount=0, got %+v (%v)", inspected, err)
	}

	// 6. Lease ev3 again and simulate mid-run worker crash (lease expires after 5m -> reaped & retried)
	tLease := now.Add(2 * time.Minute)
	leasedAgain, _, err := LeaseNextBatches(vaultDir, tLease, 2*time.Minute)
	if err != nil || len(leasedAgain) != 1 {
		t.Fatalf("expected ev3 re-lease after 30s quota hold, got %d (%v)", len(leasedAgain), err)
	}
	// Advance past LeaseExpiresAt (+2m) + 1st retry backoff (+5s)
	tAfterCrash := tLease.Add(2*time.Minute + 1*time.Second)
	_, _, _ = LeaseNextBatches(vaultDir, tAfterCrash, 2*time.Minute) // triggers reap -> AvailableAt = +5s
	retried, _, err := LeaseNextBatches(vaultDir, tAfterCrash.Add(6*time.Second), 2*time.Minute)
	if err != nil || len(retried) != 1 || retried[0].Events[0].AttemptCount != 1 {
		t.Fatalf("expected crashed worker event to retry with AttemptCount=1, got %+v (%v)", retried, err)
	}
}

func TestDaemonTokenConstantTimeValidation(t *testing.T) {
	vaultDir := t.TempDir()
	tok, err := EnsureDaemonToken(vaultDir)
	if err != nil || len(tok) != 64 {
		t.Fatalf("EnsureDaemonToken: tok=%q err=%v", tok, err)
	}
	if !ValidateDaemonToken(vaultDir, tok) {
		t.Fatalf("expected valid daemon token to pass")
	}
	if ValidateDaemonToken(vaultDir, "bad-token") {
		t.Fatalf("expected invalid token to fail")
	}
}
