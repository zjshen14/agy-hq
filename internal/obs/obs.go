package obs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/store"
)

const MaxDashboardAnomalies = 20

// SubagentSpawnEdge records a ground-truth parent -> child subagent invocation extracted
// directly from parent transcript tool calls / GENERIC responses or message files.
type SubagentSpawnEdge struct {
	StepIndex            int    `json:"stepIndex"`
	ChildConversationID  string `json:"childConversationId"`
	ParentConversationID string `json:"parentConversationId"`
	Role                 string `json:"role,omitempty"`
	Model                string `json:"model,omitempty"`
	TypeName             string `json:"typeName,omitempty"`
	Workspace            string `json:"workspace,omitempty"`
	PromptPrefix         string `json:"promptPrefix,omitempty"`
	CreatedAt            string `json:"createdAt,omitempty"`
}

// JournalModelSpan records a turn/execution slice's ground-truth model from local/memory/journal/.
type JournalModelSpan struct {
	ExecNum   int    `json:"execNum,omitempty"`
	StepStart int    `json:"stepStart"`
	StepEnd   int    `json:"stepEnd"`
	Model     string `json:"model"`
	Timestamp string `json:"timestamp,omitempty"`
}

// TurnMetrics captures token burn, prefix cache efficiency, context watermarks, and 3-part latency for a single turn.
type TurnMetrics struct {
	TurnIndex                int      `json:"turnIndex"`
	StepStartIdx             int      `json:"stepStartIdx"`
	StepEndIdx               int      `json:"stepEndIdx"`
	StepCount                int      `json:"stepCount"`
	PlannerSteps             int      `json:"plannerSteps"`
	StartedAt                string   `json:"startedAt"`
	EndedAt                  string   `json:"endedAt"`
	TriggerKind              string   `json:"triggerKind"` // "user" | "schedule" | "subagent_callback" | "system_wakeup"
	PromptPreview            string   `json:"promptPreview,omitempty"`
	Model                    string   `json:"model,omitempty"`
	ModelTier                string   `json:"modelTier,omitempty"`
	InputTokens              int64    `json:"inputTokens"`
	CacheReadTokens          int64    `json:"cacheReadTokens"`
	OutputTokens             int64    `json:"outputTokens"`
	UncachedTokens           int64    `json:"uncachedTokens"`
	GrossTokens              int64    `json:"grossTokens"`
	CacheHitRatio            float64  `json:"cacheHitRatio"`
	FirstStepCHR             float64  `json:"firstStepChr,omitempty"`
	ColdStartInputTokens     int64    `json:"coldStartInputTokens,omitempty"`
	PeakContextTokens        int64    `json:"peakContextTokens"`
	CurrentContextTokens     int64    `json:"currentContextTokens"`
	CompactedPendingNextTurn bool     `json:"compactedPendingNextTurn,omitempty"`
	CompactionCount          int      `json:"compactionCount"`
	LLMLatencyMs             int64    `json:"llmLatencyMs"`
	ToolLatencyMs            int64    `json:"toolLatencyMs"`
	HumanWaitMs              int64    `json:"humanWaitMs"`
	ActiveLatencyMs          int64    `json:"activeLatencyMs"`
	ToolsUsed                []string `json:"toolsUsed,omitempty"`
	HasWriteOrEdit           bool     `json:"hasWriteOrEdit,omitempty"`
	ToolErrorCount           int      `json:"toolErrorCount,omitempty"`
	QuotaUnits               float64  `json:"quotaUnits"`
	EstCostUSD               float64  `json:"estCostUsd"`
	CacheSavedUSD            float64  `json:"cacheSavedUsd,omitempty"`
}

// TokenRollup aggregates token, context, compaction, latency, and cost metrics across turns or DAG subtrees.
type TokenRollup struct {
	TurnCount         int     `json:"turnCount"`
	StepCount         int     `json:"stepCount"`
	PlannerSteps      int     `json:"plannerSteps"`
	InputTokens       int64   `json:"inputTokens"`
	CacheReadTokens   int64   `json:"cacheReadTokens"`
	OutputTokens      int64   `json:"outputTokens"`
	UncachedTokens    int64   `json:"uncachedTokens"`
	GrossTokens       int64   `json:"grossTokens"`
	CacheHitRatio     float64 `json:"cacheHitRatio"`
	PeakContextTokens int64   `json:"peakContextTokens"`
	CompactionCount   int     `json:"compactionCount"`
	ActiveLatencyMs   int64   `json:"activeLatencyMs"`
	HumanWaitMs       int64   `json:"humanWaitMs"`
	QuotaUnits        float64 `json:"quotaUnits"`
	EstCostUSD        float64 `json:"estCostUsd"`
	CacheSavedUSD     float64 `json:"cacheSavedUsd,omitempty"`
}

// ConversationMetrics stores full parsed metrics + DAG metadata for a single conversation.
type ConversationMetrics struct {
	ConversationID           string              `json:"conversationId"`
	ParentConversationID     string              `json:"parentConversationId,omitempty"`
	RootConversationID       string              `json:"rootConversationId,omitempty"`
	TaskSlug                 string              `json:"taskSlug,omitempty"`
	Title                    string              `json:"title,omitempty"`
	Kind                     string              `json:"kind"` // "front_desk" | "task" | "subagent" | "nested_subagent" | "auto"
	Scope                    string              `json:"scope,omitempty"`
	Tier                     string              `json:"tier"`                    // Canonical model tier: "quick" | "standard" | "deep"
	RequestedTier            string              `json:"requestedTier,omitempty"` // Role/task requested tier
	Model                    string              `json:"model,omitempty"`
	ModelSource              string              `json:"modelSource,omitempty"` // "journal" | "task" | "spawn_edge" | "fallback"
	Status                   string              `json:"status,omitempty"`
	TranscriptPath           string              `json:"transcriptPath"`
	FileSize                 int64               `json:"fileSize"`
	ModTimeNano              int64               `json:"modTimeNano"`
	StartedAt                string              `json:"startedAt,omitempty"`
	LastActiveAt             string              `json:"lastActiveAt,omitempty"`
	CurrentContextTokens     int64               `json:"currentContextTokens"`
	PostCompactContextTokens int64               `json:"postCompactContextTokens,omitempty"`
	CompactedPendingNextTurn bool                `json:"compactedPendingNextTurn,omitempty"`
	DescendantCount          int                 `json:"descendantCount,omitempty"`
	ChildConversationIDs     []string            `json:"childConversationIds,omitempty"`
	SpawnEdges               []SubagentSpawnEdge `json:"spawnEdges,omitempty"`
	JournalSpans             []JournalModelSpan  `json:"journalSpans,omitempty"`
	Self                     TokenRollup         `json:"self"`
	Tree                     TokenRollup         `json:"tree"`
	Turns                    []TurnMetrics       `json:"turns,omitempty"`
}

// ObsIndex is the stat-cached observability index persisted in local/memory/obs/index.json.
type ObsIndex struct {
	Version       int                            `json:"version"`
	UpdatedAt     string                         `json:"updatedAt"`
	Conversations map[string]ConversationMetrics `json:"conversations"`
}

// BucketMetrics represents an aggregated slice (by day, hour, model, tier, kind, or scope).
type BucketMetrics struct {
	Key               string      `json:"key"`
	Label             string      `json:"label,omitempty"`
	ConvCount         int         `json:"convCount"`
	Rollup            TokenRollup `json:"rollup"`
	AvgUncachedPerRun int64       `json:"avgUncachedPerRun,omitempty"`
	P50UncachedPerRun int64       `json:"p50UncachedPerRun,omitempty"`
	AvgGrossPerRun    int64       `json:"avgGrossPerRun,omitempty"`
	P50GrossPerRun    int64       `json:"p50GrossPerRun,omitempty"`
	ShareOfQBU        float64     `json:"shareOfQbu"`
}

// ObsSummaryReport is returned by `hq obs [tokens]`.
type ObsSummaryReport struct {
	GeneratedAt               string          `json:"generatedAt"`
	Since                     string          `json:"since"`
	GroupBy                   string          `json:"groupBy"`
	ConversationCount         int             `json:"conversationCount"`
	Totals                    TokenRollup     `json:"totals"`
	FrontDesk                 *TokenRollup    `json:"frontDesk,omitempty"`
	FrontDeskContext          int64           `json:"frontDeskContextTokens"`
	FrontDeskCompactedPending bool            `json:"frontDeskCompactedPending,omitempty"`
	FrontDeskConvID           string          `json:"frontDeskConvId,omitempty"`
	Buckets                   []BucketMetrics `json:"buckets"`
	ByTier                    []BucketMetrics `json:"byTier"`
	ByKind                    []BucketMetrics `json:"byKind"`
	Budget                    BudgetConfig    `json:"budget"`
	TodayUncached             int64           `json:"todayUncachedTokens"`
	TodayDeepUncached         int64           `json:"todayDeepUncachedTokens"`
	Anomalies                 []TokenAnomaly  `json:"anomalies,omitempty"`
}

// TopItem represents a ranked conversation/task row in `hq obs top`.
type TopItem struct {
	Rank                 int         `json:"rank"`
	ConversationID       string      `json:"conversationId"`
	ParentConversationID string      `json:"parentConversationId,omitempty"`
	TaskSlug             string      `json:"taskSlug,omitempty"`
	Title                string      `json:"title"`
	Kind                 string      `json:"kind"`
	Scope                string      `json:"scope"`
	Tier                 string      `json:"tier"`
	Model                string      `json:"model"`
	ChildCount           int         `json:"childCount"`
	DescendantCount      int         `json:"descendantCount,omitempty"`
	CurrentContextTokens int64       `json:"currentContextTokens"`
	Metrics              TokenRollup `json:"metrics"`
	SelfMetrics          TokenRollup `json:"selfMetrics"`
	TreeMetrics          TokenRollup `json:"treeMetrics"`
}

// TopTurnItem represents a ranked turn row in `hq obs top --turns`.
type TopTurnItem struct {
	Rank           int         `json:"rank"`
	ConversationID string      `json:"conversationId"`
	TaskSlug       string      `json:"taskSlug,omitempty"`
	Title          string      `json:"title"`
	Kind           string      `json:"kind"`
	Tier           string      `json:"tier"`
	Model          string      `json:"model"`
	Turn           TurnMetrics `json:"turn"`
}

// TopReport is returned by `hq obs top`.
type TopReport struct {
	GeneratedAt string        `json:"generatedAt"`
	SortBy      string        `json:"sortBy"`
	KindFilter  string        `json:"kindFilter"`
	TreeMode    bool          `json:"treeMode"`
	TurnsMode   bool          `json:"turnsMode"`
	AllNodes    bool          `json:"allNodes,omitempty"`
	Limit       int           `json:"limit"`
	Items       []TopItem     `json:"items,omitempty"`
	TurnItems   []TopTurnItem `json:"turnItems,omitempty"`
}

// InspectReport is returned by `hq obs inspect <convId|taskSlug>`.
type InspectReport struct {
	Conversation ConversationMetrics   `json:"conversation"`
	Children     []ConversationMetrics `json:"children,omitempty"`
	Anomalies    []TokenAnomaly        `json:"anomalies,omitempty"`
}

// DailyBurnPoint is a 1-day summary for the 7-day trend chart in the Web UI.
type DailyBurnPoint struct {
	Date            string  `json:"date"`
	UncachedTokens  int64   `json:"uncachedTokens"`
	CacheReadTokens int64   `json:"cacheReadTokens"`
	OutputTokens    int64   `json:"outputTokens"`
	GrossTokens     int64   `json:"grossTokens"`
	CacheHitRatio   float64 `json:"cacheHitRatio"`
	QuotaUnits      float64 `json:"quotaUnits"`
	EstCostUSD      float64 `json:"estCostUsd,omitempty"`
	CacheSavedUSD   float64 `json:"cacheSavedUsd,omitempty"`
}

// TierBurnPoint summarizes token usage for a single tier ("quick", "standard", "deep").
type TierBurnPoint struct {
	Tier           string  `json:"tier"`
	UncachedTokens int64   `json:"uncachedTokens"`
	GrossTokens    int64   `json:"grossTokens"`
	CacheHitRatio  float64 `json:"cacheHitRatio"`
	QuotaUnits     float64 `json:"quotaUnits"`
	EstCostUSD     float64 `json:"estCostUsd,omitempty"`
	SharePct       float64 `json:"sharePct"`
}

// TaskTokenBadge provides compact token telemetry for Task Board rows in Web UI and hq_console.md.
type TaskTokenBadge struct {
	ConversationID       string  `json:"conversationId"`
	UncachedTokens       int64   `json:"uncachedTokens"`
	TreeUncachedTokens   int64   `json:"treeUncachedTokens"`
	GrossTokens          int64   `json:"grossTokens"`
	CacheHitRatio        float64 `json:"cacheHitRatio"`
	PeakContextTokens    int64   `json:"peakContextTokens"`
	CurrentContextTokens int64   `json:"currentContextTokens"`
	QuotaUnits           float64 `json:"quotaUnits"`
	EstCostUSD           float64 `json:"estCostUsd,omitempty"`
	ChildCount           int     `json:"childCount"`
	DescendantCount      int     `json:"descendantCount,omitempty"`
	CompactLabel         string  `json:"compactLabel"`
}

// DashboardSummary is embedded into Web UI `/api/state`, `/api/obs`, and `SyncMarkdownConsole`.
type DashboardSummary struct {
	UpdatedAt                  string                     `json:"updatedAt"`
	TodayInputTokens           int64                      `json:"todayInputTokens"`
	TodayCacheReadTokens       int64                      `json:"todayCacheReadTokens"`
	TodayOutputTokens          int64                      `json:"todayOutputTokens"`
	TodayUncachedTokens        int64                      `json:"todayUncachedTokens"`
	TodayGrossTokens           int64                      `json:"todayGrossTokens"`
	TodayCacheHitRatio         float64                    `json:"todayCacheHitRatio"`
	TodayDeepUncachedTokens    int64                      `json:"todayDeepUncachedTokens"`
	TodayQuotaUnits            float64                    `json:"todayQuotaUnits"`
	TodayEstCostUSD            float64                    `json:"todayEstCostUsd"`
	TodayCacheSavedUSD         float64                    `json:"todayCacheSavedUsd"`
	TodayColdSpikes            int                        `json:"todayColdSpikes,omitempty"`
	TodayColdSpikeTokens       int64                      `json:"todayColdSpikeTokens,omitempty"`
	TodayTurnCount             int                        `json:"todayTurnCount"`
	AllTimeUncachedTokens      int64                      `json:"allTimeUncachedTokens"`
	AllTimeGrossTokens         int64                      `json:"allTimeGrossTokens"`
	AllTimeCacheHitRatio       float64                    `json:"allTimeCacheHitRatio"`
	AllTimeEstCostUSD          float64                    `json:"allTimeEstCostUsd"`
	AllTimeCacheSavedUSD       float64                    `json:"allTimeCacheSavedUsd"`
	FrontDeskConvID            string                     `json:"frontDeskConvId,omitempty"`
	FrontDeskContextTokens     int64                      `json:"frontDeskContextTokens"`
	FrontDeskPostCompactTokens int64                      `json:"frontDeskPostCompactTokens,omitempty"`
	FrontDeskPeakTokens        int64                      `json:"frontDeskPeakTokens"`
	FrontDeskCompactions       int                        `json:"frontDeskCompactions"`
	FrontDeskCacheHitRatio     float64                    `json:"frontDeskCacheHitRatio"`
	FrontDeskContextStatus     string                     `json:"frontDeskContextStatus"` // "OK" | "WARN" | "CRITICAL"
	FrontDeskCompactedPending  bool                       `json:"frontDeskCompactedPending,omitempty"`
	Budget                     BudgetConfig               `json:"budget"`
	DailyBudgetPct             float64                    `json:"dailyBudgetPct"`
	DeepBudgetPct              float64                    `json:"deepBudgetPct"`
	DailyTrend                 []DailyBurnPoint           `json:"dailyTrend"`
	HourlyTrend                []HourlyBurnPoint          `json:"hourlyTrend,omitempty"`
	TierBreakdown              []TierBurnPoint            `json:"tierBreakdown"`
	Windows                    map[string]WindowBreakdown `json:"windows,omitempty"`
	TotalAnomalies             int                        `json:"totalAnomalies,omitempty"`
	Anomalies                  []TokenAnomaly             `json:"anomalies,omitempty"`
	TaskBadges                 map[string]TaskTokenBadge  `json:"taskBadges,omitempty"`
}

// ModelCanonicalTier derives "quick", "standard", or "deep" strictly from a model identifier.
func ModelCanonicalTier(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if strings.Contains(m, "pro") || strings.Contains(m, "carbon") || strings.Contains(m, "argon") || strings.Contains(m, "opus") || strings.Contains(m, "sonnet") {
		return "deep"
	}
	if strings.Contains(m, "flash_lite") || strings.Contains(m, "flash-lite") || strings.HasSuffix(m, "-low") {
		return "quick"
	}
	return "standard"
}

// ModelFamily classifies a model string into its UI model family ("claude", "carbon", "neon", "hydrogen", "other").
func ModelFamily(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(m, "claude") || strings.Contains(m, "opus") || strings.Contains(m, "sonnet"):
		return "claude"
	case strings.Contains(m, "carbon") || strings.Contains(m, "argon") || strings.Contains(m, "pro"):
		return "carbon"
	case strings.Contains(m, "neon"):
		return "neon"
	case strings.Contains(m, "hydrogen") || strings.Contains(m, "flash"):
		return "hydrogen"
	default:
		return "other"
	}
}

// NormalizeTier infers "quick", "standard", or "deep" from tier and model strings.
// When model is explicitly known (not empty/"inherit"/"auto"), the model's canonical tier takes precedence
// if tier is unset; callers checking actual model tier should use ModelCanonicalTier(model).
func NormalizeTier(tier, model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m != "" && m != "inherit" && m != "auto" && strings.TrimSpace(tier) == "" {
		return ModelCanonicalTier(m)
	}
	t := strings.ToLower(strings.TrimSpace(tier))
	if t == "quick" || t == "standard" || t == "deep" {
		return t
	}
	return ModelCanonicalTier(m)
}

// ModelRatesPerMillion returns API-equivalent USD rates per 1M tokens (input, cacheRead, output)
// derived strictly from the executed model when known, falling back to fallbackTier only when model is empty.
func ModelRatesPerMillion(model, fallbackTier string) (inPerM, cachePerM, outPerM float64) {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(m, "opus"):
		return 5.00, 0.50, 25.00
	case strings.Contains(m, "sonnet"):
		return 3.00, 0.30, 15.00
	case strings.Contains(m, "carbon") || strings.Contains(m, "argon") || strings.Contains(m, "pro"):
		return 1.25, 0.3125, 10.00
	case strings.Contains(m, "neon"):
		return 0.50, 0.125, 3.00
	case strings.Contains(m, "flash_lite") || strings.Contains(m, "flash-lite") || strings.HasSuffix(m, "-low"):
		return 0.10, 0.025, 0.40
	case strings.Contains(m, "hydrogen-medium") || m == "flash":
		return 0.25, 0.0625, 1.50
	case strings.Contains(m, "hydrogen") || strings.Contains(m, "flash"):
		return 0.30, 0.075, 2.50
	case m == "" || m == "inherit" || m == "auto":
		switch NormalizeTier(fallbackTier, "") {
		case "deep":
			return 1.25, 0.3125, 10.00
		case "quick":
			return 0.10, 0.025, 0.40
		default:
			return 0.30, 0.075, 2.50
		}
	default:
		return 0.30, 0.075, 2.50
	}
}

// ComputeQuotaCostAndSavings calculates estimated API-equivalent USD cost, prefix-cache USD savings,
// and internal QuotaUnits (1 QBU = $0.01 for backward CLI/JSON compatibility).
func ComputeQuotaCostAndSavings(input, cacheRead, output int64, model, tier string) (quotaUnits, estCostUSD, cacheSavedUSD float64) {
	inPerM, cachePerM, outPerM := ModelRatesPerMillion(model, tier)
	estCostUSD = (float64(input)*inPerM + float64(cacheRead)*cachePerM + float64(output)*outPerM) / 1_000_000.0
	if inPerM > cachePerM && cacheRead > 0 {
		cacheSavedUSD = float64(cacheRead) * (inPerM - cachePerM) / 1_000_000.0
	}
	quotaUnits = estCostUSD * 100.0
	return quotaUnits, estCostUSD, cacheSavedUSD
}

// ComputeQuotaAndCost calculates internal QuotaUnits (1 QBU = $0.01) and estimated API-equivalent USD cost.
func ComputeQuotaAndCost(input, cacheRead, output int64, model, tier string) (quotaUnits, estCostUSD float64) {
	q, c, _ := ComputeQuotaCostAndSavings(input, cacheRead, output, model, tier)
	return q, c
}

// RollupTurns computes a TokenRollup across a slice of TurnMetrics.
func RollupTurns(turns []TurnMetrics, totalSteps int, model, tier string) TokenRollup {
	r := TokenRollup{
		TurnCount: len(turns),
		StepCount: totalSteps,
	}
	for _, t := range turns {
		r.PlannerSteps += t.PlannerSteps
		r.InputTokens += t.InputTokens
		r.CacheReadTokens += t.CacheReadTokens
		r.OutputTokens += t.OutputTokens
		r.UncachedTokens += t.UncachedTokens
		r.GrossTokens += t.GrossTokens
		if t.PeakContextTokens > r.PeakContextTokens {
			r.PeakContextTokens = t.PeakContextTokens
		}
		r.CompactionCount += t.CompactionCount
		r.ActiveLatencyMs += t.ActiveLatencyMs
		r.HumanWaitMs += t.HumanWaitMs
		r.QuotaUnits += t.QuotaUnits
		r.EstCostUSD += t.EstCostUSD
		r.CacheSavedUSD += t.CacheSavedUSD
	}
	denom := r.InputTokens + r.CacheReadTokens
	if denom > 0 {
		r.CacheHitRatio = float64(r.CacheReadTokens) / float64(denom)
	}
	return r
}

// AddRollup combines two TokenRollup structs (used for DAG Tree rollups and bucket aggregations).
func AddRollup(a, b TokenRollup) TokenRollup {
	out := TokenRollup{
		TurnCount:         a.TurnCount + b.TurnCount,
		StepCount:         a.StepCount + b.StepCount,
		PlannerSteps:      a.PlannerSteps + b.PlannerSteps,
		InputTokens:       a.InputTokens + b.InputTokens,
		CacheReadTokens:   a.CacheReadTokens + b.CacheReadTokens,
		OutputTokens:      a.OutputTokens + b.OutputTokens,
		UncachedTokens:    a.UncachedTokens + b.UncachedTokens,
		GrossTokens:       a.GrossTokens + b.GrossTokens,
		PeakContextTokens: a.PeakContextTokens,
		CompactionCount:   a.CompactionCount + b.CompactionCount,
		ActiveLatencyMs:   a.ActiveLatencyMs + b.ActiveLatencyMs,
		HumanWaitMs:       a.HumanWaitMs + b.HumanWaitMs,
		QuotaUnits:        a.QuotaUnits + b.QuotaUnits,
		EstCostUSD:        a.EstCostUSD + b.EstCostUSD,
		CacheSavedUSD:     a.CacheSavedUSD + b.CacheSavedUSD,
	}
	if b.PeakContextTokens > out.PeakContextTokens {
		out.PeakContextTokens = b.PeakContextTokens
	}
	denom := out.InputTokens + out.CacheReadTokens
	if denom > 0 {
		out.CacheHitRatio = float64(out.CacheReadTokens) / float64(denom)
	}
	return out
}

// FormatTokensCompact formats a token count into a concise human string (e.g., "1.42M", "184k", "950").
func FormatTokensCompact(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000.0)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1_000.0)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000.0)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// FormatDurationCompact formats milliseconds into a compact human string (e.g., "4.2s", "1m15s").
func FormatDurationCompact(ms int64) string {
	if ms <= 0 {
		return "0s"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	sec := float64(ms) / 1000.0
	if sec < 60 {
		return fmt.Sprintf("%.1fs", sec)
	}
	mins := int(sec) / 60
	remSec := int(sec) % 60
	if mins < 60 {
		return fmt.Sprintf("%dm%02ds", mins, remSec)
	}
	hours := mins / 60
	remMins := mins % 60
	return fmt.Sprintf("%dh%02dm", hours, remMins)
}

func obsIndexFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "obs", "index.json")
}

// ResolveBrainDir returns the Antigravity brain directory to scan for conversation transcripts.
// When vaultDir is inside os.TempDir() (unit/E2E tests) and HQ_BRAIN_DIR / ANTIGRAVITY_APP_DATA_DIR
// are unset, hostpaths.Resolve returns a hermetic path inside the temp dir rather than scanning ~/.gemini.
func ResolveBrainDir(vaultDir string) string {
	return hostpaths.Resolve(vaultDir).BrainDir
}

type fileStatStamp struct {
	size    int64
	modNano int64
}

func statFileStamp(path string) fileStatStamp {
	if path == "" {
		return fileStatStamp{}
	}
	st, err := os.Stat(path)
	if err != nil {
		return fileStatStamp{size: -1, modNano: -1}
	}
	return fileStatStamp{size: st.Size(), modNano: st.ModTime().UnixNano()}
}

type obsMetaStamps struct {
	tasks        fileStatStamp
	archive      fileStatStamp
	nested       fileStatStamp
	fd           fileStatStamp
	fdTranscript fileStatStamp
	todayJournal fileStatStamp
}

func currentObsMetaStamps(vaultDir, brainDir string) obsMetaStamps {
	memDir := filepath.Join(vaultDir, "memory")
	_, _, fdSize, fdMod := currentFrontDeskTranscriptStamp(vaultDir, brainDir)
	return obsMetaStamps{
		tasks:        statFileStamp(filepath.Join(memDir, "tasks.json")),
		archive:      statFileStamp(filepath.Join(memDir, "tasks_archive.jsonl")),
		nested:       statFileStamp(filepath.Join(memDir, "nested_conversations.json")),
		fd:           statFileStamp(filepath.Join(memDir, "front_desk.json")),
		fdTranscript: fileStatStamp{size: fdSize, modNano: fdMod},
		todayJournal: statFileStamp(filepath.Join(memDir, "journal", time.Now().Format("2006-01-02"))),
	}
}

type journalDateCacheEntry struct {
	dirModNano int64
	hints      []transcriptCandidate
}

type obsMemCacheEntry struct {
	idx           *ObsIndex
	indexStamp    fileStatStamp
	metaStamps    obsMetaStamps
	brainDir      string
	lastCheckedAt time.Time
	journalByDate map[string]journalDateCacheEntry
}

var (
	obsCacheMu sync.Mutex
	obsMemByV  = make(map[string]*obsMemCacheEntry)
)

const currentObsIndexVersion = 6

func shallowCopyObsIndex(src *ObsIndex) *ObsIndex {
	if src == nil {
		return &ObsIndex{Version: currentObsIndexVersion, Conversations: make(map[string]ConversationMetrics)}
	}
	cp := &ObsIndex{
		Version:       src.Version,
		UpdatedAt:     src.UpdatedAt,
		Conversations: make(map[string]ConversationMetrics, len(src.Conversations)),
	}
	for k, v := range src.Conversations {
		cp.Conversations[k] = v
	}
	return cp
}

func loadCachedIndex(vaultDir string) *ObsIndex {
	idx := &ObsIndex{
		Version:       currentObsIndexVersion,
		Conversations: make(map[string]ConversationMetrics),
	}
	data, err := os.ReadFile(obsIndexFilePath(vaultDir))
	if err != nil {
		return idx
	}
	var parsed ObsIndex
	if json.Unmarshal(data, &parsed) == nil && parsed.Version == currentObsIndexVersion && parsed.Conversations != nil {
		return &parsed
	}
	return idx
}

func saveCachedIndex(vaultDir string, idx *ObsIndex) error {
	p := obsIndexFilePath(vaultDir)
	idx.Version = currentObsIndexVersion
	idx.UpdatedAt = time.Now().Format(time.RFC3339)
	data, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	return store.WriteAtomic(p, append(data, '\n'), 0o644)
}

type transcriptCandidate struct {
	convID         string
	transcriptPath string
	modelHint      string
	execNum        int
	spans          []JournalModelSpan
	fileSize       int64
	modTimeNano    int64
}

func parseExecNumFromJournalFilename(name string) int {
	trimmed := strings.TrimSuffix(name, ".json")
	idx := strings.LastIndexByte(trimmed, '-')
	if idx < 0 || idx+1 >= len(trimmed) {
		return 0
	}
	n, err := strconv.Atoi(trimmed[idx+1:])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func journalSpansEqual(a, b []JournalModelSpan) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func resolveTranscriptInDirWithStat(dir string) (string, int64, int64) {
	if dir == "" {
		return "", 0, 0
	}
	for _, p := range []string{
		filepath.Join(dir, ".system_generated", "logs", "transcript.jsonl"),
		filepath.Join(dir, "transcript.jsonl"),
	} {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, info.Size(), info.ModTime().UnixNano()
		}
	}
	return "", 0, 0
}

func resolveTranscriptInDir(dir string) string {
	p, _, _ := resolveTranscriptInDirWithStat(dir)
	return p
}

func mergeJournalCandidate(byConv map[string]transcriptCandidate, h transcriptCandidate, preferExistingModel bool) {
	if h.convID == "" {
		return
	}
	cand := byConv[h.convID]
	cand.convID = h.convID
	if h.modelHint != "" && (!preferExistingModel || cand.modelHint == "") {
		cand.modelHint = h.modelHint
	}
	if len(h.spans) > 0 {
		for _, sp := range h.spans {
			if sp.Model == "" {
				continue
			}
			dup := false
			for _, existing := range cand.spans {
				if existing.ExecNum == sp.ExecNum && existing.StepStart == sp.StepStart && existing.StepEnd == sp.StepEnd && existing.Model == sp.Model {
					dup = true
					break
				}
			}
			if !dup {
				cand.spans = append(cand.spans, sp)
			}
		}
	}
	if cand.transcriptPath == "" && h.transcriptPath != "" {
		if info, err := os.Stat(h.transcriptPath); err == nil && !info.IsDir() {
			cand.transcriptPath = h.transcriptPath
			cand.fileSize = info.Size()
			cand.modTimeNano = info.ModTime().UnixNano()
		}
	}
	if cand.transcriptPath != "" {
		byConv[h.convID] = cand
	}
}

func buildJournalHintFromRaw(raw []byte, filename string) (transcriptCandidate, bool) {
	var je struct {
		Timestamp      string `json:"timestamp"`
		ConversationID string `json:"conversationId"`
		ExecutionNum   int    `json:"executionNum"`
		TranscriptPath string `json:"transcriptPath"`
		ModelName      string `json:"modelName"`
		StepCount      int    `json:"stepCount"`
		StepStartIdx   int    `json:"stepStartIdx"`
		TurnStepCount  int    `json:"turnStepCount"`
	}
	if json.Unmarshal(raw, &je) != nil || strings.TrimSpace(je.ConversationID) == "" {
		return transcriptCandidate{}, false
	}
	cid := strings.ToLower(strings.TrimSpace(je.ConversationID))
	execNum := je.ExecutionNum
	if execNum <= 0 && filename != "" {
		execNum = parseExecNumFromJournalFilename(filename)
	}
	model := strings.TrimSpace(je.ModelName)
	var spans []JournalModelSpan
	if model != "" {
		stepStart := je.StepStartIdx
		stepEnd := je.StepCount - 1
		if stepEnd < stepStart && je.TurnStepCount > 0 {
			stepEnd = stepStart + je.TurnStepCount - 1
		}
		if stepEnd < stepStart {
			stepEnd = stepStart
		}
		spans = []JournalModelSpan{{
			ExecNum:   execNum,
			StepStart: stepStart,
			StepEnd:   stepEnd,
			Model:     model,
			Timestamp: strings.TrimSpace(je.Timestamp),
		}}
	}
	return transcriptCandidate{
		convID:         cid,
		transcriptPath: je.TranscriptPath,
		modelHint:      model,
		execNum:        execNum,
		spans:          spans,
	}, true
}

func discoverTranscriptsCached(vaultDir string, journalCache map[string]journalDateCacheEntry) []transcriptCandidate {
	byConv := make(map[string]transcriptCandidate)

	// 1. Scan brain directory (<brainDir>/<convId>/.system_generated/logs/transcript.jsonl)
	brainDir := ResolveBrainDir(vaultDir)
	if brainDir != "" {
		if entries, err := os.ReadDir(brainDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				cid := strings.ToLower(strings.TrimSpace(e.Name()))
				if tp, sz, mt := resolveTranscriptInDirWithStat(filepath.Join(brainDir, e.Name())); tp != "" {
					byConv[cid] = transcriptCandidate{convID: cid, transcriptPath: tp, fileSize: sz, modTimeNano: mt}
				}
			}
		}
	}

	// 2. Scan front_desk.json artifactDir
	if fd, ok := kernel.GetFrontDesk(vaultDir); ok && fd.ConversationID != "" && fd.ArtifactDir != "" {
		cid := strings.ToLower(strings.TrimSpace(fd.ConversationID))
		if _, exists := byConv[cid]; !exists {
			if tp, sz, mt := resolveTranscriptInDirWithStat(fd.ArtifactDir); tp != "" {
				byConv[cid] = transcriptCandidate{convID: cid, transcriptPath: tp, fileSize: sz, modTimeNano: mt}
			}
		}
	}

	// 3. Scan local/memory/journal/*/*.json using per-date-directory mtime cache (skipping archive/)
	journalRoot := filepath.Join(vaultDir, "memory", "journal")
	if dateDirs, err := os.ReadDir(journalRoot); err == nil {
		seenDates := make(map[string]bool, len(dateDirs))
		var sortedDates []os.DirEntry
		for _, d := range dateDirs {
			if !d.IsDir() || d.Type()&os.ModeSymlink != 0 || d.Name() == "archive" {
				continue
			}
			sortedDates = append(sortedDates, d)
		}
		sort.Slice(sortedDates, func(i, j int) bool { return sortedDates[i].Name() < sortedDates[j].Name() })
		for _, d := range sortedDates {
			dateName := d.Name()
			seenDates[dateName] = true
			datePath := filepath.Join(journalRoot, dateName)
			dirMod := int64(0)
			if dInfo, iErr := d.Info(); iErr == nil {
				dirMod = dInfo.ModTime().UnixNano()
			}
			var dateHints []transcriptCandidate
			if journalCache != nil && dirMod != 0 {
				if cachedDate, ok := journalCache[dateName]; ok && cachedDate.dirModNano == dirMod {
					dateHints = cachedDate.hints
				}
			}
			if dateHints == nil {
				files, _ := os.ReadDir(datePath)
				for _, f := range files {
					if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
						continue
					}
					b, rErr := os.ReadFile(filepath.Join(datePath, f.Name()))
					if rErr != nil {
						continue
					}
					if hint, ok := buildJournalHintFromRaw(b, f.Name()); ok {
						dateHints = append(dateHints, hint)
					}
				}
				// Sort numerically by executionNum (fixing lexicographical -10.json < -2.json ordering)
				sort.SliceStable(dateHints, func(i, j int) bool {
					if dateHints[i].convID != dateHints[j].convID {
						return dateHints[i].convID < dateHints[j].convID
					}
					if dateHints[i].execNum != dateHints[j].execNum {
						return dateHints[i].execNum < dateHints[j].execNum
					}
					ti, tj := "", ""
					if len(dateHints[i].spans) > 0 {
						ti = dateHints[i].spans[0].Timestamp
					}
					if len(dateHints[j].spans) > 0 {
						tj = dateHints[j].spans[0].Timestamp
					}
					return ti < tj
				})
				if journalCache != nil && dirMod != 0 {
					journalCache[dateName] = journalDateCacheEntry{
						dirModNano: dirMod,
						hints:      dateHints,
					}
				}
			}
			for _, h := range dateHints {
				mergeJournalCandidate(byConv, h, false)
			}
		}
		if journalCache != nil {
			for k := range journalCache {
				if !seenDates[k] {
					delete(journalCache, k)
				}
			}
		}
	}

	// 3b. Scan cold journal archives (memory/journal/archive/*.jsonl) for archived conversations
	archDir := filepath.Join(journalRoot, "archive")
	if archEntries, aErr := os.ReadDir(archDir); aErr == nil {
		for _, ae := range archEntries {
			if ae.IsDir() || ae.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(ae.Name(), ".jsonl") {
				continue
			}
			af, oErr := os.Open(filepath.Join(archDir, ae.Name()))
			if oErr != nil {
				continue
			}
			sc := bufio.NewScanner(af)
			sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
			for sc.Scan() {
				if hint, ok := buildJournalHintFromRaw(sc.Bytes(), ""); ok {
					mergeJournalCandidate(byConv, hint, true)
				}
			}
			_ = af.Close()
		}
	}

	var out []transcriptCandidate
	for _, c := range byConv {
		if c.transcriptPath != "" {
			if len(c.spans) > 1 {
				sort.SliceStable(c.spans, func(i, j int) bool {
					if c.spans[i].StepStart != c.spans[j].StepStart {
						return c.spans[i].StepStart < c.spans[j].StepStart
					}
					if c.spans[i].ExecNum != c.spans[j].ExecNum {
						return c.spans[i].ExecNum < c.spans[j].ExecNum
					}
					return c.spans[i].Timestamp < c.spans[j].Timestamp
				})
			}
			if len(c.spans) > 0 && c.spans[len(c.spans)-1].Model != "" {
				c.modelHint = c.spans[len(c.spans)-1].Model
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].convID < out[j].convID
	})
	return out
}

// LoadOrRefreshIndex incrementally parses new/modified transcripts and builds the full
// parent-child DAG and metadata attribution across Front Desk, Tasks, and Subagents.
// Concurrent callers within the same process are serialized via obsCacheMu and share the
// parsed in-memory index so large index.json files are never unmarshaled or rewritten redundantly.
func LoadOrRefreshIndex(vaultDir string) (*ObsIndex, error) {
	cleanVault := filepath.Clean(vaultDir)
	obsCacheMu.Lock()
	defer obsCacheMu.Unlock()

	now := time.Now()
	idxPath := obsIndexFilePath(vaultDir)
	diskStamp := statFileStamp(idxPath)
	curBrainDir := ResolveBrainDir(vaultDir)
	metaStamps := currentObsMetaStamps(vaultDir, curBrainDir)

	mem, hasMem := obsMemByV[cleanVault]
	if !hasMem || mem.idx == nil || mem.brainDir != curBrainDir || mem.indexStamp != diskStamp {
		loaded := loadCachedIndex(vaultDir)
		jCache := make(map[string]journalDateCacheEntry)
		if hasMem && mem.journalByDate != nil && mem.brainDir == curBrainDir {
			jCache = mem.journalByDate
		}
		mem = &obsMemCacheEntry{
			idx:           loaded,
			indexStamp:    diskStamp,
			brainDir:      curBrainDir,
			journalByDate: jCache,
		}
		obsMemByV[cleanVault] = mem
		hasMem = false // force at least one DAG/stat verification pass after disk load
	} else if !store.InGoTest() && now.Sub(mem.lastCheckedAt) < 10*time.Second && mem.metaStamps == metaStamps {
		return shallowCopyObsIndex(mem.idx), nil
	}

	idx := mem.idx
	candidates := discoverTranscriptsCached(vaultDir, mem.journalByDate)
	dirty := false

	seen := make(map[string]bool, len(candidates))
	for _, cand := range candidates {
		seen[cand.convID] = true
		fileSize := cand.fileSize
		modTimeNano := cand.modTimeNano
		if modTimeNano == 0 {
			info, err := os.Stat(cand.transcriptPath)
			if err != nil {
				continue
			}
			fileSize = info.Size()
			modTimeNano = info.ModTime().UnixNano()
		}
		cached, ok := idx.Conversations[cand.convID]
		if ok && cached.TranscriptPath == cand.transcriptPath && cached.FileSize == fileSize && cached.ModTimeNano == modTimeNano {
			updatedCached := false
			if !journalSpansEqual(cached.JournalSpans, cand.spans) {
				cached.JournalSpans = cand.spans
				updatedCached = true
			}
			if cand.modelHint != "" && (cached.Model == "" || cached.ModelSource == "fallback" || (cached.ModelSource == "journal" && cached.Model != cand.modelHint)) {
				cached.Model = cand.modelHint
				cached.ModelSource = "journal"
				updatedCached = true
			}
			if updatedCached {
				idx.Conversations[cand.convID] = cached
				dirty = true
			}
			continue
		}

		cm, pErr := ParseTranscriptFile(cand.convID, cand.transcriptPath, cand.modelHint, "")
		if pErr != nil {
			continue
		}
		cm.JournalSpans = cand.spans
		if cand.modelHint != "" {
			cm.ModelSource = "journal"
		}
		idx.Conversations[cand.convID] = cm
		dirty = true
	}

	// Remove stale entries whose transcript no longer exists
	for cid := range idx.Conversations {
		if !seen[cid] {
			delete(idx.Conversations, cid)
			dirty = true
		}
	}

	// Only re-run DAG metadata enrichment when transcripts changed, metadata files changed, or after cold load.
	if !hasMem || dirty || mem.metaStamps != metaStamps {
		if enrichAndBuildDAG(vaultDir, idx) {
			dirty = true
		}
	}

	if dirty {
		_ = saveCachedIndex(vaultDir, idx)
		mem.indexStamp = statFileStamp(idxPath)
	}
	mem.metaStamps = metaStamps
	mem.lastCheckedAt = now
	return shallowCopyObsIndex(idx), nil
}

// ParseSinceCutoff converts a window specifier ("today", "24h", "7d", "30d", "all", or Go duration) into a cutoff time.
func ParseSinceCutoff(since string, now time.Time) time.Time {
	return parseSinceCutoff(since, now)
}

func parseSinceCutoff(since string, now time.Time) time.Time {
	s := strings.ToLower(strings.TrimSpace(since))
	switch s {
	case "", "all":
		return time.Time{}
	case "today":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	case "24h", "1d":
		return now.Add(-24 * time.Hour)
	case "7d", "week":
		return now.Add(-7 * 24 * time.Hour)
	case "30d", "month":
		return now.Add(-30 * 24 * time.Hour)
	default:
		if dur, err := time.ParseDuration(s); err == nil {
			return now.Add(-dur)
		}
		return time.Time{}
	}
}

func turnEffectiveTime(t TurnMetrics) time.Time {
	ts := parseTimestamp(t.EndedAt)
	if ts.IsZero() {
		ts = parseTimestamp(t.StartedAt)
	}
	return ts
}

func turnMatchesCutoff(t TurnMetrics, cutoff time.Time) bool {
	if cutoff.IsZero() {
		return true
	}
	ts := turnEffectiveTime(t)
	if ts.IsZero() {
		return false
	}
	return !ts.Before(cutoff)
}

func turnLocalDateStr(t TurnMetrics, loc *time.Location) string {
	ts := turnEffectiveTime(t)
	if ts.IsZero() {
		return "unknown"
	}
	if loc != nil {
		ts = ts.In(loc)
	}
	return ts.Format("2006-01-02")
}

func turnLocalHourStr(t TurnMetrics, loc *time.Location) string {
	ts := turnEffectiveTime(t)
	if ts.IsZero() {
		return "unknown"
	}
	if loc != nil {
		ts = ts.In(loc)
	}
	return ts.Format("2006-01-02 15:00")
}

func turnModel(t TurnMetrics, cm ConversationMetrics) string {
	if t.Model != "" {
		return t.Model
	}
	if cm.Model != "" {
		return cm.Model
	}
	return "unknown"
}

func turnModelTier(t TurnMetrics, cm ConversationMetrics) string {
	if t.ModelTier != "" {
		return t.ModelTier
	}
	if t.Model != "" {
		return ModelCanonicalTier(t.Model)
	}
	if cm.Model != "" {
		return ModelCanonicalTier(cm.Model)
	}
	return NormalizeTier(cm.Tier, "")
}

// BuildSummaryReport produces the `hq obs [tokens]` summary and bucket breakdown.
func BuildSummaryReport(vaultDir, sinceFilter, groupBy string, now time.Time) (*ObsSummaryReport, error) {
	if now.IsZero() {
		now = time.Now()
	}
	if sinceFilter == "" {
		sinceFilter = "7d"
	}
	if groupBy == "" {
		groupBy = "day"
	}
	idx, err := LoadOrRefreshIndex(vaultDir)
	if err != nil {
		return nil, err
	}
	budget := LoadBudgetConfig(vaultDir)
	cutoff := parseSinceCutoff(sinceFilter, now)
	todayCutoff := parseSinceCutoff("today", now)

	rep := &ObsSummaryReport{
		GeneratedAt: now.Format(time.RFC3339),
		Since:       sinceFilter,
		GroupBy:     groupBy,
		Budget:      budget,
	}

	fd, hasFD := kernel.GetFrontDesk(vaultDir)
	if hasFD {
		rep.FrontDeskConvID = fd.ConversationID
		if fdCM, ok := idx.Conversations[strings.ToLower(fd.ConversationID)]; ok {
			cp := fdCM.Self
			rep.FrontDesk = &cp
			rep.FrontDeskContext = fdCM.CurrentContextTokens
			rep.FrontDeskCompactedPending = fdCM.CompactedPendingNextTurn
		}
	}

	type bucketConvTotals struct {
		uncached int64
		gross    int64
	}
	bucketMap := make(map[string]*BucketMetrics)
	bucketConvs := make(map[string]map[string]*bucketConvTotals)
	tierMap := make(map[string]*BucketMetrics)
	tierConvs := make(map[string]map[string]*bucketConvTotals)
	kindMap := make(map[string]*BucketMetrics)
	kindConvs := make(map[string]map[string]*bucketConvTotals)

	addToBucket := func(m map[string]*BucketMetrics, convTotals map[string]map[string]*bucketConvTotals, key, convID string, turn TurnMetrics) {
		if key == "" {
			key = "unknown"
		}
		b, ok := m[key]
		if !ok {
			b = &BucketMetrics{Key: key}
			m[key] = b
		}
		tr := RollupTurns([]TurnMetrics{turn}, turn.StepCount, "", "")
		b.Rollup = AddRollup(b.Rollup, tr)
		if convTotals != nil {
			if convTotals[key] == nil {
				convTotals[key] = make(map[string]*bucketConvTotals)
			}
			ct, seen := convTotals[key][convID]
			if !seen {
				ct = &bucketConvTotals{}
				convTotals[key][convID] = ct
				b.ConvCount++
			}
			ct.uncached += tr.UncachedTokens
			ct.gross += tr.GrossTokens
		}
	}

	convsSeen := make(map[string]bool)
	for cid, cm := range idx.Conversations {
		for _, t := range cm.Turns {
			tModel := turnModel(t, cm)
			tTier := turnModelTier(t, cm)
			if turnMatchesCutoff(t, todayCutoff) {
				rep.TodayUncached += t.UncachedTokens
				if tTier == "deep" {
					rep.TodayDeepUncached += t.UncachedTokens
				}
			}
			if !turnMatchesCutoff(t, cutoff) {
				continue
			}
			convsSeen[cid] = true
			tr := RollupTurns([]TurnMetrics{t}, t.StepCount, tModel, tTier)
			rep.Totals = AddRollup(rep.Totals, tr)

			var bKey string
			switch strings.ToLower(groupBy) {
			case "hour":
				bKey = turnLocalHourStr(t, now.Location())
			case "model":
				bKey = tModel
			case "tier":
				bKey = tTier
			case "kind":
				bKey = cm.Kind
			case "scope":
				bKey = cm.Scope
			default: // "day"
				bKey = turnLocalDateStr(t, now.Location())
			}
			addToBucket(bucketMap, bucketConvs, bKey, cid, t)
			addToBucket(tierMap, tierConvs, tTier, cid, t)
			addToBucket(kindMap, kindConvs, cm.Kind, cid, t)
		}
	}
	rep.ConversationCount = len(convsSeen)

	medianInt64 := func(vals []int64) int64 {
		n := len(vals)
		if n == 0 {
			return 0
		}
		sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
		if n%2 == 1 {
			return vals[n/2]
		}
		return (vals[n/2-1] + vals[n/2]) / 2
	}

	finalizeSlice := func(m map[string]*BucketMetrics, convTotals map[string]map[string]*bucketConvTotals, sortByKeyDesc bool) []BucketMetrics {
		var out []BucketMetrics
		for key, v := range m {
			if rep.Totals.QuotaUnits > 0 {
				v.ShareOfQBU = (v.Rollup.QuotaUnits / rep.Totals.QuotaUnits) * 100.0
			}
			if cts := convTotals[key]; len(cts) > 0 {
				uncVals := make([]int64, 0, len(cts))
				grossVals := make([]int64, 0, len(cts))
				for _, ct := range cts {
					uncVals = append(uncVals, ct.uncached)
					grossVals = append(grossVals, ct.gross)
				}
				n := int64(len(cts))
				v.AvgUncachedPerRun = v.Rollup.UncachedTokens / n
				v.P50UncachedPerRun = medianInt64(uncVals)
				v.AvgGrossPerRun = v.Rollup.GrossTokens / n
				v.P50GrossPerRun = medianInt64(grossVals)
			}
			out = append(out, *v)
		}
		sort.Slice(out, func(i, j int) bool {
			if sortByKeyDesc {
				return out[i].Key > out[j].Key
			}
			if out[i].Rollup.UncachedTokens != out[j].Rollup.UncachedTokens {
				return out[i].Rollup.UncachedTokens > out[j].Rollup.UncachedTokens
			}
			return out[i].Key < out[j].Key
		})
		return out
	}

	isTimeKey := groupBy == "day" || groupBy == "hour"
	rep.Buckets = finalizeSlice(bucketMap, bucketConvs, isTimeKey)
	rep.ByTier = finalizeSlice(tierMap, tierConvs, false)
	rep.ByKind = finalizeSlice(kindMap, kindConvs, false)
	rep.Anomalies = EvaluateAnomaliesSince(vaultDir, idx, budget, now, cutoff)
	return rep, nil
}

// BuildTopReport ranks conversations/tasks or individual turns by token burn, cost, gross tokens, context size, or latency.
// In --tree mode with kind="all", only disjoint root trees (ParentConversationID == "") are ranked by default.
func BuildTopReport(vaultDir, sortBy, kindFilter string, treeMode, turnsMode bool, limit int, now time.Time) (*TopReport, error) {
	return BuildTopReportWithOptions(vaultDir, sortBy, kindFilter, treeMode, turnsMode, false, limit, now)
}

// BuildTopReportWithOptions extends BuildTopReport with allNodes so callers can explicitly include intermediate DAG nodes in --tree mode.
func BuildTopReportWithOptions(vaultDir, sortBy, kindFilter string, treeMode, turnsMode, allNodes bool, limit int, now time.Time) (*TopReport, error) {
	if now.IsZero() {
		now = time.Now()
	}
	if sortBy == "" {
		sortBy = "uncached"
	}
	if kindFilter == "" {
		kindFilter = "all"
	}
	if limit <= 0 {
		limit = 10
	}
	idx, err := LoadOrRefreshIndex(vaultDir)
	if err != nil {
		return nil, err
	}

	rep := &TopReport{
		GeneratedAt: now.Format(time.RFC3339),
		SortBy:      sortBy,
		KindFilter:  kindFilter,
		TreeMode:    treeMode,
		TurnsMode:   turnsMode,
		AllNodes:    allNodes,
		Limit:       limit,
	}

	normKind := strings.ToLower(strings.TrimSpace(kindFilter))
	disjointRootsOnly := treeMode && !allNodes && (normKind == "" || normKind == "all")

	matchesKind := func(cm ConversationMetrics) bool {
		if disjointRootsOnly && cm.ParentConversationID != "" {
			return false
		}
		if normKind == "" || normKind == "all" {
			return true
		}
		if normKind == "subagent" {
			return cm.Kind == "subagent" || cm.Kind == "nested_subagent"
		}
		return cm.Kind == normKind
	}

	if turnsMode {
		var turnItems []TopTurnItem
		for _, cm := range idx.Conversations {
			if !matchesKind(cm) {
				continue
			}
			for _, t := range cm.Turns {
				if t.GrossTokens == 0 && t.StepCount <= 1 {
					continue
				}
				turnItems = append(turnItems, TopTurnItem{
					ConversationID: cm.ConversationID,
					TaskSlug:       cm.TaskSlug,
					Title:          cm.Title,
					Kind:           cm.Kind,
					Tier:           turnModelTier(t, cm),
					Model:          turnModel(t, cm),
					Turn:           t,
				})
			}
		}
		sort.Slice(turnItems, func(i, j int) bool {
			ti, tj := turnItems[i].Turn, turnItems[j].Turn
			switch strings.ToLower(sortBy) {
			case "cost", "usd", "qbu", "quota":
				return ti.EstCostUSD > tj.EstCostUSD
			case "gross":
				return ti.GrossTokens > tj.GrossTokens
			case "context":
				return ti.PeakContextTokens > tj.PeakContextTokens
			case "latency":
				return ti.ActiveLatencyMs > tj.ActiveLatencyMs
			default: // "uncached"
				if ti.UncachedTokens != tj.UncachedTokens {
					return ti.UncachedTokens > tj.UncachedTokens
				}
				return ti.GrossTokens > tj.GrossTokens
			}
		})
		if len(turnItems) > limit {
			turnItems = turnItems[:limit]
		}
		for i := range turnItems {
			turnItems[i].Rank = i + 1
		}
		rep.TurnItems = turnItems
		return rep, nil
	}

	var items []TopItem
	for _, cm := range idx.Conversations {
		if !matchesKind(cm) {
			continue
		}
		m := cm.Self
		if treeMode {
			m = cm.Tree
		}
		items = append(items, TopItem{
			ConversationID:       cm.ConversationID,
			ParentConversationID: cm.ParentConversationID,
			TaskSlug:             cm.TaskSlug,
			Title:                cm.Title,
			Kind:                 cm.Kind,
			Scope:                cm.Scope,
			Tier:                 cm.Tier,
			Model:                cm.Model,
			ChildCount:           len(cm.ChildConversationIDs),
			DescendantCount:      cm.DescendantCount,
			CurrentContextTokens: cm.CurrentContextTokens,
			Metrics:              m,
			SelfMetrics:          cm.Self,
			TreeMetrics:          cm.Tree,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		mi, mj := items[i].Metrics, items[j].Metrics
		switch strings.ToLower(sortBy) {
		case "cost", "usd", "qbu", "quota":
			return mi.EstCostUSD > mj.EstCostUSD
		case "gross":
			return mi.GrossTokens > mj.GrossTokens
		case "context":
			if items[i].CurrentContextTokens != items[j].CurrentContextTokens {
				return items[i].CurrentContextTokens > items[j].CurrentContextTokens
			}
			return mi.PeakContextTokens > mj.PeakContextTokens
		case "latency":
			return mi.ActiveLatencyMs > mj.ActiveLatencyMs
		default: // "uncached"
			if mi.UncachedTokens != mj.UncachedTokens {
				return mi.UncachedTokens > mj.UncachedTokens
			}
			return mi.GrossTokens > mj.GrossTokens
		}
	})

	if len(items) > limit {
		items = items[:limit]
	}
	for i := range items {
		items[i].Rank = i + 1
	}
	rep.Items = items
	return rep, nil
}

// BuildInspectReport returns detailed conversation + turn + child DAG telemetry for a conversation ID or taskSlug.
func BuildInspectReport(vaultDir, target string, now time.Time) (*InspectReport, error) {
	if now.IsZero() {
		now = time.Now()
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("conversation ID or taskSlug is required")
	}
	idx, err := LoadOrRefreshIndex(vaultDir)
	if err != nil {
		return nil, err
	}

	var matched *ConversationMetrics
	lowerTarget := strings.ToLower(target)
	if cm, ok := idx.Conversations[lowerTarget]; ok {
		cp := cm
		matched = &cp
	} else {
		for _, cm := range idx.Conversations {
			if strings.EqualFold(cm.TaskSlug, target) || strings.HasPrefix(cm.ConversationID, lowerTarget) {
				cp := cm
				matched = &cp
				break
			}
		}
	}
	if matched == nil {
		return nil, fmt.Errorf("conversation or task %q not found in observability index", target)
	}

	rep := &InspectReport{
		Conversation: *matched,
	}
	for _, childID := range matched.ChildConversationIDs {
		if childCM, ok := idx.Conversations[childID]; ok {
			rep.Children = append(rep.Children, childCM)
		}
	}
	budget := LoadBudgetConfig(vaultDir)
	allAnom := EvaluateAnomaliesSince(vaultDir, idx, budget, now, time.Time{})
	for _, a := range allAnom {
		if a.ConversationID == matched.ConversationID {
			rep.Anomalies = append(rep.Anomalies, a)
		}
	}
	return rep, nil
}

// BuildDashboardSummary computes the live observability payload for Web UI `/api/state`, `/api/obs`, and `hq_console.md`.
func BuildDashboardSummary(vaultDir string, now time.Time) DashboardSummary {
	if now.IsZero() {
		now = time.Now()
	}
	budget := LoadBudgetConfig(vaultDir)
	sum := DashboardSummary{
		UpdatedAt:              now.Format("15:04:05"),
		FrontDeskContextStatus: "OK",
		Budget:                 budget,
		TaskBadges:             make(map[string]TaskTokenBadge),
	}

	idx, err := LoadOrRefreshIndex(vaultDir)
	if err != nil || idx == nil {
		return sum
	}

	var hotConvIDs map[string]bool
	if len(idx.Conversations) > 64 {
		hotConvIDs = make(map[string]bool)
		if hotTasks, lErr := kernel.ListTasks(vaultDir); lErr == nil {
			for _, ht := range hotTasks {
				if cid := strings.ToLower(strings.TrimSpace(ht.ConversationID)); cid != "" {
					hotConvIDs[cid] = true
				}
			}
		}
		if fd, ok := kernel.GetFrontDesk(vaultDir); ok && fd.ConversationID != "" {
			hotConvIDs[strings.ToLower(strings.TrimSpace(fd.ConversationID))] = true
		}
	}

	todayStr := now.Format("2006-01-02")
	var allInput, allCache, allOutput int64

	// Prepare 7-day trend buckets
	dayBuckets := make(map[string]*DailyBurnPoint)
	var dayOrder []string
	for d := 6; d >= 0; d-- {
		ds := now.AddDate(0, 0, -d).Format("2006-01-02")
		dayOrder = append(dayOrder, ds)
		dayBuckets[ds] = &DailyBurnPoint{Date: ds}
	}

	tierBuckets := map[string]*TierBurnPoint{
		"deep":     {Tier: "deep"},
		"standard": {Tier: "standard"},
		"quick":    {Tier: "quick"},
	}
	tierInput := make(map[string]int64)
	tierCache := make(map[string]int64)

	for _, cm := range idx.Conversations {
		allInput += cm.Self.InputTokens
		allCache += cm.Self.CacheReadTokens
		allOutput += cm.Self.OutputTokens
		sum.AllTimeUncachedTokens += cm.Self.UncachedTokens
		sum.AllTimeGrossTokens += cm.Self.GrossTokens
		sum.AllTimeEstCostUSD += cm.Self.EstCostUSD
		sum.AllTimeCacheSavedUSD += cm.Self.CacheSavedUSD

		if hotConvIDs == nil || hotConvIDs[cm.ConversationID] {
			sum.TaskBadges[cm.ConversationID] = TaskTokenBadge{
				ConversationID:       cm.ConversationID,
				UncachedTokens:       cm.Self.UncachedTokens,
				TreeUncachedTokens:   cm.Tree.UncachedTokens,
				GrossTokens:          cm.Tree.GrossTokens,
				CacheHitRatio:        cm.Tree.CacheHitRatio,
				PeakContextTokens:    cm.Tree.PeakContextTokens,
				CurrentContextTokens: cm.CurrentContextTokens,
				QuotaUnits:           cm.Tree.QuotaUnits,
				EstCostUSD:           cm.Tree.EstCostUSD,
				ChildCount:           len(cm.ChildConversationIDs),
				DescendantCount:      cm.DescendantCount,
				CompactLabel: fmt.Sprintf("%s uncached · %.0f%% cached · ~$%.2f",
					FormatTokensCompact(cm.Tree.UncachedTokens),
					cm.Tree.CacheHitRatio*100.0,
					cm.Tree.EstCostUSD,
				),
			}
		}

		for _, t := range cm.Turns {
			tKey := turnModelTier(t, cm)
			tb := tierBuckets[tKey]
			if tb == nil {
				tb = tierBuckets["standard"]
			}
			tb.UncachedTokens += t.UncachedTokens
			tb.GrossTokens += t.GrossTokens
			tb.QuotaUnits += t.QuotaUnits
			tb.EstCostUSD += t.EstCostUSD
			tierInput[tKey] += t.InputTokens
			tierCache[tKey] += t.CacheReadTokens

			ds := turnLocalDateStr(t, now.Location())
			if dp, ok := dayBuckets[ds]; ok {
				dp.UncachedTokens += t.UncachedTokens
				dp.CacheReadTokens += t.CacheReadTokens
				dp.OutputTokens += t.OutputTokens
				dp.GrossTokens += t.GrossTokens
				dp.QuotaUnits += t.QuotaUnits
				dp.EstCostUSD += t.EstCostUSD
				dp.CacheSavedUSD += t.CacheSavedUSD
			}
			if ds == todayStr {
				sum.TodayInputTokens += t.InputTokens
				sum.TodayCacheReadTokens += t.CacheReadTokens
				sum.TodayOutputTokens += t.OutputTokens
				sum.TodayUncachedTokens += t.UncachedTokens
				sum.TodayGrossTokens += t.GrossTokens
				sum.TodayQuotaUnits += t.QuotaUnits
				sum.TodayEstCostUSD += t.EstCostUSD
				sum.TodayCacheSavedUSD += t.CacheSavedUSD
				sum.TodayTurnCount++
				if t.ColdStartInputTokens >= 50_000 {
					sum.TodayColdSpikes++
					sum.TodayColdSpikeTokens += t.ColdStartInputTokens
				}
				if tKey == "deep" {
					sum.TodayDeepUncachedTokens += t.UncachedTokens
				}
			}
		}
	}
	_ = allOutput

	if denom := sum.TodayInputTokens + sum.TodayCacheReadTokens; denom > 0 {
		sum.TodayCacheHitRatio = float64(sum.TodayCacheReadTokens) / float64(denom)
	}
	if denom := allInput + allCache; denom > 0 {
		sum.AllTimeCacheHitRatio = float64(allCache) / float64(denom)
	}

	for _, ds := range dayOrder {
		dp := dayBuckets[ds]
		inTok := dp.UncachedTokens - dp.OutputTokens
		if denom := inTok + dp.CacheReadTokens; denom > 0 {
			dp.CacheHitRatio = float64(dp.CacheReadTokens) / float64(denom)
		}
		sum.DailyTrend = append(sum.DailyTrend, *dp)
	}

	for _, tKey := range []string{"deep", "standard", "quick"} {
		tb := tierBuckets[tKey]
		if denom := tierInput[tKey] + tierCache[tKey]; denom > 0 {
			tb.CacheHitRatio = float64(tierCache[tKey]) / float64(denom)
		}
		if sum.AllTimeUncachedTokens > 0 {
			tb.SharePct = (float64(tb.UncachedTokens) / float64(sum.AllTimeUncachedTokens)) * 100.0
		}
		sum.TierBreakdown = append(sum.TierBreakdown, *tb)
	}

	if fd, ok := kernel.GetFrontDesk(vaultDir); ok && fd.ConversationID != "" {
		sum.FrontDeskConvID = fd.ConversationID
		if fdCM, exists := idx.Conversations[strings.ToLower(fd.ConversationID)]; exists {
			sum.FrontDeskContextTokens = fdCM.CurrentContextTokens
			sum.FrontDeskPostCompactTokens = fdCM.PostCompactContextTokens
			sum.FrontDeskPeakTokens = fdCM.Self.PeakContextTokens
			sum.FrontDeskCompactions = fdCM.Self.CompactionCount
			sum.FrontDeskCacheHitRatio = fdCM.Self.CacheHitRatio
			sum.FrontDeskCompactedPending = fdCM.CompactedPendingNextTurn
			switch {
			case fdCM.CompactedPendingNextTurn:
				sum.FrontDeskContextStatus = "OK"
			case fdCM.CurrentContextTokens >= budget.CritContextTokens:
				sum.FrontDeskContextStatus = "CRITICAL"
			case fdCM.CurrentContextTokens >= budget.WarnContextTokens:
				sum.FrontDeskContextStatus = "WARN"
			default:
				sum.FrontDeskContextStatus = "OK"
			}
		}
	}

	if !budget.TokenCircuitDisabled {
		if budget.DailyUncachedBudget > 0 {
			sum.DailyBudgetPct = (float64(sum.TodayUncachedTokens) / float64(budget.DailyUncachedBudget)) * 100.0
		}
		if budget.DailyDeepUncachedBudget > 0 {
			sum.DeepBudgetPct = (float64(sum.TodayDeepUncachedTokens) / float64(budget.DailyDeepUncachedBudget)) * 100.0
		}
	}

	populateDashboardWindows(&sum, idx, now)

	allAnomalies := EvaluateAnomalies(vaultDir, idx, budget, now)
	sum.TotalAnomalies = len(allAnomalies)
	if len(allAnomalies) > MaxDashboardAnomalies {
		sum.Anomalies = allAnomalies[:MaxDashboardAnomalies]
	} else {
		sum.Anomalies = allAnomalies
	}
	saveSummaryCache(vaultDir, ResolveBrainDir(vaultDir), now, sum)
	return sum
}

// GetFrontDeskContextStatus inspects the active Front Desk conversation's current prompt context
// and returns a concise 1-line `/compact` nudge banner when CurrentContextTokens >= NudgeContextTokens (160,000).
func GetFrontDeskContextStatus(vaultDir, transcriptPath string) (currentTokens int64, budget BudgetConfig, nudgeBanner string) {
	budget = LoadBudgetConfig(vaultDir)
	fd, ok := kernel.GetFrontDesk(vaultDir)
	if !ok || fd.ConversationID == "" {
		return 0, budget, ""
	}
	if transcriptPath == "" && fd.ArtifactDir != "" {
		transcriptPath = resolveTranscriptInDir(fd.ArtifactDir)
	}
	compactedPending := false
	if transcriptPath != "" {
		tm := ParseSingleTurnSlice(transcriptPath, 0)
		currentTokens = tm.CurrentContextTokens
		compactedPending = tm.CompactedPendingNextTurn
	} else if idx, err := LoadOrRefreshIndex(vaultDir); err == nil {
		if cm, exists := idx.Conversations[strings.ToLower(fd.ConversationID)]; exists {
			currentTokens = cm.CurrentContextTokens
			compactedPending = cm.CompactedPendingNextTurn
		}
	}
	if !compactedPending && budget.NudgeContextTokens > 0 && currentTokens >= budget.NudgeContextTokens {
		nudgeBanner = fmt.Sprintf(
			"## 💡 Front Desk Context Health (`%s` / `%s` tokens)\n"+
				"- Active `[HQ] Front Desk` context is at **%s tokens** (cold-wakeup hygiene threshold: `%s`, auto-compaction ceiling: `~%s`). To prevent cold-cache wakeup token spikes between idle turns, consider running `/compact` or starting a fresh Front Desk shift (`./bin/hq release-desk`).",
			FormatTokensCompact(currentTokens),
			FormatTokensCompact(budget.EffectiveContextCeiling()),
			FormatTokensCompact(currentTokens),
			FormatTokensCompact(budget.NudgeContextTokens),
			FormatTokensCompact(budget.EffectiveContextCeiling()),
		)
	}
	return currentTokens, budget, nudgeBanner
}
