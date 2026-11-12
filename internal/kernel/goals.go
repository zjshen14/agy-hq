package kernel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/outbox"
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
	Channel       string `json:"channel"` // gmail | gchat | external
	ThreadOrSpace string `json:"threadOrSpace,omitempty"`
	Summary       string `json:"summary"`
	SentAt        string `json:"sentAt"`
	FollowUpAfter string `json:"followUpAfter"` // RFC3339 deadline
	Status        string `json:"status"`        // waiting | replied | overdue | nudge_queued
	RepliedAt     string `json:"repliedAt,omitempty"`
	OutboxItemID  string `json:"outboxItemId,omitempty"`
}

// Goal represents a stateful multi-day objective tracked in local/memory/goals.json (Active Focus).
type Goal struct {
	Slug        string      `json:"slug"`
	Title       string      `json:"title"`
	Scope       string      `json:"scope"`
	Status      string      `json:"status"`               // active | paused | completed
	TargetDate  string      `json:"targetDate,omitempty"` // YYYY-MM-DD
	Milestones  []Milestone `json:"milestones,omitempty"`
	WaitingOn   []WaitingOn `json:"waitingOn,omitempty"`
	CreatedAt   string      `json:"createdAt"`
	UpdatedAt   string      `json:"updatedAt"`
	CompletedAt string      `json:"completedAt,omitempty"`
}

func goalsFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "goals.json")
}

// ListGoals reads all tracked Goals from local/memory/goals.json.
func ListGoals(vaultDir string) ([]Goal, error) {
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

// SaveGoals persists goals atomically to local/memory/goals.json (0600 permissions).
func SaveGoals(vaultDir string, goals []Goal) error {
	p := goalsFilePath(vaultDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(goals, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
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

	goals, err := ListGoals(vaultDir)
	if err != nil {
		return Goal{}, err
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
			return goals[i], SaveGoals(vaultDir, goals)
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
	return g, SaveGoals(vaultDir, goals)
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
	goals, err := ListGoals(vaultDir)
	if err != nil {
		return Goal{}, Milestone{}, err
	}
	for i := range goals {
		if goals[i].Slug == goalSlug {
			ms := Milestone{
				ID:    fmt.Sprintf("ms-%d", len(goals[i].Milestones)+1),
				Title: title,
			}
			goals[i].Milestones = append(goals[i].Milestones, ms)
			goals[i].UpdatedAt = now.UTC().Format(time.RFC3339)
			return goals[i], ms, SaveGoals(vaultDir, goals)
		}
	}
	return Goal{}, Milestone{}, fmt.Errorf("goal %q not found", goalSlug)
}

// CheckMilestone marks a milestone (matched by ID or case-insensitive title substring) as completed.
func CheckMilestone(vaultDir, goalSlug, target string, now time.Time) (Goal, error) {
	if now.IsZero() {
		now = time.Now()
	}
	goalSlug = strings.TrimSpace(goalSlug)
	target = strings.TrimSpace(target)
	goals, err := ListGoals(vaultDir)
	if err != nil {
		return Goal{}, err
	}
	nowStr := now.UTC().Format(time.RFC3339)
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
				return goals[i], SaveGoals(vaultDir, goals)
			}
		}
		return Goal{}, fmt.Errorf("milestone %q not found in goal %q", target, goalSlug)
	}
	return Goal{}, fmt.Errorf("goal %q not found", goalSlug)
}

// AddWaitingOn registers a counterparty follow-up obligation with a deterministic SLA timer on a Goal.
func AddWaitingOn(vaultDir, goalSlug, counterparty, channel, threadOrSpace, summary string, sla time.Duration, now time.Time) (Goal, WaitingOn, error) {
	if now.IsZero() {
		now = time.Now()
	}
	goalSlug = strings.TrimSpace(goalSlug)
	counterparty = strings.TrimSpace(counterparty)
	summary = strings.TrimSpace(summary)
	if goalSlug == "" || counterparty == "" || summary == "" {
		return Goal{}, WaitingOn{}, fmt.Errorf("--goal, --counterparty, and --summary are required")
	}
	if sla <= 0 {
		sla = 48 * time.Hour
	}
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		channel = "gmail"
	}
	goals, err := ListGoals(vaultDir)
	if err != nil {
		return Goal{}, WaitingOn{}, err
	}
	nowStr := now.UTC().Format(time.RFC3339)
	for i := range goals {
		if goals[i].Slug == goalSlug {
			w := WaitingOn{
				ID:            fmt.Sprintf("wait-%d", len(goals[i].WaitingOn)+1),
				Counterparty:  counterparty,
				Channel:       channel,
				ThreadOrSpace: strings.TrimSpace(threadOrSpace),
				Summary:       summary,
				SentAt:        nowStr,
				FollowUpAfter: now.Add(sla).UTC().Format(time.RFC3339),
				Status:        "waiting",
			}
			goals[i].WaitingOn = append(goals[i].WaitingOn, w)
			goals[i].UpdatedAt = nowStr
			return goals[i], w, SaveGoals(vaultDir, goals)
		}
	}
	return Goal{}, WaitingOn{}, fmt.Errorf("goal %q not found", goalSlug)
}

// MarkWaitingReplied resolves a WaitingOn obligation when the counterparty replies.
func MarkWaitingReplied(vaultDir, goalSlug, target string, now time.Time) (Goal, error) {
	if now.IsZero() {
		now = time.Now()
	}
	goalSlug = strings.TrimSpace(goalSlug)
	target = strings.TrimSpace(target)
	goals, err := ListGoals(vaultDir)
	if err != nil {
		return Goal{}, err
	}
	nowStr := now.UTC().Format(time.RFC3339)
	for i := range goals {
		if goalSlug != "" && goals[i].Slug != goalSlug {
			continue
		}
		for j := range goals[i].WaitingOn {
			w := &goals[i].WaitingOn[j]
			if w.ID == target || strings.EqualFold(w.Counterparty, target) || (target != "" && strings.Contains(strings.ToLower(w.Counterparty), strings.ToLower(target))) {
				w.Status = "replied"
				w.RepliedAt = nowStr
				goals[i].UpdatedAt = nowStr
				return goals[i], SaveGoals(vaultDir, goals)
			}
		}
	}
	return Goal{}, fmt.Errorf("waiting-on item %q not found", target)
}

// CompleteGoal marks an active Goal as completed.
func CompleteGoal(vaultDir, goalSlug string, now time.Time) (Goal, error) {
	if now.IsZero() {
		now = time.Now()
	}
	goalSlug = strings.TrimSpace(goalSlug)
	goals, err := ListGoals(vaultDir)
	if err != nil {
		return Goal{}, err
	}
	nowStr := now.UTC().Format(time.RFC3339)
	for i := range goals {
		if goals[i].Slug == goalSlug {
			goals[i].Status = "completed"
			goals[i].UpdatedAt = nowStr
			goals[i].CompletedAt = nowStr
			return goals[i], SaveGoals(vaultDir, goals)
		}
	}
	return Goal{}, fmt.Errorf("goal %q not found", goalSlug)
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

// SweepWaitingOnObligations runs in 0 LLM tokens inside `hq heartbeat` to detect overdue
// counterparty SLAs, transitioning `"waiting"` -> `"nudge_queued"` and staging a structured
// follow-up prompt in local/memory/outbox/items.jsonl.
func SweepWaitingOnObligations(vaultDir string, now time.Time) ([]string, error) {
	if now.IsZero() {
		now = time.Now()
	}
	goals, err := ListGoals(vaultDir)
	if err != nil || len(goals) == 0 {
		return nil, err
	}
	var actions []string
	changed := false

	for i := range goals {
		if goals[i].Status != "active" {
			continue
		}
		urgentGoal := false
		if targetTime, ok := parseGoalTargetTime(goals[i].TargetDate); ok && targetTime.Sub(now) <= 24*time.Hour {
			urgentGoal = true
		}
		for j := range goals[i].WaitingOn {
			w := &goals[i].WaitingOn[j]
			if w.Status != "waiting" || strings.TrimSpace(w.FollowUpAfter) == "" {
				continue
			}
			deadline, pErr := time.Parse(time.RFC3339, w.FollowUpAfter)
			if pErr != nil || now.Before(deadline) {
				continue
			}

			tier := outbox.TierBriefRollup
			if urgentGoal {
				tier = outbox.TierInterrupt
			}
			draftHint := fmt.Sprintf("Stage a polite follow-up via `./bin/hq propose`")
			if w.Channel == "gmail" || w.Channel == "gchat" {
				draftHint = fmt.Sprintf("Draft a polite follow-up via `./bin/hq %s draft` and stage `./bin/hq propose --type=%s_send`", w.Channel, w.Channel)
			}
			item, _, addErr := outbox.Add(vaultDir, outbox.OutboxItem{
				SourceSkill:   "morning-brief",
				Scope:         goals[i].Scope,
				Tier:          tier,
				Title:         fmt.Sprintf("Follow-up SLA overdue: %s (%s)", w.Counterparty, goals[i].Title),
				Summary:       fmt.Sprintf("Waiting on %s for %q via %s (SLA expired %s). %s.", w.Counterparty, w.Summary, w.Channel, w.FollowUpAfter, draftHint),
				SuggestedSlug: goals[i].Slug,
			}, now)
			w.Status = "nudge_queued"
			if addErr == nil {
				w.OutboxItemID = item.ID
			}
			goals[i].UpdatedAt = now.UTC().Format(time.RFC3339)
			changed = true
			actions = append(actions, fmt.Sprintf("goal:%s/wait:%s (%s overdue -> outbox:%s [%s])", goals[i].Slug, w.ID, w.Counterparty, w.OutboxItemID, item.Tier))
		}
	}

	if changed {
		if err := SaveGoals(vaultDir, goals); err != nil {
			return actions, err
		}
	}
	return actions, nil
}
