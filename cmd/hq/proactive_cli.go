package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/sandbox"
	"github.com/zjshen14/agy-hq/internal/timers"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/webui"
)

func runOutboxCLI(vaultDir string, args []string) {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	now := time.Now()

	switch sub {
	case "add":
		fs := flag.NewFlagSet("outbox add", flag.ExitOnError)
		tier := fs.String("tier", outbox.TierBriefRollup, "Delivery tier (interrupt, brief_rollup, ideas_feed, silent_ledger)")
		scope := fs.String("scope", "general", "Topic/task scope")
		skill := fs.String("skill", "", "Originating skill name")
		title := fs.String("title", "", "Concise item title")
		summary := fs.String("summary", "", "Detailed summary or rationale")
		slug := fs.String("slug", "", "Optional suggested task/topic slug")
		critical := fs.Bool("critical", false, "Critical breach override (bypasses quiet-hours hold)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*title) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq outbox add --title=\"...\" [--tier=brief_rollup|ideas_feed|interrupt|silent_ledger] [--scope=...] [--skill=...] [--summary=\"...\"] [--critical]")
			os.Exit(1)
		}
		item, cooledDown, err := outbox.Add(vaultDir, outbox.OutboxItem{
			SourceSkill:   *skill,
			Scope:         *scope,
			Tier:          *tier,
			Title:         *title,
			Summary:       *summary,
			SuggestedSlug: *slug,
			Critical:      *critical,
		}, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq outbox add failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{"item": item, "cooledDown": cooledDown})
			return
		}
		fmt.Printf("Added outbox item %s [tier=%s, status=%s, cooledDown=%v, downgradedBy=%s]\n", item.ID, item.Tier, item.Status, cooledDown, item.DowngradedBy)

	case "list":
		fs := flag.NewFlagSet("outbox list", flag.ExitOnError)
		tier := fs.String("tier", "", "Optional tier filter")
		status := fs.String("status", "", "Optional status filter (pending, delivered, promoted, snoozed, dismissed)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		items, err := outbox.List(vaultDir, *tier, *status, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq outbox list failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(items)
			return
		}
		if len(items) == 0 {
			fmt.Println("No matching outbox items.")
			return
		}
		for _, it := range items {
			fmt.Printf("- [%s] (%s/%s) %s — %s\n", it.ID, it.Tier, it.Status, it.Title, it.Summary)
		}

	case "drain":
		fs := flag.NewFlagSet("outbox drain", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		drained, err := outbox.DrainBriefRollup(vaultDir, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq outbox drain failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(drained)
			return
		}
		fmt.Printf("Drained %d brief_rollup item(s) for Morning Brief:\n", len(drained))
		for _, it := range drained {
			fmt.Printf("- [%s] (%s) %s: %s\n", it.ID, it.Scope, it.Title, it.Summary)
		}

	case "eval":
		fs := flag.NewFlagSet("outbox eval", flag.ExitOnError)
		fixturesPath := fs.String("fixtures", "", "Optional newline-delimited JSON EvalFixture file (defaults to built-in 10-case calibration corpus)")
		asJSON := fs.Bool("json", false, "Output JSON report")
		_ = fs.Parse(rest)
		var fixtures []outbox.EvalFixture
		if strings.TrimSpace(*fixturesPath) != "" {
			loaded, err := outbox.LoadEvalFixturesFromFile(strings.TrimSpace(*fixturesPath))
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq outbox eval failed to load %s: %v\n", *fixturesPath, err)
				os.Exit(1)
			}
			fixtures = loaded
		}
		rep := outbox.RunEvalSuite(fixtures)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(rep)
			if !rep.AllThresholdsMet {
				os.Exit(1)
			}
			return
		}
		fmt.Printf("Outbox Signal-to-Noise Evaluation: %d/%d passed (allThresholdsMet=%v)\n", rep.PassedCases, rep.TotalCases, rep.AllThresholdsMet)
		fmt.Printf("  urgentRecall=%.1f%% · quietHoursSuppression=%.1f%% · duplicateSuppression=%.1f%% · falseInterruptRate=%.1f%%\n",
			rep.UrgentInterruptRecall*100.0, rep.QuietHoursSuppression*100.0, rep.DuplicateSuppressionRate*100.0, rep.FalseInterruptRate*100.0)
		for _, c := range rep.Cases {
			mark := "PASS"
			if !c.Passed {
				mark = "FAIL"
			}
			fmt.Printf("  [%s] %-36s got=%s (cooled=%v) want=%s (cooled=%v)\n", mark, c.Name, c.GotTier, c.GotCooled, c.ExpectTier, c.ExpectCooled)
		}
		if !rep.AllThresholdsMet {
			os.Exit(1)
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown outbox subcommand: %s (expected add, list, drain, or eval)\n", sub)
		os.Exit(1)
	}
}

func runIdeasCLI(vaultDir string, args []string) {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	now := time.Now()

	switch sub {
	case "list":
		fs := flag.NewFlagSet("ideas list", flag.ExitOnError)
		status := fs.String("status", outbox.StatusPending, "Status filter (pending, promoted, snoozed, dismissed, or all)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		stFilter := *status
		if stFilter == "all" {
			stFilter = ""
		}
		items, err := outbox.List(vaultDir, outbox.TierIdeasFeed, stFilter, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq ideas list failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(items)
			return
		}
		if len(items) == 0 {
			fmt.Println("No matching proactive ideas.")
			return
		}
		for _, it := range items {
			fmt.Printf("- [%s] (%s · skill=%s) %s — %s\n", it.ID, it.Status, it.SourceSkill, it.Title, it.Summary)
		}

	case "promote":
		var id string
		var flagArgs []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") && id == "" {
				id = a
			} else {
				flagArgs = append(flagArgs, a)
			}
		}
		fs := flag.NewFlagSet("ideas promote", flag.ExitOnError)
		spawn := fs.Bool("spawn", false, "Also spawn a dedicated Task Conversation from the promoted idea")
		dryRun := fs.Bool("dry-run", false, "Dry-run spawn when --spawn is set")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if id == "" {
			fmt.Fprintln(os.Stderr, "usage: hq ideas promote <idea-id> [--spawn] [--dry-run] [--json]")
			os.Exit(1)
		}
		item, err := outbox.PromoteIdea(vaultDir, id, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq ideas promote failed: %v\n", err)
			os.Exit(1)
		}
		var spawnOut string
		if *spawn {
			slug := item.SuggestedSlug
			if slug == "" {
				slug = item.Scope + "-" + strings.ToLower(item.ID)
			}
			scope := item.Scope
			if scope == "" {
				scope = "general"
			}
			title := fmt.Sprintf("[Task:%s] %s", scope, item.Title)
			prompt := fmt.Sprintf("%s\n\n%s", item.Title, item.Summary)
			resolvedModel, resolvedTier, _ := kernel.ResolveModel(prompt, "", "", "")
			createdAt := now.UTC().Format(time.RFC3339)
			_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
				TaskSlug:      slug,
				Title:         title,
				Kind:          "task",
				Scope:         scope,
				Tier:          resolvedTier,
				Model:         resolvedModel,
				CreatedAt:     createdAt,
				LastUpdatedAt: createdAt,
				LastSummary:   item.Summary,
				Status:        "active",
			})
			spawnOut, _ = kernel.SpawnTask(vaultDir, "idea-promote:"+item.ID, item.SourceSkill, title, prompt, resolvedModel, *dryRun, false)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{"item": item, "spawnOutput": spawnOut})
			return
		}
		fmt.Printf("Promoted idea %s (%s)\n", item.ID, item.Title)
		if spawnOut != "" {
			fmt.Println(spawnOut)
		}

	case "snooze":
		var id string
		var flagArgs []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") && id == "" {
				id = a
			} else {
				flagArgs = append(flagArgs, a)
			}
		}
		fs := flag.NewFlagSet("ideas snooze", flag.ExitOnError)
		durStr := fs.String("duration", "24h", "Snooze duration (e.g. 24h, 72h)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if id == "" {
			fmt.Fprintln(os.Stderr, "usage: hq ideas snooze <idea-id> [--duration=24h]")
			os.Exit(1)
		}
		dur, err := time.ParseDuration(*durStr)
		if err != nil || dur <= 0 {
			dur = 24 * time.Hour
		}
		item, err := outbox.SnoozeIdea(vaultDir, id, dur, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq ideas snooze failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(item)
			return
		}
		fmt.Printf("Snoozed idea %s until %s\n", item.ID, item.SnoozeUntil)

	case "dismiss":
		var id string
		var flagArgs []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") && id == "" {
				id = a
			} else {
				flagArgs = append(flagArgs, a)
			}
		}
		fs := flag.NewFlagSet("ideas dismiss", flag.ExitOnError)
		reason := fs.String("reason", "", "Optional dismissal reason for RSI feedback")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if id == "" {
			fmt.Fprintln(os.Stderr, "usage: hq ideas dismiss <idea-id> [--reason=\"...\"]")
			os.Exit(1)
		}
		item, err := outbox.DismissIdea(vaultDir, id, *reason, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq ideas dismiss failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(item)
			return
		}
		fmt.Printf("Dismissed idea %s\n", item.ID)

	default:
		fmt.Fprintf(os.Stderr, "unknown ideas subcommand: %s (expected list, promote, snooze, or dismiss)\n", sub)
		os.Exit(1)
	}
}

func runGoalsCLI(vaultDir string, args []string) {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	now := time.Now()

	switch sub {
	case "list":
		fs := flag.NewFlagSet("goals list", flag.ExitOnError)
		all := fs.Bool("all", false, "Include completed/paused goals")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		goals, err := kernel.ListGoals(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq goals list failed: %v\n", err)
			os.Exit(1)
		}
		var out []kernel.Goal
		for _, g := range goals {
			if !*all && g.Status != "active" {
				continue
			}
			out = append(out, g)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(out)
			return
		}
		if len(out) == 0 {
			fmt.Println("No matching goals.")
			return
		}
		for _, g := range out {
			done := 0
			for _, m := range g.Milestones {
				if m.Done {
					done++
				}
			}
			fmt.Printf("- [%s] (%s · scope=%s) %s — %d/%d milestones, %d waiting-on (target=%s)\n",
				g.Slug, g.Status, g.Scope, g.Title, done, len(g.Milestones), len(g.WaitingOn), g.TargetDate)
		}

	case "add":
		fs := flag.NewFlagSet("goals add", flag.ExitOnError)
		slug := fs.String("slug", "", "Optional goal slug")
		title := fs.String("title", "", "Goal title")
		scope := fs.String("scope", "", "Goal scope (e.g. engineering, investing, admin)")
		target := fs.String("target", "", "Target completion date (YYYY-MM-DD)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		g, err := kernel.AddGoal(vaultDir, *slug, *title, *scope, *target, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq goals add failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(g)
			return
		}
		fmt.Printf("Saved goal %q (%s)\n", g.Slug, g.Title)

	case "milestone":
		fs := flag.NewFlagSet("goals milestone", flag.ExitOnError)
		goalSlug := fs.String("goal", "", "Goal slug")
		title := fs.String("title", "", "Milestone title")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		g, ms, err := kernel.AddMilestone(vaultDir, *goalSlug, *title, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq goals milestone failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{"goal": g, "milestone": ms})
			return
		}
		fmt.Printf("Added milestone %s (%s) to goal %s\n", ms.ID, ms.Title, g.Slug)

	case "check-off", "check":
		var pos []string
		var flagArgs []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				pos = append(pos, a)
			} else {
				flagArgs = append(flagArgs, a)
			}
		}
		fs := flag.NewFlagSet("goals check-off", flag.ExitOnError)
		goalSlug := fs.String("goal", "", "Goal slug")
		msTarget := fs.String("milestone", "", "Milestone ID or title substring")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if *goalSlug == "" && len(pos) >= 1 {
			*goalSlug = pos[0]
		}
		if *msTarget == "" && len(pos) >= 2 {
			*msTarget = pos[1]
		}
		g, err := kernel.CheckMilestone(vaultDir, *goalSlug, *msTarget, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq goals check-off failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(g)
			return
		}
		fmt.Printf("Checked off milestone %q on goal %s\n", *msTarget, g.Slug)

	case "wait-on":
		fs := flag.NewFlagSet("goals wait-on", flag.ExitOnError)
		goalSlug := fs.String("goal", "", "Goal slug")
		counterparty := fs.String("counterparty", "", "Counterparty name, handle, or email")
		channel := fs.String("channel", "gmail", "Channel (gmail, gchat, external)")
		thread := fs.String("thread", "", "Optional thread or space ID")
		summary := fs.String("summary", "", "What we are waiting on")
		slaStr := fs.String("sla", "48h", "Follow-up SLA duration (e.g. 24h, 48h, 72h)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		sla, err := time.ParseDuration(*slaStr)
		if err != nil || sla <= 0 {
			sla = 48 * time.Hour
		}
		g, w, err := kernel.AddWaitingOn(vaultDir, *goalSlug, *counterparty, *channel, *thread, *summary, sla, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq goals wait-on failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{"goal": g, "waitingOn": w})
			return
		}
		fmt.Printf("Registered waiting-on %s for %s on goal %s (followUpAfter=%s)\n", w.ID, w.Counterparty, g.Slug, w.FollowUpAfter)

	case "mark-replied":
		var pos []string
		var flagArgs []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				pos = append(pos, a)
			} else {
				flagArgs = append(flagArgs, a)
			}
		}
		fs := flag.NewFlagSet("goals mark-replied", flag.ExitOnError)
		goalSlug := fs.String("goal", "", "Optional goal slug filter")
		target := fs.String("target", "", "Waiting-on ID or counterparty name")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if *target == "" && len(pos) >= 1 {
			*target = pos[0]
		}
		g, err := kernel.MarkWaitingReplied(vaultDir, *goalSlug, *target, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq goals mark-replied failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(g)
			return
		}
		fmt.Printf("Marked waiting-on %q replied on goal %s\n", *target, g.Slug)

	case "due":
		fs := flag.NewFlagSet("goals due", flag.ExitOnError)
		withinStr := fs.String("within", "72h", "Lookahead horizon (e.g. 24h, 72h, 168h)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		within, err := time.ParseDuration(*withinStr)
		if err != nil || within <= 0 {
			within = 72 * time.Hour
		}
		dueGoals, dueWaits, err := kernel.DueGoals(vaultDir, within, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq goals due failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{"dueGoals": dueGoals, "dueWaitingOn": dueWaits})
			return
		}
		fmt.Printf("Goals Due / SLAs Expiring within %s: %d goal(s), %d waiting-on SLA(s)\n", within, len(dueGoals), len(dueWaits))
		for _, g := range dueGoals {
			fmt.Printf("  - Goal [%s] %s (target=%s)\n", g.Slug, g.Title, g.TargetDate)
		}
		for _, w := range dueWaits {
			fmt.Printf("  - SLA [%s/%s] %s: %s (deadline=%s, status=%s)\n",
				w.GoalSlug, w.Waiting.ID, w.Waiting.Counterparty, w.Waiting.Summary, w.Waiting.FollowUpAfter, w.Waiting.Status)
		}

	case "complete":
		var goalSlug string
		var flagArgs []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") && goalSlug == "" {
				goalSlug = a
			} else {
				flagArgs = append(flagArgs, a)
			}
		}
		fs := flag.NewFlagSet("goals complete", flag.ExitOnError)
		slugFlag := fs.String("goal", "", "Goal slug")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if goalSlug == "" {
			goalSlug = *slugFlag
		}
		g, err := kernel.CompleteGoal(vaultDir, goalSlug, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq goals complete failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(g)
			return
		}
		fmt.Printf("Completed goal %s (%s)\n", g.Slug, g.Title)

	default:
		fmt.Fprintf(os.Stderr, "unknown goals subcommand: %s (expected list, add, milestone, check-off, wait-on, mark-replied, due, or complete)\n", sub)
		os.Exit(1)
	}
}

func runSpacesCLI(vaultDir string, args []string) {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	now := time.Now()

	switch sub {
	case "list":
		fs := flag.NewFlagSet("spaces list", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		spaces, err := webui.ListSpaces(vaultDir, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq spaces list failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(spaces)
			return
		}
		if len(spaces) == 0 {
			fmt.Println("No Console Spaces configured.")
			return
		}
		for _, sp := range spaces {
			fmt.Printf("- [%s] %s (schedule=%q, activeNow=%v, override=%q)\n", sp.ID, sp.Title, sp.Schedule, sp.ActiveNow, sp.OverrideMode)
		}

	case "active":
		fs := flag.NewFlagSet("spaces active", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		spaces, err := webui.ActiveSpaces(vaultDir, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq spaces active failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(spaces)
			return
		}
		if len(spaces) == 0 {
			fmt.Println("No Console Spaces active right now.")
			return
		}
		for _, sp := range spaces {
			fmt.Printf("🗂️ %s (%s)\n", sp.Title, sp.ID)
			for _, c := range sp.RenderedCards {
				fmt.Printf("  [%s] %s\n", c.Title, strings.Join(c.Lines, " | "))
			}
		}

	case "pin", "hide", "auto":
		if len(rest) < 1 {
			fmt.Fprintf(os.Stderr, "usage: hq spaces %s <space-id>\n", sub)
			os.Exit(1)
		}
		id := rest[0]
		if err := webui.SetSpaceOverride(vaultDir, id, sub); err != nil {
			fmt.Fprintf(os.Stderr, "hq spaces %s failed: %v\n", sub, err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Set Console Space %q override to %q\n", id, sub)

	default:
		fmt.Fprintf(os.Stderr, "unknown spaces subcommand: %s (expected list, active, pin, hide, or auto)\n", sub)
		os.Exit(1)
	}
}

func parseOptionalNowFlag(raw string) time.Time {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return t
	}
	return time.Now()
}

func runEventsCLI(vaultDir string, args []string) {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}

	switch sub {
	case "enqueue", "add":
		fs := flag.NewFlagSet("events enqueue", flag.ExitOnError)
		source := fs.String("source", "cli", "Event source (cli, webhook, timer, inbox_drop, gatekeeper)")
		dedup := fs.String("dedup", "", "Optional idempotency dedup key")
		scope := fs.String("scope", "", "Serialization scope key")
		session := fs.String("session", "", "Optional target conversation ID to wake")
		skill := fs.String("skill", "", "Optional skill to invoke")
		model := fs.String("model", "", "Optional model override")
		title := fs.String("title", "", "Event title")
		prompt := fs.String("prompt", "", "Event prompt")
		availAt := fs.String("available-at", "", "Optional earliest dispatch timestamp (RFC3339)")
		nowStr := fs.String("now", "", "Optional reference timestamp (RFC3339)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*title) == "" && strings.TrimSpace(*prompt) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq events enqueue --title=\"...\" --prompt=\"...\" [--scope=...] [--session=...] [--skill=...] [--dedup=...]")
			os.Exit(1)
		}
		now := parseOptionalNowFlag(*nowStr)
		ev, deduped, err := events.Enqueue(vaultDir, events.Event{
			Source:      *source,
			DedupKey:    *dedup,
			ScopeKey:    *scope,
			SessionID:   *session,
			Skill:       *skill,
			Model:       *model,
			Title:       *title,
			Prompt:      *prompt,
			AvailableAt: *availAt,
		}, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq events enqueue failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{"event": ev, "deduplicated": deduped})
			return
		}
		fmt.Printf("Enqueued event %s [scope=%s, status=%s, deduplicated=%v]\n", ev.ID, ev.ScopeKey, ev.Status, deduped)

	case "list":
		fs := flag.NewFlagSet("events list", flag.ExitOnError)
		status := fs.String("status", "", "Status filter (pending, leased, done, dropped)")
		archived := fs.Bool("archived", false, "Include recent archived events")
		limit := fs.Int("limit", 50, "Maximum archived events to include")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		items, err := events.List(vaultDir, *status, *archived, *limit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq events list failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(items)
			return
		}
		if len(items) == 0 {
			fmt.Println("No matching events in durable inbox.")
			return
		}
		for _, ev := range items {
			fmt.Printf("- [%s] (%s · source=%s · scope=%s · attempts=%d/%d) %s\n",
				ev.ID, ev.Status, ev.Source, ev.ScopeKey, ev.AttemptCount, ev.MaxAttempts, ev.Title)
		}

	case "inspect":
		var id string
		var flagArgs []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") && id == "" {
				id = a
			} else {
				flagArgs = append(flagArgs, a)
			}
		}
		fs := flag.NewFlagSet("events inspect", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if id == "" {
			fmt.Fprintln(os.Stderr, "usage: hq events inspect <event-id> [--json]")
			os.Exit(1)
		}
		ev, err := events.Inspect(vaultDir, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq events inspect failed: %v\n", err)
			os.Exit(1)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(ev)
		_ = asJSON

	case "retry":
		var id string
		var flagArgs []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") && id == "" {
				id = a
			} else {
				flagArgs = append(flagArgs, a)
			}
		}
		fs := flag.NewFlagSet("events retry", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if id == "" {
			fmt.Fprintln(os.Stderr, "usage: hq events retry <event-id> [--json]")
			os.Exit(1)
		}
		ev, err := events.Retry(vaultDir, id, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq events retry failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(ev)
			return
		}
		fmt.Printf("Reset event %s to pending\n", ev.ID)

	case "drop":
		var id string
		var flagArgs []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") && id == "" {
				id = a
			} else {
				flagArgs = append(flagArgs, a)
			}
		}
		fs := flag.NewFlagSet("events drop", flag.ExitOnError)
		reason := fs.String("reason", "Dropped via CLI", "Drop reason")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if id == "" {
			fmt.Fprintln(os.Stderr, "usage: hq events drop <event-id> [--reason=\"...\"] [--json]")
			os.Exit(1)
		}
		ev, err := events.Drop(vaultDir, id, *reason, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq events drop failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(ev)
			return
		}
		fmt.Printf("Dropped event %s\n", ev.ID)

	case "dispatch":
		fs := flag.NewFlagSet("events dispatch", flag.ExitOnError)
		dryRun := fs.Bool("dry-run", false, "Simulate spawns/continuations without executing")
		nowStr := fs.String("now", "", "Optional reference timestamp (RFC3339)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		now := parseOptionalNowFlag(*nowStr)
		dueFired, _ := timers.EvaluateDue(vaultDir, now)
		_ = kernel.EnqueueInboxFiles(vaultDir, now)
		actions, err := kernel.DispatchPendingEvents(vaultDir, now, *dryRun)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq events dispatch failed: %v\n", err)
			os.Exit(1)
		}
		outboxCount, _ := kernel.DispatchOutboxToFrontDesk(vaultDir, now, *dryRun)
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{
				"timersFired":     len(dueFired),
				"actions":         actions,
				"outboxDelivered": outboxCount,
			})
			return
		}
		fmt.Printf("Dispatched %d event batch(es) (timersFired=%d, outboxDelivered=%d)\n", len(actions), len(dueFired), outboxCount)
		for _, a := range actions {
			fmt.Printf("  - %s\n", a)
		}

	case "token":
		fs := flag.NewFlagSet("events token", flag.ExitOnError)
		rotate := fs.Bool("rotate", false, "Regenerate local/private/daemon_token")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		if *rotate {
			_ = os.Remove(filepath.Join(vaultDir, "private", "daemon_token"))
		}
		tok, err := events.EnsureDaemonToken(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq events token failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]string{"token": tok})
			return
		}
		fmt.Println(tok)

	default:
		fmt.Fprintf(os.Stderr, "unknown events subcommand: %s (expected enqueue, list, inspect, retry, drop, dispatch, or token)\n", sub)
		os.Exit(1)
	}
}

func runTimersCLI(vaultDir string, args []string) {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}

	switch sub {
	case "set", "add":
		fs := flag.NewFlagSet("timers set", flag.ExitOnError)
		kind := fs.String("kind", string(timers.TimerFollowUp), "Timer category (follow_up or schedule)")
		inDur := fs.String("in", "", "Relative duration for follow_up (e.g. 2m, 4h)")
		atStr := fs.String("at", "", "Absolute RFC3339 timestamp for follow_up")
		id := fs.String("id", "", "Schedule ID when --kind=schedule")
		cronExpr := fs.String("cron", "", "5-field cron expression (with optional CRON_TZ=) when --kind=schedule")
		scope := fs.String("scope", "", "Optional serialization scope key")
		session := fs.String("session", "", "Target conversation ID for follow_up (defaults to ANTIGRAVITY_CONVERSATION_ID when set)")
		noSession := fs.Bool("no-session", false, "Do not bind follow_up to ANTIGRAVITY_CONVERSATION_ID")
		skill := fs.String("skill", "", "Optional skill to invoke")
		model := fs.String("model", "", "Optional model override")
		title := fs.String("title", "", "Optional timer title")
		prompt := fs.String("prompt", "", "Follow-up or schedule instructions")
		nowStr := fs.String("now", "", "Optional reference timestamp (RFC3339)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		now := parseOptionalNowFlag(*nowStr)

		var tm timers.Timer
		var err error
		if strings.ToLower(strings.TrimSpace(*kind)) == string(timers.TimerSchedule) {
			tm, err = timers.UpsertSchedule(vaultDir, *id, *cronExpr, *scope, *skill, *model, *title, *prompt, now)
		} else {
			var dur time.Duration
			if strings.TrimSpace(*inDur) != "" {
				dur, err = time.ParseDuration(strings.TrimSpace(*inDur))
				if err != nil {
					fmt.Fprintf(os.Stderr, "hq timers set invalid --in %q: %v\n", *inDur, err)
					os.Exit(1)
				}
			}
			var dueAt time.Time
			if strings.TrimSpace(*atStr) != "" {
				dueAt, err = time.Parse(time.RFC3339, strings.TrimSpace(*atStr))
				if err != nil {
					fmt.Fprintf(os.Stderr, "hq timers set invalid --at %q: %v\n", *atStr, err)
					os.Exit(1)
				}
			}
			sessID := strings.TrimSpace(*session)
			if sessID == "" && !*noSession {
				sessID = strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
			}
			tm, err = timers.SetFollowUp(vaultDir, timers.FollowUpRequest{
				DueAt:           dueAt,
				InDuration:      dur,
				ScopeKey:        *scope,
				SessionID:       sessID,
				Skill:           *skill,
				Model:           *model,
				Title:           *title,
				Prompt:          *prompt,
				CreatedByConvID: strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")),
			}, now)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq timers set failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(tm)
			return
		}
		fmt.Printf("Armed %s timer %s [dueAt=%s, scope=%s, session=%s]\n", tm.Category, tm.ID, tm.DueAt, tm.ScopeKey, tm.SessionID)

	case "list":
		fs := flag.NewFlagSet("timers list", flag.ExitOnError)
		all := fs.Bool("all", false, "Include fired and cancelled timers")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		list, err := timers.List(vaultDir, *all)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq timers list failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(list)
			return
		}
		if len(list) == 0 {
			fmt.Println("No matching timers.")
			return
		}
		for _, t := range list {
			fmt.Printf("- [%s] (%s/%s · due=%s · session=%s) %s\n",
				t.ID, t.Category, t.Status, t.DueAt, t.SessionID, t.Title)
		}

	case "cancel", "rm", "remove":
		var id string
		var flagArgs []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") && id == "" {
				id = a
			} else {
				flagArgs = append(flagArgs, a)
			}
		}
		fs := flag.NewFlagSet("timers cancel", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if id == "" {
			fmt.Fprintln(os.Stderr, "usage: hq timers cancel <timer-id> [--json]")
			os.Exit(1)
		}
		tm, err := timers.Cancel(vaultDir, id, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq timers cancel failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(tm)
			return
		}
		fmt.Printf("Cancelled timer %s\n", tm.ID)

	default:
		fmt.Fprintf(os.Stderr, "unknown timers subcommand: %s (expected set, list, or cancel)\n", sub)
		os.Exit(1)
	}
}

func runProgressCLI(vaultDir string, args []string) {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}

	resolveDefaultSlug := func(explicit string) string {
		if s := strings.TrimSpace(explicit); s != "" {
			return s
		}
		if convID := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")); convID != "" {
			if t, ok := kernel.FindTaskByConversationID(vaultDir, convID); ok && t.TaskSlug != "" {
				return t.TaskSlug
			}
		}
		return ""
	}

	switch sub {
	case "save":
		fs := flag.NewFlagSet("progress save", flag.ExitOnError)
		taskSlug := fs.String("task", "", "Task slug (defaults to active task for ANTIGRAVITY_CONVERSATION_ID)")
		goal := fs.String("goal", "", "Current task goal")
		done := fs.String("done", "", "Completed & verified milestones")
		next := fs.String("next", "", "Remaining next steps")
		pointers := fs.String("pointers", "", "Key file/commit/PR pointers")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		slug := resolveDefaultSlug(*taskSlug)
		if slug == "" {
			fmt.Fprintln(os.Stderr, "usage: hq progress save --task=<slug> --goal=\"...\" --done=\"...\" --next=\"...\" [--pointers=\"...\"]")
			os.Exit(1)
		}
		cp, err := vault.SaveProgress(vaultDir, vault.ProgressCheckpoint{
			TaskSlug:  slug,
			Goal:      *goal,
			Completed: *done,
			NextSteps: *next,
			Pointers:  *pointers,
		}, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq progress save failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(cp)
			return
		}
		fmt.Printf("Saved progress checkpoint for %q (%s)\n", cp.TaskSlug, cp.Path)

	case "show", "get":
		var posSlug string
		var flagArgs []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") && posSlug == "" {
				posSlug = a
			} else {
				flagArgs = append(flagArgs, a)
			}
		}
		fs := flag.NewFlagSet("progress show", flag.ExitOnError)
		taskSlug := fs.String("task", "", "Task slug")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		slug := resolveDefaultSlug(*taskSlug)
		if slug == "" {
			slug = posSlug
		}
		if slug == "" {
			fmt.Fprintln(os.Stderr, "usage: hq progress show <task-slug> [--json]")
			os.Exit(1)
		}
		cp, raw, found, err := vault.LoadProgress(vaultDir, slug)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq progress show failed: %v\n", err)
			os.Exit(1)
		}
		if !found {
			fmt.Fprintf(os.Stderr, "no active progress checkpoint for %q\n", slug)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(cp)
			return
		}
		fmt.Print(raw)

	case "list":
		fs := flag.NewFlagSet("progress list", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		list, err := vault.ListProgress(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq progress list failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(list)
			return
		}
		if len(list) == 0 {
			fmt.Println("No active task progress checkpoints.")
			return
		}
		for _, cp := range list {
			fmt.Printf("- [%s] updated=%s — goal=%s | next=%s\n", cp.TaskSlug, cp.UpdatedAt, cp.Goal, cp.NextSteps)
		}

	case "archive", "clear":
		var posSlug string
		var flagArgs []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") && posSlug == "" {
				posSlug = a
			} else {
				flagArgs = append(flagArgs, a)
			}
		}
		fs := flag.NewFlagSet("progress archive", flag.ExitOnError)
		taskSlug := fs.String("task", "", "Task slug")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		slug := resolveDefaultSlug(*taskSlug)
		if slug == "" {
			slug = posSlug
		}
		if slug == "" {
			fmt.Fprintln(os.Stderr, "usage: hq progress archive <task-slug>")
			os.Exit(1)
		}
		dst, err := vault.ArchiveProgress(vaultDir, slug, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq progress archive failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]string{"taskSlug": slug, "archivedTo": dst})
			return
		}
		fmt.Printf("Archived progress checkpoint %q -> %s\n", slug, dst)

	default:
		fmt.Fprintf(os.Stderr, "unknown progress subcommand: %s (expected save, show, list, or archive)\n", sub)
		os.Exit(1)
	}
}

func runExecSandboxedCLI(vaultDir string, args []string) {
	var flagPart []string
	var cmdArgv []string
	sepFound := false
	for i, a := range args {
		if a == "--" {
			cmdArgv = args[i+1:]
			sepFound = true
			break
		}
		if !strings.HasPrefix(a, "-") {
			cmdArgv = args[i:]
			sepFound = true
			break
		}
		flagPart = append(flagPart, a)
	}
	_ = sepFound

	fs := flag.NewFlagSet("exec-sandboxed", flag.ExitOnError)
	workDir := fs.String("workdir", "", "Optional working directory")
	netIsolated := fs.Bool("net-isolated", false, "Unshare/deny network inside OS sandbox when supported")
	statusOnly := fs.Bool("status", false, "Print OS credential sandbox capability and config without running a command")
	asJSON := fs.Bool("json", false, "Output JSON when --status is set")
	_ = fs.Parse(flagPart)
	if len(cmdArgv) == 0 && fs.NArg() > 0 {
		cmdArgv = fs.Args()
	}

	if *statusOnly {
		cfg := sandbox.LoadConfig(vaultDir)
		cap := sandbox.DetectCapability()
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{
				"mode":       cfg.Mode,
				"capability": cap,
			})
			return
		}
		fmt.Printf("OS Credential Sandbox: mode=%s, available=%v, backend=%s — %s\n", cfg.Mode, cap.Available, cap.Backend, cap.Detail)
		return
	}

	if len(cmdArgv) == 0 {
		fmt.Fprintln(os.Stderr, "usage: hq exec-sandboxed [--workdir=...] [--net-isolated] -- <command> [args...]")
		os.Exit(1)
	}
	cmd, _, err := sandbox.BuildCommand(context.Background(), vaultDir, *workDir, *netIsolated, cmdArgv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hq exec-sandboxed error: %v\n", err)
		os.Exit(1)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if runErr := cmd.Run(); runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "hq exec-sandboxed failed: %v\n", runErr)
		os.Exit(1)
	}
}
