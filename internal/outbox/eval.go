package outbox

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// EvalFixture defines a single synthetic proactive event routing test case for `hq outbox eval`.
type EvalFixture struct {
	Name         string     `json:"name"`
	Timestamp    string     `json:"timestamp"`
	Mode         string     `json:"mode,omitempty"` // optional override (balanced default)
	Item         OutboxItem `json:"item"`
	ExpectTier   string     `json:"expectTier"`
	ExpectCooled bool       `json:"expectCooled"`
}

// EvalCaseResult captures the pass/fail outcome of a single EvalFixture.
type EvalCaseResult struct {
	Name         string `json:"name"`
	Passed       bool   `json:"passed"`
	GotTier      string `json:"gotTier"`
	ExpectTier   string `json:"expectTier"`
	GotCooled    bool   `json:"gotCooled"`
	ExpectCooled bool   `json:"expectCooled"`
	DowngradedBy string `json:"downgradedBy,omitempty"`
}

// EvalReport summarizes signal-to-noise calibration metrics across an EvalFixture corpus.
type EvalReport struct {
	TotalCases               int              `json:"totalCases"`
	PassedCases              int              `json:"passedCases"`
	UrgentInterruptRecall    float64          `json:"urgentInterruptRecall"`
	QuietHoursSuppression    float64          `json:"quietHoursSuppression"`
	DuplicateSuppressionRate float64          `json:"duplicateSuppressionRate"`
	FalseInterruptRate       float64          `json:"falseInterruptRate"`
	AllThresholdsMet         bool             `json:"allThresholdsMet"`
	Cases                    []EvalCaseResult `json:"cases"`
}

// DefaultEvalFixtures returns the built-in 10-case golden corpus calibrating signal-to-noise routing.
func DefaultEvalFixtures() []EvalFixture {
	return []EvalFixture{
		{
			Name:       "urgent-daytime-kc1-breach",
			Timestamp:  "2026-11-12T15:00:00Z",
			Item:       OutboxItem{ID: "ev-1", SourceSkill: "thesis-check", Scope: "ACME", Tier: TierInterrupt, Critical: true, Title: "ACME KC-1 Revenue Breach", Summary: "Q3 filing missed floor by 8%."},
			ExpectTier: TierInterrupt,
		},
		{
			Name:         "exact-duplicate-within-cooldown",
			Timestamp:    "2026-11-12T16:00:00Z",
			Item:         OutboxItem{ID: "ev-2", SourceSkill: "thesis-check", Scope: "ACME", Tier: TierInterrupt, Critical: true, Title: "ACME KC-1 Revenue Breach", Summary: "Repeated check."},
			ExpectTier:   TierSilentLedger,
			ExpectCooled: true,
		},
		{
			Name:       "daytime-ci-failure-initial",
			Timestamp:  "2026-11-12T16:10:00Z",
			Item:       OutboxItem{ID: "ev-3", SourceSkill: "swe-pulse", Scope: "headquarter", Tier: TierInterrupt, Title: "PR #42 CI Failing", Summary: "Unit test failure on branch."},
			ExpectTier: TierInterrupt,
		},
		{
			Name:         "reworded-semantic-duplicate-same-pr",
			Timestamp:    "2026-11-12T21:30:00Z", // >4h cooldown, within 12h semantic window!
			Item:         OutboxItem{ID: "ev-4", SourceSkill: "swe-pulse", Scope: "headquarter", Tier: TierBriefRollup, Title: "CI Failure on PR #42", Summary: "Same failing workflow reworded."},
			ExpectTier:   TierSilentLedger,
			ExpectCooled: true,
		},
		{
			Name:       "distinct-pr-number-not-suppressed",
			Timestamp:  "2026-11-12T16:20:00Z",
			Item:       OutboxItem{ID: "ev-5", SourceSkill: "swe-pulse", Scope: "headquarter", Tier: TierInterrupt, Title: "PR #43 CI Failing", Summary: "Different PR failing."},
			ExpectTier: TierInterrupt,
		},
		{
			Name:       "quiet-hours-noncritical-downgraded",
			Timestamp:  "2026-11-12T23:30:00Z",
			Item:       OutboxItem{ID: "ev-6", SourceSkill: "inbox-triage", Scope: "vendor", Tier: TierInterrupt, Title: "Vendor Invoice Received", Summary: "Non-urgent monthly statement."},
			ExpectTier: TierBriefRollup,
		},
		{
			Name:       "quiet-hours-critical-breach-allowed",
			Timestamp:  "2026-11-12T23:45:00Z",
			Item:       OutboxItem{ID: "ev-7", SourceSkill: "swe-pulse", Scope: "prod", Tier: TierInterrupt, Critical: true, Title: "Production Healthcheck Down", Summary: "5xx spike above threshold."},
			ExpectTier: TierInterrupt,
		},
		{
			Name:       "focused-mode-noncritical-downgraded",
			Timestamp:  "2026-11-13T14:00:00Z",
			Mode:       "focused",
			Item:       OutboxItem{ID: "ev-8", SourceSkill: "swe-pulse", Scope: "repo", Tier: TierInterrupt, Title: "Minor Dependency Update Ready", Summary: "Patch release available."},
			ExpectTier: TierBriefRollup,
		},
		{
			Name:       "low-urgency-opportunity-ideas-feed",
			Timestamp:  "2026-11-13T15:00:00Z",
			Item:       OutboxItem{ID: "ev-9", SourceSkill: "swe-pulse", Scope: "repo", Tier: TierIdeasFeed, Title: "Refactor legacy helper", Summary: "Could save 80 LOC."},
			ExpectTier: TierIdeasFeed,
		},
		{
			Name:       "background-bookkeeping-silent-ledger",
			Timestamp:  "2026-11-13T07:00:00Z",
			Item:       OutboxItem{ID: "ev-10", SourceSkill: "sleep-consolidate", Tier: TierSilentLedger, Title: "Nightly consolidation complete", Summary: "3 facts verified."},
			ExpectTier: TierSilentLedger,
		},
	}
}

// LoadEvalFixturesFromFile parses newline-delimited JSON EvalFixture records from disk.
func LoadEvalFixturesFromFile(path string) ([]EvalFixture, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []EvalFixture
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var fx EvalFixture
		if err := json.Unmarshal([]byte(line), &fx); err != nil {
			return nil, fmt.Errorf("parse fixture line: %w", err)
		}
		out = append(out, fx)
	}
	return out, scanner.Err()
}

// RunEvalSuite executes offline signal-to-noise routing evaluation over fixtures (or DefaultEvalFixtures if empty).
func RunEvalSuite(fixtures []EvalFixture) EvalReport {
	if len(fixtures) == 0 {
		fixtures = DefaultEvalFixtures()
	}
	baseCfg := AttentionConfig{
		Mode: "balanced",
		QuietHours: QuietHoursConfig{
			Enabled:             true,
			Start:               "22:00",
			End:                 "07:30",
			Timezone:            "UTC",
			AllowCriticalBreach: true,
		},
		CooldownPerTopic: 4 * time.Hour,
	}

	var history []OutboxItem
	var report EvalReport
	report.TotalCases = len(fixtures)

	var urgentExpected, urgentHit int
	var quietNonCritExpected, quietNonCritSuppressed int
	var dupExpected, dupSuppressed int
	var nonInterruptExpected, falseInterrupts int

	for _, fx := range fixtures {
		cfg := baseCfg
		if strings.TrimSpace(fx.Mode) != "" {
			cfg.Mode = strings.TrimSpace(fx.Mode)
		}
		now, err := time.Parse(time.RFC3339, fx.Timestamp)
		if err != nil {
			now = time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
		}
		inItem := fx.Item
		inItem.CreatedAt = now.UTC().Format(time.RFC3339)
		inItem.UpdatedAt = inItem.CreatedAt

		got, cooled := EvaluateDelivery(history, cfg, now, inItem)
		history = append(history, got)

		passed := got.Tier == fx.ExpectTier && cooled == fx.ExpectCooled
		if passed {
			report.PassedCases++
		}
		report.Cases = append(report.Cases, EvalCaseResult{
			Name:         fx.Name,
			Passed:       passed,
			GotTier:      got.Tier,
			ExpectTier:   fx.ExpectTier,
			GotCooled:    cooled,
			ExpectCooled: fx.ExpectCooled,
			DowngradedBy: got.DowngradedBy,
		})

		if fx.ExpectTier == TierInterrupt {
			urgentExpected++
			if got.Tier == TierInterrupt {
				urgentHit++
			}
		} else {
			nonInterruptExpected++
			if got.Tier == TierInterrupt {
				falseInterrupts++
			}
		}
		if InQuietHours(cfg, now) && !fx.Item.Critical && fx.Item.Tier == TierInterrupt {
			quietNonCritExpected++
			if got.Tier != TierInterrupt {
				quietNonCritSuppressed++
			}
		}
		if fx.ExpectCooled {
			dupExpected++
			if cooled {
				dupSuppressed++
			}
		}
	}

	if urgentExpected > 0 {
		report.UrgentInterruptRecall = float64(urgentHit) / float64(urgentExpected)
	} else {
		report.UrgentInterruptRecall = 1.0
	}
	if quietNonCritExpected > 0 {
		report.QuietHoursSuppression = float64(quietNonCritSuppressed) / float64(quietNonCritExpected)
	} else {
		report.QuietHoursSuppression = 1.0
	}
	if dupExpected > 0 {
		report.DuplicateSuppressionRate = float64(dupSuppressed) / float64(dupExpected)
	} else {
		report.DuplicateSuppressionRate = 1.0
	}
	if nonInterruptExpected > 0 {
		report.FalseInterruptRate = float64(falseInterrupts) / float64(nonInterruptExpected)
	}

	report.AllThresholdsMet = report.PassedCases == report.TotalCases &&
		report.UrgentInterruptRecall >= 1.0 &&
		report.QuietHoursSuppression >= 1.0 &&
		report.DuplicateSuppressionRate >= 0.95 &&
		report.FalseInterruptRate <= 0.05

	return report
}
