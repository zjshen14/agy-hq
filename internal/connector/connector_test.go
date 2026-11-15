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
