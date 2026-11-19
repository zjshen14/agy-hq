package kernel

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/workspace"
)

// RFC-0025 §8 lifecycle integration for provider workspace bindings (ActiveTask.Workspaces). Git worktrees keep
// their existing archive/reopen/compact/prune code paths unchanged; provider bindings are handled alongside them.
//
// Archived tasks keep their provider bindings as history (like WorktreeDir), so ReopenTaskWithOptions knows what
// to rebind or re-create. Liveness is decided by task status: bindings of archived tasks never count as live, and
// archiving releases the task from each ledger entry's bound_tasks and writer lease.

type workspaceArchiveAction int

const (
	wsArchiveUnbind     workspaceArchiveAction = iota // keep the workspace; release this task's binding and lease
	wsArchiveRemove                                   // provider remove (both preconditions re-checked), then drop the entry
	wsArchiveKeepOrphan                               // owned with open changes: keep, release, mark orphaned_open_changes
	wsArchiveForget                                   // the workspace is gone: drop the ledger entry
)

type workspaceArchiveStep struct {
	Entry        workspace.LedgerEntry
	Action       workspaceArchiveAction
	RequireClean bool
	OpenChanges  int
	Note         string
}

// workspaceObservation is the status used for one archive decision: either a fresh provider status or the cache.
type workspaceObservation struct {
	State       workspace.State
	OpenChanges int
	Err         error
}

func observationFromInfo(info workspace.Info, err error) workspaceObservation {
	if err != nil {
		if workspace.ErrorCode(err) == workspace.CodeNotFound {
			return workspaceObservation{State: workspace.StateMissing}
		}
		return workspaceObservation{Err: err}
	}
	return workspaceObservation{State: info.State, OpenChanges: openWorkspaceChanges(info.Changes)}
}

func dedupeBindings(bs []workspace.Binding) []workspace.Binding {
	seen := make(map[string]bool, len(bs))
	var out []workspace.Binding
	for _, b := range bs {
		if b.Key == "" || seen[b.Key] {
			continue
		}
		seen[b.Key] = true
		out = append(out, b)
	}
	return out
}

func workspaceRemoveHint(key string) string {
	return fmt.Sprintf("to delete it anyway, stage an approval: %s", WorkspaceRemoveProposal("hq", key, "open changes"))
}

// decideWorkspaceArchive applies the RFC-0025 §8 archive table to one binding. sole means no other live task is
// bound to the entry (so an owned entry is removable); checkDirty means a dirty workspace blocks the archive
// unless force.
func decideWorkspaceArchive(target string, e workspace.LedgerEntry, sole, checkDirty, force bool, obs workspaceObservation) (workspaceArchiveStep, error) {
	key := e.Key()
	removable := sole && e.Ownership == workspace.OwnershipOwned
	step := workspaceArchiveStep{Entry: e, Action: wsArchiveUnbind, OpenChanges: obs.OpenChanges}
	if obs.Err != nil {
		if checkDirty && !force {
			return step, fmt.Errorf("refusing to archive %q: cannot verify workspace %s (%s) (pass --force to keep it and unbind)", target, key, clipWorkspaceText(obs.Err.Error(), 160))
		}
		step.Note = fmt.Sprintf("kept workspace %s (status unavailable: %s)", key, clipWorkspaceText(obs.Err.Error(), 120))
		return step, nil
	}
	if obs.State == workspace.StateMissing {
		if sole {
			step.Action = wsArchiveForget
			step.Note = fmt.Sprintf("workspace %s no longer exists; dropped its ledger entry", key)
		}
		return step, nil
	}
	dirty := obs.State != workspace.StateClean
	if dirty && checkDirty && !force {
		return step, fmt.Errorf("refusing to archive %q: workspace %s is %s (pass --force to override)", target, key, obs.State)
	}
	if !removable {
		return step, nil
	}
	if obs.OpenChanges > 0 {
		step.Action = wsArchiveKeepOrphan
		step.Note = fmt.Sprintf("kept owned workspace %s with %d open change(s) (unbound, marked orphaned_open_changes); %s", key, obs.OpenChanges, workspaceRemoveHint(key))
		return step, nil
	}
	if dirty && !force {
		step.Note = fmt.Sprintf("kept owned workspace %s (%s)", key, obs.State)
		return step, nil
	}
	step.Action = wsArchiveRemove
	step.RequireClean = !dirty
	return step, nil
}

// planTaskWorkspaceArchive takes a fresh provider status for every provider binding of t (outside all locks,
// writing it into the status cache) and decides each binding's archive action. A dirty non-readonly binding the
// task writes to refuses the archive unless force. With no provider bindings it touches nothing.
func planTaskWorkspaceArchive(vaultDir, target string, t ActiveTask, force bool, now time.Time) ([]workspaceArchiveStep, error) {
	bindings := dedupeBindings(t.Workspaces)
	if len(bindings) == 0 {
		return nil, nil
	}
	l, err := workspace.LoadLedger(vaultDir)
	if err != nil {
		if force {
			return nil, nil
		}
		return nil, fmt.Errorf("refusing to archive %q: workspace ledger unreadable: %w (pass --force to skip workspace cleanup)", target, err)
	}
	ix := loadWorkspaceTaskIndex(vaultDir)
	var steps []workspaceArchiveStep
	for _, b := range bindings {
		e, ok := l.Entries[b.Key]
		if !ok {
			continue
		}
		others := otherLiveBinders(ix, e, t.TaskSlug)
		sole := len(others) == 0
		removable := e.Ownership == workspace.OwnershipOwned && sole
		checkDirty := b.Role != workspace.RoleReadonly && (e.WriterTask == "" || e.WriterTask == t.TaskSlug || !ix.isLive(e.WriterTask))
		var obs workspaceObservation
		if removable || checkDirty {
			info, stErr := fetchWorkspaceStatus(vaultDir, e)
			if updated, _, _ := applyWorkspaceStatusCAS(vaultDir, b.Key, e.Status.RefreshedAt, info, stErr, now); updated.Provider != "" {
				e = updated
			}
			obs = observationFromInfo(info, stErr)
		} else {
			obs = workspaceObservation{State: e.Status.State, OpenChanges: openWorkspaceChanges(e.Status.Changes)}
		}
		step, dErr := decideWorkspaceArchive(target, e, sole, checkDirty, force, obs)
		if dErr != nil {
			return nil, dErr
		}
		steps = append(steps, step)
	}
	return steps, nil
}

func otherLiveBinders(ix workspaceTaskIndex, e workspace.LedgerEntry, slug string) []string {
	var out []string
	for _, s := range ix.liveBinders(e) {
		if s != slug {
			out = append(out, s)
		}
	}
	return out
}

// executeTaskWorkspaceArchive carries out archive steps: provider removals run first (outside every lock), then one
// ledger update drops removed/forgotten entries and releases slug from the rest. A provider that refuses a removal
// leaves the workspace kept and unbound (orphaned when it reports open changes). It returns user-facing notes.
func executeTaskWorkspaceArchive(vaultDir, slug string, steps []workspaceArchiveStep) []string {
	if len(steps) == 0 {
		return nil
	}
	var notes []string
	for i := range steps {
		st := &steps[i]
		if st.Action != wsArchiveRemove {
			if st.Note != "" {
				notes = append(notes, st.Note)
			}
			continue
		}
		key := st.Entry.Key()
		p, err := workspace.Resolve(vaultDir, st.Entry.Provider)
		if err == nil {
			ctx, cancel := workspaceProviderContext()
			err = p.Remove(ctx, workspace.RemoveRequest{
				WorkspaceID:          st.Entry.WorkspaceID,
				ExpectedRoot:         st.Entry.Root,
				RequireClean:         st.RequireClean,
				RequireNoOpenChanges: true,
			})
			cancel()
		}
		switch {
		case err == nil:
			notes = append(notes, fmt.Sprintf("removed owned workspace %s", key))
		case workspace.ErrorCode(err) == workspace.CodeNotFound:
			st.Action = wsArchiveForget
			notes = append(notes, fmt.Sprintf("workspace %s no longer exists; dropped its ledger entry", key))
		case workspace.ErrorCode(err) == workspace.CodeHasOpenChanges:
			st.Action = wsArchiveKeepOrphan
			notes = append(notes, fmt.Sprintf("kept owned workspace %s: provider reports open changes (unbound, marked orphaned_open_changes); %s", key, workspaceRemoveHint(key)))
		default:
			st.Action = wsArchiveUnbind
			notes = append(notes, fmt.Sprintf("kept workspace %s: remove failed: %s", key, clipWorkspaceText(err.Error(), 160)))
		}
	}
	_ = workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
		changed := false
		for _, st := range steps {
			key := st.Entry.Key()
			e, ok := l.Entries[key]
			if !ok {
				continue
			}
			switch st.Action {
			case wsArchiveRemove, wsArchiveForget:
				delete(l.Entries, key)
				changed = true
				continue
			case wsArchiveKeepOrphan:
				if !e.OrphanedOpenChanges {
					e.OrphanedOpenChanges = true
					changed = true
				}
			}
			if releaseLedgerBinder(&e, slug) {
				changed = true
			}
			l.Put(e)
		}
		return changed, nil
	})
	return notes
}

// workspaceRefreshSecondsCache memoizes provider refresh cadences (no subprocess: Resolve only reads manifests).
type workspaceRefreshSecondsCache struct {
	vaultDir string
	byFQID   map[string]int
	resolved map[string]bool
}

func newWorkspaceRefreshSecondsCache(vaultDir string) *workspaceRefreshSecondsCache {
	return &workspaceRefreshSecondsCache{vaultDir: vaultDir, byFQID: make(map[string]int), resolved: make(map[string]bool)}
}

// get returns the provider's refresh cadence and whether the provider currently resolves (trusted and valid).
func (c *workspaceRefreshSecondsCache) get(fqid string) (int, bool) {
	if v, ok := c.byFQID[fqid]; ok {
		return v, c.resolved[fqid]
	}
	p, err := workspace.Resolve(c.vaultDir, fqid)
	if err != nil {
		c.byFQID[fqid] = 0
		c.resolved[fqid] = false
		return 0, false
	}
	v := WorkspaceStatusRefreshSeconds(p)
	c.byFQID[fqid] = v
	c.resolved[fqid] = true
	return v, true
}

// workspaceCompaction is the lazily loaded, cache-only view CompactTasks uses for provider bindings.
type workspaceCompaction struct {
	vaultDir string
	ledger   workspace.Ledger
	err      error
	ix       workspaceTaskIndex
	refresh  *workspaceRefreshSecondsCache
	now      time.Time
	loaded   bool
}

func (wc *workspaceCompaction) load() {
	if wc.loaded {
		return
	}
	wc.loaded = true
	wc.ledger, wc.err = workspace.LoadLedger(wc.vaultDir)
	wc.ix = loadWorkspaceTaskIndex(wc.vaultDir)
	wc.refresh = newWorkspaceRefreshSecondsCache(wc.vaultDir)
}

// eligible reports whether every non-readonly provider binding of t allows auto-archiving from the cache alone.
// requireClosed additionally demands every change landed or abandoned (dormant tasks). Fail-closed: a stale,
// errored, or unrefreshed cache, an unreadable ledger, or an unresolvable provider means not eligible.
func (wc *workspaceCompaction) eligible(t ActiveTask, requireClosed bool) bool {
	if len(t.Workspaces) == 0 {
		return true
	}
	wc.load()
	if wc.err != nil {
		return false
	}
	for _, b := range dedupeBindings(t.Workspaces) {
		if b.Role == workspace.RoleReadonly {
			continue
		}
		e, ok := wc.ledger.Entries[b.Key]
		if !ok {
			continue
		}
		if e.WriterTask != "" && e.WriterTask != t.TaskSlug && wc.ix.isLive(e.WriterTask) {
			continue // another live task writes here; this task only releases its binding
		}
		secs, resolved := wc.refresh.get(e.Provider)
		if !resolved {
			return false
		}
		if requireClosed {
			if ok, _ := WorkspaceDormantEligible(e, secs, wc.now); !ok {
				return false
			}
			continue
		}
		usable, _ := WorkspaceCacheUsable(e, secs, wc.now)
		if !usable || (e.Status.State != workspace.StateClean && e.Status.State != workspace.StateMissing) {
			return false
		}
	}
	return true
}

// release archives t's provider bindings from the cache (no status call): owned workspaces with a usable clean,
// fully landed cache are removed (the provider re-checks both preconditions atomically); owned workspaces with
// open changes are kept and marked orphaned; everything else is kept and unbound.
func (wc *workspaceCompaction) release(t ActiveTask) []string {
	bindings := dedupeBindings(t.Workspaces)
	if len(bindings) == 0 {
		return nil
	}
	wc.load()
	if wc.err != nil {
		return nil
	}
	var steps []workspaceArchiveStep
	for _, b := range bindings {
		e, ok := wc.ledger.Entries[b.Key]
		if !ok {
			continue
		}
		step := workspaceArchiveStep{Entry: e, Action: wsArchiveUnbind}
		sole := len(otherLiveBinders(wc.ix, e, t.TaskSlug)) == 0
		removable := e.Ownership == workspace.OwnershipOwned && sole
		secs, resolved := wc.refresh.get(e.Provider)
		usable := false
		if resolved {
			usable, _ = WorkspaceCacheUsable(e, secs, wc.now)
		}
		if usable && sole && e.Status.State == workspace.StateMissing {
			step.Action = wsArchiveForget
		} else if removable && usable {
			open := openWorkspaceChanges(e.Status.Changes)
			switch {
			case open > 0:
				step.Action = wsArchiveKeepOrphan
				step.OpenChanges = open
			case e.Status.State == workspace.StateClean:
				step.Action = wsArchiveRemove
				step.RequireClean = true
			}
		}
		steps = append(steps, step)
	}
	return executeTaskWorkspaceArchive(wc.vaultDir, t.TaskSlug, steps)
}

// restoreTaskWorkspaces rebinds a reopened task's provider bindings (RFC-0025 §8): a ledger entry whose fresh
// status is not missing is rebound (the task takes the writer lease back when it is vacant); a missing workspace
// behind a primary binding is re-created with its original name (the ledger entry's name when the entry survived,
// otherwise the task slug that `hq spawn --workspace` uses) and the matching idempotency key; other gone
// workspaces are dropped from the task. With noWorktree, providers are not called and only existing ledger
// entries are rebound.
func restoreTaskWorkspaces(vaultDir string, t ActiveTask, noWorktree bool, now time.Time) []string {
	bindings := dedupeBindings(t.Workspaces)
	if len(bindings) == 0 {
		return nil
	}
	slug := t.TaskSlug
	l, err := workspace.LoadLedger(vaultDir)
	if err != nil {
		return []string{fmt.Sprintf("workspace bindings not restored: ledger unreadable: %v", err)}
	}
	ix := loadWorkspaceTaskIndex(vaultDir)
	var notes []string
	var rebuilt []workspace.Binding
	rebind := make(map[string]workspace.Binding)
	for _, b := range bindings {
		e, exists := l.Entries[b.Key]
		createName := slug
		if exists && !noWorktree {
			info, stErr := fetchWorkspaceStatus(vaultDir, e)
			_, _, _ = applyWorkspaceStatusCAS(vaultDir, b.Key, e.Status.RefreshedAt, info, stErr, now)
			obs := observationFromInfo(info, stErr)
			if obs.Err != nil {
				notes = append(notes, fmt.Sprintf("rebound workspace %s without a fresh status (%s)", b.Key, clipWorkspaceText(obs.Err.Error(), 120)))
			}
			if obs.State == workspace.StateMissing {
				_ = dropLedgerEntry(vaultDir, b.Key)
				exists = false
				if workspace.ValidateCreateName(e.Name) == nil {
					createName = e.Name
				}
			}
		}
		if exists {
			rebuilt = append(rebuilt, b)
			rebind[b.Key] = b
			continue
		}
		if noWorktree || b.Role != workspace.RolePrimary {
			notes = append(notes, fmt.Sprintf("workspace %s no longer exists; dropped its binding", b.Key))
			continue
		}
		acq, aErr := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{
			Provider:      b.Provider,
			Name:          createName,
			TaskSlug:      slug,
			Role:          workspace.RolePrimary,
			LeaseEnforced: true,
			Now:           now,
		})
		if aErr != nil {
			notes = append(notes, fmt.Sprintf("workspace %s no longer exists and could not be re-created: %s", b.Key, clipWorkspaceText(aErr.Error(), 160)))
			continue
		}
		notes = append(notes, fmt.Sprintf("re-created workspace %s at %s", acq.Binding.Key, acq.Entry.WorkDir))
		rebuilt = append(rebuilt, acq.Binding)
	}
	if len(rebind) > 0 {
		_ = workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
			changed := false
			for key, b := range rebind {
				e, ok := l.Entries[key]
				if !ok {
					continue
				}
				e.BoundTasks = addUniqueSorted(e.BoundTasks, slug)
				if b.Role != workspace.RoleReadonly && (e.WriterTask == "" || e.WriterTask == slug || !ix.isLive(e.WriterTask)) {
					e.WriterTask = slug
					e.LeaseEnforced = b.Role == workspace.RolePrimary
				}
				e.OrphanedOpenChanges = false
				l.Put(e)
				changed = true
			}
			return changed, nil
		})
	}
	if !sameBindingKeys(bindings, rebuilt) {
		_ = UpdateTasks(vaultDir, func(tasks []ActiveTask) ([]ActiveTask, bool, error) {
			for i := range tasks {
				if tasks[i].TaskSlug == slug {
					tasks[i].Workspaces = append([]workspace.Binding(nil), rebuilt...)
					return tasks, true, nil
				}
			}
			return tasks, false, nil
		})
	}
	return notes
}

func sameBindingKeys(a, b []workspace.Binding) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------------------------------------------
// Orphan prune

// PruneWorkspacesOptions configures PruneOrphanWorkspaces.
type PruneWorkspacesOptions struct {
	Now    time.Time
	MinAge time.Duration // keep:recent window (DefaultOrphanWorkspaceMinAge when zero; negative disables)
	DryRun bool
}

// PruneWorkspaceDecision describes one ledger entry PruneOrphanWorkspaces kept.
type PruneWorkspaceDecision struct {
	Key    string `json:"key"`
	Action string `json:"action"` // keep:linked | keep:attached | keep:recent | keep:dirty | keep:open_changes | keep:status_error | keep:remove_error
	Detail string `json:"detail,omitempty"`
}

// PruneWorkspacesReport summarizes PruneOrphanWorkspaces.
type PruneWorkspacesReport struct {
	Scanned    int                      `json:"scanned"`
	Pruned     []string                 `json:"pruned,omitempty"`
	WouldPrune []string                 `json:"wouldPrune,omitempty"`
	Forgotten  []string                 `json:"forgotten,omitempty"`
	Kept       []PruneWorkspaceDecision `json:"kept,omitempty"`
}

// pruneOrphanWorkspacesForCompaction runs PruneOrphanWorkspaces for CompactTasks and returns the pruned (or, in
// dry-run, would-be-pruned) ledger keys.
func pruneOrphanWorkspacesForCompaction(vaultDir string, opts CompactTasksOptions) []string {
	rep, err := PruneOrphanWorkspaces(vaultDir, PruneWorkspacesOptions{Now: opts.Now, DryRun: opts.DryRun})
	if err != nil || rep == nil {
		return nil
	}
	if opts.DryRun {
		return rep.WouldPrune
	}
	return rep.Pruned
}

// PruneOrphanWorkspaces removes owned ledger workspaces that no live task is bound to (RFC-0025 §8): it keeps
// linked, attached, recently created (MinAge), dirty, and open-change entries, and otherwise sends `remove` with
// both require_clean and require_no_open_changes after a fresh status. It does nothing when the ledger is empty.
func PruneOrphanWorkspaces(vaultDir string, opts PruneWorkspacesOptions) (*PruneWorkspacesReport, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	minAge := opts.MinAge
	if minAge == 0 {
		minAge = DefaultOrphanWorkspaceMinAge
	}
	rep := &PruneWorkspacesReport{}
	l, err := workspace.LoadLedger(vaultDir)
	if err != nil {
		return rep, err
	}
	if len(l.Entries) == 0 {
		return rep, nil
	}
	ix := loadWorkspaceTaskIndex(vaultDir)
	keys := make([]string, 0, len(l.Entries))
	for k := range l.Entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	keep := func(key, action, detail string) {
		rep.Kept = append(rep.Kept, PruneWorkspaceDecision{Key: key, Action: action, Detail: detail})
	}
	for _, key := range keys {
		e := l.Entries[key]
		rep.Scanned++
		if binders := ix.liveBinders(e); len(binders) > 0 {
			keep(key, "keep:linked", strings.Join(binders, ","))
			continue
		}
		if e.Ownership != workspace.OwnershipOwned {
			keep(key, "keep:attached", "")
			continue
		}
		if minAge > 0 {
			created, cErr := time.Parse(time.RFC3339, strings.TrimSpace(e.CreatedAt))
			if cErr != nil || opts.Now.Sub(created) < minAge {
				keep(key, "keep:recent", e.CreatedAt)
				continue
			}
		}
		info, stErr := fetchWorkspaceStatus(vaultDir, e)
		if !opts.DryRun {
			_, _, _ = applyWorkspaceStatusCAS(vaultDir, key, e.Status.RefreshedAt, info, stErr, opts.Now)
		}
		obs := observationFromInfo(info, stErr)
		switch {
		case obs.Err != nil:
			keep(key, "keep:status_error", clipWorkspaceText(obs.Err.Error(), 160))
			continue
		case obs.State == workspace.StateMissing:
			if !opts.DryRun {
				_ = dropLedgerEntry(vaultDir, key)
			}
			rep.Forgotten = append(rep.Forgotten, key)
			continue
		case obs.State != workspace.StateClean:
			keep(key, "keep:dirty", string(obs.State))
			continue
		case obs.OpenChanges > 0:
			keep(key, "keep:open_changes", fmt.Sprintf("%d open", obs.OpenChanges))
			continue
		}
		if opts.DryRun {
			rep.WouldPrune = append(rep.WouldPrune, key)
			continue
		}
		p, rErr := workspace.Resolve(vaultDir, e.Provider)
		if rErr == nil {
			ctx, cancel := workspaceProviderContext()
			rErr = p.Remove(ctx, workspace.RemoveRequest{WorkspaceID: e.WorkspaceID, ExpectedRoot: e.Root, RequireClean: true, RequireNoOpenChanges: true})
			cancel()
		}
		if rErr != nil && workspace.ErrorCode(rErr) != workspace.CodeNotFound {
			action := "keep:remove_error"
			switch workspace.ErrorCode(rErr) {
			case workspace.CodeDirty:
				action = "keep:dirty"
			case workspace.CodeHasOpenChanges:
				action = "keep:open_changes"
			}
			keep(key, action, clipWorkspaceText(rErr.Error(), 160))
			continue
		}
		_ = dropLedgerEntry(vaultDir, key)
		if rErr != nil {
			rep.Forgotten = append(rep.Forgotten, key)
		} else {
			rep.Pruned = append(rep.Pruned, key)
		}
	}
	return rep, nil
}
