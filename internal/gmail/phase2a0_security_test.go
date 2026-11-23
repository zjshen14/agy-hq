package gmail

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/taint"
)

func TestRecordTriageResultsRejectsPathTraversal(t *testing.T) {
	vaultDir := t.TempDir()
	items := []TriageItem{{
		ThreadID: "thr-1",
		From:     "alice@example.com",
		Subject:  "Status",
		Urgency:  "fyi",
		Category: "work",
		Summary:  "All good",
	}}
	for _, bad := range []string{"../../USER", "../extensions", "a/b", `a\b`, "..", ".hidden", "bad..id"} {
		if err := RecordTriageResults(vaultDir, bad, items); err == nil {
			t.Fatalf("expected RecordTriageResults to reject traversal batchID %q", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "USER.md")); !os.IsNotExist(err) {
		t.Fatalf("USER.md must not be created via path traversal")
	}
	if err := RecordTriageResults(vaultDir, "batch-ok_1.0", items); err != nil {
		t.Fatalf("expected valid batchID to succeed, got %v", err)
	}
}

func TestReadQuarantinedThreadCheckBeforeMark(t *testing.T) {
	vaultDir := t.TempDir()
	mock := filepath.Join(vaultDir, "mock-gmail")
	if err := os.MkdirAll(mock, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HQ_GMAIL_MOCK_DIR", mock)
	t.Setenv("HQ_STRICT_CALLER_ENV", "0")

	if err := SaveRegistry(vaultDir, AccountRegistry{
		DefaultAccount: "primary",
		Accounts:       map[string]AccountConfig{"primary": {Alias: "primary", Email: "user@example.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Pre-taint session locked to thr-allowed only
	if _, err := taint.Mark(context.Background(), vaultDir, taint.Record{
		ConvID:    "conv-tainted-1",
		Source:    "spawn_batch:connector:gmail",
		Connector: "gmail",
		Account:   "primary",
		ItemKeys:  []string{"gmail:thread:thr-allowed"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ReadQuarantinedThread(vaultDir, "primary", "thr-foreign", "conv-tainted-1"); err == nil {
		t.Fatalf("expected foreign thread read to be rejected")
	}
	// Verify foreign thread was NOT appended into taint.Record.ItemKeys
	rec, ok := taint.Check(vaultDir, "conv-tainted-1")
	if !ok {
		t.Fatalf("expected taint record to exist")
	}
	for _, k := range rec.ItemKeys {
		if k == "thr-foreign" || k == "gmail:thread:thr-foreign" {
			t.Fatalf("check-before-mark violated: foreign key %q leaked into ItemKeys %v", k, rec.ItemKeys)
		}
	}

	// Tainted session with empty ItemKeys must fail closed
	if _, err := taint.Mark(context.Background(), vaultDir, taint.Record{
		ConvID: "conv-empty-keys",
		Source: "spawn_batch:delegate",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadQuarantinedThread(vaultDir, "primary", "thr-allowed", "conv-empty-keys"); err == nil {
		t.Fatalf("expected tainted session with empty ItemKeys to fail closed")
	}
}

func TestCreateOrUpdateDraftRejectsPathTraversalAndMaliciousRemoteID(t *testing.T) {
	vaultDir := t.TempDir()
	t.Setenv("HQ_GMAIL_MOCK_DIR", "")
	t.Setenv("HQ_STRICT_CALLER_ENV", "0")

	if _, err := InitOrAddAccountWithOptions(vaultDir, InitAccountOptions{Alias: "../../escaped", Email: "user@example.com"}); err == nil {
		t.Fatal("expected InitOrAddAccountWithOptions to reject traversal alias")
	}
	if _, err := InitOrAddAccountWithOptions(vaultDir, InitAccountOptions{Alias: "primary", Email: "user@example.com", SetDefault: true}); err != nil {
		t.Fatalf("init primary: %v", err)
	}

	// 1. Input traversal draftIDs rejected by CreateOrUpdateDraft
	for _, bad := range []string{"../../escaped", "../foo", "a/b", `a\b`, ".hidden", "bad..id"} {
		if _, err := CreateOrUpdateDraft(vaultDir, "primary", bad, "", "alice@example.com", "", "Subj", "Body", ""); err == nil {
			t.Fatalf("expected CreateOrUpdateDraft to reject traversal draftID %q", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "memory", "escaped.json")); !os.IsNotExist(err) {
		t.Fatal("escaped.json must not be created via draftID traversal")
	}

	// 2. Malicious remote draft ID returned by trusted custom connector CLI must fail closed
	cliScript := filepath.Join(t.TempDir(), "bad-gmail-cli")
	scriptBody := `#!/bin/sh
echo '{"id":"../../escaped"}'
`
	if err := os.WriteFile(cliScript, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	vaultConnDir := filepath.Join(vaultDir, "connectors", "gmail")
	if err := os.MkdirAll(vaultConnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestMap := map[string]any{
		"schema_version":           1,
		"id":                       "gmail",
		"requires_desktop_session": false,
		"driver":                   map[string]any{"builtin": "gmail"},
		"transport": map[string]any{
			"kind": "cli",
			"cli": map[string]any{
				"path": cliScript,
				"verbs": map[string][]string{
					"create_draft": {"create-draft", "--to={{to}}", "--subject={{subject}}", "--body-file={{payload_file}}"},
				},
			},
		},
	}
	manifestRaw, _ := json.MarshalIndent(manifestMap, "", "  ")
	manifestPath := filepath.Join(vaultConnDir, "connector.json")
	if err := os.WriteFile(manifestPath, manifestRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	tb, err := extension.BuildTrustBundle(vaultDir, "vault", manifestPath)
	if err != nil {
		t.Fatalf("BuildTrustBundle: %v", err)
	}
	tbRaw, _ := json.Marshal(tb)
	if err := extension.ApplyTrustBundle(vaultDir, tbRaw); err != nil {
		t.Fatalf("ApplyTrustBundle: %v", err)
	}

	if _, err := CreateOrUpdateDraft(vaultDir, "primary", "", "", "alice@example.com", "", "Subj", "Body", ""); err == nil || !strings.Contains(err.Error(), "invalid draft ID") {
		t.Fatalf("expected malicious remote draft ID to fail closed, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "memory", "escaped.json")); !os.IsNotExist(err) {
		t.Fatal("escaped.json must not be created via malicious remote draft ID")
	}

	// 3. saveThreadParticipants & readMockThread reject traversal threadID
	if err := saveThreadParticipants(vaultDir, "primary", "x/../../../../escaped", []string{"alice@example.com"}); err == nil {
		t.Fatal("expected saveThreadParticipants to reject traversal threadID")
	}
	if _, err := readMockThread(t.TempDir(), "primary", "../../escaped"); err == nil {
		t.Fatal("expected readMockThread to reject traversal threadID")
	}
	if _, _, err := ReadQuarantinedThread(vaultDir, "primary", "x/../../../../escaped", "conv-thread-trav"); err == nil {
		t.Fatal("expected ReadQuarantinedThread to reject traversal threadID")
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "memory", "escaped.json")); !os.IsNotExist(err) {
		t.Fatal("escaped.json must not be created via threadID traversal")
	}
}
