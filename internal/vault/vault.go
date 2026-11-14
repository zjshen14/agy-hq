package vault

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/templates"
)

// AgyHQModulePath is the canonical Go module path identifying the agy-hq engine checkout.
const AgyHQModulePath = "module github.com/zjshen14/agy-hq"

// GlobalConfig stores the user's canonical Vault pointer at ~/.gemini/antigravity/hq/config.json
// so CLI, Desktop, IDE, and background daemon sessions never suffer cross-surface split-brain.
type GlobalConfig struct {
	VaultDir      string `json:"vaultDir"`
	SourceRepoDir string `json:"sourceRepoDir,omitempty"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
}

// Fact represents a bi-temporal fact entry in local/memory/facts.jsonl.
type Fact struct {
	ID           string  `json:"id"`
	Topic        string  `json:"topic"`
	Statement    string  `json:"statement"`
	SourceConv   string  `json:"source_conv,omitempty"`
	Verified     bool    `json:"verified,omitempty"`
	ValidFrom    string  `json:"valid_from"`
	ValidTo      *string `json:"valid_to"`
	SupersededBy string  `json:"superseded_by,omitempty"`
}

// IsTempPath returns true if p resides inside os.TempDir() (handling macOS /private/var symlink resolution).
func IsTempPath(p string) bool {
	if p == "" {
		return false
	}
	tmp := filepath.Clean(os.TempDir())
	cleanP := filepath.Clean(p)
	if strings.HasPrefix(cleanP, tmp) || strings.HasPrefix(cleanP, "/private"+tmp) || strings.HasPrefix(cleanP, "/tmp/") || strings.HasPrefix(cleanP, "/private/tmp/") {
		return true
	}
	if evalTmp, err := filepath.EvalSymlinks(tmp); err == nil && evalTmp != "" && strings.HasPrefix(cleanP, filepath.Clean(evalTmp)) {
		return true
	}
	return false
}

// IsAgyHQRepo returns true iff dir contains a go.mod declaring module github.com/zjshen14/agy-hq.
// This prevents third-party Go repositories from being mistaken for the agy-hq engine checkout.
func IsAgyHQRepo(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	f, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	if n <= 0 {
		return false
	}
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == AgyHQModulePath || strings.HasPrefix(trimmed, AgyHQModulePath+" ") || strings.HasPrefix(trimmed, AgyHQModulePath+"\t") {
			return true
		}
	}
	return false
}

// IsInitializedVault returns true if dir looks like an initialized agy-hq Vault.
func IsInitializedVault(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	if st, err := os.Stat(filepath.Join(dir, "USER.md")); err == nil && !st.IsDir() {
		return true
	}
	if st, err := os.Stat(filepath.Join(dir, "memory")); err == nil && st.IsDir() {
		return true
	}
	return false
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

// GlobalConfigPath returns ~/.gemini/antigravity/hq/config.json.
func GlobalConfigPath() (string, error) {
	home, err := resolveHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".gemini", "antigravity", "hq", "config.json"), nil
}

// DefaultGlobalVaultDir returns ~/.gemini/antigravity/hq/vault.
func DefaultGlobalVaultDir() (string, error) {
	home, err := resolveHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".gemini", "antigravity", "hq", "vault"), nil
}

// LoadGlobalConfig reads ~/.gemini/antigravity/hq/config.json if present.
func LoadGlobalConfig() (*GlobalConfig, error) {
	cfgPath, err := GlobalConfigPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}
	var cfg GlobalConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SaveGlobalConfig atomically writes ~/.gemini/antigravity/hq/config.json with 0600 permissions.
func SaveGlobalConfig(cfg GlobalConfig) error {
	cfgPath, err := GlobalConfigPath()
	if err != nil {
		return err
	}
	if cfg.UpdatedAt == "" {
		cfg.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := cfgPath + ".tmp"
	if err := os.WriteFile(tmpPath, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, cfgPath)
}

// SanitizedGitEnv returns os.Environ() with parent git hook environment variables stripped
// so subprocess git commands never bleed into an outer repository when invoked inside git hooks.
func SanitizedGitEnv() []string {
	raw := os.Environ()
	out := make([]string, 0, len(raw))
	for _, kv := range raw {
		if strings.HasPrefix(kv, "GIT_DIR=") ||
			strings.HasPrefix(kv, "GIT_WORK_TREE=") ||
			strings.HasPrefix(kv, "GIT_INDEX_FILE=") ||
			strings.HasPrefix(kv, "GIT_OBJECT_DIRECTORY=") ||
			strings.HasPrefix(kv, "GIT_ALTERNATE_OBJECT_DIRECTORIES=") ||
			strings.HasPrefix(kv, "GIT_COMMON_DIR=") ||
			strings.HasPrefix(kv, "GIT_PREFIX=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// SanitizedGitCommand constructs an exec.Cmd for git with SanitizedGitEnv() and optional working directory.
func SanitizedGitCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = SanitizedGitEnv()
	return cmd
}

func findWorktreeParentVault(startDir string) string {
	if strings.TrimSpace(startDir) == "" {
		return ""
	}
	dir := filepath.Clean(startDir)
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		if filepath.Base(parent) == "worktrees" {
			vaultCand := filepath.Dir(parent)
			if IsInitializedVault(vaultCand) {
				return vaultCand
			}
		}
		dir = parent
	}
	return ""
}

func findEnclosingAgyHQRepo(startDir string) string {
	if strings.TrimSpace(startDir) == "" {
		return ""
	}
	dir := filepath.Clean(startDir)
	for {
		if IsAgyHQRepo(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func isUnderWorktreesDir(dir string) bool {
	cur := filepath.Clean(dir)
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		if filepath.Base(parent) == "worktrees" {
			return true
		}
		cur = parent
	}
}

// ResolveRoot finds the agy-hq engine repository root directory if available,
// falling back to the current working directory when running standalone.
func ResolveRoot() (string, error) {
	if envRoot := strings.TrimSpace(os.Getenv("HQ_ROOT_DIR")); envRoot != "" {
		return filepath.Abs(envRoot)
	}
	if plugRoot := strings.TrimSpace(os.Getenv("PLUGIN_ROOT")); plugRoot != "" {
		cand := filepath.Clean(filepath.Join(plugRoot, "..", "..", ".."))
		if IsAgyHQRepo(cand) {
			return filepath.Abs(cand)
		}
	}
	cwd, cwdErr := os.Getwd()
	if cwdErr == nil && cwd != "" {
		if r := findEnclosingAgyHQRepo(cwd); r != "" {
			return r, nil
		}
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Dir(filepath.Dir(exe))
		if IsAgyHQRepo(cand) {
			return cand, nil
		}
	}
	if cfg, err := LoadGlobalConfig(); err == nil && cfg != nil && strings.TrimSpace(cfg.SourceRepoDir) != "" {
		if IsAgyHQRepo(cfg.SourceRepoDir) {
			return filepath.Abs(cfg.SourceRepoDir)
		}
	}
	if cwdErr != nil {
		return "", cwdErr
	}
	return cwd, nil
}

// ResolveVaultDirWithSource returns the resolved Vault path alongside the resolution tier name.
func ResolveVaultDirWithSource() (string, string, error) {
	// 1. Explicit HQ_VAULT_DIR environment override
	if envVault := strings.TrimSpace(os.Getenv("HQ_VAULT_DIR")); envVault != "" {
		abs, err := filepath.Abs(envVault)
		return abs, "env:HQ_VAULT_DIR", err
	}
	// 2. Explicit HQ_ROOT_DIR test/harness override
	if envRoot := strings.TrimSpace(os.Getenv("HQ_ROOT_DIR")); envRoot != "" {
		absRoot, err := filepath.Abs(envRoot)
		if err != nil {
			return "", "", err
		}
		return filepath.Join(absRoot, "local"), "env:HQ_ROOT_DIR", nil
	}

	cwd, _ := os.Getwd()
	plugRoot := strings.TrimSpace(os.Getenv("PLUGIN_ROOT"))

	// 3. Enclosing <Vault>/worktrees/<slug> ancestor (from cwd or PLUGIN_ROOT)
	for _, probe := range []string{cwd, plugRoot} {
		if v := findWorktreeParentVault(probe); v != "" {
			return v, "worktree-ancestor", nil
		}
	}

	// 4. Checkout-local ./local inside a verified agy-hq repository (excluding nested worktrees)
	for _, probe := range []string{cwd, plugRoot} {
		if r := findEnclosingAgyHQRepo(probe); r != "" && !isUnderWorktreesDir(r) {
			localCand := filepath.Join(r, "local")
			if IsInitializedVault(localCand) || os.Getenv("HQ_LOCAL_VAULT") == "1" {
				return localCand, "repo-local", nil
			}
			if cfg, err := LoadGlobalConfig(); err != nil || cfg == nil || strings.TrimSpace(cfg.VaultDir) == "" {
				return localCand, "repo-local-bootstrap", nil
			}
		}
	}

	// 5. Global pointer config (~/.gemini/antigravity/hq/config.json)
	if cfg, err := LoadGlobalConfig(); err == nil && cfg != nil && strings.TrimSpace(cfg.VaultDir) != "" {
		absV, aErr := filepath.Abs(cfg.VaultDir)
		if aErr == nil {
			return absV, "global-config", nil
		}
	}

	// 6. Executable located directly inside <agy-hq-repo>/bin/hq with initialized ./local
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Dir(filepath.Dir(exe))
		if IsAgyHQRepo(cand) && !isUnderWorktreesDir(cand) && IsInitializedVault(filepath.Join(cand, "local")) {
			return filepath.Join(cand, "local"), "exe-repo-local", nil
		}
	}

	// 7. Canonical surface-agnostic global Vault (~/.gemini/antigravity/hq/vault)
	defV, err := DefaultGlobalVaultDir()
	return defV, "global-default", err
}

// ResolveVaultDir returns the path to the private user Vault.
func ResolveVaultDir() (string, error) {
	v, _, err := ResolveVaultDirWithSource()
	return v, err
}

// ReadTemplateFile reads relPath from disk templateDir if available, falling back to embedded templates.FS.
func ReadTemplateFile(templateDir, relPath string) ([]byte, error) {
	if strings.TrimSpace(templateDir) != "" {
		diskPath := filepath.Join(templateDir, filepath.FromSlash(relPath))
		if b, err := os.ReadFile(diskPath); err == nil {
			return b, nil
		}
	}
	return fs.ReadFile(templates.FS, filepath.ToSlash(relPath))
}

// ResolveConversationID resolves the active Antigravity conversation ID from the environment
// (checking ANTIGRAVITY_CONVERSATION_ID first, then ANTIGRAVITY_SOURCE_METADATA JSON).
func ResolveConversationID() string {
	if convID := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")); convID != "" {
		return convID
	}
	if metaRaw := strings.TrimSpace(os.Getenv("ANTIGRAVITY_SOURCE_METADATA")); metaRaw != "" {
		var meta struct {
			Tool struct {
				ConversationID string `json:"conversationId"`
			} `json:"tool"`
		}
		if json.Unmarshal([]byte(metaRaw), &meta) == nil && strings.TrimSpace(meta.Tool.ConversationID) != "" {
			return strings.TrimSpace(meta.Tool.ConversationID)
		}
	}
	return ""
}

// ResolveArtifactDir determines the artifact directory for a conversation ID.
// It checks ANTIGRAVITY_APP_DATA_DIR (falling back to ~/.gemini/antigravity),
// returning <appDataDir>/brain/<convID>.
func ResolveArtifactDir(convID string) string {
	if convID == "" {
		return ""
	}
	appDataDir := os.Getenv("ANTIGRAVITY_APP_DATA_DIR")
	if appDataDir == "" {
		if envVault := os.Getenv("HQ_VAULT_DIR"); envVault != "" && IsTempPath(envVault) {
			return filepath.Join(filepath.Dir(filepath.Clean(envVault)), ".brain", convID)
		}
		if home, err := resolveHomeDir(); err == nil && home != "" {
			appDataDir = filepath.Join(home, ".gemini", "antigravity")
		}
	}
	if appDataDir == "" {
		return ""
	}
	return filepath.Join(appDataDir, "brain", convID)
}

// Init scaffolds the private Vault directory structure, copies starter templates
// (from disk templateDir or embedded templates.FS) if files do not already exist,
// and initializes an independent private git repository.
func Init(vaultDir, templateDir string) ([]string, error) {
	var created []string

	dirs := []string{
		vaultDir,
		filepath.Join(vaultDir, "inbox"),
		filepath.Join(vaultDir, "notes", "briefs"),
		filepath.Join(vaultDir, "notes", "investing"),
		filepath.Join(vaultDir, "notes", "writing"),
		filepath.Join(vaultDir, "notes", "general"),
		filepath.Join(vaultDir, "worktrees"),
		filepath.Join(vaultDir, "repos"),
		filepath.Join(vaultDir, "private", "ledger"),
		filepath.Join(vaultDir, "private", "taxes"),
		filepath.Join(vaultDir, "memory", "topics"),
		filepath.Join(vaultDir, "memory", "entities"),
		filepath.Join(vaultDir, "memory", "journal"),
		filepath.Join(vaultDir, "memory", "pending"),
		filepath.Join(vaultDir, "memory", "outbox"),
		filepath.Join(vaultDir, "memory", "proposals"),
		filepath.Join(vaultDir, "memory", "rsi", "hypotheses"),
		filepath.Join(vaultDir, "console_spaces"),
	}

	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return created, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	fileMappings := map[string]string{
		"gitignore.tmpl": ".gitignore",
		"USER.md":        "USER.md",
		"MEMORY.md":      "MEMORY.md",
		"config.yaml":    "config.yaml",
		"triggers.yaml":  "triggers.yaml",
	}

	for srcName, dstName := range fileMappings {
		dstPath := filepath.Join(vaultDir, dstName)
		if _, err := os.Stat(dstPath); err == nil {
			continue // Never overwrite existing user Vault files
		}
		data, err := ReadTemplateFile(templateDir, srcName)
		if err != nil {
			return created, fmt.Errorf("read template %s: %w", srcName, err)
		}
		if err := os.WriteFile(dstPath, data, 0o600); err != nil {
			return created, fmt.Errorf("write %s: %w", dstPath, err)
		}
		created = append(created, dstName)
	}

	// Ensure facts.jsonl and interventions.jsonl exist
	for _, jsonl := range []string{
		filepath.Join(vaultDir, "memory", "facts.jsonl"),
		filepath.Join(vaultDir, "memory", "interventions.jsonl"),
	} {
		if _, err := os.Stat(jsonl); os.IsNotExist(err) {
			if err := os.WriteFile(jsonl, []byte(""), 0o600); err != nil {
				return created, err
			}
			rel, _ := filepath.Rel(vaultDir, jsonl)
			created = append(created, rel)
		}
	}

	if ensureStarterTopicPage(vaultDir) {
		created = append(created, filepath.Join("memory", "topics", "agy-hq-architecture.md"))
	}

	// Initialize independent git repository inside the private Vault if not already present
	gitDir := filepath.Join(vaultDir, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		initCmd := SanitizedGitCommand(vaultDir, "init", "-b", "main")
		if out, err := initCmd.CombinedOutput(); err != nil {
			return created, fmt.Errorf("git init in vault: %w (%s)", err, string(out))
		}
		_ = SanitizedGitCommand(vaultDir, "config", "user.name", "hq-vault").Run()
		_ = SanitizedGitCommand(vaultDir, "config", "user.email", "hq-vault@localhost").Run()

		addCmd := SanitizedGitCommand(vaultDir, "add", ".")
		if err := addCmd.Run(); err == nil {
			commitCmd := SanitizedGitCommand(vaultDir, "commit", "-m", "chore(vault): initialize private agy-hq vault")
			_ = commitCmd.Run()
		}
		created = append(created, ".git (initialized private Vault repository)")
	}

	// Install root repository .git/hooks/pre-commit and pre-push ONLY inside a verified agy-hq checkout
	if strings.TrimSpace(templateDir) != "" {
		repoRoot := filepath.Dir(filepath.Clean(templateDir))
		if IsAgyHQRepo(repoRoot) {
			rootHooksDir := filepath.Join(repoRoot, ".git", "hooks")
			if info, err := os.Stat(rootHooksDir); err == nil && info.IsDir() {
				preCommitPath := filepath.Join(rootHooksDir, "pre-commit")
				if _, err := os.Stat(preCommitPath); os.IsNotExist(err) {
					hookScript := "#!/bin/sh\n" +
						"# agy-hq managed pre-commit hook (Ring 0 TCB — RFC-0011)\n" +
						"set -e\n\n" +
						"# 1. Fast shell fallback guard on local/ and vault/ paths\n" +
						"if git diff --cached --name-only | grep -qE '^(local|vault)/'; then\n" +
						"  echo \"ERROR: Refusing to commit files inside private local/ Vault.\" >&2\n" +
						"  exit 1\n" +
						"fi\n" +
						"USER_EMAIL=\"$(git config user.email 2>/dev/null || true)\"\n" +
						"HOME_DIR=\"$HOME\"\n" +
						"if [ -n \"$HOME_DIR\" ] && git diff --cached -U0 | grep -E '^\\+' | grep -qF \"$HOME_DIR\"; then\n" +
						"  echo \"ERROR: Staged diff contains your local HOME path ($HOME_DIR). Keep personal paths in local/.\" >&2\n" +
						"  exit 1\n" +
						"fi\n" +
						"if [ -n \"$USER_EMAIL\" ] && git diff --cached -U0 | grep -E '^\\+' | grep -qFi \"$USER_EMAIL\"; then\n" +
						"  echo \"ERROR: Staged diff contains your personal email ($USER_EMAIL). Keep personal state in local/.\" >&2\n" +
						"  exit 1\n" +
						"fi\n\n" +
						"# 2. Full 5-invariant staged PII & plugin hooks check via ./bin/hq\n" +
						"if [ -x \"./bin/hq\" ]; then\n" +
						"  ./bin/hq ci pre-commit\n" +
						"fi\n"
					if err := os.WriteFile(preCommitPath, []byte(hookScript), 0o755); err == nil {
						created = append(created, "../.git/hooks/pre-commit (PII guard)")
					}
				}
				prePushPath := filepath.Join(rootHooksDir, "pre-push")
				if _, err := os.Stat(prePushPath); os.IsNotExist(err) {
					pushScript := "#!/bin/sh\n" +
						"# agy-hq managed pre-push hook (Ring 0 TCB — RFC-0011)\n" +
						"set -e\n\n" +
						"if [ \"${HQ_IN_PRE_PUSH:-0}\" = \"1\" ]; then\n" +
						"  exit 0\n" +
						"fi\n" +
						"export HQ_IN_PRE_PUSH=1\n" +
						"export PATH=\"/opt/homebrew/bin:/usr/local/go/bin:$HOME/.local/go/bin:$HOME/.local/bin:$PATH\"\n\n" +
						"if [ ! -x \"./bin/hq\" ] && command -v go >/dev/null 2>&1 && [ -d \"./cmd/hq\" ]; then\n" +
						"  mkdir -p ./bin\n" +
						"  go build -o ./bin/hq ./cmd/hq\n" +
						"fi\n\n" +
						"if [ -x \"./bin/hq\" ]; then\n" +
						"  exec ./bin/hq ci pre-push\n" +
						"fi\n"
					if err := os.WriteFile(prePushPath, []byte(pushScript), 0o755); err == nil {
						created = append(created, "../.git/hooks/pre-push (CI & regression guard)")
					}
				}
			}
		}
	}

	return created, nil
}

// CommitVault stages and commits all pending changes inside the private Vault git repository.
func CommitVault(vaultDir, message string) (bool, error) {
	gitDir := filepath.Join(vaultDir, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return false, fmt.Errorf("vault git repository not initialized at %s", vaultDir)
	}
	statusOut, err := SanitizedGitCommand(vaultDir, "status", "--porcelain").Output()
	if err != nil {
		return false, err
	}
	if len(strings.TrimSpace(string(statusOut))) == 0 {
		return false, nil
	}
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("chore(memory): consolidate vault %s", time.Now().Format("2006-01-02"))
	}
	if out, err := SanitizedGitCommand(vaultDir, "add", "-A").CombinedOutput(); err != nil {
		return false, fmt.Errorf("git add: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if out, err := SanitizedGitCommand(vaultDir, "commit", "-m", message).CombinedOutput(); err != nil {
		return false, fmt.Errorf("git commit: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

// LoadActiveFacts reads active facts (valid_to == null) from local/memory/facts.jsonl,
// deduplicating records by Fact.ID in chronological order so superseded or expired facts
// are never returned as active.
func LoadActiveFacts(vaultDir string, limit int) ([]Fact, error) {
	active, _, _, err := LoadFactsState(vaultDir)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(active) > limit {
		active = active[len(active)-limit:]
	}
	return active, nil
}
