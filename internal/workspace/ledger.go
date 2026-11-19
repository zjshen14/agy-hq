package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/store"
)

const (
	// LedgerSchemaVersion is the current <vault>/memory/workspaces.json schema version.
	LedgerSchemaVersion = 1
	// MaxProbeCandidates bounds the deduplicated probe candidate queue (oldest entries are dropped first).
	MaxProbeCandidates = 200
)

// Ownership records whether hq created a workspace (and may therefore remove it) or merely attached to it.
type Ownership string

const (
	// OwnershipOwned marks a workspace created by hq (create with existed=false); eligible for removal.
	OwnershipOwned Ownership = "owned"
	// OwnershipAttached marks a pre-existing workspace bound to a task; hq only ever unbinds it.
	OwnershipAttached Ownership = "attached"
)

// LedgerStatus is the daemon-refreshed status cache for one ledger entry. Render paths read only this cache.
type LedgerStatus struct {
	State       State       `json:"state"`
	Base        string      `json:"base,omitempty"`
	Changes     []ChangeRef `json:"changes,omitempty"`
	RefreshedAt string      `json:"refreshed_at,omitempty"`
	Error       string      `json:"error,omitempty"`
}

// LedgerEntry records ownership, leases, bindings, and cached status for one provider workspace (RFC-0025 §7.1).
type LedgerEntry struct {
	Provider            string       `json:"provider"`
	WorkspaceID         string       `json:"workspace_id"`
	Name                string       `json:"name,omitempty"`
	Root                string       `json:"root"`
	WorkDir             string       `json:"work_dir,omitempty"`
	Ownership           Ownership    `json:"ownership"`
	CreatedByTask       string       `json:"created_by_task,omitempty"`
	CreatedAt           string       `json:"created_at,omitempty"`
	WriterTask          string       `json:"writer_task,omitempty"`
	LeaseEnforced       bool         `json:"lease_enforced,omitempty"`
	BoundTasks          []string     `json:"bound_tasks,omitempty"`
	Status              LedgerStatus `json:"status"`
	OrphanedOpenChanges bool         `json:"orphaned_open_changes,omitempty"`
	NotifiedLanded      []string     `json:"notified_landed,omitempty"`
}

// Key returns the entry's ledger key.
func (e LedgerEntry) Key() string {
	return LedgerKey(e.Provider, e.WorkspaceID)
}

// ProbeCandidate is a path recorded by the Stop hook for a later daemon `probe` (RFC-0025 §16.8).
type ProbeCandidate struct {
	Path       string `json:"path"`
	TaskSlug   string `json:"task_slug,omitempty"`
	RecordedAt string `json:"recorded_at"`
}

// Ledger is the decoded <vault>/memory/workspaces.json document.
type Ledger struct {
	SchemaVersion   int                    `json:"schema_version"`
	Entries         map[string]LedgerEntry `json:"entries"`
	ProbeCandidates []ProbeCandidate       `json:"probe_candidates,omitempty"`
}

// Put stores e under its ledger key.
func (l *Ledger) Put(e LedgerEntry) {
	if l.Entries == nil {
		l.Entries = make(map[string]LedgerEntry)
	}
	l.Entries[e.Key()] = e
}

// LedgerKey returns the canonical ledger key "<provider>:<workspaceID>". Provider FQIDs never contain ':',
// so ParseLedgerKey splits unambiguously at the first colon.
func LedgerKey(provider, workspaceID string) string {
	return provider + ":" + workspaceID
}

// ParseLedgerKey splits a ledger (or binding) key into provider FQID and workspace ID.
func ParseLedgerKey(key string) (provider, workspaceID string, ok bool) {
	idx := strings.Index(key, ":")
	if idx <= 0 || idx == len(key)-1 {
		return "", "", false
	}
	return key[:idx], key[idx+1:], true
}

func ledgerPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "workspaces.json")
}

func ledgerLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", ".workspaces.lock")
}

func newLedger() Ledger {
	return Ledger{SchemaVersion: LedgerSchemaVersion, Entries: make(map[string]LedgerEntry)}
}

// LoadLedger reads <vault>/memory/workspaces.json without locking (writes are atomic renames). A missing file
// yields an empty ledger; a symlinked, corrupt, or newer-schema file is an error.
func LoadLedger(vaultDir string) (Ledger, error) {
	if strings.TrimSpace(vaultDir) == "" {
		return Ledger{}, fmt.Errorf("workspace ledger: vaultDir is required")
	}
	p := ledgerPath(vaultDir)
	st, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return newLedger(), nil
		}
		return Ledger{}, err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return Ledger{}, fmt.Errorf("workspace ledger: refusing symlinked or non-regular %s", p)
	}
	f, err := store.OpenReadNoFollow(p)
	if err != nil {
		return Ledger{}, err
	}
	defer f.Close()
	var l Ledger
	dec := json.NewDecoder(f)
	if err := dec.Decode(&l); err != nil {
		return Ledger{}, fmt.Errorf("workspace ledger: corrupt %s: %w", p, err)
	}
	if l.SchemaVersion > LedgerSchemaVersion {
		return Ledger{}, fmt.Errorf("workspace ledger: %s has schema_version %d (this hq understands <= %d)", p, l.SchemaVersion, LedgerSchemaVersion)
	}
	l.normalize()
	return l, nil
}

// UpdateLedger runs fn under the RankWorkspaces lock on a freshly loaded ledger and atomically rewrites
// <vault>/memory/workspaces.json (mode 0600) when fn reports a change and returns no error.
//
// fn must be pure: no provider subprocesses, network calls, or other store locks. Callers snapshot, call the
// provider outside the lock, then re-enter UpdateLedger to apply the result (compare-and-set on refreshed_at).
// A task update (RankTasks) may nest a ledger update, never the reverse.
func UpdateLedger(vaultDir string, fn func(*Ledger) (changed bool, err error)) error {
	if strings.TrimSpace(vaultDir) == "" {
		return fmt.Errorf("workspace ledger: vaultDir is required")
	}
	if fn == nil {
		return fmt.Errorf("workspace ledger: update func is required")
	}
	return store.With(context.Background(), store.RankWorkspaces, ledgerLockPath(vaultDir), func(context.Context) error {
		l, err := LoadLedger(vaultDir)
		if err != nil {
			return err
		}
		changed, err := fn(&l)
		if err != nil || !changed {
			return err
		}
		l.normalize()
		if err := l.validate(); err != nil {
			return err
		}
		data, err := json.MarshalIndent(l, "", "  ")
		if err != nil {
			return err
		}
		return store.WriteAtomic(ledgerPath(vaultDir), append(data, '\n'), 0o600)
	})
}

// GetEntry returns the ledger entry stored under key.
func GetEntry(vaultDir, key string) (LedgerEntry, bool) {
	l, err := LoadLedger(vaultDir)
	if err != nil {
		return LedgerEntry{}, false
	}
	e, ok := l.Entries[key]
	return e, ok
}

// FindEntryForPath returns the ledger entry whose root is the longest segment-aware prefix of absPath
// ("/a/b" never matches "/a/bc"). It tries the cleaned path first (no filesystem access) and then the
// symlink-resolved path. It never spawns a subprocess.
func FindEntryForPath(vaultDir, absPath string) (LedgerEntry, bool) {
	l, err := LoadLedger(vaultDir)
	if err != nil || len(l.Entries) == 0 {
		return LedgerEntry{}, false
	}
	return l.findEntryForPath(absPath)
}

func (l Ledger) findEntryForPath(absPath string) (LedgerEntry, bool) {
	clean := cleanAbsPath(absPath)
	if clean == "" {
		return LedgerEntry{}, false
	}
	candidates := []string{clean}
	if canon := CanonicalPath(clean); canon != "" && canon != clean {
		candidates = append(candidates, canon)
	}
	return l.findEntryForCandidates(candidates)
}

// findEntryForCandidates performs the pure (no filesystem access) longest-root match for pre-resolved path forms.
func (l Ledger) findEntryForCandidates(candidates []string) (LedgerEntry, bool) {
	for _, p := range candidates {
		if p == "" {
			continue
		}
		var best LedgerEntry
		bestLen := -1
		for _, key := range l.sortedKeys() {
			e := l.Entries[key]
			root := filepath.Clean(e.Root)
			if !filepath.IsAbs(root) || root == string(filepath.Separator) {
				continue
			}
			if pathWithin(p, root) && len(root) > bestLen {
				best, bestLen = e, len(root)
			}
		}
		if bestLen >= 0 {
			return best, true
		}
	}
	return LedgerEntry{}, false
}

// RecordProbeCandidate queues path (normally a matched workspace root) for a later daemon probe. It is a
// no-op when the path is already queued for the same task or already covered by a ledger entry. The queue is
// deduplicated by path and bounded to MaxProbeCandidates (oldest dropped).
func RecordProbeCandidate(vaultDir, path, taskSlug string) error {
	raw := strings.TrimSpace(path)
	p := cleanAbsPath(raw)
	if raw == "" || !filepath.IsAbs(raw) || hasControlOrFormatRune(p) || len(p) > MaxPathLength {
		return fmt.Errorf("workspace ledger: invalid probe candidate path %q", clipForError(path))
	}
	slug := strings.TrimSpace(taskSlug)
	if len(slug) > 128 || hasControlOrFormatRune(slug) {
		slug = ""
	}
	// Resolve symlinks before taking the lock so the update callback stays free of filesystem access.
	candidates := []string{p}
	if canon := CanonicalPath(p); canon != "" && canon != p {
		candidates = append(candidates, canon)
	}
	nowStr := time.Now().UTC().Format(time.RFC3339)
	return UpdateLedger(vaultDir, func(l *Ledger) (bool, error) {
		if _, covered := l.findEntryForCandidates(candidates); covered {
			return false, nil
		}
		for i, c := range l.ProbeCandidates {
			if c.Path != p {
				continue
			}
			if c.TaskSlug == slug || slug == "" {
				return false, nil
			}
			l.ProbeCandidates = append(l.ProbeCandidates[:i], l.ProbeCandidates[i+1:]...)
			break
		}
		l.ProbeCandidates = append(l.ProbeCandidates, ProbeCandidate{Path: p, TaskSlug: slug, RecordedAt: nowStr})
		return true, nil
	})
}

// DrainProbeCandidates removes and returns up to max of the oldest queued probe candidates (all when max <= 0).
func DrainProbeCandidates(vaultDir string, max int) ([]ProbeCandidate, error) {
	var drained []ProbeCandidate
	err := UpdateLedger(vaultDir, func(l *Ledger) (bool, error) {
		if len(l.ProbeCandidates) == 0 {
			return false, nil
		}
		n := len(l.ProbeCandidates)
		if max > 0 && max < n {
			n = max
		}
		drained = append([]ProbeCandidate(nil), l.ProbeCandidates[:n]...)
		l.ProbeCandidates = append([]ProbeCandidate(nil), l.ProbeCandidates[n:]...)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return drained, nil
}

func (l Ledger) sortedKeys() []string {
	keys := make([]string, 0, len(l.Entries))
	for k := range l.Entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// normalize fills defaults and enforces the probe candidate bounds.
func (l *Ledger) normalize() {
	if l.SchemaVersion == 0 {
		l.SchemaVersion = LedgerSchemaVersion
	}
	if l.Entries == nil {
		l.Entries = make(map[string]LedgerEntry)
	}
	for k, e := range l.Entries {
		if e.Status.State == "" {
			e.Status.State = StateUnknown
			l.Entries[k] = e
		}
	}
	if len(l.ProbeCandidates) > 0 {
		seen := make(map[string]int, len(l.ProbeCandidates))
		var deduped []ProbeCandidate
		for _, c := range l.ProbeCandidates {
			if idx, ok := seen[c.Path]; ok {
				deduped[idx] = c
				continue
			}
			seen[c.Path] = len(deduped)
			deduped = append(deduped, c)
		}
		if len(deduped) > MaxProbeCandidates {
			deduped = deduped[len(deduped)-MaxProbeCandidates:]
		}
		l.ProbeCandidates = deduped
	} else {
		l.ProbeCandidates = nil
	}
}

// validate rejects ledger states that would break the ownership invariants before they are persisted.
func (l Ledger) validate() error {
	for key, e := range l.Entries {
		if strings.TrimSpace(e.Provider) == "" || strings.Contains(e.Provider, ":") || strings.TrimSpace(e.WorkspaceID) == "" {
			return fmt.Errorf("workspace ledger: entry %q needs a provider FQID and workspace_id", key)
		}
		if key != e.Key() {
			return fmt.Errorf("workspace ledger: entry key %q does not match %q", key, e.Key())
		}
		root := e.Root
		if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) || hasControlOrFormatRune(root) {
			return fmt.Errorf("workspace ledger: entry %q has invalid root %q", key, clipForError(root))
		}
		if e.WorkDir != "" && (!filepath.IsAbs(e.WorkDir) || filepath.Clean(e.WorkDir) != e.WorkDir || !pathWithin(e.WorkDir, root)) {
			return fmt.Errorf("workspace ledger: entry %q has work_dir outside its root", key)
		}
		if e.Ownership != OwnershipOwned && e.Ownership != OwnershipAttached {
			return fmt.Errorf("workspace ledger: entry %q has invalid ownership %q", key, e.Ownership)
		}
		if !e.Status.State.Valid() {
			return fmt.Errorf("workspace ledger: entry %q has invalid status state %q", key, e.Status.State)
		}
		for _, c := range e.Status.Changes {
			if !c.State.Valid() {
				return fmt.Errorf("workspace ledger: entry %q change %q has invalid state %q", key, c.ID, c.State)
			}
		}
	}
	for _, c := range l.ProbeCandidates {
		if !filepath.IsAbs(c.Path) {
			return fmt.Errorf("workspace ledger: probe candidate %q is not absolute", clipForError(c.Path))
		}
	}
	return nil
}
