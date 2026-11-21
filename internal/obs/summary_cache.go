package obs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/zjshen14/agy-hq/internal/store"
)

type summaryCacheRecord struct {
	Version   int              `json:"version"`
	Date      string           `json:"date"`
	WrittenAt string           `json:"writtenAt"`
	BrainDir  string           `json:"brainDir"`
	Summary   DashboardSummary `json:"summary"`
}

func obsSummaryCacheFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "obs", "summary_cache.json")
}

func saveSummaryCache(vaultDir, brainDir string, now time.Time, sum DashboardSummary) {
	if vaultDir == "" {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	rec := summaryCacheRecord{
		Version:   currentObsIndexVersion,
		Date:      now.Format("2006-01-02"),
		WrittenAt: now.UTC().Format(time.RFC3339Nano),
		BrainDir:  brainDir,
		Summary:   sum,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_ = store.WriteAtomic(obsSummaryCacheFilePath(vaultDir), append(data, '\n'), 0o644)
}

func loadSummaryCache(vaultDir, brainDir string, now time.Time, maxAge time.Duration) (DashboardSummary, bool) {
	if vaultDir == "" || maxAge <= 0 {
		return DashboardSummary{}, false
	}
	data, err := os.ReadFile(obsSummaryCacheFilePath(vaultDir))
	if err != nil {
		return DashboardSummary{}, false
	}
	var rec summaryCacheRecord
	if json.Unmarshal(data, &rec) != nil {
		return DashboardSummary{}, false
	}
	if rec.Version != currentObsIndexVersion || rec.BrainDir != brainDir {
		return DashboardSummary{}, false
	}
	if rec.Date != now.Format("2006-01-02") {
		return DashboardSummary{}, false
	}
	writtenAt, err := time.Parse(time.RFC3339Nano, rec.WrittenAt)
	if err != nil {
		return DashboardSummary{}, false
	}
	age := now.Sub(writtenAt)
	if age < 0 || age > maxAge {
		return DashboardSummary{}, false
	}
	sum := rec.Summary
	if sum.TaskBadges == nil {
		sum.TaskBadges = make(map[string]TaskTokenBadge)
	}
	budget := LoadBudgetConfig(vaultDir)
	sum.Budget = budget
	sum.DailyBudgetPct = 0
	sum.DeepBudgetPct = 0
	if !budget.TokenCircuitDisabled {
		if budget.DailyUncachedBudget > 0 {
			sum.DailyBudgetPct = (float64(sum.TodayUncachedTokens) / float64(budget.DailyUncachedBudget)) * 100.0
		}
		if budget.DailyDeepUncachedBudget > 0 {
			sum.DeepBudgetPct = (float64(sum.TodayDeepUncachedTokens) / float64(budget.DailyDeepUncachedBudget)) * 100.0
		}
	}
	return sum, true
}

// BuildDashboardSummaryCached returns a recently persisted DashboardSummary from
// local/memory/obs/summary_cache.json (~3 KB) when fresh within maxAge, avoiding full
// index.json unmarshaling and brain-directory rescans on cold hook/CLI invocations.
// Inside Go tests it always computes fresh to preserve deterministic test isolation.
func BuildDashboardSummaryCached(vaultDir string, now time.Time, maxAge time.Duration) DashboardSummary {
	if now.IsZero() {
		now = time.Now()
	}
	if maxAge > 0 && !store.InGoTest() {
		if sum, ok := loadSummaryCache(vaultDir, ResolveBrainDir(vaultDir), now, maxAge); ok {
			return sum
		}
	}
	return BuildDashboardSummary(vaultDir, now)
}
