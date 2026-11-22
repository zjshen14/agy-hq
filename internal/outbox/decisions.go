package outbox

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Closed enum of safe 1-tap proactive DecisionOption action kinds (RFC-0027).
// Raw shell commands are structurally absent from the schema.
const (
	ActionApproveProposal    = "approve_proposal"
	ActionRejectProposal     = "reject_proposal"
	ActionRestageProposal    = "restage_proposal"
	ActionParkUntil          = "park_until"
	ActionDismissHandled     = "dismiss_handled"
	ActionDismissNoisy       = "dismiss_noisy"
	ActionSkipChecklist      = "skip_checklist"
	ActionPromoteGoal        = "promote_goal"
	ActionSpawnInvestigation = "spawn_investigation"
)

const MaxDecisionOptions = 4

// DecisionOption declares one safe 1-tap button on an OutboxItem.
type DecisionOption struct {
	ID              string `json:"id"`
	Label           string `json:"label"`
	Kind            string `json:"kind"`
	ProposalID      string `json:"proposalId,omitempty"`
	ProposalSHA     string `json:"proposalSha,omitempty"`
	GoalSlug        string `json:"goalSlug,omitempty"`
	ChecklistItemID string `json:"checklistItemId,omitempty"`
	ParkHorizon     string `json:"parkHorizon,omitempty"`
	SkillSlug       string `json:"skillSlug,omitempty"`
	Scope           string `json:"scope,omitempty"`
	Reason          string `json:"reason,omitempty"`
	Recommended     bool   `json:"recommended,omitempty"`
}

var (
	reSafeSkillSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	reSafeOptID     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
	validActionKind = map[string]bool{
		ActionApproveProposal:    true,
		ActionRejectProposal:     true,
		ActionRestageProposal:    true,
		ActionParkUntil:          true,
		ActionDismissHandled:     true,
		ActionDismissNoisy:       true,
		ActionSkipChecklist:      true,
		ActionPromoteGoal:        true,
		ActionSpawnInvestigation: true,
	}
)

// ValidateDecisionOptions validates, bounds, and sanitizes item.Decisions in place, enforcing
// Ring-0 taint safety so untrusted sessions cannot smuggle cross-proposal or arbitrary actions.
func ValidateDecisionOptions(item *OutboxItem) error {
	if item == nil || len(item.Decisions) == 0 {
		return nil
	}
	if len(item.Decisions) > MaxDecisionOptions {
		item.Decisions = item.Decisions[:MaxDecisionOptions]
	}
	allowedProposals := make(map[string]bool, len(item.ProposalIDs))
	for _, pid := range item.ProposalIDs {
		if p := strings.TrimSpace(pid); p != "" {
			allowedProposals[p] = true
		}
	}
	seenIDs := make(map[string]bool, len(item.Decisions))
	for i := range item.Decisions {
		opt := &item.Decisions[i]
		opt.ID = strings.TrimSpace(opt.ID)
		if opt.ID == "" {
			opt.ID = fmt.Sprintf("opt-%d", i+1)
		}
		if !reSafeOptID.MatchString(opt.ID) {
			return fmt.Errorf("invalid decision option id %q", opt.ID)
		}
		if seenIDs[opt.ID] {
			return fmt.Errorf("duplicate decision option id %q", opt.ID)
		}
		seenIDs[opt.ID] = true

		opt.Kind = strings.ToLower(strings.TrimSpace(opt.Kind))
		if !validActionKind[opt.Kind] {
			return fmt.Errorf("unsupported decision action kind %q", opt.Kind)
		}
		opt.Label = SanitizeAntiCue(opt.Label, 80)
		if opt.Label == "" {
			return fmt.Errorf("decision option %q requires non-empty label", opt.ID)
		}
		opt.Reason = SanitizeAntiCue(opt.Reason, 160)
		opt.GoalSlug = strings.TrimSpace(opt.GoalSlug)
		opt.ChecklistItemID = strings.TrimSpace(opt.ChecklistItemID)
		opt.ProposalID = strings.TrimSpace(opt.ProposalID)
		opt.ProposalSHA = strings.TrimSpace(opt.ProposalSHA)
		opt.ParkHorizon = strings.TrimSpace(opt.ParkHorizon)
		opt.SkillSlug = strings.TrimSpace(opt.SkillSlug)
		opt.Scope = strings.TrimSpace(opt.Scope)

		switch opt.Kind {
		case ActionApproveProposal, ActionRejectProposal, ActionRestageProposal:
			if opt.ProposalID == "" {
				return fmt.Errorf("decision %q (%s) requires proposalId", opt.ID, opt.Kind)
			}
			if item.Tainted && !allowedProposals[opt.ProposalID] {
				return fmt.Errorf("tainted outbox item may not reference unrelated proposal %q in decision %q", opt.ProposalID, opt.ID)
			}
		case ActionSkipChecklist:
			if opt.GoalSlug == "" || opt.ChecklistItemID == "" {
				return fmt.Errorf("decision %q (skip_checklist) requires goalSlug and checklistItemId", opt.ID)
			}
			if item.Tainted {
				return fmt.Errorf("tainted outbox item may not emit skip_checklist decision %q", opt.ID)
			}
		case ActionSpawnInvestigation:
			if !reSafeSkillSlug.MatchString(opt.SkillSlug) {
				return fmt.Errorf("decision %q (spawn_investigation) requires valid skillSlug", opt.ID)
			}
		case ActionParkUntil:
			if opt.ParkHorizon == "" {
				opt.ParkHorizon = "24h"
			}
		}
	}
	return nil
}

// GetByID loads a single OutboxItem by ID.
func GetByID(vaultDir, id string) (OutboxItem, error) {
	items, err := LoadAll(vaultDir)
	if err != nil {
		return OutboxItem{}, err
	}
	id = strings.TrimSpace(id)
	for _, it := range items {
		if it.ID == id {
			return it, nil
		}
	}
	return OutboxItem{}, fmt.Errorf("outbox item %q not found", id)
}

// FindDecisionOption locates a DecisionOption on an OutboxItem by option ID.
func FindDecisionOption(item OutboxItem, optID string) (DecisionOption, bool) {
	optID = strings.TrimSpace(optID)
	for _, d := range item.Decisions {
		if d.ID == optID {
			return d, true
		}
	}
	return DecisionOption{}, false
}

// RecordDecisionOutcome updates an OutboxItem's lifecycle state after executing a DecisionOption.
func RecordDecisionOutcome(vaultDir, itemID, status, parkHorizon, snoozeUntil, reason, resultSummary string, now time.Time) (OutboxItem, error) {
	return mutateItem(vaultDir, itemID, now, func(it *OutboxItem, nowStr string) {
		it.Status = status
		if strings.TrimSpace(parkHorizon) != "" {
			it.ParkHorizon = strings.TrimSpace(parkHorizon)
		}
		if strings.TrimSpace(snoozeUntil) != "" {
			it.SnoozeUntil = strings.TrimSpace(snoozeUntil)
		}
		if strings.TrimSpace(reason) != "" {
			it.DismissReason = strings.TrimSpace(reason)
		}
		it.ActionResult = SanitizeAntiCue(resultSummary, 240)
		it.ActedAt = nowStr
		it.UpdatedAt = nowStr
	})
}

// ParseParkDurationOrTime resolves a ParkHorizon string ("24h", "72h", "2026-12-02", RFC3339) into an RFC3339 snooze time.
// Returns ("", true) when parkHorizon is a goal-relative "after:<slug>" horizon (handled by Goal/Pulse wakeup).
func ParseParkDurationOrTime(horizon string, now time.Time) (snoozeUntil string, isGoalRelative bool) {
	if now.IsZero() {
		now = time.Now()
	}
	h := strings.TrimSpace(horizon)
	if strings.HasPrefix(strings.ToLower(h), "after:") {
		return "", true
	}
	if d, err := time.ParseDuration(h); err == nil && d > 0 {
		return now.Add(d).UTC().Format(time.RFC3339), false
	}
	if t, err := time.Parse(time.RFC3339, h); err == nil {
		return t.UTC().Format(time.RFC3339), false
	}
	if t, err := time.Parse("2006-01-02", h); err == nil {
		return t.UTC().Format(time.RFC3339), false
	}
	return now.Add(24 * time.Hour).UTC().Format(time.RFC3339), false
}
