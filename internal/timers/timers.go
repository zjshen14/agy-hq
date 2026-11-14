package timers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zjshen14/agy-hq/internal/events"
)

// TimerCategory distinguishes agent/user one-shot Follow-Ups from recurring Schedules.
type TimerCategory string

const (
	TimerFollowUp TimerCategory = "follow_up"
	TimerSchedule TimerCategory = "schedule"
)

const (
	StatusArmed     = "armed"
	StatusDone      = "done"
	StatusCancelled = "cancelled"

	MinFollowUpHorizon = 1 * time.Minute
	MaxActiveFollowUps = 50
)

// Timer models a durable future deadline in local/memory/timers.json.
type Timer struct {
	ID              string        `json:"id"`
	Category        TimerCategory `json:"category"`
	Status          string        `json:"status"` // armed | done | cancelled
	DueAt           string        `json:"dueAt"`
	CronExpr        string        `json:"cronExpr,omitempty"`
	ScopeKey        string        `json:"scopeKey"`
	SessionID       string        `json:"sessionId,omitempty"`
	Skill           string        `json:"skill,omitempty"`
	Model           string        `json:"model,omitempty"`
	Title           string        `json:"title"`
	Prompt          string        `json:"prompt"`
	CreatedByConvID string        `json:"createdByConvId,omitempty"`
	CreatedAt       string        `json:"createdAt"`
	FiredAt         string        `json:"firedAt,omitempty"`
}

// FollowUpRequest specifies parameters for setting a one-shot Follow-Up timer.
type FollowUpRequest struct {
	DueAt           time.Time
	InDuration      time.Duration
	ScopeKey        string
	SessionID       string
	Skill           string
	Model           string
	Title           string
	Prompt          string
	CreatedByConvID string
}

var inProcTimerMu sync.Mutex

func timersFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "timers.json")
}

func timersLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", ".timers.lock")
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func withTimersLock(vaultDir string, fn func() error) error {
	inProcTimerMu.Lock()
	defer inProcTimerMu.Unlock()

	memDir := filepath.Join(vaultDir, "memory")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(timersLockPath(vaultDir), os.O_CREATE|os.O_RDWR, 0o600)
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

func loadTimersUnlocked(vaultDir string) ([]Timer, error) {
	data, err := os.ReadFile(timersFilePath(vaultDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}
	var list []Timer
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func saveTimersUnlocked(vaultDir string, list []Timer) error {
	p := timersFilePath(vaultDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if list == nil {
		list = []Timer{}
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp." + randomHex(4)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func parseTimerDue(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, raw)
}

// SetFollowUp creates a durable one-shot Follow-Up timer enforcing minimum horizon (>=1m) and active cap (<=50).
func SetFollowUp(vaultDir string, req FollowUpRequest, now time.Time) (Timer, error) {
	if now.IsZero() {
		now = time.Now()
	}
	due := req.DueAt
	if due.IsZero() && req.InDuration > 0 {
		due = now.Add(req.InDuration)
	}
	if due.IsZero() {
		return Timer{}, fmt.Errorf("follow-up timer requires --in=<duration> or --at=<RFC3339>")
	}
	if due.Sub(now) < MinFollowUpHorizon-time.Second {
		return Timer{}, fmt.Errorf("follow-up horizon must be at least %s (got %s)", MinFollowUpHorizon, due.Sub(now).Round(time.Second))
	}

	prompt := strings.TrimSpace(req.Prompt)
	title := strings.TrimSpace(req.Title)
	if prompt == "" && title == "" {
		return Timer{}, fmt.Errorf("follow-up timer requires non-empty --prompt or --title")
	}
	if title == "" {
		title = "[Auto] Follow-Up: " + prompt
		if r := []rune(title); len(r) > 72 {
			title = string(r[:69]) + "..."
		}
	}
	if prompt == "" {
		prompt = title
	}

	sessionID := strings.TrimSpace(req.SessionID)
	skill := strings.TrimSpace(req.Skill)
	scope := strings.TrimSpace(req.ScopeKey)
	if scope == "" {
		if sessionID != "" {
			scope = "session:" + sessionID
		} else if skill != "" {
			scope = "skill:" + skill
		} else {
			scope = "follow_up"
		}
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "gemini-4-hydrogen-low"
	}

	var created Timer
	err := withTimersLock(vaultDir, func() error {
		list, err := loadTimersUnlocked(vaultDir)
		if err != nil {
			return err
		}
		activeFollowUps := 0
		for _, t := range list {
			if t.Category == TimerFollowUp && t.Status == StatusArmed {
				activeFollowUps++
			}
		}
		if activeFollowUps >= MaxActiveFollowUps {
			return fmt.Errorf("active follow-up timer cap (%d) reached; cancel stale timers via `hq timers cancel <id>`", MaxActiveFollowUps)
		}

		nowStr := now.UTC().Format(time.RFC3339)
		tm := Timer{
			ID:              fmt.Sprintf("tmr-%s-%s", now.UTC().Format("20060102-150405"), randomHex(2)),
			Category:        TimerFollowUp,
			Status:          StatusArmed,
			DueAt:           due.UTC().Format(time.RFC3339),
			ScopeKey:        scope,
			SessionID:       sessionID,
			Skill:           skill,
			Model:           model,
			Title:           title,
			Prompt:          prompt,
			CreatedByConvID: strings.TrimSpace(req.CreatedByConvID),
			CreatedAt:       nowStr,
		}
		list = pruneOldCompletedTimers(append(list, tm))
		if err := saveTimersUnlocked(vaultDir, list); err != nil {
			return err
		}
		created = tm
		return nil
	})
	return created, err
}

// UpsertSchedule creates or updates a recurring Schedule timer backed by a 5-field cron expression.
func UpsertSchedule(vaultDir, id, cronExpr, scopeKey, skill, model, title, prompt string, now time.Time) (Timer, error) {
	if now.IsZero() {
		now = time.Now()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return Timer{}, fmt.Errorf("schedule id is required")
	}
	nextDue, err := NextCronOccurrence(cronExpr, now)
	if err != nil {
		return Timer{}, err
	}
	if strings.TrimSpace(scopeKey) == "" {
		if strings.TrimSpace(skill) != "" {
			scopeKey = "skill:" + strings.TrimSpace(skill)
		} else {
			scopeKey = "schedule:" + id
		}
	}

	var stored Timer
	err = withTimersLock(vaultDir, func() error {
		list, err := loadTimersUnlocked(vaultDir)
		if err != nil {
			return err
		}
		nowStr := now.UTC().Format(time.RFC3339)
		timerID := "sched-" + id
		updated := false
		for i := range list {
			if list[i].ID == timerID {
				list[i].CronExpr = strings.TrimSpace(cronExpr)
				list[i].ScopeKey = scopeKey
				list[i].Skill = strings.TrimSpace(skill)
				list[i].Model = strings.TrimSpace(model)
				list[i].Title = strings.TrimSpace(title)
				list[i].Prompt = strings.TrimSpace(prompt)
				if list[i].Status != StatusArmed || list[i].DueAt == "" {
					list[i].Status = StatusArmed
					list[i].DueAt = nextDue.UTC().Format(time.RFC3339)
				}
				stored = list[i]
				updated = true
				break
			}
		}
		if !updated {
			stored = Timer{
				ID:        timerID,
				Category:  TimerSchedule,
				Status:    StatusArmed,
				DueAt:     nextDue.UTC().Format(time.RFC3339),
				CronExpr:  strings.TrimSpace(cronExpr),
				ScopeKey:  scopeKey,
				Skill:     strings.TrimSpace(skill),
				Model:     strings.TrimSpace(model),
				Title:     strings.TrimSpace(title),
				Prompt:    strings.TrimSpace(prompt),
				CreatedAt: nowStr,
			}
			list = append(list, stored)
		}
		return saveTimersUnlocked(vaultDir, list)
	})
	return stored, err
}

func pruneOldCompletedTimers(list []Timer) []Timer {
	if len(list) <= 200 {
		return list
	}
	var armed []Timer
	var inactive []Timer
	for _, t := range list {
		if t.Status == StatusArmed {
			armed = append(armed, t)
		} else {
			inactive = append(inactive, t)
		}
	}
	if len(inactive) > 100 {
		inactive = inactive[len(inactive)-100:]
	}
	return append(armed, inactive...)
}

// List returns timers sorted by DueAt ascending. By default only armed timers are returned unless includeAll is true.
func List(vaultDir string, includeAll bool) ([]Timer, error) {
	var out []Timer
	err := withTimersLock(vaultDir, func() error {
		list, err := loadTimersUnlocked(vaultDir)
		if err != nil {
			return err
		}
		for _, t := range list {
			if includeAll || t.Status == StatusArmed {
				out = append(out, t)
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].DueAt < out[j].DueAt
	})
	return out, err
}

// Cancel marks an armed timer as StatusCancelled.
func Cancel(vaultDir, id string, now time.Time) (Timer, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Timer{}, fmt.Errorf("timer id is required")
	}
	var cancelled Timer
	err := withTimersLock(vaultDir, func() error {
		list, err := loadTimersUnlocked(vaultDir)
		if err != nil {
			return err
		}
		for i := range list {
			if list[i].ID == id {
				list[i].Status = StatusCancelled
				cancelled = list[i]
				return saveTimersUnlocked(vaultDir, list)
			}
		}
		return fmt.Errorf("timer %q not found", id)
	})
	return cancelled, err
}

// EvaluateDue scans armed timers due at or before now, enqueues corresponding events into internal/events,
// marks follow_up timers done, and advances schedule timers to their next occurrence.
// Never spawns an agent run directly.
func EvaluateDue(vaultDir string, now time.Time) ([]events.Event, error) {
	if now.IsZero() {
		now = time.Now()
	}
	var enqueued []events.Event
	err := withTimersLock(vaultDir, func() error {
		list, err := loadTimersUnlocked(vaultDir)
		if err != nil || len(list) == 0 {
			return err
		}
		dirty := false
		nowStr := now.UTC().Format(time.RFC3339)
		for i := range list {
			t := &list[i]
			if t.Status != StatusArmed || t.DueAt == "" {
				continue
			}
			due, pErr := parseTimerDue(t.DueAt)
			if pErr != nil || now.Before(due) {
				continue
			}

			dedupKey := fmt.Sprintf("timer:%s:%s", t.ID, t.DueAt)
			evPrompt := t.Prompt
			if t.SessionID != "" {
				evPrompt = fmt.Sprintf("[Scheduled Follow-Up Timer Fired (%s · due %s)]\n\n%s", t.ID, t.DueAt, t.Prompt)
			}
			ev, deduped, enqErr := events.Enqueue(vaultDir, events.Event{
				Source:    "timer",
				DedupKey:  dedupKey,
				ScopeKey:  t.ScopeKey,
				SessionID: t.SessionID,
				Skill:     t.Skill,
				Model:     t.Model,
				Title:     t.Title,
				Prompt:    evPrompt,
				Metadata: map[string]string{
					"timer_id":       t.ID,
					"timer_category": string(t.Category),
				},
			}, now)
			if enqErr != nil {
				continue
			}
			if !deduped {
				enqueued = append(enqueued, ev)
			}

			t.FiredAt = nowStr
			if t.Category == TimerSchedule && strings.TrimSpace(t.CronExpr) != "" {
				base := now
				if due.After(base) {
					base = due
				}
				if next, cErr := NextCronOccurrence(t.CronExpr, base); cErr == nil {
					t.DueAt = next.UTC().Format(time.RFC3339)
				} else {
					t.Status = StatusDone
				}
			} else {
				t.Status = StatusDone
			}
			dirty = true
		}
		if !dirty {
			return nil
		}
		return saveTimersUnlocked(vaultDir, list)
	})
	return enqueued, err
}

// NextCronOccurrence computes the next wall-clock minute strictly after afterTime matching cronExpr.
// Supports optional leading CRON_TZ=<IANA> prefix plus standard 5-field cron syntax.
func NextCronOccurrence(cronExpr string, afterTime time.Time) (time.Time, error) {
	loc := afterTime.Location()
	fields := strings.Fields(strings.TrimSpace(cronExpr))
	if len(fields) == 6 && strings.HasPrefix(fields[0], "CRON_TZ=") {
		tz := strings.TrimPrefix(fields[0], "CRON_TZ=")
		l, err := time.LoadLocation(tz)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid CRON_TZ %q: %w", tz, err)
		}
		loc = l
		fields = fields[1:]
	}
	if len(fields) != 5 {
		return time.Time{}, fmt.Errorf("expected 5-field cron expression, got %q", cronExpr)
	}

	cand := afterTime.In(loc).Truncate(time.Minute).Add(time.Minute)
	// Search up to 366 days ahead (527,040 minutes).
	for step := 0; step < 366*24*60; step++ {
		if matchField(fields[0], cand.Minute(), 0, 59) &&
			matchField(fields[1], cand.Hour(), 0, 23) &&
			matchField(fields[2], cand.Day(), 1, 31) &&
			matchField(fields[3], int(cand.Month()), 1, 12) &&
			matchDOW(fields[4], int(cand.Weekday())) {
			return cand.UTC(), nil
		}
		cand = cand.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("no matching cron occurrence within 366d for %q", cronExpr)
}

func matchDOW(expr string, weekday int) bool {
	if matchField(expr, weekday, 0, 7) {
		return true
	}
	if weekday == 0 && matchField(expr, 7, 0, 7) {
		return true
	}
	return false
}

func matchField(expr string, val, minV, maxV int) bool {
	expr = strings.TrimSpace(expr)
	if expr == "*" {
		return true
	}
	for _, part := range strings.Split(expr, ",") {
		part = strings.TrimSpace(part)
		step := 1
		if slash := strings.Index(part, "/"); slash >= 0 {
			s, err := strconv.Atoi(part[slash+1:])
			if err != nil || s <= 0 {
				continue
			}
			step = s
			part = part[:slash]
		}
		low, high := minV, maxV
		if part != "*" {
			if dash := strings.Index(part, "-"); dash >= 0 {
				l, err1 := strconv.Atoi(part[:dash])
				h, err2 := strconv.Atoi(part[dash+1:])
				if err1 != nil || err2 != nil {
					continue
				}
				low, high = l, h
			} else {
				v, err := strconv.Atoi(part)
				if err != nil {
					continue
				}
				if step == 1 {
					if val == v {
						return true
					}
					continue
				}
				low = v
			}
		}
		if val >= low && val <= high && (val-low)%step == 0 {
			return true
		}
	}
	return false
}
