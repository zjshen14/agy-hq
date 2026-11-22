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
	"github.com/zjshen14/agy-hq/internal/living"
	"github.com/zjshen14/agy-hq/internal/outbox"
)

func TestLivingSpacesCSPSandboxOutboxActAndApprovalsRestageHTTP(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Now()

	// Seed a Living Space
	_, err := living.Save(vaultDir, living.SpaceDoc{
		Slug:  "sprint-cockpit",
		Title: living.Text("Sprint Cockpit"),
		Blocks: []living.BlockSpec{
			{ID: "n1", Type: living.BlockCalloutNotes, Notes: []living.CalloutNote{{Title: living.Text("Goal"), Body: living.Text("Ship RFC-0027")}}},
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	// Seed an expired proposal + a decision card
	prop, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:  "messaging",
		Summary:     "Ping reviewer on PR #12",
		Command:     "hq gchat send --to=alice@example.com --body=Ping",
		CommandArgv: []string{"hq", "gchat", "send", "--to=alice@example.com", "--body=Ping"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = approvals.ExpireStaleProposals(vaultDir, now.Add(48*time.Hour))

	decItem, _, err := outbox.Add(vaultDir, outbox.OutboxItem{
		ID:          "dec-http-1",
		SourceSkill: "pulse",
		Scope:       "engineering",
		Tier:        outbox.TierInterrupt,
		Title:       "Workspace needs review",
		Summary:     "PR #12 waiting on owner",
		Decisions: []outbox.DecisionOption{
			{ID: "handled", Label: "I'm On It", Kind: outbox.ActionDismissHandled, Recommended: true},
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	h := NewHandler(vaultDir)

	// 1. Verify /living-spaces/sprint-cockpit serves <iframe sandbox="allow-scripts"> WITHOUT allow-same-origin
	reqWrap := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/living-spaces/sprint-cockpit", nil)
	recWrap := httptest.NewRecorder()
	h.ServeHTTP(recWrap, reqWrap)
	if recWrap.Code != http.StatusOK {
		t.Fatalf("GET /living-spaces/sprint-cockpit failed: %d", recWrap.Code)
	}
	wrapBody := recWrap.Body.String()
	if !strings.Contains(wrapBody, `sandbox="allow-scripts"`) || strings.Contains(wrapBody, "allow-same-origin") {
		t.Fatalf("expected strict iframe sandbox without allow-same-origin, got:\n%s", wrapBody)
	}

	// 2. Verify /living-spaces/sprint-cockpit/raw sends strict default-src 'none' CSP
	reqRaw := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/living-spaces/sprint-cockpit/raw", nil)
	recRaw := httptest.NewRecorder()
	h.ServeHTTP(recRaw, reqRaw)
	if recRaw.Code != http.StatusOK {
		t.Fatalf("GET /living-spaces/sprint-cockpit/raw failed: %d", recRaw.Code)
	}
	csp := recRaw.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(recRaw.Body.String(), "Sprint Cockpit") {
		t.Fatalf("unexpected raw response csp=%q body=%s", csp, recRaw.Body.String())
	}

	// 3. Fetch main console HTML -> extract CSRF -> POST /api/outbox/act & /api/approvals/restage
	reqHome := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/", nil)
	recHome := httptest.NewRecorder()
	h.ServeHTTP(recHome, reqHome)
	if recHome.Code != http.StatusOK {
		t.Fatalf("GET / failed: %d", recHome.Code)
	}
	csrf := extractTestCSRFToken(t, recHome.Body.String())

	reqAct := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4242/api/outbox/act", strings.NewReader(`{"id":"`+decItem.ID+`","option":"handled"}`))
	reqAct.Header.Set("Content-Type", "application/json")
	reqAct.Header.Set("X-HQ-CSRF-Token", csrf)
	recAct := httptest.NewRecorder()
	h.ServeHTTP(recAct, reqAct)
	if recAct.Code != http.StatusOK || !strings.Contains(recAct.Body.String(), `"ok":true`) {
		t.Fatalf("POST /api/outbox/act failed (%d): %s", recAct.Code, recAct.Body.String())
	}

	reqRestage := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4242/api/approvals/restage", strings.NewReader(`{"id":"`+prop.ID+`"}`))
	reqRestage.Header.Set("Content-Type", "application/json")
	reqRestage.Header.Set("X-HQ-CSRF-Token", csrf)
	recRestage := httptest.NewRecorder()
	h.ServeHTTP(recRestage, reqRestage)
	if recRestage.Code != http.StatusOK || !strings.Contains(recRestage.Body.String(), `"ok":true`) {
		t.Fatalf("POST /api/approvals/restage failed (%d): %s", recRestage.Code, recRestage.Body.String())
	}

	// 4. Verify CONSOLE.md includes Living Spaces + collapsible Ops Inspector
	if err := SyncMarkdownConsole(vaultDir); err != nil {
		t.Fatal(err)
	}
	mdBytes, _ := os.ReadFile(filepath.Join(vaultDir, "CONSOLE.md"))
	md := string(mdBytes)
	if !strings.Contains(md, "Sprint Cockpit") || !strings.Contains(md, "Ops & Diagnostics Inspector") {
		t.Fatalf("expected Living Space link and Ops Inspector in CONSOLE.md:\n%s", md)
	}
}
