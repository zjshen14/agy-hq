package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/doctor"
	"github.com/zjshen14/agy-hq/internal/gmail"
	"github.com/zjshen14/agy-hq/internal/hooks"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/webui"
	"github.com/zjshen14/agy-hq/internal/worktree"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	rootDir, err := vault.ResolveRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hq: resolve root: %v\n", err)
		os.Exit(1)
	}
	vaultDir, err := vault.ResolveVaultDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hq: resolve vault: %v\n", err)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "init":
		templateDir := filepath.Join(rootDir, "templates")
		created, err := vault.Init(vaultDir, templateDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq init failed: %v\n", err)
			os.Exit(1)
		}
		if convID := vault.ResolveConversationID(); convID != "" {
			if _, ok := kernel.GetFrontDesk(vaultDir); !ok {
				artDir := vault.ResolveArtifactDir(convID)
				_ = kernel.SetFrontDesk(vaultDir, convID, artDir)
			}
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Initialized agy-hq private Vault at %s\n", vaultDir)
		for _, item := range created {
			fmt.Printf("  + %s\n", item)
		}

	case "console":
		fs := flag.NewFlagSet("console", flag.ExitOnError)
		_ = fs.Bool("sync", true, "Regenerate local/CONSOLE.md and <artifactDir>/hq_console.md")
		_ = fs.Parse(os.Args[2:])
		if err := webui.SyncMarkdownConsole(vaultDir); err != nil {
			fmt.Fprintf(os.Stderr, "hq console sync failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Synced local/CONSOLE.md and active Front Desk hq_console.md artifact")

	case "migrate":
		fs := flag.NewFlagSet("migrate", flag.ExitOnError)
		quiet := fs.Bool("quiet", false, "Suppress normal migration output")
		_ = fs.Parse(os.Args[2:])
		templateDir := filepath.Join(rootDir, "templates")
		rep, err := vault.Migrate(vaultDir, templateDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq migrate failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if !*quiet {
			fmt.Printf("Vault Schema Migration: v%d -> v%d (snapshotCommitted=%v)\n", rep.PreviousVersion, rep.CurrentVersion, rep.SnapshotCommitted)
			for _, step := range rep.AppliedSteps {
				fmt.Printf("  [step] %s\n", step)
			}
			for _, item := range rep.CreatedItems {
				fmt.Printf("  [new]  %s\n", item)
			}
			for _, key := range rep.AddedConfigKeys {
				fmt.Printf("  [cfg]  added missing top-level key %s\n", key)
			}
		}

	case "upgrade":
		fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
		noPull := fs.Bool("no-pull", false, "Skip git pull on root repository (offline/test mode)")
		_ = fs.Parse(os.Args[2:])

		// 1. Pre-upgrade snapshot of uncommitted private Vault changes
		if vault.SnapshotVault(vaultDir, "hq upgrade") {
			fmt.Println("==> [1/5] Committed pre-upgrade restore point in local/.git")
		} else {
			fmt.Println("==> [1/5] Private Vault working tree clean (or no uncommitted changes)")
		}

		// 2. Pull upstream Layer 1 root repo (and optional private Vault remote if configured)
		if !*noPull {
			fmt.Println("==> [2/5] Pulling latest agy-hq upstream commits...")
			pullCmd := exec.Command("git", "-C", rootDir, "pull", "--ff-only")
			if out, err := pullCmd.CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "hq upgrade: git pull --ff-only failed (%v):\n%s\n", err, strings.TrimSpace(string(out)))
				os.Exit(1)
			} else {
				fmt.Printf("    %s\n", strings.TrimSpace(string(out)))
			}
			if err := exec.Command("git", "-C", vaultDir, "remote", "get-url", "origin").Run(); err == nil {
				_ = exec.Command("git", "-C", vaultDir, "pull", "--rebase", "origin", "main").Run()
			}
		} else {
			fmt.Println("==> [2/5] Skipping git pull (--no-pull)")
		}

		// 3. Recompile ./bin/hq from the updated Go source
		targetBin := filepath.Join(rootDir, "bin", "hq")
		if exe, err := os.Executable(); err == nil && strings.HasPrefix(exe, os.TempDir()) {
			targetBin = exe
		}
		fmt.Printf("==> [3/5] Rebuilding hq microkernel (%s)...\n", targetBin)
		_ = os.MkdirAll(filepath.Dir(targetBin), 0o755)
		buildCmd := exec.Command("go", "build", "-o", targetBin, "./cmd/hq")
		buildCmd.Dir = rootDir
		if out, err := buildCmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "hq upgrade: go build failed (%v):\n%s\n", err, string(out))
			os.Exit(1)
		}

		// 4. Re-exec the newly compiled binary's `migrate` subcommand so newly pulled migrations run
		fmt.Println("==> [4/5] Running non-destructive Vault schema & template migration...")
		migCmd := exec.Command(targetBin, "migrate")
		migCmd.Dir = rootDir
		migCmd.Env = append(os.Environ(), "HQ_ROOT_DIR="+rootDir, "HQ_VAULT_DIR="+vaultDir)
		if out, err := migCmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "hq upgrade: migrate failed (%v):\n%s\n", err, string(out))
			os.Exit(1)
		} else {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if line != "" {
					fmt.Printf("    %s\n", line)
				}
			}
		}

		// 5. Run health check
		fmt.Println("==> [5/5] Verifying system health (hq doctor)...")
		for _, r := range doctor.Run(vaultDir) {
			fmt.Printf("    [%s] %-30s %s\n", r.Status, r.Name, r.Detail)
		}

	case "doctor":
		results := doctor.Run(vaultDir)
		for _, r := range results {
			fmt.Printf("[%s] %-30s %s\n", r.Status, r.Name, r.Detail)
		}

	case "status":
		fs := flag.NewFlagSet("status", flag.ExitOnError)
		quiet := fs.Bool("quiet", false, "Suppress normal status output")
		_ = fs.Parse(os.Args[2:])
		_ = webui.SyncMarkdownConsole(vaultDir)
		pending, _ := approvals.List(vaultDir)
		wts, _ := worktree.List(vaultDir)
		if !*quiet {
			fmt.Printf("Vault:            %s\n", vaultDir)
			fmt.Printf("Pending Actions:  %d\n", len(pending))
			fmt.Printf("Active Worktrees: %d\n", len(wts))
			for _, p := range pending {
				fmt.Printf("  - [%s] %s (%s) sha256:%s\n", p.ID, p.Summary, p.ActionType, p.SHA256[:12])
			}
		}

	case "hook":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: hq hook <session-start|pre-tool|stop>")
			os.Exit(1)
		}
		switch os.Args[2] {
		case "session-start":
			if err := hooks.HandleSessionStart(os.Stdin, os.Stdout, vaultDir); err != nil {
				fmt.Fprintf(os.Stderr, "hq hook session-start: %v\n", err)
				os.Exit(1)
			}
			_ = webui.SyncMarkdownConsole(vaultDir)
		case "pre-tool":
			if err := hooks.HandlePreToolUse(os.Stdin, os.Stdout, vaultDir); err != nil {
				fmt.Fprintf(os.Stderr, "hq hook pre-tool: %v\n", err)
				os.Exit(1)
			}
			_ = webui.SyncMarkdownConsole(vaultDir)
		case "stop":
			if err := hooks.HandleStop(os.Stdin, os.Stdout, vaultDir); err != nil {
				fmt.Fprintf(os.Stderr, "hq hook stop: %v\n", err)
				os.Exit(1)
			}
			_ = webui.SyncMarkdownConsole(vaultDir)
		default:
			fmt.Fprintf(os.Stderr, "unknown hook subcommand: %s\n", os.Args[2])
			os.Exit(1)
		}

	case "propose":
		fs := flag.NewFlagSet("propose", flag.ExitOnError)
		actionType := fs.String("type", "external_action", "Action category (e.g., gmail_send, pr_merge, git_push)")
		summary := fs.String("summary", "", "Human-readable summary of the proposed action")
		cmdStr := fs.String("cmd", "", "Exact deterministic shell command to execute upon approval")
		draftID := fs.String("draft", "", "Optional Gmail draft ID when --type=gmail_send")
		account := fs.String("account", "", "Optional Gmail account alias when --type=gmail_send")
		workDir := fs.String("workdir", "", "Optional working directory for command execution")
		convID := fs.String("conv", "", "Optional conversation ID")
		_ = fs.Parse(os.Args[2:])
		var meta map[string]string
		if *draftID != "" {
			if *actionType == "external_action" {
				*actionType = "gmail_send"
			}
			spec, err := gmail.GetDraft(vaultDir, *account, *draftID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq propose: load draft %s failed: %v\n", *draftID, err)
				os.Exit(1)
			}
			hashShort := spec.ContentHash
			if len(hashShort) > 12 {
				hashShort = hashShort[:12]
			}
			if *summary == "" {
				*summary = fmt.Sprintf("Send Gmail Draft %s [From: %s -> To: %s | Subject: %s] (hash:%s)", spec.DraftID, spec.From, spec.To, spec.Subject, hashShort)
			}
			if *cmdStr == "" {
				hqBin := "./bin/hq"
				if exe, err := os.Executable(); err == nil && strings.HasPrefix(exe, os.TempDir()) {
					hqBin = exe
				}
				*cmdStr = fmt.Sprintf("%s gmail send-draft --account=%s --draft=%s --hash=%s", hqBin, spec.Account, spec.DraftID, hashShort)
			}
			if *workDir == "" {
				*workDir = rootDir
			}
			preview := spec.Body
			if len(preview) > 500 {
				preview = preview[:500] + "..."
			}
			meta = map[string]string{
				"draftId":     spec.DraftID,
				"account":     spec.Account,
				"from":        spec.From,
				"to":          spec.To,
				"cc":          spec.Cc,
				"subject":     spec.Subject,
				"bodyPreview": preview,
				"contentHash": spec.ContentHash,
			}
		}
		if *cmdStr == "" || *summary == "" {
			fmt.Fprintln(os.Stderr, "usage: hq propose --type=<type> --summary=\"...\" --cmd=\"...\" (or --type=gmail_send --draft=<draftId>)")
			os.Exit(1)
		}
		prop, err := approvals.CreateWithMetadata(vaultDir, *actionType, *summary, *cmdStr, *workDir, *convID, meta)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq propose failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Staged proposal %s (sha256: %s)\n", prop.ID, prop.SHA256[:12])
		fmt.Printf("  Approve with: ./bin/hq approve %s --sha256=%s\n", prop.ID, prop.SHA256[:12])

	case "approve":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: hq approve <proposal-id> [--sha256=<prefix>]")
			os.Exit(1)
		}
		id := os.Args[2]
		fs := flag.NewFlagSet("approve", flag.ExitOnError)
		shaPrefix := fs.String("sha256", "", "SHA-256 prefix verification")
		_ = fs.Parse(os.Args[3:])
		out, err := approvals.Approve(vaultDir, id, *shaPrefix)
		_ = webui.SyncMarkdownConsole(vaultDir)
		if out != "" {
			fmt.Print(out)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq approve execution error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Approved and executed proposal %s\n", id)

	case "reject":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: hq reject <proposal-id> [--reason=\"...\"]")
			os.Exit(1)
		}
		id := os.Args[2]
		fs := flag.NewFlagSet("reject", flag.ExitOnError)
		reason := fs.String("reason", "Rejected by user", "Reason for rejection")
		_ = fs.Parse(os.Args[3:])
		if err := approvals.Reject(vaultDir, id, *reason); err != nil {
			fmt.Fprintf(os.Stderr, "hq reject error: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Rejected proposal %s\n", id)

	case "worktree":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: hq worktree <add|list> ...")
			os.Exit(1)
		}
		switch os.Args[2] {
		case "add":
			if len(os.Args) < 5 {
				fmt.Fprintln(os.Stderr, "usage: hq worktree add <repo-path-or-github-slug> <task-name> [--branch=<branch>]")
				os.Exit(1)
			}
			repoSpec := os.Args[3]
			taskName := os.Args[4]
			fs := flag.NewFlagSet("worktree add", flag.ExitOnError)
			branch := fs.String("branch", "", "Branch name (defaults to hq/<task-name>)")
			_ = fs.Parse(os.Args[5:])
			dir, err := worktree.Add(vaultDir, repoSpec, taskName, *branch)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq worktree add failed: %v\n", err)
				os.Exit(1)
			}
			_ = webui.SyncMarkdownConsole(vaultDir)
			fmt.Printf("Created isolated worktree at %s\n", dir)
		case "list":
			list, err := worktree.List(vaultDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq worktree list failed: %v\n", err)
				os.Exit(1)
			}
			for _, d := range list {
				fmt.Println(d)
			}
		case "remove", "rm":
			if len(os.Args) < 4 {
				fmt.Fprintln(os.Stderr, "usage: hq worktree remove <worktree-name-or-path>")
				os.Exit(1)
			}
			if err := worktree.Remove(vaultDir, os.Args[3]); err != nil {
				fmt.Fprintf(os.Stderr, "hq worktree remove failed: %v\n", err)
				os.Exit(1)
			}
			_ = webui.SyncMarkdownConsole(vaultDir)
			fmt.Printf("Removed worktree %s\n", os.Args[3])
		default:
			fmt.Fprintf(os.Stderr, "unknown worktree subcommand: %s\n", os.Args[2])
			os.Exit(1)
		}

	case "heartbeat":
		fs := flag.NewFlagSet("heartbeat", flag.ExitOnError)
		dryRun := fs.Bool("dry-run", false, "Simulate agentapi spawns without calling LLM")
		_ = fs.Parse(os.Args[2:])
		status, err := kernel.RunHeartbeat(vaultDir, *dryRun)
		_ = webui.SyncMarkdownConsole(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq heartbeat error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(status)

	case "claim-desk":
		fs := flag.NewFlagSet("claim-desk", flag.ExitOnError)
		convID := fs.String("conv", "", "Conversation ID to set as the single active Front Desk")
		artDir := fs.String("artifact-dir", "", "Optional artifact directory for the Front Desk")
		_ = fs.Parse(os.Args[2:])
		if *convID == "" {
			*convID = vault.ResolveConversationID()
		}
		if *convID == "" {
			fmt.Fprintln(os.Stderr, "usage: hq claim-desk --conv=<conversation-id> [--artifact-dir=...]")
			os.Exit(1)
		}
		if *artDir == "" {
			*artDir = vault.ResolveArtifactDir(*convID)
		}
		if prev, hadPrev := kernel.GetFrontDesk(vaultDir); hadPrev && prev.ConversationID != "" && prev.ConversationID != *convID {
			if _, alreadyTracked := kernel.FindTaskByConversationID(vaultDir, prev.ConversationID); !alreadyTracked {
				shortPrev := prev.ConversationID
				if len(shortPrev) > 8 {
					shortPrev = shortPrev[:8]
				}
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
		fmt.Printf("  - Step 1 (Pin Console Artifact): write_file to %s with ArtifactMetadata={UserFacing:true, RequestFeedback:false}\n", artPath)
		if len(unarmed) > 0 {
			fmt.Printf("  - Step 2 (Arm %d Proactive Schedule(s) from local/triggers.yaml):\n", len(unarmed))
			for _, s := range unarmed {
				fmt.Printf("      * %s: schedule(CronExpression=%q, IsDaemon=true, Prompt=%q)\n", s.ID, s.FiveFieldCron(), s.DefaultPrompt())
			}
		}

	case "release-desk":
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

	case "adopt":
		fs := flag.NewFlagSet("adopt", flag.ExitOnError)
		convID := fs.String("conv", "", "Existing conversation ID to adopt into headquarter orchestration")
		taskSlug := fs.String("task", "", "Short task slug")
		title := fs.String("title", "", "Human-readable title for the task conversation")
		summary := fs.String("summary", "", "Current status/summary of the conversation")
		status := fs.String("status", "active", "Status (active or completed)")
		_ = fs.Parse(os.Args[2:])
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

	case "complete":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: hq complete <task-slug-or-conv-id> [--summary=\"...\"]")
			os.Exit(1)
		}
		target := os.Args[2]
		fs := flag.NewFlagSet("complete", flag.ExitOnError)
		summary := fs.String("summary", "", "Optional completion summary")
		_ = fs.Parse(os.Args[3:])
		if err := kernel.CompleteTask(vaultDir, target, *summary); err != nil {
			fmt.Fprintf(os.Stderr, "hq complete error: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Marked task %q as completed\n", target)

	case "archive":
		var target string
		force := false
		for _, arg := range os.Args[2:] {
			if arg == "--force" || arg == "-force" || arg == "-f" {
				force = true
			} else if !strings.HasPrefix(arg, "-") && target == "" {
				target = arg
			}
		}
		if target == "" {
			fmt.Fprintln(os.Stderr, "usage: hq archive <task-slug-or-conv-id> [--force]")
			os.Exit(1)
		}
		if err := kernel.ArchiveTask(vaultDir, target, force); err != nil {
			fmt.Fprintf(os.Stderr, "hq archive error: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Archived task %q (worktree cleaned up)\n", target)

	case "reopen":
		if len(os.Args) < 3 || strings.HasPrefix(os.Args[2], "-") {
			fmt.Fprintln(os.Stderr, "usage: hq reopen <task-slug-or-conv-id>")
			os.Exit(1)
		}
		target := os.Args[2]
		if err := kernel.ReopenTask(vaultDir, target); err != nil {
			fmt.Fprintf(os.Stderr, "hq reopen error: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		convID := ""
		if tasks, err := kernel.ListTasks(vaultDir); err == nil {
			for _, t := range tasks {
				if t.TaskSlug == target || t.ConversationID == target {
					convID = t.ConversationID
					break
				}
			}
		}
		fmt.Printf("Reopened task %q (status: active, link: conversation://%s)\n", target, convID)

	case "spawn":
		fs := flag.NewFlagSet("spawn", flag.ExitOnError)
		repoSpec := fs.String("repo", "", "Optional repo path or GitHub slug to create an isolated worktree for")
		taskName := fs.String("task", "", "Short task slug for worktree and conversation title")
		role := fs.String("role", "", "Optional subagent role when --subagent is set")
		parentConv := fs.String("parent", "", "Optional parent conversation ID when --subagent is set (defaults to ANTIGRAVITY_CONVERSATION_ID)")
		asSubagent := fs.Bool("subagent", false, "Launch as a nested child subagent under the parent conversation instead of a top-level sidebar thread")
		prompt := fs.String("prompt", "", "Task instructions for the new conversation")
		model := fs.String("model", "", "Optional model override (flash, argon, sonnet, opus, or full model ID)")
		depth := fs.String("depth", "", "Optional thinking depth override (low, medium, high)")
		tier := fs.String("tier", "", "Optional complexity tier override (quick, standard, deep)")
		dryRun := fs.Bool("dry-run", false, "Simulate agentapi spawn")
		_ = fs.Parse(os.Args[2:])
		if *prompt == "" || (*taskName == "" && *role == "") {
			fmt.Fprintln(os.Stderr, "usage: hq spawn --task=<slug> [--subagent] [--parent=<conv-id>] [--role=\"...\"] [--repo=<repo>] [--tier=quick|standard|deep] [--model=...] [--depth=low|medium|high] --prompt=\"...\"")
			os.Exit(1)
		}
		resolvedModel, resolvedTier, _ := kernel.ResolveModel(*prompt, *model, *depth, *tier)
		fullPrompt := *prompt
		var wtDir string
		var repoRootPath string
		if *repoSpec != "" {
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
			if info, err := os.Stat(*repoSpec); err == nil && info.IsDir() {
				if abs, err := filepath.Abs(*repoSpec); err == nil {
					repoRootPath = abs
				}
			}
			fullPrompt = fmt.Sprintf("Work inside the isolated git worktree at %s.\n\nTask: %s", wtDir, *prompt)
		}
		scope := "general"
		if *repoSpec != "" {
			scope = filepath.Base(strings.TrimSuffix(*repoSpec, ".git"))
		} else if parts := strings.SplitN(*taskName, "-", 2); len(parts) > 0 && parts[0] != "" {
			scope = parts[0]
		}
		if *asSubagent {
			subRole := *role
			if subRole == "" {
				subRole = fmt.Sprintf("[%s] %s: %s", resolvedTier, scope, *taskName)
			}
			workDir := wtDir
			if workDir == "" {
				workDir = rootDir
			}
			out, err := kernel.SpawnSubagent(vaultDir, *parentConv, subRole, fullPrompt, resolvedModel, workDir, *dryRun)
			_ = webui.SyncMarkdownConsole(vaultDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq spawn --subagent error: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(out)
			return
		}
		title := fmt.Sprintf("[Task:%s] %s (%s/%s)", scope, *taskName, resolvedTier, resolvedModel)
		createdAt := time.Now().Format(time.RFC3339)
		// Pre-register before SpawnTask so child SessionStart/Stop hooks can link immediately without race conditions.
		_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
			TaskSlug:      *taskName,
			Title:         title,
			Tier:          resolvedTier,
			Model:         resolvedModel,
			WorktreeDir:   wtDir,
			RepoRoot:      repoRootPath,
			CreatedAt:     createdAt,
			LastUpdatedAt: createdAt,
			LastSummary:   *prompt,
			Status:        "active",
		})
		out, err := kernel.SpawnTask(vaultDir, "manual-spawn:"+*taskName, "swe-pulse", title, fullPrompt, resolvedModel, *dryRun)
		if spawnedID := kernel.ExtractUUID(out); spawnedID != "" {
			_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
				TaskSlug:       *taskName,
				Title:          title,
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
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq spawn error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(out)

	case "subagent":
		fs := flag.NewFlagSet("subagent", flag.ExitOnError)
		role := fs.String("role", "", "Subagent role title, e.g. \"[deep] rfc-0005: Opus 5.5 Reviewer\"")
		parentConv := fs.String("parent", "", "Parent conversation ID (defaults to ANTIGRAVITY_CONVERSATION_ID)")
		repoSpec := fs.String("repo", "", "Optional repo path or GitHub slug to create an isolated worktree for")
		taskName := fs.String("task", "", "Optional worktree task slug")
		prompt := fs.String("prompt", "", "Task instructions for the nested subagent")
		model := fs.String("model", "", "Optional model override (flash, argon, sonnet, opus, or full model ID)")
		depth := fs.String("depth", "", "Optional thinking depth override (low, medium, high)")
		tier := fs.String("tier", "", "Optional complexity tier override (quick, standard, deep)")
		dryRun := fs.Bool("dry-run", false, "Simulate subagent spawn")
		_ = fs.Parse(os.Args[2:])
		if *prompt == "" || (*role == "" && *taskName == "") {
			fmt.Fprintln(os.Stderr, "usage: hq subagent --role=\"[<tier>] <scope>: <role>\" [--model=...] [--depth=low|medium|high] [--parent=<conv-id>] [--repo=<repo>] [--task=<slug>] --prompt=\"...\"")
			os.Exit(1)
		}
		resolvedModel, resolvedTier, _ := kernel.ResolveModel(*prompt, *model, *depth, *tier)
		subRole := *role
		if subRole == "" {
			subRole = fmt.Sprintf("[%s] general: %s", resolvedTier, *taskName)
		}
		fullPrompt := *prompt
		workDir := rootDir
		if *repoSpec != "" {
			wtSlug := *taskName
			if wtSlug == "" {
				_, _, _, _, wtSlug = kernel.ParseSubagentRole(subRole, resolvedModel)
			}
			wtDir, err := worktree.Add(vaultDir, *repoSpec, wtSlug, "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq subagent worktree error: %v\n", err)
				os.Exit(1)
			}
			workDir = wtDir
			fullPrompt = fmt.Sprintf("Work inside the isolated git worktree at %s.\n\nTask: %s", wtDir, *prompt)
		}
		out, err := kernel.SpawnSubagent(vaultDir, *parentConv, subRole, fullPrompt, resolvedModel, workDir, *dryRun)
		_ = webui.SyncMarkdownConsole(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq subagent error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(out)

	case "gmail":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: hq gmail <init|add-account|accounts|search|read-quarantined|draft|send-draft|precheck|triage-result|attachment-quarantine> [args]")
			os.Exit(1)
		}
		switch os.Args[2] {
		case "init", "add-account":
			fs := flag.NewFlagSet("gmail "+os.Args[2], flag.ExitOnError)
			alias := fs.String("alias", "primary", "Account alias (e.g. primary, personal-2, shopping)")
			email := fs.String("email", "", "Gmail address (e.g. you@gmail.com)")
			agyAcct := fs.String("agy-account", "", "Optional AGY_ACCOUNT name if signed in via Antigravity plugin")
			tokenFile := fs.String("token-file", "", "Optional path to read-only OAuth2 token JSON for hq daemon sidecar")
			query := fs.String("query", gmail.DefaultPrimaryQuery, "Default unread search query for sidecar precheck")
			setDefault := fs.Bool("default", os.Args[2] == "init", "Set this account as the default Gmail account")
			_ = fs.Parse(os.Args[3:])
			cfg, err := gmail.InitOrAddAccount(vaultDir, *alias, *email, *agyAcct, *tokenFile, *query, *setDefault)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gmail %s failed: %v\n", os.Args[2], err)
				os.Exit(1)
			}
			fmt.Printf("Configured Gmail account %q (%s) [token=%s]\n", cfg.Alias, cfg.Email, cfg.TokenFile)

		case "accounts":
			reg, err := gmail.LoadRegistry(vaultDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gmail accounts failed: %v\n", err)
				os.Exit(1)
			}
			b, _ := json.MarshalIndent(reg, "", "  ")
			fmt.Println(string(b))

		case "search":
			fs := flag.NewFlagSet("gmail search", flag.ExitOnError)
			account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
			query := fs.String("query", "", "Gmail search query (defaults to account's Primary unread query)")
			maxRes := fs.Int("max", 20, "Maximum messages to return")
			_ = fs.Parse(os.Args[3:])
			msgs, err := gmail.SearchMessages(vaultDir, *account, *query, *maxRes)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gmail search failed: %v\n", err)
				os.Exit(1)
			}
			b, _ := json.MarshalIndent(msgs, "", "  ")
			fmt.Println(string(b))

		case "read-quarantined":
			fs := flag.NewFlagSet("gmail read-quarantined", flag.ExitOnError)
			account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
			threadID := fs.String("thread", "", "Thread ID to read in quarantined mode")
			convID := fs.String("conv", "", "Optional conversation ID (defaults to ANTIGRAVITY_CONVERSATION_ID)")
			_ = fs.Parse(os.Args[3:])
			_, formatted, err := gmail.ReadQuarantinedThread(vaultDir, *account, *threadID, *convID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gmail read-quarantined failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Print(formatted)

		case "draft":
			fs := flag.NewFlagSet("gmail draft", flag.ExitOnError)
			account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
			draftID := fs.String("draft", "", "Optional existing draft ID to update")
			threadID := fs.String("thread", "", "Optional thread ID being replied to")
			to := fs.String("to", "", "Recipient email address(es)")
			cc := fs.String("cc", "", "Optional CC email address(es)")
			subject := fs.String("subject", "", "Email subject line")
			body := fs.String("body", "", "Plain-text email body")
			convID := fs.String("conv", "", "Optional conversation ID")
			_ = fs.Parse(os.Args[3:])
			spec, err := gmail.CreateOrUpdateDraft(vaultDir, *account, *draftID, *threadID, *to, *cc, *subject, *body, *convID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gmail draft failed: %v\n", err)
				os.Exit(1)
			}
			b, _ := json.MarshalIndent(spec, "", "  ")
			fmt.Println(string(b))

		case "send-draft":
			fs := flag.NewFlagSet("gmail send-draft", flag.ExitOnError)
			account := fs.String("account", "", "Account alias or email")
			draftID := fs.String("draft", "", "Draft ID to verify and send")
			hashPrefix := fs.String("hash", "", "Expected contentHash prefix from approved proposal")
			_ = fs.Parse(os.Args[3:])
			out, err := gmail.VerifyAndSendDraft(vaultDir, *account, *draftID, *hashPrefix)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gmail send-draft failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(out)

		case "precheck":
			fs := flag.NewFlagSet("gmail precheck", flag.ExitOnError)
			account := fs.String("account", "", "Optional account alias filter")
			_ = fs.Parse(os.Args[3:])
			batches, status, err := gmail.RunPrecheck(vaultDir, *account)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gmail precheck failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(status)
			if len(batches) > 0 {
				b, _ := json.MarshalIndent(batches, "", "  ")
				fmt.Println(string(b))
			}

		case "triage-result":
			fs := flag.NewFlagSet("gmail triage-result", flag.ExitOnError)
			batchID := fs.String("batch", "", "Batch ID being summarized")
			rawJSON := fs.String("json", "", "JSON array of TriageItem objects")
			_ = fs.Parse(os.Args[3:])
			var items []gmail.TriageItem
			if err := json.Unmarshal([]byte(*rawJSON), &items); err != nil {
				fmt.Fprintf(os.Stderr, "hq gmail triage-result invalid JSON: %v\n", err)
				os.Exit(1)
			}
			if err := gmail.RecordTriageResults(vaultDir, *batchID, items); err != nil {
				fmt.Fprintf(os.Stderr, "hq gmail triage-result validation failed: %v\n", err)
				os.Exit(1)
			}
			_ = webui.SyncMarkdownConsole(vaultDir)
			fmt.Printf("Recorded %d triaged Gmail thread(s) for batch %s\n", len(items), *batchID)

		case "attachment-quarantine":
			fs := flag.NewFlagSet("gmail attachment-quarantine", flag.ExitOnError)
			filePath := fs.String("file", "", "Path to downloaded attachment file")
			fromHdr := fs.String("from", "", "Sender From header")
			authRes := fs.String("auth-results", "", "Authentication-Results header (e.g. dkim=pass)")
			_ = fs.Parse(os.Args[3:])
			content, err := os.ReadFile(*filePath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gmail attachment-quarantine read error: %v\n", err)
				os.Exit(1)
			}
			dest, autoRouted, err := gmail.QuarantineAttachment(vaultDir, filepath.Base(*filePath), content, *fromHdr, *authRes)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gmail attachment-quarantine error: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Attachment saved to %s (autoRoutedToInbox=%v)\n", dest, autoRouted)

		default:
			fmt.Fprintf(os.Stderr, "unknown gmail subcommand: %s\n", os.Args[2])
			os.Exit(1)
		}

	case "daemon":
		if len(os.Args) >= 3 && !strings.HasPrefix(os.Args[2], "-") {
			switch os.Args[2] {
			case "install":
				fs := flag.NewFlagSet("daemon install", flag.ExitOnError)
				_ = fs.Parse(os.Args[3:])
				msg, err := kernel.InstallDaemon(rootDir, vaultDir)
				if err != nil {
					fmt.Fprintf(os.Stderr, "hq daemon install failed: %v\n", err)
					os.Exit(1)
				}
				fmt.Println(msg)
				return
			case "uninstall":
				fs := flag.NewFlagSet("daemon uninstall", flag.ExitOnError)
				_ = fs.Parse(os.Args[3:])
				msg, err := kernel.UninstallDaemon(vaultDir)
				if err != nil {
					fmt.Fprintf(os.Stderr, "hq daemon uninstall failed: %v\n", err)
					os.Exit(1)
				}
				fmt.Println(msg)
				return
			case "status":
				fs := flag.NewFlagSet("daemon status", flag.ExitOnError)
				asJSON := fs.Bool("json", false, "Output status as JSON")
				_ = fs.Parse(os.Args[3:])
				info := kernel.GetDaemonInfo(vaultDir)
				if *asJSON {
					b, _ := json.MarshalIndent(info, "", "  ")
					fmt.Println(string(b))
				} else {
					fmt.Print(kernel.FormatDaemonStatus(info))
				}
				return
			case "start", "run":
				// continue to daemon server startup below
			default:
				fmt.Fprintf(os.Stderr, "unknown daemon subcommand: %s (expected install, uninstall, status, or run)\n", os.Args[2])
				os.Exit(1)
			}
		}

		fs := flag.NewFlagSet("daemon", flag.ExitOnError)
		defaultAddr := "127.0.0.1:4747"
		if p := os.Getenv("PORT"); p != "" {
			defaultAddr = "127.0.0.1:" + p
		}
		addr := fs.String("addr", defaultAddr, "Listen address for HQ Console HTTP server")
		dryRun := fs.Bool("dry-run", false, "Simulate agentapi spawns in background watcher")
		daemonArgs := os.Args[2:]
		if len(daemonArgs) > 0 && (daemonArgs[0] == "start" || daemonArgs[0] == "run") {
			daemonArgs = daemonArgs[1:]
		}
		_ = fs.Parse(daemonArgs)

		if err := kernel.RecordDaemonStart(vaultDir, *addr); err != nil {
			fmt.Fprintf(os.Stderr, "hq daemon error: %v\n", err)
			os.Exit(1)
		}
		defer kernel.RecordDaemonStop(vaultDir)

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigChan
			_ = kernel.RecordDaemonStop(vaultDir)
			os.Exit(0)
		}()

		// Background 15s inbox watcher & 30m 0-token heartbeat gatekeeper loop
		go func() {
			_, _ = kernel.RunHeartbeat(vaultDir, *dryRun)
			inboxTicker := time.NewTicker(15 * time.Second)
			hbTicker := time.NewTicker(30 * time.Minute)
			defer inboxTicker.Stop()
			defer hbTicker.Stop()
			for {
				select {
				case <-inboxTicker.C:
					_, _ = kernel.ProcessInbox(vaultDir, *dryRun)
				case <-hbTicker.C:
					_, _ = kernel.RunHeartbeat(vaultDir, *dryRun)
				}
			}
		}()

		fmt.Printf("Starting agy-hq Console & Daemon on http://%s (Vault: %s)\n", *addr, vaultDir)
		if err := http.ListenAndServe(*addr, webui.NewHandler(vaultDir)); err != nil {
			fmt.Fprintf(os.Stderr, "hq daemon server error: %v\n", err)
			os.Exit(1)
		}

	case "memory":
		sub := "status"
		if len(os.Args) >= 3 {
			sub = os.Args[2]
		}
		switch sub {
		case "status", "audit":
			var completedSlugs []string
			if tasks, err := kernel.ListTasks(vaultDir); err == nil {
				for _, t := range tasks {
					if t.Status == "completed" || t.Status == "cancelled" {
						completedSlugs = append(completedSlugs, t.TaskSlug)
					}
				}
			}
			rep, err := vault.AuditMemory(vaultDir, completedSlugs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq memory status failed: %v\n", err)
				os.Exit(1)
			}
			budgetBadge := "OK"
			if rep.OverBudget {
				budgetBadge = "OVER_BUDGET"
			}
			fmt.Printf("Memory Roadmap Index (MEMORY.md): %d / %d lines (~%d tokens) [%s]\n", rep.MemoryLines, rep.MemoryMaxLines, rep.EstimatedTokens, budgetBadge)
			fmt.Printf("Knowledge Wiki Topics (topics/):  %d page(s) (unlinked in MEMORY.md: %d)\n", len(rep.Topics), len(rep.OrphanTopicSlugs))
			for _, tp := range rep.Topics {
				fmt.Printf("  - [%s] %s (%d lines): %s\n", tp.Slug, tp.Title, tp.LineCount, tp.Summary)
			}
			fmt.Printf("Bi-Temporal Facts (facts.jsonl):  %d active, %d expired (%d raw lines)\n", rep.ActiveFactCount, rep.ExpiredFactCount, rep.RawFactLines)
			if len(rep.StaleReferences) > 0 {
				fmt.Println("Stale References Detected:")
				for _, sr := range rep.StaleReferences {
					fmt.Printf("  ! %s\n", sr)
				}
			}

		case "topics":
			topics, err := vault.LoadTopicIndex(vaultDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq memory topics failed: %v\n", err)
				os.Exit(1)
			}
			for _, tp := range topics {
				fmt.Printf("- %s (%s): %s [%s]\n", tp.Slug, tp.Title, tp.Summary, tp.Path)
			}

		case "add-fact":
			fs := flag.NewFlagSet("memory add-fact", flag.ExitOnError)
			id := fs.String("id", "", "Optional explicit fact ID")
			topic := fs.String("topic", "", "Fact topic category (e.g., architecture, workflow, investing)")
			statement := fs.String("statement", "", "Concise durable fact statement")
			sourceConv := fs.String("source", "", "Optional source conversation ID")
			verified := fs.Bool("verified", false, "Mark fact as ground-truth transcript verified")
			_ = fs.Parse(os.Args[3:])
			fact, err := vault.AddFact(vaultDir, *id, *topic, *statement, *sourceConv, *verified)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq memory add-fact failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Added fact %s [%s]: %s\n", fact.ID, fact.Topic, fact.Statement)

		case "prune-fact":
			if len(os.Args) < 4 {
				fmt.Fprintln(os.Stderr, "usage: hq memory prune-fact <fact-id> [--superseded-by=<new-id>]")
				os.Exit(1)
			}
			factID := os.Args[3]
			fs := flag.NewFlagSet("memory prune-fact", flag.ExitOnError)
			supersededBy := fs.String("superseded-by", "", "Optional ID of the new fact that supersedes this one")
			_ = fs.Parse(os.Args[4:])
			pruned, err := vault.PruneFact(vaultDir, factID, *supersededBy)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq memory prune-fact failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Pruned fact %s (valid_to=%s)\n", pruned.ID, *pruned.ValidTo)

		case "compact":
			before, after, err := vault.CompactFacts(vaultDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq memory compact failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Compacted facts.jsonl: %d -> %d canonical lines\n", before, after)

		case "verify-transcript":
			if len(os.Args) < 4 {
				fmt.Fprintln(os.Stderr, "usage: hq memory verify-transcript <conv-id-or-path>")
				os.Exit(1)
			}
			digest, err := hooks.VerifyTranscript(os.Args[3], vaultDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq memory verify-transcript failed: %v\n", err)
				os.Exit(1)
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			enc.SetEscapeHTML(false)
			_ = enc.Encode(digest)

		default:
			fmt.Fprintf(os.Stderr, "unknown memory subcommand: %s (expected status, topics, add-fact, prune-fact, compact, verify-transcript)\n", sub)
			os.Exit(1)
		}

	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Usage: hq <command> [args]

Commands:
  init                                Initialize local/ private Vault and git repository
  upgrade [--no-pull]                 Pull latest agy-hq commits, rebuild hq, and migrate Vault
  migrate [--quiet]                   Run non-destructive Vault schema & template migrations
  doctor                              Check Antigravity sandbox, CLI, and Vault health
  status [--quiet]                    Show pending approvals and active worktrees
  memory <status|topics|add-fact|prune-fact|compact|verify-transcript>  Manage 4-layer memory engine
  heartbeat [--dry-run]               Run 0-token gatekeeper pre-check and inbox router once
  daemon [install|uninstall|status]   Run or manage the HQ Console background daemon service
  hook <session-start|pre-tool|stop>  Antigravity hooks.json handlers
  propose --type=... --summary=... --cmd=...  Stage an outbound action for 1-tap approval
  approve <id> [--sha256=<prefix>]    Verify SHA-256 and deterministically execute proposal
  reject <id> [--reason=...]          Reject a pending proposal
  worktree add <repo> <task>          Checkout an isolated git worktree in local/worktrees/
  gmail <init|search|read-quarantined|draft|precheck>  Layer 1 Gmail primitives & sidecar precheck
  spawn --task=<slug> --prompt="..."  Spawn a dedicated Task Conversation in an isolated worktree
  subagent --role="..." --model=...   Spawn a nested child subagent under the current conversation
  adopt --conv=<id> --task=<slug>     Adopt an existing conversation (or demote Front Desk to a Task)
  claim-desk --conv=<id>              Claim the single active [HQ] Front Desk intake lease
  release-desk                        Release Front Desk lease so next new conversation claims it`)
}
