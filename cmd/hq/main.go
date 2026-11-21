package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/channels"
	"github.com/zjshen14/agy-hq/internal/ci"
	"github.com/zjshen14/agy-hq/internal/doctor"
	"github.com/zjshen14/agy-hq/internal/hooks"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/plugin"
	"github.com/zjshen14/agy-hq/internal/rsi"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/version"
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
	vaultDir, vaultSource, err := vault.ResolveVaultDirWithSource()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hq: resolve vault: %v\n", err)
		os.Exit(1)
	}
	isPreToolHook := os.Args[1] == "hook" && len(os.Args) >= 3 && (os.Args[2] == "pre-tool" || os.Args[2] == "pre-tool-use")
	if !isPreToolHook {
		kernel.BackfillFrontDeskProjectID(vaultDir)
	}
	if os.Args[1] != "hook" {
		_ = kernel.RecordLiveLSSession(vaultDir)
	}

	switch os.Args[1] {
	case "version", "--version", "-v":
		fs := flag.NewFlagSet("version", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output version metadata as JSON")
		_ = fs.Parse(os.Args[2:])
		if *asJSON {
			printJSON(map[string]interface{}{
				"version":            version.Version,
				"gitCommit":          version.EffectiveCommit(),
				"vaultSchemaVersion": vault.CurrentSchemaVersion,
				"vaultDir":           vaultDir,
				"vaultSource":        vaultSource,
			})
			return
		}
		fmt.Printf("hq v%s (commit %s, vault-schema v%d)\n", version.Version, version.EffectiveCommit(), vault.CurrentSchemaVersion)

	case "vault-path":
		fs := flag.NewFlagSet("vault-path", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output resolved Vault path and source as JSON")
		_ = fs.Parse(os.Args[2:])
		if *asJSON {
			printJSON(map[string]string{
				"vaultDir": vaultDir,
				"source":   vaultSource,
			})
			return
		}
		fmt.Println(vaultDir)

	case "plugin":
		runPluginCLI(rootDir, vaultDir, os.Args[2:])

	case "channel":
		runChannelCLI(rootDir, vaultDir, os.Args[2:])

	case "skill", "skills":
		runSkillCLI(rootDir, vaultDir, os.Args[2:])

	case "init":
		fs := flag.NewFlagSet("init", flag.ExitOnError)
		demo := fs.Bool("demo", false, "Seed synthetic [Sample] ACME 10-Q inbox filing, thesis, tasks, approval, and facts")
		withSample := fs.Bool("with-sample-data", false, "Alias for --demo: seed synthetic [Sample] dataset")
		globalVault := fs.Bool("global-vault", false, "Initialize and register the canonical global Vault (~/.gemini/antigravity/hq/vault)")
		attachPlugin := fs.Bool("attach-plugin", false, "Write .agents/plugins.json and .agents/skills.json in the current directory")
		startersFlag := fs.String("starters", "", "Comma-separated starter skills to install (or 'all')")
		allStarters := fs.Bool("all-starters", false, "Install all available starter skill templates into <vault>/skills/")
		_ = fs.Parse(os.Args[2:])
		if *globalVault {
			if gv, gErr := vault.DefaultGlobalVaultDir(); gErr == nil && gv != "" {
				vaultDir = gv
			}
		}
		templateDir := resolveTemplateDir(rootDir)
		created, err := vault.Init(vaultDir, templateDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq init failed: %v\n", err)
			os.Exit(1)
		}
		var starterSlugs []string
		if *allStarters || strings.EqualFold(strings.TrimSpace(*startersFlag), "all") {
			starterSlugs = append(starterSlugs, vault.StarterSkillSlugs...)
		} else if strings.TrimSpace(*startersFlag) != "" {
			for _, part := range strings.Split(*startersFlag, ",") {
				if s := strings.TrimSpace(part); s != "" {
					starterSlugs = append(starterSlugs, s)
				}
			}
		}
		for _, slug := range starterSlugs {
			if changed, dst, sErr := vault.AddStarterSkill(vaultDir, templateDir, slug, false); sErr != nil {
				fmt.Fprintf(os.Stderr, "hq init starter skill %q failed: %v\n", slug, sErr)
				os.Exit(1)
			} else if changed {
				created = append(created, dst+" (--starters)")
			}
		}
		if *globalVault {
			_ = vault.SaveGlobalConfig(vault.GlobalConfig{
				VaultDir:    vaultDir,
				PluginScope: plugin.ScopeProject,
				UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
			})
		}
		if *attachPlugin {
			if wd, wErr := os.Getwd(); wErr == nil && wd != "" {
				if mPath, aErr := plugin.AttachProjectPlugin(wd, ""); aErr == nil && mPath != "" {
					created = append(created, mPath+" (project-scoped plugin)")
				}
				if !vault.IsAgyHQRepo(wd) {
					if sPath, sErr := plugin.AttachProjectVaultSkills(wd, vaultDir); sErr == nil && sPath != "" {
						created = append(created, sPath+" (vault skills)")
					}
				}
			}
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
		if vault.IsAgyHQRepo(rootDir) {
			_, _ = ci.InstallGitHooks(rootDir)
		}
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
		templateDir := resolveTemplateDir(rootDir)
		rep, err := vault.Migrate(vaultDir, templateDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq migrate failed: %v\n", err)
			os.Exit(1)
		}
		var rsiMig *rsi.OverlayMigrationReport
		if vault.IsAgyHQRepo(rootDir) {
			_, _ = ci.InstallGitHooks(rootDir)
			if mr, mErr := rsi.MigrateOverlays(rootDir, vaultDir); mErr == nil {
				rsiMig = mr
			}
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
			if rsiMig != nil && rsiMig.ExtractedRules > 0 {
				fmt.Printf("  [rsi]  extracted %d legacy tracked rule(s) into Vault overlays (%s)\n", rsiMig.ExtractedRules, strings.Join(rsiMig.UpdatedTargets, ", "))
			}
		}

	case "upgrade":
		fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
		noPull := fs.Bool("no-pull", false, "Skip git pull on root repository (offline/test mode)")
		_ = fs.Parse(os.Args[2:])
		if _, sErr := os.Stat(filepath.Join(rootDir, "cmd", "hq")); sErr != nil {
			fmt.Fprintf(os.Stderr, "hq upgrade: %s is not an agy-hq source checkout (cd into your agy-hq repository or set HQ_ROOT_DIR)\n", rootDir)
			os.Exit(1)
		}

		// 1. Pre-upgrade snapshot of uncommitted private Vault changes
		if vault.SnapshotVault(vaultDir, "hq upgrade") {
			fmt.Println("==> [1/5] Committed pre-upgrade restore point in Vault .git")
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

		// 3. Recompile ./bin/hq from the updated Go source (embedding GitCommit via -ldflags)
		targetBin := filepath.Join(rootDir, "bin", "hq")
		if exe, err := os.Executable(); err == nil && vault.IsTempPath(exe) {
			targetBin = exe
		}
		fmt.Printf("==> [3/5] Rebuilding hq microkernel (%s)...\n", targetBin)
		_ = os.MkdirAll(filepath.Dir(targetBin), 0o755)
		buildArgs := []string{"build"}
		if out, cErr := vault.SanitizedGitCommand(rootDir, "rev-parse", "--short=12", "HEAD").Output(); cErr == nil {
			if sha := strings.TrimSpace(string(out)); sha != "" {
				buildArgs = append(buildArgs, "-ldflags", "-X github.com/zjshen14/agy-hq/internal/version.GitCommit="+sha)
			}
		}
		buildArgs = append(buildArgs, "-o", targetBin, "./cmd/hq")
		buildCmd := exec.Command("go", buildArgs...)
		buildCmd.Dir = rootDir
		if out, err := buildCmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "hq upgrade: go build failed (%v):\n%s\n", err, string(out))
			os.Exit(1)
		}
		if !vault.IsTempPath(targetBin) {
			switch runtime.GOOS {
			case "linux":
				if os.Getenv("HQ_SKIP_SYSTEMCTL") != "1" {
					if ctl, lErr := exec.LookPath("systemctl"); lErr == nil {
						if out, rErr := exec.Command(ctl, "--user", "try-restart", "agy-hq.service").CombinedOutput(); rErr != nil {
							fmt.Printf("    [WARN] systemctl --user try-restart agy-hq.service failed: %v (%s)\n", rErr, strings.TrimSpace(string(out)))
						} else if activeOut, aErr := exec.Command(ctl, "--user", "is-active", "agy-hq.service").Output(); aErr == nil && strings.TrimSpace(string(activeOut)) == "active" {
							fmt.Println("    Restarted agy-hq.service (systemd user daemon)")
						}
					}
				}
			case "darwin":
				if os.Getenv("HQ_SKIP_LAUNCHCTL") != "1" {
					if ctl, lErr := exec.LookPath("launchctl"); lErr == nil {
						target := fmt.Sprintf("gui/%d/com.antigravity.agy-hq", os.Getuid())
						if out, rErr := exec.Command(ctl, "kickstart", "-k", target).CombinedOutput(); rErr == nil {
							fmt.Println("    Restarted com.antigravity.agy-hq (launchd user daemon)")
						} else if strings.TrimSpace(string(out)) != "" && !strings.Contains(strings.ToLower(string(out)), "could not find service") {
							fmt.Printf("    [WARN] launchctl kickstart failed: %v (%s)\n", rErr, strings.TrimSpace(string(out)))
						}
					}
				}
			}
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
		if !vault.IsTempPath(targetBin) {
			if st, sErr := plugin.Status(vaultDir, rootDir); sErr == nil && st != nil && (st.GlobalPluginInstalled || st.SharedPluginInstalled || st.GlobalBinaryInstalled) {
				syncArgs := []string{"plugin", "install", "--vault-dir=" + vaultDir}
				if cfg, cErr := vault.LoadGlobalConfig(); cErr == nil && cfg != nil {
					if cfg.PluginScope == plugin.ScopeGlobal && st.GlobalPluginInstalled {
						syncArgs = append(syncArgs, "--global")
					} else if cfg.ConsoleSidecarInstalled {
						syncArgs = append(syncArgs, "--console-sidecar")
					}
				}
				syncCmd := exec.Command(targetBin, syncArgs...)
				syncCmd.Dir = rootDir
				syncCmd.Env = append(os.Environ(), "HQ_ROOT_DIR="+rootDir, "HQ_VAULT_DIR="+vaultDir)
				if out, syncErr := syncCmd.CombinedOutput(); syncErr == nil {
					for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
						if line != "" {
							fmt.Printf("    %s\n", line)
						}
					}
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
			printJSON(results)
		} else {
			for _, r := range results {
				fmt.Printf("[%s] %-30s %s\n", r.Status, r.Name, r.Detail)
			}
		}

	case "status":
		denyIfCallerTainted(vaultDir, "status")
		fs := flag.NewFlagSet("status", flag.ExitOnError)
		quiet := fs.Bool("quiet", false, "Suppress normal status output")
		_ = fs.Parse(os.Args[2:])
		_ = webui.SyncMarkdownConsole(vaultDir)
		pending, _ := approvals.List(vaultDir)
		wts, _ := worktree.List(vaultDir)
		bridges, _ := channels.ListBridgeSummaries(vaultDir)
		if !*quiet {
			fmt.Printf("Vault:            %s\n", vaultDir)
			fmt.Printf("Pending Actions:  %d\n", len(pending))
			fmt.Printf("Active Worktrees: %d\n", len(wts))
			if wsSummary, ok := kernel.SummarizeWorkspaces(vaultDir, len(wts)); ok {
				fmt.Println(wsSummary.StatusLine())
			}
			if len(bridges) > 0 {
				fmt.Printf("Channel Bridges:  %d\n", len(bridges))
				for _, b := range bridges {
					dest := b.DestSHA256Short
					if dest == "" {
						dest = "-"
					}
					fmt.Printf("  - [%s] enabled=%v health=%s dest=%s queued=%d\n", b.BridgeID, b.Enabled, b.HealthStatus, dest, b.QueuedCount)
				}
			}
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
			webui.SyncMarkdownConsoleForHook(vaultDir, 4*time.Second)
		case "pre-tool", "pre-tool-use":
			needsSync, err := hooks.HandlePreToolUseWithSync(os.Stdin, os.Stdout, vaultDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq hook pre-tool: %v\n", err)
				os.Exit(1)
			}
			if needsSync {
				webui.SyncMarkdownConsoleForHook(vaultDir, 4*time.Second)
			}
		case "stop":
			if err := hooks.HandleStop(os.Stdin, os.Stdout, vaultDir); err != nil {
				fmt.Fprintf(os.Stderr, "hq hook stop: %v\n", err)
				os.Exit(1)
			}
			webui.SyncMarkdownConsoleForHook(vaultDir, 4*time.Second)
		default:
			fmt.Fprintf(os.Stderr, "unknown hook subcommand: %s\n", os.Args[2])
			os.Exit(1)
		}

	case "propose":
		runProposeCLI(rootDir, vaultDir, os.Args[2:])

	case "approve":
		runApproveCLI(vaultDir, os.Args[2:])

	case "reject":
		runRejectCLI(vaultDir, os.Args[2:])

	case "worktree":
		runWorktreeCLI(vaultDir, os.Args[2:])

	case "heartbeat":
		runHeartbeatCLI(vaultDir, os.Args[2:])

	case "claim-desk":
		runClaimDeskCLI(vaultDir, os.Args[2:])

	case "release-desk":
		runReleaseDeskCLI(vaultDir)

	case "adopt":
		runAdoptCLI(vaultDir, os.Args[2:])

	case "complete":
		runCompleteCLI(vaultDir, os.Args[2:])

	case "archive":
		runArchiveCLI(vaultDir, os.Args[2:])

	case "reopen":
		runReopenTaskCLI(vaultDir, "hq reopen", os.Args[2:])

	case "workspace":
		if code := runWorkspaceCLI(rootDir, vaultDir, os.Args[2:], os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}

	case "spawn":
		runSpawnCLI(rootDir, vaultDir, os.Args[2:])

	case "subagent":
		runSubagentCLI(rootDir, vaultDir, os.Args[2:])

	case "gmail":
		runGmailCLI(rootDir, vaultDir, os.Args[2:])

	case "gchat":
		runGChatCLI(rootDir, vaultDir, os.Args[2:])

	case "daemon", "webui":
		runDaemonCLI(rootDir, vaultDir, os.Args[1], os.Args[2:])

	case "memory":
		runMemoryCLI(rootDir, vaultDir, os.Args[2:])

	case "tasks":
		runTasksCLI(vaultDir, os.Args[2:])

	case "outbox":
		runOutboxCLI(vaultDir, os.Args[2:])

	case "events":
		runEventsCLI(vaultDir, os.Args[2:])

	case "timers":
		runTimersCLI(vaultDir, os.Args[2:])

	case "progress":
		runProgressCLI(vaultDir, os.Args[2:])

	case "exec-sandboxed":
		runExecSandboxedCLI(vaultDir, os.Args[2:])

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

	case "triggers":
		runTriggersCLI(vaultDir, os.Args[2:])

	case "dispatch":
		runDispatchCLI(vaultDir, os.Args[2:])

	case "extension":
		runExtensionCLI(rootDir, vaultDir, os.Args[2:])

	case "connector":
		runConnectorCLI(vaultDir, os.Args[2:])

	case "connector-driver":
		runConnectorDriverCLI(vaultDir, os.Args[2:])

	case "watch":
		runWatchCLI(vaultDir, os.Args[2:])

	case "offer":
		runOfferCLI(vaultDir, os.Args[2:])

	case "brief":
		runBriefCLI(vaultDir, os.Args[2:])

	case "sidecar":
		runSidecarCLI(vaultDir, os.Args[2:])

	case "delegate":
		runDelegateCLI(vaultDir, os.Args[2:])

	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Usage: hq <command> [args]

Commands:
  version [--json]                    Show hq binary SemVer, git commit, and Vault schema version
  vault-path [--json]                 Print the resolved private Vault path and resolution source
  plugin <install|uninstall|status|validate>  Manage project-scoped (default) or --global plugin & hq binary
  skill <list|add|remove>             Manage opt-in Vault user-space skills (<vault>/skills/)
  init [--demo] [--global-vault] [--attach-plugin] [--starters=...|all-starters]  Initialize private Vault
  upgrade [--no-pull]                 Pull latest agy-hq commits, rebuild hq, sync plugin & migrate Vault
  migrate [--quiet]                   Run non-destructive Vault schema & template migrations
  doctor [--json]                     Check Antigravity sandbox, CLI, plugin sync, and Vault health
  status [--quiet]                    Show pending approvals and active worktrees
  tasks [--active|--all|--archive|--kind=|--scope=|--search=]  Query tiered Task Board & cold archive
  tasks <stats|compact|sync-sidebar|pin|unpin>  Task Board telemetry, compaction, sidebar archive sync, and pinning
  goals <list|add|milestone|check-off|wait-on|mark-replied|due|complete>  Stateful multi-day goals & counterparty SLAs
  outbox <add|list|drain|eval>        4-tier attention outbox, quiet-hours DND & signal-to-noise eval
  events <enqueue|list|inspect|retry|drop|dispatch|token>  Durable crash-safe Event Inbox & webhook token
  timers <set|list|cancel>            Durable one-shot Follow-Up & recurring Schedule timers
  triggers <list|pause|resume|fire>   Inspect, pause, resume, or fire declarative schedules
  dispatch resolve <skill>            Resolve active core, Vault skill, or RSI overlay rules
  extension <list|lint|enable|disable|refresh|trust|doctor>  Manage companion extension packs & trust hashes
  channel <list|status|test|pause|resume|audit|configure>  Manage Front Desk channel bridges & SHA-256 audit log
  delegate <list|show|validate|configure|test|ack-nudge|audit>  Manage declarative scoped peer delegates (RFC-0026)
  connector <list|lint|migrate|doctor|test>  Manage modular JSON-RPC/CLI connectors & fixtures
  watch <precheck|read|query|ack>     Proactive watcher precheck, tainted read/query, and event ack
  offer stage --connector=...         Stage taint-safe outbound offers with participant_lock & /dev/fd/3
  brief <collect|commit|format-quarantined>  Lease-based Morning Brief packet assembly & commit
  sidecar console                     Run native Go stdio JSON-RPC console sidecar
  progress <save|show|list|archive>   Structured cross-run & pre-compaction task checkpoints
  exec-sandboxed [--net-isolated] -- <cmd>  Execute subprocess inside OS credential sandbox
  ideas <list|promote|snooze|dismiss> Triage non-interrupting proactive suggestions
  spaces <list|active|pin|hide|auto>  Manage schedule-aware Console Spaces
  obs <tokens|top|inspect|anomalies|budget|reconcile>  Token, prefix cache, latency & context observability
  memory <status|topics|add-fact|prune-fact|forget|entities|add-entity|lookup|compact|verify-transcript|commit>  5-layer living memory engine
  rsi <mine|hypothesize|verify|propose|apply|ablate|export|migrate-overlays|status>  Two-Tier 4-Stage Agent Recursive Self-Improvement engine
  ci <check-pii|check-harness|scrub-history|pre-commit|pre-push|install-hooks>  CI/CD PII & Harness verification
  heartbeat [--dry-run]               Run 0-token gatekeeper pre-check and inbox router once
  daemon [install|uninstall|status]   Run or manage the HQ Console background daemon service
  webui [--addr=127.0.0.1:4242]       Start interactive Web UI Console & Antigravity Sidecar
  hook <session-start|pre-tool|stop>  Antigravity hooks.json handlers
  propose --type=... --summary=... --cmd=...  Stage an outbound action for 1-tap approval
  approve <id> [--sha256=<prefix>]    Verify SHA-256 and deterministically execute proposal
  reject <id> [--reason=...]          Reject a pending proposal
  worktree <add|list|attach|remove|prune>  Manage isolated git worktrees & prune merged orphans in <vault>/worktrees/
  workspace <providers|list|create|adopt|status|handoff|unbind|remove|own|prune>  Manage pluggable provider workspaces (RFC-0025)
  gmail <init|status|list|read|draft|forward|triage>  Layer 1 Gmail primitives & sidecar precheck
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
