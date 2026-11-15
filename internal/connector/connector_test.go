package connector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
