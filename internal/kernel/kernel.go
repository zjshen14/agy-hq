package kernel

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// ArmedTrigger tracks a proactive schedule from local/triggers.yaml that has been armed
// in the active Front Desk conversation via Antigravity's native `schedule` tool.
type ArmedTrigger struct {
	ID      string `json:"id"`
	Cron    string `json:"cron"`
	Skill   string `json:"skill,omitempty"`
	ArmedAt string `json:"armedAt"`
}

// FrontDeskLease tracks the single active "Front Desk" intake conversationId in local/memory/front_desk.json
// to prevent split-brain race conditions across multiple top-level conversations.
type FrontDeskLease struct {
	ConversationID string                  `json:"conversationId"`
	ArtifactDir    string                  `json:"artifactDir,omitempty"`
	ConsolePinned  bool                    `json:"consolePinned,omitempty"`
	ArmedTriggers  map[string]ArmedTrigger `json:"armedTriggers,omitempty"`
	UpdatedAt      string                  `json:"updatedAt"`
}

// SanitizeTaskTitle normalizes a task or subagent title into a clean, single-line string
// safe for Markdown tables and SessionStart context injection.
func SanitizeTaskTitle(title, slug string) string {
	s := strings.TrimSpace(title)
	if idx := strings.Index(s, "<USER_REQUEST>"); idx >= 0 {
		s = s[idx+len("<USER_REQUEST>"):]
	}
	if idx := strings.Index(s, "</USER_REQUEST>"); idx >= 0 {
		s = s[:idx]
	}
	if idx := strings.Index(s, "<ADDITIONAL_METADATA>"); idx >= 0 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)

	lines := strings.Split(s, "\n")
	var chosen string
	prefixOnly := ""
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if prefixOnly == "" && (l == "[Task:general]" || strings.HasSuffix(l, "]")) && !strings.Contains(l, " ") {
			prefixOnly = l
			continue
		}
		if prefixOnly != "" {
			chosen = prefixOnly + " " + l
		} else {
			chosen = l
		}
		break
	}
	if chosen == "" {
		chosen = prefixOnly
	}
	chosen = strings.Join(strings.Fields(chosen), " ")
	chosen = strings.ReplaceAll(chosen, "|", "-")

	if chosen == "" {
		chosen = "[Task:general] Untitled Task"
	} else if !strings.HasPrefix(chosen, "[Task:") && !strings.HasPrefix(chosen, "[Subagent:") && !strings.HasPrefix(chosen, "[HQ]") && !strings.HasPrefix(chosen, "[Auto]") {
		scope := "general"
		if parts := strings.SplitN(slug, "-", 2); len(parts) > 0 && parts[0] != "" && parts[0] != "conv" {
			scope = parts[0]
		}
		chosen = fmt.Sprintf("[Task:%s] %s", scope, chosen)
	}

	if runes := []rune(chosen); len(runes) > 88 {
		chosen = string(runes[:85]) + "..."
	}
	return chosen
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

// FindTaskByConversationID finds an ActiveTask by its ConversationID in local/memory/tasks.json.
func FindTaskByConversationID(vaultDir, convID string) (ActiveTask, bool) {
	if convID == "" {
		return ActiveTask{}, false
	}
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return ActiveTask{}, false
	}
	for _, t := range tasks {
		if t.ConversationID == convID {
			return t, true
		}
	}
	return ActiveTask{}, false
}

// LinkPendingSpawnTask links an active task in local/memory/tasks.json that was registered
// with an empty ConversationID (e.g., during `hq spawn`) to convID.
func LinkPendingSpawnTask(vaultDir, convID, model string) (ActiveTask, bool) {
	if convID == "" {
		return ActiveTask{}, false
	}
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return ActiveTask{}, false
	}
	for i := len(tasks) - 1; i >= 0; i-- {
		if tasks[i].ConversationID == "" && tasks[i].Status == "active" {
			tasks[i].ConversationID = convID
			if model != "" && tasks[i].Model == "" {
				tasks[i].Model = model
			}
			tasks[i].LastUpdatedAt = time.Now().Format(time.RFC3339)
			if data, mErr := json.MarshalIndent(tasks, "", "  "); mErr == nil {
				_ = os.WriteFile(tasksFilePath(vaultDir), data, 0o644)
			}
			return tasks[i], true
		}
	}
	return ActiveTask{}, false
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
			task.Title = SanitizeTaskTitle(task.Title, task.TaskSlug)
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
				tasks[i].Title = SanitizeTaskTitle(tasks[i].Title, tasks[i].TaskSlug)
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
		task.Title = SanitizeTaskTitle(task.Title, task.TaskSlug)
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

// CancelTasks marks tasks matching conversationIDs (or taskSlugs) as cancelled.
func CancelTasks(vaultDir string, conversationIDs []string, reason string) error {
	if len(conversationIDs) == 0 {
		return nil
	}
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return err
	}
	targetSet := make(map[string]bool)
	for _, id := range conversationIDs {
		targetSet[id] = true
	}
	changed := false
	for i := range tasks {
		if targetSet[tasks[i].ConversationID] || targetSet[tasks[i].TaskSlug] {
			tasks[i].Status = "cancelled"
			tasks[i].LastUpdatedAt = time.Now().Format(time.RFC3339)
			if strings.TrimSpace(reason) != "" {
				tasks[i].LastSummary = strings.TrimSpace(reason)
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tasksFilePath(vaultDir), data, 0o644)
}

// CancelAllSubagentTasks marks all active subagent tasks as cancelled.
func CancelAllSubagentTasks(vaultDir string, reason string) error {
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return err
	}
	changed := false
	for i := range tasks {
		if tasks[i].Status == "active" && (strings.HasPrefix(tasks[i].TaskSlug, "subagent-") || strings.HasPrefix(tasks[i].Title, "[Subagent:")) {
			tasks[i].Status = "cancelled"
			tasks[i].LastUpdatedAt = time.Now().Format(time.RFC3339)
			if strings.TrimSpace(reason) != "" {
				tasks[i].LastSummary = strings.TrimSpace(reason)
			}
			changed = true
		}
	}
	if !changed {
		return nil
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

func saveFrontDeskLease(vaultDir string, lease FrontDeskLease) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(frontDeskPath(vaultDir), data, 0o644)
}

// SetFrontDesk atomically claims the single Front Desk intake lease for convID.
// If convID already holds the lease, ConsolePinned and ArmedTriggers are preserved.
func SetFrontDesk(vaultDir, convID, artifactDir string) error {
	if convID == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	artifactDir = strings.TrimSpace(artifactDir)
	prev, hasPrev := GetFrontDesk(vaultDir)
	if artifactDir == "" && hasPrev && prev.ConversationID == convID && prev.ArtifactDir != "" {
		artifactDir = prev.ArtifactDir
	}
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
	if hasPrev && prev.ConversationID != convID && prev.ArtifactDir != "" {
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
	if hasPrev && prev.ConversationID == convID {
		lease.ConsolePinned = prev.ConsolePinned
		lease.ArmedTriggers = prev.ArmedTriggers
	}
	if artifactDir != "" {
		_ = os.WriteFile(filepath.Join(vaultDir, "memory", "active_artifact_dir.txt"), []byte(artifactDir), 0o644)
	}
	_ = os.Remove(filepath.Join(vaultDir, "memory", ".vacated"))
	if err := saveFrontDeskLease(vaultDir, lease); err != nil {
		return err
	}
	ReconcileFrontDeskFromTranscript(vaultDir, "")
	return nil
}

// MarkConsolePinned records that the active Front Desk has pinned hq_console.md via write_file.
func MarkConsolePinned(vaultDir, convID string) error {
	fd, ok := GetFrontDesk(vaultDir)
	if !ok {
		return nil
	}
	if convID != "" && fd.ConversationID != convID {
		return nil
	}
	if fd.ConsolePinned {
		return nil
	}
	fd.ConsolePinned = true
	fd.UpdatedAt = time.Now().Format(time.RFC3339)
	return saveFrontDeskLease(vaultDir, fd)
}

func matchScheduleTrigger(schedules []ScheduleTrigger, cronExpr, prompt string) (ScheduleTrigger, bool) {
	normCron := strings.Join(strings.Fields(strings.TrimSpace(cronExpr)), " ")
	if strings.HasPrefix(strings.ToUpper(normCron), "CRON_TZ=") {
		parts := strings.Fields(normCron)
		if len(parts) > 1 {
			normCron = strings.Join(parts[1:], " ")
		}
	}
	lowerPrompt := strings.ToLower(prompt)
	for _, s := range schedules {
		if normCron != "" && s.FiveFieldCron() == normCron {
			return s, true
		}
		if s.ID != "" && strings.Contains(lowerPrompt, strings.ToLower(s.ID)) {
			return s, true
		}
		if s.Skill != "" && strings.Contains(lowerPrompt, strings.ToLower(s.Skill)) {
			return s, true
		}
	}
	return ScheduleTrigger{}, false
}

// RecordArmedTrigger records a scheduled trigger armed in the active Front Desk via Antigravity's `schedule` tool.
func RecordArmedTrigger(vaultDir, convID, cronExpr, prompt string) (string, error) {
	fd, ok := GetFrontDesk(vaultDir)
	if !ok {
		return "", nil
	}
	if convID != "" && fd.ConversationID != convID {
		return "", nil
	}
	if fd.ArmedTriggers == nil {
		fd.ArmedTriggers = make(map[string]ArmedTrigger)
	}
	cfg, _ := LoadTriggers(vaultDir)
	var schedules []ScheduleTrigger
	if cfg != nil {
		schedules = cfg.Schedules
	}
	if matched, found := matchScheduleTrigger(schedules, cronExpr, prompt); found {
		fd.ArmedTriggers[matched.ID] = ArmedTrigger{
			ID:      matched.ID,
			Cron:    matched.FiveFieldCron(),
			Skill:   matched.Skill,
			ArmedAt: time.Now().Format(time.RFC3339),
		}
		fd.UpdatedAt = time.Now().Format(time.RFC3339)
		return matched.ID, saveFrontDeskLease(vaultDir, fd)
	}
	if strings.TrimSpace(cronExpr) != "" {
		customID := "cron-" + slugify(cronExpr)
		fd.ArmedTriggers[customID] = ArmedTrigger{
			ID:      customID,
			Cron:    strings.TrimSpace(cronExpr),
			ArmedAt: time.Now().Format(time.RFC3339),
		}
		fd.UpdatedAt = time.Now().Format(time.RFC3339)
		return customID, saveFrontDeskLease(vaultDir, fd)
	}
	return "", nil
}

// ReconcileFrontDeskFromTranscript inspects the active Front Desk conversation's transcript.jsonl
// (if present on disk) to backfill ConsolePinned and ArmedTriggers state.
func ReconcileFrontDeskFromTranscript(vaultDir, transcriptPath string) bool {
	fd, ok := GetFrontDesk(vaultDir)
	if !ok || fd.ConversationID == "" {
		return false
	}
	if transcriptPath == "" && fd.ArtifactDir != "" {
		transcriptPath = filepath.Join(fd.ArtifactDir, ".system_generated", "logs", "transcript.jsonl")
	}
	if transcriptPath == "" {
		return false
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return false
	}
	defer f.Close()

	cfg, _ := LoadTriggers(vaultDir)
	var schedules []ScheduleTrigger
	if cfg != nil {
		schedules = cfg.Schedules
	}

	changed := false
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)

	for scanner.Scan() {
		var step struct {
			ToolCalls []struct {
				Name string                 `json:"name"`
				Args map[string]interface{} `json:"args"`
			} `json:"tool_calls"`
		}
		if json.Unmarshal(scanner.Bytes(), &step) != nil {
			continue
		}
		for _, tc := range step.ToolCalls {
			switch tc.Name {
			case "write_file", "edit_file":
				if p, _ := tc.Args["Path"].(string); strings.HasSuffix(filepath.ToSlash(p), "/hq_console.md") || p == "hq_console.md" {
					if !fd.ConsolePinned {
						fd.ConsolePinned = true
						changed = true
					}
				}
			case "schedule":
				cronExpr, _ := tc.Args["CronExpression"].(string)
				prompt, _ := tc.Args["Prompt"].(string)
				if cronExpr == "" && prompt == "" {
					continue
				}
				if fd.ArmedTriggers == nil {
					fd.ArmedTriggers = make(map[string]ArmedTrigger)
				}
				if matched, found := matchScheduleTrigger(schedules, cronExpr, prompt); found {
					if _, exists := fd.ArmedTriggers[matched.ID]; !exists {
						fd.ArmedTriggers[matched.ID] = ArmedTrigger{
							ID:      matched.ID,
							Cron:    matched.FiveFieldCron(),
							Skill:   matched.Skill,
							ArmedAt: time.Now().Format(time.RFC3339),
						}
						changed = true
					}
				}
			}
		}
	}

	if changed {
		fd.UpdatedAt = time.Now().Format(time.RFC3339)
		_ = saveFrontDeskLease(vaultDir, fd)
	}
	return changed
}

// GetUnarmedTriggers compares local/triggers.yaml against the active Front Desk's ArmedTriggers
// and returns the armed and unarmed ScheduleTrigger slices.
func GetUnarmedTriggers(vaultDir string) (armed []ScheduleTrigger, unarmed []ScheduleTrigger) {
	cfg, err := LoadTriggers(vaultDir)
	if err != nil || cfg == nil {
		return nil, nil
	}
	fd, ok := GetFrontDesk(vaultDir)
	for _, s := range cfg.Schedules {
		if ok && fd.ArmedTriggers != nil {
			if _, isArmed := fd.ArmedTriggers[s.ID]; isArmed {
				armed = append(armed, s)
				continue
			}
		}
		unarmed = append(unarmed, s)
	}
	return armed, unarmed
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
	_ = os.Remove(filepath.Join(vaultDir, "memory", "active_artifact_dir.txt"))
	_ = os.Remove(filepath.Join(vaultDir, "memory", "front_desk_turn.json"))
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

// LoadQuota reads today's QuotaState from local/memory/quota-YYYY-MM-DD.json or quota_state.json.
func LoadQuota(vaultDir string) QuotaState {
	now := time.Now()
	today := now.Format("2006-01-02")
	p := quotaFilePath(vaultDir, now)
	state := QuotaState{
		Date:           today,
		MaxDailySpawns: 15,
	}
	data, err := os.ReadFile(p)
	if err != nil {
		fallbackPath := filepath.Join(vaultDir, "memory", "quota_state.json")
		if fbData, fbErr := os.ReadFile(fallbackPath); fbErr == nil {
			var fbState QuotaState
			if unErr := json.Unmarshal(fbData, &fbState); unErr == nil {
				if fbState.Date == today {
					state = fbState
				} else {
					state.MaxDailySpawns = fbState.MaxDailySpawns
				}
			}
		}
	} else {
		_ = json.Unmarshal(data, &state)
	}
	if state.MaxDailySpawns <= 0 {
		state.MaxDailySpawns = 15
	}
	if state.Date != today {
		state.Date = today
		state.SpawnsToday = 0
		state.CircuitOpen = false
		state.CircuitReason = ""
	}
	return state
}

// LoadQuotaState is an alias for LoadQuota.
func LoadQuotaState(vaultDir string) QuotaState {
	return LoadQuota(vaultDir)
}

// SaveQuota persists today's QuotaState to quota-YYYY-MM-DD.json and quota_state.json.
func SaveQuota(vaultDir string, state QuotaState) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	datedPath := quotaFilePath(vaultDir, time.Now())
	if err := os.WriteFile(datedPath, data, 0o644); err != nil {
		return err
	}
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "quota_state.json"), data, 0o644)
	return nil
}

// RecordSubagentSpawn increments the daily background spawn quota when a subagent is invoked.
func RecordSubagentSpawn(vaultDir, role, model, prompt string) error {
	state := LoadQuota(vaultDir)
	state.SpawnsToday++
	state.LastStatus = fmt.Sprintf("Subagent spawned: %s (%s)", role, time.Now().Format("15:04:05"))
	summary := prompt
	if runes := []rune(summary); len(runes) > 200 {
		summary = string(runes[:200]) + "..."
	}
	state.Events = append(state.Events, SpawnEvent{
		Timestamp: time.Now().Format(time.RFC3339),
		Trigger:   "invoke_subagent",
		Skill:     role,
		Model:     model,
		Outcome:   summary,
	})
	return SaveQuota(vaultDir, state)
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

var (
	reRoleWithTierScope = regexp.MustCompile(`^\[([^\]]+)\]\s*([^:]+):\s*(.*)$`)
	reRoleAlreadyNorm   = regexp.MustCompile(`^\[Subagent:([^\]]+)\]\s*([^\[]+?)(?:\s*\[([^\]]+)\])?$`)
	reRoleScopeOnly     = regexp.MustCompile(`^([^:]+):\s*(.*)$`)
)

func slugify(s string) string {
	s = strings.ToLower(s)
	reg := regexp.MustCompile(`[^a-z0-9]+`)
	s = reg.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func inferTierFromModel(model string) string {
	m := strings.ToLower(model)
	if strings.Contains(m, "pro") || strings.Contains(m, "argon") || strings.Contains(m, "opus") {
		return "deep"
	}
	if strings.Contains(m, "flash_lite") || strings.Contains(m, "flash-lite") || strings.Contains(m, "low") {
		return "quick"
	}
	return "standard"
}

// ParseSubagentRole extracts tier, scope, role title, normalized Title and TaskSlug from Role string.
func ParseSubagentRole(role, model string) (tier, scope, roleName, title, slug string) {
	trimmed := strings.TrimSpace(role)
	if m := reRoleAlreadyNorm.FindStringSubmatch(trimmed); m != nil {
		scope = strings.TrimSpace(m[1])
		roleName = strings.TrimSpace(m[2])
		tier = strings.TrimSpace(m[3])
		if tier == "" {
			tier = inferTierFromModel(model)
		}
		title = fmt.Sprintf("[Subagent:%s] %s [%s]", scope, roleName, tier)
		slug = fmt.Sprintf("subagent-%s-%s", slugify(scope), slugify(roleName))
		return tier, scope, roleName, title, slug
	}

	if m := reRoleWithTierScope.FindStringSubmatch(trimmed); m != nil {
		tier = strings.TrimSpace(m[1])
		scope = strings.TrimSpace(m[2])
		roleName = strings.TrimSpace(m[3])
		title = fmt.Sprintf("[Subagent:%s] %s [%s]", scope, roleName, tier)
		slug = fmt.Sprintf("subagent-%s-%s", slugify(scope), slugify(roleName))
		return tier, scope, roleName, title, slug
	}

	if m := reRoleScopeOnly.FindStringSubmatch(trimmed); m != nil {
		scope = strings.TrimSpace(m[1])
		roleName = strings.TrimSpace(m[2])
		tier = inferTierFromModel(model)
		title = fmt.Sprintf("[Subagent:%s] %s [%s]", scope, roleName, tier)
		slug = fmt.Sprintf("subagent-%s-%s", slugify(scope), slugify(roleName))
		return tier, scope, roleName, title, slug
	}

	roleName = trimmed
	if roleName == "" {
		roleName = "Task Worker"
	}
	scope = "general"
	tier = inferTierFromModel(model)
	title = fmt.Sprintf("[Subagent:%s] %s [%s]", scope, roleName, tier)
	slug = fmt.Sprintf("subagent-%s", slugify(roleName))
	return tier, scope, roleName, title, slug
}

// PendingSubagent stores metadata of a subagent invoked via invoke_subagent.
type PendingSubagent struct {
	Role         string    `json:"role"`
	Model        string    `json:"model"`
	Tier         string    `json:"tier,omitempty"`
	TaskSlug     string    `json:"taskSlug"`
	Title        string    `json:"title"`
	PromptPrefix string    `json:"promptPrefix,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

func pendingSubagentsFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "pending_subagents.json")
}

func loadPendingSubagents(vaultDir string) ([]PendingSubagent, error) {
	data, err := os.ReadFile(pendingSubagentsFilePath(vaultDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var items []PendingSubagent
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func savePendingSubagents(vaultDir string, items []PendingSubagent) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pendingSubagentsFilePath(vaultDir), data, 0o644)
}

// QueuePendingSubagent enqueues metadata for an invoked subagent.
func QueuePendingSubagent(vaultDir, role, model, prompt string) (PendingSubagent, error) {
	tier, _, _, title, slug := ParseSubagentRole(role, model)
	promptPrefix := prompt
	if runes := []rune(promptPrefix); len(runes) > 200 {
		promptPrefix = string(runes[:200]) + "..."
	}
	item := PendingSubagent{
		Role:         role,
		Model:        model,
		Tier:         tier,
		TaskSlug:     slug,
		Title:        title,
		PromptPrefix: promptPrefix,
		CreatedAt:    time.Now(),
	}

	queue, _ := loadPendingSubagents(vaultDir)
	cutoff := time.Now().Add(-60 * time.Minute)
	var active []PendingSubagent
	for _, q := range queue {
		if q.CreatedAt.After(cutoff) {
			active = append(active, q)
		}
	}
	active = append(active, item)
	err := savePendingSubagents(vaultDir, active)
	return item, err
}

// PopPendingSubagent pops the oldest unclaimed pending subagent created within maxAge.
func PopPendingSubagent(vaultDir string, maxAge time.Duration) (PendingSubagent, bool) {
	queue, err := loadPendingSubagents(vaultDir)
	if err != nil || len(queue) == 0 {
		return PendingSubagent{}, false
	}
	cutoff := time.Now().Add(-maxAge)
	var remaining []PendingSubagent
	var matched PendingSubagent
	found := false
	for _, item := range queue {
		if !found && item.CreatedAt.After(cutoff) {
			matched = item
			found = true
			continue
		}
		if item.CreatedAt.After(cutoff) {
			remaining = append(remaining, item)
		}
	}
	if found {
		_ = savePendingSubagents(vaultDir, remaining)
		return matched, true
	}
	return PendingSubagent{}, false
}

var staticModelEnumMap = map[string]string{
	"gemini-4-argon-high":      "MODEL_PLACEHOLDER_M417",
	"gemini-4-argon-medium":    "MODEL_PLACEHOLDER_M418",
	"gemini-4-argon-low":       "MODEL_PLACEHOLDER_M419",
	"gemini-3.8-flash-high":    "MODEL_PLACEHOLDER_M318",
	"gemini-3.8-flash-medium":  "MODEL_PLACEHOLDER_M319",
	"gemini-3.8-flash-low":     "MODEL_PLACEHOLDER_M320",
	"claude-opus-5-5-low":      "MODEL_PLACEHOLDER_M400",
	"claude-opus-5-5-medium":   "MODEL_PLACEHOLDER_M401",
	"claude-opus-5-5-high":     "MODEL_PLACEHOLDER_M402",
	"claude-sonnet-5-5-low":    "MODEL_PLACEHOLDER_M403",
	"claude-sonnet-5-5-medium": "MODEL_PLACEHOLDER_M404",
	"claude-sonnet-5-5-high":   "MODEL_PLACEHOLDER_M405",
	"gpt-oss-120b-medium":      "MODEL_OPENAI_GPT_OSS_120B_MEDIUM",
}

// ResolveModelEnum maps a human-readable Antigravity model ID (e.g. "claude-opus-5-5-high")
// to its LanguageServerService Model enum string (e.g. "MODEL_PLACEHOLDER_M402") by querying
// GetCascadeModelConfigData on the local language server with a static fallback table.
func ResolveModelEnum(lsAddr, csrfToken, modelID string) (string, error) {
	trimmed := strings.TrimSpace(modelID)
	if strings.HasPrefix(trimmed, "MODEL_") {
		return trimmed, nil
	}
	normalized := strings.ToLower(trimmed)

	if lsAddr != "" && csrfToken != "" {
		url := fmt.Sprintf("http://%s/exa.language_server_pb.LanguageServerService/GetCascadeModelConfigData", lsAddr)
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-codeium-csrf-token", csrfToken)
			client := &http.Client{Timeout: 2 * time.Second}
			if resp, err := client.Do(req); err == nil {
				defer resp.Body.Close()
				var payload struct {
					ClientModelConfigs []struct {
						ModelID      string `json:"modelId"`
						ModelOrAlias struct {
							Model string `json:"model"`
						} `json:"modelOrAlias"`
					} `json:"clientModelConfigs"`
				}
				if json.NewDecoder(resp.Body).Decode(&payload) == nil {
					for _, cfg := range payload.ClientModelConfigs {
						if strings.EqualFold(cfg.ModelID, normalized) && cfg.ModelOrAlias.Model != "" {
							return cfg.ModelOrAlias.Model, nil
						}
					}
				}
			}
		}
	}

	if enumVal, ok := staticModelEnumMap[normalized]; ok {
		return enumVal, nil
	}
	return "", fmt.Errorf("unknown Antigravity model ID %q", modelID)
}

// SpawnSubagent launches a nested child subagent conversation under parentConvID on any supported
// Antigravity model (including cross-family models such as claude-opus-5-5-high or claude-sonnet-5-5-high)
// via LanguageServerService/StartCascade (with parentConversationId, subagentDescriptor, and agentScriptItem)
// followed by delivering the initial prompt from parentConvID.
func SpawnSubagent(vaultDir, parentConvID, role, prompt, modelID, workspaceDir string, dryRun bool) (string, error) {
	if modelID == "" {
		modelID, _, _ = ResolveModel(prompt, "", "", "")
	} else if !strings.HasPrefix(strings.TrimSpace(modelID), "MODEL_") {
		modelID, _, _ = ResolveModel(prompt, modelID, "", "")
	}
	parentConvID = strings.TrimSpace(parentConvID)
	if parentConvID == "" {
		parentConvID = strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	}
	role = strings.TrimSpace(role)
	if role == "" {
		role = "[standard] general: Subagent Worker"
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

	tier, _, _, title, slug := ParseSubagentRole(role, modelID)

	if dryRun {
		output := fmt.Sprintf("[dry-run] hq subagent --parent=%s --model=%s --role=%q", parentConvID, modelID, role)
		state.SpawnsToday++
		state.LastStatus = fmt.Sprintf("SUBAGENT SPAWNED (%s [%s])", role, modelID)
		state.Events = append(state.Events, SpawnEvent{
			Timestamp: time.Now().Format(time.RFC3339),
			Trigger:   "hq-subagent:" + slug,
			Skill:     role,
			Model:     modelID,
			Outcome:   output,
		})
		_ = SaveQuota(vaultDir, state)
		return output, nil
	}

	lsAddr := strings.TrimSpace(os.Getenv("ANTIGRAVITY_LS_ADDRESS"))
	csrfToken := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CSRF_TOKEN"))
	if lsAddr == "" || csrfToken == "" {
		return "", fmt.Errorf("ANTIGRAVITY_LS_ADDRESS and ANTIGRAVITY_CSRF_TOKEN are required to spawn a live subagent")
	}

	modelEnum, err := ResolveModelEnum(lsAddr, csrfToken, modelID)
	if err != nil {
		return "", err
	}

	reqBody := map[string]interface{}{
		"baseTrajectoryIdentifier": map[string]interface{}{
			"lastActiveDoc": true,
		},
		"source":               "CORTEX_TRAJECTORY_SOURCE_AGENT_API",
		"trajectoryType":       "CORTEX_TRAJECTORY_TYPE_CASCADE",
		"requestedModel":       modelEnum,
		"parentConversationId": parentConvID,
		"subagentDescriptor": map[string]interface{}{
			"typeName": "self",
			"role":     role,
		},
		"agentScriptItem": map[string]interface{}{
			"name": "self",
		},
		"customAgentSpec": map[string]interface{}{
			"codingAgent":                 map[string]interface{}{},
			"commandExecutionPolicy":      "eager",
			"enforcedWorkspaceValidation": false,
			"cascadeConfig": map[string]interface{}{
				"plannerConfig": map[string]interface{}{
					"planModel": modelEnum,
					"requestedModel": map[string]interface{}{
						"model": modelEnum,
					},
				},
			},
		},
	}

	if projID := strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID")); projID != "" {
		reqBody["projectEnvConfig"] = map[string]interface{}{
			"projectId":                 projID,
			"defaultProjectEnvironment": map[string]interface{}{},
		}
	} else if workspaceDir != "" {
		if absDir, absErr := filepath.Abs(workspaceDir); absErr == nil {
			workspaceDir = absDir
		}
		reqBody["workspaceUris"] = []string{"file://" + filepath.ToSlash(workspaceDir)}
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	startURL := fmt.Sprintf("http://%s/exa.language_server_pb.LanguageServerService/StartCascade", lsAddr)
	req, err := http.NewRequest(http.MethodPost, startURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-codeium-csrf-token", csrfToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("StartCascade RPC failed: %w", err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("StartCascade HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}

	var startResp struct {
		CascadeID string `json:"cascadeId"`
	}
	if err := json.Unmarshal(respBytes, &startResp); err != nil || startResp.CascadeID == "" {
		return "", fmt.Errorf("invalid StartCascade response: %s", strings.TrimSpace(string(respBytes)))
	}
	cascadeID := startResp.CascadeID

	fullPrompt := prompt
	if parentConvID != "" && !strings.Contains(fullPrompt, parentConvID) {
		fullPrompt = fmt.Sprintf("%s\n\nWhen you finish, send your final report back to parent conversation %s using the send_message tool (Recipient=%q).", prompt, parentConvID, parentConvID)
	}

	// Register in local/memory/tasks.json before sending the initial message so SessionStart sees it immediately.
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       slug,
		Title:          title,
		Tier:           tier,
		Model:          modelID,
		WorktreeDir:    workspaceDir,
		ConversationID: cascadeID,
		CreatedAt:      time.Now().Format(time.RFC3339),
		LastUpdatedAt:  time.Now().Format(time.RFC3339),
		LastSummary:    prompt,
		Status:         "active",
	})

	// Deliver initial message via agentapi send-message (SendAgentMessage), with Connect JSON fallback.
	sent := false
	if parentConvID != "" {
		msgCmd := exec.Command("agentapi", "send-message", "--title=Message from "+parentConvID, cascadeID, fullPrompt)
		if err := msgCmd.Run(); err == nil {
			sent = true
		}
	}
	if !sent {
		sendURL := fmt.Sprintf("http://%s/exa.language_server_pb.LanguageServerService/SendUserCascadeMessage", lsAddr)
		sendPayload, _ := json.Marshal(map[string]interface{}{
			"cascadeId": cascadeID,
			"items": []map[string]interface{}{
				{"text": fullPrompt},
			},
		})
		if sendReq, reqErr := http.NewRequest(http.MethodPost, sendURL, bytes.NewReader(sendPayload)); reqErr == nil {
			sendReq.Header.Set("Content-Type", "application/json")
			sendReq.Header.Set("x-codeium-csrf-token", csrfToken)
			if sendResp, doErr := client.Do(sendReq); doErr == nil {
				_, _ = io.Copy(io.Discard, sendResp.Body)
				_ = sendResp.Body.Close()
			}
		}
	}

	output := fmt.Sprintf("Spawned subagent %s (parent=%s, model=%s, role=%q)", cascadeID, parentConvID, modelID, role)
	state.SpawnsToday++
	state.LastStatus = fmt.Sprintf("SUBAGENT SPAWNED (%s [%s] -> %s)", role, modelID, cascadeID)
	state.Events = append(state.Events, SpawnEvent{
		Timestamp: time.Now().Format(time.RFC3339),
		Trigger:   "hq-subagent:" + slug,
		Skill:     role,
		Model:     modelID,
		Outcome:   output,
	})
	_ = SaveQuota(vaultDir, state)
	return output, nil
}
