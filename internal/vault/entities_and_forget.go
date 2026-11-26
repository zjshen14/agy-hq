package vault

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/store"
)

const MaxEntityBullets = 10

var reNonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// EntityDossier represents an on-demand counterparty/contact/project dossier in local/memory/entities/<slug>.md.
// Entity dossiers carry 0 bytes of SessionStart prompt overhead and are queried on-demand via LookupEntities.
type EntityDossier struct {
	Slug      string   `json:"slug"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Emails    []string `json:"emails,omitempty"`
	Handles   []string `json:"handles,omitempty"`
	Spaces    []string `json:"spaces,omitempty"`
	Context   string   `json:"context,omitempty"`
	Notes     []string `json:"notes,omitempty"`
	Path      string   `json:"path,omitempty"`
	UpdatedAt string   `json:"updatedAt,omitempty"`
}

// ForgetReport summarizes a surgical cascading forget operation across the local/ Vault.
type ForgetReport struct {
	Query              string   `json:"query,omitempty"`
	TopicSlug          string   `json:"topicSlug,omitempty"`
	EntitySlug         string   `json:"entitySlug,omitempty"`
	DryRun             bool     `json:"dryRun"`
	ExpiredFactIDs     []string `json:"expiredFactIds,omitempty"`
	MemoryLinesRemoved int      `json:"memoryLinesRemoved"`
	DeletedFiles       []string `json:"deletedFiles,omitempty"`
	ScrubbedFiles      []string `json:"scrubbedFiles,omitempty"`
	SourceConvIDs      []string `json:"sourceConvIds,omitempty"`
	DenylistUpdated    bool     `json:"denylistUpdated"`
	SnapshotCommitted  bool     `json:"snapshotCommitted"`
}

// SanitizeEntitySlug normalizes an entity slug or display name into kebab-case.
func SanitizeEntitySlug(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = reNonSlug.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

func dedupCleanStrings(items ...string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, raw := range items {
		for _, part := range strings.Split(raw, ",") {
			v := strings.TrimSpace(part)
			if v == "" {
				continue
			}
			key := strings.ToLower(v)
			if !seen[key] {
				seen[key] = true
				out = append(out, v)
			}
		}
	}
	return out
}

func isRegularNonSymlinkFile(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
}

// ParseEntityDossierFile parses a local/memory/entities/<slug>.md markdown file into an EntityDossier.
func ParseEntityDossierFile(path string) (EntityDossier, error) {
	if !isRegularNonSymlinkFile(path) {
		return EntityDossier{}, os.ErrNotExist
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return EntityDossier{}, err
	}
	slug := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	d := EntityDossier{
		Slug: slug,
		Name: slug,
		Kind: "person",
		Path: path,
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		d.UpdatedAt = info.ModTime().Format("2006-01-02")
	}

	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# ") && d.Name == slug {
			d.Name = strings.TrimSpace(strings.TrimPrefix(line, "# "))
			continue
		}
		if key, val, ok := parseBlockquoteMeta(line); ok {
			switch key {
			case "kind":
				d.Kind = val
			case "emails":
				d.Emails = dedupCleanStrings(val)
			case "handles":
				d.Handles = dedupCleanStrings(val)
			case "spaces":
				d.Spaces = dedupCleanStrings(val)
			case "context":
				d.Context = val
			case "updated":
				if val != "" {
					d.UpdatedAt = val
				}
			}
			continue
		}
		if strings.HasPrefix(line, "- ") {
			note := strings.TrimSpace(strings.TrimPrefix(line, "- "))
			if note != "" {
				d.Notes = append(d.Notes, note)
			}
		}
	}
	return d, nil
}

func renderEntityDossierMarkdown(d EntityDossier) string {
	var sb strings.Builder
	name := strings.TrimSpace(d.Name)
	if name == "" {
		name = d.Slug
	}
	kind := strings.TrimSpace(d.Kind)
	if kind == "" {
		kind = "person"
	}
	updated := strings.TrimSpace(d.UpdatedAt)
	if updated == "" {
		updated = time.Now().Format("2006-01-02")
	}

	sb.WriteString("# " + name + "\n")
	sb.WriteString("> **Kind:** " + kind + "\n")
	if len(d.Emails) > 0 {
		sb.WriteString("> **Emails:** " + strings.Join(d.Emails, ", ") + "\n")
	}
	if len(d.Handles) > 0 {
		sb.WriteString("> **Handles:** " + strings.Join(d.Handles, ", ") + "\n")
	}
	if len(d.Spaces) > 0 {
		sb.WriteString("> **Spaces:** " + strings.Join(d.Spaces, ", ") + "\n")
	}
	if strings.TrimSpace(d.Context) != "" {
		sb.WriteString("> **Context:** " + strings.TrimSpace(d.Context) + "\n")
	}
	sb.WriteString("> **Updated:** " + updated + "\n")
	if len(d.Notes) > 0 {
		sb.WriteString("\n## Interaction & Working Style Notes\n")
		for _, n := range d.Notes {
			sb.WriteString("- " + strings.TrimSpace(n) + "\n")
		}
	}
	return sb.String()
}

// UpsertEntity creates or merges an entity dossier in local/memory/entities/<slug>.md
// under store.RankMemory with atomic 0600 writes. Overflow bullets beyond MaxEntityBullets
// are rolled into local/memory/entities/archived/<slug>-history.md.
func UpsertEntity(vaultDir string, input EntityDossier, appendNote string) (EntityDossier, error) {
	slug := SanitizeEntitySlug(input.Slug)
	if slug == "" {
		slug = SanitizeEntitySlug(input.Name)
	}
	if slug == "" {
		return EntityDossier{}, fmt.Errorf("entity --slug or --name is required")
	}

	entDir := filepath.Join(vaultDir, "memory", "entities")
	if err := os.MkdirAll(entDir, 0o700); err != nil {
		return EntityDossier{}, err
	}
	entPath := filepath.Join(entDir, slug+".md")

	var result EntityDossier
	err := withMemoryLock(vaultDir, func() error {
		cur := EntityDossier{
			Slug: slug,
			Name: slug,
			Kind: "person",
			Path: entPath,
		}
		if existing, err := ParseEntityDossierFile(entPath); err == nil {
			cur = existing
		}

		if strings.TrimSpace(input.Name) != "" {
			cur.Name = strings.TrimSpace(input.Name)
		}
		if strings.TrimSpace(input.Kind) != "" {
			cur.Kind = strings.TrimSpace(input.Kind)
		}
		cur.Emails = dedupCleanStrings(append(cur.Emails, input.Emails...)...)
		cur.Handles = dedupCleanStrings(append(cur.Handles, input.Handles...)...)
		cur.Spaces = dedupCleanStrings(append(cur.Spaces, input.Spaces...)...)
		if strings.TrimSpace(input.Context) != "" {
			cur.Context = strings.TrimSpace(input.Context)
		}
		for _, n := range input.Notes {
			cur.Notes = appendUniqueNote(cur.Notes, n)
		}
		if strings.TrimSpace(appendNote) != "" {
			note := strings.TrimSpace(appendNote)
			if !strings.HasPrefix(note, "[") {
				note = fmt.Sprintf("[%s] %s", time.Now().Format("2006-01-02"), note)
			}
			cur.Notes = appendUniqueNote(cur.Notes, note)
		}

		// Roll overflow bullets beyond MaxEntityBullets into entities/archived/<slug>-history.md
		if len(cur.Notes) > MaxEntityBullets {
			overflow := cur.Notes[:len(cur.Notes)-MaxEntityBullets]
			cur.Notes = append([]string(nil), cur.Notes[len(cur.Notes)-MaxEntityBullets:]...)
			if err := appendEntityArchivedHistoryLocked(vaultDir, cur.Slug, cur.Name, overflow); err != nil {
				return err
			}
		}

		cur.UpdatedAt = time.Now().Format("2006-01-02")
		cur.Path = entPath

		if err := store.WriteAtomic(entPath, []byte(renderEntityDossierMarkdown(cur)), 0o600); err != nil {
			return err
		}
		result = cur
		return nil
	})
	if err != nil {
		return EntityDossier{}, err
	}
	return result, nil
}

func appendEntityArchivedHistoryLocked(vaultDir, slug, name string, overflow []string) error {
	if len(overflow) == 0 {
		return nil
	}
	archDir := filepath.Join(vaultDir, "memory", "entities", "archived")
	if err := os.MkdirAll(archDir, 0o700); err != nil {
		return err
	}
	histPath := filepath.Join(archDir, slug+"-history.md")
	var existing string
	if isRegularNonSymlinkFile(histPath) {
		if b, err := os.ReadFile(histPath); err == nil {
			existing = string(b)
		}
	}
	var sb strings.Builder
	if strings.TrimSpace(existing) == "" {
		title := strings.TrimSpace(name)
		if title == "" {
			title = slug
		}
		sb.WriteString("# " + title + " — Archived Notes\n\n")
	} else {
		sb.WriteString(strings.TrimRight(existing, "\n") + "\n")
	}
	for _, n := range overflow {
		sb.WriteString("- " + strings.TrimSpace(n) + "\n")
	}
	return store.WriteAtomic(histPath, []byte(sb.String()), 0o600)
}

func appendUniqueNote(notes []string, candidate string) []string {
	c := strings.TrimSpace(candidate)
	if c == "" {
		return notes
	}
	for _, existing := range notes {
		if strings.EqualFold(strings.TrimSpace(existing), c) {
			return notes
		}
	}
	return append(notes, c)
}

// ListEntities loads all entity dossiers from local/memory/entities/*.md sorted by slug.
func ListEntities(vaultDir string) ([]EntityDossier, error) {
	entDir := filepath.Join(vaultDir, "memory", "entities")
	names, err := listSortedRegularFiles(entDir, ".md")
	if err != nil {
		return nil, err
	}

	var out []EntityDossier
	for _, name := range names {
		d, err := ParseEntityDossierFile(filepath.Join(entDir, name))
		if err != nil {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// LookupEntities performs a fast (<5ms) case-insensitive match across entity slugs, names, emails, handles, GChat space IDs, context, and notes.
func LookupEntities(vaultDir, query string) ([]EntityDossier, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return ListEntities(vaultDir)
	}
	all, err := ListEntities(vaultDir)
	if err != nil {
		return nil, err
	}
	var matches []EntityDossier
	for _, d := range all {
		if entityMatchesQuery(d, q) {
			matches = append(matches, d)
		}
	}
	return matches, nil
}

// LookupEntitiesRedacted performs a taint-safe lookup that rejects empty queries, matches ONLY against
// public routing fields (Slug, Name, Emails, Handles — never Context, Spaces, or Notes), and strips
// private Context, Notes, Spaces, and Path fields from every returned dossier (RFC-0026 Phase 2A-0).
func LookupEntitiesRedacted(vaultDir, query string) ([]EntityDossier, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, fmt.Errorf("non-empty query is required in quarantined sessions")
	}
	all, err := ListEntities(vaultDir)
	if err != nil {
		return nil, err
	}
	var matches []EntityDossier
	for _, d := range all {
		if entityMatchesRedactedQuery(d, q) {
			d.Context = ""
			d.Notes = nil
			d.Spaces = nil
			d.Path = ""
			matches = append(matches, d)
		}
	}
	return matches, nil
}

func entityMatchesRedactedQuery(d EntityDossier, qLower string) bool {
	fields := []string{d.Slug, d.Name}
	fields = append(fields, d.Emails...)
	fields = append(fields, d.Handles...)
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), qLower) {
			return true
		}
	}
	slugQ := SanitizeEntitySlug(qLower)
	return slugQ != "" && strings.Contains(d.Slug, slugQ)
}

func entityMatchesQuery(d EntityDossier, qLower string) bool {
	fields := []string{d.Slug, d.Name, d.Kind, d.Context}
	fields = append(fields, d.Emails...)
	fields = append(fields, d.Handles...)
	fields = append(fields, d.Spaces...)
	fields = append(fields, d.Notes...)
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), qLower) {
			return true
		}
	}
	slugQ := SanitizeEntitySlug(qLower)
	return slugQ != "" && strings.Contains(d.Slug, slugQ)
}

// ForgetMemory executes a surgical, cascading deletion across facts.jsonl, MEMORY.md,
// memory/topics/{*.md,archived/*.md}, memory/entities/{*.md,archived/*.md}, notes/**/*.md,
// memory/progress/{*.md,archived/*.md}, hot & archived memory/journal/, memory/obs/index.json,
// memory/rsi/, and optionally local/private/pii-denylist.txt.
func ForgetMemory(vaultDir, query, topicSlug, entitySlug string, blockPII, dryRun bool) (*ForgetReport, error) {
	query = strings.TrimSpace(query)
	topicSlug = SanitizeEntitySlug(topicSlug)
	entitySlug = SanitizeEntitySlug(entitySlug)
	if query == "" && topicSlug == "" && entitySlug == "" {
		return nil, fmt.Errorf("at least one of --query, --topic, or --entity is required")
	}

	rep := &ForgetReport{
		Query:      query,
		TopicSlug:  topicSlug,
		EntitySlug: entitySlug,
		DryRun:     dryRun,
	}

	execute := func() error {
		return forgetMemoryInner(vaultDir, query, topicSlug, entitySlug, blockPII, dryRun, rep)
	}

	var err error
	if dryRun {
		err = execute()
	} else {
		err = withMemoryLock(vaultDir, execute)
	}
	if err != nil {
		return nil, err
	}

	// Commit audit snapshot inside private local/.git strictly OUTSIDE store.RankMemory.
	if !dryRun {
		rep.SnapshotCommitted = SnapshotVault(vaultDir, "hq memory forget")
	}

	return rep, nil
}

func forgetMemoryInner(vaultDir, query, topicSlug, entitySlug string, blockPII, dryRun bool, rep *ForgetReport) error {
	qLower := strings.ToLower(query)
	convSet := make(map[string]bool)
	recordConv := func(id string) {
		id = strings.TrimSpace(id)
		if id != "" && !convSet[id] {
			convSet[id] = true
			rep.SourceConvIDs = append(rep.SourceConvIDs, id)
		}
	}
	scrubbedSet := make(map[string]bool)
	recordScrubbed := func(p string) {
		rel, err := filepath.Rel(vaultDir, p)
		if err != nil {
			rel = p
		}
		rel = filepath.ToSlash(rel)
		if !scrubbedSet[rel] {
			scrubbedSet[rel] = true
			rep.ScrubbedFiles = append(rep.ScrubbedFiles, rel)
		}
	}

	// 1. Soft-expire & redact matching facts in local/memory/facts.jsonl
	active, expired, rawLines, err := LoadFactsState(vaultDir)
	if err != nil {
		return err
	}
	nowStr := time.Now().Format(time.RFC3339)
	factsMutated := false
	var nextActive []Fact
	for _, f := range active {
		matched := false
		if query != "" && (strings.EqualFold(f.ID, query) ||
			strings.Contains(strings.ToLower(f.Statement), qLower) ||
			strings.Contains(strings.ToLower(f.Topic), qLower)) {
			matched = true
		}
		if topicSlug != "" && (SanitizeEntitySlug(f.Topic) == topicSlug || strings.Contains(strings.ToLower(f.Statement), topicSlug)) {
			matched = true
		}
		if entitySlug != "" && (SanitizeEntitySlug(f.Topic) == entitySlug || strings.Contains(strings.ToLower(f.Statement), entitySlug)) {
			matched = true
		}
		if matched {
			rep.ExpiredFactIDs = append(rep.ExpiredFactIDs, f.ID)
			recordConv(f.SourceConv)
			f.ValidTo = &nowStr
			f.SupersededBy = "forgotten"
			f.Statement = "[FORGOTTEN BY USER]"
			expired = append(expired, f)
			factsMutated = true
		} else {
			nextActive = append(nextActive, f)
		}
	}
	if query != "" {
		for i := range expired {
			if strings.Contains(strings.ToLower(expired[i].Statement), qLower) && expired[i].Statement != "[FORGOTTEN BY USER]" {
				recordConv(expired[i].SourceConv)
				expired[i].Statement = "[FORGOTTEN BY USER]"
				factsMutated = true
			}
		}
	}
	if factsMutated && !dryRun && rawLines > 0 {
		all := make([]Fact, 0, len(nextActive)+len(expired))
		all = append(all, nextActive...)
		all = append(all, expired...)
		if err := writeCanonicalFactsLocked(vaultDir, all); err != nil {
			return err
		}
	}

	// 2. Scrub matching lines from local/MEMORY.md
	memPath := filepath.Join(vaultDir, "MEMORY.md")
	if isRegularNonSymlinkFile(memPath) {
		if memData, err := os.ReadFile(memPath); err == nil {
			lines := strings.Split(strings.ReplaceAll(string(memData), "\r\n", "\n"), "\n")
			var kept []string
			removed := 0
			for _, line := range lines {
				ll := strings.ToLower(line)
				drop := false
				if query != "" && strings.Contains(ll, qLower) {
					drop = true
				}
				if topicSlug != "" && strings.Contains(ll, topicSlug) {
					drop = true
				}
				if entitySlug != "" && strings.Contains(ll, entitySlug) {
					drop = true
				}
				if drop {
					removed++
					continue
				}
				kept = append(kept, line)
			}
			rep.MemoryLinesRemoved = removed
			if removed > 0 && !dryRun {
				_ = store.WriteAtomic(memPath, []byte(strings.Join(kept, "\n")), 0o600)
			}
		}
	}

	// 3. Delete targeted topic or entity markdown file (and archived variants) when requested by slug
	deletedAbs := make(map[string]bool)
	tryDelete := func(p string) {
		if !isRegularNonSymlinkFile(p) {
			return
		}
		rel, _ := filepath.Rel(vaultDir, p)
		rep.DeletedFiles = append(rep.DeletedFiles, filepath.ToSlash(rel))
		deletedAbs[p] = true
		if !dryRun {
			_ = os.Remove(p)
		}
	}
	if topicSlug != "" {
		tryDelete(filepath.Join(vaultDir, "memory", "topics", topicSlug+".md"))
		tryDelete(filepath.Join(vaultDir, "memory", "topics", "archived", topicSlug+".md"))
	}
	if entitySlug != "" {
		tryDelete(filepath.Join(vaultDir, "memory", "entities", entitySlug+".md"))
		tryDelete(filepath.Join(vaultDir, "memory", "entities", "archived", entitySlug+"-history.md"))
	}

	// 4. Scrub matching lines across markdown files: topics, entities, notes, progress, and rsi/rules
	if query != "" {
		mdRoots := []string{
			filepath.Join(vaultDir, "memory", "topics"),
			filepath.Join(vaultDir, "memory", "entities"),
			filepath.Join(vaultDir, "memory", "progress"),
			filepath.Join(vaultDir, "memory", "rsi", "rules"),
			filepath.Join(vaultDir, "notes"),
		}
		for _, root := range mdRoots {
			_ = filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return nil
				}
				if d.Type()&os.ModeSymlink != 0 {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if d.IsDir() || deletedAbs[p] {
					return nil
				}
				if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
					return nil
				}
				if !isRegularNonSymlinkFile(p) {
					return nil
				}
				raw, err := os.ReadFile(p)
				if err != nil {
					return nil
				}
				lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
				var kept []string
				changed := false
				for _, line := range lines {
					if strings.Contains(strings.ToLower(line), qLower) {
						changed = true
						continue
					}
					kept = append(kept, line)
				}
				if changed {
					recordScrubbed(p)
					if !dryRun {
						_ = store.WriteAtomic(p, []byte(strings.Join(kept, "\n")), 0o600)
					}
				}
				return nil
			})
		}

		// 5. Scrub hot memory/journal/YYYY-MM-DD/*.json and cold memory/journal/archive/*.jsonl
		journalRoot := filepath.Join(vaultDir, "memory", "journal")
		_ = filepath.WalkDir(journalRoot, func(p string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || !isRegularNonSymlinkFile(p) {
				return nil
			}
			lowerName := strings.ToLower(d.Name())
			switch {
			case strings.HasSuffix(lowerName, ".json"):
				changed, convIDs := scrubJSONObjectFile(p, qLower, dryRun)
				for _, cid := range convIDs {
					recordConv(cid)
				}
				if changed {
					recordScrubbed(p)
				}
			case strings.HasSuffix(lowerName, ".jsonl"):
				changed, convIDs := scrubJSONLFile(p, qLower, dryRun)
				for _, cid := range convIDs {
					recordConv(cid)
				}
				if changed {
					recordScrubbed(p)
				}
			}
			return nil
		})

		// 6. Scrub memory/obs/index.json
		obsIndexPath := filepath.Join(vaultDir, "memory", "obs", "index.json")
		if isRegularNonSymlinkFile(obsIndexPath) {
			if changed, convIDs := scrubJSONObjectFile(obsIndexPath, qLower, dryRun); changed {
				for _, cid := range convIDs {
					recordConv(cid)
				}
				recordScrubbed(obsIndexPath)
			}
		}

		// 7. Scrub memory/rsi/observations.jsonl, archive.jsonl, skill_baselines.json, and hypotheses/*.json
		for _, rsiJSONL := range []string{
			filepath.Join(vaultDir, "memory", "rsi", "observations.jsonl"),
			filepath.Join(vaultDir, "memory", "rsi", "archive.jsonl"),
		} {
			if isRegularNonSymlinkFile(rsiJSONL) {
				if changed, convIDs := scrubJSONLFile(rsiJSONL, qLower, dryRun); changed {
					for _, cid := range convIDs {
						recordConv(cid)
					}
					recordScrubbed(rsiJSONL)
				}
			}
		}
		rsiBaselinesPath := filepath.Join(vaultDir, "memory", "rsi", "skill_baselines.json")
		if isRegularNonSymlinkFile(rsiBaselinesPath) {
			if changed, convIDs := scrubJSONObjectFile(rsiBaselinesPath, qLower, dryRun); changed {
				for _, cid := range convIDs {
					recordConv(cid)
				}
				recordScrubbed(rsiBaselinesPath)
			}
		}
		rsiHypDir := filepath.Join(vaultDir, "memory", "rsi", "hypotheses")
		if entries, err := os.ReadDir(rsiHypDir); err == nil {
			for _, e := range entries {
				if e.IsDir() || e.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
					continue
				}
				hp := filepath.Join(rsiHypDir, e.Name())
				if !isRegularNonSymlinkFile(hp) {
					continue
				}
				if changed, convIDs := scrubJSONObjectFile(hp, qLower, dryRun); changed {
					for _, cid := range convIDs {
						recordConv(cid)
					}
					recordScrubbed(hp)
				}
			}
		}

		// 7b. Scrub matching lines from local/private/profile.md (Tier-2 household profile)
		profPath := ProfilePath(vaultDir)
		if isRegularNonSymlinkFile(profPath) {
			if raw, err := os.ReadFile(profPath); err == nil {
				lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
				var kept []string
				changed := false
				for _, line := range lines {
					if strings.Contains(strings.ToLower(line), qLower) {
						changed = true
						continue
					}
					kept = append(kept, line)
				}
				if changed {
					recordScrubbed(profPath)
					if !dryRun {
						_ = store.WriteAtomic(profPath, []byte(strings.Join(kept, "\n")), 0o600)
					}
				}
			}
		}
	}

	// 8. Optionally append query to local/private/pii-denylist.txt (--block-pii)
	if blockPII && query != "" {
		privDir := filepath.Join(vaultDir, "private")
		denyPath := filepath.Join(privDir, "pii-denylist.txt")
		existingLines, _ := os.ReadFile(denyPath)
		alreadyPresent := false
		for _, line := range strings.Split(string(existingLines), "\n") {
			if strings.EqualFold(strings.TrimSpace(line), query) {
				alreadyPresent = true
				break
			}
		}
		if !alreadyPresent {
			rep.DenylistUpdated = true
			if !dryRun {
				if err := os.MkdirAll(privDir, 0o700); err == nil {
					prefix := strings.TrimRight(string(existingLines), "\n")
					var nextContent string
					if strings.TrimSpace(prefix) == "" {
						nextContent = query + "\n"
					} else {
						nextContent = prefix + "\n" + query + "\n"
					}
					_ = store.WriteAtomic(denyPath, []byte(nextContent), 0o600)
				}
			}
		}
	}

	sort.Strings(rep.DeletedFiles)
	sort.Strings(rep.ScrubbedFiles)
	sort.Strings(rep.SourceConvIDs)
	return nil
}

// scrubJSONValue recursively replaces any string containing qLower with "[FORGOTTEN BY USER]"
// and extracts conversationId values from matched JSON objects.
func scrubJSONValue(v interface{}, qLower string) (interface{}, bool, []string) {
	switch val := v.(type) {
	case string:
		if strings.Contains(strings.ToLower(val), qLower) && val != "[FORGOTTEN BY USER]" {
			return "[FORGOTTEN BY USER]", true, nil
		}
		return val, false, nil
	case []interface{}:
		changed := false
		var convIDs []string
		for i, elem := range val {
			nv, ch, cids := scrubJSONValue(elem, qLower)
			if ch {
				val[i] = nv
				changed = true
			}
			convIDs = append(convIDs, cids...)
		}
		return val, changed, convIDs
	case map[string]interface{}:
		changed := false
		var convIDs []string
		for k, elem := range val {
			nv, ch, cids := scrubJSONValue(elem, qLower)
			if ch {
				val[k] = nv
				changed = true
			}
			convIDs = append(convIDs, cids...)
		}
		if changed {
			for _, idKey := range []string{"conversationId", "convId", "sourceConv"} {
				if cid, ok := val[idKey].(string); ok && strings.TrimSpace(cid) != "" && cid != "[FORGOTTEN BY USER]" {
					convIDs = append(convIDs, strings.TrimSpace(cid))
				}
			}
		}
		return val, changed, convIDs
	default:
		return v, false, nil
	}
}

func scrubJSONObjectFile(path, qLower string, dryRun bool) (bool, []string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, nil
	}
	if !strings.Contains(strings.ToLower(string(raw)), qLower) {
		return false, nil
	}
	var parsed interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false, nil
	}
	nv, changed, convIDs := scrubJSONValue(parsed, qLower)
	if !changed {
		return false, nil
	}
	if !dryRun {
		if out, err := json.MarshalIndent(nv, "", "  "); err == nil {
			_ = store.WriteAtomic(path, append(out, '\n'), 0o600)
		}
	}
	return true, convIDs
}

func scrubJSONLFile(path, qLower string, dryRun bool) (bool, []string) {
	f, err := os.Open(path)
	if err != nil {
		return false, nil
	}
	defer f.Close()

	var outLines []string
	changed := false
	var convIDs []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(strings.ToLower(line), qLower) {
			outLines = append(outLines, line)
			continue
		}
		var parsed interface{}
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			changed = true
			continue
		}
		nv, ch, cids := scrubJSONValue(parsed, qLower)
		if ch {
			changed = true
			convIDs = append(convIDs, cids...)
			if b, mErr := json.Marshal(nv); mErr == nil {
				outLines = append(outLines, string(b))
			}
		} else {
			outLines = append(outLines, line)
		}
	}
	if !changed {
		return false, nil
	}
	if !dryRun {
		content := strings.Join(outLines, "\n")
		if content != "" {
			content += "\n"
		}
		_ = store.WriteAtomic(path, []byte(content), 0o600)
	}
	return true, convIDs
}
