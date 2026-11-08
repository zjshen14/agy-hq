package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/doctor"
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
		fmt.Printf("Initialized agy-hq private Vault at %s\n", vaultDir)
		for _, item := range created {
			fmt.Printf("  + %s\n", item)
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
		case "pre-tool":
			if err := hooks.HandlePreToolUse(os.Stdin, os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "hq hook pre-tool: %v\n", err)
				os.Exit(1)
			}
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
		workDir := fs.String("workdir", "", "Optional working directory for command execution")
		convID := fs.String("conv", "", "Optional conversation ID")
		_ = fs.Parse(os.Args[2:])
		if *cmdStr == "" || *summary == "" {
			fmt.Fprintln(os.Stderr, "usage: hq propose --type=<type> --summary=\"...\" --cmd=\"...\"")
			os.Exit(1)
		}
		prop, err := approvals.Create(vaultDir, *actionType, *summary, *cmdStr, *workDir, *convID)
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
			fmt.Fprintln(os.Stderr, "usage: hq claim-desk --conv=<conversation-id> [--artifact-dir=...]")
			os.Exit(1)
		}
		if err := kernel.SetFrontDesk(vaultDir, *convID, *artDir); err != nil {
			fmt.Fprintf(os.Stderr, "hq claim-desk error: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Claimed Front Desk intake lease for conversation %s\n", *convID)

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

	case "spawn":
		fs := flag.NewFlagSet("spawn", flag.ExitOnError)
		repoSpec := fs.String("repo", "", "Optional repo path or GitHub slug to create an isolated worktree for")
		taskName := fs.String("task", "", "Short task slug for worktree and conversation title")
		prompt := fs.String("prompt", "", "Task instructions for the new conversation")
		model := fs.String("model", "", "Optional model override (flash, argon, sonnet, opus, or full model ID)")
		depth := fs.String("depth", "", "Optional thinking depth override (low, medium, high)")
		tier := fs.String("tier", "", "Optional complexity tier override (quick, standard, deep)")
		dryRun := fs.Bool("dry-run", false, "Simulate agentapi spawn")
		_ = fs.Parse(os.Args[2:])
		if *prompt == "" || *taskName == "" {
			fmt.Fprintln(os.Stderr, "usage: hq spawn --task=<slug> [--repo=<repo>] [--tier=quick|standard|deep] [--model=...] [--depth=low|medium|high] --prompt=\"...\"")
			os.Exit(1)
		}
		resolvedModel, resolvedTier, _ := kernel.ResolveModel(*prompt, *model, *depth, *tier)
		fullPrompt := *prompt
		if *repoSpec != "" {
			wtDir, err := worktree.Add(vaultDir, *repoSpec, *taskName, "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq spawn worktree error: %v\n", err)
				os.Exit(1)
			}
			fullPrompt = fmt.Sprintf("Work inside the isolated git worktree at %s.\n\nTask: %s", wtDir, *prompt)
		}
		scope := "general"
		if *repoSpec != "" {
			scope = filepath.Base(strings.TrimSuffix(*repoSpec, ".git"))
		}
		title := fmt.Sprintf("[Task:%s] %s (%s/%s)", scope, *taskName, resolvedTier, resolvedModel)
		out, err := kernel.SpawnTask(vaultDir, "manual-spawn:"+*taskName, "swe-pulse", title, fullPrompt, resolvedModel, *dryRun)
		_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
			TaskSlug:      *taskName,
			Title:         title,
			Tier:          resolvedTier,
			Model:         resolvedModel,
			WorktreeDir:   *repoSpec,
			CreatedAt:     time.Now().Format(time.RFC3339),
			LastUpdatedAt: time.Now().Format(time.RFC3339),
			LastSummary:   *prompt,
			Status:        "active",
		})
		_ = webui.SyncMarkdownConsole(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq spawn error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(out)

	case "daemon":
		fs := flag.NewFlagSet("daemon", flag.ExitOnError)
		defaultAddr := "127.0.0.1:4747"
		if p := os.Getenv("PORT"); p != "" {
			defaultAddr = "127.0.0.1:" + p
		}
		addr := fs.String("addr", defaultAddr, "Listen address for HQ Console HTTP server")
		dryRun := fs.Bool("dry-run", false, "Simulate agentapi spawns in background watcher")
		_ = fs.Parse(os.Args[2:])

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

	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Usage: hq <command> [args]

Commands:
  init                                Initialize local/ private Vault and git repository
  doctor                              Check Antigravity sandbox, CLI, and Vault health
  status [--quiet]                    Show pending approvals and active worktrees
  heartbeat [--dry-run]               Run 0-token gatekeeper pre-check and inbox router once
  daemon [--addr=127.0.0.1:4747]      Run the HQ Console web UI & background gatekeeper
  hook <session-start|pre-tool|stop>  Antigravity hooks.json handlers
  propose --type=... --summary=... --cmd=...  Stage an outbound action for 1-tap approval
  approve <id> [--sha256=<prefix>]    Verify SHA-256 and deterministically execute proposal
  reject <id> [--reason=...]          Reject a pending proposal
  worktree add <repo> <task>          Checkout an isolated git worktree in local/worktrees/`)
}
