package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/outbox"
)

func TestSyncConsoleLivingSpaceMobileArtifactAndCSPSandbox(t *testing.T) {
	vaultDir := t.TempDir()
	artDir := t.TempDir()
	now := time.Now()

	if err := kernel.SetFrontDesk(vaultDir, "fd-mobile-0001", artDir); err != nil {
		t.Fatal(err)
	}
	prop, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:  "messaging",
		Summary:     "Reply to Alice on mobile launch",
		Command:     "hq gchat send --to=alice@example.com --body=Ship",
		CommandArgv: []string{"hq", "gchat", "send", "--to=alice@example.com", "--body=Ship"},
		Metadata:    map[string]string{"bodyPreview": "Ready to ship mobile console!"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = outbox.Add(vaultDir, outbox.OutboxItem{
		ID:          "dec-mob-1",
		SourceSkill: "pulse",
		Scope:       "engineering",
		Tier:        outbox.TierInterrupt,
		Title:       "PR #88 awaiting review",
		Summary:     "Reviewer commented 20m ago.",
		Decisions:   []outbox.DecisionOption{{ID: "handled", Label: "I'm On It", Kind: outbox.ActionDismissHandled}},
	}, now)
	_ = kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:       "mobile-console",
		Title:          "[Task:eng] Mobile Live Console",
		Kind:           "task",
		ConversationID: "conv-mobile-task-1",
		Status:         "active",
	})

	if err := SyncMarkdownConsole(vaultDir); err != nil {
		t.Fatalf("SyncMarkdownConsole: %v", err)
	}

	// 1. Verify hq_console_live.html + metadata exist in Front Desk ArtifactDir (Remote Control accessible!)
	liveHTMLPath := filepath.Join(artDir, "hq_console_live.html")
	liveBytes, err := os.ReadFile(liveHTMLPath)
	if err != nil {
		t.Fatalf("expected hq_console_live.html in Front Desk artifact dir: %v", err)
	}
	if _, err := os.Stat(liveHTMLPath + ".metadata.json"); err != nil {
		t.Fatalf("expected hq_console_live.html.metadata.json: %v", err)
	}
	htmlStr := string(liveBytes)
	if len(liveBytes) > 45*1024 {
		t.Fatalf("expected portable hq_console_live.html <= 45 KB, got %d bytes", len(liveBytes))
	}
	if strings.Contains(htmlStr, "All Views") {
		t.Fatalf("expected HideAllPerspective to omit 'All Views' pill in hq_console_live.html")
	}
	for _, banned := range []string{"QBU", "Compute Share", "/ 256k"} {
		if strings.Contains(htmlStr, banned) {
			t.Fatalf("expected zero jargon/legacy ceiling %q in hq_console_live.html", banned)
		}
	}
	for _, want := range []string{
		"agy-hq Live Console",
		`"defaultPerspective":"needs_me"`,
		`data-persp="tokens"`,
		"512k",
		"Reply to Alice on mobile launch",
		"hq approve " + prop.ID,
		"hq outbox act dec-mob-1 --option=handled",
		"conversation://conv-mobile-task-1",
		"data-copy=",
	} {
		if !strings.Contains(htmlStr, want) {
			t.Fatalf("expected %q in hq_console_live.html", want)
		}
	}

	// 2. After steady-state sync, a repeat SyncMarkdownConsole skips hq_console_live.html rewrite
	if err := SyncMarkdownConsole(vaultDir); err != nil {
		t.Fatalf("second SyncMarkdownConsole: %v", err)
	}
	st1, _ := os.Stat(liveHTMLPath)
	time.Sleep(15 * time.Millisecond)
	if err := SyncMarkdownConsole(vaultDir); err != nil {
		t.Fatalf("third SyncMarkdownConsole: %v", err)
	}
	st2, _ := os.Stat(liveHTMLPath)
	if st2.ModTime().UnixNano() != st1.ModTime().UnixNano() {
		t.Fatalf("expected unchanged steady-state body to skip hq_console_live.html rewrite")
	}

	// 3. Verify :4242/living-spaces/hq-console serves the live console inside CSP sandbox
	h := NewHandler(vaultDir)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/living-spaces/hq-console/raw", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("expected 200 + strict CSP on /living-spaces/hq-console/raw, got %d csp=%q", rec.Code, rec.Header().Get("Content-Security-Policy"))
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
	if !strings.Contains(string(liveBytes), `"defaultPerspective":"work"`) {
		t.Fatalf("expected defaultPerspective=work when 0 pending actions and >=1 active task")
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
}
