package hooks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/gchat"
	"github.com/zjshen14/agy-hq/internal/gmail"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/worktree"
)

// CommonPayload contains the camelCase fields passed by Antigravity on hook stdin.
type CommonPayload struct {
	ConversationID        string   `json:"conversationId"`
	WorkspacePaths        []string `json:"workspacePaths"`
	TranscriptPath        string   `json:"transcriptPath"`
	ArtifactDirectoryPath string   `json:"artifactDirectoryPath"`
	ModelName             string   `json:"modelName"`
	LastUserInput         string   `json:"lastUserInput"`
}

// ToolCallPayload represents PreToolUse stdin JSON.
type ToolCallPayload struct {
	CommonPayload
	StepIdx  int `json:"stepIdx"`
	ToolCall struct {
		Name string                 `json:"name"`
		Args map[string]interface{} `json:"args"`
	} `json:"toolCall"`
}

// StopPayload represents Stop stdin JSON.
type StopPayload struct {
	CommonPayload
	ExecutionNum      int    `json:"executionNum"`
	TerminationReason string `json:"terminationReason"`
	Error             string `json:"error"`
	FullyIdle         bool   `json:"fullyIdle"`
}

// JournalEntry is the immutable per-turn summary written by hq hook stop (0 LLM tokens).
type JournalEntry struct {
	Timestamp            string   `json:"timestamp"`
	ConversationID       string   `json:"conversationId"`
	ParentConvID         string   `json:"parentConversationId,omitempty"`
	ExecutionNum         int      `json:"executionNum"`
	TranscriptPath       string   `json:"transcriptPath,omitempty"`
	WorkspacePaths       []string `json:"workspacePaths,omitempty"`
	ModelName            string   `json:"modelName,omitempty"`
	StepCount            int      `json:"stepCount,omitempty"`
	ToolErrorCount       int      `json:"toolErrorCount,omitempty"`
	StepStartIdx         int      `json:"stepStartIdx,omitempty"`
	TurnStepCount        int      `json:"turnStepCount,omitempty"`
	TurnErrorCount       int      `json:"turnErrorCount,omitempty"`
	TransientErrors      int      `json:"transientErrors,omitempty"`
	PreToolDenials       int      `json:"preToolDenials,omitempty"`
	TaintedSession       bool     `json:"taintedSession,omitempty"`
	TurnInputTokens      int64    `json:"turnInputTokens,omitempty"`
	TurnCacheReadTokens  int64    `json:"turnCacheReadTokens,omitempty"`
	TurnOutputTokens     int64    `json:"turnOutputTokens,omitempty"`
	TurnUncachedTokens   int64    `json:"turnUncachedTokens,omitempty"`
	TurnGrossTokens      int64    `json:"turnGrossTokens,omitempty"`
	TurnCacheHitRatio    float64  `json:"turnCacheHitRatio,omitempty"`
	PeakContextTokens    int64    `json:"peakContextTokens,omitempty"`
	CurrentContextTokens int64    `json:"currentContextTokens,omitempty"`
	CompactionCount      int      `json:"compactionCount,omitempty"`
	ActiveLatencyMs      int64    `json:"activeLatencyMs,omitempty"`
	HumanWaitMs          int64    `json:"humanWaitMs,omitempty"`
	UserPrompt           string   `json:"userPrompt,omitempty"`
	ToolsUsed            []string `json:"toolsUsed,omitempty"`
	FilesModified        []string `json:"filesModified,omitempty"`
	TerminationReason    string   `json:"terminationReason"`
	ResponseSummary      string   `json:"responseSummary,omitempty"`
}

// TranscriptStepError captures a single failed tool or command execution step from transcript.jsonl.
type TranscriptStepError struct {
	StepIndex int    `json:"stepIndex"`
	Type      string `json:"type,omitempty"`
	Summary   string `json:"summary"`
}

// TranscriptVerificationDigest is the 0-token ground-truth digest of a conversation's raw transcript.jsonl.
type TranscriptVerificationDigest struct {
	ConversationID      string                `json:"conversationId,omitempty"`
	TranscriptPath      string                `json:"transcriptPath"`
	TotalSteps          int                   `json:"totalSteps"`
	UserInputs          []string              `json:"userInputs,omitempty"`
	ToolsUsed           []string              `json:"toolsUsed,omitempty"`
	FilesModified       []string              `json:"filesModified,omitempty"`
	CommandsExecuted    []string              `json:"commandsExecuted,omitempty"`
	ToolErrorCount      int                   `json:"toolErrorCount"`
	Errors              []TranscriptStepError `json:"errors,omitempty"`
	FinalResponse       string                `json:"finalResponse,omitempty"`
	DiscrepancyWarnings []string              `json:"discrepancyWarnings,omitempty"`
}

// HandleSessionStart reads local/USER.md, local/MEMORY.md, and active facts,
// returning an injectSteps systemMessage JSON object on stdout.
func HandleSessionStart(r io.Reader, w io.Writer, vaultDir string) error {
	var payload CommonPayload
	_ = json.NewDecoder(r).Decode(&payload)

	// Zero-touch auto-bootstrap: if local/USER.md does not exist yet on a fresh clone,
	// automatically initialize the private Vault from templates/.
	if _, err := os.Stat(filepath.Join(vaultDir, "USER.md")); os.IsNotExist(err) {
		if rootDir, err := vault.ResolveRoot(); err == nil {
			templateDir := filepath.Join(rootDir, "templates")
			if _, statErr := os.Stat(templateDir); statErr == nil {
				_, _ = vault.Init(vaultDir, templateDir)
			}
		}
	}

	var sections []string
	if payload.ConversationID != "" {
		existing, ok := kernel.GetFrontDesk(vaultDir)

		isSubagent := kernel.IsSubagentConversation(vaultDir, payload.ConversationID)
		if !isSubagent {
			if _, alreadyTracked := kernel.FindTaskByConversationID(vaultDir, payload.ConversationID); !alreadyTracked {
				if kernel.IsNestedSubagentConversation(vaultDir, payload.ConversationID) {
					// Known nested descendant subagent: do NOT register into tasks.json!
					isSubagent = true
				} else if nestedSub, isNested := kernel.PopPendingNestedSubagent(vaultDir, 60*time.Minute); isNested {
					_ = kernel.RecordNestedConversation(vaultDir, payload.ConversationID, nestedSub.ParentConversationID)
					// Nested descendant subagent: do NOT register into tasks.json!
					isSubagent = true
				} else if sub, found := kernel.PopPendingSubagent(vaultDir, 60*time.Minute); found {
					isSubagent = true
					modelName := sub.Model
					if modelName == "" {
						modelName = payload.ModelName
					}
					_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
						TaskSlug:             sub.TaskSlug,
						Title:                sub.Title,
						Kind:                 "subagent",
						Scope:                sub.Scope,
						ParentConversationID: sub.ParentConversationID,
						Tier:                 sub.Tier,
						Model:                modelName,
						ConversationID:       payload.ConversationID,
						Status:               "active",
						LastSummary:          "Running in background...",
						CreatedAt:            time.Now().Format(time.RFC3339),
						LastUpdatedAt:        time.Now().Format(time.RFC3339),
					})
				} else {
					_, _ = kernel.LinkPendingSpawnTask(vaultDir, payload.ConversationID, payload.ModelName)
				}
			}
		}

		isFrontDesk := !isSubagent && (!ok || existing.ConversationID == payload.ConversationID)

		if isFrontDesk {
			turnFile := filepath.Join(vaultDir, "memory", "front_desk_turn.json")
			resetState := map[string]interface{}{
				"conversationId": payload.ConversationID,
				"count":          0,
				"updatedAt":      time.Now().Format(time.RFC3339),
			}
			_ = os.MkdirAll(filepath.Dir(turnFile), 0o755)
			if data, err := json.MarshalIndent(resetState, "", "  "); err == nil {
				_ = os.WriteFile(turnFile, data, 0o644)
			}
			if payload.ArtifactDirectoryPath != "" {
				_ = os.WriteFile(filepath.Join(vaultDir, "memory", "active_artifact_dir.txt"), []byte(strings.TrimSpace(payload.ArtifactDirectoryPath)), 0o644)
			}

			_ = kernel.SetFrontDesk(vaultDir, payload.ConversationID, payload.ArtifactDirectoryPath)
			kernel.ReconcileFrontDeskFromTranscript(vaultDir, payload.TranscriptPath)
			fd, _ := kernel.GetFrontDesk(vaultDir)
			_, unarmedTriggers := kernel.GetUnarmedTriggers(vaultDir)

			roleBanner := fmt.Sprintf(
				"## 🎯 Active Role: [HQ] Front Desk Coordinator (NON-BLOCKING)\n"+
					"- You are the single Front Desk intake coordinator (`%s`).\n"+
					"- **Hard Mechanical Limit:** You have at most **3 inline lookup tool calls** per turn, and direct code/worktree edits or test runs are mechanically blocked by PreToolUse hooks on this thread.\n"+
					"- For ANY bug investigation, root-cause analysis, or code change, **delegate immediately on Turn 1** via `invoke_subagent` (working in `local/worktrees/<repo>-<task>/`) or `./bin/hq spawn` so the user can keep chatting without waiting.\n"+
					"- **Sidebar Pin Recommendation:** %s",
				payload.ConversationID,
				kernel.FrontDeskPinTip,
			)
			sections = append(sections, roleBanner)

			if !ok || !fd.ConsolePinned || len(unarmedTriggers) > 0 {
				artDir := strings.TrimSpace(payload.ArtifactDirectoryPath)
				if artDir == "" {
					artDir = strings.TrimSpace(fd.ArtifactDir)
				}
				if artDir == "" {
					artDir = vault.ResolveArtifactDir(payload.ConversationID)
				}
				artPath := filepath.Join(artDir, "hq_console.md")
				consolePath := filepath.Join(vaultDir, "CONSOLE.md")

				var initLines []string
				initLines = append(initLines,
					"## 🚀 New [HQ] Front Desk Initialization Required",
					fmt.Sprintf("- Active Front Desk Conversation ID: `%s`.", payload.ConversationID),
					"- **Step 1 (Zero Redundant Reads):** `USER.md`, `MEMORY.md`, active worktrees, tracked Task Conversations, and `local/triggers.yaml` schedules are already loaded below. Do **NOT** run shell commands or `read_file` to re-fetch them.",
				)
				if !fd.ConsolePinned {
					initLines = append(initLines, fmt.Sprintf(
						"- **Step 2 (Pin Live Console Artifact):** Read `%s` and call `write_file` (`Overwrite=true`, `ArtifactMetadata={UserFacing: true, RequestFeedback: false, Summary: \"Live [HQ] Front Desk Console & Task Board\"}`) to `%s` so Antigravity pins the live status artifact in this conversation's UI.",
						consolePath, artPath,
					))
				} else {
					initLines = append(initLines, fmt.Sprintf(
						"- **Step 2 (Pin Live Console Artifact):** Already pinned at `%s`.",
						artPath,
					))
				}
				if len(unarmedTriggers) > 0 {
					initLines = append(initLines, fmt.Sprintf(
						"- **Step 3 (Arm Proactive Schedules from `local/triggers.yaml`):** %d declared schedule(s) are currently unarmed in this Front Desk session. Call the `schedule` tool for each unarmed trigger now:",
						len(unarmedTriggers),
					))
					for _, s := range unarmedTriggers {
						initLines = append(initLines, fmt.Sprintf(
							"  - `%s` (`%s`): `schedule(CronExpression=%q, IsDaemon=true, Prompt=%q)`",
							s.ID, s.Cron, s.FiveFieldCron(), s.DefaultPrompt(),
						))
					}
				}
				initLines = append(initLines, fmt.Sprintf(
					"- **Step 4 (Concise Welcome & Sidebar Pin):** Present a short status summary with clickable links to `[hq_console.md](file://%s)` and all active `[Task:<scope>]` conversations, and recommend the user pin this `[HQ] Front Desk` conversation to their Antigravity sidebar so it stays anchored at the top.",
					artPath,
				))
				sections = append(sections, strings.Join(initLines, "\n"))
			}
			if _, _, nudgeBanner := obs.GetFrontDeskContextStatus(vaultDir, payload.TranscriptPath); nudgeBanner != "" {
				sections = append(sections, nudgeBanner)
			}
		} else {
			// Subagent or secondary conversation: never inject Front Desk notice or claim-desk instructions for subagents
			if !isSubagent && existing.ConversationID != "" {
				sections = append(sections, fmt.Sprintf(
					"## ⚠️ Front Desk Notice\n- Another conversation (`%s`) currently holds the Front Desk intake lease (since %s).\n- To avoid split-brain race conditions, either use `%s` as your main intake desk, or if you are replacing/archiving it, run `./bin/hq claim-desk --conv=%s` to transfer the Front Desk lease to this thread.\n- **Tip:** %s",
					existing.ConversationID, existing.UpdatedAt, existing.ConversationID, payload.ConversationID,
					kernel.FrontDeskPinTip,
				))
			}
		}
	} else if payload.ArtifactDirectoryPath != "" {
		if _, ok := kernel.GetFrontDesk(vaultDir); !ok {
			_ = os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755)
			_ = os.WriteFile(filepath.Join(vaultDir, "memory", "active_artifact_dir.txt"), []byte(strings.TrimSpace(payload.ArtifactDirectoryPath)), 0o644)
		}
	}

	if userBytes, err := os.ReadFile(filepath.Join(vaultDir, "USER.md")); err == nil {
		if s := strings.TrimSpace(string(userBytes)); s != "" {
			sections = append(sections, "## HQ Vault: USER.md\n"+s)
		}
	}
	if memBytes, err := os.ReadFile(filepath.Join(vaultDir, "MEMORY.md")); err == nil {
		if s := strings.TrimSpace(string(memBytes)); s != "" {
			lines := strings.Split(s, "\n")
			if len(lines) > vault.MaxMemoryIndexLines {
				s = strings.Join(lines[:vault.MaxMemoryIndexLines], "\n") +
					fmt.Sprintf("\n\n> ⚠️ **[Roadmap Index Budget Warning]:** `local/MEMORY.md` exceeds %d lines (%d lines). Run `/sleep-consolidate` or `./bin/hq memory status` to distill detailed notes into `local/memory/topics/<slug>.md`.", vault.MaxMemoryIndexLines, len(lines))
			}
			sections = append(sections, "## HQ Vault: MEMORY.md\n"+s)
		}
	}
	if topics, err := vault.LoadTopicIndex(vaultDir); err == nil && len(topics) > 0 {
		var topicLines []string
		topicLines = append(topicLines, "> **Retrieval Directive:** When your task touches any topic listed below, call `read_file` on its topic page first to load verified architectural context.")
		for _, tp := range topics {
			if tp.Summary != "" {
				topicLines = append(topicLines, fmt.Sprintf("- **[%s](file://%s)** (`%s`): %s", tp.Title, tp.Path, tp.Slug, tp.Summary))
			} else {
				topicLines = append(topicLines, fmt.Sprintf("- **[%s](file://%s)** (`%s`)", tp.Title, tp.Path, tp.Slug))
			}
		}
		sections = append(sections, "## HQ Knowledge Wiki (Topic Index — Read On-Demand)\n"+strings.Join(topicLines, "\n"))
	}
	if facts, err := vault.LoadActiveFacts(vaultDir, 25); err == nil && len(facts) > 0 {
		var lines []string
		for _, f := range facts {
			meta := ""
			if f.ID != "" {
				if f.Verified {
					meta = fmt.Sprintf(" (%s · verified)", f.ID)
				} else {
					meta = fmt.Sprintf(" (%s)", f.ID)
				}
			}
			lines = append(lines, fmt.Sprintf("- [%s]%s %s", f.Topic, meta, f.Statement))
		}
		sections = append(sections, "## Active Vault Facts\n"+strings.Join(lines, "\n"))
	}
	if pending, err := approvals.List(vaultDir); err == nil && len(pending) > 0 {
		var lines []string
		for _, p := range pending {
			short := p.SHA256
			if len(short) > 12 {
				short = short[:12]
			}
			lines = append(lines, fmt.Sprintf("- `%s` (%s · sha256:%s): %s — `%s`", p.ID, p.ActionType, short, p.Summary, p.Command))
		}
		sections = append(sections, "## Pending Approvals in Queue\n"+strings.Join(lines, "\n"))
	}
	if wts, err := worktree.List(vaultDir); err == nil && len(wts) > 0 {
		var lines []string
		for _, wt := range wts {
			lines = append(lines, fmt.Sprintf("- `%s` (%s)", filepath.Base(wt), wt))
		}
		sections = append(sections, "## Active Git Worktrees\n"+strings.Join(lines, "\n"))
	}
	if tasks, err := kernel.ListTasks(vaultDir); err == nil && len(tasks) > 0 {
		now := time.Now()
		layer1Tasks := kernel.FilterLayer1Tasks(vaultDir, tasks)
		selected, omitted := kernel.SelectSessionStartTasks(layer1Tasks, now)
		if len(selected) > 0 {
			var lines []string
			for _, t := range selected {
				cleanTitle := kernel.SanitizeTaskTitle(t.Title, t.TaskSlug)
				lines = append(lines, fmt.Sprintf("- **%s** (`%s`, kind: `%s`, status: `%s`, conv: `%s`)", cleanTitle, t.TaskSlug, t.EffectiveKind(), t.EffectiveStatus(now), t.ConversationID))
			}
			if omitted > 0 {
				lines = append(lines, fmt.Sprintf("- *(+%d older completed/archived task(s) omitted from context; run `./bin/hq tasks` to inspect)*", omitted))
			}
			sections = append(sections, "## Tracked Task Conversations\n"+strings.Join(lines, "\n"))
		}
	}
	if cfg, err := kernel.LoadTriggers(vaultDir); err == nil && len(cfg.Schedules) > 0 {
		fd, _ := kernel.GetFrontDesk(vaultDir)
		var lines []string
		for _, s := range cfg.Schedules {
			status := "UNARMED"
			if fd.ArmedTriggers != nil {
				if _, ok := fd.ArmedTriggers[s.ID]; ok {
					status = "ARMED"
				}
			}
			lines = append(lines, fmt.Sprintf("- `%s` (`%s`, status: `%s`, skill: `%s`)", s.ID, s.Cron, status, s.Skill))
		}
		sections = append(sections, "## Proactive Triggers (`local/triggers.yaml`)\n"+strings.Join(lines, "\n"))
	}

	if len(sections) == 0 {
		_, err := w.Write([]byte("{}\n"))
		return err
	}

	msg := "[agy-hq Session Context Loaded]\n\n" + strings.Join(sections, "\n\n")
	resp := map[string]interface{}{
		"injectSteps": []map[string]interface{}{
			{
				"systemMessage": map[string]interface{}{
					"systemMessage": msg,
				},
			},
		},
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(resp)
}

var (
	reHQPropose    = regexp.MustCompile(`(?i)(?:^|&&|;)\s*(?:\S+/)?hq\s+propose\b`)
	reCmdFlagValue = regexp.MustCompile(`--cmd=(?:"(?:\\.|[^"\\])*"|'[^']*'|\S+)`)

	outboundCommandPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bgws\s+(gmail|chat)\s+(send|create|post|patch|update|delete)\b`),
		regexp.MustCompile(`(?i)\bgmail(?:-[0-9]+)?\b.*\b(send-message|send-draft)\b`),
		regexp.MustCompile(`(?i)\bhq\s+gmail\s+send-draft\b`),
		regexp.MustCompile(`(?i)\bhq\s+gchat\s+(send|send-draft)\b`),
		regexp.MustCompile(`(?i)\bchat\.googleapis\.com/v1/spaces/[^\s]*messages\b`),
		regexp.MustCompile(`(?i)\bgh\s+pr\s+merge\b`),
		regexp.MustCompile(`(?i)\bgh\s+release\s+create\b`),
		regexp.MustCompile(`(?i)\bvercel\s+(--prod|deploy\s+--prod)\b`),
		regexp.MustCompile(`(?i)\bhq\s+rsi\s+apply\b`),
		regexp.MustCompile(`(?i)/api/(?:approve|reject)\b`),
	}

	reDoubleQuotedCmdSubst   = regexp.MustCompile(`--cmd="(?:\\.|[^"\\])*(?:\$\(|` + "`" + `)`)
	reAllowedQuarantineHQCmd = regexp.MustCompile(`^\s*(?:\./)?bin/hq\s+(?:gmail\s+(?:search|read-quarantined|draft|triage-result|attachment-quarantine)|gchat\s+(?:spaces|search|read-quarantined|draft|triage-result)|outbox\s+add|propose|status)\b`)
	reLocalCredentialShell   = regexp.MustCompile(`(?i)(?:^|[\s"'=:/])local/private/(?:gmail|gchat|credentials|pii-denylist\.txt)(?:[\s"'/]|$)`)

	reHeavyBuildTest     = regexp.MustCompile(`(?i)\b(go\s+(?:test|build)|npm\s+(?:test|run\s+build)|pnpm\s+test|pytest|cargo\s+(?:test|build))\b`)
	reGChatBrowserURL    = regexp.MustCompile(`(?i)(?:chat\.google\.com|mail\.google\.com/chat)`)
	reQuarantinedReadCmd = regexp.MustCompile(`^\s*(?:\./)?bin/hq\s+(?:gmail|gchat)\s+read-quarantined\b`)
	reHQClaimDesk        = regexp.MustCompile(`(?i)(?:^|[\s;&|])(?:go\s+run\s+\./cmd/hq|(?:\./)?bin/hq|hq)\s+claim-desk\b`)
)

func canonicalizeHookPath(vaultDir, rawPath string) string {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return ""
	}
	p := trimmed
	if !filepath.IsAbs(p) {
		rootDir := filepath.Dir(filepath.Clean(vaultDir))
		p = filepath.Join(rootDir, p)
	}
	clean := filepath.Clean(p)
	cur := clean
	var tail []string
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return clean
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// HandlePreToolUse gates outbound/irreversible commands and enforces approval-bound execution,
// as well as Front Desk non-blocking policy (blocking repo/worktree edits, test runs, and capping lookups at 3).
func HandlePreToolUse(r io.Reader, w io.Writer, vaultDir string) error {
	var payload ToolCallPayload
	if err := json.NewDecoder(r).Decode(&payload); err != nil {
		return json.NewEncoder(w).Encode(map[string]interface{}{
			"decision": "deny",
			"reason":   "Blocked by agy-hq PreToolUse safety policy: malformed hook payload JSON (fail-closed).",
		})
	}

	if payload.ToolCall.Name == "run_command" {
		cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
		if reHQClaimDesk.MatchString(cmdLine) && kernel.IsSubagentConversation(vaultDir, payload.ConversationID) {
			return json.NewEncoder(w).Encode(map[string]interface{}{
				"decision": "deny",
				"reason":   "Blocked by Front Desk Lease Subagent Invariant: subagents are ephemeral worker processes. They must never attempt to claim, adopt, or modify the Front Desk intake lease (claim-desk is strictly prohibited for subagents).",
			})
		}
		// Universal Credential Vault Guard: block direct shell reads/mutations of raw OAuth tokens, Chrome cookies, credentials, or pii-denylist.txt across ALL sessions
		cleanVault := filepath.Clean(vaultDir)
		canonVault := canonicalizeHookPath(vaultDir, vaultDir)
		credShellHit := reLocalCredentialShell.MatchString(cmdLine)
		if !credShellHit {
			vaultBases := []string{cleanVault}
			if canonVault != "" && canonVault != cleanVault {
				vaultBases = append(vaultBases, canonVault)
			}
			for _, vBase := range vaultBases {
				if vBase == "" || vBase == "." {
					continue
				}
				for _, sub := range []string{"gmail", "gchat", "credentials", "pii-denylist.txt"} {
					if strings.Contains(cmdLine, filepath.Join(vBase, "private", sub)) {
						credShellHit = true
						break
					}
				}
				if credShellHit {
					break
				}
			}
		}
		if !credShellHit && canonVault != "" {
			credTargets := []string{
				filepath.Join(canonVault, "private", "gmail"),
				filepath.Join(canonVault, "private", "gchat"),
				filepath.Join(canonVault, "private", "credentials"),
				filepath.Join(canonVault, "private", "pii-denylist.txt"),
			}
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
				canonTok := canonicalizeHookPath(vaultDir, tok)
				for _, cp := range credTargets {
					if canonTok == cp || strings.HasPrefix(canonTok, cp+string(filepath.Separator)) {
						credShellHit = true
						break
					}
				}
				if credShellHit {
					break
				}
			}
		}
		if credShellHit {
			return json.NewEncoder(w).Encode(map[string]interface{}{
				"decision": "deny",
				"reason":   "Blocked by agy-hq Credential Vault policy: direct shell access to local/private/{gmail,gchat,credentials,pii-denylist.txt} is strictly prohibited across all sessions. Use `./bin/hq gmail`, `./bin/hq gchat`, or `./bin/hq memory forget` instead.",
			})
		}
		checkLine := cmdLine
		if reHQPropose.MatchString(cmdLine) {
			if reDoubleQuotedCmdSubst.MatchString(cmdLine) {
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision": "deny",
					"reason":   "Blocked by agy-hq approval policy: command substitution ($(...) or backticks) inside double-quoted `hq propose --cmd=\"...\"` executes immediately before approval. Use single quotes (`--cmd='...'`) or literal strings.",
				})
			}
			checkLine = reCmdFlagValue.ReplaceAllString(cmdLine, `--cmd=""`)
		}
		for _, re := range outboundCommandPatterns {
			if re.MatchString(checkLine) {
				resp := map[string]interface{}{
					"decision": "deny",
					"reason": fmt.Sprintf(
						"Blocked by agy-hq approval policy: irreversible outbound command %q must not be executed directly. Stage it using `./bin/hq propose --type=external_action --summary=\"...\" --cmd=%q` and ask the user via `ask_question`.",
						cmdLine, cmdLine,
					),
				}
				return json.NewEncoder(w).Encode(resp)
			}
		}
	}

	// Universal Credential Vault Read/Write Guard: protect raw OAuth tokens, Chrome cookies, credentials, and pii-denylist.txt across ALL sessions (tainted or untainted)
	switch payload.ToolCall.Name {
	case "read_file", "write_file", "edit_file", "grep_search", "find_by_name":
		canonVault := canonicalizeHookPath(vaultDir, vaultDir)
		credPaths := []string{
			filepath.Join(canonVault, "private", "gmail"),
			filepath.Join(canonVault, "private", "gchat"),
			filepath.Join(canonVault, "private", "credentials"),
			filepath.Join(canonVault, "private", "pii-denylist.txt"),
		}
		for _, key := range []string{"Path", "SearchPath", "SearchDirectory"} {
			if rawArg, ok := payload.ToolCall.Args[key].(string); ok && strings.TrimSpace(rawArg) != "" {
				canonP := canonicalizeHookPath(vaultDir, rawArg)
				for _, cp := range credPaths {
					if canonP == cp || strings.HasPrefix(canonP, cp+string(filepath.Separator)) {
						return json.NewEncoder(w).Encode(map[string]interface{}{
							"decision": "deny",
							"reason":   "Blocked by agy-hq Credential Vault policy: direct LLM access to local/private/{gmail,gchat,credentials,pii-denylist.txt} is strictly prohibited across all sessions. Use `./bin/hq gmail`, `./bin/hq gchat`, or `./bin/hq memory forget`.",
						})
					}
				}
			}
		}
	}

	// Protect deterministic proposal & RSI verification stores from direct LLM file tampering across all sessions
	if payload.ToolCall.Name == "write_file" || payload.ToolCall.Name == "edit_file" {
		if pathArg, ok := payload.ToolCall.Args["Path"].(string); ok && strings.TrimSpace(pathArg) != "" {
			canonPath := canonicalizeHookPath(vaultDir, pathArg)
			canonVault := canonicalizeHookPath(vaultDir, vaultDir)
			for _, protectedSub := range []string{
				filepath.Join(canonVault, "memory", "pending"),
				filepath.Join(canonVault, "memory", "rsi", "hypotheses"),
				filepath.Join(canonVault, "memory", "rsi", "archive.jsonl"),
				filepath.Join(canonVault, "memory", "interventions.jsonl"),
			} {
				if canonPath == protectedSub || strings.HasPrefix(canonPath, protectedSub+string(filepath.Separator)) {
					return json.NewEncoder(w).Encode(map[string]interface{}{
						"decision": "deny",
						"reason":   "Blocked by agy-hq integrity policy: direct file writes to pending approval proposals, RSI hypotheses, or intervention audit logs are prohibited. Use `./bin/hq propose` or `./bin/hq rsi`.",
					})
				}
			}
		}
	}

	// Block direct LLM browser automation (chrome-devtools MCP) against chat.google.com to prevent bypassing hq gchat quarantine & approval gates
	if strings.Contains(strings.ToLower(payload.ToolCall.Name), "chrome-devtools") ||
		payload.ToolCall.Name == "call_mcp_tool" ||
		payload.ToolCall.Name == "navigate_page" || payload.ToolCall.Name == "new_page" ||
		payload.ToolCall.Name == "evaluate_script" || payload.ToolCall.Name == "click" || payload.ToolCall.Name == "fill" {
		rawArgs, _ := json.Marshal(payload.ToolCall.Args)
		if reGChatBrowserURL.Match(rawArgs) {
			return json.NewEncoder(w).Encode(map[string]interface{}{
				"decision": "deny",
				"reason":   "Blocked by agy-hq Google Chat safety policy: direct chrome-devtools interaction with chat.google.com is forbidden. Use `./bin/hq gchat <spaces|search|read-quarantined|draft>` and `./bin/hq propose --type=gchat_send --draft=<draftId>` instead.",
			})
		}
	}

	// Enforce Quarantined Reader profile on sessions that have ingested untrusted external email/chat
	if taintReason, tainted := gmail.IsSessionTainted(vaultDir, payload.ConversationID); tainted {
		switch payload.ToolCall.Name {
		case "search_web", "write_file", "edit_file", "invoke_subagent", "define_subagent", "send_message", "schedule", "call_mcp_tool":
			return json.NewEncoder(w).Encode(map[string]interface{}{
				"decision": "deny",
				"reason":   fmt.Sprintf("Blocked by agy-hq Quarantined Reader policy (%s): tool %q is disabled after reading untrusted external messages.", taintReason, payload.ToolCall.Name),
			})
		case "read_file", "grep_search", "find_by_name":
			canonVault := canonicalizeHookPath(vaultDir, vaultDir)
			cleanPrivate := filepath.Join(canonVault, "private")
			cleanMem := filepath.Join(canonVault, "MEMORY.md")
			for _, key := range []string{"Path", "SearchPath", "SearchDirectory"} {
				if p, ok := payload.ToolCall.Args[key].(string); ok && p != "" {
					cleanP := canonicalizeHookPath(vaultDir, p)
					if cleanP == cleanPrivate || strings.HasPrefix(cleanP, cleanPrivate+string(filepath.Separator)) || cleanP == cleanMem {
						return json.NewEncoder(w).Encode(map[string]interface{}{
							"decision": "deny",
							"reason":   fmt.Sprintf("Blocked by agy-hq Quarantined Reader policy (%s): access to local/private/ and local/MEMORY.md is forbidden in quarantined sessions.", taintReason),
						})
					}
				}
			}
		case "run_command":
			cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
			if !reAllowedQuarantineHQCmd.MatchString(cmdLine) || strings.ContainsAny(cmdLine, "\n\r;|&`$") {
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision": "deny",
					"reason":   fmt.Sprintf("Blocked by agy-hq Quarantined Reader policy (%s): only single-line `./bin/hq gmail <search|read-quarantined|draft|triage-result|attachment-quarantine>`, `./bin/hq gchat <spaces|search|read-quarantined|draft|triage-result>`, and `./bin/hq propose` are permitted in quarantined sessions.", taintReason),
				})
			}
		default:
			if strings.HasPrefix(payload.ToolCall.Name, "mcp_") {
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision": "deny",
					"reason":   fmt.Sprintf("Blocked by agy-hq Quarantined Reader policy (%s): MCP tools (%s) are disabled in quarantined sessions.", taintReason, payload.ToolCall.Name),
				})
			}
		}
	}

	// Canonical Gmail / Google Chat / RSI Patch Preview overwrite on ask_question for pending proposals
	if payload.ToolCall.Name == "ask_question" {
		if pending, err := approvals.List(vaultDir); err == nil && len(pending) > 0 {
			rawArgs, _ := json.Marshal(payload.ToolCall.Args)
			rawStr := string(rawArgs)
			for _, prop := range pending {
				isStructuredPreview := (prop.ActionType == "gmail_send" || prop.ActionType == "gchat_send" || prop.ActionType == "rsi_patch" || prop.ActionType == "rsi_skill_patch") && prop.Metadata != nil
				hasPIIWarning := len(prop.RiskFlags) > 0
				if (isStructuredPreview || hasPIIWarning) && strings.Contains(rawStr, prop.ID) {
					meta := prop.Metadata
					if meta == nil {
						meta = map[string]string{}
					}
					hashShort := meta["contentHash"]
					if hashShort == "" {
						hashShort = meta["diffSha256"]
					}
					bodyPreview := meta["bodyPreview"]
					fromVal := meta["from"]
					toVal := meta["to"]
					subjVal := meta["subject"]
					spaceName := meta["spaceName"]
					spaceID := meta["spaceId"]
					if prop.ActionType == "gchat_send" && meta["draftId"] != "" {
						if spec, dErr := gchat.GetDraft(vaultDir, meta["account"], meta["draftId"]); dErr == nil {
							bodyPreview = spec.Body
							hashShort = spec.ContentHash
							if spec.From != "" {
								fromVal = spec.From
							}
							if spec.SpaceName != "" {
								spaceName = spec.SpaceName
							}
							if spec.SpaceID != "" {
								spaceID = spec.SpaceID
							}
						}
					} else if prop.ActionType == "gmail_send" && meta["draftId"] != "" {
						if spec, dErr := gmail.GetDraft(vaultDir, meta["account"], meta["draftId"]); dErr == nil {
							bodyPreview = spec.Body
							hashShort = spec.ContentHash
							if spec.From != "" {
								fromVal = spec.From
							}
							if spec.To != "" {
								toVal = spec.To
							}
							if spec.Subject != "" {
								subjVal = spec.Subject
							}
						}
					}
					if len(hashShort) > 12 {
						hashShort = hashShort[:12]
					}
					shaShort := prop.SHA256
					if len(shaShort) > 12 {
						shaShort = shaShort[:12]
					}
					var canonicalQ string
					if prop.ActionType == "gchat_send" {
						threadLabel := meta["threadId"]
						if threadLabel == "" {
							threadLabel = "(new message)"
						}
						canonicalQ = fmt.Sprintf(
							"Approve sending Google Chat Message %s?\n• Account: %s (%s)\n• Space: %s [%s]\n• Thread: %s\n• Message: %s\n• Draft Hash: %s",
							meta["draftId"],
							meta["account"],
							fromVal,
							spaceName,
							spaceID,
							threadLabel,
							bodyPreview,
							hashShort,
						)
					} else if prop.ActionType == "rsi_patch" || prop.ActionType == "rsi_skill_patch" {
						canonicalQ = fmt.Sprintf(
							"Approve RSI Self-Improvement Patch %s (%s)?\n• Hypothesis: %s (%s)\n• Fault Layer: %s\n• Target Files: %s\n• Verification Scorecard: %s\n• Diff SHA-256: %s\n• Diff Preview:\n%s",
							prop.ID,
							meta["ringLabel"],
							meta["rsiId"],
							meta["title"],
							meta["faultLayer"],
							meta["targetFiles"],
							meta["gateSummary"],
							hashShort,
							meta["canonicalDiffPreview"],
						)
					} else if prop.ActionType == "gmail_send" {
						canonicalQ = fmt.Sprintf(
							"Approve sending Gmail Draft %s?\n• From: %s\n• To: %s\n• Subject: %s\n• Body: %s\n• Draft Hash: %s",
							meta["draftId"],
							fromVal,
							toVal,
							subjVal,
							bodyPreview,
							hashShort,
						)
					} else {
						canonicalQ = fmt.Sprintf(
							"Approve outbound action %s (%s)?\n• Summary: %s\n• Command: %s",
							prop.ID,
							prop.ActionType,
							prop.Summary,
							prop.Command,
						)
					}
					if hasPIIWarning {
						canonicalQ = fmt.Sprintf("⚠️ [PII WARNING: %s]\n%s", strings.Join(prop.RiskFlags, ", "), canonicalQ)
					}
					overwriteArgs := map[string]interface{}{
						"questions": []map[string]interface{}{
							{
								"question":        canonicalQ,
								"is_multi_select": false,
								"options": []string{
									fmt.Sprintf("(Recommended) Approve (%s · sha256:%s)", prop.ID, shaShort),
									fmt.Sprintf("Reject (%s)", prop.ID),
									"Keep pending",
								},
							},
						},
					}
					return json.NewEncoder(w).Encode(map[string]interface{}{
						"decision":  "allow",
						"overwrite": overwriteArgs,
					})
				}
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
		} else {
			// Descendant subagent spawned by a subagent or secondary task conversation:
			// do NOT increment daily spawn quota and do NOT queue as a Layer-1 task.
			_, _ = kernel.QueuePendingNestedSubagent(vaultDir, payload.ConversationID, role, model, prompt)
		}
		_, err := w.Write([]byte(`{"decision":"allow"}` + "\n"))
		return err
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
		} else if action == "kill_all" {
			_ = kernel.CancelAllSubagentTasks(vaultDir, "Terminated by Front Desk")
		}
		_, err := w.Write([]byte(`{"decision":"allow"}` + "\n"))
		return err
	}

	if payload.ToolCall.Name == "schedule" {
		cronExpr, _ := payload.ToolCall.Args["CronExpression"].(string)
		prompt, _ := payload.ToolCall.Args["Prompt"].(string)
		_, _ = kernel.RecordArmedTrigger(vaultDir, payload.ConversationID, cronExpr, prompt)
		_, err := w.Write([]byte(`{"decision":"allow"}` + "\n"))
		return err
	}

	fd, hasFD := kernel.GetFrontDesk(vaultDir)
	isFrontDesk := hasFD && payload.ConversationID != "" && fd.ConversationID == payload.ConversationID

	if !isFrontDesk {
		_, err := w.Write([]byte(`{"decision":"allow"}` + "\n"))
		return err
	}

	// Rule A: Block Direct Source/Worktree File Edits on the Front Desk (edit_file / write_file)
	if payload.ToolCall.Name == "edit_file" || payload.ToolCall.Name == "write_file" {
		pathArg, _ := payload.ToolCall.Args["Path"].(string)
		cleanPath := filepath.Clean(pathArg)

		cleanArtDir := filepath.Clean(payload.ArtifactDirectoryPath)
		fdArtDir := filepath.Clean(fd.ArtifactDir)

		isArtifact := false
		if (cleanArtDir != "." && cleanArtDir != "" && (cleanPath == cleanArtDir || strings.HasPrefix(cleanPath, cleanArtDir+string(filepath.Separator)))) ||
			(fdArtDir != "." && fdArtDir != "" && (cleanPath == fdArtDir || strings.HasPrefix(cleanPath, fdArtDir+string(filepath.Separator)))) ||
			strings.Contains(cleanPath, "/.gemini/antigravity/brain/") {
			isArtifact = true
		}

		cleanVaultDir := filepath.Clean(vaultDir)
		isLocalNonWorktree := false
		if rel, err := filepath.Rel(cleanVaultDir, cleanPath); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
			relSlash := filepath.ToSlash(rel)
			if !strings.HasPrefix(relSlash, "worktrees/") && relSlash != "worktrees" &&
				!strings.HasPrefix(relSlash, "repos/") && relSlash != "repos" {
				isLocalNonWorktree = true
			}
		}

		if !isArtifact && !isLocalNonWorktree {
			resp := map[string]interface{}{
				"decision": "deny",
				"reason":   "Blocked by agy-hq Front Desk non-blocking policy: Do not edit source code or worktree files inline on the [HQ] Front Desk thread. Delegate coding and multi-file edits to a background subagent via invoke_subagent (in local/worktrees/<repo>-<task>/) or ./bin/hq spawn so the Front Desk stays unblocked.",
			}
			return json.NewEncoder(w).Encode(resp)
		}

		if filepath.Base(cleanPath) == "hq_console.md" || strings.HasSuffix(filepath.ToSlash(cleanPath), "/hq_console.md") {
			_ = kernel.MarkConsolePinned(vaultDir, payload.ConversationID)
		}

		_, err := w.Write([]byte(`{"decision":"allow"}` + "\n"))
		return err
	}

	// Rule B: Block Heavy Build/Test Commands Inline on the Front Desk (run_command)
	if payload.ToolCall.Name == "run_command" {
		cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
		trimmed := strings.TrimSpace(cmdLine)
		isHQCmd := strings.HasPrefix(trimmed, "./bin/hq") || strings.HasPrefix(trimmed, "bin/hq") ||
			strings.HasPrefix(trimmed, "./scripts/bootstrap.sh") || strings.HasPrefix(trimmed, "scripts/bootstrap.sh") ||
			strings.Contains(cmdLine, "bootstrap.sh")

		if !isHQCmd && reHeavyBuildTest.MatchString(cmdLine) {
			resp := map[string]interface{}{
				"decision": "deny",
				"reason":   "Blocked by agy-hq Front Desk non-blocking policy: Do not run build or test suites inline on the [HQ] Front Desk thread. Delegate to a background subagent via invoke_subagent or use ./bin/hq upgrade.",
			}
			return json.NewEncoder(w).Encode(resp)
		}
	}

	// Rule C: Per-Turn Inline Investigation Cap (Max 3 Research Tool Calls on Front Desk)
	isResearchTool := false
	switch payload.ToolCall.Name {
	case "read_file":
		pathArg, _ := payload.ToolCall.Args["Path"].(string)
		base := filepath.Base(filepath.Clean(pathArg))
		// Turn-1 housekeeping reads (CONSOLE.md, hq_console.md, triggers.yaml) are exempt from the 3-lookup cap.
		if base != "CONSOLE.md" && base != "hq_console.md" && base != "triggers.yaml" {
			isResearchTool = true
		}
	case "grep_search", "find_by_name":
		isResearchTool = true
	case "run_command":
		cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
		trimmed := strings.TrimSpace(cmdLine)
		isHQCmd := strings.HasPrefix(trimmed, "./bin/hq") || strings.HasPrefix(trimmed, "bin/hq") ||
			strings.HasPrefix(trimmed, "./scripts/bootstrap.sh") || strings.HasPrefix(trimmed, "scripts/bootstrap.sh")
		if !isHQCmd {
			isResearchTool = true
		}
	}

	if isResearchTool {
		turnFile := filepath.Join(vaultDir, "memory", "front_desk_turn.json")
		var turnState struct {
			ConversationID string `json:"conversationId"`
			Count          int    `json:"count"`
			UpdatedAt      string `json:"updatedAt"`
		}
		if data, err := os.ReadFile(turnFile); err == nil {
			_ = json.Unmarshal(data, &turnState)
		}
		if turnState.ConversationID != payload.ConversationID {
			turnState.ConversationID = payload.ConversationID
			turnState.Count = 0
		}
		turnState.Count++
		turnState.UpdatedAt = time.Now().Format(time.RFC3339)
		_ = os.MkdirAll(filepath.Dir(turnFile), 0o755)
		if data, err := json.MarshalIndent(turnState, "", "  "); err == nil {
			_ = os.WriteFile(turnFile, data, 0o644)
		}

		if turnState.Count > 3 {
			resp := map[string]interface{}{
				"decision": "deny",
				"reason":   "Blocked by agy-hq Front Desk non-blocking policy: Inline investigation limit (3 tool calls per turn) reached. Stop investigating inline and delegate the task to a background subagent via invoke_subagent so the user is never blocked.",
			}
			return json.NewEncoder(w).Encode(resp)
		}
	}

	_, err := w.Write([]byte(`{"decision":"allow"}` + "\n"))
	return err
}

// HandleStop writes an immutable 0-token turn journal entry to local/memory/journal/YYYY-MM-DD/<convId>-<execNum>.json
// and returns {"decision":"stop"} so the turn finishes immediately without blocking.
func HandleStop(r io.Reader, w io.Writer, vaultDir string) error {
	var payload StopPayload
	if err := json.NewDecoder(r).Decode(&payload); err != nil {
		_, wErr := w.Write([]byte(`{"decision":"stop"}` + "\n"))
		return wErr
	}

	fd, hasFD := kernel.GetFrontDesk(vaultDir)
	isFrontDesk := hasFD && payload.ConversationID != "" && fd.ConversationID == payload.ConversationID

	if payload.ArtifactDirectoryPath != "" && (isFrontDesk || !hasFD) {
		_ = os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755)
		_ = os.WriteFile(filepath.Join(vaultDir, "memory", "active_artifact_dir.txt"), []byte(strings.TrimSpace(payload.ArtifactDirectoryPath)), 0o644)
	}

	if payload.ConversationID != "" {
		if isFrontDesk {
			turnFile := filepath.Join(vaultDir, "memory", "front_desk_turn.json")
			resetState := map[string]interface{}{
				"conversationId": payload.ConversationID,
				"count":          0,
				"updatedAt":      time.Now().Format(time.RFC3339),
			}
			if data, err := json.MarshalIndent(resetState, "", "  "); err == nil {
				_ = os.WriteFile(turnFile, data, 0o644)
			}
			if payload.ArtifactDirectoryPath != "" {
				_ = kernel.SetFrontDesk(vaultDir, payload.ConversationID, payload.ArtifactDirectoryPath)
			}
			kernel.ReconcileFrontDeskFromTranscript(vaultDir, payload.TranscriptPath)
		}

		todayDateStr := time.Now().Format("2006-01-02")
		dateDir := filepath.Join(vaultDir, "memory", "journal", todayDateStr)
		_ = os.MkdirAll(dateDir, 0o755)
		execNum := payload.ExecutionNum
		if execNum <= 0 {
			execNum = nextExecutionNum(dateDir, payload.ConversationID)
		}
		stepStartIdx := priorJournalStepCountWithDate(vaultDir, payload.ConversationID, todayDateStr, execNum)
		tm := parseTranscriptTurnMetrics(payload.TranscriptPath, stepStartIdx)

		// B1 Duplicate Stop Hook Guard: When both workspace and plugin hooks.json invoke `hq hook stop`
		// back-to-back on the same turn completion (ExecutionNum == 0, 0 new steps since the entry written <3s ago),
		// skip writing a duplicate 0-step journal file and skip double-incrementing TurnCount.
		if payload.ExecutionNum <= 0 && execNum > 1 && payload.TranscriptPath != "" && tm.StepCount > 0 && tm.StepCount <= stepStartIdx {
			prevFile := filepath.Join(dateDir, fmt.Sprintf("%s-%d.json", payload.ConversationID, execNum-1))
			if info, stErr := os.Stat(prevFile); stErr == nil && time.Since(info.ModTime()) < 3*time.Second {
				_, wErr := w.Write([]byte(`{"decision":"stop"}` + "\n"))
				return wErr
			}
		}

		obsTurn := obs.ParseSingleTurnSlice(payload.TranscriptPath, stepStartIdx)
		toolsUsed := tm.ToolsUsed
		filesModified := tm.FilesModified
		summary := tm.LastResponse
		transcriptUserPrompt := tm.LastUserPrompt
		stepCount := tm.StepCount
		toolErrorCount := tm.ToolErrorCount
		userPrompt := strings.TrimSpace(payload.LastUserInput)
		if userPrompt == "" {
			userPrompt = transcriptUserPrompt
		}
		_, isTainted := gmail.IsSessionTainted(vaultDir, payload.ConversationID)
		parentConvID := resolveParentConversationID(vaultDir, payload.ConversationID)

		if _, err := os.Stat(dateDir); err == nil {
			entry := JournalEntry{
				Timestamp:            time.Now().Format(time.RFC3339),
				ConversationID:       payload.ConversationID,
				ParentConvID:         parentConvID,
				ExecutionNum:         execNum,
				TranscriptPath:       payload.TranscriptPath,
				WorkspacePaths:       payload.WorkspacePaths,
				ModelName:            payload.ModelName,
				StepCount:            stepCount,
				ToolErrorCount:       toolErrorCount,
				StepStartIdx:         stepStartIdx,
				TurnStepCount:        tm.TurnStepCount,
				TurnErrorCount:       tm.TurnErrorCount,
				TransientErrors:      tm.TransientErrors,
				PreToolDenials:       tm.PreToolDenials,
				TaintedSession:       isTainted || tm.TaintedCmdSeen,
				TurnInputTokens:      obsTurn.InputTokens,
				TurnCacheReadTokens:  obsTurn.CacheReadTokens,
				TurnOutputTokens:     obsTurn.OutputTokens,
				TurnUncachedTokens:   obsTurn.UncachedTokens,
				TurnGrossTokens:      obsTurn.GrossTokens,
				TurnCacheHitRatio:    obsTurn.CacheHitRatio,
				PeakContextTokens:    obsTurn.PeakContextTokens,
				CurrentContextTokens: obsTurn.CurrentContextTokens,
				CompactionCount:      obsTurn.CompactionCount,
				ActiveLatencyMs:      obsTurn.ActiveLatencyMs,
				HumanWaitMs:          obsTurn.HumanWaitMs,
				UserPrompt:           userPrompt,
				ToolsUsed:            toolsUsed,
				FilesModified:        filesModified,
				TerminationReason:    payload.TerminationReason,
				ResponseSummary:      summary,
			}
			if data, err := json.MarshalIndent(entry, "", "  "); err == nil {
				fileName := fmt.Sprintf("%s-%d.json", payload.ConversationID, execNum)
				_ = os.WriteFile(filepath.Join(dateDir, fileName), data, 0o644)
			}

			// If this conversation is not the Front Desk, auto-adopt/update it in local/memory/tasks.json
			if !isFrontDesk {
				alreadyTracked := false
				var matchedTask *kernel.ActiveTask
				var existingTasks []kernel.ActiveTask
				if tasks, err := kernel.ListTasks(vaultDir); err == nil {
					existingTasks = tasks
					for i := range existingTasks {
						if existingTasks[i].ConversationID == payload.ConversationID {
							alreadyTracked = true
							matchedTask = &existingTasks[i]
							break
						}
					}
				}

				vacatedMarker := filepath.Join(vaultDir, "memory", ".vacated")
				_, vacatedExists := os.Stat(vacatedMarker)

				if !hasFD && !alreadyTracked && len(existingTasks) == 0 && os.IsNotExist(vacatedExists) {
					// In-session bootstrap fallback: fresh Vault with no tracked tasks, front_desk is vacant,
					// and no explicit release-desk marker exists. Claim Front Desk instead of registering [Task:general]!
					_ = kernel.SetFrontDesk(vaultDir, payload.ConversationID, payload.ArtifactDirectoryPath)
					kernel.ReconcileFrontDeskFromTranscript(vaultDir, payload.TranscriptPath)
				} else {
					isNested := kernel.IsNestedSubagentConversation(vaultDir, payload.ConversationID)
					if !alreadyTracked && !isNested {
						if nestedSub, foundNested := kernel.PopPendingNestedSubagent(vaultDir, 60*time.Minute); foundNested {
							_ = kernel.RecordNestedConversation(vaultDir, payload.ConversationID, nestedSub.ParentConversationID)
							isNested = true
						}
					}

					if !alreadyTracked && !isNested {
						// Subagent fallback: match against pending queue if not yet registered
						if sub, found := kernel.PopPendingSubagent(vaultDir, 60*time.Minute); found {
							modelName := sub.Model
							if modelName == "" {
								modelName = payload.ModelName
							}
							nowISO := time.Now().Format(time.RFC3339)
							task := kernel.ActiveTask{
								TaskSlug:             sub.TaskSlug,
								Title:                sub.Title,
								Kind:                 "subagent",
								Scope:                sub.Scope,
								ParentConversationID: sub.ParentConversationID,
								Tier:                 sub.Tier,
								Model:                modelName,
								ConversationID:       payload.ConversationID,
								Status:               "completed",
								TurnCount:            1,
								CreatedAt:            nowISO,
								LastUpdatedAt:        nowISO,
								CompletedAt:          nowISO,
								LastSummary:          summary,
							}
							if strings.TrimSpace(task.LastSummary) == "" {
								task.LastSummary = "Task completed."
							}
							_ = kernel.RegisterTask(vaultDir, task)
							alreadyTracked = true
							matchedTask = &task
						} else if linkedTask, linked := kernel.LinkPendingSpawnTask(vaultDir, payload.ConversationID, payload.ModelName); linked {
							alreadyTracked = true
							matchedTask = &linkedTask
						}
					}

					if alreadyTracked && matchedTask != nil {
						// Subagent lifecycle transition: mark completed if it was a subagent task
						isSubagent := matchedTask.EffectiveKind() == "subagent" || strings.HasPrefix(matchedTask.TaskSlug, "subagent-") || strings.HasPrefix(matchedTask.Title, "[Subagent:")
						newStatus := matchedTask.Status
						if isSubagent && matchedTask.Status == "active" {
							newStatus = "completed"
						}
						newSummary := summary
						if strings.TrimSpace(newSummary) == "" {
							newSummary = matchedTask.LastSummary
						}
						if strings.TrimSpace(newSummary) == "" {
							newSummary = "Task completed."
						}
						nowISO := time.Now().Format(time.RFC3339)
						completedAt := matchedTask.CompletedAt
						if newStatus == "completed" && completedAt == "" {
							completedAt = nowISO
						}
						_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
							TaskSlug:             matchedTask.TaskSlug,
							Title:                matchedTask.Title,
							Kind:                 matchedTask.EffectiveKind(),
							Scope:                matchedTask.EffectiveScope(),
							ParentConversationID: matchedTask.ParentConversationID,
							Tier:                 matchedTask.Tier,
							Model:                matchedTask.Model,
							WorktreeDir:          matchedTask.WorktreeDir,
							RepoRoot:             matchedTask.RepoRoot,
							ConversationID:       payload.ConversationID,
							Status:               newStatus,
							TurnCount:            matchedTask.TurnCount + 1,
							Pinned:               matchedTask.Pinned,
							CreatedAt:            matchedTask.CreatedAt,
							LastUpdatedAt:        nowISO,
							CompletedAt:          completedAt,
							ArchivedAt:           matchedTask.ArchivedAt,
							LastSummary:          newSummary,
						})
					} else if !alreadyTracked && !isNested {
						title := ""
						if strings.TrimSpace(userPrompt) != "" {
							title = kernel.SanitizeTaskTitle(userPrompt, "")
						}
						_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
							Title:          title,
							Kind:           "task",
							Model:          payload.ModelName,
							ConversationID: payload.ConversationID,
							TurnCount:      1,
							LastUpdatedAt:  time.Now().Format(time.RFC3339),
							LastSummary:    summary,
						})
					}
				}
			}

			if payload.TranscriptPath != "" {
				_, _ = obs.ReconcileSubagentAttribution(vaultDir)
				_, _ = obs.SyncDailyBudgetCircuitBreaker(vaultDir, time.Now())
			}
		}
	}

	_, err := w.Write([]byte(`{"decision":"stop"}` + "\n"))
	return err
}

func nextExecutionNum(dateDir, convID string) int {
	entries, err := os.ReadDir(dateDir)
	if err != nil {
		return 1
	}
	prefix := convID + "-"
	maxNum := 0
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".json") {
			numStr := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")
			if n, err := strconv.Atoi(numStr); err == nil && n > maxNum {
				maxNum = n
			}
		}
	}
	return maxNum + 1
}

func priorJournalStepCount(vaultDir, convID string, currentExecNum int) int {
	return priorJournalStepCountWithDate(vaultDir, convID, time.Now().Format("2006-01-02"), currentExecNum)
}

func priorJournalStepCountWithDate(vaultDir, convID, todayDateStr string, currentExecNum int) int {
	if convID == "" {
		return 0
	}
	journalRoot := filepath.Join(vaultDir, "memory", "journal")
	dateDirs, err := os.ReadDir(journalRoot)
	if err != nil {
		return 0
	}
	maxStep := 0
	prefix := convID + "-"
	for _, d := range dateDirs {
		if !d.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(journalRoot, d.Name()))
		for _, f := range files {
			name := f.Name()
			if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
				continue
			}
			numStr := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")
			if n, err := strconv.Atoi(numStr); err == nil && d.Name() == todayDateStr && currentExecNum > 0 && n >= currentExecNum {
				continue
			}
			if b, err := os.ReadFile(filepath.Join(journalRoot, d.Name(), name)); err == nil {
				var je JournalEntry
				if json.Unmarshal(b, &je) == nil && je.StepCount > maxStep {
					maxStep = je.StepCount
				}
			}
		}
	}
	return maxStep
}

func resolveParentConversationID(vaultDir, convID string) string {
	if convID == "" {
		return ""
	}
	if tasks, err := kernel.ListTasks(vaultDir); err == nil {
		for _, t := range tasks {
			if t.ConversationID == convID && t.ParentConversationID != "" {
				return t.ParentConversationID
			}
		}
	}
	return ""
}

func parseTranscriptSummary(transcriptPath string) (toolsUsed []string, filesModified []string, lastResponse string, lastUserPrompt string) {
	toolsUsed, filesModified, lastResponse, lastUserPrompt, _, _ = parseTranscriptSummaryWithMetrics(transcriptPath)
	return toolsUsed, filesModified, lastResponse, lastUserPrompt
}

// isReadOnlyFileViewerOutput returns true if the tool output step is a file viewer (`read_file`)
// or if the preceding planner step only invoked non-command tools (preventing string literals like
// "Command exited with code 1" inside source files from triggering false-positive error counts).
func isReadOnlyFileViewerOutput(content string, lastPlannerHadTools bool, lastPlannerHadRunCommand bool) bool {
	if strings.HasPrefix(content, "Showing lines ") || strings.Contains(content, "\nShowing lines ") {
		return true
	}
	if lastPlannerHadTools && !lastPlannerHadRunCommand {
		return true
	}
	return false
}

func hasCommandExitNonZeroLine(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Command exited with code ") {
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "Command exited with code "))
			if len(rest) > 0 && rest[0] >= '1' && rest[0] <= '9' {
				return true
			}
		}
	}
	return false
}

func isTranscriptStepError(status, content string) bool {
	return isTranscriptStepErrorWithContext(status, content, false, true)
}

func isPreToolDenialStep(status, content string) bool {
	if !strings.Contains(content, "Blocked by agy-hq") {
		return false
	}
	if strings.HasPrefix(content, "Showing lines ") || strings.Contains(content, "\nShowing lines ") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(status), "ERROR") ||
		strings.Contains(content, "Encountered error") ||
		strings.HasPrefix(strings.TrimSpace(content), "Blocked by agy-hq") ||
		strings.Contains(content, `"decision":"deny"`) {
		return true
	}
	return false
}

func isTranscriptStepErrorWithContext(status, content string, lastPlannerHadTools bool, lastPlannerHadRunCommand bool) bool {
	if strings.EqualFold(strings.TrimSpace(status), "ERROR") {
		return true
	}
	if strings.HasPrefix(content, "Showing lines ") || strings.Contains(content, "\nShowing lines ") {
		return false
	}
	if strings.Contains(content, "Encountered error in tool execution:") || strings.Contains(content, "Encountered error in step execution:") {
		return true
	}
	if !isReadOnlyFileViewerOutput(content, lastPlannerHadTools, lastPlannerHadRunCommand) && hasCommandExitNonZeroLine(content) {
		return true
	}
	return false
}

func isTransientExternalError(content string) bool {
	lower := strings.ToLower(content)
	for _, pat := range []string{
		"429 too many requests",
		"status 429",
		"502 bad gateway",
		"503 service unavailable",
		"504 gateway timeout",
		"etimedout",
		"econnreset",
		"rate limit exceeded",
		"connection reset by peer",
	} {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

type transcriptTurnMetrics struct {
	ToolsUsed       []string
	FilesModified   []string
	LastResponse    string
	LastUserPrompt  string
	StepCount       int
	ToolErrorCount  int
	TurnStepCount   int
	TurnErrorCount  int
	TransientErrors int
	PreToolDenials  int
	TaintedCmdSeen  bool
}

func parseTranscriptSummaryWithMetrics(transcriptPath string) (toolsUsed []string, filesModified []string, lastResponse string, lastUserPrompt string, stepCount int, toolErrorCount int) {
	tm := parseTranscriptTurnMetrics(transcriptPath, 0)
	return tm.ToolsUsed, tm.FilesModified, tm.LastResponse, tm.LastUserPrompt, tm.StepCount, tm.ToolErrorCount
}

func parseTranscriptTurnMetrics(transcriptPath string, stepStartIdx int) transcriptTurnMetrics {
	var tm transcriptTurnMetrics
	if transcriptPath == "" {
		return tm
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return tm
	}
	defer f.Close()

	toolSet := make(map[string]bool)
	fileSet := make(map[string]bool)

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)

	lastPlannerHadTools := false
	lastPlannerHadRunCommand := false

	for scanner.Scan() {
		var step struct {
			Type      string `json:"type"`
			Status    string `json:"status"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Name string                 `json:"name"`
				Args map[string]interface{} `json:"args"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &step); err != nil {
			continue
		}
		curIdx := tm.StepCount
		tm.StepCount++
		inCurrentTurn := curIdx >= stepStartIdx

		if isTranscriptStepErrorWithContext(step.Status, step.Content, lastPlannerHadTools, lastPlannerHadRunCommand) {
			tm.ToolErrorCount++
			if inCurrentTurn {
				tm.TurnErrorCount++
				if isTransientExternalError(step.Content) {
					tm.TransientErrors++
				}
			}
		}
		if inCurrentTurn && !isReadOnlyFileViewerOutput(step.Content, lastPlannerHadTools, lastPlannerHadRunCommand) && isPreToolDenialStep(step.Status, step.Content) {
			tm.PreToolDenials++
		}
		if step.Type == "USER_INPUT" && strings.TrimSpace(step.Content) != "" {
			cleaned := strings.ReplaceAll(step.Content, "<USER_REQUEST>", "")
			cleaned = strings.ReplaceAll(cleaned, "</USER_REQUEST>", "")
			cleaned = strings.TrimSpace(cleaned)
			if cleaned != "" {
				tm.LastUserPrompt = cleaned
			}
		}
		if step.Type == "PLANNER_RESPONSE" {
			if strings.TrimSpace(step.Content) != "" {
				tm.LastResponse = strings.TrimSpace(step.Content)
				if runes := []rune(tm.LastResponse); len(runes) > 500 {
					tm.LastResponse = string(runes[:500]) + "..."
				}
			}
			lastPlannerHadTools = len(step.ToolCalls) > 0
			lastPlannerHadRunCommand = false
		}
		for _, tc := range step.ToolCalls {
			if tc.Name != "" {
				toolSet[tc.Name] = true
			}
			if tc.Name == "run_command" {
				lastPlannerHadRunCommand = true
				if cmdStr, ok := tc.Args["CommandLine"].(string); ok {
					if reQuarantinedReadCmd.MatchString(cmdStr) {
						tm.TaintedCmdSeen = true
					}
				}
			}
			if tc.Name == "write_file" || tc.Name == "edit_file" {
				if rawP, ok := tc.Args["Path"].(string); ok {
					p := strings.Trim(strings.TrimSpace(rawP), "\"")
					if p != "" {
						fileSet[p] = true
					}
				}
			}
			if tc.Name == "send_message" {
				if rawMsg, ok := tc.Args["Message"].(string); ok {
					msg := strings.Trim(strings.TrimSpace(rawMsg), "\"")
					if msg != "" {
						if strings.TrimSpace(tm.LastResponse) == "" || len(tm.LastResponse) < 20 {
							tm.LastResponse = msg
							if runes := []rune(tm.LastResponse); len(runes) > 500 {
								tm.LastResponse = string(runes[:500]) + "..."
							}
						}
					}
				}
			}
		}
	}

	if tm.StepCount > stepStartIdx {
		tm.TurnStepCount = tm.StepCount - stepStartIdx
	}
	for k := range toolSet {
		tm.ToolsUsed = append(tm.ToolsUsed, k)
	}
	for k := range fileSet {
		tm.FilesModified = append(tm.FilesModified, k)
	}
	return tm
}

func cleanUserPrompt(raw string) string {
	s := strings.TrimSpace(raw)
	if idx := strings.Index(s, "<ADDITIONAL_METADATA>"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	s = strings.TrimPrefix(s, "<USER_REQUEST>")
	s = strings.TrimSuffix(s, "</USER_REQUEST>")
	return strings.TrimSpace(s)
}

// VerifyTranscript resolves a conversation ID, journal JSON file path, or raw transcript.jsonl path
// and produces a 0-token ground-truth verification digest for nightly dreaming consolidation.
func VerifyTranscript(target string, vaultDir string) (*TranscriptVerificationDigest, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("conversation ID or transcript/journal path is required")
	}

	var convID string
	var transcriptPath string

	if info, err := os.Stat(target); err == nil && !info.IsDir() {
		if strings.HasSuffix(strings.ToLower(target), ".json") {
			data, err := os.ReadFile(target)
			if err != nil {
				return nil, err
			}
			var je JournalEntry
			if err := json.Unmarshal(data, &je); err == nil && (je.ConversationID != "" || je.TranscriptPath != "") {
				convID = je.ConversationID
				transcriptPath = je.TranscriptPath
			}
		}
		if transcriptPath == "" && !strings.HasSuffix(strings.ToLower(target), ".json") {
			transcriptPath = target
		}
	}

	if transcriptPath == "" {
		if convID == "" {
			convID = target
		}
		// 1. Check journal files in local/memory/journal/*/<convID>-*.json for a recorded transcriptPath
		journalRoot := filepath.Join(vaultDir, "memory", "journal")
		if dateDirs, err := os.ReadDir(journalRoot); err == nil {
			for i := len(dateDirs) - 1; i >= 0; i-- {
				d := dateDirs[i]
				if !d.IsDir() {
					continue
				}
				files, _ := os.ReadDir(filepath.Join(journalRoot, d.Name()))
				for _, f := range files {
					if strings.HasPrefix(f.Name(), convID+"-") && strings.HasSuffix(f.Name(), ".json") {
						if b, err := os.ReadFile(filepath.Join(journalRoot, d.Name(), f.Name())); err == nil {
							var je JournalEntry
							if json.Unmarshal(b, &je) == nil && je.TranscriptPath != "" {
								transcriptPath = je.TranscriptPath
								break
							}
						}
					}
				}
				if transcriptPath != "" {
					break
				}
			}
		}
		// 2. Fallback to standard Antigravity brain directory
		if transcriptPath == "" {
			artDir := vault.ResolveArtifactDir(convID)
			if artDir != "" {
				candidate1 := filepath.Join(artDir, ".system_generated", "logs", "transcript.jsonl")
				candidate2 := filepath.Join(artDir, "transcript.jsonl")
				if _, err := os.Stat(candidate1); err == nil {
					transcriptPath = candidate1
				} else if _, err := os.Stat(candidate2); err == nil {
					transcriptPath = candidate2
				} else {
					transcriptPath = candidate1
				}
			}
		}
	}

	if transcriptPath == "" {
		return nil, fmt.Errorf("could not resolve transcriptPath for %q", target)
	}

	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil, fmt.Errorf("open transcript %s: %w", transcriptPath, err)
	}
	defer f.Close()

	digest := &TranscriptVerificationDigest{
		ConversationID: convID,
		TranscriptPath: transcriptPath,
	}

	var toolOrder []string
	toolSeen := make(map[string]bool)
	var fileOrder []string
	fileSeen := make(map[string]bool)

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)

	lastPlannerHadTools := false
	lastPlannerHadRunCommand := false

	for scanner.Scan() {
		var step struct {
			StepIndex int    `json:"step_index"`
			Type      string `json:"type"`
			Status    string `json:"status"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Name string                 `json:"name"`
				Args map[string]interface{} `json:"args"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &step); err != nil {
			continue
		}
		digest.TotalSteps++

		if step.Type == "USER_INPUT" && strings.TrimSpace(step.Content) != "" {
			cleaned := cleanUserPrompt(step.Content)
			if runes := []rune(cleaned); len(runes) > 300 {
				cleaned = string(runes[:300]) + "..."
			}
			if cleaned != "" {
				digest.UserInputs = append(digest.UserInputs, cleaned)
			}
		}

		if isTranscriptStepErrorWithContext(step.Status, step.Content, lastPlannerHadTools, lastPlannerHadRunCommand) {
			digest.ToolErrorCount++
			errSummary := strings.TrimSpace(step.Content)
			if idx := strings.Index(errSummary, "Command exited with code "); idx >= 0 {
				errSummary = strings.TrimSpace(errSummary[idx:])
			}
			errSummary = strings.ReplaceAll(errSummary, "\n", " ")
			if runes := []rune(errSummary); len(runes) > 240 {
				errSummary = string(runes[:240]) + "..."
			}
			digest.Errors = append(digest.Errors, TranscriptStepError{
				StepIndex: step.StepIndex,
				Type:      step.Type,
				Summary:   errSummary,
			})
		}

		if step.Type == "PLANNER_RESPONSE" {
			if strings.TrimSpace(step.Content) != "" {
				resp := strings.TrimSpace(step.Content)
				if runes := []rune(resp); len(runes) > 600 {
					resp = string(runes[:600]) + "..."
				}
				digest.FinalResponse = resp
			}
			lastPlannerHadTools = len(step.ToolCalls) > 0
			lastPlannerHadRunCommand = false
		}

		for _, tc := range step.ToolCalls {
			if tc.Name != "" && !toolSeen[tc.Name] {
				toolSeen[tc.Name] = true
				toolOrder = append(toolOrder, tc.Name)
			}
			if tc.Name == "run_command" {
				lastPlannerHadRunCommand = true
			}
			if tc.Name == "write_file" || tc.Name == "edit_file" {
				if p, ok := tc.Args["Path"].(string); ok && p != "" && !fileSeen[p] {
					fileSeen[p] = true
					fileOrder = append(fileOrder, p)
				}
			}
			if tc.Name == "run_command" {
				if cmdStr, ok := tc.Args["CommandLine"].(string); ok && strings.TrimSpace(cmdStr) != "" {
					c := strings.TrimSpace(cmdStr)
					if runes := []rune(c); len(runes) > 160 {
						c = string(runes[:160]) + "..."
					}
					digest.CommandsExecuted = append(digest.CommandsExecuted, c)
				}
			}
		}
	}

	digest.ToolsUsed = toolOrder
	digest.FilesModified = fileOrder

	if digest.ToolErrorCount > 0 {
		digest.DiscrepancyWarnings = append(digest.DiscrepancyWarnings,
			fmt.Sprintf("Ground-truth transcript recorded %d tool/command error step(s); verify that the final claim resolved these errors.", digest.ToolErrorCount))
	}
	for _, fp := range digest.FilesModified {
		if !isModifiedFilePresentOnDisk(fp, vaultDir) {
			digest.DiscrepancyWarnings = append(digest.DiscrepancyWarnings,
				fmt.Sprintf("File %s was written/edited during the session but no longer exists on disk.", fp))
		}
	}

	return digest, nil
}

// isModifiedFilePresentOnDisk checks whether fp exists at its recorded path or, if fp was inside an
// isolated git worktree (`.../worktrees/<wtName>/<relPath>`) that has since been merged and removed,
// whether `<rootDir>/<relPath>` exists in the primary checkout.
func isModifiedFilePresentOnDisk(fp string, vaultDir string) bool {
	if _, err := os.Stat(fp); err == nil {
		return true
	}
	slashPath := filepath.ToSlash(filepath.Clean(fp))
	const marker = "/worktrees/"
	if idx := strings.Index(slashPath, marker); idx >= 0 {
		after := slashPath[idx+len(marker):]
		if slashIdx := strings.IndexByte(after, '/'); slashIdx > 0 && slashIdx+1 < len(after) {
			relPath := filepath.FromSlash(after[slashIdx+1:])
			candidates := []string{
				filepath.Join(filepath.Dir(filepath.Clean(vaultDir)), relPath),
			}
			if rootDir, err := vault.ResolveRoot(); err == nil && rootDir != "" {
				candidates = append(candidates, filepath.Join(rootDir, relPath))
			}
			for _, c := range candidates {
				if _, err := os.Stat(c); err == nil {
					return true
				}
			}
		}
	}
	return false
}
