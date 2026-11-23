package watcher

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPriorityPatterns(t *testing.T) {
	vaultDir := t.TempDir()
	cfgYAML := "other:\n  priority_rules:\n    - ignored\nattention:\n  priority_rules: # header comment\n    - \"Sec #1 Alert\" # inline comment\n    - 'Board@Example.COM'\n    - \"\"\n    - urgent-wire\n  sibling_key:\n    - should-not-match\n"
	if err := os.WriteFile(filepath.Join(vaultDir, "config.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	got := loadPriorityPatterns(vaultDir)
	want := []string{"sec #1 alert", "board@example.com", "urgent-wire"}
	if len(got) != len(want) {
		t.Fatalf("loadPriorityPatterns = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("loadPriorityPatterns[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	inlineYAML := "attention:\n  priority_rules: [\"VIP #1\", 'Term Sheet']\n"
	if err := os.WriteFile(filepath.Join(vaultDir, "config.yaml"), []byte(inlineYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	gotInline := loadPriorityPatterns(vaultDir)
	wantInline := []string{"vip #1", "term sheet"}
	if len(gotInline) != len(wantInline) || gotInline[0] != wantInline[0] || gotInline[1] != wantInline[1] {
		t.Fatalf("inline loadPriorityPatterns = %v, want %v", gotInline, wantInline)
	}
}
