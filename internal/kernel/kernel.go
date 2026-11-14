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
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/gchat"
	"github.com/zjshen14/agy-hq/internal/gmail"
	"github.com/zjshen14/agy-hq/internal/outbox"
)

// QuotaState tracks daily background spawns and circuit-breaker status in local/memory/quota-YYYY-MM-DD.json.
type QuotaState struct {
	Date                 string       `json:"date"`
	SpawnsToday          int          `json:"spawnsToday"`
	GatekeeperSavesToday int          `json:"gatekeeperSavesToday,omitempty"`
	MaxDailySpawns       int          `json:"maxDailySpawns"`
	CircuitOpen          bool         `json:"circuitOpen"`
	CircuitReason        string       `json:"circuitReason,omitempty"`
	LastHeartbeatAt      string       `json:"lastHeartbeatAt,omitempty"`
	LastStatus           string       `json:"lastStatus,omitempty"`
	Events               []SpawnEvent `json:"events,omitempty"`
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
	ProjectID      string                  `json:"projectId,omitempty"`
	WorkspaceDir   string                  `json:"workspaceDir,omitempty"`
	ConsolePinned  bool                    `json:"consolePinned,omitempty"`
	ArmedTriggers  map[string]ArmedTrigger `json:"armedTriggers,omitempty"`
	UpdatedAt      string                  `json:"updatedAt"`
}

// FrontDeskPinTip is the standardized recommendation for users to pin the [HQ] Front Desk conversation to their Antigravity sidebar.
const FrontDeskPinTip = "Pin the '[HQ] Front Desk' conversation to your Antigravity sidebar so it stays permanently anchored above ephemeral task threads."

// FormatFrontDeskPinTip returns a user-facing formatted tip string.
func FormatFrontDeskPinTip() string {
	return "💡 Tip: " + FrontDeskPinTip
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

// Default lifecycle thresholds for the Task Board (RFC-0009).
const (
	DefaultIdleThreshold            = 6 * time.Hour
	DefaultDormantArchiveThreshold  = 48 * time.Hour
	DefaultStalledSubagentThreshold = 30 * time.Minute
	DefaultSubagentHotMaxAge        = 24 * time.Hour
	DefaultTaskArchiveGracePeriod   = 24 * time.Hour
	DefaultArchivedColdMaxAge       = 48 * time.Hour
	MaxHotCompletedSubagents        = 10
	MaxConsoleIdleTasks             = 5
	MaxConsoleCompletedTasks        = 3
	ConsoleCompletedMaxAge          = 2 * time.Hour
	MaxSessionStartIdleTasks        = 5
	MaxSessionStartCompletedTasks   = 3
)

// ActiveTask tracks a dedicated multi-turn Task Conversation or background Subagent
// in local/memory/tasks.json (hot working set) and local/memory/tasks_archive.jsonl (cold ledger).
type ActiveTask struct {
	TaskSlug             string `json:"taskSlug"`
	Title                string `json:"title"`
	Kind                 string `json:"kind,omitempty"`  // "task" | "subagent" | "auto"
	Scope                string `json:"scope,omitempty"` // e.g. "agy-hq", "brand", "NVDA", "personal"
	Tier                 string `json:"tier"`            // "quick" | "standard" | "deep"
	Model                string `json:"model"`
	ParentConversationID string `json:"parentConversationId,omitempty"` // Links nested subagent to parent Task or Front Desk
	WorktreeDir          string `json:"worktreeDir,omitempty"`
	RepoRoot             string `json:"repoRoot,omitempty"`
	ConversationID       string `json:"conversationId,omitempty"`
	TurnCount            int    `json:"turnCount,omitempty"`
	Pinned               bool   `json:"pinned,omitempty"`
	CreatedAt            string `json:"createdAt"`
	LastUpdatedAt        string `json:"lastUpdatedAt"`
	CompletedAt          string `json:"completedAt,omitempty"`
	ArchivedAt           string `json:"archivedAt,omitempty"`
	LastSummary          string `json:"lastSummary,omitempty"`
	Status               string `json:"status"` // active, idle, completed, archived, cancelled
}

// EffectiveKind returns "task", "subagent", or "auto", inferring from TaskSlug/Title when Kind is empty.
func (t ActiveTask) EffectiveKind() string {
	k := strings.ToLower(strings.TrimSpace(t.Kind))
	if k == "task" || k == "subagent" || k == "auto" {
		return k
	}
	if strings.HasPrefix(t.TaskSlug, "subagent-") || strings.HasPrefix(t.Title, "[Subagent:") {
		return "subagent"
	}
	if strings.HasPrefix(t.Title, "[Auto]") || strings.HasPrefix(t.TaskSlug, "auto-") {
		return "auto"
	}
	return "task"
}

// EffectiveScope extracts the task/subagent domain scope (e.g. "brand", "agy-hq", "NVDA").
func (t ActiveTask) EffectiveScope() string {
	if s := strings.TrimSpace(t.Scope); s != "" {
		return s
	}
	for _, prefix := range []string{"[Task:", "[Subagent:"} {
		if strings.HasPrefix(t.Title, prefix) {
			rest := t.Title[len(prefix):]
			if idx := strings.Index(rest, "]"); idx > 0 {
				return strings.TrimSpace(rest[:idx])
			}
		}
	}
	slug := strings.TrimPrefix(t.TaskSlug, "subagent-")
	if parts := strings.SplitN(slug, "-", 2); len(parts) > 0 && parts[0] != "" && parts[0] != "conv" {
		return parts[0]
	}
	return "general"
}

// UpdatedTime parses LastUpdatedAt (falling back to CreatedAt) into time.Time.
func (t ActiveTask) UpdatedTime() time.Time {
	for _, raw := range []string{t.LastUpdatedAt, t.CreatedAt} {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return ts
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts
		}
	}
	return time.Time{}
}

// EffectiveStatus resolves the live display status ("active", "idle", "stalled", "completed", "cancelled", "archived")
// at reference time now. Top-level tasks untouched for >= 6h (and !Pinned) resolve to "idle";
// subagents stuck in "active" for >= 30m without a Stop hook resolve to "stalled".
func (t ActiveTask) EffectiveStatus(now time.Time) string {
	st := strings.ToLower(strings.TrimSpace(t.Status))
	switch st {
	case "completed", "cancelled", "archived", "idle":
		return st
	}
	if t.Pinned {
		return "active"
	}
	updated := t.UpdatedTime()
	if !updated.IsZero() && !now.IsZero() && now.After(updated) {
		age := now.Sub(updated)
		kind := t.EffectiveKind()
		if kind == "task" && age >= DefaultIdleThreshold {
			return "idle"
		}
		if (kind == "subagent" || kind == "auto") && age >= DefaultStalledSubagentThreshold {
			return "stalled"
		}
	}
	return "active"
}

// FormatRelativeAge formats the time elapsed since UpdatedTime() as a compact human badge ("just now", "4m ago", "2h ago", "3d ago").
func (t ActiveTask) FormatRelativeAge(now time.Time) string {
	updated := t.UpdatedTime()
	if updated.IsZero() || now.IsZero() {
		return ""
	}
	if !now.After(updated) {
		return "just now"
	}
	d := now.Sub(updated)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func tasksFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "tasks.json")
}

func tasksArchiveFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "tasks_archive.jsonl")
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
	for i := range tasks {
		if tasks[i].Kind == "" {
			tasks[i].Kind = tasks[i].EffectiveKind()
		}
		if tasks[i].Scope == "" {
			tasks[i].Scope = tasks[i].EffectiveScope()
		}
	}
	return tasks, nil
}

// ListArchivedTasks reads cold historical tasks from local/memory/tasks_archive.jsonl (newest first).
// If limit > 0, returns at most limit items along with the total deduplicated count in the file.
func ListArchivedTasks(vaultDir string, limit int) ([]ActiveTask, int, error) {
	f, err := os.Open(tasksArchiveFilePath(vaultDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	var ordered []ActiveTask
	indexByKey := make(map[string]int)
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var t ActiveTask
		if json.Unmarshal([]byte(line), &t) != nil {
			continue
		}
		if t.Kind == "" {
			t.Kind = t.EffectiveKind()
		}
		if t.Scope == "" {
			t.Scope = t.EffectiveScope()
		}
		key := t.ConversationID
		if key == "" {
			key = t.TaskSlug
		}
		if idx, exists := indexByKey[key]; exists && key != "" {
			ordered[idx] = t
		} else {
			if key != "" {
				indexByKey[key] = len(ordered)
			}
			ordered = append(ordered, t)
		}
	}

	sort.SliceStable(ordered, func(i, j int) bool {
		ti, tj := ordered[i].UpdatedTime(), ordered[j].UpdatedTime()
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return i > j
	})

	total := len(ordered)
	if limit > 0 && len(ordered) > limit {
		ordered = ordered[:limit]
	}
	return ordered, total, nil
}

// CountColdArchivedTasks returns the total number of tasks in local/memory/tasks_archive.jsonl.
func CountColdArchivedTasks(vaultDir string) int {
	_, total, err := ListArchivedTasks(vaultDir, 1)
	if err != nil {
		return 0
	}
	return total
}

// AppendTasksToColdArchive appends tasks to local/memory/tasks_archive.jsonl, deduplicating by ConversationID/TaskSlug.
func AppendTasksToColdArchive(vaultDir string, toArchive []ActiveTask) error {
	if len(toArchive) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	existing, _, _ := ListArchivedTasks(vaultDir, 0)
	// Reverse existing back to chronological order before merging
	for i, j := 0, len(existing)-1; i < j; i, j = i+1, j-1 {
		existing[i], existing[j] = existing[j], existing[i]
	}
	indexByKey := make(map[string]int)
	for i, t := range existing {
		key := t.ConversationID
		if key == "" {
			key = t.TaskSlug
		}
		if key != "" {
			indexByKey[key] = i
		}
	}
	for _, t := range toArchive {
		if t.Kind == "" {
			t.Kind = t.EffectiveKind()
		}
		if t.Scope == "" {
			t.Scope = t.EffectiveScope()
		}
		key := t.ConversationID
		if key == "" {
			key = t.TaskSlug
		}
		if idx, ok := indexByKey[key]; ok && key != "" {
			existing[idx] = t
		} else {
			if key != "" {
				indexByKey[key] = len(existing)
			}
			existing = append(existing, t)
		}
	}
	var b strings.Builder
	for _, t := range existing {
		if line, err := json.Marshal(t); err == nil {
			b.Write(line)
			b.WriteByte('\n')
		}
	}
	return os.WriteFile(tasksArchiveFilePath(vaultDir), []byte(b.String()), 0o644)
}

func statusPriority(effStatus string) int {
	switch effStatus {
	case "active", "stalled":
		return 0
	case "idle":
		return 1
	case "completed", "cancelled":
		return 2
	default: // "archived"
		return 3
	}
}

// SortTasksForBoard returns a sorted copy of tasks ordered by:
// 1. Lifecycle tier (active/stalled -> idle -> completed/cancelled -> archived)
// 2. Pinned (true before false)
// 3. LastUpdatedAt descending (newest first), tie-broken by reverse slice index.
func SortTasksForBoard(tasks []ActiveTask, now time.Time) []ActiveTask {
	type indexedTask struct {
		task ActiveTask
		idx  int
	}
	items := make([]indexedTask, len(tasks))
	for i, t := range tasks {
		items[i] = indexedTask{task: t, idx: i}
	}
	sort.SliceStable(items, func(i, j int) bool {
		ti, tj := items[i].task, items[j].task
		pi := statusPriority(ti.EffectiveStatus(now))
		pj := statusPriority(tj.EffectiveStatus(now))
		if pi != pj {
			return pi < pj
		}
		if ti.Pinned != tj.Pinned {
			return ti.Pinned
		}
		ui, uj := ti.UpdatedTime(), tj.UpdatedTime()
		if !ui.Equal(uj) {
			return ui.After(uj)
		}
		return items[i].idx > items[j].idx
	})
	out := make([]ActiveTask, len(items))
	for i, it := range items {
		out[i] = it.task
	}
	return out
}

// ConsoleTaskSelection holds the tiered task slice and summary counters for hq_console.md.
type ConsoleTaskSelection struct {
	VisibleRows          []ActiveTask
	ActiveCount          int
	IdleCount            int
	CompletedCount       int
	ArchivedHotCount     int
	ArchivedColdCount    int
	HiddenIdleCount      int
	HiddenCompletedCount int
}

// SelectConsoleTasks applies the RFC-0009 per-tier visibility policy for local/CONSOLE.md and hq_console.md:
// - Tier 1 (Active / Stalled): Show 100% (never truncate genuinely active work!).
// - Tier 2 (Idle 6h..48h): Show up to MaxConsoleIdleTasks (5) newest.
// - Tier 3 (Completed / Cancelled): Show up to MaxConsoleCompletedTasks (3) newest.
// - Tier 4 (Archived): 0 table rows (summarized in footer counter).
func SelectConsoleTasks(tasks []ActiveTask, coldArchivedCount int, now time.Time) ConsoleTaskSelection {
	sorted := SortTasksForBoard(tasks, now)
	sel := ConsoleTaskSelection{
		ArchivedColdCount: coldArchivedCount,
	}
	var activeRows []ActiveTask
	var idleRows []ActiveTask
	var completedRows []ActiveTask

	for _, t := range sorted {
		eff := t.EffectiveStatus(now)
		switch eff {
		case "active", "stalled":
			sel.ActiveCount++
			activeRows = append(activeRows, t)
		case "idle":
			sel.IdleCount++
			if len(idleRows) < MaxConsoleIdleTasks {
				idleRows = append(idleRows, t)
			} else {
				sel.HiddenIdleCount++
			}
		case "completed", "cancelled":
			sel.CompletedCount++
			if len(completedRows) < MaxConsoleCompletedTasks {
				completedRows = append(completedRows, t)
			} else {
				sel.HiddenCompletedCount++
			}
		case "archived":
			sel.ArchivedHotCount++
		}
	}

	sel.VisibleRows = append(sel.VisibleRows, activeRows...)
	sel.VisibleRows = append(sel.VisibleRows, idleRows...)
	sel.VisibleRows = append(sel.VisibleRows, completedRows...)
	return sel
}

// SelectSessionStartTasks selects the bounded task slice injected into SessionStart (<SYSTEM_MESSAGE>):
// - All active/stalled tasks
// - Up to MaxSessionStartIdleTasks (5) idle tasks
// - Up to MaxSessionStartCompletedTasks (3) recently completed tasks
func SelectSessionStartTasks(tasks []ActiveTask, now time.Time) (selected []ActiveTask, omittedCount int) {
	sorted := SortTasksForBoard(tasks, now)
	idleShown := 0
	compShown := 0
	for _, t := range sorted {
		eff := t.EffectiveStatus(now)
		switch eff {
		case "active", "stalled":
			selected = append(selected, t)
		case "idle":
			if idleShown < MaxSessionStartIdleTasks {
				selected = append(selected, t)
				idleShown++
			} else {
				omittedCount++
			}
		case "completed", "cancelled":
			if compShown < MaxSessionStartCompletedTasks {
				selected = append(selected, t)
				compShown++
			} else {
				omittedCount++
			}
		default:
			omittedCount++
		}
	}
	return selected, omittedCount
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

// FindTaskAnywhere searches hot tasks.json first ("hot") and cold tasks_archive.jsonl second ("cold")
// by TaskSlug or ConversationID.
func FindTaskAnywhere(vaultDir, target string) (ActiveTask, string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return ActiveTask{}, "", false
	}
	if tasks, err := ListTasks(vaultDir); err == nil {
		for _, t := range tasks {
			if t.TaskSlug == target || t.ConversationID == target {
				return t, "hot", true
			}
		}
	}
	if archived, _, err := ListArchivedTasks(vaultDir, 0); err == nil {
		for _, t := range archived {
			if t.TaskSlug == target || t.ConversationID == target {
				return t, "cold", true
			}
		}
	}
	return ActiveTask{}, "", false
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
			if task.Kind == "" {
				task.Kind = tasks[i].Kind
			}
			if task.Scope == "" {
				task.Scope = tasks[i].Scope
			}
			if task.Tier == "" {
				task.Tier = tasks[i].Tier
			}
			if task.Model == "" {
				task.Model = tasks[i].Model
			}
			if task.ParentConversationID == "" {
				task.ParentConversationID = tasks[i].ParentConversationID
			}
			if task.WorktreeDir == "" {
				task.WorktreeDir = tasks[i].WorktreeDir
			}
			if task.RepoRoot == "" {
				task.RepoRoot = tasks[i].RepoRoot
			}
			if task.TurnCount == 0 {
				task.TurnCount = tasks[i].TurnCount
			}
			if !task.Pinned && tasks[i].Pinned {
				task.Pinned = tasks[i].Pinned
			}
			if task.CreatedAt == "" {
				task.CreatedAt = tasks[i].CreatedAt
			}
			if task.Status == "active" {
				task.ArchivedAt = ""
				task.CompletedAt = ""
			} else {
				if task.CompletedAt == "" {
					task.CompletedAt = tasks[i].CompletedAt
				}
				if task.ArchivedAt == "" {
					task.ArchivedAt = tasks[i].ArchivedAt
				}
			}
			if task.Status == "" {
				task.Status = tasks[i].Status
			}
			if task.LastSummary == "" {
				task.LastSummary = tasks[i].LastSummary
			}
			task.Title = SanitizeTaskTitle(task.Title, task.TaskSlug)
			task.Kind = task.EffectiveKind()
			task.Scope = task.EffectiveScope()
			if (task.Status == "completed" || task.Status == "cancelled") && task.CompletedAt == "" {
				task.CompletedAt = task.LastUpdatedAt
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
				if task.TurnCount > 0 {
					tasks[i].TurnCount = task.TurnCount
				}
				tasks[i].Title = SanitizeTaskTitle(tasks[i].Title, tasks[i].TaskSlug)
				tasks[i].Kind = tasks[i].EffectiveKind()
				tasks[i].Scope = tasks[i].EffectiveScope()
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
		task.Kind = task.EffectiveKind()
		task.Scope = task.EffectiveScope()
		if task.Status == "" {
			task.Status = "active"
		}
		if task.CreatedAt == "" {
			task.CreatedAt = time.Now().Format(time.RFC3339)
		}
		if task.LastUpdatedAt == "" {
			task.LastUpdatedAt = task.CreatedAt
		}
		if (task.Status == "completed" || task.Status == "cancelled") && task.CompletedAt == "" {
			task.CompletedAt = task.LastUpdatedAt
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
	nowStr := time.Now().Format(time.RFC3339)
	found := false
	for i := range tasks {
		if tasks[i].TaskSlug == slugOrConvID || tasks[i].ConversationID == slugOrConvID {
			tasks[i].Status = "completed"
			tasks[i].LastUpdatedAt = nowStr
			tasks[i].CompletedAt = nowStr
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

// SetTaskPinned pins or unpins a task in local/memory/tasks.json so it stays anchored at the top of the Active board.
func SetTaskPinned(vaultDir, slugOrConvID string, pinned bool) error {
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return err
	}
	found := false
	for i := range tasks {
		if tasks[i].TaskSlug == slugOrConvID || tasks[i].ConversationID == slugOrConvID {
			tasks[i].Pinned = pinned
			tasks[i].LastUpdatedAt = time.Now().Format(time.RFC3339)
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

// ArchiveTask archives a task in local/memory/tasks.json (matched by TaskSlug or ConversationID).
// If WorktreeDir exists on disk:
//   - runs `git -C <worktreeDir> status --porcelain`. If there are uncommitted changes and !force, returns:
//     refusing to archive %q: uncommitted changes in worktree (pass --force to override)
//   - safely removes the worktree using `git -C <repoRoot> worktree remove` (adding --force if force is true).
//
// Sets task status to "archived", sets LastUpdatedAt to now, and saves tasks.json.
func ArchiveTask(vaultDir, target string, force bool) error {
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return err
	}
	nowStr := time.Now().Format(time.RFC3339)
	found := false
	for i := range tasks {
		if tasks[i].TaskSlug == target || tasks[i].ConversationID == target {
			task := &tasks[i]
			if task.WorktreeDir != "" {
				if info, err := os.Stat(task.WorktreeDir); err == nil && info.IsDir() {
					cmdStatus := sanitizedGitCmd("-C", task.WorktreeDir, "status", "--porcelain")
					outStatus, err := cmdStatus.Output()
					if err != nil {
						return fmt.Errorf("git status check failed on %s: %w", task.WorktreeDir, err)
					}
					if len(strings.TrimSpace(string(outStatus))) > 0 && !force {
						return fmt.Errorf("refusing to archive %q: uncommitted changes in worktree (pass --force to override)", target)
					}

					repoRoot := task.RepoRoot
					if repoRoot == "" {
						repoRoot = resolveRepoRootForWorktree(vaultDir, task.WorktreeDir)
					}
					task.RepoRoot = repoRoot

					var removeArgs []string
					if force {
						removeArgs = []string{"-C", repoRoot, "worktree", "remove", "--force", task.WorktreeDir}
					} else {
						removeArgs = []string{"-C", repoRoot, "worktree", "remove", task.WorktreeDir}
					}
					cmdRemove := sanitizedGitCmd(removeArgs...)
					if out, err := cmdRemove.CombinedOutput(); err != nil {
						return fmt.Errorf("git worktree remove failed: %w (%s)", err, string(out))
					}
					if _, err := os.Stat(task.WorktreeDir); err == nil {
						_ = os.RemoveAll(task.WorktreeDir)
					}
					_ = sanitizedGitCmd("-C", repoRoot, "worktree", "prune").Run()
				}
			}

			task.Status = "archived"
			task.LastUpdatedAt = nowStr
			task.ArchivedAt = nowStr
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("task %q not found in local/memory/tasks.json", target)
	}

	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tasksFilePath(vaultDir), data, 0o644)
}

func restoreWorktreeForReopen(vaultDir string, task *ActiveTask) error {
	if task.WorktreeDir == "" {
		return nil
	}
	if _, err := os.Stat(task.WorktreeDir); !os.IsNotExist(err) {
		return nil
	}
	repoRoot := task.RepoRoot
	if repoRoot == "" {
		repoRoot = findRepoRootForTask(vaultDir, task)
	}
	branch := ""
	if gitBranchExists(repoRoot, "hq/"+task.TaskSlug) {
		branch = "hq/" + task.TaskSlug
	} else if gitBranchExists(repoRoot, task.TaskSlug) {
		branch = task.TaskSlug
	}
	_ = os.MkdirAll(filepath.Dir(task.WorktreeDir), 0o755)
	var cmd *exec.Cmd
	if branch != "" {
		cmd = sanitizedGitCmd("-C", repoRoot, "worktree", "add", task.WorktreeDir, branch)
	} else {
		cmd = sanitizedGitCmd("-C", repoRoot, "worktree", "add", "-b", "hq/"+task.TaskSlug, task.WorktreeDir)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add failed: %w (%s)", err, string(out))
	}
	return nil
}

// ReopenTask restores a task in local/memory/tasks.json (or recalls it from cold storage
// local/memory/tasks_archive.jsonl) back to active status, recreating its git worktree if needed.
func ReopenTask(vaultDir, target string) error {
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return err
	}
	nowStr := time.Now().Format(time.RFC3339)
	found := false
	for i := range tasks {
		if tasks[i].TaskSlug == target || tasks[i].ConversationID == target {
			if err := restoreWorktreeForReopen(vaultDir, &tasks[i]); err != nil {
				return err
			}
			tasks[i].Status = "active"
			tasks[i].LastUpdatedAt = nowStr
			found = true
			break
		}
	}
	if !found {
		// Check cold storage (local/memory/tasks_archive.jsonl)
		archived, _, archErr := ListArchivedTasks(vaultDir, 0)
		if archErr == nil && len(archived) > 0 {
			var recalled *ActiveTask
			var remaining []ActiveTask
			for i := len(archived) - 1; i >= 0; i-- {
				t := archived[i]
				if recalled == nil && (t.TaskSlug == target || t.ConversationID == target) {
					cp := t
					recalled = &cp
					continue
				}
				remaining = append(remaining, t)
			}
			if recalled != nil {
				if err := restoreWorktreeForReopen(vaultDir, recalled); err != nil {
					return err
				}
				recalled.Status = "active"
				recalled.LastUpdatedAt = nowStr
				tasks = append(tasks, *recalled)
				found = true
				var b strings.Builder
				for _, rem := range remaining {
					if line, mErr := json.Marshal(rem); mErr == nil {
						b.Write(line)
						b.WriteByte('\n')
					}
				}
				_ = os.WriteFile(tasksArchiveFilePath(vaultDir), []byte(b.String()), 0o644)
			}
		}
	}
	if !found {
		return fmt.Errorf("task %q not found in local/memory/tasks.json", target)
	}

	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tasksFilePath(vaultDir), data, 0o644)
}

// CompactTasksOptions configures rolling auto-archiving and hot-to-cold storage rotation.
type CompactTasksOptions struct {
	Now                      time.Time
	SubagentHotMaxAge        time.Duration
	MaxHotCompletedSubagents int
	TaskAutoArchiveAfter     time.Duration
	IdleDormantArchiveAfter  time.Duration
	ArchivedColdMaxAge       time.Duration
	DryRun                   bool
}

// CompactTasksReport summarizes what CompactTasks transitioned and rotated.
type CompactTasksReport struct {
	HotBefore         int      `json:"hotBefore"`
	HotAfter          int      `json:"hotAfter"`
	AutoArchivedSlugs []string `json:"autoArchivedSlugs,omitempty"`
	RotatedColdSlugs  []string `json:"rotatedColdSlugs,omitempty"`
	TotalColdAfter    int      `json:"totalColdAfter"`
}

func isWorktreeCleanOrAbsent(wtDir string) bool {
	if strings.TrimSpace(wtDir) == "" {
		return true
	}
	info, err := os.Stat(wtDir)
	if err != nil || !info.IsDir() {
		return true
	}
	cmd := sanitizedGitCmd("-C", wtDir, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(out))) == 0
}

// CompactTasks executes the RFC-0009 rolling lifecycle compaction:
//  1. Auto-archives completed/cancelled top-level tasks older than TaskAutoArchiveAfter (24h) if their worktree is clean.
//  2. Auto-archives unpinned idle top-level tasks untouched for > IdleDormantArchiveAfter (48h) if their worktree is clean.
//  3. Flushes completed/cancelled subagents (>24h or beyond MaxHotCompletedSubagents) and archived tasks (>48h)
//     from local/memory/tasks.json into append-only local/memory/tasks_archive.jsonl.
func CompactTasks(vaultDir string, opts CompactTasksOptions) (*CompactTasksReport, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.SubagentHotMaxAge <= 0 {
		opts.SubagentHotMaxAge = DefaultSubagentHotMaxAge
	}
	if opts.MaxHotCompletedSubagents <= 0 {
		opts.MaxHotCompletedSubagents = MaxHotCompletedSubagents
	}
	if opts.TaskAutoArchiveAfter <= 0 {
		opts.TaskAutoArchiveAfter = DefaultTaskArchiveGracePeriod
	}
	if opts.IdleDormantArchiveAfter <= 0 {
		opts.IdleDormantArchiveAfter = DefaultDormantArchiveThreshold
	}
	if opts.ArchivedColdMaxAge <= 0 {
		opts.ArchivedColdMaxAge = DefaultArchivedColdMaxAge
	}

	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return nil, err
	}
	rep := &CompactTasksReport{
		HotBefore: len(tasks),
	}
	if len(tasks) == 0 {
		rep.TotalColdAfter = CountColdArchivedTasks(vaultDir)
		return rep, nil
	}

	nowStr := opts.Now.Format(time.RFC3339)

	// Step 1: Auto-archive eligible completed (>24h) or dormant (>48h) top-level tasks with clean worktrees
	for i := range tasks {
		t := &tasks[i]
		kind := t.EffectiveKind()
		updated := t.UpdatedTime()
		age := time.Duration(0)
		if !updated.IsZero() && opts.Now.After(updated) {
			age = opts.Now.Sub(updated)
		}
		st := strings.ToLower(strings.TrimSpace(t.Status))
		shouldArchive := false
		if kind == "task" && (st == "completed" || st == "cancelled") && age >= opts.TaskAutoArchiveAfter && isWorktreeCleanOrAbsent(t.WorktreeDir) {
			shouldArchive = true
		} else if kind == "task" && (st == "active" || st == "idle") && !t.Pinned && age >= opts.IdleDormantArchiveAfter && isWorktreeCleanOrAbsent(t.WorktreeDir) {
			shouldArchive = true
		}
		if shouldArchive {
			if !opts.DryRun && t.WorktreeDir != "" {
				if info, sErr := os.Stat(t.WorktreeDir); sErr == nil && info.IsDir() {
					repoRoot := t.RepoRoot
					if repoRoot == "" {
						repoRoot = resolveRepoRootForWorktree(vaultDir, t.WorktreeDir)
					}
					_ = sanitizedGitCmd("-C", repoRoot, "worktree", "remove", t.WorktreeDir).Run()
					_ = sanitizedGitCmd("-C", repoRoot, "worktree", "prune").Run()
				}
			}
			t.Status = "archived"
			if t.ArchivedAt == "" {
				t.ArchivedAt = nowStr
			}
			rep.AutoArchivedSlugs = append(rep.AutoArchivedSlugs, t.TaskSlug)
		}
	}

	// Step 2: Rank completed/cancelled subagents newest-first to identify items exceeding MaxHotCompletedSubagents
	type subIdx struct {
		idx int
		ts  time.Time
	}
	var compSubs []subIdx
	for i, t := range tasks {
		st := strings.ToLower(strings.TrimSpace(t.Status))
		if (t.EffectiveKind() == "subagent" || t.EffectiveKind() == "auto") && (st == "completed" || st == "cancelled") {
			compSubs = append(compSubs, subIdx{idx: i, ts: t.UpdatedTime()})
		}
	}
	sort.SliceStable(compSubs, func(i, j int) bool {
		if !compSubs[i].ts.Equal(compSubs[j].ts) {
			return compSubs[i].ts.After(compSubs[j].ts)
		}
		return compSubs[i].idx > compSubs[j].idx
	})
	subOverCap := make(map[int]bool)
	for rank, item := range compSubs {
		if rank >= opts.MaxHotCompletedSubagents {
			subOverCap[item.idx] = true
		}
	}

	var keptHot []ActiveTask
	var rotatedCold []ActiveTask
	for i, t := range tasks {
		st := strings.ToLower(strings.TrimSpace(t.Status))
		kind := t.EffectiveKind()
		updated := t.UpdatedTime()
		age := time.Duration(0)
		if !updated.IsZero() && opts.Now.After(updated) {
			age = opts.Now.Sub(updated)
		}

		rotate := false
		if (kind == "subagent" || kind == "auto") && (st == "completed" || st == "cancelled") {
			if age >= opts.SubagentHotMaxAge || subOverCap[i] {
				rotate = true
			}
		} else if st == "archived" && age >= opts.ArchivedColdMaxAge {
			rotate = true
		}

		if rotate {
			rotatedCold = append(rotatedCold, t)
			rep.RotatedColdSlugs = append(rep.RotatedColdSlugs, t.TaskSlug)
		} else {
			keptHot = append(keptHot, t)
		}
	}

	rep.HotAfter = len(keptHot)
	if !opts.DryRun {
		if len(rotatedCold) > 0 {
			if err := AppendTasksToColdArchive(vaultDir, rotatedCold); err != nil {
				return rep, err
			}
		}
		if len(rotatedCold) > 0 || len(rep.AutoArchivedSlugs) > 0 {
			data, err := json.MarshalIndent(keptHot, "", "  ")
			if err != nil {
				return rep, err
			}
			if err := os.WriteFile(tasksFilePath(vaultDir), data, 0o644); err != nil {
				return rep, err
			}
		}
	}
	rep.TotalColdAfter = CountColdArchivedTasks(vaultDir)
	if opts.DryRun {
		rep.TotalColdAfter += len(rotatedCold)
	}
	return rep, nil
}

// TaskStats summarizes task counts across status, kind, tier, and storage layer for `./bin/hq tasks stats`.
type TaskStats struct {
	HotTotal  int            `json:"hotTotal"`
	ColdTotal int            `json:"coldTotal"`
	ByStatus  map[string]int `json:"byStatus"`
	ByKind    map[string]int `json:"byKind"`
	ByTier    map[string]int `json:"byTier"`
	ByScope   map[string]int `json:"byScope"`
}

// ComputeTaskStats aggregates counts across hot tasks.json and cold tasks_archive.jsonl.
func ComputeTaskStats(vaultDir string, now time.Time) TaskStats {
	if now.IsZero() {
		now = time.Now()
	}
	hot, _ := ListTasks(vaultDir)
	cold, coldCount, _ := ListArchivedTasks(vaultDir, 0)
	st := TaskStats{
		HotTotal:  len(hot),
		ColdTotal: coldCount,
		ByStatus:  make(map[string]int),
		ByKind:    make(map[string]int),
		ByTier:    make(map[string]int),
		ByScope:   make(map[string]int),
	}
	for _, t := range hot {
		eff := t.EffectiveStatus(now)
		st.ByStatus[eff]++
		st.ByKind[t.EffectiveKind()]++
		if t.Tier != "" {
			st.ByTier[t.Tier]++
		}
		st.ByScope[t.EffectiveScope()]++
	}
	for _, t := range cold {
		st.ByStatus["cold_archive"]++
		st.ByKind[t.EffectiveKind()]++
		if t.Tier != "" {
			st.ByTier[t.Tier]++
		}
		st.ByScope[t.EffectiveScope()]++
	}
	return st
}

// FilterTasks queries hot tasks (and optionally cold tasks_archive.jsonl) with status, kind, scope, and full-text search filters.
func FilterTasks(vaultDir string, activeOnly, archiveOnly, includeAllHot, includeArchive bool, kindFilter, scopeFilter, searchQuery string, limit int, now time.Time) ([]ActiveTask, error) {
	if now.IsZero() {
		now = time.Now()
	}
	hot, err := ListTasks(vaultDir)
	if err != nil {
		return nil, err
	}
	combined := append([]ActiveTask(nil), hot...)
	if includeArchive || archiveOnly {
		cold, _, _ := ListArchivedTasks(vaultDir, 0)
		seen := make(map[string]bool)
		for _, t := range combined {
			if t.ConversationID != "" {
				seen[t.ConversationID] = true
			}
			if t.TaskSlug != "" {
				seen[t.TaskSlug] = true
			}
		}
		for _, c := range cold {
			if (c.ConversationID != "" && seen[c.ConversationID]) || (c.TaskSlug != "" && seen[c.TaskSlug]) {
				continue
			}
			combined = append(combined, c)
		}
	}

	sorted := SortTasksForBoard(combined, now)
	kindFilter = strings.ToLower(strings.TrimSpace(kindFilter))
	scopeFilter = strings.ToLower(strings.TrimSpace(scopeFilter))
	q := strings.ToLower(strings.TrimSpace(searchQuery))

	var out []ActiveTask
	compShown := 0
	for _, t := range sorted {
		eff := t.EffectiveStatus(now)
		if activeOnly && eff != "active" && eff != "stalled" && eff != "idle" {
			continue
		}
		if archiveOnly && eff != "archived" {
			continue
		}
		if !activeOnly && !archiveOnly && !includeAllHot && !includeArchive && q == "" && kindFilter == "" && scopeFilter == "" {
			// Default `./bin/hq tasks` view: all active/stalled/idle + up to 5 recent completed
			if eff == "archived" {
				continue
			}
			if eff == "completed" || eff == "cancelled" {
				if compShown >= 5 {
					continue
				}
				compShown++
			}
		}
		if kindFilter != "" && t.EffectiveKind() != kindFilter {
			continue
		}
		if scopeFilter != "" && !strings.EqualFold(t.EffectiveScope(), scopeFilter) {
			continue
		}
		if q != "" {
			haystack := strings.ToLower(t.TaskSlug + " " + t.Title + " " + t.LastSummary + " " + t.ConversationID + " " + t.EffectiveScope() + " " + t.Model)
			if !strings.Contains(haystack, q) {
				continue
			}
		}
		out = append(out, t)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func sanitizedGitCmd(args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	raw := os.Environ()
	env := make([]string, 0, len(raw))
	for _, kv := range raw {
		if strings.HasPrefix(kv, "GIT_DIR=") ||
			strings.HasPrefix(kv, "GIT_WORK_TREE=") ||
			strings.HasPrefix(kv, "GIT_INDEX_FILE=") ||
			strings.HasPrefix(kv, "GIT_OBJECT_DIRECTORY=") ||
			strings.HasPrefix(kv, "GIT_ALTERNATE_OBJECT_DIRECTORIES=") ||
			strings.HasPrefix(kv, "GIT_COMMON_DIR=") ||
			strings.HasPrefix(kv, "GIT_PREFIX=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	return cmd
}

func resolveRepoRootForWorktree(vaultDir, worktreeDir string) string {
	cmdCommon := sanitizedGitCmd("-C", worktreeDir, "rev-parse", "--git-common-dir")
	if out, err := cmdCommon.Output(); err == nil {
		commonDir := strings.TrimSpace(string(out))
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Clean(filepath.Join(worktreeDir, commonDir))
		}
		if strings.HasSuffix(commonDir, "/.git") || strings.HasSuffix(commonDir, string(filepath.Separator)+".git") {
			return filepath.Dir(commonDir)
		}
		return commonDir
	}
	if envRoot := os.Getenv("HQ_ROOT_DIR"); envRoot != "" {
		if abs, err := filepath.Abs(envRoot); err == nil {
			return abs
		}
	}
	return filepath.Dir(vaultDir)
}

func gitBranchExists(repoDir, branch string) bool {
	if repoDir == "" || branch == "" {
		return false
	}
	cmd := sanitizedGitCmd("-C", repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return cmd.Run() == nil
}

func findRepoRootForTask(vaultDir string, task *ActiveTask) string {
	rootCandidate := ""
	if envRoot := os.Getenv("HQ_ROOT_DIR"); envRoot != "" {
		if abs, err := filepath.Abs(envRoot); err == nil {
			rootCandidate = abs
		}
	}
	if rootCandidate == "" {
		rootCandidate = filepath.Dir(vaultDir)
	}

	if gitBranchExists(rootCandidate, "hq/"+task.TaskSlug) || gitBranchExists(rootCandidate, task.TaskSlug) {
		return rootCandidate
	}

	reposDir := filepath.Join(vaultDir, "repos")
	if entries, err := os.ReadDir(reposDir); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasSuffix(e.Name(), ".git") {
				bareRepo := filepath.Join(reposDir, e.Name())
				if gitBranchExists(bareRepo, "hq/"+task.TaskSlug) || gitBranchExists(bareRepo, task.TaskSlug) {
					return bareRepo
				}
			}
		}
	}

	return rootCandidate
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
	if IsSubagentConversation(vaultDir, convID) {
		return fmt.Errorf("cannot claim Front Desk lease for a child subagent")
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
		lease.ProjectID = prev.ProjectID
		lease.WorkspaceDir = prev.WorkspaceDir
	}
	if os.Getenv("ANTIGRAVITY_SIDECAR_WEB_PORT") == "" {
		if projID := strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID")); projID != "" {
			lease.ProjectID = projID
		}
	}
	if lease.WorkspaceDir == "" && vaultDir != "" {
		cleanV := filepath.Clean(vaultDir)
		if filepath.Base(cleanV) == "local" {
			lease.WorkspaceDir = filepath.Dir(cleanV)
		}
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

// SetFrontDeskWorkspace records the primary workspace root associated with the active Front Desk conversation.
func SetFrontDeskWorkspace(vaultDir, convID, workspaceDir string) error {
	workspaceDir = strings.TrimSpace(workspaceDir)
	if workspaceDir == "" || convID == "" {
		return nil
	}
	fd, ok := GetFrontDesk(vaultDir)
	if !ok || fd.ConversationID != convID {
		return nil
	}
	if abs, err := filepath.Abs(workspaceDir); err == nil {
		workspaceDir = abs
	}
	if fd.WorkspaceDir == workspaceDir {
		return nil
	}
	fd.WorkspaceDir = workspaceDir
	fd.UpdatedAt = time.Now().Format(time.RFC3339)
	return saveFrontDeskLease(vaultDir, fd)
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
				rawPath, _ := tc.Args["Path"].(string)
				p := strings.Trim(strings.TrimSpace(rawPath), "\"")
				if strings.HasSuffix(filepath.ToSlash(p), "/hq_console.md") || p == "hq_console.md" {
					if !fd.ConsolePinned {
						fd.ConsolePinned = true
						changed = true
					}
				}
			case "schedule":
				rawCron, _ := tc.Args["CronExpression"].(string)
				rawPrompt, _ := tc.Args["Prompt"].(string)
				cronExpr := strings.Trim(strings.TrimSpace(rawCron), "\"")
				prompt := strings.Trim(strings.TrimSpace(rawPrompt), "\"")
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
// (and active background daemon coverage for daemon_gatekeeper / isolated_worker schedules)
// and returns the armed and unarmed ScheduleTrigger slices.
func GetUnarmedTriggers(vaultDir string) (armed []ScheduleTrigger, unarmed []ScheduleTrigger) {
	cfg, err := LoadTriggers(vaultDir)
	if err != nil || cfg == nil {
		return nil, nil
	}
	daemonActive := IsDaemonActiveFast(vaultDir)
	fd, ok := GetFrontDesk(vaultDir)
	for _, s := range cfg.Schedules {
		if daemonActive && (s.EffectiveMode() == "daemon_gatekeeper" || s.EffectiveMode() == "isolated_worker") {
			armed = append(armed, s)
			continue
		}
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
	reTaxForm          = regexp.MustCompile(`(?i)(w-?2|1099|1098|k-?1|tax)`)
	reSecFiling        = regexp.MustCompile(`(?i)(10-[kq]|8-k|earnings)`)
	reDeepHighOnlyTask = regexp.MustCompile(`(?i)\b(architect|architecture|design|security|audit|race\s+condition|root\s+cause|falsif|kc-?1|breach|rfc)\b`)
	reDeepTask         = regexp.MustCompile(`(?i)\b(architect|architecture|design|security|audit|race\s+condition|root\s+cause|thesis|falsif|kc-?1|breach|rfc|complex|deep)\b`)
	reDeepMediumTask   = regexp.MustCompile(`(?i)\b(sleep[- ]?consolidat\w*|consolidat(?:e|ion)|dreaming|research\s+memo|deep\s+research)\b`)
	reHeavySweepTask   = regexp.MustCompile(`(?i)\b(exhaustive|multi[- ]?file\s+refactor\w*|hard\s+e2e|e2e\s+debug\w*|test\s+debug\w*|sweep)\b`)
	reRoutineTask      = regexp.MustCompile(`(?i)\b(morning[- ]?brief|edgar[- ]?close[- ]?watch|close\s+watch|filing\s+extract\w*|multi[- ]?source\s+summar\w*|brief)\b`)
	reQuickTask        = regexp.MustCompile(`(?i)\b(triage|status|summary|summarize|list|format|lint|typo|quick|hiive|poll\w*|lookup)\b`)
	reUUID             = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	rePromptModelHint  = regexp.MustCompile(`(?i)\b(?:use|with|on)\s+(opus|sonnet|carbon|argon|hydrogen|flash_lite|flash-lite|flash)\b`)
)

// ExtractUUID returns the first UUID v4 found in s (e.g., from agentapi stdout), or "".
func ExtractUUID(s string) string {
	return strings.ToLower(reUUID.FindString(s))
}

// ResolveModel selects the Antigravity model ID and thinking depth (low/medium/high/xhigh)
// based on explicit user overrides or automatic task-complexity classification.
//
// Supported routing profiles (canonical tier returned is quick|standard|deep):
//   - quick (gemini-4-hydrogen-low, flash_lite): high-frequency polling, simple lookups, status summaries
//   - routine / synthesis / medium (gemini-4-hydrogen-medium, flash): structured crons (morning-brief, edgar-close-watch), filing extraction
//   - standard (gemini-4-hydrogen-high, flash): feature coding, unit tests, bug fixes, doc searches
//   - heavy / sweep / xhigh (gemini-4-hydrogen-xhigh, flash): exhaustive multi-file refactors, hard E2E/test debugging
//   - deep-medium / consolidation (gemini-4-carbon-medium, pro): nightly sleep consolidation, deep research memos
//   - deep (gemini-4-carbon-high, pro): Phase 1 Architecture/RFC design, security/concurrency audits, thesis falsification
//
// Verified Antigravity model IDs (`agy models`):
//   - gemini-4-hydrogen-{low,medium,high,xhigh}
//   - gemini-3.8-flash-{low,medium,high}
//   - gemini-4-carbon-{low,medium,high}
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

	// 1. Infer profile/tier from prompt if not explicitly specified
	if tier == "" {
		switch {
		case reDeepMediumTask.MatchString(prompt) && !reDeepHighOnlyTask.MatchString(prompt):
			tier = "deep-medium"
		case reHeavySweepTask.MatchString(prompt) && !reDeepHighOnlyTask.MatchString(prompt):
			tier = "heavy"
		case reRoutineTask.MatchString(prompt) && !reDeepHighOnlyTask.MatchString(prompt):
			tier = "routine"
		case reDeepTask.MatchString(prompt):
			tier = "deep"
		case reQuickTask.MatchString(prompt):
			tier = "quick"
		default:
			tier = "standard"
		}
	}

	// 2. Normalize tier to canonical quick|standard|deep and assign default depth + model family
	inferredDepth := ""
	switch tier {
	case "quick":
		inferredDepth = "low"
		if model == "" {
			model = "gemini-4-hydrogen"
		}
		subagentModel = "flash_lite"
	case "routine", "synthesis", "medium", "standard-medium":
		tier = "standard"
		inferredDepth = "medium"
		if model == "" {
			model = "gemini-4-hydrogen"
		}
		subagentModel = "flash"
	case "heavy", "sweep", "xhigh", "standard-xhigh":
		tier = "standard"
		inferredDepth = "xhigh"
		if model == "" {
			model = "gemini-4-hydrogen"
		}
		subagentModel = "flash"
	case "deep-medium", "deep_medium", "deep-routine", "deep_routine", "consolidation", "research":
		tier = "deep"
		inferredDepth = "medium"
		if model == "" {
			model = "gemini-4-carbon"
		}
		subagentModel = "pro"
	case "deep":
		inferredDepth = "high"
		if model == "" {
			model = "gemini-4-carbon"
		}
		subagentModel = "pro"
	default: // "standard"
		tier = "standard"
		inferredDepth = "high"
		if model == "" {
			model = "gemini-4-hydrogen"
		}
		subagentModel = "flash"
	}
	if depth == "" {
		depth = inferredDepth
	}

	// Normalize shorthand model aliases
	switch model {
	case "hydrogen":
		model = "gemini-4-hydrogen"
		subagentModel = "flash"
	case "flash":
		model = "gemini-4-hydrogen"
		subagentModel = "flash"
	case "flash_lite", "flash-lite":
		model = "gemini-4-hydrogen"
		depth = "low"
		subagentModel = "flash_lite"
	case "carbon":
		model = "gemini-4-carbon"
		subagentModel = "pro"
	case "argon", "pro":
		model = "gemini-4-carbon"
		subagentModel = "pro"
	case "sonnet", "claude-sonnet":
		model = "claude-sonnet-5-5"
		subagentModel = "pro"
	case "opus", "claude-opus":
		model = "claude-opus-5-5"
		subagentModel = "pro"
	}

	// If model already ends with -low, -medium, -high, or -xhigh, keep it as-is
	if strings.HasSuffix(model, "-low") || strings.HasSuffix(model, "-medium") || strings.HasSuffix(model, "-high") || strings.HasSuffix(model, "-xhigh") {
		if strings.HasPrefix(model, "gemini-4-carbon-") || strings.HasPrefix(model, "gemini-4-argon-") ||
			strings.HasPrefix(model, "claude-opus-") || strings.HasPrefix(model, "claude-sonnet-") {
			subagentModel = "pro"
			if strings.TrimSpace(explicitTier) == "" && (strings.HasPrefix(model, "gemini-4-carbon-") || strings.HasPrefix(model, "gemini-4-argon-")) {
				tier = "deep"
			}
		} else if strings.HasPrefix(model, "gemini-4-hydrogen-low") || strings.HasPrefix(model, "gemini-3.8-flash-low") {
			subagentModel = "flash_lite"
			if strings.TrimSpace(explicitTier) == "" {
				tier = "quick"
			}
		} else if strings.HasPrefix(model, "gemini-4-hydrogen-") || strings.HasPrefix(model, "gemini-3.8-flash-") {
			subagentModel = "flash"
			if strings.TrimSpace(explicitTier) == "" {
				tier = "standard"
			}
		}
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
		state.GatekeeperSavesToday = 0
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
	summary := prompt
	if runes := []rune(summary); len(runes) > 200 {
		summary = string(runes[:200]) + "..."
	}
	now := time.Now()
	if n := len(state.Events); n > 0 {
		last := state.Events[n-1]
		if last.Trigger == "invoke_subagent" && last.Skill == role && last.Model == model && last.Outcome == summary {
			if ts, err := time.Parse(time.RFC3339, last.Timestamp); err == nil && now.Sub(ts) >= 0 && now.Sub(ts) < 2*time.Second {
				return nil
			}
		}
	}
	state.SpawnsToday++
	state.LastStatus = fmt.Sprintf("Subagent spawned: %s (%s)", role, now.Format("15:04:05"))
	state.Events = append(state.Events, SpawnEvent{
		Timestamp: now.Format(time.RFC3339),
		Trigger:   "invoke_subagent",
		Skill:     role,
		Model:     model,
		Outcome:   summary,
	})
	return SaveQuota(vaultDir, state)
}

func configureSpawnCommand(cmd *exec.Cmd, vaultDir, trigger string) {
	fd, hasFD := GetFrontDesk(vaultDir)
	isManual := strings.HasPrefix(trigger, "manual-spawn:")
	if !isManual {
		workDir := ""
		if hasFD && fd.WorkspaceDir != "" {
			if st, err := os.Stat(fd.WorkspaceDir); err == nil && st.IsDir() {
				workDir = fd.WorkspaceDir
			}
		}
		if workDir == "" && vaultDir != "" {
			cleanV := filepath.Clean(vaultDir)
			if filepath.Base(cleanV) == "local" {
				parent := filepath.Dir(cleanV)
				if st, err := os.Stat(parent); err == nil && st.IsDir() {
					workDir = parent
				}
			}
			if workDir == "" {
				if st, err := os.Stat(cleanV); err == nil && st.IsDir() {
					workDir = cleanV
				}
			}
		}
		if workDir != "" {
			cmd.Dir = workDir
		}
	}

	rawEnv := os.Environ()
	cleanEnv := make([]string, 0, len(rawEnv)+1)
	inSidecar := os.Getenv("ANTIGRAVITY_SIDECAR_WEB_PORT") != ""
	overrideProj := !isManual || inSidecar

	for _, kv := range rawEnv {
		if strings.HasPrefix(kv, "ANTIGRAVITY_CONVERSATION_ID=") ||
			strings.HasPrefix(kv, "ANTIGRAVITY_TRAJECTORY_ID=") ||
			strings.HasPrefix(kv, "ANTIGRAVITY_SOURCE_METADATA=") ||
			strings.HasPrefix(kv, "ANTIGRAVITY_SIDECAR_WEB_PORT=") ||
			strings.HasPrefix(kv, "ANTIGRAVITY_SIDECAR_UI_TOKEN=") {
			continue
		}
		if overrideProj && strings.HasPrefix(kv, "ANTIGRAVITY_PROJECT_ID=") {
			continue
		}
		cleanEnv = append(cleanEnv, kv)
	}
	if overrideProj && hasFD && strings.TrimSpace(fd.ProjectID) != "" {
		cleanEnv = append(cleanEnv, "ANTIGRAVITY_PROJECT_ID="+strings.TrimSpace(fd.ProjectID))
	}
	cmd.Env = cleanEnv
}

// SpawnTask invokes `agentapi new-conversation --model=<modelID>` (or simulates it if dryRun is true),
// enforcing the daily spawn budget, auto-downgrading to gemini-4-hydrogen-medium if a higher-tier model
// hits 429 RESOURCE_EXHAUSTED, and tripping the circuit breaker only if Hydrogen/Flash is also exhausted.
func SpawnTask(vaultDir, trigger, skill, title, prompt, modelID string, dryRun bool, force ...bool) (string, error) {
	if modelID == "" {
		modelID, _, _ = ResolveModel(prompt, "", "", "")
	}
	isForced := len(force) > 0 && force[0]
	state := LoadQuota(vaultDir)
	if state.CircuitOpen && !isForced {
		return "", fmt.Errorf("quota circuit breaker is OPEN: %s", state.CircuitReason)
	}
	if state.SpawnsToday >= state.MaxDailySpawns && !isForced {
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
		configureSpawnCommand(cmd, vaultDir, trigger)
		outBytes, err := cmd.CombinedOutput()
		output = strings.TrimSpace(string(outBytes))
		if err != nil {
			// Quota-aware fallback: if a non-Hydrogen/non-Flash model hit 429, retry once on gemini-4-hydrogen-medium
			if (strings.Contains(output, "RESOURCE_EXHAUSTED") || strings.Contains(output, "429")) && !strings.Contains(modelID, "flash") && !strings.Contains(modelID, "hydrogen") {
				fallbackModel := "gemini-4-hydrogen-medium"
				fbCmd := exec.Command("agentapi", "new-conversation", "--model="+fallbackModel, "--title="+title, "--", prompt)
				configureSpawnCommand(fbCmd, vaultDir, trigger)
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

// RunHeartbeat executes the zero-token Go sidecar pre-check loop (proposal TTL sweep, inbox drop router,
// Gmail/GChat batch coalescer, daemon gatekeepers, and Waiting-On obligation SLA sweeps).
// If no actionable deltas are detected, zero LLM tokens are used.
func RunHeartbeat(vaultDir string, dryRun bool) (string, error) {
	now := time.Now()
	_, _ = approvals.ExpireStaleProposals(vaultDir, now)

	inboxActions, err := ProcessInbox(vaultDir, dryRun)
	if err != nil {
		return "", err
	}

	attCfg := outbox.LoadAttentionConfig(vaultDir)

	var readyGmail []CoalescedTriageEntry
	if batches, _, gErr := gmail.RunPrecheck(vaultDir, ""); gErr == nil && len(batches) > 0 {
		for _, batch := range batches {
			readyGmail = append(readyGmail, EnqueueOrFlushCoalescedTriage(vaultDir, "gmail", batch.Account, batch.BatchID, batch.ThreadIDs, attCfg.CoalesceWindow, now)...)
		}
	} else if attCfg.CoalesceWindow > 0 {
		readyGmail = EnqueueOrFlushCoalescedTriage(vaultDir, "gmail", "", "", nil, attCfg.CoalesceWindow, now)
	}
	for _, entry := range readyGmail {
		batchLabel := strings.Join(entry.BatchIDs, "+")
		title := fmt.Sprintf("[Auto] Gmail Triage (%s · %s)", entry.Account, batchLabel)
		prompt := fmt.Sprintf(
			"Activate the inbox-triage skill to process Gmail triage batch %s (account=%s, threads=%s). "+
				"Read each thread via `hq gmail read-quarantined --account=%s --thread=<threadId>`, "+
				"create a plain-text draft reply via `hq gmail draft` if a reply is needed, "+
				"stage a send proposal via `hq propose --type=gmail_send --draft=<draftId>`, "+
				"and save your structured summary via `hq gmail triage-result --batch=%s`.",
			batchLabel, entry.Account, strings.Join(entry.Targets, ","), entry.Account, entry.BatchIDs[0],
		)
		out, spawnErr := SpawnFlashTask(vaultDir, "gmail-precheck:"+batchLabel, "inbox-triage", title, prompt, dryRun)
		if spawnErr != nil {
			inboxActions = append(inboxActions, fmt.Sprintf("gmail:%s -> /inbox-triage (deferred: %v)", batchLabel, spawnErr))
		} else {
			inboxActions = append(inboxActions, fmt.Sprintf("gmail:%s -> /inbox-triage (%s)", batchLabel, out))
		}
	}

	var readyChat []CoalescedTriageEntry
	if chatBatches, _, cErr := gchat.RunPrecheck(vaultDir, ""); cErr == nil && len(chatBatches) > 0 {
		for _, batch := range chatBatches {
			readyChat = append(readyChat, EnqueueOrFlushCoalescedTriage(vaultDir, "gchat", batch.Account, batch.BatchID, batch.SpaceIDs, attCfg.CoalesceWindow, now)...)
		}
	} else if attCfg.CoalesceWindow > 0 {
		readyChat = EnqueueOrFlushCoalescedTriage(vaultDir, "gchat", "", "", nil, attCfg.CoalesceWindow, now)
	}
	for _, entry := range readyChat {
		batchLabel := strings.Join(entry.BatchIDs, "+")
		title := fmt.Sprintf("[Auto] GChat Triage (%s · %s)", entry.Account, batchLabel)
		prompt := fmt.Sprintf(
			"Activate the inbox-triage skill to process Google Chat triage batch %s (account=%s, spaces=%s). "+
				"Read each space/thread via `hq gchat read-quarantined --account=%s --space=<spaceId>`, "+
				"create a plain-text draft reply via `hq gchat draft` if a reply is needed, "+
				"stage a send proposal via `hq propose --type=gchat_send --draft=<draftId>`, "+
				"and save your structured summary via `hq gchat triage-result --batch=%s`.",
			batchLabel, entry.Account, strings.Join(entry.Targets, ","), entry.Account, entry.BatchIDs[0],
		)
		out, spawnErr := SpawnFlashTask(vaultDir, "gchat-precheck:"+batchLabel, "inbox-triage", title, prompt, dryRun)
		if spawnErr != nil {
			inboxActions = append(inboxActions, fmt.Sprintf("gchat:%s -> /inbox-triage (deferred: %v)", batchLabel, spawnErr))
		} else {
			inboxActions = append(inboxActions, fmt.Sprintf("gchat:%s -> /inbox-triage (%s)", batchLabel, out))
		}
	}

	if gkActions, _, gkErr := EvaluateDaemonGatekeepers(vaultDir, now, false, dryRun); gkErr == nil && len(gkActions) > 0 {
		inboxActions = append(inboxActions, gkActions...)
	}

	if goalActions, gErr := SweepWaitingOnObligations(vaultDir, now); gErr == nil && len(goalActions) > 0 {
		inboxActions = append(inboxActions, goalActions...)
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
	if strings.Contains(m, "pro") || strings.Contains(m, "carbon") || strings.Contains(m, "argon") || strings.Contains(m, "opus") {
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
	Role                 string    `json:"role"`
	Model                string    `json:"model"`
	Tier                 string    `json:"tier,omitempty"`
	Scope                string    `json:"scope,omitempty"`
	ParentConversationID string    `json:"parentConversationId,omitempty"`
	TaskSlug             string    `json:"taskSlug"`
	Title                string    `json:"title"`
	PromptPrefix         string    `json:"promptPrefix,omitempty"`
	CreatedAt            time.Time `json:"createdAt"`
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
	return QueuePendingSubagentWithParent(vaultDir, "", role, model, prompt)
}

// QueuePendingSubagentWithParent enqueues metadata for an invoked subagent along with its parentConversationId.
func QueuePendingSubagentWithParent(vaultDir, parentConvID, role, model, prompt string) (PendingSubagent, error) {
	tier, scope, _, title, slug := ParseSubagentRole(role, model)
	promptPrefix := prompt
	if runes := []rune(promptPrefix); len(runes) > 200 {
		promptPrefix = string(runes[:200]) + "..."
	}
	item := PendingSubagent{
		Role:                 role,
		Model:                model,
		Tier:                 tier,
		Scope:                scope,
		ParentConversationID: strings.TrimSpace(parentConvID),
		TaskSlug:             slug,
		Title:                title,
		PromptPrefix:         promptPrefix,
		CreatedAt:            time.Now(),
	}

	queue, _ := loadPendingSubagents(vaultDir)
	cutoff := time.Now().Add(-60 * time.Minute)
	var active []PendingSubagent
	for _, q := range queue {
		if q.CreatedAt.After(cutoff) {
			active = append(active, q)
		}
	}
	if n := len(active); n > 0 {
		last := active[n-1]
		if last.ParentConversationID == item.ParentConversationID && last.Role == item.Role &&
			last.Model == item.Model && last.PromptPrefix == item.PromptPrefix &&
			item.CreatedAt.Sub(last.CreatedAt) >= 0 && item.CreatedAt.Sub(last.CreatedAt) < 2*time.Second {
			return last, nil
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

// IsFrontDeskConversation returns true if convID matches the currently leased Front Desk conversation ID.
func IsFrontDeskConversation(vaultDir, convID string) bool {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return false
	}
	fd, ok := GetFrontDesk(vaultDir)
	return ok && fd.ConversationID == convID
}

func pendingNestedSubagentsFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "pending_nested_subagents.json")
}

func nestedConversationsFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "nested_conversations.json")
}

func loadPendingNestedSubagents(vaultDir string) ([]PendingSubagent, error) {
	data, err := os.ReadFile(pendingNestedSubagentsFilePath(vaultDir))
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

func savePendingNestedSubagents(vaultDir string, items []PendingSubagent) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pendingNestedSubagentsFilePath(vaultDir), data, 0o644)
}

// QueuePendingNestedSubagent enqueues metadata for a nested child subagent invoked from a subagent or secondary task.
func QueuePendingNestedSubagent(vaultDir, parentConvID, role, model, prompt string) (PendingSubagent, error) {
	tier, scope, _, title, slug := ParseSubagentRole(role, model)
	promptPrefix := prompt
	if runes := []rune(promptPrefix); len(runes) > 200 {
		promptPrefix = string(runes[:200]) + "..."
	}
	item := PendingSubagent{
		Role:                 role,
		Model:                model,
		Tier:                 tier,
		Scope:                scope,
		ParentConversationID: strings.TrimSpace(parentConvID),
		TaskSlug:             slug,
		Title:                title,
		PromptPrefix:         promptPrefix,
		CreatedAt:            time.Now(),
	}

	queue, _ := loadPendingNestedSubagents(vaultDir)
	cutoff := time.Now().Add(-60 * time.Minute)
	var active []PendingSubagent
	for _, q := range queue {
		if q.CreatedAt.After(cutoff) {
			active = append(active, q)
		}
	}
	if n := len(active); n > 0 {
		last := active[n-1]
		if last.ParentConversationID == item.ParentConversationID && last.Role == item.Role &&
			last.Model == item.Model && last.PromptPrefix == item.PromptPrefix &&
			item.CreatedAt.Sub(last.CreatedAt) >= 0 && item.CreatedAt.Sub(last.CreatedAt) < 2*time.Second {
			return last, nil
		}
	}
	active = append(active, item)
	err := savePendingNestedSubagents(vaultDir, active)
	return item, err
}

// PopPendingNestedSubagent pops the oldest pending nested subagent created within maxAge.
func PopPendingNestedSubagent(vaultDir string, maxAge time.Duration) (PendingSubagent, bool) {
	queue, err := loadPendingNestedSubagents(vaultDir)
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
		_ = savePendingNestedSubagents(vaultDir, remaining)
		return matched, true
	}
	return PendingSubagent{}, false
}

// NestedConversation tracks known descendant subagent conversations spawned by non-Front Desk callers.
type NestedConversation struct {
	ConversationID       string `json:"conversationId"`
	ParentConversationID string `json:"parentConversationId,omitempty"`
	CreatedAt            string `json:"createdAt"`
}

func loadNestedConversations(vaultDir string) (map[string]NestedConversation, error) {
	data, err := os.ReadFile(nestedConversationsFilePath(vaultDir))
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]NestedConversation), nil
		}
		return nil, err
	}
	var list []NestedConversation
	if err := json.Unmarshal(data, &list); err == nil {
		m := make(map[string]NestedConversation)
		for _, item := range list {
			if item.ConversationID != "" {
				m[item.ConversationID] = item
			}
		}
		return m, nil
	}
	var m map[string]NestedConversation
	if err := json.Unmarshal(data, &m); err == nil {
		return m, nil
	}
	return make(map[string]NestedConversation), nil
}

func saveNestedConversations(vaultDir string, m map[string]NestedConversation) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	var list []NestedConversation
	for _, v := range m {
		list = append(list, v)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt < list[j].CreatedAt
	})
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(nestedConversationsFilePath(vaultDir), data, 0o644)
}

// RecordNestedConversation records a conversation ID as a nested descendant subagent.
func RecordNestedConversation(vaultDir, convID, parentConvID string) error {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return nil
	}
	m, _ := loadNestedConversations(vaultDir)
	if m == nil {
		m = make(map[string]NestedConversation)
	}
	m[convID] = NestedConversation{
		ConversationID:       convID,
		ParentConversationID: strings.TrimSpace(parentConvID),
		CreatedAt:            time.Now().Format(time.RFC3339),
	}
	return saveNestedConversations(vaultDir, m)
}

// IsNestedSubagentConversation returns true if convID is a known nested descendant subagent.
func IsNestedSubagentConversation(vaultDir, convID string) bool {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return false
	}
	m, err := loadNestedConversations(vaultDir)
	if err == nil && m != nil {
		if _, ok := m[convID]; ok {
			return true
		}
	}
	return false
}

// IsSubagentConversation returns true if convID belongs to an ephemeral child subagent
// (Layer-1 subagent or nested descendant subagent), and should never be treated as a Front Desk conversation.
func IsSubagentConversation(vaultDir, convID string) bool {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return false
	}
	if IsNestedSubagentConversation(vaultDir, convID) {
		return true
	}
	if task, _, found := FindTaskAnywhere(vaultDir, convID); found {
		if strings.EqualFold(task.Kind, "subagent") || task.EffectiveKind() == "subagent" ||
			task.ParentConversationID != "" ||
			strings.HasPrefix(task.TaskSlug, "subagent-") ||
			strings.HasPrefix(task.Title, "[Subagent:") {
			return true
		}
	}
	return false
}

// IsLayer1Task returns true if task is a Layer-1 task orchestrated by the Front Desk:
// 1. Top-level task conversation (EffectiveKind == "task").
// 2. Automated daemon trigger (EffectiveKind == "auto").
// 3. Direct subagent of the Front Desk (EffectiveKind == "subagent"), where:
//   - ParentConversationID matches frontDeskConvID, OR
//   - ParentConversationID is empty (legacy subagents created from Front Desk).
//
// Returns false if Kind is "subagent" and ParentConversationID points to another task/subagent (not Front Desk).
func IsLayer1Task(task ActiveTask, frontDeskConvID string) bool {
	kind := task.EffectiveKind()
	if kind == "task" || kind == "auto" {
		return true
	}
	if kind == "subagent" {
		frontDeskConvID = strings.TrimSpace(frontDeskConvID)
		parentID := strings.TrimSpace(task.ParentConversationID)
		if parentID != "" && frontDeskConvID != "" && parentID != frontDeskConvID {
			return false
		}
		return true
	}
	return true
}

// FilterLayer1Tasks filters tasks to strictly include Layer-1 tasks.
func FilterLayer1Tasks(vaultDir string, tasks []ActiveTask) []ActiveTask {
	fd, _ := GetFrontDesk(vaultDir)
	var out []ActiveTask
	for _, t := range tasks {
		if t.ConversationID != "" && IsNestedSubagentConversation(vaultDir, t.ConversationID) {
			continue
		}
		if IsLayer1Task(t, fd.ConversationID) {
			out = append(out, t)
		}
	}
	return out
}

// CleanNestedTasks archives or removes nested child subagents from local/memory/tasks.json,
// moving them to tasks_archive.jsonl and adjusting quota so that only Layer-1 tasks are tracked.
func CleanNestedTasks(vaultDir string) ([]string, error) {
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return nil, err
	}
	fd, _ := GetFrontDesk(vaultDir)

	// Pre-seed known nested conversations from quota events or journal if any
	quota := LoadQuota(vaultDir)
	re := regexp.MustCompile(`Spawned subagent ([0-9a-fA-F-]+) \(parent=([0-9a-fA-F-]+)`)
	for _, ev := range quota.Events {
		if strings.Contains(ev.Outcome, "parent=") {
			if m := re.FindStringSubmatch(ev.Outcome); len(m) == 3 {
				cid := m[1]
				pid := m[2]
				if fd.ConversationID != "" && pid != fd.ConversationID {
					_ = RecordNestedConversation(vaultDir, cid, pid)
				}
			}
		}
	}

	var layer1Tasks []ActiveTask
	var removedTasks []ActiveTask
	var removedSlugs []string
	nowStr := time.Now().Format(time.RFC3339)

	for _, t := range tasks {
		isNested := false
		if t.ConversationID != "" && IsNestedSubagentConversation(vaultDir, t.ConversationID) {
			isNested = true
		} else if t.EffectiveKind() == "subagent" {
			parentID := strings.TrimSpace(t.ParentConversationID)
			if parentID != "" && fd.ConversationID != "" && parentID != fd.ConversationID {
				isNested = true
			}
		}

		if isNested {
			t.Status = "archived"
			t.ArchivedAt = nowStr
			removedTasks = append(removedTasks, t)
			if t.ConversationID != "" {
				_ = RecordNestedConversation(vaultDir, t.ConversationID, t.ParentConversationID)
			}
			removedSlugs = append(removedSlugs, t.TaskSlug)
		} else {
			layer1Tasks = append(layer1Tasks, t)
		}
	}

	if len(removedSlugs) > 0 {
		if err := AppendTasksToColdArchive(vaultDir, removedTasks); err != nil {
			return nil, err
		}
		data, err := json.MarshalIndent(layer1Tasks, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(tasksFilePath(vaultDir), data, 0o644); err != nil {
			return nil, err
		}

		// Reconcile quota SpawnsToday if nested spawns were previously counted
		if quota.SpawnsToday >= len(removedSlugs) {
			quota.SpawnsToday -= len(removedSlugs)
			_ = SaveQuota(vaultDir, quota)
		}
	}

	return removedSlugs, nil
}

var staticModelEnumMap = map[string]string{
	"gemini-4-hydrogen-xhigh":  "MODEL_PLACEHOLDER_M528",
	"gemini-4-hydrogen-high":   "MODEL_PLACEHOLDER_M525",
	"gemini-4-hydrogen-medium": "MODEL_PLACEHOLDER_M526",
	"gemini-4-hydrogen-low":    "MODEL_PLACEHOLDER_M527",
	"gemini-4-carbon-high":     "MODEL_PLACEHOLDER_M420",
	"gemini-4-carbon-medium":   "MODEL_PLACEHOLDER_M421",
	"gemini-4-carbon-low":      "MODEL_PLACEHOLDER_M422",
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
func SpawnSubagent(vaultDir, parentConvID, role, prompt, modelID, workspaceDir string, dryRun bool, force ...bool) (string, error) {
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

	isForced := len(force) > 0 && force[0]
	state := LoadQuota(vaultDir)
	if state.CircuitOpen && !isForced {
		return "", fmt.Errorf("quota circuit breaker is OPEN: %s", state.CircuitReason)
	}

	isFrontDesk := IsFrontDeskConversation(vaultDir, parentConvID)
	if isFrontDesk && state.SpawnsToday >= state.MaxDailySpawns && !isForced {
		state.CircuitOpen = true
		state.CircuitReason = fmt.Sprintf("daily spawn budget reached (%d/%d)", state.SpawnsToday, state.MaxDailySpawns)
		_ = SaveQuota(vaultDir, state)
		return "", fmt.Errorf("daily spawn budget exhausted (%d/%d)", state.SpawnsToday, state.MaxDailySpawns)
	}

	tier, scope, _, title, slug := ParseSubagentRole(role, modelID)

	if dryRun {
		output := fmt.Sprintf("[dry-run] hq subagent --parent=%s --model=%s --role=%q", parentConvID, modelID, role)
		if isFrontDesk {
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
		}
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

	if isFrontDesk {
		// Layer-1 subagent: Register in local/memory/tasks.json before sending the initial message so SessionStart sees it immediately.
		_ = RegisterTask(vaultDir, ActiveTask{
			TaskSlug:             slug,
			Title:                title,
			Kind:                 "subagent",
			Scope:                scope,
			Tier:                 tier,
			Model:                modelID,
			ParentConversationID: parentConvID,
			WorktreeDir:          workspaceDir,
			ConversationID:       cascadeID,
			CreatedAt:            time.Now().Format(time.RFC3339),
			LastUpdatedAt:        time.Now().Format(time.RFC3339),
			LastSummary:          prompt,
			Status:               "active",
		})
	} else {
		// Nested descendant subagent: record in nested conversations so hooks ignore it for task tracking
		_ = RecordNestedConversation(vaultDir, cascadeID, parentConvID)
	}

	// Deliver initial prompt via SendUserCascadeMessage to kick off Turn 1 immediately
	// (Note: agentapi send-message only queues an inbox message and will not wake a 0-step StartCascade trajectory).
	sent := false
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
			if sendResp.StatusCode >= 200 && sendResp.StatusCode < 300 {
				sent = true
			}
		}
	}
	if !sent && parentConvID != "" {
		msgCmd := exec.Command("agentapi", "send-message", "--title=Message from "+parentConvID, cascadeID, fullPrompt)
		_ = msgCmd.Run()
	}

	output := fmt.Sprintf("Spawned subagent %s (parent=%s, model=%s, role=%q)", cascadeID, parentConvID, modelID, role)
	if isFrontDesk {
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
	}
	return output, nil
}
