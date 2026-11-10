package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func resolveRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func TestCheckPII_RealRepositoryClean(t *testing.T) {
	rootDir := resolveRepoRoot(t)
	violations, err := CheckPII(rootDir, false)
	if err != nil {
		t.Fatalf("CheckPII failed: %v", err)
	}
	if len(violations) > 0 {
		for _, v := range violations {
			t.Errorf("unexpected PII violation in repo: %s", v.String())
		}
		t.Fatalf("expected 0 PII violations in tracked repo, got %d", len(violations))
	}
}

func TestCheckPII_AllFiveInvariantsDetected(t *testing.T) {
	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}

	runGit("init", "-b", "main")
	runGit("config", "user.email", "test@agy-hq.local")
	runGit("config", "user.name", "Test")

	// Missing /local/ in .gitignore -> triggers P2-VaultIgnore
	_ = os.WriteFile(filepath.Join(tmpRepo, ".gitignore"), []byte("/bin/\n"), 0o644)

	// Forbidden tracked path in local/ -> triggers P1-ForbiddenPath
	_ = os.MkdirAll(filepath.Join(tmpRepo, "local"), 0o755)
	_ = os.WriteFile(filepath.Join(tmpRepo, "local", "notes.md"), []byte("secret vault note\n"), 0o644)

	// Tracked file containing P3 (workstation home path), P4 (OAuth token + GitHub PAT + SSN), and P5 (non-allowlisted personal email).
	// Concatenated at runtime so ci_test.go itself remains 100% free of literal PII/secret patterns when tracked by git.
	leakedSrc := strings.Join([]string{
		"package sample",
		`const wsPath = "/Users/"` + ` + "johndoe/Workspaces/secret"`,
		`const wsLiteral = "` + "/Users/" + "johndoe/Workspaces/secret" + `"`,
		`const oauthTok = "` + "ya29." + "a0AfH6SMBx1234567890abcdefghijklmnop" + `"`,
		`const ghPat = "` + "ghp_" + "1234567890abcdefghijklmnopqrstuvwxyzABCD" + `"`,
		`const ssnVal = "` + "987-" + "65-4321" + `"`,
		`const personalEmail = "` + "johndoe.personal" + "@gmail.com" + `"`,
		`const allowedEmail = "alice@example.com"`,
	}, "\n") + "\n"
	_ = os.WriteFile(filepath.Join(tmpRepo, "leak.go"), []byte(leakedSrc), 0o644)

	runGit("add", "-f", ".gitignore", "local/notes.md", "leak.go")

	// 1. Test staged mode (`--staged`)
	stagedViolations, err := CheckPII(tmpRepo, true)
	if err != nil {
		t.Fatalf("CheckPII staged failed: %v", err)
	}
	if len(stagedViolations) < 5 {
		t.Fatalf("expected at least 5 staged PII violations, got %d: %+v", len(stagedViolations), stagedViolations)
	}

	// 2. Commit and test full tracked-tree mode
	runGit("commit", "-m", "seed violations")
	treeViolations, err := CheckPII(tmpRepo, false)
	if err != nil {
		t.Fatalf("CheckPII full tree failed: %v", err)
	}

	seenInvariants := make(map[string]bool)
	for _, v := range treeViolations {
		seenInvariants[v.Invariant] = true
	}
	for _, want := range []string{
		"P1-ForbiddenPath",
		"P2-VaultIgnore",
		"P3-WorkstationPath",
		"P4-SecretPattern",
		"P4-SSNPattern",
		"P5-EmailAllowlist",
	} {
		if !seenInvariants[want] {
			t.Errorf("expected invariant %s to be triggered, got violations: %+v", want, treeViolations)
		}
	}
}

func TestCheckHarness_RealRepositoryAndMutations(t *testing.T) {
	rootDir := resolveRepoRoot(t)

	// 1. Real repository must pass all harness gates (H1, H2-A, H2-B, H3)
	results, ok := CheckHarness(rootDir)
	if !ok {
		for _, r := range results {
			t.Logf("[%s] %s: passed=%v (%s)", r.Gate, r.Name, r.Passed, r.Detail)
		}
		t.Fatalf("expected CheckHarness to pass on real repository")
	}

	// 2. Test negative mutations in an isolated workspace copy
	badRoot := t.TempDir()
	_ = os.MkdirAll(filepath.Join(badRoot, ".agents", "plugins", "agy-hq", "skills", "bad-skill"), 0o755)
	_ = os.MkdirAll(filepath.Join(badRoot, "templates"), 0o755)

	// Write drifted hooks.json files
	_ = os.WriteFile(filepath.Join(badRoot, ".agents", "hooks.json"), []byte(`{"hq-kernel":{}}`), 0o644)
	_ = os.WriteFile(filepath.Join(badRoot, ".agents", "plugins", "agy-hq", "hooks.json"), []byte(`{"drifted":true}`), 0o644)

	// Write SKILL.md with mismatched name in YAML frontmatter
	_ = os.WriteFile(filepath.Join(badRoot, ".agents", "plugins", "agy-hq", "skills", "bad-skill", "SKILL.md"),
		[]byte("---\nname: wrong-slug\ndescription: test\n---\n# Bad Skill\n"), 0o644)

	// Write triggers.yaml referencing a missing skill
	_ = os.WriteFile(filepath.Join(badRoot, "templates", "triggers.yaml"),
		[]byte("schedules:\n  - id: test-job\n    cron: \"30 7 * * *\"\n    skill: non-existent-skill\n"), 0o644)

	// Write go.mod with a forbidden external require directive -> triggers H4-A
	_ = os.WriteFile(filepath.Join(badRoot, "go.mod"),
		[]byte("module github.com/zjshen14/agy-hq\n\ngo 1.25.0\n\nrequire github.com/pkg/errors v0.9.1\n"), 0o644)

	// Write a Markdown file with a broken relative link -> triggers H4-B
	_ = os.WriteFile(filepath.Join(badRoot, "README.md"),
		[]byte("# Test\nSee [missing doc](docs/does-not-exist.md).\n"), 0o644)

	badResults, badOK := CheckHarness(badRoot)
	if badOK {
		t.Fatalf("expected CheckHarness to fail on mutated badRoot, got pass")
	}
	failedGates := make(map[string]bool)
	for _, r := range badResults {
		if !r.Passed {
			failedGates[r.Gate] = true
		}
	}
	for _, wantFail := range []string{"H1", "H2-A", "H3", "H4-A", "H4-B"} {
		if !failedGates[wantFail] {
			t.Errorf("expected gate %s to fail on mutated workspace, got results: %+v", wantFail, badResults)
		}
	}
}

func TestInstallAndCheckGitHooks(t *testing.T) {
	tmpRepo := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmpRepo, ".git", "hooks"), 0o755)

	if ok, _ := CheckGitHooksInstalled(tmpRepo); ok {
		t.Fatalf("expected CheckGitHooksInstalled to be false before installation")
	}

	installed, err := InstallGitHooks(tmpRepo)
	if err != nil {
		t.Fatalf("InstallGitHooks failed: %v", err)
	}
	if len(installed) != 2 {
		t.Fatalf("expected 2 hooks installed (pre-commit, pre-push), got %v", installed)
	}

	if ok, detail := CheckGitHooksInstalled(tmpRepo); !ok {
		t.Fatalf("expected CheckGitHooksInstalled to pass after installation, got: %s", detail)
	}

	// Second run should be idempotent (0 files rewritten)
	installedAgain, err := InstallGitHooks(tmpRepo)
	if err != nil || len(installedAgain) != 0 {
		t.Fatalf("expected idempotent InstallGitHooks to rewrite 0 files, got %v (%v)", installedAgain, err)
	}
}

func TestP6ContextualPIIAndLocalDenylist(t *testing.T) {
	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}

	runGit("init", "-b", "main")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@agy-hq.local")

	_ = os.WriteFile(filepath.Join(tmpRepo, ".gitignore"), []byte("/local/\n/vault/\n"), 0o644)

	// Configure an untracked local/private/pii-denylist.txt
	privDir := filepath.Join(tmpRepo, "local", "private")
	_ = os.MkdirAll(privDir, 0o755)
	secretSlug := "secret" + "-family-calendar"
	_ = os.WriteFile(filepath.Join(privDir, "pii-denylist.txt"), []byte("# Local private terms\n"+secretSlug+"\n"), 0o644)

	// Synthetic space IDs and Workspaces/... paths should pass, while opaque space IDs and denylist terms fail
	opaqueSpace := "spaces/" + "OpaqueDm_77"
	content := strings.Join([]string{
		"package sample",
		`const okSpace1 = "spaces/AAAA_LAUNCH_01"`,
		`const okSpace2 = "spaces/TEST_DM_AAAAE"`,
		`const okRoute = "spaces/direct-messages"`,
		`const okWorkspace = "Workspaces/project-alpha"`,
		`const badSpace = "` + opaqueSpace + `"`,
		`const badTerm = "` + strings.ToUpper(secretSlug) + `"`,
	}, "\n") + "\n"
	_ = os.WriteFile(filepath.Join(tmpRepo, "sample.go"), []byte(content), 0o644)
	runGit("add", ".gitignore", "sample.go")
	runGit("commit", "-m", "test P6 invariants")

	violations, err := CheckPII(tmpRepo, false)
	if err != nil {
		t.Fatalf("CheckPII failed: %v", err)
	}
	if len(violations) != 2 {
		t.Fatalf("expected exactly 2 P6-ContextualPII violations (opaque space ID + denylist term), got %d: %+v", len(violations), violations)
	}
	for _, v := range violations {
		if v.Invariant != "P6-ContextualPII" {
			t.Errorf("expected P6-ContextualPII, got %+v", v)
		}
		if strings.Contains(strings.ToLower(v.Detail), secretSlug) {
			t.Errorf("expected denylist violation detail to redact full secret term, got %q", v.Detail)
		}
	}
}

func TestCheckPIIHistoryAndReplacementEngine(t *testing.T) {
	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}

	runGit("init", "-b", "main")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@agy-hq.local")
	_ = os.WriteFile(filepath.Join(tmpRepo, ".gitignore"), []byte("/local/\n/vault/\n"), 0o644)

	leakedEmail := "historical.leak" + "@personal-mail.io"
	leakedSpace := "spaces/" + "OpaqueRm_99"

	// Commit 1: introduces historical PII
	_ = os.WriteFile(filepath.Join(tmpRepo, "notes.txt"),
		[]byte("Contact: "+leakedEmail+" in "+leakedSpace+"\n"), 0o644)
	runGit("add", ".gitignore", "notes.txt")
	runGit("commit", "-m", "initial commit with leak")

	// Commit 2: removes PII from HEAD
	_ = os.WriteFile(filepath.Join(tmpRepo, "notes.txt"),
		[]byte("Contact: alice@example.com in spaces/AAAA_LAUNCH_01\n"), 0o644)
	runGit("add", "notes.txt")
	runGit("commit", "-m", "scrubbed HEAD only")

	// HEAD tree check must be clean (0 violations)
	headViolations, err := CheckPII(tmpRepo, false)
	if err != nil || len(headViolations) != 0 {
		t.Fatalf("expected HEAD CheckPII to be clean, got %v (%v)", headViolations, err)
	}

	// Full history check must still catch Commit 1's leaked email and opaque space ID!
	histViolations, err := CheckPIIHistory(tmpRepo)
	if err != nil {
		t.Fatalf("CheckPIIHistory failed: %v", err)
	}
	if len(histViolations) < 2 {
		t.Fatalf("expected CheckPIIHistory to catch >=2 historical leaks from Commit 1, got %v", histViolations)
	}

	// Verify replacement rule parser & dry-run scrubber report
	rulesFile := filepath.Join(t.TempDir(), "replacements.txt")
	rulesContent := strings.Join([]string{
		"# Deterministic history replacement map",
		leakedEmail + "==>alice@example.com",
		"regex:spaces/" + "OpaqueRm_[A-Za-z0-9]+==>spaces/AAAA_LAUNCH_01",
	}, "\n") + "\n"
	_ = os.WriteFile(rulesFile, []byte(rulesContent), 0o644)

	rep, err := ScrubGitHistory(tmpRepo, rulesFile, false)
	if err != nil {
		t.Fatalf("ScrubGitHistory dry-run failed: %v", err)
	}
	if !rep.DryRun || rep.RuleCount != 2 || rep.MatchedCommits == 0 || rep.MatchedReplacements < 2 {
		t.Fatalf("unexpected dry-run ScrubGitHistory report: %+v", rep)
	}

	// Verify ApplyReplacementRulesToTree rewrites matching files in place
	treeDir := t.TempDir()
	targetFile := filepath.Join(treeDir, "doc.md")
	_ = os.WriteFile(targetFile, []byte("Send to "+leakedEmail+" and "+leakedSpace+"\n"), 0o644)
	modified, err := ApplyReplacementRulesToTree(treeDir, rulesFile)
	if err != nil || modified != 1 {
		t.Fatalf("expected ApplyReplacementRulesToTree to modify 1 file, got %d (%v)", modified, err)
	}
	updatedBytes, _ := os.ReadFile(targetFile)
	if string(updatedBytes) != "Send to alice@example.com and spaces/AAAA_LAUNCH_01\n" {
		t.Fatalf("unexpected scrubbed file content: %q", string(updatedBytes))
	}
}
