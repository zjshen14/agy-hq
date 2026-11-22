package outbox

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	"github.com/zjshen14/agy-hq/internal/store"
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
	StatusPending      = "pending"
	StatusDispatching  = "dispatching"
	StatusDelivered    = "delivered"
	StatusPromoted     = "promoted"
	StatusSnoozed      = "snoozed"
	StatusDismissed    = "dismissed"
	StatusActedHandled = "acted_handled"
)

const (
	SemanticDedupWindow = 12 * time.Hour
	DispatchingClaimTTL = 2 * time.Minute
)

// CoalescedBullet captures a secondary alert merged into a pending burst OutboxItem.
type CoalescedBullet struct {
	Title     string `json:"title"`
	Summary   string `json:"summary,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// OutboxItem represents a proactive insight, rollup item, or actionable idea persisted
// in local/memory/outbox/items.jsonl.
type OutboxItem struct {
	ID                     string            `json:"id"`
	CreatedAt              string            `json:"createdAt"`
	SourceSkill            string            `json:"sourceSkill"`
	SourceConvID           string            `json:"sourceConvId,omitempty"`
	Scope                  string            `json:"scope,omitempty"`
	Tier                   string            `json:"tier"` // interrupt | brief_rollup | ideas_feed | silent_ledger
	Title                  string            `json:"title"`
	Summary                string            `json:"summary"`
	IncidentKey            string            `json:"incidentKey,omitempty"`
	AvailableForDispatchAt string            `json:"availableForDispatchAt,omitempty"`
	CorrelatedFromID       string            `json:"correlatedFromId,omitempty"`
	CorrelatedCount        int               `json:"correlatedCount,omitempty"`
	CoalescedItems         []CoalescedBullet `json:"coalescedItems,omitempty"`
	Decisions              []DecisionOption  `json:"decisions,omitempty"`
	ProposalIDs            []string          `json:"proposalIds,omitempty"`
	SuggestedSlug          string            `json:"suggestedSlug,omitempty"`
	Critical               bool              `json:"critical,omitempty"`
	Force                  bool              `json:"force,omitempty"`
	Tainted                bool              `json:"tainted,omitempty"`
	Connector              string            `json:"connector,omitempty"`
	Account                string            `json:"account,omitempty"`
	ThreadKey              string            `json:"threadKey,omitempty"`
	BriefLeaseID           string            `json:"briefLeaseId,omitempty"`
	BriefLeaseExpiresAt    string            `json:"briefLeaseExpiresAt,omitempty"`
	BriefLeaseConvID       string            `json:"briefLeaseConvId,omitempty"`
	DispatchClaimUntil     string            `json:"dispatchClaimUntil,omitempty"`
	Status                 string            `json:"status"` // pending | dispatching | delivered | promoted | snoozed | dismissed | acted_handled
	DismissReason          string            `json:"dismissReason,omitempty"`
	ActionResult           string            `json:"actionResult,omitempty"`
	ActedAt                string            `json:"actedAt,omitempty"`
	SnoozeUntil            string            `json:"snoozeUntil,omitempty"`
	ParkHorizon            string            `json:"parkHorizon,omitempty"`
	DowngradedBy           string            `json:"downgradedBy,omitempty"`
	CoalescedCount         int               `json:"coalescedCount,omitempty"`
	DeliveryAttempts       int               `json:"deliveryAttempts,omitempty"`
	NextAttemptAt          string            `json:"nextAttemptAt,omitempty"`
	LastDeliveryError      string            `json:"lastDeliveryError,omitempty"`
	UpdatedAt              string            `json:"updatedAt"`
}

// QuietHoursConfig configures DND quiet hours for proactive interrupts.
type QuietHoursConfig struct {
	Enabled             bool   `json:"enabled"`
	Start               string `json:"start"`    // HH:MM (24-hour)
	End                 string `json:"end"`      // HH:MM (24-hour)
	Timezone            string `json:"timezone"` // IANA timezone, e.g. America/Los_Angeles
	AllowCriticalBreach bool   `json:"allowCriticalBreach"`
}

// ScopeAttentionRule downgrades non-critical interrupts for matching scopes during a schedule window.
type ScopeAttentionRule struct {
	Scopes                 []string `json:"scopes"`
	Schedule               string   `json:"schedule"`                        // e.g. "weekdays 09:00-17:30 America/Los_Angeles"
	DowngradeNonCriticalTo string   `json:"downgradeNonCriticalInterruptTo"` // brief_rollup | ideas_feed | silent_ledger
}

// AttentionConfig governs the 4-tier attention budget, quiet hours, dedup cooldown, burst coalescing, and scope rules.
type AttentionConfig struct {
	Mode                      string               `json:"mode"` // focused | balanced | proactive
	QuietHours                QuietHoursConfig     `json:"quietHours"`
	CooldownPerTopic          time.Duration        `json:"cooldownPerTopic"`
	CoalesceWindow            time.Duration        `json:"coalesceWindow"`
	IncidentCorrelationWindow time.Duration        `json:"incidentCorrelationWindow"`
	CooldownRaw               string               `json:"cooldownRaw,omitempty"`
	CoalesceWindowRaw         string               `json:"coalesceWindowRaw,omitempty"`
	CorrelationWindowRaw      string               `json:"correlationWindowRaw,omitempty"`
	ScopeRules                []ScopeAttentionRule `json:"scopeRules,omitempty"`
}

// SkillDismissalStat summarizes user feedback on proactive ideas, interrupts, and outbound proposals emitted by a skill.
type SkillDismissalStat struct {
	Skill                  string  `json:"skill"`
	TotalResolved          int     `json:"totalResolved"`
	PromotedCount          int     `json:"promotedCount"`
	DismissedCount         int     `json:"dismissedCount"`
	ApprovedCount          int     `json:"approvedCount,omitempty"`
	RejectedCount          int     `json:"rejectedCount,omitempty"`
	DismissRate            float64 `json:"dismissRate"`
	InterruptActedCount    int     `json:"interruptActedCount,omitempty"`
	InterruptParkedCount   int     `json:"interruptParkedCount,omitempty"`
	InterruptNoisyCount    int     `json:"interruptNoisyCount,omitempty"`
	InterruptResolvedCount int     `json:"interruptResolvedCount,omitempty"`
	InterruptNoisyRate     float64 `json:"interruptNoisyRate,omitempty"`
	ProposalExpiredCount   int     `json:"proposalExpiredCount,omitempty"`
	ProposalTotalCount     int     `json:"proposalTotalCount,omitempty"`
	ProposalExpiredRate    float64 `json:"proposalExpiredRate,omitempty"`
	SampleReasons          string  `json:"sampleReasons,omitempty"`
	LastSeenAt             string  `json:"lastSeenAt,omitempty"`
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
		CooldownPerTopic:          4 * time.Hour,
		CoalesceWindow:            0,
		IncidentCorrelationWindow: 30 * time.Minute,
		CooldownRaw:               "4h",
		CorrelationWindowRaw:      "30m",
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
	inScopeRules := false
	curRuleIdx := -1

	parseYAMLList := func(raw string) []string {
		s := strings.TrimSpace(raw)
		s = strings.TrimPrefix(s, "[")
		s = strings.TrimSuffix(s, "]")
		var out []string
		for _, part := range strings.Split(s, ",") {
			v := cleanYAMLVal(part)
			if v != "" {
				out = append(out, v)
			}
		}
		return out
	}

	for _, rawLine := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(rawLine) - len(strings.TrimLeft(rawLine, " \t"))
		if indent == 0 {
			inAttention = strings.HasPrefix(trimmed, "attention:")
			inQuietHours = false
			inScopeRules = false
			curRuleIdx = -1
			continue
		}
		if !inAttention {
			continue
		}
		if indent == 2 {
			inQuietHours = false
			inScopeRules = false
			curRuleIdx = -1
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
				// Presence of an explicit `quiet_hours:` block enables quiet hours by default unless `enabled: false` is set.
				cfg.QuietHours.Enabled = true
			case "scope_rules":
				inScopeRules = true
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
			case "incident_correlation_window":
				if d, err := time.ParseDuration(val); err == nil && d >= 0 {
					cfg.IncidentCorrelationWindow = d
					cfg.CorrelationWindowRaw = val
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
			case "timezone", "tz":
				if val != "" {
					cfg.QuietHours.Timezone = val
				}
			case "allow_critical_breach":
				cfg.QuietHours.AllowCriticalBreach = strings.EqualFold(val, "true") || val == "1" || strings.EqualFold(val, "yes")
			}
			continue
		}
		if inScopeRules && indent >= 4 {
			line := trimmed
			if strings.HasPrefix(line, "- ") {
				cfg.ScopeRules = append(cfg.ScopeRules, ScopeAttentionRule{DowngradeNonCriticalTo: TierBriefRollup})
				curRuleIdx = len(cfg.ScopeRules) - 1
				line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
			}
			if curRuleIdx < 0 {
				continue
			}
			idx := strings.Index(line, ":")
			if idx < 0 {
				continue
			}
			key := strings.TrimSpace(line[:idx])
			val := cleanYAMLVal(line[idx+1:])
			switch key {
			case "scopes":
				cfg.ScopeRules[curRuleIdx].Scopes = parseYAMLList(line[idx+1:])
			case "schedule":
				cfg.ScopeRules[curRuleIdx].Schedule = val
			case "downgrade_non_critical_interrupt_to", "downgrade_to":
				if val != "" {
					cfg.ScopeRules[curRuleIdx].DowngradeNonCriticalTo = normalizeTier(val)
				}
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

// ParseHHMM parses a 24-hour "HH:MM" clock string into hour and minute components.
func ParseHHMM(s string) (hour, minute int, ok bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

func parseHHMM(s string) (int, bool) {
	h, m, ok := ParseHHMM(s)
	if !ok {
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

func outboxLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "outbox", ".outbox.lock")
}

// WithOutboxLock executes fn while holding RankOutbox on <vault>/memory/outbox/.outbox.lock.
func WithOutboxLock(vaultDir string, fn func() error) error {
	return store.With(context.Background(), store.RankOutbox, outboxLockPath(vaultDir), func(_ context.Context) error {
		return fn()
	})
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

// LoadAll reads and deduplicates all OutboxItems from local/memory/outbox/items.jsonl under RankOutbox.
func LoadAll(vaultDir string) ([]OutboxItem, error) {
	var out []OutboxItem
	err := WithOutboxLock(vaultDir, func() error {
		var lErr error
		out, lErr = LoadAllUnlocked(vaultDir)
		return lErr
	})
	return out, err
}

// LoadAllUnlocked reads items.jsonl when RankOutbox is already held by the caller.
func LoadAllUnlocked(vaultDir string) ([]OutboxItem, error) {
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

// SaveCanonicalUnlocked atomically writes items.jsonl via store.WriteAtomic when RankOutbox is held.
func SaveCanonicalUnlocked(vaultDir string, items []OutboxItem) error {
	p := itemsFilePath(vaultDir)
	var sb strings.Builder
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			continue
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return store.WriteAtomic(p, []byte(sb.String()), 0o600)
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

// MatchesScopeSchedule evaluates a schedule expression like "weekdays 09:00-17:30 America/Los_Angeles"
// or "daily 08:00-18:00 UTC" or "weekends 10:00-20:00 America/New_York".
func MatchesScopeSchedule(schedule string, now time.Time) bool {
	fields := strings.Fields(strings.TrimSpace(schedule))
	if len(fields) < 2 {
		return false
	}
	daySpec := strings.ToLower(fields[0])
	timeRange := fields[1]
	loc := now.Location()
	if len(fields) >= 3 {
		if l, err := time.LoadLocation(fields[2]); err == nil {
			loc = l
		}
	}
	localNow := now.In(loc)
	wd := localNow.Weekday()
	dayMatch := false
	switch daySpec {
	case "daily", "everyday", "*":
		dayMatch = true
	case "weekdays", "mon-fri":
		dayMatch = wd >= time.Monday && wd <= time.Friday
	case "weekends", "sat-sun":
		dayMatch = wd == time.Saturday || wd == time.Sunday
	default:
		dayMap := map[string]time.Weekday{
			"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
			"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
		}
		for _, tok := range strings.Split(daySpec, ",") {
			if target, ok := dayMap[strings.TrimSpace(tok)]; ok && target == wd {
				dayMatch = true
				break
			}
		}
	}
	if !dayMatch {
		return false
	}
	parts := strings.SplitN(timeRange, "-", 2)
	if len(parts) != 2 {
		return false
	}
	startMin, ok1 := parseHHMM(parts[0])
	endMin, ok2 := parseHHMM(parts[1])
	if !ok1 || !ok2 || startMin == endMin {
		return false
	}
	curMin := localNow.Hour()*60 + localNow.Minute()
	if startMin < endMin {
		return curMin >= startMin && curMin < endMin
	}
	return curMin >= startMin || curMin < endMin
}

func matchScopeRule(cfg AttentionConfig, scope string, now time.Time) (string, bool) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" || len(cfg.ScopeRules) == 0 {
		return "", false
	}
	for _, rule := range cfg.ScopeRules {
		matchedScope := false
		for _, s := range rule.Scopes {
			pat := strings.ToLower(strings.TrimSpace(s))
			if pat == "*" || pat == scope || (strings.HasSuffix(pat, "*") && strings.HasPrefix(scope, strings.TrimSuffix(pat, "*"))) {
				matchedScope = true
				break
			}
		}
		if !matchedScope {
			continue
		}
		if MatchesScopeSchedule(rule.Schedule, now) {
			target := normalizeTier(rule.DowngradeNonCriticalTo)
			if target == TierInterrupt {
				target = TierBriefRollup
			}
			return target, true
		}
	}
	return "", false
}

// EvaluateDelivery applies attention mode, quiet-hours hold, scope rules, exact topic cooldown,
// 12h semantic Jaccard dedup, burst coalescing hold, and incident correlation deterministically in Go.
func EvaluateDelivery(existing []OutboxItem, cfg AttentionConfig, now time.Time, item OutboxItem) (OutboxItem, bool) {
	if now.IsZero() {
		now = time.Now()
	}
	item.Tier = normalizeTier(item.Tier)
	if item.Status == "" {
		item.Status = StatusPending
	}
	if item.Tainted {
		// Tainted workers can never force cooldown bypasses or critical quiet-hour breaches.
		item.Force = false
		item.Critical = false
	}
	if item.Tier == TierSilentLedger {
		item.Status = StatusDelivered
		return item, false
	}

	// 1. Exact per-topic dedup cooldown, tainted (connector, account, threadKey) dedup, + 12h entity-preserving semantic Jaccard dedup (unless --force)
	if !item.Force && (item.Tier == TierInterrupt || item.Tier == TierBriefRollup || item.Tier == TierIdeasFeed) {
		key := topicDedupKey(item)
		semWindow := SemanticDedupWindow
		if cfg.CooldownPerTopic > semWindow {
			semWindow = cfg.CooldownPerTopic
		}
		for i := len(existing) - 1; i >= 0; i-- {
			prev := existing[i]
			if prev.DowngradedBy == "topic_cooldown" || prev.DowngradedBy == "semantic_dedup" || prev.DowngradedBy == "thread_cooldown" || prev.DowngradedBy == "burst_coalesced" {
				continue
			}
			ts, err := time.Parse(time.RFC3339, prev.CreatedAt)
			if err != nil || now.Sub(ts) < 0 {
				continue
			}
			if item.Tainted && strings.TrimSpace(item.ThreadKey) != "" && cfg.CooldownPerTopic > 0 && now.Sub(ts) < cfg.CooldownPerTopic {
				if strings.EqualFold(strings.TrimSpace(prev.Connector), strings.TrimSpace(item.Connector)) &&
					strings.EqualFold(strings.TrimSpace(prev.Account), strings.TrimSpace(item.Account)) &&
					strings.TrimSpace(prev.ThreadKey) == strings.TrimSpace(item.ThreadKey) {
					item.DowngradedBy = "thread_cooldown"
					item.Tier = TierSilentLedger
					item.Status = StatusDelivered
					return item, true
				}
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

	// 4. Scope-specific quiet/focus rules (e.g. quiet personal alerts during weekday coding blocks)
	if item.Tier == TierInterrupt && !item.Critical {
		if targetTier, matched := matchScopeRule(cfg, item.Scope, now); matched {
			item.Tier = targetTier
			if item.DowngradedBy == "" {
				item.DowngradedBy = "scope_rule"
			}
		}
	}

	// 5. Burst coalescing hold & 30m incident correlation for interrupts sharing IncidentKey
	if item.Tier == TierInterrupt {
		incKey := strings.TrimSpace(item.IncidentKey)
		if incKey != "" {
			// 5a. If an undispatched pending interrupt with the same IncidentKey is still in its CoalesceWindow, merge into it.
			if cfg.CoalesceWindow > 0 && !item.Critical && !item.Force {
				for i := len(existing) - 1; i >= 0; i-- {
					prev := existing[i]
					if prev.Tier != TierInterrupt || prev.Status != StatusPending {
						continue
					}
					if !strings.EqualFold(strings.TrimSpace(prev.IncidentKey), incKey) {
						continue
					}
					if ts, err := time.Parse(time.RFC3339, prev.CreatedAt); err == nil && now.Sub(ts) >= 0 && now.Sub(ts) <= cfg.CoalesceWindow {
						item.DowngradedBy = "burst_coalesced"
						item.CorrelatedFromID = prev.ID
						item.Tier = TierSilentLedger
						item.Status = StatusDelivered
						return item, true
					}
				}
			}
			// 5b. If a prior interrupt with the same IncidentKey was already delivered within IncidentCorrelationWindow, link it.
			corrWin := cfg.IncidentCorrelationWindow
			if corrWin <= 0 {
				corrWin = 30 * time.Minute
			}
			corrCount := 0
			var lastID string
			for i := len(existing) - 1; i >= 0; i-- {
				prev := existing[i]
				if prev.Tier != TierInterrupt || prev.DowngradedBy == "burst_coalesced" {
					continue
				}
				if !strings.EqualFold(strings.TrimSpace(prev.IncidentKey), incKey) {
					continue
				}
				if ts, err := time.Parse(time.RFC3339, prev.CreatedAt); err == nil && now.Sub(ts) >= 0 && now.Sub(ts) <= corrWin {
					corrCount++
					if lastID == "" {
						lastID = prev.ID
					}
				}
			}
			if corrCount > 0 {
				item.CorrelatedFromID = lastID
				item.CorrelatedCount = corrCount + 1
			}
		}
		// 5c. Apply burst-coalescing hold for non-critical, non-force interrupts when CoalesceWindow > 0.
		if cfg.CoalesceWindow > 0 && !item.Critical && !item.Force && strings.TrimSpace(item.AvailableForDispatchAt) == "" {
			item.AvailableForDispatchAt = now.Add(cfg.CoalesceWindow).UTC().Format(time.RFC3339)
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
	if err := ValidateDecisionOptions(&item); err != nil {
		return OutboxItem{}, false, err
	}

	cfg := LoadAttentionConfig(vaultDir)
	nowStr := now.UTC().Format(time.RFC3339)
	if strings.TrimSpace(item.ID) == "" {
		item.ID = generateID(now)
	}
	item.CreatedAt = nowStr
	item.UpdatedAt = nowStr

	var evaluated OutboxItem
	var cooled bool
	err := WithOutboxLock(vaultDir, func() error {
		existing, lErr := LoadAllUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		evaluated, cooled = EvaluateDelivery(existing, cfg, now, item)
		if cooled && evaluated.DowngradedBy == "burst_coalesced" && evaluated.CorrelatedFromID != "" {
			for i := len(existing) - 1; i >= 0; i-- {
				if existing[i].ID == evaluated.CorrelatedFromID {
					existing[i].CoalescedCount++
					existing[i].CoalescedItems = append(existing[i].CoalescedItems, CoalescedBullet{
						Title:     SanitizeAntiCue(item.Title, 160),
						Summary:   SanitizeAntiCue(item.Summary, 400),
						CreatedAt: nowStr,
					})
					for _, pid := range item.ProposalIDs {
						existing[i].ProposalIDs = appendUniqueStr(existing[i].ProposalIDs, pid)
					}
					existing[i].UpdatedAt = nowStr
					evaluated = existing[i]
					return SaveCanonicalUnlocked(vaultDir, existing)
				}
			}
		}
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
		return SaveCanonicalUnlocked(vaultDir, existing)
	})
	if err != nil {
		return OutboxItem{}, false, err
	}
	return evaluated, cooled, nil
}

func appendUniqueStr(slice []string, val string) []string {
	val = strings.TrimSpace(val)
	if val == "" {
		return slice
	}
	for _, s := range slice {
		if s == val {
			return slice
		}
	}
	return append(slice, val)
}

// List returns OutboxItems filtered by tier and status, automatically waking expired snoozes.
func List(vaultDir, tierFilter, statusFilter string, now time.Time) ([]OutboxItem, error) {
	if now.IsZero() {
		now = time.Now()
	}
	var items []OutboxItem
	err := WithOutboxLock(vaultDir, func() error {
		var lErr error
		items, lErr = LoadAllUnlocked(vaultDir)
		if lErr != nil {
			return lErr
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
			return SaveCanonicalUnlocked(vaultDir, items)
		}
		return nil
	})
	if err != nil {
		return nil, err
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
	var drained []OutboxItem
	err := WithOutboxLock(vaultDir, func() error {
		items, lErr := LoadAllUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		nowStr := now.UTC().Format(time.RFC3339)
		for i := range items {
			if items[i].Tier == TierBriefRollup && items[i].Status == StatusPending && !hasActiveBriefLease(items[i], now) {
				items[i].Status = StatusDelivered
				items[i].UpdatedAt = nowStr
				drained = append(drained, items[i])
			}
		}
		if len(drained) > 0 {
			return SaveCanonicalUnlocked(vaultDir, items)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, it := range drained {
		_ = RecordReportedFromItem(vaultDir, it, now)
	}
	return drained, nil
}

func hashBriefLeaseToken(rawTok string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rawTok)))
	return hex.EncodeToString(sum[:])
}

// PeekBriefItems returns all pending brief_rollup items (plus quiet-hours-held items) WITHOUT mutating
// items.jsonl or reported.jsonl (100% zero-write computed view; expired leases are treated as available).
func PeekBriefItems(vaultDir string, now time.Time) ([]OutboxItem, error) {
	if now.IsZero() {
		now = time.Now()
	}
	var out []OutboxItem
	err := WithOutboxLock(vaultDir, func() error {
		items, lErr := LoadAllUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		for _, it := range items {
			if it.Tier == TierBriefRollup && it.Status == StatusPending && !hasActiveBriefLease(it, now) {
				out = append(out, it)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out, nil
}

// LeaseBriefItems acquires a non-destructive caller-bound lease over all unleased pending brief_rollup items,
// storing only SHA-256(leaseToken) in items.jsonl so readable outbox files never expose raw lease tokens.
func LeaseBriefItems(vaultDir, callerConvID string, ttl time.Duration, now time.Time) (string, string, []OutboxItem, error) {
	if now.IsZero() {
		now = time.Now()
	}
	if ttl <= 0 {
		ttl = 20 * time.Minute
	}
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", "", nil, err
	}
	rawTok := "blease_" + hex.EncodeToString(rnd[:])
	tokHash := hashBriefLeaseToken(rawTok)
	expStr := now.Add(ttl).UTC().Format(time.RFC3339)
	callerConvID = strings.TrimSpace(callerConvID)

	var leased []OutboxItem
	err := WithOutboxLock(vaultDir, func() error {
		items, lErr := LoadAllUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		dirty := false
		nowStr := now.UTC().Format(time.RFC3339)
		for i := range items {
			if items[i].Tier == TierBriefRollup && items[i].Status == StatusPending && !hasActiveBriefLease(items[i], now) {
				items[i].BriefLeaseID = tokHash
				items[i].BriefLeaseConvID = callerConvID
				items[i].BriefLeaseExpiresAt = expStr
				items[i].UpdatedAt = nowStr
				leased = append(leased, items[i])
				dirty = true
			}
		}
		if dirty {
			return SaveCanonicalUnlocked(vaultDir, items)
		}
		return nil
	})
	if err != nil {
		return "", "", nil, err
	}
	return rawTok, expStr, leased, nil
}

// CommitBriefLease atomically marks all items matching rawLeaseToken (and callerConvID when non-empty)
// as delivered and appends fenced entries to reported.jsonl.
func CommitBriefLease(vaultDir, rawLeaseToken, callerConvID string, now time.Time) ([]OutboxItem, error) {
	rawLeaseToken = strings.TrimSpace(rawLeaseToken)
	if !strings.HasPrefix(rawLeaseToken, "blease_") || len(rawLeaseToken) < 16 {
		return nil, fmt.Errorf("invalid brief lease token")
	}
	if now.IsZero() {
		now = time.Now()
	}
	wantHash := hashBriefLeaseToken(rawLeaseToken)
	callerConvID = strings.TrimSpace(callerConvID)

	var committed []OutboxItem
	var convMismatch bool
	err := WithOutboxLock(vaultDir, func() error {
		items, lErr := LoadAllUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		nowStr := now.UTC().Format(time.RFC3339)
		for i := range items {
			if items[i].Status != StatusPending || len(items[i].BriefLeaseID) != len(wantHash) {
				continue
			}
			if subtle.ConstantTimeCompare([]byte(items[i].BriefLeaseID), []byte(wantHash)) != 1 {
				continue
			}
			if items[i].BriefLeaseConvID != "" && items[i].BriefLeaseConvID != callerConvID {
				convMismatch = true
				continue
			}
			items[i].Status = StatusDelivered
			items[i].BriefLeaseID = ""
			items[i].BriefLeaseExpiresAt = ""
			items[i].UpdatedAt = nowStr
			committed = append(committed, items[i])
		}
		if convMismatch && len(committed) == 0 {
			return fmt.Errorf("brief lease belongs to a different conversation")
		}
		if len(committed) > 0 {
			return SaveCanonicalUnlocked(vaultDir, items)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = RecordReportedBatch(vaultDir, committed, now)
	return committed, nil
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
	var updated OutboxItem
	err := WithOutboxLock(vaultDir, func() error {
		items, lErr := LoadAllUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		nowStr := now.UTC().Format(time.RFC3339)
		for i := range items {
			if items[i].ID == id {
				fn(&items[i], nowStr)
				if sErr := SaveCanonicalUnlocked(vaultDir, items); sErr != nil {
					return sErr
				}
				updated = items[i]
				return nil
			}
		}
		return fmt.Errorf("outbox item %q not found", id)
	})
	return updated, err
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
		if it.Tier == TierInterrupt {
			switch it.Status {
			case StatusActedHandled, StatusPromoted:
				entry := ensureSkill(it.SourceSkill)
				entry.stat.InterruptActedCount++
				entry.stat.InterruptResolvedCount++
				if it.UpdatedAt > entry.stat.LastSeenAt {
					entry.stat.LastSeenAt = it.UpdatedAt
				}
			case StatusSnoozed:
				if strings.TrimSpace(it.ParkHorizon) != "" || strings.TrimSpace(it.SnoozeUntil) != "" {
					entry := ensureSkill(it.SourceSkill)
					entry.stat.InterruptParkedCount++
					entry.stat.InterruptResolvedCount++
					if it.UpdatedAt > entry.stat.LastSeenAt {
						entry.stat.LastSeenAt = it.UpdatedAt
					}
				}
			case StatusDismissed:
				entry := ensureSkill(it.SourceSkill)
				entry.stat.InterruptNoisyCount++
				entry.stat.InterruptResolvedCount++
				if r := strings.TrimSpace(it.DismissReason); r != "" && len(entry.reasons) < 3 {
					entry.reasons = append(entry.reasons, r)
				}
				if it.UpdatedAt > entry.stat.LastSeenAt {
					entry.stat.LastSeenAt = it.UpdatedAt
				}
			}
		}
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

	// Join approved/rejected/expired proposals carrying explicit OriginSkill attribution from interventions.jsonl
	if interventions, iErr := approvals.LoadInterventions(vaultDir); iErr == nil {
		for _, rec := range interventions {
			if rec.Decision != "approved" && rec.Decision != "rejected" && rec.Decision != "expired" {
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
			entry.stat.ProposalTotalCount++
			switch rec.Decision {
			case "approved":
				entry.stat.TotalResolved++
				entry.stat.ApprovedCount++
			case "rejected":
				entry.stat.TotalResolved++
				entry.stat.RejectedCount++
				entry.stat.DismissedCount++
				if r := strings.TrimSpace(rec.Output); r != "" && len(entry.reasons) < 3 {
					entry.reasons = append(entry.reasons, r)
				}
			case "expired":
				entry.stat.ProposalExpiredCount++
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
		if entry.stat.InterruptResolvedCount > 0 {
			entry.stat.InterruptNoisyRate = float64(entry.stat.InterruptNoisyCount) / float64(entry.stat.InterruptResolvedCount)
		}
		if entry.stat.ProposalTotalCount > 0 {
			entry.stat.ProposalExpiredRate = float64(entry.stat.ProposalExpiredCount) / float64(entry.stat.ProposalTotalCount)
		}
		if len(entry.reasons) > 0 {
			entry.stat.SampleReasons = strings.Join(entry.reasons, "; ")
		}
		out = append(out, entry.stat)
	}
	return out, nil
}
