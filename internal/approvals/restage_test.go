package approvals

import (
	"errors"
	"testing"
	"time"
)

func TestRestageExpiredProposalAndThreadLookup(t *testing.T) {
	vaultDir := t.TempDir()

	p, err := CreateWithOptions(vaultDir, CreateOptions{
		ActionType:      "messaging",
		Summary:         "Reply to Alice on Q4 spec",
		Command:         "hq gchat send --to=alice@example.com --body=LGTM",
		CommandArgv:     []string{"hq", "gchat", "send", "--to=alice@example.com", "--body=LGTM"},
		TargetConnector: "gchat",
		TargetAccount:   "work",
		TargetThreadKey: "thr-spec-1",
		OriginSkill:     "inbox-triage",
	})
	if err != nil {
		t.Fatalf("CreateProposalWithOptions: %v", err)
	}

	// Expire it 48h after creation
	expTime := time.Now().Add(48 * time.Hour)
	expired, err := ExpireStaleProposals(vaultDir, expTime)
	if err != nil || len(expired) != 1 || expired[0].ID != p.ID {
		t.Fatalf("expected 1 expired proposal, got %+v (%v)", expired, err)
	}

	// Should be found regardless of 12h brief window
	found := FindUnrestagedExpiredOfferForThread(vaultDir, "gchat", "work", "thr-spec-1", 0, expTime.Add(24*time.Hour))
	if found == nil || found.Proposal.ID != p.ID {
		t.Fatalf("expected unrestaged expired offer for thr-spec-1, got %+v", found)
	}

	// Precondition failure blocks restaging
	prevHook := RegisteredApplyPrecondition
	defer func() { RegisteredApplyPrecondition = prevHook }()
	RegisteredApplyPrecondition = func(vd string, prop Proposal) error {
		return errors.New("new counterparty reply arrived")
	}
	if _, err := RestageExpiredProposal(vaultDir, p.ID, expTime.Add(25*time.Hour)); err == nil {
		t.Fatalf("expected precondition error to block restaging")
	}

	// Clear precondition -> restage succeeds and mints a fresh pending proposal
	RegisteredApplyPrecondition = nil
	fresh, err := RestageExpiredProposal(vaultDir, p.ID, expTime.Add(25*time.Hour))
	if err != nil {
		t.Fatalf("RestageExpiredProposal: %v", err)
	}
	if fresh.ID == p.ID || fresh.Metadata["restagedFrom"] != p.ID || fresh.TargetThreadKey != "thr-spec-1" {
		t.Fatalf("unexpected restaged proposal: %+v", fresh)
	}

	// Now no longer listed as unrestaged expired
	if still := FindUnrestagedExpiredOfferForThread(vaultDir, "gchat", "work", "thr-spec-1", 0, expTime.Add(26*time.Hour)); still != nil {
		t.Fatalf("expected expired offer cleared after restaging, got %+v", still)
	}

	// DismissExpiredProposal clears an expired proposal without restaging it
	p2, err := CreateWithOptions(vaultDir, CreateOptions{
		ActionType:  "messaging",
		Summary:     "Stale draft to dismiss",
		Command:     "hq gchat send --to=bob@example.com --body=Hi",
		CommandArgv: []string{"hq", "gchat", "send", "--to=bob@example.com", "--body=Hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = ExpireStaleProposals(vaultDir, time.Now().Add(72*time.Hour))
	if err := DismissExpiredProposal(vaultDir, p2.ID, "No longer needed", time.Now().Add(73*time.Hour)); err != nil {
		t.Fatalf("DismissExpiredProposal: %v", err)
	}
	rem, err := ListExpiredProposals(vaultDir, 0, time.Now().Add(74*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rem {
		if r.Proposal.ID == p2.ID {
			t.Fatalf("expected dismissed expired proposal %s to be excluded from ListExpiredProposals", p2.ID)
		}
	}
}
