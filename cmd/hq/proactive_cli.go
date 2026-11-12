package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/outbox"
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

	default:
		fmt.Fprintf(os.Stderr, "unknown outbox subcommand: %s (expected add, list, or drain)\n", sub)
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
