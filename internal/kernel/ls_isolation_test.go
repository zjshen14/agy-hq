package kernel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/authz"
)

func TestSandboxedTempVaultIsolatesInheritedHostLSEnv(t *testing.T) {
	var hostLSHits atomic.Int32
	hostLS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostLSHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cascadeId":"99999999-9999-4999-8999-999999999999"}`))
	}))
	defer hostLS.Close()

	sandboxRoot := t.TempDir()
	fakeHome := filepath.Join(sandboxRoot, "home")
	vaultDir := filepath.Join(sandboxRoot, "vault")
	fakeAppData := filepath.Join(sandboxRoot, "appdata")
	if err := os.MkdirAll(filepath.Join(vaultDir, "inbox"), 0o755); err != nil {
		t.Fatalf("mkdir inbox: %v", err)
	}
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatalf("mkdir fakeHome: %v", err)
	}
	if err := os.MkdirAll(fakeAppData, 0o755); err != nil {
		t.Fatalf("mkdir fakeAppData: %v", err)
	}

	// Simulate a screenshot / demo script running under /tmp with overridden HOME, HQ_VAULT_DIR,
	// and ANTIGRAVITY_APP_DATA_DIR while inheriting live host IDE LS credentials.
	t.Setenv("HOME", fakeHome)
	t.Setenv("HQ_HOME_DIR", "")
	t.Setenv("HQ_VAULT_DIR", vaultDir)
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", fakeAppData)
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", strings.TrimPrefix(hostLS.URL, "http://"))
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "host-ide-csrf-secret")
	t.Setenv("ANTIGRAVITY_AGENTAPI_EXE", "/usr/local/bin/language_server")
	t.Setenv("ANTIGRAVITY_PLUGIN_SHIM_DIR", sandboxRoot)
	t.Setenv("ANTIGRAVITY_CREDENTIAL_SCOPE", "host-scope")
	t.Setenv("HQ_ALLOW_LIVE_LS_DISCOVERY", "")
	t.Setenv("HQ_DISABLE_LS", "")

	// 1. Isolation reason must trigger fail-closed.
	if reason := LSEnvIsolationReason(vaultDir); reason == "" {
		t.Fatalf("expected non-empty LSEnvIsolationReason for temp sandbox vault")
	}

	// 2. RecordLiveLSSession must NOT persist inherited host LS credentials into temp vault.
	if err := RecordLiveLSSession(vaultDir); err != nil {
		t.Fatalf("RecordLiveLSSession: %v", err)
	}
	if _, err := os.Stat(lsSessionFilePath(vaultDir)); !os.IsNotExist(err) {
		t.Fatalf("expected ls_session.json NOT to be created in isolated temp vault, err=%v", err)
	}

	// 3. HasReachableLSSession and DiagnoseLSSession must report isolated / none.
	if HasReachableLSSession(vaultDir) {
		t.Fatalf("expected HasReachableLSSession=false in isolated temp vault")
	}
	diag := DiagnoseLSSession(vaultDir)
	if !diag.EnvIsolated || diag.IsolationReason == "" || diag.EffectiveSource != "none" || diag.EnvReachable {
		t.Fatalf("expected isolated LS diagnostics with EffectiveSource=none, got %+v", diag)
	}

	// 4. Drop a synthetic demo filing into inbox/ and run full heartbeat + spawn + delivery + sidebar sync:
	// ZERO requests may ever reach hostLS!
	if err := os.WriteFile(filepath.Join(vaultDir, "inbox", "acme-10q-sample.md"), []byte("# Sample 10-Q\n"), 0o644); err != nil {
		t.Fatalf("write sample inbox file: %v", err)
	}
	if _, err := RunHeartbeat(vaultDir, false); err != nil {
		t.Fatalf("RunHeartbeat: %v", err)
	}
	if _, err := SpawnTask(vaultDir, "gatekeeper:demo", "inbox-triage", "[Auto] Demo", "Triage", "gemini-4-hydrogen-medium", false); err == nil {
		t.Fatalf("expected SpawnTask to fail closed in isolated sandbox vault")
	}
	if _, err := SpawnSubagent(vaultDir, "00000000-0000-4000-8000-000000000001", "[standard] demo: Worker", "Do work", "gemini-4-hydrogen-medium", "", false); err == nil {
		t.Fatalf("expected SpawnSubagent to fail closed in isolated sandbox vault")
	}
	if _, err := DeliverMessageToSession(vaultDir, "00000000-0000-4000-8000-000000000001", "Test", "Hello", false); err == nil {
		t.Fatalf("expected DeliverMessageToSession to fail closed in isolated sandbox vault")
	}
	_ = SyncConversationPermissionPreset(vaultDir, "00000000-0000-4000-8000-000000000001", authz.HostPresetRequestReview)
	_ = EnqueueSidebarSync(vaultDir, SidebarSyncQueueItem{
		ConversationID:  "00000000-0000-4000-8000-000000000002",
		TaskSlug:        "demo-task",
		DesiredArchived: true,
		NextRetryAt:     time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339),
	}, time.Now().Add(-1*time.Minute))
	_, _ = DrainSidebarSyncQueue(vaultDir, time.Second)

	if hits := hostLSHits.Load(); hits != 0 {
		t.Fatalf("ISOLATION LEAK: sandboxed temp vault sent %d HTTP request(s) to inherited host LS!", hits)
	}

	// 5. configureSpawnCommandWithTarget must scrub inherited host LS & plugin scope env vars.
	cmd := exec.Command("true")
	configureSpawnCommandWithTarget(cmd, vaultDir, "gatekeeper:demo", vaultDir, "")
	for _, kv := range cmd.Env {
		for _, forbidden := range []string{
			"ANTIGRAVITY_LS_ADDRESS=",
			"ANTIGRAVITY_CSRF_TOKEN=",
			"ANTIGRAVITY_AGENTAPI_EXE=",
			"ANTIGRAVITY_PLUGIN_SHIM_DIR=",
			"ANTIGRAVITY_CREDENTIAL_SCOPE=",
		} {
			if strings.HasPrefix(kv, forbidden) {
				t.Fatalf("expected %s to be scrubbed from isolated spawn env, got %q", forbidden, kv)
			}
		}
	}
}

func TestSandboxedVaultAllowsMatchingPersistedSessionTempHubAndExplicitOptIn(t *testing.T) {
	sandboxLS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer sandboxLS.Close()

	u, err := url.Parse(sandboxLS.URL)
	if err != nil {
		t.Fatalf("parse sandboxLS URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	sandboxRoot := t.TempDir()
	vaultDir := filepath.Join(sandboxRoot, "vault")
	appDataA := filepath.Join(sandboxRoot, "appdata-a")
	appDataB := filepath.Join(sandboxRoot, "appdata-b")
	if err := os.MkdirAll(filepath.Join(vaultDir, "private", "runtime"), 0o700); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}

	t.Setenv("HQ_VAULT_DIR", vaultDir)
	t.Setenv("HQ_ALLOW_LIVE_LS_DISCOVERY", "")
	t.Setenv("HQ_DISABLE_LS", "")
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", u.Host)
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-sandbox")

	// 1. Explicit persisted ls_session.json bound to appDataA works when ANTIGRAVITY_APP_DATA_DIR matches appDataA.
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", appDataA)
	sessBytes, _ := json.Marshal(lsCredentials{
		RawAddress: u.Host,
		BaseURL:    sandboxLS.URL,
		CSRFToken:  "csrf-sandbox",
		AppDataDir: appDataA,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
	})
	if err := os.WriteFile(lsSessionFilePath(vaultDir), sessBytes, 0o600); err != nil {
		t.Fatalf("write ls_session.json: %v", err)
	}
	if !HasReachableLSSession(vaultDir) {
		t.Fatalf("expected matching persisted ls_session.json to be reachable")
	}
	if diag := DiagnoseLSSession(vaultDir); diag.EffectiveSource != "persisted" {
		t.Fatalf("expected EffectiveSource=persisted, got %+v", diag)
	}

	// 2. Overriding ANTIGRAVITY_APP_DATA_DIR to a different directory (appDataB) must reject the mismatched persisted session.
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", appDataB)
	if HasReachableLSSession(vaultDir) {
		t.Fatalf("expected mismatched ANTIGRAVITY_APP_DATA_DIR to reject persisted ls_session.json")
	}
	if diag := DiagnoseLSSession(vaultDir); diag.PersistedReason != "app_data_dir_mismatch" || diag.EffectiveSource != "none" {
		t.Fatalf("expected app_data_dir_mismatch and EffectiveSource=none, got %+v", diag)
	}

	// 3. A dedicated daemon/hub_*.json inside the sandbox's own temp appDataB IS discovered cleanly.
	hubDir := filepath.Join(appDataB, "daemon")
	if err := os.MkdirAll(hubDir, 0o700); err != nil {
		t.Fatalf("mkdir hubDir: %v", err)
	}
	hubJSON, _ := json.Marshal(map[string]interface{}{
		"pid":       os.Getpid(),
		"httpPort":  port,
		"csrfToken": "csrf-temp-hub",
	})
	if err := os.WriteFile(filepath.Join(hubDir, "hub_sandbox.json"), hubJSON, 0o600); err != nil {
		t.Fatalf("write hub_sandbox.json: %v", err)
	}
	if creds, ok := discoverLSCredentials(vaultDir); !ok || creds.CSRFToken != "csrf-temp-hub" {
		t.Fatalf("expected temp appDataB hub file to be discovered, got ok=%v creds=%+v", ok, creds)
	}

	// 4. HQ_DISABLE_LS=1 unconditionally disables env, persisted session, and temp hub even with HQ_ALLOW_LIVE_LS_DISCOVERY=1.
	t.Setenv("HQ_ALLOW_LIVE_LS_DISCOVERY", "1")
	t.Setenv("HQ_DISABLE_LS", "1")
	if HasReachableLSSession(vaultDir) {
		t.Fatalf("expected HQ_DISABLE_LS=1 to block all LS discovery")
	}
	if diag := DiagnoseLSSession(vaultDir); diag.IsolationReason != "HQ_DISABLE_LS=1" || diag.EffectiveSource != "none" {
		t.Fatalf("expected HQ_DISABLE_LS=1 diagnostics, got %+v", diag)
	}

	// 5. Removing HQ_DISABLE_LS while keeping HQ_ALLOW_LIVE_LS_DISCOVERY=1 opts back in to env LS.
	t.Setenv("HQ_DISABLE_LS", "")
	if reason := LSEnvIsolationReason(vaultDir); reason != "" {
		t.Fatalf("expected empty isolation reason with HQ_ALLOW_LIVE_LS_DISCOVERY=1, got %q", reason)
	}
	if diag := DiagnoseLSSession(vaultDir); diag.EffectiveSource != "env" {
		t.Fatalf("expected EffectiveSource=env with HQ_ALLOW_LIVE_LS_DISCOVERY=1, got %+v", diag)
	}
}
