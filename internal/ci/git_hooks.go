package ci

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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
		if strings.HasPrefix(kv, "GIT_") ||
			strings.HasPrefix(kv, "ANTIGRAVITY_") ||
			strings.HasPrefix(kv, "HQ_VAULT_DIR=") ||
			strings.HasPrefix(kv, "HQ_ROOT_DIR=") ||
			strings.HasPrefix(kv, "HQ_HOME_DIR=") ||
			strings.HasPrefix(kv, "HQ_GEMINI_DIR=") ||
			strings.HasPrefix(kv, "HQ_CONFIG_DIR=") ||
			strings.HasPrefix(kv, "HQ_APP_DATA_DIR=") ||
			strings.HasPrefix(kv, "HQ_BRAIN_DIR=") ||
			strings.HasPrefix(kv, "HQ_APPROVAL_EXEC_ID=") ||
			strings.HasPrefix(kv, "HQ_APPROVAL_EXEC_TOKEN=") ||
			strings.HasPrefix(kv, "HQ_FORCE_CAP_CHECK=") {
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
