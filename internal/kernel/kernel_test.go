package kernel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyInboxAndHeartbeatAndCircuitBreaker(t *testing.T) {
	if got := ClassifyInboxFile("chase-2026-11.csv"); got != "ledger-ingest" {
		t.Fatalf("expected ledger-ingest, got %s", got)
	}
	if got := ClassifyInboxFile("2026-W2-employer.pdf"); got != "tax-prep" {
		t.Fatalf("expected tax-prep, got %s", got)
	}
	if got := ClassifyInboxFile("NVDA-10-Q-Q3.pdf"); got != "thesis-check" {
		t.Fatalf("expected thesis-check, got %s", got)
	}
	if got := ClassifyInboxFile("random-article-notes.md"); got != "inbox-triage" {
		t.Fatalf("expected inbox-triage fallback, got %s", got)
	}

	vaultDir := t.TempDir()
	// 1. Empty heartbeat should cost 0 tokens and record HEARTBEAT_OK
	status, err := RunHeartbeat(vaultDir, true)
	if err != nil || !strings.Contains(status, "HEARTBEAT_OK (0 LLM tokens)") {
		t.Fatalf("unexpected empty heartbeat status: %q (%v)", status, err)
	}

	// 2. Drop a CSV file in local/inbox/ and run heartbeat in dry-run mode
	inboxDir := filepath.Join(vaultDir, "inbox")
	_ = os.MkdirAll(inboxDir, 0o755)
	_ = os.WriteFile(filepath.Join(inboxDir, "stmt.csv"), []byte("date,amount\n"), 0o644)

	status2, err := RunHeartbeat(vaultDir, true)
	if err != nil || !strings.Contains(status2, "stmt.csv -> /ledger-ingest") {
		t.Fatalf("expected stmt.csv routed to /ledger-ingest, got: %q (%v)", status2, err)
	}

	q := LoadQuota(vaultDir)
	if q.SpawnsToday != 1 {
		t.Fatalf("expected SpawnsToday == 1, got %d", q.SpawnsToday)
	}

	// 3. Verify smart model + depth routing and user overrides
	if m, tier, sub := ResolveModel("Summarize the status of open PRs", "", "", ""); m != "gemini-3.8-flash-low" || tier != "quick" || sub != "flash_lite" {
		t.Fatalf("unexpected quick tier routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("Implement the CSV parser for bank statements", "", "", ""); m != "gemini-3.8-flash-high" || tier != "standard" || sub != "flash" {
		t.Fatalf("unexpected standard tier routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, tier, sub := ResolveModel("Perform a security audit and architecture review", "", "", ""); m != "gemini-4-argon-high" || tier != "deep" || sub != "pro" {
		t.Fatalf("unexpected deep tier routing: model=%s tier=%s sub=%s", m, tier, sub)
	}
	if m, _, _ := ResolveModel("Fix a bug", "opus", "medium", ""); m != "claude-opus-5-5-medium" {
		t.Fatalf("unexpected explicit user override: got %s", m)
	}
}
