package outbox

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	MaxReportedRetention = 72 * time.Hour
	MaxReportedStored    = 100
	DefaultDigestLimit   = 12
	DefaultDigestWindow  = 48 * time.Hour
)

// ReportedEntry records an outbound alert or rollup item already delivered to the user
// so subsequent disjointed agent runs do not repeat the same message.
type ReportedEntry struct {
	Timestamp   string `json:"timestamp"`
	ItemID      string `json:"itemId,omitempty"`
	Tier        string `json:"tier"`
	SourceSkill string `json:"sourceSkill"`
	Scope       string `json:"scope,omitempty"`
	Title       string `json:"title"`
	Summary     string `json:"summary,omitempty"`
}

func reportedFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "reported.jsonl")
}

// RecordReportedFromItem appends a compact anti-repetition record for a delivered OutboxItem.
func RecordReportedFromItem(vaultDir string, it OutboxItem, now time.Time) error {
	sum := strings.Join(strings.Fields(strings.TrimSpace(it.Summary)), " ")
	if runes := []rune(sum); len(runes) > 120 {
		sum = string(runes[:117]) + "..."
	}
	return RecordReported(vaultDir, ReportedEntry{
		Timestamp:   now.UTC().Format(time.RFC3339),
		ItemID:      it.ID,
		Tier:        it.Tier,
		SourceSkill: it.SourceSkill,
		Scope:       it.Scope,
		Title:       strings.TrimSpace(it.Title),
		Summary:     sum,
	}, now)
}

// RecordReported appends an entry to local/memory/reported.jsonl and compacts entries older than 72h.
func RecordReported(vaultDir string, entry ReportedEntry, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	if strings.TrimSpace(entry.Timestamp) == "" {
		entry.Timestamp = now.UTC().Format(time.RFC3339)
	}
	existing, _ := LoadRecentReported(vaultDir, now, MaxReportedRetention, MaxReportedStored)
	if entry.ItemID != "" {
		for _, prev := range existing {
			if prev.ItemID == entry.ItemID {
				return nil
			}
		}
	}
	existing = append(existing, entry)
	if len(existing) > MaxReportedStored {
		existing = existing[len(existing)-MaxReportedStored:]
	}

	p := reportedFilePath(vaultDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	var sb strings.Builder
	for _, r := range existing {
		b, err := json.Marshal(r)
		if err != nil {
			continue
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return os.WriteFile(p, []byte(sb.String()), 0o600)
}

// LoadRecentReported returns recent reported entries within window, ordered oldest to newest (capped at limit).
func LoadRecentReported(vaultDir string, now time.Time, window time.Duration, limit int) ([]ReportedEntry, error) {
	if now.IsZero() {
		now = time.Now()
	}
	if window <= 0 {
		window = DefaultDigestWindow
	}
	if limit <= 0 {
		limit = DefaultDigestLimit
	}
	f, err := os.Open(reportedFilePath(vaultDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	cutoff := now.Add(-window)
	var out []ReportedEntry
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 512*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e ReportedEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if ts, pErr := time.Parse(time.RFC3339, e.Timestamp); pErr == nil && ts.Before(cutoff) {
			continue
		}
		out = append(out, e)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, scanner.Err()
}

// FormatReportedDigest renders a compact SessionStart anti-repetition block (<= 250 tokens).
func FormatReportedDigest(vaultDir string, now time.Time, limit int) string {
	entries, err := LoadRecentReported(vaultDir, now, DefaultDigestWindow, limit)
	if err != nil || len(entries) == 0 {
		return ""
	}
	var lines []string
	lines = append(lines, "> **Anti-Repetition Rule:** The items below were already delivered to the user recently. Do NOT send duplicate alerts unless material facts changed.")
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		scopeTag := e.SourceSkill
		if e.Scope != "" {
			scopeTag += ":" + e.Scope
		}
		if e.Summary != "" {
			lines = append(lines, fmt.Sprintf("- [%s · `%s`] **%s** — %s", e.Timestamp, scopeTag, e.Title, e.Summary))
		} else {
			lines = append(lines, fmt.Sprintf("- [%s · `%s`] **%s**", e.Timestamp, scopeTag, e.Title))
		}
	}
	return strings.Join(lines, "\n")
}
