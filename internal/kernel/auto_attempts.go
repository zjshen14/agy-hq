package kernel

import "strings"

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

// HasSupersededConversationID reports whether convID matches any superseded retry attempt ID recorded on t.
func (t ActiveTask) HasSupersededConversationID(convID string) bool {
	target := normalizeUUID(convID)
	if target == "" {
		return false
	}
	for _, sid := range t.SupersededConversationIDs {
		if normalizeUUID(sid) == target {
			return true
		}
	}
	return false
}

func mergeSupersededConversationIDs(existing, extra []string, prevConvID, nextConvID string) []string {
	nextNorm := normalizeUUID(nextConvID)
	seen := make(map[string]bool, len(existing)+len(extra)+1)
	var out []string
	add := func(raw string) {
		norm := normalizeUUID(raw)
		if norm == "" || (nextNorm != "" && norm == nextNorm) || seen[norm] {
			return
		}
		seen[norm] = true
		out = append(out, norm)
	}
	for _, id := range existing {
		add(id)
	}
	for _, id := range extra {
		add(id)
	}
	if prevNorm := normalizeUUID(prevConvID); prevNorm != "" && nextNorm != "" && prevNorm != nextNorm {
		add(prevNorm)
	}
	return out
}

func taskListHasSupersededConvID(tasks []ActiveTask, convID string) bool {
	for _, t := range tasks {
		if t.HasSupersededConversationID(convID) {
			return true
		}
	}
	return false
}

// FindTaskAttemptAnywhere resolves convID across hot (local/memory/tasks.json) and cold
// (local/memory/tasks_archive.jsonl) ledgers, matching either the active ConversationID or any
// entry in SupersededConversationIDs. Hot ledger and active ConversationID matches take priority.
func FindTaskAttemptAnywhere(vaultDir, convID string) (task ActiveTask, location string, superseded bool, found bool) {
	target := normalizeUUID(convID)
	if target == "" {
		return ActiveTask{}, "", false, false
	}
	if hotTasks, hotErr := ListTasks(vaultDir); hotErr == nil {
		for _, t := range hotTasks {
			if normalizeUUID(t.ConversationID) == target {
				return t, "hot", false, true
			}
		}
		for _, t := range hotTasks {
			if t.HasSupersededConversationID(target) {
				return t, "hot", true, true
			}
		}
	}
	if coldTasks, _, coldErr := ListArchivedTasks(vaultDir, 0); coldErr == nil {
		for _, t := range coldTasks {
			if normalizeUUID(t.ConversationID) == target {
				return t, "cold", false, true
			}
		}
		for _, t := range coldTasks {
			if t.HasSupersededConversationID(target) {
				return t, "cold", true, true
			}
		}
	}
	return ActiveTask{}, "", false, false
}

func shouldRejectPendingSpawnLink(vaultDir, convID string) bool {
	if HasQueuedSidebarArchive(vaultDir, convID) {
		return true
	}
	if attempt, _, superseded, found := FindTaskAttemptAnywhere(vaultDir, convID); found &&
		(superseded || attempt.EffectiveKind() == "auto" || attempt.EffectiveKind() == "subagent") {
		return true
	}
	return false
}

func shouldRejectAutoAdoptCandidate(vaultDir string, task ActiveTask) (autoAdoptCandidate bool, reject bool) {
	cid := strings.TrimSpace(task.ConversationID)
	if cid == "" {
		return false, false
	}
	slug := strings.TrimSpace(task.TaskSlug)
	if slug != "" && !isAutoConvSlug(slug, cid) {
		return false, false
	}
	if HasQueuedSidebarArchive(vaultDir, cid) {
		return true, true
	}
	if archived, _, aErr := ListArchivedTasks(vaultDir, 0); aErr == nil {
		cidNorm := normalizeUUID(cid)
		for _, ct := range archived {
			if ct.HasSupersededConversationID(cidNorm) {
				return true, true
			}
			if normalizeUUID(ct.ConversationID) == cidNorm && (ct.EffectiveKind() == "auto" || ct.EffectiveKind() == "subagent") {
				return true, true
			}
		}
	}
	return true, false
}

func addSupersededSidebarCandidates(t ActiveTask, add func(string)) {
	for _, scid := range t.SupersededConversationIDs {
		add(scid)
	}
}

func removeTaskBySlugAny(vaultDir, slug string) {
	if slug == "" {
		return
	}
	_ = UpdateTasks(vaultDir, func(tasks []ActiveTask) ([]ActiveTask, bool, error) {
		kept := make([]ActiveTask, 0, len(tasks))
		changed := false
		for _, t := range tasks {
			if t.TaskSlug == slug {
				changed = true
				continue
			}
			kept = append(kept, t)
		}
		return kept, changed, nil
	})
}

func restoreAutoTaskAfterFailedSpawn(vaultDir string, prevTask ActiveTask, extraSuperseded ...string) {
	if prevTask.TaskSlug == "" {
		return
	}
	_ = UpdateTasks(vaultDir, func(tasks []ActiveTask) ([]ActiveTask, bool, error) {
		for i := range tasks {
			if tasks[i].TaskSlug != prevTask.TaskSlug {
				continue
			}
			superseded := mergeSupersededConversationIDs(
				tasks[i].SupersededConversationIDs,
				prevTask.SupersededConversationIDs,
				"",
				prevTask.ConversationID,
			)
			for _, id := range extraSuperseded {
				if norm := normalizeUUID(id); norm != "" && norm != normalizeUUID(prevTask.ConversationID) {
					superseded = mergeSupersededConversationIDs(superseded, nil, norm, prevTask.ConversationID)
				}
			}
			restored := prevTask
			restored.SupersededConversationIDs = superseded
			tasks[i] = restored
			return tasks, true, nil
		}
		return tasks, false, nil
	})
}
