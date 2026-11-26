package rsi

import (
	"crypto/sha256"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/vault"
)

// EvalSplitMetrics summarizes ground-truth reliability & token efficiency on one deterministic
// conversation partition (Selection or HeldOut).
type EvalSplitMetrics struct {
	Conversations         int     `json:"conversations"`
	Turns                 int     `json:"turns"`
	TotalErrors           int     `json:"totalErrors"`
	PreToolDenials        int     `json:"preToolDenials"`
	ErrorRatePer100Turns  float64 `json:"errorRatePer100Turns"`
	DenialRatePer100Turns float64 `json:"denialRatePer100Turns"`
	MeanTokensPerTurn     float64 `json:"meanTokensPerTurn"`
}

// MutationEvalDelta compares pre- vs. post-apply held-out telemetry for an applied RSI rule.
type MutationEvalDelta struct {
	RSIID                string  `json:"rsiId"`
	RuleID               string  `json:"ruleId,omitempty"`
	TargetSkill          string  `json:"targetSkill"`
	Status               string  `json:"status"`
	PreHeldOutTurns      int     `json:"preHeldOutTurns"`
	PostHeldOutTurns     int     `json:"postHeldOutTurns"`
	PreHeldOutErrPer100  float64 `json:"preHeldOutErrPer100"`
	PostHeldOutErrPer100 float64 `json:"postHeldOutErrPer100"`
	ErrDeltaPct          float64 `json:"errDeltaPct"`
	Regressed            bool    `json:"regressed"`
}

// EvalFunnelMetrics summarizes the RSI pipeline funnel state.
type EvalFunnelMetrics struct {
	ObservationsTotal  int `json:"observationsTotal"`
	ObservationsOpen   int `json:"observationsOpen"`
	HypothesesDraft    int `json:"hypothesesDraft"`
	HypothesesVerified int `json:"hypothesesVerified"`
	HypothesesProposed int `json:"hypothesesProposed"`
	CanariesActive     int `json:"canariesActive"`
	PromotedCount      int `json:"promotedCount"`
	RejectedCount      int `json:"rejectedCount"`
	RevertedCount      int `json:"revertedCount"`
}

// EvalReport is the deterministic, 0-token, held-out ratchet evaluation emitted by `hq rsi eval`.
type EvalReport struct {
	GeneratedAt    string              `json:"generatedAt"`
	Since          string              `json:"since"`
	Selection      EvalSplitMetrics    `json:"selection"`
	HeldOut        EvalSplitMetrics    `json:"heldOut"`
	Funnel         EvalFunnelMetrics   `json:"funnel"`
	MutationDeltas []MutationEvalDelta `json:"mutationDeltas,omitempty"`
	Regressions    []string            `json:"regressions,omitempty"`
	Advisories     int                 `json:"advisories"`
}

// IsHeldOutConversation deterministically assigns 50% of conversations to the Held-Out split
// using SHA-256(convID)[0] & 1 == 1 so selection vs. validation never leak across runs.
func IsHeldOutConversation(convID string) bool {
	sum := sha256.Sum256([]byte(strings.TrimSpace(convID)))
	return (sum[0] & 1) == 1
}

// Evaluate computes the deterministic 0-token held-out RSI evaluation report across journals,
// token observability telemetry, hypotheses, and archive records.
func Evaluate(rootDir, vaultDir string, since time.Duration) (*EvalReport, error) {
	now := time.Now().UTC()
	if since <= 0 {
		since = 14 * 24 * time.Hour
	}
	cutoff := now.Add(-since)

	var obsIdx *obs.ObsIndex
	if idx, err := obs.LoadOrRefreshIndex(vaultDir); err == nil && idx != nil {
		obsIdx = idx
	}

	vaultSkills := listInstalledVaultSkillSlugs(vaultDir)

	type turnSample struct {
		ts      time.Time
		convID  string
		heldOut bool
		skill   string
		errors  int
		denials int
		tokens  int64
	}
	var samples []turnSample
	selConvs := make(map[string]bool)
	heldConvs := make(map[string]bool)

	var sel, held EvalSplitMetrics
	var selTokSum, heldTokSum int64
	var selTokTurns, heldTokTurns int

	_ = vault.WalkJournals(vaultDir, cutoff, true, func(_ string, rec vault.JournalRecord) bool {
		if rec.TaintedSession {
			return true
		}
		cid := strings.TrimSpace(rec.ConversationID)
		if cid == "" {
			return true
		}
		ts, _ := time.Parse(time.RFC3339, rec.Timestamp)
		isHeld := IsHeldOutConversation(cid)
		errs := rec.TurnErrorCount
		if errs == 0 && rec.StepStartIdx == 0 && rec.TurnStepCount == 0 {
			errs = rec.ToolErrorCount
		}
		var toks int64
		if obsIdx != nil {
			if cm, ok := obsIdx.Conversations[cid]; ok {
				toks = cm.Self.GrossTokens
				if toks == 0 {
					toks = cm.Self.InputTokens + cm.Self.OutputTokens
				}
			}
		}
		skill := inferSkillOrPkg(rec.UserPrompt+" "+rec.ResponseSummary, rec.FilesModified, vaultSkills)
		samples = append(samples, turnSample{
			ts:      ts,
			convID:  cid,
			heldOut: isHeld,
			skill:   skill,
			errors:  errs,
			denials: rec.PreToolDenials,
			tokens:  toks,
		})
		if isHeld {
			heldConvs[cid] = true
			held.Turns++
			held.TotalErrors += errs
			held.PreToolDenials += rec.PreToolDenials
			if toks > 0 {
				heldTokSum += toks
				heldTokTurns++
			}
		} else {
			selConvs[cid] = true
			sel.Turns++
			sel.TotalErrors += errs
			sel.PreToolDenials += rec.PreToolDenials
			if toks > 0 {
				selTokSum += toks
				selTokTurns++
			}
		}
		return true
	})

	sel.Conversations = len(selConvs)
	held.Conversations = len(heldConvs)
	if sel.Turns > 0 {
		sel.ErrorRatePer100Turns = roundEval2(100.0 * float64(sel.TotalErrors) / float64(sel.Turns))
		sel.DenialRatePer100Turns = roundEval2(100.0 * float64(sel.PreToolDenials) / float64(sel.Turns))
	}
	if selTokTurns > 0 {
		sel.MeanTokensPerTurn = roundEval2(float64(selTokSum) / float64(selTokTurns))
	}
	if held.Turns > 0 {
		held.ErrorRatePer100Turns = roundEval2(100.0 * float64(held.TotalErrors) / float64(held.Turns))
		held.DenialRatePer100Turns = roundEval2(100.0 * float64(held.PreToolDenials) / float64(held.Turns))
	}
	if heldTokTurns > 0 {
		held.MeanTokensPerTurn = roundEval2(float64(heldTokSum) / float64(heldTokTurns))
	}

	// Funnel metrics
	obsList, _ := LoadObservations(vaultDir)
	hyps, _ := ListHypotheses(vaultDir)
	arch, _ := LoadArchive(vaultDir)

	var funnel EvalFunnelMetrics
	funnel.ObservationsTotal = len(obsList)
	for _, o := range obsList {
		if o.Status == "open" {
			funnel.ObservationsOpen++
		}
	}
	for _, h := range hyps {
		switch h.Status {
		case "draft":
			funnel.HypothesesDraft++
		case "verified":
			funnel.HypothesesVerified++
		case "proposed":
			funnel.HypothesesProposed++
		}
	}
	for _, a := range arch {
		switch a.Status {
		case "canary":
			funnel.CanariesActive++
		case "promoted":
			funnel.PromotedCount++
		case "rejected":
			funnel.RejectedCount++
		case "reverted":
			funnel.RevertedCount++
		}
	}

	// Pre- vs. Post-apply held-out directional regret evaluation for applied mutations
	var deltas []MutationEvalDelta
	var regressions []string
	for _, a := range arch {
		if a.Status != "canary" && a.Status != "promoted" {
			continue
		}
		appliedTS, err := time.Parse(time.RFC3339, a.AppliedAt)
		if err != nil {
			continue
		}
		skill := targetSkillFromFiles(a.TargetFiles)
		if skill == "" {
			continue
		}
		var preTurns, preErrs, postTurns, postErrs int
		for _, s := range samples {
			if !s.heldOut || s.skill != skill || s.ts.IsZero() {
				continue
			}
			if s.ts.Before(appliedTS) {
				preTurns++
				preErrs += s.errors
			} else {
				postTurns++
				postErrs += s.errors
			}
		}
		d := MutationEvalDelta{
			RSIID:            a.ID,
			RuleID:           a.RuleID,
			TargetSkill:      skill,
			Status:           a.Status,
			PreHeldOutTurns:  preTurns,
			PostHeldOutTurns: postTurns,
		}
		if preTurns > 0 {
			d.PreHeldOutErrPer100 = roundEval2(100.0 * float64(preErrs) / float64(preTurns))
		}
		if postTurns > 0 {
			d.PostHeldOutErrPer100 = roundEval2(100.0 * float64(postErrs) / float64(postTurns))
		}
		if preTurns >= 2 && postTurns >= 2 {
			if d.PreHeldOutErrPer100 > 0 {
				d.ErrDeltaPct = roundEval2(100.0 * (d.PostHeldOutErrPer100 - d.PreHeldOutErrPer100) / d.PreHeldOutErrPer100)
				if d.ErrDeltaPct > 25.0 {
					d.Regressed = true
				}
			} else if d.PostHeldOutErrPer100 > 25.0 {
				d.ErrDeltaPct = 100.0
				d.Regressed = true
			}
		}
		if d.Regressed {
			regressions = append(regressions, fmt.Sprintf("%s (%s): held-out error rate regressed %.1f -> %.1f per 100 turns (+%.1f%%)", a.ID, skill, d.PreHeldOutErrPer100, d.PostHeldOutErrPer100, d.ErrDeltaPct))
		}
		deltas = append(deltas, d)
	}

	return &EvalReport{
		GeneratedAt:    now.UTC().Format(time.RFC3339),
		Since:          cutoff.UTC().Format(time.RFC3339),
		Selection:      sel,
		HeldOut:        held,
		Funnel:         funnel,
		MutationDeltas: deltas,
		Regressions:    regressions,
		Advisories:     len(ListAdvisories(rootDir, vaultDir)),
	}, nil
}

func targetSkillFromFiles(targetFiles []string) string {
	for _, tf := range targetFiles {
		slash := strings.TrimPrefix(strings.TrimSpace(tf), "./")
		if strings.HasSuffix(slash, "/SKILL.md") {
			parts := strings.Split(slash, "/")
			if len(parts) >= 2 {
				return parts[len(parts)-2]
			}
		}
		if strings.HasPrefix(slash, "local/memory/rsi/rules/") && strings.HasSuffix(slash, ".md") {
			return strings.TrimSuffix(strings.TrimPrefix(slash, "local/memory/rsi/rules/"), ".md")
		}
	}
	return ""
}

func roundEval2(v float64) float64 {
	return math.Round(v*100) / 100
}
