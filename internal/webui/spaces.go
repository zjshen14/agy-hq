package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/internal/workspace"
	"github.com/zjshen14/agy-hq/internal/yamlmini"
)

// SpaceWidgetOutput holds safe declarative lines rendered by a built-in Console Space widget.
type SpaceWidgetOutput struct {
	Kind  string   `json:"kind"`
	Title string   `json:"title"`
	Lines []string `json:"lines"`
}

// ConsoleSpace represents a declarative, schedule-aware Console Space (RFC-0014 Phase 2C).
type ConsoleSpace struct {
	ID            string              `json:"id"`
	Title         string              `json:"title"`
	Schedule      string              `json:"schedule"`
	Scope         string              `json:"scope,omitempty"`
	Widgets       []string            `json:"widgets"`
	OverrideMode  string              `json:"overrideMode,omitempty"` // "", "pin", "hide"
	ActiveNow     bool                `json:"activeNow"`
	RenderedCards []SpaceWidgetOutput `json:"renderedCards,omitempty"`
}

func spaceOverridesPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "space_overrides.json")
}

func loadSpaceOverrides(vaultDir string) map[string]string {
	out := make(map[string]string)
	data, err := os.ReadFile(spaceOverridesPath(vaultDir))
	if err == nil {
		_ = json.Unmarshal(data, &out)
	}
	return out
}

// SetSpaceOverride sets the visibility override ("pin", "hide", or "auto") for a Console Space ID.
func SetSpaceOverride(vaultDir, id, mode string) error {
	id = strings.TrimSpace(id)
	mode = strings.ToLower(strings.TrimSpace(mode))
	if id == "" {
		return fmt.Errorf("space id is required")
	}
	if mode != "pin" && mode != "hide" && mode != "auto" && mode != "" {
		return fmt.Errorf("invalid space override mode %q (expected pin, hide, or auto)", mode)
	}
	memDir := filepath.Join(vaultDir, "memory")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		return err
	}
	lockPath := filepath.Join(memDir, ".spaces.lock")
	return store.With(context.Background(), store.RankMemory, lockPath, func(context.Context) error {
		overrides := loadSpaceOverrides(vaultDir)
		if mode == "auto" || mode == "" {
			delete(overrides, id)
		} else {
			overrides[id] = mode
		}
		b, err := json.MarshalIndent(overrides, "", "  ")
		if err != nil {
			return err
		}
		return store.WriteAtomic(spaceOverridesPath(vaultDir), append(b, '\n'), 0o600)
	})
}

// ParseSpacesFromConfigYAML extracts the `spaces:` list block from local/config.yaml.
func ParseSpacesFromConfigYAML(raw string) []ConsoleSpace {
	var spaces []ConsoleSpace
	inSpaces := false
	var cur *ConsoleSpace
	inWidgetsList := false

	flush := func() {
		if cur != nil && cur.ID != "" {
			if cur.Title == "" {
				cur.Title = cur.ID
			}
			spaces = append(spaces, *cur)
		}
		cur = nil
		inWidgetsList = false
	}

	for _, rawLine := range yamlmini.Lines(raw) {
		clean := yamlmini.StripComment(rawLine)
		if clean == "" {
			continue
		}
		indent := yamlmini.LeadingIndent(rawLine)
		if indent == 0 {
			flush()
			inSpaces = (clean == "spaces:")
			continue
		}
		if !inSpaces {
			continue
		}
		if rest, isItem := yamlmini.TrimListItem(clean); isItem {
			if inWidgetsList && cur != nil {
				if _, _, isKV := yamlmini.SplitKV(rest); !isKV {
					if item := yamlmini.UnquoteScalar(rest); item != "" {
						cur.Widgets = append(cur.Widgets, item)
					}
					continue
				}
			}
			flush()
			cur = &ConsoleSpace{}
			if k, v, ok := yamlmini.SplitKV(rest); ok {
				applySpaceKV(cur, k, v, &inWidgetsList)
			}
			continue
		}
		if cur != nil {
			if k, v, ok := yamlmini.SplitKV(clean); ok {
				inWidgetsList = false
				applySpaceKV(cur, k, v, &inWidgetsList)
			}
		}
	}
	flush()
	return spaces
}

// ParseStandaloneSpaceYAML parses a single ConsoleSpace YAML file from local/console_spaces/<slug>.yaml.
func ParseStandaloneSpaceYAML(raw, fallbackID string) []ConsoleSpace {
	if strings.Contains(raw, "\nspaces:") || strings.HasPrefix(strings.TrimSpace(raw), "spaces:") {
		return ParseSpacesFromConfigYAML(raw)
	}
	sp := ConsoleSpace{ID: fallbackID}
	inWidgets := false
	for _, rawLine := range yamlmini.Lines(raw) {
		clean := yamlmini.StripComment(rawLine)
		if clean == "" {
			continue
		}
		if rest, isItem := yamlmini.TrimListItem(clean); isItem && inWidgets {
			if v := yamlmini.UnquoteScalar(rest); v != "" {
				sp.Widgets = append(sp.Widgets, v)
			}
			continue
		}
		if k, v, ok := yamlmini.SplitKV(clean); ok {
			inWidgets = false
			applySpaceKV(&sp, k, v, &inWidgets)
		}
	}
	if sp.ID == "" {
		return nil
	}
	if sp.Title == "" {
		sp.Title = sp.ID
	}
	return []ConsoleSpace{sp}
}

func applySpaceKV(sp *ConsoleSpace, key, val string, inWidgets *bool) {
	unquoted := yamlmini.UnquoteScalar(val)
	switch key {
	case "id":
		sp.ID = unquoted
	case "title":
		sp.Title = unquoted
	case "schedule":
		sp.Schedule = unquoted
	case "scope":
		sp.Scope = unquoted
	case "widgets":
		if val == "" {
			*inWidgets = true
		} else if arr, ok := yamlmini.ParseInlineList(val); ok {
			sp.Widgets = arr
		} else if unquoted != "" {
			sp.Widgets = []string{unquoted}
		}
	}
}

// MatchesSpaceSchedule evaluates whether a schedule expression (e.g. "weekdays 06:30-10:00 America/Los_Angeles")
// is active at `now`.
func MatchesSpaceSchedule(sched string, now time.Time) bool {
	s := strings.TrimSpace(sched)
	if s == "" || strings.EqualFold(s, "always") {
		return true
	}
	if strings.EqualFold(s, "manual") || strings.EqualFold(s, "never") {
		return false
	}

	fields := strings.Fields(s)
	evalTime := now
	if len(fields) >= 2 {
		last := fields[len(fields)-1]
		if strings.Contains(last, "/") || strings.EqualFold(last, "UTC") {
			if loc, err := time.LoadLocation(last); err == nil {
				evalTime = now.In(loc)
			}
			fields = fields[:len(fields)-1]
		}
	}

	daySpec := "daily"
	windowSpec := ""
	if len(fields) == 1 {
		if strings.Contains(fields[0], "-") && strings.Contains(fields[0], ":") {
			windowSpec = fields[0]
		} else {
			daySpec = strings.ToLower(fields[0])
		}
	} else if len(fields) >= 2 {
		daySpec = strings.ToLower(fields[0])
		windowSpec = fields[1]
	}

	wd := evalTime.Weekday()
	switch daySpec {
	case "weekdays", "mon-fri":
		if wd == time.Saturday || wd == time.Sunday {
			return false
		}
	case "weekends", "sat-sun":
		if wd != time.Saturday && wd != time.Sunday {
			return false
		}
	case "daily", "everyday", "*":
		// always matches day
	}

	if windowSpec == "" {
		return true
	}
	parts := strings.SplitN(windowSpec, "-", 2)
	if len(parts) != 2 {
		return false
	}
	startMin, ok1 := outbox.ParseHHMMMinutes(parts[0])
	endMin, ok2 := outbox.ParseHHMMMinutes(parts[1])
	if !ok1 || !ok2 {
		return false
	}
	curMin := evalTime.Hour()*60 + evalTime.Minute()
	if startMin < endMin {
		return curMin >= startMin && curMin < endMin
	}
	// Overnight window (e.g. 21:00-02:00)
	return curMin >= startMin || curMin < endMin
}

// ListSpaces loads Console Spaces from local/config.yaml and local/console_spaces/*.yaml,
// applies user pin/hide overrides, and evaluates ActiveNow at `now`.
func ListSpaces(vaultDir string, now time.Time) ([]ConsoleSpace, error) {
	var orderedIDs []string
	byID := make(map[string]ConsoleSpace)

	if cfgBytes, err := os.ReadFile(filepath.Join(vaultDir, "config.yaml")); err == nil {
		for _, sp := range ParseSpacesFromConfigYAML(string(cfgBytes)) {
			if _, exists := byID[sp.ID]; !exists {
				orderedIDs = append(orderedIDs, sp.ID)
			}
			byID[sp.ID] = sp
		}
	}

	spacesDir := filepath.Join(vaultDir, "console_spaces")
	if entries, err := os.ReadDir(spacesDir); err == nil {
		var names []string
		for _, e := range entries {
			lower := strings.ToLower(e.Name())
			if !e.IsDir() && (strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")) {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			raw, err := os.ReadFile(filepath.Join(spacesDir, name))
			if err != nil {
				continue
			}
			fallbackID := strings.TrimSuffix(name, filepath.Ext(name))
			for _, sp := range ParseStandaloneSpaceYAML(string(raw), fallbackID) {
				if _, exists := byID[sp.ID]; !exists {
					orderedIDs = append(orderedIDs, sp.ID)
				}
				byID[sp.ID] = sp
			}
		}
	}

	overrides := loadSpaceOverrides(vaultDir)
	var out []ConsoleSpace
	for _, id := range orderedIDs {
		sp := byID[id]
		ov := overrides[id]
		sp.OverrideMode = ov
		switch ov {
		case "pin":
			sp.ActiveNow = true
		case "hide":
			sp.ActiveNow = false
		default:
			sp.ActiveNow = MatchesSpaceSchedule(sp.Schedule, now)
		}
		out = append(out, sp)
	}
	return out, nil
}

type spaceWidgetCache struct {
	goals          []kernel.Goal
	goalsLoaded    bool
	ideas          []outbox.OutboxItem
	ideasLoaded    bool
	surfaceEntries []workspace.SurfaceEntry
	surfaceLoaded  bool
	layer1Tasks    []kernel.ActiveTask
	tasksLoaded    bool
}

func (c *spaceWidgetCache) getGoals(vaultDir string) []kernel.Goal {
	if !c.goalsLoaded {
		c.goals, _ = kernel.ListGoals(vaultDir)
		c.goalsLoaded = true
	}
	return c.goals
}

func (c *spaceWidgetCache) getIdeas(vaultDir string, now time.Time) []outbox.OutboxItem {
	if !c.ideasLoaded {
		c.ideas, _ = outbox.List(vaultDir, outbox.TierIdeasFeed, outbox.StatusPending, now)
		c.ideasLoaded = true
	}
	return c.ideas
}

func (c *spaceWidgetCache) getSurfaceEntries(vaultDir string, now time.Time) []workspace.SurfaceEntry {
	if !c.surfaceLoaded {
		c.surfaceEntries = kernel.UnifiedSurfaceEntries(vaultDir, now)
		c.surfaceLoaded = true
	}
	return c.surfaceEntries
}

func (c *spaceWidgetCache) getLayer1Tasks(vaultDir string) []kernel.ActiveTask {
	if !c.tasksLoaded {
		tasks, _ := kernel.ListTasks(vaultDir)
		c.layer1Tasks = kernel.FilterLayer1Tasks(vaultDir, tasks)
		c.tasksLoaded = true
	}
	return c.layer1Tasks
}

// ActiveSpaces returns all Console Spaces currently active (by schedule or pin override) with rendered widgets.
func ActiveSpaces(vaultDir string, now time.Time) ([]ConsoleSpace, error) {
	return activeSpacesWithCache(vaultDir, now, &spaceWidgetCache{})
}

func activeSpacesWithCache(vaultDir string, now time.Time, cache *spaceWidgetCache) ([]ConsoleSpace, error) {
	if cache == nil {
		cache = &spaceWidgetCache{}
	}
	all, err := ListSpaces(vaultDir, now)
	if err != nil {
		return nil, err
	}
	var active []ConsoleSpace
	for _, sp := range all {
		if !sp.ActiveNow {
			continue
		}
		sp.RenderedCards = renderSpaceWidgets(vaultDir, sp, now, cache)
		active = append(active, sp)
	}
	return active, nil
}

func renderSpaceWidgets(vaultDir string, sp ConsoleSpace, now time.Time, cache *spaceWidgetCache) []SpaceWidgetOutput {
	var cards []SpaceWidgetOutput
	for _, w := range sp.Widgets {
		kind := strings.ToLower(strings.TrimSpace(w))
		switch kind {
		case "goals_summary":
			goals := cache.getGoals(vaultDir)
			var lines []string
			for _, g := range goals {
				if g.Status != "active" {
					continue
				}
				if sp.Scope != "" && g.Scope != "" && !strings.EqualFold(sp.Scope, g.Scope) {
					continue
				}
				done := 0
				for _, m := range g.Milestones {
					if m.Done {
						done++
					}
				}
				line := fmt.Sprintf("%s (%d/%d milestones)", g.Title, done, len(g.Milestones))
				if g.TargetDate != "" {
					line += " · target " + g.TargetDate
				}
				lines = append(lines, line)
			}
			if len(lines) == 0 {
				lines = []string{"No active goals in scope."}
			}
			cards = append(cards, SpaceWidgetOutput{Kind: kind, Title: "Goals Summary", Lines: lines})

		case "waiting_on_slas":
			goals := cache.getGoals(vaultDir)
			var lines []string
			for _, g := range goals {
				if g.Status != "active" {
					continue
				}
				for _, wItem := range g.WaitingOn {
					if wItem.Status == "replied" {
						continue
					}
					lines = append(lines, fmt.Sprintf("%s — %s [%s · follow-up %s]", wItem.Counterparty, wItem.Summary, wItem.Status, wItem.FollowUpAfter))
				}
			}
			if len(lines) == 0 {
				lines = []string{"No open counterparty SLAs."}
			}
			cards = append(cards, SpaceWidgetOutput{Kind: kind, Title: "Counterparty Waiting-On SLAs", Lines: lines})

		case "ideas_feed":
			ideas := cache.getIdeas(vaultDir, now)
			var lines []string
			for _, it := range ideas {
				if sp.Scope != "" && it.Scope != "" && !strings.EqualFold(sp.Scope, it.Scope) {
					continue
				}
				lines = append(lines, fmt.Sprintf("[%s] %s — %s", it.ID, it.Title, it.Summary))
				if len(lines) >= 5 {
					break
				}
			}
			if len(lines) == 0 {
				lines = []string{"No pending proactive ideas."}
			}
			cards = append(cards, SpaceWidgetOutput{Kind: kind, Title: "Proactive Ideas", Lines: lines})

		case "worktree_git_status", "active_workspaces", "workspaces_status":
			entries := cache.getSurfaceEntries(vaultDir, now)
			var lines []string
			for _, e := range entries {
				label := e.DisplayName()
				if e.Provider != workspace.CoreGitProviderID && e.ProviderShort != "" {
					label = e.ProviderShort + ":" + label
				}
				if e.OpenChanges > 0 {
					if e.NeedsOwner > 0 {
						label += fmt.Sprintf(" (%d open, %d needs you)", e.OpenChanges, e.NeedsOwner)
					} else {
						label += fmt.Sprintf(" (%d open)", e.OpenChanges)
					}
				} else if e.State == workspace.StateDirty {
					label += " (dirty)"
				}
				lines = append(lines, label)
			}
			if len(lines) == 0 {
				lines = []string{"No active workspaces."}
			}
			cards = append(cards, SpaceWidgetOutput{Kind: kind, Title: "Active Workspaces", Lines: lines})

		case "thesis_status":
			invDir := filepath.Join(vaultDir, "notes", "investing")
			var lines []string
			if entries, err := os.ReadDir(invDir); err == nil {
				for _, e := range entries {
					if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
						info, _ := e.Info()
						mod := ""
						if info != nil {
							mod = " (updated " + info.ModTime().Format("2006-01-02") + ")"
						}
						lines = append(lines, strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))+mod)
					}
				}
			}
			if len(lines) == 0 {
				lines = []string{"No thesis notes tracked in local/notes/investing/."}
			}
			cards = append(cards, SpaceWidgetOutput{Kind: kind, Title: "Thesis Watchlist", Lines: lines})

		case "tasks_scope":
			layer1 := cache.getLayer1Tasks(vaultDir)
			var lines []string
			for _, t := range layer1 {
				if t.EffectiveStatus(now) != "active" {
					continue
				}
				if sp.Scope != "" && !strings.EqualFold(t.EffectiveScope(), sp.Scope) {
					continue
				}
				lines = append(lines, fmt.Sprintf("%s (%s)", kernel.SanitizeTaskTitle(t.Title, t.TaskSlug), t.TaskSlug))
			}
			if len(lines) == 0 {
				lines = []string{"No active tasks in scope."}
			}
			cards = append(cards, SpaceWidgetOutput{Kind: kind, Title: "Scoped Active Tasks", Lines: lines})
		}
	}
	return cards
}
