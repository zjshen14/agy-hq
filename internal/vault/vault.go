package vault

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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

// ResolveRoot finds the agy-hq repository root directory.
func ResolveRoot() (string, error) {
	if envRoot := os.Getenv("HQ_ROOT_DIR"); envRoot != "" {
		return filepath.Abs(envRoot)
	}
	// Check relative to executable path first (e.g., <root>/bin/hq)
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidate := filepath.Dir(exeDir)
		if _, err := os.Stat(filepath.Join(candidate, "templates")); err == nil {
			return candidate, nil
		}
	}
	// Walk up from current working directory
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return cwd, nil
}

// ResolveVaultDir returns the path to the private user Vault (local/).
func ResolveVaultDir() (string, error) {
	if envVault := os.Getenv("HQ_VAULT_DIR"); envVault != "" {
		return filepath.Abs(envVault)
	}
	root, err := ResolveRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "local"), nil
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
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			appDataDir = filepath.Join(home, ".gemini", "antigravity")
		}
	}
	if appDataDir == "" {
		return ""
	}
	return filepath.Join(appDataDir, "brain", convID)
}

// Init scaffolds the local/ private Vault directory structure, copies starter templates
// if files do not already exist, and initializes an independent private git repository.
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
		filepath.Join(vaultDir, "memory", "journal"),
		filepath.Join(vaultDir, "memory", "pending"),
		filepath.Join(vaultDir, "memory", "outbox"),
		filepath.Join(vaultDir, "memory", "proposals"),
		filepath.Join(vaultDir, "memory", "rsi", "hypotheses"),
	}

	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
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
		srcPath := filepath.Join(templateDir, srcName)
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return created, fmt.Errorf("read template %s: %w", srcPath, err)
		}
		if err := os.WriteFile(dstPath, data, 0o644); err != nil {
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
			if err := os.WriteFile(jsonl, []byte(""), 0o644); err != nil {
				return created, err
			}
			rel, _ := filepath.Rel(vaultDir, jsonl)
			created = append(created, rel)
		}
	}

	if ensureStarterTopicPage(vaultDir) {
		created = append(created, filepath.Join("memory", "topics", "agy-hq-architecture.md"))
	}

	// Initialize independent git repository inside local/ if not already present
	gitDir := filepath.Join(vaultDir, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		initCmd := exec.Command("git", "init", "-b", "main")
		initCmd.Dir = vaultDir
		if out, err := initCmd.CombinedOutput(); err != nil {
			return created, fmt.Errorf("git init in vault: %w (%s)", err, string(out))
		}
		// Configure local git identity fallback if needed so initial commit succeeds
		_ = exec.Command("git", "-C", vaultDir, "config", "user.name", "hq-vault").Run()
		_ = exec.Command("git", "-C", vaultDir, "config", "user.email", "hq-vault@localhost").Run()

		addCmd := exec.Command("git", "add", ".")
		addCmd.Dir = vaultDir
		if err := addCmd.Run(); err == nil {
			commitCmd := exec.Command("git", "commit", "-m", "chore(vault): initialize private agy-hq vault")
			commitCmd.Dir = vaultDir
			_ = commitCmd.Run()
		}
		created = append(created, ".git (initialized private Vault repository)")
	}

	// Ensure root repository .git/hooks/pre-commit and pre-push guards are installed
	rootHooksDir := filepath.Join(filepath.Dir(templateDir), ".git", "hooks")
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
				"# 2. Full 5-invariant staged PII & hooks.json parity check via ./bin/hq\n" +
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

	return created, nil
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
