package kernel

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

// RFC-0025 §7.3, §11.1, §16.4, §16.8: daemon-side workspace maintenance. It drains Stop-hook probe candidates into
// attached ledger entries, refreshes the status cache of workspaces bound to live tasks (and of owned workspaces kept
// with open changes) on each provider's status_refresh_seconds cadence, and emits one deduplicated brief_rollup item
// per (ledger key, change id) that transitions to landed. With an empty ledger it does no work: no lock, no manifest
// read, no subprocess.

const (
	// MaxWorkspaceProbesPerTick bounds probe candidates drained per maintenance tick.
	MaxWorkspaceProbesPerTick = 10
	// MaxWorkspaceRefreshesPerTick bounds status refreshes per maintenance tick.
	MaxWorkspaceRefreshesPerTick = 20
	// MaxConcurrentWorkspaceRefreshes bounds provider status/probe calls in flight.
	MaxConcurrentWorkspaceRefreshes = 4
	// DefaultWorkspaceMaintenanceBudget bounds the wall-clock duration of one maintenance tick so slow or hung
	// providers cannot monopolize background work.
	DefaultWorkspaceMaintenanceBudget = 45 * time.Second
)

// WorkspaceMaintenanceReport summarizes one RunWorkspaceMaintenance tick.
type WorkspaceMaintenanceReport struct {
	ProbesDrained  int      `json:"probesDrained,omitempty"`
	Attached       []string `json:"attached,omitempty"` // ledger keys newly attached from probe candidates
	Bound          []string `json:"bound,omitempty"`    // "<key>@<task>" secondary bindings added
	Refreshed      int      `json:"refreshed,omitempty"`
	RefreshErrors  int      `json:"refreshErrors,omitempty"`
	LandedNotified int      `json:"landedNotified,omitempty"`
}

// Changed reports whether the tick changed anything a console render would show.
func (r WorkspaceMaintenanceReport) Changed() bool {
	return len(r.Attached) > 0 || len(r.Bound) > 0 || r.Refreshed > 0 || r.RefreshErrors > 0 || r.LandedNotified > 0
}

// RunWorkspaceMaintenance runs one daemon maintenance tick bounded by DefaultWorkspaceMaintenanceBudget.
func RunWorkspaceMaintenance(vaultDir string, now time.Time) (WorkspaceMaintenanceReport, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultWorkspaceMaintenanceBudget)
	defer cancel()
	return RunWorkspaceMaintenanceContext(ctx, vaultDir, now)
}

// RunWorkspaceMaintenanceContext runs one daemon maintenance tick bounded by ctx (see the file comment).
func RunWorkspaceMaintenanceContext(ctx context.Context, vaultDir string, now time.Time) (WorkspaceMaintenanceReport, error) {
	var rep WorkspaceMaintenanceReport
	if strings.TrimSpace(vaultDir) == "" {
		return rep, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now()
	}
	l, err := workspace.LoadLedger(vaultDir)
	if err != nil {
		return rep, err
	}
	if len(l.Entries) == 0 && len(l.ProbeCandidates) == 0 {
		return rep, nil
	}
	if len(l.ProbeCandidates) > 0 && ctx.Err() == nil {
		drainWorkspaceProbeCandidates(ctx, vaultDir, now, &rep)
	}
	if ctx.Err() == nil {
		refreshDueWorkspaces(ctx, vaultDir, now, &rep)
	}
	return rep, nil
}

// drainWorkspaceProbeCandidates turns up to MaxWorkspaceProbesPerTick probe candidates into attached ledger
// entries (advisory writer lease) and secondary task bindings with bounded concurrency and ctx budget checks.
// Candidates skipped due to tick budget exhaustion are re-queued; candidates no trusted provider claims are
// dropped. Ledger and task updates run sequentially, never nested.
func drainWorkspaceProbeCandidates(ctx context.Context, vaultDir string, now time.Time, rep *WorkspaceMaintenanceReport) {
	cands, err := workspace.DrainProbeCandidates(vaultDir, MaxWorkspaceProbesPerTick)
	if err != nil || len(cands) == 0 {
		return
	}
	nowStr := now.UTC().Format(time.RFC3339)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, MaxConcurrentWorkspaceRefreshes)
	for _, c := range cands {
		if ctx.Err() != nil {
			_ = workspace.RecordProbeCandidate(vaultDir, c.Path, c.TaskSlug)
			continue
		}
		wg.Add(1)
		go func(c workspace.ProbeCandidate) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				_ = workspace.RecordProbeCandidate(vaultDir, c.Path, c.TaskSlug)
				return
			}
			if ctx.Err() != nil {
				_ = workspace.RecordProbeCandidate(vaultDir, c.Path, c.TaskSlug)
				return
			}
			mu.Lock()
			rep.ProbesDrained++
			mu.Unlock()

			task, hasTask := ActiveTask{}, false
			if slug := strings.TrimSpace(c.TaskSlug); slug != "" {
				if t, ok := ResolveOwningTask(vaultDir, slug); ok && isLiveTaskStatus(t) {
					task, hasTask = t, true
				}
			}
			var key string
			var entry workspace.LedgerEntry
			if e, hit := workspace.FindEntryForPath(vaultDir, c.Path); hit {
				key = e.Key()
			} else {
				fqid, _, ok := matchWorkspaceRootPattern(vaultDir, c.Path)
				if !ok {
					return
				}
				p, rErr := workspace.Resolve(vaultDir, fqid)
				if rErr != nil {
					return
				}
				callCtx, cancel := workspaceProviderContextFrom(ctx)
				info, owned, pErr := p.Probe(callCtx, c.Path)
				cancel()
				if pErr != nil {
					if ctx.Err() != nil {
						_ = workspace.RecordProbeCandidate(vaultDir, c.Path, c.TaskSlug)
					}
					return
				}
				if !owned {
					return
				}
				info.Provider = fqid
				if info.WorkDir == "" {
					info.WorkDir = info.Root
				}
				if validateAcquiredWorkspaceInfo(vaultDir, p, info) != nil {
					return
				}
				canonRoot := workspace.CanonicalPath(info.Root)
				key = workspace.LedgerKey(fqid, info.WorkspaceID)
				created := false
				uErr := workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
					created = false
					if _, exists := l.Entries[key]; exists {
						return false, nil
					}
					for _, other := range l.Entries {
						if wsPathsOverlap(other.Root, info.Root) {
							return false, nil
						}
					}
					st := workspace.StateUnknown
					if info.State.Valid() {
						st = info.State
					}
					l.Put(workspace.LedgerEntry{
						Provider:      fqid,
						WorkspaceID:   info.WorkspaceID,
						Name:          info.Name,
						Root:          info.Root,
						CanonicalRoot: canonRoot,
						WorkDir:       info.WorkDir,
						Ownership:     workspace.OwnershipAttached,
						CreatedAt:     nowStr,
						Status:        workspace.LedgerStatus{State: st},
					})
					created = true
					return true, nil
				})
				if uErr != nil {
					return
				}
				if created {
					mu.Lock()
					rep.Attached = append(rep.Attached, key)
					mu.Unlock()
				}
			}
			if !hasTask {
				return
			}
			slug := task.TaskSlug
			_ = workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
				e, ok := l.Entries[key]
				if !ok {
					return false, nil
				}
				changed := false
				if !containsString(e.BoundTasks, slug) {
					e.BoundTasks = addUniqueSorted(e.BoundTasks, slug)
					changed = true
				}
				if e.WriterTask == "" {
					e.WriterTask = slug // advisory until confirmed with handoff or adopt --task (RFC-0025 §16.3)
					changed = true
				}
				l.Put(e)
				entry = cloneLedgerEntry(e)
				return changed, nil
			})
			if entry.Provider == "" {
				return
			}
			if _, bound := taskHasWorkspaceBinding(task, key); bound {
				return
			}
			if AddTaskWorkspaceBinding(vaultDir, slug, bindingForEntry(entry, workspace.RoleSecondary)) == nil {
				mu.Lock()
				rep.Bound = append(rep.Bound, key+"@"+slug)
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()
	sort.Strings(rep.Attached)
	sort.Strings(rep.Bound)
}

// refreshDueWorkspaces refreshes the status cache of ledger entries bound to live tasks (plus owned entries kept
// as orphaned_open_changes, so their landing is noticed) whose provider declares a non-zero status_refresh_seconds
// and whose cache is due, oldest first, at most MaxWorkspaceRefreshesPerTick per tick with
// MaxConcurrentWorkspaceRefreshes provider calls in flight.
func refreshDueWorkspaces(ctx context.Context, vaultDir string, now time.Time, rep *WorkspaceMaintenanceReport) {
	l, err := workspace.LoadLedger(vaultDir)
	if err != nil || len(l.Entries) == 0 {
		return
	}
	ix := loadWorkspaceTaskIndex(vaultDir)
	cadence := newWorkspaceRefreshSecondsCache(vaultDir)
	type due struct {
		key       string
		refreshed time.Time
	}
	var queue []due
	for key, e := range l.Entries {
		if len(ix.liveBinders(e)) == 0 && !(e.OrphanedOpenChanges && e.Ownership == workspace.OwnershipOwned) {
			continue
		}
		last, perr := time.Parse(time.RFC3339, strings.TrimSpace(e.Status.RefreshedAt))
		if perr == nil && now.Sub(last) < time.Duration(extension.MinWorkspaceStatusRefreshSeconds)*time.Second {
			continue // no pack cadence is shorter; skip without resolving the provider
		}
		secs, ok := cadence.get(e.Provider)
		if !ok || secs <= 0 {
			continue
		}
		if perr == nil && now.Sub(last) < time.Duration(secs)*time.Second {
			continue
		}
		queue = append(queue, due{key: key, refreshed: last})
	}
	if len(queue) == 0 {
		return
	}
	sort.Slice(queue, func(i, j int) bool {
		if !queue[i].refreshed.Equal(queue[j].refreshed) {
			return queue[i].refreshed.Before(queue[j].refreshed)
		}
		return queue[i].key < queue[j].key
	})
	if len(queue) > MaxWorkspaceRefreshesPerTick {
		queue = queue[:MaxWorkspaceRefreshesPerTick]
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, MaxConcurrentWorkspaceRefreshes)
	for _, d := range queue {
		if ctx.Err() != nil {
			break
		}
		e := l.Entries[d.key]
		wg.Add(1)
		go func(key string, e workspace.LedgerEntry) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if ctx.Err() != nil {
				return
			}
			info, _, callErr := fetchWorkspaceStatusContext(ctx, vaultDir, e)
			if ctx.Err() != nil && callErr != nil {
				return // tick budget expired; retry on the next tick instead of polluting status.error
			}
			_, applied, sent, aErr := applyWorkspaceStatusCASCounted(vaultDir, key, e.Status.RefreshedAt, info, callErr, now)
			mu.Lock()
			defer mu.Unlock()
			if aErr != nil || !applied {
				return
			}
			if callErr != nil && workspace.ErrorCode(callErr) != workspace.CodeNotFound {
				rep.RefreshErrors++
				return
			}
			rep.Refreshed++
			rep.LandedNotified += sent
		}(d.key, e)
	}
	wg.Wait()
}
