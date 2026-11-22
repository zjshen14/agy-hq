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
