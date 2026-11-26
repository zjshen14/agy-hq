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
	"github.com/zjshen14/agy-hq/internal/store"
)

// DefaultContextCeilingTokens is Antigravity's automatic CHECKPOINT compaction ceiling on Gemini 4 models (~512k).
const DefaultContextCeilingTokens int64 = 512_000

// BudgetConfig defines daily token budgets and context/turn anomaly thresholds in local/memory/obs/budget.json.
type BudgetConfig struct {
	DailyUncachedBudget     int64 `json:"dailyUncachedBudget"`
	DailyDeepUncachedBudget int64 `json:"dailyDeepUncachedBudget"`
	TokenCircuitDisabled    bool  `json:"tokenCircuitDisabled,omitempty"`
	ContextCeilingTokens    int64 `json:"contextCeilingTokens,omitempty"`
	WarnContextTokens       int64 `json:"warnContextTokens"`
	NudgeContextTokens      int64 `json:"nudgeContextTokens"`
	CritContextTokens       int64 `json:"critContextTokens"`
	MaxTurnSteps            int   `json:"maxTurnSteps"`
	MaxTurnUncachedTokens   int64 `json:"maxTurnUncachedTokens"`
	MaxTurnOutputTokens     int64 `json:"maxTurnOutputTokens"`
	MaxDailySpawns          int   `json:"maxDailySpawns,omitempty"`
}

// EffectiveContextCeiling returns ContextCeilingTokens or DefaultContextCeilingTokens (512,000) when unset.
func (c BudgetConfig) EffectiveContextCeiling() int64 {
	if c.ContextCeilingTokens > 0 {
		return c.ContextCeilingTokens
	}
	return DefaultContextCeilingTokens
}

// DefaultBudgetConfig returns the calibrated default budget and context guard configuration.
func DefaultBudgetConfig() BudgetConfig {
	return BudgetConfig{
		DailyUncachedBudget:     50_000_000,
		DailyDeepUncachedBudget: 30_000_000,
		ContextCeilingTokens:    DefaultContextCeilingTokens,
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
	cfg.TokenCircuitDisabled = parsed.TokenCircuitDisabled
	if parsed.ContextCeilingTokens > 0 {
		cfg.ContextCeilingTokens = parsed.ContextCeilingTokens
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
	if parsed.MaxDailySpawns > 0 {
		cfg.MaxDailySpawns = parsed.MaxDailySpawns
	}
	return cfg
}

// SaveBudgetConfig writes BudgetConfig to local/memory/obs/budget.json atomically.
func SaveBudgetConfig(vaultDir string, cfg BudgetConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(budgetFilePath(vaultDir), append(data, '\n'), 0o644)
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

// EvaluateAnomalies evaluates live token/context/cache/budget anomalies within the default 24h active window.
func EvaluateAnomalies(vaultDir string, idx *ObsIndex, cfg BudgetConfig, now time.Time) []TokenAnomaly {
	if now.IsZero() {
		now = time.Now()
	}
	return EvaluateAnomaliesSince(vaultDir, idx, cfg, now, now.Add(-24*time.Hour))
}

// EvaluateAnomaliesSince evaluates all 5 anomaly detectors (A1..A5) against the observability index,
// scoping historical turn/subagent alerts (A2, A3, A4) to turns occurring at or after cutoff (or all-time when cutoff is zero).
func EvaluateAnomaliesSince(vaultDir string, idx *ObsIndex, cfg BudgetConfig, now, cutoff time.Time) []TokenAnomaly {
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
	ceilingStr := FormatTokensCompact(cfg.EffectiveContextCeiling())

	fd, hasFD := kernel.GetFrontDesk(vaultDir)
	fdID := ""
	if hasFD {
		fdID = strings.ToLower(strings.TrimSpace(fd.ConversationID))
	}

	var convIDs []string
	for cid := range idx.Conversations {
		convIDs = append(convIDs, cid)
	}
	sort.Strings(convIDs)

	for _, cid := range convIDs {
		cm := idx.Conversations[cid]

		// A1: Front Desk Context Bloat (live active Front Desk state)
		if fdID != "" && cid == fdID && !cm.CompactedPendingNextTurn && cm.CurrentContextTokens >= cfg.WarnContextTokens {
			sev := "WARN"
			thresh := cfg.WarnContextTokens
			if cm.CurrentContextTokens >= cfg.CritContextTokens {
				sev = "CRITICAL"
				thresh = cfg.CritContextTokens
			}
			anomalies = append(anomalies, TokenAnomaly{
				ID:             fmt.Sprintf("A1-%s", store.ShortConvID(cid)),
				Code:           "A1",
				Kind:           "FrontDeskContextBloat",
				Severity:       sev,
				ConversationID: cid,
				TaskSlug:       "front-desk",
				Timestamp:      cm.LastActiveAt,
				Summary: fmt.Sprintf(
					"[HQ] Front Desk context is at %s tokens (hygiene threshold: %s, auto-compaction ceiling: ~%s). Idle gaps >5m cause cold-cache wakeups.",
					FormatTokensCompact(cm.CurrentContextTokens),
					FormatTokensCompact(thresh),
					ceilingStr,
				),
				Remediation:    "Run `/compact` in the Front Desk thread or start a fresh Front Desk shift (`./bin/hq release-desk`).",
				MetricValue:    cm.CurrentContextTokens,
				ThresholdValue: thresh,
			})
		}

		var coldSpikes int
		var coldSpikeTokens int64
		var lastColdTS string
		var windowDeepUncached, windowDeepGross int64
		var windowDeepCost float64
		var windowPlannerSteps int
		var hasAnyWriteEdit bool
		var lastWindowTS, lastDeepModel string

		for _, t := range cm.Turns {
			if t.HasWriteOrEdit {
				hasAnyWriteEdit = true
			}
			tTier := turnModelTier(t, cm)
			if turnLocalDateStr(t, now.Location()) == todayStr {
				todayUncached += t.UncachedTokens
				if tTier == "deep" {
					todayDeepUncached += t.UncachedTokens
				}
			}

			if !turnMatchesCutoff(t, cutoff) {
				continue
			}
			if t.EndedAt != "" {
				lastWindowTS = t.EndedAt
			} else if t.StartedAt != "" {
				lastWindowTS = t.StartedAt
			}
			windowPlannerSteps += t.PlannerSteps

			if t.ColdStartInputTokens >= 50_000 {
				coldSpikes++
				coldSpikeTokens += t.ColdStartInputTokens
				lastColdTS = lastWindowTS
			}

			if tTier == "deep" {
				windowDeepUncached += t.UncachedTokens
				windowDeepGross += t.GrossTokens
				windowDeepCost += t.EstCostUSD
				lastDeepModel = turnModel(t, cm)
			}

			// A3: Runaway Turn Burn (checks PlannerSteps, NOT raw JSONL StepCount)
			isRunawaySteps := cfg.MaxTurnSteps > 0 && t.PlannerSteps >= cfg.MaxTurnSteps
			isRunawayUncached := cfg.MaxTurnUncachedTokens > 0 && t.UncachedTokens >= cfg.MaxTurnUncachedTokens
			isRunawayOutput := cfg.MaxTurnOutputTokens > 0 && t.OutputTokens >= cfg.MaxTurnOutputTokens

			if isRunawaySteps || isRunawayUncached || isRunawayOutput {
				sev := "WARN"
				if (cfg.MaxTurnUncachedTokens > 0 && t.UncachedTokens >= cfg.MaxTurnUncachedTokens*2) ||
					(cfg.MaxTurnSteps > 0 && t.PlannerSteps >= cfg.MaxTurnSteps*2) {
					sev = "CRITICAL"
				}
				var reasons []string
				metricVal := t.UncachedTokens
				threshVal := cfg.MaxTurnUncachedTokens
				if isRunawayUncached {
					reasons = append(reasons, fmt.Sprintf("%s uncached tokens", FormatTokensCompact(t.UncachedTokens)))
				}
				if isRunawaySteps {
					reasons = append(reasons, fmt.Sprintf("%d planner steps", t.PlannerSteps))
					if !isRunawayUncached {
						metricVal = int64(t.PlannerSteps)
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
				ts := t.EndedAt
				if ts == "" {
					ts = t.StartedAt
				}
				anomalies = append(anomalies, TokenAnomaly{
					ID:             fmt.Sprintf("A3-%s-T%d", store.ShortConvID(cid), t.TurnIndex),
					Code:           "A3",
					Kind:           "RunawayTurnBurn",
					Severity:       sev,
					ConversationID: cid,
					TaskSlug:       cm.TaskSlug,
					TurnIndex:      t.TurnIndex,
					Timestamp:      ts,
					Summary: fmt.Sprintf(
						"Turn #%d in %s (%s) consumed %s (gross %s, %.1f%% cached).",
						t.TurnIndex,
						cm.Title,
						store.ShortConvID(cid),
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

		// A2: Cold-Cache Wakeup Thrash (Turn-opening cold spikes >= 50k input at <20% first-step CHR)
		if coldSpikes >= 3 {
			sev := "WARN"
			if coldSpikes >= 6 || coldSpikeTokens >= 1_000_000 {
				sev = "CRITICAL"
			}
			ts := lastColdTS
			if ts == "" {
				ts = cm.LastActiveAt
			}
			anomalies = append(anomalies, TokenAnomaly{
				ID:             fmt.Sprintf("A2-%s", store.ShortConvID(cid)),
				Code:           "A2",
				Kind:           "ColdCacheThrash",
				Severity:       sev,
				ConversationID: cid,
				TaskSlug:       cm.TaskSlug,
				Timestamp:      ts,
				Summary: fmt.Sprintf(
					"%s (%s) woke cold %d times (>=50k input, <20%% cache hit), burning %s uncached cold-start tokens.",
					cm.Title,
					store.ShortConvID(cid),
					coldSpikes,
					FormatTokensCompact(coldSpikeTokens),
				),
				Remediation:    "Run `/compact` on long-lived coordinator threads and delegate deep multi-turn work via `./bin/hq spawn`.",
				MetricValue:    coldSpikeTokens,
				ThresholdValue: 150_000,
			})
		}

		// A4: Tier Misrouting Burn (auto/quick routed to deep, or non-editing lookup subagent burning deep tokens)
		reqTier := strings.ToLower(strings.TrimSpace(cm.RequestedTier))
		isMisroutedAutoOrQuick := (cm.Kind == "auto" && reqTier != "deep" && windowDeepUncached >= 50_000) ||
			(reqTier == "quick" && windowDeepUncached >= 50_000)
		isNonEditingDeepSubagent := (cm.Kind == "subagent" || cm.Kind == "nested_subagent") && !hasAnyWriteEdit &&
			((windowPlannerSteps <= 5 && windowDeepUncached >= 100_000) || windowDeepUncached >= 300_000 || windowDeepGross >= 2_500_000)

		if windowDeepUncached > 0 && (isMisroutedAutoOrQuick || isNonEditingDeepSubagent) {
			ts := lastWindowTS
			if ts == "" {
				ts = cm.LastActiveAt
			}
			mLabel := lastDeepModel
			if mLabel == "" {
				mLabel = cm.Model
			}
			anomalies = append(anomalies, TokenAnomaly{
				ID:             fmt.Sprintf("A4-%s", store.ShortConvID(cid)),
				Code:           "A4",
				Kind:           "TierMisroutingBurn",
				Severity:       "WARN",
				ConversationID: cid,
				TaskSlug:       cm.TaskSlug,
				Timestamp:      ts,
				Summary: fmt.Sprintf(
					"Deep-tier %s %s (%s, model: %s) burned %s uncached (%s gross, ~$%.2f) with 0 file edits.",
					cm.Kind,
					cm.Title,
					store.ShortConvID(cid),
					mLabel,
					FormatTokensCompact(windowDeepUncached),
					FormatTokensCompact(windowDeepGross),
					windowDeepCost,
				),
				Remediation:    "Route lookup, extraction, and routine background runs to `standard` (`neon`) or `routine` (`hydrogen`) to preserve `deep` (`carbon`/`opus`) quota.",
				MetricValue:    windowDeepUncached,
				ThresholdValue: 300_000,
			})
		}
	}

	// A5: Daily Token Budget Guard (soft-warn at 80%, pause kind:auto triggers at 100%)
	if !cfg.TokenCircuitDisabled {
		for _, bc := range []struct {
			idPrefix string
			used     int64
			limit    int64
			label    string
			warnRem  string
			critRem  string
		}{
			{
				idPrefix: "A5-daily-uncached-",
				used:     todayUncached,
				limit:    cfg.DailyUncachedBudget,
				label:    "uncached token burn",
				warnRem:  "Approaching daily uncached token budget; prefer `standard` (`neon`/`hydrogen`) for background subagents.",
				critRem:  "Daily uncached token budget reached: automated `kind: auto` background triggers are paused until midnight or `./bin/hq obs budget --reset-circuit`.",
			},
			{
				idPrefix: "A5-daily-deep-",
				used:     todayDeepUncached,
				limit:    cfg.DailyDeepUncachedBudget,
				label:    "deep-tier uncached token burn",
				warnRem:  "Approaching daily deep-tier token budget; route non-architectural subagents to `neon`/`hydrogen`.",
				critRem:  "Daily deep-tier token budget reached: automated `kind: auto` background triggers are paused until midnight or `./bin/hq obs budget --reset-circuit`.",
			},
		} {
			if bc.limit <= 0 {
				continue
			}
			warnThresh := int64(float64(bc.limit) * 0.80)
			if bc.used < warnThresh {
				continue
			}
			sev, thresh, rem := "WARN", warnThresh, bc.warnRem
			if bc.used >= bc.limit {
				sev, thresh, rem = "CRITICAL", bc.limit, bc.critRem
			}
			scopeSuffix := "daily budget"
			if bc.idPrefix == "A5-daily-deep-" {
				scopeSuffix = "daily deep budget"
			}
			anomalies = append(anomalies, TokenAnomaly{
				ID:             bc.idPrefix + todayStr,
				Code:           "A5",
				Kind:           "DailyTokenBudgetGuard",
				Severity:       sev,
				Timestamp:      now.Format(time.RFC3339),
				Summary:        fmt.Sprintf("Today's %s is %s / %s (%.1f%% of %s).", bc.label, FormatTokensCompact(bc.used), FormatTokensCompact(bc.limit), (float64(bc.used)/float64(bc.limit))*100.0, scopeSuffix),
				Remediation:    rem,
				MetricValue:    bc.used,
				ThresholdValue: thresh,
			})
		}
	}

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
			if turnLocalDateStr(t, now.Location()) == todayStr {
				todayUncached += t.UncachedTokens
				if turnModelTier(t, cm) == "deep" {
					todayDeepUncached += t.UncachedTokens
				}
			}
		}
	}

	if !cfg.TokenCircuitDisabled {
		if cfg.DailyUncachedBudget > 0 && todayUncached >= cfg.DailyUncachedBudget {
			reason = fmt.Sprintf("daily uncached token budget reached (%s / %s)", FormatTokensCompact(todayUncached), FormatTokensCompact(cfg.DailyUncachedBudget))
			tripped = true
		} else if cfg.DailyDeepUncachedBudget > 0 && todayDeepUncached >= cfg.DailyDeepUncachedBudget {
			reason = fmt.Sprintf("daily deep-tier token budget reached (%s / %s)", FormatTokensCompact(todayDeepUncached), FormatTokensCompact(cfg.DailyDeepUncachedBudget))
			tripped = true
		}
	}

	if tripped {
		q := kernel.LoadQuota(vaultDir)
		if !q.CircuitOpen {
			q.CircuitOpen = true
			q.CircuitReason = reason
			_ = kernel.SaveQuota(vaultDir, q)
		}
	} else {
		q := kernel.LoadQuota(vaultDir)
		if q.CircuitOpen && (strings.HasPrefix(q.CircuitReason, "daily uncached token budget reached") ||
			strings.HasPrefix(q.CircuitReason, "daily deep-tier token budget reached")) {
			q.CircuitOpen = false
			q.CircuitReason = ""
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
