package obs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/store"
)

type summaryCacheRecord struct {
	Version              int              `json:"version"`
	Date                 string           `json:"date"`
	WrittenAt            string           `json:"writtenAt"`
	BrainDir             string           `json:"brainDir"`
	FrontDeskConvID      string           `json:"frontDeskConvId,omitempty"`
	FrontDeskTranscript  string           `json:"frontDeskTranscript,omitempty"`
	FrontDeskFileSize    int64            `json:"frontDeskFileSize,omitempty"`
	FrontDeskModTimeNano int64            `json:"frontDeskModTimeNano,omitempty"`
	TodayJournalSize     int64            `json:"todayJournalSize"`
	TodayJournalModNano  int64            `json:"todayJournalModNano"`
	ObsIndexSize         int64            `json:"obsIndexSize"`
	ObsIndexModNano      int64            `json:"obsIndexModNano"`
	TasksFileSize        int64            `json:"tasksFileSize"`
	TasksModTimeNano     int64            `json:"tasksModTimeNano"`
	Summary              DashboardSummary `json:"summary"`
}

func obsSummaryCacheFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "obs", "summary_cache.json")
}

// FrontDeskTranscriptStamp returns a lightweight O(1) stat fingerprint for the active
// Front Desk lease and its transcript.jsonl so caches and daemon ticks detect /compact
// checkpoints and new turns without scanning or parsing transcripts.
func FrontDeskTranscriptStamp(vaultDir string) (convID, transcriptPath string, size, modNano int64) {
	return currentFrontDeskTranscriptStamp(vaultDir, ResolveBrainDir(vaultDir))
}

func currentFrontDeskTranscriptStamp(vaultDir, brainDir string) (convID, transcriptPath string, size, modNano int64) {
	if vaultDir == "" {
		return "", "", 0, 0
	}
	fd, ok := kernel.GetFrontDesk(vaultDir)
	if !ok || strings.TrimSpace(fd.ConversationID) == "" {
		return "", "", 0, 0
	}
	rawID := strings.TrimSpace(fd.ConversationID)
	convID = strings.ToLower(rawID)
	if fd.ArtifactDir != "" {
		if tp, sz, mt := resolveTranscriptInDirWithStat(fd.ArtifactDir); tp != "" {
			return convID, tp, sz, mt
		}
	}
	if brainDir != "" {
		if tp, sz, mt := resolveTranscriptInDirWithStat(filepath.Join(brainDir, rawID)); tp != "" {
			return convID, tp, sz, mt
		}
		if convID != rawID {
			if tp, sz, mt := resolveTranscriptInDirWithStat(filepath.Join(brainDir, convID)); tp != "" {
				return convID, tp, sz, mt
			}
		}
	}
	return convID, "", 0, 0
}

// InvalidateSummaryCache removes the persisted summary_cache.json and clears the
// in-process index throttle timer so the next DashboardSummary / SyncMarkdownConsole
// call immediately recomputes from disk (e.g. right after a /compact CHECKPOINT).
func InvalidateSummaryCache(vaultDir string) {
	if vaultDir == "" {
		return
	}
	_ = os.Remove(obsSummaryCacheFilePath(vaultDir))
	cleanVault := filepath.Clean(vaultDir)
	obsCacheMu.Lock()
	if mem, ok := obsMemByV[cleanVault]; ok && mem != nil {
		mem.lastCheckedAt = time.Time{}
	}
	obsCacheMu.Unlock()
}

func currentNonFDActivityStamps(vaultDir string, dateStr string) (jStamp, idxStamp, tasksStamp fileStatStamp) {
	memDir := filepath.Join(vaultDir, "memory")
	jStamp = statFileStamp(filepath.Join(memDir, "journal", dateStr))
	idxStamp = statFileStamp(obsIndexFilePath(vaultDir))
	tasksStamp = statFileStamp(filepath.Join(memDir, "tasks.json"))
	return jStamp, idxStamp, tasksStamp
}

func saveSummaryCache(vaultDir, brainDir string, now time.Time, sum DashboardSummary) {
	if vaultDir == "" {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	dateStr := now.Format("2006-01-02")
	fdConvID, fdPath, fdSize, fdMod := currentFrontDeskTranscriptStamp(vaultDir, brainDir)
	jStamp, idxStamp, tasksStamp := currentNonFDActivityStamps(vaultDir, dateStr)
	rec := summaryCacheRecord{
		Version:              currentObsIndexVersion,
		Date:                 dateStr,
		WrittenAt:            now.UTC().Format(time.RFC3339Nano),
		BrainDir:             brainDir,
		FrontDeskConvID:      fdConvID,
		FrontDeskTranscript:  fdPath,
		FrontDeskFileSize:    fdSize,
		FrontDeskModTimeNano: fdMod,
		TodayJournalSize:     jStamp.size,
		TodayJournalModNano:  jStamp.modNano,
		ObsIndexSize:         idxStamp.size,
		ObsIndexModNano:      idxStamp.modNano,
		TasksFileSize:        tasksStamp.size,
		TasksModTimeNano:     tasksStamp.modNano,
		Summary:              sum,
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
	dateStr := now.Format("2006-01-02")
	if rec.Date != dateStr {
		return DashboardSummary{}, false
	}
	fdConvID, fdPath, fdSize, fdMod := currentFrontDeskTranscriptStamp(vaultDir, brainDir)
	if rec.FrontDeskConvID != fdConvID || rec.FrontDeskTranscript != fdPath || rec.FrontDeskFileSize != fdSize || rec.FrontDeskModTimeNano != fdMod {
		return DashboardSummary{}, false
	}
	jStamp, idxStamp, tasksStamp := currentNonFDActivityStamps(vaultDir, dateStr)
	if rec.TodayJournalSize != jStamp.size || rec.TodayJournalModNano != jStamp.modNano ||
		rec.ObsIndexSize != idxStamp.size || rec.ObsIndexModNano != idxStamp.modNano ||
		rec.TasksFileSize != tasksStamp.size || rec.TasksModTimeNano != tasksStamp.modNano {
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
