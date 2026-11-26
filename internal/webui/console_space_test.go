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

func TestSyncConsoleLiveArtifactUnifiedTemplateAndCSPSandbox(t *testing.T) {
	vaultDir := t.TempDir()
	artDir := t.TempDir()
	now := time.Now()
	if err := kernel.SetFrontDesk(vaultDir, "fd-console-1", artDir); err != nil {
		t.Fatal(err)
	}
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

	// 1. Verify hq_console_live.html + metadata exist in Front Desk ArtifactDir (Mobile Artifact Pane ready!)
	liveHTMLPath := filepath.Join(artDir, "hq_console_live.html")
	liveBytes, err := os.ReadFile(liveHTMLPath)
	if err != nil {
		t.Fatalf("expected hq_console_live.html in Front Desk artifact dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artDir, "hq_console_live.html.metadata.json")); err != nil {
		t.Fatalf("expected hq_console_live.html.metadata.json: %v", err)
	}
	liveHTML := string(liveBytes)
	for _, want := range []string{
		`data-static-mode="1"`,
		`data-default-tab="needs_me"`,
		`data-tab="needs_me"`,
		`data-tab="work"`,
		`data-tab="tokens"`,
		`data-tab="ops"`,
		`data-win="30d"`,
		`function copyCommandText(`,
		`overflow-wrap: anywhere`,
		`SNAPSHOT`,
		`512k`,
	} {
		if !strings.Contains(liveHTML, want) {
			t.Fatalf("expected %q in unified hq_console_live.html", want)
		}
	}
	for _, banned := range []string{"/preload.js", "/favicon.svg", "QBU", "Compute Share", "/ 256k"} {
		if strings.Contains(liveHTML, banned) {
			t.Fatalf("expected zero %q in offline static hq_console_live.html", banned)
		}
	}

	// Verify bootstrap JSON in static HTML parses cleanly (no double-encoding)
	const bootOpen = `<script id="hq-bootstrap-state" type="application/json">`
	startIdx := strings.Index(liveHTML, bootOpen)
	if startIdx < 0 {
		t.Fatalf("missing bootstrap JSON script tag in hq_console_live.html")
	}
	rest := liveHTML[startIdx+len(bootOpen):]
	endIdx := strings.Index(rest, "</script>")
	if endIdx < 0 {
		t.Fatalf("unterminated bootstrap JSON script tag in hq_console_live.html")
	}
	var bootPayload struct {
		Observability struct {
			Budget struct {
				ContextCeilingTokens int64 `json:"contextCeilingTokens"`
			} `json:"budget"`
		} `json:"observability"`
	}
	if err := json.Unmarshal([]byte(rest[:endIdx]), &bootPayload); err != nil {
		t.Fatalf("static hq_console_live.html bootstrap JSON must parse directly: %v", err)
	}
	if bootPayload.Observability.Budget.ContextCeilingTokens != 512_000 {
		t.Fatalf("expected 512000 context ceiling in static bootstrap JSON, got %d", bootPayload.Observability.Budget.ContextCeilingTokens)
	}

	// 2. After steady-state sync (Check #4 settled to PASS), a repeat SyncMarkdownConsole skips hq_console_live.html rewrite
	if _, err := os.Stat(filepath.Join(living.SpaceDir(vaultDir, ConsoleLivingSpaceSlug), "index.html")); err != nil {
		t.Fatalf("expected living_spaces/hq-console/index.html: %v", err)
	}
	if err := SyncMarkdownConsole(vaultDir); err != nil {
		t.Fatalf("steady-state warm SyncMarkdownConsole: %v", err)
	}
	oldTS := time.Now().Add(-2 * time.Minute)
	_ = os.Chtimes(liveHTMLPath, oldTS, oldTS)
	if err := SyncMarkdownConsole(vaultDir); err != nil {
		t.Fatalf("repeat SyncMarkdownConsole: %v", err)
	}
	stAfter, err := os.Stat(liveHTMLPath)
	if err != nil {
		t.Fatal(err)
	}
	if !stAfter.ModTime().Equal(oldTS.Truncate(time.Second)) && stAfter.ModTime().Sub(oldTS) > time.Second {
		t.Fatalf("expected unchanged steady-state body to skip hq_console_live.html rewrite")
	}

	// 3. Verify /living-spaces/hq-console/raw serves 200 OK with strict default-src 'none' CSP
	h := NewHandler(vaultDir)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/living-spaces/hq-console/raw", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("expected 200 + strict CSP on /living-spaces/hq-console/raw, got %d csp=%q", rec.Code, rec.Header().Get("Content-Security-Policy"))
	}
	if !strings.Contains(rec.Body.String(), `data-static-mode="1"`) {
		t.Fatalf("expected /living-spaces/hq-console/raw to serve unified static console.html")
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
	liveBytes, err := os.ReadFile(filepath.Join(artDir, "hq_console_live.html"))
	if err != nil {
		t.Fatalf("read hq_console_live.html: %v", err)
	}
	if !strings.Contains(string(liveBytes), `data-default-tab="work"`) {
		t.Fatalf("expected default tab 'work' in hq_console_live.html when 0 pending actions and >=1 active task")
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
		`/preload.js`,
		`512k`,
	} {
		if !strings.Contains(webHTML, want) {
			t.Fatalf("expected %q in Web Console HTML", want)
		}
	}
	if strings.Contains(webHTML, `data-static-mode="1"`) {
		t.Fatalf("live :4242 console must NOT set data-static-mode=1")
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
