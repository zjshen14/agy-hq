package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/channels"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/routing"
	"github.com/zjshen14/agy-hq/internal/taint"
	"github.com/zjshen14/agy-hq/internal/watcher"
)

// DeliverMessageToSession injects a user/system message into an existing Antigravity conversation
// (used for resumable follow-up timers, deferred human-approval continuations, and Front Desk alerts).
// It attempts LanguageServerService/SendUserCascadeMessage first when LS credentials are present,
// falling back to `agentapi send-message`.
func DeliverMessageToSession(vaultDir, sessionID, title, text string, dryRun bool) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("sessionID is required")
	}
	if strings.TrimSpace(title) == "" {
		title = "[HQ] Automated Continuation"
	}
	if dryRun {
		return fmt.Sprintf("[dry-run] send-message --title=%q %s", title, sessionID), nil
	}

	if creds, ok := discoverLSCredentials(vaultDir); ok {
		if sendErr := sendUserCascadeMessage(creds.BaseURL, creds.CSRFToken, sessionID, text, 3*time.Second); sendErr == nil {
			return fmt.Sprintf("delivered via LanguageServerService (%s)", sessionID), nil
		} else if _, lpErr := exec.LookPath("agentapi"); lpErr != nil {
			return "", sendErr
		}
	}

	cmd := exec.Command("agentapi", "send-message", "--title="+title, sessionID, text)
	configureSpawnCommand(cmd, vaultDir, "deliver-session:"+sessionID)
	outBytes, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(outBytes))
	if err != nil {
		return outStr, fmt.Errorf("agentapi send-message failed: %w (%s)", err, outStr)
	}
	if outStr == "" {
		outStr = fmt.Sprintf("delivered via agentapi (%s)", sessionID)
	}
	return outStr, nil
}

// RemoveUnboundTaskBySlug removes any pre-registered active task matching slug that has not bound a ConversationID yet.
func RemoveUnboundTaskBySlug(vaultDir, slug string) {
	if slug == "" {
		return
	}
	_ = UpdateTasks(vaultDir, func(tasks []ActiveTask) ([]ActiveTask, bool, error) {
		kept := make([]ActiveTask, 0, len(tasks))
		changed := false
		for _, t := range tasks {
			if t.TaskSlug == slug && t.ConversationID == "" {
				changed = true
				continue
			}
			kept = append(kept, t)
		}
		return kept, changed, nil
	})
}

func removeTaskBySlug(vaultDir, slug string) {
	RemoveUnboundTaskBySlug(vaultDir, slug)
}

func isQuotaDeferralError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "quota") ||
		strings.Contains(msg, "budget") ||
		strings.Contains(msg, "circuit breaker") ||
		strings.Contains(msg, "429") ||
		strings.Contains(msg, "resource_exhausted")
}

func batchHeadEvent(batch events.LeasedBatch) events.Event {
	if len(batch.Events) > 0 {
		return batch.Events[0]
	}
	return events.Event{ID: batch.LeaseID, Source: "event"}
}

func batchActionLabel(batch events.LeasedBatch) string {
	head := batchHeadEvent(batch)
	if head.Metadata != nil {
		if lbl := strings.TrimSpace(head.Metadata["action_label"]); lbl != "" {
			if len(batch.Events) > 1 {
				return fmt.Sprintf("%s (+%d batched)", lbl, len(batch.Events)-1)
			}
			return lbl
		}
	}
	if batch.SessionID != "" {
		return fmt.Sprintf("%s -> session:%s", head.Source, batch.SessionID)
	}
	if batch.Skill != "" {
		return fmt.Sprintf("%s -> /%s", head.Source, batch.Skill)
	}
	return fmt.Sprintf("event:%s (%s)", head.ID, head.Source)
}

func batchTriggerName(batch events.LeasedBatch) string {
	head := batchHeadEvent(batch)
	if head.Metadata != nil {
		if trig := strings.TrimSpace(head.Metadata["trigger"]); trig != "" {
			return trig
		}
	}
	return fmt.Sprintf("event:%s:%s", head.Source, head.ID)
}

func batchItemKeys(evs []events.Event) []string {
	var keys []string
	for _, ev := range evs {
		if ev.Metadata != nil {
			if ik := strings.TrimSpace(ev.Metadata["item_key"]); ik != "" {
				keys = append(keys, ik)
			}
			if tk := strings.TrimSpace(ev.Metadata["thread_key"]); tk != "" {
				keys = append(keys, tk)
			}
			for _, cid := range strings.Split(ev.Metadata["coalesced_item_ids"], ",") {
				if cid = strings.TrimSpace(cid); cid != "" {
					keys = append(keys, cid)
				}
			}
		}
	}
	return keys
}

func headMetaVal(ev events.Event, key string) string {
	if ev.Metadata == nil {
		return ""
	}
	return strings.TrimSpace(ev.Metadata[key])
}

func emitDroppedEventAlerts(vaultDir string, dropped []events.Event, now time.Time) {
	dropped = watcher.RecordPoisonedDrops(vaultDir, dropped, now)
	for _, d := range dropped {
		skillName := d.Skill
		if skillName == "" {
			skillName = d.Source
		}
		_, _, _ = outbox.Add(vaultDir, outbox.OutboxItem{
			Tier:        outbox.TierBriefRollup,
			SourceSkill: skillName,
			Scope:       d.ScopeKey,
			Title:       fmt.Sprintf("Background event dropped after %d failed attempts: %s", d.AttemptCount, d.Title),
			Summary:     fmt.Sprintf("Event %s (source=%s, scope=%s) exhausted retry budget. Last error: %s", d.ID, d.Source, d.ScopeKey, d.LastError),
		}, now)
	}
}

// HasMaterialDispatchActions returns true when at least one returned dispatch action represents
// an actual delivery or spawn rather than a "(deferred: ...)" hold, avoiding idle console sync thrash.
func HasMaterialDispatchActions(actions []string) bool {
	for _, a := range actions {
		if !strings.Contains(a, " (deferred: ") {
			return true
		}
	}
	return false
}

// DispatchPendingEvents leases ready event batches from local/memory/events.json (enforcing
// single-active-worker per ScopeKey and crash-lease expiry reaping) and dispatches them either
// by waking the target SessionID or spawning a fresh background worker.
func DispatchPendingEvents(vaultDir string, now time.Time, dryRun bool) ([]string, error) {
	if now.IsZero() {
		now = time.Now()
	}
	batches, dropped, err := events.LeaseNextBatches(vaultDir, now, events.DefaultLeaseTTL)
	if err != nil {
		return nil, err
	}
	emitDroppedEventAlerts(vaultDir, dropped, now)

	var actions []string
	for _, batch := range batches {
		label := batchActionLabel(batch)
		ids := batch.EventIDs()
		head := batchHeadEvent(batch)

		if batch.SessionID == "" && head.Source == "channel_inbound" {
			if fd, ok := GetFrontDesk(vaultDir); ok && strings.TrimSpace(fd.ConversationID) != "" {
				batch.SessionID = strings.TrimSpace(fd.ConversationID)
			}
		}

		if batch.SessionID != "" {
			out, delivErr := DeliverMessageToSession(vaultDir, batch.SessionID, batch.Title, batch.Prompt, dryRun)
			if delivErr != nil {
				if headMetaVal(head, "schedule_mode") == "front_desk_cron" && headMetaVal(head, "scheduled_tick") != "1" {
					// Fall back to an isolated [Auto] spawn only for explicit manual fires when direct Front Desk delivery failed.
					batch.SessionID = ""
				} else {
					newlyDropped, _ := events.DeferBatchOnSpawnError(vaultDir, ids, delivErr.Error(), false, now)
					emitDroppedEventAlerts(vaultDir, newlyDropped, now)
					actions = append(actions, fmt.Sprintf("%s (deferred: %v)", label, delivErr))
					continue
				}
			} else {
				// Once DeliverMessageToSession succeeds (HTTP 2xx from LanguageServerService or exit 0
				// from agentapi send-message), the continuation is queued in the session's input stream;
				// complete the event batch immediately so subsequent follow-ups/approvals are not blocked
				// and the message is never duplicate-sent after lease expiry.
				_, _ = events.CompleteIDs(vaultDir, ids, now)
				actions = append(actions, fmt.Sprintf("%s (%s)", label, out))
				continue
			}
		}

		trigger := batchTriggerName(batch)
		autoSlug := "auto-" + slugify(batch.ScopeKey+"-"+head.ID)
		nowStr := now.UTC().Format(time.RFC3339)

		effectiveModel := batch.Model
		if strings.HasPrefix(head.Source, "connector:") {
			effTier := "quick"
			if len(batch.Events) >= 4 {
				effTier = "routine"
			} else {
				for _, bev := range batch.Events {
					if bev.Metadata != nil && bev.Metadata["tier"] == "routine" {
						effTier = "routine"
						break
					}
				}
			}
			effectiveModel = routing.Resolve(vaultDir, routing.Request{
				Prompt:       batch.Prompt,
				ExplicitTier: effTier,
			}).ModelID
		}

		if !dryRun {
			qState := LoadQuota(vaultDir)
			autoUsed := qState.EffectiveAutoSpawns()
			if qState.CircuitOpen || (qState.MaxDailySpawns > 0 && autoUsed >= qState.MaxDailySpawns) {
				reason := qState.CircuitReason
				if reason == "" {
					reason = fmt.Sprintf("daily spawn budget exhausted (%d/%d)", autoUsed, qState.MaxDailySpawns)
				}
				newlyDropped, _ := events.DeferBatchOnSpawnError(vaultDir, ids, "quota circuit breaker is OPEN: "+reason, true, now)
				emitDroppedEventAlerts(vaultDir, newlyDropped, now)
				actions = append(actions, fmt.Sprintf("%s (deferred: quota circuit breaker is OPEN: %s)", label, reason))
				continue
			}
		}

		var prevTask ActiveTask
		hadPrev := false
		if !dryRun && head.Source != "channel_inbound" {
			if existingTasks, lErr := ListTasks(vaultDir); lErr == nil {
				for _, et := range existingTasks {
					if et.TaskSlug == autoSlug {
						prevTask = et
						hadPrev = true
						break
					}
				}
			}
			if !hadPrev {
				_ = RegisterTask(vaultDir, ActiveTask{
					TaskSlug:      autoSlug,
					Title:         batch.Title,
					Kind:          "auto",
					Scope:         batch.ScopeKey,
					Tier:          inferTierFromModel(effectiveModel),
					Model:         effectiveModel,
					EventIDs:      ids,
					CreatedAt:     nowStr,
					LastUpdatedAt: nowStr,
					Status:        "active",
				})
			}
		}

		if !dryRun && head.Source == "channel_inbound" {
			// Clear .vacated BEFORE SpawnTask so SessionStart during Turn 1 immediately activates [HQ] Front Desk Coordinator!
			_ = os.Remove(filepath.Join(vaultDir, "memory", ".vacated"))
			if strings.TrimSpace(effectiveModel) == "" {
				effectiveModel = routing.Resolve(vaultDir, routing.Request{ExplicitTier: "routine"}).ModelID
			}
		}

		spawnPrompt := batch.Prompt
		if batch.SpawnToken != "" {
			spawnPrompt = spawnPrompt + "\n\n[hq-spawn:" + batch.SpawnToken + "]"
		}

		out, spawnErr := SpawnTask(vaultDir, trigger, batch.Skill, batch.Title, spawnPrompt, effectiveModel, dryRun)
		if spawnErr != nil {
			if !dryRun && !hadPrev && head.Source != "channel_inbound" {
				removeTaskBySlug(vaultDir, autoSlug)
			}
			newlyDropped, _ := events.DeferBatchOnSpawnError(vaultDir, ids, spawnErr.Error(), isQuotaDeferralError(spawnErr), now)
			emitDroppedEventAlerts(vaultDir, newlyDropped, now)
			actions = append(actions, fmt.Sprintf("%s (deferred: %v)", label, spawnErr))
			continue
		}

		if dryRun {
			_, _ = events.CompleteIDs(vaultDir, ids, now)
		} else if uuid := ExtractUUID(out); uuid != "" {
			if head.Source == "channel_inbound" {
				_ = os.Remove(filepath.Join(vaultDir, "memory", ".vacated"))
				_ = SetFrontDesk(vaultDir, uuid, "")
				channels.BindVacantCorrelations(vaultDir, uuid, now)
				_, _ = events.CompleteIDs(vaultDir, ids, now)
				actions = append(actions, fmt.Sprintf("%s (bootstrapped Front Desk %s)", label, uuid))
				continue
			}
			if hadPrev && prevTask.ConversationID != "" && prevTask.ConversationID != uuid && !prevTask.Pinned {
				_ = EnqueueSidebarSync(vaultDir, SidebarSyncQueueItem{
					ConversationID:  prevTask.ConversationID,
					TaskSlug:        autoSlug,
					DesiredArchived: true,
					NextRetryAt:     now.Add(3 * time.Second).UTC().Format(time.RFC3339),
					LastError:       "superseded_by_retry",
				}, now)
			}
			_ = RegisterTask(vaultDir, ActiveTask{
				TaskSlug:       autoSlug,
				Title:          batch.Title,
				Kind:           "auto",
				Scope:          batch.ScopeKey,
				Tier:           inferTierFromModel(effectiveModel),
				Model:          effectiveModel,
				EventIDs:       ids,
				ConversationID: uuid,
				LastUpdatedAt:  time.Now().UTC().Format(time.RFC3339),
				Status:         "active",
			})
			_ = events.BindBatchConversation(vaultDir, ids, uuid, now, events.DefaultLeaseTTL)
			if batch.TaintOnSpawn {
				_, _ = taint.Mark(context.Background(), vaultDir, taint.Record{
					ConvID:    uuid,
					Source:    "spawn_batch:" + head.Source,
					Connector: headMetaVal(head, "connector"),
					Account:   headMetaVal(head, "account"),
					BatchID:   batch.LeaseID,
					EventIDs:  ids,
					ItemKeys:  batchItemKeys(batch.Events),
				})
			}
		}
		actions = append(actions, fmt.Sprintf("%s (%s)", label, out))
	}
	return actions, nil
}

// CompleteConversationEvents marks all leased events bound to convID as completed in local/memory/events_archive.jsonl
// (requeuing unacknowledged watcher events), and moves any completed `inbox_drop` files from `local/inbox/queued/` to `local/inbox/processed/`.
func CompleteConversationEvents(vaultDir, convID string, now time.Time) (int, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return 0, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	completed, _, dropped, err := events.FinalizeByConversation(vaultDir, convID, now)
	if err != nil {
		return 0, err
	}
	emitDroppedEventAlerts(vaultDir, dropped, now)
	finalizeCompletedInboxFiles(vaultDir, completed, now)
	return len(completed), nil
}

func finalizeCompletedInboxFiles(vaultDir string, completed []events.Event, now time.Time) {
	if len(completed) == 0 {
		return
	}
	processedDir := filepath.Join(vaultDir, "inbox", "processed")
	datePrefix := now.UTC().Format("2006-01-02")
	for _, ev := range completed {
		if ev.Metadata == nil {
			continue
		}
		qPath := strings.TrimSpace(ev.Metadata["queued_path"])
		if qPath == "" {
			continue
		}
		if _, err := os.Stat(qPath); err != nil {
			continue
		}
		if err := os.MkdirAll(processedDir, 0o755); err != nil {
			continue
		}
		dst := filepath.Join(processedDir, fmt.Sprintf("%s_%s", datePrefix, filepath.Base(qPath)))
		_ = os.Rename(qPath, dst)
	}
}

func fileContentDigest16(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		if st, sErr := os.Stat(path); sErr == nil {
			sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", filepath.Base(path), st.Size(), st.ModTime().UnixNano())))
			return hex.EncodeToString(sum[:8])
		}
		return "unknown"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// EnqueueInboxFiles moves newly dropped files in local/inbox/ to local/inbox/queued/,
// scans local/inbox/queued/ for any stranded files lacking an active or recently completed
// event record, and enqueues them into the durable Event Inbox.
func EnqueueInboxFiles(vaultDir string, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	inboxDir := filepath.Join(vaultDir, "inbox")
	queuedDir := filepath.Join(inboxDir, "queued")

	if entries, err := os.ReadDir(inboxDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if mkErr := os.MkdirAll(queuedDir, 0o755); mkErr != nil {
				return mkErr
			}
			srcPath := filepath.Join(inboxDir, e.Name())
			dstPath := filepath.Join(queuedDir, e.Name())
			if renErr := os.Rename(srcPath, dstPath); renErr != nil {
				continue
			}
			enqueueQueuedInboxFile(vaultDir, dstPath, e.Name(), now)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	// Reconcile any stranded files in local/inbox/queued/ that were orphaned prior to crash/circuit-open
	if qEntries, qErr := os.ReadDir(queuedDir); qErr == nil {
		for _, qe := range qEntries {
			if qe.IsDir() || strings.HasPrefix(qe.Name(), ".") {
				continue
			}
			qPath := filepath.Join(queuedDir, qe.Name())
			dedupKey := fmt.Sprintf("inbox_drop:%s:%s", qe.Name(), fileContentDigest16(qPath))
			if events.HasActiveOrCompletedDedup(vaultDir, dedupKey, now) {
				continue
			}
			enqueueQueuedInboxFile(vaultDir, qPath, qe.Name(), now)
		}
	}
	return nil
}

func enqueueQueuedInboxFile(vaultDir, dstPath, name string, now time.Time) {
	skill := ClassifyInboxFileForVault(vaultDir, name)
	title := fmt.Sprintf("[Auto] Inbox Drop: %s (/%s)", name, skill)
	prompt := fmt.Sprintf("A new file was dropped into the inbox and moved to %s. Activate the %s skill and process this file.", dstPath, skill)
	modelID := routing.Resolve(vaultDir, routing.Request{Prompt: prompt, ExplicitTier: "standard"}).ModelID
	dedupKey := fmt.Sprintf("inbox_drop:%s:%s", name, fileContentDigest16(dstPath))

	_, _, _ = events.Enqueue(vaultDir, events.Event{
		Source:   "inbox_drop",
		DedupKey: dedupKey,
		ScopeKey: fmt.Sprintf("inbox:%s:%s", skill, name),
		Skill:    skill,
		Model:    modelID,
		Title:    title,
		Prompt:   prompt,
		Metadata: map[string]string{
			"trigger":      "inbox-drop:" + name,
			"action_label": fmt.Sprintf("%s -> /%s", name, skill),
			"queued_path":  dstPath,
		},
	}, now)
}

// DispatchOutboxToFrontDesk delivers pending `interrupt` outbox notifications to the active
// `[HQ] Front Desk` conversation when leased and reachable, persisting anti-repetition records
// in `local/memory/reported.jsonl` and retrying with exponential backoff on transient failures.
func DispatchOutboxToFrontDesk(vaultDir string, now time.Time, dryRun bool) (int, error) {
	if now.IsZero() {
		now = time.Now()
	}
	fd, ok := GetFrontDesk(vaultDir)
	if !ok || strings.TrimSpace(fd.ConversationID) == "" {
		_ = channels.EnqueuePendingInterruptsWhenDeskVacant(vaultDir, now)
		fRep, _ := channels.SyncAndFlush(context.Background(), vaultDir, now, dryRun)
		return fRep.NotificationsSent, nil
	}
	delivered, err := outbox.DispatchPendingInterrupts(vaultDir, now, func(it outbox.OutboxItem, formattedCard string) error {
		title := "[HQ Interrupt] " + it.Title
		_, dErr := DeliverMessageToSession(vaultDir, fd.ConversationID, title, formattedCard, dryRun)
		return dErr
	})
	if len(delivered) > 0 {
		_ = channels.EnqueueOutboxInterrupts(vaultDir, fd.ConversationID, delivered, now)
	}
	if outbox.InQuietHours(outbox.LoadAttentionConfig(vaultDir), now) {
		_ = channels.EnqueuePendingInterruptsForQuietExemptBridges(vaultDir, fd.ConversationID, now)
	}
	_, _ = channels.SyncAndFlush(context.Background(), vaultDir, now, dryRun)
	return len(delivered), err
}
