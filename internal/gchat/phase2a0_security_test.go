package gchat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestGChatCreateOrUpdateDraftRejectsPathTraversal(t *testing.T) {
	vaultDir := t.TempDir()
	t.Setenv("HQ_STRICT_CALLER_ENV", "0")

	if _, err := InitOrAddAccount(vaultDir, "../../escaped", "user@gmail.com", "mock", "", "", "", "", "", "", true); err == nil {
		t.Fatal("expected InitOrAddAccount to reject traversal alias")
	}
	if _, err := InitOrAddAccount(vaultDir, "personal", "user@gmail.com", "mock", "", "", "", "", "", "", true); err != nil {
		t.Fatalf("InitOrAddAccount: %v", err)
	}
	mDir := filepath.Join(vaultDir, "memory", "gchat", "mock")
	if err := os.MkdirAll(mDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveSpaceAccess(vaultDir, "../../escaped", "spaces/AAA1", "Room", nil); err == nil {
		t.Fatal("expected saveSpaceAccess to reject traversal account")
	}

	for _, bad := range []string{"../../escaped-gchat", "../foo", "a/b", `a\b`, ".hidden", "bad..id"} {
		if _, err := CreateOrUpdateDraft(vaultDir, "personal", bad, "spaces/AAA1", "Room", "", "hello", ""); err == nil {
			t.Fatalf("expected CreateOrUpdateDraft to reject traversal draftID %q", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "memory", "escaped-gchat.json")); !os.IsNotExist(err) {
		t.Fatal("escaped-gchat.json must not be created via draftID traversal")
	}

	// VerifyAndSendDraft normalizes whitespace-padded draftID to cleanID in non-mock output
	t.Setenv("HQ_GCHAT_MOCK_DIR", "")
	_ = os.RemoveAll(mDir)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	whFile := filepath.Join(vaultDir, "private", "gchat", "webhooks", "wh.url")
	if err := os.MkdirAll(filepath.Dir(whFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(whFile, []byte(srv.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitOrAddAccount(vaultDir, "whacct", "user@acme.com", "webhook", "", "", "", "", "", whFile, false); err != nil {
		t.Fatalf("InitOrAddAccount webhook: %v", err)
	}
	if err := saveCachedSpaces(vaultDir, "whacct", []SpaceSummary{{SpaceID: "spaces/AAA1", Account: "whacct", DisplayName: "Room", SpaceType: "SPACE"}}); err != nil {
		t.Fatal(err)
	}
	spec, err := CreateOrUpdateDraft(vaultDir, "whacct", " gchat-draft-clean1 ", "spaces/AAA1", "Room", "", "hello", "")
	if err != nil {
		t.Fatalf("CreateOrUpdateDraft: %v", err)
	}
	if spec.DraftID != "gchat-draft-clean1" {
		t.Fatalf("expected trimmed DraftID gchat-draft-clean1, got %q", spec.DraftID)
	}
	sentMsg, err := VerifyAndSendDraft(vaultDir, "whacct", " gchat-draft-clean1 ", spec.ContentHash[:12])
	if err != nil {
		t.Fatalf("VerifyAndSendDraft: %v", err)
	}
	if !strings.HasPrefix(sentMsg, "Sent Google Chat draft gchat-draft-clean1 ") {
		t.Fatalf("expected normalized cleanID in sentMsg, got %q", sentMsg)
	}
}
