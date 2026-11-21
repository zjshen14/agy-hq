package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/authz"
	"github.com/zjshen14/agy-hq/internal/delegates"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/taint"
	"github.com/zjshen14/agy-hq/internal/webui"
)

type multiStringFlag []string

func (m *multiStringFlag) String() string { return strings.Join(*m, "\n") }
func (m *multiStringFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func parseKVFlags(items []string) (map[string]string, error) {
	out := make(map[string]string, len(items))
	for _, raw := range items {
		k, v, ok := strings.Cut(raw, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("expected key=value pair, got %q", raw)
		}
		out[k] = v
	}
	return out, nil
}

// requireInteractiveOwnerOrCLI enforces that privileged management commands (`show`, `ack-nudge`)
// cannot execute inside tainted, [Auto], or subagent sessions (RFC-0026 §6.1 / §8.3).
func requireInteractiveOwnerOrCLI(vaultDir, cmdLabel string) (string, bool) {
	convID, _ := taint.ResolveCallerConvID(vaultDir, os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	if strings.TrimSpace(convID) == "" {
		return "", true
	}
	isSub := kernel.IsSubagentConversation(vaultDir, convID)
	isAuto := false
	if t, ok := kernel.FindTaskByConversationID(vaultDir, convID); ok && t.EffectiveKind() == "auto" {
		isAuto = true
	}
	pol := authz.Resolve(vaultDir, convID, authz.ResolveHints{
		IsSubagent:   isSub,
		IsAutoWorker: isAuto,
	})
	if pol.Tainted || pol.Profile == authz.ProfileUnattendedAuto || pol.Profile == authz.ProfileSubagent {
		fmt.Fprintf(os.Stderr, "%s denied: only interactive owner or direct terminal CLI sessions may run this command (active profile: %s)\n", cmdLabel, pol.Profile)
		os.Exit(1)
	}
	return convID, false
}

func runDelegateCLI(vaultDir string, args []string) {
	sub, rest := splitSubcommand(args, "list")

	switch sub {
	case "list", "ls":
		denyIfCallerTainted(vaultDir, "delegate list")
		fs := flag.NewFlagSet("delegate list", flag.ExitOnError)
		available := fs.Bool("available", false, "List available transports, workspaces, and policies for authoring")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		if *available {
			res := delegates.ListAvailableResources(vaultDir)
			if *asJSON {
				printJSON(res)
				return
			}
			fmt.Println("Available resources for delegate policy authoring:")
			for k, v := range res {
				fmt.Printf("  %-12s: %v\n", k, v)
			}
			return
		}
		sum := delegates.LoadDashboardSummary(vaultDir)
		if *asJSON {
			printJSON(sum)
			return
		}
		fmt.Printf("Scoped Delegates Summary (configured=%v · policies=%d/%d enabled · transports=%d · replies_1h=%d/%d · pending_nudges=%d)\n",
			sum.PolicyConfigured, sum.EnabledPolicyCount, sum.PolicyCount, sum.TransportCount, sum.RepliesLastHour, sum.GlobalMaxPerHour, sum.PendingNudges)
		if cfg, _, exists, err := delegates.LoadConfig(vaultDir); err == nil && exists {
			for _, p := range cfg.Policies {
				state := "enabled"
				if !p.IsEnabled() {
					state = "disabled"
				}
				fmt.Printf("  - %-22s [%s] egress=%-19s tasks=%d workspaces=%d cmds=%d nudges=%d\n",
					p.ID, state, p.Egress.Mode, len(p.Grants.Tasks), len(p.Grants.Workspaces), len(p.Grants.Commands), len(p.Grants.Nudges))
			}
		}

	case "show":
		requireInteractiveOwnerOrCLI(vaultDir, "hq delegate show")
		fs := flag.NewFlagSet("delegate show", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		res, err := delegates.ShowPolicy(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate show failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(res)
			return
		}
		if !res.Exists {
			fmt.Printf("No delegate policy configured yet (%s).\n", res.Path)
			return
		}
		fmt.Printf("# Delegate Policy (%s)\n# sha256: %s\n\n%s\n", res.Path, res.SHA256, strings.TrimSpace(res.RawJSONC))

	case "validate":
		denyIfCallerTainted(vaultDir, "delegate validate")
		fs := flag.NewFlagSet("delegate validate", flag.ExitOnError)
		filePath := fs.String("file", "", "Optional draft JSONC file to validate (defaults to private/delegates/policy.json)")
		checkPins := fs.Bool("check-pins", false, "Also verify command binary SHA-256 pins in private/trust.json")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		var cfg delegates.Config
		var err error
		if strings.TrimSpace(*filePath) != "" {
			raw, rErr := os.ReadFile(strings.TrimSpace(*filePath))
			if rErr != nil {
				fmt.Fprintf(os.Stderr, "hq delegate validate read failed: %v\n", rErr)
				os.Exit(1)
			}
			cfg, err = delegates.ParseConfig(raw)
		} else {
			var exists bool
			cfg, _, exists, err = delegates.LoadConfig(vaultDir)
			if err == nil && !exists {
				fmt.Fprintln(os.Stderr, "hq delegate validate: no policy.json configured (pass --file=<draft.jsonc>)")
				os.Exit(1)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate validate failed: %v\n", err)
			os.Exit(1)
		}
		warnings, vErr := delegates.ValidateConfig(vaultDir, cfg, delegates.ValidateOptions{CheckBinaryPins: *checkPins})
		if vErr != nil {
			fmt.Fprintf(os.Stderr, "hq delegate validate failed: %v\n", vErr)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(map[string]interface{}{"valid": true, "policies": len(cfg.Policies), "commands": len(cfg.Commands), "warnings": warnings})
			return
		}
		fmt.Printf("Valid delegate policy (%d policy(s), %d command(s), %d snapshot(s)).\n", len(cfg.Policies), len(cfg.Commands), len(cfg.Snapshots))
		for _, w := range warnings {
			fmt.Printf("  [WARN] %s\n", w)
		}

	case "configure":
		convID, isCLI := requireInteractiveOwnerOrCLI(vaultDir, "hq delegate configure")
		fs := flag.NewFlagSet("delegate configure", flag.ExitOnError)
		filePath := fs.String("file", "", "Path to draft delegate policy JSONC file")
		baseSHA := fs.String("base-sha256", "", "Expected current policy.json SHA-256 for optimistic concurrency")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*filePath) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq delegate configure --file=<draft.jsonc> [--base-sha256=<sha256>] [--json]")
			os.Exit(1)
		}
		raw, err := os.ReadFile(strings.TrimSpace(*filePath))
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate configure read failed: %v\n", err)
			os.Exit(1)
		}
		res, err := delegates.ConfigurePolicy(vaultDir, raw, *baseSHA, convID, isCLI)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate configure failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			printJSON(res)
			return
		}
		fmt.Println(res.SummaryText)
		for _, r := range res.Reasons {
			fmt.Printf("  - %s\n", r)
		}
		for _, w := range res.Warnings {
			fmt.Printf("  [WARN] %s\n", w)
		}

	case "trust-command":
		convID, isCLI := requireInteractiveOwnerOrCLI(vaultDir, "hq delegate trust-command")
		fs := flag.NewFlagSet("delegate trust-command", flag.ExitOnError)
		refresh := fs.Bool("refresh", false, "Re-pin upgraded command binaries into Targets[\"delegates\"]")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		if !*refresh {
			fmt.Fprintln(os.Stderr, "usage: hq delegate trust-command --refresh [--json]")
			os.Exit(1)
		}
		res, err := delegates.TrustCommandsRefresh(vaultDir, convID, isCLI)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate trust-command failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(res)
			return
		}
		fmt.Println(res.SummaryText)

	case "test":
		denyIfCallerTainted(vaultDir, "delegate test")
		fs := flag.NewFlagSet("delegate test", flag.ExitOnError)
		caller := fs.String("caller", "", "Simulated verified peer caller_id")
		text := fs.String("text", "", "Simulated peer request text")
		source := fs.String("source", "", "Optional transport:<pack>/<id> source")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*caller) == "" || strings.TrimSpace(*text) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq delegate test --caller=<email-or-id> --text=\"...\" [--source=transport:<pack>/<id>] [--json]")
			os.Exit(1)
		}
		cfg, _, exists, err := delegates.LoadConfig(vaultDir)
		if err != nil || !exists {
			fmt.Fprintf(os.Stderr, "hq delegate test: delegate policy unavailable (%v)\n", err)
			os.Exit(1)
		}
		dm := delegates.DryRunMatch(cfg, *caller, *source, *text)
		if *asJSON {
			printJSON(dm)
			return
		}
		fmt.Printf("Caller: %s\nMatched Policies: %v\nCompiled Profile: %s\n", dm.CallerID, dm.MatchedPolicies, dm.CompiledProfile)
		if dm.FastPathEligible {
			fmt.Printf("0-Token Fast Path: nudge=%s params=%v\n", dm.FastPathNudgeID, dm.ExtractedParams)
		}
		if len(dm.CompiledVerbs) > 0 {
			fmt.Printf("Allowed HQ Verbs: %s\n", strings.Join(dm.CompiledVerbs, ", "))
		}

	case "bind-target":
		requireInteractiveOwnerOrCLI(vaultDir, "hq delegate bind-target")
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: hq delegate bind-target <policy-id> <task-slug>")
			os.Exit(1)
		}
		if err := delegates.BindTargetOverride(vaultDir, rest[0], rest[1]); err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate bind-target failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Bound delegate policy %q target task override -> %q\n", rest[0], rest[1])

	case "ack-nudge":
		requireInteractiveOwnerOrCLI(vaultDir, "hq delegate ack-nudge")
		posID, flagArgs := splitFirstPositionalAndFlags(rest)
		fs := flag.NewFlagSet("delegate ack-nudge", flag.ExitOnError)
		taskSlug := fs.String("task", "", "Task slug whose queued nudges to acknowledge")
		all := fs.Bool("all", false, "Acknowledge all queued nudges for --task")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		n, err := delegates.AckNudge(vaultDir, posID, *taskSlug, *all)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate ack-nudge failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			printJSON(map[string]interface{}{"acknowledged": n})
			return
		}
		fmt.Printf("Acknowledged %d queued peer nudge(s).\n", n)

	case "audit":
		denyIfCallerTainted(vaultDir, "delegate audit")
		fs := flag.NewFlagSet("delegate audit", flag.ExitOnError)
		limit := fs.Int("limit", 20, "Maximum audit entries to display")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		entries, err := delegates.ReadAudit(vaultDir, *limit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate audit failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(entries)
			return
		}
		if len(entries) == 0 {
			fmt.Println("No delegate audit entries yet.")
			return
		}
		for _, e := range entries {
			fmt.Printf("%s  %-14s %-18s req=%-16s caller=%-20s outcome=%s %s\n",
				e.Timestamp, e.Action, e.PolicyID, e.RequestID, e.CallerID, e.Outcome, e.Detail)
		}

	case "poll":
		if strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")) != "" {
			fmt.Fprintln(os.Stderr, "hq delegate poll refused: daemon-only command")
			os.Exit(1)
		}
		fs := flag.NewFlagSet("delegate poll", flag.ExitOnError)
		dryRun := fs.Bool("dry-run", false, "Dry-run poll check")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		rep, err := delegates.PollDue(context.Background(), vaultDir, time.Now().UTC(), *dryRun)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate poll failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(rep)
			return
		}
		fmt.Printf("Delegate poll: transports=%d items=%d fast_path=%d workers=%d classifiers=%d escalated=%d\n",
			rep.TransportsPolled, rep.ItemsProcessed, rep.FastPathNudges, rep.WorkersEnqueued, rep.Classifiers, rep.Escalated)

	case "sync-snapshots":
		if strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")) != "" {
			fmt.Fprintln(os.Stderr, "hq delegate sync-snapshots refused: daemon-only command")
			os.Exit(1)
		}
		fs := flag.NewFlagSet("delegate sync-snapshots", flag.ExitOnError)
		dryRun := fs.Bool("dry-run", false, "Dry-run snapshot build & egress check")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		n, err := delegates.SyncDueSnapshots(context.Background(), vaultDir, time.Now().UTC(), *dryRun)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate sync-snapshots failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(map[string]int{"published": n})
			return
		}
		fmt.Printf("Published %d delegate offline snapshot(s).\n", n)

	case "apply-policy":
		if strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")) != "" {
			fmt.Fprintln(os.Stderr, "hq delegate apply-policy refused: approval-runner-only command (use `hq delegate configure`)")
			os.Exit(1)
		}
		fs := flag.NewFlagSet("delegate apply-policy", flag.ExitOnError)
		payloadFile := fs.String("payload-file", "", "Path or /dev/fd/3 containing approved PolicyApplyBundle JSON")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*payloadFile) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq delegate apply-policy --payload-file=/dev/fd/3")
			os.Exit(1)
		}
		raw, err := os.ReadFile(strings.TrimSpace(*payloadFile))
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate apply-policy read failed: %v\n", err)
			os.Exit(1)
		}
		shaHex, err := delegates.ApplyPolicyFD3(vaultDir, raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate apply-policy failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Applied approved delegate policy (sha256:%s).\n", shaHex[:12])

	case "apply-reply":
		if strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")) != "" {
			fmt.Fprintln(os.Stderr, "hq delegate apply-reply refused: approval-runner-only command")
			os.Exit(1)
		}
		fs := flag.NewFlagSet("delegate apply-reply", flag.ExitOnError)
		payloadFile := fs.String("payload-file", "", "Path or /dev/fd/3 containing approved ReplyApplyBundle JSON")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*payloadFile) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq delegate apply-reply --payload-file=/dev/fd/3")
			os.Exit(1)
		}
		raw, err := os.ReadFile(strings.TrimSpace(*payloadFile))
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate apply-reply read failed: %v\n", err)
			os.Exit(1)
		}
		if err := delegates.ApplyReplyFD3(context.Background(), vaultDir, raw); err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate apply-reply failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Println("Delivered approved delegate reply.")

	case "read-request":
		posReq, flagArgs := splitFirstPositionalAndFlags(rest)
		fs := flag.NewFlagSet("delegate read-request", flag.ExitOnError)
		reqFlag := fs.String("request", "", "Request ID bound to active session grant")
		_ = fs.Parse(flagArgs)
		reqID := strings.TrimSpace(*reqFlag)
		if reqID == "" {
			reqID = posReq
		}
		out, err := delegates.ReadRequest(vaultDir, os.Getenv("ANTIGRAVITY_CONVERSATION_ID"), reqID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate read-request failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(out)

	case "route":
		fs := flag.NewFlagSet("delegate route", flag.ExitOnError)
		reqFlag := fs.String("request", "", "Request ID bound to classifier grant")
		policyFlag := fs.String("policy", "", "Selected candidate policy ID or `escalate`")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*policyFlag) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq delegate route --request=<id> --policy=<candidate-id|escalate>")
			os.Exit(1)
		}
		res, err := delegates.RouteFromClassifier(context.Background(), vaultDir, os.Getenv("ANTIGRAVITY_CONVERSATION_ID"), *reqFlag, *policyFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate route failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Routed request %q -> %s\n", *reqFlag, res)

	case "read-task":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: hq delegate read-task <task-slug>")
			os.Exit(1)
		}
		view, err := delegates.ReadTask(vaultDir, os.Getenv("ANTIGRAVITY_CONVERSATION_ID"), rest[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate read-task failed: %v\n", err)
			os.Exit(1)
		}
		printJSON(view)

	case "read-workspace":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: hq delegate read-workspace <workspace-key>")
			os.Exit(1)
		}
		view, err := delegates.ReadWorkspace(vaultDir, os.Getenv("ANTIGRAVITY_CONVERSATION_ID"), rest[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate read-workspace failed: %v\n", err)
			os.Exit(1)
		}
		printJSON(view)

	case "run":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: hq delegate run <cmd-id> [--param key=val ... | --arg=val ...]")
			os.Exit(1)
		}
		cmdID := rest[0]
		argPairs := make(map[string]string)
		for i := 1; i < len(rest); i++ {
			tok := rest[i]
			if tok == "--param" && i+1 < len(rest) {
				i++
				if k, v, ok := strings.Cut(rest[i], "="); ok {
					argPairs[strings.TrimSpace(k)] = v
				}
				continue
			}
			if strings.HasPrefix(tok, "--param=") {
				if k, v, ok := strings.Cut(strings.TrimPrefix(tok, "--param="), "="); ok {
					argPairs[strings.TrimSpace(k)] = v
				}
				continue
			}
			if strings.HasPrefix(tok, "--") {
				trimmed := strings.TrimPrefix(tok, "--")
				if k, v, ok := strings.Cut(trimmed, "="); ok {
					argPairs[strings.TrimSpace(k)] = v
				} else if i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "-") {
					i++
					argPairs[strings.TrimSpace(trimmed)] = rest[i]
				}
			}
		}
		out, err := delegates.RunPinnedCommand(context.Background(), vaultDir, os.Getenv("ANTIGRAVITY_CONVERSATION_ID"), cmdID, argPairs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate run failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(out)

	case "nudge":
		fs := flag.NewFlagSet("delegate nudge", flag.ExitOnError)
		reqFlag := fs.String("request", "", "Request ID")
		nudgeFlag := fs.String("nudge", "", "Granted nudge ID")
		var params multiStringFlag
		fs.Var(&params, "param", "Typed nudge parameter key=value (repeatable)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*nudgeFlag) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq delegate nudge --request=<id> --nudge=<id> [--param k=v ...]")
			os.Exit(1)
		}
		kv, err := parseKVFlags(params)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate nudge invalid --param: %v\n", err)
			os.Exit(1)
		}
		res, err := delegates.ExecuteNudge(context.Background(), vaultDir, os.Getenv("ANTIGRAVITY_CONVERSATION_ID"), *reqFlag, *nudgeFlag, kv, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate nudge failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(res)
			return
		}
		fmt.Println(res.Detail)

	case "reply":
		fs := flag.NewFlagSet("delegate reply", flag.ExitOnError)
		reqFlag := fs.String("request", "", "Request ID")
		bodyFlag := fs.String("body", "", "Single-line reply body")
		var lines multiStringFlag
		fs.Var(&lines, "line", "Repeatable reply line (joined with \\n)")
		bodyB64 := fs.String("body-b64", "", "RFC-4648 base64-encoded UTF-8 multiline reply body")
		var fields multiStringFlag
		fs.Var(&fields, "field", "Typed field key=value for structured_template egress (repeatable)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		kv, err := parseKVFlags(fields)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate reply invalid --field: %v\n", err)
			os.Exit(1)
		}
		var rawBody string
		if len(kv) == 0 {
			rawBody, err = delegates.DecodeReplyBody(*bodyFlag, lines, *bodyB64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq delegate reply failed: %v\n", err)
				os.Exit(1)
			}
		}
		res, err := delegates.SendReply(context.Background(), vaultDir, os.Getenv("ANTIGRAVITY_CONVERSATION_ID"), *reqFlag, rawBody, kv, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate reply failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(res)
			return
		}
		fmt.Println(res.Detail)

	case "escalate":
		fs := flag.NewFlagSet("delegate escalate", flag.ExitOnError)
		reqFlag := fs.String("request", "", "Request ID")
		catFlag := fs.String("category", "", "Category: out_of_scope | missing_info | needs_approval | uncertain | ambiguous")
		reasonFlag := fs.String("reason", "", "Concise single-line reason (<= 240 runes)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		item, err := delegates.EscalateRequest(context.Background(), vaultDir, os.Getenv("ANTIGRAVITY_CONVERSATION_ID"), *reqFlag, *catFlag, *reasonFlag, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq delegate escalate failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(item)
			return
		}
		fmt.Printf("Escalated request %q to Front Desk (outbox:%s).\n", *reqFlag, item.ID)

	default:
		fmt.Fprintf(os.Stderr, "unknown delegate subcommand: %s\n", sub)
		os.Exit(1)
	}
}
