package githooks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const preCommitScript = `#!/bin/sh
# agy-hq managed pre-commit hook (Ring 0 TCB — RFC-0011)
set -e
unset $(git rev-parse --local-env-vars 2>/dev/null || true)
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_PREFIX

# 1. Fast shell fallback guard on local/ and vault/ paths
if git diff --cached --name-only | grep -qE '^(local|vault)/'; then
  echo "ERROR: Refusing to commit files inside private local/ Vault." >&2
  exit 1
fi
USER_EMAIL="$(git config user.email 2>/dev/null || true)"
HOME_DIR="$HOME"
if [ -n "$HOME_DIR" ] && git diff --cached -U0 | grep -E '^\+' | grep -qF "$HOME_DIR"; then
  echo "ERROR: Staged diff contains your local HOME path ($HOME_DIR). Keep personal paths in local/." >&2
  exit 1
fi
if [ -n "$USER_EMAIL" ] && git diff --cached -U0 | grep -E '^\+' | grep -qFi "$USER_EMAIL"; then
  echo "ERROR: Staged diff contains your personal email ($USER_EMAIL). Keep personal state in local/." >&2
  exit 1
fi

# 2. Auto-rebuild ./bin/hq if missing or older than cmd/, internal/, or templates/ .go sources
export PATH="/opt/homebrew/bin:/usr/local/go/bin:$HOME/.local/go/bin:$HOME/.local/bin:$PATH"
if command -v go >/dev/null 2>&1 && [ -d "./cmd/hq" ]; then
  if [ ! -x "./bin/hq" ] || [ -n "$(find cmd internal templates -name '*.go' -newer ./bin/hq 2>/dev/null | head -n 1)" ]; then
    mkdir -p ./bin
    go build -o ./bin/hq ./cmd/hq
  fi
fi

# 3. Full staged PII, plugin/hooks contract, gofmt & Markdown check via ./bin/hq
if [ -x "./bin/hq" ]; then
  ./bin/hq ci pre-commit
fi
`

const prePushScript = `#!/bin/sh
# agy-hq managed pre-push hook (Ring 0 TCB — RFC-0011)
set -e
unset $(git rev-parse --local-env-vars 2>/dev/null || true)
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_PREFIX

if [ "${HQ_IN_PRE_PUSH:-0}" = "1" ]; then
  exit 0
fi
export HQ_IN_PRE_PUSH=1
export PATH="/opt/homebrew/bin:/usr/local/go/bin:$HOME/.local/go/bin:$HOME/.local/bin:$PATH"

if command -v go >/dev/null 2>&1 && [ -d "./cmd/hq" ]; then
  if [ ! -x "./bin/hq" ] || [ -n "$(find cmd internal templates -name '*.go' -newer ./bin/hq 2>/dev/null | head -n 1)" ]; then
    mkdir -p ./bin
    go build -o ./bin/hq ./cmd/hq
  fi
fi

if [ -x "./bin/hq" ]; then
  exec ./bin/hq ci pre-push
fi
`

func sanitizedGitEnv() []string {
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

func resolveGitHooksDir(rootDir string) string {
	hooksDir := filepath.Join(rootDir, ".git", "hooks")
	if st, err := os.Stat(hooksDir); err == nil && st.IsDir() {
		return hooksDir
	}
	if _, err := os.Stat(filepath.Join(rootDir, ".git")); err == nil {
		cmd := exec.Command("git", "rev-parse", "--git-path", "hooks")
		cmd.Dir = rootDir
		cmd.Env = sanitizedGitEnv()
		if out, gErr := cmd.Output(); gErr == nil {
			cand := strings.TrimSpace(string(out))
			if cand != "" {
				if !filepath.IsAbs(cand) {
					cand = filepath.Clean(filepath.Join(rootDir, cand))
				}
				if st, sErr := os.Stat(cand); sErr == nil && st.IsDir() {
					return cand
				}
			}
		}
	}
	return hooksDir
}

// Install installs or updates `.git/hooks/pre-commit` and `.git/hooks/pre-push` in rootDir.
func Install(rootDir string) ([]string, error) {
	hooksDir := resolveGitHooksDir(rootDir)
	st, err := os.Stat(hooksDir)
	if err != nil || !st.IsDir() {
		return nil, nil
	}

	var updated []string
	targets := []struct {
		name    string
		content string
	}{
		{"pre-commit", preCommitScript},
		{"pre-push", prePushScript},
	}

	for _, t := range targets {
		p := filepath.Join(hooksDir, t.name)
		existing, rErr := os.ReadFile(p)
		info, sErr := os.Stat(p)
		isExec := sErr == nil && (info.Mode().Perm()&0o111) != 0
		if rErr == nil && string(existing) == t.content && isExec {
			continue
		}
		if err := os.WriteFile(p, []byte(t.content), 0o755); err != nil {
			return updated, fmt.Errorf("write %s: %w", p, err)
		}
		if err := os.Chmod(p, 0o755); err != nil {
			return updated, fmt.Errorf("chmod %s: %w", p, err)
		}
		updated = append(updated, filepath.ToSlash(filepath.Join(".git", "hooks", t.name)))
	}
	return updated, nil
}

// CheckInstalled verifies that `.git/hooks/pre-commit` and `.git/hooks/pre-push` exist and are executable.
func CheckInstalled(rootDir string) (bool, string) {
	hooksDir := resolveGitHooksDir(rootDir)
	var missing []string
	for _, name := range []string{"pre-commit", "pre-push"} {
		p := filepath.Join(hooksDir, name)
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || (info.Mode().Perm()&0o111) == 0 {
			missing = append(missing, name)
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil || (!strings.Contains(string(data), "ci "+name) && name == "pre-push") {
			missing = append(missing, name+" (outdated)")
		}
	}
	if len(missing) > 0 {
		return false, fmt.Sprintf("missing or outdated git hook(s): %s (run './bin/hq ci install-hooks')", strings.Join(missing, ", "))
	}
	return true, "pre-commit & pre-push installed and executable (.git/hooks/)"
}
