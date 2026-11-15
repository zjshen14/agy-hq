package taint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestMarkMonotonicLedgerAndFailClosed(t *testing.T) {
	vaultDir := t.TempDir()
	convID := "conv-watch-123"

	if _, ok := Check(vaultDir, convID); ok {
		t.Fatalf("expected clean session before Mark")
	}

	// Concurrent Mark calls must never clobber or race
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := Mark(context.Background(), vaultDir, Record{
				ConvID:    convID,
				Source:    "watch:gmail:primary",
				Connector: "gmail",
				Account:   "primary",
				ItemKeys:  []string{"thr-" + string(rune('a'+idx))},
			})
			if err != nil {
				t.Errorf("Mark(%d): %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	rec, tainted := Check(vaultDir, convID)
	if !tainted {
		t.Fatalf("expected session to be tainted")
	}
	if len(rec.ItemKeys) != 8 {
		t.Fatalf("expected all 8 merged ItemKeys, got %v", rec.ItemKeys)
	}

	// Verify marker file is 0600 and ledger has 8 lines
	mp := markerPath(vaultDir, convID)
	st, err := os.Stat(mp)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 marker file, got st=%v err=%v", st, err)
	}
	ledgerBytes, err := os.ReadFile(ledgerPath(vaultDir))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(string(ledgerBytes)), "\n"); len(lines) != 8 {
		t.Fatalf("expected 8 ledger entries, got %d", len(lines))
	}

	// Idempotent re-mark with unchanged ItemKeys must NOT append duplicate lines to ledger.jsonl
	if _, err := Mark(context.Background(), vaultDir, Record{
		ConvID:    convID,
		Source:    "watch:gmail:primary",
		Connector: "gmail",
		Account:   "primary",
		ItemKeys:  []string{"thr-a"},
	}); err != nil {
		t.Fatalf("idempotent Mark: %v", err)
	}
	ledgerBytesAfterNoop, _ := os.ReadFile(ledgerPath(vaultDir))
	if lines := strings.Split(strings.TrimSpace(string(ledgerBytesAfterNoop)), "\n"); len(lines) != 8 {
		t.Fatalf("expected idempotent Mark to skip duplicate ledger append, got %d lines", len(lines))
	}

	// Removing <sha256>.json marker must STILL report tainted via append-only ledger.jsonl failover
	if err := os.Remove(mp); err != nil {
		t.Fatal(err)
	}
	if ledRec, ledOK := Check(vaultDir, convID); !ledOK || len(ledRec.ItemKeys) != 8 {
		t.Fatalf("expected ledger.jsonl failover to reconstruct tainted record, got ok=%v rec=%+v", ledOK, ledRec)
	}

	// Corrupting marker JSON must fail CLOSED (still report tainted = true!)
	if err := os.WriteFile(mp, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if corrRec, corrOK := Check(vaultDir, convID); !corrOK || !strings.HasPrefix(corrRec.Source, "fail_closed:") {
		t.Fatalf("expected corrupt marker to fail closed, got ok=%v rec=%+v", corrOK, corrRec)
	}

	// Legacy tainted_sessions.json fallback + corrupt legacy fail-closed
	vault2 := t.TempDir()
	_ = os.MkdirAll(filepath.Join(vault2, "memory"), 0o755)
	_ = os.WriteFile(filepath.Join(vault2, "memory", "tainted_sessions.json"), []byte(`{"conv-legacy":"gmail:thread:t9"}`), 0o600)
	if _, ok := IsTainted(vault2, "conv-legacy"); !ok {
		t.Fatalf("expected legacy tainted_sessions.json fallback to detect conv-legacy")
	}
	_ = os.WriteFile(filepath.Join(vault2, "memory", "tainted_sessions.json"), []byte(`{bad-json`), 0o600)
	if _, ok := IsTainted(vault2, "conv-clean"); !ok {
		t.Fatalf("expected corrupt legacy tainted_sessions.json to fail closed")
	}

	// Strict caller env rejects spoofed --conv when HQ_STRICT_CALLER_ENV=1 and ANTIGRAVITY_CONVERSATION_ID is unset
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "")
	t.Setenv("HQ_STRICT_CALLER_ENV", "1")
	if _, err := ResolveCallerConvID(vaultDir, "spoofed-conv"); err == nil {
		t.Fatalf("expected ResolveCallerConvID to reject spoofed flag when HQ_STRICT_CALLER_ENV=1")
	}
}
