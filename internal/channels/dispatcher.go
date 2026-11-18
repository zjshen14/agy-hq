package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/outbox"
)

func init() {
	approvals.RegisteredProposalCreatedHook = func(vaultDir string, prop approvals.Proposal) {
		if strings.TrimSpace(vaultDir) == "" {
			return
		}
		_ = EnqueueTaskBlocked(vaultDir, prop, time.Now())
	}
}

var enqueueGen atomic.Uint64

func bumpEnqueueGeneration() {
	enqueueGen.Add(1)
}

type fastSyncMemo struct {
	mu              sync.Mutex
	vaultDir        string
	fdModUnixNano   int64
	fdSize          int64
	fdExists        bool
	seenGen         uint64
	nextDueAt       time.Time
	hasNextDue      bool
	lastCheckedWall time.Time
}

var daemonSyncMemo fastSyncMemo

type frontDeskDiskSnapshot struct {
	ConversationID string `json:"conversationId"`
	ArtifactDir    string `json:"artifactDir,omitempty"`
	ProjectID      string `json:"projectId,omitempty"`
	WorkspaceDir   string `json:"workspaceDir,omitempty"`
	UpdatedAt      string `json:"updatedAt,omitempty"`
}

func readFrontDeskDiskSnapshot(vaultDir string) (frontDeskDiskSnapshot, bool) {
	var snap frontDeskDiskSnapshot
	data, err := os.ReadFile(filepath.Join(vaultDir, "memory", "front_desk.json"))
	if err != nil {
		return snap, false
	}
	if json.Unmarshal(data, &snap) != nil || strings.TrimSpace(snap.ConversationID) == "" {
		return snap, false
	}
	return snap, true
}

func frontDeskStatFingerprint(vaultDir string) (bool, int64, int64) {
	st, err := os.Stat(filepath.Join(vaultDir, "memory", "front_desk.json"))
	if err != nil {
		return false, 0, 0
	}
	return true, st.ModTime().UnixNano(), st.Size()
}

// FlushReport summarizes one SyncAndFlush execution pass.
type FlushReport struct {
	BindsSent         int
	NotificationsSent int
	RetriedLater      int
	DroppedPermanent  int
}

type claimedBridgeWork struct {
	Bridge      ResolvedBridge
	Bind        *PendingDeskBind
	DeskThread  DeskThreadRef
	OpenThreads []OpenItemThreadRef
	Items       []QueuedNotification
	ItemLookup  map[string]string // itemKey -> existing external_thread_id
	DeskCID     string
	InQuiet     bool
}

type rollupNotice struct {
	SourceSkill string
	Scope       string
	Title       string
	Summary     string
}

func isClaimExpired(claimUntil string, now time.Time) bool {
	claimUntil = strings.TrimSpace(claimUntil)
	if claimUntil == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, claimUntil)
	if err != nil {
		return true
	}
	return !now.Before(t)
}

func isAttemptDue(nextAttemptAt string, now time.Time) bool {
	nextAttemptAt = strings.TrimSpace(nextAttemptAt)
	if nextAttemptAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, nextAttemptAt)
	if err != nil {
		return true
	}
	return !now.Before(t)
}

func nextWakeAfterQuietHours(attCfg outbox.AttentionConfig, now time.Time) time.Time {
	// Re-evaluate every 5 minutes during quiet hours so timezone/config changes or quiet-hours exit flush promptly.
	_ = attCfg
	return now.Add(5 * time.Minute)
}

func workspaceDisplayName(workspaceDir string) string {
	w := strings.TrimSpace(workspaceDir)
	if w == "" {
		return ""
	}
	return filepath.Base(filepath.Clean(w))
}

// SyncAndFlush detects `front_desk.json` `ConversationID` rotations/vacations and drains due
// per-bridge retry queues using a 3-phase lock-safe pipeline (`RankChannels = 65`).
func SyncAndFlush(ctx context.Context, vaultDir string, now time.Time, dryRun bool) (FlushReport, error) {
	var rep FlushReport
	if strings.TrimSpace(vaultDir) == "" {
		return rep, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now()
	}

	fdExists, fdMod, fdSize := frontDeskStatFingerprint(vaultDir)
	curGen := enqueueGen.Load()

	daemonSyncMemo.mu.Lock()
	canSkip := daemonSyncMemo.vaultDir == vaultDir &&
		daemonSyncMemo.fdExists == fdExists &&
		daemonSyncMemo.fdModUnixNano == fdMod &&
		daemonSyncMemo.fdSize == fdSize &&
		daemonSyncMemo.seenGen == curGen &&
		!daemonSyncMemo.lastCheckedWall.IsZero() &&
		now.Sub(daemonSyncMemo.lastCheckedWall) < 60*time.Second &&
		(!daemonSyncMemo.hasNextDue || now.Before(daemonSyncMemo.nextDueAt))
	daemonSyncMemo.mu.Unlock()
	if canSkip {
		return rep, nil
	}

	bridges, err := DiscoverBridges(vaultDir)
	if err != nil || len(bridges) == 0 {
		daemonSyncMemo.mu.Lock()
		daemonSyncMemo.vaultDir = vaultDir
		daemonSyncMemo.fdExists = fdExists
		daemonSyncMemo.fdModUnixNano = fdMod
		daemonSyncMemo.fdSize = fdSize
		daemonSyncMemo.seenGen = curGen
		daemonSyncMemo.hasNextDue = false
		daemonSyncMemo.lastCheckedWall = now
		daemonSyncMemo.mu.Unlock()
		return rep, err
	}

	attCfg := outbox.LoadAttentionConfig(vaultDir)
	inQuiet := outbox.InQuietHours(attCfg, now)
	fd, hasFD := readFrontDeskDiskSnapshot(vaultDir)
	curDeskCID := ""
	curLeaseState := LeaseStateVacated
	if hasFD {
		curDeskCID = strings.TrimSpace(fd.ConversationID)
		curLeaseState = LeaseStateActive
	}

	nowStr := now.UTC().Format(time.RFC3339)
	claimUntilStr := now.Add(DispatchClaimTTL).UTC().Format(time.RFC3339)

	// Phase A (under RankChannels = 65): detect desk rotation, auto-clear recovered pauses, and claim due work.
	var claimed []claimedBridgeWork
	var rollups []rollupNotice
	var earliestDue time.Time
	hasEarliestDue := false
	noteDue := func(ts string) {
		if ts == "" {
			return
		}
		if t, pErr := time.Parse(time.RFC3339, ts); pErr == nil {
			if !hasEarliestDue || t.Before(earliestDue) {
				earliestDue = t
				hasEarliestDue = true
			}
		}
	}

	err = WithChannelsLock(ctx, vaultDir, func(_ context.Context) error {
		for _, rb := range bridges {
			if !rb.EffectiveEnabled() {
				continue
			}
			st, lErr := loadBridgeStateUnlocked(vaultDir, rb.FQID)
			if lErr != nil && !os.IsNotExist(lErr) {
				continue
			}
			dirty := false

			// Auto-recover paused_auth / paused_permanent when credential file or private config changes.
			if st.Health.Status == HealthStatusPausedAuth {
				if rb.PrivateCfg.CredFingerprint != st.Health.CredFingerprintAtPause || rb.PrivateCfg.ConfigSHA256 != st.Health.ConfigSHA256AtPause {
					st.Health.Status = HealthStatusHealthy
					st.Health.PausedReason = ""
					st.Health.ConsecutiveErrors = 0
					dirty = true
				} else {
					continue
				}
			}
			if st.Health.Status == HealthStatusPausedPermanent {
				if rb.PrivateCfg.ConfigSHA256 != st.Health.ConfigSHA256AtPause {
					st.Health.Status = HealthStatusHealthy
					st.Health.PausedReason = ""
					st.Health.ConsecutiveErrors = 0
					dirty = true
				} else {
					continue
				}
			}

			// Detect Front Desk rotation (`"" -> cid1`, `cid1 -> cid2`, `cid1 -> ""` vacated).
			if !st.LastDeskInitialized && !hasFD {
				st.LastDeskInitialized = true
				st.LastDeskCID = ""
				st.LastDeskLeaseState = LeaseStateVacated
				dirty = true
			} else if rb.Spec.Capabilities.DeskBinding && (!st.LastDeskInitialized || st.LastDeskCID != curDeskCID || st.LastDeskLeaseState != curLeaseState) {
				alreadyQueuedSame := st.PendingBind != nil &&
					st.PendingBind.ConversationID == curDeskCID &&
					st.PendingBind.LeaseState == curLeaseState
				if !alreadyQueuedSame {
					prevShort := st.LastDeskCID
					if len(prevShort) > 8 {
						prevShort = prevShort[:8]
					}
					curShort := curDeskCID
					if len(curShort) > 8 {
						curShort = curShort[:8]
					}
					updAt := fd.UpdatedAt
					if updAt == "" {
						updAt = nowStr
					}
					st.PendingBind = &PendingDeskBind{
						IdempotencyKey:         sanitizeIdempotencyKey(fmt.Sprintf("bind-desk:%s:%s:%s:%s", rb.FQID, prevShort, curShort, updAt)),
						LeaseState:             curLeaseState,
						ConversationID:         curDeskCID,
						PreviousConversationID: st.LastDeskCID,
						ProjectID:              fd.ProjectID,
						WorkspaceName:          workspaceDisplayName(fd.WorkspaceDir),
						UpdatedAt:              updAt,
					}
					dirty = true
				}
			}

			work := claimedBridgeWork{
				Bridge:     rb,
				DeskThread: st.DeskThread,
				ItemLookup: make(map[string]string),
				DeskCID:    curDeskCID,
				InQuiet:    inQuiet && rb.PrivateCfg.ResolvedPolicy.RespectQuietHours,
			}

			st.ItemThreads = pruneItemThreads(st.ItemThreads, now)
			for _, it := range st.ItemThreads {
				work.ItemLookup[it.ItemKey] = it.ExternalThreadID
				if it.Status != "closed" && len(work.OpenThreads) < 32 {
					work.OpenThreads = append(work.OpenThreads, OpenItemThreadRef{
						ItemKey:          it.ItemKey,
						Kind:             it.Kind,
						ExternalThreadID: it.ExternalThreadID,
						Status:           "open",
					})
				}
			}

			if st.PendingBind != nil {
				if !isClaimExpired(st.PendingBind.DispatchClaimUntil, now) {
					noteDue(st.PendingBind.DispatchClaimUntil)
				} else if !isAttemptDue(st.PendingBind.NextAttemptAt, now) {
					noteDue(st.PendingBind.NextAttemptAt)
				} else {
					st.PendingBind.DispatchClaimUntil = claimUntilStr
					cp := *st.PendingBind
					work.Bind = &cp
					dirty = true
				}
			}

			keptQueue := make([]QueuedNotification, 0, len(st.RetryQueue))
			for i := range st.RetryQueue {
				q := st.RetryQueue[i]
				if hasSeenKey(st.RecentDeliveredKeys, q.IdempotencyKey, now) {
					dirty = true
					continue
				}
				if !isClaimExpired(q.DispatchClaimUntil, now) {
					noteDue(q.DispatchClaimUntil)
					keptQueue = append(keptQueue, q)
					continue
				}
				q.DispatchClaimUntil = ""
				if !isAttemptDue(q.NextAttemptAt, now) {
					noteDue(q.NextAttemptAt)
					keptQueue = append(keptQueue, q)
					continue
				}

				disp := EvaluateNotificationPolicy(q, rb.PrivateCfg.ResolvedPolicy, attCfg, &st.RateWindow, now)
				switch disp {
				case DispositionDropDisabled:
					dirty = true
					continue
				case DispositionHoldQuiet:
					q.NextAttemptAt = nextWakeAfterQuietHours(attCfg, now).UTC().Format(time.RFC3339)
					noteDue(q.NextAttemptAt)
					keptQueue = append(keptQueue, q)
					dirty = true
					continue
				case DispositionRollupQuiet, DispositionRollupRate:
					rollups = append(rollups, rollupNotice{
						SourceSkill: "channels",
						Scope:       rb.FQID,
						Title:       fmt.Sprintf("Coalesced channel digest (%s): %s", disp, q.Notification.Title),
						Summary:     q.Notification.BodyMarkdown,
					})
					dirty = true
					continue
				case DispositionSendNow:
					q.DispatchClaimUntil = claimUntilStr
					work.Items = append(work.Items, q)
					keptQueue = append(keptQueue, q)
					dirty = true
				}
			}
			st.RetryQueue = keptQueue
			if dirty {
				_ = saveBridgeStateUnlocked(vaultDir, st, now)
			}
			if work.Bind != nil || len(work.Items) > 0 {
				claimed = append(claimed, work)
			}
		}
		return nil
	})
	if err != nil {
		return rep, err
	}

	// Phase B (unlocked): execute `bind-desk` and `notify` subprocesses without holding RankChannels.
	type bindOutcome struct {
		resp      BindDeskResponse
		err       error
		pErr      *ProtocolError
		latencyMs int64
		payHash   string
	}
	type itemOutcome struct {
		key        string
		resp       NotifyResponse
		err        error
		pErr       *ProtocolError
		latencyMs  int64
		payHash    string
		sentThread string
	}
	type bridgeOutcomes struct {
		work    claimedBridgeWork
		bindOut *bindOutcome
		items   map[string]itemOutcome
	}

	results := make([]bridgeOutcomes, 0, len(claimed))
	for _, w := range claimed {
		bo := bridgeOutcomes{
			work:  w,
			items: make(map[string]itemOutcome, len(w.Items)),
		}
		hasCred := strings.TrimSpace(w.Bridge.PrivateCfg.Config.CredentialFile) != ""
		activeDeskThreadID := strings.TrimSpace(w.DeskThread.ExternalThreadID)

		if w.Bind != nil {
			var dtPtr *DeskThreadRef
			if activeDeskThreadID != "" {
				cp := w.DeskThread
				dtPtr = &cp
			}
			var openItems []OpenItemThreadRef
			if w.Bridge.Spec.Capabilities.ItemThreads {
				openItems = w.OpenThreads
			}
			req := BindDeskRequest{
				Protocol:         ProtocolV1,
				Op:               OpBindDesk,
				BridgeID:         w.Bridge.FQID,
				IdempotencyKey:   w.Bind.IdempotencyKey,
				Timestamp:        nowStr,
				HasCredentialFD3: hasCred,
				Silent:           w.InQuiet,
				Destination:      w.Bridge.PrivateCfg.Config.Destination,
				Desk: DeskLeasePayload{
					LeaseState:             w.Bind.LeaseState,
					ConversationID:         w.Bind.ConversationID,
					PreviousConversationID: w.Bind.PreviousConversationID,
					ProjectID:              w.Bind.ProjectID,
					WorkspaceName:          w.Bind.WorkspaceName,
					UpdatedAt:              w.Bind.UpdatedAt,
				},
				DeskThread:      dtPtr,
				OpenItemThreads: openItems,
			}
			reqBytes, _ := json.Marshal(req)
			pHash := sha256HexBytes(reqBytes)

			if dryRun {
				bo.bindOut = &bindOutcome{
					resp:    BindDeskResponse{Protocol: ProtocolV1, OK: true, DeskThread: &BoundDeskThread{ExternalThreadID: activeDeskThreadID}},
					payHash: pHash,
				}
			} else {
				t0 := time.Now()
				stdout, _, runErr := InvokeBridge(ctx, vaultDir, w.Bridge.PackID, w.Bridge.PackDir, w.Bridge.Spec, w.Bridge.PrivateCfg.Config.CredentialFile, req)
				lat := time.Since(t0).Milliseconds()
				var parsed BindDeskResponse
				if len(stdout) > 0 {
					_ = json.Unmarshal(stdout, &parsed)
				}
				var pErr *ProtocolError
				if !parsed.OK && parsed.Error != nil {
					pErr = parsed.Error
				}
				if runErr == nil && !parsed.OK && pErr == nil {
					runErr = fmt.Errorf("bridge returned ok=false on bind-desk")
				}
				if runErr == nil && parsed.OK && parsed.DeskThread != nil && strings.TrimSpace(parsed.DeskThread.ExternalThreadID) != "" {
					activeDeskThreadID = strings.TrimSpace(parsed.DeskThread.ExternalThreadID)
				}
				bo.bindOut = &bindOutcome{
					resp:      parsed,
					err:       runErr,
					pErr:      pErr,
					latencyMs: lat,
					payHash:   pHash,
				}
			}
		}

		for _, q := range w.Items {
			sanitized := SanitizeAndFormatNotification(q.Notification, w.Bridge.PrivateCfg.ResolvedPolicy)
			if w.Bridge.Spec.Capabilities.CallbackButtons &&
				w.Bridge.PrivateCfg.ResolvedPolicy.PrivacyMode != PrivacyModeZeroBody &&
				len(sanitized.ProposalIDs) > 0 && !dryRun {
				if acts, mErr := MintProposalActionTokens(vaultDir, w.Bridge.FQID, sanitized.ProposalIDs, now); mErr == nil && len(acts) > 0 {
					sanitized.Actions = acts
				}
			}

			hint := ThreadHint{
				Mode:                 ThreadModeDeskThread,
				DeskExternalThreadID: activeDeskThreadID,
			}
			if q.Notification.Kind == KindDeskReply && strings.TrimSpace(q.PreferredThreadID) != "" {
				if q.ItemKey != "" && w.Bridge.Spec.Capabilities.ItemThreads {
					hint.Mode = ThreadModeItemThread
					hint.ItemKey = q.ItemKey
					hint.ExistingExternalThreadID = q.PreferredThreadID
				} else {
					hint.DeskExternalThreadID = q.PreferredThreadID
				}
			} else if q.ItemKey != "" &&
				w.Bridge.Spec.Capabilities.ItemThreads &&
				w.Bridge.PrivateCfg.ThreadMode == ConfigThreadModeGrouped {
				hint.Mode = ThreadModeItemThread
				hint.ItemKey = q.ItemKey
				hint.ExistingExternalThreadID = w.ItemLookup[q.ItemKey]
			}

			deskCID := q.DeskConversationID
			if deskCID == "" {
				deskCID = w.DeskCID
			}
			req := NotifyRequest{
				Protocol:           ProtocolV1,
				Op:                 OpNotify,
				BridgeID:           w.Bridge.FQID,
				IdempotencyKey:     q.IdempotencyKey,
				Timestamp:          nowStr,
				HasCredentialFD3:   hasCred,
				Destination:        w.Bridge.PrivateCfg.Config.Destination,
				DeskConversationID: deskCID,
				ThreadHint:         hint,
				Notification:       sanitized,
			}
			// Compute audit hash over sanitized notification WITHOUT one-shot callback tokens.
			auditCopy := sanitized
			auditCopy.Actions = nil
			auditBytes, _ := json.Marshal(auditCopy)
			pHash := sha256HexBytes(auditBytes)

			if dryRun {
				bo.items[q.IdempotencyKey] = itemOutcome{
					key:        q.IdempotencyKey,
					resp:       NotifyResponse{Protocol: ProtocolV1, OK: true, DeliveryID: "dry-run"},
					payHash:    pHash,
					sentThread: hint.ExistingExternalThreadID,
				}
				continue
			}

			t0 := time.Now()
			stdout, _, runErr := InvokeBridge(ctx, vaultDir, w.Bridge.PackID, w.Bridge.PackDir, w.Bridge.Spec, w.Bridge.PrivateCfg.Config.CredentialFile, req)
			lat := time.Since(t0).Milliseconds()
			var parsed NotifyResponse
			if len(stdout) > 0 {
				_ = json.Unmarshal(stdout, &parsed)
			}
			var pErr *ProtocolError
			if !parsed.OK && parsed.Error != nil {
				pErr = parsed.Error
			}
			if runErr == nil && !parsed.OK && pErr == nil {
				runErr = fmt.Errorf("bridge returned ok=false on notify")
			}
			extThr := strings.TrimSpace(parsed.ExternalThreadID)
			if extThr == "" {
				extThr = hint.ExistingExternalThreadID
			}
			bo.items[q.IdempotencyKey] = itemOutcome{
				key:        q.IdempotencyKey,
				resp:       parsed,
				err:        runErr,
				pErr:       pErr,
				latencyMs:  lat,
				payHash:    pHash,
				sentThread: extThr,
			}
		}
		results = append(results, bo)
	}

	// Phase C (under RankChannels = 65): reload state, commit outcomes, update backoff/pauses, and write channels.jsonl.
	err = WithChannelsLock(ctx, vaultDir, func(_ context.Context) error {
		for _, bo := range results {
			rb := bo.work.Bridge
			st, lErr := loadBridgeStateUnlocked(vaultDir, rb.FQID)
			if lErr != nil && !os.IsNotExist(lErr) {
				continue
			}

			applyBridgeError := func(cls ErrorClass, msg string, code string) {
				st.Health.ConsecutiveErrors++
				st.Health.LastErrorAt = nowStr
				st.Health.LastError = msg
				switch cls {
				case ErrClassAuthExpired:
					if st.Health.Status != HealthStatusPausedAuth {
						rollups = append(rollups, rollupNotice{
							SourceSkill: "channels",
							Scope:       rb.FQID,
							Title:       fmt.Sprintf("Channel bridge %s paused (authentication expired)", rb.FQID),
							Summary:     fmt.Sprintf("Bridge returned auth_expired (%s). Refresh credentials and run `hq channel status %s`.", msg, rb.FQID),
						})
					}
					st.Health.Status = HealthStatusPausedAuth
					st.Health.PausedReason = msg
					st.Health.ConfigSHA256AtPause = rb.PrivateCfg.ConfigSHA256
					st.Health.CredFingerprintAtPause = rb.PrivateCfg.CredFingerprint
				case ErrClassPermanentDestination:
					if st.Health.Status != HealthStatusPausedPermanent {
						rollups = append(rollups, rollupNotice{
							SourceSkill: "channels",
							Scope:       rb.FQID,
							Title:       fmt.Sprintf("Channel bridge %s paused (%s)", rb.FQID, code),
							Summary:     fmt.Sprintf("Bridge returned permanent destination error (%s). Update private/channels config to resume.", msg),
						})
					}
					st.Health.Status = HealthStatusPausedPermanent
					st.Health.PausedReason = msg
					st.Health.ConfigSHA256AtPause = rb.PrivateCfg.ConfigSHA256
				default:
					st.Health.Status = HealthStatusDegraded
					if st.Health.ConsecutiveErrors == 3 {
						rollups = append(rollups, rollupNotice{
							SourceSkill: "channels",
							Scope:       rb.FQID,
							Title:       fmt.Sprintf("Channel bridge %s experiencing transient errors (3 consecutive failures)", rb.FQID),
							Summary:     msg,
						})
					}
				}
			}

			if bo.bindOut != nil && st.PendingBind != nil && st.PendingBind.IdempotencyKey == bo.work.Bind.IdempotencyKey {
				bOut := bo.bindOut
				attempt := st.PendingBind.Attempt + 1
				if bOut.err == nil && bOut.resp.OK {
					rep.BindsSent++
					st.LastDeskInitialized = true
					st.LastDeskCID = bo.work.Bind.ConversationID
					st.LastDeskLeaseState = bo.work.Bind.LeaseState
					if bOut.resp.DeskThread != nil && strings.TrimSpace(bOut.resp.DeskThread.ExternalThreadID) != "" {
						st.DeskThread = DeskThreadRef{
							ExternalThreadID: strings.TrimSpace(bOut.resp.DeskThread.ExternalThreadID),
							Permalink:        strings.TrimSpace(bOut.resp.DeskThread.Permalink),
							Metadata:         bOut.resp.DeskThread.Metadata,
						}
					}
					for _, bt := range bOut.resp.ItemThreads {
						if strings.TrimSpace(bt.ItemKey) != "" && strings.TrimSpace(bt.ExternalThreadID) != "" {
							st.ItemThreads = upsertItemThread(st.ItemThreads, ItemThreadEntry{
								ItemKey:          bt.ItemKey,
								ExternalThreadID: bt.ExternalThreadID,
								Status:           "open",
							}, now)
						}
					}
					st.PendingBind = nil
					st.RecentDeliveredKeys = addSeenKey(st.RecentDeliveredKeys, bo.work.Bind.IdempotencyKey, RecentKeyTTL, now)
					st.Health.Status = HealthStatusHealthy
					st.Health.ConsecutiveErrors = 0
					st.Health.LastSuccessAt = nowStr
					st.Health.LastError = ""
					_ = appendAuditEntry(vaultDir, AuditEntry{
						TS:                 nowStr,
						BridgeID:           rb.FQID,
						Op:                 OpBindDesk,
						Kind:               bo.work.Bind.LeaseState,
						IdempotencyKey:     bo.work.Bind.IdempotencyKey,
						DeskCID:            bo.work.Bind.ConversationID,
						DestSHA256:         rb.PrivateCfg.DestSHA256Short,
						ExternalThreadHash: sha256Short12(st.DeskThread.ExternalThreadID),
						PayloadSHA256:      bOut.payHash,
						Outcome:            "delivered",
						Attempt:            attempt,
						LatencyMs:          bOut.latencyMs,
					})
				} else {
					cls, msg, retrySec := ClassifyError(bOut.pErr, bOut.err)
					code := string(cls)
					if bOut.pErr != nil && bOut.pErr.Code != "" {
						code = bOut.pErr.Code
					}
					applyBridgeError(cls, msg, code)
					if cls == ErrClassPermanentItem || cls == ErrClassPermanentDestination || attempt >= MaxDeliveryAttempts {
						rep.DroppedPermanent++
						st.PendingBind = nil
					} else {
						rep.RetriedLater++
						st.PendingBind.Attempt = attempt
						st.PendingBind.DispatchClaimUntil = ""
						st.PendingBind.LastError = msg
						st.PendingBind.NextAttemptAt = now.Add(retryBackoffDuration(attempt, retrySec)).UTC().Format(time.RFC3339)
						noteDue(st.PendingBind.NextAttemptAt)
					}
					_ = appendAuditEntry(vaultDir, AuditEntry{
						TS:             nowStr,
						BridgeID:       rb.FQID,
						Op:             OpBindDesk,
						Kind:           bo.work.Bind.LeaseState,
						IdempotencyKey: bo.work.Bind.IdempotencyKey,
						DeskCID:        bo.work.Bind.ConversationID,
						DestSHA256:     rb.PrivateCfg.DestSHA256Short,
						PayloadSHA256:  bOut.payHash,
						Outcome:        "failed",
						Attempt:        attempt,
						LatencyMs:      bOut.latencyMs,
						ErrorCode:      code,
					})
				}
			}

			if len(bo.items) > 0 {
				nextQueue := make([]QueuedNotification, 0, len(st.RetryQueue))
				for _, q := range st.RetryQueue {
					iOut, wasClaimed := bo.items[q.IdempotencyKey]
					if !wasClaimed {
						if q.NextAttemptAt != "" {
							noteDue(q.NextAttemptAt)
						}
						nextQueue = append(nextQueue, q)
						continue
					}
					attempt := q.Attempt + 1
					if iOut.err == nil && iOut.resp.OK {
						rep.NotificationsSent++
						st.RecentDeliveredKeys = addSeenKey(st.RecentDeliveredKeys, q.IdempotencyKey, RecentKeyTTL, now)
						if strings.TrimSpace(iOut.resp.DeskExternalThreadID) != "" {
							st.DeskThread.ExternalThreadID = strings.TrimSpace(iOut.resp.DeskExternalThreadID)
						}
						if q.ItemKey != "" && strings.TrimSpace(iOut.sentThread) != "" {
							propID := ""
							if len(q.Notification.ProposalIDs) > 0 {
								propID = q.Notification.ProposalIDs[0]
							}
							status := "open"
							if q.Notification.Kind == KindTaskDone {
								status = "closed"
							}
							st.ItemThreads = upsertItemThread(st.ItemThreads, ItemThreadEntry{
								ItemKey:          q.ItemKey,
								Kind:             q.Notification.Kind,
								ExternalThreadID: strings.TrimSpace(iOut.sentThread),
								TargetConvID:     q.TargetConvID,
								TaskSlug:         q.Notification.TaskSlug,
								ProposalID:       propID,
								Status:           status,
							}, now)
						}
						st.Health.Status = HealthStatusHealthy
						st.Health.ConsecutiveErrors = 0
						st.Health.LastSuccessAt = nowStr
						st.Health.LastError = ""
						_ = appendAuditEntry(vaultDir, AuditEntry{
							TS:                 nowStr,
							BridgeID:           rb.FQID,
							Op:                 OpNotify,
							Kind:               q.Notification.Kind,
							IdempotencyKey:     q.IdempotencyKey,
							DeskCID:            q.DeskConversationID,
							DestSHA256:         rb.PrivateCfg.DestSHA256Short,
							ExternalThreadHash: sha256Short12(iOut.sentThread),
							PayloadSHA256:      iOut.payHash,
							Outcome:            "delivered",
							Attempt:            attempt,
							LatencyMs:          iOut.latencyMs,
						})
						continue
					}

					cls, msg, retrySec := ClassifyError(iOut.pErr, iOut.err)
					code := string(cls)
					if iOut.pErr != nil && iOut.pErr.Code != "" {
						code = iOut.pErr.Code
					}
					applyBridgeError(cls, msg, code)
					outcome := "retry_scheduled"
					if cls == ErrClassPermanentItem || cls == ErrClassPermanentDestination {
						rep.DroppedPermanent++
						outcome = "dropped_permanent"
					} else if attempt >= MaxDeliveryAttempts {
						rep.DroppedPermanent++
						outcome = "exhausted_to_brief"
						rollups = append(rollups, rollupNotice{
							SourceSkill: "channels",
							Scope:       rb.FQID,
							Title:       fmt.Sprintf("Channel notification exhausted %d retries: %s", MaxDeliveryAttempts, q.Notification.Title),
							Summary:     fmt.Sprintf("Last error: %s\n\n%s", msg, q.Notification.BodyMarkdown),
						})
					} else {
						rep.RetriedLater++
						q.Attempt = attempt
						q.DispatchClaimUntil = ""
						q.LastError = msg
						q.NextAttemptAt = now.Add(retryBackoffDuration(attempt, retrySec)).UTC().Format(time.RFC3339)
						noteDue(q.NextAttemptAt)
						nextQueue = append(nextQueue, q)
					}
					_ = appendAuditEntry(vaultDir, AuditEntry{
						TS:             nowStr,
						BridgeID:       rb.FQID,
						Op:             OpNotify,
						Kind:           q.Notification.Kind,
						IdempotencyKey: q.IdempotencyKey,
						DeskCID:        q.DeskConversationID,
						DestSHA256:     rb.PrivateCfg.DestSHA256Short,
						PayloadSHA256:  iOut.payHash,
						Outcome:        outcome,
						Attempt:        attempt,
						LatencyMs:      iOut.latencyMs,
						ErrorCode:      code,
					})
				}
				st.RetryQueue = nextQueue
			}
			_ = saveBridgeStateUnlocked(vaultDir, st, now)
		}
		return nil
	})

	// Phase D (unlocked — RankOutbox = 60 safe outside RankChannels = 65): record brief_rollup diagnostic notices.
	for _, rn := range rollups {
		_, _, _ = outbox.Add(vaultDir, outbox.OutboxItem{
			Tier:        outbox.TierBriefRollup,
			SourceSkill: rn.SourceSkill,
			Scope:       rn.Scope,
			Title:       rn.Title,
			Summary:     rn.Summary,
		}, now)
	}

	fdExists2, fdMod2, fdSize2 := frontDeskStatFingerprint(vaultDir)
	daemonSyncMemo.mu.Lock()
	daemonSyncMemo.vaultDir = vaultDir
	daemonSyncMemo.fdExists = fdExists2
	daemonSyncMemo.fdModUnixNano = fdMod2
	daemonSyncMemo.fdSize = fdSize2
	daemonSyncMemo.seenGen = curGen
	daemonSyncMemo.hasNextDue = hasEarliestDue
	daemonSyncMemo.nextDueAt = earliestDue
	daemonSyncMemo.lastCheckedWall = now
	daemonSyncMemo.mu.Unlock()

	return rep, err
}

func outboxItemKey(it outbox.OutboxItem) string {
	if len(it.ProposalIDs) > 0 && strings.TrimSpace(it.ProposalIDs[0]) != "" {
		return "proposal:" + strings.TrimSpace(it.ProposalIDs[0])
	}
	if strings.TrimSpace(it.Scope) != "" && strings.Contains(it.Scope, ":") {
		return "thread:" + strings.TrimSpace(it.Scope)
	}
	return "outbox:" + strings.TrimSpace(it.ID)
}

// EnqueueOutboxInterrupts enqueues delivered Front Desk `TierInterrupt` items into each enabled bridge's
// independent retry queue (`idempotency_key = "outbox:" + it.ID`), preventing duplicate Front Desk wakeups on chat API errors.
func EnqueueOutboxInterrupts(vaultDir, deskCID string, items []outbox.OutboxItem, now time.Time) error {
	if len(items) == 0 || strings.TrimSpace(vaultDir) == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	bridges, err := DiscoverBridges(vaultDir)
	if err != nil || len(bridges) == 0 {
		return err
	}
	enqueued := 0
	err = WithChannelsLock(context.Background(), vaultDir, func(_ context.Context) error {
		for _, rb := range bridges {
			if !rb.EffectiveEnabled() || !rb.PrivateCfg.ResolvedPolicy.DeliverInterrupts {
				continue
			}
			st, lErr := loadBridgeStateUnlocked(vaultDir, rb.FQID)
			if lErr != nil && !os.IsNotExist(lErr) {
				continue
			}
			changed := false
			for _, it := range items {
				idemKey := sanitizeIdempotencyKey("outbox:" + it.ID)
				if hasSeenKey(st.RecentDeliveredKeys, idemKey, now) {
					continue
				}
				kind := KindInterrupt
				sev := "high"
				if it.Critical && !it.Tainted {
					kind = KindCriticalBreach
					sev = "critical"
				}
				q := QueuedNotification{
					IdempotencyKey:     idemKey,
					DeskConversationID: strings.TrimSpace(deskCID),
					ItemKey:            outboxItemKey(it),
					Critical:           it.Critical && !it.Tainted,
					Notification: NotificationPayload{
						Kind:          kind,
						Severity:      sev,
						SourceSkill:   it.SourceSkill,
						Scope:         it.Scope,
						Title:         it.Title,
						BodyMarkdown:  it.Summary,
						ProposalIDs:   it.ProposalIDs,
						TaintedOrigin: it.Tainted,
					},
					CreatedAt: now.UTC().Format(time.RFC3339),
				}
				st.RetryQueue = enqueueWithBoundedEviction(st.RetryQueue, q)
				changed = true
				enqueued++
			}
			if changed {
				_ = saveBridgeStateUnlocked(vaultDir, st, now)
			}
		}
		return nil
	})
	if enqueued > 0 {
		bumpEnqueueGeneration()
	}
	return err
}

// EnqueuePendingInterruptsWhenDeskVacant peeks pending `TierInterrupt` items while `front_desk.json` is vacant
// so urgent mobile/chat notifications still deliver immediately without altering `OutboxItem.Status`.
// When a new Front Desk later claims the lease, `RecentDeliveredKeys["outbox:"+it.ID]` prevents duplicate sends.
func EnqueuePendingInterruptsWhenDeskVacant(vaultDir string, now time.Time) error {
	if strings.TrimSpace(vaultDir) == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	var pendingInterrupts []outbox.OutboxItem
	err := outbox.WithOutboxLock(vaultDir, func() error {
		all, lErr := outbox.LoadAllUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		for _, it := range all {
			if it.Tier != outbox.TierInterrupt || it.Status != outbox.StatusPending {
				continue
			}
			if it.BriefLeaseExpiresAt != "" {
				if exp, pErr := time.Parse(time.RFC3339, it.BriefLeaseExpiresAt); pErr == nil && now.Before(exp) {
					continue
				}
			}
			pendingInterrupts = append(pendingInterrupts, it)
		}
		return nil
	})
	if err != nil || len(pendingInterrupts) == 0 {
		return err
	}
	return EnqueueOutboxInterrupts(vaultDir, "", pendingInterrupts, now)
}

// EnqueueTaskDone enqueues an opt-in (`policy.deliver_task_done: true`) Layer-1 `kind: "task"` completion digest.
func EnqueueTaskDone(vaultDir, slug, title, summary, completedAt string, now time.Time) error {
	slug = strings.TrimSpace(slug)
	if strings.TrimSpace(vaultDir) == "" || slug == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	bridges, err := DiscoverBridges(vaultDir)
	if err != nil || len(bridges) == 0 {
		return err
	}
	fd, _ := readFrontDeskDiskSnapshot(vaultDir)
	if completedAt == "" {
		completedAt = now.UTC().Format(time.RFC3339)
	}
	idemKey := sanitizeIdempotencyKey("task-done:" + slug + ":" + completedAt)
	cleanTitle := strings.TrimSpace(title)
	if cleanTitle == "" {
		cleanTitle = "Task completed: " + slug
	}
	body := strings.TrimSpace(summary)
	if body == "" {
		body = "Task `" + slug + "` marked completed."
	}

	enqueued := 0
	err = WithChannelsLock(context.Background(), vaultDir, func(_ context.Context) error {
		for _, rb := range bridges {
			if !rb.EffectiveEnabled() || !rb.Spec.Capabilities.TaskDigests || !rb.PrivateCfg.ResolvedPolicy.DeliverTaskDone {
				continue
			}
			st, lErr := loadBridgeStateUnlocked(vaultDir, rb.FQID)
			if lErr != nil && !os.IsNotExist(lErr) {
				continue
			}
			if hasSeenKey(st.RecentDeliveredKeys, idemKey, now) {
				continue
			}
			st.RetryQueue = enqueueWithBoundedEviction(st.RetryQueue, QueuedNotification{
				IdempotencyKey:     idemKey,
				DeskConversationID: strings.TrimSpace(fd.ConversationID),
				ItemKey:            "task:" + slug,
				Notification: NotificationPayload{
					Kind:         KindTaskDone,
					Severity:     "low",
					SourceSkill:  "tasks",
					Scope:        slug,
					Title:        cleanTitle,
					BodyMarkdown: body,
					TaskSlug:     slug,
				},
				CreatedAt: now.UTC().Format(time.RFC3339),
			})
			if saveBridgeStateUnlocked(vaultDir, st, now) == nil {
				enqueued++
			}
		}
		return nil
	})
	if enqueued > 0 {
		bumpEnqueueGeneration()
	}
	return err
}

// EnqueueTaskBlocked enqueues a `task_blocked` digest when a non-Front-Desk conversation stages a pending proposal.
func EnqueueTaskBlocked(vaultDir string, prop approvals.Proposal, now time.Time) error {
	propConv := strings.TrimSpace(prop.ConversationID)
	if strings.TrimSpace(vaultDir) == "" || prop.ID == "" || propConv == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	fd, hasFD := readFrontDeskDiskSnapshot(vaultDir)
	if hasFD && strings.TrimSpace(fd.ConversationID) == propConv {
		return nil
	}
	// Quarantine / [Auto] inbox-triage runs already escalate explicitly via `hq outbox add --tier=interrupt`
	// when urgent; avoid duplicate alerts for tainted reader sessions.
	if _, err := os.Stat(filepath.Join(vaultDir, "private", "taint", propConv+".json")); err == nil {
		return nil
	}

	bridges, err := DiscoverBridges(vaultDir)
	if err != nil || len(bridges) == 0 {
		return err
	}
	idemKey := sanitizeIdempotencyKey("task-blocked:" + prop.ID)
	title := fmt.Sprintf("Task awaiting approval (%s): %s", prop.ActionType, prop.Summary)
	body := fmt.Sprintf("Proposal `%s` (`%s`) staged and awaiting human approval.\n\n- **Summary:** %s", prop.ID, prop.ActionType, prop.Summary)

	enqueued := 0
	err = WithChannelsLock(context.Background(), vaultDir, func(_ context.Context) error {
		for _, rb := range bridges {
			if !rb.EffectiveEnabled() || !rb.Spec.Capabilities.TaskDigests || !rb.PrivateCfg.ResolvedPolicy.DeliverTaskBlocked {
				continue
			}
			st, lErr := loadBridgeStateUnlocked(vaultDir, rb.FQID)
			if lErr != nil && !os.IsNotExist(lErr) {
				continue
			}
			if hasSeenKey(st.RecentDeliveredKeys, idemKey, now) {
				continue
			}
			st.RetryQueue = enqueueWithBoundedEviction(st.RetryQueue, QueuedNotification{
				IdempotencyKey:     idemKey,
				DeskConversationID: strings.TrimSpace(fd.ConversationID),
				ItemKey:            "proposal:" + prop.ID,
				TargetConvID:       propConv,
				Notification: NotificationPayload{
					Kind:         KindTaskBlocked,
					Severity:     "normal",
					SourceSkill:  prop.OriginSkill,
					Title:        title,
					BodyMarkdown: body,
					ProposalIDs:  []string{prop.ID},
				},
				CreatedAt: now.UTC().Format(time.RFC3339),
			})
			if saveBridgeStateUnlocked(vaultDir, st, now) == nil {
				enqueued++
			}
		}
		return nil
	})
	if enqueued > 0 {
		bumpEnqueueGeneration()
	}
	return err
}

// ListBridgeSummaries returns redacted channel bridge rows for CLI, Console, and Web UI surfaces.
func ListBridgeSummaries(vaultDir string) ([]BridgeSummary, error) {
	bridges, err := DiscoverBridges(vaultDir)
	if err != nil {
		return nil, err
	}
	out := make([]BridgeSummary, 0, len(bridges))
	for _, rb := range bridges {
		st, _ := loadBridgeStateUnlocked(vaultDir, rb.FQID)
		health := st.Health.Status
		lastErr := st.Health.LastError
		switch {
		case !rb.PackTrusted || !rb.BinaryTrusted:
			health = HealthStatusUntrusted
		case !rb.OverrideOn || !rb.PackEnabled || (rb.HasPrivateCfg && rb.PrivateCfg.Config.Enabled != nil && !*rb.PrivateCfg.Config.Enabled):
			health = HealthStatusPausedUser
		case !rb.HasPrivateCfg:
			health = HealthStatusUnconfigured
		case rb.PrivateCfgErr != nil:
			health = HealthStatusDegraded
			lastErr = rb.PrivateCfgErr.Error()
		case health == "":
			health = HealthStatusHealthy
		}
		out = append(out, BridgeSummary{
			BridgeID:        rb.FQID,
			PackID:          rb.PackID,
			Enabled:         rb.EffectiveEnabled(),
			Trusted:         rb.PackTrusted && rb.BinaryTrusted,
			Configured:      rb.HasPrivateCfg && rb.PrivateCfgErr == nil,
			HealthStatus:    health,
			DestSHA256Short: rb.PrivateCfg.DestSHA256Short,
			LastDeskCID:     st.LastDeskCID,
			QueuedCount:     len(st.RetryQueue),
			LastSuccessAt:   st.Health.LastSuccessAt,
			LastError:       lastErr,
		})
	}
	return out, nil
}

// BridgeStatusResult captures live `op: "status"` output plus drift-healing details.
type BridgeStatusResult struct {
	BridgeID              string `json:"bridgeId"`
	Enabled               bool   `json:"enabled"`
	Trusted               bool   `json:"trusted"`
	Configured            bool   `json:"configured"`
	Healthy               bool   `json:"healthy"`
	AuthState             string `json:"authState,omitempty"`
	ExpectedDeskCID       string `json:"expectedDeskCid,omitempty"`
	InboundTunnelBoundCID string `json:"inboundTunnelBoundCid,omitempty"`
	DriftDetected         bool   `json:"driftDetected,omitempty"`
	SelfHealed            bool   `json:"selfHealed,omitempty"`
	DestSHA256Short       string `json:"destSha256Short,omitempty"`
	QueuedCount           int    `json:"queuedCount"`
	Detail                string `json:"detail,omitempty"`
	Error                 string `json:"error,omitempty"`
}

// ProbeStatus invokes `op: "status"` on one or all configured channel bridges and triggers self-healing `bind-desk`
// whenever `inbound_tunnel_bound_cid` drifts from `front_desk.json`.
func ProbeStatus(ctx context.Context, vaultDir, filterFQID string, now time.Time) ([]BridgeStatusResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now()
	}
	filterFQID = strings.TrimSpace(filterFQID)
	if filterFQID != "" {
		if _, _, err := ParseBridgeFQID(filterFQID); err != nil {
			return nil, err
		}
	}
	bridges, err := DiscoverBridges(vaultDir)
	if err != nil {
		return nil, err
	}
	fd, hasFD := readFrontDeskDiskSnapshot(vaultDir)
	expectedCID := ""
	if hasFD {
		expectedCID = strings.TrimSpace(fd.ConversationID)
	}

	var results []BridgeStatusResult
	needsHealFlush := false

	for _, rb := range bridges {
		if filterFQID != "" && rb.FQID != filterFQID {
			continue
		}
		st, _ := loadBridgeStateUnlocked(vaultDir, rb.FQID)
		res := BridgeStatusResult{
			BridgeID:        rb.FQID,
			Enabled:         rb.EffectiveEnabled(),
			Trusted:         rb.PackTrusted && rb.BinaryTrusted,
			Configured:      rb.HasPrivateCfg && rb.PrivateCfgErr == nil,
			ExpectedDeskCID: expectedCID,
			DestSHA256Short: rb.PrivateCfg.DestSHA256Short,
			QueuedCount:     len(st.RetryQueue),
		}
		if rb.PrivateCfgErr != nil {
			res.Error = rb.PrivateCfgErr.Error()
			results = append(results, res)
			continue
		}
		if !rb.EffectiveEnabled() {
			res.Detail = "bridge inactive (pack/override disabled, untrusted, or unconfigured)"
			results = append(results, res)
			continue
		}

		req := StatusRequest{
			Protocol:             ProtocolV1,
			Op:                   OpStatus,
			BridgeID:             rb.FQID,
			Timestamp:            now.UTC().Format(time.RFC3339),
			HasCredentialFD3:     strings.TrimSpace(rb.PrivateCfg.Config.CredentialFile) != "",
			Destination:          rb.PrivateCfg.Config.Destination,
			DeskConversationID:   expectedCID,
			DeskExternalThreadID: st.DeskThread.ExternalThreadID,
		}
		stdout, _, runErr := InvokeBridge(ctx, vaultDir, rb.PackID, rb.PackDir, rb.Spec, rb.PrivateCfg.Config.CredentialFile, req)
		var parsed StatusResponse
		if len(stdout) > 0 {
			_ = json.Unmarshal(stdout, &parsed)
		}
		if runErr != nil || !parsed.OK {
			_, msg, _ := ClassifyError(parsed.Error, runErr)
			res.Error = msg
			res.AuthState = parsed.AuthState
			results = append(results, res)
			continue
		}

		res.Healthy = parsed.Healthy
		res.AuthState = parsed.AuthState
		res.InboundTunnelBoundCID = strings.TrimSpace(parsed.InboundTunnelBoundCID)
		res.Detail = parsed.Detail

		// Clear paused_auth if live status probe confirms authenticated health.
		if parsed.Healthy {
			_ = WithChannelsLock(ctx, vaultDir, func(_ context.Context) error {
				cur, _ := loadBridgeStateUnlocked(vaultDir, rb.FQID)
				if cur.Health.Status == HealthStatusPausedAuth || cur.Health.Status == HealthStatusDegraded {
					cur.Health.Status = HealthStatusHealthy
					cur.Health.PausedReason = ""
					cur.Health.ConsecutiveErrors = 0
					_ = saveBridgeStateUnlocked(vaultDir, cur, now)
				}
				return nil
			})
		}

		if rb.Spec.Capabilities.DeskBinding && res.InboundTunnelBoundCID != expectedCID {
			res.DriftDetected = true
			leaseState := LeaseStateVacated
			if expectedCID != "" {
				leaseState = LeaseStateActive
			}
			_ = WithChannelsLock(ctx, vaultDir, func(_ context.Context) error {
				cur, _ := loadBridgeStateUnlocked(vaultDir, rb.FQID)
				cur.PendingBind = &PendingDeskBind{
					IdempotencyKey:         sanitizeIdempotencyKey(fmt.Sprintf("bind-heal:%s:%s:%d", rb.FQID, expectedCID, now.Unix())),
					LeaseState:             leaseState,
					ConversationID:         expectedCID,
					PreviousConversationID: res.InboundTunnelBoundCID,
					ProjectID:              fd.ProjectID,
					WorkspaceName:          workspaceDisplayName(fd.WorkspaceDir),
					UpdatedAt:              now.UTC().Format(time.RFC3339),
				}
				return saveBridgeStateUnlocked(vaultDir, cur, now)
			})
			bumpEnqueueGeneration()
			needsHealFlush = true
		}
		results = append(results, res)
	}
	if filterFQID != "" && len(results) == 0 {
		return nil, fmt.Errorf("channel bridge %q not found", filterFQID)
	}
	if needsHealFlush {
		if fRep, fErr := SyncAndFlush(ctx, vaultDir, now, false); fErr == nil && fRep.BindsSent > 0 {
			for i := range results {
				if results[i].DriftDetected {
					results[i].SelfHealed = true
				}
			}
		}
	}
	return results, nil
}

// BridgeTestResult reports the result of `hq channel test <bridge-fqid>`.
type BridgeTestResult struct {
	OK               bool   `json:"ok"`
	BridgeID         string `json:"bridgeId"`
	DeliveryID       string `json:"deliveryId,omitempty"`
	ExternalThreadID string `json:"externalThreadId,omitempty"`
	DestSHA256Short  string `json:"destSha256Short"`
	LatencyMs        int64  `json:"latencyMs"`
}

// SendTestPing dispatches a synthetic `kind: "test_ping"` notification to the pre-configured private destination.
func SendTestPing(ctx context.Context, vaultDir, fqid string, now time.Time) (BridgeTestResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now()
	}
	rb, err := requireActiveTrustedBridge(vaultDir, fqid)
	if err != nil {
		return BridgeTestResult{}, err
	}
	fd, _ := readFrontDeskDiskSnapshot(vaultDir)
	st, _ := loadBridgeStateUnlocked(vaultDir, rb.FQID)

	notif := SanitizeAndFormatNotification(NotificationPayload{
		Kind:         KindTestPing,
		Severity:     "low",
		SourceSkill:  "channel-test",
		Title:        "agy-hq Channel Bridge Test",
		BodyMarkdown: fmt.Sprintf("Connectivity check succeeded for `%s` at `%s`.", rb.FQID, now.UTC().Format(time.RFC3339)),
	}, rb.PrivateCfg.ResolvedPolicy)

	idemKey := sanitizeIdempotencyKey(fmt.Sprintf("test-ping:%s:%d", rb.FQID, now.UnixNano()))
	req := NotifyRequest{
		Protocol:           ProtocolV1,
		Op:                 OpNotify,
		BridgeID:           rb.FQID,
		IdempotencyKey:     idemKey,
		Timestamp:          now.UTC().Format(time.RFC3339),
		HasCredentialFD3:   strings.TrimSpace(rb.PrivateCfg.Config.CredentialFile) != "",
		Destination:        rb.PrivateCfg.Config.Destination,
		DeskConversationID: strings.TrimSpace(fd.ConversationID),
		ThreadHint: ThreadHint{
			Mode:                 ThreadModeDeskThread,
			DeskExternalThreadID: st.DeskThread.ExternalThreadID,
		},
		Notification: notif,
	}
	auditBytes, _ := json.Marshal(notif)
	pHash := sha256HexBytes(auditBytes)

	t0 := time.Now()
	stdout, _, runErr := InvokeBridge(ctx, vaultDir, rb.PackID, rb.PackDir, rb.Spec, rb.PrivateCfg.Config.CredentialFile, req)
	lat := time.Since(t0).Milliseconds()
	var parsed NotifyResponse
	if len(stdout) > 0 {
		_ = json.Unmarshal(stdout, &parsed)
	}
	if runErr != nil || !parsed.OK {
		cls, msg, _ := ClassifyError(parsed.Error, runErr)
		_ = WithChannelsLock(ctx, vaultDir, func(_ context.Context) error {
			return appendAuditEntry(vaultDir, AuditEntry{
				TS:             now.UTC().Format(time.RFC3339),
				BridgeID:       rb.FQID,
				Op:             OpNotify,
				Kind:           KindTestPing,
				IdempotencyKey: idemKey,
				DeskCID:        strings.TrimSpace(fd.ConversationID),
				DestSHA256:     rb.PrivateCfg.DestSHA256Short,
				PayloadSHA256:  pHash,
				Outcome:        "failed",
				Attempt:        1,
				LatencyMs:      lat,
				ErrorCode:      string(cls),
			})
		})
		return BridgeTestResult{}, fmt.Errorf("test ping failed (%s): %s", cls, msg)
	}

	_ = WithChannelsLock(ctx, vaultDir, func(_ context.Context) error {
		cur, _ := loadBridgeStateUnlocked(vaultDir, rb.FQID)
		cur.Health.Status = HealthStatusHealthy
		cur.Health.ConsecutiveErrors = 0
		cur.Health.PausedReason = ""
		cur.Health.LastSuccessAt = now.UTC().Format(time.RFC3339)
		cur.Health.LastError = ""
		if strings.TrimSpace(parsed.DeskExternalThreadID) != "" {
			cur.DeskThread.ExternalThreadID = strings.TrimSpace(parsed.DeskExternalThreadID)
		} else if strings.TrimSpace(parsed.ExternalThreadID) != "" && cur.DeskThread.ExternalThreadID == "" {
			cur.DeskThread.ExternalThreadID = strings.TrimSpace(parsed.ExternalThreadID)
		}
		_ = saveBridgeStateUnlocked(vaultDir, cur, now)
		return appendAuditEntry(vaultDir, AuditEntry{
			TS:                 now.UTC().Format(time.RFC3339),
			BridgeID:           rb.FQID,
			Op:                 OpNotify,
			Kind:               KindTestPing,
			IdempotencyKey:     idemKey,
			DeskCID:            strings.TrimSpace(fd.ConversationID),
			DestSHA256:         rb.PrivateCfg.DestSHA256Short,
			ExternalThreadHash: sha256Short12(parsed.ExternalThreadID),
			PayloadSHA256:      pHash,
			Outcome:            "delivered",
			Attempt:            1,
			LatencyMs:          lat,
		})
	})

	return BridgeTestResult{
		OK:               true,
		BridgeID:         rb.FQID,
		DeliveryID:       parsed.DeliveryID,
		ExternalThreadID: parsed.ExternalThreadID,
		DestSHA256Short:  rb.PrivateCfg.DestSHA256Short,
		LatencyMs:        lat,
	}, nil
}

// DoctorFinding reports a single channel-bridge diagnostic row for `hq doctor`.
type DoctorFinding struct {
	Name   string
	Status string // PASS, WARN, FAIL
	Detail string
}

// DoctorChecks validates private/channels/*.json schemas, 0600 permissions, symlink hygiene, trust pins, and bridge health.
func DoctorChecks(vaultDir string) []DoctorFinding {
	bridges, err := DiscoverBridges(vaultDir)
	if err != nil {
		return []DoctorFinding{{
			Name:   "Channel Bridges",
			Status: "WARN",
			Detail: err.Error(),
		}}
	}

	// Also inspect orphan files in private/channels/*.json that don't match any discovered pack bridge.
	knownFiles := make(map[string]bool)
	var findings []DoctorFinding

	for _, rb := range bridges {
		stem, sErr := SafeBridgeFileStem(rb.FQID)
		if sErr == nil {
			knownFiles[stem+".json"] = true
		}
		st, _ := loadBridgeStateUnlocked(vaultDir, rb.FQID)
		name := "Channel Bridge (" + rb.FQID + ")"

		if rb.PrivateCfgErr != nil {
			findings = append(findings, DoctorFinding{
				Name:   name,
				Status: "FAIL",
				Detail: rb.PrivateCfgErr.Error(),
			})
			continue
		}
		if rb.HasPrivateCfg && rb.PrivateCfg.FileMode&0o077 != 0 {
			findings = append(findings, DoctorFinding{
				Name:   name,
				Status: "WARN",
				Detail: fmt.Sprintf("private/channels/%s.json has mode %04o (expected 0600)", stem, rb.PrivateCfg.FileMode),
			})
			continue
		}
		if rb.PackEnabled && (!rb.PackTrusted || !rb.BinaryTrusted) {
			findings = append(findings, DoctorFinding{
				Name:   name,
				Status: "WARN",
				Detail: fmt.Sprintf("pack enabled, but binary or manifest is untrusted in private/trust.json (run 'hq extension trust %s')", rb.PackID),
			})
			continue
		}
		if !rb.EffectiveEnabled() {
			continue
		}
		if st.Health.Status == HealthStatusPausedAuth || st.Health.Status == HealthStatusPausedPermanent || st.Health.Status == HealthStatusDegraded {
			findings = append(findings, DoctorFinding{
				Name:   name,
				Status: "WARN",
				Detail: fmt.Sprintf("status=%s · queued=%d · lastError=%s", st.Health.Status, len(st.RetryQueue), st.Health.LastError),
			})
			continue
		}
		findings = append(findings, DoctorFinding{
			Name:   name,
			Status: "PASS",
			Detail: fmt.Sprintf("healthy · dest=%s · desk=%s · queued=%d", rb.PrivateCfg.DestSHA256Short, st.LastDeskCID, len(st.RetryQueue)),
		})
	}

	if entries, rErr := os.ReadDir(privateChannelsDir(vaultDir)); rErr == nil {
		var orphans []string
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			if !knownFiles[e.Name()] {
				orphans = append(orphans, e.Name())
			}
		}
		sort.Strings(orphans)
		if len(orphans) > 0 {
			findings = append(findings, DoctorFinding{
				Name:   "Channel Bridge Private Config",
				Status: "WARN",
				Detail: fmt.Sprintf("%d config file(s) in private/channels/ do not match any discovered companion pack bridge: %s", len(orphans), strings.Join(orphans, ", ")),
			})
		}
	}

	return findings
}
