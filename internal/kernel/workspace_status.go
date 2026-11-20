package kernel

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

const (
	// maxNotifiedLanded bounds LedgerEntry.NotifiedLanded (oldest change IDs are dropped first).
	maxNotifiedLanded = 64
	// workspaceUncachedMaxAge is the cache freshness bound used when a provider disables daemon refresh.
	workspaceUncachedMaxAge = time.Hour
	// WorkspaceLandedSourceSkill attributes landed-change outbox items.
	WorkspaceLandedSourceSkill = "workspace-lifecycle"
)

// ---------------------------------------------------------------------------------------------------------------
// Status refresh (compare-and-set) and landed notifications

// landedNotice is one (ledger key, change id) transition to landed, emitted once as a brief_rollup outbox item.
type landedNotice struct {
	Key       string
	ChangeID  string
	Task      string
	Owned     bool
	OpenCount int
}

// applyWorkspaceInfo writes a fresh provider status into e's cache and returns the change IDs that transitioned
// to landed since the previous successful refresh and were not notified before (recorded in notified_landed).
// A provider reporting a different root than the ledger is treated as an error: the cache keeps its last state.
func applyWorkspaceInfo(e *workspace.LedgerEntry, info workspace.Info, nowStr string) []string {
	if filepath.Clean(info.Root) != filepath.Clean(e.Root) {
		e.Status.Error = workspace.SanitizeDisplayText(fmt.Sprintf("provider reported root %s but the ledger has %s", info.Root, e.Root), 200)
		e.Status.RefreshedAt = nowStr
		return nil
	}
	prev := e.Status
	e.Status = ledgerStatusFromInfo(info, nowStr)
	if e.OrphanedOpenChanges && workspace.CountOpenChanges(e.Status.Changes) == 0 {
		e.OrphanedOpenChanges = false // every change kept it alive has landed or been abandoned
	}
	if prev.RefreshedAt == "" {
		return nil // first observation is the baseline, not a transition
	}
	prevState := make(map[string]workspace.ChangeState, len(prev.Changes))
	for _, c := range prev.Changes {
		prevState[c.ID] = c.State
	}
	var landed []string
	for _, c := range e.Status.Changes {
		if c.State != workspace.ChangeLanded || prevState[c.ID] == workspace.ChangeLanded || slices.Contains(e.NotifiedLanded, c.ID) {
			continue
		}
		e.NotifiedLanded = append(e.NotifiedLanded, c.ID)
		landed = append(landed, c.ID)
	}
	if n := len(e.NotifiedLanded); n > maxNotifiedLanded {
		e.NotifiedLanded = append([]string(nil), e.NotifiedLanded[n-maxNotifiedLanded:]...)
	}
	return landed
}

func buildLandedNotices(e workspace.LedgerEntry, ids []string) []landedNotice {
	if len(ids) == 0 {
		return nil
	}
	task := e.WriterTask
	if task == "" && len(e.BoundTasks) > 0 {
		task = e.BoundTasks[0]
	}
	if task == "" {
		task = e.CreatedByTask
	}
	out := make([]landedNotice, 0, len(ids))
	for _, id := range ids {
		out = append(out, landedNotice{
			Key:       e.Key(),
			ChangeID:  id,
			Task:      task,
			Owned:     e.Ownership == workspace.OwnershipOwned,
			OpenCount: workspace.CountOpenChanges(e.Status.Changes),
		})
	}
	return out
}

// emitLandedNotices adds one brief_rollup outbox item per notice. Only opaque IDs, enums, and counts are included
// (never provider-authored titles), because brief items reach LLM context. Delivery is at-most-once: the
// notified_landed mark was committed under the ledger lock before this runs.
func emitLandedNotices(vaultDir string, notices []landedNotice, now time.Time) int {
	sent := 0
	for _, n := range notices {
		scope := n.Task
		if scope == "" {
			scope = "workspaces"
		}
		summary := fmt.Sprintf("Change %s in workspace %s landed.", n.ChangeID, n.Key)
		switch {
		case n.OpenCount == 0 && n.Owned:
			summary += fmt.Sprintf(" No open changes remain; the workspace is eligible for cleanup (archive its task, or `hq workspace remove %s`).", n.Key)
		case n.OpenCount == 0:
			summary += " No open changes remain (attached workspace: hq only unbinds it)."
		default:
			summary += fmt.Sprintf(" %d change(s) are still open.", n.OpenCount)
		}
		if _, _, err := outbox.Add(vaultDir, outbox.OutboxItem{
			SourceSkill:   WorkspaceLandedSourceSkill,
			Scope:         scope,
			Tier:          outbox.TierBriefRollup,
			Title:         fmt.Sprintf("Workspace change landed: %s %s", n.Key, n.ChangeID),
			Summary:       summary,
			SuggestedSlug: n.Task,
		}, now); err == nil {
			sent++
		}
	}
	return sent
}

// fetchWorkspaceStatus resolves the entry's provider and asks it for a fresh status (outside every lock).
func fetchWorkspaceStatus(vaultDir string, e workspace.LedgerEntry) (workspace.Info, error) {
	info, _, err := fetchWorkspaceStatusContext(context.Background(), vaultDir, e)
	return info, err
}

// fetchWorkspaceStatusContext resolves the entry's provider, queries its status bounded by parent ctx,
// and verifies the reported root matches the ledger entry (failing closed on root mismatch).
func fetchWorkspaceStatusContext(parent context.Context, vaultDir string, e workspace.LedgerEntry) (workspace.Info, bool, error) {
	p, err := workspace.Resolve(vaultDir, e.Provider)
	if err != nil {
		return workspace.Info{}, false, err
	}
	tracksChanges := p.Capabilities().Changes
	ctx, cancel := workspaceProviderContextFrom(parent)
	defer cancel()
	info, err := p.Status(ctx, e.WorkspaceID)
	if err != nil {
		return workspace.Info{}, tracksChanges, err
	}
	info.Provider = e.Provider
	if info.State != workspace.StateMissing && filepath.Clean(info.Root) != filepath.Clean(e.Root) {
		return workspace.Info{}, tracksChanges, wsConflict("provider reported root mismatch for %s", e.Key())
	}
	return info, tracksChanges, nil
}

// applyWorkspaceStatusCAS applies a provider status result (or error) to the cache of entry key only when
// status.refreshed_at still equals expectRefreshedAt (a concurrent refresh wins otherwise). A not_found error
// records the workspace as missing. Landed transitions are emitted after the ledger lock is released. It returns
// the entry as stored after the attempt and whether this result was applied.
func applyWorkspaceStatusCAS(vaultDir, key, expectRefreshedAt string, info workspace.Info, callErr error, now time.Time) (workspace.LedgerEntry, bool, error) {
	e, applied, _, err := applyWorkspaceStatusCASCounted(vaultDir, key, expectRefreshedAt, info, callErr, now)
	return e, applied, err
}

// applyWorkspaceStatusCASCounted is applyWorkspaceStatusCAS that also reports how many landed notices it emitted.
func applyWorkspaceStatusCASCounted(vaultDir, key, expectRefreshedAt string, info workspace.Info, callErr error, now time.Time) (workspace.LedgerEntry, bool, int, error) {
	nowStr := now.UTC().Format(time.RFC3339)
	var out workspace.LedgerEntry
	var notices []landedNotice
	applied := false
	err := workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
		out, notices, applied = workspace.LedgerEntry{}, nil, false
		e, ok := l.Entries[key]
		if !ok {
			return false, nil
		}
		out = cloneLedgerEntry(e)
		if e.Status.RefreshedAt != expectRefreshedAt {
			return false, nil
		}
		switch {
		case callErr != nil && workspace.ErrorCode(callErr) == workspace.CodeNotFound:
			e.Status = workspace.LedgerStatus{State: workspace.StateMissing, RefreshedAt: nowStr}
		case callErr != nil:
			e.Status.Error = WorkspaceSafeErrorSummary(callErr)
			if e.Status.Error == "" {
				e.Status.Error = "status refresh failed"
			}
			e.Status.RefreshedAt = nowStr
		default:
			notices = buildLandedNotices(e, applyWorkspaceInfo(&e, info, nowStr))
		}
		l.Put(e)
		out = cloneLedgerEntry(e)
		applied = true
		return true, nil
	})
	if err != nil {
		return out, false, 0, err
	}
	sent := emitLandedNotices(vaultDir, notices, now)
	return out, applied, sent, nil
}

// RefreshWorkspaceStatus asks the provider for a fresh status of the ledger entry key and stores it in the
// status cache (compare-and-set on refreshed_at). Provider errors are recorded in status.error and returned.
func RefreshWorkspaceStatus(vaultDir, key string, now time.Time) (workspace.LedgerEntry, error) {
	if now.IsZero() {
		now = time.Now()
	}
	e, ok := workspace.GetEntry(vaultDir, key)
	if !ok {
		return workspace.LedgerEntry{}, wsNotFound("workspace %s is not in the ledger", key)
	}
	info, callErr := fetchWorkspaceStatus(vaultDir, e)
	updated, _, err := applyWorkspaceStatusCAS(vaultDir, key, e.Status.RefreshedAt, info, callErr, now)
	if err != nil {
		return updated, err
	}
	if callErr != nil && workspace.ErrorCode(callErr) != workspace.CodeNotFound {
		return updated, callErr
	}
	return updated, nil
}

// WorkspaceRefreshResult is one entry's outcome from RefreshWorkspaces.
type WorkspaceRefreshResult struct {
	Key   string
	Entry workspace.LedgerEntry
	Err   error
}

// RefreshWorkspaces refreshes the given ledger keys with at most concurrency provider calls in flight.
func RefreshWorkspaces(vaultDir string, keys []string, concurrency int, now time.Time) []WorkspaceRefreshResult {
	if concurrency <= 0 {
		concurrency = 4
	}
	results := make([]WorkspaceRefreshResult, len(keys))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	wg.Add(len(keys))
	for i, key := range keys {
		go func(i int, key string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			e, err := RefreshWorkspaceStatus(vaultDir, key, now)
			results[i] = WorkspaceRefreshResult{Key: key, Entry: e, Err: err}
		}(i, key)
	}
	wg.Wait()
	return results
}

// ---------------------------------------------------------------------------------------------------------------
// Cache freshness and eligibility (cache-only; used by compaction)

// WorkspaceCacheUsable reports whether e's cached status is trustworthy for automated decisions: refreshed,
// not errored, and fresher than 2 x refreshSeconds (or 1h when daemon refresh is disabled). Fail-closed.
func WorkspaceCacheUsable(e workspace.LedgerEntry, refreshSeconds int, now time.Time) (bool, string) {
	if strings.TrimSpace(e.Status.Error) != "" {
		return false, "status errored"
	}
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(e.Status.RefreshedAt))
	if err != nil {
		return false, "status never refreshed"
	}
	maxAge := workspaceUncachedMaxAge
	if refreshSeconds > 0 {
		maxAge = 2 * time.Duration(refreshSeconds) * time.Second
	}
	if now.Sub(ts) > maxAge {
		return false, "status cache stale"
	}
	return true, ""
}

// IsLiveWorkspaceWriter reports whether writer (a task slug or conversation ID recorded in LedgerEntry.WriterTask)
// currently belongs to a live (non-archived, non-cancelled) task in local/memory/tasks.json. Used by PreToolUse W1
// so leases left behind by archived or deleted tasks are treated as vacant rather than permanently blocking edits.
func IsLiveWorkspaceWriter(vaultDir, writer string) bool {
	writer = strings.TrimSpace(writer)
	if writer == "" || strings.TrimSpace(vaultDir) == "" {
		return false
	}
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return true // fail closed when tasks.json is unreadable
	}
	for _, t := range tasks {
		st := strings.ToLower(strings.TrimSpace(t.Status))
		if st == "archived" || st == "cancelled" {
			continue
		}
		if strings.TrimSpace(t.TaskSlug) == writer || (t.ConversationID != "" && strings.TrimSpace(t.ConversationID) == writer) {
			return true
		}
	}
	return false
}

// ReleaseTaskWorkspaceLeases releases slug from bound_tasks and writer leases across the workspace ledger without
// invoking any provider subprocess. Used when a Stop hook auto-archives a task (e.g. an unpinned [Auto] worker)
// directly via RegisterTask. Owned entries with cached open changes and no other live binders are marked orphaned.
func ReleaseTaskWorkspaceLeases(vaultDir, slug string) {
	slug = strings.TrimSpace(slug)
	if slug == "" || strings.TrimSpace(vaultDir) == "" {
		return
	}
	ix := loadWorkspaceTaskIndex(vaultDir)
	_ = workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
		changed := false
		for key, e := range l.Entries {
			if e.WriterTask != slug && !slices.Contains(e.BoundTasks, slug) {
				continue
			}
			if releaseLedgerBinder(&e, slug) {
				changed = true
			}
			if e.Ownership == workspace.OwnershipOwned && len(otherLiveBinders(ix, e, slug)) == 0 && workspace.CountOpenChanges(e.Status.Changes) > 0 && !e.OrphanedOpenChanges {
				e.OrphanedOpenChanges = true
				changed = true
			}
			l.Put(e)
			_ = key
		}
		return changed, nil
	})
}

// WorkspaceDormantEligible reports whether e lets a dormant task auto-archive: a usable cache, clean state, and
// every change landed or abandoned (RFC-0025 §8).
func WorkspaceDormantEligible(e workspace.LedgerEntry, refreshSeconds int, now time.Time) (bool, string) {
	if ok, why := WorkspaceCacheUsable(e, refreshSeconds, now); !ok {
		return false, why
	}
	if e.Status.State != workspace.StateClean {
		return false, "workspace " + string(e.Status.State)
	}
	if n := workspace.CountOpenChanges(e.Status.Changes); n > 0 {
		return false, fmt.Sprintf("%d open change(s)", n)
	}
	return true, ""
}

// ---------------------------------------------------------------------------------------------------------------
// Inventory and summaries (cache-only render helpers)

// WorkspaceChangeView is a display-safe change summary (opaque ID and enums only; never provider titles).
type WorkspaceChangeView struct {
	ID         string                `json:"id"`
	State      workspace.ChangeState `json:"state"`
	NeedsOwner bool                  `json:"needs_owner,omitempty"`
}

// WorkspaceInventoryRow is one row of `hq workspace list` / `status`.
type WorkspaceInventoryRow struct {
	Source              string                `json:"source"` // ledger | git | provider-list
	Provider            string                `json:"provider"`
	Key                 string                `json:"key"`
	WorkspaceID         string                `json:"workspace_id"`
	Name                string                `json:"name,omitempty"`
	Root                string                `json:"root"`
	WorkDir             string                `json:"work_dir,omitempty"`
	Ownership           string                `json:"ownership"` // owned | attached | git | unledgered
	WriterTask          string                `json:"writer_task,omitempty"`
	LeaseEnforced       bool                  `json:"lease_enforced,omitempty"`
	BoundTasks          []string              `json:"bound_tasks,omitempty"`
	State               workspace.State       `json:"state"`
	OpenChanges         int                   `json:"open_changes"`
	NeedsOwner          int                   `json:"needs_owner"`
	Changes             []WorkspaceChangeView `json:"changes,omitempty"`
	RefreshedAt         string                `json:"refreshed_at,omitempty"`
	Error               string                `json:"error,omitempty"`
	OrphanedOpenChanges bool                  `json:"orphaned_open_changes,omitempty"`
}

func changeViews(changes []workspace.ChangeRef) []WorkspaceChangeView {
	var out []WorkspaceChangeView
	for _, c := range changes {
		out = append(out, WorkspaceChangeView{ID: c.ID, State: c.State, NeedsOwner: c.NeedsOwner})
	}
	return out
}

// WorkspaceRowFromEntry renders a ledger entry as an inventory row.
func WorkspaceRowFromEntry(e workspace.LedgerEntry) WorkspaceInventoryRow {
	return WorkspaceInventoryRow{
		Source:              "ledger",
		Provider:            e.Provider,
		Key:                 e.Key(),
		WorkspaceID:         e.WorkspaceID,
		Name:                e.Name,
		Root:                e.Root,
		WorkDir:             e.WorkDir,
		Ownership:           string(e.Ownership),
		WriterTask:          e.WriterTask,
		LeaseEnforced:       e.LeaseEnforced,
		BoundTasks:          append([]string(nil), e.BoundTasks...),
		State:               e.Status.State,
		OpenChanges:         workspace.CountOpenChanges(e.Status.Changes),
		NeedsOwner:          needsOwnerWorkspaceChanges(e.Status.Changes),
		Changes:             changeViews(e.Status.Changes),
		RefreshedAt:         e.Status.RefreshedAt,
		Error:               e.Status.Error,
		OrphanedOpenChanges: e.OrphanedOpenChanges,
	}
}

// WorkspaceInventoryOptions configures WorkspaceInventory.
type WorkspaceInventoryOptions struct {
	Provider   string // only this provider FQID ("" = all)
	All        bool   // also page through provider `list` for workspaces missing from the ledger
	IncludeGit bool   // include git worktrees mapped from InspectWorktrees
	Now        time.Time
}

const maxInventoryListPages = 10

// WorkspaceInventory merges ledger entries, git worktrees (InspectWorktrees), and optionally unledgered provider
// workspaces paged from `list`. Provider failures during --all paging are returned as warnings.
func WorkspaceInventory(vaultDir string, opts WorkspaceInventoryOptions) ([]WorkspaceInventoryRow, []string, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	filter := strings.TrimSpace(opts.Provider)
	l, err := workspace.LoadLedger(vaultDir)
	if err != nil {
		return nil, nil, err
	}
	var rows []WorkspaceInventoryRow
	seen := make(map[string]bool)
	keys := make([]string, 0, len(l.Entries))
	for k := range l.Entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := l.Entries[k]
		if filter != "" && e.Provider != filter {
			continue
		}
		rows = append(rows, WorkspaceRowFromEntry(e))
		seen[k] = true
	}
	if opts.IncludeGit && (filter == "" || filter == workspace.CoreGitProviderID) {
		statuses, gErr := InspectWorktrees(vaultDir, now)
		if gErr == nil {
			for _, st := range statuses {
				info := gitWorktreeInfo(st)
				row := WorkspaceInventoryRow{
					Source:      "git",
					Provider:    workspace.CoreGitProviderID,
					Key:         workspace.LedgerKey(workspace.CoreGitProviderID, info.WorkspaceID),
					WorkspaceID: info.WorkspaceID,
					Name:        info.Name,
					Root:        info.Root,
					WorkDir:     info.WorkDir,
					Ownership:   "git",
					WriterTask:  st.OwnerTaskSlug,
					State:       info.State,
					OpenChanges: workspace.CountOpenChanges(info.Changes),
					Changes:     changeViews(info.Changes),
				}
				if st.OwnerTaskSlug != "" {
					row.BoundTasks = []string{st.OwnerTaskSlug}
				}
				rows = append(rows, row)
			}
		}
	}
	var warnings []string
	if opts.All {
		for _, ps := range workspace.ListProviders(vaultDir) {
			if ps.FQID == workspace.CoreGitProviderID || (filter != "" && ps.FQID != filter) {
				continue
			}
			if !ps.Trusted {
				warnings = append(warnings, fmt.Sprintf("%s disabled: %s", ps.FQID, ps.Reason))
				continue
			}
			p, rErr := workspace.Resolve(vaultDir, ps.FQID)
			if rErr != nil {
				warnings = append(warnings, fmt.Sprintf("%s: %v", ps.FQID, rErr))
				continue
			}
			cursor := ""
			for page := 0; page < maxInventoryListPages; page++ {
				ctx, cancel := workspaceProviderContext()
				infos, next, lErr := p.List(ctx, workspace.ListRequest{Cursor: cursor})
				cancel()
				if lErr != nil {
					warnings = append(warnings, fmt.Sprintf("%s list: %s", ps.FQID, WorkspaceSafeErrorSummary(lErr)))
					break
				}
				for _, info := range infos {
					key := workspace.LedgerKey(ps.FQID, info.WorkspaceID)
					if seen[key] {
						continue
					}
					seen[key] = true
					rows = append(rows, WorkspaceInventoryRow{
						Source:      "provider-list",
						Provider:    ps.FQID,
						Key:         key,
						WorkspaceID: info.WorkspaceID,
						Name:        info.Name,
						Root:        info.Root,
						WorkDir:     info.WorkDir,
						Ownership:   "unledgered",
						State:       info.State,
					})
				}
				if next == "" {
					break
				}
				cursor = next
				if page == maxInventoryListPages-1 {
					warnings = append(warnings, fmt.Sprintf("%s list truncated after %d pages", ps.FQID, maxInventoryListPages))
				}
			}
		}
	}
	return rows, warnings, nil
}

// UnifiedSurfaceEntries returns a single display-safe slice combining external provider ledger entries
// (read-only cache; never calls external providers) and managed git worktrees (InspectWorktrees), sorted
// deterministically by provider and display name. Used by CONSOLE.md, the WebUI, Console Spaces, and Brief.
func UnifiedSurfaceEntries(vaultDir string, now time.Time) []workspace.SurfaceEntry {
	if now.IsZero() {
		now = time.Now()
	}
	ledgerEntries, _ := workspace.LoadSurfaceEntries(vaultDir)
	seen := make(map[string]bool, len(ledgerEntries))
	out := make([]workspace.SurfaceEntry, 0, len(ledgerEntries))
	for _, e := range ledgerEntries {
		seen[e.Key] = true
		out = append(out, e)
	}
	if statuses, err := InspectWorktrees(vaultDir, now); err == nil {
		for _, st := range statuses {
			info := gitWorktreeInfo(st)
			entry := workspace.LedgerEntry{
				Provider:    workspace.CoreGitProviderID,
				WorkspaceID: info.WorkspaceID,
				Name:        info.Name,
				Root:        info.Root,
				WorkDir:     info.WorkDir,
				Ownership:   workspace.OwnershipOwned,
				WriterTask:  st.OwnerTaskSlug,
				Status: workspace.LedgerStatus{
					State:   info.State,
					Changes: info.Changes,
				},
			}
			if st.OwnerTaskSlug != "" {
				entry.BoundTasks = []string{st.OwnerTaskSlug}
			}
			se := workspace.NewSurfaceEntry(entry)
			if seen[se.Key] {
				continue
			}
			seen[se.Key] = true
			out = append(out, se)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if di, dj := out[i].DisplayName(), out[j].DisplayName(); di != dj {
			return di < dj
		}
		return out[i].WorkspaceID < out[j].WorkspaceID
	})
	return out
}

// FastGitWorktreeSurfaceEntries projects managed git worktree directories onto workspace.SurfaceEntry without
// spawning git subprocesses (safe for the SessionStart hook hot path). Owner task slugs are matched from tasks.
func FastGitWorktreeSurfaceEntries(vaultDir string, wts []string, tasks []ActiveTask, now time.Time) []workspace.SurfaceEntry {
	if len(wts) == 0 {
		return nil
	}
	ownerByDir := make(map[string]string, len(tasks))
	for _, t := range tasks {
		if t.EffectiveStatus(now) == "archived" || t.EffectiveStatus(now) == "cancelled" {
			continue
		}
		for _, d := range t.AllWorktreeDirs() {
			clean := workspace.CanonicalPath(d)
			if clean != "" && ownerByDir[clean] == "" {
				ownerByDir[clean] = t.TaskSlug
			}
		}
	}
	out := make([]workspace.SurfaceEntry, 0, len(wts))
	for _, wt := range wts {
		clean := workspace.CanonicalPath(wt)
		if clean == "" {
			continue
		}
		owner := ownerByDir[clean]
		entry := workspace.LedgerEntry{
			Provider:    workspace.CoreGitProviderID,
			WorkspaceID: clean,
			Name:        filepath.Base(clean),
			Root:        clean,
			WorkDir:     clean,
			Ownership:   workspace.OwnershipOwned,
			WriterTask:  owner,
			Status: workspace.LedgerStatus{
				State: workspace.StateUnknown,
			},
		}
		if owner != "" {
			entry.BoundTasks = []string{owner}
		}
		out = append(out, workspace.NewSurfaceEntry(entry))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].DisplayName() < out[j].DisplayName()
	})
	return out
}

// WorkspaceSummary aggregates the ledger cache for `hq status`.
type WorkspaceSummary struct {
	Total       int            `json:"total"`
	Git         int            `json:"git"`
	ByProvider  map[string]int `json:"by_provider"`
	OpenChanges int            `json:"open_changes"`
	NeedsOwner  int            `json:"needs_owner"`
	Orphaned    int            `json:"orphaned"`
}

// SummarizeWorkspaces aggregates the provider ledger (cache only, no provider calls) plus gitCount git worktrees.
// It returns false when the ledger has no entries, so callers keep their pre-RFC-0025 output.
func SummarizeWorkspaces(vaultDir string, gitCount int) (WorkspaceSummary, bool) {
	l, err := workspace.LoadLedger(vaultDir)
	if err != nil || len(l.Entries) == 0 {
		return WorkspaceSummary{}, false
	}
	s := WorkspaceSummary{Git: gitCount, ByProvider: make(map[string]int)}
	for _, e := range l.Entries {
		s.ByProvider[e.Provider]++
		s.OpenChanges += workspace.CountOpenChanges(e.Status.Changes)
		s.NeedsOwner += needsOwnerWorkspaceChanges(e.Status.Changes)
		if e.OrphanedOpenChanges {
			s.Orphaned++
		}
	}
	s.Total = gitCount + len(l.Entries)
	return s, true
}

// StatusLine renders `Workspaces: N (git: a, <provider>: b) · open changes: c · needs you: d`.
func (s WorkspaceSummary) StatusLine() string {
	parts := []string{fmt.Sprintf("git: %d", s.Git)}
	providers := make([]string, 0, len(s.ByProvider))
	for p := range s.ByProvider {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	for _, p := range providers {
		parts = append(parts, fmt.Sprintf("%s: %d", p, s.ByProvider[p]))
	}
	return fmt.Sprintf("Workspaces: %d (%s) · open changes: %d · needs you: %d", s.Total, strings.Join(parts, ", "), s.OpenChanges, s.NeedsOwner)
}
