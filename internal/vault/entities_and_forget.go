package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

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

// ParseEntityDossierFile parses a local/memory/entities/<slug>.md markdown file into an EntityDossier.
func ParseEntityDossierFile(path string) (EntityDossier, error) {
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
	if info, statErr := os.Stat(path); statErr == nil {
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
		if strings.HasPrefix(line, ">") {
			meta := strings.TrimSpace(strings.TrimLeft(line, ">"))
			lower := strings.ToLower(meta)
			switch {
			case strings.HasPrefix(lower, "**kind:**"):
				d.Kind = strings.TrimSpace(meta[len("**kind:**"):])
			case strings.HasPrefix(lower, "**emails:**"):
				d.Emails = dedupCleanStrings(meta[len("**emails:**"):])
			case strings.HasPrefix(lower, "**handles:**"):
				d.Handles = dedupCleanStrings(meta[len("**handles:**"):])
			case strings.HasPrefix(lower, "**spaces:**"):
				d.Spaces = dedupCleanStrings(meta[len("**spaces:**"):])
			case strings.HasPrefix(lower, "**context:**"):
				d.Context = strings.TrimSpace(meta[len("**context:**"):])
			case strings.HasPrefix(lower, "**updated:**"):
				if u := strings.TrimSpace(meta[len("**updated:**"):]); u != "" {
					d.UpdatedAt = u
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

// UpsertEntity creates or merges an entity dossier in local/memory/entities/<slug>.md (0600 permissions).
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
	cur.UpdatedAt = time.Now().Format("2006-01-02")
	cur.Path = entPath

	if err := os.WriteFile(entPath, []byte(renderEntityDossierMarkdown(cur)), 0o600); err != nil {
		return EntityDossier{}, err
	}
	return cur, nil
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
	entries, err := os.ReadDir(entDir)
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
// memory/topics/*.md, memory/entities/*.md, notes/**/*.md, and optionally local/private/pii-denylist.txt.
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
	qLower := strings.ToLower(query)

	// 1. Soft-expire & redact matching facts in local/memory/facts.jsonl
	active, expired, rawLines, err := LoadFactsState(vaultDir)
	if err != nil {
		return nil, err
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
			f.ValidTo = &nowStr
			f.SupersededBy = "forgotten"
			f.Statement = "[FORGOTTEN BY USER]"
			expired = append(expired, f)
			factsMutated = true
		} else {
			nextActive = append(nextActive, f)
		}
	}
	// Also redact any already-expired fact statement that still contains the forgotten query string
	if query != "" {
		for i := range expired {
			if strings.Contains(strings.ToLower(expired[i].Statement), qLower) && expired[i].Statement != "[FORGOTTEN BY USER]" {
				expired[i].Statement = "[FORGOTTEN BY USER]"
				factsMutated = true
			}
		}
	}
	if factsMutated && !dryRun && rawLines > 0 {
		all := make([]Fact, 0, len(nextActive)+len(expired))
		all = append(all, nextActive...)
		all = append(all, expired...)
		if err := writeCanonicalFacts(vaultDir, all); err != nil {
			return nil, err
		}
	}

	// 2. Scrub matching lines from local/MEMORY.md
	memPath := filepath.Join(vaultDir, "MEMORY.md")
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
			_ = os.WriteFile(memPath, []byte(strings.Join(kept, "\n")), 0o600)
		}
	}

	// 3. Delete targeted topic or entity markdown file when requested by slug
	deletedAbs := make(map[string]bool)
	if topicSlug != "" {
		tpPath := filepath.Join(vaultDir, "memory", "topics", topicSlug+".md")
		if _, err := os.Stat(tpPath); err == nil {
			rel, _ := filepath.Rel(vaultDir, tpPath)
			rep.DeletedFiles = append(rep.DeletedFiles, filepath.ToSlash(rel))
			deletedAbs[tpPath] = true
			if !dryRun {
				_ = os.Remove(tpPath)
			}
		}
	}
	if entitySlug != "" {
		entPath := filepath.Join(vaultDir, "memory", "entities", entitySlug+".md")
		if _, err := os.Stat(entPath); err == nil {
			rel, _ := filepath.Rel(vaultDir, entPath)
			rep.DeletedFiles = append(rep.DeletedFiles, filepath.ToSlash(rel))
			deletedAbs[entPath] = true
			if !dryRun {
				_ = os.Remove(entPath)
			}
		}
	}

	// 4. Scrub matching lines across topics, entities, and notes when query != ""
	if query != "" {
		scanRoots := []string{
			filepath.Join(vaultDir, "memory", "topics"),
			filepath.Join(vaultDir, "memory", "entities"),
			filepath.Join(vaultDir, "notes"),
		}
		for _, root := range scanRoots {
			_ = filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
				if walkErr != nil || d.IsDir() || deletedAbs[p] {
					return nil
				}
				if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
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
					rel, _ := filepath.Rel(vaultDir, p)
					rep.ScrubbedFiles = append(rep.ScrubbedFiles, filepath.ToSlash(rel))
					if !dryRun {
						_ = os.WriteFile(p, []byte(strings.Join(kept, "\n")), 0o600)
					}
				}
				return nil
			})
		}
	}

	// 5. Optionally append query to local/private/pii-denylist.txt (--block-pii)
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
					_ = os.WriteFile(denyPath, []byte(nextContent), 0o600)
				}
			}
		}
	}

	// 6. Commit audit snapshot inside private local/.git
	if !dryRun {
		rep.SnapshotCommitted = SnapshotVault(vaultDir, "hq memory forget")
	}

	return rep, nil
}
