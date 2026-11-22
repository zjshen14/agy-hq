package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfile_CRUDAndSecretGuardAndForgetCascade(t *testing.T) {
	vaultDir := t.TempDir()

	// 1. Valid Tier-2 autofill fields succeed (explicit section + shorthand section.key)
	if _, err := SetProfileField(vaultDir, "self", "phone", "408-555-0199"); err != nil {
		t.Fatalf("SetProfileField phone: %v", err)
	}
	if _, err := SetProfileField(vaultDir, "", "addresses.home", "1234 Oak St, Santa Clara, CA 95051"); err != nil {
		t.Fatalf("SetProfileField shorthand address: %v", err)
	}
	if _, err := SetProfileField(vaultDir, "household", "spouse_name", "Alex Example"); err != nil {
		t.Fatalf("SetProfileField household: %v", err)
	}
	if _, err := SetProfileField(vaultDir, "travel", "ktn", "TT987654321"); err != nil {
		t.Fatalf("SetProfileField ktn: %v", err)
	}

	// Verify 0600 permissions
	info, err := os.Stat(ProfilePath(vaultDir))
	if err != nil {
		t.Fatalf("stat profile.md: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 perms on profile.md, got %o", info.Mode().Perm())
	}

	// Reload and verify values
	prof, err := LoadProfile(vaultDir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if prof.Sections["self"]["phone"] != "408-555-0199" {
		t.Fatalf("unexpected phone: %+v", prof.Sections)
	}
	if prof.Sections["addresses"]["home"] != "1234 Oak St, Santa Clara, CA 95051" {
		t.Fatalf("unexpected home address: %+v", prof.Sections)
	}
	if prof.Sections["household"]["spouse_name"] != "Alex Example" {
		t.Fatalf("unexpected spouse_name: %+v", prof.Sections)
	}

	// 2. Verify private/profile.md is unconditionally excluded from BM25 `SearchMemory`
	hits, err := SearchMemory(vaultDir, SearchOptions{Query: "1234 Oak St Santa Clara", Limit: 5})
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected private/profile.md to be excluded from SearchMemory, got %+v", hits)
	}

	// 3. Verify Tier-3 secret guard rejects passwords, SSNs, Luhn payment cards, API tokens, and PEM keys
	badCases := []struct {
		sec, key, val string
	}{
		{"self", "bank_password", "hunter2"},
		{"self", "ssn", "123-45-6789"},
		{"self", "note", "My tax ID is 123-45-6789"},
		{"self", "card", "4532 0151 1283 0366"}, // valid Luhn Visa test number
		{"self", "credit_card", "1111"},
		{"self", "token", "abc123"},
		{"self", "note", "sk-" + "abcdefghijklmnopqrstuvwx"},
		{"self", "note", "-----BEGIN " + "RSA PRIVATE KEY-----"},
		{"self", "passport_number", "A12345678"},
	}
	for _, tc := range badCases {
		if _, err := SetProfileField(vaultDir, tc.sec, tc.key, tc.val); err == nil {
			t.Fatalf("expected Tier-3 secret guard to reject (%s.%s=%q), but it succeeded", tc.sec, tc.key, tc.val)
		}
	}

	// 4. Unset removes field cleanly
	_, removed, err := UnsetProfileField(vaultDir, "travel", "ktn")
	if err != nil || !removed {
		t.Fatalf("UnsetProfileField ktn: removed=%v err=%v", removed, err)
	}
	profAfterUnset, _ := LoadProfile(vaultDir)
	if _, exists := profAfterUnset.Sections["travel"]; exists {
		t.Fatalf("expected empty travel section pruned after unset, got %+v", profAfterUnset.Sections)
	}

	// 5. ForgetMemory cascades into private/profile.md
	rep, err := ForgetMemory(vaultDir, "1234 Oak St", "", "", false, false)
	if err != nil {
		t.Fatalf("ForgetMemory: %v", err)
	}
	foundProfileScrub := false
	for _, rel := range rep.ScrubbedFiles {
		if rel == "private/profile.md" {
			foundProfileScrub = true
		}
	}
	if !foundProfileScrub {
		t.Fatalf("expected private/profile.md in ForgetMemory ScrubbedFiles, got %+v", rep.ScrubbedFiles)
	}
	rawAfterForget, _ := os.ReadFile(ProfilePath(vaultDir))
	if strings.Contains(string(rawAfterForget), "1234 Oak St") {
		t.Fatalf("expected 1234 Oak St scrubbed from private/profile.md, got:\n%s", string(rawAfterForget))
	}

	// 6. Symlink defense: LoadProfile rejects symlinked private/profile.md
	symVault := t.TempDir()
	_ = os.MkdirAll(filepath.Join(symVault, "private"), 0o700)
	targetFile := filepath.Join(t.TempDir(), "outside.md")
	_ = os.WriteFile(targetFile, []byte("# Outside\n"), 0o600)
	_ = os.Symlink(targetFile, ProfilePath(symVault))
	if _, err := LoadProfile(symVault); err == nil {
		t.Fatalf("expected LoadProfile to reject symlinked profile.md")
	}
}
