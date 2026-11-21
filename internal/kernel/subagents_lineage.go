package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/store"
)

var (
	reRoleWithTierScope    = regexp.MustCompile(`^\[([^\]]+)\]\s*([^:]+):\s*(.*)$`)
	reRoleAlreadyNorm      = regexp.MustCompile(`^\[Subagent:([^\]]+)\]\s*([^\[]+?)(?:\s*\[([^\]]+)\])?$`)
	reRoleScopeOnly        = regexp.MustCompile(`^([^:]+):\s*(.*)$`)
	reNonAlnum             = regexp.MustCompile(`[^a-z0-9]+`)
	reQuotaSubagentOutcome = regexp.MustCompile(`Spawned subagent ([0-9a-fA-F-]+) \(parent=([0-9a-fA-F-]+)`)
)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = reNonAlnum.ReplaceAllString(s, "-")
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

func subagentQueueLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", ".subagents.lock")
}

func withSubagentQueueLock(vaultDir string, fn func() error) error {
	return store.With(context.Background(), store.RankSubagentQueue, subagentQueueLockPath(vaultDir), func(_ context.Context) error {
		return fn()
	})
}

func pendingSubagentsFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "pending_subagents.json")
}

func loadPendingSubagentFile(filePath string) ([]PendingSubagent, error) {
	data, err := os.ReadFile(filePath)
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

func savePendingSubagentFile(filePath string, items []PendingSubagent) error {
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(filePath, data, 0o644)
}

func queuePendingSubagentFile(vaultDir, filePath, parentConvID, role, model, prompt string) (PendingSubagent, error) {
	tier, scope, _, title, slug := ParseSubagentRole(role, model)
	promptPrefix := truncateRunes(prompt, 200)
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

	var outItem PendingSubagent
	err := withSubagentQueueLock(vaultDir, func() error {
		queue, _ := loadPendingSubagentFile(filePath)
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
				outItem = last
				return nil
			}
		}
		active = append(active, item)
		outItem = item
		return savePendingSubagentFile(filePath, active)
	})
	return outItem, err
}

func popPendingSubagentFile(vaultDir, filePath, callerConvID, callerPrompt string, maxAge time.Duration) (PendingSubagent, bool) {
	var matched PendingSubagent
	found := false
	_ = withSubagentQueueLock(vaultDir, func() error {
		queue, err := loadPendingSubagentFile(filePath)
		if err != nil || len(queue) == 0 {
			return nil
		}
		callerConvID = strings.TrimSpace(callerConvID)
		cutoff := time.Now().Add(-maxAge)
		var remaining []PendingSubagent
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
			_ = savePendingSubagentFile(filePath, remaining)
		}
		return nil
	})
	if found {
		return matched, true
	}
	return PendingSubagent{}, false
}

// QueuePendingSubagentWithParent enqueues metadata for an invoked subagent along with its parentConversationId.
func QueuePendingSubagentWithParent(vaultDir, parentConvID, role, model, prompt string) (PendingSubagent, error) {
	return queuePendingSubagentFile(vaultDir, pendingSubagentsFilePath(vaultDir), parentConvID, role, model, prompt)
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

// PopPendingSubagentForCaller pops the oldest matching unclaimed pending subagent within maxAge,
// rejecting self-parenting (`ParentConversationID == callerConvID`) and mismatched prompts when callerPrompt is known.
func PopPendingSubagentForCaller(vaultDir, callerConvID, callerPrompt string, maxAge time.Duration) (PendingSubagent, bool) {
	return popPendingSubagentFile(vaultDir, pendingSubagentsFilePath(vaultDir), callerConvID, callerPrompt, maxAge)
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

// QueuePendingNestedSubagent enqueues metadata for a nested child subagent invoked from a subagent or secondary task.
func QueuePendingNestedSubagent(vaultDir, parentConvID, role, model, prompt string) (PendingSubagent, error) {
	return queuePendingSubagentFile(vaultDir, pendingNestedSubagentsFilePath(vaultDir), parentConvID, role, model, prompt)
}

// PopPendingNestedSubagentForCaller pops the oldest matching pending nested subagent within maxAge,
// rejecting self-parenting (`ParentConversationID == callerConvID`) and mismatched prompts when callerPrompt is known.
func PopPendingNestedSubagentForCaller(vaultDir, callerConvID, callerPrompt string, maxAge time.Duration) (PendingSubagent, bool) {
	return popPendingSubagentFile(vaultDir, pendingNestedSubagentsFilePath(vaultDir), callerConvID, callerPrompt, maxAge)
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

func saveNestedConversationsUnlocked(vaultDir string, m map[string]NestedConversation) error {
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
	return store.WriteAtomic(nestedConversationsFilePath(vaultDir), data, 0o644)
}

// RecordNestedConversation records a conversation ID as a nested descendant subagent.
func RecordNestedConversation(vaultDir, convID, parentConvID string) error {
	convID = strings.TrimSpace(convID)
	parentConvID = strings.TrimSpace(parentConvID)
	if convID == "" || (parentConvID != "" && convID == parentConvID) {
		return nil
	}
	return withSubagentQueueLock(vaultDir, func() error {
		m, _ := loadNestedConversations(vaultDir)
		if m == nil {
			m = make(map[string]NestedConversation)
		}
		m[convID] = NestedConversation{
			ConversationID:       convID,
			ParentConversationID: parentConvID,
			CreatedAt:            time.Now().Format(time.RFC3339),
		}
		return saveNestedConversationsUnlocked(vaultDir, m)
	})
}

// RemoveNestedConversation deletes any stale nested-subagent classification for convID
// (used when registering, spawning, or adopting a top-level Kind="task" conversation).
func RemoveNestedConversation(vaultDir, convID string) error {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return nil
	}
	return withSubagentQueueLock(vaultDir, func() error {
		m, err := loadNestedConversations(vaultDir)
		if err != nil || len(m) == 0 {
			return nil
		}
		if _, exists := m[convID]; !exists {
			return nil
		}
		delete(m, convID)
		return saveNestedConversationsUnlocked(vaultDir, m)
	})
}

// GetNestedConversationParent returns the recorded parentConversationId for a nested conversation, if any.
func GetNestedConversationParent(vaultDir, convID string) string {
	convID = normalizeUUID(convID)
	if convID == "" {
		return ""
	}
	m, err := loadNestedConversations(vaultDir)
	if err != nil || m == nil {
		return ""
	}
	if rec, ok := m[convID]; ok {
		return strings.TrimSpace(rec.ParentConversationID)
	}
	for k, rec := range m {
		if normalizeUUID(k) == convID {
			return strings.TrimSpace(rec.ParentConversationID)
		}
	}
	return ""
}

// ActiveSpawner tracks a conversation that recently launched child workers via run_workflow or raw agentapi
// so HandleSessionStart / HandleStop can check its transcript before auto-adopting unknown child conversations.
type ActiveSpawner struct {
	ConversationID string `json:"conversationId"`
	TranscriptPath string `json:"transcriptPath,omitempty"`
	Kind           string `json:"kind"` // "agentapi" | "run_workflow"
	UpdatedAt      string `json:"updatedAt"`
}

func activeSpawnersFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "active_spawners.json")
}

// RecordActiveSpawner registers convID as a recent spawner of agentapi or run_workflow child workers.
func RecordActiveSpawner(vaultDir, convID, transcriptPath, kind string) error {
	convID = normalizeUUID(convID)
	transcriptPath = strings.TrimSpace(transcriptPath)
	kind = strings.ToLower(strings.TrimSpace(kind))
	if convID == "" || kind == "" {
		return nil
	}
	return withSubagentQueueLock(vaultDir, func() error {
		var items []ActiveSpawner
		if data, err := os.ReadFile(activeSpawnersFilePath(vaultDir)); err == nil {
			_ = json.Unmarshal(data, &items)
		}
		cutoff := time.Now().Add(-24 * time.Hour)
		nowISO := time.Now().Format(time.RFC3339)
		kept := make([]ActiveSpawner, 0, len(items)+1)
		for _, it := range items {
			if normalizeUUID(it.ConversationID) == convID && it.Kind == kind {
				continue
			}
			if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(it.UpdatedAt)); err == nil && ts.Before(cutoff) {
				continue
			}
			kept = append(kept, it)
		}
		kept = append(kept, ActiveSpawner{
			ConversationID: convID,
			TranscriptPath: transcriptPath,
			Kind:           kind,
			UpdatedAt:      nowISO,
		})
		if len(kept) > 100 {
			kept = kept[len(kept)-100:]
		}
		data, err := json.MarshalIndent(kept, "", "  ")
		if err != nil {
			return err
		}
		return store.WriteAtomic(activeSpawnersFilePath(vaultDir), data, 0o644)
	})
}

// ListActiveSpawners returns recent active spawners within maxAge (newest first).
func ListActiveSpawners(vaultDir string, maxAge time.Duration) []ActiveSpawner {
	data, err := os.ReadFile(activeSpawnersFilePath(vaultDir))
	if err != nil {
		return nil
	}
	var items []ActiveSpawner
	if json.Unmarshal(data, &items) != nil {
		return nil
	}
	cutoff := time.Time{}
	if maxAge > 0 {
		cutoff = time.Now().Add(-maxAge)
	}
	var out []ActiveSpawner
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if !cutoff.IsZero() {
			if ts, tErr := time.Parse(time.RFC3339, strings.TrimSpace(it.UpdatedAt)); tErr == nil && ts.Before(cutoff) {
				continue
			}
		}
		out = append(out, it)
	}
	return out
}

// LoadNestedConversationParents reads local/memory/nested_conversations.json once and returns a
// normalized map of nested conversationID -> parentConversationID (empty string when parent unknown).
// Callers iterating over many conversations (such as observability DAG builders) should use this
// batch snapshot rather than calling IsNestedSubagentConversation in a loop.
func LoadNestedConversationParents(vaultDir string) map[string]string {
	m, err := loadNestedConversations(vaultDir)
	if err != nil || len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, rec := range m {
		nk := normalizeUUID(k)
		if nk == "" {
			continue
		}
		out[nk] = normalizeUUID(rec.ParentConversationID)
	}
	return out
}

// IsNestedSubagentConversation returns true if convID is a known nested descendant subagent.
func IsNestedSubagentConversation(vaultDir, convID string) bool {
	convID = normalizeUUID(convID)
	if convID == "" {
		return false
	}
	m, err := loadNestedConversations(vaultDir)
	if err == nil && m != nil {
		if _, ok := m[convID]; ok {
			return true
		}
		for k := range m {
			if normalizeUUID(k) == convID {
				return true
			}
		}
	}
	return false
}

func buildAncestryMaps(vaultDir string) (fdID string, knownFDs map[string]bool, nestedMap map[string]NestedConversation, parentByConv map[string]string) {
	if fd, hasFD := GetFrontDesk(vaultDir); hasFD {
		fdID = normalizeUUID(fd.ConversationID)
	}
	knownFDs = GetKnownFrontDeskIDSet(vaultDir)
	parentByConv = make(map[string]string)
	if m, err := loadNestedConversations(vaultDir); err == nil && m != nil {
		nestedMap = m
		for k, rec := range m {
			nk := normalizeUUID(k)
			np := normalizeUUID(rec.ParentConversationID)
			if nk != "" && np != "" && nk != np {
				parentByConv[nk] = np
			}
		}
	} else {
		nestedMap = make(map[string]NestedConversation)
	}
	if tasks, err := ListTasks(vaultDir); err == nil {
		for _, t := range tasks {
			tcid := normalizeUUID(t.ConversationID)
			tpid := normalizeUUID(t.ParentConversationID)
			if tcid != "" && tpid != "" && tcid != tpid {
				if _, exists := parentByConv[tcid]; !exists {
					parentByConv[tcid] = tpid
				}
			}
		}
	}
	return fdID, knownFDs, nestedMap, parentByConv
}

func hasNonFrontDeskAncestryWithMaps(knownFDs map[string]bool, nestedMap map[string]NestedConversation, parentByConv map[string]string, cid, immediateParentID string) (bool, string) {
	cid = normalizeUUID(cid)
	if cid != "" && knownFDs[cid] {
		return false, ""
	}
	if cid != "" {
		if rec, ok := nestedMap[cid]; ok {
			return true, strings.TrimSpace(rec.ParentConversationID)
		}
	}
	cur := normalizeUUID(immediateParentID)
	if cur == "" && cid != "" {
		cur = parentByConv[cid]
	}
	if cur == "" || cur == cid {
		return false, ""
	}
	seen := map[string]bool{cid: true}
	firstParent := cur
	for cur != "" {
		if knownFDs[cur] {
			return false, cur
		}
		if len(knownFDs) == 0 {
			return true, firstParent
		}
		next, hasParent := parentByConv[cur]
		if !hasParent || next == "" || seen[next] {
			// Reached a root parent that is NOT any known Front Desk conversation.
			return true, firstParent
		}
		seen[cur] = true
		cur = next
	}
	return false, firstParent
}

// HasNonFrontDeskAncestry returns true if convID (or its immediateParentID) is a nested descendant
// or has a transitive parent chain whose root is not a known Front Desk conversation.
func HasNonFrontDeskAncestry(vaultDir, convID, immediateParentID string) (bool, string) {
	_, knownFDs, nestedMap, parentByConv := buildAncestryMaps(vaultDir)
	return hasNonFrontDeskAncestryWithMaps(knownFDs, nestedMap, parentByConv, convID, immediateParentID)
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
		if IsExplicitTopLevelTask(task) {
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

// IsLayer1Task returns true if task is a Layer-1 task orchestrated by the Front Desk.
func IsLayer1Task(task ActiveTask, frontDeskConvID string) bool {
	knownFDs := make(map[string]bool)
	if nid := normalizeUUID(frontDeskConvID); nid != "" {
		knownFDs[nid] = true
	}
	return isLayer1TaskWithFDSet(task, knownFDs, true)
}

func isLayer1TaskWithFDSet(task ActiveTask, knownFDs map[string]bool, strictWhenNoFD bool) bool {
	if IsExplicitTopLevelTask(task) {
		return true
	}
	parentID := normalizeUUID(task.ParentConversationID)
	hasSubagentMarkers := strings.HasPrefix(task.TaskSlug, "subagent-") || strings.HasPrefix(task.Title, "[Subagent:")

	if parentID != "" {
		if knownFDs[parentID] {
			return true
		}
		if len(knownFDs) > 0 || strictWhenNoFD {
			return false
		}
		// Vacant Front Desk with no recorded history yet: keep Kind=="subagent" entries unless
		// nested ancestry proves non-FD parentage; reject Kind=="task" with non-empty ParentConversationID.
		return strings.EqualFold(strings.TrimSpace(task.Kind), "subagent")
	}
	// Reject Kind=="task" entries carrying subagent markers without a Front Desk parent.
	if strings.EqualFold(strings.TrimSpace(task.Kind), "task") && hasSubagentMarkers {
		return false
	}
	if hasSubagentMarkers {
		return true
	}
	kind := task.EffectiveKind()
	if kind == "task" || kind == "auto" || kind == "subagent" {
		return true
	}
	return true
}

// FilterLayer1Tasks filters tasks to strictly include Layer-1 tasks in O(N) single disk load.
func FilterLayer1Tasks(vaultDir string, tasks []ActiveTask) []ActiveTask {
	_, knownFDs, nestedMap, parentByConv := buildAncestryMaps(vaultDir)
	var out []ActiveTask
	for _, t := range tasks {
		if !IsExplicitTopLevelTask(t) && t.ConversationID != "" {
			cid := normalizeUUID(t.ConversationID)
			if _, isNested := nestedMap[cid]; isNested {
				continue
			}
			if hasAnc, _ := hasNonFrontDeskAncestryWithMaps(knownFDs, nestedMap, parentByConv, cid, t.ParentConversationID); hasAnc {
				continue
			}
		}
		if isLayer1TaskWithFDSet(t, knownFDs, len(knownFDs) > 0) {
			out = append(out, t)
		}
	}
	return out
}

// CleanNestedTasks archives or removes nested child subagents, workflow workers, and agentapi child spawns
// from local/memory/tasks.json, moving them to tasks_archive.jsonl, scrubbing poisoned top-level task/self-parented IDs
// from nested_conversations.json, and adjusting quota so that only Layer-1 tasks are tracked.
func CleanNestedTasks(vaultDir string) ([]string, error) {
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		return nil, err
	}
	fdID, knownFDs, nestedMap, parentByConv := buildAncestryMaps(vaultDir)
	// Also backfill known historical Front Desks from archived headquarter-front-desk-* records.
	if archived, _, aErr := ListArchivedTasks(vaultDir, 0); aErr == nil {
		for _, at := range archived {
			if strings.HasPrefix(at.TaskSlug, "headquarter-front-desk-") && strings.TrimSpace(at.ConversationID) != "" {
				cid := normalizeUUID(at.ConversationID)
				if !knownFDs[cid] {
					knownFDs[cid] = true
					recordHistoricalFrontDeskID(vaultDir, cid)
				}
			}
		}
	}

	explicitTaskIDs := make(map[string]bool)
	for _, t := range tasks {
		if t.ConversationID != "" && IsExplicitTopLevelTask(t) {
			explicitTaskIDs[normalizeUUID(t.ConversationID)] = true
		}
	}
	_ = withSubagentQueueLock(vaultDir, func() error {
		curNested, nErr := loadNestedConversations(vaultDir)
		if nErr != nil || len(curNested) == 0 {
			return nil
		}
		scrubbed := false
		for cid, rec := range curNested {
			ncid := normalizeUUID(cid)
			npid := normalizeUUID(rec.ParentConversationID)
			if explicitTaskIDs[ncid] || knownFDs[ncid] || (npid != "" && ncid == npid) {
				delete(curNested, cid)
				delete(nestedMap, ncid)
				scrubbed = true
			}
		}
		if scrubbed {
			return saveNestedConversationsUnlocked(vaultDir, curNested)
		}
		return nil
	})

	// Pre-seed known nested conversations from quota events
	type nestedPendingRec struct {
		convID   string
		parentID string
	}
	var toRecordNested []nestedPendingRec

	quota := LoadQuota(vaultDir)
	for _, ev := range quota.Events {
		if strings.Contains(ev.Outcome, "parent=") {
			if m := reQuotaSubagentOutcome.FindStringSubmatch(ev.Outcome); len(m) == 3 {
				cid := normalizeUUID(m[1])
				pid := normalizeUUID(m[2])
				if pid != "" && !knownFDs[pid] && len(knownFDs) > 0 && !explicitTaskIDs[cid] {
					if _, exists := nestedMap[cid]; !exists {
						nestedMap[cid] = NestedConversation{ConversationID: cid, ParentConversationID: pid}
						toRecordNested = append(toRecordNested, nestedPendingRec{convID: cid, parentID: pid})
					}
				}
			}
		}
	}

	// Pre-seed from live Language Server trajectory metadata if online (catches run_workflow & subtrajectories).
	lsLineageByID, hasLS := fetchAllLSConversationLineageWithTimeout(vaultDir, 10*time.Second)
	var fallbackCreds lsCredentials
	hasFallbackCreds := false
	if hasLS {
		fallbackCreds, hasFallbackCreds = discoverLSCredentials(vaultDir)
	}
	missingProbes := 0
	const maxMissingLineageProbes = 16
	if hasLS {
		for _, t := range tasks {
			cid := normalizeUUID(t.ConversationID)
			if cid == "" || explicitTaskIDs[cid] {
				continue
			}
			lin, ok := lsLineageByID[cid]
			if !ok && hasFallbackCreds && missingProbes < maxMissingLineageProbes && strings.HasPrefix(t.TaskSlug, "conv-") {
				missingProbes++
				if resp, supported, tErr := inspectCascadeTrajectory(fallbackCreds.BaseURL, fallbackCreds.CSRFToken, t.ConversationID, DefaultSidebarUnaryTimeout); tErr == nil && supported && resp != nil {
					var meta LSTrajectoryMetadata
					if resp.Trajectory.Metadata != nil {
						meta = *resp.Trajectory.Metadata
					}
					lin = evaluateLSTrajectoryLineageWithMaps(fdID, knownFDs, nestedMap, parentByConv, cid, meta)
					ok = true
				}
			}
			if ok && lin.IsNestedDescendant {
				if _, exists := nestedMap[cid]; !exists {
					nestedMap[cid] = NestedConversation{ConversationID: t.ConversationID, ParentConversationID: lin.DirectParentID}
					toRecordNested = append(toRecordNested, nestedPendingRec{convID: t.ConversationID, parentID: lin.DirectParentID})
				}
			}
		}
	}

	var removedTasks []ActiveTask
	var removedSlugs []string
	var removedKeys []string
	removedTodaySubagentCount := 0
	now := time.Now()
	nowStr := now.Format(time.RFC3339)
	todayLocal := now.In(time.Local).Format("2006-01-02")

	taskIdentity := func(t ActiveTask) string {
		if cid := normalizeUUID(t.ConversationID); cid != "" {
			return "cid:" + cid
		}
		return "slug:" + t.TaskSlug
	}

	isCreatedToday := func(createdAt string) bool {
		if strings.TrimSpace(createdAt) == "" {
			return true
		}
		if ts := parseFlexibleRFC3339(createdAt); !ts.IsZero() {
			return ts.In(time.Local).Format("2006-01-02") == todayLocal
		}
		return false
	}

	for _, t := range tasks {
		isNested := false
		discoveredParent := strings.TrimSpace(t.ParentConversationID)
		cid := normalizeUUID(t.ConversationID)

		if !IsExplicitTopLevelTask(t) {
			if rec, ok := nestedMap[cid]; ok && cid != "" {
				isNested = true
				if discoveredParent == "" {
					discoveredParent = strings.TrimSpace(rec.ParentConversationID)
				}
			} else if hasLS {
				if lin, ok := lsLineageByID[cid]; ok && lin.IsNestedDescendant {
					isNested = true
					if discoveredParent == "" {
						discoveredParent = lin.DirectParentID
					}
				}
			}
			if !isNested {
				parentID := normalizeUUID(t.ParentConversationID)
				if parentID != "" && !knownFDs[parentID] && (len(knownFDs) > 0 || !strings.EqualFold(strings.TrimSpace(t.Kind), "subagent")) {
					isNested = true
				} else if !isLayer1TaskWithFDSet(t, knownFDs, len(knownFDs) > 0) {
					isNested = true
				} else if hasAnc, ancParent := hasNonFrontDeskAncestryWithMaps(knownFDs, nestedMap, parentByConv, cid, parentID); hasAnc {
					isNested = true
					if discoveredParent == "" {
						discoveredParent = ancParent
					}
				}
			}
		}

		if isNested {
			if strings.EqualFold(strings.TrimSpace(t.Kind), "subagent") && isCreatedToday(t.CreatedAt) {
				removedTodaySubagentCount++
			}
			if strings.EqualFold(strings.TrimSpace(t.Kind), "task") || t.Kind == "" {
				t.Kind = "subagent"
			}
			if t.ParentConversationID == "" && discoveredParent != "" {
				t.ParentConversationID = discoveredParent
			}
			t.Status = "archived"
			t.ArchivedAt = nowStr
			removedTasks = append(removedTasks, t)
			if t.ConversationID != "" {
				toRecordNested = append(toRecordNested, nestedPendingRec{convID: t.ConversationID, parentID: discoveredParent})
			}
			removedSlugs = append(removedSlugs, t.TaskSlug)
			removedKeys = append(removedKeys, taskIdentity(t))
		}
	}

	_ = fdID // captured via knownFDs

	if len(removedTasks) > 0 {
		removedKeySet := make(map[string]bool, len(removedKeys))
		for _, k := range removedKeys {
			removedKeySet[k] = true
		}
		var verifiedSlugs []string
		if err := withTasksLock(vaultDir, func() error {
			curTasks, err := ListTasks(vaultDir)
			if err != nil {
				return err
			}
			kept := make([]ActiveTask, 0, len(curTasks))
			stillRemovedKeys := make(map[string]bool, len(removedKeySet))
			for _, cur := range curTasks {
				key := taskIdentity(cur)
				// Re-check IsExplicitTopLevelTask under lock in case hq adopt promoted cur concurrently.
				if removedKeySet[key] && !IsExplicitTopLevelTask(cur) {
					stillRemovedKeys[key] = true
					verifiedSlugs = append(verifiedSlugs, cur.TaskSlug)
					continue
				}
				kept = append(kept, cur)
			}
			if len(stillRemovedKeys) == 0 {
				removedSlugs = nil
				return nil
			}
			var confirmedArchive []ActiveTask
			for _, rt := range removedTasks {
				if stillRemovedKeys[taskIdentity(rt)] {
					confirmedArchive = append(confirmedArchive, rt)
				}
			}
			if err := appendTasksToColdArchiveUnlocked(vaultDir, confirmedArchive); err != nil {
				return err
			}
			removedSlugs = verifiedSlugs
			return saveTasksUnlocked(vaultDir, kept)
		}); err != nil {
			return nil, err
		}

		for _, rec := range toRecordNested {
			cidNorm := normalizeUUID(rec.convID)
			if explicitTaskIDs[cidNorm] {
				continue
			}
			_ = RecordNestedConversation(vaultDir, rec.convID, rec.parentID)
		}

		if removedTodaySubagentCount > 0 && len(removedSlugs) > 0 {
			_ = UpdateQuota(vaultDir, func(st *QuotaState) (bool, error) {
				st.normalizeSpawnCounters()
				changed := false
				if st.SpawnsToday >= removedTodaySubagentCount {
					st.SpawnsToday -= removedTodaySubagentCount
					changed = true
				}
				if st.InteractiveSpawnsToday >= removedTodaySubagentCount {
					st.InteractiveSpawnsToday -= removedTodaySubagentCount
					changed = true
				}
				return changed, nil
			})
		}
	} else {
		for _, rec := range toRecordNested {
			_ = RecordNestedConversation(vaultDir, rec.convID, rec.parentID)
		}
	}

	return removedSlugs, nil
}
