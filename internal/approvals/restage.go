package approvals

import (
	"fmt"
	"strings"
	"time"
)

// ListExpiredProposals returns expired InterventionRecords from interventions.jsonl that have not yet
// been restaged or explicitly superseded. When within > 0, limits results to records within that age.
func ListExpiredProposals(vaultDir string, within time.Duration, now time.Time) ([]InterventionRecord, error) {
	if now.IsZero() {
		now = time.Now()
	}
	recs, err := LoadInterventions(vaultDir)
	if err != nil {
		return nil, err
	}
	restagedOrResolved := make(map[string]bool)
	for _, r := range recs {
		if r.Decision == "restaged" || r.Decision == "expired_dismissed" || r.Decision == "dismissed" {
			restagedOrResolved[r.Proposal.ID] = true
		}
		if r.Proposal.Metadata != nil {
			if from := strings.TrimSpace(r.Proposal.Metadata["restagedFrom"]); from != "" {
				restagedOrResolved[from] = true
			}
		}
	}
	// Also check currently pending proposals for restagedFrom metadata.
	if pending, pErr := List(vaultDir); pErr == nil {
		for _, p := range pending {
			if p.Metadata != nil {
				if from := strings.TrimSpace(p.Metadata["restagedFrom"]); from != "" {
					restagedOrResolved[from] = true
				}
			}
		}
	}

	var out []InterventionRecord
	seen := make(map[string]bool)
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		if r.Decision != "expired" || r.Proposal.ID == "" {
			continue
		}
		if restagedOrResolved[r.Proposal.ID] || seen[r.Proposal.ID] {
			continue
		}
		if within > 0 {
			if ts, pErr := time.Parse(time.RFC3339, r.Timestamp); pErr == nil && now.Sub(ts) > within {
				continue
			}
		}
		seen[r.Proposal.ID] = true
		out = append(out, r)
	}
	return out, nil
}

// FindUnrestagedExpiredOfferForThread finds the most recent expired, unrestaged proposal targeting
// (connector, account, threadKey) within `within` (or any age when within <= 0).
func FindUnrestagedExpiredOfferForThread(vaultDir, connector, account, threadKey string, within time.Duration, now time.Time) *InterventionRecord {
	tk := strings.TrimSpace(threadKey)
	if tk == "" {
		return nil
	}
	expired, err := ListExpiredProposals(vaultDir, within, now)
	if err != nil {
		return nil
	}
	conn := strings.ToLower(strings.TrimSpace(connector))
	acct := strings.ToLower(strings.TrimSpace(account))
	for _, rec := range expired {
		p := rec.Proposal
		if strings.TrimSpace(p.TargetThreadKey) != tk {
			continue
		}
		if conn != "" && p.TargetConnector != "" && !strings.EqualFold(strings.TrimSpace(p.TargetConnector), conn) {
			continue
		}
		if acct != "" && p.TargetAccount != "" && !strings.EqualFold(strings.TrimSpace(p.TargetAccount), acct) {
			continue
		}
		cp := rec
		return &cp
	}
	return nil
}

// RestageExpiredProposal mints a fresh 24h pending Proposal from an expired proposal in interventions.jsonl
// in 0 LLM tokens, checking RegisteredApplyPrecondition first to reject stale thread replies.
func RestageExpiredProposal(vaultDir, expiredID string, now time.Time) (Proposal, error) {
	expiredID = strings.TrimSpace(expiredID)
	if err := ValidateProposalID(expiredID); err != nil {
		return Proposal{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	recs, err := LoadInterventions(vaultDir)
	if err != nil {
		return Proposal{}, err
	}
	var target *Proposal
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Decision == "expired" && recs[i].Proposal.ID == expiredID {
			cp := recs[i].Proposal
			target = &cp
			break
		}
	}
	if target == nil {
		return Proposal{}, fmt.Errorf("expired proposal %q not found in interventions log", expiredID)
	}
	// Reject restaging if the original proposal relied on an ephemeral {{payload_file}} that was purged on expiry.
	for _, a := range target.CommandArgv {
		if strings.Contains(a, "{{payload_file}}") {
			return Proposal{}, fmt.Errorf("expired proposal %s used an ephemeral payload file purged at expiry; re-stage a fresh draft", expiredID)
		}
	}
	if RegisteredApplyPrecondition != nil {
		if preErr := RegisteredApplyPrecondition(vaultDir, *target); preErr != nil {
			return Proposal{}, fmt.Errorf("cannot restage %s (thread state changed): %w", expiredID, preErr)
		}
	}

	meta := make(map[string]string, len(target.Metadata)+2)
	for k, v := range target.Metadata {
		meta[k] = v
	}
	meta["restagedFrom"] = expiredID
	meta["restagedAt"] = now.UTC().Format(time.RFC3339)

	fresh, err := CreateWithOptions(vaultDir, CreateOptions{
		ActionType:       target.ActionType,
		Summary:          target.Summary,
		Command:          target.Command,
		CommandArgv:      append([]string(nil), target.CommandArgv...),
		WorkDir:          target.WorkDir,
		ConversationID:   target.ConversationID,
		TargetConnector:  target.TargetConnector,
		TargetAccount:    target.TargetAccount,
		TargetThreadKey:  target.TargetThreadKey,
		ResumeOnDecide:   target.ResumeOnDecide,
		ContinuationNote: target.ContinuationNote,
		OriginSkill:      target.OriginSkill,
		Metadata:         meta,
	})
	if err != nil {
		return Proposal{}, err
	}

	expiredCopy := *target
	expiredCopy.SupersededBy = fresh.ID
	appendIntervention(vaultDir, InterventionRecord{
		Timestamp: now.UTC().Format(time.RFC3339),
		Decision:  "restaged",
		Proposal:  expiredCopy,
		Output:    "Restaged as " + fresh.ID,
	})
	return fresh, nil
}

// DismissExpiredProposal permanently marks an expired proposal in interventions.jsonl as dismissed
// so it no longer surfaces in `hq approvals expired`, Morning Brief, or the Live Console.
func DismissExpiredProposal(vaultDir, expiredID, reason string, now time.Time) error {
	expiredID = strings.TrimSpace(expiredID)
	if err := ValidateProposalID(expiredID); err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now()
	}
	recs, err := LoadInterventions(vaultDir)
	if err != nil {
		return err
	}
	var target *Proposal
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Decision == "expired" && recs[i].Proposal.ID == expiredID {
			cp := recs[i].Proposal
			target = &cp
			break
		}
	}
	if target == nil {
		return fmt.Errorf("expired proposal %q not found in interventions log", expiredID)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "Dismissed by user"
	}
	expiredCopy := *target
	expiredCopy.Status = "dismissed"
	appendIntervention(vaultDir, InterventionRecord{
		Timestamp: now.UTC().Format(time.RFC3339),
		Decision:  "expired_dismissed",
		Proposal:  expiredCopy,
		Output:    reason,
	})
	return nil
}
