package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zjshen14/agy-hq/internal/brief"
	"github.com/zjshen14/agy-hq/internal/connector"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/offer"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/watcher"
	"github.com/zjshen14/agy-hq/internal/webui"
)

type repeatFlag []string

func (rf *repeatFlag) String() string { return strings.Join(*rf, ",") }
func (rf *repeatFlag) Set(val string) error {
	*rf = append(*rf, val)
	return nil
}

func splitPositionalAndFlags(args []string) ([]string, []string) {
	var pos, fl []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
		} else {
			fl = append(fl, a)
		}
	}
	return pos, fl
}

func runExtensionCLI(rootDir, vaultDir string, args []string) {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}

	switch sub {
	case "list":
		fs := flag.NewFlagSet("extension list", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		packs, err := extension.DiscoverPacks(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq extension list failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(packs)
			return
		}
		if len(packs) == 0 {
			fmt.Println("No companion extension packs discovered.")
			return
		}
		for _, dp := range packs {
			fmt.Printf("- %-24s v%-10s enabled=%-5v trusted=%-5v (%s)\n", dp.ID, dp.Version, dp.Enabled, dp.Trusted, dp.Path)
		}

	case "lint":
		pos, fl := splitPositionalAndFlags(rest)
		fs := flag.NewFlagSet("extension lint", flag.ExitOnError)
		pathFlag := fs.String("path", "", "Path to companion extension pack directory")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(fl)
		targetDir := strings.TrimSpace(*pathFlag)
		if targetDir == "" && len(pos) > 0 {
			targetDir = pos[0]
		}
		if targetDir == "" {
			targetDir = "."
		}
		warnings, err := extension.LintPack(targetDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			out := map[string]interface{}{
				"ok":       err == nil,
				"path":     targetDir,
				"warnings": warnings,
			}
			if err != nil {
				out["error"] = err.Error()
			}
			_ = enc.Encode(out)
			if err != nil {
				os.Exit(1)
			}
			return
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq extension lint FAILED (%s): %v\n", targetDir, err)
			os.Exit(1)
		}
		fmt.Printf("Extension pack lint PASSED (%s)\n", targetDir)
		for _, w := range warnings {
			fmt.Printf("  [WARN] %s\n", w)
		}

	case "enable":
		denyIfCallerTainted(vaultDir, "extension enable")
		pos, fl := splitPositionalAndFlags(rest)
		fs := flag.NewFlagSet("extension enable", flag.ExitOnError)
		pathFlag := fs.String("path", "", "Optional explicit directory path to companion pack")
		usePackRouting := fs.Bool("use-pack-routing", false, "Opt into pack routing_defaults.tiers")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(fl)
		if len(pos) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq extension enable <pack-id> [--path=...] [--use-pack-routing] [--json]")
			os.Exit(1)
		}
		m, err := extension.EnablePack(context.Background(), vaultDir, pos[0], *pathFlag, *usePackRouting)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq extension enable failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(m)
			return
		}
		fmt.Printf("Enabled companion extension pack %s@%s in extensions.json (run '%s extension trust %s' to pin SHA-256 trust)\n",
			m.ID, m.Version, displayHQCmd(rootDir), m.ID)

	case "disable":
		denyIfCallerTainted(vaultDir, "extension disable")
		pos, fl := splitPositionalAndFlags(rest)
		fs := flag.NewFlagSet("extension disable", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(fl)
		if len(pos) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq extension disable <pack-id> [--json]")
			os.Exit(1)
		}
		if err := extension.DisablePack(context.Background(), vaultDir, pos[0]); err != nil {
			fmt.Fprintf(os.Stderr, "hq extension disable failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{"id": pos[0], "enabled": false})
			return
		}
		fmt.Printf("Disabled companion extension pack %s\n", pos[0])

	case "trust":
		denyIfCallerTainted(vaultDir, "extension trust")
		pos, fl := splitPositionalAndFlags(rest)
		fs := flag.NewFlagSet("extension trust", flag.ExitOnError)
		pathFlag := fs.String("path", "", "Optional explicit pack path")
		convFlag := fs.String("conv", "", "Optional conversation ID")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(fl)
		targetSpec := "vault"
		if len(pos) > 0 && strings.TrimSpace(pos[0]) != "" {
			targetSpec = strings.TrimSpace(pos[0])
		}
		convID := strings.TrimSpace(*convFlag)
		if convID == "" {
			convID = vault.ResolveConversationID()
		}
		prop, bundle, err := extension.StageTrustProposal(vaultDir, targetSpec, *pathFlag, convID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq extension trust failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{
				"proposal": prop,
				"bundle":   bundle,
			})
			return
		}
		fmt.Printf("Staged extension trust proposal %s for %s (sha256:%s)\n", prop.ID, bundle.Target.SourceID, prop.SHA256[:12])
		for _, ln := range bundle.SummaryLines {
			fmt.Printf("  %s\n", ln)
		}
		fmt.Printf("  Approve with: %s approve %s --sha256=%s\n", displayHQCmd(rootDir), prop.ID, prop.SHA256[:12])

	case "apply-trust":
		denyIfCallerTainted(vaultDir, "extension apply-trust")
		fs := flag.NewFlagSet("extension apply-trust", flag.ExitOnError)
		payloadFile := fs.String("payload-file", "", "Path or /dev/fd/3 pipe containing TrustBundle JSON")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*payloadFile) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq extension apply-trust --payload-file=/dev/fd/3")
			os.Exit(1)
		}
		raw, err := os.ReadFile(strings.TrimSpace(*payloadFile))
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq extension apply-trust read failed: %v\n", err)
			os.Exit(1)
		}
		if err := extension.ApplyTrustBundle(vaultDir, raw); err != nil {
			fmt.Fprintf(os.Stderr, "hq extension apply-trust failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Println("Applied SHA-256 trust bundle to private/trust.json")

	case "doctor":
		fs := flag.NewFlagSet("extension doctor", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		items := extension.DoctorPacks(vaultDir)
		hasFail := false
		for _, it := range items {
			if it.Status == "FAIL" {
				hasFail = true
			}
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(items)
			if hasFail {
				os.Exit(1)
			}
			return
		}
		for _, it := range items {
			fmt.Printf("[%s] %-28s %s\n", it.Status, it.Name, it.Detail)
		}
		if hasFail {
			os.Exit(1)
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown extension subcommand %q (expected list, lint, enable, disable, trust, apply-trust, doctor)\n", sub)
		os.Exit(1)
	}
}

func runConnectorCLI(vaultDir string, args []string) {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}

	switch sub {
	case "list":
		fs := flag.NewFlagSet("connector list", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		conns, err := connector.ListConnectors(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq connector list failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(conns)
			return
		}
		for _, rc := range conns {
			fmt.Printf("- %-18s fqid=%-22s source=%-14s enabled=%-5v accounts=%d\n",
				rc.ID, rc.FQID, rc.SourceID, rc.Instance.IsEnabled(), len(rc.Instance.Accounts))
		}

	case "lint":
		pos, fl := splitPositionalAndFlags(rest)
		fs := flag.NewFlagSet("connector lint", flag.ExitOnError)
		sourceID := fs.String("source", "vault", "Manifest origin (vault or pack:<id>)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(fl)
		if len(pos) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq connector lint <connector.json> [--source=vault|pack:<id>] [--json]")
			os.Exit(1)
		}
		raw, err := os.ReadFile(pos[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq connector lint read failed: %v\n", err)
			os.Exit(1)
		}
		m, lErr := connector.LintManifest(raw, *sourceID)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			out := map[string]interface{}{"ok": lErr == nil, "id": m.ID}
			if lErr != nil {
				out["error"] = lErr.Error()
			}
			_ = enc.Encode(out)
			if lErr != nil {
				os.Exit(1)
			}
			return
		}
		if lErr != nil {
			fmt.Fprintf(os.Stderr, "hq connector lint FAILED (%s): %v\n", pos[0], lErr)
			os.Exit(1)
		}
		fmt.Printf("Connector manifest lint PASSED: %s (schema v%d)\n", m.ID, m.SchemaVersion)

	case "migrate":
		denyIfCallerTainted(vaultDir, "connector migrate")
		pos, fl := splitPositionalAndFlags(rest)
		fs := flag.NewFlagSet("connector migrate", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(fl)
		target := "all"
		if len(pos) > 0 && strings.TrimSpace(pos[0]) != "" {
			target = pos[0]
		}
		migrated, err := connector.MigrateLegacyAccounts(context.Background(), vaultDir, target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq connector migrate failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{"target": target, "migrated": migrated})
			return
		}
		if len(migrated) == 0 {
			fmt.Println("No legacy connector accounts found to migrate.")
			return
		}
		for _, m := range migrated {
			fmt.Printf("Migrated %s\n", m)
		}

	case "doctor":
		pos, fl := splitPositionalAndFlags(rest)
		fs := flag.NewFlagSet("connector doctor", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(fl)
		filter := ""
		if len(pos) > 0 {
			filter = pos[0]
		}
		items := connector.DoctorConnectors(vaultDir, filter)
		hasFail := false
		for _, it := range items {
			if it.Status == "FAIL" {
				hasFail = true
			}
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(items)
			if hasFail {
				os.Exit(1)
			}
			return
		}
		for _, it := range items {
			fmt.Printf("[%s] %-26s %s\n", it.Status, it.Name, it.Detail)
		}
		if hasFail {
			os.Exit(1)
		}

	case "test":
		pos, fl := splitPositionalAndFlags(rest)
		fs := flag.NewFlagSet("connector test", flag.ExitOnError)
		fixtures := fs.String("fixtures", "", "Directory containing poll.stdout / poll.json / poll.ndjson")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(fl)
		if len(pos) == 0 || strings.TrimSpace(*fixtures) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq connector test <connector-id> --fixtures=<dir> [--json]")
			os.Exit(1)
		}
		items, err := connector.TestConnectorFixture(vaultDir, pos[0], *fixtures)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq connector test failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(items)
			return
		}
		fmt.Printf("Connector fixture test PASSED (%s): mapped %d NormalizedItem(s)\n", pos[0], len(items))
		for _, it := range items {
			fmt.Printf("  - item=%s thread=%s sender=%s\n", it.ItemID, it.ThreadKey, it.Sender)
		}

	case "apply-offer":
		denyIfCallerTainted(vaultDir, "connector apply-offer")
		fs := flag.NewFlagSet("connector apply-offer", flag.ExitOnError)
		payloadFile := fs.String("payload-file", "", "Path or /dev/fd/3 pipe containing OfferApplyEnvelope JSON")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*payloadFile) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq connector apply-offer --payload-file=/dev/fd/3")
			os.Exit(1)
		}
		raw, err := os.ReadFile(strings.TrimSpace(*payloadFile))
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq connector apply-offer read failed: %v\n", err)
			os.Exit(1)
		}
		allowDesktop := connector.HasActiveDesktopPluginScope(vaultDir)
		out, err := connector.ApplyOfferPayload(context.Background(), vaultDir, raw, allowDesktop)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq connector apply-offer failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(out)

	default:
		fmt.Fprintf(os.Stderr, "unknown connector subcommand %q (expected list, lint, migrate, doctor, test, apply-offer)\n", sub)
		os.Exit(1)
	}
}

func runConnectorDriverCLI(vaultDir string, args []string) {
	if len(args) == 0 || args[0] != "jsonmap" {
		fmt.Fprintln(os.Stderr, "usage: hq connector-driver jsonmap")
		os.Exit(1)
	}
	if err := connector.RunJSONMapDriverStdio(context.Background(), vaultDir, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "hq connector-driver jsonmap failed: %v\n", err)
		os.Exit(1)
	}
}

func runWatchCLI(vaultDir string, args []string) {
	sub := "precheck"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}

	switch sub {
	case "precheck":
		denyIfCallerTainted(vaultDir, "watch precheck")
		fs := flag.NewFlagSet("watch precheck", flag.ExitOnError)
		connFlag := fs.String("connector", "", "Optional connector ID filter")
		acctFlag := fs.String("account", "", "Optional account alias filter")
		desktopOnly := fs.Bool("desktop-only", false, "Poll only connectors with requires_desktop_session=true")
		dryRun := fs.Bool("dry-run", false, "Poll without writing watcher state or enqueuing events")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		allowDesktop := *desktopOnly || connector.HasActiveDesktopPluginScope(vaultDir)
		sum, err := watcher.RunPrecheck(context.Background(), vaultDir, watcher.PrecheckOptions{
			ConnectorFilter:   *connFlag,
			AccountFilter:     *acctFlag,
			DesktopOnly:       *desktopOnly,
			AllowDesktopScope: allowDesktop,
			DryRun:            *dryRun,
			Now:               time.Now(),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq watch precheck failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(sum)
			return
		}
		if sum.EnqueuedTotal == 0 && sum.RollupTotal == 0 {
			// Exit 0 with empty stdout when 0 new items so gatekeepers stay silent when idle unless status printed?
			// Print concise human summary only when non-empty or when errors/pauses occurred:
			for _, r := range sum.Results {
				if r.Status != "ok" {
					fmt.Fprintf(os.Stderr, "watch %s:%s -> %s\n", r.Connector, r.Account, r.Status)
				}
			}
			return
		}
		fmt.Printf("Watcher precheck: enqueued=%d, briefRollups=%d\n", sum.EnqueuedTotal, sum.RollupTotal)

	case "read":
		fs := flag.NewFlagSet("watch read", flag.ExitOnError)
		connFlag := fs.String("connector", "", "Connector ID")
		acctFlag := fs.String("account", "", "Optional account alias")
		threadFlag := fs.String("thread", "", "Thread or item key")
		itemFlag := fs.String("item", "", "Alias for --thread")
		spawnTok := fs.String("spawn-token", "", "Optional single-use [hq-spawn:<token>]")
		convFlag := fs.String("conv", "", "Test-only conversation ID")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		key := strings.TrimSpace(*threadFlag)
		if key == "" {
			key = strings.TrimSpace(*itemFlag)
		}
		if strings.TrimSpace(*connFlag) == "" || key == "" {
			fmt.Fprintln(os.Stderr, "usage: hq watch read --connector=<id> [--account=<acct>] --thread=<threadOrItemKey> [--spawn-token=...]")
			os.Exit(1)
		}
		fenced, qt, err := watcher.ReadItem(context.Background(), vaultDir, *connFlag, *acctFlag, key, *spawnTok, *convFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq watch read failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{
				"fenced": fenced,
				"thread": qt,
			})
			return
		}
		fmt.Print(fenced)

	case "query":
		var kvFlags repeatFlag
		fs := flag.NewFlagSet("watch query", flag.ExitOnError)
		connFlag := fs.String("connector", "", "Connector ID")
		acctFlag := fs.String("account", "", "Optional account alias")
		queryName := fs.String("query", "", "Declared query name (e.g. freebusy)")
		startFlag := fs.String("start", "", "Optional start parameter")
		endFlag := fs.String("end", "", "Optional end parameter")
		fs.Var(&kvFlags, "param", "Repeatable key=value parameter")
		convFlag := fs.String("conv", "", "Test-only conversation ID")
		asJSON := fs.Bool("json", true, "Output JSON")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*connFlag) == "" || strings.TrimSpace(*queryName) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq watch query --connector=<id> [--account=<acct>] --query=<name> [--start=...] [--end=...] [--param=k=v]")
			os.Exit(1)
		}
		params := map[string]string{}
		if *startFlag != "" {
			params["start"] = *startFlag
		}
		if *endFlag != "" {
			params["end"] = *endFlag
		}
		for _, kv := range kvFlags {
			if k, v, ok := strings.Cut(kv, "="); ok && strings.TrimSpace(k) != "" {
				params[strings.TrimSpace(k)] = v
			}
		}
		data, err := watcher.Query(context.Background(), vaultDir, *connFlag, *acctFlag, *queryName, params, *convFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq watch query failed: %v\n", err)
			os.Exit(1)
		}
		_ = asJSON
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(data)

	case "ack":
		var items repeatFlag
		pos, fl := splitPositionalAndFlags(rest)
		fs := flag.NewFlagSet("watch ack", flag.ExitOnError)
		fs.Var(&items, "item", "Item or thread ID(s) to acknowledge (repeatable or comma-separated)")
		ackAll := fs.Bool("all", false, "Acknowledge all leased watcher events bound to the caller conversation")
		convFlag := fs.String("conv", "", "Test-only conversation ID")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(fl)
		var allIDs []string
		if !*ackAll {
			for _, raw := range append([]string(items), pos...) {
				for _, part := range strings.Split(raw, ",") {
					if trimmed := strings.TrimSpace(part); trimmed != "" {
						allIDs = append(allIDs, trimmed)
					}
				}
			}
		}
		n, err := watcher.Ack(context.Background(), vaultDir, allIDs, *convFlag, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq watch ack failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{"acked": n, "items": allIDs})
			return
		}
		fmt.Printf("Acknowledged %d watcher event(s)\n", n)

	case "reset-poisoned":
		denyIfCallerTainted(vaultDir, "watch reset-poisoned")
		fs := flag.NewFlagSet("watch reset-poisoned", flag.ExitOnError)
		connFlag := fs.String("connector", "", "Optional connector ID filter")
		acctFlag := fs.String("account", "", "Optional account alias filter")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		clearedItems, clearedEvents, err := watcher.ResetPoisoned(context.Background(), vaultDir, *connFlag, *acctFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq watch reset-poisoned failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{
				"cleared_items":  clearedItems,
				"cleared_events": clearedEvents,
			})
			return
		}
		fmt.Printf("Cleared %d poisoned watcher item(s) and %d dropped archive event(s).\n", clearedItems, clearedEvents)

	default:
		fmt.Fprintf(os.Stderr, "unknown watch subcommand %q (expected precheck, read, query, ack, reset-poisoned)\n", sub)
		os.Exit(1)
	}
}

func runOfferCLI(vaultDir string, args []string) {
	sub := "stage"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	if sub != "stage" {
		fmt.Fprintf(os.Stderr, "unknown offer subcommand %q (expected stage)\n", sub)
		os.Exit(1)
	}

	var kvFlags repeatFlag
	fs := flag.NewFlagSet("offer stage", flag.ExitOnError)
	connFlag := fs.String("connector", "", "Connector ID")
	acctFlag := fs.String("account", "", "Optional account alias")
	actionFlag := fs.String("action", "reply", "Declared offer action (e.g. reply, send)")
	threadFlag := fs.String("thread", "", "Thread key")
	toFlag := fs.String("to", "", "Optional To recipient(s)")
	ccFlag := fs.String("cc", "", "Optional Cc recipient(s)")
	subjFlag := fs.String("subject", "", "Optional subject")
	spaceFlag := fs.String("space", "", "Optional chat space/channel")
	fs.Var(&kvFlags, "param", "Repeatable key=value offer parameter")
	bodyFlag := fs.String("body", "", "Plain-text message body")
	summaryFlag := fs.String("summary", "", "Human-readable proposal summary")
	idemFlag := fs.String("idempotency-key", "", "Optional idempotency key")
	supersedeFlag := fs.Bool("supersede", true, "Supersede prior pending offers on the same thread")
	skillFlag := fs.String("skill", "inbox-triage", "Originating skill")
	convFlag := fs.String("conv", "", "Test-only conversation ID")
	asJSON := fs.Bool("json", false, "Output JSON")
	_ = fs.Parse(rest)

	params := map[string]string{}
	if *toFlag != "" {
		params["to"] = *toFlag
	}
	if *ccFlag != "" {
		params["cc"] = *ccFlag
	}
	if *subjFlag != "" {
		params["subject"] = *subjFlag
	}
	if *spaceFlag != "" {
		params["space"] = *spaceFlag
	}
	for _, kv := range kvFlags {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.TrimSpace(k) != "" {
			params[strings.TrimSpace(k)] = v
		}
	}
	sup := *supersedeFlag
	prop, err := offer.StageOffer(context.Background(), vaultDir, offer.StageRequest{
		Connector:      *connFlag,
		Account:        *acctFlag,
		Action:         *actionFlag,
		ThreadKey:      *threadFlag,
		Params:         params,
		Body:           *bodyFlag,
		Summary:        *summaryFlag,
		IdempotencyKey: *idemFlag,
		SupersedePrior: &sup,
		OriginSkill:    *skillFlag,
		FlagConv:       *convFlag,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hq offer stage failed: %v\n", err)
		os.Exit(1)
	}
	_ = webui.SyncMarkdownConsole(vaultDir)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(prop)
		return
	}
	fmt.Printf("Staged offer proposal %s [connector=%s, thread=%s, sha256=%s]\n",
		prop.ID, prop.TargetConnector, prop.TargetThreadKey, prop.SHA256[:12])
}

func runBriefCLI(vaultDir string, args []string) {
	sub := "collect"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}

	switch sub {
	case "collect":
		denyIfCallerTainted(vaultDir, "brief collect")
		fs := flag.NewFlagSet("brief collect", flag.ExitOnError)
		lease := fs.Bool("lease", false, "Acquire non-destructive caller-bound lease over overnight brief_rollup items")
		ttlStr := fs.String("ttl", "20m", "Lease TTL duration when --lease is set")
		convFlag := fs.String("conv", "", "Test-only conversation ID")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		ttl, err := time.ParseDuration(*ttlStr)
		if err != nil || ttl <= 0 {
			ttl = 20 * time.Minute
		}
		pkt, err := brief.Collect(context.Background(), vaultDir, brief.CollectOptions{
			Lease:    *lease,
			TTL:      ttl,
			FlagConv: *convFlag,
			Now:      time.Now(),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq brief collect failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(pkt)
			return
		}
		fmt.Printf("Morning Brief Packet [mode=%s, outbox=%d, proposals=%d, connectors=%d, extensions=%d, unurged=%d]\n",
			pkt.Mode, len(pkt.OutboxItems), len(pkt.Proposals), len(pkt.Connectors), len(pkt.Extensions), len(pkt.UnurgedQuarantinedIDs))
		if pkt.LeaseToken != "" {
			fmt.Printf("  LeaseToken: %s (expires %s)\n", pkt.LeaseToken, pkt.LeaseExpiresAt)
		}

	case "commit":
		denyIfCallerTainted(vaultDir, "brief commit")
		fs := flag.NewFlagSet("brief commit", flag.ExitOnError)
		leaseTok := fs.String("lease", "", "Active blease_<hex> token from 'hq brief collect --lease'")
		briefFile := fs.String("brief-file", "", "Path to saved brief markdown inside <vault>/notes/")
		convFlag := fs.String("conv", "", "Test-only conversation ID")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		res, err := brief.Commit(context.Background(), vaultDir, *leaseTok, *briefFile, *convFlag, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq brief commit failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(res)
			return
		}
		fmt.Printf("Committed brief lease (%d outbox item(s) marked delivered; brief=%s)\n", res.CommittedCount, res.BriefFile)

	case "format-quarantined":
		fs := flag.NewFlagSet("brief format-quarantined", flag.ExitOnError)
		jsonIn := fs.String("json-input", "", "Inline JSON array of QuarantinedDigestEntry")
		fileIn := fs.String("file", "", "Optional JSON file path")
		_ = fs.Parse(rest)
		var raw []byte
		var err error
		if strings.TrimSpace(*jsonIn) != "" {
			raw = []byte(*jsonIn)
		} else if strings.TrimSpace(*fileIn) != "" {
			raw, err = os.ReadFile(*fileIn)
		} else {
			raw, err = io.ReadAll(io.LimitReader(os.Stdin, 256*1024))
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq brief format-quarantined read failed: %v\n", err)
			os.Exit(1)
		}
		fenced, err := brief.FormatQuarantinedSummaries(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq brief format-quarantined failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(fenced)

	default:
		fmt.Fprintf(os.Stderr, "unknown brief subcommand %q (expected collect, commit, format-quarantined)\n", sub)
		os.Exit(1)
	}
}

func runSidecarCLI(vaultDir string, args []string) {
	sub := "console"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	if sub != "console" {
		fmt.Fprintf(os.Stderr, "unknown sidecar subcommand %q (expected console)\n", sub)
		os.Exit(1)
	}
	denyIfCallerTainted(vaultDir, "sidecar console")
	fs := flag.NewFlagSet("sidecar console", flag.ExitOnError)
	listenAddr := fs.String("listen", "", "Optional listen address override (defaults to 127.0.0.1:$ANTIGRAVITY_SIDECAR_WEB_PORT)")
	targetAddr := fs.String("target", "", "Optional daemon target address override (defaults to $HQ_DAEMON_ADDR or 127.0.0.1:4242)")
	supervise := fs.Bool("supervise", true, "Automatically start and supervise local hq daemon")
	_ = fs.Parse(rest)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := extension.RunConsoleSidecar(ctx, extension.ConsoleSidecarOptions{
		ListenAddr:      *listenAddr,
		TargetAddr:      *targetAddr,
		VaultDir:        vaultDir,
		SuperviseDaemon: *supervise,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "hq sidecar console failed: %v\n", err)
		os.Exit(1)
	}
}
