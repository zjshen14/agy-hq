package kernel

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestResolveOwningTask(t *testing.T) {
	vaultDir := t.TempDir()
	const (
		deskConv  = "00000000-0000-0000-0000-0000000000fd"
		convA     = "00000000-0000-0000-0000-00000000000a"
		convB     = "00000000-0000-0000-0000-00000000000b"
		child     = "00000000-0000-0000-0000-0000000000c1"
		grandkid  = "00000000-0000-0000-0000-0000000000c2"
		deskChild = "00000000-0000-0000-0000-0000000000d1"
		cycleX    = "00000000-0000-0000-0000-0000000000e1"
		cycleY    = "00000000-0000-0000-0000-0000000000e2"
	)
	if err := SetFrontDesk(vaultDir, deskConv, filepath.Join(vaultDir, "art")); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}
	for _, task := range []ActiveTask{
		{TaskSlug: "ledger-fix", Title: "[Task:acme] Ledger fix", Kind: "task", ConversationID: convA, Status: "active"},
		{TaskSlug: "other-task", Title: "[Task:acme] Other", Kind: "task", ConversationID: convB, Status: "active"},
		{TaskSlug: "desk-entry", Title: "[Task:acme] Desk entry", Kind: "task", ConversationID: deskConv, Status: "active"},
	} {
		if err := RegisterTask(vaultDir, task); err != nil {
			t.Fatalf("RegisterTask(%s): %v", task.TaskSlug, err)
		}
	}
	nested := [][2]string{
		{child, convA},
		{grandkid, child},
		{deskChild, deskConv},
		{cycleX, cycleY},
		{cycleY, cycleX},
	}
	// A chain long enough to exceed the 8-hop bound: chain-0 -> chain-1 -> ... -> chain-7 -> convB.
	chain := func(i int) string { return fmt.Sprintf("00000000-0000-0000-0000-0000000001%02d", i) }
	for i := 0; i < 8; i++ {
		parent := chain(i + 1)
		if i == 7 {
			parent = convB
		}
		nested = append(nested, [2]string{chain(i), parent})
	}
	for _, link := range nested {
		if err := RecordNestedConversation(vaultDir, link[0], link[1]); err != nil {
			t.Fatalf("RecordNestedConversation(%s -> %s): %v", link[0], link[1], err)
		}
	}

	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "")
	cases := []struct {
		name     string
		input    string
		wantSlug string
	}{
		{"direct slug", "ledger-fix", "ledger-fix"},
		{"direct conversation", convA, "ledger-fix"},
		{"padded input", "  " + convB + "  ", "other-task"},
		{"nested child to parent task", child, "ledger-fix"},
		{"two-hop nesting", grandkid, "ledger-fix"},
		{"seven parent links stays within the bound", chain(1), "other-task"},
		{"eight parent links exceeds the bound", chain(0), ""},
		{"front desk", deskConv, ""},
		{"front desk child never walks through the desk", deskChild, ""},
		{"task entry owned by the front desk", "desk-entry", ""},
		{"cycle", cycleX, ""},
		{"unknown", "00000000-0000-0000-0000-00000000ffff", ""},
		{"empty without env", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ResolveOwningTask(vaultDir, tc.input)
			if tc.wantSlug == "" {
				if ok {
					t.Fatalf("ResolveOwningTask(%q) = %s, want no owner", tc.input, got.TaskSlug)
				}
				return
			}
			if !ok || got.TaskSlug != tc.wantSlug {
				t.Fatalf("ResolveOwningTask(%q) = %q (%v), want %q", tc.input, got.TaskSlug, ok, tc.wantSlug)
			}
		})
	}

	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", grandkid)
	if got, ok := ResolveOwningTask(vaultDir, ""); !ok || got.TaskSlug != "ledger-fix" {
		t.Fatalf("env fallback = %q (%v), want ledger-fix", got.TaskSlug, ok)
	}
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", deskConv)
	if got, ok := ResolveOwningTask(vaultDir, ""); ok {
		t.Fatalf("env fallback to the Front Desk = %q, want no owner", got.TaskSlug)
	}
	if _, ok := ResolveOwningTask(t.TempDir(), "ledger-fix"); ok {
		t.Fatal("expected no owner in an empty Vault")
	}
}
