package gmail

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
