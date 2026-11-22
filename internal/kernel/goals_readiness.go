package kernel

import (
	"fmt"
	"strings"
	"time"
)

// ChecklistStatus values for 3-state Goal checklists (RFC-0027).
const (
	ChecklistOpen      = "open"
	ChecklistConfirmed = "confirmed"
	ChecklistSkipped   = "skipped"
)

// ChecklistItem tracks a 3-state actionable item inside a Goal (`open` | `confirmed` | `skipped`).
// `skipped` acts as a persistent tombstone so background checks never re-nag on dismissed items.
type ChecklistItem struct {
	ID                       string `json:"id"`
	Title                    string `json:"title"`
	Tag                      string `json:"tag,omitempty"`
	Status                   string `json:"status"` // open | confirmed | skipped
	WindowStartAt            string `json:"windowStartAt,omitempty"`
	WindowEndAt              string `json:"windowEndAt,omitempty"`
	ConstraintNote           string `json:"constraintNote,omitempty"`
	Group                    string `json:"group,omitempty"`
	ExternalRef              string `json:"externalRef,omitempty"`
	StatusNote               string `json:"statusNote,omitempty"`
	CutoffAt                 string `json:"cutoffAt,omitempty"`
	MustCompleteBeforeWindow bool   `json:"mustCompleteBeforeWindow,omitempty"`
	ActionURL                string `json:"actionUrl,omitempty"`
	SkipReason               string `json:"skipReason,omitempty"`
	UpdatedAt                string `json:"updatedAt,omitempty"`
}

// ReadinessProgress summarizes 3-state completion across a Goal's checklist (and fallback milestones).
type ReadinessProgress struct {
	Confirmed int `json:"confirmed"`
	Open      int `json:"open"`
	Skipped   int `json:"skipped"`
	Active    int `json:"active"` // Confirmed + Open (denominator excludes Skipped tombstones)
	Percent   int `json:"percent"`
}

// AddGoalOptions configures extended RFC-0027 Goal fields on AddGoalWithOptions.
type AddGoalOptions struct {
	Slug                string
	Title               string
	Scope               string
	TargetDate          string
	WindowStartAt       string
	WindowEndAt         string
	Timezone            string
	BlocksAvailability  bool
	RequiredCoverageTag string
	ParkedUntil         string
	SpaceSlug           string
}

// ChecklistItemOptions configures a new ChecklistItem on AddChecklistItem.
type ChecklistItemOptions struct {
	ID                       string
	Title                    string
	Tag                      string
	Status                   string
	WindowStartAt            string
	WindowEndAt              string
	ConstraintNote           string
	Group                    string
	ExternalRef              string
	StatusNote               string
	CutoffAt                 string
	MustCompleteBeforeWindow bool
	ActionURL                string
}

// ParseFlexibleTime parses RFC3339 or YYYY-MM-DD timestamps (in the provided location or UTC).
func ParseFlexibleTime(raw, tz string) (time.Time, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	loc := time.UTC
	if strings.TrimSpace(tz) != "" {
		if l, err := time.LoadLocation(strings.TrimSpace(tz)); err == nil {
			loc = l
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", s, loc); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// ParseFlexibleEndTime parses RFC3339 or YYYY-MM-DD (treating date-only as end-of-day 23:59:59).
func ParseFlexibleEndTime(raw, tz string) (time.Time, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	loc := time.UTC
	if strings.TrimSpace(tz) != "" {
		if l, err := time.LoadLocation(strings.TrimSpace(tz)); err == nil {
			loc = l
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t.Add(23*time.Hour + 59*time.Minute + 59*time.Second), true
	}
	return ParseFlexibleTime(raw, tz)
}

// Readiness computes 3-state readiness (`confirmed` / `open` / `skipped`) for a Goal.
// When Checklist is empty, it falls back to Milestones for backward compatibility.
func (g Goal) Readiness() ReadinessProgress {
	var p ReadinessProgress
	if len(g.Checklist) > 0 {
		for _, item := range g.Checklist {
			switch strings.ToLower(strings.TrimSpace(item.Status)) {
			case ChecklistConfirmed:
				p.Confirmed++
			case ChecklistSkipped:
				p.Skipped++
			default:
				p.Open++
			}
		}
	} else {
		for _, ms := range g.Milestones {
			if ms.Done {
				p.Confirmed++
			} else {
				p.Open++
			}
		}
	}
	p.Active = p.Confirmed + p.Open
	if p.Active > 0 {
		p.Percent = (p.Confirmed * 100) / p.Active
	}
	return p
}

// ResolveParkWakeTime resolves a Goal's ParkedUntil expression (`YYYY-MM-DD`, `RFC3339`,
// or `"after:<goal-slug>"`) into a concrete wake time. Returns (zero, false) if not parked.
func ResolveParkWakeTime(parkedUntil string, allGoals []Goal) (time.Time, bool) {
	raw := strings.TrimSpace(parkedUntil)
	if raw == "" {
		return time.Time{}, false
	}
	if strings.HasPrefix(strings.ToLower(raw), "after:") {
		targetSlug := strings.TrimSpace(raw[len("after:"):])
		for _, og := range allGoals {
			if strings.EqualFold(og.Slug, targetSlug) {
				if og.Status == "completed" {
					if t, ok := ParseFlexibleTime(og.CompletedAt, og.Timezone); ok {
						return t, true
					}
					return time.Time{}, false
				}
				if t, ok := ParseFlexibleEndTime(og.WindowEndAt, og.Timezone); ok {
					return t, true
				}
				if t, ok := ParseFlexibleEndTime(og.TargetDate, og.Timezone); ok {
					return t, true
				}
				return time.Now().Add(365 * 24 * time.Hour), true
			}
		}
		return time.Time{}, false
	}
	if t, ok := ParseFlexibleTime(raw, ""); ok {
		return t, true
	}
	return time.Time{}, false
}

// IsParked reports whether the Goal is currently parked relative to `now`.
func (g Goal) IsParked(allGoals []Goal, now time.Time) bool {
	if strings.TrimSpace(g.ParkedUntil) == "" {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	wake, ok := ResolveParkWakeTime(g.ParkedUntil, allGoals)
	if !ok {
		return false
	}
	return now.Before(wake)
}

// AddGoalWithOptions creates or updates a Goal with extended RFC-0027 fields.
func AddGoalWithOptions(vaultDir string, opts AddGoalOptions, now time.Time) (Goal, error) {
	if now.IsZero() {
		now = time.Now()
	}
	title := strings.TrimSpace(opts.Title)
	if title == "" {
		return Goal{}, fmt.Errorf("goal --title is required")
	}
	slug := strings.TrimSpace(opts.Slug)
	if slug == "" {
		slug = slugify(title)
	}
	scope := strings.TrimSpace(opts.Scope)
	if scope == "" {
		if parts := strings.SplitN(slug, "-", 2); len(parts) > 0 && parts[0] != "" {
			scope = parts[0]
		} else {
			scope = "general"
		}
	}
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
				if s := strings.TrimSpace(opts.TargetDate); s != "" {
					goals[i].TargetDate = s
				}
				if s := strings.TrimSpace(opts.WindowStartAt); s != "" {
					goals[i].WindowStartAt = s
				}
				if s := strings.TrimSpace(opts.WindowEndAt); s != "" {
					goals[i].WindowEndAt = s
				}
				if s := strings.TrimSpace(opts.Timezone); s != "" {
					goals[i].Timezone = s
				}
				if opts.BlocksAvailability {
					goals[i].BlocksAvailability = true
				}
				if s := strings.TrimSpace(opts.RequiredCoverageTag); s != "" {
					goals[i].RequiredCoverageTag = s
				}
				if s := strings.TrimSpace(opts.ParkedUntil); s != "" {
					goals[i].ParkedUntil = s
				}
				if s := strings.TrimSpace(opts.SpaceSlug); s != "" {
					goals[i].SpaceSlug = s
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
			Slug:                slug,
			Title:               title,
			Scope:               scope,
			Status:              "active",
			TargetDate:          strings.TrimSpace(opts.TargetDate),
			WindowStartAt:       strings.TrimSpace(opts.WindowStartAt),
			WindowEndAt:         strings.TrimSpace(opts.WindowEndAt),
			Timezone:            strings.TrimSpace(opts.Timezone),
			BlocksAvailability:  opts.BlocksAvailability,
			RequiredCoverageTag: strings.TrimSpace(opts.RequiredCoverageTag),
			ParkedUntil:         strings.TrimSpace(opts.ParkedUntil),
			SpaceSlug:           strings.TrimSpace(opts.SpaceSlug),
			CreatedAt:           nowStr,
			UpdatedAt:           nowStr,
		}
		goals = append(goals, g)
		out = g
		return saveGoalsUnlocked(vaultDir, goals)
	})
	return out, err
}

func normalizeChecklistStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ChecklistConfirmed:
		return ChecklistConfirmed
	case ChecklistSkipped:
		return ChecklistSkipped
	default:
		return ChecklistOpen
	}
}

// AddChecklistItem appends (or updates by explicit ID) a 3-state ChecklistItem on a Goal.
func AddChecklistItem(vaultDir, goalSlug string, opts ChecklistItemOptions, now time.Time) (Goal, ChecklistItem, error) {
	if now.IsZero() {
		now = time.Now()
	}
	goalSlug = strings.TrimSpace(goalSlug)
	title := strings.TrimSpace(opts.Title)
	if goalSlug == "" || title == "" {
		return Goal{}, ChecklistItem{}, fmt.Errorf("both --goal and --title are required")
	}
	nowStr := now.UTC().Format(time.RFC3339)

	var outG Goal
	var outC ChecklistItem
	err := withGoalsLock(vaultDir, func() error {
		goals, lErr := loadGoalsUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		for i := range goals {
			if goals[i].Slug != goalSlug {
				continue
			}
			itemID := strings.TrimSpace(opts.ID)
			if itemID == "" {
				itemID = fmt.Sprintf("chk-%d", len(goals[i].Checklist)+1)
			}
			item := ChecklistItem{
				ID:                       itemID,
				Title:                    title,
				Tag:                      strings.TrimSpace(opts.Tag),
				Status:                   normalizeChecklistStatus(opts.Status),
				WindowStartAt:            strings.TrimSpace(opts.WindowStartAt),
				WindowEndAt:              strings.TrimSpace(opts.WindowEndAt),
				ConstraintNote:           strings.TrimSpace(opts.ConstraintNote),
				Group:                    strings.TrimSpace(opts.Group),
				ExternalRef:              strings.TrimSpace(opts.ExternalRef),
				StatusNote:               strings.TrimSpace(opts.StatusNote),
				CutoffAt:                 strings.TrimSpace(opts.CutoffAt),
				MustCompleteBeforeWindow: opts.MustCompleteBeforeWindow,
				ActionURL:                strings.TrimSpace(opts.ActionURL),
				UpdatedAt:                nowStr,
			}
			replaced := false
			for j := range goals[i].Checklist {
				if goals[i].Checklist[j].ID == itemID {
					goals[i].Checklist[j] = item
					replaced = true
					break
				}
			}
			if !replaced {
				goals[i].Checklist = append(goals[i].Checklist, item)
			}
			goals[i].UpdatedAt = nowStr
			outG = goals[i]
			outC = item
			return saveGoalsUnlocked(vaultDir, goals)
		}
		return fmt.Errorf("goal %q not found", goalSlug)
	})
	return outG, outC, err
}

func matchChecklistItem(c ChecklistItem, target string) bool {
	t := strings.TrimSpace(target)
	if t == "" {
		return false
	}
	return c.ID == t || strings.EqualFold(c.Title, t) || strings.Contains(strings.ToLower(c.Title), strings.ToLower(t))
}

// ConfirmChecklistItem transitions a Goal checklist item to `confirmed`, optionally setting ExternalRef and StatusNote.
func ConfirmChecklistItem(vaultDir, goalSlug, target, externalRef, statusNote string, now time.Time) (Goal, ChecklistItem, error) {
	if now.IsZero() {
		now = time.Now()
	}
	nowStr := now.UTC().Format(time.RFC3339)
	var outG Goal
	var outC ChecklistItem
	err := withGoalsLock(vaultDir, func() error {
		goals, lErr := loadGoalsUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		for i := range goals {
			if goals[i].Slug != strings.TrimSpace(goalSlug) {
				continue
			}
			for j := range goals[i].Checklist {
				if matchChecklistItem(goals[i].Checklist[j], target) {
					goals[i].Checklist[j].Status = ChecklistConfirmed
					if s := strings.TrimSpace(externalRef); s != "" {
						goals[i].Checklist[j].ExternalRef = s
					}
					if s := strings.TrimSpace(statusNote); s != "" {
						goals[i].Checklist[j].StatusNote = s
					}
					goals[i].Checklist[j].SkipReason = ""
					goals[i].Checklist[j].UpdatedAt = nowStr
					goals[i].UpdatedAt = nowStr
					outG = goals[i]
					outC = goals[i].Checklist[j]
					return saveGoalsUnlocked(vaultDir, goals)
				}
			}
			return fmt.Errorf("checklist item %q not found in goal %q", target, goalSlug)
		}
		return fmt.Errorf("goal %q not found", goalSlug)
	})
	return outG, outC, err
}

// SkipChecklistItem transitions a Goal checklist item to `skipped` with a persistent SkipReason tombstone.
func SkipChecklistItem(vaultDir, goalSlug, target, reason string, now time.Time) (Goal, ChecklistItem, error) {
	if now.IsZero() {
		now = time.Now()
	}
	nowStr := now.UTC().Format(time.RFC3339)
	var outG Goal
	var outC ChecklistItem
	err := withGoalsLock(vaultDir, func() error {
		goals, lErr := loadGoalsUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		for i := range goals {
			if goals[i].Slug != strings.TrimSpace(goalSlug) {
				continue
			}
			for j := range goals[i].Checklist {
				if matchChecklistItem(goals[i].Checklist[j], target) {
					goals[i].Checklist[j].Status = ChecklistSkipped
					goals[i].Checklist[j].SkipReason = strings.TrimSpace(reason)
					goals[i].Checklist[j].UpdatedAt = nowStr
					goals[i].UpdatedAt = nowStr
					outG = goals[i]
					outC = goals[i].Checklist[j]
					return saveGoalsUnlocked(vaultDir, goals)
				}
			}
			return fmt.Errorf("checklist item %q not found in goal %q", target, goalSlug)
		}
		return fmt.Errorf("goal %q not found", goalSlug)
	})
	return outG, outC, err
}

// ParkGoal sets ParkedUntil on a Goal (`YYYY-MM-DD`, `RFC3339`, or `"after:<goal-slug>"`).
func ParkGoal(vaultDir, goalSlug, until string, now time.Time) (Goal, error) {
	if now.IsZero() {
		now = time.Now()
	}
	until = strings.TrimSpace(until)
	if until == "" {
		return Goal{}, fmt.Errorf("--until is required (YYYY-MM-DD, RFC3339, or after:<goal-slug>)")
	}
	nowStr := now.UTC().Format(time.RFC3339)
	var out Goal
	err := withGoalsLock(vaultDir, func() error {
		goals, lErr := loadGoalsUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		for i := range goals {
			if goals[i].Slug == strings.TrimSpace(goalSlug) {
				goals[i].ParkedUntil = until
				goals[i].UpdatedAt = nowStr
				out = goals[i]
				return saveGoalsUnlocked(vaultDir, goals)
			}
		}
		return fmt.Errorf("goal %q not found", goalSlug)
	})
	return out, err
}

// UnparkGoal clears ParkedUntil on a Goal.
func UnparkGoal(vaultDir, goalSlug string, now time.Time) (Goal, error) {
	if now.IsZero() {
		now = time.Now()
	}
	nowStr := now.UTC().Format(time.RFC3339)
	var out Goal
	err := withGoalsLock(vaultDir, func() error {
		goals, lErr := loadGoalsUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		for i := range goals {
			if goals[i].Slug == strings.TrimSpace(goalSlug) {
				goals[i].ParkedUntil = ""
				goals[i].UpdatedAt = nowStr
				out = goals[i]
				return saveGoalsUnlocked(vaultDir, goals)
			}
		}
		return fmt.Errorf("goal %q not found", goalSlug)
	})
	return out, err
}
