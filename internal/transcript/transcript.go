package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"strings"
)

var (
	reQuarantinedReadCmd = regexp.MustCompile(`^\s*(?:(?:\S+/)?hq)\s+(?:(?:gmail|gchat)\s+read-quarantined|watch\s+read|brief\s+format-quarantined)\b`)
	reCreatedAtPrefix    = regexp.MustCompile(`^(?:Created At:\s*\S+\s*)?(?:Completed At:\s*\S+\s*)?`)
)

// ToolCall represents a single tool invocation recorded on a PLANNER_RESPONSE transcript step.
type ToolCall struct {
	Name    string                 `json:"name"`
	Args    map[string]interface{} `json:"args"`
	CmdLine string                 `json:"-"`
}

// Step represents a normalized step extracted from transcript.jsonl with 1:1 tool-call pairing.
type Step struct {
	StepIndex int
	Type      string
	Status    string
	Content   string
	ToolName  string
	CmdLine   string
	ToolCalls []ToolCall
	IsError   bool
	Transient bool
	IsDenial  bool
}

// IsQuarantinedReadCmd returns true if cmdLine invokes a quarantined untrusted-content reader.
func IsQuarantinedReadCmd(cmdLine string) bool {
	return reQuarantinedReadCmd.MatchString(cmdLine)
}

// StripTimestampPrefix removes leading "Created At: ... Completed At: ..." wrapper headers.
func StripTimestampPrefix(s string) string {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "Created At:") && !strings.HasPrefix(trimmed, "Completed At:") {
		return trimmed
	}
	return strings.TrimSpace(reCreatedAtPrefix.ReplaceAllString(trimmed, ""))
}

// IsReadOnlyFileViewerOutput returns true if the tool output step is a file viewer (`read_file`)
// or if the preceding planner step only invoked non-command tools.
func IsReadOnlyFileViewerOutput(content string, lastPlannerHadTools, lastPlannerHadRunCommand bool) bool {
	if strings.HasPrefix(content, "Showing lines ") || strings.Contains(content, "\nShowing lines ") {
		return true
	}
	if lastPlannerHadTools && !lastPlannerHadRunCommand {
		return true
	}
	return false
}

// HasCommandExitNonZeroLine reports whether content contains a non-zero "Command exited with code N" line.
func HasCommandExitNonZeroLine(content string) bool {
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

// IsPreToolDenialStep reports whether a step represents a PreToolUse hook denial.
func IsPreToolDenialStep(status, content string) bool {
	if !strings.Contains(content, "Blocked by agy-hq") {
		return false
	}
	if strings.HasPrefix(content, "Showing lines ") || strings.Contains(content, "\nShowing lines ") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(status), "ERROR") ||
		strings.Contains(content, "Encountered error") ||
		strings.HasPrefix(strings.TrimSpace(content), "Blocked by agy-hq") ||
		strings.Contains(content, `"decision":"deny"`)
}

// IsStepError reports whether a transcript step represents a tool or command error.
func IsStepError(status, content string, lastPlannerHadTools, lastPlannerHadRunCommand bool) bool {
	if strings.EqualFold(strings.TrimSpace(status), "ERROR") {
		return true
	}
	if strings.HasPrefix(content, "Showing lines ") || strings.Contains(content, "\nShowing lines ") {
		return false
	}
	if strings.Contains(content, "Encountered error in tool execution:") || strings.Contains(content, "Encountered error in step execution:") {
		return true
	}
	if !IsReadOnlyFileViewerOutput(content, lastPlannerHadTools, lastPlannerHadRunCommand) && HasCommandExitNonZeroLine(content) {
		return true
	}
	return false
}

// IsTransientExternalError reports whether content indicates transient network/upstream noise.
func IsTransientExternalError(content string) bool {
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

// FirstLineExcerpt extracts a concise single-line summary while stripping leading timestamp headers
// and focusing on non-zero exit lines or "Blocked by agy-hq" reasons.
func FirstLineExcerpt(s string, maxLen int) string {
	s = StripTimestampPrefix(s)
	if idx := strings.Index(s, "Blocked by agy-hq"); idx >= 0 {
		s = strings.TrimSpace(s[idx:])
	} else if idx := strings.Index(s, "Command exited with code "); idx >= 0 {
		rest := strings.TrimSpace(s[idx+len("Command exited with code "):])
		if len(rest) > 0 && rest[0] >= '1' && rest[0] <= '9' {
			s = strings.TrimSpace(s[idx:])
		}
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if runes := []rune(s); len(runes) > maxLen {
		return string(runes[:maxLen]) + "..."
	}
	return s
}

// ParseSteps reads transcript.jsonl starting at minStepIdx (0 scans from start), pairing each
// tool-result step 1:1 with its originating ToolCall from parallel PLANNER_RESPONSE batches,
// and reporting whether any step invoked a quarantined reader.
func ParseSteps(transcriptPath string, minStepIdx int) ([]Step, bool, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	var steps []Step
	var pendingCalls []ToolCall
	tainted := false

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		var raw struct {
			StepIndex int    `json:"step_index"`
			Type      string `json:"type"`
			Status    string `json:"status"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Name string                 `json:"name"`
				Args map[string]interface{} `json:"args"`
			} `json:"tool_calls"`
		}
		if json.Unmarshal(scanner.Bytes(), &raw) != nil {
			continue
		}

		// Always detect quarantine commands across the full transcript so session taint is accurate
		// even when minStepIdx > 0.
		var calls []ToolCall
		if raw.Type == "PLANNER_RESPONSE" {
			pendingCalls = nil
			for _, tc := range raw.ToolCalls {
				cmdLine := ""
				if tc.Name == "run_command" {
					if c, ok := tc.Args["CommandLine"].(string); ok {
						cmdLine = strings.TrimSpace(c)
						if IsQuarantinedReadCmd(cmdLine) {
							tainted = true
						}
					}
				}
				call := ToolCall{Name: tc.Name, Args: tc.Args, CmdLine: cmdLine}
				calls = append(calls, call)
				pendingCalls = append(pendingCalls, call)
			}
		}

		if minStepIdx > 0 && raw.StepIndex < minStepIdx {
			// Maintain queue alignment across skipped steps
			if raw.Type != "PLANNER_RESPONSE" && raw.Type != "USER_INPUT" && len(pendingCalls) > 0 {
				pendingCalls = pendingCalls[1:]
			}
			continue
		}

		toolName := ""
		cmdLine := ""
		isRunCmd := false
		hasPairedTool := false

		if raw.Type == "PLANNER_RESPONSE" {
			if len(calls) > 0 {
				toolName = calls[0].Name
				cmdLine = calls[0].CmdLine
			}
		} else if raw.Type != "USER_INPUT" && len(pendingCalls) > 0 {
			// Pair 1:1 in FIFO order with the exact tool call from the preceding PLANNER_RESPONSE.
			// If content clearly contains "Command exited with code " and a later queued call is run_command,
			// prefer pairing that run_command call to avoid cross-tool clobbering.
			pickIdx := 0
			if HasCommandExitNonZeroLine(raw.Content) && pendingCalls[0].Name != "run_command" {
				for idx, pc := range pendingCalls {
					if pc.Name == "run_command" {
						pickIdx = idx
						break
					}
				}
			}
			paired := pendingCalls[pickIdx]
			pendingCalls = append(pendingCalls[:pickIdx], pendingCalls[pickIdx+1:]...)
			toolName = paired.Name
			cmdLine = paired.CmdLine
			isRunCmd = paired.Name == "run_command"
			hasPairedTool = true
		}

		isReadViewer := IsReadOnlyFileViewerOutput(raw.Content, hasPairedTool, isRunCmd)
		isErr := raw.Type != "USER_INPUT" && raw.Type != "PLANNER_RESPONSE" &&
			IsStepError(raw.Status, raw.Content, hasPairedTool, isRunCmd)
		isDenial := !isReadViewer && IsPreToolDenialStep(raw.Status, raw.Content)
		isTransient := IsTransientExternalError(raw.Content)

		steps = append(steps, Step{
			StepIndex: raw.StepIndex,
			Type:      raw.Type,
			Status:    raw.Status,
			Content:   raw.Content,
			ToolName:  toolName,
			CmdLine:   cmdLine,
			ToolCalls: calls,
			IsError:   isErr,
			Transient: isTransient,
			IsDenial:  isDenial,
		})
	}
	return steps, tainted, scanner.Err()
}
