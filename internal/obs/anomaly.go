package obs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
)

// BudgetConfig defines daily token budgets and context/turn anomaly thresholds in local/memory/obs/budget.json.
type BudgetConfig struct {
	DailyUncachedBudget     int64 `json:"dailyUncachedBudget"`
	DailyDeepUncachedBudget int64 `json:"dailyDeepUncachedBudget"`
	WarnContextTokens       int64 `json:"warnContextTokens"`
	NudgeContextTokens      int64 `json:"nudgeContextTokens"`
	CritContextTokens       int64 `json:"critContextTokens"`
	MaxTurnSteps            int   `json:"maxTurnSteps"`
	MaxTurnUncachedTokens   int64 `json:"maxTurnUncachedTokens"`
	MaxTurnOutputTokens     int64 `json:"maxTurnOutputTokens"`
}

// DefaultBudgetConfig returns the RFC-0012 default budget and context guard configuration.
func DefaultBudgetConfig() BudgetConfig {
	return BudgetConfig{
		DailyUncachedBudget:     15_000_000,
		DailyDeepUncachedBudget: 5_000_000,
		WarnContextTokens:       120_000,
		NudgeContextTokens:      160_000,
		CritContextTokens:       180_000,
		MaxTurnSteps:            60,
		MaxTurnUncachedTokens:   500_000,
		MaxTurnOutputTokens:     50_000,
	}
}

func budgetFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "obs", "budget.json")
}

// LoadBudgetConfig reads local/memory/obs/budget.json, filling any zero fields from DefaultBudgetConfig.
func LoadBudgetConfig(vaultDir string) BudgetConfig {
	cfg := DefaultBudgetConfig()
	data, err := os.ReadFile(budgetFilePath(vaultDir))
	if err != nil {
		return cfg
	}
	var parsed BudgetConfig
	if json.Unmarshal(data, &parsed) != nil {
		return cfg
	}
	if parsed.DailyUncachedBudget > 0 {
		cfg.DailyUncachedBudget = parsed.DailyUncachedBudget
	}
	if parsed.DailyDeepUncachedBudget > 0 {
		cfg.DailyDeepUncachedBudget = parsed.DailyDeepUncachedBudget
	}
	if parsed.WarnContextTokens > 0 {
		cfg.WarnContextTokens = parsed.WarnContextTokens
	}
	if parsed.NudgeContextTokens > 0 {
		cfg.NudgeContextTokens = parsed.NudgeContextTokens
	}
	if parsed.CritContextTokens > 0 {
		cfg.CritContextTokens = parsed.CritContextTokens
	}
	if parsed.MaxTurnSteps > 0 {
		cfg.MaxTurnSteps = parsed.MaxTurnSteps
	}
	if parsed.MaxTurnUncachedTokens > 0 {
		cfg.MaxTurnUncachedTokens = parsed.MaxTurnUncachedTokens
	}
	if parsed.MaxTurnOutputTokens > 0 {
		cfg.MaxTurnOutputTokens = parsed.MaxTurnOutputTokens
	}
	return cfg
}

// SaveBudgetConfig writes BudgetConfig to local/memory/obs/budget.json.
func SaveBudgetConfig(vaultDir string, cfg BudgetConfig) error {
	p := budgetFilePath(vaultDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// TokenAnomaly represents a detected token/context/cache/budget anomaly (A1..A5).
type TokenAnomaly struct {
	ID             string `json:"id"`
	Code           string `json:"code"`     // "A1" | "A2" | "A3" | "A4" | "A5"
	Kind           string `json:"kind"`     // "FrontDeskContextBloat" | "ColdCacheThrash" | "RunawayTurnBurn" | "TierMisroutingBurn" | "DailyTokenBudgetGuard"
	Severity       string `json:"severity"` // "WARN" | "CRITICAL"
	ConversationID string `json:"conversationId,omitempty"`
	TaskSlug       string `json:"taskSlug,omitempty"`
	TurnIndex      int    `json:"turnIndex,omitempty"`
	Timestamp      string `json:"timestamp"`
	Summary        string `json:"summary"`
	Remediation    string `json:"remediation"`
	MetricValue    int64  `json:"metricValue"`
	ThresholdValue int64  `json:"thresholdValue"`
}

// EvaluateAnomalies evaluates all 5 RFC-0012 anomaly detectors (A1..A5) against the observability index.
func EvaluateAnomalies(vaultDir string, idx *ObsIndex, cfg BudgetConfig, now time.Time) []TokenAnomaly {
	if now.IsZero() {
		now = time.Now()
	}
	if idx == nil {
		var err error
		idx, err = LoadOrRefreshIndex(vaultDir)
		if err != nil || idx == nil {
			return nil
		}
	}

	var anomalies []TokenAnomaly
	todayStr := now.Format("2006-01-02")
	var todayUncached, todayDeepUncached int64

	fd, hasFD := kernel.GetFrontDesk(vaultDir)
	fdID := ""
	if hasFD {
		fdID = strings.ToLower(strings.TrimSpace(fd.ConversationID))
	}

	// Sort conversation IDs for deterministic anomaly ordering
	var convIDs []string
	for cid := range idx.Conversations {
		convIDs = append(convIDs, cid)
	}
	sort.Strings(convIDs)

	for _, cid := range convIDs {
		cm := idx.Conversations[cid]

		// A1: Front Desk Context Bloat
		if fdID != "" && cid == fdID && cm.CurrentContextTokens >= cfg.WarnContextTokens {
			sev := "WARN"
			thresh := cfg.WarnContextTokens
			if cm.CurrentContextTokens >= cfg.CritContextTokens {
				sev = "CRITICAL"
				thresh = cfg.CritContextTokens
			}
			anomalies = append(anomalies, TokenAnomaly{
				ID:             fmt.Sprintf("A1-%s", shortConvID(cid)),
				Code:           "A1",
				Kind:           "FrontDeskContextBloat",
				Severity:       sev,
				ConversationID: cid,
				TaskSlug:       "front-desk",
				Timestamp:      cm.LastActiveAt,
				Summary: fmt.Sprintf(
					"[HQ] Front Desk context is at %s tokens (threshold: %s, compaction ceiling: ~256k). Idle gaps >5m cause cold-cache wakeups.",
					FormatTokensCompact(cm.CurrentContextTokens),
					FormatTokensCompact(thresh),
				),
				Remediation:    "Run `/compact` in the Front Desk thread or start a fresh Front Desk shift (`./bin/hq release-desk`).",
				MetricValue:    cm.CurrentContextTokens,
				ThresholdValue: thresh,
			})
		}

		// A2: Cold-Cache Wakeup Thrash (TurnCount >= 5, PeakContextTokens >= 100k, CacheHitRatio < 70%)
		if cm.Self.TurnCount >= 5 && cm.Self.PeakContextTokens >= 100_000 && cm.Self.CacheHitRatio < 0.70 {
			sev := "WARN"
			if cm.Self.CacheHitRatio < 0.50 && cm.Self.UncachedTokens >= 2_000_000 {
				sev = "CRITICAL"
			}
			anomalies = append(anomalies, TokenAnomaly{
				ID:             fmt.Sprintf("A2-%s", shortConvID(cid)),
				Code:           "A2",
				Kind:           "ColdCacheThrash",
				Severity:       sev,
				ConversationID: cid,
				TaskSlug:       cm.TaskSlug,
				Timestamp:      cm.LastActiveAt,
				Summary: fmt.Sprintf(
					"%s (%s) has %.1f%% prefix cache hit ratio across %d turns (peak context %s, uncached burn %s).",
					cm.Title,
					shortConvID(cid),
					cm.Self.CacheHitRatio*100.0,
					cm.Self.TurnCount,
					FormatTokensCompact(cm.Self.PeakContextTokens),
					FormatTokensCompact(cm.Self.UncachedTokens),
				),
				Remediation:    "Keep long-lived threads compact (`/compact`) and move multi-turn deep work into dedicated `./bin/hq spawn` Task Conversations.",
				MetricValue:    int64(cm.Self.CacheHitRatio * 100.0),
				ThresholdValue: 70,
			})
		}

		// A3: Runaway Turn Burn
		for _, t := range cm.Turns {
			if localDateStr(t.StartedAt, now.Location()) == todayStr {
				todayUncached += t.UncachedTokens
				if cm.Tier == "deep" {
					todayDeepUncached += t.UncachedTokens
				}
			}

			isRunawaySteps := cfg.MaxTurnSteps > 0 && t.StepCount >= cfg.MaxTurnSteps
			isRunawayUncached := cfg.MaxTurnUncachedTokens > 0 && t.UncachedTokens >= cfg.MaxTurnUncachedTokens
			isRunawayOutput := cfg.MaxTurnOutputTokens > 0 && t.OutputTokens >= cfg.MaxTurnOutputTokens

			if isRunawaySteps || isRunawayUncached || isRunawayOutput {
				sev := "WARN"
				if t.UncachedTokens >= cfg.MaxTurnUncachedTokens*2 || t.StepCount >= cfg.MaxTurnSteps*2 {
					sev = "CRITICAL"
				}
				var reasons []string
				metricVal := t.UncachedTokens
				threshVal := cfg.MaxTurnUncachedTokens
				if isRunawayUncached {
					reasons = append(reasons, fmt.Sprintf("%s uncached tokens", FormatTokensCompact(t.UncachedTokens)))
				}
				if isRunawaySteps {
					reasons = append(reasons, fmt.Sprintf("%d steps", t.StepCount))
					if !isRunawayUncached {
						metricVal = int64(t.StepCount)
						threshVal = int64(cfg.MaxTurnSteps)
					}
				}
				if isRunawayOutput {
					reasons = append(reasons, fmt.Sprintf("%s output tokens", FormatTokensCompact(t.OutputTokens)))
					if !isRunawayUncached && !isRunawaySteps {
						metricVal = t.OutputTokens
						threshVal = cfg.MaxTurnOutputTokens
					}
				}
				anomalies = append(anomalies, TokenAnomaly{
					ID:             fmt.Sprintf("A3-%s-T%d", shortConvID(cid), t.TurnIndex),
					Code:           "A3",
					Kind:           "RunawayTurnBurn",
					Severity:       sev,
					ConversationID: cid,
					TaskSlug:       cm.TaskSlug,
					TurnIndex:      t.TurnIndex,
					Timestamp:      t.EndedAt,
					Summary: fmt.Sprintf(
						"Turn #%d in %s (%s) consumed %s (gross %s, CHR %.1f%%).",
						t.TurnIndex,
						cm.Title,
						shortConvID(cid),
						strings.Join(reasons, ", "),
						FormatTokensCompact(t.GrossTokens),
						t.CacheHitRatio*100.0,
					),
					Remediation:    "Break monolithic turns into bounded phases and avoid polling loops inside a single turn.",
					MetricValue:    metricVal,
					ThresholdValue: threshVal,
				})
			}
		}

		// A4: Tier Misrouting Burn (deep-tier subagent burning >= 300k uncached or >= 2.5M gross tokens)
		if (cm.Kind == "subagent" || cm.Kind == "nested_subagent") && cm.Tier == "deep" &&
			(cm.Self.UncachedTokens >= 300_000 || cm.Self.GrossTokens >= 2_500_000) {
			anomalies = append(anomalies, TokenAnomaly{
				ID:             fmt.Sprintf("A4-%s", shortConvID(cid)),
				Code:           "A4",
				Kind:           "TierMisroutingBurn",
				Severity:       "WARN",
				ConversationID: cid,
				TaskSlug:       cm.TaskSlug,
				Timestamp:      cm.LastActiveAt,
				Summary: fmt.Sprintf(
					"Deep-tier subagent %s (%s, model: %s) burned %s uncached (%s gross, %.0f QBU).",
					cm.Title,
					shortConvID(cid),
					cm.Model,
					FormatTokensCompact(cm.Self.UncachedTokens),
					FormatTokensCompact(cm.Self.GrossTokens),
					cm.Self.QuotaUnits,
				),
				Remediation:    "Route routine coding, test execution, and extraction subagents to `standard` (`flash`) to preserve `deep` (`pro`/`opus`) quota.",
				MetricValue:    cm.Self.UncachedTokens,
				ThresholdValue: 300_000,
			})
		}
	}

	// A5: Daily Token Budget Guard (soft-warn at 80%, pause kind:auto triggers at 100%)
	if cfg.DailyUncachedBudget > 0 {
		warnThresh := int64(float64(cfg.DailyUncachedBudget) * 0.80)
		if todayUncached >= warnThresh {
			sev := "WARN"
			thresh := warnThresh
			rem := "Approaching daily uncached token budget; prefer `standard` (`flash`) for background subagents."
			if todayUncached >= cfg.DailyUncachedBudget {
				sev = "CRITICAL"
				thresh = cfg.DailyUncachedBudget
				rem = "Daily uncached token budget reached: automated `kind: auto` background triggers are paused until midnight or `./bin/hq obs budget --reset-circuit`."
			}
			anomalies = append(anomalies, TokenAnomaly{
				ID:             "A5-daily-uncached-" + todayStr,
				Code:           "A5",
				Kind:           "DailyTokenBudgetGuard",
				Severity:       sev,
				Timestamp:      now.Format(time.RFC3339),
				Summary:        fmt.Sprintf("Today's uncached token burn is %s / %s (%.1f%% of daily budget).", FormatTokensCompact(todayUncached), FormatTokensCompact(cfg.DailyUncachedBudget), (float64(todayUncached)/float64(cfg.DailyUncachedBudget))*100.0),
				Remediation:    rem,
				MetricValue:    todayUncached,
				ThresholdValue: thresh,
			})
		}
	}
	if cfg.DailyDeepUncachedBudget > 0 {
		warnDeep := int64(float64(cfg.DailyDeepUncachedBudget) * 0.80)
		if todayDeepUncached >= warnDeep {
			sev := "WARN"
			thresh := warnDeep
			rem := "Approaching daily deep-tier token budget; route non-architectural subagents to `flash`."
			if todayDeepUncached >= cfg.DailyDeepUncachedBudget {
				sev = "CRITICAL"
				thresh = cfg.DailyDeepUncachedBudget
				rem = "Daily deep-tier token budget reached: automated `kind: auto` background triggers are paused until midnight or `./bin/hq obs budget --reset-circuit`."
			}
			anomalies = append(anomalies, TokenAnomaly{
				ID:             "A5-daily-deep-" + todayStr,
				Code:           "A5",
				Kind:           "DailyTokenBudgetGuard",
				Severity:       sev,
				Timestamp:      now.Format(time.RFC3339),
				Summary:        fmt.Sprintf("Today's deep-tier uncached token burn is %s / %s (%.1f%% of daily deep budget).", FormatTokensCompact(todayDeepUncached), FormatTokensCompact(cfg.DailyDeepUncachedBudget), (float64(todayDeepUncached)/float64(cfg.DailyDeepUncachedBudget))*100.0),
				Remediation:    rem,
				MetricValue:    todayDeepUncached,
				ThresholdValue: thresh,
			})
		}
	}

	// Sort anomalies: CRITICAL first, then by Code (A1, A5, A2, A3, A4), then newest timestamp
	sort.SliceStable(anomalies, func(i, j int) bool {
		if anomalies[i].Severity != anomalies[j].Severity {
			return anomalies[i].Severity == "CRITICAL"
		}
		if anomalies[i].Code != anomalies[j].Code {
			return anomalies[i].Code < anomalies[j].Code
		}
		return anomalies[i].Timestamp > anomalies[j].Timestamp
	})

	return anomalies
}

// SyncDailyBudgetCircuitBreaker checks whether today's uncached or deep-tier token burn has reached 100%
// of the configured daily budget, and if so, opens the QuotaState circuit breaker so proactive
// background (`kind: auto` / `RunHeartbeat` / `SpawnTask`) triggers pause while interactive turns remain unblocked.
func SyncDailyBudgetCircuitBreaker(vaultDir string, now time.Time) (tripped bool, reason string) {
	if now.IsZero() {
		now = time.Now()
	}
	cfg := LoadBudgetConfig(vaultDir)
	idx, err := LoadOrRefreshIndex(vaultDir)
	if err != nil || idx == nil {
		return false, ""
	}
	todayStr := now.Format("2006-01-02")
	var todayUncached, todayDeepUncached int64
	for _, cm := range idx.Conversations {
		for _, t := range cm.Turns {
			if localDateStr(t.StartedAt, now.Location()) == todayStr {
				todayUncached += t.UncachedTokens
				if cm.Tier == "deep" {
					todayDeepUncached += t.UncachedTokens
				}
			}
		}
	}

	if cfg.DailyUncachedBudget > 0 && todayUncached >= cfg.DailyUncachedBudget {
		reason = fmt.Sprintf("daily uncached token budget reached (%s / %s)", FormatTokensCompact(todayUncached), FormatTokensCompact(cfg.DailyUncachedBudget))
		tripped = true
	} else if cfg.DailyDeepUncachedBudget > 0 && todayDeepUncached >= cfg.DailyDeepUncachedBudget {
		reason = fmt.Sprintf("daily deep-tier token budget reached (%s / %s)", FormatTokensCompact(todayDeepUncached), FormatTokensCompact(cfg.DailyDeepUncachedBudget))
		tripped = true
	}

	if tripped {
		q := kernel.LoadQuota(vaultDir)
		if !q.CircuitOpen {
			q.CircuitOpen = true
			q.CircuitReason = reason
			_ = kernel.SaveQuota(vaultDir, q)
		}
	}
	return tripped, reason
}

// ResetBudgetCircuitBreaker clears the QuotaState circuit breaker when requested via `hq obs budget --reset-circuit`.
func ResetBudgetCircuitBreaker(vaultDir string) error {
	q := kernel.LoadQuota(vaultDir)
	q.CircuitOpen = false
	q.CircuitReason = ""
	return kernel.SaveQuota(vaultDir, q)
}

func shortConvID(cid string) string {
	if len(cid) > 8 {
		return cid[:8]
	}
	return cid
}
