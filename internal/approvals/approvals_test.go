package approvals

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateApproveAndReject(t *testing.T) {
	vaultDir := t.TempDir()
	prop, err := Create(vaultDir, "test_echo", "Echo test message", "echo approved-ok", "", "conv-1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if prop.ExpiresAt == "" {
		t.Fatalf("expected default 24h ExpiresAt on new proposal")
	}

	list, err := List(vaultDir)
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 pending proposal, got %d (%v)", len(list), err)
	}

	// Wrong SHA prefix must fail
	if _, err := Approve(vaultDir, prop.ID, "000000000000"); err == nil {
		t.Fatalf("expected error on mismatched sha256 prefix")
	}

	// Empty or short SHA prefix must fail
	if _, err := Approve(vaultDir, prop.ID, ""); err == nil {
		t.Fatalf("expected error on empty sha256 prefix")
	}
	if _, err := Approve(vaultDir, prop.ID, prop.SHA256[:4]); err == nil {
		t.Fatalf("expected error on short (<8 char) sha256 prefix")
	}

	// Path traversal proposal ID must fail on Approve and Reject
	if _, err := Approve(vaultDir, "../../outside", prop.SHA256[:12]); err == nil {
		t.Fatalf("expected error on path traversal proposal ID in Approve")
	}
	if err := Reject(vaultDir, "../../outside", "nope"); err == nil {
		t.Fatalf("expected error on path traversal proposal ID in Reject")
	}

	// Valid SHA prefix must execute command and remove from pending
	out, err := Approve(vaultDir, prop.ID, prop.SHA256[:12])
	if err != nil {
		t.Fatalf("Approve failed: %v", err)
	}
	if !strings.Contains(out, "approved-ok") {
		t.Fatalf("unexpected command output: %q", out)
	}

	listAfter, _ := List(vaultDir)
	if len(listAfter) != 0 {
		t.Fatalf("expected 0 pending proposals after approval, got %d", len(listAfter))
	}
}

func TestProposalTTLExpirationAndContextualPIIScan(t *testing.T) {
	vaultDir := t.TempDir()
	privDir := filepath.Join(vaultDir, "private")
	_ = os.MkdirAll(privDir, 0o700)
	_ = os.WriteFile(filepath.Join(privDir, "pii-denylist.txt"), []byte("ProjectNightfall\n"), 0o600)

	pastExp := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	prop, err := CreateWithMetadata(vaultDir, "gmail_send", "Send contract draft", "echo never-run", "", "conv-ttl", map[string]string{
		"expiresAt":    pastExp,
		"draftPayload": "Includes ProjectNightfall, SSN 123-45-6789, and 742 Evergreen Avenue.",
	})
	if err != nil {
		t.Fatalf("CreateWithMetadata: %v", err)
	}
	if prop.RiskTier != "pii_warning" || len(prop.RiskFlags) < 3 {
		t.Fatalf("expected pii_warning and >=3 RiskFlags, got tier=%q flags=%v", prop.RiskTier, prop.RiskFlags)
	}

	// Attempting to approve an expired proposal must fail and record "expired"
	if _, err := ApproveProposal(vaultDir, prop.ID, prop.SHA256[:12]); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired error on ApproveProposal, got: %v", err)
	}

	// Verify interventions.jsonl recorded Decision="expired"
	interBytes, err := os.ReadFile(filepath.Join(vaultDir, "memory", "interventions.jsonl"))
	if err != nil || !strings.Contains(string(interBytes), `"decision":"expired"`) {
		t.Fatalf("expected expired record in interventions.jsonl, got: %s (%v)", string(interBytes), err)
	}
}
