package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/webui"
	"github.com/zjshen14/agy-hq/internal/worktree"
)

func runWorktreeCLI(vaultDir string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: hq worktree <add|list|attach|remove|prune> ...")
		os.Exit(1)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: hq worktree add <repo-path-or-github-slug> <task-name> [--branch=<branch>] [--task=<owner-slug-or-conv-id>] [--no-attach]")
			os.Exit(1)
		}
		repoSpec := rest[0]
		taskName := rest[1]
		fs := flag.NewFlagSet("worktree add", flag.ExitOnError)
		branch := fs.String("branch", "", "Branch name (defaults to hq/<task-name>)")
		ownerTask := fs.String("task", "", "Optional owning task slug or conversation ID (defaults to $ANTIGRAVITY_CONVERSATION_ID)")
		noAttach := fs.Bool("no-attach", false, "Do not bind this worktree to an active Task Board entry")
		_ = fs.Parse(rest[2:])
		dir, err := worktree.Add(vaultDir, repoSpec, taskName, *branch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq worktree add failed: %v\n", err)
			os.Exit(1)
		}
		attachedSlug := ""
		if !*noAttach {
			if slug, ok := kernel.AttachWorktreeToTask(vaultDir, *ownerTask, dir, ""); ok {
				attachedSlug = slug
			}
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Created isolated worktree at %s\n", dir)
		if attachedSlug != "" {
			fmt.Printf("Attached worktree to task %q\n", attachedSlug)
		}
	case "list":
		fs := flag.NewFlagSet("worktree list", flag.ExitOnError)
		showStatus := fs.Bool("status", false, "Show owning task, branch, cleanliness, and merge status")
		asJSON := fs.Bool("json", false, "Output JSON status")
		_ = fs.Parse(rest)
		if *showStatus || *asJSON {
			statuses, err := kernel.InspectWorktrees(vaultDir, time.Now())
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq worktree list failed: %v\n", err)
				os.Exit(1)
			}
			if *asJSON {
				printJSON(statuses)
				return
			}
			for _, st := range statuses {
				owner := "(orphan)"
				if st.OwnerTaskSlug != "" {
					owner = fmt.Sprintf("%s (%s)", st.OwnerTaskSlug, st.OwnerStatus)
				}
				cleanTag := "clean"
				if !st.Clean {
					cleanTag = "dirty"
				}
				mergeTag := "unmerged"
				if st.MergedIntoMain {
					mergeTag = "merged"
				} else if st.PushedToOrigin {
					mergeTag = "unmerged,pushed"
				}
				fmt.Printf("%-42s  owner=%-32s  branch=%-30s  [%s, %s]\n", st.Name, owner, st.Branch, cleanTag, mergeTag)
			}
			return
		}
		list, err := worktree.List(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq worktree list failed: %v\n", err)
			os.Exit(1)
		}
		for _, d := range list {
			fmt.Println(d)
		}
	case "attach":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: hq worktree attach <worktree-name-or-path> [--task=<owner-slug-or-conv-id>] [--keep-status]")
			os.Exit(1)
		}
		targetWT := rest[0]
		if !filepath.IsAbs(targetWT) {
			targetWT = filepath.Join(vaultDir, "worktrees", filepath.Base(targetWT))
		}
		fs := flag.NewFlagSet("worktree attach", flag.ExitOnError)
		ownerTask := fs.String("task", "", "Owning task slug or conversation ID (defaults to $ANTIGRAVITY_CONVERSATION_ID)")
		keepStatus := fs.Bool("keep-status", false, "Do not reactivate a completed task when attaching")
		_ = fs.Parse(rest[1:])
		slug, ok := kernel.AttachWorktreeToTaskWithOptions(vaultDir, *ownerTask, targetWT, "", kernel.AttachWorktreeOptions{
			ReactivateCompleted: !*keepStatus,
		})
		if !ok {
			fmt.Fprintf(os.Stderr, "hq worktree attach failed: could not attach %s to task %q\n", targetWT, *ownerTask)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Attached worktree %s to task %q\n", filepath.Base(targetWT), slug)
	case "remove", "rm":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: hq worktree remove <worktree-name-or-path>")
			os.Exit(1)
		}
		if err := worktree.Remove(vaultDir, rest[0]); err != nil {
			fmt.Fprintf(os.Stderr, "hq worktree remove failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Removed worktree %s\n", rest[0])
	case "prune":
		fs := flag.NewFlagSet("worktree prune", flag.ExitOnError)
		dryRun := fs.Bool("dry-run", false, "Preview orphan worktrees eligible for pruning without deleting")
		force := fs.Bool("force", false, "Force removal of unlinked orphan worktrees even if unmerged or dirty (never touches linked hot tasks)")
		asJSON := fs.Bool("json", false, "Output JSON report")
		_ = fs.Parse(rest)
		rep, err := kernel.PruneOrphanWorktrees(vaultDir, kernel.PruneWorktreesOptions{
			Now:    time.Now(),
			MinAge: 0,
			DryRun: *dryRun,
			Force:  *force,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq worktree prune failed: %v\n", err)
			os.Exit(1)
		}
		if !*dryRun && len(rep.Pruned) > 0 {
			_ = webui.SyncMarkdownConsole(vaultDir)
		}
		if *asJSON {
			printJSON(rep)
			return
		}
		if *dryRun {
			fmt.Printf("[DRY-RUN] Scanned %d worktree(s): would prune %d, keep %d\n", rep.Scanned, len(rep.WouldPrune), len(rep.Kept))
			for _, name := range rep.WouldPrune {
				fmt.Printf("  - would prune: %s\n", name)
			}
		} else {
			fmt.Printf("Scanned %d worktree(s): pruned %d, kept %d\n", rep.Scanned, len(rep.Pruned), len(rep.Kept))
			for _, name := range rep.Pruned {
				fmt.Printf("  - pruned: %s\n", name)
			}
		}
		for _, k := range rep.Kept {
			if k.OwnerTaskSlug == "" {
				fmt.Printf("  - kept orphan: %s (%s)\n", k.Name, k.PruneAction)
			}
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown worktree subcommand: %s\n", sub)
		os.Exit(1)
	}

}

func runHeartbeatCLI(vaultDir string, args []string) {
	fs := flag.NewFlagSet("heartbeat", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "Simulate agentapi spawns without calling LLM")
	forceGatekeepers := fs.Bool("force-gatekeepers", false, "Evaluate all daemon_gatekeeper precheck_cmd triggers immediately regardless of cron minute")
	_ = fs.Parse(args)
	var extra []string
	if *forceGatekeepers {
		gkOut, gkSaves, _ := kernel.EvaluateDaemonGatekeepers(vaultDir, time.Now(), true, *dryRun)
		extra = append(extra, gkOut...)
		if gkSaves > 0 {
			extra = append(extra, fmt.Sprintf("gatekeeperSkips=%d", gkSaves))
		}
	}
	status, err := kernel.RunHeartbeat(vaultDir, *dryRun)
	_ = webui.SyncMarkdownConsole(vaultDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hq heartbeat error: %v\n", err)
		os.Exit(1)
	}
	if len(extra) > 0 {
		fmt.Printf("%s | forcedGatekeepers: %s\n", status, strings.Join(extra, "; "))
	} else {
		fmt.Println(status)
	}

}

func runClaimDeskCLI(vaultDir string, args []string) {
	fs := flag.NewFlagSet("claim-desk", flag.ExitOnError)
	convID := fs.String("conv", "", "Conversation ID to set as the single active Front Desk")
	artDir := fs.String("artifact-dir", "", "Optional artifact directory for the Front Desk")
	_ = fs.Parse(args)
	if *convID == "" {
		*convID = vault.ResolveConversationID()
	}
	if *convID == "" {
		fmt.Fprintln(os.Stderr, "usage: hq claim-desk --conv=<conversation-id> [--artifact-dir=...]")
		os.Exit(1)
	}
	if kernel.IsSubagentConversation(vaultDir, *convID) {
		fmt.Fprintln(os.Stderr, "error: cannot claim Front Desk lease for a child subagent")
		os.Exit(1)
	}
	currentConv := vault.ResolveConversationID()
	if currentConv != "" && kernel.IsSubagentConversation(vaultDir, currentConv) {
		fmt.Fprintln(os.Stderr, "error: cannot claim Front Desk lease for a child subagent")
		os.Exit(1)
	}
	if *artDir == "" {
		*artDir = vault.ResolveArtifactDir(*convID)
	}
	if prev, hadPrev := kernel.GetFrontDesk(vaultDir); hadPrev && prev.ConversationID != "" && prev.ConversationID != *convID {
		if _, alreadyTracked := kernel.FindTaskByConversationID(vaultDir, prev.ConversationID); !alreadyTracked {
			shortPrev := shortPrefix(prev.ConversationID, 8)
			_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
				TaskSlug:       "headquarter-front-desk-" + shortPrev,
				Title:          fmt.Sprintf("[Task:headquarter] Archived Front Desk Shift (%s)", shortPrev),
				ConversationID: prev.ConversationID,
				Status:         "completed",
				CreatedAt:      prev.UpdatedAt,
				LastUpdatedAt:  time.Now().Format(time.RFC3339),
				LastSummary:    fmt.Sprintf("Handed over Front Desk lease to %s", *convID),
			})
		}
	}
	if err := kernel.SetFrontDesk(vaultDir, *convID, *artDir); err != nil {
		fmt.Fprintf(os.Stderr, "hq claim-desk error: %v\n", err)
		os.Exit(1)
	}
	_ = webui.SyncMarkdownConsole(vaultDir)
	fmt.Printf("Claimed Front Desk intake lease for conversation %s\n", *convID)
	_, unarmed := kernel.GetUnarmedTriggers(vaultDir)
	artPath := filepath.Join(*artDir, "hq_console.md")
	fmt.Printf("  - Step 1 (Pin Console Artifact): call write_file with Content=\"{{HQ_SYNC}}\" to %s (ArtifactMetadata={UserFacing:true, RequestFeedback:false})\n", artPath)
	if len(unarmed) > 0 {
		fmt.Printf("  - Step 2 (Arm %d Proactive Schedule(s) from local/triggers.yaml):\n", len(unarmed))
		for _, s := range unarmed {
			fmt.Printf("      * %s: schedule(CronExpression=%q, IsDaemon=true, Prompt=%q)\n", s.ID, s.FiveFieldCron(), s.DefaultPrompt())
		}
	}
	fmt.Printf("  - Tip: %s\n", kernel.FrontDeskPinTip)

}

func runReleaseDeskCLI(vaultDir string) {
	prev, hadPrev := kernel.GetFrontDesk(vaultDir)
	if err := kernel.ReleaseFrontDesk(vaultDir); err != nil {
		fmt.Fprintf(os.Stderr, "hq release-desk error: %v\n", err)
		os.Exit(1)
	}
	_ = webui.SyncMarkdownConsole(vaultDir)
	fmt.Println("Released Front Desk intake lease (the next new conversation will automatically claim [HQ] Front Desk on Turn 1)")
	if hadPrev && len(prev.ArmedTriggers) > 0 {
		fmt.Printf("  Note: Outgoing Front Desk (%s) had %d armed schedule(s); call manage_task(Action=\"kill\") on active cron schedules in that thread to avoid duplicate triggers.\n", prev.ConversationID, len(prev.ArmedTriggers))
	}

}

func runAdoptCLI(vaultDir string, args []string) {
	fs := flag.NewFlagSet("adopt", flag.ExitOnError)
	convID := fs.String("conv", "", "Existing conversation ID to adopt into headquarter orchestration")
	taskSlug := fs.String("task", "", "Short task slug")
	title := fs.String("title", "", "Human-readable title for the task conversation")
	summary := fs.String("summary", "", "Current status/summary of the conversation")
	status := fs.String("status", "active", "Status (active or completed)")
	_ = fs.Parse(args)
	if *convID == "" || *taskSlug == "" {
		fmt.Fprintln(os.Stderr, "usage: hq adopt --conv=<conversation-id> --task=<slug> [--title=\"...\"] [--summary=\"...\"] [--status=active|completed]")
		os.Exit(1)
	}
	if *title == "" {
		*title = *taskSlug
	}
	if !strings.HasPrefix(*title, "[Task:") {
		scope := "general"
		if parts := strings.SplitN(*taskSlug, "-", 2); len(parts) > 0 && parts[0] != "" {
			scope = parts[0]
		}
		*title = fmt.Sprintf("[Task:%s] %s", scope, *title)
	}
	if fd, ok := kernel.GetFrontDesk(vaultDir); ok && fd.ConversationID == *convID {
		armedCount := len(fd.ArmedTriggers)
		_ = kernel.ReleaseFrontDesk(vaultDir)
		fmt.Printf("Released Front Desk lease from %s (next new conversation will automatically claim [HQ] Front Desk).\n", *convID)
		if armedCount > 0 {
			fmt.Printf("  Note: Outgoing Front Desk (%s) had %d armed schedule(s); call manage_task(Action=\"kill\") on active cron schedules in this thread.\n", *convID, armedCount)
		}
	}
	if err := kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:       *taskSlug,
		Title:          *title,
		Kind:           "task",
		ConversationID: *convID,
		LastUpdatedAt:  time.Now().Format(time.RFC3339),
		LastSummary:    *summary,
		Status:         *status,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "hq adopt error: %v\n", err)
		os.Exit(1)
	}
	_ = webui.SyncMarkdownConsole(vaultDir)
	fmt.Printf("Adopted conversation %s as task %q (%s)\n", *convID, *taskSlug, *status)

}

func runCompleteCLI(vaultDir string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: hq complete <task-slug-or-conv-id> [--summary=\"...\"]")
		os.Exit(1)
	}
	target := args[0]
	fs := flag.NewFlagSet("complete", flag.ExitOnError)
	summary := fs.String("summary", "", "Optional completion summary")
	_ = fs.Parse(args[1:])
	if err := kernel.CompleteTask(vaultDir, target, *summary); err != nil {
		fmt.Fprintf(os.Stderr, "hq complete error: %v\n", err)
		os.Exit(1)
	}
	_ = webui.SyncMarkdownConsole(vaultDir)
	fmt.Printf("Marked task %q as completed\n", target)

}

func runArchiveCLI(vaultDir string, args []string) {
	denyIfCallerTainted(vaultDir, "archive")
	if workspaceCallerBlockedAsAuto(vaultDir, "archive", os.Stderr) {
		os.Exit(1)
	}
	var target string
	force := false
	noSidebar := false
	for _, arg := range args {
		switch arg {
		case "--force", "-force", "-f":
			force = true
		case "--no-sidebar", "-no-sidebar":
			noSidebar = true
		default:
			if !strings.HasPrefix(arg, "-") && target == "" {
				target = arg
			}
		}
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "usage: hq archive <task-slug-or-conv-id> [--force] [--no-sidebar]")
		os.Exit(1)
	}
	syncRes, err := kernel.ArchiveTaskWithOptions(vaultDir, target, kernel.ArchiveTaskOptions{
		Force:     force,
		NoSidebar: noSidebar,
		WorkspaceNote: func(note string) {
			fmt.Printf("  workspace: %s\n", note)
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hq archive error: %v\n", err)
		os.Exit(1)
	}
	_, _ = vault.ArchiveProgress(vaultDir, target, time.Now().UTC())
	_ = webui.SyncMarkdownConsole(vaultDir)
	if syncRes.SkippedReason == "ui_pinned" || syncRes.SkippedReason == "ui_starred" {
		fmt.Fprintln(os.Stderr, "note: archived locally, but skipped Antigravity sidebar archive because conversation is pinned/starred in UI (unpin in UI or pass --force)")
	}
	fmt.Printf("Archived task %q (worktree cleaned up)\n", target)

}

func runSpawnCLI(rootDir, vaultDir string, args []string) {
	denyIfCallerTainted(vaultDir, "spawn")
	fs := flag.NewFlagSet("spawn", flag.ExitOnError)
	repoSpec := fs.String("repo", "", "Optional repo path or GitHub slug to create an isolated worktree for")
	workspaceSpec := fs.String("workspace", "", "Optional workspace provider FQID, optionally :<existing-workspace-id>, to create or attach an isolated provider workspace (mutually exclusive with --repo)")
	shareFrom := fs.String("share-from", "", "With --workspace=<provider-fqid>: create a shared-history sibling of this ledgered workspace (ledger key or workspace id)")
	taskName := fs.String("task", "", "Short task slug for worktree and conversation title")
	role := fs.String("role", "", "Optional subagent role when --subagent is set")
	parentConv := fs.String("parent", "", "Optional parent conversation ID when --subagent is set (defaults to ANTIGRAVITY_CONVERSATION_ID)")
	asSubagent := fs.Bool("subagent", false, "Launch as a nested child subagent under the parent conversation instead of a top-level sidebar thread")
	prompt := fs.String("prompt", "", "Task instructions for the new conversation")
	model := fs.String("model", "", "Optional model override (neon, hydrogen, flash, carbon, sonnet, opus, or full model ID)")
	depth := fs.String("depth", "", "Optional thinking depth override (low, medium, high, xhigh)")
	tier := fs.String("tier", "", "Optional complexity tier override (quick, routine, standard, heavy, deep-medium, deep)")
	titleOverride := fs.String("title", "", "Optional custom conversation title")
	scopeOverride := fs.String("scope", "", "Optional custom task scope")
	ephemeral := fs.Bool("ephemeral", false, "Mark task as ephemeral (excluded from nightly sleep-consolidate and RSI mining)")
	force := fs.Bool("force", false, "Force spawn even if quota circuit breaker is open")
	ignoreBudget := fs.Bool("ignore-budget", false, "Alias for --force to bypass daily token budget circuit breaker")
	dryRun := fs.Bool("dry-run", false, "Simulate agentapi spawn")
	_ = fs.Parse(args)
	if *prompt == "" || (*taskName == "" && *role == "") {
		fmt.Fprintln(os.Stderr, "usage: hq spawn --task=<slug> [--subagent] [--parent=<conv-id>] [--role=\"...\"] [--repo=<repo> | --workspace=<provider-fqid>[:<id>] [--share-from=<key-or-id>]] [--tier=quick|routine|standard|heavy|deep-medium|deep] [--model=...] [--depth=low|medium|high|xhigh] [--force] [--ignore-budget] --prompt=\"...\"")
		os.Exit(1)
	}
	if strings.TrimSpace(*workspaceSpec) != "" && strings.TrimSpace(*repoSpec) != "" {
		fmt.Fprintln(os.Stderr, "hq spawn: --workspace and --repo are mutually exclusive (--repo is the git worktree path)")
		os.Exit(1)
	}
	if strings.TrimSpace(*shareFrom) != "" && strings.TrimSpace(*workspaceSpec) == "" {
		fmt.Fprintln(os.Stderr, "hq spawn: --share-from requires --workspace=<provider-fqid>")
		os.Exit(1)
	}
	isForced := *force || *ignoreBudget
	resolvedModel, resolvedTier, _ := kernel.ResolveVaultModel(vaultDir, *prompt, *model, *depth, *tier)
	scope := resolveTaskScope(*scopeOverride, *repoSpec, *taskName)
	if *asSubagent {
		runSubagentSpawnCLI(rootDir, vaultDir, "hq spawn --subagent", *parentConv, *role, scope, *repoSpec, *workspaceSpec, *shareFrom, *taskName, *prompt, resolvedModel, resolvedTier, *dryRun, isForced)
		return
	}
	if strings.TrimSpace(*workspaceSpec) != "" {
		wsTitle := fmt.Sprintf("[Task:%s] %s (%s/%s)", scope, *taskName, resolvedTier, resolvedModel)
		if *titleOverride != "" {
			wsTitle = *titleOverride
		}
		if code := runWorkspaceSpawn(vaultDir, workspaceSpawnRequest{
			Spec:      *workspaceSpec,
			ShareFrom: *shareFrom,
			TaskSlug:  *taskName,
			Title:     wsTitle,
			Scope:     scope,
			Tier:      resolvedTier,
			Model:     resolvedModel,
			Prompt:    *prompt,
			Ephemeral: *ephemeral,
			DryRun:    *dryRun,
			Force:     isForced,
		}, os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return
	}
	fullPrompt := *prompt
	var wtDir string
	var repoRootPath string
	wtExistedBefore := false
	if *repoSpec != "" {
		prevWTs, _ := worktree.List(vaultDir)
		prevSet := make(map[string]bool, len(prevWTs))
		for _, p := range prevWTs {
			prevSet[filepath.Clean(p)] = true
		}
		wtTask := *taskName
		if wtTask == "" {
			_, _, _, _, wtTask = kernel.ParseSubagentRole(*role, resolvedModel)
		}
		var err error
		wtDir, err = worktree.Add(vaultDir, *repoSpec, wtTask, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq spawn worktree error: %v\n", err)
			os.Exit(1)
		}
		wtExistedBefore = prevSet[filepath.Clean(wtDir)]
		if info, err := os.Stat(*repoSpec); err == nil && info.IsDir() {
			if abs, err := filepath.Abs(*repoSpec); err == nil {
				repoRootPath = abs
			}
		}
		fullPrompt = fmt.Sprintf("Work inside the isolated git worktree at %s.\n\nTask: %s", wtDir, *prompt)
	}
	title := fmt.Sprintf("[Task:%s] %s (%s/%s)", scope, *taskName, resolvedTier, resolvedModel)
	if *titleOverride != "" {
		title = *titleOverride
	}
	createdAt := time.Now().Format(time.RFC3339)
	// Pre-register before SpawnTask so child SessionStart/Stop hooks can link immediately without race conditions.
	_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:      *taskName,
		Title:         title,
		Kind:          "task",
		Scope:         scope,
		Tier:          resolvedTier,
		Model:         resolvedModel,
		WorktreeDir:   wtDir,
		RepoRoot:      repoRootPath,
		CreatedAt:     createdAt,
		LastUpdatedAt: createdAt,
		LastSummary:   *prompt,
		Status:        "active",
		Ephemeral:     *ephemeral,
	})
	out, err := kernel.SpawnTask(vaultDir, "manual-spawn:"+*taskName, "swe-pulse", title, fullPrompt, resolvedModel, *dryRun, isForced)
	if err != nil {
		kernel.RemoveUnboundTaskBySlug(vaultDir, *taskName)
		if wtDir != "" && !wtExistedBefore {
			_ = worktree.Remove(vaultDir, wtDir)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Fprintf(os.Stderr, "hq spawn error: %v\n", err)
		os.Exit(1)
	}
	if spawnedID := kernel.ExtractUUID(out); spawnedID != "" {
		_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
			TaskSlug:       *taskName,
			Title:          title,
			Kind:           "task",
			Scope:          scope,
			Tier:           resolvedTier,
			Model:          resolvedModel,
			WorktreeDir:    wtDir,
			RepoRoot:       repoRootPath,
			ConversationID: spawnedID,
			CreatedAt:      createdAt,
			LastUpdatedAt:  time.Now().Format(time.RFC3339),
			LastSummary:    *prompt,
			Status:         "active",
		})
	}
	_ = webui.SyncMarkdownConsole(vaultDir)
	fmt.Println(out)

}

func runSubagentCLI(rootDir, vaultDir string, args []string) {
	denyIfCallerTainted(vaultDir, "subagent")
	fs := flag.NewFlagSet("subagent", flag.ExitOnError)
	role := fs.String("role", "", "Subagent role title, e.g. \"[deep] rfc-0005: Opus 5.5 Reviewer\"")
	parentConv := fs.String("parent", "", "Parent conversation ID (defaults to ANTIGRAVITY_CONVERSATION_ID)")
	repoSpec := fs.String("repo", "", "Optional repo path or GitHub slug to create an isolated worktree for")
	workspaceSpec := fs.String("workspace", "", "Optional workspace provider FQID, optionally :<existing-workspace-id>, to create or attach an isolated provider workspace (mutually exclusive with --repo)")
	shareFrom := fs.String("share-from", "", "With --workspace=<provider-fqid>: create a shared-history sibling of this ledgered workspace (ledger key or workspace id)")
	taskName := fs.String("task", "", "Optional worktree task slug")
	scopeOverride := fs.String("scope", "", "Optional custom task scope")
	prompt := fs.String("prompt", "", "Task instructions for the nested subagent")
	model := fs.String("model", "", "Optional model override (neon, hydrogen, flash, carbon, sonnet, opus, or full model ID)")
	depth := fs.String("depth", "", "Optional thinking depth override (low, medium, high, xhigh)")
	tier := fs.String("tier", "", "Optional complexity tier override (quick, routine, standard, heavy, deep-medium, deep)")
	force := fs.Bool("force", false, "Force spawn even if quota circuit breaker is open")
	ignoreBudget := fs.Bool("ignore-budget", false, "Alias for --force to bypass daily token budget circuit breaker")
	dryRun := fs.Bool("dry-run", false, "Simulate subagent spawn")
	_ = fs.Parse(args)
	if *prompt == "" || (*role == "" && *taskName == "") {
		fmt.Fprintln(os.Stderr, "usage: hq subagent --role=\"[<tier>] <scope>: <role>\" [--tier=quick|routine|standard|heavy|deep-medium|deep] [--model=...] [--depth=low|medium|high|xhigh] [--parent=<conv-id>] [--repo=<repo> | --workspace=<provider-fqid>[:<id>] [--share-from=<key-or-id>]] [--task=<slug>] [--force] [--ignore-budget] --prompt=\"...\"")
		os.Exit(1)
	}
	isForced := *force || *ignoreBudget
	resolvedModel, resolvedTier, _ := kernel.ResolveVaultModel(vaultDir, *prompt, *model, *depth, *tier)
	scope := resolveTaskScope(*scopeOverride, *repoSpec, *taskName)
	runSubagentSpawnCLI(rootDir, vaultDir, "hq subagent", *parentConv, *role, scope, *repoSpec, *workspaceSpec, *shareFrom, *taskName, *prompt, resolvedModel, resolvedTier, *dryRun, isForced)

}

func runTasksCLI(vaultDir string, args []string) {
	now := time.Now()
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "stats":
			fs := flag.NewFlagSet("tasks stats", flag.ExitOnError)
			asJSON := fs.Bool("json", false, "Output JSON")
			_ = fs.Parse(args[1:])
			stats := kernel.ComputeTaskStats(vaultDir, now)
			if *asJSON {
				printJSON(stats)
				return
			}
			fmt.Printf("HQ Task Board Telemetry & Storage Summary\n")
			fmt.Printf("  Hot Index (tasks.json):           %d tasks (active: %d, idle: %d, stalled: %d, completed: %d, cancelled: %d, archived: %d)\n",
				stats.HotTotal, stats.ByStatus["active"], stats.ByStatus["idle"], stats.ByStatus["stalled"], stats.ByStatus["completed"], stats.ByStatus["cancelled"], stats.ByStatus["archived"])
			fmt.Printf("  Cold Ledger (tasks_archive.jsonl): %d archived tasks\n", stats.ColdTotal)
			fmt.Printf("  By Kind:  task=%d, subagent=%d, auto=%d\n",
				stats.ByKind["task"], stats.ByKind["subagent"], stats.ByKind["auto"])
			if len(stats.ByTier) > 0 {
				fmt.Printf("  By Tier:  deep=%d, standard=%d, quick=%d\n",
					stats.ByTier["deep"], stats.ByTier["standard"], stats.ByTier["quick"])
			}
			return

		case "compact":
			fs := flag.NewFlagSet("tasks compact", flag.ExitOnError)
			dryRun := fs.Bool("dry-run", false, "Preview compaction without writing files")
			taskTTLStr := fs.String("task-ttl", "", "Override completed task auto-archive grace period (e.g. 24h)")
			noSidebar := fs.Bool("no-sidebar", false, "Skip Antigravity sidebar archive synchronization")
			noWorktreePrune := fs.Bool("no-worktree-prune", false, "Skip orphan git worktree pruning")
			asJSON := fs.Bool("json", false, "Output JSON")
			_ = fs.Parse(args[1:])
			var taskTTL time.Duration
			if strings.TrimSpace(*taskTTLStr) != "" {
				parsedTTL, pErr := time.ParseDuration(strings.TrimSpace(*taskTTLStr))
				if pErr != nil {
					fmt.Fprintf(os.Stderr, "hq tasks compact invalid --task-ttl %q: %v\n", *taskTTLStr, pErr)
					os.Exit(1)
				}
				taskTTL = parsedTTL
			}
			res, err := kernel.CompactTasks(vaultDir, kernel.CompactTasksOptions{
				Now:                  now,
				TaskAutoArchiveAfter: taskTTL,
				DryRun:               *dryRun,
				NoSidebar:            *noSidebar,
				NoWorktreePrune:      *noWorktreePrune,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq tasks compact error: %v\n", err)
				os.Exit(1)
			}
			if !*dryRun {
				var rotated []string
				rotated = append(rotated, res.AutoArchivedSlugs...)
				rotated = append(rotated, res.RotatedColdSlugs...)
				if len(rotated) > 0 {
					_ = vault.ArchiveProgressBatch(vaultDir, rotated, now)
				}
				_ = webui.SyncMarkdownConsole(vaultDir)
			}
			if *asJSON {
				printJSON(res)
				return
			}
			prefix := "Compacted"
			if *dryRun {
				prefix = "[DRY-RUN] Would compact"
			}
			fmt.Printf("%s tasks.json: %d -> %d hot tasks (auto-archived: %d, rotated to cold archive: %d, total cold archive: %d)\n",
				prefix, res.HotBefore, res.HotAfter, len(res.AutoArchivedSlugs), len(res.RotatedColdSlugs), res.TotalColdAfter)
			if len(res.PrunedWorktrees) > 0 {
				fmt.Printf("  worktree prune: %d orphan worktree(s) (%s)\n", len(res.PrunedWorktrees), strings.Join(res.PrunedWorktrees, ", "))
			}
			if len(res.PrunedWorkspaces) > 0 {
				fmt.Printf("  workspace prune: %d orphan provider workspace(s) (%s)\n", len(res.PrunedWorkspaces), strings.Join(res.PrunedWorkspaces, ", "))
			}
			if res.SidebarSyncedCount > 0 || res.SidebarQueuedCount > 0 || res.SidebarSkippedCount > 0 {
				fmt.Printf("  sidebar sync: synced=%d, queued=%d, skipped=%d\n",
					res.SidebarSyncedCount, res.SidebarQueuedCount, res.SidebarSkippedCount)
			}
			for _, slug := range res.RotatedColdSlugs {
				fmt.Printf("  - rotated: %s\n", slug)
			}
			return

		case "sync-sidebar":
			fs := flag.NewFlagSet("tasks sync-sidebar", flag.ExitOnError)
			dryRun := fs.Bool("dry-run", false, "Preview sidebar archive transitions without calling UpdateConversationAnnotations or mutating files")
			backfill := fs.Bool("backfill", false, "Include historical cold-archived tasks from tasks_archive.jsonl")
			limit := fs.Int("limit", kernel.DefaultSidebarBackfillLimit, "Maximum conversations to update per run (0 = unlimited)")
			batchSize := fs.Int("batch-size", kernel.DefaultSidebarBatchSize, "Batch size for UpdateConversationAnnotations cascadeIds")
			asJSON := fs.Bool("json", false, "Output structured JSON report")
			_ = fs.Parse(args[1:])

			rep, err := kernel.SyncSidebar(vaultDir, kernel.SyncSidebarOptions{
				Now:       now,
				DryRun:    *dryRun,
				Backfill:  *backfill,
				Limit:     *limit,
				BatchSize: *batchSize,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq tasks sync-sidebar error: %v\n", err)
				os.Exit(1)
			}
			if !*dryRun {
				_ = webui.SyncMarkdownConsole(vaultDir)
			}
			if *asJSON {
				printJSON(rep)
				if *backfill && !rep.LSOnline {
					fmt.Fprintf(os.Stderr, "hq tasks sync-sidebar --backfill requires an active Antigravity LanguageServerService session (ANTIGRAVITY_LS_ADDRESS / ANTIGRAVITY_CSRF_TOKEN): %s\n", rep.LastError)
					os.Exit(1)
				}
				return
			}
			if *backfill && !rep.LSOnline {
				fmt.Fprintf(os.Stderr, "hq tasks sync-sidebar --backfill requires an active Antigravity LanguageServerService session (ANTIGRAVITY_LS_ADDRESS / ANTIGRAVITY_CSRF_TOKEN): %s\n", rep.LastError)
				os.Exit(1)
			}
			header := "Antigravity Sidebar Archive Sync"
			if *dryRun {
				header = "[DRY-RUN] Antigravity Sidebar Archive Sync"
			}
			fmt.Printf("%s (lsOnline=%v, backfill=%v)\n", header, rep.LSOnline, rep.Backfill)
			if *dryRun {
				fmt.Printf("  Evaluated: %d | WouldArchive: %d | WouldUnarchive: %d | AlreadySynced: %d | ProtectedSkipped: %d | MissingOrSubtrajectory: %d | PendingQueue: %d\n",
					rep.CandidatesEvaluated, len(rep.WouldArchive), len(rep.WouldUnarchive), rep.AlreadySyncedCount, len(rep.ProtectedSkipped), rep.MissingOrSubtrajectoryCount, rep.PendingQueueRemaining)
				for _, c := range rep.WouldArchive {
					fmt.Printf("  + would archive: %s (%s) [%s]\n", c.ConversationID, c.TaskSlug, c.Source)
				}
				for _, c := range rep.WouldUnarchive {
					fmt.Printf("  + would unarchive: %s (%s) [%s]\n", c.ConversationID, c.TaskSlug, c.Source)
				}
			} else {
				fmt.Printf("  Evaluated: %d | Archived: %d | Unarchived: %d | AlreadySynced: %d | ProtectedSkipped: %d | MissingOrSubtrajectory: %d | PendingQueue: %d\n",
					rep.CandidatesEvaluated, rep.ArchivedCount, rep.UnarchivedCount, rep.AlreadySyncedCount, len(rep.ProtectedSkipped), rep.MissingOrSubtrajectoryCount, rep.PendingQueueRemaining)
			}
			for _, sk := range rep.ProtectedSkipped {
				fmt.Printf("  ! skipped [%s:%s]: %s (%s)\n", sk.Gate, sk.Reason, sk.ConversationID, sk.TaskSlug)
			}
			return

		case "pin", "unpin":
			pinned := args[0] == "pin"
			if len(args) < 2 {
				fmt.Fprintf(os.Stderr, "usage: hq tasks %s <task-slug-or-conv-id>\n", args[0])
				os.Exit(1)
			}
			target := args[1]
			if err := kernel.SetTaskPinned(vaultDir, target, pinned); err != nil {
				fmt.Fprintf(os.Stderr, "hq tasks %s error: %v\n", args[0], err)
				os.Exit(1)
			}
			_ = webui.SyncMarkdownConsole(vaultDir)
			if pinned {
				fmt.Printf("Pinned task %q to Tier 1 on the HQ Console\n", target)
			} else {
				fmt.Printf("Unpinned task %q\n", target)
			}
			return

		case "clean-nested":
			fs := flag.NewFlagSet("tasks clean-nested", flag.ExitOnError)
			asJSON := fs.Bool("json", false, "Output JSON")
			_ = fs.Parse(args[1:])
			reconciled, recErr := obs.ReconcileSubagentAttribution(vaultDir)
			if recErr != nil {
				fmt.Fprintf(os.Stderr, "hq tasks clean-nested reconcile error: %v\n", recErr)
				os.Exit(1)
			}
			removed, cleanErr := kernel.CleanNestedTasks(vaultDir)
			if cleanErr != nil {
				fmt.Fprintf(os.Stderr, "hq tasks clean-nested error: %v\n", cleanErr)
				os.Exit(1)
			}
			_ = webui.SyncMarkdownConsole(vaultDir)
			if *asJSON {
				printJSON(map[string]interface{}{
					"reconciledTasks": reconciled,
					"cleanedSlugs":    removed,
					"cleanedCount":    len(removed),
				})
				return
			}
			fmt.Printf("Cleaned %d leaked nested/workflow task(s) and reconciled %d attribution(s).\n", len(removed), reconciled)
			return

		default:
			fatalf("unknown tasks subcommand %q (expected stats, compact, sync-sidebar, pin, unpin, clean-nested; use 'hq reopen' to reopen a task)", args[0])
		}
	}

	fs := flag.NewFlagSet("tasks", flag.ExitOnError)
	activeOnly := fs.Bool("active", false, "Show only active, idle, and stalled tasks")
	allFlag := fs.Bool("all", false, "Include all hot and cold archived tasks")
	archiveOnly := fs.Bool("archive", false, "Show only archived tasks (from tasks.json and tasks_archive.jsonl)")
	kindFilter := fs.String("kind", "", "Filter by kind (task, subagent, auto)")
	scopeFilter := fs.String("scope", "", "Filter by scope (e.g. gmail, gchat, headquarter)")
	searchQuery := fs.String("search", "", "Case-insensitive substring search across slug, title, summary, and conversation ID")
	limit := fs.Int("limit", 0, "Maximum number of tasks to display (0 = default)")
	asJSON := fs.Bool("json", false, "Output matching tasks as JSON")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		fatalf("unknown tasks argument %q (expected flags or subcommand stats, compact, sync-sidebar, pin, unpin, clean-nested; use 'hq reopen' to reopen a task)", fs.Arg(0))
	}

	hotTasks, _ := kernel.ListTasks(vaultDir)
	layer1Tasks := kernel.FilterLayer1Tasks(vaultDir, hotTasks)
	coldCount := kernel.CountColdArchivedTasks(vaultDir)

	// If no filter flags are passed, display the tiered Console board view + stats footer
	if !*activeOnly && !*allFlag && !*archiveOnly && *kindFilter == "" && *scopeFilter == "" && *searchQuery == "" && *limit == 0 && !*asJSON {
		sel := kernel.SelectConsoleTasks(layer1Tasks, coldCount, now)
		fmt.Printf("HQ Task Board — %d Active | %d Idle | %d Completed (Hot) | %d Archived (Hot: %d, Cold: %d)\n",
			sel.ActiveCount, sel.IdleCount, sel.CompletedCount, sel.ArchivedHotCount+sel.ArchivedColdCount, sel.ArchivedHotCount, sel.ArchivedColdCount)
		fmt.Println(strings.Repeat("─", 88))
		if len(sel.VisibleRows) == 0 {
			fmt.Println("  No active or recent tasks.")
		}
		for _, t := range sel.VisibleRows {
			printTaskBoardLine(t, now)
		}
		if sel.HiddenIdleCount > 0 || sel.HiddenCompletedCount > 0 || sel.ArchivedHotCount > 0 || sel.ArchivedColdCount > 0 {
			fmt.Println(strings.Repeat("─", 88))
			fmt.Printf("  Hidden from default view: %d idle, %d older completed, %d archived. Run `./bin/hq tasks --all` or `--search=<q>`.\n",
				sel.HiddenIdleCount, sel.HiddenCompletedCount, sel.ArchivedHotCount+sel.ArchivedColdCount)
		}
		return
	}

	includeArchive := *allFlag || *archiveOnly || *searchQuery != ""
	filtered, err := kernel.FilterTasks(vaultDir, *activeOnly, *archiveOnly, *allFlag, includeArchive, *kindFilter, *scopeFilter, *searchQuery, *limit, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hq tasks error: %v\n", err)
		os.Exit(1)
	}

	if *asJSON {
		printJSON(filtered)
		return
	}

	fmt.Printf("Matching Tasks (%d):\n", len(filtered))
	for _, t := range filtered {
		printTaskBoardLine(t, now)
	}
}

func printTaskBoardLine(t kernel.ActiveTask, now time.Time) {
	eff := t.EffectiveStatus(now)
	badge := "🟢 active   "
	switch eff {
	case "idle":
		badge = "⏸️ idle     "
	case "stalled":
		badge = "⚠️ stalled  "
	case "completed":
		badge = "✅ completed"
	case "cancelled":
		badge = "🛑 cancelled"
	case "archived":
		badge = "📦 archived "
	}
	age := t.FormatRelativeAge(now)
	if age == "" {
		age = "-"
	}
	cleanTitle := kernel.SanitizeTaskTitle(t.Title, t.TaskSlug)
	pinMark := ""
	if t.Pinned {
		pinMark = " 📌"
	}
	convShort := shortPrefix(t.ConversationID, 8)
	if convShort == "" {
		convShort = "pending "
	}
	summary := strings.Join(strings.Fields(t.LastSummary), " ")
	if len(summary) > 90 {
		summary = summary[:87] + "..."
	}
	fmt.Printf("  %s  %-6s  %-28s  (%s · %-8s)%s\n", badge, age, t.TaskSlug, t.EffectiveKind(), convShort, pinMark)
	fmt.Printf("                Title:   %s\n", cleanTitle)
	if summary != "" {
		fmt.Printf("                Summary: %s\n", summary)
	}
}

func runSubagentSpawnCLI(rootDir, vaultDir, cmdLabel, parentConv, role, scope, repoSpec, workspaceSpec, shareFrom, taskName, prompt, resolvedModel, resolvedTier string, dryRun, isForced bool) {
	subRole := role
	if subRole == "" {
		subRole = fmt.Sprintf("[%s] %s: %s", resolvedTier, scope, taskName)
	}
	if strings.TrimSpace(shareFrom) != "" && strings.TrimSpace(workspaceSpec) == "" {
		fmt.Fprintf(os.Stderr, "%s: --share-from requires --workspace=<provider-fqid>\n", cmdLabel)
		os.Exit(1)
	}
	if strings.TrimSpace(workspaceSpec) != "" {
		if strings.TrimSpace(repoSpec) != "" {
			fmt.Fprintf(os.Stderr, "%s: --workspace and --repo are mutually exclusive (--repo is the git worktree path)\n", cmdLabel)
			os.Exit(1)
		}
		if code := runWorkspaceSubagentSpawn(vaultDir, workspaceSubagentRequest{
			CmdLabel:   cmdLabel,
			Spec:       workspaceSpec,
			ShareFrom:  shareFrom,
			ParentConv: parentConv,
			Role:       subRole,
			TaskName:   taskName,
			Prompt:     prompt,
			Model:      resolvedModel,
			WorkDir:    resolveDefaultWorkDir(rootDir),
			DryRun:     dryRun,
			Force:      isForced,
		}, os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return
	}
	fullPrompt := prompt
	workDir := resolveDefaultWorkDir(rootDir)
	if repoSpec != "" {
		wtSlug := taskName
		if wtSlug == "" {
			_, _, _, _, wtSlug = kernel.ParseSubagentRole(subRole, resolvedModel)
		}
		wtDir, err := worktree.Add(vaultDir, repoSpec, wtSlug, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s worktree error: %v\n", cmdLabel, err)
			os.Exit(1)
		}
		workDir = wtDir
		fullPrompt = fmt.Sprintf("Work inside the isolated git worktree at %s.\n\nTask: %s", wtDir, prompt)
	}
	out, err := kernel.SpawnSubagent(vaultDir, parentConv, subRole, fullPrompt, resolvedModel, workDir, dryRun, isForced)
	_ = webui.SyncMarkdownConsole(vaultDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s error: %v\n", cmdLabel, err)
		os.Exit(1)
	}
	fmt.Println(out)
}

func runReopenTaskCLI(vaultDir string, args []string) {
	var target string
	noWorktree := false
	noSidebar := false
	for _, arg := range args {
		switch arg {
		case "--no-worktree", "-no-worktree":
			noWorktree = true
		case "--no-sidebar", "-no-sidebar":
			noSidebar = true
		default:
			if !strings.HasPrefix(arg, "-") && target == "" {
				target = arg
			}
		}
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "usage: hq reopen <task-slug-or-conv-id> [--no-worktree] [--no-sidebar]")
		os.Exit(1)
	}
	if _, err := kernel.ReopenTaskWithOptions(vaultDir, target, kernel.ReopenTaskOptions{
		NoWorktree: noWorktree,
		NoSidebar:  noSidebar,
		WorkspaceNote: func(note string) {
			fmt.Printf("  workspace: %s\n", note)
		},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "hq reopen error: %v\n", err)
		os.Exit(1)
	}
	_ = webui.SyncMarkdownConsole(vaultDir)
	convID := ""
	matchedSlug := target
	if tasks, err := kernel.ListTasks(vaultDir); err == nil {
		for _, t := range tasks {
			if t.TaskSlug == target || t.ConversationID == target {
				convID = t.ConversationID
				matchedSlug = t.TaskSlug
				break
			}
		}
	}
	fmt.Printf("Reopened task %q (status: active, link: conversation://%s)\n", target, convID)
	if cp, _, found, _ := vault.LoadProgress(vaultDir, matchedSlug); found {
		fmt.Printf("  Progress checkpoint ready: %s (updated %s)\n", cp.Path, cp.UpdatedAt)
	}
}
