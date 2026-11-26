package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/vault"
)

func seedMemoryTestVault(t *testing.T, now time.Time) string {
	t.Helper()
	vaultDir := t.TempDir()
	for _, sub := range []string{"memory/topics", "memory/entities", "private"} {
		if err := os.MkdirAll(filepath.Join(vaultDir, sub), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}

	memMD := "# Active Priorities\n- Ship Memory tab in HQ Console\n- Keep MEMORY.md compact under dual budget\n"
	if err := os.WriteFile(filepath.Join(vaultDir, "MEMORY.md"), []byte(memMD), 0o600); err != nil {
		t.Fatalf("write MEMORY.md: %v", err)
	}

	// 1 pinned global fact, 1 expiring-soon repo fact, 1 already-expired fact
	if _, _, err := vault.AddFactWithOptions(vaultDir, vault.AddFactOptions{
		ID:        "fact-pinned-1",
		Topic:     "preferences",
		Statement: "Prefer dark mode high-contrast dashboards.",
		Verified:  true,
		Pinned:    true,
		Scopes:    []string{"global"},
		Now:       now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("add pinned fact: %v", err)
	}
	if _, _, err := vault.AddFactWithOptions(vaultDir, vault.AddFactOptions{
		ID:        "fact-expiring-2",
		Topic:     "release",
		Statement: "Cut v1.9 release candidate before Friday.",
		Verified:  true,
		Scopes:    []string{"repo:demo-repo"},
		ExpiresAt: now.Add(24 * time.Hour).Format(time.RFC3339),
		Now:       now.Add(-1 * time.Hour),
	}); err != nil {
		t.Fatalf("add expiring fact: %v", err)
	}
	if _, _, err := vault.AddFactWithOptions(vaultDir, vault.AddFactOptions{
		ID:        "fact-expired-3",
		Topic:     "release",
		Statement: "Old sprint item already passed.",
		Verified:  true,
		Scopes:    []string{"domain:ops"},
		ExpiresAt: now.Add(-24 * time.Hour).Format(time.RFC3339),
		Now:       now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("add expired fact: %v", err)
	}

	// 2 topic pages: one with Active Playbook, one without
	topicA := `# Release Engineering
> Summary: Standard release verification checklist.
> Scopes: repo:demo-repo
> Playbook-Owner: release-eng

## Active Playbook
- Run unit tests and pre-push checks before tagging.
`
	topicB := `# General Notes
> Summary: Lightweight architecture notes.
> Scopes: global

Notes body.
`
	if err := os.WriteFile(filepath.Join(vaultDir, "memory", "topics", "release-eng.md"), []byte(topicA), 0o600); err != nil {
		t.Fatalf("write topicA: %v", err)
	}
	if err := os.WriteFile(filepath.Join(vaultDir, "memory", "topics", "general-notes.md"), []byte(topicB), 0o600); err != nil {
		t.Fatalf("write topicB: %v", err)
	}

	// 1 entity dossier
	entMD := `# Ops Channel
> Kind: space
> Spaces: spaces/TEST_OPS_1
> Context: Primary incident response space.

- Escalate P0 issues here first.
`
	if err := os.WriteFile(filepath.Join(vaultDir, "memory", "entities", "ops-channel.md"), []byte(entMD), 0o600); err != nil {
		t.Fatalf("write entity: %v", err)
	}

	// Dream cursor
	if err := vault.AdvanceDreamCursor(vaultDir, "conv-dream-test", now.Add(-6*time.Hour)); err != nil {
		t.Fatalf("advance dream cursor: %v", err)
	}

	// Private profile with sensitive sentinel values that MUST NEVER leak into WebUI / JSON / Static HTML
	if _, err := vault.SetProfileField(vaultDir, "addresses", "home_street", "SENTINEL_PRIVATE_ADDR_742"); err != nil {
		t.Fatalf("set profile address: %v", err)
	}
	if _, err := vault.SetProfileField(vaultDir, "travel", "ktn_code", "SENTINEL_PRIVATE_KTN_9988"); err != nil {
		t.Fatalf("set profile travel: %v", err)
	}

	return vaultDir
}

func assertNoPrivateProfileLeak(t *testing.T, label, body string) {
	t.Helper()
	for _, secret := range []string{
		"SENTINEL_PRIVATE_ADDR_742",
		"SENTINEL_PRIVATE_KTN_9988",
		"home_street",
		"ktn_code",
	} {
		if strings.Contains(body, secret) {
			t.Fatalf("%s leaked private/profile.md sentinel %q", label, secret)
		}
	}
}

func TestBuildMemoryDashboard_CountsAndPrivacyBoundary(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	vaultDir := seedMemoryTestVault(t, now)

	summary := BuildMemoryDashboard(vaultDir, now)

	if summary.MemoryLines != 3 {
		t.Fatalf("MemoryLines = %d, want 3", summary.MemoryLines)
	}
	if summary.OverBudget {
		t.Fatalf("expected OverBudget=false")
	}
	if len(summary.ActivePriorities) != 2 {
		t.Fatalf("ActivePriorities = %v, want 2 lines", summary.ActivePriorities)
	}
	if summary.ActiveFactCount != 2 {
		t.Fatalf("ActiveFactCount = %d, want 2", summary.ActiveFactCount)
	}
	if summary.ExpiredFactCount != 1 {
		t.Fatalf("ExpiredFactCount = %d, want 1", summary.ExpiredFactCount)
	}
	if summary.ExpiringSoonCount != 1 {
		t.Fatalf("ExpiringSoonCount = %d, want 1", summary.ExpiringSoonCount)
	}
	if len(summary.Facts) != 2 || summary.Facts[0].ID != "fact-pinned-1" || !summary.Facts[0].Pinned {
		t.Fatalf("expected pinned fact first, got %+v", summary.Facts)
	}
	if summary.Facts[1].ID != "fact-expiring-2" || !summary.Facts[1].ExpiringSoon {
		t.Fatalf("expected expiring-soon fact second, got %+v", summary.Facts)
	}
	if summary.TopicCount != 2 || summary.PlaybookCount != 1 {
		t.Fatalf("TopicCount=%d PlaybookCount=%d, want 2 and 1", summary.TopicCount, summary.PlaybookCount)
	}
	if summary.Topics[0].Slug != "release-eng" || summary.Topics[0].ActivePlaybook == "" {
		t.Fatalf("expected playbook topic sorted first, got %+v", summary.Topics)
	}
	if summary.EntityCount != 1 || summary.Entities[0].Slug != "ops-channel" {
		t.Fatalf("EntityCount=%d Entities=%+v, want ops-channel", summary.EntityCount, summary.Entities)
	}
	if summary.ProfileFieldCount != 2 || summary.ProfileSectionCount != 2 {
		t.Fatalf("Profile counts = (%d fields, %d sections), want (2, 2)", summary.ProfileFieldCount, summary.ProfileSectionCount)
	}
	if summary.LastDreamAt == "" {
		t.Fatalf("expected LastDreamAt to be populated")
	}

	rawJSON, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	assertNoPrivateProfileLeak(t, "MemoryDashboardSummary JSON", string(rawJSON))
	if strings.Contains(string(rawJSON), vaultDir) {
		t.Fatalf("MemoryDashboardSummary JSON should not leak host vaultDir %q", vaultDir)
	}
}

func TestMemoryTab_HTTP_PruneCompactAndPrivacy(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	vaultDir := seedMemoryTestVault(t, now)
	handler := NewHandler(vaultDir)

	// 1. GET / renders Memory tab, extracts CSRF token, leaks zero private profile fields
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	htmlBody := rec.Body.String()
	for _, want := range []string{
		`data-tab="memory"`,
		`id="panel-memory"`,
		`id="mem-search"`,
		`data-mem-filter="pinned"`,
		`Prefer dark mode high-contrast dashboards.`,
		`Release Engineering`,
		`Ops Channel`,
	} {
		if !strings.Contains(htmlBody, want) {
			t.Fatalf("GET / missing expected Memory tab marker %q", want)
		}
	}
	assertNoPrivateProfileLeak(t, "GET /", htmlBody)

	csrfMatch := regexp.MustCompile(`<meta name="hq-csrf-token" content="([^"]+)">`).FindStringSubmatch(htmlBody)
	if len(csrfMatch) != 2 || csrfMatch[1] == "" {
		t.Fatalf("failed to extract CSRF token from GET /")
	}
	csrfToken := csrfMatch[1]

	// 2. GET /api/state contains memory summary and zero private profile fields
	recState := httptest.NewRecorder()
	handler.ServeHTTP(recState, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/api/state", nil))
	if recState.Code != http.StatusOK {
		t.Fatalf("GET /api/state status = %d, want 200", recState.Code)
	}
	assertNoPrivateProfileLeak(t, "GET /api/state", recState.Body.String())
	var stateResp DashboardState
	if err := json.Unmarshal(recState.Body.Bytes(), &stateResp); err != nil {
		t.Fatalf("unmarshal /api/state: %v", err)
	}
	if stateResp.Memory.ActiveFactCount != 2 {
		t.Fatalf("/api/state memory.activeFactCount = %d, want 2", stateResp.Memory.ActiveFactCount)
	}

	// 3. POST /api/memory/prune-fact rejects missing CSRF token (403)
	badReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4242/api/memory/prune-fact", bytes.NewBufferString(`{"id":"fact-expiring-2"}`))
	badReq.Header.Set("Content-Type", "application/json")
	badRec := httptest.NewRecorder()
	handler.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusForbidden {
		t.Fatalf("POST /api/memory/prune-fact without CSRF status = %d, want 403", badRec.Code)
	}

	// 4. POST /api/memory/prune-fact with valid CSRF soft-expires fact and invalidates cache
	okReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4242/api/memory/prune-fact", bytes.NewBufferString(`{"id":"fact-expiring-2"}`))
	okReq.Header.Set("Content-Type", "application/json")
	okReq.Header.Set("X-HQ-CSRF-Token", csrfToken)
	okRec := httptest.NewRecorder()
	handler.ServeHTTP(okRec, okReq)
	if okRec.Code != http.StatusOK {
		t.Fatalf("POST /api/memory/prune-fact status = %d, want 200: %s", okRec.Code, okRec.Body.String())
	}
	var pruneResp map[string]interface{}
	if err := json.Unmarshal(okRec.Body.Bytes(), &pruneResp); err != nil || pruneResp["ok"] != true {
		t.Fatalf("unexpected prune response: %s", okRec.Body.String())
	}

	// Verify GET /api/state immediately sees 1 active fact
	recAfter := httptest.NewRecorder()
	handler.ServeHTTP(recAfter, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/api/state", nil))
	var stateAfter DashboardState
	if err := json.Unmarshal(recAfter.Body.Bytes(), &stateAfter); err != nil {
		t.Fatalf("unmarshal /api/state after prune: %v", err)
	}
	if stateAfter.Memory.ActiveFactCount != 1 {
		t.Fatalf("after prune, activeFactCount = %d, want 1", stateAfter.Memory.ActiveFactCount)
	}

	// 5. POST /api/memory/compact-facts rejects missing CSRF, succeeds with valid CSRF
	badComp := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4242/api/memory/compact-facts", bytes.NewBufferString(`{}`))
	badComp.Header.Set("Content-Type", "application/json")
	badCompRec := httptest.NewRecorder()
	handler.ServeHTTP(badCompRec, badComp)
	if badCompRec.Code != http.StatusForbidden {
		t.Fatalf("POST /api/memory/compact-facts without CSRF = %d, want 403", badCompRec.Code)
	}

	okComp := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4242/api/memory/compact-facts", bytes.NewBufferString(`{}`))
	okComp.Header.Set("Content-Type", "application/json")
	okComp.Header.Set("X-HQ-CSRF-Token", csrfToken)
	okCompRec := httptest.NewRecorder()
	handler.ServeHTTP(okCompRec, okComp)
	if okCompRec.Code != http.StatusOK {
		t.Fatalf("POST /api/memory/compact-facts status = %d, want 200", okCompRec.Code)
	}
	var compResp map[string]interface{}
	if err := json.Unmarshal(okCompRec.Body.Bytes(), &compResp); err != nil || compResp["ok"] != true {
		t.Fatalf("unexpected compact response: %s", okCompRec.Body.String())
	}
}

func TestMemoryTab_StaticModeSnapshot(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	vaultDir := seedMemoryTestVault(t, now)

	rawHTML, err := RenderStaticConsoleHTML(loadState(vaultDir, ""))
	if err != nil {
		t.Fatalf("RenderStaticConsoleHTML: %v", err)
	}
	html := string(rawHTML)
	assertNoPrivateProfileLeak(t, "StaticMode console HTML", html)

	for _, want := range []string{
		`data-static-mode="1"`,
		`data-tab="memory"`,
		`hq memory prune-fact `,
		`hq memory compact`,
		`hq memory profile get`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("StaticMode HTML missing %q", want)
		}
	}

	// Verify inline bootstrap JSON parses cleanly and includes memory payload for offline filtering
	re := regexp.MustCompile(`<script id="hq-bootstrap-state" type="application/json">(.*?)</script>`)
	m := re.FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatalf("missing hq-bootstrap-state script block")
	}
	var boot struct {
		Memory MemoryDashboardSummary `json:"memory"`
	}
	if err := json.Unmarshal([]byte(m[1]), &boot); err != nil {
		t.Fatalf("unmarshal bootstrap JSON: %v", err)
	}
	if boot.Memory.ActiveFactCount != 2 || boot.Memory.TopicCount != 2 || boot.Memory.EntityCount != 1 {
		t.Fatalf("unexpected static bootstrap memory payload: %+v", boot.Memory)
	}
}
