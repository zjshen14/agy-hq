package hooks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
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
	if payload.ArtifactDirectoryPath != "" {
		_ = os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755)
		_ = os.WriteFile(filepath.Join(vaultDir, "memory", "active_artifact_dir.txt"), []byte(strings.TrimSpace(payload.ArtifactDirectoryPath)), 0o644)
	}

	var sections []string
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
	return json.NewEncoder(w).Encode(resp)
}

var outboundCommandPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bgws\s+(gmail|chat)\s+(send|create|post)\b`),
	regexp.MustCompile(`(?i)\bgh\s+pr\s+merge\b`),
	regexp.MustCompile(`(?i)\bgh\s+release\s+create\b`),
	regexp.MustCompile(`(?i)\bvercel\s+(--prod|deploy\s+--prod)\b`),
}

// HandlePreToolUse gates outbound/irreversible commands and enforces approval-bound execution.
// Direct outbound commands are hard-denied (`"decision": "deny"`) so tasks stage them via `hq propose`
// and present 1-tap approval via `ask_question` (or chat inline buttons).
func HandlePreToolUse(r io.Reader, w io.Writer) error {
	var payload ToolCallPayload
	if err := json.NewDecoder(r).Decode(&payload); err != nil {
		_, wErr := w.Write([]byte(`{"decision":"allow"}` + "\n"))
		return wErr
	}

	if payload.ToolCall.Name == "run_command" {
		cmdLine, _ := payload.ToolCall.Args["CommandLine"].(string)
		for _, re := range outboundCommandPatterns {
			if re.MatchString(cmdLine) {
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
	if payload.ArtifactDirectoryPath != "" {
		_ = os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755)
		_ = os.WriteFile(filepath.Join(vaultDir, "memory", "active_artifact_dir.txt"), []byte(strings.TrimSpace(payload.ArtifactDirectoryPath)), 0o644)
	}

	if payload.ConversationID != "" {
		toolsUsed, filesModified, summary := parseTranscriptSummary(payload.TranscriptPath)
		dateDir := filepath.Join(vaultDir, "memory", "journal", time.Now().Format("2006-01-02"))
		if err := os.MkdirAll(dateDir, 0o755); err == nil {
			entry := JournalEntry{
				Timestamp:         time.Now().Format(time.RFC3339),
				ConversationID:    payload.ConversationID,
				ExecutionNum:      payload.ExecutionNum,
				WorkspacePaths:    payload.WorkspacePaths,
				ModelName:         payload.ModelName,
				UserPrompt:        payload.LastUserInput,
				ToolsUsed:         toolsUsed,
				FilesModified:     filesModified,
				TerminationReason: payload.TerminationReason,
				ResponseSummary:   summary,
			}
			if data, err := json.MarshalIndent(entry, "", "  "); err == nil {
				fileName := fmt.Sprintf("%s-%d.json", payload.ConversationID, payload.ExecutionNum)
				_ = os.WriteFile(filepath.Join(dateDir, fileName), data, 0o644)
			}
		}
	}

	_, err := w.Write([]byte(`{"decision":"stop"}` + "\n"))
	return err
}

func parseTranscriptSummary(transcriptPath string) (toolsUsed []string, filesModified []string, lastResponse string) {
	if transcriptPath == "" {
		return nil, nil, ""
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil, nil, ""
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
		if step.Type == "PLANNER_RESPONSE" && strings.TrimSpace(step.Content) != "" {
			lastResponse = strings.TrimSpace(step.Content)
			if len(lastResponse) > 500 {
				lastResponse = lastResponse[:500] + "..."
			}
		}
		for _, tc := range step.ToolCalls {
			if tc.Name != "" {
				toolSet[tc.Name] = true
			}
			if tc.Name == "write_file" || tc.Name == "edit_file" {
				if p, ok := tc.Args["Path"].(string); ok && p != "" {
					fileSet[p] = true
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
	return toolsUsed, filesModified, lastResponse
}
