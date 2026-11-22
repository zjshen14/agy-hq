package living

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/store"
)

var reValidSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidateSlug ensures a Living Space slug is safe for filenames and URL routes.
func ValidateSlug(slug string) error {
	s := strings.TrimSpace(slug)
	if !reValidSlug.MatchString(s) {
		return fmt.Errorf("invalid living space slug %q (expected lowercase alphanumeric and hyphens)", slug)
	}
	return nil
}

// SpaceDir returns `<vault>/living_spaces/<slug>`.
func SpaceDir(vaultDir, slug string) string {
	return filepath.Join(vaultDir, "living_spaces", strings.TrimSpace(slug))
}

func spaceJSONPath(vaultDir, slug string) string {
	return filepath.Join(SpaceDir(vaultDir, slug), "space.json")
}

func spaceHTMLPath(vaultDir, slug string) string {
	return filepath.Join(SpaceDir(vaultDir, slug), "index.html")
}

func spaceLockPath(vaultDir, slug string) string {
	return filepath.Join(SpaceDir(vaultDir, slug), ".space.lock")
}

// Load reads `<vault>/living_spaces/<slug>/space.json` under RankLivingSpaces.
func Load(vaultDir, slug string) (SpaceDoc, error) {
	if err := ValidateSlug(slug); err != nil {
		return SpaceDoc{}, err
	}
	var doc SpaceDoc
	err := store.With(context.Background(), store.RankLivingSpaces, spaceLockPath(vaultDir, slug), func(_ context.Context) error {
		raw, rErr := os.ReadFile(spaceJSONPath(vaultDir, slug))
		if rErr != nil {
			return rErr
		}
		return json.Unmarshal(raw, &doc)
	})
	return doc, err
}

// Save writes `<vault>/living_spaces/<slug>/space.json` atomically under RankLivingSpaces.
func Save(vaultDir string, doc SpaceDoc, now time.Time) (SpaceDoc, error) {
	if err := ValidateSlug(doc.Slug); err != nil {
		return SpaceDoc{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	cfg := LoadConfig(vaultDir)
	if doc.SchemaVersion == 0 {
		doc.SchemaVersion = 1
	}
	if len(doc.Locales) == 0 {
		doc.Locales = append([]string(nil), cfg.DefaultLocales...)
	}
	if len(doc.LocaleLabels) == 0 {
		doc.LocaleLabels = make(map[string]string, len(doc.Locales))
		for _, loc := range doc.Locales {
			if lbl, ok := cfg.LocaleLabels[loc]; ok {
				doc.LocaleLabels[loc] = lbl
			} else {
				doc.LocaleLabels[loc] = strings.ToUpper(loc)
			}
		}
	}
	nowStr := now.UTC().Format(time.RFC3339)
	if doc.CreatedAt == "" {
		doc.CreatedAt = nowStr
	}
	doc.UpdatedAt = nowStr

	err := store.With(context.Background(), store.RankLivingSpaces, spaceLockPath(vaultDir, doc.Slug), func(_ context.Context) error {
		b, mErr := json.MarshalIndent(doc, "", "  ")
		if mErr != nil {
			return mErr
		}
		return store.WriteAtomic(spaceJSONPath(vaultDir, doc.Slug), append(b, '\n'), 0o600)
	})
	return doc, err
}

// List returns summaries of all Living Spaces stored in `<vault>/living_spaces/`.
func List(vaultDir string) ([]SpaceSummary, error) {
	root := filepath.Join(vaultDir, "living_spaces")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []SpaceSummary
	for _, e := range entries {
		if !e.IsDir() || ValidateSlug(e.Name()) != nil {
			continue
		}
		doc, lErr := Load(vaultDir, e.Name())
		if lErr != nil {
			continue
		}
		primaryLoc := "en"
		if len(doc.Locales) > 0 {
			primaryLoc = doc.Locales[0]
		}
		_, htmlErr := os.Stat(spaceHTMLPath(vaultDir, doc.Slug))
		out = append(out, SpaceSummary{
			Slug:      doc.Slug,
			Title:     doc.Title.Resolve(primaryLoc, "en"),
			GoalSlug:  doc.GoalSlug,
			Locales:   doc.Locales,
			UpdatedAt: doc.UpdatedAt,
			HasHTML:   htmlErr == nil,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out, nil
}

// SyncFromGoal pulls a linked Goal's 3-state Checklist, WindowStartAt, and Timezone into the Space's
// `kpi_strip` and `checklist_ledger` blocks in 0 LLM tokens, preserving existing Space blocks.
func SyncFromGoal(vaultDir, slug, goalSlug string, now time.Time) (SpaceDoc, error) {
	if now.IsZero() {
		now = time.Now()
	}
	doc, err := Load(vaultDir, slug)
	if err != nil {
		return SpaceDoc{}, err
	}
	if strings.TrimSpace(goalSlug) == "" {
		goalSlug = doc.GoalSlug
	}
	if strings.TrimSpace(goalSlug) == "" {
		return SpaceDoc{}, fmt.Errorf("living space %q has no linked goalSlug", slug)
	}
	goals, err := kernel.ListGoals(vaultDir)
	if err != nil {
		return SpaceDoc{}, err
	}
	var target *kernel.Goal
	for i := range goals {
		if goals[i].Slug == goalSlug {
			cp := goals[i]
			target = &cp
			break
		}
	}
	if target == nil {
		return SpaceDoc{}, fmt.Errorf("goal %q not found", goalSlug)
	}

	doc.GoalSlug = target.Slug
	if target.Timezone != "" && doc.Timezone == "" {
		doc.Timezone = target.Timezone
	}
	prog := target.Readiness()
	countdownAt := target.WindowStartAt
	if countdownAt == "" {
		countdownAt = target.TargetDate
	}

	rows := make([]ChecklistRow, 0, len(target.Checklist))
	for _, ci := range target.Checklist {
		rows = append(rows, ChecklistRow{
			ID:          ci.ID,
			Title:       Text(ci.Title),
			Tag:         ci.Tag,
			Status:      ci.Status,
			Group:       ci.Group,
			ExternalRef: ci.ExternalRef,
			StatusNote:  Text(ci.StatusNote),
			SkipReason:  Text(ci.SkipReason),
			CutoffAt:    ci.CutoffAt,
			ActionURL:   ci.ActionURL,
		})
	}

	updatedKPI := false
	updatedLedger := false
	for i := range doc.Blocks {
		switch doc.Blocks[i].Type {
		case BlockKPIStrip:
			if countdownAt != "" {
				doc.Blocks[i].CountdownAt = countdownAt
			}
			doc.Blocks[i].ConfirmedCount = prog.Confirmed
			doc.Blocks[i].OpenCount = prog.Open
			doc.Blocks[i].SkippedCount = prog.Skipped
			doc.Blocks[i].ShowReadiness = true
			updatedKPI = true
		case BlockChecklistLedger:
			doc.Blocks[i].Items = rows
			updatedLedger = true
		}
	}
	if !updatedKPI {
		kpi := BlockSpec{
			ID:             "kpi-goal",
			Type:           BlockKPIStrip,
			CountdownAt:    countdownAt,
			CountdownLabel: Text(target.Title),
			ConfirmedCount: prog.Confirmed,
			OpenCount:      prog.Open,
			SkippedCount:   prog.Skipped,
			ShowReadiness:  true,
		}
		doc.Blocks = append([]BlockSpec{kpi}, doc.Blocks...)
	}
	if !updatedLedger && len(rows) > 0 {
		doc.Blocks = append(doc.Blocks, BlockSpec{
			ID:    "checklist-goal",
			Type:  BlockChecklistLedger,
			Title: Text("Readiness Checklist"),
			Items: rows,
		})
	}
	return Save(vaultDir, doc, now)
}
