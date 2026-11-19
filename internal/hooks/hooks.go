package hooks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/channels"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/gchat"
	"github.com/zjshen14/agy-hq/internal/gmail"
	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/taint"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/version"
	"github.com/zjshen14/agy-hq/internal/watcher"
	"github.com/zjshen14/agy-hq/internal/workspace"
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
	LastResponse      string `json:"lastResponse,omitempty"`
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

// shouldAutoClaimVacantFrontDesk decides whether a conversation starting while front_desk.json
// is vacant should automatically claim the [HQ] Front Desk lease.
// External third-party repositories and isolated task worktrees remain normal unblocked coding sessions
// unless explicitly promoted via `hq claim-desk`.
func isPathInsideVaultWorktrees(targetPath, vaultDir string) bool {
	if strings.TrimSpace(targetPath) == "" || strings.TrimSpace(vaultDir) == "" {
		return false
	}
	wtRoot := filepath.Clean(filepath.Join(vaultDir, "worktrees"))
	cleanTarget := filepath.Clean(targetPath)
	return cleanTarget == wtRoot || strings.HasPrefix(cleanTarget, wtRoot+string(filepath.Separator))
}

func shouldAutoClaimVacantFrontDesk(payload CommonPayload, vaultDir string) bool {
	if os.Getenv("HQ_AUTO_CLAIM_DESK") == "1" {
		return true
	}
	ws := ""
	if len(payload.WorkspacePaths) > 0 {
		ws = strings.TrimSpace(payload.WorkspacePaths[0])
	}
	if ws == "" {
		if cwd, err := os.Getwd(); err == nil && cwd != "" {
			cleanCWD := filepath.Clean(cwd)
			if isPathInsideVaultWorktrees(cleanCWD, vaultDir) {
				return false
			}
			// W3 (RFC-0025 §10): a cwd inside a provider workspace (ledger entry or trusted root_patterns) is a
			// regular coding session, even though non-git workspace roots carry none of the markers below.
			if isProviderWorkspacePath(vaultDir, cleanCWD) {
				return false
			}
			if vault.IsAgyHQRepo(cleanCWD) {
				return true
			}
			cleanVault := filepath.Clean(vaultDir)
			if cleanCWD == cleanVault || cleanCWD == filepath.Dir(cleanVault) {
				return true
			}
			// If cwd is an external project repository (.git, go.mod, or package.json) that is not agy-hq, do not hijack it.
			for _, marker := range []string{".git", "go.mod", "package.json", "Cargo.toml", "pyproject.toml"} {
				if _, sErr := os.Stat(filepath.Join(cleanCWD, marker)); sErr == nil {
					return false
				}
			}
		}
		return true
	}
	cleanWS := filepath.Clean(ws)
	if isPathInsideVaultWorktrees(cleanWS, vaultDir) {
		return false
	}
	if isProviderWorkspacePath(vaultDir, cleanWS) {
		return false
	}
	if vault.IsAgyHQRepo(cleanWS) {
		return true
	}
	cleanVault := filepath.Clean(vaultDir)
	if cleanWS == cleanVault || cleanWS == filepath.Dir(cleanVault) {
		return true
	}
	return false
}

// isSingleHQCommand returns true when cmdLine is a single non-chained invocation of `hq`
// (via PATH, ./bin/hq, bin/hq, or an absolute path) or scripts/bootstrap.sh.
func isSingleHQCommand(cmdLine string) bool {
	trimmed := strings.TrimSpace(cmdLine)
	if trimmed == "" || strings.ContainsAny(trimmed, "\n\r;|&`") {
		return false
	}
	fields := strings.Fields(trimmed)
	for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "-") && !strings.Contains(fields[0], "/") {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return false
	}
	first := filepath.ToSlash(fields[0])
	isHQ := first == "hq" ||
		first == "./bin/hq" ||
		first == "bin/hq" ||
		strings.HasSuffix(first, "/hq")
	if isHQ {
		if len(fields) >= 2 && fields[1] == "exec-sandboxed" {
			return false
		}
		return true
	}
	return first == "./scripts/bootstrap.sh" ||
		first == "scripts/bootstrap.sh"
}

// HandleSessionStart reads local/USER.md, local/MEMORY.md, and active facts,
// returning an injectSteps systemMessage JSON object on stdout.
func HandleSessionStart(r io.Reader, w io.Writer, vaultDir string) error {
	var payload CommonPayload
	_ = json.NewDecoder(r).Decode(&payload)
	_ = hostpaths.PersistFromLiveEnv(vaultDir)
	_ = kernel.RecordLiveLSSession(vaultDir)
	bindSpawnTokenFromTranscript(vaultDir, payload.ConversationID, payload.LastUserInput, payload.TranscriptPath)
	_, isTaintedStart := taint.Check(vaultDir, payload.ConversationID)

	// Zero-touch auto-bootstrap: if USER.md does not exist yet in the target Vault,
	// automatically initialize the private Vault from disk templates/ or embedded templates.FS.
	if _, err := os.Stat(filepath.Join(vaultDir, "USER.md")); os.IsNotExist(err) {
		templateDir := ""
		if rootDir, rErr := vault.ResolveRoot(); rErr == nil && vault.IsAgyHQRepo(rootDir) {
			templateDir = filepath.Join(rootDir, "templates")
		}
		_, _ = vault.Init(vaultDir, templateDir)
	}

	var zone1Sections []string
	var zone2Sections []string

	if plugRoot := strings.TrimSpace(os.Getenv("PLUGIN_ROOT")); plugRoot != "" {
		if m, mErr := version.ReadPluginManifest(plugRoot); mErr == nil && m.Version != "" && m.Version != version.Version {
			zone2Sections = append(zone2Sections, fmt.Sprintf(
				"> ⚠️ **[agy-hq Version Skew]:** Active plugin manifest is `v%s` (`%s`), while `hq` binary is `v%s`. Run `hq upgrade` or `hq plugin install` to synchronize.",
				m.Version, plugRoot, version.Version,
			))
		}
	}

	var matchedTask kernel.ActiveTask
	var hasMatchedTask bool
	leanAuto := false
	isFrontDesk := false
	isSubagent := false
	isAutoWorker := false

	promptText := strings.TrimSpace(payload.LastUserInput)
	if promptText == "" && strings.TrimSpace(payload.TranscriptPath) != "" {
		promptText = readFirstUserPromptFromTranscript(payload.TranscriptPath)
	}

	if payload.ConversationID != "" {
		existing, ok := kernel.GetFrontDesk(vaultDir)
		isFrontDeskExisting := ok && strings.EqualFold(existing.ConversationID, payload.ConversationID)

		isSubagent = kernel.IsSubagentConversation(vaultDir, payload.ConversationID)
		if !isSubagent {
			if t, alreadyTracked := kernel.FindTaskByConversationID(vaultDir, payload.ConversationID); alreadyTracked {
				matchedTask = t
				hasMatchedTask = true
			} else if !isFrontDeskExisting {
				nested, _, lin, hasLS := detectNestedDescendant(vaultDir, payload.ConversationID, promptText, existing.ConversationID, 3*time.Minute)
				if nested {
					// Known nested descendant subagent or workflow/agentapi worker: do NOT register into tasks.json!
					isSubagent = true
				} else if sub, found := kernel.PopPendingSubagentForCaller(vaultDir, payload.ConversationID, promptText, 3*time.Minute); found {
					isSubagent = true
					modelName := sub.Model
					if modelName == "" {
						modelName = payload.ModelName
					}
					t := kernel.ActiveTask{
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
					}
					_ = kernel.RegisterTask(vaultDir, t)
					matchedTask = t
					hasMatchedTask = true
				} else if hasLS && lin.IsDirectFrontDeskSubagent {
					isSubagent = true
					t := buildSubagentTaskFromLSLineage(payload.ConversationID, payload.ModelName, "active", "Running in background...", lin)
					_ = kernel.RegisterTask(vaultDir, t)
					matchedTask = t
					hasMatchedTask = true
				} else if linked, okLink := kernel.LinkPendingSpawnTask(vaultDir, payload.ConversationID, payload.ModelName); okLink {
					matchedTask = linked
					hasMatchedTask = true
				}
			}
		} else if t, found := kernel.FindTaskByConversationID(vaultDir, payload.ConversationID); found {
			matchedTask = t
			hasMatchedTask = true
		}

		isAutoWorker = (hasMatchedTask && matchedTask.EffectiveKind() == "auto") ||
			(!hasMatchedTask && !isSubagent && strings.HasPrefix(strings.ToLower(strings.TrimSpace(promptText)), "[auto]"))
		leanAuto = isSubagent || isAutoWorker || isTaintedStart
		isFrontDesk = !leanAuto && (existing.ConversationID == payload.ConversationID || (!ok && shouldAutoClaimVacantFrontDesk(payload, vaultDir)))
		if isTaintedStart {
			zone2Sections = append(zone2Sections, "## 🛡️ Active Role: Quarantined Triage Worker (TAINTED)\n- Untrusted external channel content is in scope. Private Vault memory (`MEMORY.md`, wiki topics, facts) is withheld, and outbound commands are restricted to `hq offer stage` and `hq outbox add`.")
		}

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
			if len(payload.WorkspacePaths) > 0 && strings.TrimSpace(payload.WorkspacePaths[0]) != "" {
				_ = kernel.SetFrontDeskWorkspace(vaultDir, payload.ConversationID, payload.WorkspacePaths[0])
			}
			kernel.ReconcileFrontDeskFromTranscript(vaultDir, payload.TranscriptPath)
			fd, _ := kernel.GetFrontDesk(vaultDir)
			_, unarmedTriggers := kernel.GetUnarmedTriggers(vaultDir)
			disarmTriggers := kernel.GetDisarmTriggers(vaultDir)

			roleBanner := fmt.Sprintf(
				"## 🎯 Active Role: [HQ] Front Desk Coordinator (NON-BLOCKING)\n"+
					"- You are the single Front Desk intake coordinator (`%s`) · Active Vault: `%s`.\n"+
					"- **Hard Mechanical Limit:** You have at most **3 inline lookup tool calls** per turn, and direct code/worktree edits or test runs are mechanically blocked by PreToolUse hooks on this thread.\n"+
					"- For ANY bug investigation, root-cause analysis, or code change, **delegate immediately on Turn 1** via `invoke_subagent` (working in `<vault>/worktrees/<repo>-<task>/`) or `hq spawn` so the user can keep chatting without waiting.\n"+
					"- **Sidebar Pin Recommendation:** %s",
				payload.ConversationID,
				vaultDir,
				kernel.FrontDeskPinTip,
			)
			zone2Sections = append(zone2Sections, roleBanner)

			if len(disarmTriggers) > 0 {
				var pausedLines []string
				var migratedLines []string
				for _, dt := range disarmTriggers {
					killHint := `manage_task(Action="kill")`
					if dt.TaskID != "" {
						killHint = fmt.Sprintf(`manage_task(Action="kill", TaskId=%q)`, dt.TaskID)
					}
					if dt.Reason == "migrated_to_daemon" {
						migratedLines = append(migratedLines, fmt.Sprintf("- `%s` (`%s`): now owned by `hq daemon` (`agy-hq.service`) — cancel duplicate timer in this thread via `%s`.", dt.ID, dt.Cron, killHint))
					} else {
						pausedLines = append(pausedLines, fmt.Sprintf("- `%s` (`%s`): paused via `hq triggers pause` — cancel its active timer in this thread via `%s`.", dt.ID, dt.Cron, killHint))
					}
					if dt.TaskID == "" {
						_ = kernel.ClearArmedTrigger(vaultDir, dt.ID)
					}
				}
				if len(migratedLines) > 0 {
					zone2Sections = append(zone2Sections, "## 🔄 Daemon-Managed Schedules Requiring Duplicate Front Desk Disarm\n"+strings.Join(migratedLines, "\n"))
				}
				if len(pausedLines) > 0 {
					zone2Sections = append(zone2Sections, "## ⏸️ Paused Schedules Requiring Front Desk Disarm\n"+strings.Join(pausedLines, "\n"))
				}
			}

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
					"- **Step 1 (Zero Redundant Reads):** `USER.md`, `MEMORY.md`, active worktrees, tracked Task Conversations, and `local/triggers.yaml` schedules are already loaded in this session context. Do **NOT** run shell commands or `read_file` to re-fetch them.",
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
				zone2Sections = append(zone2Sections, strings.Join(initLines, "\n"))
			}
			if _, _, nudgeBanner := obs.GetFrontDeskContextStatus(vaultDir, payload.TranscriptPath); nudgeBanner != "" {
				zone2Sections = append(zone2Sections, nudgeBanner)
			}
		} else {
			// Subagent / [Auto] worker or secondary/external workspace conversation: never inject Front Desk notice or claim-desk instructions for subagents/[Auto] workers
			if !leanAuto && existing.ConversationID != "" {
				zone2Sections = append(zone2Sections, fmt.Sprintf(
					"## ⚠️ Front Desk Notice\n- Active Vault: `%s`.\n- Another conversation (`%s`) currently holds the Front Desk intake lease (since %s); coding and test execution in this workspace are unrestricted.\n- To avoid split-brain race conditions, either use `%s` as your main intake desk, or if you are replacing/archiving it, run `hq claim-desk --conv=%s` to transfer the Front Desk lease to this thread.\n- **Tip:** %s",
					vaultDir, existing.ConversationID, existing.UpdatedAt, existing.ConversationID, payload.ConversationID,
					kernel.FrontDeskPinTip,
				))
			} else if !leanAuto && !ok {
				zone2Sections = append(zone2Sections, fmt.Sprintf(
					"## 🗂️ Active HQ Vault (`%s`)\n- Mode: Unrestricted Workspace Session (Front Desk coordinator lease is vacant; run `hq claim-desk --conv=%s` in a dedicated chat thread whenever you want to anchor `[HQ] Front Desk`).",
					vaultDir, payload.ConversationID,
				))
			}
		}
	} else if payload.ArtifactDirectoryPath != "" && shouldAutoClaimVacantFrontDesk(payload, vaultDir) {
		if _, ok := kernel.GetFrontDesk(vaultDir); !ok {
			_ = os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755)
			_ = os.WriteFile(filepath.Join(vaultDir, "memory", "active_artifact_dir.txt"), []byte(strings.TrimSpace(payload.ArtifactDirectoryPath)), 0o644)
		}
	}

	now := time.Now().UTC()
	scopeCtx := ResolveSessionScopes(vaultDir, payload, matchedTask, hasMatchedTask, isFrontDesk, isSubagent, isAutoWorker, leanAuto, promptText)

	// Quarantined Tainted Session: hard <= 800B cap; omit all private Vault memory layers.
	if isTaintedStart {
		msg := capSessionEnvelope("[agy-hq Session Context Loaded]\n\n"+strings.Join(zone2Sections, "\n\n"), vault.MaxSessionTaintBytes)
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

	// Zone 1: Byte-Stable Cacheable Prefix (Role-Aware Dual Line + Byte Budgets)
	if userBytes, err := os.ReadFile(filepath.Join(vaultDir, "USER.md")); err == nil {
		if s := strings.TrimSpace(string(userBytes)); s != "" {
			userCap := 2400
			if leanAuto {
				userCap = 1400
			}
			s = trimBytesPreservingLines(s, userCap)
			if !leanAuto && len(userBytes) > vault.MaxUserAdvisoryBytes {
				s += fmt.Sprintf("\n\n> ⚠️ `local/USER.md` is %d bytes (advisory target <= %d bytes). Trim prose to keep startup context lean.", len(userBytes), vault.MaxUserAdvisoryBytes)
			}
			zone1Sections = append(zone1Sections, "## HQ Vault: USER.md\n"+s)
		}
	}

	// MEMORY.md: Active Priorities ONLY (<= 40 lines & <= 2,200 bytes). Omitted in lean_auto to prevent Zone 2 starvation.
	if !leanAuto {
		if memBytes, err := os.ReadFile(filepath.Join(vaultDir, "MEMORY.md")); err == nil {
			if s := strings.TrimSpace(string(memBytes)); s != "" {
				trimmedMem, wasTrunc := vault.TruncateMemoryIndex(s, vault.MaxMemoryIndexLines, vault.MaxMemoryIndexBytes)
				if wasTrunc {
					trimmedMem += fmt.Sprintf("\n\n> ⚠️ **[Roadmap Index Budget Warning]:** `local/MEMORY.md` exceeds %d lines / %d bytes. Run `/sleep-consolidate` or `hq memory status` to keep `MEMORY.md` focused on Active Priorities.", vault.MaxMemoryIndexLines, vault.MaxMemoryIndexBytes)
				}
				zone1Sections = append(zone1Sections, "## HQ Vault: MEMORY.md\n"+trimmedMem)
			}
		}
	}

	topics, _ := vault.LoadTopicIndex(vaultDir)
	if len(topics) > 0 {
		var selectedTopics []vault.TopicPage
		if leanAuto {
			selectedTopics = vault.FilterTopicsForScope(topics, scopeCtx, 5)
		} else {
			topicLimit := 15
			if !isFrontDesk {
				topicLimit = 10
			}
			// Put scope-matched topics first, then remaining topics deterministically up to topicLimit.
			matched := vault.FilterTopicsForScope(topics, scopeCtx, topicLimit)
			seenSlug := make(map[string]bool, len(matched))
			for _, tp := range matched {
				seenSlug[tp.Slug] = true
				selectedTopics = append(selectedTopics, tp)
			}
			for _, tp := range topics {
				if len(selectedTopics) >= topicLimit {
					break
				}
				if !seenSlug[tp.Slug] {
					seenSlug[tp.Slug] = true
					selectedTopics = append(selectedTopics, tp)
				}
			}
		}
		if len(selectedTopics) > 0 {
			var topicLines []string
			topicLines = append(topicLines, "> **Retrieval Directive:** Call `read_file` on relevant topic pages below, or run `hq memory search \"<query>\"` for full-vault BM25 lookup.")
			for _, tp := range selectedTopics {
				summary := trimInlineRunes(tp.Summary, 110)
				if summary != "" {
					topicLines = append(topicLines, fmt.Sprintf("- **[%s](file://%s)** (`%s`): %s", tp.Title, tp.Path, tp.Slug, summary))
				} else {
					topicLines = append(topicLines, fmt.Sprintf("- **[%s](file://%s)** (`%s`)", tp.Title, tp.Path, tp.Slug))
				}
			}
			if len(topics) > len(selectedTopics) {
				topicLines = append(topicLines, fmt.Sprintf("- *(+%d more topic page(s) available via `hq memory search`)*", len(topics)-len(selectedTopics)))
			}
			topicBlock := "## HQ Knowledge Wiki (Topic Index — Read On-Demand)\n" + strings.Join(topicLines, "\n")
			if leanAuto {
				topicBlock = trimBytesPreservingLines(topicBlock, 750)
			} else {
				topicBlock = trimBytesPreservingLines(topicBlock, 1800)
			}
			zone1Sections = append(zone1Sections, topicBlock)
		}
	}

	if !leanAuto {
		if extSt, eErr := extension.LoadVaultState(vaultDir); eErr == nil && len(extSt.SkillOverrides) > 0 {
			keys := make([]string, 0, len(extSt.SkillOverrides))
			for k := range extSt.SkillOverrides {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var ovLines []string
			for _, k := range keys {
				res := extension.ResolveSkillDispatch(vaultDir, k)
				if res.Overridden && res.Trust == "ok" && res.SkillPath != "" {
					ovLines = append(ovLines, fmt.Sprintf("- `/%s` -> `%s` ([SKILL.md](file://%s))", k, res.ActiveTarget, res.SkillPath))
				}
			}
			if len(ovLines) > 0 {
				zone1Sections = append(zone1Sections, "## Active Companion Skill Overrides\n"+strings.Join(ovLines, "\n"))
			}
		}
	}

	rsiLimit := 5
	if leanAuto {
		rsiLimit = 2
	}
	if genRules := extension.LoadGeneralOverlayRules(vaultDir, rsiLimit); len(genRules) > 0 {
		rsiBlock := "## Learned Personal RSI Rules (`general`)\n" + strings.Join(genRules, "\n\n")
		if leanAuto {
			rsiBlock = trimBytesPreservingLines(rsiBlock, 550)
		}
		zone1Sections = append(zone1Sections, rsiBlock)
	}

	// Zone 2: Role-Scoped Dynamic Tail
	if !leanAuto {
		if isFrontDesk {
			if ovs := extension.LoadActiveFrontDeskOverlays(vaultDir, "front_desk"); len(ovs) > 0 {
				zone2Sections = append(zone2Sections, "## Companion Front Desk Policy Overlays (Advisory)\n"+trimBytesPreservingLines(strings.Join(ovs, "\n\n"), 900))
			}
		} else if hasMatchedTask && strings.TrimSpace(matchedTask.Scope) != "" {
			if ovs := extension.LoadActiveFrontDeskOverlays(vaultDir, "task:"+strings.TrimSpace(matchedTask.Scope)); len(ovs) > 0 {
				zone2Sections = append(zone2Sections, "## Companion Task Policy Overlays (Advisory)\n"+trimBytesPreservingLines(strings.Join(ovs, "\n\n"), 900))
			}
		}
	}

	if hasMatchedTask && matchedTask.TaskSlug != "" {
		if _, rawProg, foundProg, pErr := vault.LoadProgress(vaultDir, matchedTask.TaskSlug); pErr == nil && foundProg && strings.TrimSpace(rawProg) != "" {
			progCap := 1200
			if leanAuto {
				progCap = 750
			}
			zone2Sections = append(zone2Sections, "## Active Task Progress Checkpoint\n"+trimBytesPreservingLines(strings.TrimSpace(rawProg), progCap))
		}
	}

	// Scoped ## Active Playbook (1:1 Playbook-Owner, 0 on Front Desk)
	if len(topics) > 0 {
		playbooks := vault.SelectScopedPlaybooks(topics, scopeCtx, vault.MaxPlaybookLines, vault.MaxPlaybookBytes)
		for _, pb := range playbooks {
			pbCap := vault.MaxPlaybookBytes
			if leanAuto {
				pbCap = 850
			}
			pbBody := trimBytesPreservingLines(pb.ActivePlaybook, pbCap)
			zone2Sections = append(zone2Sections, fmt.Sprintf("### Scoped Playbook: %s (`%s`)\n%s", pb.Title, pb.PlaybookOwner, pbBody))
		}
		// 1-line Matched Context Hints (Task & Subagent sessions only; 0 on Front Desk)
		if hints := vault.MatchContextHints(topics, scopeCtx, 2); len(hints) > 0 {
			var hLines []string
			for _, h := range hints {
				hLines = append(hLines, fmt.Sprintf("- **%s**: %s (`read_file` [file](file://%s))", h.Title, trimInlineRunes(h.Summary, 90), h.Path))
			}
			zone2Sections = append(zone2Sections, "## Matched Context Hints\n"+strings.Join(hLines, "\n"))
		}
	}

	// Role-Scoped Active Vault Facts (Deterministic Go Physics + Temporal Precedence Directive)
	if activeFacts, _, _, err := vault.LoadFactsStateAt(vaultDir, now); err == nil && len(activeFacts) > 0 {
		factMaxCount, factMaxBytes := 15, 2200
		if isFrontDesk {
			factMaxCount, factMaxBytes = 12, 2000
		} else if leanAuto {
			factMaxCount, factMaxBytes = 6, 850
		}
		selectedFacts := vault.SelectFactsForSession(activeFacts, scopeCtx, now, factMaxCount, factMaxBytes)
		if len(selectedFacts) > 0 {
			var lines []string
			lines = append(lines, "> **Temporal Precedence:** If the user's current request or live tool output contradicts any stored fact below, treat the live statement/output as ground truth and update via `hq memory add-fact`.")
			for _, f := range selectedFacts {
				lines = append(lines, vault.FormatFactBullet(f))
			}
			zone2Sections = append(zone2Sections, "## Active Vault Facts\n"+strings.Join(lines, "\n"))
		}
	}

	reportedLimit := 6
	if leanAuto {
		reportedLimit = 4
	}
	if reportedDigest := outbox.FormatReportedDigest(vaultDir, now, reportedLimit); reportedDigest != "" {
		repCap := 900
		if leanAuto {
			repCap = 600
		}
		zone2Sections = append(zone2Sections, "## Recently Reported to User\n"+trimBytesPreservingLines(reportedDigest, repCap))
	}

	if !leanAuto {
		_, _ = approvals.ReconcileAskRejections(vaultDir, "", "", now)
		_, _ = approvals.ReconcileLocalGitProposals(vaultDir, now)
		if pol, _, pErr := approvals.LoadPolicy(vaultDir); pErr == nil {
			zone2Sections = append(zone2Sections, approvals.FormatActivePolicySummary(pol))
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
			zone2Sections = append(zone2Sections, "## Pending Approvals in Queue\n"+trimBytesPreservingLines(strings.Join(lines, "\n"), 1200))
		}
		if wts, err := worktree.List(vaultDir); err == nil && len(wts) > 0 {
			var lines []string
			for _, wt := range wts {
				lines = append(lines, fmt.Sprintf("- `%s` (%s)", filepath.Base(wt), wt))
			}
			zone2Sections = append(zone2Sections, "## Active Git Worktrees\n"+trimBytesPreservingLines(strings.Join(lines, "\n"), 900))
		}
		// RFC-0025 provider workspaces: additive sections rendered from the ledger cache only (no provider calls).
		// An unreadable ledger surfaces a warning (and W1 fails closed) while still listing the task's own bindings.
		wsLedger, wsErr := workspace.LoadLedger(vaultDir)
		if wsErr != nil {
			zone2Sections = append(zone2Sections, "## Workspace Ledger Warning\n- ⚠️ `local/memory/workspaces.json` could not be loaded (corrupt, symlinked, or newer schema); workspace edits outside the Vault are blocked fail-closed until repaired.")
		} else if len(wsLedger.Entries) > 0 {
			if allTasks, tErr := kernel.ListTasks(vaultDir); tErr == nil {
				if sec := formatActiveWorkspacesSection(wsLedger, kernel.FilterLayer1Tasks(vaultDir, allTasks), now); sec != "" {
					zone2Sections = append(zone2Sections, sec)
				}
			}
		}
		if hasMatchedTask && !isFrontDesk {
			if sec := formatActiveTaskWorkspacesSection(wsLedger, matchedTask); sec != "" {
				zone2Sections = append(zone2Sections, sec)
			}
		}
		if tasks, err := kernel.ListTasks(vaultDir); err == nil && len(tasks) > 0 {
			layer1Tasks := kernel.FilterLayer1Tasks(vaultDir, tasks)
			selected, omitted := kernel.SelectSessionStartTasks(layer1Tasks, now)
			if len(selected) > 0 {
				var lines []string
				for _, t := range selected {
					cleanTitle := kernel.SanitizeTaskTitle(t.Title, t.TaskSlug)
					lines = append(lines, fmt.Sprintf("- **%s** (`%s`, kind: `%s`, status: `%s`, conv: `%s`)", cleanTitle, t.TaskSlug, t.EffectiveKind(), t.EffectiveStatus(now), t.ConversationID))
				}
				body := trimBytesPreservingLines(strings.Join(lines, "\n"), 1450)
				if omitted > 0 {
					body += fmt.Sprintf("\n- *(+%d older completed/archived task(s) omitted from context; run `hq tasks` to inspect)*", omitted)
				}
				zone2Sections = append(zone2Sections, "## Tracked Task Conversations\n"+body)
			}
		}
		if cfg, err := kernel.LoadEffectiveTriggers(vaultDir); err == nil && len(cfg.Schedules) > 0 {
			fd, _ := kernel.GetFrontDesk(vaultDir)
			daemonActive := kernel.IsDaemonActiveFast(vaultDir)
			var lines []string
			for _, s := range cfg.Schedules {
				status := "UNARMED"
				if !s.IsEnabled() {
					status = "PAUSED"
				} else if daemonActive {
					status = "DAEMON_ACTIVE"
				} else if fd.ArmedTriggers != nil {
					if _, ok := fd.ArmedTriggers[s.ID]; ok {
						status = "ARMED"
					} else if _, okFQ := fd.ArmedTriggers[s.FQID()]; okFQ {
						status = "ARMED"
					}
				}
				lines = append(lines, fmt.Sprintf("- `%s` (`%s`, mode: `%s`, status: `%s`, skill: `%s`)", s.ID, s.Cron, s.EffectiveMode(), status, s.Skill))
			}
			zone2Sections = append(zone2Sections, "## Proactive Triggers (`local/triggers.yaml`)\n"+trimBytesPreservingLines(strings.Join(lines, "\n"), 1100))
			if len(fd.AdHocCrons) > 0 {
				var adHocLines []string
				for _, ac := range fd.AdHocCrons {
					idPart := ""
					if ac.TaskID != "" {
						idPart = fmt.Sprintf("taskId: `%s`, ", ac.TaskID)
					}
					adHocLines = append(adHocLines, fmt.Sprintf("- **%s** (%s`%s`)", ac.Summary, idPart, ac.Cron))
				}
				zone2Sections = append(zone2Sections, "## Ad-Hoc Conversation Crons (Preserved in Front Desk)\n"+trimBytesPreservingLines(strings.Join(adHocLines, "\n"), 600))
			}
		}
	}

	sections := append(zone1Sections, zone2Sections...)

	if len(sections) == 0 {
		_, err := w.Write([]byte("{}\n"))
		return err
	}

	header := "[agy-hq Session Context Loaded]\n\n"
	if leanAuto {
		header = "[agy-hq Lean Auto/Skill Context Loaded]\n\n"
	}
	budget := vault.MaxSessionFullBytes
	if leanAuto {
		budget = vault.MaxSessionLeanBytes
	}
	msg := assembleSessionMessage(header, sections, budget)
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

var reWorktreePathPrompt = regexp.MustCompile(`worktrees/([a-zA-Z0-9._-]+)`)

// readFirstUserPromptFromTranscript performs a bounded <=16KB read of transcript.jsonl to recover Turn-1 user prompt
// when Antigravity SessionStart stdin omits lastUserInput.
func readFirstUserPromptFromTranscript(transcriptPath string) string {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	lr := io.LimitReader(f, 16*1024)
	scanner := bufio.NewScanner(lr)
	scanner.Buffer(make([]byte, 0, 4096), 16*1024)
	for scanner.Scan() {
		var step struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		}
		if json.Unmarshal(scanner.Bytes(), &step) != nil {
			continue
		}
		if step.Type == "USER_INPUT" && strings.TrimSpace(step.Content) != "" {
			return trimInlineRunes(cleanUserPrompt(step.Content), 512)
		}
	}
	return ""
}

// ResolveSessionScopes derives deterministic canonical scopes for SessionStart selection
// with strict repo precedence: (1) WorktreeDir / worktree path in prompt -> (2) Task Scope/RepoRoot -> (3) WorkspacePaths[0].
func ResolveSessionScopes(vaultDir string, payload CommonPayload, matchedTask kernel.ActiveTask, hasMatchedTask, isFrontDesk, isSubagent, isAutoWorker, leanAuto bool, promptText string) vault.SessionScopeContext {
	role := "task"
	switch {
	case isFrontDesk:
		role = "front_desk"
	case isAutoWorker:
		role = "auto"
	case isSubagent:
		role = "subagent"
	}

	ctx := vault.SessionScopeContext{
		Role:       role,
		PromptText: promptText,
		LeanAuto:   leanAuto,
	}

	scopeSet := make(map[string]bool)
	addScope := func(raw string) {
		s := vault.NormalizeScope(raw)
		if s != "" && !scopeSet[s] {
			scopeSet[s] = true
			ctx.Scopes = append(ctx.Scopes, s)
		}
	}

	extractRepoFromWorktree := func(wt string) string {
		base := strings.ToLower(strings.TrimSpace(filepath.Base(filepath.Clean(wt))))
		if base == "" || base == "." || base == "/" {
			return ""
		}
		prefix := base
		if idx := strings.IndexByte(base, '-'); idx > 0 {
			prefix = base[:idx]
		}
		if prefix == "headquarter" || prefix == "agy" {
			return "agy-hq"
		}
		return prefix
	}

	// Precedence 1: matchedTask.WorktreeDir or explicit <vault>/worktrees/<repo>-<task> path in promptText
	if hasMatchedTask && strings.TrimSpace(matchedTask.WorktreeDir) != "" {
		if r := extractRepoFromWorktree(matchedTask.WorktreeDir); r != "" {
			ctx.RepoSlug = r
		}
	}
	if ctx.RepoSlug == "" && promptText != "" {
		if m := reWorktreePathPrompt.FindStringSubmatch(filepath.ToSlash(promptText)); len(m) >= 2 {
			if r := extractRepoFromWorktree(m[1]); r != "" {
				ctx.RepoSlug = r
			}
		}
	}

	// Precedence 2: matchedTask.RepoRoot / matchedTask.Scope
	if hasMatchedTask {
		if ctx.RepoSlug == "" && strings.TrimSpace(matchedTask.RepoRoot) != "" {
			rr := filepath.Clean(matchedTask.RepoRoot)
			if vault.IsAgyHQRepo(rr) {
				ctx.RepoSlug = "agy-hq"
			} else {
				ctx.RepoSlug = strings.ToLower(filepath.Base(rr))
			}
		}
		if sc := strings.ToLower(strings.TrimSpace(matchedTask.Scope)); sc != "" && sc != "general" {
			ctx.TaskScope = sc
			addScope("domain:" + sc)
			if ctx.RepoSlug == "" && (sc == "headquarter" || sc == "agy-hq") {
				ctx.RepoSlug = "agy-hq"
			}
		}
	}

	// Precedence 3: WorkspacePaths[0]
	if ctx.RepoSlug == "" && len(payload.WorkspacePaths) > 0 {
		ws := strings.TrimSpace(payload.WorkspacePaths[0])
		if ws != "" {
			cleanWS := filepath.Clean(ws)
			if isPathInsideVaultWorktrees(cleanWS, vaultDir) {
				ctx.RepoSlug = extractRepoFromWorktree(cleanWS)
			} else if vault.IsAgyHQRepo(cleanWS) {
				ctx.RepoSlug = "agy-hq"
			} else {
				ctx.RepoSlug = strings.ToLower(filepath.Base(cleanWS))
			}
		}
	}

	if ctx.RepoSlug != "" {
		addScope("repo:" + ctx.RepoSlug)
		if ctx.RepoSlug == "agy-hq" {
			addScope("repo:headquarter")
		}
	}

	// Skill & domain cues from task metadata + Turn-1 prompt
	cueText := strings.ToLower(strings.TrimSpace(matchedTask.TaskSlug + " " + matchedTask.Title + " " + promptText))
	skillDomainMap := map[string]string{
		"thesis-check":      "investing",
		"filing-diff":       "investing",
		"swe-pulse":         "swe",
		"gchat":             "comms",
		"gmail":             "comms",
		"tax-prep":          "finance",
		"ledger-ingest":     "finance",
		"morning-brief":     "ops",
		"sleep-consolidate": "ops",
	}
	for sk, dom := range skillDomainMap {
		if strings.Contains(cueText, sk) {
			if ctx.ActiveSkill == "" {
				ctx.ActiveSkill = sk
			}
			addScope("skill:" + sk)
			addScope("domain:" + dom)
		}
	}

	return ctx
}

func trimInlineRunes(s string, maxRunes int) string {
	clean := strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	runes := []rune(clean)
	if len(runes) <= maxRunes {
		return clean
	}
	if maxRunes <= 1 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-1]) + "…"
}

func trimBytesPreservingLines(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	lines := strings.Split(s, "\n")
	var out []string
	used := 0
	const suffix = "\n… *(trimmed to session budget)*"
	limit := maxBytes - len(suffix)
	if limit < 64 {
		return capSessionEnvelope(s, maxBytes)
	}
	for _, line := range lines {
		add := len(line)
		if len(out) > 0 {
			add++
		}
		if used+add > limit {
			if len(out) == 0 {
				return capSessionEnvelope(line, limit) + suffix
			}
			break
		}
		out = append(out, line)
		used += add
	}
	return strings.Join(out, "\n") + suffix
}

func capSessionEnvelope(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	const marker = "\n… *(capped to budget)*"
	cut := maxBytes - len(marker)
	if cut < 32 {
		cut = maxBytes
	}
	// Avoid splitting UTF-8 multi-byte rune
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	if maxBytes-len(marker) < 32 {
		return s[:cut]
	}
	return strings.TrimRight(s[:cut], "\n ") + marker
}

func sectionTrimPriority(sec string) (tier int, floor int) {
	// Tier 0 (trim first): operational chrome, indices, and verbose peripheral lists
	// Tier 1 (trim second): roadmap, task checkpoints, approvals, role banners
	// Tier 2 (protected core): USER.md standing rules, Active Vault Facts, Scoped Playbooks
	switch {
	case strings.HasPrefix(sec, "## HQ Vault: USER.md"),
		strings.HasPrefix(sec, "## Active Vault Facts"),
		strings.HasPrefix(sec, "### Scoped Playbook:"):
		return 2, 1600
	case strings.HasPrefix(sec, "## HQ Vault: MEMORY.md"),
		strings.HasPrefix(sec, "## Active Task Progress Checkpoint"),
		strings.HasPrefix(sec, "## Active Task Workspaces"),
		strings.HasPrefix(sec, "## Pending Approvals in Queue"),
		strings.HasPrefix(sec, "## Matched Context Hints"),
		strings.HasPrefix(sec, "## 🎯 Active Role:"),
		strings.HasPrefix(sec, "## 🚀 New [HQ] Front Desk"),
		strings.HasPrefix(sec, "## ⚠️ Front Desk Notice"):
		return 1, 800
	case strings.HasPrefix(sec, "## Tracked Task Conversations"):
		return 0, 420
	default:
		return 0, 260
	}
}

func trimSectionPreservingFooter(sec string, targetBytes int) string {
	// Preserve the omitted-task summary footer on Tracked Task Conversations (and the overflow footer on
	// Active Workspaces) if present.
	if strings.HasPrefix(sec, "## Tracked Task Conversations") || strings.HasPrefix(sec, "## Active Workspaces") {
		lines := strings.Split(sec, "\n")
		if len(lines) > 2 && (strings.Contains(lines[len(lines)-1], "omitted from context") || strings.Contains(lines[len(lines)-1], "more: hq workspace list")) {
			footer := lines[len(lines)-1]
			headBudget := targetBytes - len(footer) - 2
			if headBudget > 120 {
				head := trimBytesPreservingLines(strings.Join(lines[:len(lines)-1], "\n"), headBudget)
				return head + "\n" + footer
			}
		}
	}
	return trimBytesPreservingLines(sec, targetBytes)
}

func assembleSessionMessage(header string, sections []string, maxBytes int) string {
	full := header + strings.Join(sections, "\n\n")
	if maxBytes <= 0 || len(full) <= maxBytes {
		return full
	}
	// Priority-tiered budget trimming: shrink peripheral operational chrome (tier 0) first,
	// then tier 1, keeping protected core memory (USER.md, Active Vault Facts, Scoped Playbooks) intact.
	trimmed := append([]string(nil), sections...)
	for tier := 0; tier <= 2; tier++ {
		for pass := 0; pass < len(trimmed)*2; pass++ {
			cur := header + strings.Join(trimmed, "\n\n")
			if len(cur) <= maxBytes {
				return cur
			}
			excess := len(cur) - maxBytes
			bestIdx := -1
			bestSlack := 0
			bestFloor := 0
			for i, sec := range trimmed {
				st, fl := sectionTrimPriority(sec)
				if st != tier {
					continue
				}
				if maxBytes <= vault.MaxSessionLeanBytes && fl > 650 {
					fl = 650
				}
				slack := len(sec) - fl
				if slack > bestSlack {
					bestSlack = slack
					bestIdx = i
					bestFloor = fl
				}
			}
			if bestIdx < 0 || bestSlack < 32 {
				break
			}
			targetLen := len(trimmed[bestIdx]) - excess - 24
			if targetLen < bestFloor {
				targetLen = bestFloor
			}
			trimmed[bestIdx] = trimSectionPreservingFooter(trimmed[bestIdx], targetLen)
		}
	}
	return capSessionEnvelope(header+strings.Join(trimmed, "\n\n"), maxBytes)
}

var (
	reHQPropose    = regexp.MustCompile(`(?i)(?:^|&&|;)\s*(?:\S+/)?hq\s+propose\b`)
	reCmdFlagValue = regexp.MustCompile(`--cmd=(?:"(?:\\.|[^"\\])*"|'[^']*'|[^\s;&|$` + "`" + `<>()#'"]+)`)

	reChannelAPIRoute = regexp.MustCompile(`(?i)/api/channels(?:/|%2f|%5c|$|[\s"'])`)

	reDoubleQuotedCmdSubst = regexp.MustCompile(`--cmd="(?:\\.|[^"\\])*(?:\$\(|` + "`" + `)`)
	reHQOutboundBodyCmd    = regexp.MustCompile(`(?i)(?:^|[\s;&|])(?:go\s+run\s+\./cmd/hq|(?:\S+/)?hq)\s+(?:gchat\s+(?:propose|stage-send|draft)|propose|gmail\s+(?:draft|propose))\b`)
	reDoubleQuotedBodyFlag = regexp.MustCompile(`(?:^|\s)--(?:body|message)(?:=|\s+)"((?:\\.|[^"\\])*)"`)
	reSingleShellVarOnly   = regexp.MustCompile(`^\$(?:[A-Za-z_][A-Za-z0-9_]*|\{[A-Za-z_][A-Za-z0-9_]*\})$`)
	reLocalCredentialShell = regexp.MustCompile(`(?i)(?:^|[\s"'=:/])(?:local/(?:private/(?:gmail|gchat|credentials|connectors|channels|runtime|taint|exec-cache|trust\.json|approval_asks|approval_caps|approval_bridge_caps|pii-denylist\.txt|daemon_token)|memory/(?:channels|workspaces\.json|\.workspaces\.lock))|workspaces\.json|\.workspaces\.lock|workspaces\.jso\S*)(?:[\s"'/]|$)`)

	reHeavyBuildTest     = regexp.MustCompile(`(?i)\b(go\s+(?:test|build)|npm\s+(?:test|run\s+build)|pnpm\s+test|pytest|cargo\s+(?:test|build))\b`)
	reGChatBrowserURL    = regexp.MustCompile(`(?i)(?:chat\.google\.com|mail\.google\.com/chat)`)
	reQuarantinedReadCmd = regexp.MustCompile(`^\s*(?:(?:\S+/)?hq)\s+(?:(?:gmail|gchat)\s+read-quarantined|watch\s+read|brief\s+format-quarantined)\b`)
	reHQClaimDesk        = regexp.MustCompile(`(?i)(?:^|[\s;&|])(?:go\s+run\s+\./cmd/hq|(?:\S+/)?hq)\s+claim-desk\b`)
)

const PreCompactionFlushTokens int64 = 120000

func splitShellSegmentsUnquoted(cmd string) []string {
	var out []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				inSingle = false
			}
			cur.WriteByte(ch)
			continue
		}
		if inDouble {
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inDouble = false
			}
			cur.WriteByte(ch)
			continue
		}
		switch ch {
		case '\\':
			escaped = true
			cur.WriteByte(ch)
		case '\'':
			inSingle = true
			cur.WriteByte(ch)
		case '"':
			inDouble = true
			cur.WriteByte(ch)
		case ';', '|', '&', '\n', '\r', '(', ')', '`':
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func tokenizeShellWordsUnquoted(seg string) []string {
	var out []string
	var cur strings.Builder
	inTok := false
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(seg); i++ {
		ch := seg[i]
		if escaped {
			cur.WriteByte(ch)
			inTok = true
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				inSingle = false
			} else {
				cur.WriteByte(ch)
			}
			inTok = true
			continue
		}
		if inDouble {
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inDouble = false
			} else {
				cur.WriteByte(ch)
			}
			inTok = true
			continue
		}
		switch ch {
		case ' ', '\t':
			if inTok {
				out = append(out, cur.String())
				cur.Reset()
				inTok = false
			}
		case '\\':
			escaped = true
			inTok = true
		case '\'':
			inSingle = true
			inTok = true
		case '"':
			inDouble = true
			inTok = true
		default:
			cur.WriteByte(ch)
			inTok = true
		}
	}
	if inTok {
		out = append(out, cur.String())
	}
	return out
}

// isBlockedChannelGatewayShell structurally detects quote/env/`go run`-wrapped invocations of
// `hq channel inbound|callback|flush|apply-config` or `/api/channels/*` across all LLM sessions.
func isBlockedChannelGatewayShell(cmdLine string) bool {
	if reChannelAPIRoute.MatchString(cmdLine) {
		return true
	}
	for _, seg := range splitShellSegmentsUnquoted(cmdLine) {
		toks := tokenizeShellWordsUnquoted(seg)
		// Strip leading env assignments (`FOO=bar`), `env [-u X / --unset=X / -i / --]`, `command`, `exec`, `nohup`, `nice`, `timeout <dur>`
		i := 0
		for i < len(toks) {
			t := toks[i]
			if strings.Contains(t, "=") && !strings.HasPrefix(t, "-") {
				i++
				continue
			}
			switch t {
			case "env":
				i++
				for i < len(toks) {
					et := toks[i]
					if et == "--" {
						i++
						break
					}
					if et == "-u" || et == "--unset" || et == "-C" || et == "--chdir" || et == "-S" {
						i += 2
						continue
					}
					if strings.HasPrefix(et, "-") || strings.Contains(et, "=") {
						i++
						continue
					}
					break
				}
				continue
			case "command", "exec", "nohup", "builtin":
				i++
				for i < len(toks) && strings.HasPrefix(toks[i], "-") {
					i++
				}
				continue
			case "nice":
				i++
				if i < len(toks) && (toks[i] == "-n" || strings.HasPrefix(toks[i], "--adjustment")) {
					if toks[i] == "-n" {
						i += 2
					} else {
						i++
					}
				}
				continue
			case "timeout":
				i++
				for i < len(toks) && strings.HasPrefix(toks[i], "-") {
					if toks[i] == "-k" || toks[i] == "-s" {
						i += 2
					} else {
						i++
					}
				}
				if i < len(toks) {
					i++ // duration arg
				}
				continue
			}
			break
		}
		if i >= len(toks) {
			continue
		}
		rest := toks[i:]
		// Normalize `go run [flags] ./cmd/hq[/] <args...>` -> `hq <args...>`
		base0 := strings.ToLower(strings.TrimSuffix(filepath.Base(rest[0]), ".exe"))
		if base0 == "go" && len(rest) >= 3 && rest[1] == "run" {
			j := 2
			for j < len(rest) && strings.HasPrefix(rest[j], "-") {
				j++
			}
			if j < len(rest) {
				pkgClean := strings.TrimSuffix(filepath.ToSlash(strings.TrimSpace(rest[j])), "/")
				if strings.HasSuffix(pkgClean, "cmd/hq") {
					rest = append([]string{"hq"}, rest[j+1:]...)
					base0 = "hq"
				}
			}
		}
		if base0 != "hq" {
			continue
		}
		// Find first positional subcommands after `hq`
		var pos []string
		for _, a := range rest[1:] {
			if a == "--" {
				break
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			pos = append(pos, strings.ToLower(strings.TrimSpace(a)))
			if len(pos) >= 2 {
				break
			}
		}
		if len(pos) >= 2 && pos[0] == "channel" {
			switch pos[1] {
			case "inbound", "callback", "flush", "apply-config":
				return true
			}
		}
	}
	return false
}

func isAutoConversation(vaultDir, convID string) bool {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return false
	}
	t, ok := kernel.FindTaskByConversationID(vaultDir, convID)
	return ok && t.EffectiveKind() == "auto"
}

func containsUnescapedDollar(s string) bool {
	escaped := false
	for i := 0; i < len(s); i++ {
		if escaped {
			escaped = false
			continue
		}
		if s[i] == '\\' {
			escaped = true
			continue
		}
		if s[i] == '$' {
			return true
		}
	}
	return false
}

// hasUnsafeDoubleQuotedDollar returns true if a double-quoted --body or --message
// argument on an hq outbound/draft command contains an unescaped '$' that bash will
// expand before hq receives it (e.g. "$382.34" -> "82.34"). An exact single-variable
// reference like "$MSG" or "${MSG}" is permitted.
func hasUnsafeDoubleQuotedDollar(cmdLine string) bool {
	if !reHQOutboundBodyCmd.MatchString(cmdLine) {
		return false
	}
	for _, m := range reDoubleQuotedBodyFlag.FindAllStringSubmatch(cmdLine, -1) {
		if len(m) < 2 {
			continue
		}
		inner := m[1]
		if reSingleShellVarOnly.MatchString(inner) {
			continue
		}
		if containsUnescapedDollar(inner) {
			return true
		}
	}
	return false
}

func extractProposeCmdForValidation(cmdLine string) (hasCmd bool, cmdVal string, argv []string) {
	if strings.Contains(cmdLine, "--draft") || strings.Contains(cmdLine, "--rsi-id") {
		return false, "", nil
	}
	if m := reCmdFlagValue.FindString(cmdLine); m != "" {
		raw := strings.TrimPrefix(m, "--cmd=")
		if len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'' {
			raw = raw[1 : len(raw)-1]
		} else if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
			raw = strings.ReplaceAll(strings.ReplaceAll(raw[1:len(raw)-1], `\"`, `"`), `\\`, `\`)
		}
		return true, raw, nil
	}
	if idx := strings.Index(cmdLine, " -- "); idx >= 0 {
		rest := strings.TrimSpace(cmdLine[idx+4:])
		if rest != "" {
			return true, "", strings.Fields(rest)
		}
	}
	return false, "", nil
}

// hasUnquotedShellChainingOrSubst reports whether s contains any shell chaining/piping operator outside quotes
// (;, &, |, newline), process substitution (<(...), >(...)), command substitution ($(...) or backtick) outside
// single quotes, bash ANSI-C/locale quoting ($'...', $"..."), unquoted '#' comments (which can mask quote
// desynchronization), or an unclosed quote / trailing escape (fail-closed).
func hasUnquotedShellChainingOrSubst(s string) bool {
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inDouble = false
				continue
			}
			if ch == '`' || (ch == '$' && i+1 < len(s) && s[i+1] == '(') {
				return true
			}
			continue
		}
		switch ch {
		case '\\':
			escaped = true
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case ';', '&', '|', '\n', '\r', '`', '#':
			return true
		case '<', '>':
			if i+1 < len(s) && s[i+1] == '(' {
				return true
			}
		case '$':
			if i+1 < len(s) && (s[i+1] == '(' || s[i+1] == '\'' || s[i+1] == '"') {
				return true
			}
		}
	}
	return inSingle || inDouble || escaped
}

// stripSafeProposeCmdFlag replaces `--cmd=<val>` with `--cmd=""` only when `<val>` is cleanly bounded
// (single-quoted, double-quoted without command substitution, or unquoted without shell metacharacters)
// and immediately followed by whitespace or end-of-string. Anything suspicious (e.g. `--cmd=hq;evil` or
// `--cmd=$(evil)`) is left unstripped so EvaluateCommand inspects and blocks it fail-closed.
func stripSafeProposeCmdFlag(cmdLine string) string {
	var out strings.Builder
	out.Grow(len(cmdLine))
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(cmdLine); {
		ch := cmdLine[i]
		if escaped {
			out.WriteByte(ch)
			escaped = false
			i++
			continue
		}
		if inSingle {
			out.WriteByte(ch)
			if ch == '\'' {
				inSingle = false
			}
			i++
			continue
		}
		if inDouble {
			out.WriteByte(ch)
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inDouble = false
			}
			i++
			continue
		}
		if strings.HasPrefix(cmdLine[i:], "--cmd=") && (i == 0 || cmdLine[i-1] == ' ' || cmdLine[i-1] == '\t') {
			valStart := i + len("--cmd=")
			if valStart < len(cmdLine) {
				switch cmdLine[valStart] {
				case '\'':
					end := strings.IndexByte(cmdLine[valStart+1:], '\'')
					if end >= 0 {
						after := valStart + 1 + end + 1
						if after == len(cmdLine) || cmdLine[after] == ' ' || cmdLine[after] == '\t' {
							out.WriteString(`--cmd=""`)
							i = after
							continue
						}
					}
				case '"':
					j := valStart + 1
					safe := true
					closed := false
					for j < len(cmdLine) {
						c := cmdLine[j]
						if c == '\\' && j+1 < len(cmdLine) {
							j += 2
							continue
						}
						if c == '`' || (c == '$' && j+1 < len(cmdLine) && cmdLine[j+1] == '(') {
							safe = false
							break
						}
						if c == '"' {
							closed = true
							j++
							break
						}
						j++
					}
					if safe && closed && (j == len(cmdLine) || cmdLine[j] == ' ' || cmdLine[j] == '\t') {
						out.WriteString(`--cmd=""`)
						i = j
						continue
					}
				default:
					j := valStart
					safe := true
					for j < len(cmdLine) && cmdLine[j] != ' ' && cmdLine[j] != '\t' {
						c := cmdLine[j]
						if strings.ContainsRune(";&|$`<>()#'\"\\\n\r", rune(c)) {
							safe = false
							break
						}
						j++
					}
					if safe && j > valStart && (j == len(cmdLine) || cmdLine[j] == ' ' || cmdLine[j] == '\t') {
						out.WriteString(`--cmd=""`)
						i = j
						continue
					}
				}
			}
		}
		switch ch {
		case '\\':
			escaped = true
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		}
		out.WriteByte(ch)
		i++
	}
	return out.String()
}

func stripProposeInnerCommand(cmdLine string) string {
	stripped := stripSafeProposeCmdFlag(cmdLine)
	if idx := strings.Index(stripped, " -- "); idx >= 0 {
		prefix := stripped[:idx]
		tail := stripped[idx+4:]
		if !hasUnquotedShellChainingOrSubst(prefix) && !hasUnquotedShellChainingOrSubst(tail) {
			stripped = prefix
		}
	}
	return stripped
}

func isStandardOrSpoofedApprovalOption(opt string) bool {
	lower := strings.ToLower(strings.TrimSpace(opt))
	if lower == "" || lower == "keep pending" {
		return true
	}
	for _, pref := range []string{
		"approve", "(recommended) approve", "✅ approve",
		"reject", "❌ reject",
	} {
		if lower == pref || strings.HasPrefix(lower, pref+" ") || strings.HasPrefix(lower, pref+"(") || strings.HasPrefix(lower, pref+":") {
			return true
		}
	}
	return false
}

func buildCanonicalProposalQuestion(vaultDir string, prop approvals.Proposal, convID, transcriptPath string, origOpts []string) map[string]interface{} {
	hasPIIWarning := len(prop.RiskFlags) > 0
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
	} else if prop.ActionType == "rsi_patch" || prop.ActionType == "rsi_skill_patch" || prop.ActionType == "rsi_overlay" {
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
	approveOpt := fmt.Sprintf("(Recommended) Approve (%s · sha256:%s)", prop.ID, shaShort)
	if askRec, aErr := approvals.RecordAsk(vaultDir, prop.ID, convID, transcriptPath, time.Now()); aErr == nil && askRec.NonceCode != "" {
		approveOpt = fmt.Sprintf("(Recommended) Approve (%s · sha256:%s · code:%s)", prop.ID, shaShort, askRec.NonceCode)
	}

	opts := []string{
		approveOpt,
		fmt.Sprintf("Reject (%s)", prop.ID),
	}
	var customOpts []string
	seenCustom := make(map[string]bool)
	for _, rawOpt := range origOpts {
		clean := strings.TrimSpace(rawOpt)
		if isStandardOrSpoofedApprovalOption(clean) || strings.Contains(strings.ToLower(clean), "code:") || strings.Contains(clean, prop.ID) {
			continue
		}
		key := strings.ToLower(clean)
		if !seenCustom[key] {
			seenCustom[key] = true
			customOpts = append(customOpts, clean)
		}
	}
	if len(customOpts) > 0 {
		opts = append(opts, customOpts...)
	} else {
		opts = append(opts, "Keep pending")
	}

	return map[string]interface{}{
		"question":        canonicalQ,
		"is_multi_select": false,
		"options":         opts,
	}
}

func askQuestionsReferenceProposal(args map[string]interface{}, pending []approvals.Proposal) string {
	rawList, ok := args["questions"].([]interface{})
	if !ok || len(rawList) == 0 || len(pending) == 0 {
		return ""
	}
	for _, item := range rawList {
		qMap, isMap := item.(map[string]interface{})
		if !isMap {
			continue
		}
		qText, _ := qMap["question"].(string)
		for idx := range pending {
			pid := pending[idx].ID
			if strings.Contains(qText, pid) {
				return pid
			}
			if rawOpts, hasOpts := qMap["options"].([]interface{}); hasOpts {
				for _, o := range rawOpts {
					if s, okS := o.(string); okS && strings.Contains(s, pid) {
						return pid
					}
				}
			} else if strOpts, hasStrOpts := qMap["options"].([]string); hasStrOpts {
				for _, s := range strOpts {
					if strings.Contains(s, pid) {
						return pid
					}
				}
			}
		}
	}
	return ""
}

func rewriteAskQuestionsSurgical(vaultDir string, args map[string]interface{}, pending []approvals.Proposal, convID, transcriptPath string) (map[string]interface{}, bool) {
	rawList, ok := args["questions"].([]interface{})
	if !ok || len(rawList) == 0 {
		return nil, false
	}
	updatedList := make([]interface{}, len(rawList))
	modifiedAny := false

	for i, item := range rawList {
		qMap, isMap := item.(map[string]interface{})
		if !isMap {
			updatedList[i] = item
			continue
		}
		qText, _ := qMap["question"].(string)
		var origOpts []string
		if rawOpts, hasOpts := qMap["options"].([]interface{}); hasOpts {
			for _, o := range rawOpts {
				if s, okS := o.(string); okS {
					origOpts = append(origOpts, s)
				}
			}
		} else if strOpts, hasStrOpts := qMap["options"].([]string); hasStrOpts {
			origOpts = append(origOpts, strOpts...)
		}

		var matchedProp *approvals.Proposal
		for idx := range pending {
			pid := pending[idx].ID
			if strings.Contains(qText, pid) {
				matchedProp = &pending[idx]
				break
			}
			for _, o := range origOpts {
				if strings.Contains(o, pid) {
					matchedProp = &pending[idx]
					break
				}
			}
			if matchedProp != nil {
				break
			}
		}

		if matchedProp != nil {
			updatedList[i] = buildCanonicalProposalQuestion(vaultDir, *matchedProp, convID, transcriptPath, origOpts)
			modifiedAny = true
		} else {
			updatedList[i] = qMap
		}
	}

	if !modifiedAny {
		return nil, false
	}
	overwriteArgs := make(map[string]interface{}, len(args))
	for k, v := range args {
		overwriteArgs[k] = v
	}
	overwriteArgs["questions"] = updatedList
	return overwriteArgs, true
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
			if tm.CurrentContextTokens >= PreCompactionFlushTokens {
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
	_, err := w.Write([]byte(`{"decision":"allow"}` + "\n"))
	return err
}

func hasPendingResumableApproval(vaultDir, convID string) bool {
	if strings.TrimSpace(convID) == "" {
		return false
	}
	pending, err := approvals.List(vaultDir)
	if err != nil {
		return false
	}
	for _, p := range pending {
		if p.ConversationID == convID && p.ResumeOnDecide {
			return true
		}
	}
	return false
}

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

func isSystemGeneratedTranscriptPath(cleanSlash string) bool {
	return strings.Contains(cleanSlash, "/.system_generated/") || strings.HasSuffix(cleanSlash, "/.system_generated")
}

func resolveHookLexicalAbs(vaultDir, rawPath string) string {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return ""
	}
	p := trimmed
	if !filepath.IsAbs(p) {
		rootDir := filepath.Dir(filepath.Clean(vaultDir))
		p = filepath.Join(rootDir, p)
	}
	return filepath.Clean(p)
}

func pathHasSymlink(rawPath string) bool {
	cur := filepath.Clean(rawPath)
	for {
		if st, err := os.Lstat(cur); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

// isSafePathUnderBase verifies that rawTargetPath resolves canonically inside rawBaseDir,
// matches rawBaseDir lexically (accommodating OS/temp/home/vault root symlinks above rawBaseDir,
// such as macOS /var -> /private/var or enterprise symlinked $HOME), and traverses zero
// symlinks (live or dangling) at or below rawBaseDir.
func isSafePathUnderBase(vaultDir, rawBaseDir, rawTargetPath string) (string, bool) {
	absBase := resolveHookLexicalAbs(vaultDir, rawBaseDir)
	absTarget := resolveHookLexicalAbs(vaultDir, rawTargetPath)
	if absBase == "" || absTarget == "" {
		return "", false
	}
	canonBase := canonicalizeHookPath(vaultDir, absBase)
	canonTarget := canonicalizeHookPath(vaultDir, absTarget)
	if canonBase == "" || canonBase == "." || canonTarget == "" || canonTarget == "." {
		return "", false
	}
	if pathHasSymlink(canonBase) || pathHasSymlink(canonTarget) {
		return "", false
	}
	sep := string(filepath.Separator)
	if canonTarget != canonBase && !strings.HasPrefix(canonTarget, canonBase+sep) {
		return "", false
	}

	candidates := []string{absBase, canonBase}
	trustedRoots := []string{
		filepath.Clean(os.TempDir()),
		"/var",
		"/tmp",
	}
	if strings.TrimSpace(vaultDir) != "" {
		cleanV := filepath.Clean(vaultDir)
		trustedRoots = append(trustedRoots, cleanV, filepath.Dir(cleanV))
	}
	if home, err := hostpaths.ResolveHomeDir(); err == nil && home != "" {
		trustedRoots = append(trustedRoots, filepath.Clean(home))
	}
	for _, tr := range trustedRoots {
		if tr == "" || tr == "." || tr == sep {
			continue
		}
		canonTR := canonicalizeHookPath(vaultDir, tr)
		if canonTR != "" && canonTR != tr {
			if canonBase == canonTR {
				candidates = append(candidates, tr)
			} else if strings.HasPrefix(canonBase, canonTR+sep) {
				candidates = append(candidates, tr+strings.TrimPrefix(canonBase, canonTR))
			}
		}
	}

	matchedRel := ""
	matched := false
	for _, cand := range candidates {
		if cand == "" || cand == "." {
			continue
		}
		rel, err := filepath.Rel(cand, absTarget)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+sep)) {
			matchedRel = rel
			matched = true
			break
		}
	}
	if !matched {
		return "", false
	}

	if matchedRel != "." {
		cur := canonBase
		for _, seg := range strings.Split(filepath.ToSlash(matchedRel), "/") {
			if seg == "" || seg == "." {
				continue
			}
			cur = filepath.Join(cur, seg)
			if st, err := os.Lstat(cur); err == nil && st.Mode()&os.ModeSymlink != 0 {
				return "", false
			}
		}
	}
	return filepath.ToSlash(matchedRel), true
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
		return json.NewEncoder(w).Encode(map[string]interface{}{
			"decision": "deny",
			"reason":   "Blocked by agy-hq PreToolUse safety policy: malformed hook payload JSON (fail-closed).",
		})
	}

	if strings.TrimSpace(payload.ConversationID) == "" {
		payload.ConversationID = strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	}
	if strings.TrimSpace(payload.ConversationID) == "" {
		return json.NewEncoder(w).Encode(map[string]interface{}{
			"decision": "deny",
			"reason":   "Blocked by agy-hq PreToolUse safety policy: missing conversationId in hook payload (fail-closed).",
		})
	}

	_ = kernel.RecordLiveLSSession(vaultDir)
	bindSpawnTokenFromTranscript(vaultDir, payload.ConversationID, payload.LastUserInput, payload.TranscriptPath)
	convID := payload.ConversationID
	defer func() {
		_, _ = events.TryRenewLeaseByConversation(vaultDir, convID, time.Now(), events.DefaultLeaseTTL)
	}()

	protectedPrivateSubs := []string{
		"gmail", "gchat", "credentials", "connectors", "channels", "runtime", "taint", "exec-cache",
		"trust.json", "approval_asks", "approval_caps", "approval_bridge_caps", "pii-denylist.txt", "daemon_token",
	}
	var extraCredTargets []string
	if rts := hostpaths.Resolve(vaultDir); rts.AppDataDir != "" {
		extraCredTargets = append(extraCredTargets, canonicalizeHookPath(vaultDir, filepath.Join(rts.AppDataDir, "hq", "roots.json")))
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		extraCredTargets = append(extraCredTargets, canonicalizeHookPath(vaultDir, filepath.Join(xdg, "agy-hq", "roots.json")))
	}

	if payload.ToolCall.Name == "run_command" {
		cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
		if isSystemGeneratedTranscriptPath(filepath.ToSlash(cmdLine)) {
			return json.NewEncoder(w).Encode(map[string]interface{}{
				"decision": "deny",
				"reason":   "Blocked by agy-hq integrity policy: direct shell access to .system_generated transcript/log directories is prohibited across all sessions.",
			})
		}
		if isBlockedChannelGatewayShell(cmdLine) {
			return json.NewEncoder(w).Encode(map[string]interface{}{
				"decision": "deny",
				"reason":   "Blocked by agy-hq Channel Bridge policy: `hq channel inbound|callback|flush|apply-config` and `/api/channels/*` are daemon/approval-only gateways and may never be invoked directly by an LLM session (use `hq channel configure` to stage config updates).",
			})
		}
		if reHQClaimDesk.MatchString(cmdLine) && (kernel.IsSubagentConversation(vaultDir, payload.ConversationID) || isAutoConversation(vaultDir, payload.ConversationID)) {
			return json.NewEncoder(w).Encode(map[string]interface{}{
				"decision": "deny",
				"reason":   "Blocked by Front Desk Lease Subagent Invariant: subagents and autonomous background workers are ephemeral processes. They must never attempt to claim, adopt, or modify the Front Desk intake lease (claim-desk is strictly prohibited for subagents).",
			})
		}
		// Universal Credential & Trust Vault Guard: block direct shell reads/mutations of raw OAuth tokens, connectors, taint, trust, approvals/payloads, approval_asks/caps, daemon_token, pii-denylist.txt, or roots.json across ALL sessions
		cleanVault := filepath.Clean(vaultDir)
		canonVault := canonicalizeHookPath(vaultDir, vaultDir)
		credShellHit := reLocalCredentialShell.MatchString(cmdLine)
		if !credShellHit {
			for _, rt := range extraCredTargets {
				if rt != "" && strings.Contains(cmdLine, rt) {
					credShellHit = true
					break
				}
			}
		}
		if !credShellHit {
			vaultBases := []string{cleanVault}
			if canonVault != "" && canonVault != cleanVault {
				vaultBases = append(vaultBases, canonVault)
			}
			for _, vBase := range vaultBases {
				if vBase == "" || vBase == "." {
					continue
				}
				if strings.Contains(cmdLine, filepath.Join(vBase, "approvals")) ||
					strings.Contains(cmdLine, filepath.Join(vBase, "memory", "channels")) ||
					strings.Contains(cmdLine, filepath.Join(vBase, "memory", "workspaces.json")) ||
					strings.Contains(cmdLine, filepath.Join(vBase, "memory", ".workspaces.lock")) {
					credShellHit = true
					break
				}
				for _, sub := range protectedPrivateSubs {
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
				filepath.Join(canonVault, "approvals"),
				filepath.Join(canonVault, "memory", "channels"),
				filepath.Join(canonVault, "memory", "workspaces.json"),
				filepath.Join(canonVault, "memory", ".workspaces.lock"),
			}
			for _, sub := range protectedPrivateSubs {
				credTargets = append(credTargets, filepath.Join(canonVault, "private", sub))
			}
			for _, rt := range extraCredTargets {
				if rt != "" {
					credTargets = append(credTargets, rt)
				}
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
				"reason":   "Blocked by agy-hq Credential Vault policy: direct shell access to local/private/{gmail,gchat,credentials,connectors,channels,taint,trust.json,approval_asks,approval_caps,approval_bridge_caps,pii-denylist.txt,daemon_token}, local/memory/{channels,workspaces.json,.workspaces.lock}, or roots.json is strictly prohibited across all sessions.",
			})
		}

		// Canonical Human-Presence Gate on `hq approve` (N1 / S1 / S2)
		if reHQApproveAny.MatchString(cmdLine) {
			if _, isTaintedCaller := taint.Check(vaultDir, payload.ConversationID); isTaintedCaller || isAutoConversation(vaultDir, payload.ConversationID) || kernel.IsSubagentConversation(vaultDir, payload.ConversationID) {
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision": "deny",
					"reason":   "Blocked by agy-hq Human-Presence Approval policy: subagents, autonomous [Auto] runs, and quarantined sessions may never execute `hq approve`.",
				})
			}
			binTok, propID, shaPrefix, parseErr := parseCanonicalHQApprove(cmdLine)
			if parseErr != nil {
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision": "deny",
					"reason":   fmt.Sprintf("Blocked by agy-hq Human-Presence Approval policy: %v", parseErr),
				})
			}
			capTok, fullSHA, vErr := approvals.VerifyAskAndMintCap(vaultDir, propID, shaPrefix, payload.ConversationID, payload.TranscriptPath, time.Now())
			if vErr != nil {
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision": "deny",
					"reason":   fmt.Sprintf("Blocked by agy-hq Human-Presence Approval policy: %v", vErr),
				})
			}
			rewritten := fmt.Sprintf("%s approve %s --sha256=%s --cap=%s",
				shellQuoteSingle(resolveTrustedHQExecutable(binTok)),
				shellQuoteSingle(propID),
				shellQuoteSingle(fullSHA),
				shellQuoteSingle(capTok),
			)
			return json.NewEncoder(w).Encode(map[string]interface{}{
				"decision":  "allow",
				"overwrite": map[string]interface{}{"CommandLine": rewritten},
			})
		}
		if hasUnsafeDoubleQuotedDollar(cmdLine) {
			return json.NewEncoder(w).Encode(map[string]interface{}{
				"decision": "deny",
				"reason":   "Blocked by agy-hq outbound quote-safety policy: double-quoted --body/--message containing '$' risks silent bash variable expansion (e.g. \"$382.34\" -> \"82.34\"). Use --body-stdin with single-quoted heredoc instead: `cat <<'EOF' | ./bin/hq gchat propose --to=\"...\" --body-stdin`.",
			})
		}
		isAuto := isAutoConversation(vaultDir, payload.ConversationID)
		if isAuto && (strings.Contains(cmdLine, "config.yaml") || strings.Contains(cmdLine, "hq-extension.json")) {
			if strings.ContainsAny(cmdLine, ">") ||
				strings.Contains(cmdLine, "sed ") ||
				strings.Contains(cmdLine, "tee ") ||
				strings.Contains(cmdLine, "cp ") ||
				strings.Contains(cmdLine, "mv ") ||
				strings.Contains(cmdLine, "rm ") ||
				strings.Contains(cmdLine, "python") ||
				strings.Contains(cmdLine, "perl") {
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision": "deny",
					"reason":   "Blocked by agy-hq policy self-tamper guard: autonomous [Auto] sessions cannot modify config.yaml or hq-extension.json.",
				})
			}
		}

		pol, _, _ := approvals.LoadPolicy(vaultDir)
		checkLine := cmdLine
		if reHQPropose.MatchString(cmdLine) {
			if reDoubleQuotedCmdSubst.MatchString(cmdLine) {
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision": "deny",
					"reason":   "Blocked by agy-hq approval policy: command substitution ($(...) or backticks) inside double-quoted `hq propose --cmd=\"...\"` executes immediately before approval. Use single quotes (`--cmd='...'`) or literal strings.",
				})
			}
			if pol.RejectNoopCommands {
				if hasCmd, cmdVal, argvVal := extractProposeCmdForValidation(cmdLine); hasCmd {
					if vErr := approvals.ValidateDeterministicCommand(cmdVal, argvVal); vErr != nil {
						return json.NewEncoder(w).Encode(map[string]interface{}{
							"decision": "deny",
							"reason":   fmt.Sprintf("Blocked by agy-hq approval policy: %v", vErr),
						})
					}
				}
			}
			checkLine = stripProposeInnerCommand(cmdLine)
		}

		workDir := ""
		if len(payload.WorkspacePaths) > 0 {
			workDir = payload.WorkspacePaths[0]
		}
		if eval := approvals.EvaluateCommand(pol, checkLine, isAuto, workDir); eval.Gated {
			var reason string
			if isAuto {
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
			return json.NewEncoder(w).Encode(map[string]interface{}{
				"decision": "deny",
				"reason":   reason,
			})
		}
	}

	// Universal Credential Vault Read/Write Guard: protect raw OAuth tokens, connectors, taint, trust, approvals/payloads, approval_asks/caps, daemon_token, pii-denylist.txt, and roots.json across ALL sessions (tainted or untainted)
	switch payload.ToolCall.Name {
	case "read_file", "write_file", "edit_file", "grep_search", "find_by_name":
		canonVault := canonicalizeHookPath(vaultDir, vaultDir)
		credPaths := []string{
			filepath.Join(canonVault, "approvals"),
			filepath.Join(canonVault, "memory", "channels"),
		}
		for _, sub := range protectedPrivateSubs {
			credPaths = append(credPaths, filepath.Join(canonVault, "private", sub))
		}
		for _, rt := range extraCredTargets {
			if rt != "" {
				credPaths = append(credPaths, rt)
			}
		}
		for _, key := range []string{"Path", "SearchPath", "SearchDirectory"} {
			if rawArg, ok := payload.ToolCall.Args[key].(string); ok && strings.TrimSpace(rawArg) != "" {
				canonP := canonicalizeHookPath(vaultDir, rawArg)
				for _, cp := range credPaths {
					if canonP == cp || strings.HasPrefix(canonP, cp+string(filepath.Separator)) {
						return json.NewEncoder(w).Encode(map[string]interface{}{
							"decision": "deny",
							"reason":   "Blocked by agy-hq Credential Vault policy: direct LLM access to protected Vault secrets/trust/taint/approvals or roots.json is strictly prohibited across all sessions.",
						})
					}
				}
			}
		}
	}

	// Protect deterministic proposal, RSI verification, transcript logs (.system_generated), extensions.json, and durable event/timer/outbox/watcher stores from direct LLM file tampering across all sessions
	if payload.ToolCall.Name == "write_file" || payload.ToolCall.Name == "edit_file" {
		if pathArg, ok := payload.ToolCall.Args["Path"].(string); ok && strings.TrimSpace(pathArg) != "" {
			canonPath := canonicalizeHookPath(vaultDir, pathArg)
			rawCleanSlash := filepath.ToSlash(filepath.Clean(pathArg))
			canonSlash := filepath.ToSlash(canonPath)
			if isSystemGeneratedTranscriptPath(rawCleanSlash) || isSystemGeneratedTranscriptPath(canonSlash) {
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision": "deny",
					"reason":   "Blocked by agy-hq integrity policy: direct file writes inside .system_generated transcript/log directories are prohibited across all sessions.",
				})
			}
			canonVault := canonicalizeHookPath(vaultDir, vaultDir)
			for _, protectedSub := range []string{
				filepath.Join(canonVault, "extensions.json"),
				filepath.Join(canonVault, "approvals"),
				filepath.Join(canonVault, "memory", "pending"),
				filepath.Join(canonVault, "memory", "rsi", "hypotheses"),
				filepath.Join(canonVault, "memory", "rsi", "rules"),
				filepath.Join(canonVault, "memory", "rsi", "archive.jsonl"),
				filepath.Join(canonVault, "memory", "interventions.jsonl"),
				filepath.Join(canonVault, "memory", "events.json"),
				filepath.Join(canonVault, "memory", "events_archive.jsonl"),
				filepath.Join(canonVault, "memory", "timers.json"),
				filepath.Join(canonVault, "memory", "outbox"),
				filepath.Join(canonVault, "memory", "channels"),
				filepath.Join(canonVault, "memory", "watcher"),
				filepath.Join(canonVault, "memory", "watchers"),
				filepath.Join(canonVault, "memory", "trigger_cursors.json"),
				filepath.Join(canonVault, "memory", "front_desk.json"),
				filepath.Join(canonVault, "memory", "tasks.json"),
				filepath.Join(canonVault, "memory", "workspaces.json"),
				filepath.Join(canonVault, "memory", ".workspaces.lock"),
			} {
				if canonPath == protectedSub || strings.HasPrefix(canonPath, protectedSub+string(filepath.Separator)) {
					return json.NewEncoder(w).Encode(map[string]interface{}{
						"decision": "deny",
						"reason":   "Blocked by agy-hq integrity policy: direct file writes to pending approval proposals, extension state, RSI hypotheses, durable event/timer/outbox/watcher stores, task/workspace ledgers, or intervention audit logs are prohibited.",
					})
				}
			}

			canonCfgPath := filepath.Join(canonVault, "config.yaml")
			if canonPath == canonCfgPath || filepath.Base(canonPath) == "hq-extension.json" {
				if isAutoConversation(vaultDir, payload.ConversationID) {
					return json.NewEncoder(w).Encode(map[string]interface{}{
						"decision": "deny",
						"reason":   "Blocked by agy-hq policy self-tamper guard: autonomous [Auto] sessions cannot modify config.yaml or hq-extension.json.",
					})
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
							return json.NewEncoder(w).Encode(map[string]interface{}{
								"decision": "deny",
								"reason":   fmt.Sprintf("Blocked by agy-hq approval policy validator: invalid approvals configuration in config.yaml: %v", vErr),
							})
						}
					}
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
				"reason":   "Blocked by agy-hq Google Chat safety policy: direct chrome-devtools interaction with chat.google.com is forbidden. Use `./bin/hq gchat <spaces|search|read-quarantined|propose>` instead.",
			})
		}
	}

	// Enforce Strict Deny-by-Default Quarantined Worker Profile on sessions that have ingested untrusted external channel content (RFC-0017b §3)
	if taintReason, tainted := taint.IsTainted(vaultDir, payload.ConversationID); tainted {
		switch payload.ToolCall.Name {
		case "update_focus":
			_, err := w.Write([]byte(`{"decision":"allow"}` + "\n"))
			return err
		case "read_file":
			pathArg, _ := payload.ToolCall.Args["Path"].(string)
			if !isAllowedTaintedReadPath(vaultDir, payload, pathArg) {
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision": "deny",
					"reason":   fmt.Sprintf("Blocked by agy-hq Quarantined Reader policy (%s): read_file in tainted sessions is restricted to SKILL.md instructions and this conversation's own artifact directory.", taintReason),
				})
			}
			_, err := w.Write([]byte(`{"decision":"allow"}` + "\n"))
			return err
		case "run_command":
			cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
			argv, aErr := strictArgv(cmdLine)
			if aErr != nil {
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision": "deny",
					"reason":   fmt.Sprintf("Blocked by agy-hq Quarantined Reader policy (%s): %v. Invoke single canonical 'hq ...' commands directly without shell loops, ';', '&&', or pipes (use comma-separated --thread=t1,t2 on 'hq watch read' and --all or --item=i1,i2 on 'hq watch ack').", taintReason, aErr),
				})
			}
			if vErr := validateTaintedHQArgv(argv); vErr != nil {
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision": "deny",
					"reason":   fmt.Sprintf("Blocked by agy-hq Quarantined Reader policy (%s): %v", taintReason, vErr),
				})
			}
			return json.NewEncoder(w).Encode(map[string]interface{}{
				"decision":  "allow",
				"overwrite": map[string]interface{}{"CommandLine": wrapScrubbedArgvCommand(argv)},
			})
		default:
			return json.NewEncoder(w).Encode(map[string]interface{}{
				"decision": "deny",
				"reason":   fmt.Sprintf("Blocked by agy-hq Quarantined Reader policy (%s): tool %q is disabled in tainted sessions (deny-by-default).", taintReason, payload.ToolCall.Name),
			})
		}
	}

	// W1 (RFC-0025 §10 / §16.3): single-writer lease on lease-enforced provider workspaces for write_file /
	// edit_file in every non-tainted session (the Front Desk's Rule A below still applies on top of it).
	if payload.ToolCall.Name == "write_file" || payload.ToolCall.Name == "edit_file" {
		if pathArg, ok := payload.ToolCall.Args["Path"].(string); ok && strings.TrimSpace(pathArg) != "" {
			if reason := workspaceWriteLeaseDenial(vaultDir, payload.ConversationID, pathArg); reason != "" {
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision": "deny",
					"reason":   reason,
				})
			}
		}
	}

	// Surgical Per-Question Proposal Preview + Human-Presence Challenge Nonce overwrite on ask_question (RFC-0021 §4.3D)
	if payload.ToolCall.Name == "ask_question" {
		if isAutoConversation(vaultDir, payload.ConversationID) {
			return json.NewEncoder(w).Encode(map[string]interface{}{
				"decision": "deny",
				"reason":   "Blocked by agy-hq Unattended [Auto] policy: background [Auto] sessions cannot prompt via ask_question; route findings via `hq outbox add` or `hq offer stage`.",
			})
		}
		if pending, err := approvals.List(vaultDir); err == nil && len(pending) > 0 {
			if matchedID := askQuestionsReferenceProposal(payload.ToolCall.Args, pending); matchedID != "" {
				if _, isTaintedCaller := taint.Check(vaultDir, payload.ConversationID); isTaintedCaller || kernel.IsSubagentConversation(vaultDir, payload.ConversationID) {
					return json.NewEncoder(w).Encode(map[string]interface{}{
						"decision": "deny",
						"reason":   fmt.Sprintf("Blocked by agy-hq Human-Presence Approval policy: subagents and quarantined sessions cannot bind or execute 1-tap approval challenges directly for %s. Report %s back to your parent Task Conversation or `[HQ] Front Desk` so the user can approve it there (or approve in the :4242 Console).", matchedID, matchedID),
					})
				}
			}
			if overwriteArgs, ok := rewriteAskQuestionsSurgical(vaultDir, payload.ToolCall.Args, pending, payload.ConversationID, payload.TranscriptPath); ok {
				return json.NewEncoder(w).Encode(map[string]interface{}{
					"decision":  "allow",
					"overwrite": overwriteArgs,
				})
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
		_, err := w.Write([]byte(`{"decision":"allow"}` + "\n"))
		return err
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
		_, err := w.Write([]byte(`{"decision":"allow"}` + "\n"))
		return err
	}

	if payload.ToolCall.Name == "schedule" {
		cronExpr, _ := payload.ToolCall.Args["CronExpression"].(string)
		prompt, _ := payload.ToolCall.Args["Prompt"].(string)
		if armedID, _ := kernel.RecordArmedTrigger(vaultDir, payload.ConversationID, cronExpr, prompt); armedID != "" && mutatedConsole != nil {
			*mutatedConsole = true
		}
		_, err := w.Write([]byte(`{"decision":"allow"}` + "\n"))
		return err
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
		_, err := w.Write([]byte(`{"decision":"allow"}` + "\n"))
		return err
	}

	fd, hasFD := kernel.GetFrontDesk(vaultDir)
	isFrontDesk := hasFD && payload.ConversationID != "" && fd.ConversationID == payload.ConversationID

	if !isFrontDesk {
		if payload.ToolCall.Name == "run_command" && isAutoConversation(vaultDir, payload.ConversationID) {
			cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
			return json.NewEncoder(w).Encode(map[string]interface{}{
				"decision":  "allow",
				"overwrite": map[string]interface{}{"CommandLine": wrapScrubbedAutoShellCommand(cmdLine)},
			})
		}
		return writeAllowWithOptionalCompactionNudge(w, vaultDir, payload)
	}

	// Rule A: Block Direct Source/Worktree File Edits on the Front Desk (edit_file / write_file)
	if payload.ToolCall.Name == "edit_file" || payload.ToolCall.Name == "write_file" {
		pathArg, _ := payload.ToolCall.Args["Path"].(string)
		absRaw := resolveHookLexicalAbs(vaultDir, pathArg)
		cleanPath := canonicalizeHookPath(vaultDir, pathArg)

		var rawArtDirs []string
		if strings.TrimSpace(payload.ArtifactDirectoryPath) != "" {
			rawArtDirs = append(rawArtDirs, payload.ArtifactDirectoryPath)
		}
		if strings.TrimSpace(fd.ArtifactDir) != "" {
			rawArtDirs = append(rawArtDirs, fd.ArtifactDir)
		}
		if roots := hostpaths.Resolve(vaultDir); roots.BrainDir != "" && strings.TrimSpace(payload.ConversationID) != "" {
			rawArtDirs = append(rawArtDirs, filepath.Join(roots.BrainDir, strings.TrimSpace(payload.ConversationID)))
		}

		isArtifact := false
		if absRaw != "" && cleanPath != "" &&
			!isSystemGeneratedTranscriptPath(filepath.ToSlash(cleanPath)) &&
			!isSystemGeneratedTranscriptPath(filepath.ToSlash(absRaw)) {
			for _, allowedArt := range rawArtDirs {
				if _, ok := isSafePathUnderBase(vaultDir, allowedArt, pathArg); ok {
					isArtifact = true
					break
				}
			}
		}

		isLocalNonWorktree := false
		if relSlash, ok := isSafePathUnderBase(vaultDir, vaultDir, pathArg); ok && relSlash != "." {
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
		isHQCmd := isSingleHQCommand(cmdLine) || strings.Contains(cmdLine, "bootstrap.sh")

		if !isHQCmd && reHeavyBuildTest.MatchString(cmdLine) {
			resp := map[string]interface{}{
				"decision": "deny",
				"reason":   "Blocked by agy-hq Front Desk non-blocking policy: Do not run build or test suites inline on the [HQ] Front Desk thread. Delegate to a background subagent via invoke_subagent or use `hq upgrade`.",
			}
			return json.NewEncoder(w).Encode(resp)
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
		if base != "CONSOLE.md" && base != "hq_console.md" && base != "triggers.yaml" && base != "SKILL.md" && !isOverlayRule {
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
	_ = kernel.RecordLiveLSSession(vaultDir)

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
		tm, obsTurn := parseTranscriptTurnMetrics(payload.TranscriptPath, stepStartIdx)

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
		_, isTainted := taint.Check(vaultDir, payload.ConversationID)
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

				isNested := kernel.IsNestedSubagentConversation(vaultDir, payload.ConversationID)
				var lsLineage kernel.LSConversationLineage
				var hasLSLineage bool
				if !alreadyTracked && !isNested {
					isNested, _, lsLineage, hasLSLineage = detectNestedDescendant(vaultDir, payload.ConversationID, payload.LastUserInput, fd.ConversationID, 60*time.Minute)
				}

				if !hasFD && !isNested && shouldAutoClaimVacantFrontDesk(payload.CommonPayload, vaultDir) && !alreadyTracked && len(existingTasks) == 0 && os.IsNotExist(vacatedExists) {
					// In-session bootstrap fallback: fresh Vault with no tracked tasks, front_desk is vacant,
					// and no explicit release-desk marker exists. Claim Front Desk instead of registering [Task:general]!
					_ = kernel.SetFrontDesk(vaultDir, payload.ConversationID, payload.ArtifactDirectoryPath)
					kernel.ReconcileFrontDeskFromTranscript(vaultDir, payload.TranscriptPath)
				} else {
					if !alreadyTracked && !isNested {
						// Subagent fallback: match against pending queue or LS trajectory metadata if not yet registered
						if sub, found := kernel.PopPendingSubagentForCaller(vaultDir, payload.ConversationID, payload.LastUserInput, 60*time.Minute); found {
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
						} else if hasLSLineage && lsLineage.IsDirectFrontDeskSubagent {
							task := buildSubagentTaskFromLSLineage(payload.ConversationID, payload.ModelName, "completed", summary, lsLineage)
							_ = kernel.RegisterTask(vaultDir, task)
							alreadyTracked = true
							matchedTask = &task
						} else if linkedTask, linked := kernel.LinkPendingSpawnTask(vaultDir, payload.ConversationID, payload.ModelName); linked {
							alreadyTracked = true
							matchedTask = &linkedTask
						}
					}

					if alreadyTracked && matchedTask != nil {
						// Subagent & [Auto] worker lifecycle transition: mark completed (or immediately archived for unpinned [Auto] workers) unless awaiting a resumable human approval
						isSubagent := matchedTask.EffectiveKind() == "subagent" || strings.HasPrefix(matchedTask.TaskSlug, "subagent-") || strings.HasPrefix(matchedTask.Title, "[Subagent:")
						isAuto := matchedTask.EffectiveKind() == "auto"
						newStatus := matchedTask.Status
						if isSubagent && matchedTask.Status == "active" {
							newStatus = "completed"
						} else if isAuto && !hasPendingResumableApproval(vaultDir, payload.ConversationID) && matchedTask.Status == "active" {
							if matchedTask.Pinned {
								newStatus = "completed"
							} else {
								newStatus = "archived"
							}
						}
						newSummary := summary
						if strings.TrimSpace(newSummary) == "" {
							newSummary = matchedTask.LastSummary
						}
						if strings.TrimSpace(newSummary) == "" {
							newSummary = "Task completed."
						}
						nowTime := time.Now()
						nowISO := nowTime.Format(time.RFC3339)
						completedAt := matchedTask.CompletedAt
						if (newStatus == "completed" || newStatus == "archived") && completedAt == "" {
							completedAt = nowISO
						}
						archivedAt := matchedTask.ArchivedAt
						if newStatus == "archived" && archivedAt == "" {
							archivedAt = nowISO
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
							WorktreeDirs:         matchedTask.WorktreeDirs,
							RepoRoot:             matchedTask.RepoRoot,
							ConversationID:       payload.ConversationID,
							EventIDs:             matchedTask.EventIDs,
							Status:               newStatus,
							TurnCount:            matchedTask.TurnCount + 1,
							Pinned:               matchedTask.Pinned,
							CreatedAt:            matchedTask.CreatedAt,
							LastUpdatedAt:        nowISO,
							CompletedAt:          completedAt,
							ArchivedAt:           archivedAt,
							SidebarArchived:      matchedTask.SidebarArchived,
							SidebarSyncedAt:      matchedTask.SidebarSyncedAt,
							LastSummary:          newSummary,
						})
						if newStatus == "archived" && matchedTask.Status != "archived" && matchedTask.TaskSlug != "" {
							kernel.ReleaseTaskWorkspaceLeases(vaultDir, matchedTask.TaskSlug)
						}
						if isAuto && newStatus == "archived" && !matchedTask.Pinned && !matchedTask.SidebarArchived {
							_ = kernel.EnqueueSidebarSync(vaultDir, kernel.SidebarSyncQueueItem{
								ConversationID:  payload.ConversationID,
								TaskSlug:        matchedTask.TaskSlug,
								DesiredArchived: true,
								NextRetryAt:     nowTime.Add(3 * time.Second).UTC().Format(time.RFC3339),
								LastError:       "auto_stop_deferred",
							}, nowTime)
						}
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

			// RFC-0025 §7.3 / §16.8: bind provider workspaces written this turn to the owning task (ledger hits) or
			// queue daemon probe candidates (root_patterns hits). Ledger/tasks reads only; no provider calls.
			if !isFrontDesk && !isTainted && !tm.TaintedCmdSeen {
				autoBindWorkspacesFromStop(vaultDir, payload.ConversationID, tm.TurnFilesModified)
			}

			if obsTurn.CurrentContextTokens >= PreCompactionFlushTokens {
				if activeTask, ok := kernel.FindTaskByConversationID(vaultDir, payload.ConversationID); ok && activeTask.TaskSlug != "" {
					if _, _, found, _ := vault.LoadProgress(vaultDir, activeTask.TaskSlug); !found {
						goalText := strings.TrimSpace(activeTask.Title)
						if goalText == "" {
							goalText = strings.TrimSpace(userPrompt)
						}
						doneText := strings.TrimSpace(summary)
						if doneText == "" {
							doneText = "Context reached 120k+ tokens; auto-checkpointed from turn journal."
						}
						_, _ = vault.SaveProgress(vaultDir, vault.ProgressCheckpoint{
							TaskSlug:  activeTask.TaskSlug,
							Goal:      goalText,
							Completed: doneText,
							NextSteps: "Continue from latest turn journal or refine with `hq progress save`.",
							Pointers:  strings.Join(filesModified, ", "),
						}, time.Now())
					}
				}
			}
			if obsTurn.CompactionCount > 0 {
				_ = os.Remove(compactionNudgeMarkerPath(vaultDir, payload.ConversationID))
			}
			_, _ = kernel.CompleteConversationEvents(vaultDir, payload.ConversationID, time.Now())
			_, _ = approvals.ReconcileAskRejections(vaultDir, payload.ConversationID, payload.TranscriptPath, time.Now())
			_, _ = approvals.ReconcileLocalGitProposals(vaultDir, time.Now())

			replyText := strings.TrimSpace(payload.LastResponse)
			if replyText == "" {
				replyText = strings.TrimSpace(tm.CurrentTurnFullResponse)
			}
			if replyText != "" {
				if enqCount, _ := channels.EnqueueTurnReplyIfCorrelated(vaultDir, payload.ConversationID, execNum, replyText, tm.CurrentTurnStartedAt, time.Now()); enqCount > 0 {
					channels.BestEffortFlushLoopback(vaultDir, 150*time.Millisecond)
				}
			}

			if payload.TranscriptPath != "" {
				_, _ = obs.ReconcileSubagentAttribution(vaultDir)
				_, _ = obs.SyncDailyBudgetCircuitBreaker(vaultDir, time.Now())
			}
		}
	}

	if kernel.HasPendingSidebarSyncQueue(vaultDir) {
		_, _ = kernel.DrainSidebarSyncQueue(vaultDir, kernel.DefaultSidebarStopHookTimeout)
	}

	watcher.MaybeLaunchDesktopPiggyback(vaultDir, time.Now())

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
		if !d.IsDir() || d.Type()&os.ModeSymlink != 0 || d.Name() == "archive" {
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
	if maxStep == 0 {
		maxStep = vault.FindArchivedMaxStepCount(vaultDir, convID)
	}
	return maxStep
}

func detectNestedDescendant(vaultDir, convID, lastUserInput, fdConvID string, popMaxAge time.Duration) (bool, string, kernel.LSConversationLineage, bool) {
	if kernel.IsNestedSubagentConversation(vaultDir, convID) {
		return true, kernel.GetNestedConversationParent(vaultDir, convID), kernel.LSConversationLineage{}, false
	}
	lin, hasLS := kernel.InspectLSConversationLineage(vaultDir, convID)
	if hasLS && lin.IsNestedDescendant {
		_ = kernel.RecordNestedConversation(vaultDir, convID, lin.DirectParentID)
		return true, lin.DirectParentID, lin, true
	}
	if nestedSub, foundNested := kernel.PopPendingNestedSubagentForCaller(vaultDir, convID, lastUserInput, popMaxAge); foundNested {
		_ = kernel.RecordNestedConversation(vaultDir, convID, nestedSub.ParentConversationID)
		return true, nestedSub.ParentConversationID, lin, hasLS
	}
	if hasLS && lin.IsDirectFrontDeskSubagent {
		return false, "", lin, true
	}
	knownFDs := kernel.GetKnownFrontDeskIDSet(vaultDir)
	if fdID := strings.ToLower(strings.TrimSpace(fdConvID)); fdID != "" {
		knownFDs[fdID] = true
	}
	if edge, foundEdge := obs.FindSpawnEdgeInActiveSpawners(vaultDir, convID); foundEdge {
		ep := strings.ToLower(strings.TrimSpace(edge.ParentConversationID))
		if edge.TypeName == "run_workflow" || edge.TypeName == "agentapi" ||
			(len(knownFDs) > 0 && ep != "" && !knownFDs[ep]) {
			_ = kernel.RecordNestedConversation(vaultDir, convID, edge.ParentConversationID)
			return true, edge.ParentConversationID, lin, hasLS
		}
	}
	if hasAnc, ancParent := kernel.HasNonFrontDeskAncestry(vaultDir, convID, lin.DirectParentID); hasAnc {
		_ = kernel.RecordNestedConversation(vaultDir, convID, ancParent)
		return true, ancParent, lin, hasLS
	}
	return false, "", lin, hasLS
}

func buildSubagentTaskFromLSLineage(convID, payloadModel, status, summary string, lin kernel.LSConversationLineage) kernel.ActiveTask {
	modelName := strings.TrimSpace(lin.SubagentModel)
	if modelName == "" || modelName == "inherit" {
		modelName = payloadModel
	}
	role := strings.TrimSpace(lin.SubagentRole)
	if role == "" {
		role = strings.TrimSpace(lin.SourceTitle)
	}
	tier, scope, _, title, slug := kernel.ParseSubagentRole(role, modelName)
	nowISO := time.Now().Format(time.RFC3339)
	t := kernel.ActiveTask{
		TaskSlug:             slug,
		Title:                title,
		Kind:                 "subagent",
		Scope:                scope,
		ParentConversationID: lin.DirectParentID,
		Tier:                 tier,
		Model:                modelName,
		ConversationID:       convID,
		Status:               status,
		TurnCount:            1,
		CreatedAt:            nowISO,
		LastUpdatedAt:        nowISO,
		LastSummary:          strings.TrimSpace(summary),
	}
	if status == "completed" {
		t.CompletedAt = nowISO
		if t.LastSummary == "" {
			t.LastSummary = "Task completed."
		}
	}
	return t
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
	return kernel.GetNestedConversationParent(vaultDir, convID)
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
	ToolsUsed               []string
	FilesModified           []string
	TurnFilesModified       []string // write_file / edit_file paths from the current turn only (sorted)
	LastResponse            string
	CurrentTurnFullResponse string
	CurrentTurnStartedAt    time.Time
	LastUserPrompt          string
	StepCount               int
	ToolErrorCount          int
	TurnStepCount           int
	TurnErrorCount          int
	TransientErrors         int
	PreToolDenials          int
	TaintedCmdSeen          bool
}

func parseTranscriptTurnMetrics(transcriptPath string, stepStartIdx int) (transcriptTurnMetrics, obs.TurnMetrics) {
	var tm transcriptTurnMetrics
	if transcriptPath == "" {
		return tm, obs.TurnMetrics{}
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return tm, obs.TurnMetrics{}
	}
	defer f.Close()

	toolSet := make(map[string]bool)
	fileSet := make(map[string]bool)
	turnFileSet := make(map[string]bool)

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)

	obsAcc := obs.NewTurnSliceScanner(stepStartIdx)
	lastPlannerHadTools := false
	lastPlannerHadRunCommand := false

	for scanner.Scan() {
		rawLine := scanner.Bytes()
		obsAcc.ObserveLine(rawLine)
		var step struct {
			Type      string `json:"type"`
			Status    string `json:"status"`
			Content   string `json:"content"`
			CreatedAt string `json:"created_at"`
			ToolCalls []struct {
				Name string                 `json:"name"`
				Args map[string]interface{} `json:"args"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(rawLine, &step); err != nil {
			continue
		}
		curIdx := tm.StepCount
		tm.StepCount++
		inCurrentTurn := curIdx >= stepStartIdx
		if inCurrentTurn && tm.CurrentTurnStartedAt.IsZero() && strings.TrimSpace(step.CreatedAt) != "" {
			if ts, pErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(step.CreatedAt)); pErr == nil {
				tm.CurrentTurnStartedAt = ts
			} else if ts2, pErr2 := time.Parse(time.RFC3339, strings.TrimSpace(step.CreatedAt)); pErr2 == nil {
				tm.CurrentTurnStartedAt = ts2
			}
		}

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
				full := strings.TrimSpace(step.Content)
				if inCurrentTurn {
					if runes := []rune(full); len(runes) > 8000 {
						tm.CurrentTurnFullResponse = string(runes[:7997]) + "..."
					} else {
						tm.CurrentTurnFullResponse = full
					}
				}
				tm.LastResponse = full
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
						if inCurrentTurn {
							turnFileSet[p] = true
						}
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
	for k := range turnFileSet {
		tm.TurnFilesModified = append(tm.TurnFilesModified, k)
	}
	sort.Strings(tm.TurnFilesModified)
	return tm, obsAcc.Finish()
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
				if !d.IsDir() || d.Type()&os.ModeSymlink != 0 || d.Name() == "archive" {
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
		// 1b. Fallback to compacted cold journal archives (memory/journal/archive/*.jsonl)
		if transcriptPath == "" {
			transcriptPath = vault.FindArchivedTranscriptPath(vaultDir, convID)
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
