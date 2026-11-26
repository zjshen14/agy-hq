package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/living"
	"github.com/zjshen14/agy-hq/internal/pulse"
	"github.com/zjshen14/agy-hq/internal/webui"
)

func init() {
	kernel.RegisteredPulsePrecheck = func(ctx context.Context, vaultDir string, now time.Time, dryRun bool) ([]byte, error) {
		rep, err := pulse.RunPrecheck(ctx, vaultDir, pulse.PrecheckOptions{Now: now, DryRun: dryRun})
		if err != nil {
			return nil, err
		}
		return json.Marshal(rep)
	}
}

func runProactiveV3CLI(vaultDir, topCmd string, args []string) {
	switch topCmd {
	case "pulse":
		runPulseCLI(vaultDir, args)
	case "space":
		runLivingSpaceCLI(vaultDir, args)
	case "cancel":
		runCancelCLI(vaultDir, args)
	case "approvals":
		runApprovalsCLI(vaultDir, args)
	}
}

func runPulseCLI(vaultDir string, args []string) {
	sub, rest := splitSubcommand(args, "status")
	now := time.Now()
	switch sub {
	case "precheck":
		fs := flag.NewFlagSet("pulse precheck", flag.ExitOnError)
		dryRun := fs.Bool("dry-run", false, "Evaluate detectors without emitting outbox items or advancing watermarks")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		rep, err := pulse.RunPrecheck(context.Background(), vaultDir, pulse.PrecheckOptions{Now: now, DryRun: *dryRun})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq pulse precheck failed: %v\n", err)
			os.Exit(1)
		}
		if !*dryRun && rep.EmittedCount > 0 {
			_ = webui.SyncMarkdownConsole(vaultDir)
		}
		if *asJSON {
			printJSON(rep)
			return
		}
		fmt.Printf("Pulse precheck complete: emitted=%d skipped=%d disabled=%v\n", rep.EmittedCount, rep.SkippedCount, rep.Disabled)
		for _, f := range rep.Findings {
			fmt.Printf("- [%s] (%s) %s (outbox=%s)\n", f.Detector, f.Tier, f.Title, f.OutboxID)
		}

	case "status":
		fs := flag.NewFlagSet("pulse status", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		cfg := pulse.LoadConfig(vaultDir)
		st, err := pulse.LoadState(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq pulse status failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(map[string]interface{}{"config": cfg, "state": st})
			return
		}
		fmt.Printf("Pulse enabled=%v lastRunAt=%s firedKeys=%d preMeetingLead=%s cutoffLead=%s\n",
			cfg.Enabled, st.LastRunAt, len(st.FiredKeys), cfg.PreMeetingLead, cfg.CutoffLead)

	default:
		fmt.Fprintf(os.Stderr, "unknown pulse subcommand: %s (expected precheck or status)\n", sub)
		os.Exit(1)
	}
}

func runLivingSpaceCLI(vaultDir string, args []string) {
	sub, rest := splitSubcommand(args, "list")
	now := time.Now()
	switch sub {
	case "list":
		fs := flag.NewFlagSet("space list", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		list, err := living.List(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq space list failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(list)
			return
		}
		if len(list) == 0 {
			fmt.Println("No Living Spaces found.")
			return
		}
		for _, s := range list {
			fmt.Printf("- [%s] %s (locales=%s, goal=%s, html=%v, updated=%s)\n",
				s.Slug, s.Title, strings.Join(s.Locales, ","), s.GoalSlug, s.HasHTML, s.UpdatedAt)
		}

	case "create":
		fs := flag.NewFlagSet("space create", flag.ExitOnError)
		slug := fs.String("slug", "", "Living Space slug (lowercase alphanumeric and hyphens)")
		title := fs.String("title", "", "Space title")
		subtitle := fs.String("subtitle", "", "Optional subtitle")
		theme := fs.String("theme", "", "Optional visual theme (modern|editorial|heritage|ocean|forest)")
		goalSlug := fs.String("goal", "", "Optional linked Goal slug")
		tz := fs.String("tz", "", "Optional IANA timezone")
		localesRaw := fs.String("locales", "", "Optional comma-separated locales (defaults to living_spaces.default_locales)")
		fromJSON := fs.String("from-json", "", "Optional path to canonical space.json payload")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)

		var doc living.SpaceDoc
		if strings.TrimSpace(*fromJSON) != "" {
			raw, err := os.ReadFile(strings.TrimSpace(*fromJSON))
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq space create: read %s: %v\n", *fromJSON, err)
				os.Exit(1)
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				fmt.Fprintf(os.Stderr, "hq space create: parse JSON: %v\n", err)
				os.Exit(1)
			}
		}
		if strings.TrimSpace(*slug) != "" {
			doc.Slug = strings.TrimSpace(*slug)
		}
		if strings.TrimSpace(*title) != "" {
			doc.Title = living.Text(strings.TrimSpace(*title))
		}
		if strings.TrimSpace(*subtitle) != "" {
			doc.Subtitle = living.Text(strings.TrimSpace(*subtitle))
		}
		if strings.TrimSpace(*theme) != "" {
			doc.Theme = strings.TrimSpace(*theme)
		}
		if strings.TrimSpace(*goalSlug) != "" {
			doc.GoalSlug = strings.TrimSpace(*goalSlug)
		}
		if strings.TrimSpace(*tz) != "" {
			doc.Timezone = strings.TrimSpace(*tz)
		}
		if strings.TrimSpace(*localesRaw) != "" {
			doc.Locales = splitCSV(*localesRaw)
		}
		saved, err := living.Save(vaultDir, doc, now)
		dieIfErr("hq space create failed", err)
		if saved.GoalSlug != "" {
			if synced, sErr := living.SyncFromGoal(vaultDir, saved.Slug, saved.GoalSlug, now); sErr == nil {
				saved = synced
			}
		}
		htmlPath, size, bErr := living.Build(vaultDir, saved.Slug)
		if bErr != nil {
			fmt.Fprintf(os.Stderr, "hq space build failed: %v\n", bErr)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			printJSON(map[string]interface{}{"space": saved, "htmlPath": htmlPath, "bytes": size})
			return
		}
		fmt.Printf("Created & built Living Space %q -> %s (%d bytes)\n", saved.Slug, htmlPath, size)

	case "show":
		slug, flagArgs := splitFirstPositionalAndFlags(rest)
		fs := flag.NewFlagSet("space show", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		doc, err := living.Load(vaultDir, slug)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq space show failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(doc)
			return
		}
		fmt.Printf("Space %s: %s (locales=%v, blocks=%d, goal=%s)\n", doc.Slug, doc.Title.Resolve("en", ""), doc.Locales, len(doc.Blocks), doc.GoalSlug)

	case "build":
		slug, flagArgs := splitFirstPositionalAndFlags(rest)
		fs := flag.NewFlagSet("space build", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		htmlPath, size, err := living.Build(vaultDir, slug)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq space build failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			printJSON(map[string]interface{}{"slug": slug, "htmlPath": htmlPath, "bytes": size})
			return
		}
		fmt.Printf("Built Living Space %q -> %s (%d bytes)\n", slug, htmlPath, size)

	case "refresh":
		slug, flagArgs := splitFirstPositionalAndFlags(rest)
		fs := flag.NewFlagSet("space refresh", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		doc, err := living.RefreshDataSources(context.Background(), vaultDir, slug, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq space refresh failed: %v\n", err)
			os.Exit(1)
		}
		if doc.GoalSlug != "" {
			if synced, sErr := living.SyncFromGoal(vaultDir, doc.Slug, doc.GoalSlug, now); sErr == nil {
				doc = synced
			}
		}
		htmlPath, size, err := living.Build(vaultDir, doc.Slug)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq space build failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			printJSON(map[string]interface{}{"space": doc, "htmlPath": htmlPath, "bytes": size})
			return
		}
		fmt.Printf("Refreshed & rebuilt Living Space %q (%d data sources, %d bytes)\n", doc.Slug, len(doc.DataSources), size)

	case "sync-goal":
		slug, flagArgs := splitFirstPositionalAndFlags(rest)
		fs := flag.NewFlagSet("space sync-goal", flag.ExitOnError)
		goalSlug := fs.String("goal", "", "Goal slug to sync")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		doc, err := living.SyncFromGoal(vaultDir, slug, *goalSlug, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq space sync-goal failed: %v\n", err)
			os.Exit(1)
		}
		htmlPath, size, err := living.Build(vaultDir, doc.Slug)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq space build failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			printJSON(map[string]interface{}{"space": doc, "htmlPath": htmlPath, "bytes": size})
			return
		}
		fmt.Printf("Synced goal %q -> Living Space %q (%d bytes)\n", doc.GoalSlug, doc.Slug, size)

	case "export-artifact":
		slug, flagArgs := splitFirstPositionalAndFlags(rest)
		fs := flag.NewFlagSet("space export-artifact", flag.ExitOnError)
		dirFlag := fs.String("dir", "", "Target Antigravity artifact directory (defaults to Front Desk ArtifactDir)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		artDir := strings.TrimSpace(*dirFlag)
		if artDir == "" {
			if fd, ok := kernel.GetFrontDesk(vaultDir); ok {
				artDir = strings.TrimSpace(fd.ArtifactDir)
			}
		}
		outPath, err := living.ExportArtifact(vaultDir, slug, artDir, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq space export-artifact failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(map[string]string{"slug": slug, "artifactPath": outPath})
			return
		}
		fmt.Printf("Exported Living Space artifact -> %s\n", outPath)

	case "export":
		slug, flagArgs := splitFirstPositionalAndFlags(rest)
		fs := flag.NewFlagSet("space export", flag.ExitOnError)
		outFlag := fs.String("out", "", "Destination standalone HTML file path")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		outPath, err := living.ExportStandalone(vaultDir, slug, *outFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq space export failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(map[string]string{"slug": slug, "outputPath": outPath})
			return
		}
		fmt.Printf("Exported standalone Living Space -> %s\n", outPath)

	default:
		fmt.Fprintf(os.Stderr, "unknown space subcommand: %s (expected create, list, show, build, refresh, sync-goal, export-artifact, or export)\n", sub)
		os.Exit(1)
	}
}

func runCancelCLI(vaultDir string, args []string) {
	target, flagArgs := splitFirstPositionalAndFlags(args)
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	reason := fs.String("reason", "reverted_by_user", "Reason for cancellation")
	asJSON := fs.Bool("json", false, "Output JSON")
	_ = fs.Parse(flagArgs)
	if strings.TrimSpace(target) == "" {
		fmt.Fprintln(os.Stderr, "usage: hq cancel <id-or-scope> [--reason=\"...\"] [--json]")
		os.Exit(1)
	}
	rep, err := kernel.CancelTarget(vaultDir, target, *reason, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "hq cancel failed: %v\n", err)
		os.Exit(1)
	}
	_ = webui.SyncMarkdownConsole(vaultDir)
	if *asJSON {
		printJSON(rep)
		return
	}
	fmt.Printf("Cancelled %d item(s) matching %q (events=%d timers=%d proposals=%d)\n",
		rep.TotalCancelled, rep.Target, len(rep.CancelledEvents), len(rep.CancelledTimers), len(rep.CancelledProposals))
}

func runApprovalsCLI(vaultDir string, args []string) {
	sub, rest := splitSubcommand(args, "list")
	now := time.Now()
	switch sub {
	case "list":
		fs := flag.NewFlagSet("approvals list", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		pending, err := approvals.List(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq approvals list failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(pending)
			return
		}
		if len(pending) == 0 {
			fmt.Println("No pending approvals.")
			return
		}
		for _, p := range pending {
			fmt.Printf("- [%s] (%s) %s (expires=%s)\n", p.ID, p.ActionType, p.Summary, p.ExpiresAt)
		}

	case "expired":
		fs := flag.NewFlagSet("approvals expired", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		recs, err := approvals.ListExpiredProposals(vaultDir, 0, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq approvals expired failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(recs)
			return
		}
		if len(recs) == 0 {
			fmt.Println("No unrestaged expired proposals.")
			return
		}
		for _, r := range recs {
			fmt.Printf("- [%s] (%s · expired %s) %s\n", r.Proposal.ID, r.Proposal.ActionType, r.Timestamp, r.Proposal.Summary)
		}

	case "restage":
		id, flagArgs := splitFirstPositionalAndFlags(rest)
		fs, asJSON := newJSONFlagSet("approvals restage")
		_ = fs.Parse(flagArgs)
		if strings.TrimSpace(id) == "" {
			fatalf("usage: hq approvals restage <expired-proposal-id> [--json]")
		}
		fresh, err := approvals.RestageExpiredProposal(vaultDir, id, now)
		dieIfErr("hq approvals restage failed", err)
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			printJSON(fresh)
			return
		}
		fmt.Printf("Restaged %s as fresh 24h proposal %s (sha256:%s): %s\n", id, fresh.ID, shortPrefix(fresh.SHA256, 12), fresh.Summary)

	case "dismiss":
		id, flagArgs := splitFirstPositionalAndFlags(rest)
		fs := flag.NewFlagSet("approvals dismiss", flag.ExitOnError)
		reason := fs.String("reason", "Dismissed by user", "Reason for dismissing expired proposal")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if strings.TrimSpace(id) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq approvals dismiss <expired-proposal-id> [--reason=\"...\"] [--json]")
			os.Exit(1)
		}
		if err := approvals.DismissExpiredProposal(vaultDir, id, *reason, now); err != nil {
			fmt.Fprintf(os.Stderr, "hq approvals dismiss failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			printJSON(map[string]interface{}{"id": id, "status": "expired_dismissed", "reason": *reason})
			return
		}
		fmt.Printf("Dismissed expired proposal %s\n", id)

	default:
		fmt.Fprintf(os.Stderr, "unknown approvals subcommand: %s (expected list, expired, restage, or dismiss)\n", sub)
		os.Exit(1)
	}
}
