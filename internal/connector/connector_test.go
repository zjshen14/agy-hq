package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/extension"
)

func TestManifestAndInstanceStrictValidation(t *testing.T) {
	// 1. Embedded reference manifests must lint cleanly
	vaultDir := t.TempDir()
	for _, id := range []string{"gmail", "gchat", "gcalendar"} {
		rc, err := Resolve(vaultDir, id)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", id, err)
		}
		if !rc.EffectiveParticipantLock(vaultDir) {
			t.Fatalf("expected %s ParticipantLock default true", id)
		}
	}

	// 2. Reject unknown fields or bare {{param}} in connector.json
	_, err := LintManifest([]byte(`{
  "schema_version": 1,
  "id": "bad-cli",
  "driver": {"builtin": "jsonmap"},
  "transport": {"kind": "cli", "cli": {"path": "/usr/bin/tool", "verbs": {"poll": ["list", "{{cursor}}"]}}}
}`), "vault")
	if err == nil || !strings.Contains(err.Error(), "fused-flag") {
		t.Fatalf("expected bare {{cursor}} rejection, got %v", err)
	}

	// 3. Reject shell basenames with {{param}}
	_, err = LintManifest([]byte(`{
  "schema_version": 1,
  "id": "bad-sh",
  "driver": {"builtin": "jsonmap"},
  "transport": {"kind": "cli", "cli": {"path": "/bin/bash", "verbs": {"poll": ["-c", "--cmd={{cursor}}"]}}}
}`), "vault")
	if err == nil || !strings.Contains(err.Error(), "forbidden shell") {
		t.Fatalf("expected /bin/bash rejection, got %v", err)
	}

	// 4. Reject executable keys in instance.json
	_, err = ParseInstanceStrict([]byte(`{"schema_version":1,"connector":"gmail","path":"/tmp/evil","accounts":{}}`))
	if err == nil {
		t.Fatal("expected ParseInstanceStrict to reject executable key 'path'")
	}

	// 5. ExpandVerbArgv rejects leading '-' and '@'
	tmpl := []string{"send", "--to={{to}}", "--cc={{?cc}}", "--body-file={{payload_file}}"}
	if _, err := ExpandVerbArgv(tmpl, map[string]string{"to": "-inject"}, nil, "/dev/fd/3"); err == nil {
		t.Fatal("expected leading '-' rejection")
	}
	if _, err := ExpandVerbArgv(tmpl, map[string]string{"to": "@etc/passwd"}, nil, "/dev/fd/3"); err == nil {
		t.Fatal("expected leading '@' rejection")
	}
	argv, err := ExpandVerbArgv(tmpl, map[string]string{"to": "alice@example.com"}, nil, "/dev/fd/3")
	if err != nil || len(argv) != 3 || argv[1] != "--to=alice@example.com" || argv[2] != "--body-file=/dev/fd/3" {
		t.Fatalf("unexpected ExpandVerbArgv result: argv=%v err=%v", argv, err)
	}
}

func TestJSONMapPointerAndNDJSON(t *testing.T) {
	ndjson := `{"id":"CL-101","project":"core~0net/api","owner":{"email":"  Alice@Acme.com "},"created":1731600000,"status":"NEW"}
{"id":"CL-102","project":"core~0net/api","owner":{"email":"bob@acme.com"},"created":1731600100,"status":"MERGED"}
{"type":"stats","rowCount":2}
`
	spec := JSONMapSpec{
		ItemID:        "/id",
		ThreadKey:     "/id",
		Sender:        "/owner/email",
		SubjectOrRoom: "/project",
		Timestamp:     "/created",
		Transforms: map[string][]string{
			"sender":    {"trim", "lower"},
			"timestamp": {"rfc3339"},
		},
		Predicates: []PredicateSpec{
			{Pointer: "/status", Op: "eq", Value: "NEW"},
		},
	}
	items, _, _, err := MapCLIOutput([]byte(ndjson), spec)
	if err != nil {
		t.Fatalf("MapCLIOutput NDJSON: %v", err)
	}
	if len(items) != 1 || items[0].ItemID != "CL-101" || items[0].Sender != "alice@acme.com" || !strings.HasSuffix(items[0].Timestamp, "Z") {
		t.Fatalf("unexpected NDJSON mapped items: %+v", items)
	}
}

func TestSanitizedSubprocessEnvPayloadPipeAndRFC5322(t *testing.T) {
	base := []string{
		"PATH=/usr/bin:/bin",
		"ANTIGRAVITY_CSRF_TOKEN=secret-csrf",
		"ANTIGRAVITY_LS_ADDRESS=127.0.0.1:9999",
		"ANTIGRAVITY_CREDENTIAL_SCOPE=scope-xyz",
		"CUSTOM_CORP_VAR=allowed-val",
	}
	env := SanitizedSubprocessEnv(base, []string{"CUSTOM_CORP_VAR", "ANTIGRAVITY_CSRF_TOKEN"}, nil, false)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "ANTIGRAVITY_CSRF_TOKEN") || strings.Contains(joined, "ANTIGRAVITY_LS_ADDRESS") || strings.Contains(joined, "ANTIGRAVITY_CREDENTIAL_SCOPE") {
		t.Fatalf("expected CSRF/LS/SCOPE stripped, got:\n%s", joined)
	}
	if !strings.Contains(joined, "ANTIGRAVITY_PLUGIN_SHIM_DIR=/nonexistent-hq-sentinel") || !strings.Contains(joined, "CUSTOM_CORP_VAR=allowed-val") {
		t.Fatalf("expected sentinel SHIM_DIR and CUSTOM_CORP_VAR, got:\n%s", joined)
	}

	cleaned := SanitizeStderr("Auth failed\n<plugin-card catalog-id=\"gmail\">Install</plugin-card>\nplease re-login")
	if strings.Contains(cleaned, "plugin-card") || !strings.Contains(cleaned, "Auth failed please re-login") {
		t.Fatalf("expected plugin-card stripped, got %q", cleaned)
	}

	// Verify /dev/fd/3 ExtraFiles payload delivery
	script := filepath.Join(t.TempDir(), "read-fd3.sh")
	_ = os.WriteFile(script, []byte("#!/bin/sh\ncat /dev/fd/3\n"), 0o755)
	stdout, _, err := RunSubprocess(context.Background(), SubprocessSpec{
		Binary:       script,
		PayloadBytes: []byte("immutable-payload-123"),
		Timeout:      2 * time.Second,
	})
	if err != nil || string(stdout) != "immutable-payload-123" {
		t.Fatalf("expected /dev/fd/3 payload delivery, got stdout=%q err=%v", string(stdout), err)
	}

	// Verify RFC 5322 display-name smuggling defense
	addrs, err := ParseRFC5322Emails(`"ceo@acme.com" <x@evil.com>`)
	if err != nil || len(addrs) != 1 || addrs[0] != "x@evil.com" {
		t.Fatalf("expected display-name smuggling stripped to x@evil.com, got %v (err=%v)", addrs, err)
	}
}

func TestLegacyMigrationAndDriftCheck(t *testing.T) {
	vaultDir := t.TempDir()
	gmailDir := filepath.Join(vaultDir, "private", "gmail")
	_ = os.MkdirAll(gmailDir, 0o700)
	_ = os.WriteFile(filepath.Join(gmailDir, "accounts.json"), []byte(`{
  "defaultAccount": "work",
  "accounts": {
    "work": {"alias": "work", "email": "user@acme.com", "agyAccount": "user@acme.com", "enabled": true}
  }
}`), 0o600)

	migrated, err := MigrateLegacyAccounts(context.Background(), vaultDir, "gmail")
	if err != nil || len(migrated) != 1 {
		t.Fatalf("MigrateLegacyAccounts: migrated=%v err=%v", migrated, err)
	}
	if _, err := os.Stat(filepath.Join(gmailDir, "accounts.json")); err != nil {
		t.Fatalf("expected legacy accounts.json preserved non-destructively: %v", err)
	}
	drift0 := CheckDrift(vaultDir)
	if len(drift0) != 1 || drift0[0].Status != "PASS" {
		t.Fatalf("expected zero drift right after migration, got %+v", drift0)
	}

	// Modify legacy accounts.json to simulate version-skew drift
	_ = os.WriteFile(filepath.Join(gmailDir, "accounts.json"), []byte(`{
  "defaultAccount": "work",
  "accounts": {
    "work": {"alias": "work", "email": "changed@acme.com", "enabled": true}
  }
}`), 0o600)
	drift1 := CheckDrift(vaultDir)
	if len(drift1) != 1 || drift1[0].Status != "WARN" {
		t.Fatalf("expected WARN drift after accounts.json diverged, got %+v", drift1)
	}
}

func TestDKIMAndSPFDomainAlignmentStrictness(t *testing.T) {
	cases := []struct {
		name     string
		from     string
		authRes  string
		isMock   bool
		expected bool
	}{
		{
			name:     "exact header.d match",
			from:     "user@acme.com",
			authRes:  "mx.google.com; dkim=pass header.d=acme.com; spf=fail",
			expected: true,
		},
		{
			name:     "subdomain header.i match",
			from:     "user@acme.com",
			authRes:  "mx.google.com; dkim=pass header.i=@mail.acme.com",
			expected: true,
		},
		{
			name:     "aligned spf smtp.mailfrom and subdomain smtp.helo",
			from:     "alerts@acme.com",
			authRes:  "mx.google.com; dkim=fail; spf=pass smtp.helo=notify.acme.com",
			expected: true,
		},
		{
			name:     "reject lookalike suffix attacker-acme.com",
			from:     "user@acme.com",
			authRes:  "mx.google.com; dkim=pass header.d=attacker-acme.com",
			expected: false,
		},
		{
			name:     "reject cross-clause unaligned pass",
			from:     "user@acme.com",
			authRes:  "mx.acme.com; dkim=fail header.d=acme.com; spf=pass smtp.mailfrom=user@evil.com",
			expected: false,
		},
		{
			name:     "reject bare dkim=pass with no domain tag",
			from:     "user@acme.com",
			authRes:  "mx.google.com; dkim=pass",
			expected: false,
		},
		{
			name:     "empty authResults allowed only in mock mode",
			from:     "user@acme.com",
			authRes:  "",
			isMock:   true,
			expected: true,
		},
		{
			name:     "empty authResults rejected in live mode",
			from:     "user@acme.com",
			authRes:  "",
			isMock:   false,
			expected: false,
		},
	}
	for _, tc := range cases {
		if got := IsDKIMOrSPFAligned(tc.from, tc.authRes, tc.isMock); got != tc.expected {
			t.Fatalf("%s: IsDKIMOrSPFAligned(%q, %q, %v) = %v, want %v", tc.name, tc.from, tc.authRes, tc.isMock, got, tc.expected)
		}
	}
}

func TestEndOfOptionsPositionalExpansionAndGuard(t *testing.T) {
	vaultDir := t.TempDir()
	rc, err := Resolve(vaultDir, "gmail")
	if err != nil {
		t.Fatalf("Resolve(gmail): %v", err)
	}

	argv, err := ExpandVerbArgv(rc.Manifest.Transport.CLI.Verbs["search"], map[string]string{
		"query": "label:INBOX is:unread -category:promotions",
		"max":   "25",
	}, nil, "")
	if err != nil {
		t.Fatalf("ExpandVerbArgv(search): %v", err)
	}
	want := []string{"readonly", "read", "--max=25", "--json", "--", "label:INBOX is:unread -category:promotions"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("search argv = %q, want %q", argv, want)
	}

	threadArgv, err := ExpandVerbArgv(rc.Manifest.Transport.CLI.Verbs["get_thread"], map[string]string{
		"thread_key": "th-abc123",
	}, nil, "")
	if err != nil {
		t.Fatalf("ExpandVerbArgv(get_thread): %v", err)
	}
	wantThread := []string{"readonly", "get-thread", "--json", "--", "th-abc123"}
	if strings.Join(threadArgv, "\x00") != strings.Join(wantThread, "\x00") {
		t.Fatalf("get_thread argv = %q, want %q", threadArgv, wantThread)
	}

	// Trailing optional positional should omit dangling "--" when empty.
	optArgv, err := ExpandVerbArgv([]string{"list", "--json", "--", "{{?cursor}}"}, map[string]string{}, nil, "")
	if err != nil {
		t.Fatalf("ExpandVerbArgv(empty optional positional): %v", err)
	}
	if strings.Join(optArgv, "\x00") != strings.Join([]string{"list", "--json"}, "\x00") {
		t.Fatalf("expected dangling -- dropped when optional positional empty, got %q", optArgv)
	}

	// Bare placeholder BEFORE "--" must still be rejected by B5 fused-flag validation.
	if _, err := LintManifest([]byte(`{
  "schema_version": 1,
  "id": "bad-before-sep",
  "driver": {"builtin": "jsonmap"},
  "transport": {"kind": "cli", "cli": {"path": "/usr/bin/tool", "verbs": {"search": ["read", "{{query}}", "--", "{{thread_key}}"]}}}
}`), "vault"); err == nil || !strings.Contains(err.Error(), "fused-flag") {
		t.Fatalf("expected fused-flag rejection before --, got: %v", err)
	}

	// {{payload_file}} is forbidden as raw positional after "--".
	if _, err := LintManifest([]byte(`{
  "schema_version": 1,
  "id": "bad-payload-pos",
  "driver": {"builtin": "jsonmap"},
  "transport": {"kind": "cli", "cli": {"path": "/usr/bin/tool", "verbs": {"send": ["send", "--", "{{payload_file}}"]}}}
}`), "vault"); err == nil {
		t.Fatalf("expected rejection for {{payload_file}} after --")
	}
}

func TestSanitizedSubprocessEnvShimPreservationAndPathStripping(t *testing.T) {
	liveShimDir := filepath.Join(t.TempDir(), "agy-plugin-cli-live123")
	if err := os.MkdirAll(liveShimDir, 0o700); err != nil {
		t.Fatalf("mkdir liveShimDir: %v", err)
	}
	otherShimDir := filepath.Join(t.TempDir(), "agy-plugin-cli-stale999")
	baseEnv := []string{
		"PATH=" + liveShimDir + string(os.PathListSeparator) + otherShimDir + string(os.PathListSeparator) + "/usr/bin",
		"ANTIGRAVITY_CREDENTIAL_SCOPE=desktop-scope",
		"ANTIGRAVITY_PLUGIN_SHIM_DIR=" + liveShimDir,
		"ANTIGRAVITY_APP_DATA_DIR=/tmp/app",
		"ANTIGRAVITY_GEMINI_DIR=/tmp/gemini",
		"ANTIGRAVITY_CONFIG_REL_DIR=antigravity",
		"ANTIGRAVITY_LS_ADDRESS=127.0.0.1:43001",
		"ANTIGRAVITY_CSRF_TOKEN=secret-csrf",
		"HQ_PLUGIN_CLI_HOST=/usr/bin/language_server",
	}

	// 1. Live desktop session with existing shim dir preserves live shim & safe env, strips LS creds (B11).
	liveOut := envSliceToMap(SanitizedSubprocessEnv(baseEnv, []string{"AGY_ACCOUNT"}, nil, true))
	if liveOut["ANTIGRAVITY_PLUGIN_SHIM_DIR"] != liveShimDir {
		t.Fatalf("expected live shim preserved, got %q", liveOut["ANTIGRAVITY_PLUGIN_SHIM_DIR"])
	}
	if _, leaked := liveOut["ANTIGRAVITY_LS_ADDRESS"]; leaked {
		t.Fatalf("B11 violation: ANTIGRAVITY_LS_ADDRESS leaked into subprocess env")
	}
	if _, leaked := liveOut["ANTIGRAVITY_CSRF_TOKEN"]; leaked {
		t.Fatalf("B11 violation: ANTIGRAVITY_CSRF_TOKEN leaked into subprocess env")
	}
	if liveOut["ANTIGRAVITY_APP_DATA_DIR"] != "/tmp/app" || liveOut["HQ_PLUGIN_CLI_HOST"] != "/usr/bin/language_server" {
		t.Fatalf("expected safe plugin env keys preserved, got %+v", liveOut)
	}

	// 2. Non-desktop (desktopAllowed=false) sets sentinel AND strips shim dirs from PATH.
	headlessOut := envSliceToMap(SanitizedSubprocessEnv(baseEnv, []string{"AGY_ACCOUNT"}, nil, false))
	if headlessOut["ANTIGRAVITY_PLUGIN_SHIM_DIR"] != "/nonexistent-hq-sentinel" {
		t.Fatalf("expected sentinel shim dir when headless, got %q", headlessOut["ANTIGRAVITY_PLUGIN_SHIM_DIR"])
	}
	if strings.Contains(headlessOut["PATH"], "agy-plugin-cli-") {
		t.Fatalf("expected agy-plugin-cli- stripped from PATH when sentinel active, got %q", headlessOut["PATH"])
	}
	if !strings.Contains(headlessOut["PATH"], "/usr/bin") {
		t.Fatalf("expected /usr/bin kept in PATH, got %q", headlessOut["PATH"])
	}

	// 3. desktopAllowed=true with missing/nonexistent shim dir falls back to sentinel and strips PATH shim dirs.
	missingShimEnv := append([]string{}, baseEnv...)
	missingShimEnv[2] = "ANTIGRAVITY_PLUGIN_SHIM_DIR=" + filepath.Join(t.TempDir(), "does-not-exist")
	desktopStaleOut := envSliceToMap(SanitizedSubprocessEnv(missingShimEnv, nil, nil, true))
	if desktopStaleOut["ANTIGRAVITY_PLUGIN_SHIM_DIR"] != "/nonexistent-hq-sentinel" {
		t.Fatalf("expected sentinel when shim dir nonexistent, got %q", desktopStaleOut["ANTIGRAVITY_PLUGIN_SHIM_DIR"])
	}
	if strings.Contains(desktopStaleOut["PATH"], "agy-plugin-cli-") {
		t.Fatalf("expected agy-plugin-cli- stripped from PATH when fallback sentinel active, got %q", desktopStaleOut["PATH"])
	}
}

func TestGmailHybridDesktopOptionAAndDirectOAuthOptionB(t *testing.T) {
	vaultDir := t.TempDir()
	tokDir := filepath.Join(vaultDir, "private", "gmail", "tokens")
	if err := os.MkdirAll(tokDir, 0o700); err != nil {
		t.Fatalf("mkdir tokens: %v", err)
	}
	emptyTokPath := filepath.Join(tokDir, "empty.json")
	validTokPath := filepath.Join(tokDir, "valid.json")
	_ = os.WriteFile(emptyTokPath, []byte(`{}`), 0o600)
	_ = os.WriteFile(validTokPath, []byte(`{"refresh_token":"rt-test-valid"}`), 0o600)

	if HasUsableTokenFile(vaultDir, nil) {
		t.Fatalf("expected nil token ref to be unusable")
	}
	missing := filepath.Join(tokDir, "missing.json")
	if HasUsableTokenFile(vaultDir, &missing) {
		t.Fatalf("expected missing token file to be unusable")
	}
	if HasUsableTokenFile(vaultDir, &emptyTokPath) {
		t.Fatalf("expected empty JSON token file to be unusable")
	}
	if !HasUsableTokenFile(vaultDir, &validTokPath) {
		t.Fatalf("expected refresh_token JSON file to be usable")
	}

	if err := SaveInstance(context.Background(), vaultDir, InstanceConfig{
		SchemaVersion:  ConnectorSchemaVersion,
		Connector:      "gmail",
		DefaultAccount: "optiona",
		Accounts: map[string]AccountInstance{
			"optiona": {Email: "a@example.com", TransportKind: "cli"},
			"optionb": {
				Email:         "b@example.com",
				TransportKind: "direct_oauth",
				TokenRefs:     TokenRefs{Read: &validTokPath},
			},
		},
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	rc, err := Resolve(vaultDir, "gmail")
	if err != nil {
		t.Fatalf("Resolve(gmail): %v", err)
	}
	if !rc.EffectiveRequiresDesktopSession(vaultDir, "optiona") {
		t.Fatalf("expected Option A account without token file to require desktop session")
	}
	if rc.EffectiveRequiresDesktopSession(vaultDir, "optionb") {
		t.Fatalf("expected Option B account with valid token file NOT to require desktop session")
	}

	// Verify HQ_PLUGIN_CLI_HOST rewrites builtin gmail invocation to `<host> plugin-cli gmail ...`.
	hostBin := filepath.Join(t.TempDir(), "fake-language-server")
	script := `#!/bin/sh
if [ "$1" = "plugin-cli" ] && [ "$2" = "gmail" ]; then
  echo '[{"id":"msg-host-1","threadId":"th-host-1"}]'
  exit 0
fi
exit 2
`
	if err := os.WriteFile(hostBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake host: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HQ_PLUGIN_CLI_HOST", hostBin)
	t.Setenv("ANTIGRAVITY_CREDENTIAL_SCOPE", "desktop-test")
	out, err := ExecCLIVerb(context.Background(), vaultDir, "gmail", "optiona", "search", map[string]string{
		"query": "label:INBOX is:unread",
	}, nil, nil, true)
	if err != nil || !strings.Contains(string(out), "msg-host-1") {
		t.Fatalf("expected HQ_PLUGIN_CLI_HOST plugin-cli rewrite to succeed, err=%v out=%s", err, string(out))
	}
}

func envSliceToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

func TestLoadReachableDesktopPluginEnvAndSanitizedExtra(t *testing.T) {
	vaultDir := t.TempDir()
	shimDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rtDir := filepath.Join(vaultDir, "private", "runtime")
	if err := os.MkdirAll(rtDir, 0o700); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	sess := runtimeLSSession{
		BaseURL:         srv.URL,
		CSRFToken:       "csrf-secret-value",
		AgentAPIExe:     "/opt/fake/language_server",
		PluginShimDir:   shimDir,
		CredentialScope: "user-desktop-scope",
		AppDataDir:      "/tmp/fake-appdata",
		GeminiDir:       "/tmp/fake-gemini",
		ConfigRelDir:    ".gemini/config",
	}
	raw, _ := json.Marshal(sess)
	if err := os.WriteFile(filepath.Join(rtDir, "ls_session.json"), raw, 0o600); err != nil {
		t.Fatalf("write ls_session.json: %v", err)
	}

	hydrated, ok := LoadReachableDesktopPluginEnv(vaultDir)
	if !ok {
		t.Fatalf("expected LoadReachableDesktopPluginEnv=true while LS reachable and shimDir exists")
	}
	if hydrated["ANTIGRAVITY_PLUGIN_SHIM_DIR"] != shimDir || hydrated["ANTIGRAVITY_CREDENTIAL_SCOPE"] != "user-desktop-scope" {
		t.Fatalf("unexpected hydrated plugin env: %+v", hydrated)
	}

	// SanitizedSubprocessEnv with desktop scope enabled allows valid hydrated shim/scope
	// while still stripping CSRF/LS_ADDRESS/AGENTAPI_EXE.
	envWithDesktop := envSliceToMap(SanitizedSubprocessEnv([]string{
		"PATH=/usr/bin",
		"ANTIGRAVITY_CSRF_TOKEN=leak",
		"ANTIGRAVITY_LS_ADDRESS=127.0.0.1:1234",
	}, []string{"AGY_ACCOUNT"}, hydrated, true))
	if envWithDesktop["ANTIGRAVITY_PLUGIN_SHIM_DIR"] != shimDir || envWithDesktop["ANTIGRAVITY_CREDENTIAL_SCOPE"] != "user-desktop-scope" {
		t.Fatalf("expected desktop shim/scope in sanitized env, got %+v", envWithDesktop)
	}
	if _, leaked := envWithDesktop["ANTIGRAVITY_CSRF_TOKEN"]; leaked {
		t.Fatalf("CSRF token leaked into sanitized env")
	}
	if _, leaked := envWithDesktop["ANTIGRAVITY_LS_ADDRESS"]; leaked {
		t.Fatalf("LS address leaked into sanitized env")
	}

	// When allowDesktopScope=false (untrusted), extra desktop shim/scope MUST NOT bypass sentinel.
	envUntrusted := envSliceToMap(SanitizedSubprocessEnv([]string{"PATH=/usr/bin"}, []string{"AGY_ACCOUNT"}, hydrated, false))
	if envUntrusted["ANTIGRAVITY_PLUGIN_SHIM_DIR"] != "/nonexistent-hq-sentinel" {
		t.Fatalf("expected sentinel shim for untrusted subprocess, got %q", envUntrusted["ANTIGRAVITY_PLUGIN_SHIM_DIR"])
	}
	if _, leaked := envUntrusted["ANTIGRAVITY_CREDENTIAL_SCOPE"]; leaked {
		t.Fatalf("expected credential scope stripped when allowDesktopScope=false")
	}

	// Sentinel shimDir inside ls_session.json MUST be rejected.
	sess.PluginShimDir = "/nonexistent-hq-sentinel"
	rawSentinel, _ := json.Marshal(sess)
	_ = os.WriteFile(filepath.Join(rtDir, "ls_session.json"), rawSentinel, 0o600)
	if _, ok := LoadReachableDesktopPluginEnv(vaultDir); ok {
		t.Fatalf("expected sentinel pluginShimDir in ls_session.json to be rejected")
	}
}

func TestClassifyGmailAutomationAndUrgency(t *testing.T) {
	makeHeaderPart := func(kv ...string) RawMIMEPart {
		var p RawMIMEPart
		for i := 0; i+1 < len(kv); i += 2 {
			p.Headers = append(p.Headers, RawMIMEHeader{Name: kv[i], Value: kv[i+1]})
		}
		return p
	}

	// 1. Pure newsletter with List-Unsubscribe + substack host -> isBulkAutomated=true
	subMsg := RawMailMessage{
		Payload: makeHeaderPart("List-Unsubscribe", "<https://example.com/unsubscribe>"),
	}
	auto1, sec1, sched1 := classifyGmailAutomationAndUrgency(subMsg, "news@"+"substack.com", "weekly engineering digest", "Read online")
	if !auto1 || sec1 || sched1 {
		t.Fatalf("expected pure Substack newsletter to be bulk automated, got auto=%v sec=%v sched=%v", auto1, sec1, sched1)
	}

	// 2. Transactional no-reply portal without bulk headers (e.g., school schedule notice) -> isBulkAutomated=false, hasSchedule=true
	schoolMsg := RawMailMessage{}
	auto2, sec2, sched2 := classifyGmailAutomationAndUrgency(schoolMsg, "no-reply@example.org", "early dismissal tomorrow at 1pm", "Please pick up students early.")
	if auto2 || sec2 || !sched2 {
		t.Fatalf("expected school no-reply notice NOT to be bulk-swallowed and to flag schedule, got auto=%v sec=%v sched=%v", auto2, sec2, sched2)
	}

	// 3. Brokerage/IPO deadline carrying List-Unsubscribe -> deadline override keeps isBulkAutomated=false!
	ipoMsg := RawMailMessage{
		Payload: makeHeaderPart("List-Unsubscribe", "<https://example.com/unsub>", "Precedence", "bulk"),
	}
	auto3, _, _ := classifyGmailAutomationAndUrgency(ipoMsg, "no-reply@example.com", "indication of interest closing soon", "Confirm allocation before tonight.")
	if auto3 {
		t.Fatalf("expected deadline/IOI cue to override bulk headers so item is triaged")
	}

	// 4. Delivery bounce from mailer-daemon -> isBulkAutomated=false, hasSecurity=true
	bounceMsg := RawMailMessage{
		Payload: makeHeaderPart("Precedence", "bulk"),
	}
	auto4, sec4, _ := classifyGmailAutomationAndUrgency(bounceMsg, "mailer-daemon@example.com", "delivery status notification (failure)", "Address not found")
	if auto4 || !sec4 {
		t.Fatalf("expected bounce notification to set hasSecurity=true and isBulkAutomated=false, got auto=%v sec=%v", auto4, sec4)
	}
}

func TestGCalendarReadQuarantinedMockAndEventsFallbackWithWindowCache(t *testing.T) {
	// 1. Mock mode ReadQuarantined on embedded gcalendar manifest
	mockVault := t.TempDir()
	mockDir := filepath.Join(mockVault, "memory", "gcalendar")
	if err := os.MkdirAll(mockDir, 0o700); err != nil {
		t.Fatalf("mkdir mockDir: %v", err)
	}
	if err := SaveInstance(context.Background(), mockVault, InstanceConfig{
		SchemaVersion:  ConnectorSchemaVersion,
		Connector:      "gcalendar",
		DefaultAccount: "work",
		Accounts: map[string]AccountInstance{
			"work": {Email: "user@acme.com", TransportKind: "cli"},
		},
	}); err != nil {
		t.Fatalf("SaveInstance(mockVault): %v", err)
	}
	mockPayload := `{"events":[
		{"item_id":"evt-mock-1","thread_key":"evt-mock-1","subject_or_room":"Architecture Sync","timestamp":"2026-11-17T16:00:00Z","sender":"lead@acme.com","body":"Discuss API roadmap"},
		{"item_id":"evt-mock-2","thread_key":"evt-mock-2","subject_or_room":"1:1 Check-in","timestamp":"2026-11-17T17:00:00Z","sender":"mgr@acme.com","body":"Weekly sync"}
	]}`
	if err := os.WriteFile(filepath.Join(mockDir, "mock.json"), []byte(mockPayload), 0o600); err != nil {
		t.Fatalf("write mock.json: %v", err)
	}
	qtMock, err := ReadQuarantined(context.Background(), mockVault, "gcalendar", "work", "evt-mock-2", false)
	if err != nil {
		t.Fatalf("ReadQuarantined(mock): %v", err)
	}
	if qtMock.ThreadKey != "evt-mock-2" || qtMock.SubjectOrRoom != "1:1 Check-in" || len(qtMock.Messages) != 1 || !strings.Contains(qtMock.Messages[0].Body, "Weekly sync") {
		t.Fatalf("unexpected mock ReadQuarantined output: %+v", qtMock)
	}

	// 2. CLI mode where connector declares ONLY "events" verb (no single-item read_quarantined/get verb)
	//    Verify fallback window fetch, synthesized event summary when body is empty, and 15s window cache deduplication!
	cliVault := t.TempDir()
	countFile := filepath.Join(t.TempDir(), "invocations.txt")
	fakeCalBin := filepath.Join(t.TempDir(), "fake-cal-cli")
	script := "#!/bin/sh\necho x >> \"" + countFile + "\"\ncat <<'EOF'\n" +
		`[{"id":"evt-cli-a","title":"Design Review","start":"2026-11-17T18:00:00Z","htmlLink":"https://calendar.example.com/e/a"},{"id":"evt-cli-b","title":"Sprint Retro","start":"2026-11-17T19:00:00Z","htmlLink":"https://calendar.example.com/e/b"}]` +
		"\nEOF\n"
	if err := os.WriteFile(fakeCalBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fakeCalBin: %v", err)
	}

	ovrDir := filepath.Join(cliVault, "connectors", "gcalendar")
	if err := os.MkdirAll(ovrDir, 0o755); err != nil {
		t.Fatalf("mkdir ovrDir: %v", err)
	}
	customManifest := `{
  "schema_version": 1,
  "id": "gcalendar",
  "capabilities": {
    "brief": {"enabled": true, "quarantined": true},
    "read_quarantined": {"enabled": true}
  },
  "driver": {
    "builtin": "gcalendar"
  },
  "transport": {
    "kind": "cli",
    "cli": {
      "path": "` + fakeCalBin + `",
      "verbs": {
        "events": ["events", "--start={{start}}", "--end={{end}}"]
      },
      "output": {
        "events": {
          "jsonmap": {
            "item_id": "/id",
            "thread_key": "/id",
            "subject_or_room": "/title",
            "timestamp": "/start",
            "permalink": "/htmlLink"
          }
        }
      }
    }
  }
}`
	if err := os.WriteFile(filepath.Join(ovrDir, "connector.json"), []byte(customManifest), 0o644); err != nil {
		t.Fatalf("write override connector.json: %v", err)
	}
	bundle, err := extension.BuildTrustBundle(cliVault, "vault", "")
	if err != nil {
		t.Fatalf("BuildTrustBundle: %v", err)
	}
	rawBundle, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal trust bundle: %v", err)
	}
	if err := extension.ApplyTrustBundle(cliVault, rawBundle); err != nil {
		t.Fatalf("ApplyTrustBundle: %v", err)
	}
	if err := SaveInstance(context.Background(), cliVault, InstanceConfig{
		SchemaVersion:  ConnectorSchemaVersion,
		Connector:      "gcalendar",
		DefaultAccount: "work",
		Accounts: map[string]AccountInstance{
			"work": {Email: "user@acme.com", TransportKind: "cli"},
		},
	}); err != nil {
		t.Fatalf("SaveInstance(cliVault): %v", err)
	}

	qtA, err := ReadQuarantined(context.Background(), cliVault, "gcalendar", "work", "evt-cli-a", false)
	if err != nil {
		t.Fatalf("ReadQuarantined(evt-cli-a): %v", err)
	}
	if qtA.SubjectOrRoom != "Design Review" || len(qtA.Messages) != 1 || !strings.Contains(qtA.Messages[0].Body, "Design Review") {
		t.Fatalf("expected synthesized plain text summary for evt-cli-a, got %+v", qtA)
	}
	qtB, err := ReadQuarantined(context.Background(), cliVault, "gcalendar", "work", "evt-cli-b", false)
	if err != nil {
		t.Fatalf("ReadQuarantined(evt-cli-b): %v", err)
	}
	if qtB.SubjectOrRoom != "Sprint Retro" {
		t.Fatalf("expected Sprint Retro for evt-cli-b, got %+v", qtB)
	}
	invBytes, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("read countFile: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(string(invBytes)), "\n"); len(lines) != 1 {
		t.Fatalf("expected 15s window cache to invoke CLI once across 2 back-to-back event reads, got %d invocations", len(lines))
	}
}

func TestRunSubprocessReportsParentDeadlineTimeout(t *testing.T) {
	slowScript := filepath.Join(t.TempDir(), "slow.sh")
	if err := os.WriteFile(slowScript, []byte("#!/bin/sh\nsleep 2\n"), 0o755); err != nil {
		t.Fatalf("write slowScript: %v", err)
	}
	parentCtx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	_, _, err := RunSubprocess(parentCtx, SubprocessSpec{
		Binary:  slowScript,
		Timeout: 10 * time.Second,
	})
	if err == nil {
		t.Fatalf("expected RunSubprocess timeout error")
	}
	if strings.Contains(err.Error(), "after 10s") {
		t.Fatalf("expected error to report parent context deadline (<1s) rather than spec 10s, got: %v", err)
	}
}

func TestParseRawMailAndDecodeBase64URL(t *testing.T) {
	if got := DecodeBase64URL(""); got != "" {
		t.Fatalf("DecodeBase64URL(empty) = %q, want empty", got)
	}
	if got := DecodeBase64URL("SGVsbG8gd29ybGQ"); got != "Hello world" {
		t.Fatalf("DecodeBase64URL(unpadded) = %q, want Hello world", got)
	}
	if got := DecodeBase64URL("SGVsbG8gd29ybGQ="); got != "Hello world" {
		t.Fatalf("DecodeBase64URL(padded) = %q, want Hello world", got)
	}
	if got := DecodeBase64URL("!!!not-b64!!!"); got != "" {
		t.Fatalf("DecodeBase64URL(invalid) = %q, want empty", got)
	}

	rm := RawMailMessage{
		ID:           " msg-1 ",
		Snippet:      "Tom &amp; Jerry",
		InternalDate: "1700000000000",
		Payload: RawMIMEPart{
			MimeType: "multipart/mixed",
			Headers: []RawMIMEHeader{
				{Name: "From", Value: "alice@example.com"},
				{Name: "From", Value: "spoof@evil.com"},
				{Name: "To", Value: "bob@example.com"},
				{Name: "Subject", Value: "Hello"},
				{Name: "Date", Value: "Tue, 14 Nov 2023 22:13:20 +0000"},
			},
			Parts: []RawMIMEPart{
				{MimeType: "text/plain", Body: RawMIMEBody{Data: "SGVsbG8gd29ybGQ"}},
				{MimeType: "text/html", Body: RawMIMEBody{Data: "PGI-SGVsbG88L2I-"}},
				{MimeType: "application/pdf", Filename: "doc.pdf", Body: RawMIMEBody{AttachmentID: "att-9", Size: 42}},
			},
		},
	}
	p := ParseRawMail(rm)
	if p.ID != "msg-1" || p.ThreadID != "msg-1" {
		t.Fatalf("ID/ThreadID = %q/%q", p.ID, p.ThreadID)
	}
	if p.From != "alice@example.com" || p.To != "bob@example.com" || p.Subject != "Hello" {
		t.Fatalf("unexpected headers: %+v", p)
	}
	if p.PlainBody != "Hello world" || p.HTMLBody != "<b>Hello</b>" {
		t.Fatalf("unexpected bodies: plain=%q html=%q", p.PlainBody, p.HTMLBody)
	}
	if p.Snippet != "Tom & Jerry" || p.Timestamp != "2023-11-14T22:13:20Z" {
		t.Fatalf("unexpected snippet/ts: %q / %q", p.Snippet, p.Timestamp)
	}
	if len(p.Attachments) != 1 || p.Attachments[0].AttachmentID != "att-9" {
		t.Fatalf("unexpected attachments: %+v", p.Attachments)
	}
}

func TestMessagingHelpersDraftWatermarkAndTriage(t *testing.T) {
	vaultDir := t.TempDir()

	for _, badDraft := range []string{"../etc/passwd", ".", "..", ".hidden", "a..b", `a\b`} {
		if _, err := ValidateDraftID("gmail", badDraft); err == nil {
			t.Fatalf("expected invalid draft ID %q to fail", badDraft)
		}
	}
	if id, err := ValidateDraftID("gmail", " draft-1234 "); err != nil || id != "draft-1234" {
		t.Fatalf("ValidateDraftID = %q, %v", id, err)
	}
	if err := SaveAccountWatermark(vaultDir, "gmail", AccountWatermark{Account: "../../escaped", SeenMessageIDs: map[string]bool{"m": true}}); err == nil {
		t.Fatal("expected SaveAccountWatermark with traversal account to fail")
	}
	if err := SaveAccountWatermark(vaultDir, "../../escaped", AccountWatermark{Account: "primary"}); err == nil {
		t.Fatal("expected SaveAccountWatermark with traversal channel to fail")
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "memory", "state-escaped.json")); !os.IsNotExist(err) {
		t.Fatal("traversal watermark file must not be created")
	}
	if got := LoadAccountWatermark(vaultDir, "gmail", "../../escaped"); len(got.SeenMessageIDs) != 0 {
		t.Fatalf("expected empty watermark for invalid alias, got %+v", got)
	}
	if _, err := LoadInstance(vaultDir, "../../escaped"); err == nil {
		t.Fatal("expected LoadInstance to reject traversal connID")
	}

	if err := VerifyDraftSendPreconditions("gchat", "me@example.com", "me@example.com", "abcdef0123456789", "abc"); err == nil {
		t.Fatal("expected short hash prefix to fail")
	}
	if err := VerifyDraftSendPreconditions("gchat", "me@example.com", "other@example.com", "abcdef0123456789", "abcdef01"); err == nil {
		t.Fatal("expected account mismatch to fail")
	}
	if err := VerifyDraftSendPreconditions("gchat", "me@example.com", "me@example.com", "abcdef0123456789", "deadbeef"); err == nil || !strings.Contains(err.Error(), "bait-and-switch blocked") {
		t.Fatalf("expected bait-and-switch error, got %v", err)
	}
	if err := VerifyDraftSendPreconditions("gchat", "ME@example.com", "me@example.com", "ABCDEF0123456789", "abcdef01"); err != nil {
		t.Fatalf("expected valid preconditions to pass, got %v", err)
	}

	wm := LoadAccountWatermark(vaultDir, "gmail", "primary")
	if wm.Account != "primary" || wm.SeenMessageIDs == nil {
		t.Fatalf("unexpected empty watermark: %+v", wm)
	}
	wm.SeenMessageIDs["m-1"] = true
	if err := SaveAccountWatermark(vaultDir, "gmail", wm); err != nil {
		t.Fatalf("SaveAccountWatermark: %v", err)
	}
	loaded := LoadAccountWatermark(vaultDir, "gmail", "primary")
	if !loaded.SeenMessageIDs["m-1"] {
		t.Fatalf("expected persisted seen message m-1, got %+v", loaded)
	}

	if err := ValidateTriageBatchID("../bad"); err == nil {
		t.Fatal("expected invalid batch ID to fail")
	}
	err := WriteTriageResults(vaultDir, "gmail", "triage", "Gmail Triage Digest", "batch-1", []string{"item1"}, []TriageDigestEntry{
		{Urgency: "urgent", Category: "work", TargetID: "th-1", Title: "Urgent", SenderLab: "From", Sender: "alice@example.com", Summary: "Reply soon"},
		{Urgency: "ignore", Category: "other", TargetID: "th-2", Title: "Noise", Sender: "bob@example.com", Summary: "Skip"},
	})
	if err != nil {
		t.Fatalf("WriteTriageResults: %v", err)
	}
	digestBytes, err := os.ReadFile(filepath.Join(vaultDir, "notes", "inbox", "batch-1.md"))
	if err != nil {
		t.Fatalf("read triage digest: %v", err)
	}
	if !strings.Contains(string(digestBytes), "Urgent") || strings.Contains(string(digestBytes), "Noise") {
		t.Fatalf("unexpected triage digest:\n%s", string(digestBytes))
	}
	if err := WriteTriageResults(vaultDir, "../../escaped", "triage", "Digest", "batch-2", []string{"item1"}, nil); err == nil {
		t.Fatal("expected WriteTriageResults with traversal channel to fail")
	}
}

func TestRefreshableOAuthTokenEdgeCases(t *testing.T) {
	dir := t.TempDir()
	writeTok := func(name string, obj any) string {
		t.Helper()
		b, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	assertProtocolCode := func(err error, wantCode string) {
		t.Helper()
		if err == nil {
			t.Fatalf("expected error with ProtocolError code %q, got nil", wantCode)
		}
		var pe *ProtocolError
		if !errors.As(err, &pe) || pe.Code != wantCode {
			t.Fatalf("expected ProtocolError code %q, got %T (%v)", wantCode, err, err)
		}
	}

	// 1. Empty or whitespace-only tokenPath -> auth_expired
	for _, p := range []string{"", "   \t\n"} {
		_, err := RefreshableOAuthToken(p)
		assertProtocolCode(err, "auth_expired")
	}

	// 2. Valid access_token with surrounding whitespace -> trimmed token
	pValid := writeTok("valid.json", map[string]any{"access_token": "  ya29.ok \n"})
	if tok, err := RefreshableOAuthToken(pValid); err != nil || tok != "ya29.ok" {
		t.Fatalf("RefreshableOAuthToken(valid) = %q, %v; want ya29.ok, nil", tok, err)
	}

	// 3. Whitespace-only access_token with no refresh_token -> auth_expired (never "", nil)
	pWS := writeTok("ws.json", map[string]any{"access_token": "   \t"})
	if tok, err := RefreshableOAuthToken(pWS); err == nil || tok != "" {
		t.Fatalf("expected error for whitespace access_token, got %q, %v", tok, err)
	} else {
		assertProtocolCode(err, "auth_expired")
	}

	// 4. Whitespace-only refresh credentials must fail locally without HTTP call
	hitCount := 0
	noCallSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer noCallSrv.Close()
	pWSRefresh := writeTok("ws-refresh.json", map[string]any{
		"access_token":  "   ",
		"refresh_token": "   ",
		"client_id":     "cid",
		"client_secret": "csec",
		"token_uri":     noCallSrv.URL,
	})
	if _, err := RefreshableOAuthToken(pWSRefresh); err == nil {
		t.Fatal("expected whitespace refresh_token to fail locally")
	} else {
		assertProtocolCode(err, "auth_expired")
	}
	if hitCount != 0 {
		t.Fatalf("expected 0 HTTP calls for whitespace refresh credentials, got %d", hitCount)
	}

	// 5. Whitespace-only access_token WITH valid trimmed refresh creds triggers refresh POST
	var statusMode int
	var respBody string
	var gotFormClientID, gotFormSecret, gotFormRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil {
			gotFormClientID = r.Form.Get("client_id")
			gotFormSecret = r.Form.Get("client_secret")
			gotFormRefresh = r.Form.Get("refresh_token")
		}
		w.WriteHeader(statusMode)
		_, _ = fmt.Fprint(w, respBody)
	}))
	defer srv.Close()

	statusMode = http.StatusOK
	respBody = `{"access_token":"  ya29.refreshed  ","expires_in":3600}`
	pDoRefresh := writeTok("do-refresh.json", map[string]any{
		"access_token":  "   ",
		"refresh_token": " rt-1 ",
		"client_id":     " cid-1 ",
		"client_secret": " sec-1 ",
		"token_uri":     srv.URL,
	})
	if tok, err := RefreshableOAuthToken(pDoRefresh); err != nil || tok != "ya29.refreshed" {
		t.Fatalf("refresh from whitespace access_token = %q, %v; want ya29.refreshed", tok, err)
	}
	if gotFormClientID != "cid-1" || gotFormSecret != "sec-1" || gotFormRefresh != "rt-1" {
		t.Fatalf("expected trimmed form params, got cid=%q sec=%q rt=%q", gotFormClientID, gotFormSecret, gotFormRefresh)
	}

	// 6. Expiry formats: past RFC3339, within-60s RFC3339, naive ISO8601 UTC, expires_at RFC3339, expires_at numeric epoch (past vs future)
	pastNaive := time.Now().UTC().Add(-2 * time.Minute).Format("2006-01-02T15:04:05")
	within60 := time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339)
	pastEpoch := time.Now().Unix() - 10
	futureEpoch := time.Now().Unix() + 600
	for name, tokObj := range map[string]map[string]any{
		"past-naive": {
			"access_token": "stale", "refresh_token": "rt", "client_id": "c", "client_secret": "s",
			"token_uri": srv.URL, "expiry": pastNaive,
		},
		"within-60s": {
			"access_token": "stale", "refresh_token": "rt", "client_id": "c", "client_secret": "s",
			"token_uri": srv.URL, "expiry": within60,
		},
		"expires-at-str": {
			"access_token": "stale", "refresh_token": "rt", "client_id": "c", "client_secret": "s",
			"token_uri": srv.URL, "expires_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		},
		"expires-at-epoch-past": {
			"access_token": "stale", "refresh_token": "rt", "client_id": "c", "client_secret": "s",
			"token_uri": srv.URL, "expires_at": pastEpoch,
		},
	} {
		p := writeTok(name+".json", tokObj)
		if tok, err := RefreshableOAuthToken(p); err != nil || tok != "ya29.refreshed" {
			t.Fatalf("%s: got %q, %v; want refreshed token", name, tok, err)
		}
	}
	pFutureEpoch := writeTok("future-epoch.json", map[string]any{
		"access_token": "ya29.still-fresh",
		"expires_at":   futureEpoch,
		"token_uri":    noCallSrv.URL,
	})
	if tok, err := RefreshableOAuthToken(pFutureEpoch); err != nil || tok != "ya29.still-fresh" {
		t.Fatalf("future-epoch: got %q, %v; want ya29.still-fresh", tok, err)
	}

	// 7. HTTP 200 with whitespace access_token -> auth_expired
	statusMode = http.StatusOK
	respBody = `{"access_token":"   "}`
	if tok, err := RefreshableOAuthToken(pDoRefresh); err == nil || tok != "" {
		t.Fatalf("expected error for whitespace refreshed access_token, got %q, %v", tok, err)
	} else {
		assertProtocolCode(err, "auth_expired")
	}

	// 8. HTTP 400/401/403 -> auth_expired; HTTP 500 & bad JSON -> transient
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden} {
		statusMode = code
		respBody = `{"error":"invalid_grant"}`
		_, err := RefreshableOAuthToken(pDoRefresh)
		assertProtocolCode(err, "auth_expired")
	}
	statusMode = http.StatusInternalServerError
	respBody = `internal error`
	_, err := RefreshableOAuthToken(pDoRefresh)
	assertProtocolCode(err, "transient")

	statusMode = http.StatusOK
	respBody = `{not-json`
	_, err = RefreshableOAuthToken(pDoRefresh)
	assertProtocolCode(err, "transient")
}
