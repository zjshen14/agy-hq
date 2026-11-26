package webui

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/vault"
)

// MemoryFactRow is a read-safe projection of an active bi-temporal fact for the Console UI.
type MemoryFactRow struct {
	ID           string   `json:"id"`
	Topic        string   `json:"topic"`
	Statement    string   `json:"statement"`
	Scopes       []string `json:"scopes,omitempty"`
	Verified     bool     `json:"verified,omitempty"`
	Pinned       bool     `json:"pinned,omitempty"`
	ValidFrom    string   `json:"validFrom,omitempty"`
	ExpiresAt    string   `json:"expiresAt,omitempty"`
	ExpiringSoon bool     `json:"expiringSoon,omitempty"`
	SourceConv   string   `json:"sourceConv,omitempty"`
}

// MemoryTopicRow is a read-safe projection of a Knowledge Wiki page (no host filesystem paths).
type MemoryTopicRow struct {
	Slug           string   `json:"slug"`
	Title          string   `json:"title"`
	Summary        string   `json:"summary,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
	PlaybookOwner  string   `json:"playbookOwner,omitempty"`
	ActivePlaybook string   `json:"activePlaybook,omitempty"`
	LineCount      int      `json:"lineCount"`
	ByteSize       int      `json:"byteSize"`
	UpdatedAt      string   `json:"updatedAt,omitempty"`
	BrokenLinks    []string `json:"brokenLinks,omitempty"`
}

// MemoryEntityRow is a read-safe projection of an on-demand Entity Dossier (no host filesystem paths).
type MemoryEntityRow struct {
	Slug      string   `json:"slug"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Emails    []string `json:"emails,omitempty"`
	Handles   []string `json:"handles,omitempty"`
	Spaces    []string `json:"spaces,omitempty"`
	Context   string   `json:"context,omitempty"`
	Notes     []string `json:"notes,omitempty"`
	UpdatedAt string   `json:"updatedAt,omitempty"`
}

// MemoryDashboardSummary aggregates 4-layer Vault memory health, Active Priorities, verified facts,
// topic wiki / active playbooks, and entity dossiers for the Console Memory tab.
// Strict privacy boundary: raw private/ files and private/profile.md keys/values are NEVER included.
type MemoryDashboardSummary struct {
	MemoryLines         int               `json:"memoryLines"`
	MemoryMaxLines      int               `json:"memoryMaxLines"`
	MemoryBytes         int               `json:"memoryBytes"`
	MemoryMaxBytes      int               `json:"memoryMaxBytes"`
	BudgetFillPct       float64           `json:"budgetFillPct"`
	EstimatedTokens     int               `json:"estimatedTokens"`
	OverBudget          bool              `json:"overBudget"`
	ActiveFactCount     int               `json:"activeFactCount"`
	ExpiredFactCount    int               `json:"expiredFactCount"`
	OverdueTTLFactCount int               `json:"overdueTTLFactCount"`
	ExpiringSoonCount   int               `json:"expiringSoonCount"`
	TopicCount          int               `json:"topicCount"`
	PlaybookCount       int               `json:"playbookCount"`
	TopicsTotalBytes    int               `json:"topicsTotalBytes"`
	EntityCount         int               `json:"entityCount"`
	ProfileFieldCount   int               `json:"profileFieldCount"`
	ProfileSectionCount int               `json:"profileSectionCount"`
	LastDreamAt         string            `json:"lastDreamAt,omitempty"`
	HealthWarnings      []string          `json:"healthWarnings,omitempty"`
	ActivePriorities    []string          `json:"activePriorities,omitempty"`
	Facts               []MemoryFactRow   `json:"facts,omitempty"`
	Topics              []MemoryTopicRow  `json:"topics,omitempty"`
	Entities            []MemoryEntityRow `json:"entities,omitempty"`
}

// BuildMemoryDashboard builds a privacy-safe, 0-token snapshot of Vault memory state.
func BuildMemoryDashboard(vaultDir string, now time.Time) MemoryDashboardSummary {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	out := MemoryDashboardSummary{
		MemoryMaxLines: vault.MaxMemoryIndexLines,
		MemoryMaxBytes: vault.MaxMemoryIndexBytes,
	}

	if rep, err := vault.AuditMemory(vaultDir, nil); err == nil && rep != nil {
		out.MemoryLines = rep.MemoryLines
		if rep.MemoryMaxLines > 0 {
			out.MemoryMaxLines = rep.MemoryMaxLines
		}
		out.MemoryBytes = rep.MemoryBytes
		if rep.MemoryMaxBytes > 0 {
			out.MemoryMaxBytes = rep.MemoryMaxBytes
		}
		out.EstimatedTokens = rep.EstimatedTokens
		out.OverBudget = rep.OverBudget
		out.ActiveFactCount = rep.ActiveFactCount
		out.ExpiredFactCount = rep.ExpiredFactCount
		out.OverdueTTLFactCount = rep.OverdueTTLFactCount
		out.TopicsTotalBytes = rep.TopicsTotalBytes

		linePct := 0.0
		if out.MemoryMaxLines > 0 {
			linePct = (float64(out.MemoryLines) / float64(out.MemoryMaxLines)) * 100.0
		}
		bytePct := 0.0
		if out.MemoryMaxBytes > 0 {
			bytePct = (float64(out.MemoryBytes) / float64(out.MemoryMaxBytes)) * 100.0
		}
		out.BudgetFillPct = linePct
		if bytePct > out.BudgetFillPct {
			out.BudgetFillPct = bytePct
		}
		if out.BudgetFillPct > 100 {
			out.BudgetFillPct = 100
		}

		// Health warnings summary
		if rep.OverBudget {
			out.HealthWarnings = append(out.HealthWarnings,
				fmt.Sprintf("MEMORY.md over budget (%d/%d lines, %d/%d B) — distill details into topic pages",
					out.MemoryLines, out.MemoryMaxLines, out.MemoryBytes, out.MemoryMaxBytes))
		}
		if rep.OverdueTTLFactCount > 0 {
			out.HealthWarnings = append(out.HealthWarnings,
				fmt.Sprintf("%d overdue TTL fact(s) pending compaction", rep.OverdueTTLFactCount))
		}
		if len(rep.BrokenTopicLinks) > 0 {
			out.HealthWarnings = append(out.HealthWarnings,
				fmt.Sprintf("%d broken topic link(s): %s", len(rep.BrokenTopicLinks), strings.Join(rep.BrokenTopicLinks, ", ")))
		}
		if len(rep.InvalidTopicHeaders) > 0 {
			out.HealthWarnings = append(out.HealthWarnings,
				fmt.Sprintf("%d topic page(s) missing title/summary header: %s", len(rep.InvalidTopicHeaders), strings.Join(rep.InvalidTopicHeaders, ", ")))
		}
		if len(rep.DuplicatePlaybookOwners) > 0 {
			out.HealthWarnings = append(out.HealthWarnings,
				fmt.Sprintf("Duplicate Playbook-Owner claims: %s", strings.Join(rep.DuplicatePlaybookOwners, "; ")))
		}
		if len(rep.SkillPlaybookCollisions) > 0 {
			out.HealthWarnings = append(out.HealthWarnings,
				fmt.Sprintf("Skill Playbook-Owner collisions: %s", strings.Join(rep.SkillPlaybookCollisions, "; ")))
		}
		if len(rep.NearDuplicateTopics) > 0 {
			out.HealthWarnings = append(out.HealthWarnings,
				fmt.Sprintf("Near-duplicate topics detected: %s", strings.Join(rep.NearDuplicateTopics, "; ")))
		}
		if len(rep.StaleReferences) > 0 {
			out.HealthWarnings = append(out.HealthWarnings,
				fmt.Sprintf("Stale MEMORY.md references: %s", strings.Join(rep.StaleReferences, ", ")))
		}
		if len(rep.MissingTTLEventFacts) > 0 {
			out.HealthWarnings = append(out.HealthWarnings,
				fmt.Sprintf("%d date-bound fact(s) missing expires_at TTL", len(rep.MissingTTLEventFacts)))
		}

		// Project topics (omit host file Path)
		topics := make([]MemoryTopicRow, 0, len(rep.Topics))
		playbooks := 0
		for _, tp := range rep.Topics {
			pb := strings.TrimSpace(tp.ActivePlaybook)
			if pb != "" {
				playbooks++
			}
			topics = append(topics, MemoryTopicRow{
				Slug:           tp.Slug,
				Title:          tp.Title,
				Summary:        tp.Summary,
				Scopes:         append([]string(nil), tp.Scopes...),
				PlaybookOwner:  tp.PlaybookOwner,
				ActivePlaybook: pb,
				LineCount:      tp.LineCount,
				ByteSize:       tp.ByteSize,
				UpdatedAt:      tp.UpdatedAt,
				BrokenLinks:    append([]string(nil), tp.BrokenLinks...),
			})
		}
		sort.Slice(topics, func(i, j int) bool {
			hasI := topics[i].ActivePlaybook != ""
			hasJ := topics[j].ActivePlaybook != ""
			if hasI != hasJ {
				return hasI
			}
			return topics[i].Slug < topics[j].Slug
		})
		out.Topics = topics
		out.TopicCount = len(topics)
		out.PlaybookCount = playbooks
	}

	// Active priorities from MEMORY.md
	out.ActivePriorities = parseMemoryIndexLines(filepath.Join(vaultDir, "MEMORY.md"))

	// Active facts (sorted: Pinned -> ExpiringSoon -> newest ValidFrom -> Topic/ID)
	if activeFacts, expiredFacts, _, err := vault.LoadFactsStateAt(vaultDir, now); err == nil {
		out.ActiveFactCount = len(activeFacts)
		out.ExpiredFactCount = len(expiredFacts)
		rows := make([]MemoryFactRow, 0, len(activeFacts))
		expSoonCount := 0
		for _, f := range activeFacts {
			expSoon := isFactExpiringSoon(f.ExpiresAt, now)
			if expSoon {
				expSoonCount++
			}
			rows = append(rows, MemoryFactRow{
				ID:           f.ID,
				Topic:        f.Topic,
				Statement:    f.Statement,
				Scopes:       vault.EffectiveFactScopes(f),
				Verified:     f.Verified,
				Pinned:       f.Pinned,
				ValidFrom:    f.ValidFrom,
				ExpiresAt:    f.ExpiresAt,
				ExpiringSoon: expSoon,
				SourceConv:   f.SourceConv,
			})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Pinned != rows[j].Pinned {
				return rows[i].Pinned
			}
			if rows[i].ExpiringSoon != rows[j].ExpiringSoon {
				return rows[i].ExpiringSoon
			}
			if rows[i].ValidFrom != rows[j].ValidFrom {
				return rows[i].ValidFrom > rows[j].ValidFrom
			}
			if rows[i].Topic != rows[j].Topic {
				return rows[i].Topic < rows[j].Topic
			}
			return rows[i].ID < rows[j].ID
		})
		out.Facts = rows
		out.ExpiringSoonCount = expSoonCount
	}

	// Entity dossiers (omit host file Path)
	if entities, err := vault.ListEntities(vaultDir); err == nil {
		erows := make([]MemoryEntityRow, 0, len(entities))
		for _, e := range entities {
			erows = append(erows, MemoryEntityRow{
				Slug:      e.Slug,
				Name:      e.Name,
				Kind:      e.Kind,
				Emails:    append([]string(nil), e.Emails...),
				Handles:   append([]string(nil), e.Handles...),
				Spaces:    append([]string(nil), e.Spaces...),
				Context:   e.Context,
				Notes:     append([]string(nil), e.Notes...),
				UpdatedAt: e.UpdatedAt,
			})
		}
		sort.Slice(erows, func(i, j int) bool {
			return strings.ToLower(erows[i].Name) < strings.ToLower(erows[j].Name)
		})
		out.Entities = erows
		out.EntityCount = len(erows)
	}

	// Nightly sleep consolidation cursor
	if cur := vault.LoadDreamCursor(vaultDir); strings.TrimSpace(cur.LastCommittedAt) != "" {
		out.LastDreamAt = strings.TrimSpace(cur.LastCommittedAt)
	}

	// Private profile field count ONLY — never serialize section names, keys, or values!
	if prof, err := vault.LoadProfile(vaultDir); err == nil && prof != nil {
		secCount := 0
		fieldCount := 0
		for _, kv := range prof.Sections {
			if len(kv) > 0 {
				secCount++
				fieldCount += len(kv)
			}
		}
		out.ProfileSectionCount = secCount
		out.ProfileFieldCount = fieldCount
	}

	return out
}

func isFactExpiringSoon(expiresAt string, now time.Time) bool {
	s := strings.TrimSpace(expiresAt)
	if s == "" {
		return false
	}
	var exp time.Time
	var err error
	if exp, err = time.Parse(time.RFC3339, s); err != nil {
		if d, dErr := time.Parse("2006-01-02", s); dErr == nil {
			exp = time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, time.UTC)
		} else {
			return false
		}
	}
	return exp.After(now) && exp.Sub(now) <= vault.ImminentExpiryWindow
}

func parseMemoryIndexLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	raw := strings.ReplaceAll(string(data), "\r\n", "\n")
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Skip top-level H1 title and HTML comment scaffolding
		if strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "<!--") {
			continue
		}
		out = append(out, trimmed)
		if len(out) >= vault.MaxMemoryIndexLines {
			break
		}
	}
	return out
}

// computeMemoryStateStamp returns a fast XOR fingerprint over Vault memory files so edits
// to MEMORY.md, facts.jsonl, topics/, entities/, dream_cursor.json, or profile.md immediately
// invalidate the 15s loadStateCached read-through cache.
func computeMemoryStateStamp(vaultDir string) int64 {
	var stamp int64
	mixStat := func(p string, shift uint) {
		if st, err := os.Stat(p); err == nil {
			stamp ^= st.ModTime().UnixNano() ^ (st.Size() << shift)
		}
	}
	mixDir := func(dir string) {
		mixStat(dir, 5)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for i, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if info, iErr := e.Info(); iErr == nil {
				stamp ^= info.ModTime().UnixNano() ^ (info.Size() << uint((i%7)+1))
			}
		}
	}

	mixStat(filepath.Join(vaultDir, "MEMORY.md"), 9)
	mixStat(filepath.Join(vaultDir, "memory", "facts.jsonl"), 11)
	mixStat(filepath.Join(vaultDir, "memory", "dream_cursor.json"), 3)
	mixStat(filepath.Join(vaultDir, "private", "profile.md"), 4)
	mixDir(filepath.Join(vaultDir, "memory", "topics"))
	mixDir(filepath.Join(vaultDir, "memory", "entities"))
	return stamp
}

// registerMemoryRoutes mounts CSRF-protected interactive Memory maintenance endpoints for Live Mode.
func registerMemoryRoutes(mux *http.ServeMux, vaultDir, csrfToken string) {
	mux.HandleFunc("/api/memory/prune-fact", func(w http.ResponseWriter, r *http.Request) {
		p, ok := decodeCSRFPostAction(w, r, csrfToken)
		if !ok {
			return
		}
		if p.ID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"ok":      false,
				"message": "fact id is required",
			})
			return
		}
		pruned, err := vault.PruneFact(vaultDir, p.ID, "console-prune")
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"ok":      false,
				"message": fmt.Sprintf("Error pruning fact %s: %v", p.ID, err),
			})
			return
		}
		invalidateStateCache(vaultDir)
		_ = SyncMarkdownConsole(vaultDir)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":      true,
			"id":      pruned.ID,
			"message": fmt.Sprintf("Pruned fact %s", pruned.ID),
		})
	})

	mux.HandleFunc("/api/memory/compact-facts", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := decodeCSRFPostAction(w, r, csrfToken); !ok {
			return
		}
		before, after, err := vault.CompactFacts(vaultDir)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"ok":      false,
				"message": fmt.Sprintf("Error compacting facts: %v", err),
			})
			return
		}
		invalidateStateCache(vaultDir)
		_ = SyncMarkdownConsole(vaultDir)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":      true,
			"before":  before,
			"after":   after,
			"message": fmt.Sprintf("Compacted facts.jsonl (%d -> %d lines)", before, after),
		})
	})
}
