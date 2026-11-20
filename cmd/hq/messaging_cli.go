package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/gchat"
	"github.com/zjshen14/agy-hq/internal/gmail"
	"github.com/zjshen14/agy-hq/internal/rsi"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/webui"
)

func truncateBytePreview(s string, maxBytes int) string {
	if len(s) > maxBytes {
		return s[:maxBytes] + "..."
	}
	return s
}

func buildGChatDraftProposalData(hqBin string, spec gchat.DraftSpec) (summary, cmdStr string, cmdArgv []string, meta map[string]string) {
	hashShort := shortPrefix(spec.ContentHash, 12)
	summary = gchat.ProposalSummary(spec)
	cmdStr = fmt.Sprintf("%s gchat send-draft --account=%s --draft=%s --hash=%s", hqBin, spec.Account, spec.DraftID, hashShort)
	cmdArgv = []string{hqBin, "gchat", "send-draft", "--account=" + spec.Account, "--draft=" + spec.DraftID, "--hash=" + hashShort}
	meta = map[string]string{
		"draftId":      spec.DraftID,
		"account":      spec.Account,
		"from":         spec.From,
		"spaceId":      spec.SpaceID,
		"spaceName":    spec.SpaceName,
		"threadId":     spec.ThreadID,
		"bodyPreview":  truncateBytePreview(spec.Body, 500),
		"draftPayload": spec.Body,
		"contentHash":  spec.ContentHash,
	}
	return summary, cmdStr, cmdArgv, meta
}

func buildGmailDraftProposalData(hqBin string, spec gmail.DraftSpec) (summary, cmdStr string, cmdArgv []string, meta map[string]string) {
	hashShort := shortPrefix(spec.ContentHash, 12)
	summary = fmt.Sprintf("Send Gmail Draft %s [From: %s -> To: %s | Subject: %s] (hash:%s)", spec.DraftID, spec.From, spec.To, spec.Subject, hashShort)
	cmdStr = fmt.Sprintf("%s gmail send-draft --account=%s --draft=%s --hash=%s", hqBin, spec.Account, spec.DraftID, hashShort)
	cmdArgv = []string{hqBin, "gmail", "send-draft", "--account=" + spec.Account, "--draft=" + spec.DraftID, "--hash=" + hashShort}
	meta = map[string]string{
		"draftId":      spec.DraftID,
		"account":      spec.Account,
		"from":         spec.From,
		"to":           spec.To,
		"cc":           spec.Cc,
		"subject":      spec.Subject,
		"bodyPreview":  truncateBytePreview(spec.Body, 500),
		"draftPayload": spec.Body,
		"contentHash":  spec.ContentHash,
	}
	return summary, cmdStr, cmdArgv, meta
}

func runProposeCLI(rootDir, vaultDir string, args []string) {
	denyIfCallerTainted(vaultDir, "propose")
	var proposeFlagArgs []string
	var cmdArgv []string
	for i, a := range args {
		if a == "--" {
			cmdArgv = args[i+1:]
			break
		}
		proposeFlagArgs = append(proposeFlagArgs, a)
	}
	fs := flag.NewFlagSet("propose", flag.ExitOnError)
	actionType := fs.String("type", "external_action", "Action category (e.g., gmail_send, gchat_send, rsi_patch, rsi_skill_patch, rsi_overlay)")
	summary := fs.String("summary", "", "Human-readable summary of the proposed action")
	cmdStr := fs.String("cmd", "", "Exact deterministic shell command to execute upon approval")
	draftID := fs.String("draft", "", "Optional Gmail or Google Chat draft ID when --type=gmail_send or --type=gchat_send")
	rsiID := fs.String("rsi-id", "", "Optional verified RSI hypothesis ID when --type=rsi_patch, rsi_skill_patch, or rsi_overlay")
	account := fs.String("account", "", "Optional account alias when --type=gmail_send or --type=gchat_send")
	workDir := fs.String("workdir", "", "Optional working directory for command execution")
	convID := fs.String("conv", "", "Optional conversation ID")
	ttlStr := fs.String("ttl", "", "Optional proposal expiration TTL duration (default 24h)")
	resumeOnDecide := fs.Bool("resume-on-decide", false, "Wake the originating conversation upon human approve or reject")
	continuation := fs.String("continuation", "", "Optional continuation instruction injected when resuming")
	originSkill := fs.String("skill", "", "Optional originating skill slug for signal-to-noise attribution")
	keepPrior := fs.Bool("keep-prior", false, "Retain older pending proposals of the same type in this conversation")
	_ = fs.Parse(proposeFlagArgs)
	if *convID == "" {
		*convID = vault.ResolveConversationID()
	}
	if *rsiID != "" {
		hqBin := resolveProposalHQBin(rootDir)
		prop, _, err := rsi.Propose(rootDir, vaultDir, *rsiID, *convID, hqBin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq propose --rsi-id=%s failed: %v\n", *rsiID, err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Staged proposal %s (sha256: %s)\n", prop.ID, prop.SHA256[:12])
		fmt.Printf("  Approve with: %s approve %s --sha256=%s\n", displayHQCmd(rootDir), prop.ID, prop.SHA256[:12])
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
		hqBin := resolveProposalHQBin(rootDir)
		var defSummary, defCmd string
		var defArgv []string
		var defSkill string
		if *actionType == "gchat_send" {
			spec, err := gchat.GetDraft(vaultDir, *account, *draftID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq propose: load gchat draft %s failed: %v\n", *draftID, err)
				os.Exit(1)
			}
			defSummary, defCmd, defArgv, meta = buildGChatDraftProposalData(hqBin, spec)
			defSkill = "gchat"
		} else {
			spec, err := gmail.GetDraft(vaultDir, *account, *draftID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq propose: load draft %s failed: %v\n", *draftID, err)
				os.Exit(1)
			}
			defSummary, defCmd, defArgv, meta = buildGmailDraftProposalData(hqBin, spec)
			defSkill = "gmail"
		}
		if *summary == "" {
			*summary = defSummary
		}
		if *cmdStr == "" && len(cmdArgv) == 0 {
			*cmdStr = defCmd
			cmdArgv = defArgv
		}
		if *originSkill == "" {
			*originSkill = defSkill
		}
		if *workDir == "" {
			*workDir = resolveDefaultWorkDir(rootDir)
		}
	}
	if (*cmdStr == "" && len(cmdArgv) == 0) || *summary == "" {
		fmt.Fprintln(os.Stderr, "usage: hq propose --type=<type> --summary=\"...\" [--keep-prior] [--resume-on-decide] [--continuation=\"...\"] --cmd=\"...\" (or -- <argv...> / --draft=<draftId>)")
		os.Exit(1)
	}
	if *draftID == "" && *rsiID == "" {
		pol, _, _ := approvals.LoadPolicy(vaultDir)
		if pol.RejectNoopCommands {
			if vErr := approvals.ValidateDeterministicCommand(*cmdStr, cmdArgv); vErr != nil {
				fmt.Fprintf(os.Stderr, "hq propose failed: %v\n", vErr)
				os.Exit(1)
			}
		}
	}
	if strings.TrimSpace(*ttlStr) != "" {
		if d, tErr := time.ParseDuration(strings.TrimSpace(*ttlStr)); tErr == nil {
			if meta == nil {
				meta = make(map[string]string)
			}
			meta["expiresAt"] = time.Now().Add(d).UTC().Format(time.RFC3339Nano)
		}
	}
	prop, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:        *actionType,
		Summary:           *summary,
		Command:           *cmdStr,
		CommandArgv:       cmdArgv,
		WorkDir:           *workDir,
		ConversationID:    *convID,
		SupersedeConvType: *draftID == "" && *rsiID == "" && !*keepPrior,
		KeepPrior:         *keepPrior,
		ResumeOnDecide:    *resumeOnDecide,
		ContinuationNote:  *continuation,
		OriginSkill:       *originSkill,
		Metadata:          meta,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hq propose failed: %v\n", err)
		os.Exit(1)
	}
	_ = webui.SyncMarkdownConsole(vaultDir)
	fmt.Printf("Staged proposal %s (sha256: %s)\n", prop.ID, prop.SHA256[:12])
	if len(prop.RiskFlags) > 0 {
		fmt.Printf("  ⚠️ Contextual PII Warning: %s\n", strings.Join(prop.RiskFlags, ", "))
	}
	fmt.Printf("  Approve with: %s approve %s --sha256=%s\n", displayHQCmd(rootDir), prop.ID, prop.SHA256[:12])
	fmt.Printf("  Next: Present a markdown blockquote preview + ask_question referencing %s (or approve in :4242 Web Console).\n", prop.ID)

}

func runApproveCLI(vaultDir string, args []string) {
	denyIfCallerTainted(vaultDir, "approve")
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: hq approve <proposal-id> [--sha256=<prefix>] [--cap=<token>]")
		os.Exit(1)
	}
	id := args[0]
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	shaPrefix := fs.String("sha256", "", "SHA-256 prefix verification")
	capTok := fs.String("cap", "", "One-shot human-presence capability token")
	_ = fs.Parse(args[1:])
	out, err := approvals.ApproveFromCLI(vaultDir, id, *shaPrefix, *capTok)
	_ = webui.SyncMarkdownConsole(vaultDir)
	if out != "" {
		fmt.Print(out)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "hq approve execution error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Approved and executed proposal %s\n", id)

}

func runRejectCLI(vaultDir string, args []string) {
	denyIfCallerTainted(vaultDir, "reject")
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: hq reject <proposal-id> [--reason=\"...\"] [--dismiss|--no-resume]")
		os.Exit(1)
	}
	var id string
	var rejectFlagArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			rejectFlagArgs = append(rejectFlagArgs, arg)
			if (arg == "--reason" || arg == "-reason") && i+1 < len(args) {
				i++
				rejectFlagArgs = append(rejectFlagArgs, args[i])
			}
		} else if id == "" {
			id = arg
		} else {
			rejectFlagArgs = append(rejectFlagArgs, arg)
		}
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: hq reject <proposal-id> [--reason=\"...\"] [--dismiss|--no-resume]")
		os.Exit(1)
	}
	fs := flag.NewFlagSet("reject", flag.ExitOnError)
	reason := fs.String("reason", "Rejected by user", "Reason for rejection")
	dismiss := fs.Bool("dismiss", false, "Suppress LLM resumption event even when a custom --reason is provided")
	noResume := fs.Bool("no-resume", false, "Alias for --dismiss: suppress LLM resumption event")
	_ = fs.Parse(rejectFlagArgs)
	if err := approvals.RejectProposalWithOptions(vaultDir, id, approvals.RejectOptions{
		Reason:         *reason,
		SuppressResume: *dismiss || *noResume,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "hq reject error: %v\n", err)
		os.Exit(1)
	}
	_ = webui.SyncMarkdownConsole(vaultDir)
	fmt.Printf("Rejected proposal %s\n", id)

}

func runGmailCLI(rootDir, vaultDir string, args []string) {
	if len(args) < 1 {
		printGmailUsage(os.Stderr)
		os.Exit(1)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "--help", "-h", "help":
		printGmailUsage(os.Stdout)
	case "init", "add-account":
		fs := flag.NewFlagSet("gmail "+sub, flag.ExitOnError)
		alias := fs.String("alias", "primary", "Account alias (e.g. primary, personal-2, shopping)")
		email := fs.String("email", "", "Gmail address (e.g. you@example.com)")
		agyAcct := fs.String("agy-account", "", "Optional AGY_ACCOUNT name if signed in via Antigravity plugin")
		tokenFile := fs.String("token-file", "", "Optional path to read-only OAuth2 token JSON for hq daemon sidecar")
		cliPath := fs.String("cli-path", "", "Optional explicit path to gmail CLI binary")
		installPlugin := fs.Bool("install-plugin", false, "Opt-in install of official Antigravity Gmail plugin via agy CLI")
		query := fs.String("query", gmail.DefaultPrimaryQuery, "Default unread search query for sidecar precheck")
		setDefault := fs.Bool("default", sub == "init", "Set this account as the default Gmail account")
		_ = fs.Parse(rest)
		cfg, err := gmail.InitOrAddAccountWithOptions(vaultDir, gmail.InitAccountOptions{
			Alias:         *alias,
			Email:         *email,
			AGYAccount:    *agyAcct,
			TokenFile:     *tokenFile,
			CLIPath:       *cliPath,
			Query:         *query,
			SetDefault:    *setDefault,
			InstallPlugin: *installPlugin,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gmail %s failed: %v\n", sub, err)
			os.Exit(1)
		}
		fmt.Printf("Configured Gmail account %q (%s) [token=%s]\n", cfg.Alias, cfg.Email, cfg.TokenFile)

	case "accounts", "status":
		reg, err := gmail.LoadRegistry(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gmail accounts failed: %v\n", err)
			os.Exit(1)
		}
		printJSON(reg)

	case "search", "list":
		denyIfCallerTainted(vaultDir, "gmail search")
		fs := flag.NewFlagSet("gmail search", flag.ExitOnError)
		account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
		query := fs.String("query", "", "Gmail search query (defaults to account's Primary unread query)")
		maxRes := fs.Int("max", 20, "Maximum messages to return")
		_ = fs.Parse(rest)
		msgs, err := gmail.SearchMessages(vaultDir, *account, *query, *maxRes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gmail search failed: %v\n", err)
			os.Exit(1)
		}
		printJSON(msgs)

	case "read-quarantined", "read":
		fs := flag.NewFlagSet("gmail read-quarantined", flag.ExitOnError)
		account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
		threadID := fs.String("thread", "", "Thread ID to read in quarantined mode")
		convID := fs.String("conv", "", "Optional conversation ID (defaults to ANTIGRAVITY_CONVERSATION_ID)")
		_ = fs.Parse(rest)
		_, formatted, err := gmail.ReadQuarantinedThread(vaultDir, *account, *threadID, *convID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gmail read-quarantined failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(formatted)

	case "draft":
		denyIfCallerTainted(vaultDir, "gmail draft")
		fs := flag.NewFlagSet("gmail draft", flag.ExitOnError)
		account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
		draftID := fs.String("draft", "", "Optional existing draft ID to update")
		threadID := fs.String("thread", "", "Optional thread ID being replied to")
		to := fs.String("to", "", "Recipient email address(es)")
		cc := fs.String("cc", "", "Optional CC email address(es)")
		subject := fs.String("subject", "", "Email subject line")
		body := fs.String("body", "", "Plain-text email body")
		bodyStdin := fs.Bool("body-stdin", false, "Read plain-text email body from stdin (avoids shell quote expansion)")
		bodyFile := fs.String("body-file", "", "Read plain-text email body from file path")
		convID := fs.String("conv", "", "Optional conversation ID")
		_ = fs.Parse(rest)
		resolvedBody, bErr := resolveBodyInput(*body, *bodyStdin, *bodyFile, os.Stdin)
		if bErr != nil {
			fmt.Fprintf(os.Stderr, "hq gmail draft failed: %v\n", bErr)
			os.Exit(1)
		}
		spec, err := gmail.CreateOrUpdateDraft(vaultDir, *account, *draftID, *threadID, *to, *cc, *subject, resolvedBody, *convID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gmail draft failed: %v\n", err)
			os.Exit(1)
		}
		printJSON(spec)

	case "send-draft", "send":
		denyIfCallerTainted(vaultDir, "gmail send-draft")
		fs := flag.NewFlagSet("gmail send-draft", flag.ExitOnError)
		account := fs.String("account", "", "Account alias or email")
		draftID := fs.String("draft", "", "Draft ID to verify and send")
		hashPrefix := fs.String("hash", "", "Expected contentHash prefix from approved proposal")
		_ = fs.Parse(rest)
		out, err := gmail.VerifyAndSendDraft(vaultDir, *account, *draftID, *hashPrefix)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gmail send-draft failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(out)

	case "precheck", "triage":
		fs := flag.NewFlagSet("gmail precheck", flag.ExitOnError)
		account := fs.String("account", "", "Optional account alias filter")
		_ = fs.Parse(rest)
		batches, status, err := gmail.RunPrecheck(vaultDir, *account)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gmail precheck failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(status)
		if len(batches) > 0 {
			printJSON(batches)
		}

	case "triage-result":
		fs := flag.NewFlagSet("gmail triage-result", flag.ExitOnError)
		batchID := fs.String("batch", "", "Batch ID being summarized")
		rawJSON := fs.String("json", "", "JSON array of TriageItem objects")
		_ = fs.Parse(rest)
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
		_ = fs.Parse(rest)
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

	case "forward":
		denyIfCallerTainted(vaultDir, "gmail forward")
		fs := flag.NewFlagSet("gmail forward", flag.ExitOnError)
		account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
		messageID := fs.String("message", "", "Source Gmail message ID to forward")
		to := fs.String("to", "", "Comma-separated recipient email address(es)")
		cc := fs.String("cc", "", "Optional comma-separated CC email address(es)")
		subject := fs.String("subject", "", "Optional subject override (defaults to Fwd: <original subject>)")
		note := fs.String("note", "", "Optional top note to prepend above the forwarded message")
		noteStdin := fs.Bool("note-stdin", false, "Read top note from stdin")
		noteFile := fs.String("note-file", "", "Read top note from file path")
		convID := fs.String("conv", "", "Optional conversation ID")
		resumeOnDecide := fs.Bool("resume-on-decide", false, "Wake the originating conversation upon human approve or reject")
		continuation := fs.String("continuation", "", "Optional continuation instruction injected when resuming")
		_ = fs.Parse(rest)
		resolvedNote, nErr := resolveBodyInput(*note, *noteStdin, *noteFile, os.Stdin)
		if nErr != nil {
			fmt.Fprintf(os.Stderr, "hq gmail forward failed: %v\n", nErr)
			os.Exit(1)
		}
		if *convID == "" {
			*convID = vault.ResolveConversationID()
		}
		plan, err := gmail.PrepareForward(vaultDir, gmail.ForwardOptions{
			Account:   *account,
			MessageID: *messageID,
			To:        *to,
			Cc:        *cc,
			Subject:   *subject,
			Note:      resolvedNote,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gmail forward failed: %v\n", err)
			os.Exit(1)
		}
		preview := resolvedNote
		if preview == "" {
			preview = "(no note; forwarding original message)"
		}
		if len(preview) > 500 {
			preview = preview[:500] + "..."
		}
		meta := map[string]string{
			"messageId":       plan.MessageID,
			"account":         plan.Account,
			"from":            plan.From,
			"to":              plan.To,
			"cc":              plan.Cc,
			"subject":         plan.Subject,
			"bodyFile":        plan.BodyFile,
			"forwardTempDir":  plan.TempDir,
			"bodyPreview":     preview,
			"attachmentCount": fmt.Sprintf("%d", len(plan.AttachmentFiles)),
		}
		prop, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
			ActionType:       "messaging",
			Summary:          plan.Summary,
			Command:          plan.Command,
			CommandArgv:      plan.CommandArgv,
			WorkDir:          resolveDefaultWorkDir(rootDir),
			ConversationID:   *convID,
			ResumeOnDecide:   *resumeOnDecide,
			ContinuationNote: *continuation,
			OriginSkill:      "gmail",
			Metadata:         meta,
		})
		if err != nil {
			_ = os.RemoveAll(plan.TempDir)
			fmt.Fprintf(os.Stderr, "hq gmail forward failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Staged Gmail forward proposal %s (message: %s, to: %s, attachments: %d, sha256: %s)\n",
			prop.ID, plan.MessageID, plan.To, len(plan.AttachmentFiles), prop.SHA256[:12])
		fmt.Printf("  Body file: %s\n", plan.BodyFile)
		for i, attPath := range plan.AttachmentFiles {
			fmt.Printf("  Attachment %d: %s\n", i+1, attPath)
		}
		fmt.Printf("  Approve with: %s approve %s --sha256=%s\n", displayHQCmd(rootDir), prop.ID, prop.SHA256[:12])

	default:
		fmt.Fprintf(os.Stderr, "unknown gmail subcommand: %s\n", sub)
		os.Exit(1)
	}

}

func runGChatCLI(rootDir, vaultDir string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: hq gchat <init|add-account|accounts|login|launch-chrome|spaces|search|read-quarantined|draft|propose|send-draft|precheck|triage-result> [args]")
		os.Exit(1)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "init", "add-account":
		fs := flag.NewFlagSet("gchat "+sub, flag.ExitOnError)
		alias := fs.String("alias", "personal", "Account alias (e.g. personal, work)")
		email := fs.String("email", "", "Google account address (e.g. you@gmail.com)")
		transport := fs.String("transport", "", "Transport adapter (cdp, gmail-bridge, oauth, webhook, mock; defaults to cdp for @gmail.com)")
		cdpEndpoint := fs.String("cdp-endpoint", gchat.DefaultCDPEndpoint, "Local Chrome DevTools Protocol HTTP endpoint")
		chromeProfile := fs.String("chrome-profile", "", "Optional path to isolated Chrome user-data-dir")
		gmailFallback := fs.String("gmail-fallback", "primary", "Gmail account alias for chat-noreply@google.com fallback")
		readTokenFile := fs.String("read-token-file", "", "Optional Workspace read-only OAuth2 token file")
		sendTokenFile := fs.String("send-token-file", "", "Optional Workspace chat.messages.create OAuth2 token file")
		webhookFile := fs.String("webhook-file", "", "Optional Space Incoming Webhook URL file")
		setDefault := fs.Bool("default", sub == "init", "Set this account as the default Google Chat account")
		_ = fs.Parse(rest)
		cfg, err := gchat.InitOrAddAccount(vaultDir, *alias, *email, *transport, *cdpEndpoint, *chromeProfile, *gmailFallback, *readTokenFile, *sendTokenFile, *webhookFile, *setDefault)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gchat %s failed: %v\n", sub, err)
			os.Exit(1)
		}
		_, _, _ = vault.AddStarterSkill(vaultDir, resolveTemplateDir(rootDir), "gchat", false)
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Configured Google Chat account %q (%s) [type=%s, transport=%s, fallback=gmail:%s]\n",
			cfg.Alias, cfg.Email, cfg.AccountType, cfg.Transport, cfg.GmailFallbackAlias)

	case "accounts", "status":
		reg, err := gchat.LoadRegistry(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gchat accounts failed: %v\n", err)
			os.Exit(1)
		}
		printJSON(reg)

	case "login":
		fs := flag.NewFlagSet("gchat login", flag.ExitOnError)
		account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
		reset := fs.Bool("reset", false, "Back up and clear existing session cookies to test a fresh first-time login")
		remote := fs.Bool("remote", false, "Run login in headless mode with automatic email entry, Google Prompt 2-digit readout, and PNG screenshot")
		screenshot := fs.String("screenshot", "", "Optional path to save live Google Sign-In challenge PNG screenshot")
		timeoutSec := fs.Int("timeout", 120, "Seconds to wait for Google Sign-In")
		_ = fs.Parse(rest)
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
		_, _, _ = vault.AddStarterSkill(vaultDir, resolveTemplateDir(rootDir), "gchat", false)
		spaces, _ := gchat.ListSpaces(vaultDir, *account, "", 50)
		fmt.Printf("Authenticated Google Chat session active in background headless mode (%s, %d space(s) cached)\n", ep, len(spaces))

	case "logout", "reset-session":
		fs := flag.NewFlagSet("gchat logout", flag.ExitOnError)
		account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
		_ = fs.Parse(rest)
		msg, err := gchat.LogoutOrResetProfile(vaultDir, *account)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gchat logout failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(msg)

	case "restore-session":
		fs := flag.NewFlagSet("gchat restore-session", flag.ExitOnError)
		account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
		_ = fs.Parse(rest)
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
		_ = fs.Parse(rest)
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
		_ = fs.Parse(rest)
		spaces, err := gchat.ListSpaces(vaultDir, *account, *query, *maxRes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gchat spaces failed: %v\n", err)
			os.Exit(1)
		}
		printJSON(spaces)

	case "search", "list":
		denyIfCallerTainted(vaultDir, "gchat search")
		fs := flag.NewFlagSet("gchat search", flag.ExitOnError)
		account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
		spaceID := fs.String("space", "", "Optional space name or ID filter (e.g. \"Project Launch Space\" or spaces/AAAA123)")
		query := fs.String("query", "", "Optional search query")
		maxRes := fs.Int("max", 20, "Maximum messages to return")
		_ = fs.Parse(rest)
		msgs, err := gchat.SearchMessages(vaultDir, *account, *spaceID, *query, *maxRes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gchat search failed: %v\n", err)
			os.Exit(1)
		}
		printJSON(msgs)

	case "read-quarantined", "read":
		fs := flag.NewFlagSet("gchat read-quarantined", flag.ExitOnError)
		account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
		spaceID := fs.String("space", "", "Space name or ID to read in quarantined mode (e.g. spaces/AAAA123)")
		threadID := fs.String("thread", "", "Optional thread ID filter")
		maxRes := fs.Int("max", 20, "Maximum messages to read")
		convID := fs.String("conv", "", "Optional conversation ID (defaults to ANTIGRAVITY_CONVERSATION_ID)")
		_ = fs.Parse(rest)
		_, formatted, err := gchat.ReadQuarantinedThread(vaultDir, *account, *spaceID, *threadID, *convID, *maxRes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gchat read-quarantined failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(formatted)

	case "draft":
		denyIfCallerTainted(vaultDir, "gchat draft")
		fs := flag.NewFlagSet("gchat draft", flag.ExitOnError)
		account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
		draftID := fs.String("draft", "", "Optional existing draft ID to update")
		toSpace := fs.String("to", "", "Target space name or ID (e.g. \"Project Launch Space\" or spaces/AAAA123)")
		spaceID := fs.String("space", "", "Alias for --to: target space name or ID")
		spaceName := fs.String("space-name", "", "Optional display name of the target space")
		threadID := fs.String("thread", "", "Optional thread ID being replied to")
		body := fs.String("body", "", "Plain-text message body")
		bodyStdin := fs.Bool("body-stdin", false, "Read plain-text message body from stdin (avoids shell quote expansion)")
		bodyFile := fs.String("body-file", "", "Read plain-text message body from file path")
		convID := fs.String("conv", "", "Optional conversation ID")
		_ = fs.Parse(rest)
		resolvedBody, bErr := resolveBodyInput(*body, *bodyStdin, *bodyFile, os.Stdin)
		if bErr != nil {
			fmt.Fprintf(os.Stderr, "hq gchat draft failed: %v\n", bErr)
			os.Exit(1)
		}
		target := strings.TrimSpace(*toSpace)
		if target == "" {
			target = strings.TrimSpace(*spaceID)
		}
		spec, err := gchat.CreateOrUpdateDraft(vaultDir, *account, *draftID, target, *spaceName, *threadID, resolvedBody, *convID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gchat draft failed: %v\n", err)
			os.Exit(1)
		}
		printJSON(spec)

	case "propose", "stage-send":
		denyIfCallerTainted(vaultDir, "gchat propose")
		fs := flag.NewFlagSet("gchat propose", flag.ExitOnError)
		account := fs.String("account", "", "Account alias or email (defaults to defaultAccount)")
		draftID := fs.String("draft", "", "Optional existing draft ID to update or propose")
		toSpace := fs.String("to", "", "Target space name or ID (e.g. \"Project Launch Space\" or spaces/AAAA123)")
		spaceID := fs.String("space", "", "Alias for --to: target space name or ID")
		spaceName := fs.String("space-name", "", "Optional display name of the target space")
		threadID := fs.String("thread", "", "Optional thread ID being replied to")
		body := fs.String("body", "", "Plain-text message body")
		bodyStdin := fs.Bool("body-stdin", false, "Read plain-text message body from stdin (avoids shell quote expansion)")
		bodyFile := fs.String("body-file", "", "Read plain-text message body from file path")
		convID := fs.String("conv", "", "Optional conversation ID")
		resumeOnDecide := fs.Bool("resume-on-decide", false, "Wake the originating conversation upon human approve or reject")
		continuation := fs.String("continuation", "", "Optional continuation instruction injected when resuming")
		_ = fs.Parse(rest)
		resolvedBody, bErr := resolveBodyInput(*body, *bodyStdin, *bodyFile, os.Stdin)
		if bErr != nil {
			fmt.Fprintf(os.Stderr, "hq gchat propose failed: %v\n", bErr)
			os.Exit(1)
		}
		if *convID == "" {
			*convID = vault.ResolveConversationID()
		}
		target := strings.TrimSpace(*toSpace)
		if target == "" {
			target = strings.TrimSpace(*spaceID)
		}
		var spec gchat.DraftSpec
		var err error
		if target == "" && resolvedBody == "" && *draftID != "" {
			spec, err = gchat.GetDraft(vaultDir, *account, *draftID)
		} else {
			spec, err = gchat.CreateOrUpdateDraft(vaultDir, *account, *draftID, target, *spaceName, *threadID, resolvedBody, *convID)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gchat propose failed: %v\n", err)
			os.Exit(1)
		}
		summary, cmdStr, cmdArgv, meta := buildGChatDraftProposalData(resolveProposalHQBin(rootDir), spec)
		prop, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
			ActionType:       "gchat_send",
			Summary:          summary,
			Command:          cmdStr,
			CommandArgv:      cmdArgv,
			WorkDir:          resolveDefaultWorkDir(rootDir),
			ConversationID:   *convID,
			ResumeOnDecide:   *resumeOnDecide,
			ContinuationNote: *continuation,
			OriginSkill:      "gchat",
			Metadata:         meta,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gchat propose failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Staged Google Chat proposal %s (draft: %s, space: %s [%s], sha256: %s)\n",
			prop.ID, spec.DraftID, spec.SpaceName, spec.SpaceID, prop.SHA256[:12])
		fmt.Printf("  %s\n", formatBodyVerificationSummary(spec.Body))
		fmt.Printf("  Approve with: %s approve %s --sha256=%s\n", displayHQCmd(rootDir), prop.ID, prop.SHA256[:12])

	case "send-draft", "send":
		denyIfCallerTainted(vaultDir, "gchat send-draft")
		fs := flag.NewFlagSet("gchat send-draft", flag.ExitOnError)
		account := fs.String("account", "", "Account alias or email")
		draftID := fs.String("draft", "", "Draft ID to verify and send")
		hashPrefix := fs.String("hash", "", "Expected contentHash prefix from approved proposal")
		_ = fs.Parse(rest)
		out, err := gchat.VerifyAndSendDraft(vaultDir, *account, *draftID, *hashPrefix)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gchat send-draft failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(out)

	case "precheck", "triage":
		fs := flag.NewFlagSet("gchat precheck", flag.ExitOnError)
		account := fs.String("account", "", "Optional account alias filter")
		_ = fs.Parse(rest)
		batches, status, err := gchat.RunPrecheck(vaultDir, *account)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq gchat precheck failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(status)
		if len(batches) > 0 {
			printJSON(batches)
		}

	case "triage-result":
		fs := flag.NewFlagSet("gchat triage-result", flag.ExitOnError)
		batchID := fs.String("batch", "", "Batch ID being summarized")
		rawJSON := fs.String("json", "", "JSON array of TriageItem objects")
		_ = fs.Parse(rest)
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
		fmt.Fprintf(os.Stderr, "unknown gchat subcommand: %s\n", sub)
		os.Exit(1)
	}

}

// resolveBodyInput selects the message body from --body, --body-stdin, or --body-file,
// rejecting conflicting flags so callers cannot accidentally mix inline and piped inputs.
func resolveBodyInput(bodyFlag string, bodyStdin bool, bodyFile string, stdin io.Reader) (string, error) {
	bodyFile = strings.TrimSpace(bodyFile)
	if bodyStdin && bodyFile != "" {
		return "", fmt.Errorf("cannot specify both --body-stdin and --body-file")
	}
	if (bodyStdin || bodyFile != "") && bodyFlag != "" {
		return "", fmt.Errorf("cannot combine --body with --body-stdin or --body-file")
	}
	if bodyStdin {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read --body-stdin: %w", err)
		}
		return string(b), nil
	}
	if bodyFile != "" {
		b, err := os.ReadFile(bodyFile)
		if err != nil {
			return "", fmt.Errorf("read --body-file %q: %w", bodyFile, err)
		}
		return string(b), nil
	}
	return bodyFlag, nil
}

func truncateLinePreview(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "..."
}

// formatBodyVerificationSummary returns a concise verification line showing byte/rune counts,
// literal '$' count, and first/last line previews so callers can verify no shell expansion occurred.
func formatBodyVerificationSummary(body string) string {
	dollarCount := strings.Count(body, "$")
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	first := truncateLinePreview(lines[0], 60)
	if len(lines) == 1 {
		return fmt.Sprintf("Body verified: %d bytes, %d runes, %d '$' chars | line: %q",
			len(body), utf8.RuneCountInString(body), dollarCount, first)
	}
	last := truncateLinePreview(lines[len(lines)-1], 60)
	return fmt.Sprintf("Body verified: %d bytes, %d runes, %d '$' chars | first: %q | last: %q",
		len(body), utf8.RuneCountInString(body), dollarCount, first, last)
}

func printGmailUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: hq gmail <subcommand> [args]

Subcommands:
  init | add-account        Configure a Gmail account in <vault>/private/gmail/accounts.json
  accounts | status         Show configured Gmail accounts
  search | list             Search Gmail messages
  read-quarantined | read   Read a Gmail thread in quarantined mode
  draft                     Create or update a plain-text Gmail draft
  send-draft | send         Verify content hash and send an existing Gmail draft
  forward                   Fetch message HTML + attachments, prepend note/header, and stage via hq propose
                              Flags: --message=<id> --to=<emails> [--cc=<emails>] [--subject=...]
                                     [--note="..." | --note-file=<path> | --note-stdin] [--account=<alias>]
                                     [--conv=<id>] [--resume-on-decide] [--continuation="..."]
  precheck | triage         Run 0-token unread Gmail precheck
  triage-result             Record validated triage results
  attachment-quarantine     Quarantine a downloaded attachment`)
}
