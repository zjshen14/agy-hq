package obs

import (
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/store"
)

func rollupsApproxEqual(a, b TokenRollup) bool {
	return a.TurnCount == b.TurnCount &&
		a.StepCount == b.StepCount &&
		a.PlannerSteps == b.PlannerSteps &&
		a.InputTokens == b.InputTokens &&
		a.CacheReadTokens == b.CacheReadTokens &&
		a.OutputTokens == b.OutputTokens &&
		a.UncachedTokens == b.UncachedTokens &&
		a.GrossTokens == b.GrossTokens &&
		a.PeakContextTokens == b.PeakContextTokens &&
		a.CompactionCount == b.CompactionCount &&
		a.ActiveLatencyMs == b.ActiveLatencyMs &&
		a.HumanWaitMs == b.HumanWaitMs &&
		floatApproxEqual(a.QuotaUnits, b.QuotaUnits) &&
		floatApproxEqual(a.EstCostUSD, b.EstCostUSD) &&
		floatApproxEqual(a.CacheSavedUSD, b.CacheSavedUSD)
}

func resolveTurnModelFromSpans(t TurnMetrics, spans []JournalModelSpan, defaultModel string) string {
	if len(spans) == 0 {
		return defaultModel
	}
	bestModel := ""
	bestOverlap := -1
	bestExec := -1
	for _, sp := range spans {
		if sp.Model == "" {
			continue
		}
		lo := t.StepStartIdx
		if sp.StepStart > lo {
			lo = sp.StepStart
		}
		hi := t.StepEndIdx
		if sp.StepEnd < hi {
			hi = sp.StepEnd
		}
		if hi >= lo {
			ov := hi - lo + 1
			if ov > bestOverlap || (ov == bestOverlap && sp.ExecNum >= bestExec) {
				bestOverlap = ov
				bestExec = sp.ExecNum
				bestModel = sp.Model
			}
		}
	}
	if bestModel != "" {
		return bestModel
	}
	tTS := turnEffectiveTime(t)
	if !tTS.IsZero() {
		var bestDiff time.Duration
		for _, sp := range spans {
			if sp.Model == "" || sp.Timestamp == "" {
				continue
			}
			spTS := parseTimestamp(sp.Timestamp)
			if spTS.IsZero() {
				continue
			}
			diff := spTS.Sub(tTS)
			if diff < 0 {
				diff = -diff
			}
			if diff <= 5*time.Minute && (bestModel == "" || diff < bestDiff) {
				bestDiff = diff
				bestModel = sp.Model
			}
		}
		if bestModel != "" {
			return bestModel
		}
	}
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].Model != "" && spans[i].StepStart <= t.StepEndIdx {
			return spans[i].Model
		}
	}
	return defaultModel
}

func floatApproxEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 1e-6
}

func collectKnownFrontDeskIDs(vaultDir string, idx *ObsIndex, taskByConv map[string]kernel.ActiveTask) map[string]bool {
	knownFDs := kernel.GetKnownFrontDeskIDSet(vaultDir)
	for parentID, cm := range idx.Conversations {
		if cm.TaskSlug != "" && strings.HasPrefix(cm.TaskSlug, "headquarter-front-desk-") {
			knownFDs[strings.ToLower(strings.TrimSpace(parentID))] = true
		}
	}
	for _, t := range taskByConv {
		if strings.HasPrefix(t.TaskSlug, "headquarter-front-desk-") && strings.TrimSpace(t.ConversationID) != "" {
			knownFDs[strings.ToLower(strings.TrimSpace(t.ConversationID))] = true
		}
	}
	return knownFDs
}

func collectWinningSpawnEdges(idx *ObsIndex) (parentIDs []string, edgeByChild map[string]SubagentSpawnEdge) {
	parentIDs = make([]string, 0, len(idx.Conversations))
	for pid := range idx.Conversations {
		parentIDs = append(parentIDs, pid)
	}
	sort.Strings(parentIDs)
	edgeByChild = make(map[string]SubagentSpawnEdge)
	for _, parentID := range parentIDs {
		cm := idx.Conversations[parentID]
		normParent := strings.ToLower(strings.TrimSpace(parentID))
		for _, edge := range cm.SpawnEdges {
			cID := strings.ToLower(strings.TrimSpace(edge.ChildConversationID))
			if cID == "" || cID == normParent {
				continue
			}
			cand := edge
			cand.ParentConversationID = normParent
			if prev, exists := edgeByChild[cID]; !exists || preferSpawnEdge(cand, prev) {
				edgeByChild[cID] = cand
			}
		}
	}
	return parentIDs, edgeByChild
}

func enrichAndBuildDAG(vaultDir string, idx *ObsIndex) bool {
	if idx == nil || len(idx.Conversations) == 0 {
		return false
	}
	changed := false

	fd, hasFD := kernel.GetFrontDesk(vaultDir)
	fdID := ""
	if hasFD {
		fdID = strings.ToLower(strings.TrimSpace(fd.ConversationID))
	}

	taskByConv := make(map[string]kernel.ActiveTask)
	supersededOwner := make(map[string]kernel.ActiveTask)
	recordTask := func(t kernel.ActiveTask, overwrite bool) {
		cid := strings.ToLower(strings.TrimSpace(t.ConversationID))
		if cid != "" {
			if overwrite {
				taskByConv[cid] = t
			} else if _, exists := taskByConv[cid]; !exists {
				taskByConv[cid] = t
			}
		}
		for _, rawSup := range t.SupersededConversationIDs {
			supID := strings.ToLower(strings.TrimSpace(rawSup))
			if supID != "" && supID != cid && supID != fdID {
				if _, exists := supersededOwner[supID]; overwrite || !exists {
					supersededOwner[supID] = t
				}
			}
		}
	}
	if hot, err := kernel.ListTasks(vaultDir); err == nil {
		for _, t := range hot {
			recordTask(t, true)
		}
	}
	if cold, _, err := kernel.ListArchivedTasks(vaultDir, 0); err == nil {
		for _, t := range cold {
			recordTask(t, false)
		}
	}

	// 1. Collect ground-truth spawn edges from all transcripts
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

	knownFDs := collectKnownFrontDeskIDs(vaultDir, idx, taskByConv)
	nestedParents := kernel.LoadNestedConversationParents(vaultDir)
	parentIDs, edgeByChild := collectWinningSpawnEdges(idx)

	// 2. Enrich each conversation's metadata (Ground-truth SpawnEdge > Journal > tasks.json > transcript fallback)
	for _, cid := range parentIDs {
		cm := idx.Conversations[cid]
		origTitle := cm.Title
		origSlug := cm.TaskSlug
		origKind := cm.Kind
		origScope := cm.Scope
		origTier := cm.Tier
		origReqTier := cm.RequestedTier
		origModel := cm.Model
		origModelSrc := cm.ModelSource
		origStatus := cm.Status
		origParent := cm.ParentConversationID
		origSelf := cm.Self

		if len(cm.JournalSpans) > 0 {
			if latest := cm.JournalSpans[len(cm.JournalSpans)-1].Model; latest != "" {
				cm.Model = latest
				cm.ModelSource = "journal"
			}
		}

		if fdID != "" && cid == fdID {
			cm.Kind = "front_desk"
			cm.TaskSlug = "front-desk"
			cm.Title = "[HQ] Front Desk"
			cm.Scope = "hq"
			if cm.Model == "" {
				cm.Model = "gemini-4-hydrogen-medium"
				cm.ModelSource = "fallback"
			}
			cm.Tier = ModelCanonicalTier(cm.Model)
			cm.Status = "active"
		} else {
			if t, ok := taskByConv[cid]; ok {
				cm.TaskSlug = t.TaskSlug
				cm.Title = t.Title
				cm.Kind = t.EffectiveKind()
				cm.Scope = t.EffectiveScope()
				if t.Tier != "" {
					cm.RequestedTier = t.Tier
					if cm.Tier == "" {
						cm.Tier = t.Tier
					}
				}
				if t.Model != "" && (cm.Model == "" || cm.ModelSource == "fallback") {
					cm.Model = t.Model
					cm.ModelSource = "task"
				}
				if t.ParentConversationID != "" {
					cm.ParentConversationID = strings.ToLower(strings.TrimSpace(t.ParentConversationID))
				}
				cm.Status = t.Status
			}

			// Ground-truth spawn edge overrides FIFO-drifted subagent role/parent
			if edge, hasEdge := edgeByChild[cid]; hasEdge {
				cm.ParentConversationID = edge.ParentConversationID
				isHiddenWorker := edge.TypeName == "run_workflow" || edge.TypeName == "agentapi"
				cleanRole := strings.Trim(strings.TrimSpace(edge.Role), "\"'")
				cleanModel := strings.Trim(strings.TrimSpace(edge.Model), "\"'")
				if cleanRole != "" && !isHiddenWorker {
					eModel := cleanModel
					if eModel == "" || eModel == "inherit" {
						eModel = cm.Model
					}
					tier, scope, _, title, slug := kernel.ParseSubagentRole(cleanRole, eModel)
					cm.Title = title
					cm.TaskSlug = slug
					cm.Scope = scope
					cm.RequestedTier = tier
					if (cm.Model == "" || cm.ModelSource == "fallback") && cleanModel != "" && cleanModel != "inherit" {
						cm.Model = cleanModel
						cm.ModelSource = "spawn_edge"
					}
					if cm.Model == "" {
						cm.Tier = tier
					}
				} else if cleanRole != "" && cm.Title == "" {
					cm.Title = cleanRole
				}
				if isHiddenWorker || (edge.ParentConversationID != "" && len(knownFDs) > 0 && !knownFDs[edge.ParentConversationID]) {
					cm.Kind = "nested_subagent"
				} else if cm.Kind != "task" || cleanRole != "" {
					cm.Kind = "subagent"
				}
			} else if nestedParent, isNested := nestedParents[cid]; isNested {
				cm.Kind = "nested_subagent"
				if cm.ParentConversationID == "" && nestedParent != "" {
					cm.ParentConversationID = nestedParent
				}
			} else if ownerTask, isSuperseded := supersededOwner[cid]; isSuperseded {
				ownerCID := strings.ToLower(strings.TrimSpace(ownerTask.ConversationID))
				cm.Scope = ownerTask.EffectiveScope()
				if ownerTask.Tier != "" && cm.RequestedTier == "" {
					cm.RequestedTier = ownerTask.Tier
				}
				if cm.Title == "" || strings.HasPrefix(cm.TaskSlug, "conv-") {
					shortID := store.ShortConvID(cid)
					cm.Title = ownerTask.Title + " (prior run " + shortID + ")"
					cm.TaskSlug = ownerTask.TaskSlug + "-superseded-" + shortID
				}
				if ownerCID != "" && ownerCID != cid {
					if _, ownerInIdx := idx.Conversations[ownerCID]; ownerInIdx {
						cm.ParentConversationID = ownerCID
						cm.Kind = "nested_subagent"
					}
				}
			}

			if cm.ParentConversationID != "" {
				addChildLink(cm.ParentConversationID, cid)
			}
		}

		// Fallbacks for untracked conversations
		if cm.Kind == "" {
			if cm.ParentConversationID != "" {
				if fdID != "" && !knownFDs[cm.ParentConversationID] && (len(knownFDs) > 0) {
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
			shortID := store.ShortConvID(cid)
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
			tierHint := cm.RequestedTier
			if tierHint == "" {
				tierHint = cm.Tier
			}
			switch strings.ToLower(strings.TrimSpace(tierHint)) {
			case "deep":
				cm.Model = "gemini-4-carbon-high"
			case "quick":
				cm.Model = "gemini-4-hydrogen-low"
			default:
				cm.Model = "gemini-4-neon-medium"
			}
			cm.ModelSource = "fallback"
		} else if cm.ModelSource == "" {
			cm.ModelSource = "journal"
		}
		cm.Tier = ModelCanonicalTier(cm.Model)

		// Recompute per-turn model, cost, and cache savings on a cloned Turns slice if changed
		turnsChanged := false
		for ti := range cm.Turns {
			tModel := resolveTurnModelFromSpans(cm.Turns[ti], cm.JournalSpans, cm.Model)
			tTier := ModelCanonicalTier(tModel)
			q, c, s := ComputeQuotaCostAndSavings(cm.Turns[ti].InputTokens, cm.Turns[ti].CacheReadTokens, cm.Turns[ti].OutputTokens, tModel, tTier)
			if cm.Turns[ti].Model != tModel || cm.Turns[ti].ModelTier != tTier ||
				!floatApproxEqual(cm.Turns[ti].QuotaUnits, q) ||
				!floatApproxEqual(cm.Turns[ti].EstCostUSD, c) ||
				!floatApproxEqual(cm.Turns[ti].CacheSavedUSD, s) {
				if !turnsChanged {
					clonedTurns := make([]TurnMetrics, len(cm.Turns))
					copy(clonedTurns, cm.Turns)
					cm.Turns = clonedTurns
					turnsChanged = true
				}
				cm.Turns[ti].Model = tModel
				cm.Turns[ti].ModelTier = tTier
				cm.Turns[ti].QuotaUnits = q
				cm.Turns[ti].EstCostUSD = c
				cm.Turns[ti].CacheSavedUSD = s
			}
		}
		cm.Self = RollupTurns(cm.Turns, cm.Self.StepCount, cm.Model, cm.Tier)

		if turnsChanged || cm.Title != origTitle || cm.TaskSlug != origSlug || cm.Kind != origKind ||
			cm.Scope != origScope || cm.Tier != origTier || cm.RequestedTier != origReqTier ||
			cm.Model != origModel || cm.ModelSource != origModelSrc ||
			cm.Status != origStatus || cm.ParentConversationID != origParent || !rollupsApproxEqual(cm.Self, origSelf) {
			changed = true
		}
		idx.Conversations[cid] = cm
	}

	// 3. Populate ChildConversationIDs, RootConversationID, DescendantCount, and compute recursive Tree rollups
	for cid, cm := range idx.Conversations {
		kids := childrenByParent[cid]
		sort.Strings(kids)
		if !slices.Equal(cm.ChildConversationIDs, kids) {
			changed = true
		}
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
		if cm.RootConversationID != root {
			changed = true
		}
		cm.RootConversationID = root
		idx.Conversations[cid] = cm
	}

	memo := make(map[string]TokenRollup)
	descMemo := make(map[string]int)
	inProgress := make(map[string]bool)
	var computeTree func(cid string) (TokenRollup, int)
	computeTree = func(cid string) (TokenRollup, int) {
		if r, ok := memo[cid]; ok {
			return r, descMemo[cid]
		}
		cm, ok := idx.Conversations[cid]
		if !ok {
			return TokenRollup{}, 0
		}
		if inProgress[cid] {
			return cm.Self, 0
		}
		inProgress[cid] = true
		agg := cm.Self
		desc := 0
		for _, childID := range cm.ChildConversationIDs {
			if _, exists := idx.Conversations[childID]; exists {
				childRollup, childDesc := computeTree(childID)
				agg = AddRollup(agg, childRollup)
				desc += 1 + childDesc
			}
		}
		delete(inProgress, cid)
		memo[cid] = agg
		descMemo[cid] = desc
		return agg, desc
	}

	for cid, cm := range idx.Conversations {
		nextTree, nextDesc := computeTree(cid)
		if !rollupsApproxEqual(cm.Tree, nextTree) || cm.DescendantCount != nextDesc {
			changed = true
		}
		cm.Tree = nextTree
		cm.DescendantCount = nextDesc
		idx.Conversations[cid] = cm
	}

	return changed
}

func spawnEdgePriority(typeName string) int {
	switch strings.ToLower(strings.TrimSpace(typeName)) {
	case "agentapi":
		return 1
	case "run_workflow":
		return 2
	default:
		return 3 // ground-truth invoke_subagent / typed subagent
	}
}

func preferSpawnEdge(cand, prev SubagentSpawnEdge) bool {
	cr := spawnEdgePriority(cand.TypeName)
	pr := spawnEdgePriority(prev.TypeName)
	if cr != pr {
		return cr > pr
	}
	if prev.Role == "" && cand.Role != "" {
		return true
	}
	if cand.Role == "" && prev.Role != "" {
		return false
	}
	if cand.CreatedAt != "" && prev.CreatedAt != "" && cand.CreatedAt != prev.CreatedAt {
		return cand.CreatedAt < prev.CreatedAt
	}
	return cand.ParentConversationID < prev.ParentConversationID
}

// ReconcileSubagentAttribution repairs FIFO-drifted subagent metadata in local/memory/tasks.json
// using ground-truth invoke_subagent, run_workflow, and agentapi spawn edges extracted from parent transcripts.
// Subagents linked to non-Front-Desk parents, run_workflow children, and raw agentapi children are
// downgraded to Kind="subagent", moved to cold archive + nested_conversations.json, and removed from tasks.json.
// Direct Front-Desk subagents misclassified as Kind="task" have Kind repaired to "subagent".
func ReconcileSubagentAttribution(vaultDir string) (int, error) {
	idx, err := LoadOrRefreshIndex(vaultDir)
	if err != nil {
		return 0, err
	}

	knownFDs := collectKnownFrontDeskIDs(vaultDir, idx, nil)
	if archived, _, aErr := kernel.ListArchivedTasks(vaultDir, 0); aErr == nil {
		for _, at := range archived {
			if strings.HasPrefix(at.TaskSlug, "headquarter-front-desk-") && strings.TrimSpace(at.ConversationID) != "" {
				knownFDs[strings.ToLower(strings.TrimSpace(at.ConversationID))] = true
			}
		}
	}

	parentIDs, edgeByChild := collectWinningSpawnEdges(idx)
	if len(edgeByChild) == 0 {
		return 0, nil
	}
	parentOf := make(map[string]string)

	// Pass 1: seed from persisted ParentConversationID in deterministic sorted order.
	for _, rawParentID := range parentIDs {
		cm := idx.Conversations[rawParentID]
		if cm.ParentConversationID != "" {
			cid := strings.ToLower(strings.TrimSpace(cm.ConversationID))
			pid := strings.ToLower(strings.TrimSpace(cm.ParentConversationID))
			if cid != "" && pid != "" && cid != pid {
				parentOf[cid] = pid
			}
		}
	}
	// Pass 2: override stale parentOf with winning ground-truth SpawnEdges.
	for cID, edge := range edgeByChild {
		parentOf[cID] = edge.ParentConversationID
	}

	hasNonFrontDeskAncestor := func(cid, immediateParent string) bool {
		cur := strings.ToLower(strings.TrimSpace(immediateParent))
		if cur == "" {
			cur = parentOf[cid]
		}
		seen := map[string]bool{cid: true}
		for cur != "" && !seen[cur] {
			seen[cur] = true
			if knownFDs[cur] {
				return false
			}
			next, hasParent := parentOf[cur]
			if !hasParent || next == "" {
				return true
			}
			cur = next
		}
		return false
	}

	type nestedRecord struct {
		convID   string
		parentID string
	}
	var nestedToRecord []nestedRecord
	repaired := 0
	nowStr := time.Now().Format(time.RFC3339)

	err = kernel.ApplyTaskReconciliation(vaultDir, func(tasks []kernel.ActiveTask) ([]kernel.ActiveTask, []kernel.ActiveTask, bool, error) {
		if len(tasks) == 0 {
			return tasks, nil, false, nil
		}
		repaired = 0
		nestedToRecord = nil
		var nestedToArchive []kernel.ActiveTask
		kept := make([]kernel.ActiveTask, 0, len(tasks))

		for _, t := range tasks {
			cid := strings.ToLower(strings.TrimSpace(t.ConversationID))
			edge, hasEdge := edgeByChild[cid]
			isExplicitTask := kernel.IsExplicitTopLevelTask(t)

			if hasEdge && !isExplicitTask {
				isHiddenWorker := edge.TypeName == "run_workflow" || edge.TypeName == "agentapi"
				cleanRole := strings.Trim(strings.TrimSpace(edge.Role), "\"'")
				cleanModel := strings.Trim(strings.TrimSpace(edge.Model), "\"'")

				isNested := isHiddenWorker ||
					(edge.ParentConversationID != "" && len(knownFDs) > 0 && !knownFDs[edge.ParentConversationID]) ||
					hasNonFrontDeskAncestor(cid, edge.ParentConversationID)

				changed := false
				if cleanRole != "" && !isHiddenWorker {
					model := t.Model
					if cleanModel != "" && cleanModel != "inherit" {
						model = cleanModel
					}
					tier, scope, _, title, slug := kernel.ParseSubagentRole(cleanRole, model)
					if t.Title != title {
						t.Title = title
						changed = true
					}
					if t.TaskSlug != slug {
						t.TaskSlug = slug
						changed = true
					}
					if t.Scope != scope {
						t.Scope = scope
						changed = true
					}
					if t.Tier != tier {
						t.Tier = tier
						changed = true
					}
				}
				if edge.ParentConversationID != "" && t.ParentConversationID != edge.ParentConversationID {
					t.ParentConversationID = edge.ParentConversationID
					changed = true
				}
				if isNested {
					if !strings.EqualFold(strings.TrimSpace(t.Kind), "subagent") {
						t.Kind = "subagent"
					}
					t.Status = "archived"
					t.ArchivedAt = nowStr
					nestedToArchive = append(nestedToArchive, t)
					nestedToRecord = append(nestedToRecord, nestedRecord{
						convID:   t.ConversationID,
						parentID: edge.ParentConversationID,
					})
					repaired++
					continue
				}
				// Direct Front-Desk subagent: ensure Kind is "subagent" rather than leaked "task".
				if !strings.EqualFold(strings.TrimSpace(t.Kind), "subagent") {
					t.Kind = "subagent"
					changed = true
				}
				if changed {
					repaired++
				}
			}
			kept = append(kept, t)
		}
		return kept, nestedToArchive, repaired > 0, nil
	})
	if err != nil {
		return 0, err
	}
	for _, rec := range nestedToRecord {
		_ = kernel.RecordNestedConversation(vaultDir, rec.convID, rec.parentID)
	}
	return repaired, nil
}
