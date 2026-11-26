package living

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

func TestLivingSpaceSingleVsMultiLocaleGoalSyncDataSourcesAndArtifact(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

	// 1. Default OSS framework config: single-locale ["en"] -> language switcher hidden
	docSingle := SpaceDoc{
		Slug:     "q4-warroom",
		Title:    Text("Q4 Release War-Room"),
		Subtitle: Text("Engineering cutover cockpit"),
		Perspectives: []PerspectiveSpec{
			{ID: "exec", Label: Text("Stakeholder TL;DR")},
			{ID: "eng", Label: Text("Engineering")},
		},
		Groups: []string{"backend", "infra"},
		DataSources: []DataSourceSpec{
			{ID: "ds-ws", Kind: "hq_builtin", Target: "workspaces"},
			{ID: "ds-bad", Kind: "trusted_argv", Argv: []string{"sh", "-c", "echo pwn"}},
		},
		Blocks: []BlockSpec{
			{ID: "tbl-ws", Type: BlockComparisonTable, Title: Text("Workspaces"), DataSourceID: "ds-ws"},
			{
				ID:    "tl-1",
				Type:  BlockTimelineAccordion,
				Title: Text("Cutover Timeline"),
				Entries: []TimelineEntry{
					{ID: "e1", DateLabel: Text("Nov 24"), Title: Text("Freeze Main"), Group: "infra", Perspectives: []string{"eng"}, Details: []LocalizedText{Text("Tag rc1 </script><img src=x onerror=alert(1)>")}},
				},
			},
			{
				ID:    "notes-1",
				Type:  BlockCalloutNotes,
				Title: Text("Rollback Runbook"),
				Notes: []CalloutNote{
					{Tone: "warn", Title: Text("Abort Criteria"), Body: Text("5xx > 0.5% for 5m"), Perspectives: []string{"exec", "eng"}},
				},
			},
		},
	}
	if _, err := Save(vaultDir, docSingle, now); err != nil {
		t.Fatalf("Save single: %v", err)
	}

	// Seed a workspace + Goal for 0-token refresh & sync
	_ = workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
		l.Put(workspace.LedgerEntry{
			Provider:    "core/git-worktree",
			WorkspaceID: "ws-cutover",
			Name:        "headquarter-cutover",
			Root:        "/home/user/ws/headquarter-cutover",
			Ownership:   workspace.OwnershipOwned,
			Status: workspace.LedgerStatus{
				State:   workspace.StateClean,
				Changes: []workspace.ChangeRef{{ID: "PR#200", State: workspace.ChangeInReview, NeedsOwner: true}},
			},
		})
		return true, nil
	})
	_, _ = kernel.AddGoalWithOptions(vaultDir, kernel.AddGoalOptions{
		Slug:          "q4-cutover",
		Title:         "Q4 Cutover",
		Scope:         "engineering",
		WindowStartAt: "2026-11-25",
		Timezone:      "America/Los_Angeles",
	}, now)
	_, _, _ = kernel.AddChecklistItem(vaultDir, "q4-cutover", kernel.ChecklistItemOptions{ID: "c1", Title: "Canary green", Status: kernel.ChecklistConfirmed, ExternalRef: "PR#200"}, now)
	_, _, _ = kernel.AddChecklistItem(vaultDir, "q4-cutover", kernel.ChecklistItemOptions{ID: "c2", Title: "Update changelog", Status: kernel.ChecklistOpen}, now)
	_, _, _ = kernel.AddChecklistItem(vaultDir, "q4-cutover", kernel.ChecklistItemOptions{ID: "c3", Title: "Optional webinar", Status: kernel.ChecklistSkipped}, now)
	_, _, _ = kernel.SkipChecklistItem(vaultDir, "q4-cutover", "c3", "Not needed this quarter", now)

	// Refresh data sources -> ds-ws succeeds, ds-bad blocked cleanly
	refreshed, err := RefreshDataSources(context.Background(), vaultDir, "q4-warroom", now)
	if err != nil {
		t.Fatalf("RefreshDataSources: %v", err)
	}
	if refreshed.DataSources[0].LastError != "" || refreshed.DataSources[1].LastError == "" {
		t.Fatalf("expected ds-ws ok and ds-bad blocked, got: %+v", refreshed.DataSources)
	}

	// Sync Goal checklist into Space
	synced, err := SyncFromGoal(vaultDir, "q4-warroom", "q4-cutover", now)
	if err != nil {
		t.Fatalf("SyncFromGoal: %v", err)
	}
	if synced.GoalSlug != "q4-cutover" {
		t.Fatalf("expected goalSlug=q4-cutover, got %q", synced.GoalSlug)
	}

	// Build HTML
	htmlPath, size, err := Build(vaultDir, "q4-warroom")
	if err != nil || size == 0 || size > 45*1024 {
		t.Fatalf("Build failed or exceeded 45KB: path=%s size=%d err=%v", htmlPath, size, err)
	}
	rawBytes, _ := os.ReadFile(htmlPath)
	htmlStr := string(rawBytes)
	if strings.Contains(htmlStr, `class="lang-switch"`) {
		t.Fatalf("single-locale space must hide lang-switch button group")
	}
	if strings.Contains(htmlStr, "</script><img") {
		t.Fatalf("XSS breakout sequence was not neutralized in HTML output")
	}
	if !strings.Contains(htmlStr, "headquarter-cutover") || !strings.Contains(htmlStr, "1 / 2 (50%)") || !strings.Contains(htmlStr, "Skipped (1") {
		t.Fatalf("expected live workspace row + 1/2 readiness + skipped tombstone in rendered HTML")
	}

	// Export Antigravity artifact
	artDir := filepath.Join(t.TempDir(), "artifacts")
	artPath, err := ExportArtifact(vaultDir, "q4-warroom", artDir, now)
	if err != nil {
		t.Fatalf("ExportArtifact: %v", err)
	}
	if _, err := os.Stat(artPath + ".metadata.json"); err != nil {
		t.Fatalf("expected artifact metadata file: %v", err)
	}

	// 2. Vault override: bilingual ["en", "zh"] -> renders lang-switch + hq-i18n dict
	_ = os.WriteFile(filepath.Join(vaultDir, "config.yaml"), []byte("living_spaces:\n  default_locales: [\"en\", \"zh\"]\n"), 0o600)
	docBi := SpaceDoc{
		Slug:  "team-offsite",
		Title: MultiText(map[string]string{"en": "Team Offsite", "zh": "团队团建"}),
	}
	if _, err := Save(vaultDir, docBi, now); err != nil {
		t.Fatalf("Save bilingual: %v", err)
	}
	biPath, _, err := Build(vaultDir, "team-offsite")
	if err != nil {
		t.Fatalf("Build bilingual: %v", err)
	}
	biBytes, _ := os.ReadFile(biPath)
	biStr := string(biBytes)
	if !strings.Contains(biStr, `class="lang-switch"`) || !strings.Contains(biStr, "团队团建") || !strings.Contains(biStr, `id="hq-i18n"`) {
		t.Fatalf("expected bilingual lang-switch and zh dictionary in compiled HTML")
	}
}

func TestLivingSpaceEditorialThemesInteractiveMapStoryCardsAndSecurity(t *testing.T) {
	now := time.Date(2026, 11, 25, 18, 0, 0, 0, time.UTC)
	vaultDir := t.TempDir()

	validDataImg := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a8b8AAAAASUVORK5CYII="
	doc := SpaceDoc{
		Slug:      "campus-field-guide",
		Theme:     ThemeHeritage,
		HeroBadge: MultiText(map[string]string{"en": "INTERACTIVE FIELD GUIDE", "zh": "互动导览"}),
		Title:     MultiText(map[string]string{"en": "Historic Campus Walk", "zh": "历史园区漫步"}),
		Subtitle:  Text("Self-guided architectural route with interactive map & spotlights"),
		Locales:   []string{"en", "zh"},
		LocaleLabels: map[string]string{
			"en": "EN",
			"zh": "中文",
		},
		Blocks: []BlockSpec{
			{
				ID:    "route-map",
				Type:  BlockInteractiveMap,
				Title: MultiText(map[string]string{"en": "Route Map", "zh": "路线图"}),
				Map: &MapSpec{
					ViewBox: "0 0 800 360",
					Zones: []MapZone{
						{X: 20, Y: 20, W: 340, H: 160, Label: MultiText(map[string]string{"en": "West Wing", "zh": "西翼"}), Tone: "gold"},
					},
					Paths: []MapPath{
						{Points: "80,100 320,120 620,240", Dashed: true, Tone: "gold"},
						{Points: `10,10" onload="alert(1)`, Tone: "accent"},
					},
					Pins: []MapPin{
						{
							ID:       "stop-1",
							Number:   1,
							X:        80,
							Y:        100,
							Title:    MultiText(map[string]string{"en": "Main Archway", "zh": "主拱门"}),
							Subtitle: Text("1892 Limestone Gate"),
							Meta:     Text("10 min"),
							Body:     MultiText(map[string]string{"en": "Start beneath the carved arches.", "zh": "从雕刻拱门开始。"}),
							Tip:      Text("Best morning light before 10:00."),
							Tone:     "gold",
						},
						{
							ID:    "stop-2",
							X:     320,
							Y:     120,
							Title: Text("Library Rotunda"),
							Body:  Text("Look up at the timber dome."),
						},
					},
					Legend: []LocalizedText{Text("Dashed line = walking path")},
				},
			},
			{
				ID:    "spotlights",
				Type:  BlockStoryCards,
				Title: Text("Key Spotlights"),
				Cards: []StoryCard{
					{
						ID:           "card-valid",
						Eyebrow:      Text("Architecture"),
						Icon:         "🏛️",
						Title:        Text("Clock Tower"),
						Subtitle:     Text("Restored 2021"),
						Body:         Text("Chimes every quarter hour."),
						Highlights:   []LocalizedText{Text("360-degree terrace view"), Text("Original brass mechanism")},
						ExpandLabel:  Text("Historical note"),
						ExpandBody:   Text("Built with local sandstone quarried 12 miles east."),
						Meta:         Text("Stop #1"),
						Badge:        "Must See",
						Tone:         "accent",
						ImageDataURI: validDataImg,
						CopyText:     "Clock Tower Main Gate",
						CopyLabel:    Text("Copy Landmark"),
					},
					{
						ID:           "card-blocked-img",
						Title:        Text("Untrusted Image Guard"),
						Body:         Text("External image URLs must be stripped for strict CSP."),
						ImageDataURI: "https://evil.example/tracker.png",
					},
				},
			},
			{
				ID:    "prep-list",
				Type:  BlockChecklistLedger,
				Title: Text("Preparation"),
				Items: []ChecklistRow{
					{ID: "chk-1", Title: Text("Download offline transit pass"), Status: "open"},
					{ID: "chk-2", Title: Text("Reserve audio guide"), Status: "confirmed"},
				},
			},
		},
	}

	if _, err := Save(vaultDir, doc, now); err != nil {
		t.Fatalf("Save editorial space: %v", err)
	}
	htmlPath, _, err := Build(vaultDir, "campus-field-guide")
	if err != nil {
		t.Fatalf("Build editorial space: %v", err)
	}
	raw, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("read rendered html: %v", err)
	}
	s := string(raw)

	for _, want := range []string{
		`data-theme="heritage"`,
		`class="hero-badge"`,
		`INTERACTIVE FIELD GUIDE`,
		`id="hq-mode-btn"`,
		`id="hq-space-search"`,
		`class="toc-strip"`,
		`class="imap-shell"`,
		`data-map-pin="0"`,
		`data-map-detail="1"`,
		`class="pill imap-next"`,
		`主拱门`,
		`class="story-grid"`,
		`class="story-deep"`,
		validDataImg,
		`data-chk-toggle="chk-1"`,
		`data-chk-sync="1"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("rendered editorial space missing %q", want)
		}
	}

	// Verify strict security sanitizers blocked external image URL & SVG attribute injection
	for _, forbidden := range []string{
		"https://evil.example/tracker.png",
		`onload="alert(1)"`,
	} {
		if strings.Contains(s, forbidden) {
			t.Fatalf("rendered editorial space leaked forbidden sequence %q", forbidden)
		}
	}

	// Verify unknown theme falls back cleanly to modern
	if got := normalizeTheme(`" onclick="alert(1)`); got != ThemeModern {
		t.Fatalf("normalizeTheme fallback = %q, want %q", got, ThemeModern)
	}
	if got := safeViewBox(`0 0 100 100" onload="alert(1)`); got != "0 0 800 400" {
		t.Fatalf("safeViewBox fallback = %q, want default", got)
	}
}
