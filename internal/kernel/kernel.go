package kernel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/routing"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/internal/timers"
)

// QuotaState tracks daily background spawns and circuit-breaker status in local/memory/quota-YYYY-MM-DD.json.
// SpawnsToday records total Layer-1 spawns (AutoSpawnsToday + InteractiveSpawnsToday), while MaxDailySpawns
// governs automated/daemon background spawns (`AutoSpawnsToday`) so interactive user delegations never starve
// or get blocked by background watcher caps.
type QuotaState struct {
	Date                   string       `json:"date"`
	SpawnsToday            int          `json:"spawnsToday"`
	AutoSpawnsToday        int          `json:"autoSpawnsToday,omitempty"`
	InteractiveSpawnsToday int          `json:"interactiveSpawnsToday,omitempty"`
	GatekeeperSavesToday   int          `json:"gatekeeperSavesToday,omitempty"`
	MaxDailySpawns         int          `json:"maxDailySpawns"`
	CircuitOpen            bool         `json:"circuitOpen"`
	CircuitReason          string       `json:"circuitReason,omitempty"`
	LastHeartbeatAt        string       `json:"lastHeartbeatAt,omitempty"`
	LastStatus             string       `json:"lastStatus,omitempty"`
	Events                 []SpawnEvent `json:"events,omitempty"`
}

// EffectiveAutoSpawns returns today's automated background spawn count used for MaxDailySpawns enforcement,
// preserving backward compatibility for legacy persisted quota files without split counters.
func (q QuotaState) EffectiveAutoSpawns() int {
	if q.AutoSpawnsToday > 0 || q.InteractiveSpawnsToday > 0 {
		return q.AutoSpawnsToday
	}
	if len(q.Events) > 0 {
		auto := 0
		for _, ev := range q.Events {
			if strings.HasPrefix(strings.TrimSpace(ev.Outcome), "error:") {
				continue
			}
			if !isInteractiveSpawnTrigger(ev.Trigger) {
				auto++
			}
		}
		return auto
	}
	return q.SpawnsToday
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
	TaskID  string `json:"taskId,omitempty"`
	Cron    string `json:"cron"`
	Skill   string `json:"skill,omitempty"`
	Reason  string `json:"reason,omitempty"`
	ArmedAt string `json:"armedAt"`
}

// AdHocCron tracks a user-requested recurring conversation cron in the active Front Desk
// conversation that is NOT managed by local/triggers.yaml.
type AdHocCron struct {
	TaskID  string `json:"taskId,omitempty"`
	Cron    string `json:"cron"`
	Summary string `json:"summary"`
	ArmedAt string `json:"armedAt,omitempty"`
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
	AdHocCrons     []AdHocCron             `json:"adHocCrons,omitempty"`
	KilledTaskIDs  []string                `json:"killedTaskIds,omitempty"`
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
	TaskSlug             string   `json:"taskSlug"`
	Title                string   `json:"title"`
	Kind                 string   `json:"kind,omitempty"`  // "task" | "subagent" | "auto"
	Scope                string   `json:"scope,omitempty"` // e.g. "agy-hq", "brand", "NVDA", "personal"
	Tier                 string   `json:"tier"`            // "quick" | "standard" | "deep"
	Model                string   `json:"model"`
	ParentConversationID string   `json:"parentConversationId,omitempty"` // Links nested subagent to parent Task or Front Desk
	WorktreeDir          string   `json:"worktreeDir,omitempty"`
	WorktreeDirs         []string `json:"worktreeDirs,omitempty"`
	RepoRoot             string   `json:"repoRoot,omitempty"`
	ConversationID       string   `json:"conversationId,omitempty"`
	TurnCount            int      `json:"turnCount,omitempty"`
	Pinned               bool     `json:"pinned,omitempty"`
	CreatedAt            string   `json:"createdAt"`
	LastUpdatedAt        string   `json:"lastUpdatedAt"`
	CompletedAt          string   `json:"completedAt,omitempty"`
	ArchivedAt           string   `json:"archivedAt,omitempty"`
	SidebarArchived      bool     `json:"sidebarArchived,omitempty"`
	SidebarSyncedAt      string   `json:"sidebarSyncedAt,omitempty"`
	EventIDs             []string `json:"eventIds,omitempty"`
	LastSummary          string   `json:"lastSummary,omitempty"`
	Status               string   `json:"status"` // active, idle, completed, archived, cancelled
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
			// Never allow an interactive session to bind to an event-leased batch;
			// background batches bind solely via [hq-spawn:<token>] or SpawnTask UUID return.
			if len(tasks[i].EventIDs) > 0 {
				continue
			}
			tasks[i].ConversationID = convID
			if model != "" && tasks[i].Model == "" {
				tasks[i].Model = model
			}
			tasks[i].LastUpdatedAt = time.Now().Format(time.RFC3339)
			if data, mErr := json.MarshalIndent(tasks, "", "  "); mErr == nil {
				_ = os.WriteFile(tasksFilePath(vaultDir), data, 0o644)
			}
			if isExplicitTopLevelTask(tasks[i]) {
				_ = RemoveNestedConversation(vaultDir, convID)
			}
			return tasks[i], true
		}
	}
	return ActiveTask{}, false
}

func isExplicitTopLevelTask(t ActiveTask) bool {
	if strings.HasPrefix(t.TaskSlug, "subagent-") || strings.HasPrefix(t.Title, "[Subagent:") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(t.Kind), "task") || t.EffectiveKind() == "task"
}

// RegisterTask records or updates a dedicated Task Conversation in local/memory/tasks.json.
func RegisterTask(vaultDir string, task ActiveTask) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	explicitTaskPromotion := strings.EqualFold(strings.TrimSpace(task.Kind), "task") &&
		!strings.HasPrefix(task.TaskSlug, "subagent-") &&
		!strings.HasPrefix(task.Title, "[Subagent:")
	tasks, _ := ListTasks(vaultDir)
	updated := false
	for i := range tasks {
		if (task.TaskSlug != "" && tasks[i].TaskSlug == task.TaskSlug) || (task.ConversationID != "" && tasks[i].ConversationID == task.ConversationID) {
			if task.TaskSlug == "" {
				task.TaskSlug = tasks[i].TaskSlug
			}
			if task.ConversationID == "" {
				task.ConversationID = tasks[i].ConversationID
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
			if task.ParentConversationID == "" && !explicitTaskPromotion {
				task.ParentConversationID = tasks[i].ParentConversationID
			}
			task.WorktreeDir, task.WorktreeDirs = mergeTaskWorktrees(tasks[i], task)
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
				task.SidebarArchived = false
			} else {
				if task.CompletedAt == "" {
					task.CompletedAt = tasks[i].CompletedAt
				}
				if task.ArchivedAt == "" {
					task.ArchivedAt = tasks[i].ArchivedAt
				}
				if !task.SidebarArchived && tasks[i].SidebarArchived {
					task.SidebarArchived = tasks[i].SidebarArchived
				}
				if task.SidebarSyncedAt == "" {
					task.SidebarSyncedAt = tasks[i].SidebarSyncedAt
				}
			}
			if task.Status == "" {
				task.Status = tasks[i].Status
			}
			if len(task.EventIDs) == 0 {
				task.EventIDs = tasks[i].EventIDs
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
				if len(tasks[i].EventIDs) > 0 {
					_ = events.BindBatchConversation(vaultDir, tasks[i].EventIDs, task.ConversationID, time.Now(), events.DefaultLeaseTTL)
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
	if err := os.WriteFile(tasksFilePath(vaultDir), data, 0o644); err != nil {
		return err
	}
	for _, t := range tasks {
		if t.ConversationID != "" && (t.ConversationID == task.ConversationID || (task.TaskSlug != "" && t.TaskSlug == task.TaskSlug)) && isExplicitTopLevelTask(t) {
			_ = RemoveNestedConversation(vaultDir, t.ConversationID)
		}
	}
	return nil
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
	if err := os.WriteFile(tasksFilePath(vaultDir), data, 0o644); err != nil {
		return err
	}
	return nil
}

// SetTaskPinned pins or unpins a task in local/memory/tasks.json so it stays anchored at the top of the Active board.
func SetTaskPinned(vaultDir, slugOrConvID string, pinned bool) error {
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return err
	}
	found := false
	convID := ""
	for i := range tasks {
		if tasks[i].TaskSlug == slugOrConvID || tasks[i].ConversationID == slugOrConvID {
			tasks[i].Pinned = pinned
			tasks[i].LastUpdatedAt = time.Now().Format(time.RFC3339)
			convID = tasks[i].ConversationID
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
	if err := os.WriteFile(tasksFilePath(vaultDir), data, 0o644); err != nil {
		return err
	}
	if convID != "" {
		SyncTaskPinnedBestEffort(vaultDir, convID, pinned, SidebarSyncConfig{})
	}
	return nil
}

// ArchiveTaskOptions configures task archival and Antigravity sidebar synchronization.
type ArchiveTaskOptions struct {
	Force         bool
	NoSidebar     bool
	SidebarConfig SidebarSyncConfig
}

// ArchiveTask archives a task in local/memory/tasks.json (matched by TaskSlug or ConversationID)
// and synchronizes archival state to Antigravity's sidebar (falling back to the retry queue if offline).
func ArchiveTask(vaultDir, target string, force bool) error {
	_, err := ArchiveTaskWithOptions(vaultDir, target, ArchiveTaskOptions{Force: force})
	return err
}

// ArchiveTaskWithOptions archives a task in local/memory/tasks.json (matched by TaskSlug or ConversationID).
// If WorktreeDir exists on disk:
//   - runs `git -C <worktreeDir> status --porcelain`. If there are uncommitted changes and !opts.Force, returns:
//     refusing to archive %q: uncommitted changes in worktree (pass --force to override)
//   - safely removes the worktree using `git -C <repoRoot> worktree remove` (adding --force if opts.Force is true).
//
// Sets task status to "archived", saves tasks.json locally first, and then syncs with Antigravity's sidebar unless NoSidebar is true.
func ArchiveTaskWithOptions(vaultDir, target string, opts ArchiveTaskOptions) (SidebarSingleSyncResult, error) {
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return SidebarSingleSyncResult{}, err
	}
	nowStr := time.Now().Format(time.RFC3339)
	archivedIdx := -1
	for i := range tasks {
		if tasks[i].TaskSlug == target || tasks[i].ConversationID == target {
			task := &tasks[i]
			if task.Pinned && !opts.Force {
				return SidebarSingleSyncResult{}, fmt.Errorf("refusing to archive %q: task is pinned (unpin first or pass --force)", target)
			}
			if err := validateTaskWorktreesForArchive(vaultDir, target, *task, opts.Force); err != nil {
				return SidebarSingleSyncResult{}, err
			}
			if err := removeTaskWorktrees(vaultDir, task, opts.Force, true); err != nil {
				return SidebarSingleSyncResult{}, err
			}

			task.Status = "archived"
			task.LastUpdatedAt = nowStr
			task.ArchivedAt = nowStr
			archivedIdx = i
			break
		}
	}
	if archivedIdx < 0 {
		return SidebarSingleSyncResult{}, fmt.Errorf("task %q not found in local/memory/tasks.json", target)
	}

	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return SidebarSingleSyncResult{}, err
	}
	if err := os.WriteFile(tasksFilePath(vaultDir), data, 0o644); err != nil {
		return SidebarSingleSyncResult{}, err
	}

	if opts.NoSidebar {
		return SidebarSingleSyncResult{SkippedReason: "no_sidebar"}, nil
	}
	syncRes := syncSingleTaskArchiveState(context.Background(), vaultDir, &tasks[archivedIdx], true, true, opts.Force, opts.SidebarConfig)
	if syncRes.Synced || syncRes.AlreadySynced {
		if updatedBytes, mErr := json.MarshalIndent(tasks, "", "  "); mErr == nil {
			_ = os.WriteFile(tasksFilePath(vaultDir), updatedBytes, 0o644)
		}
	}
	return syncRes, nil
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

// ReopenTaskOptions configures cold/hot task recall and Antigravity sidebar unarchiving.
type ReopenTaskOptions struct {
	NoWorktree    bool
	NoSidebar     bool
	SidebarConfig SidebarSyncConfig
}

// ReopenTask restores a task in local/memory/tasks.json (or recalls it from cold storage
// local/memory/tasks_archive.jsonl) back to active status, recreating its git worktree if needed
// and unarchiving the conversation in Antigravity's sidebar.
func ReopenTask(vaultDir, target string) error {
	_, err := ReopenTaskWithOptions(vaultDir, target, ReopenTaskOptions{})
	return err
}

// ReopenTaskWithOptions restores a task back to active status and optionally restores its git worktree and sidebar state.
func ReopenTaskWithOptions(vaultDir, target string, opts ReopenTaskOptions) (SidebarSingleSyncResult, error) {
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return SidebarSingleSyncResult{}, err
	}
	nowStr := time.Now().Format(time.RFC3339)
	reopenedIdx := -1
	for i := range tasks {
		if tasks[i].TaskSlug == target || tasks[i].ConversationID == target {
			if !opts.NoWorktree {
				if err := restoreWorktreeForReopen(vaultDir, &tasks[i]); err != nil {
					return SidebarSingleSyncResult{}, err
				}
			}
			tasks[i].Status = "active"
			tasks[i].ArchivedAt = ""
			tasks[i].SidebarArchived = false
			tasks[i].LastUpdatedAt = nowStr
			reopenedIdx = i
			break
		}
	}
	if reopenedIdx < 0 {
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
				if !opts.NoWorktree {
					if err := restoreWorktreeForReopen(vaultDir, recalled); err != nil {
						return SidebarSingleSyncResult{}, err
					}
				}
				recalled.Status = "active"
				recalled.ArchivedAt = ""
				recalled.SidebarArchived = false
				recalled.LastUpdatedAt = nowStr
				tasks = append(tasks, *recalled)
				reopenedIdx = len(tasks) - 1
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
	if reopenedIdx < 0 {
		return SidebarSingleSyncResult{}, fmt.Errorf("task %q not found in local/memory/tasks.json", target)
	}

	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return SidebarSingleSyncResult{}, err
	}
	if err := os.WriteFile(tasksFilePath(vaultDir), data, 0o644); err != nil {
		return SidebarSingleSyncResult{}, err
	}
	if tasks[reopenedIdx].ConversationID != "" && isExplicitTopLevelTask(tasks[reopenedIdx]) {
		_ = RemoveNestedConversation(vaultDir, tasks[reopenedIdx].ConversationID)
	}

	if opts.NoSidebar {
		RemoveSidebarSyncQueueItem(vaultDir, tasks[reopenedIdx].ConversationID, time.Now())
		return SidebarSingleSyncResult{SkippedReason: "no_sidebar"}, nil
	}
	syncRes := syncSingleTaskArchiveState(context.Background(), vaultDir, &tasks[reopenedIdx], false, false, false, opts.SidebarConfig)
	if syncRes.Synced || syncRes.AlreadySynced {
		if updatedBytes, mErr := json.MarshalIndent(tasks, "", "  "); mErr == nil {
			_ = os.WriteFile(tasksFilePath(vaultDir), updatedBytes, 0o644)
		}
	}
	return syncRes, nil
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
	NoSidebar                bool
	NoWorktreePrune          bool
	SidebarConfig            SidebarSyncConfig
}

// CompactTasksReport summarizes what CompactTasks transitioned and rotated.
type CompactTasksReport struct {
	HotBefore           int      `json:"hotBefore"`
	HotAfter            int      `json:"hotAfter"`
	AutoArchivedSlugs   []string `json:"autoArchivedSlugs,omitempty"`
	RotatedColdSlugs    []string `json:"rotatedColdSlugs,omitempty"`
	PrunedWorktrees     []string `json:"prunedWorktrees,omitempty"`
	TotalColdAfter      int      `json:"totalColdAfter"`
	SidebarSyncedCount  int      `json:"sidebarSyncedCount,omitempty"`
	SidebarQueuedCount  int      `json:"sidebarQueuedCount,omitempty"`
	SidebarSkippedCount int      `json:"sidebarSkippedCount,omitempty"`
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

// CompactTasks executes the RFC-0009 & RFC-0016 rolling lifecycle compaction:
//  1. Auto-archives unpinned completed/cancelled top-level tasks older than TaskAutoArchiveAfter (24h) if their worktree is clean.
//  2. Auto-archives unpinned idle top-level tasks untouched for > IdleDormantArchiveAfter (48h) if their worktree is clean.
//  3. Flushes completed/cancelled subagents (>24h or beyond MaxHotCompletedSubagents) and archived tasks (>48h)
//     from local/memory/tasks.json into append-only local/memory/tasks_archive.jsonl.
//  4. Synchronizes archived/rotated conversations and drains the retry queue with Antigravity's sidebar.
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
		if !opts.NoWorktreePrune {
			if pRep, pErr := PruneOrphanWorktrees(vaultDir, PruneWorktreesOptions{
				Now:    opts.Now,
				MinAge: DefaultOrphanWorktreeMinAge,
				DryRun: opts.DryRun,
			}); pErr == nil && pRep != nil {
				if opts.DryRun {
					rep.PrunedWorktrees = pRep.WouldPrune
				} else {
					rep.PrunedWorktrees = pRep.Pruned
				}
			}
		}
		if !opts.NoSidebar && HasPendingSidebarSyncQueue(vaultDir) {
			if sRep, _ := SyncSidebar(vaultDir, SyncSidebarOptions{
				Now:          opts.Now,
				DryRun:       opts.DryRun,
				QueueOnly:    true,
				ClientConfig: opts.SidebarConfig,
			}); sRep != nil {
				if opts.DryRun {
					rep.SidebarSyncedCount = len(sRep.WouldArchive) + len(sRep.WouldUnarchive)
				} else {
					rep.SidebarSyncedCount = sRep.ArchivedCount + sRep.UnarchivedCount
				}
				rep.SidebarQueuedCount = sRep.QueuedCount
				rep.SidebarSkippedCount = len(sRep.ProtectedSkipped)
			}
		}
		return rep, nil
	}

	nowStr := opts.Now.Format(time.RFC3339)
	var explicitSidebarIDs []string
	seenSidebarID := make(map[string]bool)
	addSidebarCandidate := func(convID string) {
		cid := normalizeUUID(convID)
		if IsCanonicalUUID(cid) && !seenSidebarID[cid] {
			seenSidebarID[cid] = true
			explicitSidebarIDs = append(explicitSidebarIDs, cid)
		}
	}

	// Step 1: Auto-archive eligible completed (>24h) or dormant (>48h) top-level tasks with clean worktrees,
	// and reconcile stuck active `[Auto]` workers whose bound events have already finished or been acked.
	autoPinnedCompleted := 0
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
		if kind == "task" && (st == "completed" || st == "cancelled") && !t.Pinned && age >= opts.TaskAutoArchiveAfter && areAllTaskWorktreesCleanOrAbsent(vaultDir, *t) {
			shouldArchive = true
		} else if kind == "task" && (st == "active" || st == "idle") && !t.Pinned && age >= opts.IdleDormantArchiveAfter && areAllTaskWorktreesMergedAndClean(vaultDir, *t) {
			shouldArchive = true
		} else if kind == "auto" && st == "active" {
			if ready, ackedIDs := isAutoTaskReadyToArchive(vaultDir, *t, DefaultAutoAckGracePeriod, opts.Now); ready {
				if !opts.DryRun && len(ackedIDs) > 0 {
					_, _ = events.CompleteIDs(vaultDir, ackedIDs, opts.Now)
				}
				if t.Pinned {
					t.Status = "completed"
					if t.CompletedAt == "" {
						t.CompletedAt = nowStr
					}
					t.LastUpdatedAt = nowStr
					autoPinnedCompleted++
				} else {
					shouldArchive = true
				}
			}
		}
		if shouldArchive {
			if !opts.DryRun {
				_ = removeTaskWorktrees(vaultDir, t, false, false)
			}
			t.Status = "archived"
			if t.CompletedAt == "" {
				t.CompletedAt = nowStr
			}
			if t.ArchivedAt == "" {
				t.ArchivedAt = nowStr
			}
			rep.AutoArchivedSlugs = append(rep.AutoArchivedSlugs, t.TaskSlug)
			addSidebarCandidate(t.ConversationID)
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
		if !t.Pinned && (t.EffectiveKind() == "subagent" || t.EffectiveKind() == "auto") && (st == "completed" || st == "cancelled") {
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
		if !t.Pinned && (kind == "subagent" || kind == "auto") && (st == "completed" || st == "cancelled") {
			if (age >= opts.SubagentHotMaxAge || subOverCap[i]) && areAllTaskWorktreesCleanOrAbsent(vaultDir, t) {
				rotate = true
			}
		} else if !t.Pinned && st == "archived" && age >= opts.ArchivedColdMaxAge {
			rotate = true
		}

		if rotate {
			if !opts.DryRun && areAllTaskWorktreesCleanOrAbsent(vaultDir, t) {
				_ = removeTaskWorktrees(vaultDir, &t, false, false)
			}
			rotatedCold = append(rotatedCold, t)
			rep.RotatedColdSlugs = append(rep.RotatedColdSlugs, t.TaskSlug)
			if !t.SidebarArchived {
				addSidebarCandidate(t.ConversationID)
			}
		} else {
			keptHot = append(keptHot, t)
			if st == "archived" && !t.SidebarArchived {
				addSidebarCandidate(t.ConversationID)
			}
		}
	}

	rep.HotAfter = len(keptHot)
	if !opts.DryRun {
		if len(rotatedCold) > 0 {
			if err := AppendTasksToColdArchive(vaultDir, rotatedCold); err != nil {
				return rep, err
			}
		}
		if len(rotatedCold) > 0 || len(rep.AutoArchivedSlugs) > 0 || autoPinnedCompleted > 0 {
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

	if !opts.NoWorktreePrune {
		if pRep, pErr := PruneOrphanWorktrees(vaultDir, PruneWorktreesOptions{
			Now:    opts.Now,
			MinAge: DefaultOrphanWorktreeMinAge,
			DryRun: opts.DryRun,
		}); pErr == nil && pRep != nil {
			if opts.DryRun {
				rep.PrunedWorktrees = pRep.WouldPrune
			} else {
				rep.PrunedWorktrees = pRep.Pruned
			}
		}
	}

	if !opts.NoSidebar && (len(explicitSidebarIDs) > 0 || HasPendingSidebarSyncQueue(vaultDir)) {
		if sRep, _ := SyncSidebar(vaultDir, SyncSidebarOptions{
			Now:                opts.Now,
			DryRun:             opts.DryRun,
			QueueOnly:          true,
			ExplicitArchiveIDs: explicitSidebarIDs,
			ClientConfig:       opts.SidebarConfig,
		}); sRep != nil {
			if opts.DryRun {
				rep.SidebarSyncedCount = len(sRep.WouldArchive) + len(sRep.WouldUnarchive)
			} else {
				rep.SidebarSyncedCount = sRep.ArchivedCount + sRep.UnarchivedCount
			}
			rep.SidebarQueuedCount = sRep.QueuedCount
			rep.SidebarSkippedCount = len(sRep.ProtectedSkipped)
		}
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
	if err := os.WriteFile(tasksFilePath(vaultDir), data, 0o644); err != nil {
		return err
	}
	return nil
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
	if err := os.WriteFile(tasksFilePath(vaultDir), data, 0o644); err != nil {
		return err
	}
	return nil
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
		if roots := hostpaths.Resolve(vaultDir); roots.BrainDir != "" {
			artifactDir = filepath.Join(roots.BrainDir, convID)
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
		lease.AdHocCrons = prev.AdHocCrons
		lease.KilledTaskIDs = prev.KilledTaskIDs
		lease.ProjectID = prev.ProjectID
		lease.WorkspaceDir = prev.WorkspaceDir
	}
	if lease.WorkspaceDir == "" && vaultDir != "" {
		cleanV := filepath.Clean(vaultDir)
		if filepath.Base(cleanV) == "local" {
			lease.WorkspaceDir = filepath.Dir(cleanV)
		}
	}
	candProjID := lease.ProjectID
	if os.Getenv("ANTIGRAVITY_SIDECAR_WEB_PORT") == "" {
		envConv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
		if projID := strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID")); projID != "" && (envConv == "" || envConv == convID) {
			if valid, regAvail := ValidateProjectMatchesWorkspace(vaultDir, projID, lease.WorkspaceDir); !regAvail || valid {
				candProjID = projID
			}
		}
	}
	if lease.WorkspaceDir != "" {
		verifiedProjID, _, _ := ResolveVerifiedWorkspaceProjectID(vaultDir, lease.WorkspaceDir, candProjID)
		lease.ProjectID = verifiedProjID
	} else {
		lease.ProjectID = candProjID
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
	verifiedProjID, _, repaired := ResolveVerifiedWorkspaceProjectID(vaultDir, workspaceDir, fd.ProjectID)
	if fd.WorkspaceDir == workspaceDir && (!repaired || fd.ProjectID == verifiedProjID) {
		return nil
	}
	fd.WorkspaceDir = workspaceDir
	fd.ProjectID = verifiedProjID
	fd.UpdatedAt = time.Now().Format(time.RFC3339)
	return saveFrontDeskLease(vaultDir, fd)
}

func primaryVaultWorkspaceDir(vaultDir string) string {
	if strings.TrimSpace(vaultDir) == "" {
		return ""
	}
	if fd, ok := GetFrontDesk(vaultDir); ok && strings.TrimSpace(fd.WorkspaceDir) != "" {
		w := strings.TrimSpace(fd.WorkspaceDir)
		if abs, err := filepath.Abs(w); err == nil {
			w = abs
		}
		return filepath.Clean(w)
	}
	cleanV := filepath.Clean(vaultDir)
	if abs, err := filepath.Abs(cleanV); err == nil {
		cleanV = abs
	}
	if filepath.Base(cleanV) == "local" {
		return filepath.Dir(cleanV)
	}
	return ""
}

func isCallerInPrimaryVaultWorkspace(vaultDir string) bool {
	if strings.TrimSpace(vaultDir) == "" || os.Getenv("ANTIGRAVITY_SIDECAR_WEB_PORT") != "" {
		return false
	}
	primary := primaryVaultWorkspaceDir(vaultDir)
	envProj := strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
	if envProj != "" && primary != "" {
		if valid, regAvail := ValidateProjectMatchesWorkspace(vaultDir, envProj, primary); regAvail && !valid {
			return false
		}
	}
	convID := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	fd, hasFD := GetFrontDesk(vaultDir)
	if convID != "" {
		if hasFD && fd.ConversationID == convID {
			return true
		}
		if envProj != "" && primary != "" {
			if valid, regAvail := ValidateProjectMatchesWorkspace(vaultDir, envProj, primary); regAvail && valid {
				return true
			}
		}
		if hasFD && fd.ConversationID != "" {
			// A different conversation without registry proof must never impersonate the primary Vault project via cwd alone.
			return false
		}
	}
	if primary == "" {
		return false
	}
	if envProj != "" {
		if valid, regAvail := ValidateProjectMatchesWorkspace(vaultDir, envProj, primary); regAvail {
			return valid
		}
		if hasFD && strings.TrimSpace(fd.ProjectID) != "" && envProj != strings.TrimSpace(fd.ProjectID) {
			return false
		}
	}
	wd, err := os.Getwd()
	if err != nil || strings.TrimSpace(wd) == "" {
		return false
	}
	if abs, aErr := filepath.Abs(wd); aErr == nil {
		wd = abs
	}
	return filepath.Clean(wd) == filepath.Clean(primary)
}

// BackfillFrontDeskProjectID persists or repairs ANTIGRAVITY_PROJECT_ID in front_desk.json,
// validating existing and candidate projectIds against Antigravity's `<ConfigDir>/projects/*.json` registry.
func BackfillFrontDeskProjectID(vaultDir string) {
	fd, ok := GetFrontDesk(vaultDir)
	if !ok {
		return
	}
	wsDir := primaryVaultWorkspaceDir(vaultDir)
	curProj := strings.TrimSpace(fd.ProjectID)
	if curProj != "" {
		valid, regAvail := ValidateProjectMatchesWorkspace(vaultDir, curProj, wsDir)
		if !regAvail || valid {
			return
		}
		// Existing ProjectID is poisoned (points to an unrelated workspace); repair or clear it.
		cand := ""
		if isCallerInPrimaryVaultWorkspace(vaultDir) {
			cand = strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
		}
		repairedID, _, _ := ResolveVerifiedWorkspaceProjectID(vaultDir, wsDir, cand)
		if repairedID != curProj {
			fd.ProjectID = repairedID
			fd.UpdatedAt = time.Now().Format(time.RFC3339)
			_ = saveFrontDeskLease(vaultDir, fd)
		}
		return
	}

	cand := ""
	if isCallerInPrimaryVaultWorkspace(vaultDir) {
		cand = strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
	}
	verifiedID, _, _ := ResolveVerifiedWorkspaceProjectID(vaultDir, wsDir, cand)
	if verifiedID == "" {
		return
	}
	fd.ProjectID = verifiedID
	fd.UpdatedAt = time.Now().Format(time.RFC3339)
	_ = saveFrontDeskLease(vaultDir, fd)
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

func normalizeFiveFieldCronExpr(cronExpr string) string {
	normCron := strings.Join(strings.Fields(strings.Trim(strings.TrimSpace(cronExpr), "\"")), " ")
	if strings.HasPrefix(strings.ToUpper(normCron), "CRON_TZ=") {
		parts := strings.Fields(normCron)
		if len(parts) > 1 {
			normCron = strings.Join(parts[1:], " ")
		}
	}
	return normCron
}

func matchScheduleTrigger(schedules []ScheduleTrigger, cronExpr, prompt string) (ScheduleTrigger, bool) {
	normCron := normalizeFiveFieldCronExpr(cronExpr)
	lowerPrompt := strings.ToLower(strings.Trim(strings.TrimSpace(prompt), "\""))
	for _, s := range schedules {
		if s.ID == "" {
			continue
		}
		lowerID := strings.ToLower(s.ID)
		if strings.Contains(lowerPrompt, "[hq-trigger:"+lowerID+"]") || strings.Contains(lowerPrompt, lowerID) {
			return s, true
		}
	}
	if normCron != "" {
		for _, s := range schedules {
			if s.FiveFieldCron() == normCron {
				return s, true
			}
		}
		return ScheduleTrigger{}, false
	}
	for _, s := range schedules {
		if s.Skill != "" && strings.Contains(lowerPrompt, strings.ToLower(s.Skill)) {
			return s, true
		}
	}
	return ScheduleTrigger{}, false
}

func loadAllKnownSchedules(vaultDir string) []ScheduleTrigger {
	if cfg, err := LoadEffectiveTriggers(vaultDir); err == nil && cfg != nil && len(cfg.Schedules) > 0 {
		return cfg.Schedules
	}
	if cfg, err := LoadTriggers(vaultDir); err == nil && cfg != nil {
		return cfg.Schedules
	}
	return nil
}

func upsertAdHocCron(list []AdHocCron, item AdHocCron) ([]AdHocCron, bool) {
	if item.TaskID == "" && item.Cron == "" {
		return list, false
	}
	for i := range list {
		cur := &list[i]
		if item.TaskID != "" && cur.TaskID == item.TaskID {
			if cur.Cron == item.Cron && cur.Summary == item.Summary {
				return list, false
			}
			cur.Cron = item.Cron
			cur.Summary = item.Summary
			return list, true
		}
		if cur.Cron == item.Cron && cur.Summary == item.Summary {
			if item.TaskID != "" && cur.TaskID == "" {
				cur.TaskID = item.TaskID
				return list, true
			}
			return list, false
		}
	}
	return append(list, item), true
}

// RecordArmedTrigger records a scheduled trigger or ad-hoc user cron armed in the active Front Desk via `schedule`.
func RecordArmedTrigger(vaultDir, convID, cronExpr, prompt string) (string, error) {
	fd, ok := GetFrontDesk(vaultDir)
	if !ok {
		return "", nil
	}
	if convID != "" && fd.ConversationID != convID {
		return "", nil
	}
	cleanCron := strings.Trim(strings.TrimSpace(cronExpr), "\"")
	cleanPrompt := strings.Trim(strings.TrimSpace(prompt), "\"")
	if fd.ArmedTriggers == nil {
		fd.ArmedTriggers = make(map[string]ArmedTrigger)
	}
	schedules := loadAllKnownSchedules(vaultDir)
	if matched, found := matchScheduleTrigger(schedules, cleanCron, cleanPrompt); found {
		prev := fd.ArmedTriggers[matched.ID]
		fd.ArmedTriggers[matched.ID] = ArmedTrigger{
			ID:      matched.ID,
			TaskID:  prev.TaskID,
			Cron:    matched.FiveFieldCron(),
			Skill:   matched.Skill,
			ArmedAt: time.Now().Format(time.RFC3339),
		}
		fd.UpdatedAt = time.Now().Format(time.RFC3339)
		return matched.ID, saveFrontDeskLease(vaultDir, fd)
	}
	if cleanCron != "" {
		summary := SanitizeTaskTitle(cleanPrompt, cleanCron)
		updated, changed := upsertAdHocCron(fd.AdHocCrons, AdHocCron{
			Cron:    cleanCron,
			Summary: summary,
			ArmedAt: time.Now().Format(time.RFC3339),
		})
		if changed {
			fd.AdHocCrons = updated
			fd.UpdatedAt = time.Now().Format(time.RFC3339)
			return "adhoc-" + slugify(cleanCron), saveFrontDeskLease(vaultDir, fd)
		}
		return "adhoc-" + slugify(cleanCron), nil
	}
	return "", nil
}

// RecordKilledFrontDeskTask marks a background timer/task in the active Front Desk as killed so
// transcript reconciliation never resurrects it, and removes matching armed/ad-hoc entries.
func RecordKilledFrontDeskTask(vaultDir, convID, taskID string) error {
	taskID = strings.Trim(strings.TrimSpace(taskID), "\"")
	if taskID == "" {
		return nil
	}
	fd, ok := GetFrontDesk(vaultDir)
	if !ok {
		return nil
	}
	if convID != "" && fd.ConversationID != convID {
		return nil
	}
	changed := false
	seenKilled := false
	for _, k := range fd.KilledTaskIDs {
		if k == taskID {
			seenKilled = true
			break
		}
	}
	if !seenKilled {
		fd.KilledTaskIDs = append(fd.KilledTaskIDs, taskID)
		changed = true
	}
	for id, at := range fd.ArmedTriggers {
		if at.TaskID == taskID {
			delete(fd.ArmedTriggers, id)
			changed = true
		}
	}
	if len(fd.AdHocCrons) > 0 {
		kept := fd.AdHocCrons[:0]
		for _, ac := range fd.AdHocCrons {
			if ac.TaskID == taskID {
				changed = true
				continue
			}
			kept = append(kept, ac)
		}
		fd.AdHocCrons = kept
	}
	if !changed {
		return nil
	}
	fd.UpdatedAt = time.Now().Format(time.RFC3339)
	return saveFrontDeskLease(vaultDir, fd)
}

func parseGenericCronLaunchContent(content string) (taskID, cronExpr, prompt string, ok bool) {
	if !strings.Contains(content, "Tool is running as a background task with task id:") ||
		!strings.Contains(content, "Task Description: Cron:") {
		return "", "", "", false
	}
	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if strings.HasPrefix(line, "Tool is running as a background task with task id:") {
			taskID = strings.TrimSpace(strings.TrimPrefix(line, "Tool is running as a background task with task id:"))
		} else if strings.HasPrefix(line, "Task Description:") {
			desc := strings.TrimSpace(strings.TrimPrefix(line, "Task Description:"))
			if strings.HasPrefix(desc, "Cron:") {
				rest := strings.TrimSpace(strings.TrimPrefix(desc, "Cron:"))
				c, p, hasPrompt := strings.Cut(rest, ", Prompt:")
				cronExpr = strings.Trim(strings.TrimSpace(c), "\"")
				if hasPrompt {
					prompt = strings.Trim(strings.TrimSpace(p), "\"")
				}
			}
		}
	}
	if taskID == "" || cronExpr == "" {
		return "", "", "", false
	}
	return taskID, cronExpr, prompt, true
}

// ReconcileFrontDeskFromTranscript inspects the active Front Desk conversation's transcript.jsonl
// (if present on disk) to backfill ConsolePinned, harness ArmedTriggers (with TaskIDs), AdHocCrons,
// and KilledTaskIDs.
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

	schedules := loadAllKnownSchedules(vaultDir)
	knownSched := make(map[string]bool, len(schedules)*2)
	for _, s := range schedules {
		if s.ID != "" {
			knownSched[s.ID] = true
			knownSched[s.FQID()] = true
		}
	}

	killedSet := make(map[string]bool, len(fd.KilledTaskIDs))
	for _, kid := range fd.KilledTaskIDs {
		if k := strings.TrimSpace(kid); k != "" {
			killedSet[k] = true
		}
	}

	type transcriptCronItem struct {
		taskID   string
		cronExpr string
		prompt   string
		armedAt  string
	}
	var launchedCrons []transcriptCronItem
	launchedKeys := make(map[string]bool)
	var fallbackCalls []transcriptCronItem

	changed := false
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)

	for scanner.Scan() {
		var step struct {
			Type      string `json:"type"`
			CreatedAt string `json:"created_at"`
			Content   string `json:"content"`
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
			case "manage_task":
				action, _ := tc.Args["Action"].(string)
				if strings.EqualFold(strings.Trim(strings.TrimSpace(action), "\""), "kill") {
					rawTID, _ := tc.Args["TaskId"].(string)
					if tid := strings.Trim(strings.TrimSpace(rawTID), "\""); tid != "" {
						if !killedSet[tid] {
							killedSet[tid] = true
							fd.KilledTaskIDs = append(fd.KilledTaskIDs, tid)
							changed = true
						}
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
				fallbackCalls = append(fallbackCalls, transcriptCronItem{
					cronExpr: cronExpr,
					prompt:   prompt,
					armedAt:  step.CreatedAt,
				})
			}
		}
		if tid, cExpr, pText, isCronLaunch := parseGenericCronLaunchContent(step.Content); isCronLaunch {
			launchedCrons = append(launchedCrons, transcriptCronItem{
				taskID:   tid,
				cronExpr: cExpr,
				prompt:   pText,
				armedAt:  step.CreatedAt,
			})
			launchedKeys[normalizeFiveFieldCronExpr(cExpr)+"\x00"+pText] = true
		}
	}

	if fd.ArmedTriggers == nil {
		fd.ArmedTriggers = make(map[string]ArmedTrigger)
	}
	for id, at := range fd.ArmedTriggers {
		if (at.TaskID != "" && killedSet[at.TaskID]) || !knownSched[id] {
			delete(fd.ArmedTriggers, id)
			changed = true
		}
	}
	chronological := make([]transcriptCronItem, 0, len(launchedCrons)+len(fallbackCalls))
	chronological = append(chronological, launchedCrons...)
	for _, fc := range fallbackCalls {
		key := normalizeFiveFieldCronExpr(fc.cronExpr) + "\x00" + fc.prompt
		if launchedKeys[key] {
			continue
		}
		chronological = append(chronological, fc)
	}

	// Keep only the latest launch per (normalized cron, job title prefix) signature so pre-restart
	// timers wiped by an Antigravity server restart never resurrect after their post-restart replacement is killed.
	cronSig := func(cronExpr, prompt string) string {
		head := SanitizeTaskTitle(prompt, cronExpr)
		if before, _, hasColon := strings.Cut(head, ":"); hasColon && strings.TrimSpace(before) != "" {
			head = strings.TrimSpace(before)
		}
		return normalizeFiveFieldCronExpr(cronExpr) + "\x00" + strings.ToLower(head)
	}
	latestIdxBySig := make(map[string]int, len(chronological))
	for i, item := range chronological {
		latestIdxBySig[cronSig(item.cronExpr, item.prompt)] = i
	}
	activeItems := make([]transcriptCronItem, 0, len(latestIdxBySig))
	for i, item := range chronological {
		if latestIdxBySig[cronSig(item.cronExpr, item.prompt)] != i {
			continue
		}
		if item.taskID != "" && killedSet[item.taskID] {
			continue
		}
		activeItems = append(activeItems, item)
	}

	var rebuiltAdHoc []AdHocCron
	rebuiltAdHocSigs := make(map[string]bool)
	for _, item := range activeItems {
		armedAt := strings.TrimSpace(item.armedAt)
		if armedAt == "" {
			armedAt = time.Now().Format(time.RFC3339)
		}
		if matched, found := matchScheduleTrigger(schedules, item.cronExpr, item.prompt); found {
			prev, exists := fd.ArmedTriggers[matched.ID]
			nextTaskID := item.taskID
			if nextTaskID == "" {
				nextTaskID = prev.TaskID
			}
			if !exists || prev.TaskID != nextTaskID || prev.Cron != matched.FiveFieldCron() {
				fd.ArmedTriggers[matched.ID] = ArmedTrigger{
					ID:      matched.ID,
					TaskID:  nextTaskID,
					Cron:    matched.FiveFieldCron(),
					Skill:   matched.Skill,
					ArmedAt: armedAt,
				}
				changed = true
			}
		} else if item.cronExpr != "" {
			summary := SanitizeTaskTitle(item.prompt, item.cronExpr)
			rebuiltAdHoc, _ = upsertAdHocCron(rebuiltAdHoc, AdHocCron{
				TaskID:  item.taskID,
				Cron:    item.cronExpr,
				Summary: summary,
				ArmedAt: armedAt,
			})
			rebuiltAdHocSigs[cronSig(item.cronExpr, summary)] = true
		}
	}
	// Preserve any just-armed PreToolUse ad-hoc cron (TaskID == "") not yet written to transcript.jsonl.
	for _, ac := range fd.AdHocCrons {
		if ac.TaskID == "" {
			if !rebuiltAdHocSigs[cronSig(ac.Cron, ac.Summary)] {
				rebuiltAdHoc, _ = upsertAdHocCron(rebuiltAdHoc, ac)
			}
		}
	}
	if len(rebuiltAdHoc) != len(fd.AdHocCrons) {
		fd.AdHocCrons = rebuiltAdHoc
		changed = true
	} else {
		for i := range rebuiltAdHoc {
			if rebuiltAdHoc[i] != fd.AdHocCrons[i] {
				fd.AdHocCrons = rebuiltAdHoc
				changed = true
				break
			}
		}
	}

	if changed {
		fd.UpdatedAt = time.Now().Format(time.RFC3339)
		_ = saveFrontDeskLease(vaultDir, fd)
	}
	return changed
}

// GetUnarmedTriggers compares effective schedules (core + packs + extensions.json overrides) against the active Front Desk's ArmedTriggers
// (or active background daemon ownership when `hq daemon` is running)
// and returns the armed and unarmed ScheduleTrigger slices for enabled schedules.
func GetUnarmedTriggers(vaultDir string) (armed []ScheduleTrigger, unarmed []ScheduleTrigger) {
	cfg, err := LoadEffectiveTriggers(vaultDir)
	if err != nil && (cfg == nil || len(cfg.Schedules) == 0) {
		return nil, nil
	}
	if cfg == nil {
		return nil, nil
	}
	daemonActive := IsDaemonActiveFast(vaultDir)
	fd, ok := GetFrontDesk(vaultDir)
	for _, s := range cfg.Schedules {
		if !s.IsEnabled() {
			continue
		}
		if daemonActive {
			armed = append(armed, s)
			continue
		}
		if ok && fd.ArmedTriggers != nil {
			if _, isArmed := fd.ArmedTriggers[s.ID]; isArmed {
				armed = append(armed, s)
				continue
			}
			if _, isArmedFQ := fd.ArmedTriggers[s.FQID()]; isArmedFQ {
				armed = append(armed, s)
				continue
			}
		}
		unarmed = append(unarmed, s)
	}
	return armed, unarmed
}

// GetDisarmTriggers returns armed Front Desk harness schedules that are either paused via
// <vault>/extensions.json (`Reason: "paused"`) OR superseded because `hq daemon` is active
// (`Reason: "migrated_to_daemon"`), so SessionStart can instruct the Front Desk coordinator to
// cancel the duplicate timer via manage_task(Action="kill"). Ad-hoc user crons are never included.
func GetDisarmTriggers(vaultDir string) []ArmedTrigger {
	fd, ok := GetFrontDesk(vaultDir)
	if !ok || len(fd.ArmedTriggers) == 0 {
		return nil
	}
	cfg, _ := LoadEffectiveTriggers(vaultDir)
	if cfg == nil {
		return nil
	}
	daemonActive := IsDaemonActiveFast(vaultDir)
	disabled := make(map[string]bool)
	for _, s := range cfg.Schedules {
		if !s.IsEnabled() {
			disabled[s.ID] = true
			disabled[s.FQID()] = true
		}
	}
	var out []ArmedTrigger
	for k, at := range fd.ArmedTriggers {
		if disabled[k] || disabled[at.ID] {
			at.Reason = "paused"
			out = append(out, at)
		} else if daemonActive {
			at.Reason = "migrated_to_daemon"
			out = append(out, at)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ClearArmedTrigger removes a paused/disarmed trigger record from front_desk.json if present.
func ClearArmedTrigger(vaultDir, scheduleID string) error {
	fd, ok := GetFrontDesk(vaultDir)
	if !ok || len(fd.ArmedTriggers) == 0 {
		return nil
	}
	fqid := ScheduleTrigger{ID: scheduleID}.FQID()
	bare := strings.TrimPrefix(fqid, "core/")
	changed := false
	if _, exists := fd.ArmedTriggers[scheduleID]; exists {
		delete(fd.ArmedTriggers, scheduleID)
		changed = true
	}
	if _, exists := fd.ArmedTriggers[fqid]; exists {
		delete(fd.ArmedTriggers, fqid)
		changed = true
	}
	if _, exists := fd.ArmedTriggers[bare]; exists {
		delete(fd.ArmedTriggers, bare)
		changed = true
	}
	if !changed {
		return nil
	}
	fd.UpdatedAt = time.Now().Format(time.RFC3339)
	return saveFrontDeskLease(vaultDir, fd)
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
	return routing.ResolveBuiltin(prompt, explicitModel, explicitDepth, explicitTier)
}

// ResolveVaultModel resolves the model and tier honoring <vault>/config.yaml routing.tiers and active pack defaults.
func ResolveVaultModel(vaultDir, prompt, explicitModel, explicitDepth, explicitTier string) (modelID, tier, subagentModel string) {
	res := routing.Resolve(vaultDir, routing.Request{
		Prompt:        prompt,
		ExplicitModel: explicitModel,
		ExplicitDepth: explicitDepth,
		ExplicitTier:  explicitTier,
	})
	return res.ModelID, res.CanonicalTier, res.SubagentModel
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

// ClassifyInboxFileForVault routes a filename in local/inbox/ to a domain skill only when
// that skill is installed in <vault>/skills/<skill>/SKILL.md or overridden by a trusted companion pack;
// otherwise it falls back to Core OS "inbox-triage".
func ClassifyInboxFileForVault(vaultDir, filename string) string {
	skill := ClassifyInboxFile(filename)
	if skill == "inbox-triage" || strings.TrimSpace(vaultDir) == "" {
		return skill
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "skills", skill, "SKILL.md")); err == nil {
		return skill
	}
	if res := extension.ResolveSkillDispatch(vaultDir, skill); res.Overridden && res.Trust == "ok" {
		return skill
	}
	return "inbox-triage"
}

func quotaFilePath(vaultDir string, now time.Time) string {
	return filepath.Join(vaultDir, "memory", fmt.Sprintf("quota-%s.json", now.Format("2006-01-02")))
}

// VaultQuotaConfig holds parsed settings from the top-level `quota:` section of <vault>/config.yaml.
type VaultQuotaConfig struct {
	MaxParallelRuns int
	MaxDailySpawns  int
	BackgroundModel string
}

func stripInlineYAMLComment(val string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(val); i++ {
		switch val[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble && (i == 0 || val[i-1] == ' ' || val[i-1] == '\t') {
				return strings.TrimSpace(val[:i])
			}
		}
	}
	return strings.TrimSpace(val)
}

// LoadVaultQuotaConfig reads top-level `quota:` settings (`max_daily_spawns`, `max_parallel_runs`, `background_model`)
// from <vault>/config.yaml if present.
func LoadVaultQuotaConfig(vaultDir string) VaultQuotaConfig {
	var cfg VaultQuotaConfig
	if strings.TrimSpace(vaultDir) == "" {
		return cfg
	}
	raw, err := os.ReadFile(filepath.Join(vaultDir, "config.yaml"))
	if err != nil {
		return cfg
	}
	inQuota := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent == 0 {
			inQuota = trimmed == "quota:" || strings.HasPrefix(trimmed, "quota:")
			continue
		}
		if !inQuota {
			continue
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		val := strings.Trim(stripInlineYAMLComment(v), `"'`)
		switch key {
		case "max_daily_spawns":
			if n, pErr := strconv.Atoi(val); pErr == nil && n > 0 {
				cfg.MaxDailySpawns = n
			}
		case "max_parallel_runs":
			if n, pErr := strconv.Atoi(val); pErr == nil && n > 0 {
				cfg.MaxParallelRuns = n
			}
		case "background_model":
			cfg.BackgroundModel = val
		}
	}
	return cfg
}

func isInteractiveSpawnTrigger(trigger string) bool {
	t := strings.TrimSpace(trigger)
	return t == "invoke_subagent" ||
		strings.HasPrefix(t, "hq-subagent:") ||
		strings.HasPrefix(t, "manual-spawn:") ||
		strings.HasPrefix(t, "idea-promote:")
}

func isDailySpawnCapReason(reason string) bool {
	r := strings.ToLower(strings.TrimSpace(reason))
	return strings.HasPrefix(r, "daily spawn budget reached") || strings.HasPrefix(r, "daily spawn budget exhausted")
}

// SetVaultMaxDailySpawns updates `quota.max_daily_spawns` in <vault>/config.yaml (when present) and persists
// the new cap in QuotaState, automatically closing any spawn-count circuit breaker when below the new cap.
func SetVaultMaxDailySpawns(vaultDir string, maxDailySpawns int) (QuotaState, error) {
	if maxDailySpawns <= 0 {
		return QuotaState{}, fmt.Errorf("maxDailySpawns must be > 0 (got %d)", maxDailySpawns)
	}
	cfgPath := filepath.Join(vaultDir, "config.yaml")
	if raw, err := os.ReadFile(cfgPath); err == nil {
		lines := strings.Split(string(raw), "\n")
		inQuota := false
		quotaHeaderIdx := -1
		replaced := false
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			if indent == 0 {
				if trimmed == "quota:" || strings.HasPrefix(trimmed, "quota:") {
					inQuota = true
					quotaHeaderIdx = i
				} else {
					inQuota = false
				}
				continue
			}
			if inQuota {
				if k, _, ok := strings.Cut(trimmed, ":"); ok && strings.TrimSpace(k) == "max_daily_spawns" {
					prefix := line[:indent]
					lines[i] = fmt.Sprintf("%smax_daily_spawns: %d", prefix, maxDailySpawns)
					replaced = true
					break
				}
			}
		}
		var updatedYAML string
		if replaced {
			updatedYAML = strings.Join(lines, "\n")
		} else if quotaHeaderIdx >= 0 {
			out := make([]string, 0, len(lines)+1)
			out = append(out, lines[:quotaHeaderIdx+1]...)
			out = append(out, fmt.Sprintf("  max_daily_spawns: %d", maxDailySpawns))
			out = append(out, lines[quotaHeaderIdx+1:]...)
			updatedYAML = strings.Join(out, "\n")
		} else {
			base := strings.TrimRight(string(raw), "\n")
			updatedYAML = fmt.Sprintf("%s\n\nquota:\n  max_daily_spawns: %d\n", base, maxDailySpawns)
		}
		mode := os.FileMode(0o600)
		if st, sErr := os.Stat(cfgPath); sErr == nil {
			mode = st.Mode().Perm()
		}
		if wErr := store.WriteAtomic(cfgPath, []byte(updatedYAML), mode); wErr != nil {
			return QuotaState{}, wErr
		}
	}

	state := LoadQuota(vaultDir)
	state.MaxDailySpawns = maxDailySpawns
	if state.CircuitOpen && isDailySpawnCapReason(state.CircuitReason) && state.EffectiveAutoSpawns() < state.MaxDailySpawns {
		state.CircuitOpen = false
		state.CircuitReason = ""
	}
	if err := SaveQuota(vaultDir, state); err != nil {
		return state, err
	}
	return state, nil
}

// LoadQuota reads today's QuotaState from local/memory/quota-YYYY-MM-DD.json or quota_state.json,
// honoring `quota.max_daily_spawns` from <vault>/config.yaml when configured.
func LoadQuota(vaultDir string) QuotaState {
	now := time.Now()
	today := now.Format("2006-01-02")
	cfgQuota := LoadVaultQuotaConfig(vaultDir)
	defaultCap := 15
	if cfgQuota.MaxDailySpawns > 0 {
		defaultCap = cfgQuota.MaxDailySpawns
	}
	p := quotaFilePath(vaultDir, now)
	state := QuotaState{
		Date:           today,
		MaxDailySpawns: defaultCap,
	}
	data, err := os.ReadFile(p)
	if err != nil {
		fallbackPath := filepath.Join(vaultDir, "memory", "quota_state.json")
		if fbData, fbErr := os.ReadFile(fallbackPath); fbErr == nil {
			var fbState QuotaState
			if unErr := json.Unmarshal(fbData, &fbState); unErr == nil {
				if fbState.Date == today {
					state = fbState
				} else if fbState.MaxDailySpawns > 0 {
					state.MaxDailySpawns = fbState.MaxDailySpawns
				}
			}
		}
	} else {
		_ = json.Unmarshal(data, &state)
	}
	if cfgQuota.MaxDailySpawns > 0 {
		state.MaxDailySpawns = cfgQuota.MaxDailySpawns
	} else if state.MaxDailySpawns <= 0 {
		state.MaxDailySpawns = 15
	}
	if state.Date != today {
		state.Date = today
		state.SpawnsToday = 0
		state.AutoSpawnsToday = 0
		state.InteractiveSpawnsToday = 0
		state.GatekeeperSavesToday = 0
		state.CircuitOpen = false
		state.CircuitReason = ""
		state.Events = nil
	} else if state.SpawnsToday > 0 && state.AutoSpawnsToday == 0 && state.InteractiveSpawnsToday == 0 && len(state.Events) > 0 {
		auto, inter := 0, 0
		for _, ev := range state.Events {
			if strings.HasPrefix(strings.TrimSpace(ev.Outcome), "error:") {
				continue
			}
			if isInteractiveSpawnTrigger(ev.Trigger) {
				inter++
			} else {
				auto++
			}
		}
		if auto+inter > 0 {
			state.AutoSpawnsToday = auto
			state.InteractiveSpawnsToday = inter
		}
	}
	if state.CircuitOpen && isDailySpawnCapReason(state.CircuitReason) && state.EffectiveAutoSpawns() < state.MaxDailySpawns {
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

// RecordSubagentSpawn records an interactive Layer-1 subagent invocation (`InteractiveSpawnsToday` + `SpawnsToday`)
// without consuming the daemon's automated background spawn budget (`AutoSpawnsToday`).
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
	if state.SpawnsToday > 0 && state.AutoSpawnsToday == 0 && state.InteractiveSpawnsToday == 0 {
		state.AutoSpawnsToday = state.EffectiveAutoSpawns()
	}
	state.InteractiveSpawnsToday++
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

func resolveSpawnTarget(vaultDir, trigger string) (workDir, projectID string) {
	fd, hasFD := GetFrontDesk(vaultDir)
	isManual := strings.HasPrefix(trigger, "manual-spawn:")
	if !isManual {
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
	} else if wd, err := os.Getwd(); err == nil && wd != "" {
		workDir = wd
	}

	inSidecar := os.Getenv("ANTIGRAVITY_SIDECAR_WEB_PORT") != ""
	overrideProj := !isManual || inSidecar
	if overrideProj {
		if hasFD {
			projectID = strings.TrimSpace(fd.ProjectID)
		}
		if projectID == "" {
			if sess, ok := loadReachableLSSession(vaultDir); ok && strings.TrimSpace(sess.ProjectID) != "" {
				projectID = strings.TrimSpace(sess.ProjectID)
			}
		}
		if projectID == "" && isCallerInPrimaryVaultWorkspace(vaultDir) {
			projectID = strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
		}
	} else {
		projectID = strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
		if projectID == "" && hasFD {
			projectID = strings.TrimSpace(fd.ProjectID)
		}
		if projectID == "" {
			if sess, ok := loadReachableLSSession(vaultDir); ok && strings.TrimSpace(sess.ProjectID) != "" {
				projectID = strings.TrimSpace(sess.ProjectID)
			}
		}
	}
	if workDir != "" {
		verifiedProjID, _, repaired := ResolveVerifiedWorkspaceProjectID(vaultDir, workDir, projectID)
		projectID = verifiedProjID
		if !isManual && hasFD && repaired && strings.TrimSpace(fd.ProjectID) != projectID && filepath.Clean(workDir) == filepath.Clean(primaryVaultWorkspaceDir(vaultDir)) {
			fd.ProjectID = projectID
			fd.UpdatedAt = time.Now().Format(time.RFC3339)
			_ = saveFrontDeskLease(vaultDir, fd)
		}
	}
	return workDir, projectID
}

func configureSpawnCommand(cmd *exec.Cmd, vaultDir, trigger string) {
	workDir, projectID := resolveSpawnTarget(vaultDir, trigger)
	isManual := strings.HasPrefix(trigger, "manual-spawn:")
	if !isManual && workDir != "" {
		cmd.Dir = workDir
	}

	rawEnv := os.Environ()
	cleanEnv := make([]string, 0, len(rawEnv)+1)
	inSidecar := os.Getenv("ANTIGRAVITY_SIDECAR_WEB_PORT") != ""
	envProj := strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
	overrideProj := !isManual || inSidecar || (envProj != projectID)

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
	if overrideProj && projectID != "" {
		cleanEnv = append(cleanEnv, "ANTIGRAVITY_PROJECT_ID="+projectID)
	}
	hasEnvNonEmpty := func(env []string, key string) bool {
		prefix := key + "="
		for _, kv := range env {
			if strings.HasPrefix(kv, prefix) && strings.TrimSpace(strings.TrimPrefix(kv, prefix)) != "" {
				return true
			}
		}
		return false
	}
	if !hasEnvNonEmpty(cleanEnv, "ANTIGRAVITY_LS_ADDRESS") || !hasEnvNonEmpty(cleanEnv, "ANTIGRAVITY_CSRF_TOKEN") || !hasEnvNonEmpty(cleanEnv, "ANTIGRAVITY_AGENTAPI_EXE") {
		if creds, ok := discoverLSCredentials(vaultDir); ok {
			rawAddr := strings.TrimSpace(creds.RawAddress)
			if rawAddr == "" {
				rawAddr, _ = normalizeLSAddress(creds.BaseURL)
			}
			setIfMissing := func(key, val string) {
				if val == "" || hasEnvNonEmpty(cleanEnv, key) {
					return
				}
				prefix := key + "="
				filtered := cleanEnv[:0]
				for _, kv := range cleanEnv {
					if !strings.HasPrefix(kv, prefix) {
						filtered = append(filtered, kv)
					}
				}
				cleanEnv = append(filtered, prefix+val)
			}
			setIfMissing("ANTIGRAVITY_LS_ADDRESS", rawAddr)
			setIfMissing("ANTIGRAVITY_CSRF_TOKEN", strings.TrimSpace(creds.CSRFToken))
			setIfMissing("ANTIGRAVITY_AGENTAPI_EXE", strings.TrimSpace(creds.AgentAPIExe))
		}
	}
	cmd.Env = cleanEnv
}

func isQuotaExhaustedError(msg string) bool {
	return strings.Contains(msg, "RESOURCE_EXHAUSTED") || strings.Contains(msg, "429")
}

func resolveRoutineFallbackModel(vaultDir string) string {
	rFallback := routing.Resolve(vaultDir, routing.Request{ExplicitTier: "routine"})
	fallbackModel := strings.TrimSpace(rFallback.FallbackModel)
	if fallbackModel == "" {
		fallbackModel = rFallback.ModelID
	}
	return fallbackModel
}

func spawnTaskViaLS(vaultDir, title, prompt, modelID, workDir, projectID string) (cascadeID string, usedModel string, fallbackToAgentAPI bool, fallbackReason string, err error) {
	_, regAvail := ValidateProjectMatchesWorkspace(vaultDir, projectID, workDir)
	canFallbackSafely := func(requireVerifiedProj bool) bool {
		if _, lpErr := exec.LookPath("agentapi"); lpErr != nil {
			return false
		}
		if workDir == "" {
			return true
		}
		if requireVerifiedProj || regAvail {
			return strings.TrimSpace(projectID) != ""
		}
		return true
	}

	creds, ok := discoverLSCredentials(vaultDir)
	if !ok {
		if canFallbackSafely(false) {
			return "", "", true, "Language Server credentials unavailable", nil
		}
		return "", "", false, "", fmt.Errorf("spawnTaskViaLS: Language Server credentials unavailable and no verified projectId for workspace %s (refusing unverified agentapi fallback)", workDir)
	}
	modelEnum, enumErr := ResolveModelEnum(creds.BaseURL, creds.CSRFToken, modelID)
	if enumErr != nil {
		if canFallbackSafely(true) {
			return "", "", true, fmt.Sprintf("ResolveModelEnum(%q) failed: %v", modelID, enumErr), nil
		}
		return "", "", false, "", fmt.Errorf("spawnTaskViaLS: %w (refusing unverified agentapi fallback without projectId for workspace %s)", enumErr, workDir)
	}

	usedModel = modelID
	reqBody := buildCascadeRequestBody(modelEnum, "", "", workDir, projectID)
	cascadeID, err = startCascade(creds.BaseURL, creds.CSRFToken, reqBody)
	if err != nil {
		if isQuotaExhaustedError(err.Error()) && !strings.Contains(modelID, "flash") && !strings.Contains(modelID, "hydrogen") {
			fallbackModel := resolveRoutineFallbackModel(vaultDir)
			if fbEnum, fbEnumErr := ResolveModelEnum(creds.BaseURL, creds.CSRFToken, fallbackModel); fbEnumErr == nil {
				fbReqBody := buildCascadeRequestBody(fbEnum, "", "", workDir, projectID)
				if fbCascadeID, fbErr := startCascade(creds.BaseURL, creds.CSRFToken, fbReqBody); fbErr == nil {
					cascadeID = fbCascadeID
					usedModel = fallbackModel + " (fallback)"
					err = nil
				}
			}
		}
		if err != nil {
			if isQuotaExhaustedError(err.Error()) {
				return "", usedModel, false, "", err
			}
			if canFallbackSafely(true) {
				return "", "", true, fmt.Sprintf("StartCascade failed: %v", err), nil
			}
			return "", usedModel, false, "", err
		}
	}

	if sendErr := sendUserCascadeMessage(creds.BaseURL, creds.CSRFToken, cascadeID, prompt, 10*time.Second); sendErr != nil {
		msgCmd := exec.Command("agentapi", "send-message", "--title="+title, cascadeID, prompt)
		if msgErr := msgCmd.Run(); msgErr != nil {
			return cascadeID, usedModel, false, "", fmt.Errorf("StartCascade succeeded (%s) but SendUserCascadeMessage failed: %w", cascadeID, sendErr)
		}
	}
	if strings.TrimSpace(title) != "" {
		_ = NewSidebarSyncClient(SidebarSyncConfig{
			BaseURL:   creds.BaseURL,
			CSRFToken: creds.CSRFToken,
		}).UpdateConversationAnnotations(context.Background(), cascadeID, nil, lsConversationPatch{
			Title: strings.TrimSpace(title),
		}, 3*time.Second)
	}
	return cascadeID, usedModel, false, "", nil
}

// SpawnTask creates and starts a top-level Antigravity conversation (via LanguageServerService/StartCascade
// + SendUserCascadeMessage when LS credentials are available, falling back to `agentapi new-conversation` only when workspace-safe),
// enforcing the daily spawn budget, auto-downgrading to routine tier if a higher-tier model hits 429,
// and tripping the circuit breaker only if Hydrogen/Flash is also exhausted.
func SpawnTask(vaultDir, trigger, skill, title, prompt, modelID string, dryRun bool, force ...bool) (string, error) {
	if modelID == "" {
		modelID, _, _ = ResolveVaultModel(vaultDir, prompt, "", "", "")
	}
	isForced := len(force) > 0 && force[0]
	isInteractive := isInteractiveSpawnTrigger(trigger)
	state := LoadQuota(vaultDir)
	if !isInteractive && !isForced {
		if state.CircuitOpen {
			return "", fmt.Errorf("quota circuit breaker is OPEN: %s", state.CircuitReason)
		}
		autoUsed := state.EffectiveAutoSpawns()
		if autoUsed >= state.MaxDailySpawns {
			state.CircuitOpen = true
			state.CircuitReason = fmt.Sprintf("daily spawn budget reached (%d/%d)", autoUsed, state.MaxDailySpawns)
			_ = SaveQuota(vaultDir, state)
			return "", fmt.Errorf("daily spawn budget exhausted (%d/%d)", autoUsed, state.MaxDailySpawns)
		}
	}

	var output string
	var fallbackNote string
	if dryRun {
		output = fmt.Sprintf("[dry-run] agentapi new-conversation --model=%s --title=%q (skill=%s)", modelID, title, skill)
	} else {
		workDir, projectID := resolveSpawnTarget(vaultDir, trigger)
		lsOut, lsModel, useAgentAPI, fbReason, lsErr := spawnTaskViaLS(vaultDir, title, prompt, modelID, workDir, projectID)
		if !useAgentAPI {
			if lsModel != "" {
				modelID = lsModel
			}
			if lsErr != nil {
				if !isInteractive && isQuotaExhaustedError(lsErr.Error()) {
					state.CircuitOpen = true
					state.CircuitReason = "RESOURCE_EXHAUSTED (429) detected from LanguageServerService"
				}
				errText := lsErr.Error()
				if runes := []rune(errText); len(runes) > 240 {
					errText = string(runes[:240]) + "..."
				}
				state.Events = append(state.Events, SpawnEvent{
					Timestamp: time.Now().Format(time.RFC3339),
					Trigger:   trigger,
					Skill:     skill,
					Model:     modelID,
					Outcome:   "error: " + errText,
				})
				_ = SaveQuota(vaultDir, state)
				return lsOut, lsErr
			}
			output = lsOut
		} else {
			if fbReason != "" {
				fallbackNote = fbReason
				fmt.Fprintf(os.Stderr, "[hq spawn] falling back to agentapi (%s)\n", fbReason)
			}
			cmd := exec.Command("agentapi", "new-conversation", "--model="+modelID, "--title="+title, "--", prompt)
			configureSpawnCommand(cmd, vaultDir, trigger)
			outBytes, err := cmd.CombinedOutput()
			output = strings.TrimSpace(string(outBytes))
			if err != nil {
				// Quota-aware fallback: if a non-Hydrogen/non-Flash model hit 429, retry once on fallback_on_429 (or routine tier)
				if isQuotaExhaustedError(output) && !strings.Contains(modelID, "flash") && !strings.Contains(modelID, "hydrogen") {
					fallbackModel := resolveRoutineFallbackModel(vaultDir)
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
				if !isInteractive && isQuotaExhaustedError(output) {
					state.CircuitOpen = true
					state.CircuitReason = "RESOURCE_EXHAUSTED (429) detected from agentapi"
				}
				errText := err.Error()
				if runes := []rune(errText); len(runes) > 240 {
					errText = string(runes[:240]) + "..."
				}
				state.Events = append(state.Events, SpawnEvent{
					Timestamp: time.Now().Format(time.RFC3339),
					Trigger:   trigger,
					Skill:     skill,
					Model:     modelID,
					Outcome:   "error: " + errText,
				})
				_ = SaveQuota(vaultDir, state)
				return output, fmt.Errorf("agentapi error: %w (%s)", err, output)
			}
		}
	}

	if state.SpawnsToday > 0 && state.AutoSpawnsToday == 0 && state.InteractiveSpawnsToday == 0 {
		state.AutoSpawnsToday = state.EffectiveAutoSpawns()
	}
	if isInteractive {
		state.InteractiveSpawnsToday++
	} else {
		state.AutoSpawnsToday++
	}
	state.SpawnsToday++
	state.LastStatus = fmt.Sprintf("SPAWNED (%s -> /%s [%s])", trigger, skill, modelID)
	outcome := output
	if fallbackNote != "" {
		outcome = fmt.Sprintf("%s [agentapi fallback: %s]", output, fallbackNote)
	}
	if runes := []rune(outcome); len(runes) > 240 {
		outcome = string(runes[:240]) + "..."
	}
	state.Events = append(state.Events, SpawnEvent{
		Timestamp: time.Now().Format(time.RFC3339),
		Trigger:   trigger,
		Skill:     skill,
		Model:     modelID,
		Outcome:   outcome,
	})
	_ = SaveQuota(vaultDir, state)
	return output, nil
}

// SpawnFlashTask is a convenience wrapper for background watcher spawns resolved via standard tier.
func SpawnFlashTask(vaultDir, trigger, skill, title, prompt string, dryRun bool) (string, error) {
	modelID, _, _ := ResolveVaultModel(vaultDir, prompt, "", "", "standard")
	return SpawnTask(vaultDir, trigger, skill, title, prompt, modelID, dryRun)
}

// ProcessInbox scans local/inbox/ for newly dropped files, moves each to local/inbox/queued/,
// recovers any stranded files in local/inbox/queued/, enqueues durable events, and dispatches ready batches.
func ProcessInbox(vaultDir string, dryRun bool) ([]string, error) {
	now := time.Now()
	if err := EnqueueInboxFiles(vaultDir, now); err != nil {
		return nil, err
	}
	return DispatchPendingEvents(vaultDir, now, dryRun)
}

// RunHeartbeat executes the zero-token Go sidecar pre-check loop (proposal TTL sweep, durable timer evaluation,
// inbox drop router, schedule-gated daemon gatekeepers/watchers, event inbox dispatch, outbox interrupt
// delivery, and Waiting-On obligation SLA sweeps).
// If no actionable deltas are detected, zero LLM tokens are used.
func RunHeartbeat(vaultDir string, dryRun bool) (string, error) {
	return RunHeartbeatAt(vaultDir, time.Now(), dryRun)
}

// RunHeartbeatAt runs RunHeartbeat at an explicit reference timestamp `now` so connector watcher polling
// strictly respects `triggers.yaml` schedule enabled/paused overrides and cron intervals via EvaluateDaemonGatekeepers.
func RunHeartbeatAt(vaultDir string, now time.Time, dryRun bool) (string, error) {
	if now.IsZero() {
		now = time.Now()
	}
	_, _ = approvals.ExpireStaleProposals(vaultDir, now)
	_, _ = timers.EvaluateDue(vaultDir, now)

	if err := EnqueueInboxFiles(vaultDir, now); err != nil {
		return "", err
	}
	inboxActions, err := DispatchPendingEvents(vaultDir, now, dryRun)
	if err != nil {
		return "", err
	}

	if gkActions, _, gkErr := EvaluateDaemonGatekeepers(vaultDir, now, false, dryRun); gkErr == nil && len(gkActions) > 0 {
		inboxActions = append(inboxActions, gkActions...)
	}

	if extraActions, dispErr := DispatchPendingEvents(vaultDir, now, dryRun); dispErr == nil && len(extraActions) > 0 {
		inboxActions = append(inboxActions, extraActions...)
	}

	if goalActions, gErr := SweepWaitingOnObligations(vaultDir, now); gErr == nil && len(goalActions) > 0 {
		inboxActions = append(inboxActions, goalActions...)
	}

	if !dryRun {
		_, _ = DispatchOutboxToFrontDesk(vaultDir, now, false)
	}

	_, _ = ReconcileAutoTasks(vaultDir, now, dryRun)

	if !dryRun && HasPendingSidebarSyncQueue(vaultDir) {
		_, _ = DrainSidebarSyncQueue(vaultDir, DefaultSidebarBatchTimeout)
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

func normalizeSubagentPromptText(s string) string {
	s = strings.ReplaceAll(s, "<USER_REQUEST>", " ")
	s = strings.ReplaceAll(s, "</USER_REQUEST>", " ")
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func matchesPendingSubagentPrompt(callerPrompt, itemPrefix string) bool {
	cp := normalizeSubagentPromptText(callerPrompt)
	ip := normalizeSubagentPromptText(strings.TrimSuffix(strings.TrimSpace(itemPrefix), "..."))
	if cp == "" || ip == "" {
		return true
	}
	if runes := []rune(ip); len(runes) > 80 {
		ip = string(runes[:80])
	}
	return strings.Contains(cp, ip) || strings.Contains(ip, cp)
}

// PopPendingSubagent pops the oldest unclaimed pending subagent created within maxAge.
func PopPendingSubagent(vaultDir string, maxAge time.Duration) (PendingSubagent, bool) {
	return PopPendingSubagentForCaller(vaultDir, "", "", maxAge)
}

// PopPendingSubagentForCaller pops the oldest matching unclaimed pending subagent within maxAge,
// rejecting self-parenting (`ParentConversationID == callerConvID`) and mismatched prompts when callerPrompt is known.
func PopPendingSubagentForCaller(vaultDir, callerConvID, callerPrompt string, maxAge time.Duration) (PendingSubagent, bool) {
	queue, err := loadPendingSubagents(vaultDir)
	if err != nil || len(queue) == 0 {
		return PendingSubagent{}, false
	}
	callerConvID = strings.TrimSpace(callerConvID)
	cutoff := time.Now().Add(-maxAge)
	var remaining []PendingSubagent
	var matched PendingSubagent
	found := false
	for _, item := range queue {
		if !item.CreatedAt.After(cutoff) {
			continue
		}
		isSelf := callerConvID != "" && strings.TrimSpace(item.ParentConversationID) == callerConvID
		if !found && !isSelf && matchesPendingSubagentPrompt(callerPrompt, item.PromptPrefix) {
			matched = item
			found = true
			continue
		}
		remaining = append(remaining, item)
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
	return PopPendingNestedSubagentForCaller(vaultDir, "", "", maxAge)
}

// PopPendingNestedSubagentForCaller pops the oldest matching pending nested subagent within maxAge,
// rejecting self-parenting (`ParentConversationID == callerConvID`) and mismatched prompts when callerPrompt is known.
func PopPendingNestedSubagentForCaller(vaultDir, callerConvID, callerPrompt string, maxAge time.Duration) (PendingSubagent, bool) {
	queue, err := loadPendingNestedSubagents(vaultDir)
	if err != nil || len(queue) == 0 {
		return PendingSubagent{}, false
	}
	callerConvID = strings.TrimSpace(callerConvID)
	cutoff := time.Now().Add(-maxAge)
	var remaining []PendingSubagent
	var matched PendingSubagent
	found := false
	for _, item := range queue {
		if !item.CreatedAt.After(cutoff) {
			continue
		}
		isSelf := callerConvID != "" && strings.TrimSpace(item.ParentConversationID) == callerConvID
		if !found && !isSelf && matchesPendingSubagentPrompt(callerPrompt, item.PromptPrefix) {
			matched = item
			found = true
			continue
		}
		remaining = append(remaining, item)
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
			cid := strings.TrimSpace(item.ConversationID)
			pid := strings.TrimSpace(item.ParentConversationID)
			if cid != "" && (pid == "" || cid != pid) {
				m[cid] = item
			}
		}
		return m, nil
	}
	var m map[string]NestedConversation
	if err := json.Unmarshal(data, &m); err == nil {
		for k, item := range m {
			if item.ParentConversationID != "" && strings.TrimSpace(item.ConversationID) == strings.TrimSpace(item.ParentConversationID) {
				delete(m, k)
			}
		}
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
	parentConvID = strings.TrimSpace(parentConvID)
	if convID == "" || (parentConvID != "" && convID == parentConvID) {
		return nil
	}
	m, _ := loadNestedConversations(vaultDir)
	if m == nil {
		m = make(map[string]NestedConversation)
	}
	m[convID] = NestedConversation{
		ConversationID:       convID,
		ParentConversationID: parentConvID,
		CreatedAt:            time.Now().Format(time.RFC3339),
	}
	return saveNestedConversations(vaultDir, m)
}

// RemoveNestedConversation deletes any stale nested-subagent classification for convID
// (used when registering, spawning, or adopting a top-level Kind="task" conversation).
func RemoveNestedConversation(vaultDir, convID string) error {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return nil
	}
	m, err := loadNestedConversations(vaultDir)
	if err != nil || len(m) == 0 {
		return nil
	}
	if _, exists := m[convID]; !exists {
		return nil
	}
	delete(m, convID)
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
// Explicit top-level `Kind: "task"` conversations are never classified as ephemeral subagents even if
// previously adopted or poisoned in `nested_conversations.json`.
func IsSubagentConversation(vaultDir, convID string) bool {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return false
	}
	if task, _, found := FindTaskAnywhere(vaultDir, convID); found {
		if isExplicitTopLevelTask(task) {
			return false
		}
		if strings.EqualFold(task.Kind, "subagent") || task.EffectiveKind() == "subagent" ||
			task.ParentConversationID != "" ||
			strings.HasPrefix(task.TaskSlug, "subagent-") ||
			strings.HasPrefix(task.Title, "[Subagent:") {
			return true
		}
	}
	return IsNestedSubagentConversation(vaultDir, convID)
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
		if !isExplicitTopLevelTask(t) && t.ConversationID != "" && IsNestedSubagentConversation(vaultDir, t.ConversationID) {
			continue
		}
		if IsLayer1Task(t, fd.ConversationID) {
			out = append(out, t)
		}
	}
	return out
}

// CleanNestedTasks archives or removes nested child subagents from local/memory/tasks.json,
// moving them to tasks_archive.jsonl, scrubbing poisoned top-level task/self-parented IDs from
// nested_conversations.json, and adjusting quota so that only Layer-1 tasks are tracked.
func CleanNestedTasks(vaultDir string) ([]string, error) {
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return nil, err
	}
	fd, _ := GetFrontDesk(vaultDir)

	explicitTaskIDs := make(map[string]bool)
	for _, t := range tasks {
		if t.ConversationID != "" && isExplicitTopLevelTask(t) {
			explicitTaskIDs[strings.TrimSpace(t.ConversationID)] = true
		}
	}
	if nestedMap, nErr := loadNestedConversations(vaultDir); nErr == nil && len(nestedMap) > 0 {
		scrubbed := false
		for cid, rec := range nestedMap {
			if explicitTaskIDs[cid] || (fd.ConversationID != "" && cid == fd.ConversationID) || (rec.ParentConversationID != "" && cid == rec.ParentConversationID) {
				delete(nestedMap, cid)
				scrubbed = true
			}
		}
		if scrubbed {
			_ = saveNestedConversations(vaultDir, nestedMap)
		}
	}

	// Pre-seed known nested conversations from quota events or journal if any
	quota := LoadQuota(vaultDir)
	re := regexp.MustCompile(`Spawned subagent ([0-9a-fA-F-]+) \(parent=([0-9a-fA-F-]+)`)
	for _, ev := range quota.Events {
		if strings.Contains(ev.Outcome, "parent=") {
			if m := re.FindStringSubmatch(ev.Outcome); len(m) == 3 {
				cid := m[1]
				pid := m[2]
				if fd.ConversationID != "" && pid != fd.ConversationID && !explicitTaskIDs[cid] {
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
		if !isExplicitTopLevelTask(t) && t.EffectiveKind() == "subagent" {
			if t.ConversationID != "" && IsNestedSubagentConversation(vaultDir, t.ConversationID) {
				isNested = true
			} else {
				parentID := strings.TrimSpace(t.ParentConversationID)
				if parentID != "" && fd.ConversationID != "" && parentID != fd.ConversationID {
					isNested = true
				}
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

type lsCredentials struct {
	RawAddress      string `json:"address,omitempty"`
	BaseURL         string `json:"baseUrl,omitempty"`
	CSRFToken       string `json:"csrfToken,omitempty"`
	AgentAPIExe     string `json:"agentApiExe,omitempty"`
	PluginShimDir   string `json:"pluginShimDir,omitempty"`
	CredentialScope string `json:"credentialScope,omitempty"`
	AppDataDir      string `json:"appDataDir,omitempty"`
	GeminiDir       string `json:"geminiDir,omitempty"`
	ConfigRelDir    string `json:"configRelDir,omitempty"`
	ProjectID       string `json:"projectId,omitempty"`
	UpdatedAt       string `json:"updatedAt,omitempty"`
}

func lsSessionFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "runtime", "ls_session.json")
}

func normalizeLSAddress(addr string) (rawHostPort, baseURL string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", ""
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		baseURL = strings.TrimRight(addr, "/")
		if u, err := url.Parse(baseURL); err == nil {
			rawHostPort = u.Host
		}
		return rawHostPort, baseURL
	}
	rawHostPort = strings.TrimRight(addr, "/")
	baseURL = "http://" + rawHostPort
	return rawHostPort, baseURL
}

func normalizeLSBaseURL(addr string) string {
	_, baseURL := normalizeLSAddress(addr)
	return baseURL
}

func isTCPReachable(baseURL string) bool {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return false
	}
	u, err := url.Parse(trimmed)
	hostPort := ""
	scheme := ""
	if err == nil && u.Host != "" {
		hostPort = u.Host
		scheme = u.Scheme
	} else {
		hostPort, _ = normalizeLSAddress(trimmed)
	}
	if hostPort == "" {
		return false
	}
	if _, _, splitErr := net.SplitHostPort(hostPort); splitErr != nil {
		if strings.EqualFold(scheme, "https") {
			hostPort = net.JoinHostPort(hostPort, "443")
		} else {
			hostPort = net.JoinHostPort(hostPort, "80")
		}
	}
	conn, dialErr := net.DialTimeout("tcp", hostPort, 250*time.Millisecond)
	if dialErr != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// RecordLiveLSSession persists live Language Server session coordinates from the current
// process environment into `<vault>/private/runtime/ls_session.json` (`0600`) without ever
// clobbering existing credentials or desktop plugin-CLI coordinates from headless/quarantined runs.
func RecordLiveLSSession(vaultDir string) error {
	if strings.TrimSpace(vaultDir) == "" {
		return nil
	}
	p := lsSessionFilePath(vaultDir)
	var existing lsCredentials
	hasExisting := false
	if raw, err := os.ReadFile(p); err == nil {
		if json.Unmarshal(raw, &existing) == nil {
			hasExisting = true
		}
	}

	rawAddrEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_LS_ADDRESS"))
	csrfEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CSRF_TOKEN"))
	agentExeEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_AGENTAPI_EXE"))
	shimDirEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_PLUGIN_SHIM_DIR"))
	scopeEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CREDENTIAL_SCOPE"))
	appDataEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_APP_DATA_DIR"))
	geminiDirEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_GEMINI_DIR"))
	configRelEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONFIG_REL_DIR"))
	primaryWS := primaryVaultWorkspaceDir(vaultDir)
	projEnv := ""
	if isCallerInPrimaryVaultWorkspace(vaultDir) {
		projEnv = strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
	}
	projRepaired := false
	if primaryWS != "" {
		verifiedExisting, _, _ := ResolveVerifiedWorkspaceProjectID(vaultDir, primaryWS, existing.ProjectID)
		if existing.ProjectID != verifiedExisting {
			existing.ProjectID = verifiedExisting
			projRepaired = true
		}
		if projEnv != "" {
			if projEnv == existing.ProjectID {
				projEnv = verifiedExisting
			} else {
				verifiedEnv, _, _ := ResolveVerifiedWorkspaceProjectID(vaultDir, primaryWS, projEnv)
				projEnv = verifiedEnv
			}
		}
	}

	// Never persist quarantined sentinel paths or non-existent shim directories.
	// Note: Antigravity's /tmp/agy-plugin-cli-<id> directory is created per run_command and cleaned up
	// upon command exit, whereas hooks receive ANTIGRAVITY_CREDENTIAL_SCOPE without ANTIGRAVITY_PLUGIN_SHIM_DIR;
	// keep scopeEnv intact unless explicitly quarantined by /nonexistent-hq-sentinel.
	if shimDirEnv == "/nonexistent-hq-sentinel" {
		shimDirEnv = ""
		scopeEnv = ""
	} else if shimDirEnv != "" {
		if st, err := os.Stat(shimDirEnv); err != nil || !st.IsDir() {
			shimDirEnv = ""
		}
	}
	if shimDirEnv == "" && existing.PluginShimDir != "" && existing.PluginShimDir != "/nonexistent-hq-sentinel" {
		if st, err := os.Stat(existing.PluginShimDir); err != nil || !st.IsDir() {
			existing.PluginShimDir = ""
		}
	}

	pick := func(cur, prev string) string {
		if cur != "" {
			return cur
		}
		return strings.TrimSpace(prev)
	}

	if rawAddrEnv == "" || csrfEnv == "" {
		if hasExisting && existing.BaseURL != "" && existing.CSRFToken != "" {
			updated := existing
			changed := projRepaired
			if agentExeEnv != "" && existing.AgentAPIExe != agentExeEnv {
				updated.AgentAPIExe = agentExeEnv
				changed = true
			}
			if shimDirEnv != "" && existing.PluginShimDir != shimDirEnv {
				updated.PluginShimDir = shimDirEnv
				changed = true
			}
			if scopeEnv != "" && existing.CredentialScope != scopeEnv {
				updated.CredentialScope = scopeEnv
				changed = true
			}
			if appDataEnv != "" && existing.AppDataDir != appDataEnv {
				updated.AppDataDir = appDataEnv
				changed = true
			}
			if geminiDirEnv != "" && existing.GeminiDir != geminiDirEnv {
				updated.GeminiDir = geminiDirEnv
				changed = true
			}
			if configRelEnv != "" && existing.ConfigRelDir != configRelEnv {
				updated.ConfigRelDir = configRelEnv
				changed = true
			}
			if projEnv != "" && existing.ProjectID != projEnv {
				updated.ProjectID = projEnv
				changed = true
			}
			if changed {
				updated.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				data, err := json.MarshalIndent(updated, "", "  ")
				if err != nil {
					return err
				}
				return store.WriteAtomic(p, append(data, '\n'), 0o600)
			}
		}
		return nil
	}

	normRaw, normBase := normalizeLSAddress(rawAddrEnv)
	if normBase == "" {
		return nil
	}
	creds := lsCredentials{
		RawAddress:      normRaw,
		BaseURL:         normBase,
		CSRFToken:       csrfEnv,
		AgentAPIExe:     pick(agentExeEnv, existing.AgentAPIExe),
		PluginShimDir:   pick(shimDirEnv, existing.PluginShimDir),
		CredentialScope: pick(scopeEnv, existing.CredentialScope),
		AppDataDir:      pick(appDataEnv, existing.AppDataDir),
		GeminiDir:       pick(geminiDirEnv, existing.GeminiDir),
		ConfigRelDir:    pick(configRelEnv, existing.ConfigRelDir),
		ProjectID:       pick(projEnv, existing.ProjectID),
		UpdatedAt:       time.Now().UTC().Format(time.RFC3339),
	}
	if creds.PluginShimDir == "/nonexistent-hq-sentinel" {
		creds.PluginShimDir = ""
		creds.CredentialScope = ""
	}
	if hasExisting &&
		existing.RawAddress == creds.RawAddress &&
		existing.BaseURL == creds.BaseURL &&
		existing.CSRFToken == creds.CSRFToken &&
		existing.AgentAPIExe == creds.AgentAPIExe &&
		existing.PluginShimDir == creds.PluginShimDir &&
		existing.CredentialScope == creds.CredentialScope &&
		existing.AppDataDir == creds.AppDataDir &&
		existing.GeminiDir == creds.GeminiDir &&
		existing.ConfigRelDir == creds.ConfigRelDir &&
		existing.ProjectID == creds.ProjectID {
		if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(existing.UpdatedAt)); err == nil && time.Since(ts) < 60*time.Second {
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(p, append(data, '\n'), 0o600)
}

func loadReachableLSSession(vaultDir string) (lsCredentials, bool) {
	if strings.TrimSpace(vaultDir) == "" {
		return lsCredentials{}, false
	}
	raw, err := os.ReadFile(lsSessionFilePath(vaultDir))
	if err != nil {
		return lsCredentials{}, false
	}
	var creds lsCredentials
	if json.Unmarshal(raw, &creds) != nil {
		return lsCredentials{}, false
	}
	if creds.BaseURL == "" && creds.RawAddress != "" {
		creds.RawAddress, creds.BaseURL = normalizeLSAddress(creds.RawAddress)
	} else if creds.RawAddress == "" && creds.BaseURL != "" {
		creds.RawAddress, creds.BaseURL = normalizeLSAddress(creds.BaseURL)
	}
	creds.CSRFToken = strings.TrimSpace(creds.CSRFToken)
	creds.AgentAPIExe = strings.TrimSpace(creds.AgentAPIExe)
	if creds.BaseURL == "" || creds.CSRFToken == "" || !isTCPReachable(creds.BaseURL) {
		return lsCredentials{}, false
	}
	return creds, true
}

// HasReachableLSSession reports whether live or persisted Antigravity LS credentials are
// discoverable and currently listening on TCP.
func HasReachableLSSession(vaultDir string) bool {
	creds, ok := discoverLSCredentials(vaultDir)
	if !ok {
		return false
	}
	return isTCPReachable(creds.BaseURL)
}

func discoverLSCredentials(vaultDir string) (lsCredentials, bool) {
	if addr, csrf := strings.TrimSpace(os.Getenv("ANTIGRAVITY_LS_ADDRESS")), strings.TrimSpace(os.Getenv("ANTIGRAVITY_CSRF_TOKEN")); addr != "" && csrf != "" {
		rawAddr, baseURL := normalizeLSAddress(addr)
		creds := lsCredentials{
			RawAddress:  rawAddr,
			BaseURL:     baseURL,
			CSRFToken:   csrf,
			AgentAPIExe: strings.TrimSpace(os.Getenv("ANTIGRAVITY_AGENTAPI_EXE")),
		}
		if sess, ok := loadReachableLSSession(vaultDir); ok {
			if creds.AgentAPIExe == "" {
				creds.AgentAPIExe = sess.AgentAPIExe
			}
			creds.PluginShimDir = sess.PluginShimDir
			creds.CredentialScope = sess.CredentialScope
			creds.AppDataDir = sess.AppDataDir
			creds.GeminiDir = sess.GeminiDir
			creds.ConfigRelDir = sess.ConfigRelDir
			creds.ProjectID = sess.ProjectID
		}
		return creds, true
	}
	if creds, ok := loadReachableLSSession(vaultDir); ok {
		return creds, true
	}

	roots := hostpaths.Resolve(vaultDir)
	if roots.AppDataDir == "" {
		return lsCredentials{}, false
	}
	// In hermetic temp-vault tests without an explicit temp AppDataDir, never probe the live host's ~/.gemini/antigravity/daemon.
	if hostpaths.IsTempPath(vaultDir) && !hostpaths.IsTempPath(roots.AppDataDir) && os.Getenv("HQ_ALLOW_LIVE_LS_DISCOVERY") != "1" {
		return lsCredentials{}, false
	}

	matches, err := filepath.Glob(filepath.Join(roots.AppDataDir, "daemon", "hub_*.json"))
	if err != nil || len(matches) == 0 {
		return lsCredentials{}, false
	}
	type hubCandidate struct {
		path    string
		modTime time.Time
	}
	candidates := make([]hubCandidate, 0, len(matches))
	for _, m := range matches {
		if st, sErr := os.Stat(m); sErr == nil && !st.IsDir() {
			candidates = append(candidates, hubCandidate{path: m, modTime: st.ModTime()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})

	for _, c := range candidates {
		data, rErr := os.ReadFile(c.path)
		if rErr != nil {
			continue
		}
		var hub struct {
			PID       int    `json:"pid"`
			HTTPPort  int    `json:"httpPort"`
			CSRFToken string `json:"csrfToken"`
		}
		if json.Unmarshal(data, &hub) != nil || hub.HTTPPort <= 0 || strings.TrimSpace(hub.CSRFToken) == "" {
			continue
		}
		if hub.PID > 0 && !isProcessAlive(hub.PID) {
			continue
		}
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", hub.HTTPPort)
		if !isTCPReachable(baseURL) {
			continue
		}
		return lsCredentials{
			RawAddress: fmt.Sprintf("127.0.0.1:%d", hub.HTTPPort),
			BaseURL:    baseURL,
			CSRFToken:  strings.TrimSpace(hub.CSRFToken),
		}, true
	}
	return lsCredentials{}, false
}

func buildCascadeRequestBody(modelEnum, parentConvID, role, workspaceDir, projectID string) map[string]interface{} {
	reqBody := map[string]interface{}{
		"baseTrajectoryIdentifier": map[string]interface{}{
			"lastActiveDoc": true,
		},
		"source":         "CORTEX_TRAJECTORY_SOURCE_AGENT_API",
		"trajectoryType": "CORTEX_TRAJECTORY_TYPE_CASCADE",
		"requestedModel": modelEnum,
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
	if parentConvID != "" {
		reqBody["parentConversationId"] = parentConvID
		reqBody["subagentDescriptor"] = map[string]interface{}{
			"typeName": "self",
			"role":     role,
		}
		reqBody["agentScriptItem"] = map[string]interface{}{
			"name": "self",
		}
	}
	projectID = strings.TrimSpace(projectID)
	workspaceDir = strings.TrimSpace(workspaceDir)
	// StartCascade rejects requests specifying both projectEnvConfig and workspaceUris (HTTP 400 invalid_argument).
	if projectID != "" {
		reqBody["projectEnvConfig"] = map[string]interface{}{
			"projectId":                 projectID,
			"defaultProjectEnvironment": map[string]interface{}{},
		}
	} else if workspaceDir != "" {
		if absDir, absErr := filepath.Abs(workspaceDir); absErr == nil {
			workspaceDir = absDir
		}
		reqBody["workspaceUris"] = []string{"file://" + filepath.ToSlash(workspaceDir)}
	}
	return reqBody
}

func startCascade(lsBaseURL, csrfToken string, reqBody map[string]interface{}) (string, error) {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	startURL := fmt.Sprintf("%s/exa.language_server_pb.LanguageServerService/StartCascade", normalizeLSBaseURL(lsBaseURL))
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
	return startResp.CascadeID, nil
}

func sendUserCascadeMessage(lsBaseURL, csrfToken, cascadeID, text string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	sendURL := fmt.Sprintf("%s/exa.language_server_pb.LanguageServerService/SendUserCascadeMessage", normalizeLSBaseURL(lsBaseURL))
	sendPayload, err := json.Marshal(map[string]interface{}{
		"cascadeId": cascadeID,
		"items": []map[string]interface{}{
			{"text": text},
		},
	})
	if err != nil {
		return err
	}
	sendReq, err := http.NewRequest(http.MethodPost, sendURL, bytes.NewReader(sendPayload))
	if err != nil {
		return err
	}
	sendReq.Header.Set("Content-Type", "application/json")
	sendReq.Header.Set("x-codeium-csrf-token", csrfToken)
	client := &http.Client{Timeout: timeout}
	sendResp, err := client.Do(sendReq)
	if err != nil {
		return fmt.Errorf("SendUserCascadeMessage failed: %w", err)
	}
	defer sendResp.Body.Close()
	respBody, _ := io.ReadAll(sendResp.Body)
	if sendResp.StatusCode < 200 || sendResp.StatusCode >= 300 {
		return fmt.Errorf("SendUserCascadeMessage HTTP %d: %s", sendResp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
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
		url := fmt.Sprintf("%s/exa.language_server_pb.LanguageServerService/GetCascadeModelConfigData", normalizeLSBaseURL(lsAddr))
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
		modelID, _, _ = ResolveVaultModel(vaultDir, prompt, "", "", "")
	} else if !strings.HasPrefix(strings.TrimSpace(modelID), "MODEL_") {
		modelID, _, _ = ResolveVaultModel(vaultDir, prompt, modelID, "", "")
	}
	parentConvID = strings.TrimSpace(parentConvID)
	if parentConvID == "" {
		parentConvID = strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	}
	role = strings.TrimSpace(role)
	if role == "" {
		role = "[standard] general: Subagent Worker"
	}

	_ = len(force) // preserved for API compatibility; interactive subagents do not share daemon auto-spawn cap
	state := LoadQuota(vaultDir)
	isFrontDesk := IsFrontDeskConversation(vaultDir, parentConvID)

	tier, scope, _, title, slug := ParseSubagentRole(role, modelID)

	if dryRun {
		output := fmt.Sprintf("[dry-run] hq subagent --parent=%s --model=%s --role=%q", parentConvID, modelID, role)
		if isFrontDesk {
			if state.SpawnsToday > 0 && state.AutoSpawnsToday == 0 && state.InteractiveSpawnsToday == 0 {
				state.AutoSpawnsToday = state.EffectiveAutoSpawns()
			}
			state.InteractiveSpawnsToday++
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

	creds, ok := discoverLSCredentials(vaultDir)
	if !ok {
		return "", fmt.Errorf("ANTIGRAVITY_LS_ADDRESS and ANTIGRAVITY_CSRF_TOKEN are required to spawn a live subagent")
	}

	modelEnum, err := ResolveModelEnum(creds.BaseURL, creds.CSRFToken, modelID)
	if err != nil {
		return "", err
	}

	reqBody := buildCascadeRequestBody(modelEnum, parentConvID, role, workspaceDir, strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID")))
	cascadeID, err := startCascade(creds.BaseURL, creds.CSRFToken, reqBody)
	if err != nil {
		return "", err
	}

	fullPrompt := prompt
	if parentConvID != "" && !strings.Contains(fullPrompt, parentConvID) {
		fullPrompt = fmt.Sprintf("%s\n\nWhen you finish, send your final report back to parent conversation %s using the send_message tool (Recipient=%q).", prompt, parentConvID, parentConvID)
	}

	managedWt := ""
	if IsManagedWorktreeDir(vaultDir, workspaceDir) {
		managedWt = filepath.Clean(workspaceDir)
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
			WorktreeDir:          managedWt,
			ConversationID:       cascadeID,
			CreatedAt:            time.Now().Format(time.RFC3339),
			LastUpdatedAt:        time.Now().Format(time.RFC3339),
			LastSummary:          prompt,
			Status:               "active",
		})
	} else {
		// Nested descendant subagent: record in nested conversations so hooks ignore it for task tracking
		_ = RecordNestedConversation(vaultDir, cascadeID, parentConvID)
		if managedWt != "" {
			_, _ = AttachWorktreeToTaskWithOptions(vaultDir, parentConvID, managedWt, "", AttachWorktreeOptions{ReactivateCompleted: false})
		}
	}

	// Deliver initial prompt via SendUserCascadeMessage to kick off Turn 1 immediately
	// (Note: agentapi send-message only queues an inbox message and will not wake a 0-step StartCascade trajectory).
	sent := sendUserCascadeMessage(creds.BaseURL, creds.CSRFToken, cascadeID, fullPrompt, 10*time.Second) == nil
	if !sent && parentConvID != "" {
		msgCmd := exec.Command("agentapi", "send-message", "--title=Message from "+parentConvID, cascadeID, fullPrompt)
		_ = msgCmd.Run()
	}

	output := fmt.Sprintf("Spawned subagent %s (parent=%s, model=%s, role=%q)", cascadeID, parentConvID, modelID, role)
	if isFrontDesk {
		if state.SpawnsToday > 0 && state.AutoSpawnsToday == 0 && state.InteractiveSpawnsToday == 0 {
			state.AutoSpawnsToday = state.EffectiveAutoSpawns()
		}
		state.InteractiveSpawnsToday++
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
