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
	completed, _, _, err := FinalizeByConversation(vaultDir, "00000000-0000-0000-0000-000000000001", now.Add(20*time.Second))
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

func TestLeaseNextBatchesThreadCoalescingAndQuarantinePrompt(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC)

	// Enqueue 12 watcher events across 9 unique threads:
	// - thread-1 has 4 sibling events (including one placed after thread-9!)
	// - threads 2..9 have 1 event each.
	// LeaseNextBatches must cap at MaxBatchEvents (8) unique threads while coalescing ALL 4 events on thread-1
	// into batch 1 (total 11 events in batch 1, leaving thread-9 for batch 2).
	var batchInput []Event
	makeWatchEv := func(itemID, threadKey string) Event {
		return Event{
			Source:       "connector:gmail",
			DedupKey:     "watch:gmail:work:" + itemID,
			ScopeKey:     "watch:gmail:work",
			Skill:        "inbox-triage",
			Title:        "[Auto] Channel Triage (gmail:work · " + itemID + ")",
			Prompt:       "Triage " + itemID + " on thread=" + threadKey,
			TaintOnSpawn: true,
			Metadata: map[string]string{
				"requires_ack": "1",
				"connector":    "gmail",
				"account":      "work",
				"item_id":      itemID,
				"item_key":     itemID,
				"thread_key":   threadKey,
			},
		}
	}
	batchInput = append(batchInput,
		makeWatchEv("item-1a", "thread-1"),
		makeWatchEv("item-1b", "thread-1"),
		makeWatchEv("item-2", "thread-2"),
		makeWatchEv("item-3", "thread-3"),
		makeWatchEv("item-4", "thread-4"),
		makeWatchEv("item-5", "thread-5"),
		makeWatchEv("item-6", "thread-6"),
		makeWatchEv("item-7", "thread-7"),
		makeWatchEv("item-8", "thread-8"),
		makeWatchEv("item-9", "thread-9"),
		makeWatchEv("item-1c", "thread-1"),
		makeWatchEv("item-1d", "thread-1"),
	)
	if _, _, err := EnqueueBatch(vaultDir, batchInput, now); err != nil {
		t.Fatalf("EnqueueBatch: %v", err)
	}

	batches, _, err := LeaseNextBatches(vaultDir, now.Add(time.Second), 5*time.Minute)
	if err != nil || len(batches) != 1 {
		t.Fatalf("expected 1 leased batch, got %d (%v)", len(batches), err)
	}
	if len(batches[0].Events) != 11 {
		t.Fatalf("expected 11 coalesced events across 8 unique threads in batch 1, got %d", len(batches[0].Events))
	}
	prompt := batches[0].Prompt
	if !strings.Contains(prompt, "hq watch read --connector=gmail --account=work --thread=thread-1,thread-2,thread-3,thread-4,thread-5,thread-6,thread-7,thread-8") {
		t.Fatalf("expected single multi-thread hq watch read command in batch prompt, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "### Thread 1/8 (`thread=thread-1` · items: `item-1a,item-1b,item-1c,item-1d`") {
		t.Fatalf("expected coalesced Thread 1/8 section listing all 4 item IDs, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "thread-9") {
		t.Fatalf("thread-9 (9th unique thread) must remain pending for next batch, got:\n%s", prompt)
	}
}

func TestSpawnTokenHashClearedOnLeaseExpiryDeferralAndRetry(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)

	// 1. Expired lease drop/requeue clears SpawnTokenHash so the stale token cannot re-bind after expiry.
	ev1, _, err := Enqueue(vaultDir, Event{
		Source:       "watcher",
		ScopeKey:     "watch:mail:work",
		Skill:        "inbox-triage",
		Title:        "[Auto] Expiry Token Test",
		Prompt:       "Triage thread",
		TaintOnSpawn: true,
	}, now)
	if err != nil {
		t.Fatalf("Enqueue ev1: %v", err)
	}
	batches1, _, err := LeaseNextBatches(vaultDir, now, 5*time.Minute)
	if err != nil || len(batches1) != 1 || batches1[0].SpawnToken == "" {
		t.Fatalf("LeaseNextBatches 1: batches=%+v err=%v", batches1, err)
	}
	tok1 := batches1[0].SpawnToken

	afterExpiry := now.Add(6 * time.Minute)
	batches2, dropped, err := LeaseNextBatches(vaultDir, afterExpiry, 5*time.Minute)
	if err != nil || len(dropped) != 0 || len(batches2) != 0 {
		// Wait: on first expiry it sets NextRetryAt=+5s, so verify SpawnTokenHash is cleared before requeue dispatch.
		t.Fatalf("expected ev1 in retry backoff with 0 immediate leases, got batches=%+v dropped=%v err=%v", batches2, dropped, err)
	}
	list1, err := List(vaultDir, "", false, 0)
	if err != nil || len(list1) != 1 || list1[0].ID != ev1.ID {
		t.Fatalf("List after expiry: %+v (%v)", list1, err)
	}
	if list1[0].SpawnTokenHash != "" {
		t.Fatalf("expected SpawnTokenHash cleared on lease expiry requeue, got %q", list1[0].SpawnTokenHash)
	}
	if bound, ok, bErr := BindBatchByToken(vaultDir, tok1, "11111111-1111-4111-8111-111111111111", afterExpiry, 5*time.Minute); bErr != nil || ok || len(bound) != 0 {
		t.Fatalf("expected stale token to fail BindBatchByToken after expiry, got ok=%v len=%d err=%v", ok, len(bound), bErr)
	}

	// Test DeferBatchOnSpawnError and Retry (both active leased and archived dropped) clear SpawnTokenHash.
	vault2 := t.TempDir()
	ev2, _, err := Enqueue(vault2, Event{
		Source:      "watcher",
		ScopeKey:    "watch:mail:defer",
		Title:       "[Auto] Defer Token Test",
		Prompt:      "Triage defer",
		MaxAttempts: 2,
	}, now)
	if err != nil {
		t.Fatalf("Enqueue ev2: %v", err)
	}
	bDef, _, err := LeaseNextBatches(vault2, now, 5*time.Minute)
	if err != nil || len(bDef) != 1 || bDef[0].SpawnToken == "" {
		t.Fatalf("LeaseNextBatches ev2: %+v (%v)", bDef, err)
	}
	tokDef := bDef[0].SpawnToken
	if _, dErr := DeferBatchOnSpawnError(vault2, []string{ev2.ID}, "step1 error", false, now); dErr != nil {
		t.Fatalf("DeferBatchOnSpawnError: %v", dErr)
	}
	list2, err := List(vault2, "", false, 0)
	if err != nil || len(list2) != 1 || list2[0].SpawnTokenHash != "" {
		t.Fatalf("expected SpawnTokenHash cleared after DeferBatchOnSpawnError, got %+v (%v)", list2, err)
	}
	if _, ok, _ := BindBatchByToken(vault2, tokDef, "22222222-2222-4222-8222-222222222222", now, 5*time.Minute); ok {
		t.Fatalf("expected old token invalid after DeferBatchOnSpawnError")
	}

	// Lease again -> Retry active leased event directly -> SpawnTokenHash cleared.
	curT := now.Add(time.Minute)
	bAct, _, err := LeaseNextBatches(vault2, curT, 5*time.Minute)
	if err != nil || len(bAct) != 1 || bAct[0].SpawnToken == "" {
		t.Fatalf("LeaseNextBatches active retry: %+v (%v)", bAct, err)
	}
	requeuedAct, rErr := Retry(vault2, ev2.ID, curT.Add(time.Second))
	if rErr != nil || requeuedAct.SpawnTokenHash != "" {
		t.Fatalf("expected active Retry to clear SpawnTokenHash, got %+v (%v)", requeuedAct, rErr)
	}
	if _, ok, _ := BindBatchByToken(vault2, bAct[0].SpawnToken, "33333333-3333-4333-8333-333333333333", curT.Add(2*time.Second), 5*time.Minute); ok {
		t.Fatalf("expected old token invalid after active Retry")
	}

	// Exhaust MaxAttempts so ev2 drops to archive -> Retry archived event -> SpawnTokenHash cleared.
	for i := 0; i < DefaultMaxAttempts; i++ {
		curT = curT.Add(time.Minute)
		bNext, _, _ := LeaseNextBatches(vault2, curT, 5*time.Minute)
		if len(bNext) == 0 {
			break
		}
		dropped, _ := DeferBatchOnSpawnError(vault2, bNext[0].EventIDs(), "err", false, curT)
		if len(dropped) > 0 {
			break
		}
	}
	requeued, rErr := Retry(vault2, ev2.ID, curT.Add(time.Minute))
	if rErr != nil || requeued.ID != ev2.ID || requeued.SpawnTokenHash != "" {
		t.Fatalf("Retry archived event: got %+v err=%v", requeued, rErr)
	}
	listAfterRetry, err := List(vault2, "", false, 0)
	if err != nil || len(listAfterRetry) != 1 || listAfterRetry[0].SpawnTokenHash != "" {
		t.Fatalf("expected SpawnTokenHash cleared after archived Retry(), got %+v (%v)", listAfterRetry, err)
	}
}
