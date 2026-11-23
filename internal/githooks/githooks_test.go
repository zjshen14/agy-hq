package githooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestInstallAndCheckGitHooks(t *testing.T) {
	tmpRepo := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmpRepo, ".git", "hooks"), 0o755)

	if ok, _ := CheckInstalled(tmpRepo); ok {
		t.Fatalf("expected CheckInstalled to be false before installation")
	}

	installed, err := Install(tmpRepo)
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	if len(installed) != 2 {
		t.Fatalf("expected 2 hooks installed (pre-commit, pre-push), got %v", installed)
	}

	if ok, detail := CheckInstalled(tmpRepo); !ok {
		t.Fatalf("expected CheckInstalled to pass after installation, got: %s", detail)
	}

	// Second run should be idempotent (0 files rewritten)
	installedAgain, err := Install(tmpRepo)
	if err != nil || len(installedAgain) != 0 {
		t.Fatalf("expected idempotent Install to rewrite 0 files, got %v (%v)", installedAgain, err)
	}
}

func TestInstallGitHooksResolvesWorktreeDotGitFile(t *testing.T) {
	base := t.TempDir()
	mainRepo := filepath.Join(base, "repo")
	wtDir := filepath.Join(base, "wt")

	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = sanitizedGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}

	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(mainRepo, "init", "-b", "main")
	runGit(mainRepo, "config", "user.name", "Test")
	runGit(mainRepo, "config", "user.email", "test@agy-hq.local")
	if err := os.WriteFile(filepath.Join(mainRepo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(mainRepo, "add", "README.md")
	runGit(mainRepo, "commit", "-m", "init")
	runGit(mainRepo, "worktree", "add", "-b", "feat/wt", wtDir)

	installed, err := Install(wtDir)
	if err != nil {
		t.Fatalf("Install in worktree failed: %v", err)
	}
	if len(installed) != 2 {
		t.Fatalf("expected 2 hooks installed via worktree .git file, got %v", installed)
	}
	if ok, detail := CheckInstalled(wtDir); !ok {
		t.Fatalf("expected CheckInstalled to pass in worktree, got: %s", detail)
	}
}
