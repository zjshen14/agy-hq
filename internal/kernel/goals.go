package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/connector"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/store"
)

// Milestone represents a trackable sub-deliverable within a multi-day Goal.
type Milestone struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Done   bool   `json:"done"`
	DoneAt string `json:"doneAt,omitempty"`
}

// WaitingOn tracks an outbound dependency or counterparty obligation with a follow-up SLA.
type WaitingOn struct {
	ID            string `json:"id"`
	Counterparty  string `json:"counterparty"`
	Channel       string `json:"channel"`                 // legacy alias: gmail | gchat | external
	Connector     string `json:"connector,omitempty"`     // canonical connector ID
	Account       string `json:"account,omitempty"`       // connector account alias
	ThreadKey     string `json:"threadKey,omitempty"`     // canonical thread identifier
	ThreadOrSpace string `json:"threadOrSpace,omitempty"` // legacy alias for ThreadKey
	LastItemID    string `json:"lastItemId,omitempty"`    // thread head at send/wait time
	Priority      bool   `json:"priority,omitempty"`
	Summary       string `json:"summary"`
	SentAt        string `json:"sentAt"`
	FollowUpAfter string `json:"followUpAfter"` // RFC3339 deadline
	Status        string `json:"status"`        // waiting | replied | overdue | nudge_queued
	RepliedAt     string `json:"repliedAt,omitempty"`
	OutboxItemID  string `json:"outboxItemId,omitempty"`
}

// WaitingOnOptions configures rich connector/thread tracking on `AddWaitingOnWithOptions`.
type WaitingOnOptions struct {
	GoalSlug      string
	Counterparty  string
	Channel       string
	Connector     string
	Account       string
	ThreadKey     string
	ThreadOrSpace string
	LastItemID    string
	Priority      bool
	Summary       string
	SLA           time.Duration
}

// Goal represents a stateful multi-day objective tracked in local/memory/goals.json (Active Focus).
type Goal struct {
	Slug                string          `json:"slug"`
	Title               string          `json:"title"`
	Scope               string          `json:"scope"`
	Status              string          `json:"status"`                        // active | paused | completed
	TargetDate          string          `json:"targetDate,omitempty"`          // YYYY-MM-DD
	WindowStartAt       string          `json:"windowStartAt,omitempty"`       // RFC3339 or YYYY-MM-DD
	WindowEndAt         string          `json:"windowEndAt,omitempty"`         // RFC3339 or YYYY-MM-DD
	Timezone            string          `json:"timezone,omitempty"`            // IANA timezone
	BlocksAvailability  bool            `json:"blocksAvailability,omitempty"`  // unavailable/freeze window
	RequiredCoverageTag string          `json:"requiredCoverageTag,omitempty"` // daily coverage tag invariant
	ParkedUntil         string          `json:"parkedUntil,omitempty"`         // YYYY-MM-DD, RFC3339, or "after:<goal-slug>"
	SpaceSlug           string          `json:"spaceSlug,omitempty"`           // linked Living Space slug
	Milestones          []Milestone     `json:"milestones,omitempty"`
	Checklist           []ChecklistItem `json:"checklist,omitempty"`
	WaitingOn           []WaitingOn     `json:"waitingOn,omitempty"`
	CreatedAt           string          `json:"createdAt"`
	UpdatedAt           string          `json:"updatedAt"`
	CompletedAt         string          `json:"completedAt,omitempty"`
}

func goalsFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "goals.json")
}

func goalsLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", ".goals.lock")
}

func loadGoalsUnlocked(vaultDir string) ([]Goal, error) {
	data, err := os.ReadFile(goalsFilePath(vaultDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var goals []Goal
	if err := json.Unmarshal(data, &goals); err != nil {
		return nil, err
	}
	return goals, nil
}

func saveGoalsUnlocked(vaultDir string, goals []Goal) error {
	p := goalsFilePath(vaultDir)
	data, err := json.MarshalIndent(goals, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(p, append(data, '\n'), 0o600)
}

func withGoalsLock(vaultDir string, fn func() error) error {
	return store.With(context.Background(), store.RankGoals, goalsLockPath(vaultDir), func(_ context.Context) error {
		return fn()
	})
}

// ListGoals reads all tracked Goals from local/memory/goals.json under RankGoals.
func ListGoals(vaultDir string) ([]Goal, error) {
	var out []Goal
	err := withGoalsLock(vaultDir, func() error {
		var lErr error
		out, lErr = loadGoalsUnlocked(vaultDir)
		return lErr
	})
	return out, err
}

// AddGoal creates or updates an active multi-day Goal in local/memory/goals.json.
func AddGoal(vaultDir, slug, title, scope, targetDate string, now time.Time) (Goal, error) {
	if now.IsZero() {
		now = time.Now()
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return Goal{}, fmt.Errorf("goal --title is required")
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		slug = slugify(title)
	}
	scope = strings.TrimSpace(scope)
	if scope == "" {
		if parts := strings.SplitN(slug, "-", 2); len(parts) > 0 && parts[0] != "" {
			scope = parts[0]
		} else {
			scope = "general"
		}
	}
	targetDate = strings.TrimSpace(targetDate)
	nowStr := now.UTC().Format(time.RFC3339)

	var out Goal
	err := withGoalsLock(vaultDir, func() error {
		goals, lErr := loadGoalsUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		for i := range goals {
			if goals[i].Slug == slug {
				goals[i].Title = title
				goals[i].Scope = scope
				if targetDate != "" {
					goals[i].TargetDate = targetDate
				}
				if goals[i].Status == "" {
					goals[i].Status = "active"
				}
				goals[i].UpdatedAt = nowStr
				out = goals[i]
				return saveGoalsUnlocked(vaultDir, goals)
			}
		}
		g := Goal{
			Slug:       slug,
			Title:      title,
			Scope:      scope,
			Status:     "active",
			TargetDate: targetDate,
			CreatedAt:  nowStr,
			UpdatedAt:  nowStr,
		}
		goals = append(goals, g)
		out = g
		return saveGoalsUnlocked(vaultDir, goals)
	})
	return out, err
}

// AddMilestone appends a milestone to an existing Goal.
func AddMilestone(vaultDir, goalSlug, title string, now time.Time) (Goal, Milestone, error) {
	if now.IsZero() {
		now = time.Now()
	}
	goalSlug = strings.TrimSpace(goalSlug)
	title = strings.TrimSpace(title)
	if goalSlug == "" || title == "" {
		return Goal{}, Milestone{}, fmt.Errorf("both --goal and --title are required")
	}
	var outG Goal
	var outM Milestone
	err := withGoalsLock(vaultDir, func() error {
		goals, lErr := loadGoalsUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		for i := range goals {
			if goals[i].Slug == goalSlug {
				ms := Milestone{
					ID:    fmt.Sprintf("ms-%d", len(goals[i].Milestones)+1),
					Title: title,
				}
				goals[i].Milestones = append(goals[i].Milestones, ms)
				goals[i].UpdatedAt = now.UTC().Format(time.RFC3339)
				outG = goals[i]
				outM = ms
				return saveGoalsUnlocked(vaultDir, goals)
			}
		}
		return fmt.Errorf("goal %q not found", goalSlug)
	})
	return outG, outM, err
}

// CheckMilestone marks a milestone (matched by ID or case-insensitive title substring) as completed.
func CheckMilestone(vaultDir, goalSlug, target string, now time.Time) (Goal, error) {
	if now.IsZero() {
		now = time.Now()
	}
	goalSlug = strings.TrimSpace(goalSlug)
	target = strings.TrimSpace(target)
	nowStr := now.UTC().Format(time.RFC3339)
	var out Goal
	err := withGoalsLock(vaultDir, func() error {
		goals, lErr := loadGoalsUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		for i := range goals {
			if goals[i].Slug != goalSlug {
				continue
			}
			for j := range goals[i].Milestones {
				ms := &goals[i].Milestones[j]
				if ms.ID == target || strings.EqualFold(ms.Title, target) || (target != "" && strings.Contains(strings.ToLower(ms.Title), strings.ToLower(target))) {
					ms.Done = true
					ms.DoneAt = nowStr
					goals[i].UpdatedAt = nowStr
					out = goals[i]
					return saveGoalsUnlocked(vaultDir, goals)
				}
			}
			return fmt.Errorf("milestone %q not found in goal %q", target, goalSlug)
		}
		return fmt.Errorf("goal %q not found", goalSlug)
	})
	return out, err
}

// AddWaitingOnWithOptions registers a connector-aware counterparty follow-up obligation on a Goal.
func AddWaitingOnWithOptions(vaultDir string, opts WaitingOnOptions, now time.Time) (Goal, WaitingOn, error) {
	if now.IsZero() {
		now = time.Now()
	}
	goalSlug := strings.TrimSpace(opts.GoalSlug)
	counterparty := strings.TrimSpace(opts.Counterparty)
	summary := strings.TrimSpace(opts.Summary)
	if goalSlug == "" || counterparty == "" || summary == "" {
		return Goal{}, WaitingOn{}, fmt.Errorf("--goal, --counterparty, and --summary are required")
	}
	sla := opts.SLA
	if sla <= 0 {
		sla = 48 * time.Hour
	}
	connID := strings.ToLower(strings.TrimSpace(opts.Connector))
	channel := strings.ToLower(strings.TrimSpace(opts.Channel))
	if connID == "" && channel != "" && channel != "external" {
		connID = channel
	}
	if channel == "" {
		if connID != "" {
			channel = connID
		} else {
			channel = "gmail"
		}
	}
	tk := strings.TrimSpace(opts.ThreadKey)
	tos := strings.TrimSpace(opts.ThreadOrSpace)
	if tk == "" {
		tk = tos
	}
	if tos == "" {
		tos = tk
	}

	nowStr := now.UTC().Format(time.RFC3339)
	var outG Goal
	var outW WaitingOn
	err := withGoalsLock(vaultDir, func() error {
		goals, lErr := loadGoalsUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		for i := range goals {
			if goals[i].Slug == goalSlug {
				w := WaitingOn{
					ID:            fmt.Sprintf("wait-%d", len(goals[i].WaitingOn)+1),
					Counterparty:  counterparty,
					Channel:       channel,
					Connector:     connID,
					Account:       strings.TrimSpace(opts.Account),
					ThreadKey:     tk,
					ThreadOrSpace: tos,
					LastItemID:    strings.TrimSpace(opts.LastItemID),
					Priority:      opts.Priority,
					Summary:       summary,
					SentAt:        nowStr,
					FollowUpAfter: now.Add(sla).UTC().Format(time.RFC3339),
					Status:        "waiting",
				}
				goals[i].WaitingOn = append(goals[i].WaitingOn, w)
				goals[i].UpdatedAt = nowStr
				outG = goals[i]
				outW = w
				return saveGoalsUnlocked(vaultDir, goals)
			}
		}
		return fmt.Errorf("goal %q not found", goalSlug)
	})
	return outG, outW, err
}

// MarkWaitingReplied resolves a WaitingOn obligation when the counterparty replies.
func MarkWaitingReplied(vaultDir, goalSlug, target string, now time.Time) (Goal, error) {
	if now.IsZero() {
		now = time.Now()
	}
	goalSlug = strings.TrimSpace(goalSlug)
	target = strings.TrimSpace(target)
	nowStr := now.UTC().Format(time.RFC3339)
	var out Goal
	err := withGoalsLock(vaultDir, func() error {
		goals, lErr := loadGoalsUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		for i := range goals {
			if goalSlug != "" && goals[i].Slug != goalSlug {
				continue
			}
			for j := range goals[i].WaitingOn {
				w := &goals[i].WaitingOn[j]
				if w.ID == target || w.ThreadKey == target || w.ThreadOrSpace == target ||
					strings.EqualFold(w.Counterparty, target) ||
					(target != "" && strings.Contains(strings.ToLower(w.Counterparty), strings.ToLower(target))) {
					w.Status = "replied"
					w.RepliedAt = nowStr
					goals[i].UpdatedAt = nowStr
					out = goals[i]
					return saveGoalsUnlocked(vaultDir, goals)
				}
			}
		}
		return fmt.Errorf("waiting-on item %q not found", target)
	})
	return out, err
}

// CompleteGoal marks an active Goal as completed.
func CompleteGoal(vaultDir, goalSlug string, now time.Time) (Goal, error) {
	if now.IsZero() {
		now = time.Now()
	}
	goalSlug = strings.TrimSpace(goalSlug)
	nowStr := now.UTC().Format(time.RFC3339)
	var out Goal
	err := withGoalsLock(vaultDir, func() error {
		goals, lErr := loadGoalsUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		for i := range goals {
			if goals[i].Slug == goalSlug {
				goals[i].Status = "completed"
				goals[i].UpdatedAt = nowStr
				goals[i].CompletedAt = nowStr
				out = goals[i]
				return saveGoalsUnlocked(vaultDir, goals)
			}
		}
		return fmt.Errorf("goal %q not found", goalSlug)
	})
	return out, err
}

func parseGoalTargetTime(targetDate string) (time.Time, bool) {
	s := strings.TrimSpace(targetDate)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		// End of target day UTC
		return t.Add(23*time.Hour + 59*time.Minute), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// OverdueWaitingSummary pairs an overdue/due WaitingOn entry with its parent Goal metadata.
type OverdueWaitingSummary struct {
	GoalSlug  string    `json:"goalSlug"`
	GoalTitle string    `json:"goalTitle"`
	Scope     string    `json:"scope"`
	Waiting   WaitingOn `json:"waiting"`
}

// DueGoals returns active Goals approaching their TargetDate within `within` and all WaitingOn
// obligations that are overdue or expiring within `within`.
func DueGoals(vaultDir string, within time.Duration, now time.Time) ([]Goal, []OverdueWaitingSummary, error) {
	if now.IsZero() {
		now = time.Now()
	}
	if within <= 0 {
		within = 72 * time.Hour
	}
	goals, err := ListGoals(vaultDir)
	if err != nil {
		return nil, nil, err
	}
	horizon := now.Add(within)
	var dueGoals []Goal
	var dueWaits []OverdueWaitingSummary

	for _, g := range goals {
		if g.Status != "active" {
			continue
		}
		goalAdded := false
		if targetTime, ok := parseGoalTargetTime(g.TargetDate); ok && !targetTime.After(horizon) {
			dueGoals = append(dueGoals, g)
			goalAdded = true
		}
		for _, w := range g.WaitingOn {
			if w.Status == "replied" {
				continue
			}
			if w.FollowUpAfter != "" {
				if ft, err := time.Parse(time.RFC3339, w.FollowUpAfter); err == nil && !ft.After(horizon) {
					dueWaits = append(dueWaits, OverdueWaitingSummary{
						GoalSlug:  g.Slug,
						GoalTitle: g.Title,
						Scope:     g.Scope,
						Waiting:   w,
					})
					if !goalAdded {
						dueGoals = append(dueGoals, g)
						goalAdded = true
					}
				}
			}
		}
	}
	sort.SliceStable(dueGoals, func(i, j int) bool {
		return dueGoals[i].TargetDate < dueGoals[j].TargetDate
	})
	return dueGoals, dueWaits, nil
}

// SweepWaitingOnObligations runs in 0 LLM tokens inside `hq heartbeat`:
//  1. Inspects active thread-bound `WaitingOn` items via `connector.InspectThread` in the bounded worker pool
//     (without holding RankGoals) to auto-resolve `"waiting"` -> `"replied"` when `CounterpartyReplied` is true.
//  2. Detects overdue counterparty SLAs, transitioning `"waiting"` -> `"nudge_queued"` and staging a structured
//     follow-up prompt in local/memory/outbox/items.jsonl.
func SweepWaitingOnObligations(vaultDir string, now time.Time) ([]string, error) {
	if now.IsZero() {
		now = time.Now()
	}
	snapshot, err := ListGoals(vaultDir)
	if err != nil || len(snapshot) == 0 {
		return nil, err
	}

	// Phase 1: Parallel `InspectThread` checks outside RankGoals
	type inspectJob struct {
		goalSlug  string
		waitID    string
		connID    string
		account   string
		threadKey string
		afterItem string
	}
	var jobs []inspectJob
	for _, g := range snapshot {
		if g.Status != "active" || g.IsParked(snapshot, now) {
			continue
		}
		for _, w := range g.WaitingOn {
			if w.Status != "waiting" && w.Status != "nudge_queued" {
				continue
			}
			connID := strings.TrimSpace(w.Connector)
			if connID == "" && w.Channel != "" && w.Channel != "external" {
				connID = strings.TrimSpace(w.Channel)
			}
			tk := strings.TrimSpace(w.ThreadKey)
			if tk == "" {
				tk = strings.TrimSpace(w.ThreadOrSpace)
			}
			if connID == "" || tk == "" {
				continue
			}
			rc, rErr := connector.Resolve(vaultDir, connID)
			if rErr != nil || rc.Manifest.Capabilities.InspectThread == nil || !rc.Manifest.Capabilities.InspectThread.Enabled {
				continue
			}
			jobs = append(jobs, inspectJob{
				goalSlug:  g.Slug,
				waitID:    w.ID,
				connID:    rc.FQID,
				account:   w.Account,
				threadKey: tk,
				afterItem: w.LastItemID,
			})
		}
	}

	repliedMap := make(map[string]string) // goalSlug+"/"+waitID -> headItemID
	if len(jobs) > 0 {
		allowDesktop := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CREDENTIAL_SCOPE")) != ""
		type inspOut struct {
			insp connector.ThreadInspection
			err  error
		}
		tasks := make([]func(context.Context) inspOut, len(jobs))
		for idx, jb := range jobs {
			jb := jb
			tasks[idx] = func(ctx context.Context) inspOut {
				res, iErr := connector.InspectThread(ctx, vaultDir, jb.connID, jb.account, jb.threadKey, jb.afterItem, allowDesktop)
				return inspOut{insp: res, err: iErr}
			}
		}
		results := connector.RunPool(context.Background(), connector.DefaultMaxWorkers, tasks)
		for idx, jb := range jobs {
			if results[idx].err == nil && results[idx].insp.CounterpartyReplied {
				repliedMap[jb.goalSlug+"/"+jb.waitID] = results[idx].insp.HeadItemID
			}
		}
	}

	// Phase 2: Stage overdue outbox items & update goals.json
	type queuedNudge struct {
		goalSlug string
		waitID   string
		item     outbox.OutboxItem
	}
	var nudges []queuedNudge
	var actions []string
	nowStr := now.UTC().Format(time.RFC3339)

	err = withGoalsLock(vaultDir, func() error {
		goals, lErr := loadGoalsUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		changed := false
		for i := range goals {
			if goals[i].Status != "active" || goals[i].IsParked(goals, now) {
				continue
			}
			urgentGoal := false
			if targetTime, ok := parseGoalTargetTime(goals[i].TargetDate); ok && targetTime.Sub(now) <= 24*time.Hour {
				urgentGoal = true
			}
			for j := range goals[i].WaitingOn {
				w := &goals[i].WaitingOn[j]
				if headID, autoReplied := repliedMap[goals[i].Slug+"/"+w.ID]; autoReplied && w.Status != "replied" {
					w.Status = "replied"
					w.RepliedAt = nowStr
					if headID != "" {
						w.LastItemID = headID
					}
					goals[i].UpdatedAt = nowStr
					changed = true
					actions = append(actions, fmt.Sprintf("goal:%s/wait:%s (%s auto-marked replied via inspect_thread)", goals[i].Slug, w.ID, w.Counterparty))
					continue
				}

				if w.Status != "waiting" || strings.TrimSpace(w.FollowUpAfter) == "" {
					continue
				}
				deadline, pErr := time.Parse(time.RFC3339, w.FollowUpAfter)
				if pErr != nil || now.Before(deadline) {
					continue
				}

				tier := outbox.TierBriefRollup
				if urgentGoal || w.Priority {
					tier = outbox.TierInterrupt
				}
				connLabel := w.Connector
				if connLabel == "" {
					connLabel = w.Channel
				}
				tk := w.ThreadKey
				if tk == "" {
					tk = w.ThreadOrSpace
				}
				draftHint := "Stage a polite follow-up via `hq offer stage` or `hq propose`"
				if connLabel != "" && connLabel != "external" && tk != "" {
					draftHint = fmt.Sprintf("Stage a polite follow-up via `hq offer stage --connector=%s --thread=%s`", connLabel, tk)
				}
				nudges = append(nudges, queuedNudge{
					goalSlug: goals[i].Slug,
					waitID:   w.ID,
					item: outbox.OutboxItem{
						SourceSkill:   "morning-brief",
						Scope:         goals[i].Scope,
						Tier:          tier,
						Connector:     connLabel,
						Account:       w.Account,
						ThreadKey:     tk,
						Title:         fmt.Sprintf("Follow-up SLA overdue: %s (%s)", w.Counterparty, goals[i].Title),
						Summary:       fmt.Sprintf("Waiting on %s for %q via %s (SLA expired %s). %s.", w.Counterparty, w.Summary, connLabel, w.FollowUpAfter, draftHint),
						SuggestedSlug: goals[i].Slug,
					},
				})
				w.Status = "nudge_queued"
				goals[i].UpdatedAt = nowStr
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return saveGoalsUnlocked(vaultDir, goals)
	})
	if err != nil {
		return actions, err
	}

	// Phase 3: Write nudges to Outbox (RankOutbox = 60, outside RankGoals) and record OutboxItemIDs
	if len(nudges) > 0 {
		outboxIDs := make(map[string]string, len(nudges))
		for _, n := range nudges {
			item, _, addErr := outbox.Add(vaultDir, n.item, now)
			if addErr == nil && item.ID != "" {
				outboxIDs[n.goalSlug+"/"+n.waitID] = item.ID
			}
			actions = append(actions, fmt.Sprintf("goal:%s/wait:%s (%s overdue -> outbox:%s [%s])", n.goalSlug, n.waitID, n.item.Title, item.ID, n.item.Tier))
		}
		if len(outboxIDs) > 0 {
			_ = withGoalsLock(vaultDir, func() error {
				goals, lErr := loadGoalsUnlocked(vaultDir)
				if lErr != nil {
					return lErr
				}
				updated := false
				for i := range goals {
					for j := range goals[i].WaitingOn {
						if oid, ok := outboxIDs[goals[i].Slug+"/"+goals[i].WaitingOn[j].ID]; ok {
							goals[i].WaitingOn[j].OutboxItemID = oid
							updated = true
						}
					}
				}
				if !updated {
					return nil
				}
				return saveGoalsUnlocked(vaultDir, goals)
			})
		}
	}
	return actions, nil
}
