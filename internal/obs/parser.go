package obs

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	reCreatedAtLine   = regexp.MustCompile(`(?m)^Created At:\s*(\S+)`)
	reCompletedAtLine = regexp.MustCompile(`(?m)^Completed At:\s*(\S+)`)
	reSubagentConvID  = regexp.MustCompile(`"conversationId"\s*:\s*"([0-9a-zA-Z_-]{6,64})"`)
)

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

// ParseTranscriptFile parses an Antigravity transcript.jsonl file into ConversationMetrics,
// extracting per-turn token metrics, 3-part latency, context watermarks, compaction checkpoints,
// and ground-truth invoke_subagent spawn edges.
func ParseTranscriptFile(convID, transcriptPath, model, tier string) (ConversationMetrics, error) {
	cm := ConversationMetrics{
		ConversationID: convID,
		TranscriptPath: transcriptPath,
		Model:          model,
		Tier:           tier,
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

	var steps []rawStep
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)
	for scanner.Scan() {
		var st rawStep
		if json.Unmarshal(scanner.Bytes(), &st) != nil {
			continue
		}
		normalizeRawStep(&st)
		steps = append(steps, st)
	}

	if len(steps) == 0 {
		return cm, nil
	}

	var turns []TurnMetrics
	var curTurn *TurnMetrics
	var curToolSet map[string]bool
	var pendingSpawns []pendingSpawnCall

	lastPlannerHadTools := false
	lastPlannerHadRunCmd := false
	lastPlannerHadAskQuestion := false
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

	for i, st := range steps {
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
			finishTurn(i-1, steps[i-1].CreatedAt)
			startTurn(i, st.CreatedAt, "user", cleanPromptPreview(st.Content))
			lastStepTime = time.Time{}
		} else if st.Type == "SYSTEM_MESSAGE" && st.Source == "SYSTEM" && curTurn.PlannerSteps > 0 {
			finishTurn(i-1, steps[i-1].CreatedAt)
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

		if st.Type == "CHECKPOINT" {
			curTurn.CompactionCount++
		}

		if st.Type == "PLANNER_RESPONSE" {
			curTurn.PlannerSteps++
			// Estimate LLM generation time from previous step timestamp inside the turn
			if !stTime.IsZero() && !lastStepTime.IsZero() && stTime.After(lastStepTime) {
				deltaMs := stTime.Sub(lastStepTime).Milliseconds()
				if deltaMs > 0 && deltaMs <= 180_000 {
					curTurn.LLMLatencyMs += deltaMs
				}
			} else {
				curTurn.LLMLatencyMs += 250
			}

			lastPlannerHadTools = len(st.ToolCalls) > 0
			lastPlannerHadRunCmd = false
			lastPlannerHadAskQuestion = false

			for _, tc := range st.ToolCalls {
				if tc.Name != "" {
					curToolSet[tc.Name] = true
				}
				if tc.Name == "run_command" {
					lastPlannerHadRunCmd = true
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
			if tStart, tEnd, ok := parseToolResultTiming(st.Content); ok {
				durMs := tEnd.Sub(tStart).Milliseconds()
				if durMs < 0 {
					durMs = 0
				}
				if lastPlannerHadAskQuestion {
					curTurn.HumanWaitMs += durMs
				} else {
					curTurn.ToolLatencyMs += durMs
				}
				lastStepTime = tEnd
			} else if !stTime.IsZero() {
				lastStepTime = stTime
			}

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
			}
		} else if !stTime.IsZero() {
			lastStepTime = stTime
		}

		if isStepError(st.Status, st.Content, lastPlannerHadTools, lastPlannerHadRunCmd) {
			curTurn.ToolErrorCount++
		}
	}

	if curTurn != nil {
		finishTurn(len(steps)-1, steps[len(steps)-1].CreatedAt)
	}

	// Also inspect sibling .system_generated/messages/*.json if present to discover parent-child callbacks
	sysGenDir := filepath.Dir(filepath.Dir(transcriptPath))
	if filepath.Base(sysGenDir) == ".system_generated" {
		msgDir := filepath.Join(sysGenDir, "messages")
		if entries, mErr := os.ReadDir(msgDir); mErr == nil {
			seenChild := make(map[string]bool)
			for _, edge := range cm.SpawnEdges {
				seenChild[edge.ChildConversationID] = true
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				if b, rErr := os.ReadFile(filepath.Join(msgDir, e.Name())); rErr == nil {
					var msg struct {
						Sender    string `json:"sender"`
						Recipient string `json:"recipient"`
					}
					if json.Unmarshal(b, &msg) == nil {
						sender := strings.ToLower(strings.TrimSpace(msg.Sender))
						recip := strings.ToLower(strings.TrimSpace(msg.Recipient))
						if recip == strings.ToLower(convID) && sender != "" && sender != recip && !seenChild[sender] {
							seenChild[sender] = true
							cm.SpawnEdges = append(cm.SpawnEdges, SubagentSpawnEdge{
								ChildConversationID:  sender,
								ParentConversationID: convID,
							})
						}
					}
				}
			}
		}
	}

	cm.Turns = turns
	cm.Self = RollupTurns(turns, len(steps), cm.Model, cm.Tier)
	cm.Tree = cm.Self
	return cm, nil
}

// ParseSingleTurnSlice extracts token, context, compaction, and latency metrics for steps[stepStartIdx:]
// so HandleStop can enrich JournalEntry in O(1) without scanning all conversations.
func ParseSingleTurnSlice(transcriptPath string, stepStartIdx int) TurnMetrics {
	var tm TurnMetrics
	if transcriptPath == "" {
		return tm
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return tm
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)

	idx := 0
	var lastStepTime time.Time
	lastPlannerHadAskQuestion := false

	for scanner.Scan() {
		var st rawStep
		if json.Unmarshal(scanner.Bytes(), &st) != nil {
			continue
		}
		normalizeRawStep(&st)
		curIdx := idx
		idx++

		ctx := st.InputTokens + st.CacheReadTokens
		if st.Type == "PLANNER_RESPONSE" && ctx > 0 {
			tm.CurrentContextTokens = ctx
		}

		if curIdx < stepStartIdx {
			continue
		}

		if tm.StartedAt == "" && st.CreatedAt != "" {
			tm.StartedAt = st.CreatedAt
		}
		if st.CreatedAt != "" {
			tm.EndedAt = st.CreatedAt
		}
		stTime := parseTimestamp(st.CreatedAt)

		if st.InputTokens > 0 || st.CacheReadTokens > 0 || st.OutputTokens > 0 {
			tm.InputTokens += st.InputTokens
			tm.CacheReadTokens += st.CacheReadTokens
			tm.OutputTokens += st.OutputTokens
			tm.UncachedTokens += st.InputTokens + st.OutputTokens
			tm.GrossTokens += st.InputTokens + st.CacheReadTokens + st.OutputTokens
			if ctx > tm.PeakContextTokens {
				tm.PeakContextTokens = ctx
			}
		}
		if st.Type == "CHECKPOINT" {
			tm.CompactionCount++
		}
		if st.Type == "PLANNER_RESPONSE" {
			tm.PlannerSteps++
			if !stTime.IsZero() && !lastStepTime.IsZero() && stTime.After(lastStepTime) {
				deltaMs := stTime.Sub(lastStepTime).Milliseconds()
				if deltaMs > 0 && deltaMs <= 180_000 {
					tm.LLMLatencyMs += deltaMs
				}
			} else {
				tm.LLMLatencyMs += 250
			}
			lastPlannerHadAskQuestion = false
			for _, tc := range st.ToolCalls {
				if tc.Name == "ask_question" {
					lastPlannerHadAskQuestion = true
				}
			}
		}
		if st.Type == "GENERIC" {
			if tStart, tEnd, ok := parseToolResultTiming(st.Content); ok {
				durMs := tEnd.Sub(tStart).Milliseconds()
				if durMs < 0 {
					durMs = 0
				}
				if lastPlannerHadAskQuestion {
					tm.HumanWaitMs += durMs
				} else {
					tm.ToolLatencyMs += durMs
				}
				lastStepTime = tEnd
			} else if !stTime.IsZero() {
				lastStepTime = stTime
			}
		} else if !stTime.IsZero() {
			lastStepTime = stTime
		}
	}

	if idx > stepStartIdx {
		tm.StepStartIdx = stepStartIdx
		tm.StepEndIdx = idx - 1
		tm.StepCount = idx - stepStartIdx
	}
	denom := tm.InputTokens + tm.CacheReadTokens
	if denom > 0 {
		tm.CacheHitRatio = float64(tm.CacheReadTokens) / float64(denom)
	}
	tm.ActiveLatencyMs = tm.LLMLatencyMs + tm.ToolLatencyMs
	return tm
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
