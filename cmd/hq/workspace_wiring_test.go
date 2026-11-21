package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/zjshen14/agy-hq/internal/kernel"
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

// TestFrontDeskContextInspectorWiredFromObs proves importing internal/obs in cmd/hq registers
// InspectFrontDeskContext with internal/kernel so EvaluateFrontDeskCompactionGate reads live transcripts.
func TestFrontDeskContextInspectorWiredFromObs(t *testing.T) {
	vaultDir := t.TempDir()
	brainDir := filepath.Join(t.TempDir(), "brain")
	t.Setenv("HQ_BRAIN_DIR", brainDir)

	fdID := "99999999-1111-4111-8111-111111111111"
	artDir := filepath.Join(brainDir, fdID)
	logDir := filepath.Join(artDir, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir logDir: %v", err)
	}
	if err := kernel.SetFrontDesk(vaultDir, fdID, artDir); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}

	transcriptPath := filepath.Join(logDir, "transcript.jsonl")
	lines := "" +
		`{"step_index":0,"type":"USER_INPUT","created_at":"2026-11-21T07:00:00Z","content":"hi"}` + "\n" +
		`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-11-21T07:00:02Z","input_tokens":15000,"cache_read_tokens":120000,"output_tokens":200}` + "\n" +
		`{"step_index":2,"type":"PLANNER_RESPONSE","created_at":"2026-11-21T07:01:02Z","input_tokens":16000,"cache_read_tokens":121000,"output_tokens":250}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(lines), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	dec := kernel.EvaluateFrontDeskCompactionGate(vaultDir, fdID, 0, 0)
	if !dec.ShouldCompact || dec.CurrentContextTokens != 137000 || dec.PlannerResponsesSinceCheckpoint != 2 {
		t.Fatalf("expected wired gate to allow compaction at 137k / 2 steps, got %+v", dec)
	}
}
