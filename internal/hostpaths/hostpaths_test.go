package hostpaths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveHermeticAndEnterpriseRelocation(t *testing.T) {
	tmp := t.TempDir()
	fakeHome := filepath.Join(tmp, "home")
	fakeVault := filepath.Join(tmp, "vault")
	fakeXDG := filepath.Join(tmp, "xdg")
	_ = os.MkdirAll(fakeHome, 0o700)
	_ = os.MkdirAll(fakeVault, 0o700)

	t.Setenv("HQ_HOME_DIR", fakeHome)
	t.Setenv("XDG_CONFIG_HOME", fakeXDG)
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", "")
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", "")
	t.Setenv("HQ_BRAIN_DIR", "")

	// 1. Default layout under fakeHome with hermetic temp-vault brain isolation
	r1 := Resolve(fakeVault)
	if r1.AppDataDir != filepath.Join(fakeHome, ".gemini", "antigravity") {
		t.Fatalf("AppDataDir = %q", r1.AppDataDir)
	}
	if r1.PluginsDir != filepath.Join(fakeHome, ".gemini", "config", "plugins") {
		t.Fatalf("PluginsDir = %q", r1.PluginsDir)
	}
	if r1.BrainDir != filepath.Join(tmp, ".brain") {
		t.Fatalf("expected hermetic temp brain dir, got %q", r1.BrainDir)
	}

	// 2. Temp test env must NOT write roots.json by default
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", filepath.Join(tmp, "corp-appdata"))
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", filepath.Join(tmp, "corp-config"))
	if err := PersistFromLiveEnv(fakeVault); err != nil {
		t.Fatalf("PersistFromLiveEnv: %v", err)
	}
	rootsFile := filepath.Join(fakeXDG, "agy-hq", "roots.json")
	if _, err := os.Stat(rootsFile); !os.IsNotExist(err) {
		t.Fatalf("expected hermetic test env not to persist roots.json without opt-in")
	}

	// 3. Explicit test opt-in verifies enterprise roots.json persistence (0600) & daemon fallback
	t.Setenv("HQ_ALLOW_TEST_ROOTS_CACHE", "1")
	if err := PersistFromLiveEnv(fakeVault); err != nil {
		t.Fatalf("PersistFromLiveEnv with opt-in: %v", err)
	}
	st, err := os.Stat(rootsFile)
	if err != nil {
		t.Fatalf("stat roots.json: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("roots.json perm = %o, want 0600", st.Mode().Perm())
	}

	// Simulate background CLI/daemon where ANTIGRAVITY_* env vars are unset
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", "")
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", "")
	r2 := Resolve("")
	if r2.AppDataDir != filepath.Join(tmp, "corp-appdata") {
		t.Fatalf("expected persisted AppDataDir, got %q", r2.AppDataDir)
	}
	if r2.PluginsDir != filepath.Join(tmp, "corp-config", "plugins") {
		t.Fatalf("expected persisted PluginsDir, got %q", r2.PluginsDir)
	}
}
