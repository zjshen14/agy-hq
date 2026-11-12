package obs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zjshen14/agy-hq/internal/kernel"
)

func enrichAndBuildDAG(vaultDir string, idx *ObsIndex) bool {
	if idx == nil || len(idx.Conversations) == 0 {
		return false
	}

	fd, hasFD := kernel.GetFrontDesk(vaultDir)
	fdID := ""
	if hasFD {
		fdID = strings.ToLower(strings.TrimSpace(fd.ConversationID))
	}

	taskByConv := make(map[string]kernel.ActiveTask)
	if hot, err := kernel.ListTasks(vaultDir); err == nil {
		for _, t := range hot {
			if t.ConversationID != "" {
				taskByConv[strings.ToLower(strings.TrimSpace(t.ConversationID))] = t
			}
		}
	}
	if cold, _, err := kernel.ListArchivedTasks(vaultDir, 0); err == nil {
		for _, t := range cold {
			cid := strings.ToLower(strings.TrimSpace(t.ConversationID))
			if cid != "" {
				if _, exists := taskByConv[cid]; !exists {
					taskByConv[cid] = t
				}
			}
		}
	}

	// 1. Collect ground-truth spawn edges from all transcripts
	edgeByChild := make(map[string]SubagentSpawnEdge)
	childrenByParent := make(map[string][]string)
	childSeen := make(map[string]map[string]bool)

	addChildLink := func(parentID, childID string) {
		parentID = strings.ToLower(strings.TrimSpace(parentID))
		childID = strings.ToLower(strings.TrimSpace(childID))
		if parentID == "" || childID == "" || parentID == childID || (fdID != "" && childID == fdID) {
			return
		}
		if childSeen[parentID] == nil {
			childSeen[parentID] = make(map[string]bool)
		}
		if !childSeen[parentID][childID] {
			childSeen[parentID][childID] = true
			childrenByParent[parentID] = append(childrenByParent[parentID], childID)
		}
	}

	for parentID, cm := range idx.Conversations {
		for _, edge := range cm.SpawnEdges {
			cID := strings.ToLower(strings.TrimSpace(edge.ChildConversationID))
			if cID == "" {
				continue
			}
			prev, exists := edgeByChild[cID]
			if !exists || (prev.Role == "" && edge.Role != "") {
				edge.ParentConversationID = parentID
				edgeByChild[cID] = edge
			}
			addChildLink(parentID, cID)
		}
	}

	// 2. Enrich each conversation's metadata (Ground-truth SpawnEdge > tasks.json > transcript fallback)
	for cid, cm := range idx.Conversations {
		origTitle := cm.Title
		origKind := cm.Kind
		origTier := cm.Tier
		origParent := cm.ParentConversationID

		if fdID != "" && cid == fdID {
			cm.Kind = "front_desk"
			cm.TaskSlug = "front-desk"
			cm.Title = "[HQ] Front Desk"
			cm.Scope = "hq"
			if cm.Model == "" {
				cm.Model = "gemini-4-carbon-high"
			}
			cm.Tier = NormalizeTier("deep", cm.Model)
			cm.Status = "active"
		} else {
			if t, ok := taskByConv[cid]; ok {
				cm.TaskSlug = t.TaskSlug
				cm.Title = t.Title
				cm.Kind = t.EffectiveKind()
				cm.Scope = t.EffectiveScope()
				if t.Tier != "" {
					cm.Tier = t.Tier
				}
				if t.Model != "" && cm.Model == "" {
					cm.Model = t.Model
				}
				if t.ParentConversationID != "" {
					cm.ParentConversationID = strings.ToLower(strings.TrimSpace(t.ParentConversationID))
				}
				cm.Status = t.Status
			}

			// Ground-truth spawn edge overrides FIFO-drifted subagent role/parent
			if edge, hasEdge := edgeByChild[cid]; hasEdge {
				cm.ParentConversationID = edge.ParentConversationID
				cleanRole := strings.Trim(strings.TrimSpace(edge.Role), "\"'")
				cleanModel := strings.Trim(strings.TrimSpace(edge.Model), "\"'")
				if cleanRole != "" {
					eModel := cleanModel
					if eModel == "" || eModel == "inherit" {
						eModel = cm.Model
					}
					tier, scope, _, title, slug := kernel.ParseSubagentRole(cleanRole, eModel)
					cm.Title = title
					cm.TaskSlug = slug
					cm.Scope = scope
					cm.Tier = tier
					if cm.Model == "" && cleanModel != "" && cleanModel != "inherit" {
						cm.Model = cleanModel
					}
				}
				if fdID != "" && edge.ParentConversationID != fdID {
					cm.Kind = "nested_subagent"
				} else if cm.Kind != "task" || cleanRole != "" {
					cm.Kind = "subagent"
				}
			} else if kernel.IsNestedSubagentConversation(vaultDir, cid) {
				cm.Kind = "nested_subagent"
			}

			if cm.ParentConversationID != "" {
				addChildLink(cm.ParentConversationID, cid)
			}
		}

		// Fallbacks for untracked conversations
		if cm.Kind == "" {
			if cm.ParentConversationID != "" {
				if fdID != "" && cm.ParentConversationID != fdID {
					cm.Kind = "nested_subagent"
				} else {
					cm.Kind = "subagent"
				}
			} else {
				cm.Kind = "task"
			}
		}
		if cm.Title == "" {
			preview := ""
			if len(cm.Turns) > 0 {
				preview = cm.Turns[0].PromptPreview
			}
			shortID := cid
			if len(shortID) > 8 {
				shortID = shortID[:8]
			}
			if preview != "" {
				cm.Title = kernel.SanitizeTaskTitle(preview, "conv-"+shortID)
			} else {
				cm.Title = "[Task:general] Conversation " + shortID
			}
			if cm.TaskSlug == "" {
				cm.TaskSlug = "conv-" + shortID
			}
		}
		if cm.Scope == "" {
			cm.Scope = "general"
		}
		if cm.Model == "" {
			switch cm.Tier {
			case "deep":
				cm.Model = "gemini-4-carbon-high"
			case "quick":
				cm.Model = "gemini-4-hydrogen-low"
			default:
				cm.Model = "gemini-4-hydrogen-high"
			}
		}
		cm.Tier = NormalizeTier(cm.Tier, cm.Model)

		// Recompute per-turn and Self QBU/EstCostUSD now that Model and Tier are resolved
		for ti := range cm.Turns {
			t := &cm.Turns[ti]
			t.QuotaUnits, t.EstCostUSD = ComputeQuotaAndCost(t.InputTokens, t.CacheReadTokens, t.OutputTokens, cm.Model, cm.Tier)
		}
		cm.Self = RollupTurns(cm.Turns, cm.Self.StepCount, cm.Model, cm.Tier)
		cm.Tree = cm.Self

		if cm.Title != origTitle || cm.Kind != origKind || cm.Tier != origTier || cm.ParentConversationID != origParent {
			// Metadata updated
		}
		idx.Conversations[cid] = cm
	}

	// 3. Populate ChildConversationIDs, RootConversationID, and compute recursive Tree rollups
	for cid, cm := range idx.Conversations {
		kids := childrenByParent[cid]
		sort.Strings(kids)
		cm.ChildConversationIDs = kids

		// Walk up parent chain to find RootConversationID (with cycle guard)
		root := cid
		visited := map[string]bool{cid: true}
		for {
			cur, ok := idx.Conversations[root]
			if !ok || cur.ParentConversationID == "" || visited[cur.ParentConversationID] {
				break
			}
			visited[cur.ParentConversationID] = true
			root = cur.ParentConversationID
		}
		cm.RootConversationID = root
		idx.Conversations[cid] = cm
	}

	memo := make(map[string]TokenRollup)
	inProgress := make(map[string]bool)
	var computeTree func(cid string) TokenRollup
	computeTree = func(cid string) TokenRollup {
		if r, ok := memo[cid]; ok {
			return r
		}
		cm, ok := idx.Conversations[cid]
		if !ok {
			return TokenRollup{}
		}
		if inProgress[cid] {
			return cm.Self
		}
		inProgress[cid] = true
		agg := cm.Self
		for _, childID := range cm.ChildConversationIDs {
			if _, exists := idx.Conversations[childID]; exists {
				childRollup := computeTree(childID)
				agg = AddRollup(agg, childRollup)
			}
		}
		delete(inProgress, cid)
		memo[cid] = agg
		return agg
	}

	for cid, cm := range idx.Conversations {
		cm.Tree = computeTree(cid)
		idx.Conversations[cid] = cm
	}

	return true
}

// ReconcileSubagentAttribution repairs any FIFO-drifted subagent Title, TaskSlug, Scope, Tier,
// and ParentConversationID in local/memory/tasks.json using ground-truth invoke_subagent edges
// extracted from parent transcripts.
func ReconcileSubagentAttribution(vaultDir string) (int, error) {
	idx, err := LoadOrRefreshIndex(vaultDir)
	if err != nil {
		return 0, err
	}
	tasks, err := kernel.ListTasks(vaultDir)
	if err != nil || len(tasks) == 0 {
		return 0, err
	}

	edgeByChild := make(map[string]SubagentSpawnEdge)
	for parentID, cm := range idx.Conversations {
		for _, edge := range cm.SpawnEdges {
			cID := strings.ToLower(strings.TrimSpace(edge.ChildConversationID))
			if cID != "" && edge.Role != "" {
				edge.ParentConversationID = parentID
				edgeByChild[cID] = edge
			}
		}
	}
	if len(edgeByChild) == 0 {
		return 0, nil
	}

	repaired := 0
	for i := range tasks {
		cid := strings.ToLower(strings.TrimSpace(tasks[i].ConversationID))
		edge, ok := edgeByChild[cid]
		cleanRole := strings.Trim(strings.TrimSpace(edge.Role), "\"'")
		cleanModel := strings.Trim(strings.TrimSpace(edge.Model), "\"'")
		if !ok || cleanRole == "" {
			continue
		}
		model := tasks[i].Model
		if cleanModel != "" && cleanModel != "inherit" {
			model = cleanModel
		}
		tier, scope, _, title, slug := kernel.ParseSubagentRole(cleanRole, model)
		changed := false
		if tasks[i].Title != title {
			tasks[i].Title = title
			changed = true
		}
		if tasks[i].TaskSlug != slug {
			tasks[i].TaskSlug = slug
			changed = true
		}
		if tasks[i].Scope != scope {
			tasks[i].Scope = scope
			changed = true
		}
		if tasks[i].Tier != tier {
			tasks[i].Tier = tier
			changed = true
		}
		if tasks[i].ParentConversationID != edge.ParentConversationID {
			tasks[i].ParentConversationID = edge.ParentConversationID
			changed = true
		}
		if changed {
			repaired++
		}
	}

	if repaired > 0 {
		data, err := json.MarshalIndent(tasks, "", "  ")
		if err != nil {
			return repaired, err
		}
		p := filepath.Join(vaultDir, "memory", "tasks.json")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return repaired, err
		}
	}
	return repaired, nil
}
