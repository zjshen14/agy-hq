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
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/webui"
)

func runChannelCLI(rootDir, vaultDir string, args []string) {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}

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
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(rows)
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
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(res)
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
		denyIfCallerTainted(vaultDir, "channel test")
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
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(res)
			return
		}
		fmt.Printf("Channel bridge test PASSED for %s (dest:%s · deliveryId:%s · latency:%dms)\n",
			res.BridgeID, res.DestSHA256Short, res.DeliveryID, res.LatencyMs)

	case "pause":
		denyIfCallerTainted(vaultDir, "channel pause")
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
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(ov)
			return
		}
		fmt.Printf("Paused channel bridge %s\n", pos[0])

	case "resume":
		denyIfCallerTainted(vaultDir, "channel resume")
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
		_, _ = channels.SyncAndFlush(context.Background(), vaultDir, time.Now(), false)
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(ov)
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
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(entries)
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
		denyIfCallerTainted(vaultDir, "channel configure")
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
			_, _ = channels.SyncAndFlush(context.Background(), vaultDir, time.Now(), false)
			if *asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				_ = enc.Encode(map[string]interface{}{
					"ok":              true,
					"bridgeId":        fqid,
					"destSha256Short": saved.DestSHA256Short,
				})
				return
			}
			fmt.Printf("Saved private channel config for %s (dest sha256:%s, mode 0600)\n", fqid, saved.DestSHA256Short)
			return
		}
		// Inside an agent conversation: stage an approval-gated `/dev/fd/3` proposal (`rsi_apply`).
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
		if pErr != nil {
			fmt.Fprintf(os.Stderr, "hq channel configure proposal failed: %v\n", pErr)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(prop)
			return
		}
		fmt.Printf("Staged channel config proposal %s for %s (sha256:%s)\n", prop.ID, fqid, prop.SHA256[:12])

	case "apply-config":
		denyIfCallerTainted(vaultDir, "channel apply-config")
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
		_, _ = channels.SyncAndFlush(context.Background(), vaultDir, time.Now(), false)
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Applied private channel config for %s (dest:%s)\n", strings.TrimSpace(*bridgeFlag), saved.DestSHA256Short)

	case "inbound":
		if strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")) != "" {
			fmt.Fprintln(os.Stderr, "hq channel inbound refused: daemon/sidecar-only gateway cannot be invoked inside an agent session")
			os.Exit(1)
		}
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
		res, err := channels.HandleInbound(context.Background(), vaultDir, req, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel inbound failed: %v\n", err)
			os.Exit(1)
		}
		channels.BestEffortFlushLoopback(vaultDir, 150*time.Millisecond)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)

	case "callback":
		if strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")) != "" {
			fmt.Fprintln(os.Stderr, "hq channel callback refused: daemon/sidecar-only gateway cannot be invoked inside an agent session")
			os.Exit(1)
		}
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
		res, err := channels.HandleCallback(context.Background(), vaultDir, req, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel callback failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)

	case "flush":
		if strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")) != "" {
			fmt.Fprintln(os.Stderr, "hq channel flush refused inside agent session")
			os.Exit(1)
		}
		rep, err := channels.SyncAndFlush(context.Background(), vaultDir, time.Now(), false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq channel flush failed: %v\n", err)
			os.Exit(1)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)

	default:
		fmt.Fprintf(os.Stderr, "unknown channel subcommand: %s (expected list, status, test, pause, resume, audit, configure)\n", sub)
		os.Exit(1)
	}
}
