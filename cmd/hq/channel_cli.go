package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/channels"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/webui"
)

func requireCLIDaemonAuth(vaultDir, verb, tokenFile, jsonToken string) {
	if strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")) != "" {
		fmt.Fprintf(os.Stderr, "hq channel %s refused: daemon/sidecar-only gateway cannot be invoked inside an agent session\n", verb)
		os.Exit(1)
	}
	candidate := strings.TrimSpace(jsonToken)
	if candidate == "" && strings.TrimSpace(tokenFile) != "" {
		if b, err := os.ReadFile(strings.TrimSpace(tokenFile)); err == nil {
			candidate = strings.TrimSpace(string(b))
		}
	}
	if candidate == "" {
		candidate = strings.TrimSpace(os.Getenv("HQ_DAEMON_TOKEN"))
	}
	if !events.ValidateDaemonToken(vaultDir, candidate) {
		fmt.Fprintf(os.Stderr, "hq channel %s unauthorized: valid daemon_token required (pass --daemon-token-file, HQ_DAEMON_TOKEN, or daemon_token JSON field)\n", verb)
		os.Exit(1)
	}
}

func runChannelCLI(rootDir, vaultDir string, args []string) {
	sub, rest := splitSubcommand(args, "list")
	// Defense-in-depth: block all `hq channel` subcommands inside tainted/quarantined sessions.
	denyIfCallerTainted(vaultDir, "channel "+sub)

	switch sub {
	case "list":
		fs := flag.NewFlagSet("channel list", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		rows, err := channels.ListBridgeSummaries(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel list failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(rows)
			return
		}
		if len(rows) == 0 {
			fmt.Println("No companion channel bridges discovered.")
			return
		}
		for _, r := range rows {
			dest := r.DestSHA256Short
			if dest == "" {
				dest = "-"
			}
			fmt.Printf("- %-30s enabled=%-5v trusted=%-5v configured=%-5v health=%-16s dest=%s queued=%d\n",
				r.BridgeID, r.Enabled, r.Trusted, r.Configured, r.HealthStatus, dest, r.QueuedCount)
		}

	case "status":
		pos, fl := splitPositionalAndFlags(rest)
		fs := flag.NewFlagSet("channel status", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(fl)
		filter := ""
		if len(pos) > 0 {
			filter = pos[0]
		}
		res, err := channels.ProbeStatus(context.Background(), vaultDir, filter, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel status failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(res)
			return
		}
		if len(res) == 0 {
			fmt.Println("No companion channel bridges discovered.")
			return
		}
		for _, r := range res {
			if r.Error != "" {
				fmt.Printf("[FAIL] %-28s error=%s\n", r.BridgeID, r.Error)
				continue
			}
			fmt.Printf("[OK]   %-28s healthy=%-5v auth=%-14s bound=%q expected=%q drift=%v selfHealed=%v\n",
				r.BridgeID, r.Healthy, r.AuthState, r.InboundTunnelBoundCID, r.ExpectedDeskCID, r.DriftDetected, r.SelfHealed)
		}

	case "test":
		pos, fl := splitPositionalAndFlags(rest)
		fs := flag.NewFlagSet("channel test", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(fl)
		if len(pos) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq channel test <pack/bridge-id> [--json]")
			os.Exit(1)
		}
		res, err := channels.SendTestPing(context.Background(), vaultDir, pos[0], time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel test failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(res)
			return
		}
		fmt.Printf("Channel bridge test PASSED for %s (dest:%s · deliveryId:%s · latency:%dms)\n",
			res.BridgeID, res.DestSHA256Short, res.DeliveryID, res.LatencyMs)

	case "pause":
		pos, fl := splitPositionalAndFlags(rest)
		fs := flag.NewFlagSet("channel pause", flag.ExitOnError)
		reason := fs.String("reason", "", "Optional pause reason")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(fl)
		if len(pos) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq channel pause <pack/bridge-id> [--reason=...] [--json]")
			os.Exit(1)
		}
		if _, _, err := channels.ParseBridgeFQID(pos[0]); err != nil {
			fmt.Fprintf(os.Stderr, "hq channel pause failed: %v\n", err)
			os.Exit(1)
		}
		ov, err := extension.SetChannelBridgePaused(context.Background(), vaultDir, pos[0], true, *reason, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel pause failed: %v\n", err)
			os.Exit(1)
		}
		channels.InvalidateSyncMemo()
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			printJSON(ov)
			return
		}
		fmt.Printf("Paused channel bridge %s\n", pos[0])

	case "resume":
		pos, fl := splitPositionalAndFlags(rest)
		fs := flag.NewFlagSet("channel resume", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(fl)
		if len(pos) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq channel resume <pack/bridge-id> [--json]")
			os.Exit(1)
		}
		if _, _, err := channels.ParseBridgeFQID(pos[0]); err != nil {
			fmt.Fprintf(os.Stderr, "hq channel resume failed: %v\n", err)
			os.Exit(1)
		}
		ov, err := extension.SetChannelBridgePaused(context.Background(), vaultDir, pos[0], false, "", time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel resume failed: %v\n", err)
			os.Exit(1)
		}
		channels.InvalidateSyncMemo()
		_, _ = channels.SyncAndFlush(context.Background(), vaultDir, time.Now(), false)
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			printJSON(ov)
			return
		}
		fmt.Printf("Resumed channel bridge %s\n", pos[0])

	case "audit":
		fs := flag.NewFlagSet("channel audit", flag.ExitOnError)
		limit := fs.Int("limit", 20, "Maximum recent SHA-256 audit rows to display")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		entries, err := channels.ReadRecentAuditEntries(vaultDir, *limit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel audit failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(entries)
			return
		}
		if len(entries) == 0 {
			fmt.Println("No channel audit entries recorded yet.")
			return
		}
		for _, e := range entries {
			fmt.Printf("%s  %-26s op=%-9s kind=%-15s outcome=%-16s dest=%-12s sha256=%.12s\n",
				e.TS, e.BridgeID, e.Op, e.Kind, e.Outcome, e.DestSHA256, e.PayloadSHA256)
		}

	case "configure":
		pos, fl := splitPositionalAndFlags(rest)
		fs := flag.NewFlagSet("channel configure", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(fl)
		if len(pos) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq channel configure <pack/bridge-id> < config.json")
			os.Exit(1)
		}
		fqid := strings.TrimSpace(pos[0])
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 128*1024))
		if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
			fmt.Fprintln(os.Stderr, "hq channel configure requires private config JSON on stdin")
			os.Exit(1)
		}
		loaded, vErr := channels.ValidatePrivateConfigBytes(fqid, raw, 0)
		if vErr != nil {
			fmt.Fprintf(os.Stderr, "hq channel configure validation failed: %v\n", vErr)
			os.Exit(1)
		}
		convID := vault.ResolveConversationID()
		if convID == "" {
			saved, sErr := channels.SaveBridgePrivateConfig(vaultDir, fqid, raw)
			if sErr != nil {
				fmt.Fprintf(os.Stderr, "hq channel configure save failed: %v\n", sErr)
				os.Exit(1)
			}
			channels.InvalidateSyncMemo()
			_, _ = channels.SyncAndFlush(context.Background(), vaultDir, time.Now(), false)
			if *asJSON {
				printJSON(map[string]interface{}{
					"ok":              true,
					"bridgeId":        fqid,
					"destSha256Short": saved.DestSHA256Short,
				})
				return
			}
			fmt.Printf("Saved private channel config for %s (dest sha256:%s, mode 0600)\n", fqid, saved.DestSHA256Short)
			return
		}
		hqBin := resolveProposalHQBin(rootDir)
		argv := []string{hqBin, "channel", "apply-config", "--bridge=" + fqid, "--payload-file={{payload_file}}"}
		prop, pErr := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
			ActionType:        "rsi_apply",
			Summary:           fmt.Sprintf("Configure channel bridge %s (dest:%s)", fqid, loaded.DestSHA256Short),
			Command:           strings.Join(argv, " "),
			CommandArgv:       argv,
			PayloadBytes:      raw,
			WorkDir:           resolveDefaultWorkDir(rootDir),
			ConversationID:    convID,
			SupersedeConvType: true,
		})
		dieIfErr("hq channel configure proposal failed", pErr)
		if *asJSON {
			printJSON(prop)
			return
		}
		fmt.Printf("Staged channel config proposal %s for %s (sha256:%s)\n", prop.ID, fqid, shortPrefix(prop.SHA256, 12))

	case "apply-config":
		if strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")) != "" {
			fmt.Fprintln(os.Stderr, "hq channel apply-config refused: cannot execute directly inside an agent session (use 'hq channel configure' + human approval)")
			os.Exit(1)
		}
		fs := flag.NewFlagSet("channel apply-config", flag.ExitOnError)
		bridgeFlag := fs.String("bridge", "", "Channel bridge FQID (<pack>/<id>)")
		payloadFile := fs.String("payload-file", "", "Path or /dev/fd/3 pipe containing private channel config JSON")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*bridgeFlag) == "" || strings.TrimSpace(*payloadFile) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq channel apply-config --bridge=<pack/id> --payload-file=/dev/fd/3")
			os.Exit(1)
		}
		raw, err := os.ReadFile(strings.TrimSpace(*payloadFile))
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel apply-config read failed: %v\n", err)
			os.Exit(1)
		}
		saved, err := channels.SaveBridgePrivateConfig(vaultDir, strings.TrimSpace(*bridgeFlag), raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel apply-config failed: %v\n", err)
			os.Exit(1)
		}
		channels.InvalidateSyncMemo()
		_, _ = channels.SyncAndFlush(context.Background(), vaultDir, time.Now(), false)
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Applied private channel config for %s (dest:%s)\n", strings.TrimSpace(*bridgeFlag), saved.DestSHA256Short)

	case "inbound":
		fs := flag.NewFlagSet("channel inbound", flag.ExitOnError)
		tokFile := fs.String("daemon-token-file", "", "Path to <vault>/private/daemon_token")
		_ = fs.Parse(rest)
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel inbound stdin error: %v\n", err)
			os.Exit(1)
		}
		var req channels.InboundRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			fmt.Fprintf(os.Stderr, "hq channel inbound JSON error: %v\n", err)
			os.Exit(1)
		}
		requireCLIDaemonAuth(vaultDir, "inbound", *tokFile, req.DaemonToken)
		res, err := channels.HandleInbound(context.Background(), vaultDir, req, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel inbound failed: %v\n", err)
			os.Exit(1)
		}
		channels.BestEffortFlushLoopback(vaultDir, 150*time.Millisecond)
		printJSON(res)

	case "callback":
		fs := flag.NewFlagSet("channel callback", flag.ExitOnError)
		tokFile := fs.String("daemon-token-file", "", "Path to <vault>/private/daemon_token")
		_ = fs.Parse(rest)
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 16*1024))
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel callback stdin error: %v\n", err)
			os.Exit(1)
		}
		var req channels.CallbackRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			fmt.Fprintf(os.Stderr, "hq channel callback JSON error: %v\n", err)
			os.Exit(1)
		}
		requireCLIDaemonAuth(vaultDir, "callback", *tokFile, req.DaemonToken)
		res, err := channels.HandleCallback(context.Background(), vaultDir, req, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel callback failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		printJSON(res)

	case "flush":
		fs := flag.NewFlagSet("channel flush", flag.ExitOnError)
		tokFile := fs.String("daemon-token-file", "", "Path to <vault>/private/daemon_token")
		_ = fs.Parse(rest)
		requireCLIDaemonAuth(vaultDir, "flush", *tokFile, "")
		channels.InvalidateSyncMemo()
		rep, err := channels.SyncAndFlush(context.Background(), vaultDir, time.Now(), false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel flush failed: %v\n", err)
			os.Exit(1)
		}
		printJSON(rep)

	default:
		fmt.Fprintf(os.Stderr, "unknown channel subcommand: %s (expected list, status, test, pause, resume, audit, configure)\n", sub)
		os.Exit(1)
	}
}
