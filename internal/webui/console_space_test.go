package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/living"
	"github.com/zjshen14/agy-hq/internal/outbox"
)

func TestSyncMarkdownConsoleRetiresLegacyLiveHTMLArtifact(t *testing.T) {
	vaultDir := t.TempDir()
	artDir := t.TempDir()
	now := time.Now()
	if err := kernel.SetFrontDesk(vaultDir, "fd-console-1", artDir); err != nil {
		t.Fatal(err)
	}
	// Seed legacy hq_console_live.html and living_spaces/hq-console/ from older builds
	legacyLive := filepath.Join(artDir, "hq_console_live.html")
	legacyLiveMeta := filepath.Join(artDir, "hq_console_live.html.metadata.json")
	legacySpaceDir := living.SpaceDir(vaultDir, "hq-console")
	_ = os.MkdirAll(legacySpaceDir, 0o755)
	_ = os.WriteFile(legacyLive, []byte("<html>legacy</html>"), 0o644)
	_ = os.WriteFile(legacyLiveMeta, []byte(`{"userFacing":true}`), 0o644)
	_ = os.WriteFile(filepath.Join(legacySpaceDir, "index.html"), []byte("<html>legacy</html>"), 0o644)

	if _, err := approvals.CreateWithMetadata(vaultDir, "messaging", "Send Q4 summary", "hq gchat send --to=ops", "", "fd-console-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := outbox.Add(vaultDir, outbox.OutboxItem{
		ID:          "dec-console-1",
		Tier:        outbox.TierInterrupt,
		SourceSkill: "pulse",
		Scope:       "engineering",
		Title:       "Prep for 1:1",
		Summary:     "Meeting starts in 20m",
		Decisions: []outbox.DecisionOption{
			{ID: "open", Label: "Open Prep", Kind: outbox.ActionDismissHandled, Recommended: true},
		},
	}, now); err != nil {
		t.Fatal(err)
	}

	if err := SyncMarkdownConsole(vaultDir); err != nil {
		t.Fatalf("SyncMarkdownConsole: %v", err)
	}

	// Verify hq_console.md + metadata exist and legacy hq_console_live.html / hq-console space are cleaned up
	if _, err := os.Stat(filepath.Join(artDir, "hq_console.md")); err != nil {
		t.Fatalf("expected hq_console.md in Front Desk artifact dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artDir, "hq_console.md.metadata.json")); err != nil {
		t.Fatalf("expected hq_console.md.metadata.json: %v", err)
	}
	for _, removed := range []string{legacyLive, legacyLiveMeta, legacySpaceDir} {
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Fatalf("expected legacy %s to be retired by SyncMarkdownConsole", removed)
		}
	}
	mdBytes, _ := os.ReadFile(filepath.Join(vaultDir, "CONSOLE.md"))
	mdStr := string(mdBytes)
	if !strings.Contains(mdStr, "sidecar://agy-hq-console/") || strings.Contains(mdStr, "hq_console_live.html") {
		t.Fatalf("expected CONSOLE.md to link Sidecar console without legacy hq_console_live.html, got:\n%s", mdStr)
	}
}

func TestConsole4TabSmartDefaultAndTokensUX(t *testing.T) {
	vaultDir := t.TempDir()
	artDir := t.TempDir()
	_ = kernel.SetFrontDesk(vaultDir, "fd-work-default", artDir)
	_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:       "active-only",
		Title:          "[Task:core] Active Engineering Work",
		Kind:           "task",
		ConversationID: "conv-work-only-1",
		Status:         "active",
	})

	if err := SyncMarkdownConsole(vaultDir); err != nil {
		t.Fatalf("SyncMarkdownConsole: %v", err)
	}

	h := NewHandler(vaultDir)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / failed: %d", rec.Code)
	}
	webHTML := rec.Body.String()
	for _, want := range []string{
		`data-default-tab="work"`,
		`data-tab="needs_me"`,
		`data-tab="work"`,
		`data-tab="tokens"`,
		`data-tab="ops"`,
		`data-win="today"`,
		`data-win="7d"`,
		`data-win="30d"`,
		`data-win="all"`,
		`function hqFetch(`,
		`overflow-wrap: anywhere`,
		`512k`,
	} {
		if !strings.Contains(webHTML, want) {
			t.Fatalf("expected %q in Web Console HTML", want)
		}
	}
	for _, banned := range []string{"QBU", "Compute Share", "/ 256k"} {
		if strings.Contains(webHTML, banned) {
			t.Fatalf("expected zero %q jargon in Web Console HTML", banned)
		}
	}
	const bootOpen = `<script id="hq-bootstrap-state" type="application/json">`
	startIdx := strings.Index(webHTML, bootOpen)
	if startIdx < 0 {
		t.Fatalf("missing bootstrap JSON script tag in Web Console HTML")
	}
	rest := webHTML[startIdx+len(bootOpen):]
	endIdx := strings.Index(rest, "</script>")
	if endIdx < 0 {
		t.Fatalf("unterminated bootstrap JSON script tag in Web Console HTML")
	}
	var bootState DashboardState
	if err := json.Unmarshal([]byte(rest[:endIdx]), &bootState); err != nil {
		t.Fatalf("bootstrap JSON must unmarshal directly into DashboardState (not double-encoded string): %v", err)
	}
	if bootState.Observability.Budget.EffectiveContextCeiling() != 512_000 {
		t.Fatalf("expected hydrated bootstrap context ceiling 512000, got %d", bootState.Observability.Budget.EffectiveContextCeiling())
	}
}
