package events

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zjshen14/agy-hq/internal/store"
)

// EventStatus represents the lifecycle state of an Event in the durable Event Inbox.
type EventStatus string

const (
	StatusPending EventStatus = "pending"
	StatusLeased  EventStatus = "leased"
	StatusDone    EventStatus = "done"
	StatusDropped EventStatus = "dropped"
)

const (
	DefaultMaxAttempts      = 5
	DefaultLeaseTTL         = 15 * time.Minute
	DedupArchiveWindow      = 7 * 24 * time.Hour
	MaxBatchEvents          = 8
	MaxCoalescedWatchEvents = 40
)

// Event models a single trigger or continuation row in local/memory/events.json.
type Event struct {
	ID                   string            `json:"id"`
	CreatedAt            string            `json:"createdAt"`
	UpdatedAt            string            `json:"updatedAt,omitempty"`
	AvailableAt          string            `json:"availableAt"`
	Source               string            `json:"source"`
	DedupKey             string            `json:"dedupKey,omitempty"`
	DedupIncludesDropped bool              `json:"dedupIncludesDropped,omitempty"`
	ScopeKey             string            `json:"scopeKey"`
	SessionID            string            `json:"sessionId,omitempty"`
	Skill                string            `json:"skill,omitempty"`
	Model                string            `json:"model,omitempty"`
	Title                string            `json:"title"`
	Prompt               string            `json:"prompt"`
	Metadata             map[string]string `json:"metadata,omitempty"`
	TaintOnSpawn         bool              `json:"taintOnSpawn,omitempty"`
	SpawnTokenHash       string            `json:"spawnTokenHash,omitempty"`
	Status               EventStatus       `json:"status"`
	LeaseID              string            `json:"leaseId,omitempty"`
	LeaseConvID          string            `json:"leaseConvId,omitempty"`
	LeaseExpiresAt       string            `json:"leaseExpiresAt,omitempty"`
	AttemptCount         int               `json:"attemptCount"`
	MaxAttempts          int               `json:"maxAttempts"`
	LastError            string            `json:"lastError,omitempty"`
}

// LeasedBatch groups one or more pending events sharing a ScopeKey and SessionID
// into a single serialized agent run.
type LeasedBatch struct {
	LeaseID      string  `json:"leaseId"`
	ScopeKey     string  `json:"scopeKey"`
	SessionID    string  `json:"sessionId,omitempty"`
	Skill        string  `json:"skill,omitempty"`
	Model        string  `json:"model,omitempty"`
	Title        string  `json:"title"`
	Prompt       string  `json:"prompt"`
	TaintOnSpawn bool    `json:"taintOnSpawn,omitempty"`
	SpawnToken   string  `json:"-"` // Ephemeral raw token injected into worker prompt; only SpawnTokenHash is persisted
	Events       []Event `json:"events"`
}

// EventIDs returns the slice of Event IDs included in the batch.
func (b LeasedBatch) EventIDs() []string {
	ids := make([]string, 0, len(b.Events))
	for _, ev := range b.Events {
		ids = append(ids, ev.ID)
	}
	return ids
}

var (
	wakeMu     sync.Mutex
	wakeNotify chan struct{}
)

// SubscribeWake registers and returns a buffered 1-element channel signaled whenever Enqueue commits a new event.
func SubscribeWake() <-chan struct{} {
	wakeMu.Lock()
	defer wakeMu.Unlock()
	wakeNotify = make(chan struct{}, 1)
	return wakeNotify
}

// TriggerWake nudges the in-process daemon select loop immediately when running inside `hq daemon`.
func TriggerWake() {
	signalWake()
}

func signalWake() {
	wakeMu.Lock()
	ch := wakeNotify
	wakeMu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func activeEventsPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "events.json")
}

func archiveEventsPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "events_archive.jsonl")
}

func lockFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", ".events.lock")
}

func daemonTokenPath(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "daemon_token")
}

func generateHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func generateEventID(now time.Time) string {
	return fmt.Sprintf("evt-%s-%s", now.UTC().Format("20060102-150405"), generateHex(3))
}

func withEventsLock(vaultDir string, fn func() error) error {
	return store.With(context.Background(), store.RankEvents, lockFilePath(vaultDir), func(_ context.Context) error {
		return fn()
	})
}

func loadActiveUnlocked(vaultDir string) ([]Event, error) {
	data, err := os.ReadFile(activeEventsPath(vaultDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var items []Event
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func saveActiveUnlocked(vaultDir string, items []Event) error {
	p := activeEventsPath(vaultDir)
	if items == nil {
		items = []Event{}
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(p, append(data, '\n'), 0o600)
}

func appendArchiveUnlocked(vaultDir string, items []Event) error {
	if len(items) == 0 {
		return nil
	}
	p := archiveEventsPath(vaultDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			continue
		}
		_, _ = f.Write(append(b, '\n'))
	}
	return nil
}

func loadRecentArchivedDedupWithDroppedUnlocked(vaultDir string, now time.Time, window time.Duration) (done map[string]bool, dropped map[string]bool) {
	done = make(map[string]bool)
	dropped = make(map[string]bool)
	f, err := os.Open(archiveEventsPath(vaultDir))
	if err != nil {
		return done, dropped
	}
	defer f.Close()

	cutoff := now.Add(-window)
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev Event
		if json.Unmarshal([]byte(line), &ev) != nil || ev.DedupKey == "" {
			continue
		}
		if ev.Status != StatusDone && ev.Status != StatusDropped {
			continue
		}
		ts, err := parseEventTime(ev.UpdatedAt)
		if err != nil {
			ts, err = parseEventTime(ev.CreatedAt)
		}
		if err == nil && ts.Before(cutoff) {
			continue
		}
		if ev.Status == StatusDone {
			done[ev.DedupKey] = true
		} else if ev.Status == StatusDropped {
			dropped[ev.DedupKey] = true
		}
	}
	return done, dropped
}

func loadRecentArchivedDedupUnlocked(vaultDir string, now time.Time, window time.Duration) map[string]bool {
	done, _ := loadRecentArchivedDedupWithDroppedUnlocked(vaultDir, now, window)
	return done
}

func parseEventTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, raw)
}

func defaultScopeKey(ev Event) string {
	if s := strings.TrimSpace(ev.ScopeKey); s != "" {
		return s
	}
	if s := strings.TrimSpace(ev.SessionID); s != "" {
		return "session:" + s
	}
	if s := strings.TrimSpace(ev.Skill); s != "" {
		return "skill:" + s
	}
	if s := strings.TrimSpace(ev.Source); s != "" {
		return "source:" + s
	}
	return "default"
}

func retryBackoff(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return 5 * time.Second
	case attempt == 2:
		return 30 * time.Second
	case attempt == 3:
		return 2 * time.Minute
	default:
		return 10 * time.Minute
	}
}

// Enqueue validates, deduplicates, and persists an Event to local/memory/events.json.
// Returns (storedEvent, deduplicated, error).
func Enqueue(vaultDir string, ev Event, now time.Time) (Event, bool, error) {
	if now.IsZero() {
		now = time.Now()
	}
	ev.Source = strings.TrimSpace(ev.Source)
	if ev.Source == "" {
		ev.Source = "manual"
	}
	ev.Prompt = strings.TrimSpace(ev.Prompt)
	ev.Title = strings.TrimSpace(ev.Title)
	if ev.Prompt == "" && ev.Title == "" {
		return Event{}, false, fmt.Errorf("event requires non-empty title or prompt")
	}
	if ev.Title == "" {
		ev.Title = ev.Prompt
		if r := []rune(ev.Title); len(r) > 72 {
			ev.Title = string(r[:69]) + "..."
		}
	}
	if ev.Prompt == "" {
		ev.Prompt = ev.Title
	}
	ev.ScopeKey = defaultScopeKey(ev)
	ev.DedupKey = strings.TrimSpace(ev.DedupKey)
	if ev.MaxAttempts <= 0 {
		ev.MaxAttempts = DefaultMaxAttempts
	}

	var stored Event
	deduped := false

	err := withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err != nil {
			return err
		}
		if ev.DedupKey != "" {
			for _, existing := range active {
				if existing.DedupKey == ev.DedupKey && (existing.Status == StatusPending || existing.Status == StatusLeased) {
					stored = existing
					deduped = true
					return nil
				}
			}
			recentDone, recentDropped := loadRecentArchivedDedupWithDroppedUnlocked(vaultDir, now, DedupArchiveWindow)
			if recentDone[ev.DedupKey] || (ev.DedupIncludesDropped && recentDropped[ev.DedupKey]) {
				ev.Status = StatusDone
				stored = ev
				deduped = true
				return nil
			}
		}

		nowStr := now.UTC().Format(time.RFC3339)
		if strings.TrimSpace(ev.ID) == "" {
			ev.ID = generateEventID(now)
		}
		ev.CreatedAt = nowStr
		ev.UpdatedAt = nowStr
		if strings.TrimSpace(ev.AvailableAt) == "" {
			ev.AvailableAt = nowStr
		}
		ev.Status = StatusPending
		active = append(active, ev)
		if err := saveActiveUnlocked(vaultDir, active); err != nil {
			return err
		}
		stored = ev
		return nil
	})
	if err == nil && !deduped {
		signalWake()
	}
	return stored, deduped, err
}

// EnqueueBatch atomically validates, deduplicates, and persists multiple Events in a single RankEvents lock + atomic write.
func EnqueueBatch(vaultDir string, evs []Event, now time.Time) (stored []Event, dedupCount int, err error) {
	if len(evs) == 0 {
		return nil, 0, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	err = withEventsLock(vaultDir, func() error {
		active, lErr := loadActiveUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		recentDone, recentDropped := loadRecentArchivedDedupWithDroppedUnlocked(vaultDir, now, DedupArchiveWindow)
		activeDedup := make(map[string]Event, len(active))
		for _, ex := range active {
			if ex.DedupKey != "" && (ex.Status == StatusPending || ex.Status == StatusLeased) {
				activeDedup[ex.DedupKey] = ex
			}
		}
		nowStr := now.UTC().Format(time.RFC3339)
		addedAny := false
		for _, ev := range evs {
			ev.Source = strings.TrimSpace(ev.Source)
			if ev.Source == "" {
				ev.Source = "manual"
			}
			ev.Prompt = strings.TrimSpace(ev.Prompt)
			ev.Title = strings.TrimSpace(ev.Title)
			if ev.Prompt == "" && ev.Title == "" {
				continue
			}
			if ev.Title == "" {
				ev.Title = ev.Prompt
				if r := []rune(ev.Title); len(r) > 72 {
					ev.Title = string(r[:69]) + "..."
				}
			}
			if ev.Prompt == "" {
				ev.Prompt = ev.Title
			}
			ev.ScopeKey = defaultScopeKey(ev)
			ev.DedupKey = strings.TrimSpace(ev.DedupKey)
			if ev.MaxAttempts <= 0 {
				ev.MaxAttempts = DefaultMaxAttempts
			}
			if ev.DedupKey != "" {
				if ex, ok := activeDedup[ev.DedupKey]; ok {
					stored = append(stored, ex)
					dedupCount++
					continue
				}
				if recentDone[ev.DedupKey] || (ev.DedupIncludesDropped && recentDropped[ev.DedupKey]) {
					ev.Status = StatusDone
					stored = append(stored, ev)
					dedupCount++
					continue
				}
			}
			if strings.TrimSpace(ev.ID) == "" {
				ev.ID = generateEventID(now)
			}
			ev.CreatedAt = nowStr
			ev.UpdatedAt = nowStr
			if strings.TrimSpace(ev.AvailableAt) == "" {
				ev.AvailableAt = nowStr
			}
			ev.Status = StatusPending
			active = append(active, ev)
			if ev.DedupKey != "" {
				activeDedup[ev.DedupKey] = ev
			}
			stored = append(stored, ev)
			addedAny = true
		}
		if !addedAny {
			return nil
		}
		return saveActiveUnlocked(vaultDir, active)
	})
	if err == nil && len(stored) > dedupCount {
		signalWake()
	}
	return stored, dedupCount, err
}

// HasActiveOrCompletedDedup returns true if dedupKey is currently pending, leased, or completed within DedupArchiveWindow.
func HasActiveOrCompletedDedup(vaultDir, dedupKey string, now time.Time) bool {
	dedupKey = strings.TrimSpace(dedupKey)
	if dedupKey == "" {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	found := false
	_ = withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err == nil {
			for _, ev := range active {
				if ev.DedupKey == dedupKey && (ev.Status == StatusPending || ev.Status == StatusLeased) {
					found = true
					return nil
				}
			}
		}
		recentDone := loadRecentArchivedDedupUnlocked(vaultDir, now, DedupArchiveWindow)
		if recentDone[dedupKey] {
			found = true
		}
		return nil
	})
	return found
}

// List returns active events filtered by optional status (empty status returns all active events).
// When includeArchived is true, up to maxArchive most recent archived rows are appended.
func List(vaultDir string, statusFilter string, includeArchived bool, maxArchive int) ([]Event, error) {
	statusFilter = strings.ToLower(strings.TrimSpace(statusFilter))
	var out []Event
	err := withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err != nil {
			return err
		}
		for _, ev := range active {
			if statusFilter == "" || string(ev.Status) == statusFilter {
				out = append(out, ev)
			}
		}
		if includeArchived || statusFilter == string(StatusDone) || statusFilter == string(StatusDropped) {
			archived := readArchivedTailUnlocked(vaultDir, maxArchive)
			for _, ev := range archived {
				if statusFilter == "" || string(ev.Status) == statusFilter {
					out = append(out, ev)
				}
			}
		}
		return nil
	})
	return out, err
}

func readArchivedTailUnlocked(vaultDir string, limit int) []Event {
	if limit <= 0 {
		limit = 50
	}
	f, err := os.Open(archiveEventsPath(vaultDir))
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []Event
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev Event
		if json.Unmarshal([]byte(line), &ev) == nil {
			all = append(all, ev)
		}
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all
}

// Inspect looks up an Event by ID across active and archived stores.
func Inspect(vaultDir, id string) (Event, error) {
	id = strings.TrimSpace(id)
	var found Event
	var ok bool
	err := withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err != nil {
			return err
		}
		for _, ev := range active {
			if ev.ID == id {
				found = ev
				ok = true
				return nil
			}
		}
		for _, ev := range readArchivedTailUnlocked(vaultDir, 500) {
			if ev.ID == id {
				found = ev
				ok = true
			}
		}
		return nil
	})
	if err != nil {
		return Event{}, err
	}
	if !ok {
		return Event{}, fmt.Errorf("event %q not found", id)
	}
	return found, nil
}

// LeaseNextBatches reaps expired leases, enforces single-active-worker serialization per ScopeKey,
// batches ready pending events sharing (ScopeKey, SessionID), and persists leased state.
// Also returns any events newly dropped due to exceeding MaxAttempts on lease expiry.
func LeaseNextBatches(vaultDir string, now time.Time, leaseTTL time.Duration) ([]LeasedBatch, []Event, error) {
	if now.IsZero() {
		now = time.Now()
	}
	if leaseTTL <= 0 {
		leaseTTL = DefaultLeaseTTL
	}
	var batches []LeasedBatch
	var dropped []Event

	err := withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err != nil {
			return err
		}
		if len(active) == 0 {
			return nil
		}

		nowStr := now.UTC().Format(time.RFC3339)
		dirty := false
		var kept []Event
		var completedAcked []Event

		// 1. Reap expired leases first.
		for _, ev := range active {
			if ev.Status == StatusLeased && ev.LeaseExpiresAt != "" {
				if exp, pErr := parseEventTime(ev.LeaseExpiresAt); pErr == nil && now.After(exp) {
					dirty = true
					if ev.Metadata != nil && ev.Metadata["acked"] == "1" {
						ev.Status = StatusDone
						ev.UpdatedAt = nowStr
						ev.LeaseExpiresAt = ""
						completedAcked = append(completedAcked, ev)
						continue
					}
					ev.AttemptCount++
					ev.UpdatedAt = nowStr
					if ev.AttemptCount >= ev.MaxAttempts {
						ev.Status = StatusDropped
						if ev.LastError == "" {
							ev.LastError = fmt.Sprintf("worker lease expired after %d attempts", ev.AttemptCount)
						}
						dropped = append(dropped, ev)
						continue
					}
					ev.Status = StatusPending
					ev.LeaseID = ""
					ev.LeaseConvID = ""
					ev.LeaseExpiresAt = ""
					ev.AvailableAt = now.Add(retryBackoff(ev.AttemptCount)).UTC().Format(time.RFC3339)
					ev.LastError = "worker lease expired; requeued for retry"
				}
			}
			kept = append(kept, ev)
		}
		active = kept

		// 2. Identify ScopeKeys currently busy with an active worker lease.
		busyScopes := make(map[string]bool)
		for _, ev := range active {
			if ev.Status == StatusLeased {
				busyScopes[ev.ScopeKey] = true
			}
		}

		watcherThreadKey := func(ev Event) string {
			if ev.Metadata == nil {
				return ""
			}
			if !strings.HasPrefix(ev.Source, "connector:") && ev.Metadata["requires_ack"] != "1" {
				return ""
			}
			return strings.TrimSpace(ev.Metadata["thread_key"])
		}

		// 3. Group ready pending events by ScopeKey (and matching SessionID of the head event in that scope).
		type groupAcc struct {
			scopeKey      string
			sessionID     string
			indices       []int
			uniqueThreads map[string]bool
		}
		var groupOrder []string
		groups := make(map[string]*groupAcc)

		for i, ev := range active {
			if ev.Status != StatusPending {
				continue
			}
			if busyScopes[ev.ScopeKey] {
				continue
			}
			if ev.AvailableAt != "" {
				if avail, pErr := parseEventTime(ev.AvailableAt); pErr == nil && now.Before(avail) {
					continue
				}
			}
			tk := watcherThreadKey(ev)
			g, exists := groups[ev.ScopeKey]
			if !exists {
				ut := make(map[string]bool)
				if tk != "" {
					ut[tk] = true
				}
				g = &groupAcc{
					scopeKey:      ev.ScopeKey,
					sessionID:     ev.SessionID,
					indices:       []int{i},
					uniqueThreads: ut,
				}
				groups[ev.ScopeKey] = g
				groupOrder = append(groupOrder, ev.ScopeKey)
			} else if g.sessionID == ev.SessionID {
				if tk != "" && len(g.uniqueThreads) > 0 {
					if g.uniqueThreads[tk] {
						if len(g.indices) < MaxCoalescedWatchEvents {
							g.indices = append(g.indices, i)
						}
					} else if len(g.uniqueThreads) < MaxBatchEvents && len(g.indices) < MaxCoalescedWatchEvents {
						g.uniqueThreads[tk] = true
						g.indices = append(g.indices, i)
					}
				} else if len(g.indices) < MaxBatchEvents {
					g.indices = append(g.indices, i)
				}
			}
		}

		if len(groupOrder) == 0 && !dirty {
			return nil
		}

		expStr := now.Add(leaseTTL).UTC().Format(time.RFC3339)
		for _, sk := range groupOrder {
			g := groups[sk]
			leaseID := fmt.Sprintf("lease-%s-%s", now.UTC().Format("150405"), generateHex(3))
			spawnToken := ""
			spawnTokenHash := ""
			if g.sessionID == "" {
				spawnToken = generateHex(16)
				sum := sha256.Sum256([]byte(spawnToken))
				spawnTokenHash = hex.EncodeToString(sum[:])
			}
			taintBatch := false
			batchEvents := make([]Event, 0, len(g.indices))
			for _, idx := range g.indices {
				active[idx].Status = StatusLeased
				active[idx].LeaseID = leaseID
				active[idx].LeaseExpiresAt = expStr
				active[idx].UpdatedAt = nowStr
				if spawnTokenHash != "" {
					active[idx].SpawnTokenHash = spawnTokenHash
				}
				if g.sessionID != "" {
					active[idx].LeaseConvID = g.sessionID
				}
				if active[idx].TaintOnSpawn {
					taintBatch = true
				}
				batchEvents = append(batchEvents, active[idx])
			}
			head := batchEvents[0]
			batches = append(batches, LeasedBatch{
				LeaseID:      leaseID,
				ScopeKey:     g.scopeKey,
				SessionID:    g.sessionID,
				Skill:        head.Skill,
				Model:        head.Model,
				Title:        formatBatchTitle(batchEvents),
				Prompt:       formatBatchPrompt(batchEvents),
				TaintOnSpawn: taintBatch,
				SpawnToken:   spawnToken,
				Events:       batchEvents,
			})
			dirty = true
		}

		if len(completedAcked)+len(dropped) > 0 {
			toArchive := make([]Event, 0, len(completedAcked)+len(dropped))
			toArchive = append(toArchive, completedAcked...)
			toArchive = append(toArchive, dropped...)
			_ = appendArchiveUnlocked(vaultDir, toArchive)
		}
		if dirty {
			return saveActiveUnlocked(vaultDir, active)
		}
		return nil
	})

	return batches, dropped, err
}

func formatBatchTitle(evs []Event) string {
	if len(evs) == 0 {
		return "[Auto] Event Run"
	}
	if len(evs) == 1 {
		return evs[0].Title
	}
	return fmt.Sprintf("%s (+%d coalesced)", evs[0].Title, len(evs)-1)
}

func formatBatchPrompt(evs []Event) string {
	if len(evs) == 0 {
		return ""
	}
	if len(evs) == 1 {
		return evs[0].Prompt
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[Batched Events (%d) for Scope %q — Address all items in one consolidated run]\n\n", len(evs), evs[0].ScopeKey))

	type threadGroup struct {
		threadKey string
		itemIDs   []string
		seenItem  map[string]bool
		eventIDs  []string
		rep       Event
	}
	conn := ""
	acct := ""
	allWatcher := true
	var threadOrder []string
	byThread := make(map[string]*threadGroup)
	for _, ev := range evs {
		if ev.Metadata == nil || strings.TrimSpace(ev.Metadata["thread_key"]) == "" || strings.TrimSpace(ev.Metadata["connector"]) == "" {
			allWatcher = false
			break
		}
		evConn := strings.TrimSpace(ev.Metadata["connector"])
		evAcct := strings.TrimSpace(ev.Metadata["account"])
		if conn == "" {
			conn = evConn
			acct = evAcct
		} else if conn != evConn || acct != evAcct {
			allWatcher = false
			break
		}
		tk := strings.TrimSpace(ev.Metadata["thread_key"])
		tg, ok := byThread[tk]
		if !ok {
			tg = &threadGroup{
				threadKey: tk,
				seenItem:  make(map[string]bool),
				rep:       ev,
			}
			byThread[tk] = tg
			threadOrder = append(threadOrder, tk)
		} else if strings.Contains(ev.Prompt, "[Precheck Urgency Signals:") && !strings.Contains(tg.rep.Prompt, "[Precheck Urgency Signals:") {
			tg.rep = ev
		}
		tg.eventIDs = append(tg.eventIDs, ev.ID)
		addItem := func(raw string) {
			raw = strings.TrimSpace(raw)
			if raw != "" && !tg.seenItem[raw] {
				tg.seenItem[raw] = true
				tg.itemIDs = append(tg.itemIDs, raw)
			}
		}
		addItem(ev.Metadata["item_key"])
		addItem(ev.Metadata["item_id"])
		for _, cid := range strings.Split(ev.Metadata["coalesced_item_ids"], ",") {
			addItem(cid)
		}
	}

	if allWatcher && len(threadOrder) > 0 {
		acctFlag := ""
		if acct != "" {
			acctFlag = " --account=" + acct
		}
		sb.WriteString("Quarantined strict-argv mode: invoke single canonical `hq` commands ONLY — never use shell `for` loops, `;`, `&&`, `||`, or pipes.\n")
		sb.WriteString(fmt.Sprintf("- Read all %d unique thread(s) in one canonical call: `hq watch read --connector=%s%s --thread=%s`\n",
			len(threadOrder), conn, acctFlag, strings.Join(threadOrder, ",")))
		sb.WriteString("- Emit `hq outbox add` (and optional `hq offer stage`) once per noteworthy thread.\n")
		sb.WriteString("- Acknowledge completion before ending the turn via `hq watch ack --all` (or incrementally via `hq watch ack --item=...`).\n\n")

		if len(threadOrder) < len(evs) {
			for i, tk := range threadOrder {
				tg := byThread[tk]
				sb.WriteString(fmt.Sprintf("### Thread %d/%d (`thread=%s` · items: `%s` · events: `%s` · source: `%s`)\n%s\n\n",
					i+1, len(threadOrder), tk, strings.Join(tg.itemIDs, ","), strings.Join(tg.eventIDs, ","), tg.rep.Source, tg.rep.Prompt))
			}
			return strings.TrimSpace(sb.String())
		}
	}

	for i, ev := range evs {
		sb.WriteString(fmt.Sprintf("### Event %d/%d (`%s` · source: `%s` · %s)\n%s\n\n", i+1, len(evs), ev.ID, ev.Source, ev.Title, ev.Prompt))
	}
	return strings.TrimSpace(sb.String())
}

// BindBatchConversation attaches the resolved Antigravity conversation ID to a leased batch.
func BindBatchConversation(vaultDir string, eventIDs []string, convID string, now time.Time, leaseTTL time.Duration) error {
	convID = strings.TrimSpace(convID)
	if len(eventIDs) == 0 || convID == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	if leaseTTL <= 0 {
		leaseTTL = DefaultLeaseTTL
	}
	idSet := make(map[string]bool, len(eventIDs))
	for _, id := range eventIDs {
		idSet[id] = true
	}
	return withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err != nil {
			return err
		}
		changed := false
		nowStr := now.UTC().Format(time.RFC3339)
		expStr := now.Add(leaseTTL).UTC().Format(time.RFC3339)
		for i := range active {
			if idSet[active[i].ID] && active[i].Status == StatusLeased {
				if active[i].LeaseConvID != "" && active[i].LeaseConvID != convID {
					return fmt.Errorf("events: batch event %s already bound to conversation %s", active[i].ID, active[i].LeaseConvID)
				}
				if active[i].LeaseConvID != convID {
					active[i].LeaseConvID = convID
					active[i].LeaseExpiresAt = expStr
					active[i].UpdatedAt = nowStr
					changed = true
				}
			}
		}
		if !changed {
			return nil
		}
		return saveActiveUnlocked(vaultDir, active)
	})
}

// BindBatchByToken atomically CAS-binds a leased event batch matching sha256(rawToken) to convID,
// refusing to overwrite an already-bound foreign LeaseConvID.
func BindBatchByToken(vaultDir, rawToken, convID string, now time.Time, leaseTTL time.Duration) ([]Event, bool, error) {
	rawToken = strings.TrimSpace(rawToken)
	convID = strings.TrimSpace(convID)
	if rawToken == "" || convID == "" {
		return nil, false, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	if leaseTTL <= 0 {
		leaseTTL = DefaultLeaseTTL
	}
	sum := sha256.Sum256([]byte(rawToken))
	wantHash := hex.EncodeToString(sum[:])

	var bound []Event
	matched := false
	err := withEventsLock(vaultDir, func() error {
		active, lErr := loadActiveUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		changed := false
		nowStr := now.UTC().Format(time.RFC3339)
		expStr := now.Add(leaseTTL).UTC().Format(time.RFC3339)
		for i := range active {
			if active[i].Status != StatusLeased || len(active[i].SpawnTokenHash) != len(wantHash) {
				continue
			}
			if subtle.ConstantTimeCompare([]byte(active[i].SpawnTokenHash), []byte(wantHash)) != 1 {
				continue
			}
			// CAS: reject hijacking if already bound to a different conversation
			if active[i].LeaseConvID != "" && active[i].LeaseConvID != convID {
				return fmt.Errorf("events: spawn token already bound to another conversation")
			}
			matched = true
			if active[i].LeaseConvID != convID {
				active[i].LeaseConvID = convID
				active[i].LeaseExpiresAt = expStr
				active[i].UpdatedAt = nowStr
				changed = true
			}
			bound = append(bound, active[i])
		}
		if !changed {
			return nil
		}
		return saveActiveUnlocked(vaultDir, active)
	})
	return bound, matched, err
}

// ListLeasedByConversation returns all currently leased events bound to convID.
func ListLeasedByConversation(vaultDir, convID string) ([]Event, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return nil, nil
	}
	var out []Event
	err := withEventsLock(vaultDir, func() error {
		active, lErr := loadActiveUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		for _, ev := range active {
			if ev.Status == StatusLeased && ev.LeaseConvID == convID {
				out = append(out, ev)
			}
		}
		return nil
	})
	return out, err
}

// AckWatcherEvents marks Metadata["acked"]="1" on leased watcher events bound to convID matching itemKeys (or all if itemKeys is empty).
func AckWatcherEvents(vaultDir, convID string, itemKeys []string, now time.Time) (int, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return 0, fmt.Errorf("convID is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	keySet := make(map[string]bool, len(itemKeys))
	for _, k := range itemKeys {
		k = strings.TrimSpace(k)
		if k != "" {
			keySet[k] = true
		}
	}
	acked := 0
	err := withEventsLock(vaultDir, func() error {
		active, lErr := loadActiveUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		nowStr := now.UTC().Format(time.RFC3339)
		changed := false
		for i := range active {
			if active[i].Status != StatusLeased || active[i].LeaseConvID != convID {
				continue
			}
			if len(keySet) > 0 {
				matchedKey := keySet[active[i].ID]
				if !matchedKey && active[i].Metadata != nil {
					if keySet[active[i].Metadata["item_key"]] || keySet[active[i].Metadata["item_id"]] || keySet[active[i].Metadata["thread_key"]] {
						matchedKey = true
					} else {
						for _, cid := range strings.Split(active[i].Metadata["coalesced_item_ids"], ",") {
							if keySet[strings.TrimSpace(cid)] {
								matchedKey = true
								break
							}
						}
					}
				}
				if !matchedKey {
					continue
				}
			}
			if active[i].Metadata == nil {
				active[i].Metadata = make(map[string]string)
			}
			active[i].Metadata["acked"] = "1"
			active[i].UpdatedAt = nowStr
			acked++
			changed = true
		}
		if !changed {
			return nil
		}
		return saveActiveUnlocked(vaultDir, active)
	})
	return acked, err
}

// TryRenewLeaseByConversation performs a non-blocking lease renewal (LOCK_NB) suitable for hot PreToolUse hooks
// so active tool calls never stall waiting behind background batch dispatchers.
func TryRenewLeaseByConversation(vaultDir, convID string, now time.Time, leaseTTL time.Duration) (bool, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return false, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	if leaseTTL <= 0 {
		leaseTTL = DefaultLeaseTTL
	}
	return store.TryWith(context.Background(), store.RankEvents, lockFilePath(vaultDir), func(_ context.Context) error {
		active, err := loadActiveUnlocked(vaultDir)
		if err != nil || len(active) == 0 {
			return err
		}
		changed := false
		expStr := now.Add(leaseTTL).UTC().Format(time.RFC3339)
		nowStr := now.UTC().Format(time.RFC3339)
		for i := range active {
			if active[i].Status == StatusLeased && active[i].LeaseConvID == convID {
				active[i].LeaseExpiresAt = expStr
				active[i].UpdatedAt = nowStr
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return saveActiveUnlocked(vaultDir, active)
	})
}

// DeferBatchOnSpawnError returns leased events to StatusPending when SpawnTask or session delivery fails.
// When quotaDeferred is true (e.g., CircuitOpen / daily spawn cap), AttemptCount is NOT incremented so events
// wait safely for quota recovery without dead-lettering.
func DeferBatchOnSpawnError(vaultDir string, eventIDs []string, reason string, quotaDeferred bool, now time.Time) ([]Event, error) {
	if len(eventIDs) == 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	idSet := make(map[string]bool, len(eventIDs))
	for _, id := range eventIDs {
		idSet[id] = true
	}
	var dropped []Event
	err := withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err != nil {
			return err
		}
		nowStr := now.UTC().Format(time.RFC3339)
		var kept []Event
		changed := false
		for _, ev := range active {
			if !idSet[ev.ID] {
				kept = append(kept, ev)
				continue
			}
			changed = true
			ev.UpdatedAt = nowStr
			ev.LastError = strings.TrimSpace(reason)
			ev.LeaseID = ""
			ev.LeaseConvID = ""
			ev.LeaseExpiresAt = ""
			if quotaDeferred {
				ev.Status = StatusPending
				ev.AvailableAt = now.Add(30 * time.Second).UTC().Format(time.RFC3339)
				kept = append(kept, ev)
				continue
			}
			ev.AttemptCount++
			if ev.AttemptCount >= ev.MaxAttempts {
				ev.Status = StatusDropped
				dropped = append(dropped, ev)
				continue
			}
			ev.Status = StatusPending
			ev.AvailableAt = now.Add(retryBackoff(ev.AttemptCount)).UTC().Format(time.RFC3339)
			kept = append(kept, ev)
		}
		if len(dropped) > 0 {
			_ = appendArchiveUnlocked(vaultDir, dropped)
		}
		if changed {
			return saveActiveUnlocked(vaultDir, kept)
		}
		return nil
	})
	return dropped, err
}

// CompleteIDs transitions specific Event IDs to StatusDone and archives them.
func CompleteIDs(vaultDir string, eventIDs []string, now time.Time) ([]Event, error) {
	if len(eventIDs) == 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	idSet := make(map[string]bool, len(eventIDs))
	for _, id := range eventIDs {
		idSet[id] = true
	}
	var completed []Event
	err := withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err != nil || len(active) == 0 {
			return err
		}
		nowStr := now.UTC().Format(time.RFC3339)
		var kept []Event
		for _, ev := range active {
			if idSet[ev.ID] {
				ev.Status = StatusDone
				ev.UpdatedAt = nowStr
				ev.LeaseExpiresAt = ""
				completed = append(completed, ev)
			} else {
				kept = append(kept, ev)
			}
		}
		if len(completed) == 0 {
			return nil
		}
		if err := appendArchiveUnlocked(vaultDir, completed); err != nil {
			return err
		}
		return saveActiveUnlocked(vaultDir, kept)
	})
	return completed, err
}

// FinalizeByConversation atomically finalizes leased events bound to convID under a single RankEvents lock:
// events with Metadata["requires_ack"]=="1" and Metadata["acked"]!="1" are requeued with AttemptCount++
// (or transitioned to StatusDropped once AttemptCount >= MaxAttempts); all other leased events move to StatusDone.
func FinalizeByConversation(vaultDir, convID string, now time.Time) (completed []Event, requeued []Event, dropped []Event, err error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return nil, nil, nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	err = withEventsLock(vaultDir, func() error {
		active, lErr := loadActiveUnlocked(vaultDir)
		if lErr != nil || len(active) == 0 {
			return lErr
		}
		nowStr := now.UTC().Format(time.RFC3339)
		var kept []Event
		changed := false
		for _, ev := range active {
			if ev.Status == StatusLeased && ev.LeaseConvID == convID {
				changed = true
				requiresAck := ev.Metadata != nil && ev.Metadata["requires_ack"] == "1"
				acked := ev.Metadata != nil && ev.Metadata["acked"] == "1"
				if requiresAck && !acked {
					ev.AttemptCount++
					ev.UpdatedAt = nowStr
					ev.LeaseID = ""
					ev.LeaseConvID = ""
					ev.LeaseExpiresAt = ""
					ev.SpawnTokenHash = ""
					maxAtt := ev.MaxAttempts
					if maxAtt <= 0 {
						maxAtt = DefaultMaxAttempts
					}
					if ev.AttemptCount >= maxAtt {
						ev.Status = StatusDropped
						ev.LastError = "worker exited without acknowledging item (`hq watch ack`)"
						dropped = append(dropped, ev)
					} else {
						ev.Status = StatusPending
						ev.AvailableAt = now.Add(retryBackoff(ev.AttemptCount)).UTC().Format(time.RFC3339)
						ev.LastError = "unacknowledged on Stop; requeued for retry"
						kept = append(kept, ev)
						requeued = append(requeued, ev)
					}
					continue
				}
				ev.Status = StatusDone
				ev.UpdatedAt = nowStr
				ev.LeaseExpiresAt = ""
				completed = append(completed, ev)
			} else {
				kept = append(kept, ev)
			}
		}
		if !changed {
			return nil
		}
		if len(completed)+len(dropped) > 0 {
			toArchive := make([]Event, 0, len(completed)+len(dropped))
			toArchive = append(toArchive, completed...)
			toArchive = append(toArchive, dropped...)
			if aErr := appendArchiveUnlocked(vaultDir, toArchive); aErr != nil {
				return aErr
			}
		}
		return saveActiveUnlocked(vaultDir, kept)
	})
	return completed, requeued, dropped, err
}

// AreEventIDsFinishedOrAcked reports whether all eventIDs bound to an `[Auto]` worker (convID)
// have either left the active event store (`StatusDone` / `StatusDropped`), been requeued or
// leased to another conversation, expired past `LeaseExpiresAt`, or been explicitly acknowledged
// (`Metadata["acked"]=="1"`) for at least `ackGrace`. Any still-leased acked IDs ready to finalize
// are returned in `ackedToComplete`.
func AreEventIDsFinishedOrAcked(vaultDir string, eventIDs []string, convID string, ackGrace time.Duration, now time.Time) (finished bool, ackedToComplete []string) {
	if now.IsZero() {
		now = time.Now()
	}
	convID = strings.TrimSpace(convID)
	finished = true

	_ = withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err != nil {
			finished = false
			return err
		}
		if len(active) == 0 {
			return nil
		}
		if len(eventIDs) == 0 {
			if convID == "" {
				return nil
			}
			for _, ev := range active {
				if ev.Status == StatusLeased && ev.LeaseConvID == convID {
					if ev.LeaseExpiresAt != "" {
						if exp, pErr := parseEventTime(ev.LeaseExpiresAt); pErr == nil && now.After(exp) {
							if ev.Metadata != nil && ev.Metadata["acked"] == "1" {
								ackedToComplete = append(ackedToComplete, ev.ID)
							}
							continue
						}
					}
					if ev.Metadata != nil && ev.Metadata["acked"] == "1" {
						if ackGrace <= 0 {
							ackedToComplete = append(ackedToComplete, ev.ID)
							continue
						}
						if upd, uErr := parseEventTime(ev.UpdatedAt); uErr == nil && now.Sub(upd) >= ackGrace {
							ackedToComplete = append(ackedToComplete, ev.ID)
							continue
						}
					}
					finished = false
					ackedToComplete = nil
					return nil
				}
			}
			return nil
		}

		byID := make(map[string]Event, len(active))
		for _, ev := range active {
			byID[ev.ID] = ev
		}
		for _, rawID := range eventIDs {
			id := strings.TrimSpace(rawID)
			if id == "" {
				continue
			}
			ev, exists := byID[id]
			if !exists {
				// Already completed or dropped into events_archive.jsonl.
				continue
			}
			if ev.Status != StatusLeased {
				// Requeued (StatusPending) after lease expiry; no longer owned by this worker.
				continue
			}
			if convID != "" && ev.LeaseConvID != "" && ev.LeaseConvID != convID {
				// Leased to a superseding retry worker conversation.
				continue
			}
			if ev.LeaseExpiresAt != "" {
				if exp, pErr := parseEventTime(ev.LeaseExpiresAt); pErr == nil && now.After(exp) {
					if ev.Metadata != nil && ev.Metadata["acked"] == "1" {
						ackedToComplete = append(ackedToComplete, ev.ID)
					}
					continue
				}
			}
			if ev.Metadata != nil && ev.Metadata["acked"] == "1" {
				if ackGrace <= 0 {
					ackedToComplete = append(ackedToComplete, ev.ID)
					continue
				}
				if upd, uErr := parseEventTime(ev.UpdatedAt); uErr == nil && now.Sub(upd) >= ackGrace {
					ackedToComplete = append(ackedToComplete, ev.ID)
					continue
				}
			}
			finished = false
			ackedToComplete = nil
			return nil
		}
		return nil
	})
	return finished, ackedToComplete
}

// Retry resets an active leased or archived dropped event back to StatusPending for immediate execution.
func Retry(vaultDir, id string, now time.Time) (Event, error) {
	id = strings.TrimSpace(id)
	if now.IsZero() {
		now = time.Now()
	}
	var updated Event
	err := withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err != nil {
			return err
		}
		nowStr := now.UTC().Format(time.RFC3339)
		for i := range active {
			if active[i].ID == id {
				active[i].Status = StatusPending
				active[i].AvailableAt = nowStr
				active[i].LeaseID = ""
				active[i].LeaseConvID = ""
				active[i].LeaseExpiresAt = ""
				active[i].UpdatedAt = nowStr
				updated = active[i]
				return saveActiveUnlocked(vaultDir, active)
			}
		}
		// Check archived dropped items for manual retry
		for _, arch := range readArchivedTailUnlocked(vaultDir, 500) {
			if arch.ID == id {
				arch.Status = StatusPending
				arch.AttemptCount = 0
				arch.AvailableAt = nowStr
				arch.LeaseID = ""
				arch.LeaseConvID = ""
				arch.LeaseExpiresAt = ""
				arch.UpdatedAt = nowStr
				active = append(active, arch)
				updated = arch
				return saveActiveUnlocked(vaultDir, active)
			}
		}
		return fmt.Errorf("event %q not found", id)
	})
	if err == nil {
		signalWake()
	}
	return updated, err
}

// DropPendingBySchedule transitions all pending (unleased) events matching scheduleID (bare or FQID) to StatusDropped.
func DropPendingBySchedule(vaultDir, scheduleID, reason string, now time.Time) (int, error) {
	bare := strings.TrimPrefix(strings.TrimSpace(scheduleID), "core/")
	fqid := scheduleID
	if !strings.Contains(fqid, "/") && fqid != "" {
		fqid = "core/" + fqid
	}
	if now.IsZero() {
		now = time.Now()
	}
	var dropped []Event
	err := withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err != nil || len(active) == 0 {
			return err
		}
		nowStr := now.UTC().Format(time.RFC3339)
		var kept []Event
		for _, ev := range active {
			schedMeta := ""
			if ev.Metadata != nil {
				schedMeta = ev.Metadata["schedule_id"]
			}
			if ev.Status == StatusPending && (schedMeta == bare || schedMeta == fqid || ev.ScopeKey == "gatekeeper:"+bare || ev.ScopeKey == "gatekeeper:"+fqid) {
				ev.Status = StatusDropped
				ev.UpdatedAt = nowStr
				ev.LastError = strings.TrimSpace(reason)
				dropped = append(dropped, ev)
			} else {
				kept = append(kept, ev)
			}
		}
		if len(dropped) == 0 {
			return nil
		}
		if err := appendArchiveUnlocked(vaultDir, dropped); err != nil {
			return err
		}
		return saveActiveUnlocked(vaultDir, kept)
	})
	return len(dropped), err
}

// Drop transitions an active event to StatusDropped and moves it to events_archive.jsonl.
func Drop(vaultDir, id, reason string, now time.Time) (Event, error) {
	id = strings.TrimSpace(id)
	if now.IsZero() {
		now = time.Now()
	}
	var dropped Event
	err := withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err != nil {
			return err
		}
		nowStr := now.UTC().Format(time.RFC3339)
		var kept []Event
		found := false
		for _, ev := range active {
			if ev.ID == id {
				found = true
				ev.Status = StatusDropped
				ev.UpdatedAt = nowStr
				if strings.TrimSpace(reason) != "" {
					ev.LastError = strings.TrimSpace(reason)
				}
				dropped = ev
			} else {
				kept = append(kept, ev)
			}
		}
		if !found {
			return fmt.Errorf("active event %q not found", id)
		}
		if err := appendArchiveUnlocked(vaultDir, []Event{dropped}); err != nil {
			return err
		}
		return saveActiveUnlocked(vaultDir, kept)
	})
	return dropped, err
}

// SummaryStats summarizes Event Inbox health for `hq status` and `hq doctor`.
type SummaryStats struct {
	PendingCount int      `json:"pendingCount"`
	LeasedCount  int      `json:"leasedCount"`
	Dropped24h   int      `json:"dropped24h"`
	BusyScopes   []string `json:"busyScopes,omitempty"`
}

// GetSummaryStats computes current Event Inbox queue counts.
func GetSummaryStats(vaultDir string, now time.Time) SummaryStats {
	if now.IsZero() {
		now = time.Now()
	}
	var st SummaryStats
	_ = withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err == nil {
			scopeSet := make(map[string]bool)
			for _, ev := range active {
				switch ev.Status {
				case StatusPending:
					st.PendingCount++
				case StatusLeased:
					st.LeasedCount++
					scopeSet[ev.ScopeKey] = true
				}
			}
			for s := range scopeSet {
				st.BusyScopes = append(st.BusyScopes, s)
			}
			sort.Strings(st.BusyScopes)
		}
		cutoff := now.Add(-24 * time.Hour)
		for _, arch := range readArchivedTailUnlocked(vaultDir, 200) {
			if arch.Status != StatusDropped {
				continue
			}
			if ts, pErr := parseEventTime(arch.UpdatedAt); pErr == nil && ts.After(cutoff) {
				st.Dropped24h++
			}
		}
		return nil
	})
	return st
}

// EnsureDaemonToken reads or creates a 256-bit loopback bearer secret in local/private/daemon_token (0600).
func EnsureDaemonToken(vaultDir string) (string, error) {
	p := daemonTokenPath(vaultDir)
	if raw, err := os.ReadFile(p); err == nil {
		if tok := strings.TrimSpace(string(raw)); len(tok) >= 32 {
			return tok, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	tok := generateHex(32)
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// ValidateDaemonToken performs constant-time verification against local/private/daemon_token.
func ValidateDaemonToken(vaultDir, candidate string) bool {
	c := strings.TrimSpace(candidate)
	if len(c) < 32 {
		return false
	}
	raw, err := os.ReadFile(daemonTokenPath(vaultDir))
	if err != nil {
		return false
	}
	expected := strings.TrimSpace(string(raw))
	if len(expected) < 32 || len(c) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(c)) == 1
}

func watcherEventMatchesFilter(ev Event, connFilter, acctFilter string) bool {
	if !strings.HasPrefix(ev.Source, "connector:") {
		return false
	}
	rest := strings.TrimPrefix(ev.Source, "connector:")
	cPart, aPart, hasColon := strings.Cut(rest, ":")
	evConn := strings.TrimSpace(cPart)
	evAcct := "default"
	if hasColon && strings.TrimSpace(aPart) != "" {
		evAcct = strings.TrimSpace(aPart)
	}
	if ev.Metadata != nil {
		if c := strings.TrimSpace(ev.Metadata["connector"]); c != "" {
			evConn = c
		}
		if a := strings.TrimSpace(ev.Metadata["account"]); a != "" {
			evAcct = a
		}
	}
	if connFilter != "" && evConn != connFilter && "core/"+evConn != connFilter {
		return false
	}
	if acctFilter != "" && evAcct != acctFilter {
		return false
	}
	return true
}

// ClearDroppedWatcherEvents removes dropped `connector:*` events matching optional connector/account filters
// from both active events.json and events_archive.jsonl so reset-poisoned items are not blocked by DedupIncludesDropped.
func ClearDroppedWatcherEvents(vaultDir, connFilter, acctFilter string) (int, error) {
	connFilter = strings.TrimSpace(connFilter)
	acctFilter = strings.TrimSpace(acctFilter)
	if strings.TrimSpace(vaultDir) == "" {
		return 0, nil
	}
	removed := 0
	err := withEventsLock(vaultDir, func() error {
		if active, err := loadActiveUnlocked(vaultDir); err == nil {
			kept := make([]Event, 0, len(active))
			changed := false
			for _, ev := range active {
				if ev.Status == StatusDropped && watcherEventMatchesFilter(ev, connFilter, acctFilter) {
					removed++
					changed = true
					continue
				}
				kept = append(kept, ev)
			}
			if changed {
				if wErr := saveActiveUnlocked(vaultDir, kept); wErr != nil {
					return wErr
				}
			}
		}

		archPath := archiveEventsPath(vaultDir)
		f, oErr := os.Open(archPath)
		if oErr != nil {
			if os.IsNotExist(oErr) {
				return nil
			}
			return oErr
		}
		var outBuf bytes.Buffer
		archChanged := false
		scanner := bufio.NewScanner(f)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)
		for scanner.Scan() {
			rawLine := strings.TrimSpace(scanner.Text())
			if rawLine == "" {
				continue
			}
			var ev Event
			if json.Unmarshal([]byte(rawLine), &ev) == nil && ev.Status == StatusDropped && watcherEventMatchesFilter(ev, connFilter, acctFilter) {
				removed++
				archChanged = true
				continue
			}
			outBuf.WriteString(rawLine)
			outBuf.WriteByte('\n')
		}
		scanErr := scanner.Err()
		_ = f.Close()
		if scanErr != nil {
			return scanErr
		}
		if archChanged {
			return store.WriteAtomic(archPath, outBuf.Bytes(), 0o600)
		}
		return nil
	})
	return removed, err
}

// ListPendingWatcherEvents returns a snapshot of StatusPending `connector:*` events matching optional filters.
func ListPendingWatcherEvents(vaultDir, connFilter, acctFilter string) ([]Event, error) {
	connFilter = strings.TrimSpace(connFilter)
	acctFilter = strings.TrimSpace(acctFilter)
	var out []Event
	err := withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err != nil {
			return err
		}
		for _, ev := range active {
			if ev.Status == StatusPending && watcherEventMatchesFilter(ev, connFilter, acctFilter) {
				out = append(out, ev)
			}
		}
		return nil
	})
	return out, err
}

// WatcherBacklogPruneResult summarizes 0-token backlog pruning and thread coalescing in events.json.
type WatcherBacklogPruneResult struct {
	EvaluatedPending    int                 `json:"evaluated_pending"`
	AutomatedPruned     int                 `json:"automated_pruned"`
	DuplicatesCoalesced int                 `json:"duplicates_coalesced"`
	RemainingPending    int                 `json:"remaining_pending"`
	AutoAckedByAccount  map[string][]string `json:"auto_acked_by_account,omitempty"`
}

// ApplyWatcherBacklogPrune atomically completes newly-classified automated pending watcher events
// and optionally collapses duplicate pending events sharing `(ScopeKey, connector, account, thread_key)`
// into a single representative pending event carrying `Metadata["coalesced_item_ids"]`.
func ApplyWatcherBacklogPrune(vaultDir, connFilter, acctFilter string, autoRollupEventIDs map[string]bool, dedupThreads, dryRun bool, now time.Time) (WatcherBacklogPruneResult, error) {
	connFilter = strings.TrimSpace(connFilter)
	acctFilter = strings.TrimSpace(acctFilter)
	if now.IsZero() {
		now = time.Now()
	}
	res := WatcherBacklogPruneResult{
		AutoAckedByAccount: make(map[string][]string),
	}
	err := withEventsLock(vaultDir, func() error {
		active, err := loadActiveUnlocked(vaultDir)
		if err != nil || len(active) == 0 {
			return err
		}
		nowStr := now.UTC().Format(time.RFC3339)
		var kept []Event
		var archivedDone []Event
		repIdxByThread := make(map[string]int)

		collectItemIDs := func(ev Event) []string {
			var ids []string
			seen := make(map[string]bool)
			add := func(raw string) {
				raw = strings.TrimSpace(raw)
				if raw != "" && !seen[raw] {
					seen[raw] = true
					ids = append(ids, raw)
				}
			}
			if ev.Metadata != nil {
				add(ev.Metadata["item_key"])
				add(ev.Metadata["item_id"])
				for _, cid := range strings.Split(ev.Metadata["coalesced_item_ids"], ",") {
					add(cid)
				}
			}
			return ids
		}

		resolveConnAcct := func(ev Event) (string, string) {
			rest := strings.TrimPrefix(ev.Source, "connector:")
			cPart, aPart, hasColon := strings.Cut(rest, ":")
			evConn := strings.TrimSpace(cPart)
			evAcct := "default"
			if hasColon && strings.TrimSpace(aPart) != "" {
				evAcct = strings.TrimSpace(aPart)
			}
			if ev.Metadata != nil {
				if c := strings.TrimSpace(ev.Metadata["connector"]); c != "" {
					evConn = c
				}
				if a := strings.TrimSpace(ev.Metadata["account"]); a != "" {
					evAcct = a
				}
			}
			return evConn, evAcct
		}

		for _, ev := range active {
			if ev.Status != StatusPending || !watcherEventMatchesFilter(ev, connFilter, acctFilter) {
				kept = append(kept, ev)
				continue
			}
			res.EvaluatedPending++
			evConn, evAcct := resolveConnAcct(ev)

			if autoRollupEventIDs[ev.ID] {
				res.AutomatedPruned++
				acctKey := evConn + ":" + evAcct
				res.AutoAckedByAccount[acctKey] = append(res.AutoAckedByAccount[acctKey], collectItemIDs(ev)...)
				ev.Status = StatusDone
				ev.UpdatedAt = nowStr
				ev.LastError = "pruned via 0-token automated backlog rollup"
				archivedDone = append(archivedDone, ev)
				continue
			}

			tk := ""
			if ev.Metadata != nil {
				tk = strings.TrimSpace(ev.Metadata["thread_key"])
			}
			if dedupThreads && tk != "" {
				groupKey := ev.ScopeKey + "\x00" + evConn + "\x00" + evAcct + "\x00" + tk
				if repIdx, exists := repIdxByThread[groupKey]; exists {
					res.DuplicatesCoalesced++
					rep := kept[repIdx]
					if rep.Metadata == nil {
						rep.Metadata = make(map[string]string)
					}
					mergedIDs := collectItemIDs(rep)
					seenMerged := make(map[string]bool, len(mergedIDs))
					for _, id := range mergedIDs {
						seenMerged[id] = true
					}
					for _, id := range collectItemIDs(ev) {
						if !seenMerged[id] {
							seenMerged[id] = true
							mergedIDs = append(mergedIDs, id)
						}
					}
					if len(mergedIDs) > 0 {
						rep.Metadata["coalesced_item_ids"] = strings.Join(mergedIDs, ",")
					}
					if ev.Metadata != nil && ev.Metadata["tier"] == "routine" && rep.Metadata["tier"] != "routine" {
						rep.Metadata["tier"] = "routine"
						if ev.Model != "" {
							rep.Model = ev.Model
						}
					}
					if strings.Contains(ev.Prompt, "[Precheck Urgency Signals:") && !strings.Contains(rep.Prompt, "[Precheck Urgency Signals:") {
						rep.Prompt = ev.Prompt
					}
					if ev.AttemptCount < rep.AttemptCount {
						rep.AttemptCount = ev.AttemptCount
						rep.AvailableAt = ev.AvailableAt
					}
					rep.UpdatedAt = nowStr
					kept[repIdx] = rep

					ev.Status = StatusDone
					ev.UpdatedAt = nowStr
					ev.LastError = "coalesced into representative thread event " + rep.ID
					archivedDone = append(archivedDone, ev)
					continue
				}
				repIdxByThread[groupKey] = len(kept)
			}

			res.RemainingPending++
			kept = append(kept, ev)
		}

		if dryRun || len(archivedDone) == 0 {
			return nil
		}
		if err := appendArchiveUnlocked(vaultDir, archivedDone); err != nil {
			return err
		}
		return saveActiveUnlocked(vaultDir, kept)
	})
	return res, err
}
