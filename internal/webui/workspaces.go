package webui

import (
	"fmt"
	"strings"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

// RFC-0025 Phase C console surfaces. Everything here renders the ledger cache already projected into
// DashboardState.Workspaces (workspace.SurfaceEntry): render paths never call a workspace provider.

const (
	// maxConsoleWorkspaceRows caps the regular rows of the console "Active Workspaces" table.
	maxConsoleWorkspaceRows = 20
	// maxConsoleOrphanRows caps the "Orphaned with open changes" rows.
	maxConsoleOrphanRows = 10
	// maxConsoleChangeRefs caps how many change references one table cell lists after its summary.
	maxConsoleChangeRefs = 2
)

// mdEscapeInline escapes Markdown control characters so ledger-derived display text cannot create links,
// emphasis, HTML, or extra table cells inside CONSOLE.md / hq_console.md.
func mdEscapeInline(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range strings.Join(strings.Fields(s), " ") {
		switch r {
		case '\\', '`', '*', '_', '[', ']', '<', '>', '|', '~', '#', '!':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// mdCode renders s as an inline code span that is safe inside a Markdown table cell.
func mdCode(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "—"
	}
	s = strings.ReplaceAll(s, "`", "'")
	s = strings.ReplaceAll(s, "|", "\\|")
	return "`" + s + "`"
}

// mdLinkURL makes an already-sanitized https URL safe as a Markdown link target inside a table cell.
func mdLinkURL(u string) string {
	if !strings.HasPrefix(u, "https://") {
		return ""
	}
	return strings.NewReplacer("(", "%28", ")", "%29", "|", "%7C", " ", "%20").Replace(u)
}

// surfaceEntriesByKey indexes the projected ledger entries by ledger key for Task Board badge lookups.
func surfaceEntriesByKey(entries []workspace.SurfaceEntry) map[string]workspace.SurfaceEntry {
	if len(entries) == 0 {
		return nil
	}
	m := make(map[string]workspace.SurfaceEntry, len(entries))
	for _, e := range entries {
		m[e.Key] = e
	}
	return m
}

// taskWorkspaceBadge renders the Task Board badge for a task's provider bindings, e.g.
// "🧩 acme-monorepo · 1 in review · 1 ⏳ needs you" (RFC-0025 §11.2). Git worktrees keep their own 🌿 badge, so
// core/git-worktree bindings are ignored here. It returns "" for tasks without provider bindings.
func taskWorkspaceBadge(t kernel.ActiveTask, byKey map[string]workspace.SurfaceEntry) string {
	var shorts []string
	seenShort := make(map[string]bool)
	var changes []workspace.SurfaceChange
	n := 0
	for _, b := range t.Workspaces {
		if strings.TrimSpace(b.Key) == "" || b.Provider == workspace.CoreGitProviderID {
			continue
		}
		n++
		short := workspace.ProviderShort(b.Provider)
		if short != "" && !seenShort[short] {
			seenShort[short] = true
			shorts = append(shorts, short)
		}
		if e, ok := byKey[b.Key]; ok {
			changes = append(changes, e.Changes...)
		}
	}
	if n == 0 {
		return ""
	}
	badge := "🧩 " + mdEscapeInline(strings.Join(shorts, "+"))
	if n > 1 {
		badge += fmt.Sprintf(" ×%d", n)
	}
	if summary := workspace.SummarizeChanges(changes); summary != "" {
		badge += " · " + summary
	}
	return badge
}

// consoleWorkspaceState renders the cached state enum with an icon and a refresh-failure flag.
func consoleWorkspaceState(e workspace.SurfaceEntry) string {
	icon := "❔"
	switch e.State {
	case workspace.StateClean:
		icon = "🟢"
	case workspace.StateDirty:
		icon = "🟠"
	case workspace.StateMissing:
		icon = "⚫"
	}
	cell := fmt.Sprintf("%s `%s`", icon, e.State)
	if e.RefreshError {
		cell += " · ⚠️ refresh failed"
	}
	return cell
}

// consoleWorkspaceOwners renders the owner cell: the writer (🔒 when the W1 lease is enforced), then up to two
// other bound tasks, then a "+N" overflow marker.
func consoleWorkspaceOwners(e workspace.SurfaceEntry) string {
	owners := e.Owners()
	if len(owners) == 0 {
		return "—"
	}
	var parts []string
	for i, o := range owners {
		if i >= 3 {
			parts = append(parts, fmt.Sprintf("+%d", len(owners)-i))
			break
		}
		part := mdCode(o)
		if o == e.WriterTask && e.LeaseEnforced {
			part += " 🔒"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// consoleWorkspaceChanges renders the changes cell: counts by state plus up to maxConsoleChangeRefs open change
// references with their sanitized titles (titles are allowed on the console, never in SessionStart).
func consoleWorkspaceChanges(e workspace.SurfaceEntry) string {
	cell := e.Summary
	if cell == "" {
		cell = "—"
	}
	var refs []string
	for _, c := range e.Changes {
		if !c.State.Open() {
			continue
		}
		if len(refs) >= maxConsoleChangeRefs {
			break
		}
		ref := mdCode(c.ID)
		if u := mdLinkURL(c.URL); u != "" {
			ref = "[" + ref + "](" + u + ")"
		}
		if c.Title != "" {
			ref += " " + mdEscapeInline(c.Title)
		}
		refs = append(refs, ref)
	}
	if len(refs) > 0 {
		cell += " — " + strings.Join(refs, "; ")
	}
	return cell
}

// formatConsoleWorkspacesSection renders the unified "## 🧩 3. Active Workspaces" console section covering
// both built-in core/git-worktree workspaces and external provider ledger entries, followed by
// "⚠️ Orphaned with open changes" rows for owned entries flagged orphaned_open_changes.
func formatConsoleWorkspacesSection(entries []workspace.SurfaceEntry) string {
	var b strings.Builder
	if len(entries) == 0 {
		b.WriteString("## 🧩 3. Active Workspaces (`0` Active)\n- None active.\n")
		return b.String()
	}
	var regular, orphans []workspace.SurfaceEntry
	openTotal, needsTotal := 0, 0
	for _, e := range entries {
		openTotal += e.OpenChanges
		needsTotal += e.NeedsOwner
		if e.Orphaned && e.Ownership == workspace.OwnershipOwned {
			orphans = append(orphans, e)
			continue
		}
		regular = append(regular, e)
	}
	fmt.Fprintf(&b, "## 🧩 3. Active Workspaces (`%d` Active · `%d` Open Change(s) · `%d` Needs You)\n", len(entries), openTotal, needsTotal)
	b.WriteString("| Provider | Workspace | Owner Task | State | Changes |\n| :--- | :--- | :--- | :--- | :--- |\n")
	row := func(providerCell string, e workspace.SurfaceEntry) {
		fmt.Fprintf(&b, "| %s | %s (%s) | %s | %s | %s |\n",
			providerCell, mdCode(e.DisplayName()), e.Ownership, consoleWorkspaceOwners(e), consoleWorkspaceState(e), consoleWorkspaceChanges(e))
	}
	for i, e := range regular {
		if i >= maxConsoleWorkspaceRows {
			break
		}
		row(mdCode(e.Provider), e)
	}
	for i, e := range orphans {
		if i >= maxConsoleOrphanRows {
			break
		}
		row("⚠️ Orphaned with open changes · "+mdCode(e.Provider), e)
	}
	hidden := 0
	if len(regular) > maxConsoleWorkspaceRows {
		hidden += len(regular) - maxConsoleWorkspaceRows
	}
	if len(orphans) > maxConsoleOrphanRows {
		hidden += len(orphans) - maxConsoleOrphanRows
	}
	if hidden > 0 {
		fmt.Fprintf(&b, "\n- *(+%d more: `hq workspace list`)*\n", hidden)
	}
	return b.String()
}
