package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/version"
)

const (
	PluginSlug          = "agy-hq"
	ScopeProject        = "project"
	ScopeGlobal         = "global"
	MaxRuleMarkdownSize = 24000
)

// RequiredPreToolMatchers enumerates the minimum tool names that hooks.json PreToolUse must intercept.
var RequiredPreToolMatchers = []string{
	"run_command",
	"edit_file",
	"write_file",
	"read_file",
	"grep_search",
	"find_by_name",
	"invoke_subagent",
	"manage_subagents",
	"schedule",
	"manage_task",
}

// InstallOptions configures `hq plugin install` (project-scoped by default; `--global` opt-in).
type InstallOptions struct {
	SourceRepoDir string
	PluginSrcDir  string
	ProjectDir    string
	BinDir        string
	VaultDir      string
	GlobalVault   bool
	GlobalScope   bool
}

// InstallReport summarizes the actions performed by `hq plugin install`.
type InstallReport struct {
	Scope                  string `json:"scope"`
	Version                string `json:"version"`
	Commit                 string `json:"commit"`
	BinaryPath             string `json:"binaryPath"`
	BinarySHA256           string `json:"binarySha256"`
	PluginDir              string `json:"pluginDir"`
	ProjectDir             string `json:"projectDir,omitempty"`
	ProjectPluginsManifest string `json:"projectPluginsManifest,omitempty"`
	RemovedGlobalPluginDir string `json:"removedGlobalPluginDir,omitempty"`
	RemovedLegacySidecar   string `json:"removedLegacySidecar,omitempty"`
	BundleDigest           string `json:"bundleDigest"`
	FileCount              int    `json:"fileCount"`
	VaultDir               string `json:"vaultDir"`
	GlobalConfigPath       string `json:"globalConfigPath"`
	BinDirOnPath           bool   `json:"binDirOnPath"`
	DaemonRestarted        bool   `json:"daemonRestarted"`
}

// StatusReport summarizes binary, project/global plugin bundle, and Vault resolution health.
type StatusReport struct {
	Scope                 string `json:"scope"`
	EngineVersion         string `json:"engineVersion"`
	EngineCommit          string `json:"engineCommit"`
	VaultSchemaVersion    int    `json:"vaultSchemaVersion"`
	VaultDir              string `json:"vaultDir"`
	VaultResolutionSource string `json:"vaultResolutionSource"`
	GlobalConfigPath      string `json:"globalConfigPath"`
	SourceRepoDir         string `json:"sourceRepoDir,omitempty"`

	GlobalBinaryPath      string `json:"globalBinaryPath"`
	GlobalBinaryInstalled bool   `json:"globalBinaryInstalled"`
	GlobalBinarySHA256    string `json:"globalBinarySha256,omitempty"`
	SourceBinarySHA256    string `json:"sourceBinarySha256,omitempty"`
	BinaryInSync          bool   `json:"binaryInSync"`
	BinDirOnPath          bool   `json:"binDirOnPath"`

	SharedPluginDir       string `json:"sharedPluginDir"`
	SharedPluginInstalled bool   `json:"sharedPluginInstalled"`
	SharedPluginVersion   string `json:"sharedPluginVersion,omitempty"`
	SharedBundleDigest    string `json:"sharedBundleDigest,omitempty"`

	GlobalPluginDir       string `json:"globalPluginDir"`
	GlobalPluginInstalled bool   `json:"globalPluginInstalled"`
	GlobalPluginVersion   string `json:"globalPluginVersion,omitempty"`
	GlobalBundleDigest    string `json:"globalBundleDigest,omitempty"`
	SourceBundleDigest    string `json:"sourceBundleDigest,omitempty"`
	BundleInSync          bool   `json:"bundleInSync"`
	VersionInSync         bool   `json:"versionInSync"`

	WorkspacePluginFound   bool   `json:"workspacePluginFound"`
	WorkspacePluginDir     string `json:"workspacePluginDir,omitempty"`
	WorkspacePluginVer     string `json:"workspacePluginVersion,omitempty"`
	ProjectPluginsManifest string `json:"projectPluginsManifest,omitempty"`
	LegacySidecarFound     bool   `json:"legacySidecarFound"`
	LegacySidecarDir       string `json:"legacySidecarDir,omitempty"`

	DaemonRunning bool   `json:"daemonRunning"`
	DaemonVersion string `json:"daemonVersion,omitempty"`
	DaemonCommit  string `json:"daemonCommit,omitempty"`
	DaemonSkewed  bool   `json:"daemonSkewed"`
}

// UninstallReport summarizes what `hq plugin uninstall` removed while preserving the Vault.
type UninstallReport struct {
	RemovedPluginDir       string `json:"removedPluginDir,omitempty"`
	RemovedSharedPluginDir string `json:"removedSharedPluginDir,omitempty"`
	RemovedLegacySidecar   string `json:"removedLegacySidecar,omitempty"`
	DetachedProjectDir     string `json:"detachedProjectDir,omitempty"`
	RemovedBinaryPath      string `json:"removedBinaryPath,omitempty"`
	PreservedVaultDir      string `json:"preservedVaultDir"`
}

func resolveHomeDir() (string, error) {
	if h := strings.TrimSpace(os.Getenv("HQ_HOME_DIR")); h != "" {
		return filepath.Abs(h)
	}
	if h := strings.TrimSpace(os.Getenv("HOME")); h != "" {
		return filepath.Clean(h), nil
	}
	return os.UserHomeDir()
}

// DefaultBinDir returns ~/.local/bin (or HQ_BIN_DIR if set).
func DefaultBinDir() (string, error) {
	if b := strings.TrimSpace(os.Getenv("HQ_BIN_DIR")); b != "" {
		return filepath.Abs(b)
	}
	home, err := resolveHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin"), nil
}

// DefaultSharedPluginsParentDir returns ~/.gemini/antigravity/hq/plugins (outside global ~/.gemini/config/plugins
// so unrelated projects are never auto-injected unless they opt in via .agents/plugins.json).
func DefaultSharedPluginsParentDir() (string, error) {
	home, err := resolveHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "antigravity", "hq", "plugins"), nil
}

// DefaultSharedPluginDir returns ~/.gemini/antigravity/hq/plugins/agy-hq.
func DefaultSharedPluginDir() (string, error) {
	parent, err := DefaultSharedPluginsParentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, PluginSlug), nil
}

// DefaultGlobalPluginDir returns ~/.gemini/config/plugins/agy-hq (used only when --global is explicitly set).
func DefaultGlobalPluginDir() (string, error) {
	home, err := resolveHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "config", "plugins", PluginSlug), nil
}

// DefaultLegacySidecarDir returns the pre-plugin standalone sidecar path ~/.gemini/config/sidecars/agy-hq-console.
func DefaultLegacySidecarDir() (string, error) {
	home, err := resolveHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "config", "sidecars", "agy-hq-console"), nil
}

// IsDirOnPATH reports whether targetDir appears in the current PATH environment variable.
func IsDirOnPATH(targetDir string) bool {
	cleanTarget := filepath.Clean(targetDir)
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		if filepath.Clean(entry) == cleanTarget {
			return true
		}
	}
	return false
}

// ComputeFileSHA256 computes the hex SHA-256 of a single file.
func ComputeFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ComputeBundleDigest computes a deterministic SHA-256 digest across all regular files
// inside pluginDir (excluding the local bin/hq convenience symlink and OS temp artifacts).
func ComputeBundleDigest(pluginDir string) (string, int, error) {
	var relFiles []string
	err := filepath.WalkDir(pluginDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(pluginDir, path)
		if err != nil || rel == "." {
			return err
		}
		slashRel := filepath.ToSlash(rel)
		if slashRel == "bin" || strings.HasPrefix(slashRel, "bin/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		base := filepath.Base(path)
		if base == ".DS_Store" || strings.HasSuffix(base, ".tmp") {
			return nil
		}
		if d.Type().IsRegular() {
			relFiles = append(relFiles, slashRel)
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	sort.Strings(relFiles)
	h := sha256.New()
	for _, rel := range relFiles {
		data, err := os.ReadFile(filepath.Join(pluginDir, filepath.FromSlash(rel)))
		if err != nil {
			return "", 0, err
		}
		_, _ = h.Write([]byte(rel))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), len(relFiles), nil
}

// ValidateBundle performs deterministic structural, security, and version parity checks on a plugin directory.
func ValidateBundle(pluginDir string, checkEngineVersionParity bool) error {
	st, err := os.Stat(pluginDir)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("plugin directory not found: %s", pluginDir)
	}

	// 1. plugin.json manifest & SemVer check
	manifest, err := version.ReadPluginManifest(pluginDir)
	if err != nil {
		return fmt.Errorf("invalid plugin.json: %w", err)
	}
	if manifest.Name != PluginSlug {
		return fmt.Errorf("plugin.json name = %q, want %q", manifest.Name, PluginSlug)
	}
	if !version.IsValidSemVer(manifest.Version) {
		return fmt.Errorf("plugin.json version %q is not valid SemVer (expected X.Y.Z without leading 'v')", manifest.Version)
	}
	if checkEngineVersionParity && manifest.Version != version.Version {
		return fmt.Errorf("plugin.json version %q != internal/version.Version %q", manifest.Version, version.Version)
	}

	// 2. Malformed/redundant sidecars.json must be absent; native sidecar.json must exist
	if _, err := os.Stat(filepath.Join(pluginDir, "sidecars.json")); err == nil {
		return fmt.Errorf("redundant sidecars.json found at %s (auto-discovered via sidecars/agy-hq-console/sidecar.json)", filepath.Join(pluginDir, "sidecars.json"))
	}
	sidecarManifestPath := filepath.Join(pluginDir, "sidecars", "agy-hq-console", "sidecar.json")
	scBytes, err := os.ReadFile(sidecarManifestPath)
	if err != nil {
		return fmt.Errorf("missing sidecar manifest %s: %w", sidecarManifestPath, err)
	}
	var scObj map[string]interface{}
	if err := json.Unmarshal(scBytes, &scObj); err != nil {
		return fmt.Errorf("invalid JSON in %s: %w", sidecarManifestPath, err)
	}
	if cmd, _ := scObj["command"].(string); strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("%s missing non-empty 'command'", sidecarManifestPath)
	}
	if _, err := os.Stat(filepath.Join(pluginDir, "sidecars", "agy-hq-console", "main.mjs")); err != nil {
		return fmt.Errorf("missing sidecar entrypoint main.mjs: %w", err)
	}

	// 3. rules/AGENTS.md size & plain-Markdown (no YAML frontmatter) check
	rulesPath := filepath.Join(pluginDir, "rules", "AGENTS.md")
	ruleBytes, err := os.ReadFile(rulesPath)
	if err != nil {
		return fmt.Errorf("missing global plugin rules %s: %w", rulesPath, err)
	}
	if len(ruleBytes) == 0 {
		return fmt.Errorf("%s is empty", rulesPath)
	}
	if len(ruleBytes) > MaxRuleMarkdownSize {
		return fmt.Errorf("%s is %d bytes (exceeds %d-byte Antigravity per-rule-file cap)", rulesPath, len(ruleBytes), MaxRuleMarkdownSize)
	}
	trimmedRule := strings.TrimLeft(string(ruleBytes), "\ufeff")
	if strings.HasPrefix(trimmedRule, "---\n") || strings.HasPrefix(trimmedRule, "---\r\n") {
		return fmt.Errorf("%s must be plain Markdown without YAML frontmatter", rulesPath)
	}

	// 4. hooks.json single-source schema, matchers, and zero unguarded relative ./bin/hq paths
	hooksPath := filepath.Join(pluginDir, "hooks.json")
	hooksBytes, err := os.ReadFile(hooksPath)
	if err != nil {
		return fmt.Errorf("missing %s: %w", hooksPath, err)
	}
	var hooksMap map[string]map[string]interface{}
	if err := json.Unmarshal(hooksBytes, &hooksMap); err != nil {
		return fmt.Errorf("invalid JSON in %s: %w", hooksPath, err)
	}
	kernelHook, ok := hooksMap["hq-kernel"]
	if !ok {
		return fmt.Errorf("%s missing top-level 'hq-kernel' entry", hooksPath)
	}
	for _, ev := range []string{"SessionStart", "PreToolUse", "Stop"} {
		if _, hasEv := kernelHook[ev]; !hasEv {
			return fmt.Errorf("%s 'hq-kernel' missing %s handler", hooksPath, ev)
		}
	}
	rawHooks := string(hooksBytes)
	for _, m := range RequiredPreToolMatchers {
		if !strings.Contains(rawHooks, m) {
			return fmt.Errorf("%s PreToolUse matcher missing required tool %q", hooksPath, m)
		}
	}
	if strings.Contains(rawHooks, `"./bin/hq"`) || strings.Contains(rawHooks, `(\"./bin/hq\"`) || strings.Contains(rawHooks, `"../../../bin/hq"`) {
		return fmt.Errorf("%s contains workspace-relative ./bin/hq or ../../../bin/hq command invocations that fail global plugin validation", hooksPath)
	}

	// 5. Bundled skills must not contain workspace-only ./bin/hq invocations
	skillsDir := filepath.Join(pluginDir, "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", skillsDir, err)
	}
	skillCount := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sp := filepath.Join(skillsDir, e.Name(), "SKILL.md")
		sb, rErr := os.ReadFile(sp)
		if rErr != nil {
			continue
		}
		skillCount++
		if strings.Contains(string(sb), "./bin/hq") {
			return fmt.Errorf("skill %s contains workspace-relative './bin/hq' (use 'hq' on PATH instead)", filepath.Join("skills", e.Name(), "SKILL.md"))
		}
	}
	if skillCount == 0 {
		return fmt.Errorf("no SKILL.md files found under %s", skillsDir)
	}

	return nil
}

// ValidateWithAgyCLI runs `agy plugin validate --strict <pluginDir>` when the `agy` CLI is available.
func ValidateWithAgyCLI(pluginDir string) (bool, string, error) {
	agyPath, err := exec.LookPath("agy")
	if err != nil || agyPath == "" {
		return false, "agy CLI not found on PATH (skipped external validator)", nil
	}
	cmd := exec.Command(agyPath, "plugin", "validate", "--strict", pluginDir)
	outBytes, runErr := cmd.CombinedOutput()
	out := strings.TrimSpace(string(outBytes))
	if runErr != nil {
		return true, out, fmt.Errorf("agy plugin validate --strict failed: %w\n%s", runErr, out)
	}
	if strings.Contains(out, "✗") || strings.Contains(out, "⚠") {
		return true, out, fmt.Errorf("agy plugin validate --strict reported warnings/errors:\n%s", out)
	}
	return true, out, nil
}

// pluginsManifestConfig represents Antigravity's `.agents/plugins.json` manifest format.
type pluginsManifestConfig struct {
	Inherits []map[string]interface{} `json:"inherits,omitempty"`
	Entries  []pluginsManifestEntry   `json:"entries,omitempty"`
}

type pluginsManifestEntry struct {
	Path        string   `json:"path"`
	IncludeOnly []string `json:"include_only,omitempty"`
	Exclude     []string `json:"exclude,omitempty"`
}

// AttachProjectPlugin writes or updates `<projectDir>/.agents/plugins.json` so Antigravity loads
// `agy-hq` strictly inside `projectDir` without installing globally into `~/.gemini/config/plugins/`.
// If `<projectDir>/.agents/plugins/agy-hq/plugin.json` already exists (e.g., inside the `agy-hq` source checkout),
// no `.agents/plugins.json` file is needed and `("", nil)` is returned.
func AttachProjectPlugin(projectDir, sharedPluginsParent string) (string, error) {
	if strings.TrimSpace(projectDir) == "" {
		return "", nil
	}
	absProj, err := filepath.Abs(projectDir)
	if err != nil {
		return "", err
	}
	if _, sErr := os.Stat(filepath.Join(absProj, ".agents", "plugins", PluginSlug, "plugin.json")); sErr == nil {
		return "", nil
	}
	if strings.TrimSpace(sharedPluginsParent) == "" {
		sharedPluginsParent, err = DefaultSharedPluginsParentDir()
		if err != nil {
			return "", err
		}
	}
	entryPath := filepath.ToSlash(sharedPluginsParent)
	if strings.TrimSpace(os.Getenv("HQ_HOME_DIR")) == "" {
		if userHome, uErr := os.UserHomeDir(); uErr == nil && userHome != "" {
			stdParent := filepath.Join(userHome, ".gemini", "antigravity", "hq", "plugins")
			if filepath.Clean(sharedPluginsParent) == filepath.Clean(stdParent) {
				entryPath = "~/.gemini/antigravity/hq/plugins"
			}
		}
	}

	manifestPath := filepath.Join(absProj, ".agents", "plugins.json")
	var cfg pluginsManifestConfig
	if raw, rErr := os.ReadFile(manifestPath); rErr == nil && len(strings.TrimSpace(string(raw))) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	found := false
	for i := range cfg.Entries {
		e := &cfg.Entries[i]
		if e.Path == entryPath || strings.HasSuffix(filepath.ToSlash(e.Path), "/.gemini/antigravity/hq/plugins") {
			e.Path = entryPath
			hasSlug := false
			for _, inc := range e.IncludeOnly {
				if inc == PluginSlug {
					hasSlug = true
					break
				}
			}
			if len(e.IncludeOnly) == 0 {
				e.IncludeOnly = []string{PluginSlug}
			} else if !hasSlug {
				e.IncludeOnly = append(e.IncludeOnly, PluginSlug)
			}
			found = true
			break
		}
	}
	if !found {
		cfg.Entries = append(cfg.Entries, pluginsManifestEntry{
			Path:        entryPath,
			IncludeOnly: []string{PluginSlug},
		})
	}
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		return "", err
	}
	outBytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	tmpPath := manifestPath + ".tmp"
	if err := os.WriteFile(tmpPath, append(outBytes, '\n'), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, manifestPath); err != nil {
		return "", err
	}
	return manifestPath, nil
}

// DetachProjectPlugin removes the `agy-hq` entry from `<projectDir>/.agents/plugins.json`,
// deleting `plugins.json` cleanly if no other entries or inherits remain.
func DetachProjectPlugin(projectDir string) (bool, error) {
	if strings.TrimSpace(projectDir) == "" {
		return false, nil
	}
	absProj, err := filepath.Abs(projectDir)
	if err != nil {
		return false, err
	}
	manifestPath := filepath.Join(absProj, ".agents", "plugins.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var cfg pluginsManifestConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return false, err
	}
	var kept []pluginsManifestEntry
	removed := false
	for _, e := range cfg.Entries {
		isHQPath := strings.HasSuffix(filepath.ToSlash(e.Path), "/.gemini/antigravity/hq/plugins")
		if isHQPath {
			var remInc []string
			for _, inc := range e.IncludeOnly {
				if inc != PluginSlug {
					remInc = append(remInc, inc)
				} else {
					removed = true
				}
			}
			if len(remInc) > 0 {
				e.IncludeOnly = remInc
				kept = append(kept, e)
			} else {
				removed = true
			}
			continue
		}
		kept = append(kept, e)
	}
	if !removed {
		return false, nil
	}
	if len(kept) == 0 && len(cfg.Inherits) == 0 {
		_ = os.Remove(manifestPath)
		return true, nil
	}
	cfg.Entries = kept
	outBytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(manifestPath, append(outBytes, '\n'), 0o644)
}

// SanitizedDaemonEnv strips session- and sidecar-scoped Antigravity environment variables
// (such as ANTIGRAVITY_PROJECT_ID from agy-hq/agy-hq-console) while keeping Language Server connectivity.
func SanitizedDaemonEnv() []string {
	raw := os.Environ()
	out := make([]string, 0, len(raw))
	for _, kv := range raw {
		if strings.HasPrefix(kv, "ANTIGRAVITY_PROJECT_ID=") ||
			strings.HasPrefix(kv, "ANTIGRAVITY_CONVERSATION_ID=") ||
			strings.HasPrefix(kv, "ANTIGRAVITY_TRAJECTORY_ID=") ||
			strings.HasPrefix(kv, "ANTIGRAVITY_SOURCE_METADATA=") ||
			strings.HasPrefix(kv, "ANTIGRAVITY_SIDECAR_WEB_PORT=") ||
			strings.HasPrefix(kv, "ANTIGRAVITY_SIDECAR_UI_TOKEN=") ||
			strings.HasPrefix(kv, "PORT=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func ensureConvenienceBinSymlink(pluginDir, targetBin string) {
	pluginBinDir := filepath.Join(pluginDir, "bin")
	if err := os.MkdirAll(pluginBinDir, 0o755); err == nil {
		linkPath := filepath.Join(pluginBinDir, "hq")
		_ = os.Remove(linkPath)
		if sErr := os.Symlink(targetBin, linkPath); sErr != nil {
			_ = copyFileMode(targetBin, linkPath, 0o755)
		}
	}
}

func cleanupLegacyStandaloneSidecar() string {
	legacyDir, err := DefaultLegacySidecarDir()
	if err != nil || legacyDir == "" {
		return ""
	}
	if st, sErr := os.Stat(legacyDir); sErr == nil && st.IsDir() {
		if os.RemoveAll(legacyDir) == nil {
			return legacyDir
		}
	}
	return ""
}

// Install compiles/installs ~/.local/bin/hq, stages the shared plugin bundle at
// ~/.gemini/antigravity/hq/plugins/agy-hq/ (project-scoped by default, or global ~/.gemini/config/plugins/agy-hq/
// only when opts.GlobalScope is true), cleans up superseded legacy artifacts, writes ~/.gemini/antigravity/hq/config.json,
// and initializes/migrates the Vault.
func Install(opts InstallOptions) (*InstallReport, error) {
	binDir := strings.TrimSpace(opts.BinDir)
	if binDir == "" {
		var err error
		binDir, err = DefaultBinDir()
		if err != nil {
			return nil, err
		}
	}
	absBinDir, err := filepath.Abs(binDir)
	if err != nil {
		return nil, err
	}
	sharedPluginsParent, err := DefaultSharedPluginsParentDir()
	if err != nil {
		return nil, err
	}
	sharedPluginDir := filepath.Join(sharedPluginsParent, PluginSlug)
	globalPluginDir, err := DefaultGlobalPluginDir()
	if err != nil {
		return nil, err
	}

	sourceRepo := strings.TrimSpace(opts.SourceRepoDir)
	if sourceRepo != "" && vault.IsAgyHQRepo(sourceRepo) {
		sourceRepo, _ = filepath.Abs(sourceRepo)
	} else {
		sourceRepo = ""
		if resolved, rErr := vault.ResolveRoot(); rErr == nil && vault.IsAgyHQRepo(resolved) {
			sourceRepo, _ = filepath.Abs(resolved)
		} else if existingCfg, lErr := vault.LoadGlobalConfig(); lErr == nil && existingCfg != nil && vault.IsAgyHQRepo(existingCfg.SourceRepoDir) {
			sourceRepo, _ = filepath.Abs(existingCfg.SourceRepoDir)
		}
	}

	srcPluginDir := strings.TrimSpace(opts.PluginSrcDir)
	if srcPluginDir == "" && sourceRepo != "" {
		srcPluginDir = filepath.Join(sourceRepo, ".agents", "plugins", PluginSlug)
	}
	if srcPluginDir == "" {
		if plugEnv := strings.TrimSpace(os.Getenv("PLUGIN_ROOT")); plugEnv != "" {
			srcPluginDir = plugEnv
		}
	}
	if srcPluginDir == "" {
		if st, sErr := os.Stat(filepath.Join(sharedPluginDir, "plugin.json")); sErr == nil && !st.IsDir() {
			srcPluginDir = sharedPluginDir
		} else if st, gErr := os.Stat(filepath.Join(globalPluginDir, "plugin.json")); gErr == nil && !st.IsDir() {
			srcPluginDir = globalPluginDir
		}
	}
	if srcPluginDir == "" {
		return nil, fmt.Errorf("cannot locate source plugin directory (.agents/plugins/agy-hq); pass --plugin-dir or run inside an agy-hq checkout")
	}
	srcPluginDir, err = filepath.Abs(srcPluginDir)
	if err != nil {
		return nil, err
	}

	if err := ValidateBundle(srcPluginDir, true); err != nil {
		return nil, fmt.Errorf("pre-install bundle validation failed: %w", err)
	}

	// 1. Install hq binary atomically to <binDir>/hq
	if err := os.MkdirAll(absBinDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", absBinDir, err)
	}
	targetBin := filepath.Join(absBinDir, "hq")
	tmpBin := targetBin + ".tmp"
	exePath, _ := os.Executable()

	builtFromSource := false
	if sourceRepo != "" && !vault.IsTempPath(exePath) {
		if goPath, gErr := exec.LookPath("go"); gErr == nil && goPath != "" {
			ldflags := fmt.Sprintf("-X github.com/zjshen14/agy-hq/internal/version.GitCommit=%s", resolveRepoHeadShort(sourceRepo))
			buildCmd := exec.Command(goPath, "build", "-ldflags", ldflags, "-o", tmpBin, "./cmd/hq")
			buildCmd.Dir = sourceRepo
			buildCmd.Env = vault.SanitizedGitEnv()
			if out, bErr := buildCmd.CombinedOutput(); bErr != nil {
				return nil, fmt.Errorf("go build hq failed: %w (%s)", bErr, strings.TrimSpace(string(out)))
			}
			builtFromSource = true
		}
	}
	if !builtFromSource {
		if exePath == "" {
			return nil, fmt.Errorf("unable to locate running hq executable to copy")
		}
		if err := copyFileMode(exePath, tmpBin, 0o755); err != nil {
			return nil, fmt.Errorf("copy hq binary: %w", err)
		}
	}
	if err := os.Chmod(tmpBin, 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpBin, targetBin); err != nil {
		return nil, fmt.Errorf("install %s: %w", targetBin, err)
	}
	// Keep checkout ./bin/hq byte-identical when built from sourceRepo
	if builtFromSource && sourceRepo != "" {
		repoBin := filepath.Join(sourceRepo, "bin", "hq")
		if filepath.Clean(repoBin) != filepath.Clean(targetBin) {
			_ = os.MkdirAll(filepath.Dir(repoBin), 0o755)
			_ = copyFileMode(targetBin, repoBin+".tmp", 0o755)
			_ = os.Rename(repoBin+".tmp", repoBin)
		}
	}

	// 2. Always stage shared project-scoped bundle at ~/.gemini/antigravity/hq/plugins/agy-hq/
	if filepath.Clean(srcPluginDir) != filepath.Clean(sharedPluginDir) {
		if err := syncPluginDirectory(srcPluginDir, sharedPluginDir); err != nil {
			return nil, fmt.Errorf("sync shared plugin directory: %w", err)
		}
	}
	ensureConvenienceBinSymlink(sharedPluginDir, targetBin)

	scope := ScopeProject
	activePluginDir := sharedPluginDir
	var removedGlobalPlugin string
	var resolvedProjectDir string
	var projectManifest string

	if opts.GlobalScope {
		scope = ScopeGlobal
		if filepath.Clean(srcPluginDir) != filepath.Clean(globalPluginDir) {
			if err := syncPluginDirectory(srcPluginDir, globalPluginDir); err != nil {
				return nil, fmt.Errorf("sync global plugin directory: %w", err)
			}
		}
		ensureConvenienceBinSymlink(globalPluginDir, targetBin)
		activePluginDir = globalPluginDir
	} else {
		// Project-scoped default: ensure ~/.gemini/config/plugins/agy-hq is removed so unrelated projects stay clean!
		if st, sErr := os.Stat(globalPluginDir); sErr == nil && st.IsDir() && filepath.Clean(srcPluginDir) != filepath.Clean(globalPluginDir) {
			if rErr := os.RemoveAll(globalPluginDir); rErr == nil {
				removedGlobalPlugin = globalPluginDir
			}
		}
		targetProject := strings.TrimSpace(opts.ProjectDir)
		if targetProject == "" && sourceRepo != "" {
			targetProject = sourceRepo
		}
		if targetProject != "" {
			if absProj, pErr := filepath.Abs(targetProject); pErr == nil {
				resolvedProjectDir = absProj
				if mPath, aErr := AttachProjectPlugin(absProj, sharedPluginsParent); aErr != nil {
					return nil, fmt.Errorf("attach plugin to project %s: %w", absProj, aErr)
				} else if mPath != "" {
					projectManifest = mPath
				} else if _, sErr := os.Stat(filepath.Join(absProj, ".agents", "plugins", PluginSlug, "plugin.json")); sErr == nil {
					activePluginDir = filepath.Join(absProj, ".agents", "plugins", PluginSlug)
				}
			}
		}
	}

	// Clean up superseded pre-plugin standalone sidecar copy (~/.gemini/config/sidecars/agy-hq-console)
	removedLegacySidecar := cleanupLegacyStandaloneSidecar()

	// 3. Determine target Vault and persist ~/.gemini/antigravity/hq/config.json
	var chosenVault string
	if strings.TrimSpace(opts.VaultDir) != "" {
		chosenVault, err = filepath.Abs(opts.VaultDir)
		if err != nil {
			return nil, err
		}
	} else if opts.GlobalVault {
		chosenVault, err = vault.DefaultGlobalVaultDir()
		if err != nil {
			return nil, err
		}
	} else if resolvedProjectDir != "" && vault.IsInitializedVault(filepath.Join(resolvedProjectDir, "local")) {
		chosenVault = filepath.Join(resolvedProjectDir, "local")
	} else if sourceRepo != "" && vault.IsInitializedVault(filepath.Join(sourceRepo, "local")) {
		chosenVault = filepath.Join(sourceRepo, "local")
	} else if existingCfg, lErr := vault.LoadGlobalConfig(); lErr == nil && existingCfg != nil && strings.TrimSpace(existingCfg.VaultDir) != "" {
		chosenVault, _ = filepath.Abs(existingCfg.VaultDir)
	} else if resolvedProjectDir != "" && !vault.IsAgyHQRepo(resolvedProjectDir) {
		chosenVault = filepath.Join(resolvedProjectDir, "local")
	} else {
		chosenVault, err = vault.DefaultGlobalVaultDir()
		if err != nil {
			return nil, err
		}
	}

	saveRepo := sourceRepo
	if saveRepo == "" {
		if existingCfg, lErr := vault.LoadGlobalConfig(); lErr == nil && existingCfg != nil {
			saveRepo = existingCfg.SourceRepoDir
		}
	}
	if err := vault.SaveGlobalConfig(vault.GlobalConfig{
		VaultDir:      chosenVault,
		SourceRepoDir: saveRepo,
		PluginScope:   scope,
	}); err != nil {
		return nil, fmt.Errorf("save global hq config: %w", err)
	}

	templateDir := ""
	if sourceRepo != "" {
		templateDir = filepath.Join(sourceRepo, "templates")
	}
	if _, err := vault.Migrate(chosenVault, templateDir); err != nil {
		return nil, fmt.Errorf("initialize/migrate vault at %s: %w", chosenVault, err)
	}

	// 4. Restart running :4242 daemon if version/commit skewed or if migrating off global/legacy sidecar
	forceDaemonRestart := removedGlobalPlugin != "" || removedLegacySidecar != ""
	restarted := EnsureDaemonSynced(chosenVault, targetBin, saveRepo, forceDaemonRestart)

	binSHA, _ := ComputeFileSHA256(targetBin)
	digest, count, _ := ComputeBundleDigest(activePluginDir)
	cfgPath, _ := vault.GlobalConfigPath()

	return &InstallReport{
		Scope:                  scope,
		Version:                version.Version,
		Commit:                 version.EffectiveCommit(),
		BinaryPath:             targetBin,
		BinarySHA256:           binSHA,
		PluginDir:              activePluginDir,
		ProjectDir:             resolvedProjectDir,
		ProjectPluginsManifest: projectManifest,
		RemovedGlobalPluginDir: removedGlobalPlugin,
		RemovedLegacySidecar:   removedLegacySidecar,
		BundleDigest:           digest,
		FileCount:              count,
		VaultDir:               chosenVault,
		GlobalConfigPath:       cfgPath,
		BinDirOnPath:           IsDirOnPATH(absBinDir),
		DaemonRestarted:        restarted,
	}, nil
}

// ProbeDaemonHealth queries a running daemon's /healthz endpoint and returns (reachable, version, commit).
func ProbeDaemonHealth(addr string) (bool, string, string) {
	healthURL := strings.TrimSpace(os.Getenv("HQ_DAEMON_HEALTH_URL"))
	if healthURL == "" {
		if strings.TrimSpace(addr) == "" {
			addr = "127.0.0.1:4242"
		}
		healthURL = "http://" + strings.TrimSpace(addr) + "/healthz"
	}
	client := &http.Client{Timeout: 400 * time.Millisecond}
	resp, err := client.Get(healthURL)
	if err != nil || resp == nil {
		return false, "", ""
	}
	defer resp.Body.Close()
	var body struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return true, "", ""
	}
	return true, body.Version, body.Commit
}

// RestartDaemonIfSkewed checks whether a local `hq daemon` is running an older version/commit
// and restarts it cleanly using targetBin with a sanitized environment.
func RestartDaemonIfSkewed(vaultDir, targetBin string) bool {
	return EnsureDaemonSynced(vaultDir, targetBin, "", false)
}

// EnsureDaemonSynced checks whether a local `hq daemon` is skewed (or forceRestart is set)
// and restarts it cleanly using targetBin and SanitizedDaemonEnv().
func EnsureDaemonSynced(vaultDir, targetBin, workDir string, forceRestart bool) bool {
	dInfo := kernel.GetDaemonInfo(vaultDir)
	if !dInfo.Running || dInfo.PID <= 0 {
		return false
	}
	addr := strings.TrimSpace(dInfo.Addr)
	if addr == "" {
		addr = "127.0.0.1:4242"
	}
	reachable, dVer, dCommit := ProbeDaemonHealth(addr)
	skewed := forceRestart
	if reachable && !skewed {
		if dVer == "" || dVer != version.Version {
			skewed = true
		} else if eff := version.EffectiveCommit(); eff != "dev" && dCommit != "" && dCommit != "dev" && dCommit != eff {
			skewed = true
		}
	}
	if !skewed {
		return false
	}
	if proc, pErr := os.FindProcess(dInfo.PID); pErr == nil {
		_ = proc.Signal(syscall.SIGTERM)
		time.Sleep(150 * time.Millisecond)
	}
	if !vault.IsTempPath(targetBin) {
		cmd := exec.Command(targetBin, "daemon", "--addr="+addr)
		cmd.Env = SanitizedDaemonEnv()
		if workDir != "" {
			if st, sErr := os.Stat(workDir); sErr == nil && st.IsDir() {
				cmd.Dir = workDir
			}
		}
		if cmd.Dir == "" && filepath.Base(filepath.Clean(vaultDir)) == "local" {
			parent := filepath.Dir(filepath.Clean(vaultDir))
			if st, sErr := os.Stat(parent); sErr == nil && st.IsDir() {
				cmd.Dir = parent
			}
		}
		if cmd.Dir == "" {
			if home, hErr := resolveHomeDir(); hErr == nil && home != "" {
				cmd.Dir = home
			}
		}
		if cmd.Start() == nil && cmd.Process != nil {
			_ = cmd.Process.Release()
			return true
		}
	}
	return true
}

// Status inspects binary, project/global plugin bundle, version sync, and Vault resolution state.
func Status(vaultDir, rootDir string) (*StatusReport, error) {
	resolvedVault, source, err := vault.ResolveVaultDirWithSource()
	if err != nil {
		resolvedVault = vaultDir
		source = "explicit"
	}
	if vaultDir != "" && filepath.Clean(vaultDir) != filepath.Clean(resolvedVault) {
		resolvedVault = vaultDir
	}
	cfgPath, _ := vault.GlobalConfigPath()
	binDir, _ := DefaultBinDir()
	globalBin := filepath.Join(binDir, "hq")
	sharedPlugDir, _ := DefaultSharedPluginDir()
	globalPlugDir, _ := DefaultGlobalPluginDir()
	legacySidecarDir, _ := DefaultLegacySidecarDir()

	cfg, _ := vault.LoadGlobalConfig()
	scope := ScopeProject
	if cfg != nil && cfg.PluginScope == ScopeGlobal {
		scope = ScopeGlobal
	}

	rep := &StatusReport{
		Scope:                 scope,
		EngineVersion:         version.Version,
		EngineCommit:          version.EffectiveCommit(),
		VaultSchemaVersion:    vault.CurrentSchemaStateVersion(resolvedVault),
		VaultDir:              resolvedVault,
		VaultResolutionSource: source,
		GlobalConfigPath:      cfgPath,
		GlobalBinaryPath:      globalBin,
		BinDirOnPath:          IsDirOnPATH(binDir),
		SharedPluginDir:       sharedPlugDir,
		GlobalPluginDir:       globalPlugDir,
	}

	if legacySidecarDir != "" {
		if st, sErr := os.Stat(legacySidecarDir); sErr == nil && st.IsDir() {
			rep.LegacySidecarFound = true
			rep.LegacySidecarDir = legacySidecarDir
		}
	}

	sourceRepo := ""
	if rootDir != "" && vault.IsAgyHQRepo(rootDir) {
		sourceRepo, _ = filepath.Abs(rootDir)
	} else if cfg != nil && vault.IsAgyHQRepo(cfg.SourceRepoDir) {
		sourceRepo, _ = filepath.Abs(cfg.SourceRepoDir)
	}
	rep.SourceRepoDir = sourceRepo

	if st, sErr := os.Stat(globalBin); sErr == nil && !st.IsDir() {
		rep.GlobalBinaryInstalled = true
		rep.GlobalBinarySHA256, _ = ComputeFileSHA256(globalBin)
	}
	if sourceRepo != "" {
		repoBin := filepath.Join(sourceRepo, "bin", "hq")
		if st, sErr := os.Stat(repoBin); sErr == nil && !st.IsDir() {
			rep.SourceBinarySHA256, _ = ComputeFileSHA256(repoBin)
		}
	}
	rep.BinaryInSync = rep.GlobalBinaryInstalled && (rep.SourceBinarySHA256 == "" || rep.GlobalBinarySHA256 == rep.SourceBinarySHA256)

	if st, sErr := os.Stat(sharedPlugDir); sErr == nil && st.IsDir() {
		rep.SharedPluginInstalled = true
		if m, mErr := version.ReadPluginManifest(sharedPlugDir); mErr == nil {
			rep.SharedPluginVersion = m.Version
		}
		rep.SharedBundleDigest, _, _ = ComputeBundleDigest(sharedPlugDir)
	}
	if st, sErr := os.Stat(globalPlugDir); sErr == nil && st.IsDir() {
		rep.GlobalPluginInstalled = true
		rep.Scope = ScopeGlobal
		if m, mErr := version.ReadPluginManifest(globalPlugDir); mErr == nil {
			rep.GlobalPluginVersion = m.Version
		}
		rep.GlobalBundleDigest, _, _ = ComputeBundleDigest(globalPlugDir)
	}
	if sourceRepo != "" {
		srcPlugDir := filepath.Join(sourceRepo, ".agents", "plugins", PluginSlug)
		if st, sErr := os.Stat(srcPlugDir); sErr == nil && st.IsDir() {
			rep.SourceBundleDigest, _, _ = ComputeBundleDigest(srcPlugDir)
			rep.WorkspacePluginFound = true
			rep.WorkspacePluginDir = srcPlugDir
			if m, mErr := version.ReadPluginManifest(srcPlugDir); mErr == nil {
				rep.WorkspacePluginVer = m.Version
			}
		}
	}
	if wd, wErr := os.Getwd(); wErr == nil && wd != "" {
		candManifest := filepath.Join(wd, ".agents", "plugins.json")
		if st, sErr := os.Stat(candManifest); sErr == nil && !st.IsDir() {
			rep.ProjectPluginsManifest = candManifest
		}
	}

	if rep.GlobalPluginInstalled {
		rep.BundleInSync = rep.SourceBundleDigest == "" || rep.GlobalBundleDigest == rep.SourceBundleDigest
		rep.VersionInSync = rep.GlobalPluginVersion == version.Version
	} else if rep.SharedPluginInstalled {
		rep.BundleInSync = rep.SourceBundleDigest == "" || rep.SharedBundleDigest == rep.SourceBundleDigest
		rep.VersionInSync = rep.SharedPluginVersion == version.Version
	} else {
		rep.BundleInSync = rep.WorkspacePluginFound
		rep.VersionInSync = !rep.WorkspacePluginFound || rep.WorkspacePluginVer == version.Version
	}

	dInfo := kernel.GetDaemonInfo(resolvedVault)
	if dInfo.Running {
		if ok, dVer, dCom := ProbeDaemonHealth(dInfo.Addr); ok {
			rep.DaemonRunning = true
			rep.DaemonVersion = dVer
			rep.DaemonCommit = dCom
			if dVer == "" || dVer != version.Version {
				rep.DaemonSkewed = true
			} else if eff := version.EffectiveCommit(); eff != "dev" && dCom != "" && dCom != "dev" && dCom != eff {
				rep.DaemonSkewed = true
			}
		}
	}

	return rep, nil
}

// Uninstall removes ~/.gemini/config/plugins/agy-hq/, ~/.gemini/antigravity/hq/plugins/agy-hq/,
// and legacy ~/.gemini/config/sidecars/agy-hq-console/ (plus ~/.local/bin/hq if purgeBin is true
// and optional project `.agents/plugins.json` entry if detachProjectDir is provided),
// strictly preserving the user's private Vault on disk.
func Uninstall(purgeBin bool, detachProjectDir ...string) (*UninstallReport, error) {
	vDir, _ := vault.ResolveVaultDir()
	rep := &UninstallReport{
		PreservedVaultDir: vDir,
	}
	globalPlugDir, err := DefaultGlobalPluginDir()
	if err != nil {
		return nil, err
	}
	if _, sErr := os.Stat(globalPlugDir); sErr == nil {
		if rErr := os.RemoveAll(globalPlugDir); rErr != nil {
			return nil, fmt.Errorf("remove %s: %w", globalPlugDir, rErr)
		}
		rep.RemovedPluginDir = globalPlugDir
	}
	if sharedPlugDir, sErr := DefaultSharedPluginDir(); sErr == nil && sharedPlugDir != "" {
		if _, stErr := os.Stat(sharedPlugDir); stErr == nil {
			if rErr := os.RemoveAll(sharedPlugDir); rErr != nil {
				return nil, fmt.Errorf("remove %s: %w", sharedPlugDir, rErr)
			}
			rep.RemovedSharedPluginDir = sharedPlugDir
		}
	}
	if removedLeg := cleanupLegacyStandaloneSidecar(); removedLeg != "" {
		rep.RemovedLegacySidecar = removedLeg
	}
	if len(detachProjectDir) > 0 && strings.TrimSpace(detachProjectDir[0]) != "" {
		if ok, dErr := DetachProjectPlugin(detachProjectDir[0]); dErr == nil && ok {
			rep.DetachedProjectDir, _ = filepath.Abs(detachProjectDir[0])
		}
	}
	if cfg, cErr := vault.LoadGlobalConfig(); cErr == nil && cfg != nil && cfg.PluginScope == ScopeGlobal {
		cfg.PluginScope = ScopeProject
		_ = vault.SaveGlobalConfig(*cfg)
	}
	if purgeBin {
		binDir, bErr := DefaultBinDir()
		if bErr == nil {
			binPath := filepath.Join(binDir, "hq")
			if _, sErr := os.Stat(binPath); sErr == nil {
				if rErr := os.Remove(binPath); rErr != nil {
					return nil, fmt.Errorf("remove %s: %w", binPath, rErr)
				}
				rep.RemovedBinaryPath = binPath
			}
		}
	}
	return rep, nil
}

func resolveRepoHeadShort(repoDir string) string {
	out, err := vault.SanitizedGitCommand(repoDir, "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		return version.EffectiveCommit()
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return version.EffectiveCommit()
	}
	return s
}

func copyFileMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func syncPluginDirectory(srcDir, dstDir string) error {
	parent := filepath.Dir(dstDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stageDir := dstDir + ".staging"
	_ = os.RemoveAll(stageDir)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return err
	}

	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, rErr := filepath.Rel(srcDir, path)
		if rErr != nil || rel == "." {
			return rErr
		}
		slashRel := filepath.ToSlash(rel)
		if slashRel == "bin" || strings.HasPrefix(slashRel, "bin/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		base := filepath.Base(path)
		if base == ".DS_Store" || strings.HasSuffix(base, ".tmp") {
			return nil
		}
		dstPath := filepath.Join(stageDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dstPath, 0o755)
		}
		if d.Type().IsRegular() {
			info, iErr := d.Info()
			perm := os.FileMode(0o644)
			if iErr == nil && (info.Mode().Perm()&0o111) != 0 {
				perm = 0o755
			}
			return copyFileMode(path, dstPath, perm)
		}
		return nil
	})
	if err != nil {
		_ = os.RemoveAll(stageDir)
		return err
	}

	backupDir := dstDir + ".prev"
	_ = os.RemoveAll(backupDir)
	hadPrev := false
	if _, sErr := os.Stat(dstDir); sErr == nil {
		if rErr := os.Rename(dstDir, backupDir); rErr != nil {
			_ = os.RemoveAll(stageDir)
			return rErr
		}
		hadPrev = true
	}
	if rErr := os.Rename(stageDir, dstDir); rErr != nil {
		if hadPrev {
			_ = os.Rename(backupDir, dstDir)
		}
		_ = os.RemoveAll(stageDir)
		return rErr
	}
	if hadPrev {
		_ = os.RemoveAll(backupDir)
	}
	return nil
}
