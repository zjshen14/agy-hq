package obs

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
)

var (
	reCreatedAtLine      = regexp.MustCompile(`(?m)^Created At:\s*(\S+)`)
	reCompletedAtLine    = regexp.MustCompile(`(?m)^Completed At:\s*(\S+)`)
	reSubagentConvID     = regexp.MustCompile(`"conversationId"\s*:\s*"([0-9a-zA-Z_-]{6,64})"`)
	reFlexibleConvID     = regexp.MustCompile(`\\?"conversation(?:Id|_id)\\?"\s*:\s*\\?"([0-9a-zA-Z_-]{6,64})\\?"`)
	reBgTaskIDLine       = regexp.MustCompile(`(?i)\btask id:\s*([0-9a-zA-Z_-]+/task-\d+)`)
	reFinishedTaskIDLine = regexp.MustCompile(`(?i)(?:Task id\s+"([0-9a-zA-Z_-]+/task-\d+)"\s+finished|sender=([0-9a-zA-Z_-]+/task-\d+))`)
	reStepOutputRef      = regexp.MustCompile(`\.system_generated/steps/(\d+)/output\.txt`)
	reWorkflowDescLine   = regexp.MustCompile(`Workflow\s+"([^"]+)"\s+completed`)
)

// splitShellSegmentsUnquoted splits cmdLine on unquoted &&, ||, ;, |, &, or \n separators.
func splitShellSegmentsUnquoted(cmdLine string) []string {
	var segments []string
	var cur strings.Builder
	var quote byte
	escaped := false

	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segments = append(segments, s)
		}
		cur.Reset()
	}

	for i := 0; i < len(cmdLine); i++ {
		c := cmdLine[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		if quote != 0 {
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' {
				escaped = true
			}
			continue
		}
		if c == '\\' {
			cur.WriteByte(c)
			escaped = true
			continue
		}
		if c == '\'' || c == '"' {
			cur.WriteByte(c)
			quote = c
			continue
		}
		if c == '\n' || c == ';' || c == '|' || c == '&' {
			flush()
			if (c == '|' || c == '&') && i+1 < len(cmdLine) && cmdLine[i+1] == c {
				i++
			}
			continue
		}
		cur.WriteByte(c)
	}
	flush()
	return segments
}

// tokenizeShellWords extracts unquoted/unwrapped tokens from a single shell segment.
func tokenizeShellWords(seg string) []string {
	var words []string
	var cur strings.Builder
	var quote byte
	inWord := false
	escaped := false

	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			inWord = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' {
				escaped = true
			} else {
				cur.WriteByte(c)
			}
			inWord = true
			continue
		}
		if c == '\\' {
			escaped = true
			inWord = true
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			inWord = true
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' || c == '(' || c == ')' {
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
			continue
		}
		cur.WriteByte(c)
		inWord = true
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

func isEnvAssignmentToken(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		ch := tok[i]
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9' && i > 0) || ch == '_') {
			return false
		}
	}
	return true
}

func stripCommandWrappers(words []string) []string {
	i := 0
	for i < len(words) {
		w := words[i]
		if isEnvAssignmentToken(w) {
			i++
			continue
		}
		base := filepath.Base(w)
		if base == "env" {
			i++
			for i < len(words) {
				opt := words[i]
				if opt == "-u" || opt == "--unset" {
					i += 2
					continue
				}
				if strings.HasPrefix(opt, "-") || isEnvAssignmentToken(opt) {
					i++
					continue
				}
				break
			}
			continue
		}
		if base == "command" || base == "exec" || base == "nohup" {
			i++
			continue
		}
		if base == "timeout" && i+1 < len(words) {
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				i++
			}
			if i < len(words) {
				i++ // duration arg
			}
			continue
		}
		break
	}
	return words[i:]
}

// IsRawAgentAPINewConversationCommand returns true if cmdLine invokes the external agentapi CLI
// to start an ephemeral child worker conversation (excluding hq spawn, hq adopt, and explicit [Task:...]/[Auto] titles,
// and ignoring quoted strings such as git commit messages or echo payloads).
func IsRawAgentAPINewConversationCommand(cmdLine string) bool {
	if strings.TrimSpace(cmdLine) == "" {
		return false
	}
	segments := splitShellSegmentsUnquoted(cmdLine)
	foundRawAgentAPI := false
	for _, seg := range segments {
		words := stripCommandWrappers(tokenizeShellWords(seg))
		if len(words) < 2 {
			continue
		}
		exe := strings.ToLower(filepath.Base(words[0]))
		if exe == "hq" && (words[1] == "spawn" || words[1] == "adopt") {
			return false
		}
		if exe == "agentapi" || exe == "agentapi.exe" {
			sub := ""
			hasExplicitTaskTitle := false
			for j := 1; j < len(words); j++ {
				arg := words[j]
				if arg == "--title" && j+1 < len(words) {
					tv := words[j+1]
					if strings.HasPrefix(tv, "[Task:") || strings.HasPrefix(tv, "[Auto]") {
						hasExplicitTaskTitle = true
					}
					j++
					continue
				}
				if strings.HasPrefix(arg, "--title=") {
					tv := strings.TrimPrefix(arg, "--title=")
					if strings.HasPrefix(tv, "[Task:") || strings.HasPrefix(tv, "[Auto]") {
						hasExplicitTaskTitle = true
					}
					continue
				}
				if sub == "" && !strings.HasPrefix(arg, "-") {
					sub = arg
				}
			}
			if sub == "new-conversation" {
				if hasExplicitTaskTitle {
					return false
				}
				foundRawAgentAPI = true
			}
		}
	}
	return foundRawAgentAPI
}

type rawToolCall struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Args          map[string]interface{} `json:"args"`
	ArgumentsJSON string                 `json:"arguments_json"`
}

type rawStep struct {
	StepIndex       int    `json:"step_index"`
	Type            string `json:"type"`
	Source          string `json:"source"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
	InputTokens     int64  `json:"input_tokens"`
	CacheReadTokens int64  `json:"cache_read_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	TokenUsage      *struct {
		InputTokens     int64 `json:"input_tokens"`
		CacheReadTokens int64 `json:"cache_read_tokens"`
		OutputTokens    int64 `json:"output_tokens"`
	} `json:"token_usage,omitempty"`
	Content   string        `json:"content"`
	ToolCalls []rawToolCall `json:"tool_calls"`
}

type pendingSpawnCall struct {
	stepIdx   int
	createdAt string
	role      string
	model     string
	typeName  string
	workspace string
	prompt    string
}

func isFileViewerStepContent(content string) bool {
	return strings.HasPrefix(content, "Showing lines ") ||
		strings.Contains(content, "\nShowing lines ") ||
		strings.HasPrefix(content, "Made 1 replacement in ") ||
		strings.HasPrefix(content, "Created file ")
}

func expandStepContentWithSiblingOutput(transcriptPath, content string) string {
	if transcriptPath == "" || content == "" {
		return content
	}
	matches := reStepOutputRef.FindAllStringSubmatch(content, 4)
	if len(matches) == 0 {
		return content
	}
	sysGenDir := filepath.Dir(filepath.Dir(transcriptPath))
	if filepath.Base(sysGenDir) != ".system_generated" {
		return content
	}
	var sb strings.Builder
	sb.WriteString(content)
	seen := make(map[string]bool)
	for _, m := range matches {
		if len(m) != 2 {
			continue
		}
		stepNum := m[1]
		if seen[stepNum] {
			continue
		}
		seen[stepNum] = true
		outPath := filepath.Join(sysGenDir, "steps", stepNum, "output.txt")
		f, err := os.Open(outPath)
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(f, 2*1024*1024))
		_ = f.Close()
		if len(raw) > 0 {
			sb.WriteByte('\n')
			sb.Write(raw)
		}
	}
	return sb.String()
}

func extractDistinctChildConvIDs(parentConvID, text string) []string {
	parentNorm := strings.ToLower(strings.TrimSpace(parentConvID))
	matches := reFlexibleConvID.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, m := range matches {
		if len(m) != 2 {
			continue
		}
		cid := strings.ToLower(strings.TrimSpace(m[1]))
		if len(cid) < 8 || cid == parentNorm || seen[cid] {
			continue
		}
		if !kernel.IsCanonicalUUID(cid) && !(strings.Contains(cid, "-") && strings.ContainsAny(cid, "0123456789")) {
			continue
		}
		seen[cid] = true
		out = append(out, cid)
	}
	return out
}

func parseCommandLineFromArgs(args map[string]interface{}) string {
	for _, k := range []string{"CommandLine", "commandLine", "command"} {
		if v, ok := args[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func parseTimestamp(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return ts
	}
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts
	}
	return time.Time{}
}

func parseToolResultTiming(content string) (created, completed time.Time, ok bool) {
	m1 := reCreatedAtLine.FindStringSubmatch(content)
	m2 := reCompletedAtLine.FindStringSubmatch(content)
	if len(m1) == 2 && len(m2) == 2 {
		t1 := parseTimestamp(m1[1])
		t2 := parseTimestamp(m2[1])
		if !t1.IsZero() && !t2.IsZero() {
			return t1, t2, true
		}
	}
	return time.Time{}, time.Time{}, false
}

func cleanPromptPreview(raw string) string {
	s := strings.TrimSpace(raw)
	if idx := strings.Index(s, "<ADDITIONAL_METADATA>"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	s = strings.ReplaceAll(s, "<USER_REQUEST>", "")
	s = strings.ReplaceAll(s, "</USER_REQUEST>", "")
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if runes := []rune(s); len(runes) > 160 {
		s = string(runes[:157]) + "..."
	}
	return s
}

func classifySystemTrigger(content string) string {
	lower := strings.ToLower(content)
	if strings.Contains(content, "[Auto]") || strings.Contains(lower, "cron") || strings.Contains(lower, "scheduled") {
		return "schedule"
	}
	if strings.Contains(lower, "subagent") || strings.Contains(lower, "message from") || strings.Contains(content, "<MESSAGE") {
		return "subagent_callback"
	}
	return "system_wakeup"
}

func isStepError(status, content string, lastPlannerHadTools, lastPlannerHadRunCmd bool) bool {
	if strings.EqualFold(strings.TrimSpace(status), "ERROR") {
		return true
	}
	if strings.HasPrefix(content, "Showing lines ") || strings.Contains(content, "\nShowing lines ") {
		return false
	}
	if strings.Contains(content, "Encountered error in tool execution:") || strings.Contains(content, "Encountered error in step execution:") {
		return true
	}
	if lastPlannerHadTools && !lastPlannerHadRunCmd {
		return false
	}
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

func accumulateLLMLatency(llmLatencyMs *int64, stTime, lastStepTime time.Time) {
	if !stTime.IsZero() && !lastStepTime.IsZero() && stTime.After(lastStepTime) {
		deltaMs := stTime.Sub(lastStepTime).Milliseconds()
		if deltaMs > 0 && deltaMs <= 180_000 {
			*llmLatencyMs += deltaMs
		}
	} else {
		*llmLatencyMs += 250
	}
}

func accumulateToolTiming(humanWaitMs, toolLatencyMs *int64, content string, stTime, lastStepTime time.Time, hadAskQuestion bool) time.Time {
	if tStart, tEnd, ok := parseToolResultTiming(content); ok {
		durMs := tEnd.Sub(tStart).Milliseconds()
		if durMs < 0 {
			durMs = 0
		}
		if hadAskQuestion {
			*humanWaitMs += durMs
		} else {
			*toolLatencyMs += durMs
		}
		return tEnd
	}
	if !stTime.IsZero() {
		return stTime
	}
	return lastStepTime
}

func appendExpandedSpawnEdges(cm *ConversationMetrics, recorded map[string]bool, transcriptPath, convID, content string, matched pendingSpawnCall) {
	expanded := expandStepContentWithSiblingOutput(transcriptPath, content)
	for _, childID := range extractDistinctChildConvIDs(convID, expanded) {
		if recorded[childID] {
			continue
		}
		recorded[childID] = true
		cm.SpawnEdges = append(cm.SpawnEdges, SubagentSpawnEdge{
			StepIndex:            matched.stepIdx,
			ChildConversationID:  childID,
			ParentConversationID: convID,
			Role:                 matched.role,
			TypeName:             matched.typeName,
			PromptPrefix:         matched.prompt,
			CreatedAt:            matched.createdAt,
		})
	}
}

func isCompletedCheckpointStatus(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "", "DONE", "CORTEX_STEP_STATUS_DONE":
		return true
	default:
		return false
	}
}

// ParseTranscriptFile parses an Antigravity transcript.jsonl file into ConversationMetrics,
// extracting per-turn token metrics, 3-part latency, context watermarks, compaction checkpoints,
// and ground-truth invoke_subagent spawn edges.
func ParseTranscriptFile(convID, transcriptPath, model, tier string) (ConversationMetrics, error) {
	cm := ConversationMetrics{
		ConversationID:        convID,
		TranscriptPath:        transcriptPath,
		Model:                 model,
		Tier:                  tier,
		LastCheckpointStepIdx: -1,
	}
	info, err := os.Stat(transcriptPath)
	if err != nil {
		return cm, err
	}
	cm.FileSize = info.Size()
	cm.ModTimeNano = info.ModTime().UnixNano()

	f, err := os.Open(transcriptPath)
	if err != nil {
		return cm, err
	}
	defer f.Close()

	var turns []TurnMetrics
	var curTurn *TurnMetrics
	var curToolSet map[string]bool
	var pendingSpawns []pendingSpawnCall
	var pendingWorkflowCalls []pendingSpawnCall
	var pendingAgentAPICalls []pendingSpawnCall
	pendingWorkflowTasks := make(map[string]pendingSpawnCall)
	pendingAgentAPITasks := make(map[string]pendingSpawnCall)
	recordedEdgeChildren := make(map[string]bool)

	lastPlannerHadTools := false
	lastPlannerHadRunCmd := false
	lastPlannerHadAskQuestion := false
	plannerSinceCheckpoint := 0
	var lastStepTime time.Time

	finishTurn := func(endStepIdx int, endAt string) {
		if curTurn == nil {
			return
		}
		if curTurn.StepEndIdx < endStepIdx {
			curTurn.StepEndIdx = endStepIdx
		}
		curTurn.StepCount = curTurn.StepEndIdx - curTurn.StepStartIdx + 1
		if curTurn.StepCount < 1 {
			curTurn.StepCount = 1
		}
		if endAt != "" {
			curTurn.EndedAt = endAt
		}
		denom := curTurn.InputTokens + curTurn.CacheReadTokens
		if denom > 0 {
			curTurn.CacheHitRatio = float64(curTurn.CacheReadTokens) / float64(denom)
		}
		curTurn.ActiveLatencyMs = curTurn.LLMLatencyMs + curTurn.ToolLatencyMs
		for k := range curToolSet {
			curTurn.ToolsUsed = append(curTurn.ToolsUsed, k)
		}
		sort.Strings(curTurn.ToolsUsed)
		curTurn.QuotaUnits, curTurn.EstCostUSD = ComputeQuotaAndCost(
			curTurn.InputTokens,
			curTurn.CacheReadTokens,
			curTurn.OutputTokens,
			cm.Model,
			cm.Tier,
		)
		turns = append(turns, *curTurn)
		curTurn = nil
		curToolSet = nil
	}

	startTurn := func(stepIdx int, createdAt, triggerKind, promptPreview string) {
		curTurn = &TurnMetrics{
			TurnIndex:     len(turns) + 1,
			StepStartIdx:  stepIdx,
			StepEndIdx:    stepIdx,
			StartedAt:     createdAt,
			EndedAt:       createdAt,
			TriggerKind:   triggerKind,
			PromptPreview: promptPreview,
		}
		curToolSet = make(map[string]bool)
	}

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)
	stepCount := 0
	prevCreatedAt := ""
	for scanner.Scan() {
		var st rawStep
		if json.Unmarshal(scanner.Bytes(), &st) != nil {
			continue
		}
		normalizeRawStep(&st)
		i := stepCount
		stepCount++

		if cm.StartedAt == "" && st.CreatedAt != "" {
			cm.StartedAt = st.CreatedAt
		}
		if st.CreatedAt != "" {
			cm.LastActiveAt = st.CreatedAt
		}

		// Determine if this step starts a new turn
		if curTurn == nil {
			trig := "user"
			preview := ""
			if st.Type == "USER_INPUT" {
				preview = cleanPromptPreview(st.Content)
			} else if st.Type == "SYSTEM_MESSAGE" {
				trig = classifySystemTrigger(st.Content)
				preview = cleanPromptPreview(st.Content)
			}
			startTurn(i, st.CreatedAt, trig, preview)
		} else if st.Type == "USER_INPUT" && curTurn.PlannerSteps > 0 {
			finishTurn(i-1, prevCreatedAt)
			startTurn(i, st.CreatedAt, "user", cleanPromptPreview(st.Content))
			lastStepTime = time.Time{}
		} else if st.Type == "SYSTEM_MESSAGE" && st.Source == "SYSTEM" && curTurn.PlannerSteps > 0 {
			finishTurn(i-1, prevCreatedAt)
			startTurn(i, st.CreatedAt, classifySystemTrigger(st.Content), cleanPromptPreview(st.Content))
			lastStepTime = time.Time{}
		}

		curTurn.StepEndIdx = i
		if curTurn.StartedAt == "" && st.CreatedAt != "" {
			curTurn.StartedAt = st.CreatedAt
		}
		if st.CreatedAt != "" {
			curTurn.EndedAt = st.CreatedAt
		}
		if curTurn.PromptPreview == "" && (st.Type == "USER_INPUT" || st.Type == "SYSTEM_MESSAGE") && strings.TrimSpace(st.Content) != "" {
			curTurn.PromptPreview = cleanPromptPreview(st.Content)
			if st.Type == "SYSTEM_MESSAGE" && curTurn.TriggerKind == "user" {
				curTurn.TriggerKind = classifySystemTrigger(st.Content)
			}
		}

		stTime := parseTimestamp(st.CreatedAt)

		// Token metrics on PLANNER_RESPONSE and CHECKPOINT steps
		if st.InputTokens > 0 || st.CacheReadTokens > 0 || st.OutputTokens > 0 {
			curTurn.InputTokens += st.InputTokens
			curTurn.CacheReadTokens += st.CacheReadTokens
			curTurn.OutputTokens += st.OutputTokens
			uncached := st.InputTokens + st.OutputTokens
			gross := st.InputTokens + st.CacheReadTokens + st.OutputTokens
			curTurn.UncachedTokens += uncached
			curTurn.GrossTokens += gross

			ctx := st.InputTokens + st.CacheReadTokens
			if ctx > curTurn.PeakContextTokens {
				curTurn.PeakContextTokens = ctx
			}
			if st.Type == "PLANNER_RESPONSE" {
				curTurn.CurrentContextTokens = ctx
				cm.CurrentContextTokens = ctx
			}
		}

		if st.Type == "CHECKPOINT" && isCompletedCheckpointStatus(st.Status) {
			curTurn.CompactionCount++
			cm.LastCheckpointStepIdx = i
			plannerSinceCheckpoint = 0
		}

		if st.Type == "USER_INPUT" {
			// New turn boundary: clear any unconsumed single-step tool calls from the prior turn.
			pendingSpawns = nil
			pendingWorkflowCalls = nil
			pendingAgentAPICalls = nil
		}

		if st.Type == "PLANNER_RESPONSE" {
			// Clear unconsumed immediate tool results from earlier planner steps before collecting new calls.
			pendingSpawns = nil
			pendingWorkflowCalls = nil
			pendingAgentAPICalls = nil

			curTurn.PlannerSteps++
			plannerSinceCheckpoint++
			// Estimate LLM generation time from previous step timestamp inside the turn
			accumulateLLMLatency(&curTurn.LLMLatencyMs, stTime, lastStepTime)

			lastPlannerHadTools = len(st.ToolCalls) > 0
			lastPlannerHadRunCmd = false
			lastPlannerHadAskQuestion = false

			for _, tc := range st.ToolCalls {
				if tc.Name != "" {
					curToolSet[tc.Name] = true
				}
				if tc.Name == "run_command" {
					lastPlannerHadRunCmd = true
					cmdLine := parseCommandLineFromArgs(tc.Args)
					if IsRawAgentAPINewConversationCommand(cmdLine) {
						pendingAgentAPICalls = append(pendingAgentAPICalls, pendingSpawnCall{
							stepIdx:   i,
							createdAt: st.CreatedAt,
							typeName:  "agentapi",
							prompt:    cleanPromptPreview(cmdLine),
						})
					}
				}
				if tc.Name == "run_workflow" {
					desc, _ := tc.Args["Description"].(string)
					script, _ := tc.Args["Script"].(string)
					if script == "" {
						script, _ = tc.Args["ScriptPath"].(string)
					}
					pendingWorkflowCalls = append(pendingWorkflowCalls, pendingSpawnCall{
						stepIdx:   i,
						createdAt: st.CreatedAt,
						role:      strings.TrimSpace(desc),
						typeName:  "run_workflow",
						prompt:    cleanPromptPreview(strings.TrimSpace(script)),
					})
				}
				if tc.Name == "ask_question" {
					lastPlannerHadAskQuestion = true
				}
				if tc.Name == "invoke_subagent" {
					role, _ := tc.Args["Role"].(string)
					mArg, _ := tc.Args["Model"].(string)
					tName, _ := tc.Args["TypeName"].(string)
					ws, _ := tc.Args["Workspace"].(string)
					pr, _ := tc.Args["Prompt"].(string)
					pendingSpawns = append(pendingSpawns, pendingSpawnCall{
						stepIdx:   i,
						createdAt: st.CreatedAt,
						role:      strings.Trim(strings.TrimSpace(role), "\"'"),
						model:     strings.Trim(strings.TrimSpace(mArg), "\"'"),
						typeName:  strings.Trim(strings.TrimSpace(tName), "\"'"),
						workspace: strings.Trim(strings.TrimSpace(ws), "\"'"),
						prompt:    cleanPromptPreview(strings.Trim(strings.TrimSpace(pr), "\"'")),
					})
				}
			}
		}

		if st.Type == "GENERIC" {
			// Check for tool execution duration
			lastStepTime = accumulateToolTiming(&curTurn.HumanWaitMs, &curTurn.ToolLatencyMs, st.Content, stTime, lastStepTime, lastPlannerHadAskQuestion)

			// Check for ground-truth invoke_subagent result ("Created the following subagent:\n{\n  \"conversationId\": \"...\"")
			if strings.Contains(st.Content, "Created the following subagent") {
				if m := reSubagentConvID.FindStringSubmatch(st.Content); len(m) == 2 {
					childID := strings.ToLower(strings.TrimSpace(m[1]))
					var matched pendingSpawnCall
					if len(pendingSpawns) > 0 {
						matched = pendingSpawns[0]
						pendingSpawns = pendingSpawns[1:]
					} else {
						matched = pendingSpawnCall{
							stepIdx:   i,
							createdAt: st.CreatedAt,
						}
					}
					recordedEdgeChildren[childID] = true
					cm.SpawnEdges = append(cm.SpawnEdges, SubagentSpawnEdge{
						StepIndex:            matched.stepIdx,
						ChildConversationID:  childID,
						ParentConversationID: convID,
						Role:                 matched.role,
						Model:                matched.model,
						TypeName:             matched.typeName,
						Workspace:            matched.workspace,
						PromptPrefix:         matched.prompt,
						CreatedAt:            matched.createdAt,
					})
				}
			} else if bm := reBgTaskIDLine.FindStringSubmatch(st.Content); len(bm) == 2 {
				taskID := strings.TrimSpace(bm[1])
				if len(pendingWorkflowCalls) > 0 {
					pendingWorkflowTasks[taskID] = pendingWorkflowCalls[0]
					pendingWorkflowCalls = pendingWorkflowCalls[1:]
				} else if len(pendingAgentAPICalls) > 0 {
					pendingAgentAPITasks[taskID] = pendingAgentAPICalls[0]
					pendingAgentAPICalls = pendingAgentAPICalls[1:]
				}
			} else if !isFileViewerStepContent(st.Content) {
				if len(pendingWorkflowCalls) > 0 && (strings.Contains(st.Content, "Workflow ") || reFlexibleConvID.MatchString(st.Content) || reStepOutputRef.MatchString(st.Content)) {
					matched := pendingWorkflowCalls[0]
					pendingWorkflowCalls = pendingWorkflowCalls[1:]
					appendExpandedSpawnEdges(&cm, recordedEdgeChildren, transcriptPath, convID, st.Content, matched)
				} else if len(pendingAgentAPICalls) > 0 && (reFlexibleConvID.MatchString(st.Content) || reStepOutputRef.MatchString(st.Content) || strings.Contains(st.Content, "Command exited")) {
					matched := pendingAgentAPICalls[0]
					pendingAgentAPICalls = pendingAgentAPICalls[1:]
					appendExpandedSpawnEdges(&cm, recordedEdgeChildren, transcriptPath, convID, st.Content, matched)
				}
			}
		} else if !stTime.IsZero() {
			lastStepTime = stTime
		}

		if st.Type == "SYSTEM_MESSAGE" {
			var taskID string
			if fm := reFinishedTaskIDLine.FindStringSubmatch(st.Content); len(fm) == 3 {
				if fm[1] != "" {
					taskID = strings.TrimSpace(fm[1])
				} else {
					taskID = strings.TrimSpace(fm[2])
				}
			}
			var matched *pendingSpawnCall
			if taskID != "" {
				if m, ok := pendingWorkflowTasks[taskID]; ok {
					cp := m
					matched = &cp
					delete(pendingWorkflowTasks, taskID)
				} else if m, ok := pendingAgentAPITasks[taskID]; ok {
					cp := m
					matched = &cp
					delete(pendingAgentAPITasks, taskID)
				}
			}
			if matched == nil && !strings.Contains(st.Content, "<MESSAGE") && !strings.Contains(st.Content, "sender=") {
				if wm := reWorkflowDescLine.FindStringSubmatch(st.Content); len(wm) == 2 {
					matched = &pendingSpawnCall{
						stepIdx:   i,
						createdAt: st.CreatedAt,
						role:      strings.TrimSpace(wm[1]),
						typeName:  "run_workflow",
					}
				}
			}
			if matched != nil {
				appendExpandedSpawnEdges(&cm, recordedEdgeChildren, transcriptPath, convID, st.Content, *matched)
			}
		}

		if isStepError(st.Status, st.Content, lastPlannerHadTools, lastPlannerHadRunCmd) {
			curTurn.ToolErrorCount++
		}
		prevCreatedAt = st.CreatedAt
	}

	if stepCount == 0 {
		return cm, nil
	}
	if curTurn != nil {
		finishTurn(stepCount-1, prevCreatedAt)
	}

	cm.Turns = turns
	cm.PlannerResponsesSinceCheckpoint = plannerSinceCheckpoint
	cm.Self = RollupTurns(turns, stepCount, cm.Model, cm.Tier)
	cm.Tree = cm.Self
	return cm, nil
}

// TurnSliceScanner incrementally computes TurnMetrics over JSONL transcript lines for steps[stepStartIdx:]
// so callers that already stream a transcript file can compute turn token/latency metrics in a single pass.
type TurnSliceScanner struct {
	stepStartIdx              int
	idx                       int
	lastStepTime              time.Time
	lastPlannerHadAskQuestion bool
	plannerSinceCheckpoint    int
	lastCheckpointStepIdx     int
	tm                        TurnMetrics
}

// NewTurnSliceScanner creates a single-pass TurnMetrics accumulator starting at stepStartIdx.
func NewTurnSliceScanner(stepStartIdx int) *TurnSliceScanner {
	return &TurnSliceScanner{
		stepStartIdx:          stepStartIdx,
		lastCheckpointStepIdx: -1,
	}
}

// ObserveLine feeds one JSONL line from transcript.jsonl into the accumulator.
func (s *TurnSliceScanner) ObserveLine(raw []byte) {
	var st rawStep
	if json.Unmarshal(raw, &st) != nil {
		return
	}
	normalizeRawStep(&st)
	curIdx := s.idx
	s.idx++

	ctx := st.InputTokens + st.CacheReadTokens
	if st.Type == "PLANNER_RESPONSE" {
		if ctx > 0 {
			s.tm.CurrentContextTokens = ctx
		}
		s.plannerSinceCheckpoint++
	} else if st.Type == "CHECKPOINT" && isCompletedCheckpointStatus(st.Status) {
		s.lastCheckpointStepIdx = curIdx
		s.plannerSinceCheckpoint = 0
	}

	if curIdx < s.stepStartIdx {
		return
	}

	if s.tm.StartedAt == "" && st.CreatedAt != "" {
		s.tm.StartedAt = st.CreatedAt
	}
	if st.CreatedAt != "" {
		s.tm.EndedAt = st.CreatedAt
	}
	stTime := parseTimestamp(st.CreatedAt)

	if st.InputTokens > 0 || st.CacheReadTokens > 0 || st.OutputTokens > 0 {
		s.tm.InputTokens += st.InputTokens
		s.tm.CacheReadTokens += st.CacheReadTokens
		s.tm.OutputTokens += st.OutputTokens
		s.tm.UncachedTokens += st.InputTokens + st.OutputTokens
		s.tm.GrossTokens += st.InputTokens + st.CacheReadTokens + st.OutputTokens
		if ctx > s.tm.PeakContextTokens {
			s.tm.PeakContextTokens = ctx
		}
	}
	if st.Type == "CHECKPOINT" && isCompletedCheckpointStatus(st.Status) {
		s.tm.CompactionCount++
	}
	if st.Type == "PLANNER_RESPONSE" {
		s.tm.PlannerSteps++
		accumulateLLMLatency(&s.tm.LLMLatencyMs, stTime, s.lastStepTime)
		s.lastPlannerHadAskQuestion = false
		for _, tc := range st.ToolCalls {
			if tc.Name == "ask_question" {
				s.lastPlannerHadAskQuestion = true
			}
		}
	}
	if st.Type == "GENERIC" {
		s.lastStepTime = accumulateToolTiming(&s.tm.HumanWaitMs, &s.tm.ToolLatencyMs, st.Content, stTime, s.lastStepTime, s.lastPlannerHadAskQuestion)
	} else if !stTime.IsZero() {
		s.lastStepTime = stTime
	}
}

// Finish finalizes derived step range, cache hit ratio, and active latency fields.
func (s *TurnSliceScanner) Finish() TurnMetrics {
	tm := s.tm
	if s.idx > s.stepStartIdx {
		tm.StepStartIdx = s.stepStartIdx
		tm.StepEndIdx = s.idx - 1
		tm.StepCount = s.idx - s.stepStartIdx
	}
	denom := tm.InputTokens + tm.CacheReadTokens
	if denom > 0 {
		tm.CacheHitRatio = float64(tm.CacheReadTokens) / float64(denom)
	}
	tm.ActiveLatencyMs = tm.LLMLatencyMs + tm.ToolLatencyMs
	return tm
}

// ParseSingleTurnSlice extracts token, context, compaction, and latency metrics for steps[stepStartIdx:]
// so HandleStop can enrich JournalEntry in O(1) without scanning all conversations.
func ParseSingleTurnSlice(transcriptPath string, stepStartIdx int) TurnMetrics {
	if transcriptPath == "" {
		return TurnMetrics{}
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return TurnMetrics{}
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)

	acc := NewTurnSliceScanner(stepStartIdx)
	for scanner.Scan() {
		acc.ObserveLine(scanner.Bytes())
	}
	return acc.Finish()
}

// TranscriptContextState captures a conversation's latest context window size and uncompacted planner activity.
type TranscriptContextState struct {
	CurrentContextTokens            int64
	PlannerResponsesSinceCheckpoint int
	LastCheckpointStepIdx           int
	CompactionCount                 int
}

// InspectTranscriptContextFile streams transcriptPath once via TurnSliceScanner to extract
// current prompt context tokens and uncompacted PLANNER_RESPONSE steps after the latest DONE CHECKPOINT.
func InspectTranscriptContextFile(transcriptPath string) (TranscriptContextState, bool) {
	if transcriptPath == "" {
		return TranscriptContextState{LastCheckpointStepIdx: -1}, false
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return TranscriptContextState{LastCheckpointStepIdx: -1}, false
	}
	defer f.Close()

	acc := NewTurnSliceScanner(0)
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		acc.ObserveLine(line)
	}
	if scanner.Err() != nil {
		return TranscriptContextState{LastCheckpointStepIdx: -1}, false
	}
	tm := acc.Finish()
	return TranscriptContextState{
		CurrentContextTokens:            tm.CurrentContextTokens,
		PlannerResponsesSinceCheckpoint: acc.plannerSinceCheckpoint,
		LastCheckpointStepIdx:           acc.lastCheckpointStepIdx,
		CompactionCount:                 tm.CompactionCount,
	}, true
}

func normalizeRawStep(st *rawStep) {
	if st == nil {
		return
	}
	if st.TokenUsage != nil {
		if st.InputTokens == 0 {
			st.InputTokens = st.TokenUsage.InputTokens
		}
		if st.CacheReadTokens == 0 {
			st.CacheReadTokens = st.TokenUsage.CacheReadTokens
		}
		if st.OutputTokens == 0 {
			st.OutputTokens = st.TokenUsage.OutputTokens
		}
	}
	for i := range st.ToolCalls {
		if len(st.ToolCalls[i].Args) == 0 && st.ToolCalls[i].ArgumentsJSON != "" {
			var parsed map[string]interface{}
			if json.Unmarshal([]byte(st.ToolCalls[i].ArgumentsJSON), &parsed) == nil {
				st.ToolCalls[i].Args = parsed
			}
		}
	}
}

// FindSpawnEdgeInActiveSpawners inspects recently active spawner transcripts (from active_spawners.json)
// to see if any parent conversation recorded a ground-truth spawn edge (run_workflow, agentapi, or invoke_subagent)
// for childConvID. Reuses obs/index.json cached SpawnEdges when FileSize/ModTimeNano match to stay O(stat).
func FindSpawnEdgeInActiveSpawners(vaultDir, childConvID string) (SubagentSpawnEdge, bool) {
	cid := strings.ToLower(strings.TrimSpace(childConvID))
	if cid == "" {
		return SubagentSpawnEdge{}, false
	}
	spawners := kernel.ListActiveSpawners(vaultDir, 24*time.Hour)
	if len(spawners) == 0 {
		return SubagentSpawnEdge{}, false
	}
	cachedIdx := loadCachedIndex(vaultDir)
	brainDir := ResolveBrainDir(vaultDir)
	for _, sp := range spawners {
		spID := strings.ToLower(strings.TrimSpace(sp.ConversationID))
		if spID == "" || spID == cid {
			continue
		}
		tPath := strings.TrimSpace(sp.TranscriptPath)
		if tPath == "" && brainDir != "" {
			tPath = filepath.Join(brainDir, sp.ConversationID, ".system_generated", "logs", "transcript.jsonl")
		}
		if tPath == "" {
			continue
		}
		var edges []SubagentSpawnEdge
		if info, err := os.Stat(tPath); err == nil {
			if cached, ok := cachedIdx.Conversations[spID]; ok &&
				cached.TranscriptPath == tPath &&
				cached.FileSize == info.Size() &&
				cached.ModTimeNano == info.ModTime().UnixNano() {
				edges = cached.SpawnEdges
			}
		}
		if edges == nil {
			cm, err := ParseTranscriptFile(spID, tPath, "", "")
			if err != nil {
				continue
			}
			edges = cm.SpawnEdges
		}
		for _, edge := range edges {
			if strings.ToLower(strings.TrimSpace(edge.ChildConversationID)) == cid {
				if edge.ParentConversationID == "" {
					edge.ParentConversationID = spID
				}
				return edge, true
			}
		}
	}
	return SubagentSpawnEdge{}, false
}
