package gchat

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zjshen14/agy-hq/internal/taint"
)

func TestGChatRecordTriageResultsRejectsPathTraversal(t *testing.T) {
	vaultDir := t.TempDir()
	items := []TriageItem{{
		SpaceID:   "spaces/TEST1234",
		SpaceName: "Team",
		Sender:    "alice@example.com",
		Urgency:   "fyi",
		Category:  "work",
		Summary:   "All good",
	}}
	for _, bad := range []string{"../../USER", "../extensions", "a/b", `a\b`, "..", "bad..id"} {
		if err := RecordTriageResults(vaultDir, bad, items); err == nil {
			t.Fatalf("expected RecordTriageResults to reject traversal batchID %q", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "USER.md")); !os.IsNotExist(err) {
		t.Fatalf("USER.md must not be created via path traversal")
	}
	if err := RecordTriageResults(vaultDir, "gchat-batch-1", items); err != nil {
		t.Fatalf("expected valid batchID to succeed, got %v", err)
	}
}

func TestGChatReadQuarantinedThreadCheckBeforeMark(t *testing.T) {
	vaultDir := t.TempDir()
	t.Setenv("HQ_STRICT_CALLER_ENV", "0")
	if err := SaveRegistry(vaultDir, AccountRegistry{
		DefaultAccount: "primary",
		Accounts:       map[string]AccountConfig{"primary": {Alias: "primary", Email: "user@example.com", Transport: "mock"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := taint.Mark(context.Background(), vaultDir, taint.Record{
		ConvID:    "conv-gchat-tainted",
		Source:    "spawn_batch:connector:gchat",
		Connector: "gchat",
		Account:   "primary",
		ItemKeys:  []string{"gchat:space:spaces/TESTALLOW"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadQuarantinedThread(vaultDir, "primary", "spaces/TESTFOREIGN", "", "conv-gchat-tainted", 5); err == nil {
		t.Fatalf("expected foreign space read to be rejected")
	}
	rec, ok := taint.Check(vaultDir, "conv-gchat-tainted")
	if !ok {
		t.Fatalf("expected taint record to exist")
	}
	for _, k := range rec.ItemKeys {
		if k == "spaces/TESTFOREIGN" || k == "gchat:space:spaces/TESTFOREIGN" {
			t.Fatalf("check-before-mark violated: foreign space %q leaked into ItemKeys %v", k, rec.ItemKeys)
		}
	}
}
