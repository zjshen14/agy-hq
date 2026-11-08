package vault

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Fact represents a bi-temporal fact entry in local/memory/facts.jsonl.
type Fact struct {
	ID        string  `json:"id"`
	Topic     string  `json:"topic"`
	Statement string  `json:"statement"`
	ValidFrom string  `json:"valid_from"`
	ValidTo   *string `json:"valid_to"`
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

	// Ensure root repository .git/hooks/pre-commit PII guard is installed
	rootHooksDir := filepath.Join(filepath.Dir(templateDir), ".git", "hooks")
	if info, err := os.Stat(rootHooksDir); err == nil && info.IsDir() {
		preCommitPath := filepath.Join(rootHooksDir, "pre-commit")
		if _, err := os.Stat(preCommitPath); os.IsNotExist(err) {
			hookScript := "#!/bin/sh\n" +
				"# Block any staged files inside local/ or containing the local user's home path / git email\n" +
				"if git diff --cached --name-only | grep -qE '^(local|vault)/'; then\n" +
				"  echo \"ERROR: Refusing to commit files inside private local/ Vault.\"\n" +
				"  exit 1\n" +
				"fi\n" +
				"USER_EMAIL=\"$(git config user.email 2>/dev/null || true)\"\n" +
				"HOME_DIR=\"$HOME\"\n" +
				"if [ -n \"$HOME_DIR\" ] && git diff --cached -U0 | grep -E '^\\+' | grep -qF \"$HOME_DIR\"; then\n" +
				"  echo \"ERROR: Staged diff contains your local HOME path ($HOME_DIR). Keep personal paths in local/.\"\n" +
				"  exit 1\n" +
				"fi\n" +
				"if [ -n \"$USER_EMAIL\" ] && git diff --cached -U0 | grep -E '^\\+' | grep -qFi \"$USER_EMAIL\"; then\n" +
				"  echo \"ERROR: Staged diff contains your personal email ($USER_EMAIL). Keep personal state in local/.\"\n" +
				"  exit 1\n" +
				"fi\n"
			if err := os.WriteFile(preCommitPath, []byte(hookScript), 0o755); err == nil {
				created = append(created, "../.git/hooks/pre-commit (PII guard)")
			}
		}
	}

	return created, nil
}

// LoadActiveFacts reads active facts (valid_to == null) from local/memory/facts.jsonl.
func LoadActiveFacts(vaultDir string, limit int) ([]Fact, error) {
	factsPath := filepath.Join(vaultDir, "memory", "facts.jsonl")
	f, err := os.Open(factsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var active []Fact
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var fact Fact
		if err := json.Unmarshal([]byte(line), &fact); err == nil {
			if fact.ValidTo == nil {
				active = append(active, fact)
			}
		}
	}
	if limit > 0 && len(active) > limit {
		active = active[len(active)-limit:]
	}
	return active, nil
}
