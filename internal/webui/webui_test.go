package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/kernel"
)

func TestWebUIServerAndApprovalFlow(t *testing.T) {
	vaultDir := t.TempDir()
	prop, err := approvals.Create(vaultDir, "test_action", "Run echo via UI", "echo ui-approved-ok", "", "conv-ui")
	if err != nil {
		t.Fatalf("Create proposal: %v", err)
	}

	h := NewHandler(vaultDir)

	// 1. GET / should render the pending proposal and preload.js inclusion
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Run echo via UI") {
		t.Fatalf("unexpected GET / response (%d): %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/preload.js") {
		t.Errorf("expected GET / to include /preload.js script")
	}

	// 2. GET /preload.js should return JavaScript bridge
	reqPreload := httptest.NewRequest(http.MethodGet, "/preload.js", nil)
	recPreload := httptest.NewRecorder()
	h.ServeHTTP(recPreload, reqPreload)
	if recPreload.Code != http.StatusOK || !strings.Contains(recPreload.Body.String(), "window.sidecar") {
		t.Fatalf("unexpected GET /preload.js response (%d): %s", recPreload.Code, recPreload.Body.String())
	}

	// 3. GET /api/state should return valid JSON state with pending proposal
	reqState := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	recState := httptest.NewRecorder()
	h.ServeHTTP(recState, reqState)
	if recState.Code != http.StatusOK {
		t.Fatalf("unexpected GET /api/state status (%d)", recState.Code)
	}
	var state DashboardState
	if err := json.Unmarshal(recState.Body.Bytes(), &state); err != nil {
		t.Fatalf("failed to unmarshal /api/state: %v", err)
	}
	if len(state.Pending) != 1 || state.Pending[0].ID != prop.ID {
		t.Fatalf("expected 1 pending proposal in /api/state, got: %+v", state.Pending)
	}

	// 4. POST /api/approve with matching SHA-256 prefix via form should execute and clear the queue
	form := url.Values{}
	form.Set("id", prop.ID)
	form.Set("sha256", prop.SHA256[:12])
	reqApprove := httptest.NewRequest(http.MethodPost, "/api/approve", strings.NewReader(form.Encode()))
	reqApprove.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqApprove.Header.Set("Accept", "application/json")
	recApprove := httptest.NewRecorder()
	h.ServeHTTP(recApprove, reqApprove)
	if recApprove.Code != http.StatusOK || !strings.Contains(recApprove.Body.String(), "ui-approved-ok") {
		t.Fatalf("unexpected POST /api/approve response (%d): %s", recApprove.Code, recApprove.Body.String())
	}

	// 5. Create second proposal and test JSON-based reject
	prop2, err := approvals.Create(vaultDir, "test_action_2", "Second action to reject", "echo reject-me", "", "conv-ui-2")
	if err != nil {
		t.Fatalf("Create proposal 2: %v", err)
	}

	jsonReject := `{"id":"` + prop2.ID + `","reason":"User clicked Reject in UI"}`
	reqReject := httptest.NewRequest(http.MethodPost, "/api/reject", strings.NewReader(jsonReject))
	reqReject.Header.Set("Content-Type", "application/json")
	reqReject.Header.Set("Accept", "application/json")
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
