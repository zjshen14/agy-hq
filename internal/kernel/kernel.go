package kernel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/channels"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/routing"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/internal/timers"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

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
	Ephemeral            bool     `json:"ephemeral,omitempty"`
	CreatedAt            string   `json:"createdAt"`
	LastUpdatedAt        string   `json:"lastUpdatedAt"`
	CompletedAt          string   `json:"completedAt,omitempty"`
	ArchivedAt           string   `json:"archivedAt,omitempty"`
	SidebarArchived      bool     `json:"sidebarArchived,omitempty"`
	SidebarSyncedAt      string   `json:"sidebarSyncedAt,omitempty"`
	EventIDs             []string `json:"eventIds,omitempty"`
	LastSummary          string   `json:"lastSummary,omitempty"`
	Status               string   `json:"status"` // active, idle, completed, archived, cancelled
	// Workspaces holds RFC-0025 provider workspace bindings (pack providers only; git worktrees stay in
	// WorktreeDir/WorktreeDirs). RegisterTask unions it by Key, so field-by-field rebuilds never drop bindings.
	Workspaces []workspace.Binding `json:"workspaces,omitempty"`
}

// AllWorkspaceBindings returns every workspace bound to t: read-only core/git-worktree bindings synthesized from
// the legacy WorktreeDir (role primary) and WorktreeDirs (role secondary) fields, keyed
// "core/git-worktree:<canonical path>", followed by the provider bindings stored in t.Workspaces.
func (t ActiveTask) AllWorkspaceBindings() []workspace.Binding {
	var out []workspace.Binding
	seen := make(map[string]bool)
	primary := strings.TrimSpace(t.WorktreeDir)
	for _, dir := range t.AllWorktreeDirs() {
		root := workspace.CanonicalPath(dir)
		if root == "" {
			continue
		}
		key := workspace.LedgerKey(workspace.CoreGitProviderID, root)
		if seen[key] {
			continue
		}
		seen[key] = true
		role := workspace.RoleSecondary
		if primary != "" && sameCleanPath(dir, primary) {
			role = workspace.RolePrimary
		}
		out = append(out, workspace.Binding{
			Key:      key,
			Provider: workspace.CoreGitProviderID,
			Root:     root,
			WorkDir:  root,
			Role:     role,
		})
	}
	for _, b := range t.Workspaces {
		if b.Key == "" || seen[b.Key] {
			continue
		}
		seen[b.Key] = true
		out = append(out, b)
	}
	return out
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

func parseFlexibleRFC3339(raw string) time.Time {
	s := strings.TrimSpace(raw)
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

func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if runes := []rune(s); len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "..."
	}
	return s
}

// UpdatedTime parses LastUpdatedAt (falling back to CreatedAt) into time.Time.
func (t ActiveTask) UpdatedTime() time.Time {
	for _, raw := range []string{t.LastUpdatedAt, t.CreatedAt} {
		if ts := parseFlexibleRFC3339(raw); !ts.IsZero() {
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

func tasksLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", ".tasks.lock")
}

func tasksArchiveFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "tasks_archive.jsonl")
}

func withTasksLock(vaultDir string, fn func() error) error {
	return store.With(context.Background(), store.RankTasks, tasksLockPath(vaultDir), func(_ context.Context) error {
		return fn()
	})
}

func saveTasksUnlocked(vaultDir string, tasks []ActiveTask) error {
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(tasksFilePath(vaultDir), data, 0o644)
}

// UpdateTasks executes an atomic locked read-modify-write update on local/memory/tasks.json under RankTasks.
// Callers must keep fn pure (no nested store locks, git subprocesses, or network RPCs).
func UpdateTasks(vaultDir string, fn func(tasks []ActiveTask) ([]ActiveTask, bool, error)) error {
	return withTasksLock(vaultDir, func() error {
		tasks, err := ListTasks(vaultDir)
		if err != nil {
			return err
		}
		updated, changed, err := fn(tasks)
		if err != nil || !changed {
			return err
		}
		return saveTasksUnlocked(vaultDir, updated)
	})
}

// ApplyTaskReconciliation atomically archives removed rows and updates local/memory/tasks.json
// under a single RankTasks lock acquisition.
func ApplyTaskReconciliation(vaultDir string, fn func(tasks []ActiveTask) (kept []ActiveTask, toArchive []ActiveTask, changed bool, err error)) error {
	return withTasksLock(vaultDir, func() error {
		tasks, err := ListTasks(vaultDir)
		if err != nil {
			return err
		}
		kept, toArchive, changed, err := fn(tasks)
		if err != nil || !changed {
			return err
		}
		if len(toArchive) > 0 {
			if err := appendTasksToColdArchiveUnlocked(vaultDir, toArchive); err != nil {
				return err
			}
		}
		return saveTasksUnlocked(vaultDir, kept)
	})
}

func normalizeTaskDefaults(t *ActiveTask) {
	if t.Kind == "" {
		t.Kind = t.EffectiveKind()
	}
	if t.Scope == "" {
		t.Scope = t.EffectiveScope()
	}
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
		normalizeTaskDefaults(&tasks[i])
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
		normalizeTaskDefaults(&t)
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
	return withTasksLock(vaultDir, func() error {
		return appendTasksToColdArchiveUnlocked(vaultDir, toArchive)
	})
}

func appendTasksToColdArchiveUnlocked(vaultDir string, toArchive []ActiveTask) error {
	if len(toArchive) == 0 {
		return nil
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
		normalizeTaskDefaults(&t)
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
	return store.WriteAtomic(tasksArchiveFilePath(vaultDir), []byte(b.String()), 0o644)
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
	var matched ActiveTask
	linked := false
	err := UpdateTasks(vaultDir, func(tasks []ActiveTask) ([]ActiveTask, bool, error) {
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
				matched = tasks[i]
				linked = true
				return tasks, true, nil
			}
		}
		return tasks, false, nil
	})
	if err != nil || !linked {
		return ActiveTask{}, false
	}
	if IsExplicitTopLevelTask(matched) {
		_ = RemoveNestedConversation(vaultDir, convID)
	}
	return matched, true
}

func isHexSlugSuffix(s string) bool {
	hexCount := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			hexCount++
			continue
		}
		if c == '-' {
			continue
		}
		return false
	}
	return hexCount >= 6
}

func isAutoConvSlug(slug, convID string) bool {
	if !strings.HasPrefix(slug, "conv-") {
		return false
	}
	suffix := strings.ToLower(strings.TrimPrefix(slug, "conv-"))
	cid := normalizeUUID(convID)
	return cid == "" || strings.HasPrefix(cid, suffix) || strings.HasPrefix(suffix, cid) || isHexSlugSuffix(suffix)
}

// IsExplicitTopLevelTask reports whether t is an explicitly created or adopted Layer-1 Task
// (Kind=="task", non-auto-adopted conv- slug, non-subagent slug/title, and either an explicit
// [Task:...] title or empty ParentConversationID). Explicitly adopted [Task:...] conversations
// remain Layer-1 even if they carry a Front Desk ParentConversationID or Front Desk rotates.
func IsExplicitTopLevelTask(t ActiveTask) bool {
	if !strings.EqualFold(strings.TrimSpace(t.Kind), "task") && t.EffectiveKind() != "task" {
		return false
	}
	if strings.HasPrefix(t.TaskSlug, "subagent-") || isAutoConvSlug(t.TaskSlug, t.ConversationID) {
		return false
	}
	if strings.HasPrefix(t.Title, "[Subagent:") {
		return false
	}
	return strings.HasPrefix(t.Title, "[Task:") || strings.TrimSpace(t.ParentConversationID) == ""
}

// RegisterTask records or updates a dedicated Task Conversation in local/memory/tasks.json.
func RegisterTask(vaultDir string, task ActiveTask) error {
	explicitTaskPromotion := IsExplicitTopLevelTask(task) && strings.EqualFold(strings.TrimSpace(task.Kind), "task")
	var bindEventIDs []string
	var removeNestedIDs []string
	err := UpdateTasks(vaultDir, func(tasks []ActiveTask) ([]ActiveTask, bool, error) {
		inTask := task
		updated := false
		for i := range tasks {
			if (inTask.TaskSlug != "" && tasks[i].TaskSlug == inTask.TaskSlug) || (inTask.ConversationID != "" && tasks[i].ConversationID == inTask.ConversationID) {
				effKind := strings.ToLower(strings.TrimSpace(inTask.Kind))
				if effKind == "" {
					effKind = tasks[i].EffectiveKind()
				}
				isAutoReactivation := effKind == "auto" && inTask.Status == "active" &&
					(tasks[i].Status != "active" || (inTask.ConversationID != "" && inTask.ConversationID != tasks[i].ConversationID))
				if inTask.TaskSlug == "" {
					inTask.TaskSlug = tasks[i].TaskSlug
				}
				if inTask.ConversationID == "" {
					inTask.ConversationID = tasks[i].ConversationID
				}
				if inTask.Title == "" {
					inTask.Title = tasks[i].Title
				}
				if inTask.Kind == "" {
					inTask.Kind = tasks[i].Kind
				}
				if inTask.Scope == "" {
					inTask.Scope = tasks[i].Scope
				}
				if inTask.Tier == "" {
					inTask.Tier = tasks[i].Tier
				}
				if inTask.Model == "" {
					inTask.Model = tasks[i].Model
				}
				if inTask.ParentConversationID == "" && !explicitTaskPromotion {
					inTask.ParentConversationID = tasks[i].ParentConversationID
				}
				inTask.WorktreeDir, inTask.WorktreeDirs = mergeTaskWorktrees(tasks[i], inTask)
				// Union provider bindings by key (incoming wins per key): callers such as the Stop hook rebuild
				// ActiveTask field by field and must never silently drop workspace bindings.
				inTask.Workspaces = workspace.MergeBindings(tasks[i].Workspaces, inTask.Workspaces)
				if inTask.RepoRoot == "" {
					inTask.RepoRoot = tasks[i].RepoRoot
				}
				if inTask.TurnCount == 0 && !(isAutoReactivation && inTask.ConversationID != "" && inTask.ConversationID != tasks[i].ConversationID) {
					inTask.TurnCount = tasks[i].TurnCount
				}
				if !inTask.Pinned && tasks[i].Pinned {
					inTask.Pinned = tasks[i].Pinned
				}
				if inTask.CreatedAt == "" {
					inTask.CreatedAt = tasks[i].CreatedAt
				}
				if inTask.Status == "active" {
					inTask.ArchivedAt = ""
					inTask.CompletedAt = ""
					inTask.SidebarArchived = false
				} else {
					if inTask.CompletedAt == "" {
						inTask.CompletedAt = tasks[i].CompletedAt
					}
					if inTask.ArchivedAt == "" {
						inTask.ArchivedAt = tasks[i].ArchivedAt
					}
					if !inTask.SidebarArchived && tasks[i].SidebarArchived {
						inTask.SidebarArchived = tasks[i].SidebarArchived
					}
					if inTask.SidebarSyncedAt == "" {
						inTask.SidebarSyncedAt = tasks[i].SidebarSyncedAt
					}
				}
				if inTask.Status == "" {
					inTask.Status = tasks[i].Status
				}
				if len(inTask.EventIDs) == 0 {
					inTask.EventIDs = tasks[i].EventIDs
				}
				if inTask.LastSummary == "" && !isAutoReactivation {
					inTask.LastSummary = tasks[i].LastSummary
				}
				inTask.Title = SanitizeTaskTitle(inTask.Title, inTask.TaskSlug)
				inTask.Kind = inTask.EffectiveKind()
				inTask.Scope = inTask.EffectiveScope()
				if (inTask.Status == "completed" || inTask.Status == "cancelled") && inTask.CompletedAt == "" {
					inTask.CompletedAt = inTask.LastUpdatedAt
				}
				tasks[i] = inTask
				updated = true
				break
			}
		}
		if !updated && inTask.TaskSlug == "" && inTask.ConversationID != "" {
			// If a task was just spawned via `hq spawn` with ConversationID=="" and is active, link it to this first Stop hook
			for i := len(tasks) - 1; i >= 0; i-- {
				if tasks[i].ConversationID == "" && tasks[i].Status == "active" {
					tasks[i].ConversationID = inTask.ConversationID
					if inTask.Model != "" {
						tasks[i].Model = inTask.Model
					}
					if inTask.LastUpdatedAt != "" {
						tasks[i].LastUpdatedAt = inTask.LastUpdatedAt
					}
					if inTask.LastSummary != "" {
						tasks[i].LastSummary = inTask.LastSummary
					}
					if inTask.TurnCount > 0 {
						tasks[i].TurnCount = inTask.TurnCount
					}
					if len(inTask.Workspaces) > 0 {
						tasks[i].Workspaces = workspace.MergeBindings(tasks[i].Workspaces, inTask.Workspaces)
					}
					tasks[i].Title = SanitizeTaskTitle(tasks[i].Title, tasks[i].TaskSlug)
					tasks[i].Kind = tasks[i].EffectiveKind()
					tasks[i].Scope = tasks[i].EffectiveScope()
					if len(tasks[i].EventIDs) > 0 {
						bindEventIDs = append([]string(nil), tasks[i].EventIDs...)
					}
					updated = true
					break
				}
			}
		}
		if !updated {
			if inTask.TaskSlug == "" && inTask.ConversationID != "" {
				shortID := inTask.ConversationID
				if len(shortID) > 8 {
					shortID = shortID[:8]
				}
				inTask.TaskSlug = "conv-" + shortID
			}
			if inTask.Title == "" {
				inTask.Title = "[Task:general] Untitled Task"
			}
			inTask.Title = SanitizeTaskTitle(inTask.Title, inTask.TaskSlug)
			inTask.Kind = inTask.EffectiveKind()
			inTask.Scope = inTask.EffectiveScope()
			if inTask.Status == "" {
				inTask.Status = "active"
			}
			if inTask.CreatedAt == "" {
				inTask.CreatedAt = time.Now().Format(time.RFC3339)
			}
			if inTask.LastUpdatedAt == "" {
				inTask.LastUpdatedAt = inTask.CreatedAt
			}
			if (inTask.Status == "completed" || inTask.Status == "cancelled") && inTask.CompletedAt == "" {
				inTask.CompletedAt = inTask.LastUpdatedAt
			}
			tasks = append(tasks, inTask)
		}
		for _, t := range tasks {
			if t.ConversationID != "" && (t.ConversationID == inTask.ConversationID || (inTask.TaskSlug != "" && t.TaskSlug == inTask.TaskSlug)) && IsExplicitTopLevelTask(t) {
				removeNestedIDs = append(removeNestedIDs, t.ConversationID)
			}
		}
		return tasks, true, nil
	})
	if err != nil {
		return err
	}
	if len(bindEventIDs) > 0 && task.ConversationID != "" {
		_ = events.BindBatchConversation(vaultDir, bindEventIDs, task.ConversationID, time.Now(), events.DefaultLeaseTTL)
	}
	for _, cid := range removeNestedIDs {
		_ = RemoveNestedConversation(vaultDir, cid)
	}
	return nil
}

// CompleteTask marks an existing task in local/memory/tasks.json (matched by TaskSlug or ConversationID) as completed.
func CompleteTask(vaultDir, slugOrConvID, summary string) error {
	var completedTask ActiveTask
	err := UpdateTasks(vaultDir, func(tasks []ActiveTask) ([]ActiveTask, bool, error) {
		nowStr := time.Now().Format(time.RFC3339)
		for i := range tasks {
			if tasks[i].TaskSlug == slugOrConvID || tasks[i].ConversationID == slugOrConvID {
				tasks[i].Status = "completed"
				tasks[i].LastUpdatedAt = nowStr
				tasks[i].CompletedAt = nowStr
				if strings.TrimSpace(summary) != "" {
					tasks[i].LastSummary = strings.TrimSpace(summary)
				}
				completedTask = tasks[i]
				return tasks, true, nil
			}
		}
		return tasks, false, fmt.Errorf("task %q not found in local/memory/tasks.json", slugOrConvID)
	})
	if err != nil {
		return err
	}
	if completedTask.EffectiveKind() == "task" && !IsNestedSubagentConversation(vaultDir, completedTask.ConversationID) {
		_ = channels.EnqueueTaskDone(vaultDir, completedTask.TaskSlug, completedTask.Title, completedTask.LastSummary, completedTask.CompletedAt, time.Now())
	}
	return nil
}

// SetTaskPinned pins or unpins a task in local/memory/tasks.json so it stays anchored at the top of the Active board.
func SetTaskPinned(vaultDir, slugOrConvID string, pinned bool) error {
	convID := ""
	err := UpdateTasks(vaultDir, func(tasks []ActiveTask) ([]ActiveTask, bool, error) {
		for i := range tasks {
			if tasks[i].TaskSlug == slugOrConvID || tasks[i].ConversationID == slugOrConvID {
				tasks[i].Pinned = pinned
				tasks[i].LastUpdatedAt = time.Now().Format(time.RFC3339)
				convID = tasks[i].ConversationID
				return tasks, true, nil
			}
		}
		return tasks, false, fmt.Errorf("task %q not found in local/memory/tasks.json", slugOrConvID)
	})
	if err != nil {
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
	// WorkspaceNote, when set, receives one human-readable line per provider workspace decision (removed, kept
	// with open changes plus the `hq propose --type=workspace_remove` command to stage, unbound, ...).
	WorkspaceNote func(note string)
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
//
// Provider workspace bindings (RFC-0025 §8) are decided with a fresh provider status before anything is removed:
// a dirty binding refuses the archive unless opts.Force; owned workspaces with no open changes are removed (both
// preconditions re-checked by the provider); owned workspaces with open changes are kept, unbound, and marked
// orphaned_open_changes (never force-deleted); attached workspaces are only unbound.
func syncTaskSidebarFields(vaultDir string, target ActiveTask) {
	_ = UpdateTasks(vaultDir, func(cur []ActiveTask) ([]ActiveTask, bool, error) {
		for i := range cur {
			if (target.ConversationID != "" && cur[i].ConversationID == target.ConversationID) || cur[i].TaskSlug == target.TaskSlug {
				cur[i].SidebarArchived = target.SidebarArchived
				cur[i].SidebarSyncedAt = target.SidebarSyncedAt
				return cur, true, nil
			}
		}
		return cur, false, nil
	})
}

func ArchiveTaskWithOptions(vaultDir, target string, opts ArchiveTaskOptions) (SidebarSingleSyncResult, error) {
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return SidebarSingleSyncResult{}, err
	}
	matchedIdx := -1
	for i := range tasks {
		if tasks[i].TaskSlug == target || tasks[i].ConversationID == target {
			matchedIdx = i
			break
		}
	}
	if matchedIdx < 0 {
		return SidebarSingleSyncResult{}, fmt.Errorf("task %q not found in local/memory/tasks.json", target)
	}
	cand := tasks[matchedIdx]
	if cand.Pinned && !opts.Force {
		return SidebarSingleSyncResult{}, fmt.Errorf("refusing to archive %q: task is pinned (unpin first or pass --force)", target)
	}
	if err := validateTaskWorktreesForArchive(vaultDir, target, cand, opts.Force); err != nil {
		return SidebarSingleSyncResult{}, err
	}
	wsSteps, err := planTaskWorkspaceArchive(vaultDir, target, cand, opts.Force, time.Now())
	if err != nil {
		return SidebarSingleSyncResult{}, err
	}
	if err := removeTaskWorktrees(vaultDir, &cand, opts.Force, true); err != nil {
		return SidebarSingleSyncResult{}, err
	}
	for _, note := range executeTaskWorkspaceArchive(vaultDir, cand.TaskSlug, wsSteps) {
		if opts.WorkspaceNote != nil {
			opts.WorkspaceNote(note)
		}
	}

	nowStr := time.Now().Format(time.RFC3339)
	var archivedTask ActiveTask
	if err := UpdateTasks(vaultDir, func(cur []ActiveTask) ([]ActiveTask, bool, error) {
		for i := range cur {
			if cur[i].TaskSlug == target || cur[i].ConversationID == target {
				cur[i].WorktreeDir = cand.WorktreeDir
				cur[i].WorktreeDirs = cand.WorktreeDirs
				cur[i].Status = "archived"
				cur[i].LastUpdatedAt = nowStr
				cur[i].ArchivedAt = nowStr
				archivedTask = cur[i]
				return cur, true, nil
			}
		}
		return cur, false, fmt.Errorf("task %q not found in local/memory/tasks.json", target)
	}); err != nil {
		return SidebarSingleSyncResult{}, err
	}

	if opts.NoSidebar {
		return SidebarSingleSyncResult{SkippedReason: "no_sidebar"}, nil
	}
	syncRes := syncSingleTaskArchiveState(context.Background(), vaultDir, &archivedTask, true, true, opts.Force, opts.SidebarConfig)
	if syncRes.Synced || syncRes.AlreadySynced {
		syncTaskSidebarFields(vaultDir, archivedTask)
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
	// WorkspaceNote, when set, receives one human-readable line per provider workspace binding that could not be
	// rebound as-is (re-created, dropped, or rebound without a fresh status).
	WorkspaceNote func(note string)
}

// ReopenTaskWithOptions restores a task back to active status and optionally restores its git worktree and sidebar state.
func ReopenTaskWithOptions(vaultDir, target string, opts ReopenTaskOptions) (SidebarSingleSyncResult, error) {
	hotSnap, err := ListTasks(vaultDir)
	if err != nil {
		return SidebarSingleSyncResult{}, err
	}
	var wtTarget *ActiveTask
	for i := range hotSnap {
		if hotSnap[i].TaskSlug == target || hotSnap[i].ConversationID == target {
			cp := hotSnap[i]
			wtTarget = &cp
			break
		}
	}
	if wtTarget == nil {
		if archivedSnap, _, archErr := ListArchivedTasks(vaultDir, 0); archErr == nil {
			for i := len(archivedSnap) - 1; i >= 0; i-- {
				if archivedSnap[i].TaskSlug == target || archivedSnap[i].ConversationID == target {
					cp := archivedSnap[i]
					wtTarget = &cp
					break
				}
			}
		}
	}
	if wtTarget == nil {
		return SidebarSingleSyncResult{}, fmt.Errorf("task %q not found in local/memory/tasks.json", target)
	}
	if !opts.NoWorktree {
		if err := restoreWorktreeForReopen(vaultDir, wtTarget); err != nil {
			return SidebarSingleSyncResult{}, err
		}
	}

	nowStr := time.Now().Format(time.RFC3339)
	var reopenedTask ActiveTask
	if err := withTasksLock(vaultDir, func() error {
		tasks, lErr := ListTasks(vaultDir)
		if lErr != nil {
			return lErr
		}
		for i := range tasks {
			if tasks[i].TaskSlug == target || tasks[i].ConversationID == target {
				tasks[i].Status = "active"
				tasks[i].ArchivedAt = ""
				tasks[i].SidebarArchived = false
				tasks[i].LastUpdatedAt = nowStr
				reopenedTask = tasks[i]
				return saveTasksUnlocked(vaultDir, tasks)
			}
		}
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
				recalled.Status = "active"
				recalled.ArchivedAt = ""
				recalled.SidebarArchived = false
				recalled.LastUpdatedAt = nowStr
				tasks = append(tasks, *recalled)
				reopenedTask = *recalled
				var b strings.Builder
				for _, rem := range remaining {
					if line, mErr := json.Marshal(rem); mErr == nil {
						b.Write(line)
						b.WriteByte('\n')
					}
				}
				if err := store.WriteAtomic(tasksArchiveFilePath(vaultDir), []byte(b.String()), 0o644); err != nil {
					return err
				}
				return saveTasksUnlocked(vaultDir, tasks)
			}
		}
		return fmt.Errorf("task %q not found in local/memory/tasks.json", target)
	}); err != nil {
		return SidebarSingleSyncResult{}, err
	}

	if reopenedTask.ConversationID != "" && IsExplicitTopLevelTask(reopenedTask) {
		_ = RemoveNestedConversation(vaultDir, reopenedTask.ConversationID)
	}

	for _, note := range restoreTaskWorkspaces(vaultDir, reopenedTask, opts.NoWorktree, time.Now()) {
		if opts.WorkspaceNote != nil {
			opts.WorkspaceNote(note)
		}
	}

	if opts.NoSidebar {
		RemoveSidebarSyncQueueItem(vaultDir, reopenedTask.ConversationID, time.Now())
		return SidebarSingleSyncResult{SkippedReason: "no_sidebar"}, nil
	}
	syncRes := syncSingleTaskArchiveState(context.Background(), vaultDir, &reopenedTask, false, false, false, opts.SidebarConfig)
	if syncRes.Synced || syncRes.AlreadySynced {
		syncTaskSidebarFields(vaultDir, reopenedTask)
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
	PrunedWorkspaces    []string `json:"prunedWorkspaces,omitempty"`
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
			rep.PrunedWorkspaces = pruneOrphanWorkspacesForCompaction(vaultDir, opts)
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
	// Provider workspace bindings (RFC-0025 §8) are judged from the daemon-refreshed status cache only and fail
	// closed on a stale, errored, or never-refreshed cache.
	wsCompaction := &workspaceCompaction{vaultDir: vaultDir, now: opts.Now}
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
		if kind == "task" && (st == "completed" || st == "cancelled") && !t.Pinned && age >= opts.TaskAutoArchiveAfter && areAllTaskWorktreesCleanOrAbsent(vaultDir, *t) && wsCompaction.eligible(*t, false) {
			shouldArchive = true
		} else if kind == "task" && (st == "active" || st == "idle") && !t.Pinned && age >= opts.IdleDormantArchiveAfter && areAllTaskWorktreesMergedAndClean(vaultDir, *t) && wsCompaction.eligible(*t, true) {
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
				_ = wsCompaction.release(*t)
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
			if !opts.DryRun && st != "archived" {
				// Completed subagents/auto workers rotate cold without passing through archive: release their
				// provider workspaces from the cache (kept and unbound unless clean and fully landed).
				_ = wsCompaction.release(t)
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
	if !opts.DryRun && (len(rotatedCold) > 0 || len(rep.AutoArchivedSlugs) > 0 || autoPinnedCompleted > 0) {
		taskIdentity := func(t ActiveTask) string {
			if t.ConversationID != "" {
				return "cid:" + t.ConversationID
			}
			return "slug:" + t.TaskSlug
		}
		rotatedKeys := make(map[string]bool, len(rotatedCold))
		for _, t := range rotatedCold {
			rotatedKeys[taskIdentity(t)] = true
		}
		plannedHotByKey := make(map[string]ActiveTask, len(keptHot))
		for _, t := range keptHot {
			plannedHotByKey[taskIdentity(t)] = t
		}
		if err := withTasksLock(vaultDir, func() error {
			if len(rotatedCold) > 0 {
				if err := appendTasksToColdArchiveUnlocked(vaultDir, rotatedCold); err != nil {
					return err
				}
			}
			curTasks, err := ListTasks(vaultDir)
			if err != nil {
				return err
			}
			merged := make([]ActiveTask, 0, len(curTasks))
			for _, cur := range curTasks {
				k := taskIdentity(cur)
				if rotatedKeys[k] {
					continue
				}
				if planned, ok := plannedHotByKey[k]; ok {
					cur.Status = planned.Status
					cur.CompletedAt = planned.CompletedAt
					cur.ArchivedAt = planned.ArchivedAt
					cur.LastUpdatedAt = planned.LastUpdatedAt
					cur.WorktreeDir = planned.WorktreeDir
					cur.WorktreeDirs = planned.WorktreeDirs
				}
				merged = append(merged, cur)
			}
			rep.HotAfter = len(merged)
			return saveTasksUnlocked(vaultDir, merged)
		}); err != nil {
			return rep, err
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
		rep.PrunedWorkspaces = pruneOrphanWorkspacesForCompaction(vaultDir, opts)
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

	combined = FilterLayer1Tasks(vaultDir, combined)
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
	targetSet := make(map[string]bool, len(conversationIDs))
	for _, id := range conversationIDs {
		targetSet[id] = true
	}
	return UpdateTasks(vaultDir, func(tasks []ActiveTask) ([]ActiveTask, bool, error) {
		changed := false
		nowStr := time.Now().Format(time.RFC3339)
		for i := range tasks {
			if targetSet[tasks[i].ConversationID] || targetSet[tasks[i].TaskSlug] {
				tasks[i].Status = "cancelled"
				tasks[i].LastUpdatedAt = nowStr
				if strings.TrimSpace(reason) != "" {
					tasks[i].LastSummary = strings.TrimSpace(reason)
				}
				changed = true
			}
		}
		return tasks, changed, nil
	})
}

// CancelAllSubagentTasks marks all active subagent tasks as cancelled.
func CancelAllSubagentTasks(vaultDir string, reason string) error {
	return UpdateTasks(vaultDir, func(tasks []ActiveTask) ([]ActiveTask, bool, error) {
		changed := false
		nowStr := time.Now().Format(time.RFC3339)
		for i := range tasks {
			if tasks[i].Status == "active" && (strings.HasPrefix(tasks[i].TaskSlug, "subagent-") || strings.HasPrefix(tasks[i].Title, "[Subagent:")) {
				tasks[i].Status = "cancelled"
				tasks[i].LastUpdatedAt = nowStr
				if strings.TrimSpace(reason) != "" {
					tasks[i].LastSummary = strings.TrimSpace(reason)
				}
				changed = true
			}
		}
		return tasks, changed, nil
	})
}

var (
	reTaxForm   = regexp.MustCompile(`(?i)(w-?2|1099|1098|k-?1|tax)`)
	reSecFiling = regexp.MustCompile(`(?i)(10-[kq]|8-k|earnings)`)
	reUUID      = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
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
//   - standard (gemini-4-neon-medium, flash): feature coding, unit tests, bug fixes, doc searches
//   - heavy / sweep / xhigh (gemini-4-neon-high, flash): exhaustive multi-file refactors, hard E2E/test debugging
//   - deep-medium / consolidation (gemini-4-neon-high, pro): nightly sleep consolidation, deep research memos
//   - deep (gemini-4-carbon-high, pro): Phase 1 Architecture/RFC design, security/concurrency audits, thesis falsification
//
// Verified Antigravity model IDs (`agy models`):
//   - gemini-4-neon-{low,medium,high,xhigh}
//   - gemini-4-hydrogen-{low,medium,high,xhigh}
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

// HeartbeatOptions configures optional behavior of RunHeartbeatWithOptions.
type HeartbeatOptions struct {
	// SkipWorkspaceMaintenance skips synchronous workspace probe/refresh maintenance when the caller
	// (such as `hq daemon`) already runs it on a dedicated non-blocking single-flight goroutine.
	SkipWorkspaceMaintenance bool
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
	return RunHeartbeatWithOptions(vaultDir, now, dryRun, HeartbeatOptions{})
}

// RunHeartbeatWithOptions is RunHeartbeatAt with explicit HeartbeatOptions.
func RunHeartbeatWithOptions(vaultDir string, now time.Time, dryRun bool, opts HeartbeatOptions) (string, error) {
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
	if !dryRun && HasReachableLSSession(vaultDir) {
		_, _ = CleanNestedTasks(vaultDir)
	}

	if !dryRun && !opts.SkipWorkspaceMaintenance {
		// RFC-0025 provider workspace maintenance (probe drain, cadence-gated status refresh, landed notices).
		// It is a no-op without a workspace ledger and bounded so standalone `hq heartbeat` cannot hang.
		wsCtx, wsCancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, _ = RunWorkspaceMaintenanceContext(wsCtx, vaultDir, now)
		wsCancel()
	}

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
