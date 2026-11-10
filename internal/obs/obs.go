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
	"github.com/zjshen14/agy-hq/internal/vault"
)

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

// TurnMetrics captures token burn, prefix cache efficiency, context watermarks, and 3-part latency for a single turn.
type TurnMetrics struct {
	TurnIndex            int      `json:"turnIndex"`
	StepStartIdx         int      `json:"stepStartIdx"`
	StepEndIdx           int      `json:"stepEndIdx"`
	StepCount            int      `json:"stepCount"`
	PlannerSteps         int      `json:"plannerSteps"`
	StartedAt            string   `json:"startedAt"`
	EndedAt              string   `json:"endedAt"`
	TriggerKind          string   `json:"triggerKind"` // "user" | "schedule" | "subagent_callback" | "system_wakeup"
	PromptPreview        string   `json:"promptPreview,omitempty"`
	InputTokens          int64    `json:"inputTokens"`
	CacheReadTokens      int64    `json:"cacheReadTokens"`
	OutputTokens         int64    `json:"outputTokens"`
	UncachedTokens       int64    `json:"uncachedTokens"`
	GrossTokens          int64    `json:"grossTokens"`
	CacheHitRatio        float64  `json:"cacheHitRatio"`
	PeakContextTokens    int64    `json:"peakContextTokens"`
	CurrentContextTokens int64    `json:"currentContextTokens"`
	CompactionCount      int      `json:"compactionCount"`
	LLMLatencyMs         int64    `json:"llmLatencyMs"`
	ToolLatencyMs        int64    `json:"toolLatencyMs"`
	HumanWaitMs          int64    `json:"humanWaitMs"`
	ActiveLatencyMs      int64    `json:"activeLatencyMs"`
	ToolsUsed            []string `json:"toolsUsed,omitempty"`
	ToolErrorCount       int      `json:"toolErrorCount,omitempty"`
	QuotaUnits           float64  `json:"quotaUnits"`
	EstCostUSD           float64  `json:"estCostUsd"`
}

// TokenRollup aggregates token, context, compaction, latency, and quota metrics across turns or DAG subtrees.
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
}

// ConversationMetrics stores full parsed metrics + DAG metadata for a single conversation.
type ConversationMetrics struct {
	ConversationID       string              `json:"conversationId"`
	ParentConversationID string              `json:"parentConversationId,omitempty"`
	RootConversationID   string              `json:"rootConversationId,omitempty"`
	TaskSlug             string              `json:"taskSlug,omitempty"`
	Title                string              `json:"title,omitempty"`
	Kind                 string              `json:"kind"` // "front_desk" | "task" | "subagent" | "nested_subagent" | "auto"
	Scope                string              `json:"scope,omitempty"`
	Tier                 string              `json:"tier"` // "quick" | "standard" | "deep"
	Model                string              `json:"model,omitempty"`
	Status               string              `json:"status,omitempty"`
	TranscriptPath       string              `json:"transcriptPath"`
	FileSize             int64               `json:"fileSize"`
	ModTimeNano          int64               `json:"modTimeNano"`
	StartedAt            string              `json:"startedAt,omitempty"`
	LastActiveAt         string              `json:"lastActiveAt,omitempty"`
	CurrentContextTokens int64               `json:"currentContextTokens"`
	ChildConversationIDs []string            `json:"childConversationIds,omitempty"`
	SpawnEdges           []SubagentSpawnEdge `json:"spawnEdges,omitempty"`
	Self                 TokenRollup         `json:"self"`
	Tree                 TokenRollup         `json:"tree"`
	Turns                []TurnMetrics       `json:"turns,omitempty"`
}

// ObsIndex is the stat-cached observability index persisted in local/memory/obs/index.json.
type ObsIndex struct {
	Version       int                            `json:"version"`
	UpdatedAt     string                         `json:"updatedAt"`
	Conversations map[string]ConversationMetrics `json:"conversations"`
}

// BucketMetrics represents an aggregated slice (by day, hour, model, tier, kind, or scope).
type BucketMetrics struct {
	Key        string      `json:"key"`
	Label      string      `json:"label,omitempty"`
	ConvCount  int         `json:"convCount"`
	Rollup     TokenRollup `json:"rollup"`
	ShareOfQBU float64     `json:"shareOfQbu"`
}

// ObsSummaryReport is returned by `hq obs [tokens]`.
type ObsSummaryReport struct {
	GeneratedAt       string          `json:"generatedAt"`
	Since             string          `json:"since"`
	GroupBy           string          `json:"groupBy"`
	ConversationCount int             `json:"conversationCount"`
	Totals            TokenRollup     `json:"totals"`
	FrontDesk         *TokenRollup    `json:"frontDesk,omitempty"`
	FrontDeskContext  int64           `json:"frontDeskContextTokens"`
	FrontDeskConvID   string          `json:"frontDeskConvId,omitempty"`
	Buckets           []BucketMetrics `json:"buckets"`
	ByTier            []BucketMetrics `json:"byTier"`
	ByKind            []BucketMetrics `json:"byKind"`
	Budget            BudgetConfig    `json:"budget"`
	TodayUncached     int64           `json:"todayUncachedTokens"`
	TodayDeepUncached int64           `json:"todayDeepUncachedTokens"`
	Anomalies         []TokenAnomaly  `json:"anomalies,omitempty"`
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
}

// TierBurnPoint summarizes token usage for a single tier ("quick", "standard", "deep").
type TierBurnPoint struct {
	Tier           string  `json:"tier"`
	UncachedTokens int64   `json:"uncachedTokens"`
	GrossTokens    int64   `json:"grossTokens"`
	CacheHitRatio  float64 `json:"cacheHitRatio"`
	QuotaUnits     float64 `json:"quotaUnits"`
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
	ChildCount           int     `json:"childCount"`
	CompactLabel         string  `json:"compactLabel"`
}

// DashboardSummary is embedded into Web UI `/api/state`, `/api/obs`, and `SyncMarkdownConsole`.
type DashboardSummary struct {
	UpdatedAt               string                    `json:"updatedAt"`
	TodayUncachedTokens     int64                     `json:"todayUncachedTokens"`
	TodayGrossTokens        int64                     `json:"todayGrossTokens"`
	TodayCacheHitRatio      float64                   `json:"todayCacheHitRatio"`
	TodayDeepUncachedTokens int64                     `json:"todayDeepUncachedTokens"`
	TodayQuotaUnits         float64                   `json:"todayQuotaUnits"`
	TodayTurnCount          int                       `json:"todayTurnCount"`
	AllTimeUncachedTokens   int64                     `json:"allTimeUncachedTokens"`
	AllTimeGrossTokens      int64                     `json:"allTimeGrossTokens"`
	AllTimeCacheHitRatio    float64                   `json:"allTimeCacheHitRatio"`
	FrontDeskConvID         string                    `json:"frontDeskConvId,omitempty"`
	FrontDeskContextTokens  int64                     `json:"frontDeskContextTokens"`
	FrontDeskPeakTokens     int64                     `json:"frontDeskPeakTokens"`
	FrontDeskCompactions    int                       `json:"frontDeskCompactions"`
	FrontDeskCacheHitRatio  float64                   `json:"frontDeskCacheHitRatio"`
	FrontDeskContextStatus  string                    `json:"frontDeskContextStatus"` // "OK" | "WARN" | "CRITICAL"
	Budget                  BudgetConfig              `json:"budget"`
	DailyBudgetPct          float64                   `json:"dailyBudgetPct"`
	DeepBudgetPct           float64                   `json:"deepBudgetPct"`
	DailyTrend              []DailyBurnPoint          `json:"dailyTrend"`
	TierBreakdown           []TierBurnPoint           `json:"tierBreakdown"`
	Anomalies               []TokenAnomaly            `json:"anomalies,omitempty"`
	TaskBadges              map[string]TaskTokenBadge `json:"taskBadges,omitempty"`
}

// NormalizeTier infers "quick", "standard", or "deep" from tier and model strings.
func NormalizeTier(tier, model string) string {
	t := strings.ToLower(strings.TrimSpace(tier))
	if t == "quick" || t == "standard" || t == "deep" {
		return t
	}
	m := strings.ToLower(strings.TrimSpace(model))
	if strings.Contains(m, "pro") || strings.Contains(m, "argon") || strings.Contains(m, "opus") || strings.Contains(m, "sonnet") {
		return "deep"
	}
	if strings.Contains(m, "flash_lite") || strings.Contains(m, "flash-lite") || strings.HasSuffix(m, "-low") {
		return "quick"
	}
	return "standard"
}

// ComputeQuotaAndCost calculates Quota Burn Units (QBU) and estimated API-equivalent USD cost.
func ComputeQuotaAndCost(input, cacheRead, output int64, model, tier string) (quotaUnits, estCostUSD float64) {
	normTier := NormalizeTier(tier, model)
	m := strings.ToLower(strings.TrimSpace(model))

	weight := 1.0
	inPerM := 0.30
	cachePerM := 0.075
	outPerM := 2.50

	switch {
	case strings.Contains(m, "opus"):
		weight = 8.0
		inPerM = 5.00
		cachePerM = 0.50
		outPerM = 25.00
	case normTier == "deep":
		weight = 5.0
		inPerM = 1.25
		cachePerM = 0.3125
		outPerM = 10.00
	case normTier == "quick":
		weight = 0.25
		inPerM = 0.10
		cachePerM = 0.025
		outPerM = 0.40
	}

	weightedTokens := float64(input) + 0.1*float64(cacheRead) + 3.0*float64(output)
	quotaUnits = weight * (weightedTokens / 1000.0)
	estCostUSD = (float64(input)*inPerM + float64(cacheRead)*cachePerM + float64(output)*outPerM) / 1_000_000.0
	return quotaUnits, estCostUSD
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
// are unset, it returns a hermetic path inside the temp dir rather than scanning the host's ~/.gemini.
func ResolveBrainDir(vaultDir string) string {
	if envBrain := strings.TrimSpace(os.Getenv("HQ_BRAIN_DIR")); envBrain != "" {
		return envBrain
	}
	appData := strings.TrimSpace(os.Getenv("ANTIGRAVITY_APP_DATA_DIR"))
	if vault.IsTempPath(vaultDir) {
		if appData != "" && vault.IsTempPath(appData) {
			return filepath.Join(appData, "brain")
		}
		return filepath.Join(filepath.Dir(filepath.Clean(vaultDir)), ".brain")
	}
	if appData != "" {
		return filepath.Join(appData, "brain")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".gemini", "antigravity", "brain")
	}
	return ""
}

func loadCachedIndex(vaultDir string) *ObsIndex {
	idx := &ObsIndex{
		Version:       2,
		Conversations: make(map[string]ConversationMetrics),
	}
	data, err := os.ReadFile(obsIndexFilePath(vaultDir))
	if err != nil {
		return idx
	}
	var parsed ObsIndex
	if json.Unmarshal(data, &parsed) == nil && parsed.Version == 2 && parsed.Conversations != nil {
		return &parsed
	}
	return idx
}

func saveCachedIndex(vaultDir string, idx *ObsIndex) error {
	p := obsIndexFilePath(vaultDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	idx.Version = 2
	idx.UpdatedAt = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

type transcriptCandidate struct {
	convID         string
	transcriptPath string
	modelHint      string
}

func discoverTranscripts(vaultDir string) []transcriptCandidate {
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
				p1 := filepath.Join(brainDir, e.Name(), ".system_generated", "logs", "transcript.jsonl")
				p2 := filepath.Join(brainDir, e.Name(), "transcript.jsonl")
				if info, err := os.Stat(p1); err == nil && !info.IsDir() {
					byConv[cid] = transcriptCandidate{convID: cid, transcriptPath: p1}
				} else if info, err := os.Stat(p2); err == nil && !info.IsDir() {
					byConv[cid] = transcriptCandidate{convID: cid, transcriptPath: p2}
				}
			}
		}
	}

	// 2. Scan front_desk.json artifactDir
	if fd, ok := kernel.GetFrontDesk(vaultDir); ok && fd.ConversationID != "" && fd.ArtifactDir != "" {
		cid := strings.ToLower(strings.TrimSpace(fd.ConversationID))
		p1 := filepath.Join(fd.ArtifactDir, ".system_generated", "logs", "transcript.jsonl")
		p2 := filepath.Join(fd.ArtifactDir, "transcript.jsonl")
		if _, exists := byConv[cid]; !exists {
			if info, err := os.Stat(p1); err == nil && !info.IsDir() {
				byConv[cid] = transcriptCandidate{convID: cid, transcriptPath: p1}
			} else if info, err := os.Stat(p2); err == nil && !info.IsDir() {
				byConv[cid] = transcriptCandidate{convID: cid, transcriptPath: p2}
			}
		}
	}

	// 3. Scan local/memory/journal/*/*.json for any recorded transcriptPath + modelName
	journalRoot := filepath.Join(vaultDir, "memory", "journal")
	if dateDirs, err := os.ReadDir(journalRoot); err == nil {
		for _, d := range dateDirs {
			if !d.IsDir() {
				continue
			}
			files, _ := os.ReadDir(filepath.Join(journalRoot, d.Name()))
			for _, f := range files {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
					continue
				}
				b, rErr := os.ReadFile(filepath.Join(journalRoot, d.Name(), f.Name()))
				if rErr != nil {
					continue
				}
				var je struct {
					ConversationID string `json:"conversationId"`
					TranscriptPath string `json:"transcriptPath"`
					ModelName      string `json:"modelName"`
				}
				if json.Unmarshal(b, &je) != nil || je.ConversationID == "" {
					continue
				}
				cid := strings.ToLower(strings.TrimSpace(je.ConversationID))
				cand := byConv[cid]
				cand.convID = cid
				if je.ModelName != "" {
					cand.modelHint = je.ModelName
				}
				if cand.transcriptPath == "" && je.TranscriptPath != "" {
					if info, sErr := os.Stat(je.TranscriptPath); sErr == nil && !info.IsDir() {
						cand.transcriptPath = je.TranscriptPath
					}
				}
				if cand.transcriptPath != "" {
					byConv[cid] = cand
				}
			}
		}
	}

	var out []transcriptCandidate
	for _, c := range byConv {
		if c.transcriptPath != "" {
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
func LoadOrRefreshIndex(vaultDir string) (*ObsIndex, error) {
	idx := loadCachedIndex(vaultDir)
	candidates := discoverTranscripts(vaultDir)
	dirty := false

	seen := make(map[string]bool)
	for _, cand := range candidates {
		seen[cand.convID] = true
		info, err := os.Stat(cand.transcriptPath)
		if err != nil {
			continue
		}
		cached, ok := idx.Conversations[cand.convID]
		if ok && cached.TranscriptPath == cand.transcriptPath && cached.FileSize == info.Size() && cached.ModTimeNano == info.ModTime().UnixNano() {
			if cand.modelHint != "" && cached.Model == "" {
				cached.Model = cand.modelHint
				idx.Conversations[cand.convID] = cached
				dirty = true
			}
			continue
		}

		cm, pErr := ParseTranscriptFile(cand.convID, cand.transcriptPath, cand.modelHint, "")
		if pErr != nil {
			continue
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

	// Enrich metadata from tasks.json, tasks_archive.jsonl, front_desk.json, and ground-truth spawn edges,
	// then compute DAG Tree rollups.
	if enrichAndBuildDAG(vaultDir, idx) {
		dirty = true
	}

	if dirty {
		_ = saveCachedIndex(vaultDir, idx)
	}
	return idx, nil
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

func turnMatchesCutoff(t TurnMetrics, cutoff time.Time) bool {
	if cutoff.IsZero() {
		return true
	}
	ts := parseTimestamp(t.EndedAt)
	if ts.IsZero() {
		ts = parseTimestamp(t.StartedAt)
	}
	if ts.IsZero() {
		return false
	}
	return !ts.Before(cutoff)
}

func localDateStr(rfc3339Str string, loc *time.Location) string {
	ts := parseTimestamp(rfc3339Str)
	if ts.IsZero() {
		return "unknown"
	}
	if loc != nil {
		ts = ts.In(loc)
	}
	return ts.Format("2006-01-02")
}

func localHourStr(rfc3339Str string, loc *time.Location) string {
	ts := parseTimestamp(rfc3339Str)
	if ts.IsZero() {
		return "unknown"
	}
	if loc != nil {
		ts = ts.In(loc)
	}
	return ts.Format("2006-01-02 15:00")
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
		}
	}

	bucketMap := make(map[string]*BucketMetrics)
	bucketConvs := make(map[string]map[string]bool)
	tierMap := make(map[string]*BucketMetrics)
	kindMap := make(map[string]*BucketMetrics)

	addToBucket := func(m map[string]*BucketMetrics, convSet map[string]map[string]bool, key, convID string, turn TurnMetrics) {
		if key == "" {
			key = "unknown"
		}
		b, ok := m[key]
		if !ok {
			b = &BucketMetrics{Key: key}
			m[key] = b
		}
		if convSet != nil {
			if convSet[key] == nil {
				convSet[key] = make(map[string]bool)
			}
			if !convSet[key][convID] {
				convSet[key][convID] = true
				b.ConvCount++
			}
		}
		tr := RollupTurns([]TurnMetrics{turn}, turn.StepCount, "", "")
		b.Rollup = AddRollup(b.Rollup, tr)
	}

	convsSeen := make(map[string]bool)
	for cid, cm := range idx.Conversations {
		for _, t := range cm.Turns {
			if turnMatchesCutoff(t, todayCutoff) {
				rep.TodayUncached += t.UncachedTokens
				if cm.Tier == "deep" {
					rep.TodayDeepUncached += t.UncachedTokens
				}
			}
			if !turnMatchesCutoff(t, cutoff) {
				continue
			}
			convsSeen[cid] = true
			tr := RollupTurns([]TurnMetrics{t}, t.StepCount, cm.Model, cm.Tier)
			rep.Totals = AddRollup(rep.Totals, tr)

			var bKey string
			switch strings.ToLower(groupBy) {
			case "hour":
				bKey = localHourStr(t.StartedAt, now.Location())
			case "model":
				bKey = cm.Model
				if bKey == "" {
					bKey = "unknown"
				}
			case "tier":
				bKey = cm.Tier
			case "kind":
				bKey = cm.Kind
			case "scope":
				bKey = cm.Scope
			default: // "day"
				bKey = localDateStr(t.StartedAt, now.Location())
			}
			addToBucket(bucketMap, bucketConvs, bKey, cid, t)
			addToBucket(tierMap, nil, cm.Tier, cid, t)
			addToBucket(kindMap, nil, cm.Kind, cid, t)
		}
	}
	rep.ConversationCount = len(convsSeen)

	finalizeSlice := func(m map[string]*BucketMetrics, sortByKeyDesc bool) []BucketMetrics {
		var out []BucketMetrics
		for _, v := range m {
			if rep.Totals.QuotaUnits > 0 {
				v.ShareOfQBU = (v.Rollup.QuotaUnits / rep.Totals.QuotaUnits) * 100.0
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
	rep.Buckets = finalizeSlice(bucketMap, isTimeKey)
	rep.ByTier = finalizeSlice(tierMap, false)
	rep.ByKind = finalizeSlice(kindMap, false)
	rep.Anomalies = EvaluateAnomalies(vaultDir, idx, budget, now)
	return rep, nil
}

// BuildTopReport ranks conversations/tasks or individual turns by token burn, QBU, gross tokens, context size, or latency.
func BuildTopReport(vaultDir, sortBy, kindFilter string, treeMode, turnsMode bool, limit int, now time.Time) (*TopReport, error) {
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
		Limit:       limit,
	}

	matchesKind := func(cm ConversationMetrics) bool {
		k := strings.ToLower(strings.TrimSpace(kindFilter))
		if k == "" || k == "all" {
			return true
		}
		if k == "subagent" {
			return cm.Kind == "subagent" || cm.Kind == "nested_subagent"
		}
		return cm.Kind == k
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
					Tier:           cm.Tier,
					Model:          cm.Model,
					Turn:           t,
				})
			}
		}
		sort.Slice(turnItems, func(i, j int) bool {
			ti, tj := turnItems[i].Turn, turnItems[j].Turn
			switch strings.ToLower(sortBy) {
			case "qbu", "quota":
				return ti.QuotaUnits > tj.QuotaUnits
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
			CurrentContextTokens: cm.CurrentContextTokens,
			Metrics:              m,
			SelfMetrics:          cm.Self,
			TreeMetrics:          cm.Tree,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		mi, mj := items[i].Metrics, items[j].Metrics
		switch strings.ToLower(sortBy) {
		case "qbu", "quota":
			return mi.QuotaUnits > mj.QuotaUnits
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
	allAnom := EvaluateAnomalies(vaultDir, idx, budget, now)
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

	todayStr := now.Format("2006-01-02")
	var todayInput, todayCache, todayOutput int64
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

		tKey := NormalizeTier(cm.Tier, cm.Model)
		tb := tierBuckets[tKey]
		tb.UncachedTokens += cm.Self.UncachedTokens
		tb.GrossTokens += cm.Self.GrossTokens
		tb.QuotaUnits += cm.Self.QuotaUnits
		tierInput[tKey] += cm.Self.InputTokens
		tierCache[tKey] += cm.Self.CacheReadTokens

		badge := TaskTokenBadge{
			ConversationID:       cm.ConversationID,
			UncachedTokens:       cm.Self.UncachedTokens,
			TreeUncachedTokens:   cm.Tree.UncachedTokens,
			GrossTokens:          cm.Tree.GrossTokens,
			CacheHitRatio:        cm.Tree.CacheHitRatio,
			PeakContextTokens:    cm.Tree.PeakContextTokens,
			CurrentContextTokens: cm.CurrentContextTokens,
			QuotaUnits:           cm.Tree.QuotaUnits,
			ChildCount:           len(cm.ChildConversationIDs),
			CompactLabel: fmt.Sprintf("%s uncached (%.0f%% CHR)",
				FormatTokensCompact(cm.Tree.UncachedTokens),
				cm.Tree.CacheHitRatio*100.0,
			),
		}
		sum.TaskBadges[cm.ConversationID] = badge

		for _, t := range cm.Turns {
			ds := localDateStr(t.StartedAt, now.Location())
			if dp, ok := dayBuckets[ds]; ok {
				dp.UncachedTokens += t.UncachedTokens
				dp.CacheReadTokens += t.CacheReadTokens
				dp.OutputTokens += t.OutputTokens
				dp.GrossTokens += t.GrossTokens
				dp.QuotaUnits += t.QuotaUnits
			}
			if ds == todayStr {
				todayInput += t.InputTokens
				todayCache += t.CacheReadTokens
				todayOutput += t.OutputTokens
				sum.TodayUncachedTokens += t.UncachedTokens
				sum.TodayGrossTokens += t.GrossTokens
				sum.TodayQuotaUnits += t.QuotaUnits
				sum.TodayTurnCount++
				if tKey == "deep" {
					sum.TodayDeepUncachedTokens += t.UncachedTokens
				}
			}
		}
	}

	if denom := todayInput + todayCache; denom > 0 {
		sum.TodayCacheHitRatio = float64(todayCache) / float64(denom)
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
			sum.FrontDeskPeakTokens = fdCM.Self.PeakContextTokens
			sum.FrontDeskCompactions = fdCM.Self.CompactionCount
			sum.FrontDeskCacheHitRatio = fdCM.Self.CacheHitRatio
			switch {
			case fdCM.CurrentContextTokens >= budget.CritContextTokens:
				sum.FrontDeskContextStatus = "CRITICAL"
			case fdCM.CurrentContextTokens >= budget.WarnContextTokens:
				sum.FrontDeskContextStatus = "WARN"
			default:
				sum.FrontDeskContextStatus = "OK"
			}
		}
	}

	if budget.DailyUncachedBudget > 0 {
		sum.DailyBudgetPct = (float64(sum.TodayUncachedTokens) / float64(budget.DailyUncachedBudget)) * 100.0
	}
	if budget.DailyDeepUncachedBudget > 0 {
		sum.DeepBudgetPct = (float64(sum.TodayDeepUncachedTokens) / float64(budget.DailyDeepUncachedBudget)) * 100.0
	}

	sum.Anomalies = EvaluateAnomalies(vaultDir, idx, budget, now)
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
		p1 := filepath.Join(fd.ArtifactDir, ".system_generated", "logs", "transcript.jsonl")
		p2 := filepath.Join(fd.ArtifactDir, "transcript.jsonl")
		if _, err := os.Stat(p1); err == nil {
			transcriptPath = p1
		} else if _, err := os.Stat(p2); err == nil {
			transcriptPath = p2
		}
	}
	if transcriptPath != "" {
		tm := ParseSingleTurnSlice(transcriptPath, 0)
		currentTokens = tm.CurrentContextTokens
	} else if idx, err := LoadOrRefreshIndex(vaultDir); err == nil {
		if cm, exists := idx.Conversations[strings.ToLower(fd.ConversationID)]; exists {
			currentTokens = cm.CurrentContextTokens
		}
	}
	if budget.NudgeContextTokens > 0 && currentTokens >= budget.NudgeContextTokens {
		nudgeBanner = fmt.Sprintf(
			"## 💡 Front Desk Context Health (`%s` / `256k` tokens)\n"+
				"- Active `[HQ] Front Desk` context is at **%s tokens** (threshold: `%s`). To prevent cold-cache wakeup token spikes between idle turns, consider running `/compact` or starting a fresh Front Desk shift (`./bin/hq release-desk`).",
			FormatTokensCompact(currentTokens),
			FormatTokensCompact(currentTokens),
			FormatTokensCompact(budget.NudgeContextTokens),
		)
	}
	return currentTokens, budget, nudgeBanner
}
