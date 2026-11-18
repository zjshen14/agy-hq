package channels

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/events"
)

const MaxInboundTextRunes = 16000

var reValidExternalMsgID = regexp.MustCompile(`^[A-Za-z0-9._:/@-]{1,160}$`)

// RegisteredFrontDeskBootstrapper is wired by internal/kernel at init time to auto-bootstrap
// a fresh `[HQ] Front Desk` session when an owner sends an inbound message while `front_desk.json` is vacant.
var RegisteredFrontDeskBootstrapper func(vaultDir string, now time.Time) (string, error)

// InboundRequest is sent by a trusted companion sidecar to POST /api/channels/inbound or `hq channel inbound`.
type InboundRequest struct {
	BridgeID          string `json:"bridge_id"`
	ExternalMessageID string `json:"external_message_id"`
	ExternalThreadID  string `json:"external_thread_id,omitempty"`
	Text              string `json:"text"`
	Timestamp         string `json:"timestamp,omitempty"`
}

// InboundResult reports how `agy-hq` routed an inbound owner message.
type InboundResult struct {
	OK                   bool   `json:"ok"`
	Deduplicated         bool   `json:"deduplicated,omitempty"`
	BridgeID             string `json:"bridge_id"`
	TargetConversationID string `json:"target_conversation_id,omitempty"`
	BootstrappedDesk     bool   `json:"bootstrapped_desk,omitempty"`
	EventID              string `json:"event_id,omitempty"`
	CorrelationID        string `json:"correlation_id,omitempty"`
}

type diskTaskLite struct {
	TaskSlug       string `json:"taskSlug"`
	ConversationID string `json:"conversationId"`
	Status         string `json:"status"`
}

func findActiveTaskConversation(vaultDir, slug, fallbackConvID string) string {
	data, err := os.ReadFile(filepath.Join(vaultDir, "memory", "tasks.json"))
	if err != nil {
		return ""
	}
	var tasks []diskTaskLite
	if json.Unmarshal(data, &tasks) != nil {
		return ""
	}
	for _, t := range tasks {
		st := strings.ToLower(strings.TrimSpace(t.Status))
		if st != "active" && st != "idle" {
			continue
		}
		if slug != "" && t.TaskSlug == slug && strings.TrimSpace(t.ConversationID) != "" {
			return strings.TrimSpace(t.ConversationID)
		}
		if fallbackConvID != "" && t.ConversationID == fallbackConvID {
			return strings.TrimSpace(t.ConversationID)
		}
	}
	return ""
}

func findPendingProposalConversation(vaultDir, propID string) string {
	if approvals.ValidateProposalID(propID) != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(vaultDir, "memory", "pending", propID+".json"))
	if err != nil {
		return ""
	}
	var p approvals.Proposal
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	return strings.TrimSpace(p.ConversationID)
}

func requireActiveTrustedBridge(vaultDir, fqid string) (ResolvedBridge, error) {
	bridges, err := DiscoverBridges(vaultDir)
	if err != nil {
		return ResolvedBridge{}, err
	}
	for _, rb := range bridges {
		if rb.FQID != fqid {
			continue
		}
		if !rb.PackEnabled {
			return ResolvedBridge{}, fmt.Errorf("companion pack %s is disabled", rb.PackID)
		}
		if !rb.PackTrusted || !rb.BinaryTrusted {
			return ResolvedBridge{}, fmt.Errorf("channel bridge %s is not trusted in private/trust.json", fqid)
		}
		if !rb.OverrideOn {
			return ResolvedBridge{}, fmt.Errorf("channel bridge %s is paused in extensions.json", fqid)
		}
		if !rb.HasPrivateCfg {
			return ResolvedBridge{}, fmt.Errorf("channel bridge %s has no private config in private/channels/", fqid)
		}
		if rb.PrivateCfgErr != nil {
			return ResolvedBridge{}, rb.PrivateCfgErr
		}
		if rb.PrivateCfg.Config.Enabled != nil && !*rb.PrivateCfg.Config.Enabled {
			return ResolvedBridge{}, fmt.Errorf("channel bridge %s is disabled in private/channels/", fqid)
		}
		return rb, nil
	}
	return ResolvedBridge{}, fmt.Errorf("channel bridge %q not found in enabled companion packs", fqid)
}

// HandleInbound authenticates, deduplicates, routes, and correlates a verified 1:1 owner message
// from a companion sidecar to `[HQ] Front Desk` (or a matched open item thread's task session).
func HandleInbound(ctx context.Context, vaultDir string, req InboundRequest, now time.Time) (InboundResult, error) {
	start := time.Now()
	if now.IsZero() {
		now = start
	}
	fqid := strings.TrimSpace(req.BridgeID)
	if _, _, err := ParseBridgeFQID(fqid); err != nil {
		return InboundResult{}, err
	}
	extMsgID := strings.TrimSpace(req.ExternalMessageID)
	if !reValidExternalMsgID.MatchString(extMsgID) {
		return InboundResult{}, fmt.Errorf("invalid external_message_id %q", req.ExternalMessageID)
	}
	cleanText := strings.TrimSpace(req.Text)
	if cleanText == "" {
		return InboundResult{}, fmt.Errorf("inbound text must be non-empty")
	}
	if runes := []rune(cleanText); len(runes) > MaxInboundTextRunes {
		cleanText = string(runes[:MaxInboundTextRunes])
	}

	rb, err := requireActiveTrustedBridge(vaultDir, fqid)
	if err != nil {
		return InboundResult{}, err
	}

	extThreadID := strings.TrimSpace(req.ExternalThreadID)
	dedupKey := fqid + ":" + extMsgID

	// Step 1 (under RankChannels): check replay dedup and inspect open item_threads for task routing.
	var alreadySeen bool
	var priorTargetCID string
	var matchedItemKey string
	var matchedTaskCID string

	err = WithChannelsLock(ctx, vaultDir, func(_ context.Context) error {
		st, lErr := loadBridgeStateUnlocked(vaultDir, fqid)
		if lErr != nil && !os.IsNotExist(lErr) {
			return lErr
		}
		if st.Health.Status == HealthStatusPausedAuth || st.Health.Status == HealthStatusPausedPermanent {
			return fmt.Errorf("channel bridge %s is %s (%s)", fqid, st.Health.Status, st.Health.PausedReason)
		}
		if hasSeenKey(st.RecentInboundKeys, dedupKey, now) {
			alreadySeen = true
			priorTargetCID = st.LastDeskCID
			return nil
		}
		if extThreadID != "" {
			for i := len(st.ItemThreads) - 1; i >= 0; i-- {
				it := st.ItemThreads[i]
				if it.ExternalThreadID == extThreadID && it.Status != "closed" {
					matchedItemKey = it.ItemKey
					if it.TaskSlug != "" || it.TargetConvID != "" {
						matchedTaskCID = findActiveTaskConversation(vaultDir, it.TaskSlug, it.TargetConvID)
					}
					if matchedTaskCID == "" && it.ProposalID != "" {
						matchedTaskCID = findPendingProposalConversation(vaultDir, it.ProposalID)
					}
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return InboundResult{}, err
	}
	if alreadySeen {
		return InboundResult{
			OK:                   true,
			Deduplicated:         true,
			BridgeID:             fqid,
			TargetConversationID: priorTargetCID,
		}, nil
	}

	// Step 2 (unlocked): resolve target session (matched item thread -> active Front Desk -> auto-bootstrap vacant desk).
	targetCID := matchedTaskCID
	bootstrapped := false
	if targetCID == "" {
		fd, hasFD := readFrontDeskDiskSnapshot(vaultDir)
		if hasFD && strings.TrimSpace(fd.ConversationID) != "" {
			targetCID = strings.TrimSpace(fd.ConversationID)
		} else if RegisteredFrontDeskBootstrapper != nil {
			if newCID, bErr := RegisteredFrontDeskBootstrapper(vaultDir, now); bErr == nil && strings.TrimSpace(newCID) != "" {
				targetCID = strings.TrimSpace(newCID)
				bootstrapped = true
			}
		}
	}

	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	corrID := "corr-" + strconv.FormatInt(now.Unix(), 10) + "-" + hex.EncodeToString(rnd[:])

	// Step 3 (unlocked — RankEvents=40 must NOT be acquired under RankChannels=65!): enqueue durable session event.
	scopeKey := "front_desk:channel_inbound"
	title := "[HQ] Front Desk"
	if targetCID != "" {
		scopeKey = "session:" + targetCID
		title = "[Channel Inbound] Owner Message"
	}
	ev, dedupedEv, enqErr := events.Enqueue(vaultDir, events.Event{
		Source:    "channel_inbound",
		DedupKey:  "channel-in:" + dedupKey,
		ScopeKey:  scopeKey,
		SessionID: targetCID,
		Title:     title,
		Prompt:    cleanText,
		Metadata: map[string]string{
			"channel_bridge_id":  fqid,
			"external_thread_id": extThreadID,
			"correlation_id":     corrID,
		},
	}, now)
	if enqErr != nil {
		return InboundResult{}, enqErr
	}

	// Step 4 (under RankChannels): commit RecentInboundKeys, PendingReplyCorrelation, and audit row.
	_ = WithChannelsLock(ctx, vaultDir, func(_ context.Context) error {
		st, _ := loadBridgeStateUnlocked(vaultDir, fqid)
		st.RecentInboundKeys = addSeenKey(st.RecentInboundKeys, dedupKey, RecentKeyTTL, now)
		replyThread := extThreadID
		if replyThread == "" {
			replyThread = st.DeskThread.ExternalThreadID
		}
		st.PendingReplies = prunePendingReplies(st.PendingReplies, now)
		st.PendingReplies = append(st.PendingReplies, PendingReplyCorrelation{
			CorrelationID:        corrID,
			BridgeID:             fqid,
			TargetConversationID: targetCID,
			ExternalThreadID:     replyThread,
			ItemKey:              matchedItemKey,
			InboundMessageID:     extMsgID,
			CreatedAt:            now.UTC().Format(time.RFC3339),
			ExpiresAt:            now.Add(PendingReplyTTL).UTC().Format(time.RFC3339),
		})
		if len(st.PendingReplies) > MaxPendingReplies {
			st.PendingReplies = st.PendingReplies[len(st.PendingReplies)-MaxPendingReplies:]
		}
		if err := saveBridgeStateUnlocked(vaultDir, st, now); err != nil {
			return err
		}
		return appendAuditEntry(vaultDir, AuditEntry{
			TS:                 now.UTC().Format(time.RFC3339),
			BridgeID:           fqid,
			Op:                 "inbound",
			Kind:               "owner_turn",
			IdempotencyKey:     sanitizeIdempotencyKey("inbound:" + dedupKey),
			DeskCID:            targetCID,
			DestSHA256:         rb.PrivateCfg.DestSHA256Short,
			ExternalThreadHash: sha256Short12(extThreadID),
			PayloadSHA256:      sha256HexBytes([]byte(cleanText)),
			Outcome:            "accepted",
			Attempt:            1,
			LatencyMs:          time.Since(start).Milliseconds(),
		})
	})
	bumpEnqueueGeneration()

	return InboundResult{
		OK:                   true,
		Deduplicated:         dedupedEv,
		BridgeID:             fqid,
		TargetConversationID: targetCID,
		BootstrappedDesk:     bootstrapped,
		EventID:              ev.ID,
		CorrelationID:        corrID,
	}, nil
}

func collectRecentProposalsForConversation(vaultDir, convID string, since, now time.Time) []string {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(vaultDir, "memory", "pending"))
	if err != nil {
		return nil
	}
	var ids []string
	cutoff := since.Add(-5 * time.Second)
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, rErr := os.ReadFile(filepath.Join(vaultDir, "memory", "pending", e.Name()))
		if rErr != nil {
			continue
		}
		var prop approvals.Proposal
		if json.Unmarshal(raw, &prop) != nil || prop.ConversationID != convID || prop.IsExpired(now) {
			continue
		}
		if !since.IsZero() && prop.CreatedAt != "" {
			if ct, pErr := time.Parse(time.RFC3339, prop.CreatedAt); pErr == nil && ct.Before(cutoff) {
				continue
			}
		}
		ids = append(ids, prop.ID)
	}
	return ids
}

// EnqueueTurnReplyIfCorrelated is invoked by `HandleStop` at turn completion. If `convID` has an unexpired
// `PendingReplyCorrelation` (or `policy.mirror_all_desk_turns == true` on `[HQ] Front Desk`), it enqueues
// `op: "notify", kind: "desk_reply"` (including newly staged `proposal_ids`) without scraping `transcript.jsonl`.
func EnqueueTurnReplyIfCorrelated(
	vaultDir, convID string,
	execNum int,
	lastResponse string,
	turnStartedAt, now time.Time,
) (int, error) {
	convID = strings.TrimSpace(convID)
	body := strings.TrimSpace(lastResponse)
	if convID == "" || body == "" {
		return 0, nil
	}
	if now.IsZero() {
		now = time.Now()
	}

	bridges, err := DiscoverBridges(vaultDir)
	if err != nil || len(bridges) == 0 {
		return 0, err
	}
	fd, hasFD := readFrontDeskDiskSnapshot(vaultDir)
	isFrontDesk := hasFD && strings.TrimSpace(fd.ConversationID) == convID

	enqueued := 0
	err = WithChannelsLock(context.Background(), vaultDir, func(_ context.Context) error {
		for _, rb := range bridges {
			if !rb.EffectiveEnabled() {
				continue
			}
			st, lErr := loadBridgeStateUnlocked(vaultDir, rb.FQID)
			if lErr != nil && !os.IsNotExist(lErr) {
				continue
			}
			st.PendingReplies = prunePendingReplies(st.PendingReplies, now)

			matchedIdx := -1
			var matchedCorr PendingReplyCorrelation
			for i := range st.PendingReplies {
				c := st.PendingReplies[i]
				if c.TargetConversationID == convID || (c.TargetConversationID == "" && isFrontDesk) {
					matchedIdx = i
					matchedCorr = c
					break
				}
			}

			shouldMirror := matchedIdx >= 0 || (isFrontDesk && rb.PrivateCfg.ResolvedPolicy.MirrorAllDeskTurns)
			if !shouldMirror {
				continue
			}

			if matchedIdx >= 0 {
				st.PendingReplies = append(st.PendingReplies[:matchedIdx], st.PendingReplies[matchedIdx+1:]...)
			}

			since := turnStartedAt
			if since.IsZero() && matchedCorr.CreatedAt != "" {
				if ct, pErr := time.Parse(time.RFC3339, matchedCorr.CreatedAt); pErr == nil {
					since = ct
				}
			}
			if since.IsZero() {
				since = now.Add(-5 * time.Minute)
			}
			propIDs := collectRecentProposalsForConversation(vaultDir, convID, since, now)

			idemKey := sanitizeIdempotencyKey(fmt.Sprintf("desk-reply:%s:%d:%s", convID, execNum, matchedCorr.CorrelationID))
			if hasSeenKey(st.RecentDeliveredKeys, idemKey, now) {
				_ = saveBridgeStateUnlocked(vaultDir, st, now)
				continue
			}

			prefThread := strings.TrimSpace(matchedCorr.ExternalThreadID)
			if prefThread == "" {
				prefThread = strings.TrimSpace(st.DeskThread.ExternalThreadID)
			}
			itemKey := strings.TrimSpace(matchedCorr.ItemKey)
			if itemKey == "" && len(propIDs) > 0 {
				itemKey = "proposal:" + propIDs[0]
			}

			qItem := QueuedNotification{
				IdempotencyKey:     idemKey,
				DeskConversationID: strings.TrimSpace(fd.ConversationID),
				ItemKey:            itemKey,
				TargetConvID:       convID,
				PreferredThreadID:  prefThread,
				Notification: NotificationPayload{
					Kind:         KindDeskReply,
					Severity:     "normal",
					SourceSkill:  "front-desk",
					Title:        "[HQ] Front Desk Reply",
					BodyMarkdown: body,
					ProposalIDs:  propIDs,
				},
				CreatedAt: now.UTC().Format(time.RFC3339),
			}
			st.RetryQueue = enqueueWithBoundedEviction(st.RetryQueue, qItem)
			if sErr := saveBridgeStateUnlocked(vaultDir, st, now); sErr == nil {
				enqueued++
			}
		}
		return nil
	})
	if enqueued > 0 {
		bumpEnqueueGeneration()
	}
	return enqueued, err
}

// BestEffortFlushLoopback sends a bounded loopback POST /api/channels/flush nudge to `hq daemon`
// when daemon.json + daemon_token are present, falling back silently to the 15s `fastTicker` if offline.
func BestEffortFlushLoopback(vaultDir string, timeout time.Duration) {
	if strings.TrimSpace(vaultDir) == "" {
		return
	}
	if timeout <= 0 {
		timeout = 150 * time.Millisecond
	}
	tokBytes, err := os.ReadFile(filepath.Join(vaultDir, "private", "daemon_token"))
	if err != nil {
		return
	}
	tok := strings.TrimSpace(string(tokBytes))
	if tok == "" {
		return
	}
	addr := "127.0.0.1:4242"
	if rawDaemon, dErr := os.ReadFile(filepath.Join(vaultDir, "memory", "daemon.json")); dErr == nil {
		var ds struct {
			Addr string `json:"addr"`
		}
		if json.Unmarshal(rawDaemon, &ds) == nil && strings.TrimSpace(ds.Addr) != "" {
			addr = strings.TrimSpace(ds.Addr)
		}
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/channels/flush", bytes.NewReader([]byte("{}")))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err == nil && resp != nil {
		_ = resp.Body.Close()
	}
}
