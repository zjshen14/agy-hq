package vault

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

// SearchOptions configures a lock-free BM25 search across safe Vault memory layers.
type SearchOptions struct {
	Query          string
	Scope          string
	Layers         []string // subset of: facts, topics, entities, notes, journal, progress
	IncludeArchive bool
	Limit          int
}

// SearchResult is a single ranked match with clickable file:// line-range provenance.
type SearchResult struct {
	Layer      string   `json:"layer"`
	Title      string   `json:"title"`
	Path       string   `json:"path"`
	RelPath    string   `json:"relPath"`
	LineStart  int      `json:"lineStart,omitempty"`
	LineEnd    int      `json:"lineEnd,omitempty"`
	Provenance string   `json:"provenance"`
	Score      float64  `json:"score"`
	Scopes     []string `json:"scopes,omitempty"`
	Snippet    string   `json:"snippet"`
	Archived   bool     `json:"archived,omitempty"`
}

type bm25Doc struct {
	layer      string
	title      string
	absPath    string
	relPath    string
	lineStart  int
	lineEnd    int
	scopes     []string
	text       string
	tokens     []string
	termFreq   map[string]int
	archived   bool
	recencyDay int // days since epoch for mild tie-breaking
}

var searchStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true,
	"that": true, "this": true, "are": true, "was": true, "were": true,
	"have": true, "has": true, "had": true, "not": true, "but": true,
	"you": true, "your": true, "our": true, "can": true, "will": true,
	"into": true, "onto": true, "about": true, "when": true, "what": true,
	"which": true, "where": true, "how": true, "who": true, "why": true,
}

// tokenizeForBM25 extracts normalized Latin/digit tokens and CJK unigrams+bigrams (pure Go stdlib).
func tokenizeForBM25(text string) []string {
	var tokens []string
	var cur strings.Builder
	var cjkRun []rune

	flushWord := func() {
		if cur.Len() > 0 {
			w := strings.ToLower(cur.String())
			cur.Reset()
			if len(w) >= 2 && !searchStopWords[w] {
				tokens = append(tokens, w)
			}
		}
	}
	flushCJK := func() {
		if len(cjkRun) == 0 {
			return
		}
		for i, r := range cjkRun {
			tokens = append(tokens, string(r))
			if i+1 < len(cjkRun) {
				tokens = append(tokens, string([]rune{r, cjkRun[i+1]}))
			}
		}
		cjkRun = cjkRun[:0]
	}

	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			cjkRun = append(cjkRun, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			flushCJK()
			cur.WriteRune(unicode.ToLower(r))
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return tokens
}

// SearchMemory executes a lock-free pure-Go Okapi BM25 search across explicit allowlisted Vault memory layers.
// Unconditionally excludes private/**, approvals/**, inbox/**, worktrees/**, repos/**,
// memory/{channels,outbox,events*,taint,pending}/**, all symlinks, and tainted journal entries.
func SearchMemory(vaultDir string, opts SearchOptions) ([]SearchResult, error) {
	q := strings.TrimSpace(opts.Query)
	if q == "" {
		return nil, fmt.Errorf("search query is required")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	layerSet := make(map[string]bool)
	if len(opts.Layers) == 0 {
		for _, l := range []string{"facts", "topics", "entities", "notes", "journal", "progress"} {
			layerSet[l] = true
		}
	} else {
		for _, raw := range opts.Layers {
			for _, part := range strings.Split(raw, ",") {
				p := strings.ToLower(strings.TrimSpace(part))
				if p != "" {
					layerSet[p] = true
				}
			}
		}
	}

	scopeFilter := NormalizeScope(opts.Scope)

	var docs []bm25Doc
	if layerSet["facts"] {
		docs = append(docs, collectFactSearchDocs(vaultDir, opts.IncludeArchive)...)
	}
	if layerSet["topics"] {
		docs = append(docs, collectMarkdownSearchDocs(vaultDir, filepath.Join(vaultDir, "memory", "topics"), "topics", opts.IncludeArchive)...)
	}
	if layerSet["entities"] {
		docs = append(docs, collectMarkdownSearchDocs(vaultDir, filepath.Join(vaultDir, "memory", "entities"), "entities", opts.IncludeArchive)...)
	}
	if layerSet["notes"] {
		docs = append(docs, collectMarkdownSearchDocs(vaultDir, filepath.Join(vaultDir, "notes"), "notes", opts.IncludeArchive)...)
	}
	if layerSet["progress"] {
		docs = append(docs, collectMarkdownSearchDocs(vaultDir, filepath.Join(vaultDir, "memory", "progress"), "progress", opts.IncludeArchive)...)
	}
	if layerSet["journal"] {
		docs = append(docs, collectJournalSearchDocs(vaultDir, opts.IncludeArchive)...)
	}

	if len(docs) == 0 {
		return nil, nil
	}

	qTokens := tokenizeForBM25(q)
	if len(qTokens) == 0 {
		qTokens = []string{strings.ToLower(q)}
	}
	qLower := strings.ToLower(q)

	// Compute document frequencies & average length
	df := make(map[string]int)
	totalLen := 0
	for i := range docs {
		toks := tokenizeForBM25(docs[i].title + " " + docs[i].text)
		docs[i].tokens = toks
		tf := make(map[string]int, len(toks))
		for _, tok := range toks {
			tf[tok]++
		}
		docs[i].termFreq = tf
		totalLen += len(toks)
		for tok := range tf {
			df[tok]++
		}
	}

	nDocs := float64(len(docs))
	avgDL := float64(totalLen) / math.Max(1.0, nDocs)
	const k1 = 1.2
	const b = 0.75

	// Deduplicate query terms while preserving query TF
	qTF := make(map[string]int)
	for _, qt := range qTokens {
		qTF[qt]++
	}

	type scoredDoc struct {
		doc   bm25Doc
		score float64
	}
	var scored []scoredDoc

	for _, d := range docs {
		if scopeFilter != "" && !docMatchesScopeFilter(d, scopeFilter) {
			continue
		}
		dl := float64(len(d.tokens))
		if dl == 0 {
			continue
		}
		score := 0.0
		for term := range qTF {
			tf := float64(d.termFreq[term])
			if tf == 0 {
				continue
			}
			docFreq := float64(df[term])
			idf := math.Log(1.0 + (nDocs-docFreq+0.5)/(docFreq+0.5))
			denom := tf + k1*(1.0-b+b*(dl/math.Max(1.0, avgDL)))
			score += idf * ((tf * (k1 + 1.0)) / denom)
		}
		// Exact phrase boost
		lowerText := strings.ToLower(d.title + "\n" + d.text)
		if strings.Contains(lowerText, qLower) {
			score += 2.5
		}
		if score <= 0 {
			continue
		}
		// Mild layer boost: curated layers (topics/facts/entities) slightly ahead of raw journals
		switch d.layer {
		case "topics":
			score *= 1.15
		case "facts":
			score *= 1.12
		case "entities":
			score *= 1.08
		case "notes":
			score *= 1.05
		}
		if d.archived {
			score *= 0.85
		}
		scored = append(scored, scoredDoc{doc: d, score: score})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if math.Abs(scored[i].score-scored[j].score) > 1e-6 {
			return scored[i].score > scored[j].score
		}
		if scored[i].doc.recencyDay != scored[j].doc.recencyDay {
			return scored[i].doc.recencyDay > scored[j].doc.recencyDay
		}
		return scored[i].doc.relPath < scored[j].doc.relPath
	})

	if len(scored) > limit {
		scored = scored[:limit]
	}

	results := make([]SearchResult, 0, len(scored))
	for _, s := range scored {
		d := s.doc
		prov := "file://" + filepath.ToSlash(d.absPath)
		if d.lineStart > 0 {
			if d.lineEnd > d.lineStart {
				prov = fmt.Sprintf("%s#L%d-L%d", prov, d.lineStart, d.lineEnd)
			} else {
				prov = fmt.Sprintf("%s#L%d", prov, d.lineStart)
			}
		}
		results = append(results, SearchResult{
			Layer:      d.layer,
			Title:      d.title,
			Path:       d.absPath,
			RelPath:    d.relPath,
			LineStart:  d.lineStart,
			LineEnd:    d.lineEnd,
			Provenance: prov,
			Score:      math.Round(s.score*1000) / 1000,
			Scopes:     d.scopes,
			Snippet:    buildSearchSnippet(d.text, qLower, 450),
			Archived:   d.archived,
		})
	}
	return results, nil
}

func docMatchesScopeFilter(d bm25Doc, scopeFilter string) bool {
	if scopeFilter == "" {
		return true
	}
	for _, s := range d.scopes {
		if NormalizeScope(s) == scopeFilter {
			return true
		}
	}
	// Fallback: match against path / title for notes, entities, journals
	slugPart := scopeFilter
	if idx := strings.Index(slugPart, ":"); idx >= 0 {
		slugPart = slugPart[idx+1:]
	}
	if slugPart == "" {
		return true
	}
	lowerRel := strings.ToLower(d.relPath)
	lowerTitle := strings.ToLower(d.title)
	return strings.Contains(lowerRel, slugPart) || strings.Contains(lowerTitle, slugPart)
}

func buildSearchSnippet(text, qLower string, maxRunes int) string {
	clean := strings.Join(strings.Fields(text), " ")
	runes := []rune(clean)
	if len(runes) <= maxRunes {
		return clean
	}
	lowerClean := strings.ToLower(clean)
	idx := strings.Index(lowerClean, qLower)
	if idx < 0 {
		return string(runes[:maxRunes-1]) + "…"
	}
	runeIdx := len([]rune(clean[:idx]))
	start := runeIdx - maxRunes/3
	if start < 0 {
		start = 0
	}
	end := start + maxRunes
	if end > len(runes) {
		end = len(runes)
		start = end - maxRunes
		if start < 0 {
			start = 0
		}
	}
	out := string(runes[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}

func collectFactSearchDocs(vaultDir string, includeArchive bool) []bm25Doc {
	factsPath := filepath.Join(vaultDir, "memory", "facts.jsonl")
	if !isRegularNonSymlinkFile(factsPath) {
		return nil
	}
	f, err := os.Open(factsPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	now := time.Now().UTC()
	var docs []bm25Doc
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	// Deduplicate by factKey, keeping last occurrence line number
	type indexedFact struct {
		fact   Fact
		lineNo int
	}
	var order []string
	byKey := make(map[string]indexedFact)
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item Fact
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}
		k := factKey(item)
		if _, exists := byKey[k]; !exists {
			order = append(order, k)
		}
		byKey[k] = indexedFact{fact: item, lineNo: lineNo}
	}

	absPath, _ := filepath.Abs(factsPath)
	for _, k := range order {
		entry := byKey[k]
		expired := isFactExpiredAt(entry.fact, now)
		if expired && !includeArchive {
			continue
		}
		if strings.TrimSpace(entry.fact.Statement) == "" || entry.fact.Statement == "[FORGOTTEN BY USER]" {
			continue
		}
		day := 0
		if ts, ok := parseAnyTimestamp(entry.fact.ValidFrom); ok {
			day = int(ts.Unix() / 86400)
		}
		docs = append(docs, bm25Doc{
			layer:      "facts",
			title:      fmt.Sprintf("[%s] %s", entry.fact.Topic, entry.fact.ID),
			absPath:    absPath,
			relPath:    "memory/facts.jsonl",
			lineStart:  entry.lineNo,
			lineEnd:    entry.lineNo,
			scopes:     entry.fact.Scopes,
			text:       entry.fact.Topic + " " + entry.fact.Statement,
			archived:   expired,
			recencyDay: day,
		})
	}
	return docs
}

func collectMarkdownSearchDocs(vaultDir, rootDir, layer string, includeArchive bool) []bm25Doc {
	var docs []bm25Doc
	_ = filepath.WalkDir(rootDir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if (name == "archive" || name == "archived") && !includeArchive {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(name), ".md") || !isRegularNonSymlinkFile(p) {
			return nil
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		absPath, _ := filepath.Abs(p)
		rel, _ := filepath.Rel(vaultDir, p)
		relSlash := filepath.ToSlash(rel)
		archived := strings.Contains(relSlash, "/archive/") || strings.Contains(relSlash, "/archived/")

		info, _ := os.Lstat(p)
		day := 0
		if info != nil {
			day = int(info.ModTime().Unix() / 86400)
		}

		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		baseSlug := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		docTitle := baseSlug
		var docScopes []string
		for _, raw := range lines {
			trimmed := strings.TrimSpace(raw)
			if strings.HasPrefix(trimmed, "# ") && docTitle == baseSlug {
				docTitle = strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
			}
			if key, val, ok := parseBlockquoteMeta(trimmed); ok {
				switch key {
				case "scopes", "scope":
					docScopes = append(docScopes, ParseScopeList(val)...)
				case "playbook-owner":
					if owner := NormalizeScope(val); owner != "" {
						docScopes = append(docScopes, owner)
					}
				}
			}
		}

		// Chunk by "## " headings to yield crisp line-range provenance
		sectionTitle := docTitle
		sectionStart := 1
		var sectionLines []string
		flush := func(endLine int) {
			body := strings.TrimSpace(strings.Join(sectionLines, "\n"))
			if body == "" {
				return
			}
			docs = append(docs, bm25Doc{
				layer:      layer,
				title:      sectionTitle,
				absPath:    absPath,
				relPath:    relSlash,
				lineStart:  sectionStart,
				lineEnd:    endLine,
				scopes:     docScopes,
				text:       body,
				archived:   archived,
				recencyDay: day,
			})
		}

		for i, line := range lines {
			lineNo := i + 1
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "## ") && len(sectionLines) > 0 {
				flush(lineNo - 1)
				sectionTitle = docTitle + " — " + strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
				sectionStart = lineNo
				sectionLines = []string{line}
			} else {
				sectionLines = append(sectionLines, line)
			}
		}
		flush(len(lines))
		return nil
	})
	return docs
}

func collectJournalSearchDocs(vaultDir string, includeArchive bool) []bm25Doc {
	var docs []bm25Doc
	_ = WalkJournals(vaultDir, time.Time{}, includeArchive, func(p string, rec JournalRecord) bool {
		// Unconditionally exclude tainted sessions from search recall
		if rec.TaintedSession {
			return true
		}
		text := strings.TrimSpace(rec.UserPrompt + "\n" + rec.ResponseSummary)
		if text == "" {
			return true
		}
		absPath, _ := filepath.Abs(p)
		rel, _ := filepath.Rel(vaultDir, p)
		relSlash := filepath.ToSlash(rel)
		archived := strings.Contains(relSlash, "/archive/")
		day := 0
		if ts, ok := parseAnyTimestamp(rec.Timestamp); ok {
			day = int(ts.Unix() / 86400)
		}
		title := fmt.Sprintf("Journal %s (turn %d)", shortConvID(rec.ConversationID), rec.ExecutionNum)
		docs = append(docs, bm25Doc{
			layer:      "journal",
			title:      title,
			absPath:    absPath,
			relPath:    relSlash,
			lineStart:  1,
			lineEnd:    1,
			text:       text,
			archived:   archived,
			recencyDay: day,
		})
		return true
	})
	return docs
}

func shortConvID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "unknown"
	}
	return id
}

// NormalizeScope canonicalizes a single scope tag (e.g. "repo:agy-hq", "domain:investing", "global").
func NormalizeScope(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.Trim(s, "`'\"[]()")
	return s
}

// ParseScopeList parses a comma/space separated list of scopes into canonical tags.
func ParseScopeList(raw string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t'
	}) {
		s := NormalizeScope(part)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func parseAnyTimestamp(raw string) (time.Time, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts.UTC(), true
		}
	}
	return time.Time{}, false
}

func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 1 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-1]) + "…"
}
