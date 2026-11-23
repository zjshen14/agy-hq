package pulse

import (
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/outbox"
)

func TestExecuteDecisionOptionsZeroToken(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	// Setup expired proposal + Goal with checklist item
	prop, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:  "messaging",
		Summary:     "Follow up on PR #55",
		Command:     "hq gchat send --to=alice@example.com --body=Ping",
		CommandArgv: []string{"hq", "gchat", "send", "--to=alice@example.com", "--body=Ping"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = approvals.ExpireStaleProposals(vaultDir, time.Now().Add(48*time.Hour))

	_, _ = kernel.AddGoalWithOptions(vaultDir, kernel.AddGoalOptions{Slug: "q4-launch", Title: "Q4 Launch", Scope: "engineering"}, now)
	_, _, _ = kernel.AddChecklistItem(vaultDir, "q4-launch", kernel.ChecklistItemOptions{ID: "chk-1", Title: "Optional benchmark"}, now)

	// Add outbox item with restage + skip + noisy decisions
	item, _, err := outbox.Add(vaultDir, outbox.OutboxItem{
		ID:          "act-1",
		SourceSkill: "pulse",
		Scope:       "engineering",
		Tier:        outbox.TierInterrupt,
		ProposalIDs: []string{prop.ID},
		Title:       "Expired draft + optional checklist",
		Summary:     "Choose action.",
		Decisions: []outbox.DecisionOption{
			{ID: "restage", Label: "Re-open Draft", Kind: outbox.ActionRestageProposal, ProposalID: prop.ID},
			{ID: "skip", Label: "Skip Benchmark", Kind: outbox.ActionSkipChecklist, GoalSlug: "q4-launch", ChecklistItemID: "chk-1", Reason: "Not needed"},
			{ID: "noisy", Label: "Mute Similar", Kind: outbox.ActionDismissNoisy},
			{ID: "inv", Label: "Investigate", Kind: outbox.ActionSpawnInvestigation, SkillSlug: "swe-pulse"},
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Execute restage
	r1, err := ExecuteDecision(vaultDir, item.ID, "restage", now)
	if err != nil || r1.NewProposalID == "" || r1.NewStatus != outbox.StatusActedHandled {
		t.Fatalf("restage failed: %+v (%v)", r1, err)
	}
	// 2. Execute skip_checklist
	r2, err := ExecuteDecision(vaultDir, item.ID, "skip", now)
	if err != nil || r2.NewStatus != outbox.StatusActedHandled {
		t.Fatalf("skip failed: %+v (%v)", r2, err)
	}
	goals, _ := kernel.ListGoals(vaultDir)
	if len(goals) != 1 || goals[0].Checklist[0].Status != kernel.ChecklistSkipped {
		t.Fatalf("expected checklist item skipped, got %+v", goals)
	}
	// 3. Execute spawn_investigation
	r3, err := ExecuteDecision(vaultDir, item.ID, "inv", now)
	if err != nil || r3.EventID == "" {
		t.Fatalf("spawn_investigation failed: %+v (%v)", r3, err)
	}
	// 4. Execute dismiss_noisy
	r4, err := ExecuteDecision(vaultDir, item.ID, "noisy", now)
	if err != nil || r4.NewStatus != outbox.StatusDismissed {
		t.Fatalf("dismiss_noisy failed: %+v (%v)", r4, err)
	}

	// 5. Dismissing a D6 expired-proposal decision card also dismisses the underlying expired proposal in interventions.jsonl
	prop2, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:  "messaging",
		Summary:     "Stale follow-up to dismiss",
		Command:     "hq gchat send --to=bob@example.com --body=Hi",
		CommandArgv: []string{"hq", "gchat", "send", "--to=bob@example.com", "--body=Hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = approvals.ExpireStaleProposals(vaultDir, time.Now().Add(72*time.Hour))
	item2, _, err := outbox.Add(vaultDir, outbox.OutboxItem{
		ID:          "act-exp-dismiss",
		SourceSkill: "pulse",
		Scope:       "approvals",
		Tier:        outbox.TierInterrupt,
		ProposalIDs: []string{prop2.ID},
		Title:       "Expired approval",
		Summary:     "Dismiss me",
		Decisions: []outbox.DecisionOption{
			{ID: "handled", Label: "No Longer Needed — Dismiss", Kind: outbox.ActionDismissHandled},
		},
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExecuteDecision(vaultDir, item2.ID, "handled", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("dismiss expired card: %v", err)
	}
	remExp, err := approvals.ListExpiredProposals(vaultDir, 0, time.Now().Add(73*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range remExp {
		if r.Proposal.ID == prop2.ID {
			t.Fatalf("expected prop2 %s to be cleared from ListExpiredProposals after dismissing decision card", prop2.ID)
		}
	}
}
