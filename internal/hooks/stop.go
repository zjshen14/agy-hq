package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/channels"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/taint"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/watcher"
)

func hasPendingResumableApproval(vaultDir, convID string) bool {
	if strings.TrimSpace(convID) == "" {
		return false
	}
	pending, err := approvals.List(vaultDir)
	if err != nil {
		return false
	}
	for _, p := range pending {
		if p.ConversationID == convID && p.ResumeOnDecide {
			return true
		}
	}
	return false
}

// HandleStop writes an immutable 0-token turn journal entry to local/memory/journal/YYYY-MM-DD/<convId>-<execNum>.json
// and returns {"decision":"stop"} so the turn finishes immediately without blocking.
func HandleStop(r io.Reader, w io.Writer, vaultDir string) error {
	var payload StopPayload
	if err := json.NewDecoder(r).Decode(&payload); err != nil {
		_, wErr := w.Write([]byte(`{"decision":"stop"}` + "\n"))
		return wErr
	}
	_ = kernel.RecordLiveLSSession(vaultDir)

	fd, hasFD := kernel.GetFrontDesk(vaultDir)
	isFrontDesk := hasFD && payload.ConversationID != "" && fd.ConversationID == payload.ConversationID

	if payload.ArtifactDirectoryPath != "" && (isFrontDesk || (!hasFD && shouldAutoClaimVacantFrontDesk(payload.CommonPayload, vaultDir) && payload.ConversationID == "")) {
		writeActiveArtifactDir(vaultDir, payload.ArtifactDirectoryPath)
	}

	if payload.ConversationID != "" {
		if isFrontDesk {
			resetFrontDeskTurnState(vaultDir, payload.ConversationID)
			if payload.ArtifactDirectoryPath != "" {
				_ = kernel.SetFrontDesk(vaultDir, payload.ConversationID, payload.ArtifactDirectoryPath)
			}
			kernel.ReconcileFrontDeskFromTranscript(vaultDir, payload.TranscriptPath)
		}

		todayDateStr := time.Now().Format("2006-01-02")
		dateDir := filepath.Join(vaultDir, "memory", "journal", todayDateStr)
		_ = os.MkdirAll(dateDir, 0o755)
		execNum := payload.ExecutionNum
		if execNum <= 0 {
			execNum = nextExecutionNum(dateDir, payload.ConversationID)
		}
		stepStartIdx := priorJournalStepCountWithDate(vaultDir, payload.ConversationID, todayDateStr, execNum)
		tm, obsTurn := parseTranscriptTurnMetrics(payload.TranscriptPath, stepStartIdx)

		// B1 Duplicate Stop Hook Guard: When both workspace and plugin hooks.json invoke `hq hook stop`
		// back-to-back on the same turn completion (ExecutionNum == 0, 0 new steps since the entry written <3s ago),
		// skip writing a duplicate 0-step journal file and skip double-incrementing TurnCount.
		if payload.ExecutionNum <= 0 && execNum > 1 && payload.TranscriptPath != "" && tm.StepCount > 0 && tm.StepCount <= stepStartIdx {
			prevFile := filepath.Join(dateDir, fmt.Sprintf("%s-%d.json", payload.ConversationID, execNum-1))
			if info, stErr := os.Stat(prevFile); stErr == nil && time.Since(info.ModTime()) < 3*time.Second {
				_, wErr := w.Write([]byte(`{"decision":"stop"}` + "\n"))
				return wErr
			}
		}

		toolsUsed := tm.ToolsUsed
		filesModified := tm.FilesModified
		summary := tm.LastResponse
		transcriptUserPrompt := tm.LastUserPrompt
		stepCount := tm.StepCount
		toolErrorCount := tm.ToolErrorCount
		userPrompt := strings.TrimSpace(payload.LastUserInput)
		if userPrompt == "" {
			userPrompt = transcriptUserPrompt
		}
		_, isTainted := taint.Check(vaultDir, payload.ConversationID)

		var existingTasks []kernel.ActiveTask
		if tasks, err := kernel.ListTasks(vaultDir); err == nil {
			existingTasks = tasks
		}
		parentConvID := resolveParentConversationIDFromTasks(vaultDir, payload.ConversationID, existingTasks)

		if _, err := os.Stat(dateDir); err == nil {
			entry := JournalEntry{
				Timestamp:            time.Now().Format(time.RFC3339),
				ConversationID:       payload.ConversationID,
				ParentConvID:         parentConvID,
				ExecutionNum:         execNum,
				TranscriptPath:       payload.TranscriptPath,
				WorkspacePaths:       payload.WorkspacePaths,
				ModelName:            payload.ModelName,
				StepCount:            stepCount,
				ToolErrorCount:       toolErrorCount,
				StepStartIdx:         stepStartIdx,
				TurnStepCount:        tm.TurnStepCount,
				TurnErrorCount:       tm.TurnErrorCount,
				TransientErrors:      tm.TransientErrors,
				PreToolDenials:       tm.PreToolDenials,
				TaintedSession:       isTainted || tm.TaintedCmdSeen,
				TurnInputTokens:      obsTurn.InputTokens,
				TurnCacheReadTokens:  obsTurn.CacheReadTokens,
				TurnOutputTokens:     obsTurn.OutputTokens,
				TurnUncachedTokens:   obsTurn.UncachedTokens,
				TurnGrossTokens:      obsTurn.GrossTokens,
				TurnCacheHitRatio:    obsTurn.CacheHitRatio,
				PeakContextTokens:    obsTurn.PeakContextTokens,
				CurrentContextTokens: obsTurn.CurrentContextTokens,
				CompactionCount:      obsTurn.CompactionCount,
				ActiveLatencyMs:      obsTurn.ActiveLatencyMs,
				HumanWaitMs:          obsTurn.HumanWaitMs,
				UserPrompt:           userPrompt,
				ToolsUsed:            toolsUsed,
				FilesModified:        filesModified,
				TerminationReason:    payload.TerminationReason,
				ResponseSummary:      summary,
			}
			if data, err := json.MarshalIndent(entry, "", "  "); err == nil {
				fileName := fmt.Sprintf("%s-%d.json", payload.ConversationID, execNum)
				_ = os.WriteFile(filepath.Join(dateDir, fileName), data, 0o644)
			}

			termReason := strings.ToUpper(strings.TrimSpace(payload.TerminationReason))
			cleanTurnPause := strings.TrimSpace(payload.Error) == "" && (termReason == "" || termReason == "NO_TOOL_CALL")
			awaitingBackgroundTurn := cleanTurnPause && ((payload.FullyIdle != nil && !*payload.FullyIdle) || tm.HasPendingBackgroundTasks)
			attemptSuperseded := false
			skipAutoAdopt := false

			var matchedTask *kernel.ActiveTask
			// If this conversation is not the Front Desk, auto-adopt/update it in local/memory/tasks.json
			if !isFrontDesk {
				bindSpawnTokenFromTranscript(vaultDir, payload.ConversationID, payload.LastUserInput, payload.TranscriptPath)
				alreadyTracked := false
				for i := range existingTasks {
					if existingTasks[i].ConversationID == payload.ConversationID {
						alreadyTracked = true
						matchedTask = &existingTasks[i]
						break
					}
				}

				var attemptTask kernel.ActiveTask
				var attemptLoc string
				var hasAttempt bool
				var hasQueuedArchive bool
				var autoOrSpawnEvidence bool
				if !alreadyTracked {
					attemptTask, attemptLoc, attemptSuperseded, hasAttempt = kernel.FindTaskAttemptAnywhere(vaultDir, payload.ConversationID)
					hasQueuedArchive = kernel.HasQueuedSidebarArchive(vaultDir, payload.ConversationID)
					autoOrSpawnEvidence = hasAutoOrSpawnEvidence(vaultDir, payload.ConversationID, tm.FirstUserPromptRaw, userPrompt)
					skipAutoAdopt = (hasAttempt && (attemptSuperseded || attemptTask.EffectiveKind() == "auto" || attemptTask.EffectiveKind() == "subagent")) ||
						hasQueuedArchive || autoOrSpawnEvidence
				}

				vacatedMarker := filepath.Join(vaultDir, "memory", ".vacated")
				_, vacatedExists := os.Stat(vacatedMarker)

				isNested := kernel.IsNestedSubagentConversation(vaultDir, payload.ConversationID)
				var lsLineage kernel.LSConversationLineage
				var hasLSLineage bool
				if !alreadyTracked && !isNested && !skipAutoAdopt {
					isNested, _, lsLineage, hasLSLineage = detectNestedDescendant(vaultDir, payload.ConversationID, payload.LastUserInput, fd.ConversationID, 60*time.Minute)
				}

				if !hasFD && !isNested && !skipAutoAdopt && shouldAutoClaimVacantFrontDesk(payload.CommonPayload, vaultDir) && !alreadyTracked && len(existingTasks) == 0 && os.IsNotExist(vacatedExists) {
					// In-session bootstrap fallback: fresh Vault with no tracked tasks, front_desk is vacant,
					// and no explicit release-desk marker exists. Claim Front Desk instead of registering [Task:general]!
					_ = kernel.SetFrontDesk(vaultDir, payload.ConversationID, payload.ArtifactDirectoryPath)
					kernel.ReconcileFrontDeskFromTranscript(vaultDir, payload.TranscriptPath)
				} else {
					if !alreadyTracked && !isNested && !skipAutoAdopt {
						// Subagent fallback: match against pending queue or LS trajectory metadata if not yet registered
						if sub, found := kernel.PopPendingSubagentForCaller(vaultDir, payload.ConversationID, payload.LastUserInput, 60*time.Minute); found {
							modelName := sub.Model
							if modelName == "" {
								modelName = payload.ModelName
							}
							nowISO := time.Now().Format(time.RFC3339)
							task := kernel.ActiveTask{
								TaskSlug:             sub.TaskSlug,
								Title:                sub.Title,
								Kind:                 "subagent",
								Scope:                sub.Scope,
								ParentConversationID: sub.ParentConversationID,
								Tier:                 sub.Tier,
								Model:                modelName,
								ConversationID:       payload.ConversationID,
								Status:               "completed",
								TurnCount:            1,
								CreatedAt:            nowISO,
								LastUpdatedAt:        nowISO,
								CompletedAt:          nowISO,
								LastSummary:          summary,
							}
							if strings.TrimSpace(task.LastSummary) == "" {
								task.LastSummary = "Task completed."
							}
							_ = kernel.RegisterTask(vaultDir, task)
							alreadyTracked = true
							matchedTask = &task
						} else if hasLSLineage && lsLineage.IsDirectFrontDeskSubagent {
							task := buildSubagentTaskFromLSLineage(payload.ConversationID, payload.ModelName, "completed", summary, lsLineage)
							_ = kernel.RegisterTask(vaultDir, task)
							alreadyTracked = true
							matchedTask = &task
						} else if linkedTask, linked := kernel.LinkPendingSpawnTask(vaultDir, payload.ConversationID, payload.ModelName); linked {
							alreadyTracked = true
							matchedTask = &linkedTask
						}
					}

					if alreadyTracked && matchedTask != nil {
						// Subagent & [Auto] worker lifecycle transition: mark completed (or immediately archived for unpinned [Auto] workers) unless paused on background tasks or awaiting a resumable human approval
						isSubagent := matchedTask.EffectiveKind() == "subagent" || strings.HasPrefix(matchedTask.TaskSlug, "subagent-") || strings.HasPrefix(matchedTask.Title, "[Subagent:")
						isAuto := matchedTask.EffectiveKind() == "auto"
						newStatus := matchedTask.Status
						if isSubagent && matchedTask.Status == "active" {
							if !awaitingBackgroundTurn {
								newStatus = "completed"
							}
						} else if isAuto && !awaitingBackgroundTurn && !hasPendingResumableApproval(vaultDir, payload.ConversationID) && matchedTask.Status == "active" {
							if matchedTask.Pinned {
								newStatus = "completed"
							} else {
								newStatus = "archived"
							}
						}
						newSummary := summary
						if strings.TrimSpace(newSummary) == "" {
							newSummary = matchedTask.LastSummary
						}
						if strings.TrimSpace(newSummary) == "" {
							newSummary = "Task completed."
						}
						nowTime := time.Now()
						nowISO := nowTime.Format(time.RFC3339)
						completedAt := matchedTask.CompletedAt
						if (newStatus == "completed" || newStatus == "archived") && completedAt == "" {
							completedAt = nowISO
						}
						archivedAt := matchedTask.ArchivedAt
						if newStatus == "archived" && archivedAt == "" {
							archivedAt = nowISO
						}
						updatedTask := kernel.ActiveTask{
							TaskSlug:                  matchedTask.TaskSlug,
							Title:                     matchedTask.Title,
							Kind:                      matchedTask.EffectiveKind(),
							Scope:                     matchedTask.EffectiveScope(),
							ParentConversationID:      matchedTask.ParentConversationID,
							Tier:                      matchedTask.Tier,
							Model:                     matchedTask.Model,
							WorktreeDir:               matchedTask.WorktreeDir,
							WorktreeDirs:              matchedTask.WorktreeDirs,
							RepoRoot:                  matchedTask.RepoRoot,
							ConversationID:            payload.ConversationID,
							SupersededConversationIDs: matchedTask.SupersededConversationIDs,
							EventIDs:                  matchedTask.EventIDs,
							Status:                    newStatus,
							TurnCount:                 matchedTask.TurnCount + 1,
							Pinned:                    matchedTask.Pinned,
							CreatedAt:                 matchedTask.CreatedAt,
							LastUpdatedAt:             nowISO,
							CompletedAt:               completedAt,
							ArchivedAt:                archivedAt,
							SidebarArchived:           matchedTask.SidebarArchived,
							SidebarSyncedAt:           matchedTask.SidebarSyncedAt,
							LastSummary:               newSummary,
						}
						_ = kernel.RegisterTask(vaultDir, updatedTask)
						matchedTask = &updatedTask
						if newStatus == "archived" && matchedTask.TaskSlug != "" {
							kernel.ReleaseTaskWorkspaceLeases(vaultDir, matchedTask.TaskSlug)
						}
						if isAuto && newStatus == "archived" && !matchedTask.Pinned && !matchedTask.SidebarArchived &&
							!kernel.HasQueuedSidebarArchive(vaultDir, payload.ConversationID) {
							_ = kernel.EnqueueSidebarSync(vaultDir, kernel.SidebarSyncQueueItem{
								ConversationID:  payload.ConversationID,
								TaskSlug:        matchedTask.TaskSlug,
								DesiredArchived: true,
								NextRetryAt:     nowTime.Add(3 * time.Second).UTC().Format(time.RFC3339),
								LastError:       "auto_stop_deferred",
							}, nowTime)
						}
					} else if !alreadyTracked && !isNested {
						if skipAutoAdopt {
							// Never auto-adopt a superseded, cold-archived auto/subagent, queued-archive, or [hq-spawn:]/[Auto] conversation as conv-* (`kind="task"`).
							nowTime := time.Now()
							switch {
							case attemptSuperseded:
								if !attemptTask.Pinned && kernel.IsCanonicalUUID(payload.ConversationID) && !hasQueuedArchive {
									_ = kernel.EnqueueSidebarSync(vaultDir, kernel.SidebarSyncQueueItem{
										ConversationID:  payload.ConversationID,
										TaskSlug:        attemptTask.TaskSlug,
										DesiredArchived: true,
										NextRetryAt:     nowTime.Add(3 * time.Second).UTC().Format(time.RFC3339),
										LastError:       "superseded_by_retry",
									}, nowTime)
								}
							case hasAttempt && attemptLoc == "cold":
								if attemptTask.EffectiveKind() == "auto" && !attemptTask.Pinned && !attemptTask.SidebarArchived &&
									kernel.IsCanonicalUUID(payload.ConversationID) && !hasQueuedArchive {
									_ = kernel.EnqueueSidebarSync(vaultDir, kernel.SidebarSyncQueueItem{
										ConversationID:  payload.ConversationID,
										TaskSlug:        attemptTask.TaskSlug,
										DesiredArchived: true,
										NextRetryAt:     nowTime.Add(3 * time.Second).UTC().Format(time.RFC3339),
										LastError:       "auto_stop_deferred",
									}, nowTime)
								}
							case !hasQueuedArchive && autoOrSpawnEvidence && !awaitingBackgroundTurn &&
								!hasPendingResumableApproval(vaultDir, payload.ConversationID) && kernel.IsCanonicalUUID(payload.ConversationID):
								_ = kernel.EnqueueSidebarSync(vaultDir, kernel.SidebarSyncQueueItem{
									ConversationID:  payload.ConversationID,
									DesiredArchived: true,
									NextRetryAt:     nowTime.Add(3 * time.Second).UTC().Format(time.RFC3339),
									LastError:       "auto_stop_deferred",
								}, nowTime)
							}
						} else {
							title := ""
							if strings.TrimSpace(userPrompt) != "" {
								title = kernel.SanitizeTaskTitle(userPrompt, "")
							}
							_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
								Title:          title,
								Kind:           "task",
								Model:          payload.ModelName,
								ConversationID: payload.ConversationID,
								TurnCount:      1,
								LastUpdatedAt:  time.Now().Format(time.RFC3339),
								LastSummary:    summary,
							})
						}
					}
				}
			}

			// RFC-0025 §7.3 / §16.8: bind provider workspaces written this turn to the owning task (ledger hits) or
			// queue daemon probe candidates (root_patterns hits). Ledger/tasks reads only; no provider calls.
			if !isFrontDesk && !isTainted && !tm.TaintedCmdSeen && !attemptSuperseded {
				autoBindWorkspacesFromStop(vaultDir, payload.ConversationID, tm.TurnFilesModified)
			}

			if obsTurn.CurrentContextTokens >= PreCompactionFlushTokens {
				var activeTask kernel.ActiveTask
				hasActive := false
				if matchedTask != nil && matchedTask.TaskSlug != "" {
					activeTask = *matchedTask
					hasActive = true
				} else if foundTask, ok := kernel.FindTaskByConversationID(vaultDir, payload.ConversationID); ok && foundTask.TaskSlug != "" {
					activeTask = foundTask
					hasActive = true
				}
				if hasActive {
					if _, _, found, _ := vault.LoadProgress(vaultDir, activeTask.TaskSlug); !found {
						goalText := strings.TrimSpace(activeTask.Title)
						if goalText == "" {
							goalText = strings.TrimSpace(userPrompt)
						}
						doneText := strings.TrimSpace(summary)
						if doneText == "" {
							doneText = "Context reached 120k+ tokens; auto-checkpointed from turn journal."
						}
						_, _ = vault.SaveProgress(vaultDir, vault.ProgressCheckpoint{
							TaskSlug:  activeTask.TaskSlug,
							Goal:      goalText,
							Completed: doneText,
							NextSteps: "Continue from latest turn journal or refine with `hq progress save`.",
							Pointers:  strings.Join(filesModified, ", "),
						}, time.Now())
					}
				}
			}
			if obsTurn.CompactionCount > 0 {
				_ = os.Remove(compactionNudgeMarkerPath(vaultDir, payload.ConversationID))
			}
			if !awaitingBackgroundTurn && !attemptSuperseded {
				_, _ = kernel.CompleteConversationEvents(vaultDir, payload.ConversationID, time.Now())
			}
			_, _ = approvals.ReconcileAskRejections(vaultDir, payload.ConversationID, payload.TranscriptPath, time.Now())
			_, _ = approvals.ReconcileLocalGitProposals(vaultDir, time.Now())

			replyText := strings.TrimSpace(payload.LastResponse)
			if replyText == "" {
				replyText = strings.TrimSpace(tm.CurrentTurnFullResponse)
			}
			if replyText != "" {
				if enqCount, _ := channels.EnqueueTurnReplyIfCorrelated(vaultDir, payload.ConversationID, execNum, replyText, tm.CurrentTurnStartedAt, time.Now()); enqCount > 0 {
					channels.BestEffortFlushLoopback(vaultDir, 150*time.Millisecond)
				}
			}

			if payload.TranscriptPath != "" {
				_, _ = obs.ReconcileSubagentAttribution(vaultDir)
				_, _ = obs.SyncDailyBudgetCircuitBreaker(vaultDir, time.Now())
			}
		}
	}

	if kernel.HasPendingSidebarSyncQueue(vaultDir) {
		_, _ = kernel.DrainSidebarSyncQueue(vaultDir, kernel.DefaultSidebarStopHookTimeout)
	}

	watcher.MaybeLaunchDesktopPiggyback(vaultDir, time.Now())

	_, err := w.Write([]byte(`{"decision":"stop"}` + "\n"))
	return err
}

func nextExecutionNum(dateDir, convID string) int {
	entries, err := os.ReadDir(dateDir)
	if err != nil {
		return 1
	}
	prefix := convID + "-"
	maxNum := 0
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".json") {
			numStr := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")
			if n, err := strconv.Atoi(numStr); err == nil && n > maxNum {
				maxNum = n
			}
		}
	}
	return maxNum + 1
}

func priorJournalStepCountWithDate(vaultDir, convID, todayDateStr string, currentExecNum int) int {
	if convID == "" {
		return 0
	}
	journalRoot := filepath.Join(vaultDir, "memory", "journal")
	dateDirs, err := os.ReadDir(journalRoot)
	if err != nil {
		return 0
	}
	maxStep := 0
	prefix := convID + "-"
	for _, d := range dateDirs {
		if !d.IsDir() || d.Type()&os.ModeSymlink != 0 || d.Name() == "archive" {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(journalRoot, d.Name()))
		for _, f := range files {
			name := f.Name()
			if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
				continue
			}
			numStr := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")
			if n, err := strconv.Atoi(numStr); err == nil && d.Name() == todayDateStr && currentExecNum > 0 && n >= currentExecNum {
				continue
			}
			if b, err := os.ReadFile(filepath.Join(journalRoot, d.Name(), name)); err == nil {
				var je JournalEntry
				if json.Unmarshal(b, &je) == nil && je.StepCount > maxStep {
					maxStep = je.StepCount
				}
			}
		}
	}
	if maxStep == 0 {
		maxStep = vault.FindArchivedMaxStepCount(vaultDir, convID)
	}
	return maxStep
}

func detectNestedDescendant(vaultDir, convID, lastUserInput, fdConvID string, popMaxAge time.Duration) (bool, string, kernel.LSConversationLineage, bool) {
	if kernel.IsNestedSubagentConversation(vaultDir, convID) {
		return true, kernel.GetNestedConversationParent(vaultDir, convID), kernel.LSConversationLineage{}, false
	}
	lin, hasLS := kernel.InspectLSConversationLineage(vaultDir, convID)
	if hasLS && lin.IsNestedDescendant {
		_ = kernel.RecordNestedConversation(vaultDir, convID, lin.DirectParentID)
		return true, lin.DirectParentID, lin, true
	}
	if nestedSub, foundNested := kernel.PopPendingNestedSubagentForCaller(vaultDir, convID, lastUserInput, popMaxAge); foundNested {
		_ = kernel.RecordNestedConversation(vaultDir, convID, nestedSub.ParentConversationID)
		return true, nestedSub.ParentConversationID, lin, hasLS
	}
	if hasLS && lin.IsDirectFrontDeskSubagent {
		return false, "", lin, true
	}
	knownFDs := kernel.GetKnownFrontDeskIDSet(vaultDir)
	if fdID := strings.ToLower(strings.TrimSpace(fdConvID)); fdID != "" {
		knownFDs[fdID] = true
	}
	if edge, foundEdge := obs.FindSpawnEdgeInActiveSpawners(vaultDir, convID); foundEdge {
		ep := strings.ToLower(strings.TrimSpace(edge.ParentConversationID))
		if edge.TypeName == "run_workflow" || edge.TypeName == "agentapi" ||
			(len(knownFDs) > 0 && ep != "" && !knownFDs[ep]) {
			_ = kernel.RecordNestedConversation(vaultDir, convID, edge.ParentConversationID)
			return true, edge.ParentConversationID, lin, hasLS
		}
	}
	if hasAnc, ancParent := kernel.HasNonFrontDeskAncestry(vaultDir, convID, lin.DirectParentID); hasAnc {
		_ = kernel.RecordNestedConversation(vaultDir, convID, ancParent)
		return true, ancParent, lin, hasLS
	}
	return false, "", lin, hasLS
}

func buildSubagentTaskFromLSLineage(convID, payloadModel, status, summary string, lin kernel.LSConversationLineage) kernel.ActiveTask {
	modelName := strings.TrimSpace(lin.SubagentModel)
	if modelName == "" || modelName == "inherit" {
		modelName = payloadModel
	}
	role := strings.TrimSpace(lin.SubagentRole)
	if role == "" {
		role = strings.TrimSpace(lin.SourceTitle)
	}
	tier, scope, _, title, slug := kernel.ParseSubagentRole(role, modelName)
	nowISO := time.Now().Format(time.RFC3339)
	t := kernel.ActiveTask{
		TaskSlug:             slug,
		Title:                title,
		Kind:                 "subagent",
		Scope:                scope,
		ParentConversationID: lin.DirectParentID,
		Tier:                 tier,
		Model:                modelName,
		ConversationID:       convID,
		Status:               status,
		TurnCount:            1,
		CreatedAt:            nowISO,
		LastUpdatedAt:        nowISO,
		LastSummary:          strings.TrimSpace(summary),
	}
	if status == "completed" {
		t.CompletedAt = nowISO
		if t.LastSummary == "" {
			t.LastSummary = "Task completed."
		}
	}
	return t
}

func resolveParentConversationIDFromTasks(vaultDir, convID string, tasks []kernel.ActiveTask) string {
	if convID == "" {
		return ""
	}
	for _, t := range tasks {
		if t.ConversationID == convID && t.ParentConversationID != "" {
			return t.ParentConversationID
		}
	}
	return kernel.GetNestedConversationParent(vaultDir, convID)
}
