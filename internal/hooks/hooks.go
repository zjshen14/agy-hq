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
	"github.com/zjshen14/agy-hq/internal/kernel"
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
	Timestamp         string   `json:"timestamp"`
	ConversationID    string   `json:"conversationId"`
	ExecutionNum      int      `json:"executionNum"`
	WorkspacePaths    []string `json:"workspacePaths,omitempty"`
	ModelName         string   `json:"modelName,omitempty"`
	UserPrompt        string   `json:"userPrompt,omitempty"`
	ToolsUsed         []string `json:"toolsUsed,omitempty"`
	FilesModified     []string `json:"filesModified,omitempty"`
	TerminationReason string   `json:"terminationReason"`
	ResponseSummary   string   `json:"responseSummary,omitempty"`
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
		isFrontDesk := !ok || existing.ConversationID == payload.ConversationID

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
					"- For ANY bug investigation, root-cause analysis, or code change, **delegate immediately on Turn 1** via `invoke_subagent` (working in `local/worktrees/<repo>-<task>/`) or `./bin/hq spawn` so the user can keep chatting without waiting.",
				payload.ConversationID,
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
					"- **Step 4 (Concise Welcome):** Present a short status summary with clickable links to `[hq_console.md](file://%s)` and all active `[Task:<scope>]` conversations.",
					artPath,
				))
				sections = append(sections, strings.Join(initLines, "\n"))
			}
		} else {
			// Subagent or secondary conversation: check if already tracked or waiting in pending queue / spawn list
			_, alreadyTracked := kernel.FindTaskByConversationID(vaultDir, payload.ConversationID)
			if !alreadyTracked {
				if sub, found := kernel.PopPendingSubagent(vaultDir, 60*time.Minute); found {
					modelName := sub.Model
					if modelName == "" {
						modelName = payload.ModelName
					}
					_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
						TaskSlug:       sub.TaskSlug,
						Title:          sub.Title,
						Tier:           sub.Tier,
						Model:          modelName,
						ConversationID: payload.ConversationID,
						Status:         "active",
						LastSummary:    "Running in background...",
						CreatedAt:      time.Now().Format(time.RFC3339),
						LastUpdatedAt:  time.Now().Format(time.RFC3339),
					})
				} else {
					_, _ = kernel.LinkPendingSpawnTask(vaultDir, payload.ConversationID, payload.ModelName)
				}
			}

			sections = append(sections, fmt.Sprintf(
				"## ⚠️ Front Desk Notice\n- Another conversation (`%s`) currently holds the Front Desk intake lease (since %s).\n- To avoid split-brain race conditions, either use `%s` as your main intake desk, or if you are replacing/archiving it, run `./bin/hq claim-desk --conv=%s` to transfer the Front Desk lease to this thread.",
				existing.ConversationID, existing.UpdatedAt, existing.ConversationID, payload.ConversationID,
			))
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
			sections = append(sections, "## HQ Vault: MEMORY.md\n"+s)
		}
	}
	if facts, err := vault.LoadActiveFacts(vaultDir, 25); err == nil && len(facts) > 0 {
		var lines []string
		for _, f := range facts {
			lines = append(lines, fmt.Sprintf("- [%s] %s", f.Topic, f.Statement))
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
		var lines []string
		for _, t := range tasks {
			cleanTitle := kernel.SanitizeTaskTitle(t.Title, t.TaskSlug)
			lines = append(lines, fmt.Sprintf("- **%s** (`%s`, status: `%s`, conv: `%s`)", cleanTitle, t.TaskSlug, t.Status, t.ConversationID))
		}
		sections = append(sections, "## Tracked Task Conversations\n"+strings.Join(lines, "\n"))
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
		regexp.MustCompile(`(?i)\bgws\s+(gmail|chat)\s+(send|create|post)\b`),
		regexp.MustCompile(`(?i)\bgh\s+pr\s+merge\b`),
		regexp.MustCompile(`(?i)\bgh\s+release\s+create\b`),
		regexp.MustCompile(`(?i)\bvercel\s+(--prod|deploy\s+--prod)\b`),
	}

	reHeavyBuildTest = regexp.MustCompile(`(?i)\b(go\s+(?:test|build)|npm\s+(?:test|run\s+build)|pnpm\s+test|pytest|cargo\s+(?:test|build))\b`)
)

// HandlePreToolUse gates outbound/irreversible commands and enforces approval-bound execution,
// as well as Front Desk non-blocking policy (blocking repo/worktree edits, test runs, and capping lookups at 3).
func HandlePreToolUse(r io.Reader, w io.Writer, vaultDir string) error {
	var payload ToolCallPayload
	if err := json.NewDecoder(r).Decode(&payload); err != nil {
		_, wErr := w.Write([]byte(`{"decision":"allow"}` + "\n"))
		return wErr
	}

	if payload.ToolCall.Name == "run_command" {
		cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
		checkLine := cmdLine
		if reHQPropose.MatchString(cmdLine) {
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

	if payload.ToolCall.Name == "invoke_subagent" {
		role, _ := payload.ToolCall.Args["Role"].(string)
		model, _ := payload.ToolCall.Args["Model"].(string)
		prompt, _ := payload.ToolCall.Args["Prompt"].(string)
		_ = kernel.RecordSubagentSpawn(vaultDir, role, model, prompt)
		_, _ = kernel.QueuePendingSubagent(vaultDir, role, model, prompt)
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

		toolsUsed, filesModified, summary, transcriptUserPrompt := parseTranscriptSummary(payload.TranscriptPath)
		userPrompt := strings.TrimSpace(payload.LastUserInput)
		if userPrompt == "" {
			userPrompt = transcriptUserPrompt
		}

		dateDir := filepath.Join(vaultDir, "memory", "journal", time.Now().Format("2006-01-02"))
		if err := os.MkdirAll(dateDir, 0o755); err == nil {
			execNum := payload.ExecutionNum
			if execNum <= 0 {
				execNum = nextExecutionNum(dateDir, payload.ConversationID)
			}
			entry := JournalEntry{
				Timestamp:         time.Now().Format(time.RFC3339),
				ConversationID:    payload.ConversationID,
				ExecutionNum:      execNum,
				WorkspacePaths:    payload.WorkspacePaths,
				ModelName:         payload.ModelName,
				UserPrompt:        userPrompt,
				ToolsUsed:         toolsUsed,
				FilesModified:     filesModified,
				TerminationReason: payload.TerminationReason,
				ResponseSummary:   summary,
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
					if !alreadyTracked {
						// Subagent fallback: match against pending queue if not yet registered
						if sub, found := kernel.PopPendingSubagent(vaultDir, 60*time.Minute); found {
							modelName := sub.Model
							if modelName == "" {
								modelName = payload.ModelName
							}
							task := kernel.ActiveTask{
								TaskSlug:       sub.TaskSlug,
								Title:          sub.Title,
								Tier:           sub.Tier,
								Model:          modelName,
								ConversationID: payload.ConversationID,
								Status:         "completed",
								CreatedAt:      time.Now().Format(time.RFC3339),
								LastUpdatedAt:  time.Now().Format(time.RFC3339),
								LastSummary:    summary,
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
						isSubagent := strings.HasPrefix(matchedTask.TaskSlug, "subagent-") || strings.HasPrefix(matchedTask.Title, "[Subagent:")
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
						_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
							TaskSlug:       matchedTask.TaskSlug,
							Title:          matchedTask.Title,
							Tier:           matchedTask.Tier,
							Model:          matchedTask.Model,
							WorktreeDir:    matchedTask.WorktreeDir,
							ConversationID: payload.ConversationID,
							Status:         newStatus,
							CreatedAt:      matchedTask.CreatedAt,
							LastUpdatedAt:  time.Now().Format(time.RFC3339),
							LastSummary:    newSummary,
						})
					} else if !alreadyTracked {
						title := ""
						if strings.TrimSpace(userPrompt) != "" {
							title = kernel.SanitizeTaskTitle(userPrompt, "")
						}
						_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
							Title:          title,
							Model:          payload.ModelName,
							ConversationID: payload.ConversationID,
							LastUpdatedAt:  time.Now().Format(time.RFC3339),
							LastSummary:    summary,
						})
					}
				}
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

func parseTranscriptSummary(transcriptPath string) (toolsUsed []string, filesModified []string, lastResponse string, lastUserPrompt string) {
	if transcriptPath == "" {
		return nil, nil, "", ""
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil, nil, "", ""
	}
	defer f.Close()

	toolSet := make(map[string]bool)
	fileSet := make(map[string]bool)

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)

	for scanner.Scan() {
		var step struct {
			Type      string `json:"type"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Name string                 `json:"name"`
				Args map[string]interface{} `json:"args"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &step); err != nil {
			continue
		}
		if step.Type == "USER_INPUT" && strings.TrimSpace(step.Content) != "" {
			cleaned := strings.ReplaceAll(step.Content, "<USER_REQUEST>", "")
			cleaned = strings.ReplaceAll(cleaned, "</USER_REQUEST>", "")
			cleaned = strings.TrimSpace(cleaned)
			if cleaned != "" {
				lastUserPrompt = cleaned
			}
		}
		if step.Type == "PLANNER_RESPONSE" && strings.TrimSpace(step.Content) != "" {
			lastResponse = strings.TrimSpace(step.Content)
			if runes := []rune(lastResponse); len(runes) > 500 {
				lastResponse = string(runes[:500]) + "..."
			}
		}
		for _, tc := range step.ToolCalls {
			if tc.Name != "" {
				toolSet[tc.Name] = true
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
						if strings.TrimSpace(lastResponse) == "" || len(lastResponse) < 20 {
							lastResponse = msg
							if runes := []rune(lastResponse); len(runes) > 500 {
								lastResponse = string(runes[:500]) + "..."
							}
						}
					}
				}
			}
		}
	}

	for k := range toolSet {
		toolsUsed = append(toolsUsed, k)
	}
	for k := range fileSet {
		filesModified = append(filesModified, k)
	}
	return toolsUsed, filesModified, lastResponse, lastUserPrompt
}
