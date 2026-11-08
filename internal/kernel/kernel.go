package kernel

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// QuotaState tracks daily background spawns and circuit-breaker status in local/memory/quota-YYYY-MM-DD.json.
type QuotaState struct {
	Date            string       `json:"date"`
	SpawnsToday     int          `json:"spawnsToday"`
	MaxDailySpawns  int          `json:"maxDailySpawns"`
	CircuitOpen     bool         `json:"circuitOpen"`
	CircuitReason   string       `json:"circuitReason,omitempty"`
	LastHeartbeatAt string       `json:"lastHeartbeatAt,omitempty"`
	LastStatus      string       `json:"lastStatus,omitempty"`
	Events          []SpawnEvent `json:"events,omitempty"`
}

// SpawnEvent records a background task spawn or 0-token gatekeeper event.
type SpawnEvent struct {
	Timestamp string `json:"timestamp"`
	Trigger   string `json:"trigger"`
	Skill     string `json:"skill,omitempty"`
	Model     string `json:"model,omitempty"`
	Outcome   string `json:"outcome"`
}

// FrontDeskLease tracks the single active "Front Desk" intake conversationId in local/memory/front_desk.json
// to prevent split-brain race conditions across multiple top-level conversations.
type FrontDeskLease struct {
	ConversationID string `json:"conversationId"`
	ArtifactDir    string `json:"artifactDir,omitempty"`
	UpdatedAt      string `json:"updatedAt"`
}

// ActiveTask tracks a dedicated multi-turn Task Conversation spawned via `hq spawn` in local/memory/tasks.json.
type ActiveTask struct {
	TaskSlug       string `json:"taskSlug"`
	Title          string `json:"title"`
	Tier           string `json:"tier"`
	Model          string `json:"model"`
	WorktreeDir    string `json:"worktreeDir,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
	CreatedAt      string `json:"createdAt"`
	LastUpdatedAt  string `json:"lastUpdatedAt"`
	LastSummary    string `json:"lastSummary,omitempty"`
	Status         string `json:"status"` // active, completed
}

func tasksFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "tasks.json")
}

// ListTasks returns all tracked Task Conversations from local/memory/tasks.json.
func ListTasks(vaultDir string) ([]ActiveTask, error) {
	data, err := os.ReadFile(tasksFilePath(vaultDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var tasks []ActiveTask
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

// RegisterTask records or updates a dedicated Task Conversation in local/memory/tasks.json.
func RegisterTask(vaultDir string, task ActiveTask) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	tasks, _ := ListTasks(vaultDir)
	updated := false
	for i := range tasks {
		if (task.TaskSlug != "" && tasks[i].TaskSlug == task.TaskSlug) || (task.ConversationID != "" && tasks[i].ConversationID == task.ConversationID) {
			if task.TaskSlug == "" {
				task.TaskSlug = tasks[i].TaskSlug
			}
			if task.Title == "" {
				task.Title = tasks[i].Title
			}
			if task.Tier == "" {
				task.Tier = tasks[i].Tier
			}
			if task.Model == "" {
				task.Model = tasks[i].Model
			}
			if task.WorktreeDir == "" {
				task.WorktreeDir = tasks[i].WorktreeDir
			}
			if task.CreatedAt == "" {
				task.CreatedAt = tasks[i].CreatedAt
			}
			if task.Status == "" {
				task.Status = tasks[i].Status
			}
			if task.LastSummary == "" {
				task.LastSummary = tasks[i].LastSummary
			}
			tasks[i] = task
			updated = true
			break
		}
	}
	if !updated && task.TaskSlug == "" && task.ConversationID != "" {
		// If a task was just spawned via `hq spawn` with ConversationID=="" and is active, link it to this first Stop hook
		for i := len(tasks) - 1; i >= 0; i-- {
			if tasks[i].ConversationID == "" && tasks[i].Status == "active" {
				tasks[i].ConversationID = task.ConversationID
				if task.Model != "" {
					tasks[i].Model = task.Model
				}
				if task.LastUpdatedAt != "" {
					tasks[i].LastUpdatedAt = task.LastUpdatedAt
				}
				if task.LastSummary != "" {
					tasks[i].LastSummary = task.LastSummary
				}
				updated = true
				break
			}
		}
	}
	if !updated {
		if task.TaskSlug == "" && task.ConversationID != "" {
			shortID := task.ConversationID
			if len(shortID) > 8 {
				shortID = shortID[:8]
			}
			task.TaskSlug = "conv-" + shortID
		}
		if task.Title == "" {
			task.Title = "[Task:general] Untitled Task"
		}
		if task.Status == "" {
			task.Status = "active"
		}
		if task.CreatedAt == "" {
			task.CreatedAt = time.Now().Format(time.RFC3339)
		}
		tasks = append(tasks, task)
	}
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tasksFilePath(vaultDir), data, 0o644)
}

// CompleteTask marks an existing task in local/memory/tasks.json (matched by TaskSlug or ConversationID) as completed.
func CompleteTask(vaultDir, slugOrConvID, summary string) error {
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return err
	}
	found := false
	for i := range tasks {
		if tasks[i].TaskSlug == slugOrConvID || tasks[i].ConversationID == slugOrConvID {
			tasks[i].Status = "completed"
			tasks[i].LastUpdatedAt = time.Now().Format(time.RFC3339)
			if strings.TrimSpace(summary) != "" {
				tasks[i].LastSummary = strings.TrimSpace(summary)
			}
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("task %q not found in local/memory/tasks.json", slugOrConvID)
	}
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tasksFilePath(vaultDir), data, 0o644)
}

func frontDeskPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "front_desk.json")
}

// GetFrontDesk reads the currently active Front Desk conversation lease.
func GetFrontDesk(vaultDir string) (FrontDeskLease, bool) {
	var lease FrontDeskLease
	data, err := os.ReadFile(frontDeskPath(vaultDir))
	if err != nil {
		return lease, false
	}
	if err := json.Unmarshal(data, &lease); err != nil || lease.ConversationID == "" {
		return lease, false
	}
	return lease, true
}

// SetFrontDesk atomically claims the single Front Desk intake lease for convID.
func SetFrontDesk(vaultDir, convID, artifactDir string) error {
	if convID == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	artifactDir = strings.TrimSpace(artifactDir)
	if artifactDir == "" {
		if appData := os.Getenv("ANTIGRAVITY_APP_DATA_DIR"); appData != "" {
			artifactDir = filepath.Join(appData, "brain", convID)
		} else if home, err := os.UserHomeDir(); err == nil && home != "" {
			candidate := filepath.Join(home, ".gemini", "antigravity", "brain", convID)
			artifactDir = candidate
		}
	}
	if artifactDir != "" {
		_ = os.MkdirAll(artifactDir, 0o755)
	}
	if prev, ok := GetFrontDesk(vaultDir); ok && prev.ConversationID != convID && prev.ArtifactDir != "" {
		_ = os.Remove(filepath.Join(prev.ArtifactDir, "hq_console.md"))
		_ = os.Remove(filepath.Join(prev.ArtifactDir, "hq_console.md.metadata.json"))
		_ = os.Remove(filepath.Join(prev.ArtifactDir, "hq_console_mobile.md"))
		_ = os.Remove(filepath.Join(prev.ArtifactDir, "hq_console_mobile.md.metadata.json"))
	}
	lease := FrontDeskLease{
		ConversationID: convID,
		ArtifactDir:    artifactDir,
		UpdatedAt:      time.Now().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return err
	}
	if artifactDir != "" {
		_ = os.WriteFile(filepath.Join(vaultDir, "memory", "active_artifact_dir.txt"), []byte(artifactDir), 0o644)
	}
	_ = os.Remove(filepath.Join(vaultDir, "memory", ".vacated"))
	return os.WriteFile(frontDeskPath(vaultDir), data, 0o644)
}

// ReleaseFrontDesk clears local/memory/front_desk.json so the next new conversation
// automatically claims the Front Desk intake lease on SessionStart.
func ReleaseFrontDesk(vaultDir string) error {
	if prev, ok := GetFrontDesk(vaultDir); ok && prev.ArtifactDir != "" {
		_ = os.Remove(filepath.Join(prev.ArtifactDir, "hq_console.md"))
		_ = os.Remove(filepath.Join(prev.ArtifactDir, "hq_console.md.metadata.json"))
		_ = os.Remove(filepath.Join(prev.ArtifactDir, "hq_console_mobile.md"))
		_ = os.Remove(filepath.Join(prev.ArtifactDir, "hq_console_mobile.md.metadata.json"))
	}
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755)
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", ".vacated"), []byte(time.Now().Format(time.RFC3339)), 0o644)
	err := os.Remove(frontDeskPath(vaultDir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

var (
	reTaxForm         = regexp.MustCompile(`(?i)(w-?2|1099|1098|k-?1|tax)`)
	reSecFiling       = regexp.MustCompile(`(?i)(10-[kq]|8-k|earnings)`)
	reDeepTask        = regexp.MustCompile(`(?i)\b(architect|architecture|design|security|audit|race\s+condition|root\s+cause|thesis|falsif|complex|deep|hard)\b`)
	reQuickTask       = regexp.MustCompile(`(?i)\b(triage|status|summary|summarize|brief|consolidate|list|format|lint|typo|quick)\b`)
	reUUID            = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	rePromptModelHint = regexp.MustCompile(`(?i)\b(?:use|with|on)\s+(opus|sonnet|argon|flash_lite|flash-lite|flash)\b`)
)

// ExtractUUID returns the first UUID v4 found in s (e.g., from agentapi stdout), or "".
func ExtractUUID(s string) string {
	return strings.ToLower(reUUID.FindString(s))
}

// ResolveModel selects the Antigravity model ID and thinking depth (low/medium/high)
// based on explicit user overrides or automatic task-complexity classification.
//
// Verified Antigravity model IDs (`agy models`):
//   - gemini-3.8-flash-{low,medium,high}
//   - gemini-4-argon-{low,medium,high}
//   - claude-sonnet-5-5-{low,medium,high}
//   - claude-opus-5-5-{low,medium,high}
//   - gpt-oss-120b-medium
func ResolveModel(prompt, explicitModel, explicitDepth, explicitTier string) (modelID, tier, subagentModel string) {
	depth := strings.ToLower(strings.TrimSpace(explicitDepth))
	model := strings.ToLower(strings.TrimSpace(explicitModel))
	tier = strings.ToLower(strings.TrimSpace(explicitTier))

	// Check natural-language model hint in prompt if explicitModel was not provided
	if model == "" {
		if m := rePromptModelHint.FindStringSubmatch(prompt); len(m) == 2 {
			model = strings.ToLower(m[1])
		}
	}

	// 1. Infer tier from prompt if not explicitly specified
	if tier == "" {
		switch {
		case reDeepTask.MatchString(prompt):
			tier = "deep"
		case reQuickTask.MatchString(prompt):
			tier = "quick"
		default:
			tier = "standard"
		}
	}

	// 2. Default depth + model family per tier when not overridden by user
	switch tier {
	case "quick":
		if depth == "" {
			depth = "low"
		}
		if model == "" {
			model = "gemini-3.8-flash"
		}
		subagentModel = "flash_lite"
	case "deep":
		if depth == "" {
			depth = "high"
		}
		if model == "" {
			model = "gemini-4-argon"
		}
		subagentModel = "pro"
	default: // "standard"
		tier = "standard"
		if depth == "" {
			depth = "high"
		}
		if model == "" {
			model = "gemini-3.8-flash"
		}
		subagentModel = "flash"
	}

	// Normalize shorthand model aliases
	switch model {
	case "flash":
		model = "gemini-3.8-flash"
		subagentModel = "flash"
	case "flash_lite", "flash-lite":
		model = "gemini-3.8-flash"
		depth = "low"
		subagentModel = "flash_lite"
	case "argon", "pro":
		model = "gemini-4-argon"
		subagentModel = "pro"
	case "sonnet", "claude-sonnet":
		model = "claude-sonnet-5-5"
		subagentModel = "pro"
	case "opus", "claude-opus":
		model = "claude-opus-5-5"
		subagentModel = "pro"
	}

	// If model already ends with -low, -medium, or -high, keep it as-is
	if strings.HasSuffix(model, "-low") || strings.HasSuffix(model, "-medium") || strings.HasSuffix(model, "-high") {
		return model, tier, subagentModel
	}
	return fmt.Sprintf("%s-%s", model, depth), tier, subagentModel
}

// ClassifyInboxFile routes a filename in local/inbox/ to the matching skill name.
// Unmatched files fall back to "inbox-triage" so open-ended drops are always handled.
func ClassifyInboxFile(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".csv", ".ofx", ".qfx":
		return "ledger-ingest"
	case ".pdf":
		if reTaxForm.MatchString(filename) {
			return "tax-prep"
		}
		if reSecFiling.MatchString(filename) {
			return "thesis-check"
		}
	case ".htm", ".html":
		if reSecFiling.MatchString(filename) {
			return "thesis-check"
		}
	}
	return "inbox-triage"
}

func quotaFilePath(vaultDir string, now time.Time) string {
	return filepath.Join(vaultDir, "memory", fmt.Sprintf("quota-%s.json", now.Format("2006-01-02")))
}

// LoadQuota reads today's QuotaState from local/memory/quota-YYYY-MM-DD.json.
func LoadQuota(vaultDir string) QuotaState {
	now := time.Now()
	p := quotaFilePath(vaultDir, now)
	state := QuotaState{
		Date:           now.Format("2006-01-02"),
		MaxDailySpawns: 15,
	}
	if data, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	if state.MaxDailySpawns <= 0 {
		state.MaxDailySpawns = 15
	}
	return state
}

// SaveQuota persists today's QuotaState.
func SaveQuota(vaultDir string, state QuotaState) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(quotaFilePath(vaultDir, time.Now()), data, 0o644)
}

// SpawnTask invokes `agentapi new-conversation --model=<modelID>` (or simulates it if dryRun is true),
// enforcing the daily spawn budget, auto-downgrading to gemini-3.8-flash-medium if a higher-tier model
// hits 429 RESOURCE_EXHAUSTED, and tripping the circuit breaker only if Flash is also exhausted.
func SpawnTask(vaultDir, trigger, skill, title, prompt, modelID string, dryRun bool) (string, error) {
	if modelID == "" {
		modelID, _, _ = ResolveModel(prompt, "", "", "")
	}
	state := LoadQuota(vaultDir)
	if state.CircuitOpen {
		return "", fmt.Errorf("quota circuit breaker is OPEN: %s", state.CircuitReason)
	}
	if state.SpawnsToday >= state.MaxDailySpawns {
		state.CircuitOpen = true
		state.CircuitReason = fmt.Sprintf("daily spawn budget reached (%d/%d)", state.SpawnsToday, state.MaxDailySpawns)
		_ = SaveQuota(vaultDir, state)
		return "", fmt.Errorf("daily spawn budget exhausted (%d/%d)", state.SpawnsToday, state.MaxDailySpawns)
	}

	var output string
	if dryRun {
		output = fmt.Sprintf("[dry-run] agentapi new-conversation --model=%s --title=%q (skill=%s)", modelID, title, skill)
	} else {
		cmd := exec.Command("agentapi", "new-conversation", "--model="+modelID, "--title="+title, "--", prompt)
		outBytes, err := cmd.CombinedOutput()
		output = strings.TrimSpace(string(outBytes))
		if err != nil {
			// Quota-aware fallback: if a non-Flash model hit 429, retry once on gemini-3.8-flash-medium
			if (strings.Contains(output, "RESOURCE_EXHAUSTED") || strings.Contains(output, "429")) && !strings.Contains(modelID, "flash") {
				fallbackModel := "gemini-3.8-flash-medium"
				fbCmd := exec.Command("agentapi", "new-conversation", "--model="+fallbackModel, "--title="+title, "--", prompt)
				if fbBytes, fbErr := fbCmd.CombinedOutput(); fbErr == nil {
					modelID = fallbackModel + " (fallback)"
					output = strings.TrimSpace(string(fbBytes))
					err = nil
				}
			}
		}
		if err != nil {
			if strings.Contains(output, "RESOURCE_EXHAUSTED") || strings.Contains(output, "429") {
				state.CircuitOpen = true
				state.CircuitReason = "RESOURCE_EXHAUSTED (429) detected from agentapi"
			}
			state.Events = append(state.Events, SpawnEvent{
				Timestamp: time.Now().Format(time.RFC3339),
				Trigger:   trigger,
				Skill:     skill,
				Model:     modelID,
				Outcome:   "error: " + err.Error(),
			})
			_ = SaveQuota(vaultDir, state)
			return output, fmt.Errorf("agentapi error: %w (%s)", err, output)
		}
	}

	state.SpawnsToday++
	state.LastStatus = fmt.Sprintf("SPAWNED (%s -> /%s [%s])", trigger, skill, modelID)
	state.Events = append(state.Events, SpawnEvent{
		Timestamp: time.Now().Format(time.RFC3339),
		Trigger:   trigger,
		Skill:     skill,
		Model:     modelID,
		Outcome:   output,
	})
	_ = SaveQuota(vaultDir, state)
	return output, nil
}

// SpawnFlashTask is a convenience wrapper for background watcher spawns pinned to Flash.
func SpawnFlashTask(vaultDir, trigger, skill, title, prompt string, dryRun bool) (string, error) {
	modelID, _, _ := ResolveModel(prompt, "flash", "", "")
	return SpawnTask(vaultDir, trigger, skill, title, prompt, modelID, dryRun)
}

// ProcessInbox scans local/inbox/ for newly dropped files, moves each to local/inbox/queued/,
// routes it to its matching skill, and spawns (or dry-runs) a Flash task.
func ProcessInbox(vaultDir string, dryRun bool) ([]string, error) {
	inboxDir := filepath.Join(vaultDir, "inbox")
	queuedDir := filepath.Join(inboxDir, "queued")
	entries, err := os.ReadDir(inboxDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var processed []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if err := os.MkdirAll(queuedDir, 0o755); err != nil {
			return processed, err
		}
		srcPath := filepath.Join(inboxDir, e.Name())
		dstPath := filepath.Join(queuedDir, e.Name())
		if err := os.Rename(srcPath, dstPath); err != nil {
			continue
		}
		skill := ClassifyInboxFile(e.Name())
		title := fmt.Sprintf("[Auto] Inbox Drop: %s (/%s)", e.Name(), skill)
		prompt := fmt.Sprintf("A new file was dropped into the inbox and moved to %s. Activate the %s skill and process this file.", dstPath, skill)
		out, err := SpawnFlashTask(vaultDir, "inbox-drop:"+e.Name(), skill, title, prompt, dryRun)
		if err != nil {
			processed = append(processed, fmt.Sprintf("%s -> /%s (queued; spawn deferred: %v)", e.Name(), skill, err))
		} else {
			processed = append(processed, fmt.Sprintf("%s -> /%s (%s)", e.Name(), skill, out))
		}
	}
	return processed, nil
}

// RunHeartbeat executes the 30-minute zero-token Go pre-check loop.
// If no inbox files or dead-man's switch alerts are detected, zero LLM tokens are used.
func RunHeartbeat(vaultDir string, dryRun bool) (string, error) {
	now := time.Now()
	inboxActions, err := ProcessInbox(vaultDir, dryRun)
	if err != nil {
		return "", err
	}

	state := LoadQuota(vaultDir)
	state.LastHeartbeatAt = now.Format(time.RFC3339)

	// Dead-man's switch: after 08:00 local time, check if today's morning brief file exists
	dateStr := now.Format("2006-01-02")
	briefPath := filepath.Join(vaultDir, "notes", "general", fmt.Sprintf("brief-%s.md", dateStr))
	altBriefPath := filepath.Join(vaultDir, "notes", "briefs", fmt.Sprintf("%s.md", dateStr))
	deadManWarning := ""
	if now.Hour() >= 8 {
		_, err1 := os.Stat(briefPath)
		_, err2 := os.Stat(altBriefPath)
		if os.IsNotExist(err1) && os.IsNotExist(err2) {
			deadManWarning = fmt.Sprintf(" [WARN: %s not generated yet]", filepath.Base(briefPath))
		}
	}

	if len(inboxActions) == 0 {
		state.LastStatus = "HEARTBEAT_OK (0 LLM tokens)" + deadManWarning
		_ = SaveQuota(vaultDir, state)
		return state.LastStatus, nil
	}

	summary := fmt.Sprintf("Processed %d inbox item(s): %s%s", len(inboxActions), strings.Join(inboxActions, "; "), deadManWarning)
	state.LastStatus = summary
	_ = SaveQuota(vaultDir, state)
	return summary, nil
}
