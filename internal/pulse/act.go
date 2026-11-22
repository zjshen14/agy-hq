package pulse

import (
	"fmt"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/outbox"
)

// DecisionExecResult summarizes the 0-token execution of a DecisionOption on an OutboxItem.
type DecisionExecResult struct {
	ItemID        string `json:"itemId"`
	OptionID      string `json:"optionId"`
	Kind          string `json:"kind"`
	NewStatus     string `json:"newStatus"`
	NewProposalID string `json:"newProposalId,omitempty"`
	EventID       string `json:"eventId,omitempty"`
	Summary       string `json:"summary"`
}

// ExecuteDecision executes a validated DecisionOption on an OutboxItem in 0 LLM tokens.
func ExecuteDecision(vaultDir, itemID, optID string, now time.Time) (DecisionExecResult, error) {
	if now.IsZero() {
		now = time.Now()
	}
	item, err := outbox.GetByID(vaultDir, itemID)
	if err != nil {
		return DecisionExecResult{}, err
	}
	if err := outbox.ValidateDecisionOptions(&item); err != nil {
		return DecisionExecResult{}, fmt.Errorf("decision validation failed: %w", err)
	}
	opt, ok := outbox.FindDecisionOption(item, optID)
	if !ok {
		return DecisionExecResult{}, fmt.Errorf("option %q not found on outbox item %q", optID, itemID)
	}

	res := DecisionExecResult{
		ItemID:   item.ID,
		OptionID: opt.ID,
		Kind:     opt.Kind,
	}

	switch opt.Kind {
	case outbox.ActionApproveProposal:
		sha := strings.TrimSpace(opt.ProposalSHA)
		if sha == "" {
			pending, pErr := approvals.List(vaultDir)
			if pErr == nil {
				for _, p := range pending {
					if p.ID == opt.ProposalID {
						sha = p.SHA256
						break
					}
				}
			}
		}
		if sha == "" {
			return res, fmt.Errorf("proposal %q not pending or missing sha256", opt.ProposalID)
		}
		outStr, aErr := approvals.ApproveProposal(vaultDir, opt.ProposalID, sha)
		if aErr != nil {
			return res, aErr
		}
		res.NewStatus = outbox.StatusActedHandled
		res.Summary = fmt.Sprintf("Approved proposal %s (%s)", opt.ProposalID, outbox.SanitizeAntiCue(outStr, 120))
		_, err = outbox.RecordDecisionOutcome(vaultDir, item.ID, res.NewStatus, "", "", "", res.Summary, now)
		return res, err

	case outbox.ActionRejectProposal:
		reason := opt.Reason
		if reason == "" {
			reason = "Rejected via decision card"
		}
		if rErr := approvals.RejectProposalWithOptions(vaultDir, opt.ProposalID, approvals.RejectOptions{
			Reason:         reason,
			SuppressResume: true,
		}); rErr != nil {
			return res, rErr
		}
		res.NewStatus = outbox.StatusDismissed
		res.Summary = fmt.Sprintf("Rejected proposal %s", opt.ProposalID)
		_, err = outbox.RecordDecisionOutcome(vaultDir, item.ID, res.NewStatus, "", "", reason, res.Summary, now)
		return res, err

	case outbox.ActionRestageProposal:
		fresh, rErr := approvals.RestageExpiredProposal(vaultDir, opt.ProposalID, now)
		if rErr != nil {
			return res, rErr
		}
		res.NewProposalID = fresh.ID
		res.NewStatus = outbox.StatusActedHandled
		res.Summary = fmt.Sprintf("Restaged %s as fresh 24h proposal %s", opt.ProposalID, fresh.ID)
		_, err = outbox.RecordDecisionOutcome(vaultDir, item.ID, res.NewStatus, "", "", "", res.Summary, now)
		return res, err

	case outbox.ActionParkUntil:
		horizon := strings.TrimSpace(opt.ParkHorizon)
		if horizon == "" {
			horizon = "24h"
		}
		if strings.TrimSpace(opt.GoalSlug) != "" {
			// If horizon is a relative duration like "24h", convert to RFC3339 for Goal.ParkedUntil
			goalHorizon := horizon
			if d, dErr := time.ParseDuration(horizon); dErr == nil && d > 0 {
				goalHorizon = now.Add(d).UTC().Format(time.RFC3339)
			}
			_, _ = kernel.ParkGoal(vaultDir, opt.GoalSlug, goalHorizon, now)
		}
		snoozeUntil, isGoalRel := outbox.ParseParkDurationOrTime(horizon, now)
		if isGoalRel {
			// Park outbox item until pulse D8 wakes it
			snoozeUntil = now.Add(365 * 24 * time.Hour).UTC().Format(time.RFC3339)
		}
		res.NewStatus = outbox.StatusSnoozed
		res.Summary = fmt.Sprintf("Parked until %s", horizon)
		_, err = outbox.RecordDecisionOutcome(vaultDir, item.ID, res.NewStatus, horizon, snoozeUntil, "", res.Summary, now)
		return res, err

	case outbox.ActionDismissHandled:
		res.NewStatus = outbox.StatusActedHandled
		res.Summary = "Marked handled by user"
		_, err = outbox.RecordDecisionOutcome(vaultDir, item.ID, res.NewStatus, "", "", "", res.Summary, now)
		return res, err

	case outbox.ActionDismissNoisy:
		reason := opt.Reason
		if reason == "" {
			reason = "noisy: user muted similar alerts"
		}
		res.NewStatus = outbox.StatusDismissed
		res.Summary = "Dismissed as noisy (recorded for RSI tuning)"
		_, err = outbox.RecordDecisionOutcome(vaultDir, item.ID, res.NewStatus, "", "", reason, res.Summary, now)
		return res, err

	case outbox.ActionSkipChecklist:
		reason := opt.Reason
		if reason == "" {
			reason = "Skipped via decision button"
		}
		if _, _, sErr := kernel.SkipChecklistItem(vaultDir, opt.GoalSlug, opt.ChecklistItemID, reason, now); sErr != nil {
			return res, sErr
		}
		res.NewStatus = outbox.StatusActedHandled
		res.Summary = fmt.Sprintf("Skipped checklist item %s/%s", opt.GoalSlug, opt.ChecklistItemID)
		_, err = outbox.RecordDecisionOutcome(vaultDir, item.ID, res.NewStatus, "", "", "", res.Summary, now)
		return res, err

	case outbox.ActionPromoteGoal:
		slug := strings.TrimSpace(opt.GoalSlug)
		if slug == "" {
			slug = strings.TrimSpace(item.SuggestedSlug)
		}
		scope := strings.TrimSpace(opt.Scope)
		if scope == "" {
			scope = item.Scope
		}
		g, gErr := kernel.AddGoal(vaultDir, slug, item.Title, scope, "", now)
		if gErr != nil {
			return res, gErr
		}
		res.NewStatus = outbox.StatusPromoted
		res.Summary = fmt.Sprintf("Promoted to Goal %s", g.Slug)
		_, err = outbox.RecordDecisionOutcome(vaultDir, item.ID, res.NewStatus, "", "", "", res.Summary, now)
		return res, err

	case outbox.ActionSpawnInvestigation:
		scope := strings.TrimSpace(opt.Scope)
		if scope == "" {
			scope = item.Scope
		}
		if scope == "" {
			scope = opt.SkillSlug
		}
		ev, _, eErr := events.Enqueue(vaultDir, events.Event{
			Source:   "decision_action",
			DedupKey: fmt.Sprintf("decision:%s:%s", item.ID, opt.ID),
			ScopeKey: "decision:" + scope,
			Skill:    opt.SkillSlug,
			Title:    fmt.Sprintf("[Decision] %s (%s)", opt.Label, item.Title),
			Prompt:   fmt.Sprintf("Activate skill `%s` to investigate outbox item `%s` (%s): %s", opt.SkillSlug, item.ID, item.Title, item.Summary),
			Metadata: map[string]string{
				"outbox_id": item.ID,
				"option_id": opt.ID,
				"skill":     opt.SkillSlug,
			},
		}, now)
		if eErr != nil {
			return res, eErr
		}
		res.EventID = ev.ID
		res.NewStatus = outbox.StatusActedHandled
		res.Summary = fmt.Sprintf("Queued %s investigation event %s", opt.SkillSlug, ev.ID)
		_, err = outbox.RecordDecisionOutcome(vaultDir, item.ID, res.NewStatus, "", "", "", res.Summary, now)
		return res, err

	default:
		return res, fmt.Errorf("unsupported decision action kind %q", opt.Kind)
	}
}
