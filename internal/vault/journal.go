package vault

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/store"
)

const DefaultHotArchiveDays = 14

// JournalRecord is the subset of fields shared across memory/journal/YYYY-MM-DD/*.json
// and compacted memory/journal/archive/YYYY-MM.jsonl records.
type JournalRecord struct {
	Timestamp         string   `json:"timestamp"`
	ConversationID    string   `json:"conversationId"`
	ParentConvID      string   `json:"parentConversationId,omitempty"`
	ExecutionNum      int      `json:"executionNum"`
	TranscriptPath    string   `json:"transcriptPath,omitempty"`
	WorkspacePaths    []string `json:"workspacePaths,omitempty"`
	ModelName         string   `json:"modelName,omitempty"`
	StepCount         int      `json:"stepCount,omitempty"`
	ToolErrorCount    int      `json:"toolErrorCount,omitempty"`
	StepStartIdx      int      `json:"stepStartIdx,omitempty"`
	TurnStepCount     int      `json:"turnStepCount,omitempty"`
	TurnErrorCount    int      `json:"turnErrorCount,omitempty"`
	PreToolDenials    int      `json:"preToolDenials,omitempty"`
	TaintedSession    bool     `json:"taintedSession,omitempty"`
	UserPrompt        string   `json:"userPrompt,omitempty"`
	ToolsUsed         []string `json:"toolsUsed,omitempty"`
	FilesModified     []string `json:"filesModified,omitempty"`
	TerminationReason string   `json:"terminationReason,omitempty"`
	ResponseSummary   string   `json:"responseSummary,omitempty"`
}

// CompactMemoryReport summarizes a unified memory compaction & lifecycle archival pass.
type CompactMemoryReport struct {
	FactsBefore      int      `json:"factsBefore"`
	FactsAfter       int      `json:"factsAfter"`
	ArchivedProgress []string `json:"archivedProgress,omitempty"`
	ArchivedJournals int      `json:"archivedJournals"`
	ArchivedBriefs   int      `json:"archivedBriefs"`
}

// WalkJournals iterates over hot memory/journal/YYYY-MM-DD/*.json records (skipping "archive" dir)
// and optionally cold memory/journal/archive/YYYY-MM.jsonl records, filtered by since (if non-zero).
// Lock-free and Lstat symlink-safe.
func WalkJournals(vaultDir string, since time.Time, includeArchive bool, fn func(path string, rec JournalRecord) bool) error {
	journalRoot := filepath.Join(vaultDir, "memory", "journal")
	dateDirs, err := os.ReadDir(journalRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var hotDirNames []string
	for _, d := range dateDirs {
		if !d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			continue
		}
		if d.Name() == "archive" || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		hotDirNames = append(hotDirNames, d.Name())
	}
	sort.Strings(hotDirNames)

	if includeArchive {
		archDir := filepath.Join(journalRoot, "archive")
		if archEntries, aErr := os.ReadDir(archDir); aErr == nil {
			var archFiles []string
			for _, ae := range archEntries {
				if ae.IsDir() || ae.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(ae.Name(), ".jsonl") {
					continue
				}
				archFiles = append(archFiles, ae.Name())
			}
			sort.Strings(archFiles)
			for _, af := range archFiles {
				p := filepath.Join(archDir, af)
				if fi, lErr := os.Lstat(p); lErr != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
					continue
				}
				f, oErr := os.Open(p)
				if oErr != nil {
					continue
				}
				scanner := bufio.NewScanner(f)
				scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
				stop := false
				for scanner.Scan() {
					line := strings.TrimSpace(scanner.Text())
					if line == "" {
						continue
					}
					var rec JournalRecord
					if json.Unmarshal([]byte(line), &rec) != nil {
						continue
					}
					if !since.IsZero() && rec.Timestamp != "" {
						if ts, tErr := time.Parse(time.RFC3339, rec.Timestamp); tErr == nil && ts.Before(since) {
							continue
						}
					}
					if !fn(p, rec) {
						stop = true
						break
					}
				}
				_ = f.Close()
				if stop {
					return nil
				}
			}
		}
	}

	for _, dn := range hotDirNames {
		subDir := filepath.Join(journalRoot, dn)
		files, fErr := os.ReadDir(subDir)
		if fErr != nil {
			continue
		}
		var names []string
		for _, fe := range files {
			if fe.IsDir() || fe.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(fe.Name(), ".json") {
				continue
			}
			names = append(names, fe.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			p := filepath.Join(subDir, name)
			if fi, lErr := os.Lstat(p); lErr != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
				continue
			}
			raw, rErr := os.ReadFile(p)
			if rErr != nil {
				continue
			}
			var rec JournalRecord
			if json.Unmarshal(raw, &rec) != nil {
				continue
			}
			if !since.IsZero() && rec.Timestamp != "" {
				if ts, tErr := time.Parse(time.RFC3339, rec.Timestamp); tErr == nil && ts.Before(since) {
					continue
				}
			}
			if !fn(p, rec) {
				return nil
			}
		}
	}
	return nil
}

// FindArchivedMaxStepCount returns the maximum recorded StepCount for convID in journal/archive/*.jsonl.
// Used as a fallback by HandleStop when a long-lived conversation spans > 14d archival windows.
func FindArchivedMaxStepCount(vaultDir, convID string) int {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return 0
	}
	archDir := filepath.Join(vaultDir, "memory", "journal", "archive")
	entries, err := os.ReadDir(archDir)
	if err != nil {
		return 0
	}
	maxStep := 0
	for _, e := range entries {
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(archDir, e.Name())
		f, oErr := os.Open(p)
		if oErr != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
		for scanner.Scan() {
			raw := scanner.Bytes()
			if !strings.Contains(string(raw), convID) {
				continue
			}
			var rec JournalRecord
			if json.Unmarshal(raw, &rec) == nil && rec.ConversationID == convID && rec.StepCount > maxStep {
				maxStep = rec.StepCount
			}
		}
		_ = f.Close()
	}
	return maxStep
}

// FindArchivedTranscriptPath resolves the most recent transcriptPath for convID from journal/archive/*.jsonl.
func FindArchivedTranscriptPath(vaultDir, convID string) string {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return ""
	}
	archDir := filepath.Join(vaultDir, "memory", "journal", "archive")
	entries, err := os.ReadDir(archDir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && e.Type()&os.ModeSymlink == 0 && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for i := len(names) - 1; i >= 0; i-- {
		p := filepath.Join(archDir, names[i])
		f, oErr := os.Open(p)
		if oErr != nil {
			continue
		}
		found := ""
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
		for scanner.Scan() {
			raw := scanner.Bytes()
			if !strings.Contains(string(raw), convID) {
				continue
			}
			var rec JournalRecord
			if json.Unmarshal(raw, &rec) == nil && rec.ConversationID == convID && rec.TranscriptPath != "" {
				found = rec.TranscriptPath
			}
		}
		_ = f.Close()
		if found != "" {
			return found
		}
	}
	return ""
}

// ArchiveOldJournals rolls hot memory/journal/YYYY-MM-DD/*.json folders older than hotDays
// into monthly JSONL files at memory/journal/archive/YYYY-MM.jsonl under store.RankMemory.
func ArchiveOldJournals(vaultDir string, hotDays int, now time.Time) (int, error) {
	if hotDays <= 0 {
		hotDays = DefaultHotArchiveDays
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoffDate := now.AddDate(0, 0, -hotDays).Format("2006-01-02")
	journalRoot := filepath.Join(vaultDir, "memory", "journal")
	archived := 0

	err := withMemoryLock(vaultDir, func() error {
		entries, err := os.ReadDir(journalRoot)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		archDir := filepath.Join(journalRoot, "archive")
		byMonth := make(map[string][]string)
		var dirsToRemove []string

		for _, d := range entries {
			if !d.IsDir() || d.Type()&os.ModeSymlink != 0 || d.Name() == "archive" {
				continue
			}
			if _, pErr := time.Parse("2006-01-02", d.Name()); pErr != nil {
				continue
			}
			if d.Name() >= cutoffDate {
				continue
			}
			monthKey := d.Name()[:7]
			subDir := filepath.Join(journalRoot, d.Name())
			files, fErr := os.ReadDir(subDir)
			if fErr != nil {
				continue
			}
			var names []string
			for _, fe := range files {
				if !fe.IsDir() && fe.Type()&os.ModeSymlink == 0 && strings.HasSuffix(fe.Name(), ".json") {
					names = append(names, fe.Name())
				}
			}
			sort.Strings(names)
			for _, name := range names {
				raw, rErr := os.ReadFile(filepath.Join(subDir, name))
				if rErr != nil {
					continue
				}
				var compact map[string]interface{}
				if json.Unmarshal(raw, &compact) != nil {
					continue
				}
				b, mErr := json.Marshal(compact)
				if mErr != nil {
					continue
				}
				byMonth[monthKey] = append(byMonth[monthKey], string(b))
				archived++
			}
			dirsToRemove = append(dirsToRemove, subDir)
		}

		if len(byMonth) > 0 {
			if err := os.MkdirAll(archDir, 0o700); err != nil {
				return err
			}
			for monthKey, newLines := range byMonth {
				dst := filepath.Join(archDir, monthKey+".jsonl")
				var existing []byte
				if b, rErr := os.ReadFile(dst); rErr == nil {
					existing = b
				}
				var sb strings.Builder
				if len(existing) > 0 {
					sb.Write(existing)
					if existing[len(existing)-1] != '\n' {
						sb.WriteByte('\n')
					}
				}
				for _, ln := range newLines {
					sb.WriteString(ln)
					sb.WriteByte('\n')
				}
				if wErr := store.WriteAtomic(dst, []byte(sb.String()), 0o600); wErr != nil {
					return wErr
				}
			}
		}
		for _, dirPath := range dirsToRemove {
			_ = os.RemoveAll(dirPath)
		}
		return nil
	})
	return archived, err
}

// ArchiveOldBriefs moves daily briefs in notes/briefs/*.md older than hotDays to notes/briefs/archive/YYYY-MM/.
func ArchiveOldBriefs(vaultDir string, hotDays int, now time.Time) (int, error) {
	if hotDays <= 0 {
		hotDays = DefaultHotArchiveDays
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.AddDate(0, 0, -hotDays)
	briefsDir := filepath.Join(vaultDir, "notes", "briefs")
	entries, err := os.ReadDir(briefsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	moved := 0
	for _, e := range entries {
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		fullPath := filepath.Join(briefsDir, e.Name())
		fi, lErr := os.Lstat(fullPath)
		if lErr != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			continue
		}
		fileTime := fi.ModTime()
		if len(e.Name()) >= 10 {
			if parsed, pErr := time.Parse("2006-01-02", e.Name()[:10]); pErr == nil {
				fileTime = parsed
			}
		}
		if !fileTime.Before(cutoff) {
			continue
		}
		monthDir := filepath.Join(briefsDir, "archive", fileTime.Format("2006-01"))
		if mkErr := os.MkdirAll(monthDir, 0o700); mkErr != nil {
			continue
		}
		if rErr := os.Rename(fullPath, filepath.Join(monthDir, e.Name())); rErr == nil {
			moved++
		}
	}
	return moved, nil
}

// CompactMemory executes the full Vault memory compaction & lifecycle archival pass:
// 1. CompactFacts (deduplicate + materialize elapsed ExpiresAt at 0600),
// 2. SweepOrphanProgress (archive progress/*.md not in activeTaskSlugs),
// 3. ArchiveOldJournals (14d hot -> YYYY-MM.jsonl),
// 4. ArchiveOldBriefs (14d hot -> notes/briefs/archive/YYYY-MM/).
func CompactMemory(vaultDir string, activeTaskSlugs []string, now time.Time) (*CompactMemoryReport, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rep := &CompactMemoryReport{}
	before, after, err := CompactFacts(vaultDir)
	if err != nil {
		return nil, err
	}
	rep.FactsBefore = before
	rep.FactsAfter = after

	activeSet := make(map[string]bool, len(activeTaskSlugs))
	for _, s := range activeTaskSlugs {
		if clean := strings.ToLower(strings.TrimSpace(s)); clean != "" {
			activeSet[clean] = true
		}
	}
	rep.ArchivedProgress = SweepOrphanProgress(vaultDir, activeSet, now)

	archJournals, jErr := ArchiveOldJournals(vaultDir, DefaultHotArchiveDays, now)
	if jErr == nil {
		rep.ArchivedJournals = archJournals
	}
	archBriefs, bErr := ArchiveOldBriefs(vaultDir, DefaultHotArchiveDays, now)
	if bErr == nil {
		rep.ArchivedBriefs = archBriefs
	}
	return rep, nil
}
