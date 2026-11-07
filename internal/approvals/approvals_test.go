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
