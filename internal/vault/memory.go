package vault

import (
	"bufio"
	"context"
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

	"github.com/zjshen14/agy-hq/internal/store"
)

const (
	// RFC-0024 Dual Line + Byte Budgets
	MaxMemoryIndexLines     = 40
	MaxMemoryIndexBytes     = 2200
	MaxMemoryLineBytes      = 180
	MaxUserAdvisoryBytes    = 2000
	MaxFactStatementRunes   = 240
	MaxPlaybookLines        = 25
	MaxPlaybookBytes        = 1200
	MaxSessionFullBytes     = 12000
	MaxSessionLeanBytes     = 4800
	MaxSessionDelegateBytes = 2048
	MaxSessionTaintBytes    = 800
	ImminentExpiryWindow    = 72 * time.Hour
)

// TopicPage represents a single Knowledge Wiki page discovered in local/memory/topics/<slug>.md.
type TopicPage struct {
	Slug           string   `json:"slug"`
	Title          string   `json:"title"`
	Summary        string   `json:"summary"`
	Path           string   `json:"path"`
	LineCount      int      `json:"lineCount"`
	ByteSize       int      `json:"byteSize"`
	UpdatedAt      string   `json:"updatedAt,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
	PlaybookOwner  string   `json:"playbookOwner,omitempty"`
	ActivePlaybook string   `json:"activePlaybook,omitempty"`
	HasTitle       bool     `json:"hasTitle"`
	HasSummary     bool     `json:"hasSummary"`
	BrokenLinks    []string `json:"brokenLinks,omitempty"`
}

// AddFactOptions configures AddFactWithOptions.
type AddFactOptions struct {
	ID         string
	Topic      string
	Statement  string
	SourceConv string
	Verified   bool
	Scopes     []string
	ExpiresAt  string
	Pinned     bool
	Now        time.Time
}

// SessionScopeContext captures the resolved role, repo/domain scopes, and Turn-1 prompt
// used for deterministic SessionStart selection.
type SessionScopeContext struct {
	Role        string   // "front_desk" | "task" | "subagent" | "auto"
	RepoSlug    string   // e.g. "agy-hq", "blogs"
	TaskScope   string   // e.g. "investing", "swe", "family"
	ActiveSkill string   // e.g. "thesis-check", "gchat"
	Scopes      []string // normalized canonical scope tags e.g. "repo:agy-hq", "domain:investing"
	PromptText  string   // bounded Turn-1 prompt / task title for lexical matching
	LeanAuto    bool
}

// MemoryStatusReport summarizes the health, dual-cap budget compliance, topic wiki integrity, and fact counts of the Vault.
type MemoryStatusReport struct {
	MemoryLines             int         `json:"memoryLines"`
	MemoryMaxLines          int         `json:"memoryMaxLines"`
	MemoryBytes             int         `json:"memoryBytes"`
	MemoryMaxBytes          int         `json:"memoryMaxBytes"`
	MemoryLongLines         int         `json:"memoryLongLines"`
	UserBytes               int         `json:"userBytes"`
	UserMaxBytes            int         `json:"userMaxBytes"`
	EstimatedTokens         int         `json:"estimatedTokens"`
	OverBudget              bool        `json:"overBudget"`
	Topics                  []TopicPage `json:"topics"`
	TopicsTotalBytes        int         `json:"topicsTotalBytes"`
	EntityCount             int         `json:"entityCount"`
	OrphanTopicSlugs        []string    `json:"orphanTopicSlugs,omitempty"`
	InvalidTopicHeaders     []string    `json:"invalidTopicHeaders,omitempty"`
	BrokenTopicLinks        []string    `json:"brokenTopicLinks,omitempty"`
	DuplicatePlaybookOwners []string    `json:"duplicatePlaybookOwners,omitempty"`
	NearDuplicateTopics     []string    `json:"nearDuplicateTopics,omitempty"`
	ActiveFactCount         int         `json:"activeFactCount"`
	ExpiredFactCount        int         `json:"expiredFactCount"`
	OverdueTTLFactCount     int         `json:"overdueTTLFactCount"`
	MissingTTLEventFacts    []string    `json:"missingTTLEventFacts,omitempty"`
	RawFactLines            int         `json:"rawFactLines"`
	StaleReferences         []string    `json:"staleReferences,omitempty"`
}

func memoryLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", ".memory.lock")
}

// withMemoryLock acquires store.RankMemory (78) strictly around leaf Vault memory file writes.
// Never call kernel.* (Rank 50/75), taint.* (Rank 70), extension.* (Rank 10), or SnapshotVault inside fn.
func withMemoryLock(vaultDir string, fn func() error) error {
	memDir := filepath.Join(vaultDir, "memory")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		return err
	}
	return store.With(context.Background(), store.RankMemory, memoryLockPath(vaultDir), func(context.Context) error {
		return fn()
	})
}

func factKey(f Fact) string {
	if id := strings.TrimSpace(f.ID); id != "" {
		return id
	}
	return "anon:" + strings.TrimSpace(f.Topic) + ":" + strings.TrimSpace(f.Statement)
}

func normalizeScopesList(raw []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, item := range raw {
		for _, part := range strings.Split(item, ",") {
			s := strings.ToLower(strings.TrimSpace(strings.Trim(part, "`")))
			if s == "" {
				continue
			}
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

// InferDefaultScopes derives canonical scopes in-memory for legacy facts that predate RFC-0024.
func InferDefaultScopes(topic string) []string {
	t := strings.ToLower(strings.TrimSpace(topic))
	switch t {
	case "", "general", "global", "ux", "workflow", "prefs", "preferences":
		return []string{"global"}
	case "architecture", "platform", "agy-hq", "headquarter":
		return []string{"repo:agy-hq", "domain:swe"}
	case "gchat", "gmail", "comms":
		return []string{"skill:" + t, "domain:comms"}
	default:
		slug := SanitizeEntitySlug(t)
		if slug == "" {
			return []string{"global"}
		}
		return []string{"domain:" + slug}
	}
}

// EffectiveFactScopes returns f.Scopes normalized, or inferred defaults if empty.
func EffectiveFactScopes(f Fact) []string {
	if len(f.Scopes) > 0 {
		return normalizeScopesList(f.Scopes)
	}
	return InferDefaultScopes(f.Topic)
}

// ParseFlexibleExpiry parses RFC3339, YYYY-MM-DD (end-of-day UTC), or relative duration (+7d, +48h).
func ParseFlexibleExpiry(raw string, now time.Time) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts.UTC().Format(time.RFC3339), nil
	}
	if ts, err := time.Parse("2006-01-02", s); err == nil {
		eod := time.Date(ts.Year(), ts.Month(), ts.Day(), 23, 59, 59, 0, time.UTC)
		return eod.Format(time.RFC3339), nil
	}
	if strings.HasPrefix(s, "+") {
		durStr := strings.TrimPrefix(s, "+")
		if strings.HasSuffix(durStr, "d") {
			daysStr := strings.TrimSuffix(durStr, "d")
			if days, err := strconv.Atoi(daysStr); err == nil && days > 0 {
				return now.UTC().Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339), nil
			}
		}
		if d, err := time.ParseDuration(durStr); err == nil && d > 0 {
			return now.UTC().Add(d).Format(time.RFC3339), nil
		}
	}
	return "", fmt.Errorf("invalid --expires %q (expected RFC3339, YYYY-MM-DD, +Nd, or +Nh)", raw)
}

var reCalendarDateHint = regexp.MustCompile(`(?i)\b(?:20\d{2}-\d{2}-\d{2}|(?:jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)\.?\s+\d{1,2})\b`)

// StatementHasCalendarDate returns true when a fact statement contains an explicit calendar date.
func StatementHasCalendarDate(statement string) bool {
	return reCalendarDateHint.MatchString(statement)
}

func isFactExpiredAt(f Fact, now time.Time) bool {
	if f.ValidTo != nil && strings.TrimSpace(*f.ValidTo) != "" {
		return true
	}
	if strings.TrimSpace(f.ExpiresAt) != "" && !now.IsZero() {
		if exp, err := time.Parse(time.RFC3339, strings.TrimSpace(f.ExpiresAt)); err == nil {
			return now.After(exp)
		}
	}
	return false
}

// LoadFactsState reads local/memory/facts.jsonl lock-free at time.Now().
func LoadFactsState(vaultDir string) (active []Fact, expired []Fact, rawLines int, err error) {
	return LoadFactsStateAt(vaultDir, time.Now().UTC())
}

// LoadFactsStateAt reads local/memory/facts.jsonl lock-free, deduplicating records by Fact.ID
// and treating records with valid_to != nil or now > expires_at as expired in 0 tokens.
func LoadFactsStateAt(vaultDir string, now time.Time) (active []Fact, expired []Fact, rawLines int, err error) {
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
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
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
		if len(item.Scopes) == 0 && len(prev.Scopes) > 0 {
			item.Scopes = prev.Scopes
		}
		if strings.TrimSpace(item.ExpiresAt) == "" && strings.TrimSpace(prev.ExpiresAt) != "" {
			item.ExpiresAt = prev.ExpiresAt
		}
		if !item.Pinned && prev.Pinned {
			item.Pinned = prev.Pinned
		}
		byKey[k] = item
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, rawLines, err
	}

	for _, k := range order {
		item := byKey[k]
		if isFactExpiredAt(item, now) {
			if item.ValidTo == nil || strings.TrimSpace(*item.ValidTo) == "" {
				exp := strings.TrimSpace(item.ExpiresAt)
				item.ValidTo = &exp
			}
			expired = append(expired, item)
		} else {
			item.ValidTo = nil
			active = append(active, item)
		}
	}
	return active, expired, rawLines, nil
}

// writeCanonicalFactsLocked atomically rewrites local/memory/facts.jsonl with 0600 permissions.
// Caller must hold store.RankMemory.
func writeCanonicalFactsLocked(vaultDir string, all []Fact) error {
	memDir := filepath.Join(vaultDir, "memory")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
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
	return store.WriteAtomic(factsPath, []byte(sb.String()), 0o600)
}

// CompactFacts deduplicates local/memory/facts.jsonl by Fact.ID and materializes ValidTo
// for any elapsed ExpiresAt records under store.RankMemory (0600 atomic write).
func CompactFacts(vaultDir string) (int, int, error) {
	var rawLines, afterLines int
	err := withMemoryLock(vaultDir, func() error {
		active, expired, raw, lErr := LoadFactsStateAt(vaultDir, time.Now().UTC())
		if lErr != nil {
			return lErr
		}
		rawLines = raw
		if raw == 0 {
			return nil
		}
		all := make([]Fact, 0, len(active)+len(expired))
		all = append(all, active...)
		all = append(all, expired...)
		if wErr := writeCanonicalFactsLocked(vaultDir, all); wErr != nil {
			return wErr
		}
		afterLines = len(all)
		return nil
	})
	return rawLines, afterLines, err
}

// AddFactWithOptions adds or updates a fact under store.RankMemory with atomic 0600 persistence,
// enforcing <= 240-rune statements and returning an optional advisory warning if a date-bound
// fact omits --expires.
func AddFactWithOptions(vaultDir string, opts AddFactOptions) (Fact, string, error) {
	topic := strings.TrimSpace(opts.Topic)
	statement := strings.TrimSpace(opts.Statement)
	if topic == "" || statement == "" {
		return Fact{}, "", fmt.Errorf("both --topic and --statement are required")
	}
	if runeLen := len([]rune(statement)); runeLen > MaxFactStatementRunes {
		return Fact{}, "", fmt.Errorf("fact statement is %d runes (max %d); move detailed notes into local/memory/topics/<slug>.md and store only a concise pointer", runeLen, MaxFactStatementRunes)
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	expStr, err := ParseFlexibleExpiry(opts.ExpiresAt, now)
	if err != nil {
		return Fact{}, "", err
	}
	scopes := normalizeScopesList(opts.Scopes)

	warning := ""
	if expStr == "" && StatementHasCalendarDate(statement) {
		warning = "note: statement includes a calendar date without --expires; consider setting --expires=<YYYY-MM-DD> so time-bound events auto-expire in 0 tokens"
	}

	var created Fact
	err = withMemoryLock(vaultDir, func() error {
		active, expired, _, lErr := LoadFactsStateAt(vaultDir, now)
		if lErr != nil {
			return lErr
		}

		id := strings.TrimSpace(opts.ID)
		if id == "" {
			existingIDs := make(map[string]bool, len(active)+len(expired))
			for _, f := range active {
				existingIDs[f.ID] = true
			}
			for _, f := range expired {
				existingIDs[f.ID] = true
			}
			datePrefix := now.Format("20060102")
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
			SourceConv: strings.TrimSpace(opts.SourceConv),
			Verified:   opts.Verified,
			ValidFrom:  now.Format(time.RFC3339),
			ValidTo:    nil,
			Scopes:     scopes,
			ExpiresAt:  expStr,
			Pinned:     opts.Pinned,
		}

		var nextActive []Fact
		replaced := false
		for _, f := range active {
			if f.ID == id {
				nextActive = append(nextActive, newFact)
				replaced = true
			} else {
				nextActive = append(nextActive, f)
			}
		}
		if !replaced {
			nextActive = append(nextActive, newFact)
		}
		var nextExpired []Fact
		for _, f := range expired {
			if f.ID == id {
				continue
			}
			nextExpired = append(nextExpired, f)
		}
		all := make([]Fact, 0, len(nextActive)+len(nextExpired))
		all = append(all, nextActive...)
		all = append(all, nextExpired...)
		if wErr := writeCanonicalFactsLocked(vaultDir, all); wErr != nil {
			return wErr
		}
		created = newFact
		return nil
	})
	return created, warning, err
}

// PruneFact soft-expires an active fact by setting valid_to = now (and optional superseded_by)
// under store.RankMemory and writes canonical facts.jsonl atomically (0600).
func PruneFact(vaultDir, id, supersededBy string) (Fact, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Fact{}, fmt.Errorf("fact id is required")
	}
	var pruned Fact
	err := withMemoryLock(vaultDir, func() error {
		nowTime := time.Now().UTC()
		active, expired, _, lErr := LoadFactsStateAt(vaultDir, nowTime)
		if lErr != nil {
			return lErr
		}

		now := nowTime.Format(time.RFC3339)
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
					pruned = f
					return nil
				}
			}
			return fmt.Errorf("fact %q not found in local/memory/facts.jsonl", id)
		}

		all := make([]Fact, 0, len(remainingActive)+len(expired))
		all = append(all, remainingActive...)
		all = append(all, expired...)
		return writeCanonicalFactsLocked(vaultDir, all)
	})
	return pruned, err
}

// NormalizeScopeSet computes canonical scope tags from SessionScopeContext.
func NormalizeScopeSet(ctx SessionScopeContext) map[string]bool {
	set := make(map[string]bool)
	for _, s := range normalizeScopesList(ctx.Scopes) {
		set[s] = true
	}
	if rs := SanitizeEntitySlug(ctx.RepoSlug); rs != "" {
		set["repo:"+rs] = true
		if rs == "headquarter" || rs == "agy-hq" {
			set["repo:agy-hq"] = true
			set["repo:headquarter"] = true
		}
	}
	if ts := SanitizeEntitySlug(ctx.TaskScope); ts != "" {
		set["domain:"+ts] = true
		if ts == "agy-hq" || ts == "headquarter" {
			set["repo:agy-hq"] = true
		}
	}
	if sk := SanitizeEntitySlug(ctx.ActiveSkill); sk != "" {
		set["skill:"+sk] = true
	}
	return set
}

func isImminentExpiry(f Fact, now time.Time) bool {
	if strings.TrimSpace(f.ExpiresAt) == "" || now.IsZero() {
		return false
	}
	exp, err := time.Parse(time.RFC3339, strings.TrimSpace(f.ExpiresAt))
	if err != nil {
		return false
	}
	rem := exp.Sub(now)
	return rem > 0 && rem <= ImminentExpiryWindow
}

// TokenizeStems extracts lowercase normalized non-stopword tokens with simple suffix stemming.
func TokenizeStems(text string) []string {
	var raw []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			raw = append(raw, string(cur))
			cur = cur[:0]
		}
	}
	runes := []rune(text)
	for i, r := range runes {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if len(cur) > 0 && unicode.IsUpper(r) && unicode.IsLower(runes[i-1]) {
				flush()
			}
			cur = append(cur, unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()

	var out []string
	for _, tok := range raw {
		if len(tok) < 3 || isCommonStopword(tok) {
			continue
		}
		out = append(out, simpleStem(tok))
	}
	return out
}

func simpleStem(w string) string {
	for _, suf := range []string{"ations", "ation", "ments", "ment", "ings", "ing", "ities", "ity", "ies", "ed", "es", "ly", "s"} {
		if len(w) > len(suf)+2 && strings.HasSuffix(w, suf) {
			return w[:len(w)-len(suf)]
		}
	}
	return w
}

var commonStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "that": true,
	"this": true, "have": true, "will": true, "into": true, "your": true, "when": true,
	"what": true, "where": true, "which": true, "while": true, "about": true, "after": true,
	"before": true, "under": true, "over": true, "only": true, "also": true, "more": true,
	"some": true, "such": true, "than": true, "then": true, "them": true, "they": true,
	"their": true, "there": true, "these": true, "those": true, "been": true, "being": true,
	"does": true, "doing": true, "done": true, "make": true, "made": true, "need": true,
	"needs": true, "please": true, "help": true, "check": true, "update": true, "review": true,
	"work": true, "working": true, "task": true, "file": true, "files": true, "code": true,
	"local": true, "memory": true, "vault": true, "agent": true, "session": true,
}

func isCommonStopword(w string) bool {
	return commonStopwords[w]
}

func countLexicalOverlap(promptSet map[string]bool, text string) int {
	if len(promptSet) == 0 {
		return 0
	}
	seen := make(map[string]bool)
	hits := 0
	for _, st := range TokenizeStems(text) {
		if promptSet[st] && !seen[st] {
			seen[st] = true
			hits++
		}
	}
	return hits
}

// FormatFactBullet formats a single active fact line for SessionStart injection.
func FormatFactBullet(f Fact) string {
	var tags []string
	if f.ID != "" {
		tags = append(tags, f.ID)
	}
	if f.Verified {
		tags = append(tags, "verified")
	}
	if f.Pinned {
		tags = append(tags, "pinned")
	}
	if len(f.ValidFrom) >= 10 {
		tags = append(tags, f.ValidFrom[:10])
	}
	meta := ""
	if len(tags) > 0 {
		meta = " (" + strings.Join(tags, " · ") + ")"
	}
	return fmt.Sprintf("- [%s]%s %s", f.Topic, meta, f.Statement)
}

// hasConflictingRepoScope returns true when scopeSet carries one or more active repo:*
// scopes and itemScopes declares explicit repo:* scope(s) that do not overlap with
// scopeSet (and does not include "global").
func hasConflictingRepoScope(itemScopes []string, scopeSet map[string]bool) bool {
	hasActiveRepo := false
	for sc := range scopeSet {
		if strings.HasPrefix(sc, "repo:") {
			hasActiveRepo = true
			break
		}
	}
	if !hasActiveRepo {
		return false
	}
	hasItemRepo := false
	for _, raw := range itemScopes {
		sc := NormalizeScope(raw)
		if sc == "global" {
			return false
		}
		if strings.HasPrefix(sc, "repo:") {
			hasItemRepo = true
			if scopeSet[sc] {
				return false
			}
		}
	}
	return hasItemRepo
}

func topicAllScopes(tp TopicPage) []string {
	scopes := make([]string, 0, len(tp.Scopes)+1)
	scopes = append(scopes, tp.Scopes...)
	if strings.TrimSpace(tp.PlaybookOwner) != "" {
		scopes = append(scopes, tp.PlaybookOwner)
	}
	return scopes
}

// TopicConflictsWithScope reports whether tp declares explicit repo:* scopes
// (via Scopes or PlaybookOwner) that conflict with the active repo:* scope(s) in ctx.
func TopicConflictsWithScope(tp TopicPage, ctx SessionScopeContext) bool {
	if ctx.Role == "front_desk" {
		return false
	}
	return hasConflictingRepoScope(topicAllScopes(tp), NormalizeScopeSet(ctx))
}

// SelectFactsForSession performs deterministic Go physics selection (RFC-0024 §3.3):
// no ML weights or lexical hit-count auto-demotion; strict scope filtering, 0-token expiry,
// pinned & imminent-72h elevation, Front Desk per-topic diversity cap, and dual count+byte limits.
func SelectFactsForSession(active []Fact, ctx SessionScopeContext, now time.Time, maxCount, maxBytes int) []Fact {
	if len(active) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if maxCount <= 0 {
		maxCount = 12
		if ctx.LeanAuto {
			maxCount = 6
		}
	}
	if maxBytes <= 0 {
		maxBytes = 2400
		if ctx.LeanAuto {
			maxBytes = 1000
		}
	}

	scopeSet := NormalizeScopeSet(ctx)
	hasActiveScope := len(scopeSet) > 0
	promptSet := make(map[string]bool)
	for _, st := range TokenizeStems(ctx.PromptText) {
		promptSet[st] = true
	}

	type scored struct {
		fact         Fact
		tier         int // lower is higher priority (1..4)
		scopeMatched bool
		lexMatched   bool
		validFrom    string
		orderIdx     int
	}

	var candidates []scored
	for idx, f := range active {
		if isFactExpiredAt(f, now) {
			continue
		}
		fScopes := EffectiveFactScopes(f)
		// Strictly exclude facts declaring conflicting repo:* scope(s) before prompt lexical matching.
		if ctx.Role != "front_desk" && hasConflictingRepoScope(fScopes, scopeSet) {
			continue
		}
		isGlobal := false
		scopeMatched := false
		for _, sc := range fScopes {
			if sc == "global" {
				isGlobal = true
			}
			if scopeSet[sc] {
				scopeMatched = true
			}
		}
		lexMatched := countLexicalOverlap(promptSet, f.Topic+" "+f.Statement) >= 2
		imminent := isImminentExpiry(f, now)

		// Eligibility filtering: strictly exclude mismatched scoped facts (never leak repo:B into repo:A),
		// while allowing global facts as lower-tier fallbacks so upgrading vaults never starve leanAuto.
		if ctx.Role != "front_desk" && (ctx.LeanAuto || hasActiveScope) {
			if !scopeMatched && !isGlobal && !lexMatched {
				continue
			}
		}

		tier := 4
		switch {
		case (scopeMatched && f.Pinned) || (ctx.Role == "front_desk" && isGlobal && f.Pinned) || (imminent && (ctx.Role == "front_desk" || scopeMatched || isGlobal)):
			tier = 1
		case (scopeMatched && f.Verified) || (isGlobal && f.Pinned):
			tier = 2
		case lexMatched || f.Verified:
			tier = 3
		default:
			tier = 4
		}

		candidates = append(candidates, scored{
			fact:         f,
			tier:         tier,
			scopeMatched: scopeMatched,
			lexMatched:   lexMatched,
			validFrom:    f.ValidFrom,
			orderIdx:     idx,
		})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].tier != candidates[j].tier {
			return candidates[i].tier < candidates[j].tier
		}
		if candidates[i].scopeMatched != candidates[j].scopeMatched {
			return candidates[i].scopeMatched
		}
		if candidates[i].lexMatched != candidates[j].lexMatched {
			return candidates[i].lexMatched
		}
		if candidates[i].validFrom != candidates[j].validFrom {
			return candidates[i].validFrom > candidates[j].validFrom
		}
		if candidates[i].orderIdx != candidates[j].orderIdx {
			return candidates[i].orderIdx > candidates[j].orderIdx
		}
		return candidates[i].fact.ID < candidates[j].fact.ID
	})

	perTopicCap := 0
	if ctx.Role == "front_desk" {
		perTopicCap = 3
	}
	topicCounts := make(map[string]int)

	var selected []Fact
	usedBytes := 0
	for _, c := range candidates {
		if len(selected) >= maxCount {
			break
		}
		topicKey := strings.ToLower(strings.TrimSpace(c.fact.Topic))
		if perTopicCap > 0 && !c.fact.Pinned && !isImminentExpiry(c.fact, now) && topicCounts[topicKey] >= perTopicCap {
			continue
		}
		lineBytes := len(FormatFactBullet(c.fact)) + 1
		if len(selected) > 0 && usedBytes+lineBytes > maxBytes {
			continue
		}
		selected = append(selected, c.fact)
		usedBytes += lineBytes
		topicCounts[topicKey]++
	}
	return selected
}

var reMDLinkTarget = regexp.MustCompile(`\[[^\]]+\]\(([^)]+)\)`)

func parseBlockquoteMeta(line string) (string, string, bool) {
	if !strings.HasPrefix(line, ">") {
		return "", "", false
	}
	meta := strings.TrimSpace(strings.TrimLeft(line, ">"))
	idx := strings.IndexByte(meta, ':')
	if idx <= 0 {
		return "", "", false
	}
	key := strings.ToLower(strings.TrimSpace(strings.Trim(meta[:idx], "*")))
	if key == "" {
		return "", "", false
	}
	val := strings.TrimSpace(strings.TrimPrefix(meta[idx+1:], "**"))
	return key, val, true
}

// LoadTopicIndex discovers all markdown topic pages in local/memory/topics/*.md (lock-free),
// extracting # Title, > **Summary:**, > **Scopes:**, > **Playbook-Owner:**, top ## Active Playbook,
// and verifying local markdown links (Lstat non-symlink checked).
func LoadTopicIndex(vaultDir string) ([]TopicPage, error) {
	pages, _, err := loadTopicIndexInternal(vaultDir)
	return pages, err
}

func loadTopicIndexInternal(vaultDir string) ([]TopicPage, []map[string]bool, error) {
	topicsDir := filepath.Join(vaultDir, "memory", "topics")
	names, err := listSortedRegularFiles(topicsDir, ".md")
	if err != nil || len(names) == 0 {
		return nil, nil, err
	}

	var pages []TopicPage
	var topicStems []map[string]bool
	for _, name := range names {
		fullPath := filepath.Join(topicsDir, name)
		fi, lErr := os.Lstat(fullPath)
		if lErr != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		slug := strings.TrimSuffix(name, filepath.Ext(name))
		rawStr := strings.ReplaceAll(string(data), "\r\n", "\n")
		lines := strings.Split(strings.TrimRight(rawStr, "\n"), "\n")
		title := slug
		hasTitle := false
		summary := ""
		hasSummary := false
		var scopes []string
		playbookOwner := ""

		inPlaybook := false
		var playbookLines []string

		for _, rawLine := range lines {
			line := strings.TrimSpace(rawLine)
			if strings.HasPrefix(line, "## ") {
				heading := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "## ")))
				if heading == "active playbook" {
					inPlaybook = true
					continue
				}
				if inPlaybook {
					inPlaybook = false
				}
			}
			if inPlaybook {
				playbookLines = append(playbookLines, rawLine)
				continue
			}
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "# ") && !hasTitle {
				title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
				hasTitle = true
				continue
			}
			if strings.HasPrefix(line, ">") {
				if key, val, ok := parseBlockquoteMeta(line); ok {
					switch key {
					case "scopes", "scope":
						scopes = append(scopes, normalizeScopesList([]string{val})...)
						continue
					case "playbook-owner":
						if list := normalizeScopesList([]string{val}); len(list) > 0 {
							playbookOwner = list[0]
						}
						continue
					case "updated":
						continue
					case "summary":
						if !hasSummary {
							summary = val
							hasSummary = true
						}
						continue
					}
				}
				if !hasSummary {
					clean := strings.TrimSpace(strings.TrimLeft(line, ">"))
					lowerClean := strings.ToLower(clean)
					if strings.HasPrefix(lowerClean, "**updated") || strings.HasPrefix(lowerClean, "updated:") {
						continue
					}
					summary = clean
					hasSummary = true
				}
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

		// Normalize Active Playbook slice (<= MaxPlaybookLines and <= MaxPlaybookBytes)
		activePlaybook := strings.TrimSpace(strings.Join(playbookLines, "\n"))
		if activePlaybook != "" {
			activePlaybook = truncateTextLinesAndBytes(activePlaybook, MaxPlaybookLines, MaxPlaybookBytes)
			if playbookOwner != "" {
				scopes = normalizeScopesList(append(scopes, playbookOwner))
			}
		}

		// Detect broken local markdown links
		var brokenLinks []string
		seenBroken := make(map[string]bool)
		for _, m := range reMDLinkTarget.FindAllStringSubmatch(rawStr, -1) {
			if len(m) < 2 {
				continue
			}
			tgt := strings.TrimSpace(m[1])
			if idx := strings.IndexAny(tgt, " \t"); idx > 0 {
				tgt = tgt[:idx]
			}
			tgt = strings.Trim(tgt, "<>")
			if idx := strings.IndexByte(tgt, '#'); idx >= 0 {
				tgt = tgt[:idx]
			}
			lowerT := strings.ToLower(tgt)
			if tgt == "" ||
				strings.HasPrefix(lowerT, "http://") ||
				strings.HasPrefix(lowerT, "https://") ||
				strings.HasPrefix(lowerT, "mailto:") ||
				strings.HasPrefix(lowerT, "conversation://") ||
				strings.HasPrefix(lowerT, "sidecar://") {
				continue
			}
			checkPath := tgt
			if strings.HasPrefix(lowerT, "file://") {
				checkPath = strings.TrimPrefix(tgt, "file://")
			} else if !filepath.IsAbs(checkPath) {
				checkPath = filepath.Clean(filepath.Join(topicsDir, checkPath))
			}
			if _, sErr := os.Lstat(checkPath); sErr != nil && !seenBroken[tgt] {
				seenBroken[tgt] = true
				brokenLinks = append(brokenLinks, tgt)
			}
		}

		stSet := make(map[string]bool)
		for _, st := range TokenizeStems(title + " " + rawStr) {
			stSet[st] = true
		}
		topicStems = append(topicStems, stSet)

		pages = append(pages, TopicPage{
			Slug:           slug,
			Title:          title,
			Summary:        summary,
			Path:           fullPath,
			LineCount:      len(lines),
			ByteSize:       len(data),
			UpdatedAt:      fi.ModTime().Format("2006-01-02"),
			Scopes:         normalizeScopesList(scopes),
			PlaybookOwner:  playbookOwner,
			ActivePlaybook: activePlaybook,
			HasTitle:       hasTitle,
			HasSummary:     hasSummary,
			BrokenLinks:    brokenLinks,
		})
	}
	return pages, topicStems, nil
}

// SelectScopedPlaybooks collects Active Playbook slices from canonical 1:1 Playbook-Owner topics
// matching ctx scopes, strictly bounded by maxLines and maxBytes (0 on Front Desk).
func SelectScopedPlaybooks(topics []TopicPage, ctx SessionScopeContext, maxLines, maxBytes int) []TopicPage {
	if ctx.Role == "front_desk" || len(topics) == 0 {
		return nil
	}
	if maxLines <= 0 {
		maxLines = MaxPlaybookLines
	}
	if maxBytes <= 0 {
		maxBytes = MaxPlaybookBytes
	}
	scopeSet := NormalizeScopeSet(ctx)
	if len(scopeSet) == 0 {
		return nil
	}
	claimedScope := make(map[string]bool)
	var out []TopicPage
	usedBytes := 0
	usedLines := 0
	for _, tp := range topics {
		owner := strings.ToLower(strings.TrimSpace(tp.PlaybookOwner))
		if owner == "" || strings.TrimSpace(tp.ActivePlaybook) == "" {
			continue
		}
		if !scopeSet[owner] || claimedScope[owner] {
			continue
		}
		claimedScope[owner] = true
		pLines := len(strings.Split(tp.ActivePlaybook, "\n"))
		pBytes := len(tp.ActivePlaybook)
		if len(out) > 0 && (usedBytes+pBytes > maxBytes || usedLines+pLines > maxLines) {
			continue
		}
		trimmed := tp
		if pBytes > maxBytes-usedBytes || pLines > maxLines-usedLines {
			trimmed.ActivePlaybook = truncateTextLinesAndBytes(tp.ActivePlaybook, maxLines-usedLines, maxBytes-usedBytes)
		}
		out = append(out, trimmed)
		usedBytes += len(trimmed.ActivePlaybook)
		usedLines += len(strings.Split(trimmed.ActivePlaybook, "\n"))
		if usedBytes >= maxBytes || usedLines >= maxLines {
			break
		}
	}
	return out
}

// FilterTopicsForScope returns topics matching ctx scopes or prompt tokens (used in lean_auto).
func FilterTopicsForScope(topics []TopicPage, ctx SessionScopeContext, maxTopics int) []TopicPage {
	if len(topics) == 0 {
		return nil
	}
	if maxTopics <= 0 {
		maxTopics = 3
	}
	scopeSet := NormalizeScopeSet(ctx)
	promptSet := make(map[string]bool)
	for _, st := range TokenizeStems(ctx.PromptText) {
		promptSet[st] = true
	}
	type scored struct {
		tp    TopicPage
		score int
	}
	var hits []scored
	for _, tp := range topics {
		if ctx.Role != "front_desk" && hasConflictingRepoScope(topicAllScopes(tp), scopeSet) {
			continue
		}
		sc := 0
		if tp.PlaybookOwner != "" && scopeSet[tp.PlaybookOwner] {
			sc += 10
		}
		for _, s := range tp.Scopes {
			if scopeSet[s] {
				sc += 5
			}
		}
		if ctx.PromptText != "" && strings.Contains(strings.ToLower(ctx.PromptText), strings.ToLower(tp.Slug)) {
			sc += 8
		}
		sc += countLexicalOverlap(promptSet, tp.Slug+" "+tp.Title+" "+tp.Summary)
		if sc >= 2 {
			hits = append(hits, scored{tp: tp, score: sc})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].tp.Slug < hits[j].tp.Slug
	})
	var out []TopicPage
	for i := 0; i < len(hits) && i < maxTopics; i++ {
		out = append(out, hits[i].tp)
	}
	return out
}

// MatchContextHints returns up to maxHints high-confidence topic matches for Task/Subagent sessions.
// Always returns nil on Front Desk.
func MatchContextHints(topics []TopicPage, ctx SessionScopeContext, maxHints int) []TopicPage {
	if ctx.Role == "front_desk" || len(topics) == 0 {
		return nil
	}
	if maxHints <= 0 {
		maxHints = 2
	}
	promptLower := strings.ToLower(ctx.PromptText)
	promptSet := make(map[string]bool)
	for _, st := range TokenizeStems(ctx.PromptText) {
		promptSet[st] = true
	}
	scopeSet := NormalizeScopeSet(ctx)

	type candidate struct {
		tp    TopicPage
		score int
	}
	var out []candidate
	for _, tp := range topics {
		if hasConflictingRepoScope(topicAllScopes(tp), scopeSet) {
			continue
		}
		// Exact slug match or >= 2 high-signal tokens
		score := 0
		if promptLower != "" && strings.Contains(promptLower, strings.ToLower(tp.Slug)) {
			score += 10
		}
		for _, s := range tp.Scopes {
			if scopeSet[s] {
				score += 4
			}
		}
		overlap := countLexicalOverlap(promptSet, tp.Slug+" "+tp.Title+" "+tp.Summary)
		if overlap >= 2 {
			score += overlap * 2
		}
		if score >= 4 {
			out = append(out, candidate{tp: tp, score: score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].tp.Slug < out[j].tp.Slug
	})
	var res []TopicPage
	for i := 0; i < len(out) && i < maxHints; i++ {
		res = append(res, out[i].tp)
	}
	return res
}

// TruncateMemoryIndex enforces both line and byte caps cleanly on line boundaries.
func TruncateMemoryIndex(content string, maxLines, maxBytes int) (string, bool) {
	if maxLines <= 0 {
		maxLines = MaxMemoryIndexLines
	}
	if maxBytes <= 0 {
		maxBytes = MaxMemoryIndexBytes
	}
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(content, "\r\n", "\n"), "\n"), "\n")
	if len(lines) <= maxLines && len(content) <= maxBytes {
		return strings.TrimSpace(content), false
	}
	var kept []string
	usedBytes := 0
	for i, line := range lines {
		if i >= maxLines {
			break
		}
		add := len(line)
		if len(kept) > 0 {
			add++
		}
		if len(kept) > 0 && usedBytes+add > maxBytes {
			break
		}
		if len(kept) == 0 && len(line) > maxBytes {
			line = string([]rune(line)[:minInt(len([]rune(line)), maxBytes/2)]) + "..."
			add = len(line)
		}
		kept = append(kept, line)
		usedBytes += add
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), true
}

func truncateTextLinesAndBytes(s string, maxLines, maxBytes int) string {
	out, _ := TruncateMemoryIndex(s, maxLines, maxBytes)
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type memoryAuditData struct {
	activeFacts []Fact
	memBytes    []byte
	userBytes   []byte
}

// AuditMemory evaluates dual-cap MEMORY.md & USER.md budgets, Topic Wiki graph integrity
// (broken links, header validity, duplicate Playbook-Owner claims, Jaccard >= 0.45 near-duplicates),
// active/expired/missing-TTL facts, and stale task references (lock-free).
func AuditMemory(vaultDir string, completedTaskSlugs []string) (*MemoryStatusReport, error) {
	rep, _, err := auditMemoryInternal(vaultDir, completedTaskSlugs, time.Now().UTC())
	return rep, err
}

func auditMemoryInternal(vaultDir string, completedTaskSlugs []string, now time.Time) (*MemoryStatusReport, memoryAuditData, error) {
	memPath := filepath.Join(vaultDir, "MEMORY.md")
	memBytes, _ := os.ReadFile(memPath)
	memContent := strings.TrimRight(strings.ReplaceAll(string(memBytes), "\r\n", "\n"), "\n")

	memLines := 0
	longLines := 0
	if strings.TrimSpace(memContent) != "" {
		lines := strings.Split(memContent, "\n")
		memLines = len(lines)
		for _, l := range lines {
			if len(l) > MaxMemoryLineBytes {
				longLines++
			}
		}
	}
	words := len(strings.Fields(memContent))
	estTokens := (words * 4) / 3
	if len(memBytes)/4 > estTokens {
		estTokens = len(memBytes) / 4
	}

	userBytes, _ := os.ReadFile(filepath.Join(vaultDir, "USER.md"))

	topics, topicStems, err := loadTopicIndexInternal(vaultDir)
	if err != nil {
		return nil, memoryAuditData{}, err
	}
	entities, _ := ListEntities(vaultDir)

	var invalidHeaders []string
	var brokenLinks []string
	var dupOwners []string
	ownerSeen := make(map[string]string)
	topicsTotalBytes := 0

	for _, tp := range topics {
		topicsTotalBytes += tp.ByteSize
		if !tp.HasTitle || !tp.HasSummary {
			invalidHeaders = append(invalidHeaders, tp.Slug)
		}
		for _, bl := range tp.BrokenLinks {
			brokenLinks = append(brokenLinks, fmt.Sprintf("%s -> %s", tp.Slug, bl))
		}
		if tp.PlaybookOwner != "" {
			if prev, exists := ownerSeen[tp.PlaybookOwner]; exists {
				dupOwners = append(dupOwners, fmt.Sprintf("scope %q claimed by both %s and %s", tp.PlaybookOwner, prev, tp.Slug))
			} else {
				ownerSeen[tp.PlaybookOwner] = tp.Slug
			}
		}
	}

	var nearDups []string
	for i := 0; i < len(topics); i++ {
		if len(topicStems[i]) < 8 {
			continue
		}
		for j := i + 1; j < len(topics); j++ {
			if len(topicStems[j]) < 8 {
				continue
			}
			inter := 0
			for k := range topicStems[i] {
				if topicStems[j][k] {
					inter++
				}
			}
			union := len(topicStems[i]) + len(topicStems[j]) - inter
			if union > 0 && float64(inter)/float64(union) >= 0.45 {
				nearDups = append(nearDups, fmt.Sprintf("%s <-> %s (jaccard=%.2f)", topics[i].Slug, topics[j].Slug, float64(inter)/float64(union)))
			}
		}
	}

	activeFacts, expiredFacts, rawFactLines, err := LoadFactsStateAt(vaultDir, now)
	if err != nil {
		return nil, memoryAuditData{}, err
	}

	overdueTTL := 0
	var missingTTLEvents []string
	for _, f := range activeFacts {
		if strings.TrimSpace(f.ExpiresAt) == "" && StatementHasCalendarDate(f.Statement) {
			missingTTLEvents = append(missingTTLEvents, f.ID)
		}
	}
	for _, f := range expiredFacts {
		if strings.TrimSpace(f.ExpiresAt) != "" && (f.ValidTo == nil || *f.ValidTo == f.ExpiresAt) {
			overdueTTL++
		}
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

	rep := &MemoryStatusReport{
		MemoryLines:             memLines,
		MemoryMaxLines:          MaxMemoryIndexLines,
		MemoryBytes:             len(memBytes),
		MemoryMaxBytes:          MaxMemoryIndexBytes,
		MemoryLongLines:         longLines,
		UserBytes:               len(userBytes),
		UserMaxBytes:            MaxUserAdvisoryBytes,
		EstimatedTokens:         estTokens,
		OverBudget:              memLines > MaxMemoryIndexLines || len(memBytes) > MaxMemoryIndexBytes,
		Topics:                  topics,
		TopicsTotalBytes:        topicsTotalBytes,
		EntityCount:             len(entities),
		InvalidTopicHeaders:     invalidHeaders,
		BrokenTopicLinks:        brokenLinks,
		DuplicatePlaybookOwners: dupOwners,
		NearDuplicateTopics:     nearDups,
		ActiveFactCount:         len(activeFacts),
		ExpiredFactCount:        len(expiredFacts),
		OverdueTTLFactCount:     overdueTTL,
		MissingTTLEventFacts:    missingTTLEvents,
		RawFactLines:            rawFactLines,
		StaleReferences:         staleRefs,
	}
	data := memoryAuditData{
		activeFacts: activeFacts,
		memBytes:    memBytes,
		userBytes:   userBytes,
	}
	return rep, data, nil
}
