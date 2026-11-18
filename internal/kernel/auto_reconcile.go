package kernel

import (
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/events"
)

const (
	// DefaultAutoAckGracePeriod gives an active `[Auto]` worker that just ran `hq watch ack`
	// time to finish its turn normally via `HandleStop` before daemon reconciliation archives it.
	DefaultAutoAckGracePeriod   = 2 * time.Minute
	DefaultAutoSpawnGracePeriod = 5 * time.Second
)

func hasPendingResumableApprovalForConv(vaultDir, convID string) bool {
	convID = strings.TrimSpace(convID)
	if convID == "" {
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

func isAutoTaskReadyToArchive(vaultDir string, t ActiveTask, ackGrace time.Duration, now time.Time) (ready bool, ackedToComplete []string) {
	if now.IsZero() {
		now = time.Now()
	}
	if t.EffectiveKind() != "auto" || strings.ToLower(strings.TrimSpace(t.Status)) != "active" {
		return false, nil
	}
	convID := strings.TrimSpace(t.ConversationID)
	if hasPendingResumableApprovalForConv(vaultDir, convID) {
		return false, nil
	}
	updated := t.UpdatedTime()
	age := time.Duration(0)
	if !updated.IsZero() && now.After(updated) {
		age = now.Sub(updated)
	}
	if convID == "" && age < DefaultAutoSpawnGracePeriod {
		return false, nil
	}
	if len(t.EventIDs) == 0 {
		minAge := ackGrace
		if convID == "" || minAge < events.DefaultLeaseTTL {
			minAge = events.DefaultLeaseTTL
		}
		if age < minAge {
			return false, nil
		}
	}
	return events.AreEventIDsFinishedOrAcked(vaultDir, t.EventIDs, convID, ackGrace, now)
}

// ReconcileAutoTasks transitions stuck `active` `kind == "auto"` tasks to `archived` (or `completed` when pinned)
// and enqueues sidebar archive sync when their bound EventIDs have completed/expired/acked without a `Stop` hook firing,
// or when the Language Server reports an immediate 2-step CORTEX_STEP_TYPE_ERROR_MESSAGE executor failure.
func ReconcileAutoTasks(vaultDir string, now time.Time, dryRun bool) ([]string, error) {
	if now.IsZero() {
		now = time.Now()
	}
	tasks, err := ListTasks(vaultDir)
	if err != nil || len(tasks) == 0 {
		return nil, err
	}
	creds, hasLS := discoverLSCredentials(vaultDir)
	nowISO := now.UTC().Format(time.RFC3339)
	var reconciled []string
	for _, t := range tasks {
		ready, ackedIDs := isAutoTaskReadyToArchive(vaultDir, t, DefaultAutoAckGracePeriod, now)
		step1ErrMsg := ""
		sidebarErrReason := "auto_reconciled"
		if !ready && hasLS && t.EffectiveKind() == "auto" && strings.ToLower(strings.TrimSpace(t.Status)) == "active" {
			if convID := strings.TrimSpace(t.ConversationID); convID != "" && !hasPendingResumableApprovalForConv(vaultDir, convID) {
				if traj, supported, tErr := inspectCascadeTrajectory(creds.BaseURL, creds.CSRFToken, convID, 2*time.Second); supported && tErr == nil {
					if errMsg, failed := isImmediateStep1ExecutorError(traj); failed {
						ready = true
						step1ErrMsg = errMsg
						sidebarErrReason = "auto_step1_error"
					}
				}
			}
		}
		if !ready {
			continue
		}
		newStatus := "archived"
		if t.Pinned {
			newStatus = "completed"
		}
		if !dryRun {
			if step1ErrMsg != "" {
				if leasedEvs, lErr := events.ListLeasedByConversation(vaultDir, strings.TrimSpace(t.ConversationID)); lErr == nil && len(leasedEvs) > 0 {
					leasedIDs := make([]string, 0, len(leasedEvs))
					for _, ev := range leasedEvs {
						leasedIDs = append(leasedIDs, ev.ID)
					}
					newlyDropped, _ := events.DeferBatchOnSpawnError(vaultDir, leasedIDs, step1ErrMsg, isQuotaDeferralMessage(step1ErrMsg), now)
					emitDroppedEventAlerts(vaultDir, newlyDropped, now)
				}
			} else if len(ackedIDs) > 0 {
				_, _ = events.CompleteIDs(vaultDir, ackedIDs, now)
			}
			completedAt := t.CompletedAt
			if completedAt == "" {
				completedAt = nowISO
			}
			archivedAt := t.ArchivedAt
			if newStatus == "archived" && archivedAt == "" {
				archivedAt = nowISO
			}
			summary := t.LastSummary
			if step1ErrMsg != "" {
				summary = "Executor error at step 1: " + step1ErrMsg
			} else if strings.TrimSpace(summary) == "" {
				summary = "Auto task reconciled after event completion."
			}
			_ = RegisterTask(vaultDir, ActiveTask{
				TaskSlug:             t.TaskSlug,
				Title:                t.Title,
				Kind:                 "auto",
				Scope:                t.EffectiveScope(),
				ParentConversationID: t.ParentConversationID,
				Tier:                 t.Tier,
				Model:                t.Model,
				WorktreeDir:          t.WorktreeDir,
				WorktreeDirs:         t.WorktreeDirs,
				RepoRoot:             t.RepoRoot,
				ConversationID:       t.ConversationID,
				EventIDs:             t.EventIDs,
				Status:               newStatus,
				TurnCount:            t.TurnCount,
				Pinned:               t.Pinned,
				CreatedAt:            t.CreatedAt,
				LastUpdatedAt:        nowISO,
				CompletedAt:          completedAt,
				ArchivedAt:           archivedAt,
				SidebarArchived:      t.SidebarArchived,
				SidebarSyncedAt:      t.SidebarSyncedAt,
				LastSummary:          summary,
			})
			if newStatus == "archived" && !t.Pinned && !t.SidebarArchived && strings.TrimSpace(t.ConversationID) != "" {
				_ = EnqueueSidebarSync(vaultDir, SidebarSyncQueueItem{
					ConversationID:  t.ConversationID,
					TaskSlug:        t.TaskSlug,
					DesiredArchived: true,
					NextRetryAt:     now.Add(3 * time.Second).UTC().Format(time.RFC3339),
					LastError:       sidebarErrReason,
				}, now)
			}
		}
		reconciled = append(reconciled, t.TaskSlug)
	}
	return reconciled, nil
}
