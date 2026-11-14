package ci

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

func resolveGitHooksDir(rootDir string) string {
	hooksDir := filepath.Join(rootDir, ".git", "hooks")
	if st, err := os.Stat(hooksDir); err == nil && st.IsDir() {
		return hooksDir
	}
	if _, err := os.Stat(filepath.Join(rootDir, ".git")); err == nil {
		if out, gErr := gitOutput(rootDir, "rev-parse", "--git-path", "hooks"); gErr == nil {
			cand := strings.TrimSpace(out)
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

// InstallGitHooks installs or updates `.git/hooks/pre-commit` and `.git/hooks/pre-push` in rootDir.
func InstallGitHooks(rootDir string) ([]string, error) {
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

// CheckGitHooksInstalled verifies that `.git/hooks/pre-commit` and `.git/hooks/pre-push` exist and are executable.
func CheckGitHooksInstalled(rootDir string) (bool, string) {
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

// RunPreCommit executes the fast (<150ms) staged PII scan, hooks.json parity check,
// and Go formatting / Markdown integrity checks on staged files.
func RunPreCommit(rootDir string) error {
	violations, err := CheckPII(rootDir, true)
	if err != nil {
		return err
	}
	if len(violations) > 0 {
		var lines []string
		for _, v := range violations {
			lines = append(lines, "  - "+v.String())
		}
		return fmt.Errorf("pre-commit PII & Vault boundary check failed (%d violation(s)):\n%s", len(violations), strings.Join(lines, "\n"))
	}

	// Verify single-source plugin hooks.json & bundle parity if .agents/ exists
	_, hasLegacyHooks := os.Stat(filepath.Join(rootDir, ".agents", "hooks.json"))
	_, hasPluginDir := os.Stat(filepath.Join(rootDir, ".agents", "plugins", "agy-hq"))
	if hasLegacyHooks == nil || hasPluginDir == nil {
		if ok, detail := checkGateH2HooksJSONParity(rootDir); !ok {
			return fmt.Errorf("pre-commit hook contract check failed: %s", detail)
		}
		if ok, detail := checkGateH2CGlobalPluginBundle(rootDir, false); !ok {
			return fmt.Errorf("pre-commit global plugin bundle check failed: %s", detail)
		}
	}

	// Check staged .go files with `gofmt -s` and staged .md files for Markdown/Docs integrity
	namesOut, sErr := gitOutput(rootDir, "diff", "--cached", "--name-only", "--diff-filter=ACMR")
	if sErr == nil && strings.TrimSpace(namesOut) != "" {
		var stagedGo []string
		hasMd := false
		for _, f := range strings.Split(namesOut, "\n") {
			f = strings.TrimSpace(filepath.ToSlash(f))
			if f == "" {
				continue
			}
			if strings.HasSuffix(f, ".go") {
				stagedGo = append(stagedGo, f)
			}
			if strings.HasSuffix(strings.ToLower(f), ".md") {
				hasMd = true
			}
		}
		if len(stagedGo) > 0 {
			if ok, detail := CheckGoFormatting(rootDir, stagedGo...); !ok {
				return fmt.Errorf("pre-commit Go formatting check failed: %s", detail)
			}
		}
		if hasMd {
			if ok, detail := checkGateH4BMarkdownAndDocsIntegrity(rootDir); !ok {
				return fmt.Errorf("pre-commit Markdown & Docs integrity check failed: %s", detail)
			}
		}
	}
	return nil
}

// RunPrePush executes the full tracked-tree PII scan, harness contract gates (H1-H3),
// and (unless skipTests is true) `go vet ./...` and `go test ./...`.
func RunPrePush(rootDir string, skipTests bool) error {
	fmt.Println("==> [pre-push 1/3] Auditing tracked repository tree for PII & Vault boundary leaks...")
	violations, err := CheckPII(rootDir, false)
	if err != nil {
		return err
	}
	if len(violations) > 0 {
		var lines []string
		for _, v := range violations {
			lines = append(lines, "  - "+v.String())
		}
		return fmt.Errorf("pre-push PII & Vault boundary audit failed (%d violation(s)):\n%s", len(violations), strings.Join(lines, "\n"))
	}
	fmt.Println("    [PASS] Zero PII, Vault boundary, or credential leaks in tracked files")

	fmt.Println("==> [pre-push 2/3] Verifying Agent Skills, Hook Parity, Security Matrix & Routing Contracts...")
	results, allOK := CheckHarness(rootDir)
	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
		}
		fmt.Printf("    [%s] %-4s %-52s %s\n", status, r.Gate, r.Name, r.Detail)
	}
	if !allOK {
		return fmt.Errorf("pre-push agent harness contract check failed")
	}

	if skipTests || os.Getenv("HQ_SKIP_PRE_PUSH_TESTS") == "1" {
		fmt.Println("==> [pre-push 3/3] Skipping `go test ./...` (--skip-tests / HQ_SKIP_PRE_PUSH_TESTS=1)")
		return nil
	}

	fmt.Println("==> [pre-push 3/3] Running `go vet ./...` and full test suite (`go test ./...`)...")
	var baseEnv []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		baseEnv = append(baseEnv, kv)
	}
	baseEnv = append(baseEnv, "HQ_IN_PRE_PUSH=1")

	vetCmd := exec.Command("go", "vet", "./...")
	vetCmd.Dir = rootDir
	vetCmd.Env = baseEnv
	if out, err := vetCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go vet ./... failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}

	testCmd := exec.Command("go", "test", "./...")
	testCmd.Dir = rootDir
	testCmd.Env = baseEnv
	if out, err := testCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go test ./... failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("    [PASS] All unit and CUJ E2E tests passed!")
	return nil
}
