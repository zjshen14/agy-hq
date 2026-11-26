package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zjshen14/agy-hq/internal/channels"
	"github.com/zjshen14/agy-hq/internal/ci"
	"github.com/zjshen14/agy-hq/internal/delegates"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/githooks"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/plugin"
	"github.com/zjshen14/agy-hq/internal/pulse"
	"github.com/zjshen14/agy-hq/internal/timers"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/webui"
)

func runDaemonCLI(rootDir, vaultDir, cmdName string, args []string) {
	if cmdName == "daemon" && len(args) >= 1 && !strings.HasPrefix(args[0], "-") {
		sub, rest := args[0], args[1:]
		switch sub {
		case "install":
			fs := flag.NewFlagSet("daemon install", flag.ExitOnError)
			_ = fs.Parse(rest)
			msg, err := kernel.InstallDaemon(rootDir, vaultDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq daemon install failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(msg)
			return
		case "uninstall":
			fs := flag.NewFlagSet("daemon uninstall", flag.ExitOnError)
			_ = fs.Parse(rest)
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
			_ = fs.Parse(rest)
			info := kernel.GetDaemonInfo(vaultDir)
			if *asJSON {
				printJSON(info)
			} else {
				fmt.Print(kernel.FormatDaemonStatus(info))
			}
			return
		case "start", "run":
			// continue to daemon server startup below
		default:
			fmt.Fprintf(os.Stderr, "unknown daemon subcommand: %s (expected install, uninstall, status, or run)\n", sub)
			os.Exit(1)
		}
	}

	fs := flag.NewFlagSet(cmdName, flag.ExitOnError)
	defaultAddr := "127.0.0.1:4242"
	if p := os.Getenv("ANTIGRAVITY_SIDECAR_WEB_PORT"); p != "" {
		defaultAddr = "127.0.0.1:" + p
	} else if p := os.Getenv("PORT"); p != "" {
		defaultAddr = "127.0.0.1:" + p
	}
	addr := fs.String("addr", defaultAddr, "Listen address for HQ Console HTTP server (loopback only by default)")
	allowRemote := fs.Bool("allow-remote", false, "Explicitly permit binding HQ Console HTTP server to a non-loopback interface")
	dryRun := fs.Bool("dry-run", false, "Simulate agentapi spawns in background watcher")
	daemonArgs := args
	if len(daemonArgs) > 0 && (daemonArgs[0] == "start" || daemonArgs[0] == "run") {
		daemonArgs = daemonArgs[1:]
	}
	_ = fs.Parse(daemonArgs)

	if !*allowRemote && !webui.IsLoopbackAddr(*addr) {
		fmt.Fprintf(os.Stderr, "hq daemon security error: refusing to bind HTTP console to non-loopback address %q (use 127.0.0.1:<port> or pass --allow-remote)\n", *addr)
		os.Exit(1)
	}
	if strings.TrimSpace(os.Getenv("GOMEMLIMIT")) == "" {
		debug.SetMemoryLimit(256 << 20)
	}

	if err := kernel.RecordDaemonStart(vaultDir, *addr); err != nil {
		fmt.Fprintf(os.Stderr, "hq daemon error: %v\n", err)
		os.Exit(1)
	}
	defer kernel.RecordDaemonStop(vaultDir)
	_, _ = events.EnsureDaemonToken(vaultDir)
	wakeCh := events.SubscribeWake()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		_ = kernel.RecordDaemonStop(vaultDir)
		os.Exit(0)
	}()

	// Background event-driven wake channel, 15s fast timer/inbox/outbox tick, 30s gatekeeper tick, & 30m full heartbeat loop
	go func() {
		var wsMaintRunning atomic.Bool
		triggerWorkspaceMaintenance := func(tickNow time.Time) {
			if *dryRun || !wsMaintRunning.CompareAndSwap(false, true) {
				return
			}
			go func(runAt time.Time) {
				defer wsMaintRunning.Store(false)
				ctx, cancel := context.WithTimeout(context.Background(), kernel.DefaultWorkspaceMaintenanceBudget)
				defer cancel()
				if wsRep, wsErr := kernel.RunWorkspaceMaintenanceContext(ctx, vaultDir, runAt); wsErr == nil && wsRep.Changed() {
					_ = webui.SyncMarkdownConsole(vaultDir)
				}
			}(tickNow)
		}
		_, _ = kernel.RunHeartbeatWithOptions(vaultDir, time.Now(), *dryRun, kernel.HeartbeatOptions{SkipWorkspaceMaintenance: true})
		triggerWorkspaceMaintenance(time.Now())
		_, _ = channels.SyncAndFlush(context.Background(), vaultDir, time.Now(), *dryRun)
		_ = webui.SyncMarkdownConsole(vaultDir)
		_, _, lastFDSize, lastFDMod := obs.FrontDeskTranscriptStamp(vaultDir)
		fastTicker := time.NewTicker(15 * time.Second)
		cronTicker := time.NewTicker(30 * time.Second)
		pulseTicker := time.NewTicker(15 * time.Minute)
		hbTicker := time.NewTicker(30 * time.Minute)
		defer fastTicker.Stop()
		defer cronTicker.Stop()
		defer pulseTicker.Stop()
		defer hbTicker.Stop()
		for {
			select {
			case <-wakeCh:
				now := time.Now()
				acts, dErr := kernel.DispatchPendingEvents(vaultDir, now, *dryRun)
				if dErr != nil {
					fmt.Fprintf(os.Stderr, "hq daemon wake dispatch error: %v\n", dErr)
				}
				outDelivered, _ := kernel.DispatchOutboxToFrontDesk(vaultDir, now, *dryRun)
				chRep, _ := channels.SyncAndFlush(context.Background(), vaultDir, now, *dryRun)
				if kernel.HasMaterialDispatchActions(acts) || outDelivered > 0 || chRep.BindsSent > 0 || chRep.NotificationsSent > 0 {
					_ = webui.SyncMarkdownConsole(vaultDir)
				}
			case <-fastTicker.C:
				now := time.Now()
				fired, _ := timers.EvaluateDue(vaultDir, now)
				_ = kernel.EnqueueInboxFiles(vaultDir, now)
				dlgRep, _ := delegates.PollDue(context.Background(), vaultDir, now, *dryRun)
				acts, dErr := kernel.DispatchPendingEvents(vaultDir, now, *dryRun)
				if dErr != nil {
					fmt.Fprintf(os.Stderr, "hq daemon fast-tick dispatch error: %v\n", dErr)
				}
				outDelivered, _ := kernel.DispatchOutboxToFrontDesk(vaultDir, now, *dryRun)
				chRep, _ := channels.SyncAndFlush(context.Background(), vaultDir, now, *dryRun)
				var syncedSidebar int
				if !*dryRun {
					if rep, sErr := kernel.DrainSidebarSyncQueue(vaultDir, 2*time.Second); sErr == nil && rep != nil {
						syncedSidebar = rep.ArchivedCount + rep.UnarchivedCount
					}
				}
				_, _, curFDSize, curFDMod := obs.FrontDeskTranscriptStamp(vaultDir)
				fdTranscriptChanged := curFDSize != lastFDSize || curFDMod != lastFDMod
				if fdTranscriptChanged {
					lastFDSize, lastFDMod = curFDSize, curFDMod
				}
				if len(fired) > 0 || dlgRep.ItemsProcessed > 0 || kernel.HasMaterialDispatchActions(acts) || outDelivered > 0 || chRep.BindsSent > 0 || chRep.NotificationsSent > 0 || syncedSidebar > 0 || fdTranscriptChanged {
					_ = webui.SyncMarkdownConsole(vaultDir)
				}
			case <-cronTicker.C:
				now := time.Now()
				gkActs, _, _ := kernel.EvaluateDaemonGatekeepers(vaultDir, now, false, *dryRun)
				snapsPublished, _ := delegates.SyncDueSnapshots(context.Background(), vaultDir, now, *dryRun)
				acts, _ := kernel.DispatchPendingEvents(vaultDir, now, *dryRun)
				// RFC-0025: non-blocking single-flight probe drain + cadence-gated status refresh (no-op without a ledger).
				triggerWorkspaceMaintenance(now)
				if len(gkActs) > 0 || snapsPublished > 0 || kernel.HasMaterialDispatchActions(acts) {
					_ = webui.SyncMarkdownConsole(vaultDir)
				}
			case <-pulseTicker.C:
				now := time.Now()
				if pRep, pErr := pulse.RunPrecheck(context.Background(), vaultDir, pulse.PrecheckOptions{Now: now, DryRun: *dryRun}); pErr == nil && pRep.EmittedCount > 0 {
					_, _ = kernel.DispatchOutboxToFrontDesk(vaultDir, now, *dryRun)
					_, _ = channels.SyncAndFlush(context.Background(), vaultDir, now, *dryRun)
					_ = webui.SyncMarkdownConsole(vaultDir)
				}
			case <-hbTicker.C:
				now := time.Now()
				_, _ = kernel.RunHeartbeatWithOptions(vaultDir, now, *dryRun, kernel.HeartbeatOptions{SkipWorkspaceMaintenance: true})
				triggerWorkspaceMaintenance(now)
				_ = channels.CompactAuditLog(context.Background(), vaultDir)
				_ = webui.SyncMarkdownConsole(vaultDir)
			}
		}
	}()

	handler := webui.NewHandler(vaultDir)
	newBoundedHTTPServer := func(listen string) *http.Server {
		return &http.Server{
			Addr:              listen,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
	}
	if *addr != "127.0.0.1:4242" && !strings.HasSuffix(*addr, ":4242") {
		go func() {
			_ = newBoundedHTTPServer("127.0.0.1:4242").ListenAndServe()
		}()
	}

	fmt.Printf("Starting agy-hq Console & Daemon on http://%s (Vault: %s)\n", *addr, vaultDir)
	if err := newBoundedHTTPServer(*addr).ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "hq daemon server error: %v\n", err)
		os.Exit(1)
	}

}

func runCICLI(rootDir string, args []string) {
	sub, args := splitSubcommand(args, "check-all")

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
			printJSON(violations)
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
			printJSON(rep)
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
			printJSON(results)
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

	case "denylist-add":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq ci denylist-add <term> [<term>...]")
			os.Exit(1)
		}
		vaultDir, _, vErr := vault.ResolveVaultDirWithSource()
		if vErr != nil || strings.TrimSpace(vaultDir) == "" {
			vaultDir = filepath.Join(rootDir, "local")
		}
		added, err := ci.AddPIIDenylistTerms(vaultDir, args)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq ci denylist-add failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("[OK] Added %d term(s) to private PII denylist\n", added)

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
		updated, err := githooks.Install(rootDir)
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
	sub, args := splitSubcommand(args, "tokens")
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
			printJSON(rep)
			return
		}

		fmt.Printf("HQ Token & Context Observability (Window: %s | GroupBy: %s)\n", rep.Since, rep.GroupBy)
		fmt.Printf("════════════════════════════════════════════════════════════════════════════════════════\n")
		fmt.Printf("  Uncached Burn (In+Out): %s  (Input: %s | Output: %s | Est: ~$%.2f | Cache Saved: ~$%.2f)\n",
			obs.FormatTokensCompact(rep.Totals.UncachedTokens),
			obs.FormatTokensCompact(rep.Totals.InputTokens),
			obs.FormatTokensCompact(rep.Totals.OutputTokens),
			rep.Totals.EstCostUSD,
			rep.Totals.CacheSavedUSD)
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
			fdID := shortPrefix(rep.FrontDeskConvID, 8)
			fdCtxStr := obs.FormatTokensCompact(rep.FrontDeskContext)
			if rep.FrontDeskCompactedPending {
				fdCtxStr = "compacted (pending next turn)"
			}
			fmt.Printf("  [HQ] Front Desk (%s): Context %s / %s (Peak: %s | Compactions: %d | CHR: %.1f%%)\n",
				fdID,
				fdCtxStr,
				obs.FormatTokensCompact(rep.Budget.EffectiveContextCeiling()),
				obs.FormatTokensCompact(rep.FrontDesk.PeakContextTokens),
				rep.FrontDesk.CompactionCount,
				rep.FrontDesk.CacheHitRatio*100.0)
		}

		fmt.Printf("\nBreakdown by %s:\n", strings.ToUpper(rep.GroupBy))
		fmt.Printf("  %-22s %6s %6s %10s %9s %9s %10s %10s %7s %9s %8s\n",
			"KEY", "CONVS", "TURNS", "UNCACHED", "AVG_UNC", "P50_UNC", "CACHE_RD", "GROSS", "CHR%", "EST_USD", "LATENCY")
		for _, b := range rep.Buckets {
			fmt.Printf("  %-22s %6d %6d %10s %9s %9s %10s %10s %6.1f%% %9s %8s\n",
				truncateDisplay(b.Key, 22),
				b.ConvCount,
				b.Rollup.TurnCount,
				obs.FormatTokensCompact(b.Rollup.UncachedTokens),
				obs.FormatTokensCompact(b.AvgUncachedPerRun),
				obs.FormatTokensCompact(b.P50UncachedPerRun),
				obs.FormatTokensCompact(b.Rollup.CacheReadTokens),
				obs.FormatTokensCompact(b.Rollup.GrossTokens),
				b.Rollup.CacheHitRatio*100.0,
				fmt.Sprintf("~$%.2f", b.Rollup.EstCostUSD),
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
		sortBy := fs.String("by", "uncached", "Sort metric (uncached, cost, gross, context, latency)")
		kind := fs.String("kind", "all", "Filter by kind (all, task, subagent, nested_subagent, front_desk, auto)")
		treeMode := fs.Bool("tree", false, "Rank by disjoint DAG root tree rollup (Self + Descendant Subagents)")
		allNodes := fs.Bool("all-nodes", false, "Include intermediate child nodes alongside roots when --tree is set")
		turnsMode := fs.Bool("turns", false, "Rank individual expensive turns across all transcripts")
		limit := fs.Int("limit", 10, "Maximum rows to display")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)

		rep, err := obs.BuildTopReportWithOptions(vaultDir, *sortBy, *kind, *treeMode, *turnsMode, *allNodes, *limit, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq obs top failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(rep)
			return
		}

		if *turnsMode {
			fmt.Printf("Top %d Individual Turns (Sorted by: %s | Kind: %s)\n", len(rep.TurnItems), rep.SortBy, rep.KindFilter)
			fmt.Printf("  %-4s %-10s %-6s %-26s %6s %10s %10s %7s %9s %8s\n",
				"RANK", "CONV", "TURN", "TITLE", "STEPS", "UNCACHED", "GROSS", "CHR%", "PEAK_CTX", "LATENCY")
			for _, item := range rep.TurnItems {
				fmt.Printf("  #%-3d %-10s T#%-4d %-26s %6d %10s %10s %6.1f%% %9s %8s\n",
					item.Rank,
					shortPrefix(item.ConversationID, 8),
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
			if *allNodes {
				modeLabel = "Tree (All Nodes)"
			} else {
				modeLabel = "Tree (Disjoint Roots)"
			}
		}
		fmt.Printf("Top %d Conversations — %s (Sorted by: %s | Kind: %s)\n", len(rep.Items), modeLabel, rep.SortBy, rep.KindFilter)
		fmt.Printf("  %-4s %-10s %-11s %-6s %-28s %5s %5s %10s %10s %7s %9s %9s\n",
			"RANK", "CONV", "KIND", "TIER", "TITLE", "TURNS", "SUBS", "UNCACHED", "GROSS", "CHR%", "EST_USD", "PEAK_CTX")
		for _, item := range rep.Items {
			subCount := item.ChildCount
			if *treeMode && item.DescendantCount > subCount {
				subCount = item.DescendantCount
			}
			fmt.Printf("  #%-3d %-10s %-11s %-6s %-28s %5d %5d %10s %10s %6.1f%% %9s %9s\n",
				item.Rank,
				shortPrefix(item.ConversationID, 8),
				truncateDisplay(item.Kind, 11),
				item.Tier,
				truncateDisplay(item.Title, 28),
				item.Metrics.TurnCount,
				subCount,
				obs.FormatTokensCompact(item.Metrics.UncachedTokens),
				obs.FormatTokensCompact(item.Metrics.GrossTokens),
				item.Metrics.CacheHitRatio*100.0,
				fmt.Sprintf("~$%.2f", item.Metrics.EstCostUSD),
				obs.FormatTokensCompact(item.Metrics.PeakContextTokens))
		}

	case "inspect":
		target, flagArgs := splitFirstPositionalAndFlags(args)
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
			printJSON(rep)
			return
		}

		cm := rep.Conversation
		fmt.Printf("Inspect: %s (%s)\n", cm.Title, cm.ConversationID)
		fmt.Printf("  Task Slug: %s | Kind: %s | Scope: %s | Tier: %s | Model: %s\n",
			cm.TaskSlug, cm.Kind, cm.Scope, cm.Tier, cm.Model)
		if cm.ParentConversationID != "" {
			fmt.Printf("  Parent Conversation: %s\n", cm.ParentConversationID)
		}
		curCtxStr := obs.FormatTokensCompact(cm.CurrentContextTokens)
		if cm.CompactedPendingNextTurn {
			curCtxStr = "compacted (pending next turn)"
		} else if cm.PostCompactContextTokens > 0 {
			curCtxStr = fmt.Sprintf("%s (post-compact baseline: %s)", curCtxStr, obs.FormatTokensCompact(cm.PostCompactContextTokens))
		}
		fmt.Printf("  Current Context: %s | Peak Context: %s | Compactions: %d\n",
			curCtxStr,
			obs.FormatTokensCompact(cm.Self.PeakContextTokens),
			cm.Self.CompactionCount)
		fmt.Printf("  Self Burn: Uncached %s (In: %s, Out: %s) | Gross %s | CHR %.1f%% | Est ~$%.2f | Latency %s\n",
			obs.FormatTokensCompact(cm.Self.UncachedTokens),
			obs.FormatTokensCompact(cm.Self.InputTokens),
			obs.FormatTokensCompact(cm.Self.OutputTokens),
			obs.FormatTokensCompact(cm.Self.GrossTokens),
			cm.Self.CacheHitRatio*100.0,
			cm.Self.EstCostUSD,
			obs.FormatDurationCompact(cm.Self.ActiveLatencyMs))
		if len(cm.ChildConversationIDs) > 0 {
			fmt.Printf("  Tree Burn: Uncached %s | Gross %s | CHR %.1f%% | Est ~$%.2f (%d direct child / %d descendant subagent(s))\n",
				obs.FormatTokensCompact(cm.Tree.UncachedTokens),
				obs.FormatTokensCompact(cm.Tree.GrossTokens),
				cm.Tree.CacheHitRatio*100.0,
				cm.Tree.EstCostUSD,
				len(cm.ChildConversationIDs),
				cm.DescendantCount)
		}

		if *showChildren && len(rep.Children) > 0 {
			fmt.Printf("\nChild Subagents (%d):\n", len(rep.Children))
			for _, ch := range rep.Children {
				fmt.Printf("  - [%s] %s (%s | %s): Uncached %s, Gross %s, CHR %.1f%%, Est ~$%.2f, Turns %d\n",
					shortPrefix(ch.ConversationID, 8),
					ch.Title,
					ch.Tier,
					ch.Model,
					obs.FormatTokensCompact(ch.Self.UncachedTokens),
					obs.FormatTokensCompact(ch.Self.GrossTokens),
					ch.Self.CacheHitRatio*100.0,
					ch.Self.EstCostUSD,
					ch.Self.TurnCount)
			}
		}

		if *showTurns && len(cm.Turns) > 0 {
			fmt.Printf("\nTurn-by-Turn Telemetry (%d turns):\n", len(cm.Turns))
			fmt.Printf("  %-6s %-20s %-18s %5s %9s %9s %9s %7s %8s %8s\n",
				"TURN", "ENDED_AT", "MODEL", "PLN", "UNCACHED", "CACHE_RD", "GROSS", "CHR%", "EST_USD", "LATENCY")
			for _, t := range cm.Turns {
				tsCol := t.EndedAt
				if tsCol == "" {
					tsCol = t.StartedAt
				}
				mCol := t.Model
				if mCol == "" {
					mCol = cm.Model
				}
				fmt.Printf("  T#%-4d %-20s %-18s %5d %9s %9s %9s %6.1f%% %8s %8s\n",
					t.TurnIndex,
					truncateDisplay(tsCol, 20),
					truncateDisplay(mCol, 18),
					t.PlannerSteps,
					obs.FormatTokensCompact(t.UncachedTokens),
					obs.FormatTokensCompact(t.CacheReadTokens),
					obs.FormatTokensCompact(t.GrossTokens),
					t.CacheHitRatio*100.0,
					fmt.Sprintf("~$%.2f", t.EstCostUSD),
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
		since := fs.String("since", "24h", "Time window for turn/subagent anomalies (24h, 7d, 30d, all)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)

		idx, err := obs.LoadOrRefreshIndex(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq obs anomalies failed: %v\n", err)
			os.Exit(1)
		}
		budget := obs.LoadBudgetConfig(vaultDir)
		cutoff := obs.ParseSinceCutoff(*since, now)
		anomalies := obs.EvaluateAnomaliesSince(vaultDir, idx, budget, now, cutoff)
		if *asJSON {
			printJSON(anomalies)
			return
		}
		if len(anomalies) == 0 {
			fmt.Printf("Zero token, prefix-cache, context, or budget anomalies detected (window: %s).\n", *since)
			return
		}
		fmt.Printf("Detected %d Token & Context Anomaly Alert(s) (Window: %s):\n", len(anomalies), *since)
		for _, a := range anomalies {
			fmt.Printf("  - [%s][%s] %s (%s)\n", a.Severity, a.Code, a.Kind, a.ID)
			fmt.Printf("      Summary:     %s\n", a.Summary)
			fmt.Printf("      Remediation: %s\n", a.Remediation)
		}

	case "budget":
		fs := flag.NewFlagSet("obs budget", flag.ExitOnError)
		setDailyUncached := fs.Int64("set-daily-uncached", 0, "Set daily uncached token budget (e.g. 15000000)")
		setDailyDeep := fs.Int64("set-daily-deep", 0, "Set daily deep-tier uncached token budget (e.g. 5000000)")
		disableTokenCircuit := fs.Bool("disable-token-circuit", false, "Disable daily token budget alerts and auto-spawn circuit breaker (telemetry remains active)")
		enableTokenCircuit := fs.Bool("enable-token-circuit", false, "Re-enable daily token budget alerts and auto-spawn circuit breaker")
		setCtxCeiling := fs.Int64("set-context-ceiling", 0, "Set host auto-compaction context ceiling (default 512000)")
		setWarnCtx := fs.Int64("set-warn-context", 0, "Set Front Desk context warning threshold (default 120000)")
		setNudgeCtx := fs.Int64("set-nudge-context", 0, "Set Front Desk SessionStart /compact nudge threshold (default 160000)")
		setCritCtx := fs.Int64("set-crit-context", 0, "Set Front Desk critical context threshold (default 180000)")
		setMaxDailySpawns := fs.Int("set-max-daily-spawns", 0, "Set daily automated background spawn cap in config.yaml and quota state (e.g. 30)")
		maxDailySpawnsAlias := fs.Int("max-daily-spawns", 0, "Alias for --set-max-daily-spawns")
		resetCircuit := fs.Bool("reset-circuit", false, "Clear daily token/spawn budget circuit breaker for kind:auto triggers")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)

		targetMaxSpawns := *setMaxDailySpawns
		if targetMaxSpawns <= 0 && *maxDailySpawnsAlias > 0 {
			targetMaxSpawns = *maxDailySpawnsAlias
		}

		if *disableTokenCircuit && *enableTokenCircuit {
			fmt.Fprintln(os.Stderr, "hq obs budget: --disable-token-circuit and --enable-token-circuit cannot be combined")
			os.Exit(2)
		}
		cfg := obs.LoadBudgetConfig(vaultDir)
		updated := false
		if *disableTokenCircuit {
			cfg.TokenCircuitDisabled = true
			updated = true
		} else if *enableTokenCircuit {
			cfg.TokenCircuitDisabled = false
			updated = true
		}
		if *setDailyUncached > 0 {
			cfg.DailyUncachedBudget = *setDailyUncached
			updated = true
		}
		if *setDailyDeep > 0 {
			cfg.DailyDeepUncachedBudget = *setDailyDeep
			updated = true
		}
		if *setCtxCeiling > 0 {
			cfg.ContextCeilingTokens = *setCtxCeiling
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
		if targetMaxSpawns > 0 {
			if _, err := kernel.SetVaultMaxDailySpawns(vaultDir, targetMaxSpawns); err != nil {
				fmt.Fprintf(os.Stderr, "hq obs budget --set-max-daily-spawns failed: %v\n", err)
				os.Exit(1)
			}
		}
		if *resetCircuit {
			if err := obs.ResetBudgetCircuitBreaker(vaultDir); err != nil {
				fmt.Fprintf(os.Stderr, "hq obs budget --reset-circuit failed: %v\n", err)
				os.Exit(1)
			}
		} else if !updated && targetMaxSpawns <= 0 {
			obs.SyncDailyBudgetCircuitBreaker(vaultDir, now)
		}
		qState := kernel.LoadQuota(vaultDir)
		cfg.MaxDailySpawns = qState.MaxDailySpawns
		_ = webui.SyncMarkdownConsole(vaultDir)

		if *asJSON {
			printJSON(cfg)
			return
		}
		circuitLabel := "OK"
		if qState.CircuitOpen {
			circuitLabel = "OPEN (" + qState.CircuitReason + ")"
		}
		fmt.Printf("HQ Token Budget & Context Guard Configuration (local/memory/obs/budget.json + config.yaml)\n")
		if cfg.TokenCircuitDisabled {
			fmt.Printf("  Daily Token Budget Guard:   DISABLED (telemetry only; configured reference %s / deep %s)\n",
				obs.FormatTokensCompact(cfg.DailyUncachedBudget), obs.FormatTokensCompact(cfg.DailyDeepUncachedBudget))
		} else {
			fmt.Printf("  Daily Uncached Budget:      %s tokens\n", obs.FormatTokensCompact(cfg.DailyUncachedBudget))
			fmt.Printf("  Daily Deep Uncached Budget: %s tokens\n", obs.FormatTokensCompact(cfg.DailyDeepUncachedBudget))
		}
		fmt.Printf("  Daily Auto Spawn Budget:    %d / %d auto spawns today (+%d interactive, circuit=%s)\n",
			qState.EffectiveAutoSpawns(), qState.MaxDailySpawns, qState.InteractiveSpawnsToday, circuitLabel)
		fmt.Printf("  Front Desk Context Guards:  WARN=%s | NUDGE=%s | CRITICAL=%s | CEILING=%s\n",
			obs.FormatTokensCompact(cfg.WarnContextTokens),
			obs.FormatTokensCompact(cfg.NudgeContextTokens),
			obs.FormatTokensCompact(cfg.CritContextTokens),
			obs.FormatTokensCompact(cfg.EffectiveContextCeiling()))
		fmt.Printf("  Runaway Turn Guards (A3):   MaxPlannerSteps=%d | MaxUncached=%s | MaxOutput=%s\n",
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
		removed, cleanErr := kernel.CleanNestedTasks(vaultDir)
		if cleanErr != nil {
			fmt.Fprintf(os.Stderr, "hq obs reconcile cleanup failed: %v\n", cleanErr)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			printJSON(map[string]interface{}{
				"repairedTasks": repaired,
				"cleanedCount":  len(removed),
				"cleanedSlugs":  removed,
			})
			return
		}
		fmt.Printf("Reconciled subagent DAG attribution (%d task(s) updated, %d nested/workflow leak(s) cleaned)\n", repaired, len(removed))

	default:
		fmt.Fprintf(os.Stderr, "unknown obs subcommand: %s (expected tokens, top, inspect, anomalies, budget, reconcile)\n", sub)
		os.Exit(1)
	}
}

func runPluginCLI(rootDir, vaultDir string, args []string) {
	sub, args := splitSubcommand(args, "status")

	switch sub {
	case "install":
		fs := flag.NewFlagSet("plugin install", flag.ExitOnError)
		_ = fs.Bool("force", false, "Force reinstall even if already at the same version")
		globalScope := fs.Bool("global", false, "Install into ~/.gemini/config/plugins/agy-hq/ across all projects (default: project-scoped)")
		consoleSidecar := fs.Bool("console-sidecar", false, "Also install the UI-only agy-hq-console sidecar in ~/.gemini/config/sidecars/agy-hq-console/ (zero global hooks/rules)")
		projectFlag := fs.String("project", "", "Optional target project directory to attach .agents/plugins.json and .agents/skills.json in project-scoped mode")
		globalVault := fs.Bool("global-vault", false, "Use canonical global Vault (~/.gemini/antigravity/hq/vault)")
		vaultFlag := fs.String("vault-dir", "", "Explicit default Vault directory to register in ~/.gemini/antigravity/hq/config.json")
		pluginSrcFlag := fs.String("plugin-dir", "", "Optional explicit source plugin directory")
		binDirFlag := fs.String("bin-dir", "", "Optional destination bin directory (defaults to ~/.local/bin)")
		attachSkills := fs.Bool("attach-skills", true, "Attach <vault>/skills in <project>/.agents/skills.json when --project is specified")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		targetVault := strings.TrimSpace(*vaultFlag)
		if targetVault == "" && !*globalVault && strings.TrimSpace(*projectFlag) == "" {
			targetVault = vaultDir
		}
		res, err := plugin.Install(plugin.InstallOptions{
			SourceRepoDir:         rootDir,
			PluginSrcDir:          *pluginSrcFlag,
			ProjectDir:            *projectFlag,
			BinDir:                *binDirFlag,
			VaultDir:              targetVault,
			GlobalVault:           *globalVault,
			GlobalScope:           *globalScope,
			InstallConsoleSidecar: *consoleSidecar,
			SkipAttachSkills:      !*attachSkills,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq plugin install failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(res)
			return
		}
		fmt.Printf("Installed agy-hq plugin v%s (scope: %s · commit %s)\n", res.Version, res.Scope, res.Commit)
		fmt.Printf("  Binary:       %s (sha256:%s)\n", res.BinaryPath, shortPrefix(res.BinarySHA256, 12))
		fmt.Printf("  Plugin Dir:   %s (%d files, bundle:%s)\n", res.PluginDir, res.FileCount, shortPrefix(res.BundleDigest, 12))
		if res.ConsoleSidecarDir != "" {
			fmt.Printf("  Sidecar UI:   %s (sidecar://agy-hq-console/)\n", res.ConsoleSidecarDir)
		}
		if res.ProjectDir != "" {
			fmt.Printf("  Project Root: %s\n", res.ProjectDir)
		}
		if res.ProjectPluginsManifest != "" {
			fmt.Printf("  Project Cfg:  %s\n", res.ProjectPluginsManifest)
		}
		if res.ProjectSkillsManifest != "" {
			fmt.Printf("  Skills Cfg:   %s\n", res.ProjectSkillsManifest)
		}
		if res.RemovedGlobalPluginDir != "" {
			fmt.Printf("  Tidied Up:    removed global plugin at %s (scoped strictly to project)\n", res.RemovedGlobalPluginDir)
		}
		if res.RemovedLegacySidecar != "" {
			fmt.Printf("  Tidied Up:    removed legacy standalone sidecar at %s\n", res.RemovedLegacySidecar)
		}
		fmt.Printf("  Vault Dir:    %s\n", res.VaultDir)
		fmt.Printf("  Global Cfg:   %s\n", res.GlobalConfigPath)
		if res.DaemonRestarted {
			fmt.Printf("  Daemon Sync:  restarted background daemon with clean environment\n")
		}
		if !res.BinDirOnPath {
			fmt.Printf("  Tip:          Add %s to your shell PATH so bare `hq` resolves in interactive shells.\n", filepath.Dir(res.BinaryPath))
		}

	case "uninstall":
		fs := flag.NewFlagSet("plugin uninstall", flag.ExitOnError)
		purgeBin := fs.Bool("purge-bin", false, "Also remove ~/.local/bin/hq binary")
		detachProject := fs.String("detach-project", "", "Optional project directory from which to remove .agents/plugins.json entry")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		rep, err := plugin.Uninstall(*purgeBin, *detachProject)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq plugin uninstall failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(rep)
			return
		}
		fmt.Printf("Uninstalled agy-hq plugin (removedGlobal=%q, removedShared=%q, removedLegacySidecar=%q, removedBinary=%q, preservedVault=%s)\n",
			rep.RemovedPluginDir, rep.RemovedSharedPluginDir, rep.RemovedLegacySidecar, rep.RemovedBinaryPath, rep.PreservedVaultDir)

	case "status":
		fs := flag.NewFlagSet("plugin status", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		st, err := plugin.Status(vaultDir, rootDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq plugin status failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(st)
			return
		}
		fmt.Printf("HQ Plugin & Binary Status (v%s · scope: %s · commit %s · schema v%d)\n", st.EngineVersion, st.Scope, st.EngineCommit, st.VaultSchemaVersion)
		fmt.Printf("  Active Vault:      %s (source: %s)\n", st.VaultDir, st.VaultResolutionSource)
		fmt.Printf("  Global Binary:     %s (installed=%v, synced=%v, inPATH=%v)\n",
			st.GlobalBinaryPath, st.GlobalBinaryInstalled, st.BinaryInSync, st.BinDirOnPath)
		fmt.Printf("  Shared Bundle Dir: %s (installed=%v, version=%s, bundleSynced=%v)\n",
			st.SharedPluginDir, st.SharedPluginInstalled, st.SharedPluginVersion, st.BundleInSync)
		fmt.Printf("  Global Plugin Dir: %s (installed=%v, version=%s)\n",
			st.GlobalPluginDir, st.GlobalPluginInstalled, st.GlobalPluginVersion)
		if st.WorkspacePluginFound {
			fmt.Printf("  Workspace Plugin:  %s (version=%s, activePrecedence=%s)\n",
				st.WorkspacePluginDir, st.WorkspacePluginVer, "workspace-first")
		}
		if st.ProjectPluginsManifest != "" {
			fmt.Printf("  Project Manifest:  %s\n", st.ProjectPluginsManifest)
		}
		if st.ConsoleSidecarInstalled {
			fmt.Printf("  Console Sidecar:   %s (sidecar://agy-hq-console/)\n", st.ConsoleSidecarDir)
		}
		if st.LegacySidecarFound {
			fmt.Printf("  Legacy Sidecar:    %s (superseded — run 'hq plugin install' to tidy)\n", st.LegacySidecarDir)
		}
		if st.DaemonRunning {
			fmt.Printf("  Live Daemon:       running (version=%s, commit=%s, skewed=%v)\n",
				st.DaemonVersion, st.DaemonCommit, st.DaemonSkewed)
		} else {
			fmt.Printf("  Live Daemon:       inactive\n")
		}

	case "validate":
		fs := flag.NewFlagSet("plugin validate", flag.ExitOnError)
		strict := fs.Bool("strict", true, "Treat warnings as errors")
		_ = fs.Parse(args)
		targetDir := filepath.Join(rootDir, ".agents", "plugins", "agy-hq")
		if !vault.IsAgyHQRepo(rootDir) {
			if st, sErr := plugin.Status(vaultDir, rootDir); sErr == nil && st != nil {
				if st.GlobalPluginInstalled {
					targetDir = st.GlobalPluginDir
				} else if st.SharedPluginInstalled {
					targetDir = st.SharedPluginDir
				}
			}
		}
		if len(fs.Args()) > 0 {
			targetDir = fs.Args()[0]
		}
		if err := plugin.ValidateBundle(targetDir, *strict); err != nil {
			fmt.Fprintf(os.Stderr, "hq plugin validate failed: %v\n", err)
			os.Exit(1)
		}
		agyChecked := false
		if os.Getenv("HQ_SKIP_AGY_PLUGIN_VALIDATE") != "1" {
			ran, out, vErr := plugin.ValidateWithAgyCLI(targetDir)
			if vErr != nil {
				fmt.Fprintf(os.Stderr, "hq plugin validate (agy CLI) failed: %v\n%s\n", vErr, out)
				os.Exit(1)
			}
			agyChecked = ran
		}
		fmt.Printf("Validated plugin bundle at %s (strict=%v, agyCLI=%v)\n", targetDir, *strict, agyChecked)

	default:
		fmt.Fprintf(os.Stderr, "unknown plugin subcommand: %s (expected install, uninstall, status, validate)\n", sub)
		os.Exit(1)
	}
}

// resolveBodyInput selects the message body from --body, --body-stdin, or --body-file,
// rejecting conflicting flags so callers cannot accidentally mix inline and piped inputs.
