package hooks

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

// RFC-0025 Phase C hook integration: W1 single-writer lease, W3 Front Desk claim exclusion, Stop auto-binding,
// and the SessionStart workspace sections. Everything here reads the workspace ledger, tasks.json,
// nested_conversations.json, front_desk.json, and trust-verified pack manifests only: hooks never call a
// workspace provider and never spawn a subprocess.

const (
	// maxStopBindPaths caps how many workspace lookups one Stop hook performs over the paths written this turn
	// (RFC-0025 §7.3); Vault paths and paths under an already-resolved root do not count.
	maxStopBindPaths = 20
	// maxActiveWorkspaceLines caps the body of the SessionStart "## Active Workspaces" section.
	maxActiveWorkspaceLines = 15
	// maxTaskWorkspaceBindings caps the bindings listed in "## Active Task Workspaces".
	maxTaskWorkspaceBindings = 8
)

// hookInline makes a ledger- or task-derived value safe to embed in an inline code span of a hook message:
// whitespace is collapsed, backticks, pipes, and control characters are removed, and the result is clamped.
func hookInline(s string, maxRunes int) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '`' || r == '|':
			return -1
		case r < 0x20 || r == 0x7f:
			return ' '
		}
		return r
	}, s)
	out := trimInlineRunes(cleaned, maxRunes)
	if out == "" {
		return "?"
	}
	return out
}

// isTaskWriter reports whether the resolved owning task holds the writer lease recorded as writer (a task slug,
// or a conversation ID when a lease was handed to a conversation directly).
func isTaskWriter(owner kernel.ActiveTask, writer string) bool {
	writer = strings.TrimSpace(writer)
	if writer == "" {
		return false
	}
	return owner.TaskSlug == writer || (owner.ConversationID != "" && owner.ConversationID == writer)
}

// workspaceWriteLeaseDenial enforces the RFC-0025 W1 single-writer lease and explicit readonly bindings for
// write_file / edit_file (v1 scope, §16.3). An unreadable, corrupt, or newer-schema ledger fails closed for
// writes outside the Vault. Explicit readonly bindings are always enforced (even when lease_enforced=false);
// single-writer leases block non-writers when lease_enforced=true and the recorded writer task is still live.
func workspaceWriteLeaseDenial(vaultDir, convID, pathArg string) string {
	target := resolveHookLexicalAbs(vaultDir, pathArg)
	if target == "" {
		return ""
	}
	entry, ok, lErr := workspace.FindOwningEntryChecked(vaultDir, target)
	if lErr != nil {
		return "Blocked by agy-hq workspace hygiene (RFC-0025 W1): workspace ledger `local/memory/workspaces.json` is unreadable, corrupt, or from a newer schema version; refusing file edits outside the Vault until repaired."
	}
	if !ok {
		return ""
	}
	key := entry.Key()
	keyDisp := hookInline(key, 160)
	writer := strings.TrimSpace(entry.WriterTask)
	owner, resolved := kernel.ResolveOwningTask(vaultDir, convID)
	slug := ""
	if resolved {
		slug = strings.TrimSpace(owner.TaskSlug)
	}
	if resolved {
		for _, b := range owner.Workspaces {
			if b.Key != key || b.Role != workspace.RoleReadonly {
				continue
			}
			rootDisp := hookInline(entry.Root, 300)
			reason := fmt.Sprintf(
				"Blocked by agy-hq workspace hygiene (RFC-0025 W1): task `%s` is bound to workspace `%s` with role `readonly`, so it may read but not write files under `%s`. To write here, rebind the task with a writable role (`hq workspace adopt %s --task=%s --role=secondary`)",
				hookInline(slug, 128), keyDisp, rootDisp, rootDisp, hookInline(slug, 128))
			if writer != "" && !isTaskWriter(owner, writer) && kernel.IsLiveWorkspaceWriter(vaultDir, writer) {
				reason += fmt.Sprintf(" or route the edit to the writer task `%s`", hookInline(writer, 128))
			}
			return reason + "."
		}
	}
	if !entry.LeaseEnforced {
		return ""
	}
	if writer == "" || (resolved && isTaskWriter(owner, writer)) {
		return ""
	}
	if !kernel.IsLiveWorkspaceWriter(vaultDir, writer) {
		return ""
	}
	caller := "does not resolve to a tracked Layer-1 task"
	toSlug := "<your-task-slug>"
	findSlug := " Run `hq tasks` to find the task slug linked to this conversation (match its conversation ID); a conversation spawned moments ago may not be linked yet, so retry once before handing off."
	if resolved && slug != "" {
		caller = fmt.Sprintf("belongs to task `%s`", hookInline(slug, 128))
		toSlug = hookInline(slug, 128)
		findSlug = ""
	}
	writerDisp := hookInline(writer, 128)
	return fmt.Sprintf(
		"Blocked by agy-hq workspace single-writer lease (RFC-0025 W1): `%s` is inside workspace `%s`, whose writer lease is held by task `%s`, and this conversation %s. Only the writer task may edit files in this workspace.%s To take over, transfer the lease with `hq workspace handoff %s --to=%s`; otherwise route the edit to task `%s`.",
		hookInline(target, 300), keyDisp, writerDisp, caller, findSlug, keyDisp, toSlug, writerDisp)
}

// isProviderWorkspacePath reports whether the absolute path p lies inside a provider workspace: a ledger entry
// (workspace.FindOwningEntry) or a trusted pack's root_patterns (workspace.MatchRootPattern). Paths inside the
// Vault never match. Used by W3 so conversations opened inside provider workspaces never auto-claim the Front
// Desk lease.
func isProviderWorkspacePath(vaultDir, p string) bool {
	p = strings.TrimSpace(p)
	if p == "" || !filepath.IsAbs(p) || strings.TrimSpace(vaultDir) == "" {
		return false
	}
	if _, ok := workspace.FindOwningEntry(vaultDir, p); ok {
		return true
	}
	_, _, ok := workspace.MatchRootPattern(vaultDir, p)
	return ok
}

// autoBindWorkspacesFromStop implements RFC-0025 §7.3 / §16.8 for the task that owns convID (resolved through
// nested-subagent parents). For the paths written this turn (non-Vault paths, at most maxStopBindPaths lookups,
// deduplicated by workspace root):
//   - a ledger hit adds a `secondary` binding to the task (unless it already has one for that key) and adds the
//     task slug to the entry's bound_tasks, via two separate, non-nested pure updates (RankTasks, then
//     RankWorkspaces);
//   - a root_patterns hit with no ledger entry is queued with workspace.RecordProbeCandidate for the daemon.
//
// Lease-enforced entries held by another writer are skipped: W1 denied those writes, so they bind nothing.
// Archived or cancelled tasks are never bound.
func autoBindWorkspacesFromStop(vaultDir, convID string, written []string) {
	if len(written) == 0 {
		return
	}
	paths := make([]string, 0, len(written))
	seen := make(map[string]bool, len(written))
	for _, raw := range written {
		abs := resolveHookLexicalAbs(vaultDir, raw)
		if abs == "" || seen[abs] {
			continue
		}
		seen[abs] = true
		paths = append(paths, abs)
	}
	sort.Strings(paths)
	pathOwners := workspace.ResolvePathOwners(vaultDir, paths, maxStopBindPaths)
	if len(pathOwners) == 0 {
		return
	}
	owner, ok := kernel.ResolveOwningTask(vaultDir, convID)
	slug := strings.TrimSpace(owner.TaskSlug)
	if !ok || slug == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(owner.Status)) {
	case "archived", "cancelled":
		return
	}

	var bindings []workspace.Binding
	var candidates []string
	for _, po := range pathOwners {
		if !po.InLedger {
			candidates = append(candidates, po.Root)
			continue
		}
		e := po.Entry
		if e.LeaseEnforced && strings.TrimSpace(e.WriterTask) != "" && !isTaskWriter(owner, e.WriterTask) && kernel.IsLiveWorkspaceWriter(vaultDir, e.WriterTask) {
			continue
		}
		bindings = append(bindings, workspace.Binding{
			Key:      e.Key(),
			Provider: e.Provider,
			Root:     e.Root,
			WorkDir:  e.WorkDir,
			Role:     workspace.RoleSecondary,
		})
	}

	if len(bindings) > 0 {
		_ = kernel.UpdateTasks(vaultDir, func(tasks []kernel.ActiveTask) ([]kernel.ActiveTask, bool, error) {
			for i := range tasks {
				if tasks[i].TaskSlug != slug {
					continue
				}
				existing := make(map[string]bool, len(tasks[i].Workspaces))
				for _, b := range tasks[i].Workspaces {
					existing[b.Key] = true
				}
				var add []workspace.Binding
				for _, b := range bindings {
					if !existing[b.Key] {
						add = append(add, b)
					}
				}
				if len(add) == 0 {
					return tasks, false, nil
				}
				tasks[i].Workspaces = workspace.MergeBindings(tasks[i].Workspaces, add)
				return tasks, true, nil
			}
			return tasks, false, nil
		})
		_ = workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
			changed := false
			for _, b := range bindings {
				e, found := l.Entries[b.Key]
				if !found {
					continue
				}
				already := false
				for _, t := range e.BoundTasks {
					if t == slug {
						already = true
						break
					}
				}
				if already {
					continue
				}
				e.BoundTasks = append(e.BoundTasks, slug)
				l.Entries[b.Key] = e
				changed = true
			}
			return changed, nil
		})
	}
	for _, root := range candidates {
		_ = workspace.RecordProbeCandidate(vaultDir, root, slug)
	}
}

// isLiveTask reports whether t still counts as live for workspace surfacing (not archived or cancelled).
func isLiveTask(t kernel.ActiveTask, now time.Time) bool {
	switch t.EffectiveStatus(now) {
	case "archived", "cancelled":
		return false
	}
	return strings.TrimSpace(t.TaskSlug) != ""
}

// workspaceStateLabel renders the cached state enum, flagging a failed last refresh without its error text.
func workspaceStateLabel(s workspace.SurfaceEntry) string {
	label := fmt.Sprintf("state `%s`", s.State)
	if s.RefreshError {
		label += " (last refresh failed)"
	}
	return label
}

// workspaceCountsLabel renders change counts by state (IDs, enums, and counts only; never titles or URLs).
func workspaceCountsLabel(s workspace.SurfaceEntry) string {
	if s.Summary == "" {
		return "changes: none"
	}
	return "changes: " + s.Summary
}

// formatActiveWorkspacesSection renders the SessionStart "## Active Workspaces" section for ledger entries only.
func formatActiveWorkspacesSection(l workspace.Ledger, layer1 []kernel.ActiveTask, now time.Time) string {
	return formatUnifiedActiveWorkspacesSection(l, nil, layer1, now)
}

// formatUnifiedActiveWorkspacesSection renders the unified SessionStart "## Active Workspaces" section:
// managed git worktrees plus provider ledger entries bound to live (non-archived, non-cancelled) Layer-1
// tasks, grouped by provider, with at most maxActiveWorkspaceLines body lines plus a
// "(+N more: hq workspace list)" footer. Provider entries show IDs, enums, and counts only (RFC-0025 §11.3).
// It returns "" when there are no git worktrees and no live-task-bound ledger entries.
func formatUnifiedActiveWorkspacesSection(l workspace.Ledger, gitEntries []workspace.SurfaceEntry, layer1 []kernel.ActiveTask, now time.Time) string {
	if len(l.Entries) == 0 && len(gitEntries) == 0 {
		return ""
	}
	var live []kernel.ActiveTask
	liveSlugs := make(map[string]bool)
	for _, t := range layer1 {
		if isLiveTask(t, now) && !liveSlugs[t.TaskSlug] {
			liveSlugs[t.TaskSlug] = true
			live = append(live, t)
		}
	}
	type row struct {
		s      workspace.SurfaceEntry
		owners []string
		isGit  bool
	}
	var providers []string
	groups := make(map[string][]row)
	if len(live) > 0 && len(l.Entries) > 0 {
		keys := make([]string, 0, len(l.Entries))
		for k := range l.Entries {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			e := l.Entries[key]
			var owners []string
			addOwner := func(slug string) {
				slug = strings.TrimSpace(slug)
				if slug == "" || !liveSlugs[slug] {
					return
				}
				for _, o := range owners {
					if o == slug {
						return
					}
				}
				owners = append(owners, slug)
			}
			addOwner(e.WriterTask)
			for _, t := range live {
				for _, b := range t.Workspaces {
					if b.Key == key {
						addOwner(t.TaskSlug)
						break
					}
				}
			}
			for _, t := range e.BoundTasks {
				addOwner(t)
			}
			if len(owners) == 0 {
				continue
			}
			s := workspace.NewSurfaceEntry(e)
			if _, ok := groups[s.Provider]; !ok {
				providers = append(providers, s.Provider)
			}
			groups[s.Provider] = append(groups[s.Provider], row{s: s, owners: owners})
		}
	}
	for _, ge := range gitEntries {
		if _, ok := groups[ge.Provider]; !ok {
			providers = append(providers, ge.Provider)
		}
		var owners []string
		if ge.WriterTask != "" {
			owners = []string{ge.WriterTask}
		}
		groups[ge.Provider] = append(groups[ge.Provider], row{s: ge, owners: owners, isGit: true})
	}
	if len(providers) == 0 {
		return ""
	}
	sort.Strings(providers)
	total := 0
	for _, p := range providers {
		total += len(groups[p])
	}
	var lines []string
	used, shown := 0, 0
	fits := func(line string, reserve int) bool {
		return len(lines)+1+reserve <= maxActiveWorkspaceLines && used+len(line)+1 <= maxActiveWorkspaceBytes
	}
	add := func(line string) {
		lines = append(lines, line)
		used += len(line) + 1
	}
	for _, p := range providers {
		rows := groups[p]
		header := fmt.Sprintf("- **%s** (%d)", hookInline(p, 128), len(rows))
		if !fits(header, 1) {
			break
		}
		headerAdded := false
		for _, r := range rows {
			var line string
			if r.isGit {
				parts := []string{fmt.Sprintf("`%s` (%s)", hookInline(r.s.DisplayName(), 128), r.s.Ownership)}
				if len(r.owners) > 0 {
					parts = append(parts, fmt.Sprintf("task `%s`", hookInline(r.owners[0], 128)))
				}
				if safeRoot := workspace.SafeInlinePath(r.s.Root); safeRoot != "" {
					parts = append(parts, fmt.Sprintf("(%s)", safeRoot))
				}
				line = "  - " + strings.Join(parts, " · ")
			} else {
				var ownerParts []string
				for _, o := range r.owners {
					part := fmt.Sprintf("`%s`", hookInline(o, 128))
					if o == r.s.WriterTask {
						if r.s.LeaseEnforced {
							part += " (writer, lease enforced)"
						} else {
							part += " (writer)"
						}
					}
					ownerParts = append(ownerParts, part)
				}
				label := "task"
				if len(ownerParts) > 1 {
					label = "tasks"
				}
				line = fmt.Sprintf("  - `%s` (%s) · %s %s · %s · %s",
					hookInline(r.s.DisplayName(), 128), r.s.Ownership, label, strings.Join(ownerParts, ", "), workspaceStateLabel(r.s), workspaceCountsLabel(r.s))
			}
			reserve := 0
			if !headerAdded {
				reserve = 1
			}
			if !fits(line, reserve) || (!headerAdded && used+len(header)+len(line)+2 > maxActiveWorkspaceBytes) {
				break
			}
			if !headerAdded {
				add(header)
				headerAdded = true
			}
			add(line)
			shown++
		}
		if !headerAdded {
			break
		}
	}
	if omitted := total - shown; omitted > 0 {
		lines = append(lines, fmt.Sprintf(activeWorkspacesFooterFormat, omitted))
	}
	return "## Active Workspaces\n" + strings.Join(lines, "\n")
}

// activeWorkspacesFooterFormat is the overflow footer of "## Active Workspaces"; trimSectionPreservingFooter keeps
// it when the session budget trims the section.
const activeWorkspacesFooterFormat = "- *(+%d more: hq workspace list)*"

// maxActiveWorkspaceBytes bounds the body of "## Active Workspaces" (the footer is appended on top).
const maxActiveWorkspaceBytes = 1600

// formatActiveTaskWorkspacesSection renders the SessionStart "## Active Task Workspaces" section for a matched
// non-Front-Desk task: each provider binding's provider, work dir, role, cached state, open-change count, and
// needs-owner count, so the binding survives context compaction (RFC-0025 §9.2). IDs, enums, and counts only.
func formatActiveTaskWorkspacesSection(l workspace.Ledger, task kernel.ActiveTask) string {
	var bindings []workspace.Binding
	for _, b := range task.Workspaces {
		if strings.TrimSpace(b.Key) == "" || b.Provider == workspace.CoreGitProviderID {
			continue
		}
		bindings = append(bindings, b)
	}
	if len(bindings) == 0 {
		return ""
	}
	var lines []string
	for i, b := range bindings {
		if i >= maxTaskWorkspaceBindings {
			lines = append(lines, fmt.Sprintf("- *(+%d more: hq workspace list)*", len(bindings)-i))
			break
		}
		role := b.Role
		switch role {
		case workspace.RolePrimary, workspace.RoleSecondary, workspace.RoleReadonly:
		default:
			role = "unknown"
		}
		e, inLedger := l.Entries[b.Key]
		workDir := b.WorkDir
		stateLabel := "state `unknown` (not in ledger)"
		open, needs := 0, 0
		if inLedger {
			s := workspace.NewSurfaceEntry(e)
			stateLabel = workspaceStateLabel(s)
			open, needs = s.OpenChanges, s.NeedsOwner
			if e.WorkDir != "" {
				workDir = e.WorkDir
			} else if workDir == "" {
				workDir = e.Root
			}
		}
		if workDir == "" {
			workDir = b.Root
		}
		lines = append(lines, fmt.Sprintf("- `%s` · provider `%s` · role `%s` · %s · open changes `%d` · needs you `%d`",
			hookInline(b.Key, 160), hookInline(b.Provider, 128), role, stateLabel, open, needs))
		if safe := workspace.SafeInlinePath(workDir); safe != "" {
			lines = append(lines, fmt.Sprintf("  - work dir: `%s`", safe))
		} else {
			lines = append(lines, "  - work dir: *(withheld: unsafe characters; run `hq workspace list`)*")
		}
		if inLedger && e.LeaseEnforced && strings.TrimSpace(e.WriterTask) != "" && !isTaskWriter(task, e.WriterTask) {
			lines = append(lines, fmt.Sprintf("  - ⚠️ writer lease held by task `%s`: edits here are denied until `hq workspace handoff %s --to=%s`",
				hookInline(e.WriterTask, 128), hookInline(b.Key, 160), hookInline(task.TaskSlug, 128)))
		}
	}
	return "## Active Task Workspaces\n" + trimBytesPreservingLines(strings.Join(lines, "\n"), 1200)
}
