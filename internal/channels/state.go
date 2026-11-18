package channels

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/store"
)

const (
	BridgeStateSchemaVersion = 1

	HealthStatusHealthy         = "healthy"
	HealthStatusDegraded        = "degraded"
	HealthStatusPausedAuth      = "paused_auth"
	HealthStatusPausedPermanent = "paused_permanent"
	HealthStatusPausedUser      = "paused_user"
	HealthStatusUnconfigured    = "unconfigured"
	HealthStatusUntrusted       = "untrusted"

	MaxItemThreads          = 256
	ItemThreadTTL           = 14 * 24 * time.Hour
	MaxPendingReplies       = 32
	PendingReplyTTL         = 30 * time.Minute
	MaxRecentKeys           = 256
	RecentKeyTTL            = 72 * time.Hour
	MaxRetryQueueEntries    = 64
	MaxItemsPerFlushPass    = 8
	MaxQueuedBodyRunes      = 8000
	MaxDeliveryAttempts     = 6
	DispatchClaimTTL        = 150 * time.Second // >= (1 bind + 8 notify) * 15s max subprocess timeout + 15s margin
	MaxHotAuditLines        = 5000
	RunawayUnsolicitedCap1H = 30
)

// ItemThreadEntry maps a grouped item key ("proposal:<id>", "task:<slug>", "outbox:<id>") to an external chat thread.
type ItemThreadEntry struct {
	ItemKey          string `json:"item_key"`
	Kind             string `json:"kind"`
	ExternalThreadID string `json:"external_thread_id"`
	TargetConvID     string `json:"target_conv_id,omitempty"`
	TaskSlug         string `json:"task_slug,omitempty"`
	ProposalID       string `json:"proposal_id,omitempty"`
	Status           string `json:"status"` // "open" or "closed"
	UpdatedAt        string `json:"updated_at"`
}

// PendingReplyCorrelation binds an inbound owner turn to its originating external thread for HandleStop `desk_reply` mirroring.
type PendingReplyCorrelation struct {
	CorrelationID        string `json:"correlation_id"`
	BridgeID             string `json:"bridge_id"`
	TargetConversationID string `json:"target_conversation_id"`
	ExternalThreadID     string `json:"external_thread_id,omitempty"`
	ItemKey              string `json:"item_key,omitempty"`
	InboundMessageID     string `json:"inbound_message_id"`
	CreatedAt            string `json:"created_at"`
	ExpiresAt            string `json:"expires_at"`
}

// SeenKeyEntry tracks delivered outbound or accepted inbound idempotency keys with expiry.
type SeenKeyEntry struct {
	Key       string `json:"key"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

// PendingDeskBind represents a queued `op: "bind-desk"` transition on Front Desk lease rotation or vacation.
type PendingDeskBind struct {
	IdempotencyKey         string `json:"idempotency_key"`
	LeaseState             string `json:"lease_state"`
	ConversationID         string `json:"conversation_id"`
	PreviousConversationID string `json:"previous_conversation_id,omitempty"`
	ProjectID              string `json:"project_id,omitempty"`
	WorkspaceName          string `json:"workspace_name,omitempty"`
	UpdatedAt              string `json:"updated_at"`
	Attempt                int    `json:"attempt"`
	NextAttemptAt          string `json:"next_attempt_at,omitempty"`
	DispatchClaimUntil     string `json:"dispatch_claim_until,omitempty"`
	LastError              string `json:"last_error,omitempty"`
}

// QueuedNotification represents a durable outbound notification in a bridge's independent retry queue.
type QueuedNotification struct {
	IdempotencyKey     string              `json:"idempotency_key"`
	DeskConversationID string              `json:"desk_conversation_id,omitempty"`
	ItemKey            string              `json:"item_key,omitempty"`
	TargetConvID       string              `json:"target_conv_id,omitempty"`
	PreferredThreadID  string              `json:"preferred_thread_id,omitempty"`
	Critical           bool                `json:"critical,omitempty"`
	Notification       NotificationPayload `json:"notification"`
	CreatedAt          string              `json:"created_at"`
	Attempt            int                 `json:"attempt"`
	NextAttemptAt      string              `json:"next_attempt_at,omitempty"`
	DispatchClaimUntil string              `json:"dispatch_claim_until,omitempty"`
	LastError          string              `json:"last_error,omitempty"`
}

// RateWindowState tracks 1-hour rolling emission timestamps per bridge.
type RateWindowState struct {
	NonUrgentTimestamps   []string `json:"non_urgent_timestamps,omitempty"`
	UnsolicitedTimestamps []string `json:"unsolicited_timestamps,omitempty"`
}

// BridgeHealth records runtime health, pause state, and auto-recovery fingerprints.
type BridgeHealth struct {
	Status                 string `json:"status"`
	ConsecutiveErrors      int    `json:"consecutive_errors,omitempty"`
	PausedReason           string `json:"paused_reason,omitempty"`
	ConfigSHA256AtPause    string `json:"config_sha256_at_pause,omitempty"`
	CredFingerprintAtPause string `json:"cred_fingerprint_at_pause,omitempty"`
	PackManifestSHAAtPause string `json:"pack_manifest_sha_at_pause,omitempty"`
	LastSuccessAt          string `json:"last_success_at,omitempty"`
	LastErrorAt            string `json:"last_error_at,omitempty"`
	LastError              string `json:"last_error,omitempty"`
}

// BridgeState models <vault>/memory/channels/<safe-id>.json (0600, RankChannels=65).
type BridgeState struct {
	SchemaVersion       int                       `json:"schema_version"`
	BridgeID            string                    `json:"bridge_id"`
	LastDeskInitialized bool                      `json:"last_desk_initialized,omitempty"`
	LastDeskCID         string                    `json:"last_desk_cid,omitempty"`
	LastDeskLeaseState  string                    `json:"last_desk_lease_state,omitempty"`
	DeskThread          DeskThreadRef             `json:"desk_thread,omitempty"`
	ItemThreads         []ItemThreadEntry         `json:"item_threads,omitempty"`
	PendingReplies      []PendingReplyCorrelation `json:"pending_replies,omitempty"`
	RecentDeliveredKeys []SeenKeyEntry            `json:"recent_delivered_keys,omitempty"`
	RecentInboundKeys   []SeenKeyEntry            `json:"recent_inbound_keys,omitempty"`
	RateWindow          RateWindowState           `json:"rate_window,omitempty"`
	PendingBind         *PendingDeskBind          `json:"pending_bind,omitempty"`
	RetryQueue          []QueuedNotification      `json:"retry_queue,omitempty"`
	Health              BridgeHealth              `json:"health"`
	UpdatedAt           string                    `json:"updated_at,omitempty"`
}

// AuditEntry models a single zero-PII SHA-256 audit row in <vault>/memory/channels/channels.jsonl.
type AuditEntry struct {
	TS                 string `json:"ts"`
	BridgeID           string `json:"bridge_id"`
	Op                 string `json:"op"`
	Kind               string `json:"kind,omitempty"`
	IdempotencyKey     string `json:"idempotency_key,omitempty"`
	DeskCID            string `json:"desk_cid,omitempty"`
	DestSHA256         string `json:"dest_sha256,omitempty"`
	ExternalThreadHash string `json:"external_thread_hash,omitempty"`
	PayloadSHA256      string `json:"payload_sha256"`
	Outcome            string `json:"outcome"`
	Attempt            int    `json:"attempt,omitempty"`
	LatencyMs          int64  `json:"latency_ms,omitempty"`
	ErrorCode          string `json:"error_code,omitempty"`
}

func memoryChannelsDir(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "channels")
}

func channelsLockPath(vaultDir string) string {
	return filepath.Join(memoryChannelsDir(vaultDir), ".channels.lock")
}

func auditLogPath(vaultDir string) string {
	return filepath.Join(memoryChannelsDir(vaultDir), "channels.jsonl")
}

func statePathForBridge(vaultDir, fqid string) (string, error) {
	stem, err := SafeBridgeFileStem(fqid)
	if err != nil {
		return "", err
	}
	return filepath.Join(memoryChannelsDir(vaultDir), stem+".json"), nil
}

// WithChannelsLock executes fn while holding RankChannels (65) on <vault>/memory/channels/.channels.lock.
// Callers MUST NOT invoke extension, events, or outbox locks inside fn (Rank 10..60 < Rank 65).
func WithChannelsLock(ctx context.Context, vaultDir string, fn func(context.Context) error) error {
	return store.With(ctx, store.RankChannels, channelsLockPath(vaultDir), fn)
}

func defaultBridgeState(fqid string) BridgeState {
	return BridgeState{
		SchemaVersion: BridgeStateSchemaVersion,
		BridgeID:      fqid,
		Health: BridgeHealth{
			Status: HealthStatusHealthy,
		},
	}
}

func loadBridgeStateUnlocked(vaultDir, fqid string) (BridgeState, error) {
	p, err := statePathForBridge(vaultDir, fqid)
	if err != nil {
		return defaultBridgeState(fqid), err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultBridgeState(fqid), nil
		}
		return defaultBridgeState(fqid), err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return defaultBridgeState(fqid), nil
	}
	st := defaultBridgeState(fqid)
	if err := json.Unmarshal(data, &st); err != nil {
		return defaultBridgeState(fqid), err
	}
	if st.SchemaVersion == 0 {
		st.SchemaVersion = BridgeStateSchemaVersion
	}
	if st.BridgeID == "" {
		st.BridgeID = fqid
	}
	if st.Health.Status == "" {
		st.Health.Status = HealthStatusHealthy
	}
	return st, nil
}

func saveBridgeStateUnlocked(vaultDir string, st BridgeState, now time.Time) error {
	p, err := statePathForBridge(vaultDir, st.BridgeID)
	if err != nil {
		return err
	}
	st.SchemaVersion = BridgeStateSchemaVersion
	// Never persist external room permalinks onto disk (prevents room URL exposure).
	st.DeskThread.Permalink = ""
	if !now.IsZero() {
		st.UpdatedAt = now.UTC().Format(time.RFC3339)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(p, append(data, '\n'), 0o600)
}

func pruneSeenKeys(entries []SeenKeyEntry, now time.Time) []SeenKeyEntry {
	if len(entries) == 0 {
		return nil
	}
	kept := make([]SeenKeyEntry, 0, len(entries))
	for _, e := range entries {
		exp, err := time.Parse(time.RFC3339, e.ExpiresAt)
		if err != nil || !now.After(exp) {
			kept = append(kept, e)
		}
	}
	if len(kept) > MaxRecentKeys {
		kept = kept[len(kept)-MaxRecentKeys:]
	}
	return kept
}

func hasSeenKey(entries []SeenKeyEntry, key string, now time.Time) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	for _, e := range entries {
		if e.Key != key {
			continue
		}
		if exp, err := time.Parse(time.RFC3339, e.ExpiresAt); err == nil && now.After(exp) {
			continue
		}
		return true
	}
	return false
}

func addSeenKey(entries []SeenKeyEntry, key string, ttl time.Duration, now time.Time) []SeenKeyEntry {
	entries = pruneSeenKeys(entries, now)
	key = strings.TrimSpace(key)
	if key == "" || hasSeenKey(entries, key, now) {
		return entries
	}
	entries = append(entries, SeenKeyEntry{
		Key:       key,
		CreatedAt: now.UTC().Format(time.RFC3339),
		ExpiresAt: now.Add(ttl).UTC().Format(time.RFC3339),
	})
	if len(entries) > MaxRecentKeys {
		entries = entries[len(entries)-MaxRecentKeys:]
	}
	return entries
}

func pruneItemThreads(entries []ItemThreadEntry, now time.Time) []ItemThreadEntry {
	if len(entries) == 0 {
		return nil
	}
	cutoff := now.Add(-ItemThreadTTL)
	kept := make([]ItemThreadEntry, 0, len(entries))
	for _, e := range entries {
		if t, err := time.Parse(time.RFC3339, e.UpdatedAt); err == nil && t.Before(cutoff) {
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) > MaxItemThreads {
		kept = kept[len(kept)-MaxItemThreads:]
	}
	return kept
}

func upsertItemThread(entries []ItemThreadEntry, updated ItemThreadEntry, now time.Time) []ItemThreadEntry {
	entries = pruneItemThreads(entries, now)
	if strings.TrimSpace(updated.ItemKey) == "" || strings.TrimSpace(updated.ExternalThreadID) == "" {
		return entries
	}
	updated.UpdatedAt = now.UTC().Format(time.RFC3339)
	if updated.Status == "" {
		updated.Status = "open"
	}
	out := make([]ItemThreadEntry, 0, len(entries)+1)
	for _, e := range entries {
		if e.ItemKey == updated.ItemKey {
			if updated.TargetConvID == "" {
				updated.TargetConvID = e.TargetConvID
			}
			if updated.TaskSlug == "" {
				updated.TaskSlug = e.TaskSlug
			}
			if updated.ProposalID == "" {
				updated.ProposalID = e.ProposalID
			}
			if updated.Kind == "" {
				updated.Kind = e.Kind
			}
			continue
		}
		out = append(out, e)
	}
	out = append(out, updated)
	if len(out) > MaxItemThreads {
		out = out[len(out)-MaxItemThreads:]
	}
	return out
}

func prunePendingReplies(entries []PendingReplyCorrelation, now time.Time) []PendingReplyCorrelation {
	if len(entries) == 0 {
		return nil
	}
	kept := make([]PendingReplyCorrelation, 0, len(entries))
	for _, c := range entries {
		if exp, err := time.Parse(time.RFC3339, c.ExpiresAt); err == nil && now.After(exp) {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) > MaxPendingReplies {
		kept = kept[len(kept)-MaxPendingReplies:]
	}
	return kept
}

// enqueueWithBoundedEviction enforces MaxRetryQueueEntries (64), evicting oldest `task_done` -> `task_blocked`
// before ever dropping `critical_breach`, `interrupt`, or `desk_reply`. Returns `(updatedQueue, true)` only when newly inserted.
func enqueueWithBoundedEviction(queue []QueuedNotification, item QueuedNotification) ([]QueuedNotification, bool) {
	for _, existing := range queue {
		if existing.IdempotencyKey == item.IdempotencyKey {
			return queue, false
		}
	}
	item.Notification.BodyMarkdown = truncateUTF8Runes(item.Notification.BodyMarkdown, MaxQueuedBodyRunes)
	queue = append(queue, item)
	for len(queue) > MaxRetryQueueEntries {
		evictIdx := -1
		for i, q := range queue {
			if q.Notification.Kind == KindTaskDone {
				evictIdx = i
				break
			}
		}
		if evictIdx < 0 {
			for i, q := range queue {
				if q.Notification.Kind == KindTaskBlocked {
					evictIdx = i
					break
				}
			}
		}
		if evictIdx < 0 {
			evictIdx = 0
		}
		queue = append(queue[:evictIdx], queue[evictIdx+1:]...)
	}
	return queue, true
}

func retryBackoffDuration(attempt int, retryAfterSec int) time.Duration {
	if retryAfterSec > 0 {
		d := time.Duration(retryAfterSec) * time.Second
		if d < 5*time.Second {
			return 5 * time.Second
		}
		if d > time.Hour {
			return time.Hour
		}
		return d
	}
	switch {
	case attempt <= 1:
		return 15 * time.Second
	case attempt == 2:
		return time.Minute
	case attempt == 3:
		return 5 * time.Minute
	case attempt == 4:
		return 15 * time.Minute
	default:
		return 30 * time.Minute
	}
}

func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sha256Short12(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return sha256HexBytes([]byte(s))[:12]
}

func appendAuditEntry(vaultDir string, entry AuditEntry) error {
	if strings.TrimSpace(vaultDir) == "" {
		return nil
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return store.AppendLineNoFollow(auditLogPath(vaultDir), raw, 0o600)
}

// CompactAuditLog trims <vault>/memory/channels/channels.jsonl to at most MaxHotAuditLines (5000).
func CompactAuditLog(ctx context.Context, vaultDir string) error {
	return WithChannelsLock(ctx, vaultDir, func(_ context.Context) error {
		p := auditLogPath(vaultDir)
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		var lines [][]byte
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64*1024), 512*1024)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) > 0 {
				cp := make([]byte, len(line))
				copy(cp, line)
				lines = append(lines, cp)
			}
		}
		if len(lines) <= MaxHotAuditLines {
			return nil
		}
		lines = lines[len(lines)-MaxHotAuditLines:]
		var out bytes.Buffer
		for _, l := range lines {
			out.Write(l)
			out.WriteByte('\n')
		}
		return store.WriteAtomic(p, out.Bytes(), 0o600)
	})
}

// ReadRecentAuditEntries returns up to `limit` most-recent SHA-256 audit rows from channels.jsonl.
func ReadRecentAuditEntries(vaultDir string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 500 {
		limit = 500
	}
	p := auditLogPath(vaultDir)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var all []AuditEntry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 512*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e AuditEntry
		if json.Unmarshal(line, &e) == nil {
			all = append(all, e)
		}
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}
