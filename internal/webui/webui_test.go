package webui

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/kernel"
)

var reCSRFMeta = regexp.MustCompile(`<meta name="hq-csrf-token" content="([0-9a-f]+)">`)

func extractTestCSRFToken(t *testing.T, html string) string {
	t.Helper()
	m := reCSRFMeta.FindStringSubmatch(html)
	if len(m) != 2 || len(m[1]) < 16 {
		t.Fatalf("failed to extract hq-csrf-token meta tag from HTML")
	}
	return m[1]
}

func TestWebUIServerAndApprovalFlow(t *testing.T) {
	vaultDir := t.TempDir()
	prop, err := approvals.Create(vaultDir, "test_action", "Run echo via UI", "echo ui-approved-ok", "", "conv-ui")
	if err != nil {
		t.Fatalf("Create proposal: %v", err)
	}

	h := NewHandler(vaultDir)

	// 1. GET / should render the pending proposal, preload.js inclusion, and CSRF meta token
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Run echo via UI") {
		t.Fatalf("unexpected GET / response (%d): %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/preload.js") {
		t.Errorf("expected GET / to include /preload.js script")
	}
	csrfToken := extractTestCSRFToken(t, rec.Body.String())

	// 2. GET /preload.js should return JavaScript bridge
	reqPreload := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/preload.js", nil)
	recPreload := httptest.NewRecorder()
	h.ServeHTTP(recPreload, reqPreload)
	if recPreload.Code != http.StatusOK || !strings.Contains(recPreload.Body.String(), "window.sidecar") {
		t.Fatalf("unexpected GET /preload.js response (%d): %s", recPreload.Code, recPreload.Body.String())
	}

	// 3. GET /api/state should return valid JSON state with pending proposal (and must NOT leak CSRFToken)
	reqState := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/api/state", nil)
	recState := httptest.NewRecorder()
	h.ServeHTTP(recState, reqState)
	if recState.Code != http.StatusOK {
		t.Fatalf("unexpected GET /api/state status (%d)", recState.Code)
	}
	if strings.Contains(recState.Body.String(), csrfToken) {
		t.Fatalf("/api/state must not expose the CSRF token in JSON")
	}
	var state DashboardState
	if err := json.Unmarshal(recState.Body.Bytes(), &state); err != nil {
		t.Fatalf("failed to unmarshal /api/state: %v", err)
	}
	if len(state.Pending) != 1 || state.Pending[0].ID != prop.ID {
		t.Fatalf("expected 1 pending proposal in /api/state, got: %+v", state.Pending)
	}

	// 3b. Security checks: DNS rebinding Host, foreign Origin, and missing CSRF token must all be rejected (403)
	reqRebind := httptest.NewRequest(http.MethodGet, "http://evil-rebind.example:4242/api/state", nil)
	recRebind := httptest.NewRecorder()
	h.ServeHTTP(recRebind, reqRebind)
	if recRebind.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for DNS rebinding Host header, got %d", recRebind.Code)
	}

	formNoCSRF := url.Values{}
	formNoCSRF.Set("id", prop.ID)
	formNoCSRF.Set("sha256", prop.SHA256[:12])
	reqNoCSRF := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4242/api/approve", strings.NewReader(formNoCSRF.Encode()))
	reqNoCSRF.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recNoCSRF := httptest.NewRecorder()
	h.ServeHTTP(recNoCSRF, reqNoCSRF)
	if recNoCSRF.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when POST /api/approve omits CSRF token, got %d", recNoCSRF.Code)
	}

	reqCrossOrigin := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4242/api/approve", strings.NewReader(formNoCSRF.Encode()))
	reqCrossOrigin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqCrossOrigin.Header.Set("Origin", "https://evil.example.com")
	reqCrossOrigin.Header.Set("X-HQ-CSRF-Token", csrfToken)
	recCrossOrigin := httptest.NewRecorder()
	h.ServeHTTP(recCrossOrigin, reqCrossOrigin)
	if recCrossOrigin.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when Origin is non-loopback, got %d", recCrossOrigin.Code)
	}

	// 4. POST /api/approve with matching SHA-256 prefix & CSRF token via form should execute and clear the queue
	form := url.Values{}
	form.Set("id", prop.ID)
	form.Set("sha256", prop.SHA256[:12])
	form.Set("csrf_token", csrfToken)
	reqApprove := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4242/api/approve", strings.NewReader(form.Encode()))
	reqApprove.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqApprove.Header.Set("Accept", "application/json")
	recApprove := httptest.NewRecorder()
	h.ServeHTTP(recApprove, reqApprove)
	if recApprove.Code != http.StatusOK || !strings.Contains(recApprove.Body.String(), "ui-approved-ok") {
		t.Fatalf("unexpected POST /api/approve response (%d): %s", recApprove.Code, recApprove.Body.String())
	}

	// 5. Create second proposal and test JSON-based reject with X-HQ-CSRF-Token header
	prop2, err := approvals.Create(vaultDir, "test_action_2", "Second action to reject", "echo reject-me", "", "conv-ui-2")
	if err != nil {
		t.Fatalf("Create proposal 2: %v", err)
	}

	jsonReject := `{"id":"` + prop2.ID + `","reason":"User clicked Reject in UI"}`
	reqReject := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4242/api/reject", strings.NewReader(jsonReject))
	reqReject.Header.Set("Content-Type", "application/json")
	reqReject.Header.Set("Accept", "application/json")
	reqReject.Header.Set("X-HQ-CSRF-Token", csrfToken)
	recReject := httptest.NewRecorder()
	h.ServeHTTP(recReject, reqReject)
	if recReject.Code != http.StatusOK {
		t.Fatalf("unexpected POST /api/reject response (%d): %s", recReject.Code, recReject.Body.String())
	}
	var rejResp map[string]interface{}
	if err := json.Unmarshal(recReject.Body.Bytes(), &rejResp); err != nil || rejResp["ok"] != true {
		t.Fatalf("expected JSON reject ok: true, got: %s", recReject.Body.String())
	}

	// Verify pending is now empty
	pendingAfter, _ := approvals.List(vaultDir)
	if len(pendingAfter) != 0 {
		t.Fatalf("expected 0 pending proposals after reject, got %d", len(pendingAfter))
	}
}

func TestSyncMarkdownConsoleHeaderPill(t *testing.T) {
	vaultDir := t.TempDir()
	artDir := filepath.Join(vaultDir, "test_artifact")
	_ = os.MkdirAll(artDir, 0o755)

	// Register Front Desk so it syncs to artDir
	_ = kernel.SetFrontDesk(vaultDir, "conv-test", artDir)

	if err := SyncMarkdownConsole(vaultDir); err != nil {
		t.Fatalf("SyncMarkdownConsole failed: %v", err)
	}

	consolePath := filepath.Join(vaultDir, "CONSOLE.md")
	content, err := os.ReadFile(consolePath)
	if err != nil {
		t.Fatalf("read CONSOLE.md: %v", err)
	}

	str := string(content)
	expectedPill := "[🎛️ Open Interactive Console (Side Pane)](sidecar://agy-hq-console/)"
	if !strings.Contains(str, expectedPill) {
		t.Fatalf("expected CONSOLE.md to contain sidecar header pill %q, got:\n%s", expectedPill, str)
	}

	artConsolePath := filepath.Join(artDir, "hq_console.md")
	artContent, err := os.ReadFile(artConsolePath)
	if err != nil {
		t.Fatalf("read hq_console.md: %v", err)
	}
	if !strings.Contains(string(artContent), expectedPill) {
		t.Fatalf("expected hq_console.md to contain sidecar header pill %q", expectedPill)
	}
}

func TestWebUIDraftPayloadAndMessagePreview(t *testing.T) {
	vaultDir := t.TempDir()

	// 1. Seed a Google Chat draft file in memory/gchat/drafts/
	gchatDraftDir := filepath.Join(vaultDir, "memory", "gchat", "drafts")
	if err := os.MkdirAll(gchatDraftDir, 0o755); err != nil {
		t.Fatal(err)
	}
	draftBody := "Hey Alice, let's review the proposal together!"
	draftJSON := `{
		"draftId": "gchat-draft-test1234",
		"account": "personal",
		"spaceId": "spaces/AAAA_DM_ALICE",
		"spaceName": "Alice",
		"body": "` + draftBody + `"
	}`
	if err := os.WriteFile(filepath.Join(gchatDraftDir, "gchat-draft-test1234.json"), []byte(draftJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. Create a pending proposal referencing the draft
	summary := `Send Google Chat to Alice: "Hey Alice, let's review the proposal together!"`
	cmd := `./bin/hq gchat send-draft --account=personal --draft=gchat-draft-test1234 --hash=1234567890ab`
	meta := map[string]string{
		"draftId":   "gchat-draft-test1234",
		"account":   "personal",
		"spaceId":   "spaces/AAAA_DM_ALICE",
		"spaceName": "Alice",
	}
	prop, err := approvals.CreateWithMetadata(vaultDir, "gchat_send", summary, cmd, vaultDir, "conv-preview-test", meta)
	if err != nil {
		t.Fatalf("CreateWithMetadata failed: %v", err)
	}

	h := NewHandler(vaultDir)

	// 3. GET / should render proposal summary AND the formatted Message Preview blockquote
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected GET / status (%d)", rec.Code)
	}
	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, template.HTMLEscapeString(summary)) {
		t.Fatalf("expected GET / to contain summary %q, got body:\n%s", summary, bodyStr)
	}
	if !strings.Contains(bodyStr, "💬 Message Preview") {
		t.Fatalf("expected GET / to render '💬 Message Preview' header")
	}
	if !strings.Contains(bodyStr, template.HTMLEscapeString(draftBody)) {
		t.Fatalf("expected GET / to render draft body %q", draftBody)
	}

	// 4. GET /api/state should return draftPayload in proposal metadata
	reqState := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/api/state", nil)
	recState := httptest.NewRecorder()
	h.ServeHTTP(recState, reqState)
	if recState.Code != http.StatusOK {
		t.Fatalf("unexpected GET /api/state status (%d)", recState.Code)
	}
	var state DashboardState
	if err := json.Unmarshal(recState.Body.Bytes(), &state); err != nil {
		t.Fatalf("failed to unmarshal state: %v", err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("expected 1 pending proposal, got %d", len(state.Pending))
	}
	if state.Pending[0].Metadata["draftPayload"] != draftBody {
		t.Fatalf("expected draftPayload=%q, got %q", draftBody, state.Pending[0].Metadata["draftPayload"])
	}

	// 5. SyncMarkdownConsole should render summary and formatted blockquote in CONSOLE.md
	if err := SyncMarkdownConsole(vaultDir); err != nil {
		t.Fatalf("SyncMarkdownConsole: %v", err)
	}
	consoleData, err := os.ReadFile(filepath.Join(vaultDir, "CONSOLE.md"))
	if err != nil {
		t.Fatalf("read CONSOLE.md: %v", err)
	}
	consoleStr := string(consoleData)
	if !strings.Contains(consoleStr, prop.ID) || !strings.Contains(consoleStr, summary) {
		t.Fatalf("expected CONSOLE.md to contain proposal ID and summary, got:\n%s", consoleStr)
	}
	if !strings.Contains(consoleStr, "**Message Preview:**") || !strings.Contains(consoleStr, draftBody) {
		t.Fatalf("expected CONSOLE.md to contain '**Message Preview:**' and body, got:\n%s", consoleStr)
	}
}
