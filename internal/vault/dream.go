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

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/store"
)

// DreamCursor tracks the last committed sleep-consolidate watermark in local/memory/dream_cursor.json.
// It is advanced ONLY on successful `hq memory commit`, never during `--precheck`.
type DreamCursor struct {
	LastCommittedAt string `json:"lastCommittedAt"`
	LastRunConvID   string `json:"lastRunConvId,omitempty"`
}

// DreamPlanOptions configures BuildDreamPlan.
type DreamPlanOptions struct {
	Since time.Time
	Now   time.Time
	Limit int
}

// DreamConversationItem summarizes one salient non-ephemeral, untainted conversation since the last cursor.
type DreamConversationItem struct {
	ConversationID string   `json:"conversationId"`
	Turns          int      `json:"turns"`
	TotalSteps     int      `json:"totalSteps"`
	ToolErrors     int      `json:"toolErrors"`
	PreToolDenials int      `json:"preToolDenials"`
	FilesModified  []string `json:"filesModified,omitempty"`
	TranscriptPath string   `json:"transcriptPath,omitempty"`
	LastTimestamp  string   `json:"lastTimestamp"`
	PromptSample   string   `json:"promptSample,omitempty"`
	Salience       int      `json:"salience"`
}

// DreamPlan is the deterministic, 0-token consolidation blueprint emitted by `hq memory dream-plan`.
type DreamPlan struct {
	GeneratedAt          string                  `json:"generatedAt"`
	Since                string                  `json:"since"`
	CursorLastCommitted  string                  `json:"cursorLastCommitted,omitempty"`
	HasDelta             bool                    `json:"hasDelta"`
	SalientConversations []DreamConversationItem `json:"salientConversations,omitempty"`
	SkippedTaintedTurns  int                     `json:"skippedTaintedTurns"`
	SkippedEphemeralConv int                     `json:"skippedEphemeralConvs"`
	PendingArchiveDays   int                     `json:"pendingArchiveDays"`
	OverdueTTLFactCount  int                     `json:"overdueTTLFactCount"`
	MissingTTLEventFacts []string                `json:"missingTTLEventFacts,omitempty"`
	MemoryOverBudget     bool                    `json:"memoryOverBudget"`
	MemoryLines          int                     `json:"memoryLines"`
	MemoryBytes          int                     `json:"memoryBytes"`
	StaleReferences      []string                `json:"staleReferences,omitempty"`
	WikiAlerts           []string                `json:"wikiAlerts,omitempty"`
	NegativeConstraints  []string                `json:"negativeConstraints,omitempty"`
}

func dreamCursorPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "dream_cursor.json")
}

// LoadDreamCursor reads local/memory/dream_cursor.json lock-free.
func LoadDreamCursor(vaultDir string) DreamCursor {
	var cur DreamCursor
	p := dreamCursorPath(vaultDir)
	if !isRegularNonSymlinkFile(p) {
		return cur
	}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &cur)
	}
	return cur
}

// AdvanceDreamCursor atomically persists the committed sleep-consolidate watermark under store.RankMemory.
// Must ONLY be called inside `hq memory commit` after Stage 4 succeeds.
func AdvanceDreamCursor(vaultDir, convID string, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cur := DreamCursor{
		LastCommittedAt: now.UTC().Format(time.RFC3339),
		LastRunConvID:   strings.TrimSpace(convID),
	}
	b, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return withMemoryLock(vaultDir, func() error {
		return store.WriteAtomic(dreamCursorPath(vaultDir), b, 0o600)
	})
}

// PrecheckLine returns empty string "" when HasDelta == false (required by gatekeeper exit_zero_nonempty)
// or a concise single-line summary (< 200 chars) when consolidation work exists.
func (p *DreamPlan) PrecheckLine() string {
	if p == nil || !p.HasDelta {
		return ""
	}
	line := fmt.Sprintf("dream-delta: convs=%d overdueFacts=%d staleRefs=%d wikiAlerts=%d archiveDays=%d overBudget=%v",
		len(p.SalientConversations),
		p.OverdueTTLFactCount,
		len(p.StaleReferences),
		len(p.WikiAlerts),
		p.PendingArchiveDays,
		p.MemoryOverBudget,
	)
	if len(line) > 190 {
		line = line[:187] + "..."
	}
	return line
}

type interventionLite struct {
	Timestamp string `json:"timestamp"`
	Decision  string `json:"decision"`
	Proposal  struct {
		ActionType string `json:"actionType"`
		Summary    string `json:"summary"`
	} `json:"proposal"`
}

// BuildDreamPlan builds a deterministic, lock-free consolidation plan for `sleep-consolidate`.
func BuildDreamPlan(vaultDir string, opts DreamPlanOptions) (*DreamPlan, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 15
	}

	cur := LoadDreamCursor(vaultDir)
	since := opts.Since
	if since.IsZero() {
		if ts, ok := parseAnyTimestamp(cur.LastCommittedAt); ok {
			since = ts
		} else {
			since = now.Add(-24 * time.Hour)
		}
	}

	// Build ephemeral conversation set and completed task slugs from tasks.json.
	ephemeralConvs := make(map[string]bool)
	var completedSlugs []string
	if tasks, err := kernel.ListTasks(vaultDir); err == nil {
		for _, t := range tasks {
			if t.Ephemeral && strings.TrimSpace(t.ConversationID) != "" {
				ephemeralConvs[strings.TrimSpace(t.ConversationID)] = true
			}
			st := strings.ToLower(strings.TrimSpace(t.Status))
			if (st == "done" || st == "archived") && strings.TrimSpace(t.TaskSlug) != "" {
				completedSlugs = append(completedSlugs, t.TaskSlug)
			}
		}
	}

	type convAgg struct {
		item    DreamConversationItem
		fileSet map[string]bool
	}
	byConv := make(map[string]*convAgg)
	skippedTainted := 0
	skippedEphemeralSet := make(map[string]bool)

	_ = WalkJournals(vaultDir, since, false, func(_ string, rec JournalRecord) bool {
		if rec.TaintedSession {
			skippedTainted++
			return true
		}
		cid := strings.TrimSpace(rec.ConversationID)
		if cid == "" {
			return true
		}
		if ephemeralConvs[cid] {
			skippedEphemeralSet[cid] = true
			return true
		}

		agg, ok := byConv[cid]
		if !ok {
			agg = &convAgg{
				item: DreamConversationItem{
					ConversationID: cid,
				},
				fileSet: make(map[string]bool),
			}
			byConv[cid] = agg
		}
		agg.item.Turns++
		if rec.StepCount > agg.item.TotalSteps {
			agg.item.TotalSteps = rec.StepCount
		}
		agg.item.ToolErrors += rec.TurnErrorCount
		if rec.TurnErrorCount == 0 && rec.ToolErrorCount > agg.item.ToolErrors {
			agg.item.ToolErrors = rec.ToolErrorCount
		}
		agg.item.PreToolDenials += rec.PreToolDenials
		if rec.TranscriptPath != "" {
			agg.item.TranscriptPath = rec.TranscriptPath
		}
		if rec.Timestamp > agg.item.LastTimestamp {
			agg.item.LastTimestamp = rec.Timestamp
		}
		if agg.item.PromptSample == "" && strings.TrimSpace(rec.UserPrompt) != "" {
			agg.item.PromptSample = truncateRunes(strings.Join(strings.Fields(rec.UserPrompt), " "), 140)
		}
		for _, f := range rec.FilesModified {
			f = strings.TrimSpace(f)
			if f != "" && !agg.fileSet[f] {
				agg.fileSet[f] = true
				agg.item.FilesModified = append(agg.item.FilesModified, f)
			}
		}
		return true
	})

	var salient []DreamConversationItem
	for _, agg := range byConv {
		sort.Strings(agg.item.FilesModified)
		sal := agg.item.Turns + 2*len(agg.item.FilesModified) + 2*agg.item.ToolErrors + 3*agg.item.PreToolDenials
		lowerPrompt := strings.ToLower(agg.item.PromptSample)
		for _, cue := range []string{"prefer", "always", "never", "remember", "rule", "convention", "don't", "do not"} {
			if strings.Contains(lowerPrompt, cue) {
				sal += 3
				break
			}
		}
		agg.item.Salience = sal
		salient = append(salient, agg.item)
	}
	sort.Slice(salient, func(i, j int) bool {
		if salient[i].Salience != salient[j].Salience {
			return salient[i].Salience > salient[j].Salience
		}
		return salient[i].LastTimestamp > salient[j].LastTimestamp
	})
	if len(salient) > limit {
		salient = salient[:limit]
	}

	audit, err := AuditMemory(vaultDir, completedSlugs)
	if err != nil {
		return nil, err
	}

	var wikiAlerts []string
	for _, s := range audit.InvalidTopicHeaders {
		wikiAlerts = append(wikiAlerts, "invalid_header:"+s)
	}
	for _, s := range audit.BrokenTopicLinks {
		wikiAlerts = append(wikiAlerts, "broken_link:"+s)
	}
	for _, s := range audit.DuplicatePlaybookOwners {
		wikiAlerts = append(wikiAlerts, "duplicate_playbook_owner:"+s)
	}
	for _, s := range audit.NearDuplicateTopics {
		wikiAlerts = append(wikiAlerts, "near_duplicate_topic:"+s)
	}

	pendingArchiveDays := countHotJournalDaysOlderThan(vaultDir, now.AddDate(0, 0, -DefaultHotArchiveDays))
	negConstraints := loadRecentRejectedConstraints(vaultDir, now.AddDate(0, 0, -30), 10)

	hasDelta := len(salient) > 0 ||
		audit.OverdueTTLFactCount > 0 ||
		audit.OverBudget ||
		len(audit.StaleReferences) > 0 ||
		len(wikiAlerts) > 0 ||
		pendingArchiveDays > 0

	return &DreamPlan{
		GeneratedAt:          now.UTC().Format(time.RFC3339),
		Since:                since.UTC().Format(time.RFC3339),
		CursorLastCommitted:  cur.LastCommittedAt,
		HasDelta:             hasDelta,
		SalientConversations: salient,
		SkippedTaintedTurns:  skippedTainted,
		SkippedEphemeralConv: len(skippedEphemeralSet),
		PendingArchiveDays:   pendingArchiveDays,
		OverdueTTLFactCount:  audit.OverdueTTLFactCount,
		MissingTTLEventFacts: audit.MissingTTLEventFacts,
		MemoryOverBudget:     audit.OverBudget,
		MemoryLines:          audit.MemoryLines,
		MemoryBytes:          audit.MemoryBytes,
		StaleReferences:      audit.StaleReferences,
		WikiAlerts:           wikiAlerts,
		NegativeConstraints:  negConstraints,
	}, nil
}

func countHotJournalDaysOlderThan(vaultDir string, cutoff time.Time) int {
	journalRoot := filepath.Join(vaultDir, "memory", "journal")
	entries, err := os.ReadDir(journalRoot)
	if err != nil {
		return 0
	}
	cutoffDay := cutoff.UTC().Format("2006-01-02")
	count := 0
	for _, e := range entries {
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		name := e.Name()
		if len(name) != 10 || name == "archive" {
			continue
		}
		if _, err := time.Parse("2006-01-02", name); err != nil {
			continue
		}
		if name < cutoffDay {
			count++
		}
	}
	return count
}

func scanInterventionsSince(vaultDir string, since time.Time, fn func(interventionLite)) {
	p := filepath.Join(vaultDir, "memory", "interventions.jsonl")
	if !isRegularNonSymlinkFile(p) {
		return
	}
	f, err := os.Open(p)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec interventionLite
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if ts, ok := parseAnyTimestamp(rec.Timestamp); ok && ts.Before(since) {
			continue
		}
		fn(rec)
	}
}

func loadRecentRejectedConstraints(vaultDir string, since time.Time, maxItems int) []string {
	var items []string
	seen := make(map[string]bool)
	scanInterventionsSince(vaultDir, since, func(rec interventionLite) {
		if !strings.EqualFold(strings.TrimSpace(rec.Decision), "rejected") {
			return
		}
		summary := strings.TrimSpace(rec.Proposal.Summary)
		if summary == "" {
			return
		}
		entry := fmt.Sprintf("[%s] %s", strings.TrimSpace(rec.Proposal.ActionType), truncateRunes(summary, 140))
		key := strings.ToLower(entry)
		if !seen[key] {
			seen[key] = true
			items = append(items, entry)
		}
	})
	if len(items) > maxItems {
		items = items[len(items)-maxItems:]
	}
	return items
}
