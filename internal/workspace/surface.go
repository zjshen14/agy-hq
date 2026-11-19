package workspace

import (
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// This file holds the read-only helpers shared by the RFC-0025 surfaces (Phase C): PreToolUse hygiene (W1/W3),
// Stop auto-binding, SessionStart, CONSOLE.md, and the :4242 WebUI. Every function here reads the ledger cache
// and the trust-verified pack manifests only; none of them calls a provider or spawns a subprocess.

// reSurfaceFQID matches well-formed provider FQIDs ("core/git-worktree", "<pack>/<id>").
var reSurfaceFQID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}/[a-z0-9][a-z0-9._-]{0,63}$`)

// SurfaceChange is a display-safe change reference, re-sanitized at render time. Titles and URLs may appear on
// CONSOLE.md / hq_console.md and the WebUI only; SessionStart renders IDs, states, and counts (RFC-0025 §11.3).
type SurfaceChange struct {
	ID         string      `json:"id"`
	State      ChangeState `json:"state"`
	NeedsOwner bool        `json:"needsOwner,omitempty"`
	Title      string      `json:"title,omitempty"`
	URL        string      `json:"url,omitempty"` // https only; rendered as a link, never fetched by hq
	UpdatedAt  string      `json:"updatedAt,omitempty"`
}

// SurfaceEntry is the render-ready, display-safe projection of one ledger entry shared by every surface.
type SurfaceEntry struct {
	Key           string          `json:"key"`
	Provider      string          `json:"provider"`
	ProviderShort string          `json:"providerShort"`
	WorkspaceID   string          `json:"workspaceId"`
	Name          string          `json:"name,omitempty"`
	Root          string          `json:"root"`
	WorkDir       string          `json:"workDir,omitempty"`
	Ownership     Ownership       `json:"ownership"`
	CreatedByTask string          `json:"createdByTask,omitempty"`
	WriterTask    string          `json:"writerTask,omitempty"`
	LeaseEnforced bool            `json:"leaseEnforced,omitempty"`
	BoundTasks    []string        `json:"boundTasks,omitempty"`
	State         State           `json:"state"`
	RefreshedAt   string          `json:"refreshedAt,omitempty"`
	RefreshError  bool            `json:"refreshError,omitempty"` // the last daemon refresh failed (error text withheld)
	Orphaned      bool            `json:"orphanedOpenChanges,omitempty"`
	Changes       []SurfaceChange `json:"changes,omitempty"`
	OpenChanges   int             `json:"openChanges"`
	NeedsOwner    int             `json:"needsOwner"` // open changes waiting on the workspace owner
	Summary       string          `json:"summary,omitempty"`
}

// NewSurfaceEntry projects e into a SurfaceEntry. Display text is scrubbed and clamped, malformed change IDs or
// states are dropped, URLs that are not short https links are dropped, and the refresh error text is reduced to
// a flag so provider-controlled free text never reaches a surface.
func NewSurfaceEntry(e LedgerEntry) SurfaceEntry {
	s := SurfaceEntry{
		Key:           sanitizeDisplayText(e.Key(), 200),
		Provider:      sanitizeDisplayText(e.Provider, 128),
		ProviderShort: ProviderShort(e.Provider),
		WorkspaceID:   sanitizeDisplayText(e.WorkspaceID, 128),
		Name:          sanitizeDisplayText(e.Name, 128),
		Root:          sanitizeDisplayText(e.Root, 512),
		WorkDir:       sanitizeDisplayText(e.WorkDir, 512),
		Ownership:     e.Ownership,
		CreatedByTask: sanitizeDisplayText(e.CreatedByTask, 128),
		WriterTask:    sanitizeDisplayText(e.WriterTask, 128),
		LeaseEnforced: e.LeaseEnforced,
		State:         e.Status.State,
		RefreshedAt:   sanitizeTimestamp(e.Status.RefreshedAt),
		RefreshError:  strings.TrimSpace(e.Status.Error) != "",
		Orphaned:      e.OrphanedOpenChanges,
	}
	if s.Name == s.WorkspaceID {
		s.Name = ""
	}
	if s.Ownership != OwnershipOwned && s.Ownership != OwnershipAttached {
		s.Ownership = OwnershipAttached
	}
	if !s.State.Valid() {
		s.State = StateUnknown
	}
	seen := make(map[string]bool, len(e.BoundTasks))
	for _, t := range e.BoundTasks {
		if slug := sanitizeDisplayText(t, 128); slug != "" && !seen[slug] {
			seen[slug] = true
			s.BoundTasks = append(s.BoundTasks, slug)
		}
	}
	changes := e.Status.Changes
	if len(changes) > MaxChanges {
		changes = changes[:MaxChanges]
	}
	for _, c := range changes {
		if !reChangeID.MatchString(c.ID) || !c.State.Valid() {
			continue
		}
		sc := SurfaceChange{
			ID:         c.ID,
			State:      c.State,
			NeedsOwner: c.NeedsOwner,
			Title:      sanitizeDisplayText(c.Title, MaxTitleRunes),
			URL:        sanitizeChangeURL(c.URL),
			UpdatedAt:  sanitizeTimestamp(c.UpdatedAt),
		}
		if sc.State.Open() {
			s.OpenChanges++
			if sc.NeedsOwner {
				s.NeedsOwner++
			}
		}
		s.Changes = append(s.Changes, sc)
	}
	s.Summary = SummarizeChanges(s.Changes)
	return s
}

// SurfaceEntries projects every ledger entry, sorted by provider FQID and then workspace ID.
func SurfaceEntries(l Ledger) []SurfaceEntry {
	if len(l.Entries) == 0 {
		return nil
	}
	out := make([]SurfaceEntry, 0, len(l.Entries))
	for _, key := range l.sortedKeys() {
		out = append(out, NewSurfaceEntry(l.Entries[key]))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].WorkspaceID < out[j].WorkspaceID
	})
	return out
}

// LoadSurfaceEntries reads the ledger cache and projects it for rendering. A missing ledger yields no entries.
func LoadSurfaceEntries(vaultDir string) ([]SurfaceEntry, error) {
	l, err := LoadLedger(vaultDir)
	if err != nil {
		return nil, err
	}
	return SurfaceEntries(l), nil
}

// DisplayName returns the compact workspace name for human-facing tables and cards (Name when distinct from
// WorkspaceID, such as a git worktree basename, otherwise WorkspaceID).
func (s SurfaceEntry) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	return s.WorkspaceID
}

// Owners returns the tasks associated with the entry: the writer first, then bound tasks, then the creator.
func (s SurfaceEntry) Owners() []string {
	var out []string
	seen := make(map[string]bool)
	add := func(slug string) {
		if slug != "" && !seen[slug] {
			seen[slug] = true
			out = append(out, slug)
		}
	}
	add(s.WriterTask)
	for _, t := range s.BoundTasks {
		add(t)
	}
	add(s.CreatedByTask)
	return out
}

// StateCounts returns "<n> <state>" fragments for the entry's changes in lifecycle order (abandoned omitted).
func (s SurfaceEntry) StateCounts() []string {
	return changeStateCounts(s.Changes)
}

// changeSummaryOrder lists the change states summarized on surfaces, in lifecycle order. Abandoned changes are
// omitted because they never block cleanup and carry no actionable information.
var changeSummaryOrder = []struct {
	state ChangeState
	label string
}{
	{ChangeLocal, "local"},
	{ChangeDraft, "draft"},
	{ChangeInReview, "in review"},
	{ChangeApproved, "approved"},
	{ChangeLanded, "landed"},
}

func changeStateCounts(changes []SurfaceChange) []string {
	counts := make(map[ChangeState]int, len(changes))
	for _, c := range changes {
		counts[c.State]++
	}
	var parts []string
	for _, o := range changeSummaryOrder {
		if n := counts[o.state]; n > 0 {
			parts = append(parts, strconv.Itoa(n)+" "+o.label)
		}
	}
	return parts
}

// SummarizeChanges renders a compact change summary such as "1 in review · 1 ⏳ needs you" (empty when there
// is nothing to report). Only open changes count toward "needs you".
func SummarizeChanges(changes []SurfaceChange) string {
	parts := changeStateCounts(changes)
	needs := 0
	for _, c := range changes {
		if c.NeedsOwner && c.State.Open() {
			needs++
		}
	}
	if needs > 0 {
		parts = append(parts, strconv.Itoa(needs)+" ⏳ needs you")
	}
	return strings.Join(parts, " · ")
}

// ProviderShort returns a compact badge label for a provider FQID: "git" for core/git-worktree, the provider
// name for other built-ins, and the pack ID for pack providers (e.g. "acme-monorepo/client" -> "acme-monorepo").
func ProviderShort(fqid string) string {
	fqid = strings.TrimSpace(fqid)
	if fqid == CoreGitProviderID {
		return "git"
	}
	if !reSurfaceFQID.MatchString(fqid) {
		return sanitizeDisplayText(fqid, 32)
	}
	pack, id, _ := strings.Cut(fqid, "/")
	if pack == "core" {
		return id
	}
	return pack
}

// SafeInlinePath returns p when it can be shown verbatim inside an inline code span on an LLM-visible surface:
// absolute, Clean-stable, at most 512 bytes, and free of backticks and control or invisible formatting
// characters. Otherwise it returns "" so callers withhold the path rather than render a mangled one.
func SafeInlinePath(p string) string {
	if p == "" || len(p) > 512 || !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, "`|") || hasControlOrFormatRune(p) {
		return ""
	}
	return p
}

// FindOwningEntry returns the ledger entry that owns absPath for PreToolUse hygiene and Stop auto-binding.
// It matches against each entry's stored Root and persisted CanonicalRoot purely in memory (never statting
// unrelated ledger roots on the hook hot path), and never treats a path inside the Vault, or an entry whose
// root contains the Vault, as provider-owned. It reads only the ledger file and never spawns a subprocess.
func FindOwningEntry(vaultDir, absPath string) (LedgerEntry, bool) {
	e, ok, err := FindOwningEntryChecked(vaultDir, absPath)
	if err != nil {
		return LedgerEntry{}, false
	}
	return e, ok
}

// FindOwningEntryChecked is FindOwningEntry that also surfaces ledger read/parse/schema errors so callers
// such as PreToolUse W1 can fail closed when <vault>/memory/workspaces.json exists but is unreadable,
// symlinked, corrupt, or from a newer schema version. A missing ledger returns (zero, false, nil).
func FindOwningEntryChecked(vaultDir, absPath string) (LedgerEntry, bool, error) {
	l, err := LoadLedger(vaultDir)
	if err != nil {
		// Vault paths are never provider-owned even when the workspace ledger is unreadable.
		raw := strings.TrimSpace(absPath)
		if raw != "" && filepath.IsAbs(raw) {
			vRoots := vaultRootsFor(vaultDir)
			clean := filepath.Clean(raw)
			if withinAny(clean, vRoots) {
				return LedgerEntry{}, false, nil
			}
			if canon := CanonicalPath(clean); canon != "" && withinAny(canon, vRoots) {
				return LedgerEntry{}, false, nil
			}
		}
		return LedgerEntry{}, false, err
	}
	if len(l.Entries) == 0 {
		return LedgerEntry{}, false, nil
	}
	e, ok := l.findOwningEntry(vaultRootsFor(vaultDir), absPath)
	return e, ok, nil
}

func vaultRootsFor(vaultDir string) []string {
	if strings.TrimSpace(vaultDir) == "" {
		return nil
	}
	var roots []string
	for _, v := range []string{cleanAbsPath(vaultDir), CanonicalPath(vaultDir)} {
		if v == "" || v == string(filepath.Separator) {
			continue
		}
		dup := false
		for _, r := range roots {
			if r == v {
				dup = true
				break
			}
		}
		if !dup {
			roots = append(roots, v)
		}
	}
	return roots
}

func withinAny(p string, roots []string) bool {
	for _, r := range roots {
		if pathWithin(p, r) {
			return true
		}
	}
	return false
}

func (l Ledger) findOwningEntry(vaultRoots []string, absPath string) (LedgerEntry, bool) {
	raw := strings.TrimSpace(absPath)
	if raw == "" || !filepath.IsAbs(raw) {
		return LedgerEntry{}, false
	}
	clean := filepath.Clean(raw)
	candidates := []string{clean}
	if canon := CanonicalPath(clean); canon != "" && canon != clean {
		candidates = append(candidates, canon)
	}
	for _, p := range candidates {
		if withinAny(p, vaultRoots) {
			return LedgerEntry{}, false
		}
	}
	eligible := Ledger{Entries: make(map[string]LedgerEntry, len(l.Entries))}
	for key, e := range l.Entries {
		root := filepath.Clean(e.Root)
		if !filepath.IsAbs(root) || root == string(filepath.Separator) || rootContainsAny(root, vaultRoots) {
			continue
		}
		if e.CanonicalRoot != "" {
			canonRoot := filepath.Clean(e.CanonicalRoot)
			if !filepath.IsAbs(canonRoot) || canonRoot == string(filepath.Separator) || rootContainsAny(canonRoot, vaultRoots) {
				e.CanonicalRoot = ""
			}
		}
		eligible.Entries[key] = e
	}
	return eligible.findEntryForCandidates(candidates)
}

// PathOwner reports which workspace owns a modified path (RFC-0025 §7.3 / §16.8 Stop auto-binding).
type PathOwner struct {
	Path     string      // the first examined path that resolved to Root
	Provider string      // provider FQID
	Root     string      // ledger entry root, or the workspace root matched by root_patterns
	InLedger bool        // true when Entry is a ledger hit; false for a root_patterns-only match
	Entry    LedgerEntry // valid when InLedger
}

// ResolvePathOwners resolves absolute paths (in order) to their owning workspaces, deduplicated by root: a
// ledger hit (FindOwningEntry semantics) wins, otherwise the trusted root_patterns are consulted
// (MatchRootPattern). It performs at most maxPaths lookups (maxPaths <= 0 means no cap); relative paths, paths
// inside the Vault, and paths under an already-resolved root are skipped without counting toward the cap. It
// reads the ledger once and never spawns a subprocess.
func ResolvePathOwners(vaultDir string, paths []string, maxPaths int) []PathOwner {
	if strings.TrimSpace(vaultDir) == "" || len(paths) == 0 {
		return nil
	}
	l, err := LoadLedger(vaultDir)
	if err != nil {
		return nil
	}
	hasPatterns := false
	for _, ep := range loadExternalProviders(vaultDir) {
		if len(ep.spec.RootPatterns) > 0 {
			hasPatterns = true
			break
		}
	}
	if len(l.Entries) == 0 && !hasPatterns {
		return nil
	}
	vaultRoots := vaultRootsFor(vaultDir)
	var out []PathOwner
	hasRoot := func(root string) bool {
		for _, o := range out {
			if o.Root == root {
				return true
			}
		}
		return false
	}
	covered := func(p string) bool {
		for _, o := range out {
			if pathWithin(p, filepath.Clean(o.Root)) {
				return true
			}
		}
		return false
	}
	seen := make(map[string]bool)
	lookups := 0
	for _, raw := range paths {
		if maxPaths > 0 && lookups >= maxPaths {
			break
		}
		p := strings.TrimSpace(raw)
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		if covered(p) {
			continue
		}
		if withinAny(p, vaultRoots) || withinAny(CanonicalPath(p), vaultRoots) {
			continue
		}
		lookups++
		if e, ok := l.findOwningEntry(vaultRoots, p); ok {
			if !hasRoot(e.Root) {
				out = append(out, PathOwner{Path: p, Provider: e.Provider, Root: e.Root, InLedger: true, Entry: e})
			}
			continue
		}
		if !hasPatterns {
			continue
		}
		if fqid, root, ok := MatchRootPattern(vaultDir, p); ok && !hasRoot(root) {
			out = append(out, PathOwner{Path: p, Provider: fqid, Root: root})
		}
	}
	return out
}
