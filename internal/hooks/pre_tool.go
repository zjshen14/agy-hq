package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/authz"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/taint"
)

var (
	protectedPrivateSubs = []string{
		"gmail", "gchat", "credentials", "connectors", "channels", "delegates", "runtime", "taint", "exec-cache",
		"trust.json", "approval_asks", "approval_caps", "approval_bridge_caps", "pii-denylist.txt", "daemon_token", "profile.md",
	}
	protectedCoreVaultRelTargets = []string{
		"approvals",
		filepath.Join("memory", "channels"),
	}
	protectedLedgerMemorySubs = []string{
		"workspaces.json",
		".workspaces.lock",
		"delegates_queues",
		"delegates_state.json",
		".delegates.lock",
		"delegates_audit.jsonl",
	}
	protectedBareShellPatterns = []string{
		`workspaces\.json`,
		`\.workspaces\.lock`,
		`workspaces\.jso\S*`,
		`delegates_state\.json`,
		`\.delegates\.lock`,
		`delegates_audit\.jsonl`,
	}

	reLocalCredentialShell = compileLocalCredentialShellRegex()
	reHeavyBuildTest       = regexp.MustCompile(`(?i)\b(go\s+(?:test|build)|npm\s+(?:test|run\s+build)|pnpm\s+test|pytest|cargo\s+(?:test|build))\b`)
	reGChatBrowserURL      = regexp.MustCompile(`(?i)(?:chat\.google\.com|mail\.google\.com/chat)`)
	reHQClaimDesk          = regexp.MustCompile(`(?i)(?:^|[\s;&|])(?:go\s+run\s+\./cmd/hq|(?:\S+/)?hq)\s+claim-desk\b`)
)

func compileLocalCredentialShellRegex() *regexp.Regexp {
	quoteJoin := func(items []string) string {
		quoted := make([]string, len(items))
		for i, item := range items {
			quoted[i] = regexp.QuoteMeta(item)
		}
		return strings.Join(quoted, "|")
	}
	memSubs := append([]string{"channels"}, protectedLedgerMemorySubs...)
	return regexp.MustCompile(fmt.Sprintf(
		`(?i)(?:^|[\s"'=:/])(?:local/(?:private/(?:%s)|memory/(?:%s))|%s)(?:[\s"'/]|$)`,
		quoteJoin(protectedPrivateSubs),
		quoteJoin(memSubs),
		strings.Join(protectedBareShellPatterns, "|"),
	))
}

type preToolSessionCtx struct {
	vaultDir string
	convID   string

	hasCanonVault bool
	canonVaultVal string

	hasRoots bool
	rootsVal hostpaths.Roots

	hasExtraCred bool
	extraCredVal []string

	hasAuto bool
	autoVal bool

	hasSubagent bool
	subagentVal bool

	hasAuthz    bool
	authzPolVal authz.SessionPolicy
}

func (c *preToolSessionCtx) canonVault() string {
	if !c.hasCanonVault {
		c.canonVaultVal = canonicalizeHookPath(c.vaultDir, c.vaultDir)
		c.hasCanonVault = true
	}
	return c.canonVaultVal
}

func (c *preToolSessionCtx) roots() hostpaths.Roots {
	if !c.hasRoots {
		c.rootsVal = hostpaths.Resolve(c.vaultDir)
		c.hasRoots = true
	}
	return c.rootsVal
}

func (c *preToolSessionCtx) extraCredTargets() []string {
	if !c.hasExtraCred {
		var out []string
		if rts := c.roots(); rts.AppDataDir != "" {
			out = append(out, canonicalizeHookPath(c.vaultDir, filepath.Join(rts.AppDataDir, "hq", "roots.json")))
		}
		if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
			out = append(out, canonicalizeHookPath(c.vaultDir, filepath.Join(xdg, "agy-hq", "roots.json")))
		}
		c.extraCredVal = out
		c.hasExtraCred = true
	}
	return c.extraCredVal
}

func (c *preToolSessionCtx) isAuto() bool {
	if !c.hasAuto {
		c.autoVal = isAutoConversation(c.vaultDir, c.convID)
		c.hasAuto = true
	}
	return c.autoVal
}

func (c *preToolSessionCtx) isSubagent() bool {
	if !c.hasSubagent {
		c.subagentVal = kernel.IsSubagentConversation(c.vaultDir, c.convID)
		c.hasSubagent = true
	}
	return c.subagentVal
}

func (c *preToolSessionCtx) authzPolicy() authz.SessionPolicy {
	if !c.hasAuthz {
		c.authzPolVal = authz.Resolve(c.vaultDir, c.convID, authz.ResolveHints{
			IsSubagent:   c.isSubagent(),
			IsAutoWorker: c.isAuto(),
		})
		c.hasAuthz = true
	}
	return c.authzPolVal
}

func (c *preToolSessionCtx) tainted() (string, bool) {
	pol := c.authzPolicy()
	return pol.TaintReason, pol.Tainted
}

func hasAutoOrTaintedAncestor(vaultDir, convID string) bool {
	cur := strings.ToLower(strings.TrimSpace(convID))
	if cur == "" {
		return false
	}
	nestedParents := kernel.LoadNestedConversationParents(vaultDir)
	seen := map[string]bool{cur: true}
	for depth := 0; depth < 16; depth++ {
		var parent string
		inNested := false
		if nestedParents != nil {
			if p, ok := nestedParents[cur]; ok {
				inNested = true
				parent = strings.ToLower(strings.TrimSpace(p))
			}
		}
		if parent == "" {
			if task, _, ok := kernel.FindTaskAnywhere(vaultDir, cur); ok {
				parent = strings.ToLower(strings.TrimSpace(task.ParentConversationID))
			} else if attemptTask, _, _, ok := kernel.FindTaskAttemptAnywhere(vaultDir, cur); ok {
				parent = strings.ToLower(strings.TrimSpace(attemptTask.ParentConversationID))
			}
		}
		if parent == "" {
			// A nested descendant with an unknown parent fails closed.
			return inNested
		}
		if seen[parent] {
			return false
		}
		seen[parent] = true
		if isAutoConversation(vaultDir, parent) {
			return true
		}
		if _, tainted := taint.Check(vaultDir, parent); tainted {
			return true
		}
		cur = parent
	}
	// Fail closed if ancestry chain exceeds bounded depth.
	return true
}

func protectedCredentialTargets(canonVault string, extra []string, includeLedgers bool) []string {
	var targets []string
	for _, rel := range protectedCoreVaultRelTargets {
		targets = append(targets, filepath.Join(canonVault, rel))
	}
	if includeLedgers {
		for _, sub := range protectedLedgerMemorySubs {
			targets = append(targets, filepath.Join(canonVault, "memory", sub))
		}
	}
	for _, sub := range protectedPrivateSubs {
		targets = append(targets, filepath.Join(canonVault, "private", sub))
	}
	for _, rt := range extra {
		if rt != "" {
			targets = append(targets, rt)
		}
	}
	return targets
}

func matchesAnyPathPrefix(target string, bases []string) bool {
	for _, b := range bases {
		if pathHasPrefix(target, b) {
			return true
		}
	}
	return false
}

func matchesProtectedShellTarget(vaultDir, canonVault string, extraCred []string, cmdLine string) bool {
	if reLocalCredentialShell.MatchString(cmdLine) {
		return true
	}
	for _, rt := range extraCred {
		if rt != "" && strings.Contains(cmdLine, rt) {
			return true
		}
	}
	cleanVault := filepath.Clean(vaultDir)
	vaultBases := []string{cleanVault}
	if canonVault != "" && canonVault != cleanVault {
		vaultBases = append(vaultBases, canonVault)
	}
	for _, vBase := range vaultBases {
		if vBase == "" || vBase == "." {
			continue
		}
		for _, target := range protectedCredentialTargets(vBase, nil, true) {
			if strings.Contains(cmdLine, target) {
				return true
			}
		}
	}
	if canonVault == "" {
		return false
	}
	credTargets := protectedCredentialTargets(canonVault, extraCred, true)
	for _, tok := range strings.FieldsFunc(cmdLine, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '\'', '"', '=', ';', '|', '&', '<', '>', '(', ')':
			return true
		}
		return false
	}) {
		if !strings.Contains(tok, "/") && !strings.Contains(tok, "\\") {
			continue
		}
		if matchesAnyPathPrefix(canonicalizeHookPath(vaultDir, tok), credTargets) {
			return true
		}
	}
	return false
}

func compactionNudgeMarkerPath(vaultDir, convID string) string {
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, strings.TrimSpace(convID))
	return filepath.Join(vaultDir, "memory", ".compaction_nudged_"+safe)
}

func writeAllowWithOptionalCompactionNudge(w io.Writer, vaultDir string, payload ToolCallPayload) error {
	if strings.TrimSpace(payload.ConversationID) != "" && strings.TrimSpace(payload.TranscriptPath) != "" {
		marker := compactionNudgeMarkerPath(vaultDir, payload.ConversationID)
		alreadyNudged := false
		if st, err := os.Stat(marker); err == nil && time.Since(st.ModTime()) < 2*time.Hour {
			alreadyNudged = true
		}
		if !alreadyNudged {
			tm := obs.ParseSingleTurnSlice(payload.TranscriptPath, 0)
			if !tm.CompactedPendingNextTurn && tm.CurrentContextTokens >= PreCompactionFlushTokens {
				_ = os.MkdirAll(filepath.Dir(marker), 0o700)
				_ = os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)), 0o600)
				slug := "current-task"
				if t, ok := kernel.FindTaskByConversationID(vaultDir, payload.ConversationID); ok && t.TaskSlug != "" {
					slug = t.TaskSlug
				}
				nudge := fmt.Sprintf(
					"💡 [Pre-Compaction Checkpoint Nudge]: Context has reached %s tokens (threshold: 120k). Before running `/compact` or ending a long turn, save a structured checkpoint via `./bin/hq progress save --task=%s --goal=\"...\" --done=\"...\" --next=\"...\"` so progress survives compaction.",
					obs.FormatTokensCompact(tm.CurrentContextTokens), slug,
				)
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision": "allow",
					"reason":   nudge,
				})
			}
		}
	}
	return writePreToolAllow(w)
}

// HandlePreToolUse gates outbound/irreversible commands and enforces approval-bound execution,
// as well as Front Desk non-blocking policy (blocking repo/worktree edits, test runs, and capping lookups at 3).
func HandlePreToolUse(r io.Reader, w io.Writer, vaultDir string) error {
	_, err := HandlePreToolUseWithSync(r, w, vaultDir)
	return err
}

// HandlePreToolUseWithSync executes HandlePreToolUse and reports whether Front Desk console/taskboard/quota
// state was mutated during the hook (so `hq hook pre-tool` only refreshes CONSOLE.md on rare state mutations
// rather than every read/command/edit tool call).
func HandlePreToolUseWithSync(r io.Reader, w io.Writer, vaultDir string) (bool, error) {
	mutatedConsole := false
	err := handlePreToolUse(r, w, vaultDir, &mutatedConsole)
	return mutatedConsole, err
}

func handlePreToolUse(r io.Reader, w io.Writer, vaultDir string, mutatedConsole *bool) error {
	var payload ToolCallPayload
	if err := json.NewDecoder(r).Decode(&payload); err != nil {
		return writePreToolDeny(w, "Blocked by agy-hq PreToolUse safety policy: malformed hook payload JSON (fail-closed).")
	}

	if strings.TrimSpace(payload.ConversationID) == "" {
		payload.ConversationID = strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	}
	if strings.TrimSpace(payload.ConversationID) == "" {
		return writePreToolDeny(w, "Blocked by agy-hq PreToolUse safety policy: missing conversationId in hook payload (fail-closed).")
	}

	_ = kernel.RecordLiveLSSession(vaultDir)
	bindSpawnTokenFromTranscript(vaultDir, payload.ConversationID, payload.LastUserInput, payload.TranscriptPath)
	convID := payload.ConversationID
	defer func() {
		_, _ = events.TryRenewLeaseByConversation(vaultDir, convID, time.Now(), events.DefaultLeaseTTL)
	}()

	ctx := &preToolSessionCtx{vaultDir: vaultDir, convID: convID}

	if authzPol := ctx.authzPolicy(); authzPol.Profile == authz.ProfileFailClosed {
		return writePreToolDeny(w, fmt.Sprintf("Blocked by agy-hq Fail-Closed session policy (%s): all tools are disabled because the session authorization grant expired, failed verification, or could not be restored.", authzPol.TaintReason))
	}

	if payload.ToolCall.Name == "run_command" {
		if handled, err := handlePreToolRunCommand(w, vaultDir, payload, ctx); handled || err != nil {
			return err
		}
	}

	// Universal Credential Vault Read/Write Guard: protect raw OAuth tokens, connectors, taint, trust, approvals/payloads, approval_asks/caps, daemon_token, pii-denylist.txt, and roots.json across ALL sessions (tainted or untainted)
	switch payload.ToolCall.Name {
	case "read_file", "write_file", "edit_file", "grep_search", "find_by_name":
		credPaths := protectedCredentialTargets(ctx.canonVault(), ctx.extraCredTargets(), false)
		for _, key := range []string{"Path", "SearchPath", "SearchDirectory"} {
			if rawArg, ok := payload.ToolCall.Args[key].(string); ok && strings.TrimSpace(rawArg) != "" {
				if matchesAnyPathPrefix(canonicalizeHookPath(vaultDir, rawArg), credPaths) {
					return writePreToolDeny(w, "Blocked by agy-hq Credential Vault policy: direct LLM access to protected Vault secrets/trust/taint/approvals or roots.json is strictly prohibited across all sessions.")
				}
			}
		}
	}

	// Protect deterministic proposal, RSI verification, transcript logs (.system_generated), extensions.json, and durable event/timer/outbox/watcher stores from direct LLM file tampering across all sessions
	if payload.ToolCall.Name == "write_file" || payload.ToolCall.Name == "edit_file" {
		if handled, err := handlePreToolProtectedWrites(w, vaultDir, payload, ctx); handled || err != nil {
			return err
		}
	}

	// Block direct LLM browser automation (chrome-devtools MCP) against chat.google.com to prevent bypassing hq gchat quarantine & approval gates
	if strings.Contains(strings.ToLower(payload.ToolCall.Name), "chrome-devtools") ||
		payload.ToolCall.Name == "call_mcp_tool" ||
		payload.ToolCall.Name == "navigate_page" || payload.ToolCall.Name == "new_page" ||
		payload.ToolCall.Name == "evaluate_script" || payload.ToolCall.Name == "click" || payload.ToolCall.Name == "fill" {
		rawArgs, _ := json.Marshal(payload.ToolCall.Args)
		if reGChatBrowserURL.Match(rawArgs) {
			return writePreToolDeny(w, "Blocked by agy-hq Google Chat safety policy: direct chrome-devtools interaction with chat.google.com is forbidden. Use `./bin/hq gchat <spaces|search|read-quarantined|propose>` instead.")
		}
	}

	// Enforce Unified Session Authorization Lattice on all tainted profiles (RFC-0026 §3.4 & RFC-0017b §3)
	if authzPol := ctx.authzPolicy(); authzPol.Tainted {
		switch payload.ToolCall.Name {
		case "update_focus":
			return writePreToolAllow(w)
		case "read_file":
			pathArg, _ := payload.ToolCall.Args["Path"].(string)
			if !isAllowedAuthzReadPath(vaultDir, payload, authzPol, pathArg) {
				return writePreToolDeny(w, fmt.Sprintf("Blocked by agy-hq %s policy (%s): read_file in tainted sessions is restricted to authorized SKILL.md instructions and this conversation's own artifact directory.", authzPol.Profile, authzPol.TaintReason))
			}
			return writePreToolAllow(w)
		case "run_command":
			cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
			argv, aErr := strictArgv(cmdLine)
			if aErr != nil {
				return writePreToolDeny(w, fmt.Sprintf("Blocked by agy-hq %s policy (%s): %v. Invoke single canonical 'hq ...' commands directly without shell loops, ';', '&&', or pipes (use comma-separated --thread=t1,t2 on 'hq watch read' and --all or --item=i1,i2 on 'hq watch ack').", authzPol.Profile, authzPol.TaintReason, aErr))
			}
			if vErr := authzPol.EvalHQArgv(argv); vErr != nil {
				return writePreToolDeny(w, fmt.Sprintf("Blocked by agy-hq %s policy (%s): %v", authzPol.Profile, authzPol.TaintReason, vErr))
			}
			scrubbed := map[string]interface{}{"CommandLine": wrapScrubbedArgvCommand(argv)}
			if apprPol, _, _ := approvals.LoadPolicy(vaultDir); authzPol.Exec.PreToolAutoApproveHQ && (apprPol.HostPresetBridge || apprPol.InteractiveGateMode == approvals.InteractiveGateNativeForceAsk) {
				return writePreToolAutoApproveOverwrite(w, scrubbed)
			}
			return writePreToolAllowOverwrite(w, scrubbed)
		default:
			return writePreToolDeny(w, fmt.Sprintf("Blocked by agy-hq %s policy (%s): tool %q is disabled in tainted sessions (deny-by-default).", authzPol.Profile, authzPol.TaintReason, payload.ToolCall.Name))
		}
	}

	// W1 (RFC-0025 §10 / §16.3): single-writer lease on lease-enforced provider workspaces for write_file /
	// edit_file in every non-tainted session (the Front Desk's Rule A below still applies on top of it).
	if payload.ToolCall.Name == "write_file" || payload.ToolCall.Name == "edit_file" {
		if pathArg, ok := payload.ToolCall.Args["Path"].(string); ok && strings.TrimSpace(pathArg) != "" {
			if reason := workspaceWriteLeaseDenial(vaultDir, payload.ConversationID, pathArg); reason != "" {
				return writePreToolDeny(w, reason)
			}
		}
	}

	// Surgical Per-Question Proposal Preview + Human-Presence Challenge Nonce overwrite on ask_question (RFC-0021 §4.3D)
	if payload.ToolCall.Name == "ask_question" {
		if ctx.isAuto() {
			return writePreToolDeny(w, "Blocked by agy-hq Unattended [Auto] policy: background [Auto] sessions cannot prompt via ask_question; route findings via `hq outbox add` or `hq offer stage`.")
		}
		if pending, err := approvals.List(vaultDir); err == nil && len(pending) > 0 {
			if matchedID := askQuestionsReferenceProposal(payload.ToolCall.Args, pending); matchedID != "" {
				if _, isTaintedCaller := ctx.tainted(); isTaintedCaller || ctx.isSubagent() {
					return writePreToolDeny(w, fmt.Sprintf("Blocked by agy-hq Human-Presence Approval policy: subagents and quarantined sessions cannot bind or execute 1-tap approval challenges directly for %s. Report %s back to your parent Task Conversation or `[HQ] Front Desk` so the user can approve it there (or approve in the :4242 Console).", matchedID, matchedID))
				}
			}
			if overwriteArgs, ok := rewriteAskQuestionsSurgical(vaultDir, payload.ToolCall.Args, pending, payload.ConversationID, payload.TranscriptPath); ok {
				return writePreToolAllowOverwrite(w, overwriteArgs)
			}
		}
	}

	if payload.ToolCall.Name == "invoke_subagent" {
		role, _ := payload.ToolCall.Args["Role"].(string)
		model, _ := payload.ToolCall.Args["Model"].(string)
		prompt, _ := payload.ToolCall.Args["Prompt"].(string)

		fd, hasFD := kernel.GetFrontDesk(vaultDir)
		isFrontDesk := hasFD && payload.ConversationID != "" && fd.ConversationID == payload.ConversationID

		if isFrontDesk {
			_ = kernel.RecordSubagentSpawn(vaultDir, role, model, prompt)
			_, _ = kernel.QueuePendingSubagentWithParent(vaultDir, payload.ConversationID, role, model, prompt)
			if mutatedConsole != nil {
				*mutatedConsole = true
			}
		} else {
			// Descendant subagent spawned by a subagent or secondary task conversation:
			// do NOT increment daily spawn quota and do NOT queue as a Layer-1 task.
			_, _ = kernel.QueuePendingNestedSubagent(vaultDir, payload.ConversationID, role, model, prompt)
		}
		return writePreToolAllow(w)
	}

	if payload.ToolCall.Name == "run_workflow" {
		_ = kernel.RecordActiveSpawner(vaultDir, payload.ConversationID, payload.TranscriptPath, "run_workflow")
	}
	if payload.ToolCall.Name == "run_command" {
		cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
		if !isSingleHQCommand(cmdLine) && obs.IsRawAgentAPINewConversationCommand(cmdLine) {
			_ = kernel.RecordActiveSpawner(vaultDir, payload.ConversationID, payload.TranscriptPath, "agentapi")
		}
	}

	if payload.ToolCall.Name == "manage_subagents" {
		action, _ := payload.ToolCall.Args["Action"].(string)
		if action == "kill" {
			var convIDs []string
			if rawIDs, ok := payload.ToolCall.Args["ConversationIds"].([]interface{}); ok {
				for _, v := range rawIDs {
					if s, ok := v.(string); ok && s != "" {
						convIDs = append(convIDs, s)
					}
				}
			} else if strIDs, ok := payload.ToolCall.Args["ConversationIds"].([]string); ok {
				convIDs = strIDs
			}
			_ = kernel.CancelTasks(vaultDir, convIDs, "Terminated by Front Desk")
			if mutatedConsole != nil {
				*mutatedConsole = true
			}
		} else if action == "kill_all" {
			_ = kernel.CancelAllSubagentTasks(vaultDir, "Terminated by Front Desk")
			if mutatedConsole != nil {
				*mutatedConsole = true
			}
		}
		return writePreToolAllow(w)
	}

	if payload.ToolCall.Name == "schedule" {
		cronExpr, _ := payload.ToolCall.Args["CronExpression"].(string)
		prompt, _ := payload.ToolCall.Args["Prompt"].(string)
		if armedID, _ := kernel.RecordArmedTrigger(vaultDir, payload.ConversationID, cronExpr, prompt); armedID != "" && mutatedConsole != nil {
			*mutatedConsole = true
		}
		return writePreToolAllow(w)
	}

	if payload.ToolCall.Name == "manage_task" {
		action, _ := payload.ToolCall.Args["Action"].(string)
		if strings.EqualFold(strings.TrimSpace(action), "kill") {
			taskID, _ := payload.ToolCall.Args["TaskId"].(string)
			taskID = strings.Trim(strings.TrimSpace(taskID), "\"")
			if taskID != "" {
				_ = kernel.RecordKilledFrontDeskTask(vaultDir, payload.ConversationID, taskID)
				if mutatedConsole != nil {
					*mutatedConsole = true
				}
			}
		}
		return writePreToolAllow(w)
	}

	fd, hasFD := kernel.GetFrontDesk(vaultDir)
	isFrontDesk := hasFD && payload.ConversationID != "" && fd.ConversationID == payload.ConversationID

	if !isFrontDesk {
		if payload.ToolCall.Name == "run_command" && ctx.isAuto() {
			cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
			scrubbed := map[string]interface{}{"CommandLine": wrapScrubbedAutoShellCommand(cmdLine)}
			if isSingleHQCommand(cmdLine) {
				if apprPol, _, _ := approvals.LoadPolicy(vaultDir); apprPol.HostPresetBridge {
					return writePreToolAutoApproveOverwrite(w, scrubbed)
				}
			}
			return writePreToolAllowOverwrite(w, scrubbed)
		}
		return writeAllowWithOptionalCompactionNudge(w, vaultDir, payload)
	}

	return enforceFrontDeskGuardrails(w, vaultDir, payload, fd, ctx.roots(), mutatedConsole)
}

func handlePreToolRunCommand(w io.Writer, vaultDir string, payload ToolCallPayload, ctx *preToolSessionCtx) (bool, error) {
	cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
	if isSystemGeneratedTranscriptPath(filepath.ToSlash(cmdLine)) {
		return true, writePreToolDeny(w, "Blocked by agy-hq integrity policy: direct shell access to .system_generated transcript/log directories is prohibited across all sessions.")
	}
	if isBlockedChannelGatewayShell(cmdLine) {
		return true, writePreToolDeny(w, "Blocked by agy-hq Channel Bridge policy: `hq channel inbound|callback|flush|apply-config` and `/api/channels/*` are daemon/approval-only gateways and may never be invoked directly by an LLM session (use `hq channel configure` to stage config updates).")
	}
	if isBlockedDelegateGatewayShell(cmdLine) {
		return true, writePreToolDeny(w, "Blocked by agy-hq Scoped Delegate policy: `hq delegate poll|sync-snapshots|apply-policy|apply-reply` are daemon/approval-only gateways and may never be invoked directly by an LLM session (use `hq delegate configure` to update delegate policies).")
	}
	if reHQClaimDesk.MatchString(cmdLine) && (ctx.isSubagent() || ctx.isAuto()) {
		return true, writePreToolDeny(w, "Blocked by Front Desk Lease Subagent Invariant: subagents and autonomous background workers are ephemeral processes. They must never attempt to claim, adopt, or modify the Front Desk intake lease (claim-desk is strictly prohibited for subagents).")
	}
	// Universal Credential & Trust Vault Guard: block direct shell reads/mutations of raw OAuth tokens, connectors, taint, trust, approvals/payloads, approval_asks/caps, daemon_token, pii-denylist.txt, or roots.json across ALL sessions
	if matchesProtectedShellTarget(vaultDir, ctx.canonVault(), ctx.extraCredTargets(), cmdLine) {
		return true, writePreToolDeny(w, "Blocked by agy-hq Credential Vault policy: direct shell access to local/private/{gmail,gchat,credentials,connectors,channels,taint,trust.json,approval_asks,approval_caps,approval_bridge_caps,pii-denylist.txt,daemon_token}, local/memory/{channels,workspaces.json,.workspaces.lock}, or roots.json is strictly prohibited across all sessions.")
	}

	// Canonical Human-Presence Gate on `hq approve` (N1 / S1 / S2)
	if reHQApproveAny.MatchString(cmdLine) {
		if _, isTaintedCaller := ctx.tainted(); isTaintedCaller || ctx.isAuto() || ctx.isSubagent() {
			return true, writePreToolDeny(w, "Blocked by agy-hq Human-Presence Approval policy: subagents, autonomous [Auto] runs, and quarantined sessions may never execute `hq approve`.")
		}
		binTok, propID, shaPrefix, parseErr := parseCanonicalHQApprove(cmdLine)
		if parseErr != nil {
			return true, writePreToolDeny(w, fmt.Sprintf("Blocked by agy-hq Human-Presence Approval policy: %v", parseErr))
		}
		capTok, fullSHA, vErr := approvals.VerifyAskAndMintCap(vaultDir, propID, shaPrefix, payload.ConversationID, payload.TranscriptPath, time.Now())
		if vErr != nil {
			return true, writePreToolDeny(w, fmt.Sprintf("Blocked by agy-hq Human-Presence Approval policy: %v", vErr))
		}
		rewritten := fmt.Sprintf("%s approve %s --sha256=%s --cap=%s",
			shellQuoteSingle(resolveTrustedHQExecutable(binTok)),
			shellQuoteSingle(propID),
			shellQuoteSingle(fullSHA),
			shellQuoteSingle(capTok),
		)
		return true, writePreToolAllowOverwrite(w, map[string]interface{}{"CommandLine": rewritten})
	}
	if hasUnsafeDoubleQuotedDollar(cmdLine) {
		return true, writePreToolDeny(w, "Blocked by agy-hq outbound quote-safety policy: double-quoted --body/--message containing '$' risks silent bash variable expansion (e.g. \"$382.34\" -> \"82.34\"). Use --body-stdin with single-quoted heredoc instead: `cat <<'EOF' | ./bin/hq gchat propose --to=\"...\" --body-stdin`.")
	}
	isAuto := ctx.isAuto()
	if isAuto && (strings.Contains(cmdLine, "config.yaml") || strings.Contains(cmdLine, "hq-extension.json")) {
		if strings.ContainsAny(cmdLine, ">") ||
			strings.Contains(cmdLine, "sed ") ||
			strings.Contains(cmdLine, "tee ") ||
			strings.Contains(cmdLine, "cp ") ||
			strings.Contains(cmdLine, "mv ") ||
			strings.Contains(cmdLine, "rm ") ||
			strings.Contains(cmdLine, "python") ||
			strings.Contains(cmdLine, "perl") {
			return true, writePreToolDeny(w, "Blocked by agy-hq policy self-tamper guard: autonomous [Auto] sessions cannot modify config.yaml or hq-extension.json.")
		}
	}

	pol, _, _ := approvals.LoadPolicy(vaultDir)
	checkLine := cmdLine
	if reHQPropose.MatchString(cmdLine) {
		if reDoubleQuotedCmdSubst.MatchString(cmdLine) {
			return true, writePreToolDeny(w, "Blocked by agy-hq approval policy: command substitution ($(...) or backticks) inside double-quoted `hq propose --cmd=\"...\"` executes immediately before approval. Use single quotes (`--cmd='...'`) or literal strings.")
		}
		if pol.RejectNoopCommands {
			if hasCmd, cmdVal, argvVal := extractProposeCmdForValidation(cmdLine); hasCmd {
				if vErr := approvals.ValidateDeterministicCommand(cmdVal, argvVal); vErr != nil {
					return true, writePreToolDeny(w, fmt.Sprintf("Blocked by agy-hq approval policy: %v", vErr))
				}
			}
		}
		checkLine = stripProposeInnerCommand(cmdLine)
	}

	workDir := ""
	if len(payload.WorkspacePaths) > 0 {
		workDir = payload.WorkspacePaths[0]
	}
	_, isTaintedCaller := ctx.tainted()
	isUntaintedInteractiveSubagent := !isAuto && ctx.isSubagent() && !isTaintedCaller && !hasAutoOrTaintedAncestor(vaultDir, payload.ConversationID)
	isRestricted := isAuto || ctx.isSubagent() || isTaintedCaller
	if eval := approvals.EvaluateCommandForSession(pol, checkLine, isRestricted, isUntaintedInteractiveSubagent, workDir); eval.Gated {
		var reason string
		if isUntaintedInteractiveSubagent && eval.RuleID == "forge_git" {
			reason = fmt.Sprintf(
				"Blocked by agy-hq subagent approval policy (%s): interactive subagents may only push or manage issues/PRs on repositories listed in `approvals.owned_repos` in `config.yaml`. Stage %q via `./bin/hq propose --type=%s --summary=\"...\" --cmd=%q` for human review.",
				eval.RuleID, cmdLine, eval.RuleID, cmdLine,
			)
		} else if isRestricted {
			reason = fmt.Sprintf(
				"Blocked by agy-hq Unattended [Auto] policy (%s): autonomous background runs may not execute external mutating command %q directly. Stage it via `./bin/hq propose --type=%s --summary=\"...\" --cmd=%q` for human review.",
				eval.RuleID, cmdLine, eval.RuleID, cmdLine,
			)
		} else if eval.Mode == approvals.ModeThirdPartyOnly {
			reason = fmt.Sprintf(
				"Blocked by agy-hq approval policy (%s, target=%q not in approvals.owned_repos): outbound command %q must not be executed directly. Stage it using `./bin/hq propose --type=%s --summary=\"...\" --cmd=%q` and ask the user via `ask_question` (or configure `approvals.owned_repos` in `config.yaml`).",
				eval.RuleID, eval.TargetRepo, cmdLine, eval.RuleID, cmdLine,
			)
		} else {
			reason = fmt.Sprintf(
				"Blocked by agy-hq approval policy (%s): irreversible outbound command %q must not be executed directly. Stage it using `./bin/hq propose --type=%s --summary=\"...\" --cmd=%q` and ask the user via `ask_question`.",
				eval.RuleID, cmdLine, eval.RuleID, cmdLine,
			)
		}
		if !isRestricted && pol.InteractiveGateMode == approvals.InteractiveGateNativeForceAsk && eval.RuleID != "rsi_apply" {
			return true, writePreToolForceAsk(w, fmt.Sprintf("Approval required (%s): %s", eval.RuleID, eval.Reason))
		}
		return true, writePreToolDeny(w, reason)
	}
	return false, nil
}

func handlePreToolProtectedWrites(w io.Writer, vaultDir string, payload ToolCallPayload, ctx *preToolSessionCtx) (bool, error) {
	pathArg, ok := payload.ToolCall.Args["Path"].(string)
	if !ok || strings.TrimSpace(pathArg) == "" {
		return false, nil
	}
	canonPath := canonicalizeHookPath(vaultDir, pathArg)
	rawCleanSlash := filepath.ToSlash(filepath.Clean(pathArg))
	canonSlash := filepath.ToSlash(canonPath)
	if isSystemGeneratedTranscriptPath(rawCleanSlash) || isSystemGeneratedTranscriptPath(canonSlash) {
		return true, writePreToolDeny(w, "Blocked by agy-hq integrity policy: direct file writes inside .system_generated transcript/log directories are prohibited across all sessions.")
	}
	canonVault := ctx.canonVault()
	protectedStores := []string{
		filepath.Join(canonVault, "extensions.json"),
		filepath.Join(canonVault, "memory", "pending"),
		filepath.Join(canonVault, "memory", "rsi", "hypotheses"),
		filepath.Join(canonVault, "memory", "rsi", "rules"),
		filepath.Join(canonVault, "memory", "rsi", "archive.jsonl"),
		filepath.Join(canonVault, "memory", "interventions.jsonl"),
		filepath.Join(canonVault, "memory", "events.json"),
		filepath.Join(canonVault, "memory", "events_archive.jsonl"),
		filepath.Join(canonVault, "memory", "timers.json"),
		filepath.Join(canonVault, "memory", "outbox"),
		filepath.Join(canonVault, "memory", "watcher"),
		filepath.Join(canonVault, "memory", "watchers"),
		filepath.Join(canonVault, "memory", "trigger_cursors.json"),
		filepath.Join(canonVault, "memory", "front_desk.json"),
		filepath.Join(canonVault, "memory", "tasks.json"),
	}
	for _, rel := range protectedCoreVaultRelTargets {
		protectedStores = append(protectedStores, filepath.Join(canonVault, rel))
	}
	for _, sub := range protectedLedgerMemorySubs {
		protectedStores = append(protectedStores, filepath.Join(canonVault, "memory", sub))
	}
	if matchesAnyPathPrefix(canonPath, protectedStores) {
		return true, writePreToolDeny(w, "Blocked by agy-hq integrity policy: direct file writes to pending approval proposals, extension state, RSI hypotheses, durable event/timer/outbox/watcher stores, task/workspace ledgers, or intervention audit logs are prohibited.")
	}

	canonCfgPath := filepath.Join(canonVault, "config.yaml")
	if canonPath == canonCfgPath || filepath.Base(canonPath) == "hq-extension.json" {
		if ctx.isAuto() {
			return true, writePreToolDeny(w, "Blocked by agy-hq policy self-tamper guard: autonomous [Auto] sessions cannot modify config.yaml or hq-extension.json.")
		}
		if canonPath == canonCfgPath {
			simContent := ""
			hasSim := false
			if payload.ToolCall.Name == "write_file" {
				c, _ := payload.ToolCall.Args["Content"].(string)
				appFlag, _ := payload.ToolCall.Args["Append"].(bool)
				if appFlag {
					old, _ := os.ReadFile(canonCfgPath)
					simContent = string(old) + c
				} else {
					simContent = c
				}
				hasSim = true
			} else if payload.ToolCall.Name == "edit_file" {
				target, _ := payload.ToolCall.Args["Target"].(string)
				repl, _ := payload.ToolCall.Args["Replacement"].(string)
				if old, rErr := os.ReadFile(canonCfgPath); rErr == nil && target != "" {
					simContent = strings.ReplaceAll(string(old), target, repl)
					hasSim = true
				}
			}
			if hasSim {
				if _, vErr := approvals.ValidateConfigYAML(simContent); vErr != nil {
					return true, writePreToolDeny(w, fmt.Sprintf("Blocked by agy-hq approval policy validator: invalid approvals configuration in config.yaml: %v", vErr))
				}
			}
		}
	}
	return false, nil
}

func hydrateConsoleSyncContent(vaultDir string, fd kernel.FrontDeskLease, isLiveHTML bool) (string, string) {
	artDir := strings.TrimSpace(fd.ArtifactDir)
	if isLiveHTML {
		candidates := []string{}
		if artDir != "" {
			candidates = append(candidates, filepath.Join(artDir, "hq_console_live.html"))
		}
		candidates = append(candidates, filepath.Join(vaultDir, "living_spaces", "hq-console", "index.html"))
		for _, p := range candidates {
			if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
				return string(b), "Live Interactive [HQ] Console (Mobile & Remote Control Ready) — Needs Me, Work, Tokens, and Ops"
			}
		}
		return "", ""
	}
	candidates := []string{}
	if artDir != "" {
		candidates = append(candidates, filepath.Join(artDir, "hq_console.md"))
	}
	candidates = append(candidates, filepath.Join(vaultDir, "CONSOLE.md"))
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return string(b), "Live [HQ] Front Desk Console & Task Board"
		}
	}
	return "", ""
}

func enforceFrontDeskGuardrails(w io.Writer, vaultDir string, payload ToolCallPayload, fd kernel.FrontDeskLease, roots hostpaths.Roots, mutatedConsole *bool) error {
	// Rule A: Block Direct Source/Worktree File Edits on the Front Desk (edit_file / write_file)
	if payload.ToolCall.Name == "edit_file" || payload.ToolCall.Name == "write_file" {
		pathArg, _ := payload.ToolCall.Args["Path"].(string)
		absRaw := resolveHookLexicalAbs(vaultDir, pathArg)
		cleanPath := canonicalizeHookPath(vaultDir, pathArg)

		isArtifact := false
		if absRaw != "" && cleanPath != "" &&
			!isSystemGeneratedTranscriptPath(filepath.ToSlash(cleanPath)) &&
			!isSystemGeneratedTranscriptPath(filepath.ToSlash(absRaw)) {
			artDirs := allowedArtifactDirs(payload.CommonPayload, fd.ArtifactDir, roots)
			isArtifact = isPathInAnyBase(vaultDir, artDirs, pathArg)
		}

		isLocalNonWorktree := false
		if relSlash, ok := isSafePathUnderBase(vaultDir, vaultDir, pathArg); ok && relSlash != "." {
			if !strings.HasPrefix(relSlash, "worktrees/") && relSlash != "worktrees" &&
				!strings.HasPrefix(relSlash, "repos/") && relSlash != "repos" {
				isLocalNonWorktree = true
			}
		}

		if !isArtifact && !isLocalNonWorktree {
			return writePreToolDeny(w, "Blocked by agy-hq Front Desk non-blocking policy: Do not edit source code or worktree files inline on the [HQ] Front Desk thread. Delegate coding and multi-file edits to a background subagent via invoke_subagent (in local/worktrees/<repo>-<task>/) or ./bin/hq spawn so the Front Desk stays unblocked.")
		}

		slashClean := filepath.ToSlash(cleanPath)
		isMDConsole := filepath.Base(cleanPath) == "hq_console.md" || strings.HasSuffix(slashClean, "/hq_console.md")
		isLiveConsole := filepath.Base(cleanPath) == "hq_console_live.html" || strings.HasSuffix(slashClean, "/hq_console_live.html")
		if isMDConsole {
			_ = kernel.MarkConsolePinned(vaultDir, payload.ConversationID)
			if mutatedConsole != nil {
				*mutatedConsole = true
			}
		}
		if isLiveConsole {
			_ = kernel.MarkLiveConsolePinned(vaultDir, payload.ConversationID)
			if mutatedConsole != nil {
				*mutatedConsole = true
			}
		}

		if payload.ToolCall.Name == "write_file" && (isMDConsole || isLiveConsole) {
			rawContent, _ := payload.ToolCall.Args["Content"].(string)
			trimmed := strings.TrimSpace(rawContent)
			if trimmed == "{{HQ_SYNC}}" || trimmed == "" {
				if hydrated, defaultSummary := hydrateConsoleSyncContent(vaultDir, fd, isLiveConsole); hydrated != "" {
					overwriteArgs := make(map[string]interface{}, len(payload.ToolCall.Args)+3)
					for k, v := range payload.ToolCall.Args {
						overwriteArgs[k] = v
					}
					overwriteArgs["Content"] = hydrated
					overwriteArgs["Overwrite"] = true
					if _, hasMeta := overwriteArgs["ArtifactMetadata"]; !hasMeta {
						overwriteArgs["ArtifactMetadata"] = map[string]interface{}{
							"UserFacing":      true,
							"RequestFeedback": false,
							"Summary":         defaultSummary,
						}
					}
					return writePreToolAllowOverwrite(w, overwriteArgs)
				}
			}
		}

		return writePreToolAllow(w)
	}

	// Rule B: Block Heavy Build/Test Commands Inline on the Front Desk (run_command)
	if payload.ToolCall.Name == "run_command" {
		cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
		isHQCmd := isSingleHQCommand(cmdLine) || strings.Contains(cmdLine, "bootstrap.sh")

		if !isHQCmd && reHeavyBuildTest.MatchString(cmdLine) {
			return writePreToolDeny(w, "Blocked by agy-hq Front Desk non-blocking policy: Do not run build or test suites inline on the [HQ] Front Desk thread. Delegate to a background subagent via invoke_subagent or use `hq upgrade`.")
		}
	}

	// Rule C: Per-Turn Inline Investigation Cap (Max 3 Research Tool Calls on Front Desk)
	isResearchTool := false
	switch payload.ToolCall.Name {
	case "read_file":
		pathArg, _ := payload.ToolCall.Args["Path"].(string)
		cleanPath := filepath.Clean(pathArg)
		cleanSlash := filepath.ToSlash(cleanPath)
		base := filepath.Base(cleanPath)
		isOverlayRule := (strings.Contains(cleanSlash, "/memory/rsi/rules/") || strings.HasPrefix(cleanSlash, "memory/rsi/rules/")) && strings.HasSuffix(cleanSlash, ".md")
		// Turn-1 housekeeping & skill/overlay instruction reads are exempt from the 3-lookup cap.
		if base != "CONSOLE.md" && base != "hq_console.md" && base != "hq_console_live.html" && base != "triggers.yaml" && base != "SKILL.md" && !isOverlayRule {
			isResearchTool = true
		}
	case "grep_search", "find_by_name":
		isResearchTool = true
	case "run_command":
		cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
		if !isSingleHQCommand(cmdLine) {
			isResearchTool = true
		}
	}

	if isResearchTool {
		turnFile := filepath.Join(vaultDir, "memory", "front_desk_turn.json")
		var turnState struct {
			ConversationID string `json:"conversationId"`
			Count          int    `json:"count"`
			LastStepIdx    int    `json:"lastStepIdx,omitempty"`
			UpdatedAt      string `json:"updatedAt"`
		}
		if data, err := os.ReadFile(turnFile); err == nil {
			_ = json.Unmarshal(data, &turnState)
		}
		if turnState.ConversationID != payload.ConversationID {
			turnState.ConversationID = payload.ConversationID
			turnState.Count = 0
			turnState.LastStepIdx = 0
		}
		duplicateStep := false
		if payload.StepIdx > 0 && turnState.LastStepIdx == payload.StepIdx {
			if prevT, tErr := time.Parse(time.RFC3339, turnState.UpdatedAt); tErr == nil && time.Since(prevT) < 2*time.Second {
				duplicateStep = true
			}
		}
		if !duplicateStep {
			turnState.Count++
			if payload.StepIdx > 0 {
				turnState.LastStepIdx = payload.StepIdx
			}
			turnState.UpdatedAt = time.Now().Format(time.RFC3339)
			_ = os.MkdirAll(filepath.Dir(turnFile), 0o755)
			if data, err := json.MarshalIndent(turnState, "", "  "); err == nil {
				_ = os.WriteFile(turnFile, data, 0o644)
			}
		}

		if turnState.Count > 3 {
			return writePreToolDeny(w, "Blocked by agy-hq Front Desk non-blocking policy: Inline investigation limit (3 tool calls per turn) reached. Stop investigating inline and delegate the task to a background subagent via invoke_subagent so the user is never blocked.")
		}
	}

	return writePreToolAllow(w)
}
