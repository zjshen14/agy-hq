package hooks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/vault"
)

var reQuarantinedReadCmd = regexp.MustCompile(`^\s*(?:(?:\S+/)?hq)\s+(?:(?:gmail|gchat)\s+read-quarantined|watch\s+read|brief\s+format-quarantined)\b`)

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
