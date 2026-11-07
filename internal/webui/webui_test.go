package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zjshen14/agy-hq/internal/approvals"
)

func TestWebUIServerAndApprovalFlow(t *testing.T) {
	vaultDir := t.TempDir()
	prop, err := approvals.Create(vaultDir, "test_action", "Run echo via UI", "echo ui-approved-ok", "", "conv-ui")
	if err != nil {
		t.Fatalf("Create proposal: %v", err)
	}

	h := NewHandler(vaultDir)

	// 1. GET / should render the pending proposal
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Run echo via UI") {
		t.Fatalf("unexpected GET / response (%d): %s", rec.Code, rec.Body.String())
	}

	// 2. POST /api/approve with matching SHA-256 prefix should execute and clear the queue
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
}
