package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/store"
)

const MaxProgressLines = 60

var reValidProgressSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9\-_]{0,79}$`)

// ProgressCheckpoint represents a structured cross-run / pre-compaction task handoff file
// stored at local/memory/progress/<slug>.md.
type ProgressCheckpoint struct {
	TaskSlug     string `json:"taskSlug"`
	Goal         string `json:"goal"`
	Completed    string `json:"completed"`
	NextSteps    string `json:"nextSteps"`
	Pointers     string `json:"pointers,omitempty"`
	PublicStatus string `json:"publicStatus,omitempty"`
	UpdatedAt    string `json:"updatedAt"`
	Path         string `json:"path"`
}

func progressDir(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "progress")
}

func normalizeProgressSlug(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimSuffix(s, ".md")
	if !reValidProgressSlug.MatchString(s) {
		return "", fmt.Errorf("invalid task slug %q (expected lowercase alphanumeric/kebab slug)", raw)
	}
	return s, nil
}

// SaveProgress writes a bounded, structured Markdown checkpoint to local/memory/progress/<slug>.md.
func SaveProgress(vaultDir string, cp ProgressCheckpoint, now time.Time) (ProgressCheckpoint, error) {
	slug, err := normalizeProgressSlug(cp.TaskSlug)
	if err != nil {
		return ProgressCheckpoint{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	cp.TaskSlug = slug
	cp.Goal = strings.TrimSpace(cp.Goal)
	cp.Completed = strings.TrimSpace(cp.Completed)
	cp.NextSteps = strings.TrimSpace(cp.NextSteps)
	cp.Pointers = strings.TrimSpace(cp.Pointers)
	cp.PublicStatus = strings.TrimSpace(cp.PublicStatus)
	if cp.Goal == "" && cp.Completed == "" && cp.NextSteps == "" && cp.PublicStatus == "" {
		return ProgressCheckpoint{}, fmt.Errorf("progress checkpoint requires at least one of --goal, --done, --next, or --public-status")
	}

	// Preserve existing curated PublicStatus when auto-checkpoints omit it, and preserve
	// existing Goal/Completed/NextSteps/Pointers when updating PublicStatus alone.
	if existing, _, ok, _ := LoadProgress(vaultDir, slug); ok {
		if cp.PublicStatus == "" && existing.PublicStatus != "" {
			cp.PublicStatus = existing.PublicStatus
		}
		if cp.Goal == "" && cp.Completed == "" && cp.NextSteps == "" && cp.Pointers == "" && cp.PublicStatus != "" {
			cp.Goal = existing.Goal
			cp.Completed = existing.Completed
			cp.NextSteps = existing.NextSteps
			cp.Pointers = existing.Pointers
		}
	}
	cp.UpdatedAt = now.UTC().Format(time.RFC3339)

	dir := progressDir(vaultDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ProgressCheckpoint{}, err
	}
	dst := filepath.Join(dir, slug+".md")
	cp.Path = dst

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Task Progress: `%s`\n", slug))
	sb.WriteString(fmt.Sprintf("- **Updated:** %s\n\n", cp.UpdatedAt))
	if cp.PublicStatus != "" {
		sb.WriteString("## Public Status\n" + cp.PublicStatus + "\n\n")
	}
	if cp.Goal != "" {
		sb.WriteString("## Goal\n" + cp.Goal + "\n\n")
	}
	if cp.Completed != "" {
		sb.WriteString("## Completed & Verified\n" + cp.Completed + "\n\n")
	}
	if cp.NextSteps != "" {
		sb.WriteString("## Next Steps\n" + cp.NextSteps + "\n\n")
	}
	if cp.Pointers != "" {
		sb.WriteString("## Pointers & Artifacts\n" + cp.Pointers + "\n")
	}

	lines := strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
	if len(lines) > MaxProgressLines {
		lines = append(lines[:MaxProgressLines-1], "*(truncated to 60-line progress checkpoint budget)*")
	}
	content := strings.Join(lines, "\n") + "\n"

	if err := withMemoryLock(vaultDir, func() error {
		return store.WriteAtomic(dst, []byte(content), 0o600)
	}); err != nil {
		return ProgressCheckpoint{}, err
	}
	return cp, nil
}

// LoadProgress reads and parses local/memory/progress/<slug>.md if present.
func LoadProgress(vaultDir, rawSlug string) (ProgressCheckpoint, string, bool, error) {
	slug, err := normalizeProgressSlug(rawSlug)
	if err != nil {
		return ProgressCheckpoint{}, "", false, err
	}
	p := filepath.Join(progressDir(vaultDir), slug+".md")
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return ProgressCheckpoint{}, "", false, nil
		}
		return ProgressCheckpoint{}, "", false, err
	}
	raw := string(data)
	cp := parseProgressMarkdown(slug, p, raw)
	return cp, raw, true, nil
}

func parseProgressMarkdown(slug, path, raw string) ProgressCheckpoint {
	cp := ProgressCheckpoint{
		TaskSlug: slug,
		Path:     path,
	}
	section := ""
	var buf map[string][]string = map[string][]string{}
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- **Updated:**") {
			cp.UpdatedAt = strings.TrimSpace(strings.TrimPrefix(trimmed, "- **Updated:**"))
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			h := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")))
			switch {
			case strings.HasPrefix(h, "public status"):
				section = "public_status"
			case strings.HasPrefix(h, "goal"):
				section = "goal"
			case strings.HasPrefix(h, "completed"):
				section = "completed"
			case strings.HasPrefix(h, "next"):
				section = "next"
			case strings.HasPrefix(h, "pointer"):
				section = "pointers"
			default:
				section = ""
			}
			continue
		}
		if section != "" {
			buf[section] = append(buf[section], line)
		}
	}
	cp.PublicStatus = strings.TrimSpace(strings.Join(buf["public_status"], "\n"))
	cp.Goal = strings.TrimSpace(strings.Join(buf["goal"], "\n"))
	cp.Completed = strings.TrimSpace(strings.Join(buf["completed"], "\n"))
	cp.NextSteps = strings.TrimSpace(strings.Join(buf["next"], "\n"))
	cp.Pointers = strings.TrimSpace(strings.Join(buf["pointers"], "\n"))
	return cp
}

// LoadPublicStatus returns ONLY the explicitly curated `## Public Status` section
// of local/memory/progress/<slug>.md (never exposing Goal, Completed, NextSteps, or Pointers).
func LoadPublicStatus(vaultDir, rawSlug string) (publicStatus, updatedAt string, ok bool, err error) {
	cp, _, found, err := LoadProgress(vaultDir, rawSlug)
	if err != nil || !found {
		return "", "", false, err
	}
	if strings.TrimSpace(cp.PublicStatus) == "" {
		return "", cp.UpdatedAt, false, nil
	}
	return cp.PublicStatus, cp.UpdatedAt, true, nil
}

// ListProgress returns all active task progress checkpoints sorted by UpdatedAt descending.
func ListProgress(vaultDir string) ([]ProgressCheckpoint, error) {
	names, err := listSortedRegularFiles(progressDir(vaultDir), ".md")
	if err != nil {
		return nil, err
	}
	var out []ProgressCheckpoint
	for _, name := range names {
		slug := strings.TrimSuffix(name, ".md")
		if cp, _, ok, lErr := LoadProgress(vaultDir, slug); lErr == nil && ok {
			out = append(out, cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out, nil
}

// ArchiveProgress moves local/memory/progress/<slug>.md into local/memory/progress/archived/ under store.RankMemory.
func ArchiveProgress(vaultDir, rawSlug string, now time.Time) (string, error) {
	slug, err := normalizeProgressSlug(rawSlug)
	if err != nil {
		return "", err
	}
	if now.IsZero() {
		now = time.Now()
	}
	var dst string
	err = withMemoryLock(vaultDir, func() error {
		src := filepath.Join(progressDir(vaultDir), slug+".md")
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("progress checkpoint %q not found", slug)
		}
		archDir := filepath.Join(progressDir(vaultDir), "archived")
		if err := os.MkdirAll(archDir, 0o700); err != nil {
			return err
		}
		dst = filepath.Join(archDir, fmt.Sprintf("%s_%s.md", now.UTC().Format("2006-01-02"), slug))
		return os.Rename(src, dst)
	})
	return dst, err
}

// ArchiveProgressBatch archives any existing progress checkpoints matching slugs, returning archived slugs.
func ArchiveProgressBatch(vaultDir string, slugs []string, now time.Time) []string {
	var archived []string
	for _, s := range slugs {
		if strings.TrimSpace(s) == "" {
			continue
		}
		if _, err := ArchiveProgress(vaultDir, s, now); err == nil {
			archived = append(archived, s)
		}
	}
	return archived
}

// SweepOrphanProgress archives any active local/memory/progress/<slug>.md file whose slug is not in activeSlugs.
func SweepOrphanProgress(vaultDir string, activeSlugs map[string]bool, now time.Time) []string {
	names, err := listSortedRegularFiles(progressDir(vaultDir), ".md")
	if err != nil {
		return nil
	}
	var orphans []string
	for _, name := range names {
		slug := strings.TrimSuffix(name, filepath.Ext(name))
		if !activeSlugs[strings.ToLower(slug)] {
			orphans = append(orphans, slug)
		}
	}
	return ArchiveProgressBatch(vaultDir, orphans, now)
}
