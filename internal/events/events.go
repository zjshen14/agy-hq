package events

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
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
	DefaultMaxAttempts = 5
	DefaultLeaseTTL    = 15 * time.Minute
	DedupArchiveWindow = 7 * 24 * time.Hour
)

// Event models a single trigger or continuation row in local/memory/events.json.
type Event struct {
	ID             string            `json:"id"`
	CreatedAt      string            `json:"createdAt"`
	UpdatedAt      string            `json:"updatedAt,omitempty"`
	AvailableAt    string            `json:"availableAt"`
	Source         string            `json:"source"`
	DedupKey       string            `json:"dedupKey,omitempty"`
	ScopeKey       string            `json:"scopeKey"`
	SessionID      string            `json:"sessionId,omitempty"`
	Skill          string            `json:"skill,omitempty"`
	Model          string            `json:"model,omitempty"`
	Title          string            `json:"title"`
	Prompt         string            `json:"prompt"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	Status         EventStatus       `json:"status"`
	LeaseID        string            `json:"leaseId,omitempty"`
	LeaseConvID    string            `json:"leaseConvId,omitempty"`
	LeaseExpiresAt string            `json:"leaseExpiresAt,omitempty"`
	AttemptCount   int               `json:"attemptCount"`
	MaxAttempts    int               `json:"maxAttempts"`
	LastError      string            `json:"lastError,omitempty"`
}

// LeasedBatch groups one or more pending events sharing a ScopeKey and SessionID
// into a single serialized agent run.
type LeasedBatch struct {
	LeaseID   string  `json:"leaseId"`
	ScopeKey  string  `json:"scopeKey"`
	SessionID string  `json:"sessionId,omitempty"`
	Skill     string  `json:"skill,omitempty"`
	Model     string  `json:"model,omitempty"`
	Title     string  `json:"title"`
	Prompt    string  `json:"prompt"`
	Events    []Event `json:"events"`
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
	inProcMu   sync.Mutex
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
	inProcMu.Lock()
	defer inProcMu.Unlock()

	memDir := filepath.Join(vaultDir, "memory")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(lockFilePath(vaultDir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()

	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	}()
	return fn()
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
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if items == nil {
		items = []Event{}
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp." + generateHex(4)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
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

func loadRecentArchivedDedupUnlocked(vaultDir string, now time.Time, window time.Duration) map[string]bool {
	seen := make(map[string]bool)
	f, err := os.Open(archiveEventsPath(vaultDir))
	if err != nil {
		return seen
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
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.DedupKey == "" || ev.Status != StatusDone {
			continue
		}
		ts, err := parseEventTime(ev.UpdatedAt)
		if err != nil {
			ts, err = parseEventTime(ev.CreatedAt)
		}
		if err == nil && ts.Before(cutoff) {
			continue
		}
		seen[ev.DedupKey] = true
	}
	return seen
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
			recentDone := loadRecentArchivedDedupUnlocked(vaultDir, now, DedupArchiveWindow)
			if recentDone[ev.DedupKey] {
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

		// 1. Reap expired leases first.
		for _, ev := range active {
			if ev.Status == StatusLeased && ev.LeaseExpiresAt != "" {
				if exp, pErr := parseEventTime(ev.LeaseExpiresAt); pErr == nil && now.After(exp) {
					dirty = true
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

		// 3. Group ready pending events by ScopeKey (and matching SessionID of the head event in that scope).
		type groupAcc struct {
			scopeKey  string
			sessionID string
			indices   []int
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
			g, exists := groups[ev.ScopeKey]
			if !exists {
				g = &groupAcc{
					scopeKey:  ev.ScopeKey,
					sessionID: ev.SessionID,
					indices:   []int{i},
				}
				groups[ev.ScopeKey] = g
				groupOrder = append(groupOrder, ev.ScopeKey)
			} else if g.sessionID == ev.SessionID {
				g.indices = append(g.indices, i)
			}
		}

		if len(groupOrder) == 0 && !dirty {
			return nil
		}

		expStr := now.Add(leaseTTL).UTC().Format(time.RFC3339)
		for _, sk := range groupOrder {
			g := groups[sk]
			leaseID := fmt.Sprintf("lease-%s-%s", now.UTC().Format("150405"), generateHex(3))
			batchEvents := make([]Event, 0, len(g.indices))
			for _, idx := range g.indices {
				active[idx].Status = StatusLeased
				active[idx].LeaseID = leaseID
				active[idx].LeaseExpiresAt = expStr
				active[idx].UpdatedAt = nowStr
				if g.sessionID != "" {
					active[idx].LeaseConvID = g.sessionID
				}
				batchEvents = append(batchEvents, active[idx])
			}
			head := batchEvents[0]
			batches = append(batches, LeasedBatch{
				LeaseID:   leaseID,
				ScopeKey:  g.scopeKey,
				SessionID: g.sessionID,
				Skill:     head.Skill,
				Model:     head.Model,
				Title:     formatBatchTitle(batchEvents),
				Prompt:    formatBatchPrompt(batchEvents),
				Events:    batchEvents,
			})
			dirty = true
		}

		if len(dropped) > 0 {
			_ = appendArchiveUnlocked(vaultDir, dropped)
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
				active[i].LeaseConvID = convID
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

// RenewLeaseByConversation extends LeaseExpiresAt for all leased events bound to convID.
func RenewLeaseByConversation(vaultDir, convID string, now time.Time, leaseTTL time.Duration) error {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	if leaseTTL <= 0 {
		leaseTTL = DefaultLeaseTTL
	}
	return withEventsLock(vaultDir, func() error {
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

// CompleteByConversation transitions all leased events bound to convID to StatusDone and archives them.
func CompleteByConversation(vaultDir, convID string, now time.Time) ([]Event, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
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
			if ev.Status == StatusLeased && ev.LeaseConvID == convID {
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
