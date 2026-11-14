package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/version"
)

func TestVaultResolutionPrecedenceAndStandaloneTemplates(t *testing.T) {
	origHome := os.Getenv("HOME")
	origHQHome := os.Getenv("HQ_HOME_DIR")
	origVault := os.Getenv("HQ_VAULT_DIR")
	origRoot := os.Getenv("HQ_ROOT_DIR")
	origWD, _ := os.Getwd()
	t.Cleanup(func() {
		_ = os.Setenv("HOME", origHome)
		_ = os.Setenv("HQ_HOME_DIR", origHQHome)
		_ = os.Setenv("HQ_VAULT_DIR", origVault)
		_ = os.Setenv("HQ_ROOT_DIR", origRoot)
		_ = os.Chdir(origWD)
	})

	fakeHome := t.TempDir()
	_ = os.Setenv("HOME", fakeHome)
	_ = os.Setenv("HQ_HOME_DIR", fakeHome)
	_ = os.Unsetenv("HQ_VAULT_DIR")
	_ = os.Unsetenv("HQ_ROOT_DIR")

	// 1. External Go repository that happens to contain a `./local` folder for app localization
	// must NOT be mistaken for an agy-hq Vault!
	extRepo := filepath.Join(t.TempDir(), "webapp")
	_ = os.MkdirAll(filepath.Join(extRepo, "local", "en-US"), 0o755)
	_ = os.WriteFile(filepath.Join(extRepo, "go.mod"), []byte("module example.com/webapp\n\ngo 1.25.0\n"), 0o644)
	if err := os.Chdir(extRepo); err != nil {
		t.Fatalf("chdir extRepo: %v", err)
	}
	if vault.IsAgyHQRepo(extRepo) {
		t.Fatalf("expected IsAgyHQRepo=false for external Go module")
	}

	// Tier 6 fallback: canonical global vault (~/.gemini/antigravity/hq/vault)
	gotVault, gotSrc, err := vault.ResolveVaultDirWithSource()
	if err != nil {
		t.Fatalf("ResolveVaultDirWithSource: %v", err)
	}
	wantGlobal := filepath.Join(fakeHome, ".gemini", "antigravity", "hq", "vault")
	if gotVault != wantGlobal || gotSrc != "global-default" {
		t.Fatalf("expected (%s, global-default), got (%s, %s)", wantGlobal, gotVault, gotSrc)
	}

	// 2. Standalone embedded template Init + Migrate (templateDir == "", zero files on disk)
	created, err := vault.Init(wantGlobal, "")
	if err != nil {
		t.Fatalf("standalone vault.Init failed: %v", err)
	}
	if len(created) == 0 {
		t.Fatalf("expected embedded templates to seed files in %s", wantGlobal)
	}
	// Seed a v7 triggers.yaml with legacy ./bin/hq precheck_cmd and verify v8 migration upgrades it
	legacyTriggers := "schedules:\n  - id: gmail-watch\n    cron: \"*/5 * * * *\"\n    execution_mode: daemon_gatekeeper\n    precheck_cmd: [\"./bin/hq\", \"gmail\", \"precheck\"]\n"
	_ = os.WriteFile(filepath.Join(wantGlobal, "triggers.yaml"), []byte(legacyTriggers), 0o644)
	_ = os.WriteFile(filepath.Join(wantGlobal, "memory", "schema_version.json"), []byte(`{"version":7}`), 0o644)

	rep, err := vault.Migrate(wantGlobal, "")
	if err != nil {
		t.Fatalf("standalone vault.Migrate failed: %v", err)
	}
	if rep.CurrentVersion != vault.CurrentSchemaVersion {
		t.Fatalf("expected CurrentVersion=%d, got %d", vault.CurrentSchemaVersion, rep.CurrentVersion)
	}
	trigAfter, _ := os.ReadFile(filepath.Join(wantGlobal, "triggers.yaml"))
	if strings.Contains(string(trigAfter), "./bin/hq") || !strings.Contains(string(trigAfter), `"hq"`) {
		t.Fatalf("expected v8 migration to upgrade ./bin/hq -> hq in triggers.yaml, got:\n%s", string(trigAfter))
	}

	// Forward-compatibility guard: schema v99 must refuse cleanly instead of downgrading
	_ = os.WriteFile(filepath.Join(wantGlobal, "memory", "schema_version.json"), []byte(`{"version":99}`), 0o644)
	if _, fErr := vault.Migrate(wantGlobal, ""); fErr == nil || !strings.Contains(fErr.Error(), "newer than this hq binary") {
		t.Fatalf("expected forward-compat error on v99 schema, got: %v", fErr)
	}
	_ = os.WriteFile(filepath.Join(wantGlobal, "memory", "schema_version.json"), []byte(`{"version":8}`), 0o644)

	// 3. Tier 5: ~/.gemini/antigravity/hq/config.json pointer
	customVault := filepath.Join(t.TempDir(), "custom-vault")
	_, _ = vault.Init(customVault, "")
	if err := vault.SaveGlobalConfig(vault.GlobalConfig{VaultDir: customVault}); err != nil {
		t.Fatalf("SaveGlobalConfig: %v", err)
	}
	gotVault, gotSrc, _ = vault.ResolveVaultDirWithSource()
	if gotVault != customVault || gotSrc != "global-config" {
		t.Fatalf("expected (%s, global-config), got (%s, %s)", customVault, gotVault, gotSrc)
	}

	// 4. Tier 3: <Vault>/worktrees/<slug> ancestor resolution
	wtDir := filepath.Join(wantGlobal, "worktrees", "webapp-feature", "pkg")
	_ = os.MkdirAll(wtDir, 0o755)
	_ = os.Chdir(wtDir)
	gotVault, gotSrc, _ = vault.ResolveVaultDirWithSource()
	if gotVault != wantGlobal || gotSrc != "worktree-ancestor" {
		t.Fatalf("expected (%s, worktree-ancestor), got (%s, %s)", wantGlobal, gotVault, gotSrc)
	}

	// 5. Tier 1: HQ_VAULT_DIR wins unconditionally
	overrideVault := filepath.Join(t.TempDir(), "env-vault")
	_ = os.Setenv("HQ_VAULT_DIR", overrideVault)
	gotVault, gotSrc, _ = vault.ResolveVaultDirWithSource()
	if gotVault != overrideVault || gotSrc != "env:HQ_VAULT_DIR" {
		t.Fatalf("expected (%s, env:HQ_VAULT_DIR), got (%s, %s)", overrideVault, gotVault, gotSrc)
	}
}

func TestValidateBundleMutationsAndSkewProbe(t *testing.T) {
	plugDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(plugDir, "rules"), 0o755)
	_ = os.MkdirAll(filepath.Join(plugDir, "skills", "morning-brief"), 0o755)
	_ = os.MkdirAll(filepath.Join(plugDir, "sidecars", "agy-hq-console"), 0o755)

	writeValid := func() {
		manifest, _ := json.Marshal(map[string]interface{}{
			"name":        "agy-hq",
			"version":     version.Version,
			"description": "Test bundle",
		})
		_ = os.WriteFile(filepath.Join(plugDir, "plugin.json"), manifest, 0o644)
		hooksJSON := `{
  "hq-kernel": {
    "SessionStart": [{"type":"command","command":"P=\"${PLUGIN_ROOT:-.}\"; hq hook session-start"}],
    "PreToolUse": [{"matcher":"run_command|edit_file|write_file|read_file|grep_search|find_by_name|invoke_subagent|manage_subagents|schedule|manage_task","hooks":[{"type":"command","command":"hq hook pre-tool"}]}],
    "Stop": [{"type":"command","command":"hq hook stop"}]
  }
}`
		_ = os.WriteFile(filepath.Join(plugDir, "hooks.json"), []byte(hooksJSON), 0o644)
		_ = os.WriteFile(filepath.Join(plugDir, "rules", "AGENTS.md"), []byte("# Global Rules\nPlain markdown.\n"), 0o644)
		_ = os.WriteFile(filepath.Join(plugDir, "skills", "morning-brief", "SKILL.md"), []byte("---\nname: morning-brief\ndescription: brief\n---\nRun `hq status`.\n"), 0o644)
		_ = os.WriteFile(filepath.Join(plugDir, "sidecars", "agy-hq-console", "sidecar.json"), []byte(`{"command":"node","args":["main.mjs"]}`), 0o644)
		_ = os.WriteFile(filepath.Join(plugDir, "sidecars", "agy-hq-console", "main.mjs"), []byte("// sidecar\n"), 0o644)
		_ = os.Remove(filepath.Join(plugDir, "sidecars.json"))
	}

	writeValid()
	if err := ValidateBundle(plugDir, true); err != nil {
		t.Fatalf("expected clean bundle to pass ValidateBundle, got: %v", err)
	}

	// Mutation 1: SemVer mismatch
	_ = os.WriteFile(filepath.Join(plugDir, "plugin.json"), []byte(`{"name":"agy-hq","version":"9.9.9","description":"x"}`), 0o644)
	if err := ValidateBundle(plugDir, true); err == nil || !strings.Contains(err.Error(), "!=") {
		t.Fatalf("expected version mismatch error, got: %v", err)
	}

	// Mutation 2: Forbidden sidecars.json
	writeValid()
	_ = os.WriteFile(filepath.Join(plugDir, "sidecars.json"), []byte(`{}`), 0o644)
	if err := ValidateBundle(plugDir, true); err == nil || !strings.Contains(err.Error(), "sidecars.json") {
		t.Fatalf("expected sidecars.json error, got: %v", err)
	}

	// Mutation 3: YAML frontmatter in rules/AGENTS.md
	writeValid()
	_ = os.WriteFile(filepath.Join(plugDir, "rules", "AGENTS.md"), []byte("---\ntitle: bad\n---\n# Rules\n"), 0o644)
	if err := ValidateBundle(plugDir, true); err == nil || !strings.Contains(err.Error(), "YAML frontmatter") {
		t.Fatalf("expected YAML frontmatter error on rules/AGENTS.md, got: %v", err)
	}

	// Mutation 4: ./bin/hq in SKILL.md
	writeValid()
	_ = os.WriteFile(filepath.Join(plugDir, "skills", "morning-brief", "SKILL.md"), []byte("---\nname: morning-brief\ndescription: x\n---\nRun `./bin/hq status`.\n"), 0o644)
	if err := ValidateBundle(plugDir, true); err == nil || !strings.Contains(err.Error(), "./bin/hq") {
		t.Fatalf("expected ./bin/hq error in SKILL.md, got: %v", err)
	}

	// Verify probeDaemonHealth detects skewed older daemon
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":             "ok",
			"version":            "0.0.1",
			"commit":             "oldbeef",
			"vaultSchemaVersion": 7,
		})
	}))
	defer srv.Close()

	origURL := os.Getenv("HQ_DAEMON_HEALTH_URL")
	_ = os.Setenv("HQ_DAEMON_HEALTH_URL", srv.URL)
	defer os.Setenv("HQ_DAEMON_HEALTH_URL", origURL)

	ok, ver, commit := ProbeDaemonHealth("")
	if !ok || ver != "0.0.1" || commit != "oldbeef" {
		t.Fatalf("unexpected ProbeDaemonHealth result: ok=%v ver=%q commit=%q", ok, ver, commit)
	}
}

func TestAttachDetachProjectPluginAndSanitizedDaemonEnv(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HQ_HOME_DIR", fakeHome)
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "sidecar-project-uuid")
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "conv-sidecar-1")
	t.Setenv("ANTIGRAVITY_SIDECAR_WEB_PORT", "54321")
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:43701")

	env := SanitizedDaemonEnv()
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "ANTIGRAVITY_PROJECT_ID=") ||
		strings.Contains(joined, "ANTIGRAVITY_CONVERSATION_ID=") ||
		strings.Contains(joined, "ANTIGRAVITY_SIDECAR_WEB_PORT=") {
		t.Fatalf("expected sidecar/session env vars to be stripped, got:\n%s", joined)
	}
	if !strings.Contains(joined, "ANTIGRAVITY_LS_ADDRESS=localhost:43701") {
		t.Fatalf("expected ANTIGRAVITY_LS_ADDRESS to be preserved in SanitizedDaemonEnv")
	}

	// 1. Fresh standalone project gets .agents/plugins.json preserving any existing entries
	projDir := filepath.Join(t.TempDir(), "my-fresh-hq")
	_ = os.MkdirAll(filepath.Join(projDir, ".agents"), 0o755)
	_ = os.WriteFile(filepath.Join(projDir, ".agents", "plugins.json"), []byte(`{"entries":[{"path":"tools/plugins"}]}`), 0o644)

	mPath, err := AttachProjectPlugin(projDir, "")
	if err != nil || mPath == "" {
		t.Fatalf("AttachProjectPlugin failed: path=%q err=%v", mPath, err)
	}
	raw, _ := os.ReadFile(mPath)
	if !strings.Contains(string(raw), `"tools/plugins"`) || !strings.Contains(string(raw), `"agy-hq"`) {
		t.Fatalf("expected both existing entry and agy-hq in plugins.json, got:\n%s", string(raw))
	}

	// 2. Detaching preserves other entries; detaching from a single-entry file removes plugins.json cleanly
	removed, err := DetachProjectPlugin(projDir)
	if err != nil || !removed {
		t.Fatalf("DetachProjectPlugin failed: removed=%v err=%v", removed, err)
	}
	rawAfter, _ := os.ReadFile(mPath)
	if strings.Contains(string(rawAfter), `"agy-hq"`) || !strings.Contains(string(rawAfter), `"tools/plugins"`) {
		t.Fatalf("expected agy-hq removed while keeping tools/plugins, got:\n%s", string(rawAfter))
	}

	soloProj := filepath.Join(t.TempDir(), "solo-hq")
	soloManifest, err := AttachProjectPlugin(soloProj, "")
	if err != nil || soloManifest == "" {
		t.Fatalf("AttachProjectPlugin on soloProj failed: %v", err)
	}
	if ok, err := DetachProjectPlugin(soloProj); err != nil || !ok {
		t.Fatalf("DetachProjectPlugin on soloProj failed: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(soloManifest); !os.IsNotExist(err) {
		t.Fatalf("expected empty solo plugins.json to be deleted on detach")
	}
}
