package approvals

import (
	"strings"
	"testing"
)

func TestCreateApproveAndReject(t *testing.T) {
	vaultDir := t.TempDir()
	prop, err := Create(vaultDir, "test_echo", "Echo test message", "echo approved-ok", "", "conv-1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
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
