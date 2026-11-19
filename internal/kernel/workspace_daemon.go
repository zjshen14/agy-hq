package kernel

import (
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
	// MaxConcurrentWorkspaceRefreshes bounds provider status calls in flight.
	MaxConcurrentWorkspaceRefreshes = 4
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

// RunWorkspaceMaintenance runs one daemon maintenance tick (see the file comment).
func RunWorkspaceMaintenance(vaultDir string, now time.Time) (WorkspaceMaintenanceReport, error) {
	var rep WorkspaceMaintenanceReport
	if strings.TrimSpace(vaultDir) == "" {
		return rep, nil
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
	if len(l.ProbeCandidates) > 0 {
		drainWorkspaceProbeCandidates(vaultDir, now, &rep)
	}
	refreshDueWorkspaces(vaultDir, now, &rep)
	return rep, nil
}

// drainWorkspaceProbeCandidates turns up to MaxWorkspaceProbesPerTick probe candidates into attached ledger
// entries (advisory writer lease) and secondary task bindings. Candidates that no trusted provider claims are
// dropped. Ledger and task updates run sequentially, never nested.
func drainWorkspaceProbeCandidates(vaultDir string, now time.Time, rep *WorkspaceMaintenanceReport) {
	cands, err := workspace.DrainProbeCandidates(vaultDir, MaxWorkspaceProbesPerTick)
	if err != nil || len(cands) == 0 {
		return
	}
	rep.ProbesDrained = len(cands)
	nowStr := now.UTC().Format(time.RFC3339)
	for _, c := range cands {
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
				continue
			}
			p, rErr := workspace.Resolve(vaultDir, fqid)
			if rErr != nil {
				continue
			}
			ctx, cancel := workspaceProviderContext()
			info, owned, pErr := p.Probe(ctx, c.Path)
			cancel()
			if pErr != nil || !owned {
				continue
			}
			info.Provider = fqid
			if info.WorkDir == "" {
				info.WorkDir = info.Root
			}
			if validateAcquiredWorkspaceInfo(vaultDir, info) != nil {
				continue
			}
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
					Provider:    fqid,
					WorkspaceID: info.WorkspaceID,
					Name:        info.Name,
					Root:        info.Root,
					WorkDir:     info.WorkDir,
					Ownership:   workspace.OwnershipAttached,
					CreatedAt:   nowStr,
					Status:      workspace.LedgerStatus{State: st},
				})
				created = true
				return true, nil
			})
			if uErr != nil {
				continue
			}
			if created {
				rep.Attached = append(rep.Attached, key)
			}
		}
		if !hasTask {
			continue
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
			continue
		}
		if _, bound := taskHasWorkspaceBinding(task, key); bound {
			continue
		}
		if AddTaskWorkspaceBinding(vaultDir, slug, bindingForEntry(entry, workspace.RoleSecondary)) == nil {
			rep.Bound = append(rep.Bound, key+"@"+slug)
		}
	}
}

// refreshDueWorkspaces refreshes the status cache of ledger entries bound to live tasks (plus owned entries kept
// as orphaned_open_changes, so their landing is noticed) whose provider declares a non-zero status_refresh_seconds
// and whose cache is due, oldest first, at most MaxWorkspaceRefreshesPerTick per tick with
// MaxConcurrentWorkspaceRefreshes provider calls in flight.
func refreshDueWorkspaces(vaultDir string, now time.Time, rep *WorkspaceMaintenanceReport) {
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
		e := l.Entries[d.key]
		wg.Add(1)
		go func(key string, e workspace.LedgerEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			info, callErr := fetchWorkspaceStatus(vaultDir, e)
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
