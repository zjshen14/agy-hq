package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/webui"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

// Spawn seams: tests substitute these to simulate spawn failures without a language server.
var (
	spawnTaskFn     = kernel.SpawnTask
	spawnSubagentFn = kernel.SpawnSubagent
	syncConsoleFn   = webui.SyncMarkdownConsole
)

const workspaceUsage = `usage: hq workspace <subcommand> [flags]

  providers [--json]                                     List registered workspace providers and their trust status
  list [--provider=<fqid>] [--all] [--json]              Ledger workspaces + git worktrees (--all also pages provider list)
  create <provider-fqid> <name> [--task=<slug-or-conv>] [--share-from=<key-or-workspace-id>]
                                                         Create an hq-owned workspace (bound to the task when given;
                                                         --share-from asks for a shared-history sibling of a ledgered one)
  adopt <path> [--task=<slug-or-conv>] [--role=primary|secondary|readonly]
                                                         Attach the existing workspace that owns <path>
  status [<key>] [--refresh] [--json]                    Cached status (--refresh asks the provider)
  handoff <key> --to=<slug-or-conv>                      Transfer the single-writer lease to another task
  unbind <key> [--task=<slug-or-conv>]                   Unbind a workspace from a task (the workspace is kept)
  remove <key> [--force]                                 Remove an hq-owned workspace (--force requires approval)
  own <key>                                              Promote an attached workspace to hq-owned (requires approval)
  prune [--dry-run] [--json]                             Remove clean, fully landed owned workspaces no live task uses

--task defaults to the task that owns $ANTIGRAVITY_CONVERSATION_ID. Keys look like <provider-fqid>:<workspace-id>.`

type workspaceArgs struct {
	pos   []string
	vals  map[string]string
	bools map[string]bool
}

func (a workspaceArgs) val(name string) string { return strings.TrimSpace(a.vals[name]) }

// parseWorkspaceArgs accepts flags anywhere (before or after positionals) as --name=value, --name value, or
// -name; bool flags also accept --name=true|false.
func parseWorkspaceArgs(args []string, valueFlags, boolFlags []string) (workspaceArgs, error) {
	isVal := make(map[string]bool, len(valueFlags))
	for _, f := range valueFlags {
		isVal[f] = true
	}
	isBool := make(map[string]bool, len(boolFlags))
	for _, f := range boolFlags {
		isBool[f] = true
	}
	out := workspaceArgs{vals: make(map[string]string), bools: make(map[string]bool)}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out.pos = append(out.pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			out.pos = append(out.pos, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		val, hasVal := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val, hasVal = k, v, true
		}
		switch {
		case isVal[name]:
			if !hasVal {
				if i+1 >= len(args) {
					return out, fmt.Errorf("flag --%s needs a value", name)
				}
				i++
				val = args[i]
			}
			out.vals[name] = val
		case isBool[name]:
			b := true
			if hasVal {
				parsed, err := strconv.ParseBool(val)
				if err != nil {
					return out, fmt.Errorf("flag --%s expects true or false", name)
				}
				b = parsed
			}
			out.bools[name] = b
		default:
			return out, fmt.Errorf("unknown flag %s", a)
		}
	}
	return out, nil
}

func workspaceCallerBlockedByTaint(vaultDir, verb string, stderr io.Writer) bool {
	if rec, ok := checkCallerTaintedEnv(vaultDir); ok {
		fmt.Fprintf(stderr, "hq workspace %s blocked: conversation %s is quarantined (%s)\n", verb, rec.ConvID, rec.Source)
		return true
	}
	return false
}

// workspaceCallerIsAutoWorker reports whether the calling conversation is (or is nested under) an [Auto] worker.
func workspaceCallerIsAutoWorker(vaultDir string) bool {
	conv := vault.ResolveConversationID()
	if conv == "" {
		return false
	}
	if t, ok := kernel.FindTaskByConversationID(vaultDir, conv); ok && t.EffectiveKind() == "auto" {
		return true
	}
	t, ok := kernel.ResolveOwningTask(vaultDir, conv)
	return ok && t.EffectiveKind() == "auto"
}

func workspaceCallerBlockedAsAuto(vaultDir, verb string, stderr io.Writer) bool {
	if workspaceCallerIsAutoWorker(vaultDir) {
		fmt.Fprintf(stderr, "hq workspace %s denied: unattended [Auto] workers cannot create, remove, or take ownership of workspaces; stage the action with hq propose for human review\n", verb)
		return true
	}
	return false
}

// resolveWorkspaceTaskFlag returns the live task for an explicit --task, or the task owning the caller conversation
// when --task is omitted (found=false when there is none).
func resolveWorkspaceTaskFlag(vaultDir, explicit string) (kernel.ActiveTask, bool, error) {
	explicit = strings.TrimSpace(explicit)
	target := explicit
	if target == "" {
		target = vault.ResolveConversationID()
		if target == "" {
			return kernel.ActiveTask{}, false, nil
		}
	}
	t, ok := kernel.ResolveOwningTask(vaultDir, target)
	if !ok {
		if explicit == "" {
			return kernel.ActiveTask{}, false, nil
		}
		return kernel.ActiveTask{}, false, fmt.Errorf("task %q not found (or it is the Front Desk)", explicit)
	}
	if strings.EqualFold(strings.TrimSpace(t.Status), "archived") {
		if explicit == "" {
			return kernel.ActiveTask{}, false, nil
		}
		return kernel.ActiveTask{}, false, fmt.Errorf("task %q is archived (reopen it first)", t.TaskSlug)
	}
	return t, true, nil
}

func validWorkspaceKey(key string) bool {
	provider, id, ok := workspace.ParseLedgerKey(strings.TrimSpace(key))
	return ok && provider != "" && workspace.ValidWorkspaceID(id)
}

func encodeWorkspaceJSON(w io.Writer, v interface{}) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func workspaceChangesSummary(row kernel.WorkspaceInventoryRow) string {
	if len(row.Changes) == 0 {
		return "-"
	}
	s := fmt.Sprintf("%d open", row.OpenChanges)
	if row.NeedsOwner > 0 {
		s += fmt.Sprintf(" (%d needs you)", row.NeedsOwner)
	}
	if closed := len(row.Changes) - row.OpenChanges; closed > 0 {
		s += fmt.Sprintf(", %d landed/abandoned", closed)
	}
	return s
}

func printWorkspaceRows(w io.Writer, rows []kernel.WorkspaceInventoryRow) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No workspaces.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tOWNERSHIP\tWRITER\tSTATE\tCHANGES\tWORK DIR")
	for _, r := range rows {
		writer := r.WriterTask
		if writer == "" {
			writer = "-"
		} else if r.LeaseEnforced {
			writer += "*"
		}
		state := string(r.State)
		if state == "" {
			state = string(workspace.StateUnknown)
		}
		if r.Error != "" {
			state += "!"
		}
		if r.OrphanedOpenChanges {
			state += " (orphaned)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Key, r.Ownership, writer, state, workspaceChangesSummary(r), r.WorkDir)
	}
	_ = tw.Flush()
	fmt.Fprintln(w, "(* = single-writer lease enforced, ! = last status refresh failed)")
}

func printWorkspaceEntry(w io.Writer, e workspace.LedgerEntry) {
	r := kernel.WorkspaceRowFromEntry(e)
	fmt.Fprintf(w, "%s\n", r.Key)
	writer := "-"
	if r.WriterTask != "" {
		writer = r.WriterTask
		if r.LeaseEnforced {
			writer += " (lease enforced)"
		} else {
			writer += " (advisory)"
		}
	}
	fmt.Fprintf(w, "  provider: %s  ownership: %s  writer: %s\n", r.Provider, r.Ownership, writer)
	fmt.Fprintf(w, "  root: %s  work_dir: %s\n", r.Root, r.WorkDir)
	if len(r.BoundTasks) > 0 {
		fmt.Fprintf(w, "  bound tasks: %s\n", strings.Join(r.BoundTasks, ", "))
	}
	refreshed := r.RefreshedAt
	if refreshed == "" {
		refreshed = "never"
	}
	fmt.Fprintf(w, "  state: %s  refreshed: %s\n", r.State, refreshed)
	if len(r.Changes) > 0 {
		var parts []string
		for _, c := range r.Changes {
			p := c.ID + " " + string(c.State)
			if c.NeedsOwner && c.State.Open() {
				p += " (needs you)"
			}
			parts = append(parts, p)
		}
		fmt.Fprintf(w, "  changes: %s\n", strings.Join(parts, ", "))
	}
	if r.Error != "" {
		fmt.Fprintf(w, "  error: %s\n", r.Error)
	}
	if r.OrphanedOpenChanges {
		fmt.Fprintln(w, "  warning: orphaned with open changes (no live task is bound)")
	}
}

func workspaceOwnProposal(hqCmd, key string) string {
	return fmt.Sprintf("%s propose --type=workspace_own --summary=\"Promote attached workspace %s to owned\" --cmd='hq workspace own %s'", hqCmd, key, key)
}

// runWorkspaceCLI implements `hq workspace ...` (RFC-0025 §12, Home placement only) and returns the exit code.
func runWorkspaceCLI(rootDir, vaultDir string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, workspaceUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "help", "--help", "-h":
		fmt.Fprintln(stdout, workspaceUsage)
		return 0
	case "providers":
		return runWorkspaceProviders(vaultDir, rest, stdout, stderr)
	case "list", "ls":
		return runWorkspaceList(vaultDir, rest, stdout, stderr)
	case "create":
		return runWorkspaceCreate(vaultDir, rest, stdout, stderr)
	case "adopt":
		return runWorkspaceAdopt(vaultDir, rest, stdout, stderr)
	case "status":
		return runWorkspaceStatus(vaultDir, rest, stdout, stderr)
	case "handoff":
		return runWorkspaceHandoff(vaultDir, rest, stdout, stderr)
	case "unbind":
		return runWorkspaceUnbind(vaultDir, rest, stdout, stderr)
	case "remove":
		return runWorkspaceRemove(rootDir, vaultDir, rest, stdout, stderr)
	case "own":
		return runWorkspaceOwn(vaultDir, rest, stdout, stderr)
	case "prune":
		return runWorkspacePrune(vaultDir, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown workspace subcommand: %s\n%s\n", sub, workspaceUsage)
		return 2
	}
}

func workspaceUsageError(stderr io.Writer, line string) int {
	fmt.Fprintf(stderr, "usage: hq workspace %s\n", line)
	return 2
}

func runWorkspaceProviders(vaultDir string, args []string, stdout, stderr io.Writer) int {
	a, err := parseWorkspaceArgs(args, nil, []string{"json"})
	if err != nil || len(a.pos) != 0 {
		return workspaceUsageError(stderr, "providers [--json]")
	}
	provs := workspace.ListProviders(vaultDir)
	if a.bools["json"] {
		encodeWorkspaceJSON(stdout, provs)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tSOURCE\tTRUST\tCAPABILITIES\tREFRESH\tSKILL")
	for _, p := range provs {
		source := "builtin"
		if !p.Builtin {
			source = "pack:" + p.PackID
		}
		trust := "trusted"
		if !p.Trusted {
			trust = "disabled: " + p.Reason
		}
		var caps []string
		if p.Capabilities.Create {
			caps = append(caps, "create")
		}
		if p.Capabilities.Remove {
			caps = append(caps, "remove")
		}
		if p.Capabilities.Changes {
			caps = append(caps, "changes")
		}
		if p.Capabilities.SharedHistory {
			caps = append(caps, "shared_history")
		}
		refresh := "on demand"
		if p.StatusRefreshSeconds > 0 {
			refresh = fmt.Sprintf("%ds", p.StatusRefreshSeconds)
		}
		skill := p.Skill
		if skill == "" {
			skill = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.FQID, source, trust, strings.Join(caps, ","), refresh, skill)
	}
	_ = tw.Flush()
	return 0
}

func runWorkspaceList(vaultDir string, args []string, stdout, stderr io.Writer) int {
	a, err := parseWorkspaceArgs(args, []string{"provider"}, []string{"all", "json"})
	if err != nil || len(a.pos) != 0 {
		return workspaceUsageError(stderr, "list [--provider=<fqid>] [--all] [--json]")
	}
	if workspaceCallerBlockedByTaint(vaultDir, "list", stderr) {
		return 1
	}
	rows, warnings, err := kernel.WorkspaceInventory(vaultDir, kernel.WorkspaceInventoryOptions{
		Provider:   a.val("provider"),
		All:        a.bools["all"],
		IncludeGit: true,
		Now:        time.Now(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "hq workspace list failed: %v\n", err)
		return 1
	}
	for _, w := range warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	if a.bools["json"] {
		if rows == nil {
			rows = []kernel.WorkspaceInventoryRow{}
		}
		encodeWorkspaceJSON(stdout, rows)
		return 0
	}
	printWorkspaceRows(stdout, rows)
	return 0
}

func printWorkspaceAcquired(w io.Writer, verb, taskSlug string, acq kernel.WorkspaceAcquireResult) {
	fmt.Fprintf(w, "%s workspace %s (%s) at %s\n", verb, acq.Entry.Key(), acq.Entry.Ownership, acq.Entry.WorkDir)
	if acq.Binding.Key != "" && taskSlug != "" {
		lease := "single-writer lease enforced"
		if acq.Binding.Role == workspace.RoleReadonly {
			lease = "readonly, no lease"
		}
		fmt.Fprintf(w, "Bound to task %q as %s (%s)\n", taskSlug, acq.Binding.Role, lease)
	}
	if acq.Skill != "" {
		fmt.Fprintf(w, "Provider guide: load the `%s` skill before running VCS commands.\n", acq.Skill)
	}
}

func runWorkspaceCreate(vaultDir string, args []string, stdout, stderr io.Writer) int {
	a, err := parseWorkspaceArgs(args, []string{"task", "share-from"}, nil)
	if err != nil || len(a.pos) != 2 {
		return workspaceUsageError(stderr, "create <provider-fqid> <name> [--task=<slug-or-conv>] [--share-from=<key-or-workspace-id>]")
	}
	if workspaceCallerBlockedByTaint(vaultDir, "create", stderr) || workspaceCallerBlockedAsAuto(vaultDir, "create", stderr) {
		return 1
	}
	fqid, name := strings.TrimSpace(a.pos[0]), strings.TrimSpace(a.pos[1])
	task, hasTask, err := resolveWorkspaceTaskFlag(vaultDir, a.val("task"))
	if err != nil {
		fmt.Fprintf(stderr, "hq workspace create failed: %v\n", err)
		return 1
	}
	opts := kernel.WorkspaceAcquireOptions{Provider: fqid, Name: name, ShareFrom: a.val("share-from"), Now: time.Now()}
	if hasTask {
		opts.TaskSlug = task.TaskSlug
		opts.Role = kernel.DefaultWorkspaceBindingRole(task)
		opts.LeaseEnforced = true
		opts.BindTask = true
	}
	acq, err := kernel.AcquireWorkspace(vaultDir, opts)
	if err != nil {
		fmt.Fprintf(stderr, "hq workspace create failed: %v\n", err)
		return 1
	}
	_ = syncConsoleFn(vaultDir)
	verb := "Created"
	if !acq.Created {
		verb = "Recorded existing"
	}
	printWorkspaceAcquired(stdout, verb, opts.TaskSlug, acq)
	return 0
}

func runWorkspaceAdopt(vaultDir string, args []string, stdout, stderr io.Writer) int {
	a, err := parseWorkspaceArgs(args, []string{"task", "role"}, nil)
	if err != nil || len(a.pos) != 1 {
		return workspaceUsageError(stderr, "adopt <path> [--task=<slug-or-conv>] [--role=primary|secondary|readonly]")
	}
	if workspaceCallerBlockedByTaint(vaultDir, "adopt", stderr) {
		return 1
	}
	path, err := filepath.Abs(a.pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "hq workspace adopt failed: %v\n", err)
		return 1
	}
	task, hasTask, err := resolveWorkspaceTaskFlag(vaultDir, a.val("task"))
	if err != nil {
		fmt.Fprintf(stderr, "hq workspace adopt failed: %v\n", err)
		return 1
	}
	opts := kernel.WorkspaceAdoptOptions{Path: path, Role: a.val("role"), Now: time.Now()}
	if hasTask {
		opts.TaskSlug = task.TaskSlug
	} else if opts.Role != "" {
		fmt.Fprintln(stderr, "hq workspace adopt: --role needs a task (pass --task=<slug-or-conv>)")
		return 2
	}
	acq, err := kernel.AdoptWorkspace(vaultDir, opts)
	if err != nil {
		fmt.Fprintf(stderr, "hq workspace adopt failed: %v\n", err)
		return 1
	}
	_ = syncConsoleFn(vaultDir)
	printWorkspaceAcquired(stdout, "Adopted", opts.TaskSlug, acq)
	return 0
}

func runWorkspaceStatus(vaultDir string, args []string, stdout, stderr io.Writer) int {
	a, err := parseWorkspaceArgs(args, nil, []string{"refresh", "json"})
	if err != nil || len(a.pos) > 1 {
		return workspaceUsageError(stderr, "status [<key>] [--refresh] [--json]")
	}
	if workspaceCallerBlockedByTaint(vaultDir, "status", stderr) {
		return 1
	}
	key := ""
	if len(a.pos) == 1 {
		key = strings.TrimSpace(a.pos[0])
		if !validWorkspaceKey(key) {
			fmt.Fprintf(stderr, "hq workspace status: invalid key %q (expected <provider-fqid>:<workspace-id>)\n", key)
			return 2
		}
	}
	exit := 0
	if a.bools["refresh"] {
		var keys []string
		if key != "" {
			keys = []string{key}
		} else if l, lErr := workspace.LoadLedger(vaultDir); lErr == nil {
			for k := range l.Entries {
				keys = append(keys, k)
			}
			sort.Strings(keys)
		}
		for _, r := range kernel.RefreshWorkspaces(vaultDir, keys, kernel.MaxConcurrentWorkspaceRefreshes, time.Now()) {
			if r.Err != nil {
				fmt.Fprintf(stderr, "warning: refresh %s: %v\n", r.Key, r.Err)
				if workspace.ErrorCode(r.Err) == workspace.CodeNotFound && key != "" {
					exit = 1
				}
			}
		}
		_ = syncConsoleFn(vaultDir)
	}
	l, err := workspace.LoadLedger(vaultDir)
	if err != nil {
		fmt.Fprintf(stderr, "hq workspace status failed: %v\n", err)
		return 1
	}
	if key != "" {
		e, ok := l.Entries[key]
		if !ok {
			fmt.Fprintf(stderr, "hq workspace status: workspace %s is not in the ledger\n", key)
			return 1
		}
		if a.bools["json"] {
			encodeWorkspaceJSON(stdout, kernel.WorkspaceRowFromEntry(e))
		} else {
			printWorkspaceEntry(stdout, e)
		}
		return exit
	}
	keys := make([]string, 0, len(l.Entries))
	for k := range l.Entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if a.bools["json"] {
		rows := make([]kernel.WorkspaceInventoryRow, 0, len(keys))
		for _, k := range keys {
			rows = append(rows, kernel.WorkspaceRowFromEntry(l.Entries[k]))
		}
		encodeWorkspaceJSON(stdout, rows)
		return exit
	}
	if len(keys) == 0 {
		fmt.Fprintln(stdout, "No provider workspaces in the ledger (git worktrees: hq worktree list --status).")
		return exit
	}
	for _, k := range keys {
		printWorkspaceEntry(stdout, l.Entries[k])
	}
	return exit
}

func runWorkspaceHandoff(vaultDir string, args []string, stdout, stderr io.Writer) int {
	a, err := parseWorkspaceArgs(args, []string{"to"}, nil)
	if err != nil || len(a.pos) != 1 || a.val("to") == "" {
		return workspaceUsageError(stderr, "handoff <key> --to=<slug-or-conv>")
	}
	if workspaceCallerBlockedByTaint(vaultDir, "handoff", stderr) {
		return 1
	}
	key := strings.TrimSpace(a.pos[0])
	slug, err := kernel.HandoffWorkspace(vaultDir, key, a.val("to"))
	if err != nil {
		fmt.Fprintf(stderr, "hq workspace handoff failed: %v\n", err)
		return 1
	}
	_ = syncConsoleFn(vaultDir)
	fmt.Fprintf(stdout, "Handed off workspace %s to task %q (single-writer lease enforced)\n", key, slug)
	return 0
}

func runWorkspaceUnbind(vaultDir string, args []string, stdout, stderr io.Writer) int {
	a, err := parseWorkspaceArgs(args, []string{"task"}, nil)
	if err != nil || len(a.pos) != 1 {
		return workspaceUsageError(stderr, "unbind <key> [--task=<slug-or-conv>]")
	}
	if workspaceCallerBlockedByTaint(vaultDir, "unbind", stderr) {
		return 1
	}
	key := strings.TrimSpace(a.pos[0])
	target := a.val("task")
	if target == "" {
		target = vault.ResolveConversationID()
	}
	slug, err := kernel.UnbindWorkspace(vaultDir, key, target)
	if err != nil {
		fmt.Fprintf(stderr, "hq workspace unbind failed: %v\n", err)
		return 1
	}
	_ = syncConsoleFn(vaultDir)
	fmt.Fprintf(stdout, "Unbound workspace %s from task %q (the workspace itself is kept)\n", key, slug)
	return 0
}

func runWorkspaceRemove(rootDir, vaultDir string, args []string, stdout, stderr io.Writer) int {
	a, err := parseWorkspaceArgs(args, nil, []string{"force"})
	if err != nil || len(a.pos) != 1 {
		return workspaceUsageError(stderr, "remove <key> [--force]")
	}
	if workspaceCallerBlockedByTaint(vaultDir, "remove", stderr) || workspaceCallerBlockedAsAuto(vaultDir, "remove", stderr) {
		return 1
	}
	key := strings.TrimSpace(a.pos[0])
	if !validWorkspaceKey(key) {
		fmt.Fprintf(stderr, "hq workspace remove: invalid key %q (expected <provider-fqid>:<workspace-id>)\n", key)
		return 2
	}
	res, err := kernel.RemoveWorkspace(vaultDir, key, a.bools["force"], time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "hq workspace remove failed: %v\n", err)
		return 1
	}
	if res.NeedsApproval {
		fmt.Fprintf(stderr, "hq workspace remove: %s needs human approval before deletion (%s).\n", key, res.Reason)
		fmt.Fprintln(stderr, "Stage it with:")
		fmt.Fprintf(stderr, "  %s\n", kernel.WorkspaceRemoveProposal(displayHQCmd(rootDir), key, res.Reason))
		return 1
	}
	_ = syncConsoleFn(vaultDir)
	switch {
	case res.Forgotten:
		fmt.Fprintf(stdout, "Workspace %s no longer exists; dropped its ledger entry\n", key)
	default:
		fmt.Fprintf(stdout, "Removed workspace %s\n", key)
	}
	if len(res.UnboundTasks) > 0 {
		fmt.Fprintf(stdout, "Unbound from task(s): %s\n", strings.Join(res.UnboundTasks, ", "))
	}
	return 0
}

func runWorkspaceOwn(vaultDir string, args []string, stdout, stderr io.Writer) int {
	a, err := parseWorkspaceArgs(args, nil, nil)
	if err != nil || len(a.pos) != 1 {
		return workspaceUsageError(stderr, "own <key>")
	}
	if workspaceCallerBlockedByTaint(vaultDir, "own", stderr) || workspaceCallerBlockedAsAuto(vaultDir, "own", stderr) {
		return 1
	}
	key := strings.TrimSpace(a.pos[0])
	if !validWorkspaceKey(key) {
		fmt.Fprintf(stderr, "hq workspace own: invalid key %q (expected <provider-fqid>:<workspace-id>)\n", key)
		return 2
	}
	_, changed, err := kernel.OwnWorkspace(vaultDir, key)
	if err != nil {
		fmt.Fprintf(stderr, "hq workspace own failed: %v\n", err)
		return 1
	}
	_ = syncConsoleFn(vaultDir)
	if changed {
		fmt.Fprintf(stdout, "Workspace %s is now hq-owned (removable by hq)\n", key)
	} else {
		fmt.Fprintf(stdout, "Workspace %s is already hq-owned\n", key)
	}
	return 0
}

func runWorkspacePrune(vaultDir string, args []string, stdout, stderr io.Writer) int {
	a, err := parseWorkspaceArgs(args, nil, []string{"dry-run", "json"})
	if err != nil || len(a.pos) != 0 {
		return workspaceUsageError(stderr, "prune [--dry-run] [--json]")
	}
	if workspaceCallerBlockedByTaint(vaultDir, "prune", stderr) {
		return 1
	}
	rep, err := kernel.PruneOrphanWorkspaces(vaultDir, kernel.PruneWorkspacesOptions{Now: time.Now(), DryRun: a.bools["dry-run"]})
	if err != nil {
		fmt.Fprintf(stderr, "hq workspace prune failed: %v\n", err)
		return 1
	}
	if !a.bools["dry-run"] && (len(rep.Pruned) > 0 || len(rep.Forgotten) > 0) {
		_ = syncConsoleFn(vaultDir)
	}
	if a.bools["json"] {
		encodeWorkspaceJSON(stdout, rep)
		return 0
	}
	if a.bools["dry-run"] {
		fmt.Fprintf(stdout, "[DRY-RUN] Scanned %d workspace(s): would prune %d, keep %d\n", rep.Scanned, len(rep.WouldPrune), len(rep.Kept))
		for _, k := range rep.WouldPrune {
			fmt.Fprintf(stdout, "  - would prune: %s\n", k)
		}
	} else {
		fmt.Fprintf(stdout, "Scanned %d workspace(s): pruned %d, kept %d\n", rep.Scanned, len(rep.Pruned), len(rep.Kept))
		for _, k := range rep.Pruned {
			fmt.Fprintf(stdout, "  - pruned: %s\n", k)
		}
	}
	for _, k := range rep.Forgotten {
		fmt.Fprintf(stdout, "  - gone (ledger entry dropped): %s\n", k)
	}
	for _, k := range rep.Kept {
		if k.Action != "keep:linked" {
			fmt.Fprintf(stdout, "  - kept: %s (%s)\n", k.Key, k.Action)
		}
	}
	return 0
}

// ---------------------------------------------------------------------------------------------------------------
// hq spawn --workspace / hq subagent --workspace (Home placement)

// workspaceSpawnRequest carries the already-resolved `hq spawn` inputs for a provider workspace spawn.
type workspaceSpawnRequest struct {
	Spec      string
	ShareFrom string // create a shared-history sibling of this ledgered workspace (key or id)
	TaskSlug  string
	Title     string
	Scope     string
	Tier      string
	Model     string
	Prompt    string
	Ephemeral bool
	DryRun    bool
	Force     bool
}

func workspaceSpawnPrompt(acq kernel.WorkspaceAcquireResult, prompt string) string {
	role := acq.Binding.Role
	if role == "" {
		role = workspace.RolePrimary
	}
	return kernel.WorkspacePromptPreamble(acq.Entry.Provider, acq.Entry.WorkspaceID, role, acq.Entry.WorkDir, acq.Skill) + "\n\nTask: " + prompt
}

// runWorkspaceSpawn implements `hq spawn --workspace=<fqid>[:<id>]`: it creates (owned) or attaches the workspace,
// pre-registers the task with its primary binding before SpawnTask (the RFC-0007 race fix), and on spawn failure
// rolls back the binding and removes only a workspace this call created. The conversation still starts in the
// verified Vault project (resolveSpawnTarget is unchanged).
func runWorkspaceSpawn(vaultDir string, req workspaceSpawnRequest, stdout, stderr io.Writer) int {
	slug := strings.TrimSpace(req.TaskSlug)
	if slug == "" {
		fmt.Fprintln(stderr, "hq spawn --workspace requires --task=<slug>")
		return 1
	}
	fqid, id, err := kernel.ParseWorkspaceSpec(req.Spec)
	if err != nil {
		fmt.Fprintf(stderr, "hq spawn workspace error: %v\n", err)
		return 1
	}
	if strings.TrimSpace(req.ShareFrom) != "" && id != "" {
		fmt.Fprintln(stderr, "hq spawn workspace error: --share-from creates a new sibling workspace and cannot be combined with --workspace=<provider>:<id>")
		return 1
	}
	acq, err := kernel.AcquireWorkspace(vaultDir, kernel.WorkspaceAcquireOptions{
		Provider:      fqid,
		WorkspaceID:   id,
		Name:          slug,
		ShareFrom:     req.ShareFrom,
		TaskSlug:      slug,
		Role:          workspace.RolePrimary,
		LeaseEnforced: true,
		Now:           time.Now(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "hq spawn workspace error: %v\n", err)
		return 1
	}
	fullPrompt := workspaceSpawnPrompt(acq, req.Prompt)
	bindings := []workspace.Binding{acq.Binding}
	createdAt := time.Now().Format(time.RFC3339)
	// Pre-register before SpawnTask so child SessionStart/Stop hooks can link immediately without race conditions.
	_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:      slug,
		Title:         req.Title,
		Kind:          "task",
		Scope:         req.Scope,
		Tier:          req.Tier,
		Model:         req.Model,
		CreatedAt:     createdAt,
		LastUpdatedAt: createdAt,
		LastSummary:   req.Prompt,
		Status:        "active",
		Ephemeral:     req.Ephemeral,
		Workspaces:    bindings,
	})
	out, err := spawnTaskFn(vaultDir, "manual-spawn:"+slug, "swe-pulse", req.Title, fullPrompt, req.Model, req.DryRun, req.Force)
	spawnedID := kernel.ExtractUUID(out)
	if spawnedID != "" {
		_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
			TaskSlug:       slug,
			Title:          req.Title,
			Kind:           "task",
			Scope:          req.Scope,
			Tier:           req.Tier,
			Model:          req.Model,
			ConversationID: spawnedID,
			CreatedAt:      createdAt,
			LastUpdatedAt:  time.Now().Format(time.RFC3339),
			LastSummary:    req.Prompt,
			Status:         "active",
			Workspaces:     bindings,
		})
	} else if err != nil {
		kernel.RemoveUnboundTaskBySlug(vaultDir, slug)
		if rbErr := kernel.RollbackAcquiredWorkspace(vaultDir, acq, slug); rbErr != nil {
			fmt.Fprintf(stderr, "warning: workspace rollback incomplete: %v\n", rbErr)
		}
	}
	_ = syncConsoleFn(vaultDir)
	if err != nil {
		fmt.Fprintf(stderr, "hq spawn error: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, out)
	return 0
}

// workspaceSubagentRequest carries the resolved `hq subagent --workspace` / `hq spawn --subagent --workspace` inputs.
type workspaceSubagentRequest struct {
	CmdLabel   string
	Spec       string
	ShareFrom  string // create a shared-history sibling of this ledgered workspace (key or id)
	ParentConv string
	Role       string // already defaulted subagent role
	TaskName   string
	Prompt     string
	Model      string
	WorkDir    string
	DryRun     bool
	Force      bool
}

// runWorkspaceSubagentSpawn creates or attaches the workspace for a subagent (Home placement: the subagent still
// runs in the default workspace project). A Front Desk parent makes the new Layer-1 subagent task the writer and
// primary binder; a task parent makes its owning Layer-1 task the writer with a secondary binding. Bindings are
// recorded after SpawnSubagent registers the task; a failed spawn rolls the workspace back.
func runWorkspaceSubagentSpawn(vaultDir string, req workspaceSubagentRequest, stdout, stderr io.Writer) int {
	fqid, id, err := kernel.ParseWorkspaceSpec(req.Spec)
	if err != nil {
		fmt.Fprintf(stderr, "%s workspace error: %v\n", req.CmdLabel, err)
		return 1
	}
	if strings.TrimSpace(req.ShareFrom) != "" && id != "" {
		fmt.Fprintf(stderr, "%s workspace error: --share-from creates a new sibling workspace and cannot be combined with --workspace=<provider>:<id>\n", req.CmdLabel)
		return 1
	}
	parent := strings.TrimSpace(req.ParentConv)
	if parent == "" {
		parent = vault.ResolveConversationID()
	}
	_, _, _, _, derivedSlug := kernel.ParseSubagentRole(req.Role, req.Model)
	ownerSlug, role := "", workspace.RolePrimary
	if kernel.IsFrontDeskConversation(vaultDir, parent) {
		ownerSlug = derivedSlug
	} else if t, ok := kernel.ResolveOwningTask(vaultDir, parent); ok && !strings.EqualFold(strings.TrimSpace(t.Status), "archived") {
		// Nested subagents work for their owning Layer-1 task, which keeps its own primary workspace.
		ownerSlug, role = t.TaskSlug, workspace.RoleSecondary
	}
	name := strings.TrimSpace(req.TaskName)
	if name == "" {
		name = derivedSlug
	}
	if id == "" {
		if err := workspace.ValidateCreateName(name); err != nil {
			fmt.Fprintf(stderr, "%s workspace error: %v (pass --task=<slug> to name the workspace)\n", req.CmdLabel, err)
			return 1
		}
	}
	acq, err := kernel.AcquireWorkspace(vaultDir, kernel.WorkspaceAcquireOptions{
		Provider:      fqid,
		WorkspaceID:   id,
		Name:          name,
		ShareFrom:     req.ShareFrom,
		TaskSlug:      ownerSlug,
		Role:          role,
		LeaseEnforced: ownerSlug != "",
		Now:           time.Now(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "%s workspace error: %v\n", req.CmdLabel, err)
		return 1
	}
	promptRole := workspace.RolePrimary
	fullPrompt := kernel.WorkspacePromptPreamble(acq.Entry.Provider, acq.Entry.WorkspaceID, promptRole, acq.Entry.WorkDir, acq.Skill) + "\n\nTask: " + req.Prompt
	out, err := spawnSubagentFn(vaultDir, req.ParentConv, req.Role, fullPrompt, req.Model, req.WorkDir, req.DryRun, req.Force)
	if err != nil {
		if rbErr := kernel.RollbackAcquiredWorkspace(vaultDir, acq, ownerSlug); rbErr != nil {
			fmt.Fprintf(stderr, "warning: workspace rollback incomplete: %v\n", rbErr)
		}
		_ = syncConsoleFn(vaultDir)
		fmt.Fprintf(stderr, "%s error: %v\n", req.CmdLabel, err)
		return 1
	}
	if ownerSlug != "" && acq.Binding.Key != "" {
		_ = kernel.AddTaskWorkspaceBinding(vaultDir, ownerSlug, acq.Binding)
	}
	_ = syncConsoleFn(vaultDir)
	fmt.Fprintln(stdout, out)
	return 0
}
