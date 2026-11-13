package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/ci"
	"github.com/zjshen14/agy-hq/internal/doctor"
	"github.com/zjshen14/agy-hq/internal/gchat"
	"github.com/zjshen14/agy-hq/internal/gmail"
	"github.com/zjshen14/agy-hq/internal/hooks"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/rsi"
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
		fs := flag.NewFlagSet("init", flag.ExitOnError)
		demo := fs.Bool("demo", false, "Seed synthetic [Sample] ACME 10-Q inbox filing, thesis, tasks, approval, and facts")
		withSample := fs.Bool("with-sample-data", false, "Alias for --demo: seed synthetic [Sample] dataset")
		_ = fs.Parse(os.Args[2:])
		templateDir := filepath.Join(rootDir, "templates")
		created, err := vault.Init(vaultDir, templateDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq init failed: %v\n", err)
			os.Exit(1)
		}
		if *demo || *withSample {
			demoSeeded, dErr := vault.SeedDemoData(vaultDir)
			if dErr != nil {
				fmt.Fprintf(os.Stderr, "hq init --demo failed: %v\n", dErr)
				os.Exit(1)
			}
			for _, ds := range demoSeeded {
				created = append(created, ds+" (--demo)")
			}
		}
		if convID := vault.ResolveConversationID(); convID != "" {
			if _, ok := kernel.GetFrontDesk(vaultDir); !ok {
				artDir := vault.ResolveArtifactDir(convID)
				_ = kernel.SetFrontDesk(vaultDir, convID, artDir)
			}
		}
		_, _ = ci.InstallGitHooks(rootDir)
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Initialized agy-hq private Vault at %s\n", vaultDir)
		for _, item := range created {
			fmt.Printf("  + %s\n", item)
		}
		fmt.Printf("\n%s\n", kernel.FormatFrontDeskPinTip())

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
		_, _ = ci.InstallGitHooks(rootDir)
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
			pullCmd := vault.SanitizedGitCommand(rootDir, "pull", "--ff-only")
			if out, err := pullCmd.CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "hq upgrade: git pull --ff-only failed (%v):\n%s\n", err, strings.TrimSpace(string(out)))
				os.Exit(1)
			} else {
				fmt.Printf("    %s\n", strings.TrimSpace(string(out)))
			}
			if err := vault.SanitizedGitCommand(vaultDir, "remote", "get-url", "origin").Run(); err == nil {
				_ = vault.SanitizedGitCommand(vaultDir, "pull", "--rebase", "origin", "main").Run()
			}
		} else {
			fmt.Println("==> [2/5] Skipping git pull (--no-pull)")
		}

		// 3. Recompile ./bin/hq from the updated Go source
		targetBin := filepath.Join(rootDir, "bin", "hq")
		if exe, err := os.Executable(); err == nil && vault.IsTempPath(exe) {
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
		if !vault.IsTempPath(targetBin) {
			_ = exec.Command("systemctl", "--user", "try-restart", "agy-hq.service").Run()
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
		fs := flag.NewFlagSet("doctor", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output diagnostic results as JSON")
		_ = fs.Parse(os.Args[2:])
		results := doctor.Run(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(results)
		} else {
			for _, r := range results {
				fmt.Printf("[%s] %-30s %s\n", r.Status, r.Name, r.Detail)
			}
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
		actionType := fs.String("type", "external_action", "Action category (e.g., gmail_send, gchat_send, rsi_patch, rsi_skill_patch)")
		summary := fs.String("summary", "", "Human-readable summary of the proposed action")
		cmdStr := fs.String("cmd", "", "Exact deterministic shell command to execute upon approval")
		draftID := fs.String("draft", "", "Optional Gmail or Google Chat draft ID when --type=gmail_send or --type=gchat_send")
		rsiID := fs.String("rsi-id", "", "Optional verified RSI hypothesis ID when --type=rsi_patch or --type=rsi_skill_patch")
		account := fs.String("account", "", "Optional account alias when --type=gmail_send or --type=gchat_send")
		workDir := fs.String("workdir", "", "Optional working directory for command execution")
		convID := fs.String("conv", "", "Optional conversation ID")
		ttlStr := fs.String("ttl", "", "Optional proposal expiration TTL duration (default 24h)")
		_ = fs.Parse(os.Args[2:])
		if *rsiID != "" {
			hqBin := "./bin/hq"
			if exe, err := os.Executable(); err == nil && vault.IsTempPath(exe) {
				hqBin = exe
			}
			prop, _, err := rsi.Propose(rootDir, vaultDir, *rsiID, *convID, hqBin)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq propose --rsi-id=%s failed: %v\n", *rsiID, err)
				os.Exit(1)
			}
			_ = webui.SyncMarkdownConsole(vaultDir)
			fmt.Printf("Staged proposal %s (sha256: %s)\n", prop.ID, prop.SHA256[:12])
			fmt.Printf("  Approve with: ./bin/hq approve %s --sha256=%s\n", prop.ID, prop.SHA256[:12])
			return
		}
		var meta map[string]string
		if *draftID != "" {
			if *actionType == "external_action" {
				if strings.HasPrefix(*draftID, "gchat-draft-") {
					*actionType = "gchat_send"
				} else {
					*actionType = "gmail_send"
				}
			}
			hqBin := "./bin/hq"
			if exe, err := os.Executable(); err == nil && vault.IsTempPath(exe) {
				hqBin = exe
			}
			if *actionType == "gchat_send" {
				spec, err := gchat.GetDraft(vaultDir, *account, *draftID)
				if err != nil {
					fmt.Fprintf(os.Stderr, "hq propose: load gchat draft %s failed: %v\n", *draftID, err)
					os.Exit(1)
				}
				hashShort := spec.ContentHash
				if len(hashShort) > 12 {
					hashShort = hashShort[:12]
				}
				if *summary == "" {
					*summary = gchat.ProposalSummary(spec)
				}
				if *cmdStr == "" {
					*cmdStr = fmt.Sprintf("%s gchat send-draft --account=%s --draft=%s --hash=%s", hqBin, spec.Account, spec.DraftID, hashShort)
				}
				if *workDir == "" {
					*workDir = rootDir
				}
				preview := spec.Body
				if len(preview) > 500 {
					preview = preview[:500] + "..."
				}
				meta = map[string]string{
					"draftId":      spec.DraftID,
					"account":      spec.Account,
					"from":         spec.From,
					"spaceId":      spec.SpaceID,
					"spaceName":    spec.SpaceName,
					"threadId":     spec.ThreadID,
					"bodyPreview":  preview,
					"draftPayload": spec.Body,
					"contentHash":  spec.ContentHash,
				}
			} else {
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
					"draftId":      spec.DraftID,
					"account":      spec.Account,
					"from":         spec.From,
					"to":           spec.To,
					"cc":           spec.Cc,
					"subject":      spec.Subject,
					"bodyPreview":  preview,
					"draftPayload": spec.Body,
					"contentHash":  spec.ContentHash,
				}
			}
		}
		if *cmdStr == "" || *summary == "" {
			fmt.Fprintln(os.Stderr, "usage: hq propose --type=<type> --summary=\"...\" --cmd=\"...\" (or --type=gmail_send --draft=<draftId>)")
			os.Exit(1)
		}
		if strings.TrimSpace(*ttlStr) != "" {
			if d, tErr := time.ParseDuration(strings.TrimSpace(*ttlStr)); tErr == nil {
				if meta == nil {
					meta = make(map[string]string)
				}
				meta["expiresAt"] = time.Now().Add(d).UTC().Format(time.RFC3339Nano)
			}
		}
		prop, err := approvals.CreateWithMetadata(vaultDir, *actionType, *summary, *cmdStr, *workDir, *convID, meta)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq propose failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Staged proposal %s (sha256: %s)\n", prop.ID, prop.SHA256[:12])
		if len(prop.RiskFlags) > 0 {
			fmt.Printf("  ⚠️ Contextual PII Warning: %s\n", strings.Join(prop.RiskFlags, ", "))
		}
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
		forceGatekeepers := fs.Bool("force-gatekeepers", false, "Evaluate all daemon_gatekeeper precheck_cmd triggers immediately regardless of cron minute")
		_ = fs.Parse(os.Args[2:])
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
		fmt.Printf("  - Tip: %s\n", kernel.FrontDeskPinTip)

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
		model := fs.String("model", "", "Optional model override (hydrogen, flash, carbon, sonnet, opus, or full model ID)")
		depth := fs.String("depth", "", "Optional thinking depth override (low, medium, high, xhigh)")
		tier := fs.String("tier", "", "Optional complexity tier override (quick, routine, standard, heavy, deep-medium, deep)")
		titleOverride := fs.String("title", "", "Optional custom conversation title")
		scopeOverride := fs.String("scope", "", "Optional custom task scope")
		force := fs.Bool("force", false, "Force spawn even if quota circuit breaker is open")
		ignoreBudget := fs.Bool("ignore-budget", false, "Alias for --force to bypass daily token budget circuit breaker")
		dryRun := fs.Bool("dry-run", false, "Simulate agentapi spawn")
		_ = fs.Parse(os.Args[2:])
		if *prompt == "" || (*taskName == "" && *role == "") {
			fmt.Fprintln(os.Stderr, "usage: hq spawn --task=<slug> [--subagent] [--parent=<conv-id>] [--role=\"...\"] [--repo=<repo>] [--tier=quick|routine|standard|heavy|deep-medium|deep] [--model=...] [--depth=low|medium|high|xhigh] [--force] [--ignore-budget] --prompt=\"...\"")
			os.Exit(1)
		}
		isForced := *force || *ignoreBudget
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
		if *scopeOverride != "" {
			scope = *scopeOverride
		} else if *repoSpec != "" {
			scope = filepath.Base(strings.TrimSuffix(*repoSpec, ".git"))
		} else if parts := strings.SplitN(*taskName, "-", 2); len(parts) > 0 && parts[0] != "" {
			scope = parts[0]
		}
		if scope == "investment" {
			scope = "investing"
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
			out, err := kernel.SpawnSubagent(vaultDir, *parentConv, subRole, fullPrompt, resolvedModel, workDir, *dryRun, isForced)
			_ = webui.SyncMarkdownConsole(vaultDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq spawn --subagent error: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(out)
			return
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
		})
		out, err := kernel.SpawnTask(vaultDir, "manual-spawn:"+*taskName, "swe-pulse", title, fullPrompt, resolvedModel, *dryRun, isForced)
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
		model := fs.String("model", "", "Optional model override (hydrogen, flash, carbon, sonnet, opus, or full model ID)")
		depth := fs.String("depth", "", "Optional thinking depth override (low, medium, high, xhigh)")
		tier := fs.String("tier", "", "Optional complexity tier override (quick, routine, standard, heavy, deep-medium, deep)")
		force := fs.Bool("force", false, "Force spawn even if quota circuit breaker is open")
		ignoreBudget := fs.Bool("ignore-budget", false, "Alias for --force to bypass daily token budget circuit breaker")
		dryRun := fs.Bool("dry-run", false, "Simulate subagent spawn")
		_ = fs.Parse(os.Args[2:])
		if *prompt == "" || (*role == "" && *taskName == "") {
			fmt.Fprintln(os.Stderr, "usage: hq subagent --role=\"[<tier>] <scope>: <role>\" [--tier=quick|routine|standard|heavy|deep-medium|deep] [--model=...] [--depth=low|medium|high|xhigh] [--parent=<conv-id>] [--repo=<repo>] [--task=<slug>] [--force] [--ignore-budget] --prompt=\"...\"")
			os.Exit(1)
		}
		isForced := *force || *ignoreBudget
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
		out, err := kernel.SpawnSubagent(vaultDir, *parentConv, subRole, fullPrompt, resolvedModel, workDir, *dryRun, isForced)
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

		case "accounts", "status":
			reg, err := gmail.LoadRegistry(vaultDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gmail accounts failed: %v\n", err)
				os.Exit(1)
			}
			b, _ := json.MarshalIndent(reg, "", "  ")
			fmt.Println(string(b))

		case "search", "list":
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

		case "read-quarantined", "read":
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

		case "send-draft", "send":
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

		case "precheck", "triage":
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

	case "gchat":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: hq gchat <init|add-account|accounts|login|launch-chrome|spaces|search|read-quarantined|draft|propose|send-draft|precheck|triage-result> [args]")
			os.Exit(1)
		}
		switch os.Args[2] {
		case "init", "add-account":
			fs := flag.NewFlagSet("gchat "+os.Args[2], flag.ExitOnError)
			alias := fs.String("alias", "personal", "Account alias (e.g. personal, work)")
			email := fs.String("email", "", "Google account address (e.g. you@gmail.com)")
			transport := fs.String("transport", "", "Transport adapter (cdp, gmail-bridge, oauth, webhook, mock; defaults to cdp for @gmail.com)")
			cdpEndpoint := fs.String("cdp-endpoint", gchat.DefaultCDPEndpoint, "Local Chrome DevTools Protocol HTTP endpoint")
			chromeProfile := fs.String("chrome-profile", "", "Optional path to isolated Chrome user-data-dir")
			gmailFallback := fs.String("gmail-fallback", "primary", "Gmail account alias for chat-noreply@google.com fallback")
			readTokenFile := fs.String("read-token-file", "", "Optional Workspace read-only OAuth2 token file")
			sendTokenFile := fs.String("send-token-file", "", "Optional Workspace chat.messages.create OAuth2 token file")
			webhookFile := fs.String("webhook-file", "", "Optional Space Incoming Webhook URL file")
			setDefault := fs.Bool("default", os.Args[2] == "init", "Set this account as the default Google Chat account")
			_ = fs.Parse(os.Args[3:])
			cfg, err := gchat.InitOrAddAccount(vaultDir, *alias, *email, *transport, *cdpEndpoint, *chromeProfile, *gmailFallback, *readTokenFile, *sendTokenFile, *webhookFile, *setDefault)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat %s failed: %v\n", os.Args[2], err)
				os.Exit(1)
			}
			_ = webui.SyncMarkdownConsole(vaultDir)
			fmt.Printf("Configured Google Chat account %q (%s) [type=%s, transport=%s, fallback=gmail:%s]\n",
				cfg.Alias, cfg.Email, cfg.AccountType, cfg.Transport, cfg.GmailFallbackAlias)

		case "accounts", "status":
			reg, err := gchat.LoadRegistry(vaultDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat accounts failed: %v\n", err)
				os.Exit(1)
			}
			b, _ := json.MarshalIndent(reg, "", "  ")
			fmt.Println(string(b))

		case "login":
			fs := flag.NewFlagSet("gchat login", flag.ExitOnError)
			account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
			reset := fs.Bool("reset", false, "Back up and clear existing session cookies to test a fresh first-time login")
			remote := fs.Bool("remote", false, "Run login in headless mode with automatic email entry, Google Prompt 2-digit readout, and PNG screenshot")
			screenshot := fs.String("screenshot", "", "Optional path to save live Google Sign-In challenge PNG screenshot")
			timeoutSec := fs.Int("timeout", 120, "Seconds to wait for Google Sign-In")
			_ = fs.Parse(os.Args[3:])
			ep, err := gchat.InteractiveLoginWithOptions(vaultDir, *account, gchat.LoginOptions{
				Reset:          *reset,
				RemoteHeadless: *remote,
				ScreenshotPath: *screenshot,
				Timeout:        time.Duration(*timeoutSec) * time.Second,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat login failed: %v\n", err)
				os.Exit(1)
			}
			spaces, _ := gchat.ListSpaces(vaultDir, *account, "", 50)
			fmt.Printf("Authenticated Google Chat session active in background headless mode (%s, %d space(s) cached)\n", ep, len(spaces))

		case "logout", "reset-session":
			fs := flag.NewFlagSet("gchat logout", flag.ExitOnError)
			account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
			_ = fs.Parse(os.Args[3:])
			msg, err := gchat.LogoutOrResetProfile(vaultDir, *account)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat logout failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(msg)

		case "restore-session":
			fs := flag.NewFlagSet("gchat restore-session", flag.ExitOnError)
			account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
			_ = fs.Parse(os.Args[3:])
			msg, err := gchat.RestoreProfileCookies(vaultDir, *account)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat restore-session failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(msg)

		case "launch-chrome":
			fs := flag.NewFlagSet("gchat launch-chrome", flag.ExitOnError)
			account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
			headless := fs.Bool("headless", true, "Launch Chrome in headless background mode (use `hq gchat login` for first-time interactive sign-in)")
			_ = fs.Parse(os.Args[3:])
			_, msg, err := gchat.LaunchChromeProfile(vaultDir, *account, *headless)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat launch-chrome failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(msg)

		case "spaces", "list-spaces":
			fs := flag.NewFlagSet("gchat spaces", flag.ExitOnError)
			account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
			query := fs.String("query", "", "Optional filter query")
			maxRes := fs.Int("max", 20, "Maximum spaces to return")
			_ = fs.Parse(os.Args[3:])
			spaces, err := gchat.ListSpaces(vaultDir, *account, *query, *maxRes)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat spaces failed: %v\n", err)
				os.Exit(1)
			}
			b, _ := json.MarshalIndent(spaces, "", "  ")
			fmt.Println(string(b))

		case "search", "list":
			fs := flag.NewFlagSet("gchat search", flag.ExitOnError)
			account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
			spaceID := fs.String("space", "", "Optional space name or ID filter (e.g. \"Project Launch Space\" or spaces/AAAA123)")
			query := fs.String("query", "", "Optional search query")
			maxRes := fs.Int("max", 20, "Maximum messages to return")
			_ = fs.Parse(os.Args[3:])
			msgs, err := gchat.SearchMessages(vaultDir, *account, *spaceID, *query, *maxRes)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat search failed: %v\n", err)
				os.Exit(1)
			}
			b, _ := json.MarshalIndent(msgs, "", "  ")
			fmt.Println(string(b))

		case "read-quarantined", "read":
			fs := flag.NewFlagSet("gchat read-quarantined", flag.ExitOnError)
			account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
			spaceID := fs.String("space", "", "Space name or ID to read in quarantined mode (e.g. spaces/AAAA123)")
			threadID := fs.String("thread", "", "Optional thread ID filter")
			maxRes := fs.Int("max", 20, "Maximum messages to read")
			convID := fs.String("conv", "", "Optional conversation ID (defaults to ANTIGRAVITY_CONVERSATION_ID)")
			_ = fs.Parse(os.Args[3:])
			_, formatted, err := gchat.ReadQuarantinedThread(vaultDir, *account, *spaceID, *threadID, *convID, *maxRes)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat read-quarantined failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Print(formatted)

		case "draft":
			fs := flag.NewFlagSet("gchat draft", flag.ExitOnError)
			account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
			draftID := fs.String("draft", "", "Optional existing draft ID to update")
			toSpace := fs.String("to", "", "Target space name or ID (e.g. \"Project Launch Space\" or spaces/AAAA123)")
			spaceID := fs.String("space", "", "Alias for --to: target space name or ID")
			spaceName := fs.String("space-name", "", "Optional display name of the target space")
			threadID := fs.String("thread", "", "Optional thread ID being replied to")
			body := fs.String("body", "", "Plain-text message body")
			convID := fs.String("conv", "", "Optional conversation ID")
			_ = fs.Parse(os.Args[3:])
			target := strings.TrimSpace(*toSpace)
			if target == "" {
				target = strings.TrimSpace(*spaceID)
			}
			spec, err := gchat.CreateOrUpdateDraft(vaultDir, *account, *draftID, target, *spaceName, *threadID, *body, *convID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat draft failed: %v\n", err)
				os.Exit(1)
			}
			b, _ := json.MarshalIndent(spec, "", "  ")
			fmt.Println(string(b))

		case "propose", "stage-send":
			fs := flag.NewFlagSet("gchat propose", flag.ExitOnError)
			account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
			draftID := fs.String("draft", "", "Optional existing draft ID to update or propose")
			toSpace := fs.String("to", "", "Target space name or ID (e.g. \"Project Launch Space\" or spaces/AAAA123)")
			spaceID := fs.String("space", "", "Alias for --to: target space name or ID")
			spaceName := fs.String("space-name", "", "Optional display name of the target space")
			threadID := fs.String("thread", "", "Optional thread ID being replied to")
			body := fs.String("body", "", "Plain-text message body")
			convID := fs.String("conv", "", "Optional conversation ID")
			_ = fs.Parse(os.Args[3:])
			target := strings.TrimSpace(*toSpace)
			if target == "" {
				target = strings.TrimSpace(*spaceID)
			}
			var spec gchat.DraftSpec
			var err error
			if target == "" && *body == "" && *draftID != "" {
				spec, err = gchat.GetDraft(vaultDir, *account, *draftID)
			} else {
				spec, err = gchat.CreateOrUpdateDraft(vaultDir, *account, *draftID, target, *spaceName, *threadID, *body, *convID)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat propose failed: %v\n", err)
				os.Exit(1)
			}
			hqBin := "./bin/hq"
			if exe, eErr := os.Executable(); eErr == nil && vault.IsTempPath(exe) {
				hqBin = exe
			}
			hashShort := spec.ContentHash
			if len(hashShort) > 12 {
				hashShort = hashShort[:12]
			}
			summary := gchat.ProposalSummary(spec)
			cmdStr := fmt.Sprintf("%s gchat send-draft --account=%s --draft=%s --hash=%s",
				hqBin, spec.Account, spec.DraftID, hashShort)
			preview := spec.Body
			if len(preview) > 500 {
				preview = preview[:500] + "..."
			}
			meta := map[string]string{
				"draftId":      spec.DraftID,
				"account":      spec.Account,
				"from":         spec.From,
				"spaceId":      spec.SpaceID,
				"spaceName":    spec.SpaceName,
				"threadId":     spec.ThreadID,
				"bodyPreview":  preview,
				"draftPayload": spec.Body,
				"contentHash":  spec.ContentHash,
			}
			prop, err := approvals.CreateWithMetadata(vaultDir, "gchat_send", summary, cmdStr, rootDir, *convID, meta)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat propose failed: %v\n", err)
				os.Exit(1)
			}
			_ = webui.SyncMarkdownConsole(vaultDir)
			fmt.Printf("Staged Google Chat proposal %s (draft: %s, space: %s [%s], sha256: %s)\n",
				prop.ID, spec.DraftID, spec.SpaceName, spec.SpaceID, prop.SHA256[:12])
			fmt.Printf("  Approve with: ./bin/hq approve %s --sha256=%s\n", prop.ID, prop.SHA256[:12])

		case "send-draft", "send":
			fs := flag.NewFlagSet("gchat send-draft", flag.ExitOnError)
			account := fs.String("account", "", "Account alias or email")
			draftID := fs.String("draft", "", "Draft ID to verify and send")
			hashPrefix := fs.String("hash", "", "Expected contentHash prefix from approved proposal")
			_ = fs.Parse(os.Args[3:])
			out, err := gchat.VerifyAndSendDraft(vaultDir, *account, *draftID, *hashPrefix)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat send-draft failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(out)

		case "precheck", "triage":
			fs := flag.NewFlagSet("gchat precheck", flag.ExitOnError)
			account := fs.String("account", "", "Optional account alias filter")
			_ = fs.Parse(os.Args[3:])
			batches, status, err := gchat.RunPrecheck(vaultDir, *account)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat precheck failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(status)
			if len(batches) > 0 {
				b, _ := json.MarshalIndent(batches, "", "  ")
				fmt.Println(string(b))
			}

		case "triage-result":
			fs := flag.NewFlagSet("gchat triage-result", flag.ExitOnError)
			batchID := fs.String("batch", "", "Batch ID being summarized")
			rawJSON := fs.String("json", "", "JSON array of TriageItem objects")
			_ = fs.Parse(os.Args[3:])
			var items []gchat.TriageItem
			if err := json.Unmarshal([]byte(*rawJSON), &items); err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat triage-result invalid JSON: %v\n", err)
				os.Exit(1)
			}
			if err := gchat.RecordTriageResults(vaultDir, *batchID, items); err != nil {
				fmt.Fprintf(os.Stderr, "hq gchat triage-result validation failed: %v\n", err)
				os.Exit(1)
			}
			_ = webui.SyncMarkdownConsole(vaultDir)
			fmt.Printf("Recorded %d triaged Google Chat thread(s) for batch %s\n", len(items), *batchID)

		default:
			fmt.Fprintf(os.Stderr, "unknown gchat subcommand: %s\n", os.Args[2])
			os.Exit(1)
		}

	case "daemon", "webui":
		if os.Args[1] == "daemon" && len(os.Args) >= 3 && !strings.HasPrefix(os.Args[2], "-") {
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

		fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
		defaultAddr := "127.0.0.1:4242"
		if p := os.Getenv("ANTIGRAVITY_SIDECAR_WEB_PORT"); p != "" {
			defaultAddr = "127.0.0.1:" + p
		} else if p := os.Getenv("PORT"); p != "" {
			defaultAddr = "127.0.0.1:" + p
		}
		addr := fs.String("addr", defaultAddr, "Listen address for HQ Console HTTP server (loopback only by default)")
		allowRemote := fs.Bool("allow-remote", false, "Explicitly permit binding HQ Console HTTP server to a non-loopback interface")
		dryRun := fs.Bool("dry-run", false, "Simulate agentapi spawns in background watcher")
		daemonArgs := os.Args[2:]
		if len(daemonArgs) > 0 && (daemonArgs[0] == "start" || daemonArgs[0] == "run") {
			daemonArgs = daemonArgs[1:]
		}
		_ = fs.Parse(daemonArgs)

		if !*allowRemote && !webui.IsLoopbackAddr(*addr) {
			fmt.Fprintf(os.Stderr, "hq daemon security error: refusing to bind HTTP console to non-loopback address %q (use 127.0.0.1:<port> or pass --allow-remote)\n", *addr)
			os.Exit(1)
		}

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

		// Background 15s inbox watcher, 30s cron/coalescer tick, & 30m 0-token heartbeat loop
		go func() {
			_, _ = kernel.RunHeartbeat(vaultDir, *dryRun)
			_ = webui.SyncMarkdownConsole(vaultDir)
			inboxTicker := time.NewTicker(15 * time.Second)
			cronTicker := time.NewTicker(30 * time.Second)
			hbTicker := time.NewTicker(30 * time.Minute)
			defer inboxTicker.Stop()
			defer cronTicker.Stop()
			defer hbTicker.Stop()
			for {
				select {
				case <-inboxTicker.C:
					if out, _ := kernel.ProcessInbox(vaultDir, *dryRun); len(out) > 0 {
						_ = webui.SyncMarkdownConsole(vaultDir)
					}
				case <-cronTicker.C:
					if gkActs, _, _ := kernel.EvaluateDaemonGatekeepers(vaultDir, time.Now(), false, *dryRun); len(gkActs) > 0 {
						_ = webui.SyncMarkdownConsole(vaultDir)
					}
				case <-hbTicker.C:
					_, _ = kernel.RunHeartbeat(vaultDir, *dryRun)
					_ = webui.SyncMarkdownConsole(vaultDir)
				}
			}
		}()

		handler := webui.NewHandler(vaultDir)
		if *addr != "127.0.0.1:4242" && !strings.HasSuffix(*addr, ":4242") {
			go func() {
				_ = http.ListenAndServe("127.0.0.1:4242", handler)
			}()
		}

		fmt.Printf("Starting agy-hq Console & Daemon on http://%s (Vault: %s)\n", *addr, vaultDir)
		if err := http.ListenAndServe(*addr, handler); err != nil {
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
			fmt.Printf("Entity Dossiers (entities/):      %d dossier(s) (0 SessionStart tokens; on-demand lookup)\n", rep.EntityCount)
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
			if res, tErr := kernel.CompactTasks(vaultDir, kernel.CompactTasksOptions{}); tErr == nil {
				_ = webui.SyncMarkdownConsole(vaultDir)
				fmt.Printf("Compacted tasks.json: %d -> %d hot tasks (rotated %d to tasks_archive.jsonl, total cold: %d)\n",
					res.HotBefore, res.HotAfter, len(res.RotatedColdSlugs), res.TotalColdAfter)
			}

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

		case "forget":
			var posQ string
			var flagArgs []string
			for _, a := range os.Args[3:] {
				if !strings.HasPrefix(a, "-") && posQ == "" {
					posQ = a
				} else {
					flagArgs = append(flagArgs, a)
				}
			}
			fs := flag.NewFlagSet("memory forget", flag.ExitOnError)
			query := fs.String("query", "", "Substring or fact ID to scrub across facts, MEMORY.md, topics, entities, and notes")
			topicSlug := fs.String("topic", "", "Optional topic wiki slug to delete (local/memory/topics/<slug>.md)")
			entitySlug := fs.String("entity", "", "Optional entity dossier slug to delete (local/memory/entities/<slug>.md)")
			blockPII := fs.Bool("block-pii", false, "Also append --query to local/private/pii-denylist.txt")
			dryRun := fs.Bool("dry-run", false, "Preview cascading deletions without mutating files")
			asJSON := fs.Bool("json", false, "Output JSON report")
			_ = fs.Parse(flagArgs)
			if *query == "" {
				*query = posQ
			}
			rep, err := vault.ForgetMemory(vaultDir, *query, *topicSlug, *entitySlug, *blockPII, *dryRun)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq memory forget failed: %v\n", err)
				os.Exit(1)
			}
			if !*dryRun {
				_ = webui.SyncMarkdownConsole(vaultDir)
			}
			if *asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				_ = enc.Encode(rep)
				return
			}
			mode := "Executed"
			if *dryRun {
				mode = "[DRY-RUN] Previewed"
			}
			fmt.Printf("%s surgical forget: expiredFacts=%d, memoryLinesRemoved=%d, deletedFiles=%d, scrubbedFiles=%d, denylistUpdated=%v\n",
				mode, len(rep.ExpiredFactIDs), rep.MemoryLinesRemoved, len(rep.DeletedFiles), len(rep.ScrubbedFiles), rep.DenylistUpdated)

		case "entities":
			fs := flag.NewFlagSet("memory entities", flag.ExitOnError)
			asJSON := fs.Bool("json", false, "Output JSON")
			_ = fs.Parse(os.Args[3:])
			list, err := vault.ListEntities(vaultDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq memory entities failed: %v\n", err)
				os.Exit(1)
			}
			if *asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				_ = enc.Encode(list)
				return
			}
			for _, d := range list {
				fmt.Printf("- [%s] %s (%s): %s\n", d.Slug, d.Name, d.Kind, d.Context)
			}

		case "add-entity":
			fs := flag.NewFlagSet("memory add-entity", flag.ExitOnError)
			slug := fs.String("slug", "", "Entity slug (defaults to kebab-case of --name)")
			name := fs.String("name", "", "Display name")
			kind := fs.String("kind", "person", "Entity kind (person, organization, project)")
			email := fs.String("email", "", "Comma-separated email(s)")
			handle := fs.String("handle", "", "Comma-separated handle(s)")
			space := fs.String("space", "", "Comma-separated Google Chat space ID(s)")
			ctxStr := fs.String("context", "", "1-line relationship / role summary")
			note := fs.String("note", "", "Optional interaction or working-style note to append")
			asJSON := fs.Bool("json", false, "Output JSON")
			_ = fs.Parse(os.Args[3:])
			var emails, handles, spaces []string
			if *email != "" {
				emails = []string{*email}
			}
			if *handle != "" {
				handles = []string{*handle}
			}
			if *space != "" {
				spaces = []string{*space}
			}
			ent, err := vault.UpsertEntity(vaultDir, vault.EntityDossier{
				Slug:    *slug,
				Name:    *name,
				Kind:    *kind,
				Emails:  emails,
				Handles: handles,
				Spaces:  spaces,
				Context: *ctxStr,
			}, *note)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq memory add-entity failed: %v\n", err)
				os.Exit(1)
			}
			if *asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				_ = enc.Encode(ent)
				return
			}
			fmt.Printf("Saved entity dossier %s (%s) at %s\n", ent.Slug, ent.Name, ent.Path)

		case "lookup":
			var posQ string
			var flagArgs []string
			for _, a := range os.Args[3:] {
				if !strings.HasPrefix(a, "-") && posQ == "" {
					posQ = a
				} else {
					flagArgs = append(flagArgs, a)
				}
			}
			fs := flag.NewFlagSet("memory lookup", flag.ExitOnError)
			qFlag := fs.String("query", "", "Query string (name, email, handle, space ID, or context)")
			asJSON := fs.Bool("json", false, "Output JSON")
			_ = fs.Parse(flagArgs)
			if posQ == "" {
				posQ = *qFlag
			}
			hits, err := vault.LookupEntities(vaultDir, posQ)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq memory lookup failed: %v\n", err)
				os.Exit(1)
			}
			if *asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				_ = enc.Encode(hits)
				return
			}
			if len(hits) == 0 {
				fmt.Printf("No entity dossiers matched %q.\n", posQ)
				return
			}
			for _, d := range hits {
				fmt.Printf("- [%s] %s (%s): %s (emails=%s, spaces=%s)\n",
					d.Slug, d.Name, d.Kind, d.Context, strings.Join(d.Emails, ","), strings.Join(d.Spaces, ","))
				for _, n := range d.Notes {
					fmt.Printf("    * %s\n", n)
				}
			}

		default:
			fmt.Fprintf(os.Stderr, "unknown memory subcommand: %s (expected status, topics, add-fact, prune-fact, compact, verify-transcript, forget, entities, add-entity, lookup)\n", sub)
			os.Exit(1)
		}

	case "tasks":
		runTasksCLI(vaultDir, os.Args[2:])

	case "outbox":
		runOutboxCLI(vaultDir, os.Args[2:])

	case "ideas":
		runIdeasCLI(vaultDir, os.Args[2:])

	case "goals":
		runGoalsCLI(vaultDir, os.Args[2:])

	case "spaces":
		runSpacesCLI(vaultDir, os.Args[2:])

	case "obs":
		runObsCLI(vaultDir, os.Args[2:])

	case "rsi":
		runRSICLI(rootDir, vaultDir, os.Args[2:])

	case "ci":
		runCICLI(rootDir, os.Args[2:])

	default:
		printUsage()
		os.Exit(1)
	}
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
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				_ = enc.Encode(stats)
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
			asJSON := fs.Bool("json", false, "Output JSON")
			_ = fs.Parse(args[1:])
			res, err := kernel.CompactTasks(vaultDir, kernel.CompactTasksOptions{
				Now:    now,
				DryRun: *dryRun,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq tasks compact error: %v\n", err)
				os.Exit(1)
			}
			if !*dryRun {
				_ = webui.SyncMarkdownConsole(vaultDir)
			}
			if *asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				_ = enc.Encode(res)
				return
			}
			prefix := "Compacted"
			if *dryRun {
				prefix = "[DRY-RUN] Would compact"
			}
			fmt.Printf("%s tasks.json: %d -> %d hot tasks (auto-archived: %d, rotated to cold archive: %d, total cold archive: %d)\n",
				prefix, res.HotBefore, res.HotAfter, len(res.AutoArchivedSlugs), len(res.RotatedColdSlugs), res.TotalColdAfter)
			for _, slug := range res.RotatedColdSlugs {
				fmt.Printf("  - rotated: %s\n", slug)
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
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(filtered)
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
	convShort := t.ConversationID
	if len(convShort) > 8 {
		convShort = convShort[:8]
	}
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

func runRSICLI(rootDir, vaultDir string, args []string) {
	sub := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "mine":
		fs := flag.NewFlagSet("rsi mine", flag.ExitOnError)
		sinceStr := fs.String("since", "0s", "Lookback duration (e.g. 24h, 168h, or 0s for all)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		var since time.Duration
		if *sinceStr != "" && *sinceStr != "0s" && *sinceStr != "0" {
			if d, err := time.ParseDuration(*sinceStr); err == nil {
				since = d
			}
		}
		events, err := rsi.Mine(rootDir, vaultDir, since)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi mine failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(events)
			return
		}
		fmt.Printf("RSI Stage 1 (Mine): %d deduplicated friction event(s) in local/memory/rsi/observations.jsonl\n", len(events))
		for _, ev := range events {
			fmt.Printf("  - [%s] %s (%s · fault:%s · count:%d · wasted:%d · status:%s)\n",
				ev.ID, ev.SkillOrPkg, ev.Category, ev.FaultLayer, ev.OccurrenceCount, ev.WastedSteps, ev.Status)
			fmt.Printf("      Excerpt: %s\n", ev.ErrorExcerpt)
		}

	case "hypothesize":
		var targetObs string
		var flagArgs []string
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && targetObs == "" {
				targetObs = a
			} else {
				flagArgs = append(flagArgs, a)
			}
		}
		fs := flag.NewFlagSet("rsi hypothesize", flag.ExitOnError)
		obsFlag := fs.String("obs", "", "Optional observation ID or fingerprint")
		force := fs.Bool("force", false, "Bypass Meta-ROI and 75% Quota Circuit Breaker thresholds")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if *obsFlag != "" {
			targetObs = *obsFlag
		}
		hyps, err := rsi.Hypothesize(rootDir, vaultDir, targetObs, *force)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi hypothesize failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(hyps)
			return
		}
		fmt.Printf("RSI Stage 2 (Hypothesize): synthesized %d hypothesis specification(s)\n", len(hyps))
		for _, h := range hyps {
			fmt.Printf("  - %s [%s · fault:%s · savedSteps:%d]: %s\n",
				h.ID, h.Ring.Label(), h.FaultLayer, h.ExpectedSavedSteps, h.Title)
		}

	case "verify":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq rsi verify <rsi-id> [--json]")
			os.Exit(1)
		}
		rsiID := args[0]
		fs := flag.NewFlagSet("rsi verify", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args[1:])
		h, err := rsi.Verify(rootDir, vaultDir, rsiID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi verify failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(h)
		} else {
			v := h.Verification
			fmt.Printf("RSI Stage 3 (4-Gate Sandboxed Verification) — %s [%s]\n", h.ID, strings.ToUpper(h.Status))
			fmt.Printf("  Gate 1 (TCB & Anti-Goodhart AST): %v — %s\n", v.Gate1TCBPassed, v.Gate1Detail)
			fmt.Printf("  Gate 2 (Counterfactual Repro):    %v — %s\n", v.Gate2ReproPassed, v.Gate2Detail)
			fmt.Printf("  Gate 3 (Tests & PII Guard):       %v — %s\n", v.Gate3TestSuitePassed, v.Gate3Detail)
			fmt.Printf("  Gate 4 (Budget & Contradiction):  %v — %s\n", v.Gate4BudgetPassed, v.Gate4Detail)
		}
		if h.Verification == nil || !h.Verification.AllPassed {
			os.Exit(1)
		}

	case "propose":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq rsi propose <rsi-id> [--conv=<conv-id>]")
			os.Exit(1)
		}
		rsiID := args[0]
		fs := flag.NewFlagSet("rsi propose", flag.ExitOnError)
		convID := fs.String("conv", "", "Optional conversation ID")
		_ = fs.Parse(args[1:])
		hqBin := "./bin/hq"
		if exe, err := os.Executable(); err == nil && vault.IsTempPath(exe) {
			hqBin = exe
		}
		prop, h, err := rsi.Propose(rootDir, vaultDir, rsiID, *convID, hqBin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi propose failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Staged RSI proposal %s for %s (type: %s, sha256: %s)\n", prop.ID, h.ID, prop.ActionType, prop.SHA256[:12])
		fmt.Printf("  Approve with: ./bin/hq approve %s --sha256=%s\n", prop.ID, prop.SHA256[:12])

	case "apply":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq rsi apply <rsi-id> --expect-diff-sha256=<prefix>")
			os.Exit(1)
		}
		rsiID := args[0]
		fs := flag.NewFlagSet("rsi apply", flag.ExitOnError)
		expectSHA := fs.String("expect-diff-sha256", "", "Expected diff SHA-256 prefix from verified proposal")
		_ = fs.Parse(args[1:])
		out, err := rsi.Apply(rootDir, vaultDir, rsiID, *expectSHA)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi apply failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Print(out)

	case "ablate":
		fs := flag.NewFlagSet("rsi ablate", flag.ExitOnError)
		model := fs.String("model", "", "Target upgraded model ID to ablate older model-scoped rules against")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		staged, err := rsi.Ablate(rootDir, vaultDir, *model)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi ablate failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(staged)
			return
		}
		fmt.Printf("RSI Ablation Sweep: staged %d deprecation hypothesis(es)\n", len(staged))
		for _, h := range staged {
			fmt.Printf("  - %s: %s (%s)\n", h.ID, h.Title, h.RootCause)
		}

	case "status":
		fs := flag.NewFlagSet("rsi status", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		st := rsi.GetStatus(rootDir, vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(st)
			return
		}
		fmt.Printf("HQ Recursive Self-Improvement (RSI) Engine Status\n")
		fmt.Printf("  Observations (observations.jsonl): %d total (%d open)\n", st.TotalObservations, st.OpenObservations)
		fmt.Printf("  Hypotheses (hypotheses/*.json):    %d total (%d verified ready)\n", st.TotalHypotheses, st.VerifiedReady)
		fmt.Printf("  Canary Archive (archive.jsonl):    %d active canary, %d promoted\n", st.ActiveCanaries, st.PromotedPatches)
		fmt.Printf("  Skill Rule Budget:                 %d active rsi:rule block(s) across %d skill(s) (cap: %d skills)\n",
			st.ActiveSkillRules, st.TotalSkills, rsi.MaxGlobalSkills)
		if !st.QuotaAllowed {
			fmt.Printf("  Quota Circuit Breaker:             PAUSED (%s)\n", st.QuotaMessage)
		} else {
			fmt.Printf("  Quota Circuit Breaker:             OK (<75%% rolling utilization)\n")
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown rsi subcommand: %s (expected mine, hypothesize, verify, propose, apply, ablate, status)\n", sub)
		os.Exit(1)
	}
}

func runCICLI(rootDir string, args []string) {
	sub := "check-all"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "check-pii":
		fs := flag.NewFlagSet("ci check-pii", flag.ExitOnError)
		staged := fs.Bool("staged", false, "Audit only staged git index changes")
		history := fs.Bool("history", false, "Audit full git commit history (git log --all -p), author metadata, and refs")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		var violations []ci.PIIViolation
		var err error
		mode := "tracked repository tree"
		if *history {
			mode = "full git commit history"
			violations, err = ci.CheckPIIHistory(rootDir)
		} else {
			if *staged {
				mode = "staged git index"
			}
			violations, err = ci.CheckPII(rootDir, *staged)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq ci check-pii error: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(violations)
		} else if len(violations) == 0 {
			fmt.Printf("[PASS] Zero PII, Vault boundary, or credential leaks in %s (Invariants P1-P6 clean)\n", mode)
		} else {
			fmt.Fprintf(os.Stderr, "[FAIL] %d PII / Vault boundary violation(s) detected in %s:\n", len(violations), mode)
			for _, v := range violations {
				fmt.Fprintf(os.Stderr, "  - %s\n", v.String())
			}
		}
		if len(violations) > 0 {
			os.Exit(1)
		}

	case "scrub-history":
		fs := flag.NewFlagSet("ci scrub-history", flag.ExitOnError)
		replacements := fs.String("replacements", "", "Path to deterministic replacement rules file (literal==>replacement)")
		apply := fs.Bool("apply", false, "Rewrite git history in-place (defaults to read-only dry-run)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		if strings.TrimSpace(*replacements) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq ci scrub-history --replacements=<path> [--apply] [--json]")
			os.Exit(1)
		}
		rep, err := ci.ScrubGitHistory(rootDir, *replacements, *apply)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq ci scrub-history failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(rep)
		} else if rep.DryRun {
			fmt.Printf("[DRY-RUN] Evaluated %d replacement rule(s): %d match(es) across %d commit(s) %v\n",
				rep.RuleCount, rep.MatchedReplacements, rep.MatchedCommits, rep.SampleCommits)
		} else {
			fmt.Printf("[APPLIED] Rewrote git history across %d matched commit(s) (%d replacement(s) from %d rule(s)) and pruned backup refs.\n",
				rep.MatchedCommits, rep.MatchedReplacements, rep.RuleCount)
		}

	case "apply-replacements":
		fs := flag.NewFlagSet("ci apply-replacements", flag.ExitOnError)
		dir := fs.String("dir", ".", "Target directory to scrub")
		file := fs.String("file", "", "Replacement rules file")
		_ = fs.Parse(args)
		targetDir, err := filepath.Abs(*dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "apply-replacements resolve dir: %v\n", err)
			os.Exit(1)
		}
		if _, err := ci.ApplyReplacementRulesToTree(targetDir, *file); err != nil {
			fmt.Fprintf(os.Stderr, "apply-replacements failed: %v\n", err)
			os.Exit(1)
		}

	case "apply-replacements-stdin":
		fs := flag.NewFlagSet("ci apply-replacements-stdin", flag.ExitOnError)
		file := fs.String("file", "", "Replacement rules file")
		_ = fs.Parse(args)
		rules, err := ci.ParseReplacementRules(*file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "apply-replacements-stdin parse rules: %v\n", err)
			os.Exit(1)
		}
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(1)
		}
		updated, _ := ci.ApplyReplacementRulesToText(string(raw), rules)
		fmt.Print(updated)

	case "check-harness":
		fs := flag.NewFlagSet("ci check-harness", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		results, allOK := ci.CheckHarness(rootDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(results)
		} else {
			for _, r := range results {
				status := "PASS"
				if !r.Passed {
					status = "FAIL"
				}
				fmt.Printf("[%s] %-4s %-52s %s\n", status, r.Gate, r.Name, r.Detail)
			}
		}
		if !allOK {
			os.Exit(1)
		}

	case "pre-commit":
		if err := ci.RunPreCommit(rootDir); err != nil {
			fmt.Fprintf(os.Stderr, "hq ci pre-commit failed: %v\n", err)
			os.Exit(1)
		}

	case "pre-push", "check-all":
		fs := flag.NewFlagSet("ci pre-push", flag.ExitOnError)
		skipTests := fs.Bool("skip-tests", false, "Skip `go test ./...` and run only PII + harness contract checks")
		_ = fs.Parse(args)
		if err := ci.RunPrePush(rootDir, *skipTests); err != nil {
			fmt.Fprintf(os.Stderr, "hq ci pre-push failed: %v\n", err)
			os.Exit(1)
		}

	case "install-hooks":
		updated, err := ci.InstallGitHooks(rootDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq ci install-hooks failed: %v\n", err)
			os.Exit(1)
		}
		if len(updated) == 0 {
			fmt.Println("Git hooks (.git/hooks/pre-commit & .git/hooks/pre-push) are already up to date.")
		} else {
			fmt.Printf("Installed/updated %d git hook(s): %s\n", len(updated), strings.Join(updated, ", "))
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown ci subcommand: %s (expected check-pii, check-harness, scrub-history, pre-commit, pre-push, install-hooks)\n", sub)
		os.Exit(1)
	}
}

func runObsCLI(vaultDir string, args []string) {
	sub := "tokens"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	now := time.Now()

	switch sub {
	case "tokens", "summary":
		fs := flag.NewFlagSet("obs tokens", flag.ExitOnError)
		since := fs.String("since", "7d", "Time window (today, 24h, 7d, 30d, all)")
		groupBy := fs.String("by", "day", "Group by dimension (day, hour, model, tier, kind, scope)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)

		rep, err := obs.BuildSummaryReport(vaultDir, *since, *groupBy, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq obs tokens failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(rep)
			return
		}

		fmt.Printf("HQ Token & Context Observability (Window: %s | GroupBy: %s)\n", rep.Since, rep.GroupBy)
		fmt.Printf("════════════════════════════════════════════════════════════════════════════════════════\n")
		fmt.Printf("  Uncached Burn (In+Out): %s  (Input: %s | Output: %s | QBU: %.0f | Est: $%.2f)\n",
			obs.FormatTokensCompact(rep.Totals.UncachedTokens),
			obs.FormatTokensCompact(rep.Totals.InputTokens),
			obs.FormatTokensCompact(rep.Totals.OutputTokens),
			rep.Totals.QuotaUnits,
			rep.Totals.EstCostUSD)
		fmt.Printf("  Gross Billed (w/Cache): %s  (Cache Read: %s | Prefix Cache Hit Ratio: %.1f%%)\n",
			obs.FormatTokensCompact(rep.Totals.GrossTokens),
			obs.FormatTokensCompact(rep.Totals.CacheReadTokens),
			rep.Totals.CacheHitRatio*100.0)
		fmt.Printf("  Activity Volume:        %d conversations | %d turns | %d steps | Active Latency: %s\n",
			rep.ConversationCount,
			rep.Totals.TurnCount,
			rep.Totals.StepCount,
			obs.FormatDurationCompact(rep.Totals.ActiveLatencyMs))
		dailyPct := 0.0
		if rep.Budget.DailyUncachedBudget > 0 {
			dailyPct = (float64(rep.TodayUncached) / float64(rep.Budget.DailyUncachedBudget)) * 100.0
		}
		deepPct := 0.0
		if rep.Budget.DailyDeepUncachedBudget > 0 {
			deepPct = (float64(rep.TodayDeepUncached) / float64(rep.Budget.DailyDeepUncachedBudget)) * 100.0
		}
		fmt.Printf("  Today vs Daily Budget:  %s / %s uncached (%.1f%%) | Deep: %s / %s (%.1f%%)\n",
			obs.FormatTokensCompact(rep.TodayUncached),
			obs.FormatTokensCompact(rep.Budget.DailyUncachedBudget),
			dailyPct,
			obs.FormatTokensCompact(rep.TodayDeepUncached),
			obs.FormatTokensCompact(rep.Budget.DailyDeepUncachedBudget),
			deepPct)
		if rep.FrontDesk != nil {
			fdID := rep.FrontDeskConvID
			if len(fdID) > 8 {
				fdID = fdID[:8]
			}
			fmt.Printf("  [HQ] Front Desk (%s): Context %s / 256k (Peak: %s | Compactions: %d | CHR: %.1f%%)\n",
				fdID,
				obs.FormatTokensCompact(rep.FrontDeskContext),
				obs.FormatTokensCompact(rep.FrontDesk.PeakContextTokens),
				rep.FrontDesk.CompactionCount,
				rep.FrontDesk.CacheHitRatio*100.0)
		}

		fmt.Printf("\nBreakdown by %s:\n", strings.ToUpper(rep.GroupBy))
		fmt.Printf("  %-22s %6s %6s %10s %10s %10s %7s %8s %8s\n",
			"KEY", "CONVS", "TURNS", "UNCACHED", "CACHE_RD", "GROSS", "CHR%", "QBU", "LATENCY")
		for _, b := range rep.Buckets {
			fmt.Printf("  %-22s %6d %6d %10s %10s %10s %6.1f%% %8.0f %8s\n",
				truncateDisplay(b.Key, 22),
				b.ConvCount,
				b.Rollup.TurnCount,
				obs.FormatTokensCompact(b.Rollup.UncachedTokens),
				obs.FormatTokensCompact(b.Rollup.CacheReadTokens),
				obs.FormatTokensCompact(b.Rollup.GrossTokens),
				b.Rollup.CacheHitRatio*100.0,
				b.Rollup.QuotaUnits,
				obs.FormatDurationCompact(b.Rollup.ActiveLatencyMs))
		}

		if len(rep.Anomalies) > 0 {
			fmt.Printf("\nActive Anomalies & Guard Alerts (%d):\n", len(rep.Anomalies))
			maxShow := len(rep.Anomalies)
			if maxShow > 8 {
				maxShow = 8
			}
			for _, a := range rep.Anomalies[:maxShow] {
				fmt.Printf("  - [%s][%s] %s\n", a.Severity, a.Code, a.Summary)
			}
			if len(rep.Anomalies) > maxShow {
				fmt.Printf("  ... and %d more (run './bin/hq obs anomalies' for full list)\n", len(rep.Anomalies)-maxShow)
			}
		}

	case "top":
		fs := flag.NewFlagSet("obs top", flag.ExitOnError)
		sortBy := fs.String("by", "uncached", "Sort metric (uncached, qbu, gross, context, latency)")
		kind := fs.String("kind", "all", "Filter by kind (all, task, subagent, front_desk)")
		treeMode := fs.Bool("tree", false, "Rank by full DAG tree rollup (Self + Descendant Subagents)")
		turnsMode := fs.Bool("turns", false, "Rank individual expensive turns across all transcripts")
		limit := fs.Int("limit", 10, "Maximum rows to display")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)

		rep, err := obs.BuildTopReport(vaultDir, *sortBy, *kind, *treeMode, *turnsMode, *limit, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq obs top failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(rep)
			return
		}

		if *turnsMode {
			fmt.Printf("Top %d Individual Turns (Sorted by: %s | Kind: %s)\n", len(rep.TurnItems), rep.SortBy, rep.KindFilter)
			fmt.Printf("  %-4s %-10s %-6s %-26s %6s %10s %10s %7s %9s %8s\n",
				"RANK", "CONV", "TURN", "TITLE", "STEPS", "UNCACHED", "GROSS", "CHR%", "PEAK_CTX", "LATENCY")
			for _, item := range rep.TurnItems {
				cid := item.ConversationID
				if len(cid) > 8 {
					cid = cid[:8]
				}
				fmt.Printf("  #%-3d %-10s T#%-4d %-26s %6d %10s %10s %6.1f%% %9s %8s\n",
					item.Rank,
					cid,
					item.Turn.TurnIndex,
					truncateDisplay(item.Title, 26),
					item.Turn.StepCount,
					obs.FormatTokensCompact(item.Turn.UncachedTokens),
					obs.FormatTokensCompact(item.Turn.GrossTokens),
					item.Turn.CacheHitRatio*100.0,
					obs.FormatTokensCompact(item.Turn.PeakContextTokens),
					obs.FormatDurationCompact(item.Turn.ActiveLatencyMs))
			}
			return
		}

		modeLabel := "Self"
		if *treeMode {
			modeLabel = "Tree (Self + Children)"
		}
		fmt.Printf("Top %d Conversations — %s (Sorted by: %s | Kind: %s)\n", len(rep.Items), modeLabel, rep.SortBy, rep.KindFilter)
		fmt.Printf("  %-4s %-10s %-11s %-6s %-28s %5s %5s %10s %10s %7s %8s %9s\n",
			"RANK", "CONV", "KIND", "TIER", "TITLE", "TURNS", "CHILD", "UNCACHED", "GROSS", "CHR%", "QBU", "PEAK_CTX")
		for _, item := range rep.Items {
			cid := item.ConversationID
			if len(cid) > 8 {
				cid = cid[:8]
			}
			fmt.Printf("  #%-3d %-10s %-11s %-6s %-28s %5d %5d %10s %10s %6.1f%% %8.0f %9s\n",
				item.Rank,
				cid,
				truncateDisplay(item.Kind, 11),
				item.Tier,
				truncateDisplay(item.Title, 28),
				item.Metrics.TurnCount,
				item.ChildCount,
				obs.FormatTokensCompact(item.Metrics.UncachedTokens),
				obs.FormatTokensCompact(item.Metrics.GrossTokens),
				item.Metrics.CacheHitRatio*100.0,
				item.Metrics.QuotaUnits,
				obs.FormatTokensCompact(item.Metrics.PeakContextTokens))
		}

	case "inspect":
		var target string
		var flagArgs []string
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && target == "" {
				target = a
			} else {
				flagArgs = append(flagArgs, a)
			}
		}
		fs := flag.NewFlagSet("obs inspect", flag.ExitOnError)
		showTurns := fs.Bool("turns", true, "Show turn-by-turn breakdown")
		showChildren := fs.Bool("children", true, "Show child subagent DAG")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)

		if target == "" {
			fmt.Fprintln(os.Stderr, "usage: hq obs inspect <conv-id-or-task-slug> [--turns] [--children] [--json]")
			os.Exit(1)
		}
		rep, err := obs.BuildInspectReport(vaultDir, target, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq obs inspect failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(rep)
			return
		}

		cm := rep.Conversation
		fmt.Printf("Inspect: %s (%s)\n", cm.Title, cm.ConversationID)
		fmt.Printf("  Task Slug: %s | Kind: %s | Scope: %s | Tier: %s | Model: %s\n",
			cm.TaskSlug, cm.Kind, cm.Scope, cm.Tier, cm.Model)
		if cm.ParentConversationID != "" {
			fmt.Printf("  Parent Conversation: %s\n", cm.ParentConversationID)
		}
		fmt.Printf("  Current Context: %s | Peak Context: %s | Compactions: %d\n",
			obs.FormatTokensCompact(cm.CurrentContextTokens),
			obs.FormatTokensCompact(cm.Self.PeakContextTokens),
			cm.Self.CompactionCount)
		fmt.Printf("  Self Burn: Uncached %s (In: %s, Out: %s) | Gross %s | CHR %.1f%% | QBU %.0f | Latency %s\n",
			obs.FormatTokensCompact(cm.Self.UncachedTokens),
			obs.FormatTokensCompact(cm.Self.InputTokens),
			obs.FormatTokensCompact(cm.Self.OutputTokens),
			obs.FormatTokensCompact(cm.Self.GrossTokens),
			cm.Self.CacheHitRatio*100.0,
			cm.Self.QuotaUnits,
			obs.FormatDurationCompact(cm.Self.ActiveLatencyMs))
		if len(cm.ChildConversationIDs) > 0 {
			fmt.Printf("  Tree Burn: Uncached %s | Gross %s | CHR %.1f%% | QBU %.0f (%d direct child subagent(s))\n",
				obs.FormatTokensCompact(cm.Tree.UncachedTokens),
				obs.FormatTokensCompact(cm.Tree.GrossTokens),
				cm.Tree.CacheHitRatio*100.0,
				cm.Tree.QuotaUnits,
				len(cm.ChildConversationIDs))
		}

		if *showChildren && len(rep.Children) > 0 {
			fmt.Printf("\nChild Subagents (%d):\n", len(rep.Children))
			for _, ch := range rep.Children {
				cid := ch.ConversationID
				if len(cid) > 8 {
					cid = cid[:8]
				}
				fmt.Printf("  - [%s] %s (%s | %s): Uncached %s, Gross %s, CHR %.1f%%, Turns %d\n",
					cid,
					ch.Title,
					ch.Tier,
					ch.Model,
					obs.FormatTokensCompact(ch.Self.UncachedTokens),
					obs.FormatTokensCompact(ch.Self.GrossTokens),
					ch.Self.CacheHitRatio*100.0,
					ch.Self.TurnCount)
			}
		}

		if *showTurns && len(cm.Turns) > 0 {
			fmt.Printf("\nTurn-by-Turn Telemetry (%d turns):\n", len(cm.Turns))
			fmt.Printf("  %-6s %-20s %6s %10s %10s %10s %7s %9s %8s\n",
				"TURN", "STARTED_AT", "STEPS", "UNCACHED", "CACHE_RD", "GROSS", "CHR%", "PEAK_CTX", "LATENCY")
			for _, t := range cm.Turns {
				fmt.Printf("  T#%-4d %-20s %6d %10s %10s %10s %6.1f%% %9s %8s\n",
					t.TurnIndex,
					truncateDisplay(t.StartedAt, 20),
					t.StepCount,
					obs.FormatTokensCompact(t.UncachedTokens),
					obs.FormatTokensCompact(t.CacheReadTokens),
					obs.FormatTokensCompact(t.GrossTokens),
					t.CacheHitRatio*100.0,
					obs.FormatTokensCompact(t.PeakContextTokens),
					obs.FormatDurationCompact(t.ActiveLatencyMs))
			}
		}

		if len(rep.Anomalies) > 0 {
			fmt.Printf("\nAnomalies on Conversation (%d):\n", len(rep.Anomalies))
			for _, a := range rep.Anomalies {
				fmt.Printf("  - [%s][%s] %s\n", a.Severity, a.Code, a.Summary)
			}
		}

	case "anomalies":
		fs := flag.NewFlagSet("obs anomalies", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)

		idx, err := obs.LoadOrRefreshIndex(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq obs anomalies failed: %v\n", err)
			os.Exit(1)
		}
		budget := obs.LoadBudgetConfig(vaultDir)
		anomalies := obs.EvaluateAnomalies(vaultDir, idx, budget, now)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(anomalies)
			return
		}
		if len(anomalies) == 0 {
			fmt.Println("Zero token, prefix-cache, context, or budget anomalies detected.")
			return
		}
		fmt.Printf("Detected %d Token & Context Anomaly Alert(s):\n", len(anomalies))
		for _, a := range anomalies {
			fmt.Printf("  - [%s][%s] %s (%s)\n", a.Severity, a.Code, a.Kind, a.ID)
			fmt.Printf("      Summary:     %s\n", a.Summary)
			fmt.Printf("      Remediation: %s\n", a.Remediation)
		}

	case "budget":
		fs := flag.NewFlagSet("obs budget", flag.ExitOnError)
		setDailyUncached := fs.Int64("set-daily-uncached", 0, "Set daily uncached token budget (e.g. 15000000)")
		setDailyDeep := fs.Int64("set-daily-deep", 0, "Set daily deep-tier uncached token budget (e.g. 5000000)")
		setWarnCtx := fs.Int64("set-warn-context", 0, "Set Front Desk context warning threshold (default 120000)")
		setNudgeCtx := fs.Int64("set-nudge-context", 0, "Set Front Desk SessionStart /compact nudge threshold (default 160000)")
		setCritCtx := fs.Int64("set-crit-context", 0, "Set Front Desk critical context threshold (default 180000)")
		resetCircuit := fs.Bool("reset-circuit", false, "Clear daily token budget circuit breaker for kind:auto triggers")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)

		cfg := obs.LoadBudgetConfig(vaultDir)
		updated := false
		if *setDailyUncached > 0 {
			cfg.DailyUncachedBudget = *setDailyUncached
			updated = true
		}
		if *setDailyDeep > 0 {
			cfg.DailyDeepUncachedBudget = *setDailyDeep
			updated = true
		}
		if *setWarnCtx > 0 {
			cfg.WarnContextTokens = *setWarnCtx
			updated = true
		}
		if *setNudgeCtx > 0 {
			cfg.NudgeContextTokens = *setNudgeCtx
			updated = true
		}
		if *setCritCtx > 0 {
			cfg.CritContextTokens = *setCritCtx
			updated = true
		}
		if updated {
			if err := obs.SaveBudgetConfig(vaultDir, cfg); err != nil {
				fmt.Fprintf(os.Stderr, "hq obs budget save failed: %v\n", err)
				os.Exit(1)
			}
			obs.SyncDailyBudgetCircuitBreaker(vaultDir, now)
		}
		if *resetCircuit {
			if err := obs.ResetBudgetCircuitBreaker(vaultDir); err != nil {
				fmt.Fprintf(os.Stderr, "hq obs budget --reset-circuit failed: %v\n", err)
				os.Exit(1)
			}
		} else if !updated {
			obs.SyncDailyBudgetCircuitBreaker(vaultDir, now)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)

		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(cfg)
			return
		}
		fmt.Printf("HQ Token Budget & Context Guard Configuration (local/memory/obs/budget.json)\n")
		fmt.Printf("  Daily Uncached Budget:      %s tokens\n", obs.FormatTokensCompact(cfg.DailyUncachedBudget))
		fmt.Printf("  Daily Deep Uncached Budget: %s tokens\n", obs.FormatTokensCompact(cfg.DailyDeepUncachedBudget))
		fmt.Printf("  Front Desk Context Guards:  WARN=%s | NUDGE=%s | CRITICAL=%s\n",
			obs.FormatTokensCompact(cfg.WarnContextTokens),
			obs.FormatTokensCompact(cfg.NudgeContextTokens),
			obs.FormatTokensCompact(cfg.CritContextTokens))
		fmt.Printf("  Runaway Turn Guards (A3):   MaxSteps=%d | MaxUncached=%s | MaxOutput=%s\n",
			cfg.MaxTurnSteps,
			obs.FormatTokensCompact(cfg.MaxTurnUncachedTokens),
			obs.FormatTokensCompact(cfg.MaxTurnOutputTokens))

	case "reconcile":
		fs := flag.NewFlagSet("obs reconcile", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		repaired, err := obs.ReconcileSubagentAttribution(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq obs reconcile failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]int{"repairedTasks": repaired})
			return
		}
		fmt.Printf("Reconciled subagent DAG attribution against ground-truth transcripts (%d task(s) updated)\n", repaired)

	default:
		fmt.Fprintf(os.Stderr, "unknown obs subcommand: %s (expected tokens, top, inspect, anomalies, budget, reconcile)\n", sub)
		os.Exit(1)
	}
}

func truncateDisplay(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func printUsage() {
	fmt.Println(`Usage: hq <command> [args]

Commands:
  init [--demo]                       Initialize local/ private Vault (optionally seed [Sample] dataset)
  upgrade [--no-pull]                 Pull latest agy-hq commits, rebuild hq, and migrate Vault
  migrate [--quiet]                   Run non-destructive Vault schema & template migrations
  doctor [--json]                     Check Antigravity sandbox, CLI, and Vault health
  status [--quiet]                    Show pending approvals and active worktrees
  tasks [--active|--all|--archive|--kind=|--scope=|--search=]  Query tiered Task Board & cold archive
  tasks <stats|compact|pin|unpin>     Task Board telemetry, hot/cold compaction, and pinning
  goals <list|add|milestone|check-off|wait-on|mark-replied|due|complete>  Stateful multi-day goals & counterparty SLAs
  outbox <add|list|drain>             4-tier attention outbox & quiet-hours DND governance
  ideas <list|promote|snooze|dismiss> Triage non-interrupting proactive suggestions
  spaces <list|active|pin|hide|auto>  Manage schedule-aware Console Spaces
  obs <tokens|top|inspect|anomalies|budget|reconcile>  Token, prefix cache, latency & context observability
  memory <status|topics|add-fact|prune-fact|forget|entities|add-entity|lookup|compact|verify-transcript>  5-layer living memory engine
  rsi <mine|hypothesize|verify|propose|apply|ablate|status>  4-Stage Agent Recursive Self-Improvement engine
  ci <check-pii|check-harness|scrub-history|pre-commit|pre-push|install-hooks>  CI/CD PII & Harness verification
  heartbeat [--dry-run]               Run 0-token gatekeeper pre-check and inbox router once
  daemon [install|uninstall|status]   Run or manage the HQ Console background daemon service
  webui [--addr=127.0.0.1:4242]       Start interactive Web UI Console & Antigravity Sidecar
  hook <session-start|pre-tool|stop>  Antigravity hooks.json handlers
  propose --type=... --summary=... --cmd=...  Stage an outbound action for 1-tap approval
  approve <id> [--sha256=<prefix>]    Verify SHA-256 and deterministically execute proposal
  reject <id> [--reason=...]          Reject a pending proposal
  worktree add <repo> <task>          Checkout an isolated git worktree in local/worktrees/
  gmail <init|status|list|read|draft|triage>  Layer 1 Gmail primitives & sidecar precheck
  gchat <init|spaces|search|read|draft|triage> Layer 1 Google Chat primitives & sidecar precheck
  spawn --task=<slug> --prompt="..."  Spawn a dedicated Task Conversation in an isolated worktree
  subagent --role="..." --model=...   Spawn a nested child subagent under the current conversation
  complete <task-slug-or-conv-id>     Mark an active task as completed
  archive <task-slug-or-conv-id>      Archive a completed task conversation
  reopen <task-slug-or-conv-id>       Reopen an archived task conversation (recalls from cold archive if needed)
  adopt --conv=<id> --task=<slug>     Adopt an existing conversation (or demote Front Desk to a Task)
  claim-desk --conv=<id>              Claim the single active [HQ] Front Desk intake lease
  release-desk                        Release Front Desk lease so next new conversation claims it`)
}
