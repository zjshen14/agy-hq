package outbox

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/zjshen14/agy-hq/internal/approvals"
)

// Supported attention delivery tiers.
const (
	TierInterrupt    = "interrupt"
	TierBriefRollup  = "brief_rollup"
	TierIdeasFeed    = "ideas_feed"
	TierSilentLedger = "silent_ledger"
)

// Supported outbox lifecycle states.
const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusPromoted  = "promoted"
	StatusSnoozed   = "snoozed"
	StatusDismissed = "dismissed"
)

const SemanticDedupWindow = 12 * time.Hour

// OutboxItem represents a proactive insight, rollup item, or actionable idea persisted
// in local/memory/outbox/items.jsonl.
type OutboxItem struct {
	ID                string   `json:"id"`
	CreatedAt         string   `json:"createdAt"`
	SourceSkill       string   `json:"sourceSkill"`
	SourceConvID      string   `json:"sourceConvId,omitempty"`
	Scope             string   `json:"scope,omitempty"`
	Tier              string   `json:"tier"` // interrupt | brief_rollup | ideas_feed | silent_ledger
	Title             string   `json:"title"`
	Summary           string   `json:"summary"`
	ProposalIDs       []string `json:"proposalIds,omitempty"`
	SuggestedSlug     string   `json:"suggestedSlug,omitempty"`
	Critical          bool     `json:"critical,omitempty"`
	Force             bool     `json:"force,omitempty"`
	Status            string   `json:"status"` // pending | delivered | promoted | snoozed | dismissed
	DismissReason     string   `json:"dismissReason,omitempty"`
	SnoozeUntil       string   `json:"snoozeUntil,omitempty"`
	DowngradedBy      string   `json:"downgradedBy,omitempty"`
	CoalescedCount    int      `json:"coalescedCount,omitempty"`
	DeliveryAttempts  int      `json:"deliveryAttempts,omitempty"`
	NextAttemptAt     string   `json:"nextAttemptAt,omitempty"`
	LastDeliveryError string   `json:"lastDeliveryError,omitempty"`
	UpdatedAt         string   `json:"updatedAt"`
}

// QuietHoursConfig configures DND quiet hours for proactive interrupts.
type QuietHoursConfig struct {
	Enabled             bool   `json:"enabled"`
	Start               string `json:"start"`    // HH:MM (24-hour)
	End                 string `json:"end"`      // HH:MM (24-hour)
	Timezone            string `json:"timezone"` // IANA timezone, e.g. America/Los_Angeles
	AllowCriticalBreach bool   `json:"allowCriticalBreach"`
}

// AttentionConfig governs the 4-tier attention budget, quiet hours, dedup cooldown, and batch coalescing.
type AttentionConfig struct {
	Mode              string           `json:"mode"` // focused | balanced | proactive
	QuietHours        QuietHoursConfig `json:"quietHours"`
	CooldownPerTopic  time.Duration    `json:"cooldownPerTopic"`
	CoalesceWindow    time.Duration    `json:"coalesceWindow"`
	CooldownRaw       string           `json:"cooldownRaw,omitempty"`
	CoalesceWindowRaw string           `json:"coalesceWindowRaw,omitempty"`
}

// SkillDismissalStat summarizes user feedback on proactive ideas and outbound proposals emitted by a skill.
type SkillDismissalStat struct {
	Skill          string  `json:"skill"`
	TotalResolved  int     `json:"totalResolved"`
	PromotedCount  int     `json:"promotedCount"`
	DismissedCount int     `json:"dismissedCount"`
	ApprovedCount  int     `json:"approvedCount,omitempty"`
	RejectedCount  int     `json:"rejectedCount,omitempty"`
	DismissRate    float64 `json:"dismissRate"`
	SampleReasons  string  `json:"sampleReasons,omitempty"`
	LastSeenAt     string  `json:"lastSeenAt,omitempty"`
}

// DefaultAttentionConfig returns safe defaults when local/config.yaml has no attention: block.
func DefaultAttentionConfig() AttentionConfig {
	return AttentionConfig{
		Mode: "balanced",
		QuietHours: QuietHoursConfig{
			Enabled:             false,
			Start:               "22:00",
			End:                 "07:30",
			Timezone:            "America/Los_Angeles",
			AllowCriticalBreach: true,
		},
		CooldownPerTopic: 4 * time.Hour,
		CoalesceWindow:   0,
		CooldownRaw:      "4h",
	}
}

func cleanYAMLVal(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, " #"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

// ParseAttentionConfig parses the top-level `attention:` block from YAML content.
func ParseAttentionConfig(content string) AttentionConfig {
	cfg := DefaultAttentionConfig()
	inAttention := false
	inQuietHours := false

	for _, rawLine := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(rawLine) - len(strings.TrimLeft(rawLine, " \t"))
		if indent == 0 {
			inAttention = strings.HasPrefix(trimmed, "attention:")
			inQuietHours = false
			continue
		}
		if !inAttention {
			continue
		}
		if indent == 2 {
			inQuietHours = false
			idx := strings.Index(trimmed, ":")
			if idx < 0 {
				continue
			}
			key := strings.TrimSpace(trimmed[:idx])
			val := cleanYAMLVal(trimmed[idx+1:])
			switch key {
			case "mode":
				m := strings.ToLower(val)
				if m == "focused" || m == "balanced" || m == "proactive" {
					cfg.Mode = m
				}
			case "quiet_hours":
				inQuietHours = true
			case "cooldown_per_topic":
				if d, err := time.ParseDuration(val); err == nil && d >= 0 {
					cfg.CooldownPerTopic = d
					cfg.CooldownRaw = val
				}
			case "coalesce_window":
				if d, err := time.ParseDuration(val); err == nil && d >= 0 {
					cfg.CoalesceWindow = d
					cfg.CoalesceWindowRaw = val
				}
			}
			continue
		}
		if inQuietHours && indent >= 4 {
			idx := strings.Index(trimmed, ":")
			if idx < 0 {
				continue
			}
			key := strings.TrimSpace(trimmed[:idx])
			val := cleanYAMLVal(trimmed[idx+1:])
			switch key {
			case "enabled":
				cfg.QuietHours.Enabled = strings.EqualFold(val, "true") || val == "1" || strings.EqualFold(val, "yes")
			case "start":
				if val != "" {
					cfg.QuietHours.Start = val
				}
			case "end":
				if val != "" {
					cfg.QuietHours.End = val
				}
			case "timezone":
				if val != "" {
					cfg.QuietHours.Timezone = val
				}
			case "allow_critical_breach":
				cfg.QuietHours.AllowCriticalBreach = strings.EqualFold(val, "true") || val == "1" || strings.EqualFold(val, "yes")
			}
		}
	}
	return cfg
}

// LoadAttentionConfig reads local/config.yaml from vaultDir (returning defaults if absent).
func LoadAttentionConfig(vaultDir string) AttentionConfig {
	data, err := os.ReadFile(filepath.Join(vaultDir, "config.yaml"))
	if err != nil {
		return DefaultAttentionConfig()
	}
	return ParseAttentionConfig(string(data))
}

func parseHHMM(s string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// InQuietHours returns true if reference time now falls within the configured DND quiet window.
func InQuietHours(cfg AttentionConfig, now time.Time) bool {
	if !cfg.QuietHours.Enabled {
		return false
	}
	startMin, ok1 := parseHHMM(cfg.QuietHours.Start)
	endMin, ok2 := parseHHMM(cfg.QuietHours.End)
	if !ok1 || !ok2 {
		return false
	}
	loc := now.Location()
	if tz := strings.TrimSpace(cfg.QuietHours.Timezone); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	localNow := now.In(loc)
	curMin := localNow.Hour()*60 + localNow.Minute()
	if startMin == endMin {
		return false
	}
	if startMin < endMin {
		return curMin >= startMin && curMin < endMin
	}
	// Overnight window (e.g. 22:00 -> 07:30)
	return curMin >= startMin || curMin < endMin
}

func itemsFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "outbox", "items.jsonl")
}

func generateID(now time.Time) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("idea-%d-%s", now.Unix(), hex.EncodeToString(b))
}

func normalizeTier(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case TierInterrupt:
		return TierInterrupt
	case TierBriefRollup:
		return TierBriefRollup
	case TierIdeasFeed:
		return TierIdeasFeed
	case TierSilentLedger:
		return TierSilentLedger
	default:
		return TierBriefRollup
	}
}

// LoadAll reads and deduplicates all OutboxItems from local/memory/outbox/items.jsonl.
func LoadAll(vaultDir string) ([]OutboxItem, error) {
	f, err := os.Open(itemsFilePath(vaultDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var order []string
	byID := make(map[string]OutboxItem)
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item OutboxItem
		if err := json.Unmarshal([]byte(line), &item); err != nil || strings.TrimSpace(item.ID) == "" {
			continue
		}
		if _, seen := byID[item.ID]; !seen {
			order = append(order, item.ID)
		}
		byID[item.ID] = item
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	out := make([]OutboxItem, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, nil
}

func saveCanonical(vaultDir string, items []OutboxItem) error {
	p := itemsFilePath(vaultDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	var sb strings.Builder
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			continue
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return os.WriteFile(p, []byte(sb.String()), 0o600)
}

func topicDedupKey(it OutboxItem) string {
	return strings.ToLower(strings.TrimSpace(it.SourceSkill)) + "|" +
		strings.ToLower(strings.TrimSpace(it.Scope)) + "|" +
		strings.ToLower(strings.Join(strings.Fields(it.Title), " "))
}

var (
	reNonTokenChar = regexp.MustCompile(`[^a-z0-9\-]+`)
	titleStopwords = map[string]bool{
		"a": true, "an": true, "and": true, "are": true, "at": true, "by": true,
		"for": true, "from": true, "has": true, "in": true, "is": true, "of": true,
		"on": true, "or": true, "the": true, "to": true, "with": true, "new": true,
	}
)

func extractTitleTokensAndIDs(title string) (tokens map[string]bool, idSig string) {
	tokens = make(map[string]bool)
	var ids []string
	cleaned := reNonTokenChar.ReplaceAllString(strings.ToLower(title), " ")
	for _, raw := range strings.Fields(cleaned) {
		tok := strings.Trim(raw, "-")
		if tok == "" || titleStopwords[tok] {
			continue
		}
		hasDigit := false
		for _, r := range tok {
			if unicode.IsDigit(r) {
				hasDigit = true
				break
			}
		}
		if hasDigit {
			ids = append(ids, tok)
		}
		// Normalize simple gerund/noun variants ("failing" -> "fail", "failure" -> "fail")
		norm := tok
		if !hasDigit {
			switch {
			case strings.HasSuffix(norm, "ing") && len(norm) > 5:
				norm = strings.TrimSuffix(norm, "ing")
			case strings.HasSuffix(norm, "ure") && len(norm) > 5:
				norm = strings.TrimSuffix(norm, "ure")
			case strings.HasSuffix(norm, "s") && len(norm) > 4:
				norm = strings.TrimSuffix(norm, "s")
			}
		}
		tokens[norm] = true
	}
	sort.Strings(ids)
	return tokens, strings.Join(ids, "|")
}

func isNearDuplicateTitle(a, b OutboxItem) bool {
	if !strings.EqualFold(strings.TrimSpace(a.SourceSkill), strings.TrimSpace(b.SourceSkill)) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(a.Scope), strings.TrimSpace(b.Scope)) {
		return false
	}
	tokA, idA := extractTitleTokensAndIDs(a.Title)
	tokB, idB := extractTitleTokensAndIDs(b.Title)
	// Distinct numeric/entity IDs (e.g. PR #42 vs PR #43, noisy-1 vs noisy-2) are never duplicates.
	if idA != idB {
		return false
	}
	if len(tokA) < 2 || len(tokB) < 2 {
		return false
	}
	inter := 0
	union := make(map[string]bool, len(tokA)+len(tokB))
	for k := range tokA {
		union[k] = true
		if tokB[k] {
			inter++
		}
	}
	for k := range tokB {
		union[k] = true
	}
	if len(union) == 0 {
		return false
	}
	return float64(inter)/float64(len(union)) >= 0.75
}

// EvaluateDelivery applies attention mode, quiet-hours hold, exact topic cooldown, and 12h semantic Jaccard dedup
// deterministically in Go before persisting or dispatching an OutboxItem.
func EvaluateDelivery(existing []OutboxItem, cfg AttentionConfig, now time.Time, item OutboxItem) (OutboxItem, bool) {
	if now.IsZero() {
		now = time.Now()
	}
	item.Tier = normalizeTier(item.Tier)
	if item.Status == "" {
		item.Status = StatusPending
	}
	if item.Tier == TierSilentLedger {
		item.Status = StatusDelivered
		return item, false
	}

	// 1. Exact per-topic dedup cooldown + 12h entity-preserving semantic Jaccard dedup (unless --force)
	if !item.Force && (item.Tier == TierInterrupt || item.Tier == TierBriefRollup || item.Tier == TierIdeasFeed) {
		key := topicDedupKey(item)
		semWindow := SemanticDedupWindow
		if cfg.CooldownPerTopic > semWindow {
			semWindow = cfg.CooldownPerTopic
		}
		for i := len(existing) - 1; i >= 0; i-- {
			prev := existing[i]
			if prev.DowngradedBy == "topic_cooldown" || prev.DowngradedBy == "semantic_dedup" {
				continue
			}
			ts, err := time.Parse(time.RFC3339, prev.CreatedAt)
			if err != nil || now.Sub(ts) < 0 {
				continue
			}
			if cfg.CooldownPerTopic > 0 && topicDedupKey(prev) == key && now.Sub(ts) < cfg.CooldownPerTopic {
				item.DowngradedBy = "topic_cooldown"
				item.Tier = TierSilentLedger
				item.Status = StatusDelivered
				return item, true
			}
			if now.Sub(ts) < semWindow && isNearDuplicateTitle(prev, item) {
				item.DowngradedBy = "semantic_dedup"
				item.Tier = TierSilentLedger
				item.Status = StatusDelivered
				return item, true
			}
		}
	}

	// 2. Quiet-hours DND hold: downgrade interrupt -> brief_rollup unless critical breach is permitted
	if item.Tier == TierInterrupt && InQuietHours(cfg, now) {
		if !(item.Critical && cfg.QuietHours.AllowCriticalBreach) {
			item.Tier = TierBriefRollup
			item.DowngradedBy = "quiet_hours"
		}
	}

	// 3. Focused attention mode: downgrade non-critical interrupts to brief_rollup
	if item.Tier == TierInterrupt && strings.EqualFold(cfg.Mode, "focused") && !item.Critical {
		item.Tier = TierBriefRollup
		if item.DowngradedBy == "" {
			item.DowngradedBy = "focused_mode"
		}
	}

	return item, false
}

// Add evaluates attention delivery rules and appends an OutboxItem to local/memory/outbox/items.jsonl.
func Add(vaultDir string, item OutboxItem, now time.Time) (OutboxItem, bool, error) {
	if now.IsZero() {
		now = time.Now()
	}
	if strings.TrimSpace(item.Title) == "" && strings.TrimSpace(item.Summary) == "" {
		return OutboxItem{}, false, fmt.Errorf("outbox item requires --title or --summary")
	}
	if strings.TrimSpace(item.Title) == "" {
		item.Title = item.Summary
		if runes := []rune(item.Title); len(runes) > 72 {
			item.Title = string(runes[:69]) + "..."
		}
	}
	if strings.TrimSpace(item.SourceSkill) == "" {
		item.SourceSkill = "general"
	}

	existing, err := LoadAll(vaultDir)
	if err != nil {
		return OutboxItem{}, false, err
	}
	cfg := LoadAttentionConfig(vaultDir)

	nowStr := now.UTC().Format(time.RFC3339)
	if strings.TrimSpace(item.ID) == "" {
		item.ID = generateID(now)
	}
	item.CreatedAt = nowStr
	item.UpdatedAt = nowStr

	evaluated, cooled := EvaluateDelivery(existing, cfg, now, item)
	if cooled && evaluated.DowngradedBy == "semantic_dedup" {
		for i := len(existing) - 1; i >= 0; i-- {
			if isNearDuplicateTitle(existing[i], item) {
				existing[i].CoalescedCount++
				existing[i].UpdatedAt = nowStr
				break
			}
		}
	}
	existing = append(existing, evaluated)
	if err := saveCanonical(vaultDir, existing); err != nil {
		return OutboxItem{}, false, err
	}
	return evaluated, cooled, nil
}

// List returns OutboxItems filtered by tier and status, automatically waking expired snoozes.
func List(vaultDir, tierFilter, statusFilter string, now time.Time) ([]OutboxItem, error) {
	if now.IsZero() {
		now = time.Now()
	}
	items, err := LoadAll(vaultDir)
	if err != nil {
		return nil, err
	}
	dirty := false
	nowStr := now.UTC().Format(time.RFC3339)
	for i := range items {
		if items[i].Status == StatusSnoozed && items[i].SnoozeUntil != "" {
			if wakeAt, pErr := time.Parse(time.RFC3339, items[i].SnoozeUntil); pErr == nil && !now.Before(wakeAt) {
				items[i].Status = StatusPending
				items[i].SnoozeUntil = ""
				items[i].UpdatedAt = nowStr
				dirty = true
			}
		}
	}
	if dirty {
		_ = saveCanonical(vaultDir, items)
	}

	var out []OutboxItem
	for _, it := range items {
		if tierFilter != "" && !strings.EqualFold(it.Tier, tierFilter) {
			continue
		}
		if statusFilter != "" && !strings.EqualFold(it.Status, statusFilter) {
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out, nil
}

// DrainBriefRollup returns all pending brief_rollup items, marks them "delivered",
// and records each item in local/memory/reported.jsonl so future runs do not repeat them.
func DrainBriefRollup(vaultDir string, now time.Time) ([]OutboxItem, error) {
	if now.IsZero() {
		now = time.Now()
	}
	items, err := LoadAll(vaultDir)
	if err != nil {
		return nil, err
	}
	nowStr := now.UTC().Format(time.RFC3339)
	var drained []OutboxItem
	for i := range items {
		if items[i].Tier == TierBriefRollup && items[i].Status == StatusPending {
			items[i].Status = StatusDelivered
			items[i].UpdatedAt = nowStr
			drained = append(drained, items[i])
		}
	}
	if len(drained) > 0 {
		if err := saveCanonical(vaultDir, items); err != nil {
			return nil, err
		}
		for _, it := range drained {
			_ = RecordReportedFromItem(vaultDir, it, now)
		}
	}
	return drained, nil
}

// PromoteIdea marks an OutboxItem as promoted so the caller can launch or register a task.
func PromoteIdea(vaultDir, id string, now time.Time) (OutboxItem, error) {
	return mutateItem(vaultDir, id, now, func(it *OutboxItem, nowStr string) {
		it.Status = StatusPromoted
		it.SnoozeUntil = ""
		it.UpdatedAt = nowStr
	})
}

// SnoozeIdea hides an OutboxItem until now + dur.
func SnoozeIdea(vaultDir, id string, dur time.Duration, now time.Time) (OutboxItem, error) {
	if dur <= 0 {
		dur = 24 * time.Hour
	}
	return mutateItem(vaultDir, id, now, func(it *OutboxItem, nowStr string) {
		it.Status = StatusSnoozed
		it.SnoozeUntil = now.Add(dur).UTC().Format(time.RFC3339)
		it.UpdatedAt = nowStr
	})
}

// DismissIdea marks an OutboxItem as dismissed and records the user's dismissal reason for RSI mining.
func DismissIdea(vaultDir, id, reason string, now time.Time) (OutboxItem, error) {
	return mutateItem(vaultDir, id, now, func(it *OutboxItem, nowStr string) {
		it.Status = StatusDismissed
		if strings.TrimSpace(reason) != "" {
			it.DismissReason = strings.TrimSpace(reason)
		}
		it.SnoozeUntil = ""
		it.UpdatedAt = nowStr
	})
}

func mutateItem(vaultDir, id string, now time.Time, fn func(it *OutboxItem, nowStr string)) (OutboxItem, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return OutboxItem{}, fmt.Errorf("outbox item id is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	items, err := LoadAll(vaultDir)
	if err != nil {
		return OutboxItem{}, err
	}
	nowStr := now.UTC().Format(time.RFC3339)
	for i := range items {
		if items[i].ID == id {
			fn(&items[i], nowStr)
			if err := saveCanonical(vaultDir, items); err != nil {
				return OutboxItem{}, err
			}
			return items[i], nil
		}
	}
	return OutboxItem{}, fmt.Errorf("outbox item %q not found", id)
}

// ComputeSkillDismissalStats aggregates promoted/approved vs. dismissed/rejected resolution rates per skill
// across both ideas_feed items and outbound approval interventions so `hq rsi mine` detects noisy skills.
func ComputeSkillDismissalStats(vaultDir string) ([]SkillDismissalStat, error) {
	items, err := LoadAll(vaultDir)
	if err != nil {
		return nil, err
	}
	type acc struct {
		stat    SkillDismissalStat
		reasons []string
	}
	bySkill := make(map[string]*acc)
	var skills []string

	ensureSkill := func(rawSkill string) *acc {
		sk := strings.TrimSpace(rawSkill)
		if sk == "" {
			sk = "general"
		}
		entry, ok := bySkill[sk]
		if !ok {
			entry = &acc{stat: SkillDismissalStat{Skill: sk}}
			bySkill[sk] = entry
			skills = append(skills, sk)
		}
		return entry
	}

	for _, it := range items {
		if it.Status != StatusPromoted && it.Status != StatusDismissed {
			continue
		}
		entry := ensureSkill(it.SourceSkill)
		entry.stat.TotalResolved++
		if it.Status == StatusPromoted {
			entry.stat.PromotedCount++
		} else if it.Status == StatusDismissed {
			entry.stat.DismissedCount++
			if r := strings.TrimSpace(it.DismissReason); r != "" && len(entry.reasons) < 3 {
				entry.reasons = append(entry.reasons, r)
			}
		}
		if it.UpdatedAt > entry.stat.LastSeenAt {
			entry.stat.LastSeenAt = it.UpdatedAt
		}
	}

	// Join rejected/approved proposals carrying explicit OriginSkill attribution from interventions.jsonl
	if interventions, iErr := approvals.LoadInterventions(vaultDir); iErr == nil {
		for _, rec := range interventions {
			if rec.Decision != "approved" && rec.Decision != "rejected" {
				continue
			}
			origin := strings.TrimSpace(rec.Proposal.OriginSkill)
			if origin == "" && rec.Proposal.Metadata != nil {
				origin = strings.TrimSpace(rec.Proposal.Metadata["originSkill"])
			}
			if origin == "" {
				continue
			}
			entry := ensureSkill(origin)
			entry.stat.TotalResolved++
			if rec.Decision == "approved" {
				entry.stat.ApprovedCount++
			} else {
				entry.stat.RejectedCount++
				entry.stat.DismissedCount++
				if r := strings.TrimSpace(rec.Output); r != "" && len(entry.reasons) < 3 {
					entry.reasons = append(entry.reasons, r)
				}
			}
			if rec.Timestamp > entry.stat.LastSeenAt {
				entry.stat.LastSeenAt = rec.Timestamp
			}
		}
	}

	sort.Strings(skills)
	out := make([]SkillDismissalStat, 0, len(skills))
	for _, sk := range skills {
		entry := bySkill[sk]
		if entry.stat.TotalResolved > 0 {
			entry.stat.DismissRate = float64(entry.stat.DismissedCount) / float64(entry.stat.TotalResolved)
		}
		if len(entry.reasons) > 0 {
			entry.stat.SampleReasons = strings.Join(entry.reasons, "; ")
		}
		out = append(out, entry.stat)
	}
	return out, nil
}
