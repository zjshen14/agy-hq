package outbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBurstCoalescingIncidentCorrelationScopeRulesAndDecisions(t *testing.T) {
	vaultDir := t.TempDir()
	cfgYAML := `attention:
  mode: balanced
  cooldown_per_topic: "4h"
  coalesce_window: "3m"
  incident_correlation_window: "30m"
  scope_rules:
    - scopes: ["personal"]
      schedule: "weekdays 09:00-17:30 UTC"
      downgrade_non_critical_interrupt_to: "brief_rollup"
`
	if err := os.WriteFile(filepath.Join(vaultDir, "config.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := LoadAttentionConfig(vaultDir)
	if cfg.CoalesceWindow != 3*time.Minute || cfg.IncidentCorrelationWindow != 30*time.Minute || len(cfg.ScopeRules) != 1 {
		t.Fatalf("unexpected AttentionConfig: %+v", cfg)
	}

	// Friday 15:00 UTC -> personal downgraded by scope_rule, engineering stays interrupt
	fri := time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)
	pers, _, err := Add(vaultDir, OutboxItem{
		ID:          "pers-1",
		SourceSkill: "pulse",
		Scope:       "personal",
		Tier:        TierInterrupt,
		Title:       "Non-urgent errand",
		Summary:     "Can wait.",
	}, fri)
	if err != nil || pers.Tier != TierBriefRollup || pers.DowngradedBy != "scope_rule" {
		t.Fatalf("expected scope_rule downgrade, got %+v (err=%v)", pers, err)
	}

	// Two alerts with same IncidentKey within 3m -> coalesced into 1 pending interrupt held until +3m
	a1, cooled1, err := Add(vaultDir, OutboxItem{
		ID:          "burst-1",
		SourceSkill: "inbox-triage",
		Scope:       "engineering",
		IncidentKey: "sender:ci@example.com",
		Tier:        TierInterrupt,
		Title:       "Build step failed",
		Summary:     "Job lint failed.",
		Decisions: []DecisionOption{
			{ID: "handled", Label: "My Action — Dismiss", Kind: ActionDismissHandled, Recommended: true},
			{ID: "noisy", Label: "Mute Similar", Kind: ActionDismissNoisy},
		},
	}, fri)
	if err != nil || cooled1 || a1.AvailableForDispatchAt == "" {
		t.Fatalf("expected held burst-1, got %+v cooled=%v err=%v", a1, cooled1, err)
	}
	a2, cooled2, err := Add(vaultDir, OutboxItem{
		ID:          "burst-2",
		SourceSkill: "inbox-triage",
		Scope:       "engineering",
		IncidentKey: "sender:ci@example.com",
		Tier:        TierInterrupt,
		Title:       "Retry job also failed",
		Summary:     "Job test failed.",
	}, fri.Add(90*time.Second))
	if err != nil || !cooled2 || a2.ID != "burst-1" || len(a2.CoalescedItems) != 1 {
		t.Fatalf("expected burst-2 merged into burst-1, got %+v cooled=%v err=%v", a2, cooled2, err)
	}

	// Dispatch at +2m -> still held
	var deliveredCards []string
	delEarly, err := DispatchPendingInterrupts(vaultDir, fri.Add(2*time.Minute), func(it OutboxItem, card string) error {
		deliveredCards = append(deliveredCards, card)
		return nil
	})
	if err != nil || len(delEarly) != 0 {
		t.Fatalf("expected 0 delivered before coalesce window expires, got %d (%v)", len(delEarly), err)
	}

	// Dispatch at +3m05s -> delivers 1 grouped card with CoalescedItems & Decisions
	delReady, err := DispatchPendingInterrupts(vaultDir, fri.Add(3*time.Minute+5*time.Second), func(it OutboxItem, card string) error {
		deliveredCards = append(deliveredCards, card)
		return nil
	})
	if err != nil || len(delReady) != 1 || len(deliveredCards) != 1 {
		t.Fatalf("expected 1 grouped card delivered, got %d (%v)", len(delReady), err)
	}
	if !strings.Contains(deliveredCards[0], "Grouped Updates (+1") || !strings.Contains(deliveredCards[0], "[handled]") {
		t.Fatalf("expected grouped bullets and decision options in card:\n%s", deliveredCards[0])
	}

	// Follow-up 12m later -> correlated (#2)
	a3, cooled3, err := Add(vaultDir, OutboxItem{
		ID:          "burst-3",
		SourceSkill: "inbox-triage",
		Scope:       "engineering",
		IncidentKey: "sender:ci@example.com",
		Tier:        TierInterrupt,
		Critical:    true,
		Title:       "Production canary halted",
		Summary:     "Immediate rollback triggered.",
	}, fri.Add(12*time.Minute))
	if err != nil || cooled3 || a3.CorrelatedFromID != "burst-1" || a3.CorrelatedCount != 2 {
		t.Fatalf("expected correlated follow-up #2, got %+v cooled=%v err=%v", a3, cooled3, err)
	}
	card3 := FormatInterruptCard(a3)
	if !strings.Contains(card3, "Correlated Update (#2") {
		t.Fatalf("expected correlated header in card3:\n%s", card3)
	}

	// Ring-0 taint safety on DecisionOptions: tainted item cannot reference unrelated proposal
	_, _, err = Add(vaultDir, OutboxItem{
		SourceSkill: "inbox-triage",
		Tier:        TierInterrupt,
		Tainted:     true,
		ProposalIDs: []string{"prop-allowed"},
		Title:       "External email",
		Summary:     "Summary",
		Decisions: []DecisionOption{
			{ID: "bad", Label: "Approve Other", Kind: ActionApproveProposal, ProposalID: "prop-unrelated"},
		},
	}, fri.Add(13*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "unrelated proposal") {
		t.Fatalf("expected tainted cross-proposal rejection, got %v", err)
	}

	// Eval suite passes all 20 golden fixtures
	rep := RunEvalSuite(nil)
	if rep.TotalCases < 20 || !rep.AllThresholdsMet {
		t.Fatalf("expected >=20 passing eval cases, got %+v", rep)
	}
}
