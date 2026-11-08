package vault

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// MaxMemoryIndexLines is the target line-count budget for local/MEMORY.md as a Roadmap Index.
	MaxMemoryIndexLines = 80
	// MaxMemoryIndexTokens is the target token budget for local/MEMORY.md.
	MaxMemoryIndexTokens = 1200
)

// TopicPage represents a single Knowledge Wiki page discovered in local/memory/topics/<slug>.md.
type TopicPage struct {
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	Path      string `json:"path"`
	LineCount int    `json:"lineCount"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// MemoryStatusReport summarizes the health, budget compliance, topic index, and fact counts of the Vault.
type MemoryStatusReport struct {
	MemoryLines      int         `json:"memoryLines"`
	MemoryMaxLines   int         `json:"memoryMaxLines"`
	EstimatedTokens  int         `json:"estimatedTokens"`
	OverBudget       bool        `json:"overBudget"`
	Topics           []TopicPage `json:"topics"`
	OrphanTopicSlugs []string    `json:"orphanTopicSlugs,omitempty"`
	ActiveFactCount  int         `json:"activeFactCount"`
	ExpiredFactCount int         `json:"expiredFactCount"`
	RawFactLines     int         `json:"rawFactLines"`
	StaleReferences  []string    `json:"staleReferences,omitempty"`
}

func factKey(f Fact) string {
	if id := strings.TrimSpace(f.ID); id != "" {
		return id
	}
	return "anon:" + strings.TrimSpace(f.Topic) + ":" + strings.TrimSpace(f.Statement)
}

// LoadFactsState reads local/memory/facts.jsonl, deduplicating records by Fact.ID in chronological
// order so later updates or soft-expirations (valid_to != nil) supersede earlier lines for the same ID.
func LoadFactsState(vaultDir string) (active []Fact, expired []Fact, rawLines int, err error) {
	factsPath := filepath.Join(vaultDir, "memory", "facts.jsonl")
	f, err := os.Open(factsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, 0, nil
		}
		return nil, nil, 0, err
	}
	defer f.Close()

	var order []string
	byKey := make(map[string]Fact)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		rawLines++
		var item Fact
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}
		k := factKey(item)
		prev, exists := byKey[k]
		if !exists {
			order = append(order, k)
			byKey[k] = item
			continue
		}
		// Merge tombstone/partial updates with previous fields
		if strings.TrimSpace(item.Topic) == "" {
			item.Topic = prev.Topic
		}
		if strings.TrimSpace(item.Statement) == "" {
			item.Statement = prev.Statement
		}
		if strings.TrimSpace(item.ValidFrom) == "" {
			item.ValidFrom = prev.ValidFrom
		}
		if strings.TrimSpace(item.SourceConv) == "" {
			item.SourceConv = prev.SourceConv
		}
		if !item.Verified && prev.Verified {
			item.Verified = prev.Verified
		}
		byKey[k] = item
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, rawLines, err
	}

	for _, k := range order {
		item := byKey[k]
		if item.ValidTo == nil || strings.TrimSpace(*item.ValidTo) == "" {
			item.ValidTo = nil
			active = append(active, item)
		} else {
			expired = append(expired, item)
		}
	}
	return active, expired, rawLines, nil
}

// writeCanonicalFacts atomically rewrites local/memory/facts.jsonl with deduplicated entries.
func writeCanonicalFacts(vaultDir string, all []Fact) error {
	memDir := filepath.Join(vaultDir, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		return err
	}
	factsPath := filepath.Join(memDir, "facts.jsonl")
	var sb strings.Builder
	for _, f := range all {
		b, err := json.Marshal(f)
		if err != nil {
			continue
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return os.WriteFile(factsPath, []byte(sb.String()), 0o644)
}

// CompactFacts deduplicates local/memory/facts.jsonl by Fact.ID (preserving both active and expired
// records with 1 canonical JSON line per ID) and returns the line count before and after compaction.
func CompactFacts(vaultDir string) (int, int, error) {
	active, expired, rawLines, err := LoadFactsState(vaultDir)
	if err != nil {
		return 0, 0, err
	}
	if rawLines == 0 {
		return 0, 0, nil
	}
	all := make([]Fact, 0, len(active)+len(expired))
	all = append(all, active...)
	all = append(all, expired...)
	if err := writeCanonicalFacts(vaultDir, all); err != nil {
		return rawLines, 0, err
	}
	return rawLines, len(all), nil
}

// AddFact appends or updates a bi-temporal fact in local/memory/facts.jsonl.
func AddFact(vaultDir, id, topic, statement, sourceConv string, verified bool) (Fact, error) {
	topic = strings.TrimSpace(topic)
	statement = strings.TrimSpace(statement)
	if topic == "" || statement == "" {
		return Fact{}, fmt.Errorf("both --topic and --statement are required")
	}

	active, expired, _, err := LoadFactsState(vaultDir)
	if err != nil {
		return Fact{}, err
	}

	id = strings.TrimSpace(id)
	if id == "" {
		existingIDs := make(map[string]bool)
		for _, f := range active {
			existingIDs[f.ID] = true
		}
		for _, f := range expired {
			existingIDs[f.ID] = true
		}
		datePrefix := time.Now().Format("20060102")
		seq := len(active) + len(expired) + 1
		for {
			candidate := fmt.Sprintf("fact-%s-%03d", datePrefix, seq)
			if !existingIDs[candidate] {
				id = candidate
				break
			}
			seq++
		}
	}

	newFact := Fact{
		ID:         id,
		Topic:      topic,
		Statement:  statement,
		SourceConv: strings.TrimSpace(sourceConv),
		Verified:   verified,
		ValidFrom:  time.Now().Format(time.RFC3339),
		ValidTo:    nil,
	}

	memDir := filepath.Join(vaultDir, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		return Fact{}, err
	}
	factsPath := filepath.Join(memDir, "facts.jsonl")
	b, err := json.Marshal(newFact)
	if err != nil {
		return Fact{}, err
	}
	f, err := os.OpenFile(factsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return Fact{}, err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return Fact{}, err
	}
	return newFact, nil
}

// PruneFact soft-expires an active fact by setting valid_to = now (and optional superseded_by)
// and compacts local/memory/facts.jsonl so stale duplicates do not remain active.
func PruneFact(vaultDir, id, supersededBy string) (Fact, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Fact{}, fmt.Errorf("fact id is required")
	}
	active, expired, _, err := LoadFactsState(vaultDir)
	if err != nil {
		return Fact{}, err
	}

	now := time.Now().Format(time.RFC3339)
	var pruned Fact
	found := false
	var remainingActive []Fact

	for _, f := range active {
		if f.ID == id {
			f.ValidTo = &now
			f.SupersededBy = strings.TrimSpace(supersededBy)
			pruned = f
			expired = append(expired, f)
			found = true
		} else {
			remainingActive = append(remainingActive, f)
		}
	}

	if !found {
		for _, f := range expired {
			if f.ID == id {
				return f, nil // already expired (idempotent)
			}
		}
		return Fact{}, fmt.Errorf("fact %q not found in local/memory/facts.jsonl", id)
	}

	all := make([]Fact, 0, len(remainingActive)+len(expired))
	all = append(all, remainingActive...)
	all = append(all, expired...)
	if err := writeCanonicalFacts(vaultDir, all); err != nil {
		return Fact{}, err
	}
	return pruned, nil
}

// LoadTopicIndex discovers all markdown topic pages in local/memory/topics/*.md
// and extracts each page's # Title and 1-line > Summary header.
func LoadTopicIndex(vaultDir string) ([]TopicPage, error) {
	topicsDir := filepath.Join(vaultDir, "memory", "topics")
	entries, err := os.ReadDir(topicsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var pages []TopicPage
	for _, name := range names {
		fullPath := filepath.Join(topicsDir, name)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		slug := strings.TrimSuffix(name, filepath.Ext(name))
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		title := slug
		summary := ""

		for _, rawLine := range lines {
			line := strings.TrimSpace(rawLine)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "# ") && title == slug {
				title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
				continue
			}
			if summary == "" && strings.HasPrefix(line, ">") {
				clean := strings.TrimSpace(strings.TrimLeft(line, ">"))
				for _, prefix := range []string{"**Summary:**", "**Summary**:", "Summary:"} {
					if strings.HasPrefix(strings.ToLower(clean), strings.ToLower(prefix)) {
						clean = strings.TrimSpace(clean[len(prefix):])
						break
					}
				}
				// Skip pure metadata lines like "> **Updated:** ..." if we haven't found summary yet
				if strings.HasPrefix(strings.ToLower(clean), "**updated") || strings.HasPrefix(strings.ToLower(clean), "updated:") {
					continue
				}
				summary = clean
			}
		}
		if summary == "" {
			for _, rawLine := range lines {
				line := strings.TrimSpace(rawLine)
				if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, ">") && !strings.HasPrefix(line, "---") {
					summary = line
					break
				}
			}
		}
		if runes := []rune(summary); len(runes) > 160 {
			summary = string(runes[:160]) + "..."
		}
		updatedAt := ""
		if info, statErr := os.Stat(fullPath); statErr == nil {
			updatedAt = info.ModTime().Format("2006-01-02")
		}

		pages = append(pages, TopicPage{
			Slug:      slug,
			Title:     title,
			Summary:   summary,
			Path:      fullPath,
			LineCount: len(lines),
			UpdatedAt: updatedAt,
		})
	}
	return pages, nil
}

// AuditMemory evaluates MEMORY.md budget compliance, topic wiki links, active/expired fact counts,
// and checks whether any completed/cancelled task slugs are still referenced in MEMORY.md or active facts.
func AuditMemory(vaultDir string, completedTaskSlugs []string) (*MemoryStatusReport, error) {
	memPath := filepath.Join(vaultDir, "MEMORY.md")
	memBytes, _ := os.ReadFile(memPath)
	memContent := strings.TrimRight(string(memBytes), "\n")

	memLines := 0
	if strings.TrimSpace(memContent) != "" {
		memLines = len(strings.Split(memContent, "\n"))
	}
	words := len(strings.Fields(memContent))
	estTokens := (words * 4) / 3
	if len(memBytes)/4 > estTokens {
		estTokens = len(memBytes) / 4
	}

	topics, err := LoadTopicIndex(vaultDir)
	if err != nil {
		return nil, err
	}

	var orphans []string
	for _, tp := range topics {
		if !strings.Contains(memContent, tp.Slug) {
			orphans = append(orphans, tp.Slug)
		}
	}

	activeFacts, expiredFacts, rawFactLines, err := LoadFactsState(vaultDir)
	if err != nil {
		return nil, err
	}

	var staleRefs []string
	seenStale := make(map[string]bool)
	for _, slug := range completedTaskSlugs {
		slug = strings.TrimSpace(slug)
		if slug == "" || strings.HasPrefix(slug, "subagent-") || strings.HasPrefix(slug, "conv-") {
			continue
		}
		if strings.Contains(memContent, slug) && !seenStale["MEMORY.md:"+slug] {
			seenStale["MEMORY.md:"+slug] = true
			staleRefs = append(staleRefs, fmt.Sprintf("MEMORY.md references completed/cancelled task %q", slug))
		}
		for _, f := range activeFacts {
			if strings.Contains(f.Statement, slug) && !seenStale["fact:"+f.ID] {
				seenStale["fact:"+f.ID] = true
				staleRefs = append(staleRefs, fmt.Sprintf("Active fact %s references completed/cancelled task %q", f.ID, slug))
			}
		}
	}

	return &MemoryStatusReport{
		MemoryLines:      memLines,
		MemoryMaxLines:   MaxMemoryIndexLines,
		EstimatedTokens:  estTokens,
		OverBudget:       memLines > MaxMemoryIndexLines,
		Topics:           topics,
		OrphanTopicSlugs: orphans,
		ActiveFactCount:  len(activeFacts),
		ExpiredFactCount: len(expiredFacts),
		RawFactLines:     rawFactLines,
		StaleReferences:  staleRefs,
	}, nil
}
