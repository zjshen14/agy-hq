package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/zjshen14/agy-hq/internal/workspace"
)

// TestGitWorktreeProviderWiredToWorktreeAdd proves the binary wires internal/worktree.Add into the built-in
// core/git-worktree provider, so provider-driven creation uses exactly the same code path as `hq worktree add`.
func TestGitWorktreeProviderWiredToWorktreeAdd(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "demo-repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@agy-hq.local"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "initial commit"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	vaultDir := t.TempDir()

	p, err := workspace.Resolve(vaultDir, workspace.CoreGitProviderID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !p.Capabilities().Create {
		t.Fatal("expected cmd/hq to wire worktree creation into core/git-worktree")
	}
	res, err := p.Create(context.Background(), workspace.CreateRequest{Name: "ledger-fix", Base: repo})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wtDir := filepath.Join(vaultDir, "worktrees", "demo-repo-ledger-fix")
	if st, err := os.Stat(wtDir); err != nil || !st.IsDir() {
		t.Fatalf("expected worktree.Add to create %s: %v", wtDir, err)
	}
	if res.Existed || res.Info.Name != "demo-repo-ledger-fix" || len(res.Info.Changes) != 1 || res.Info.Changes[0].ID != "branch:hq/ledger-fix" {
		t.Fatalf("unexpected create result: %+v", res)
	}
	if err := p.Remove(context.Background(), workspace.RemoveRequest{WorkspaceID: res.Info.WorkspaceID, RequireClean: true}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("worktree still present after Remove: %v", err)
	}
}
