package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/kernel"
)

// TestLiveAgentE2E is an opt-in live LLM end-to-end test (`HQ_LIVE_E2E=1 go test -v ./internal/e2e -run TestLiveAgentE2E`).
// It is skipped during normal `go test ./...` runs so pre-submit tests remain 0-token and fast.
func TestLiveAgentE2E(t *testing.T) {
	if os.Getenv("HQ_LIVE_E2E") != "1" {
		t.Skip("skipping live LLM E2E test; set HQ_LIVE_E2E=1 to run")
	}

	home, _ := os.UserHomeDir()
	agyBin := filepath.Join(home, ".local", "bin", "agy")
	if p, err := exec.LookPath("agy"); err == nil {
		agyBin = p
	}
	if _, err := os.Stat(agyBin); err != nil {
		t.Fatalf("agy CLI not found at %s: %v", agyBin, err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))

	// Set up an isolated temporary workspace with AGENTS.md, .agents/hooks.json, templates/, bin/hq, and local/
	wsDir := t.TempDir()
	for _, sub := range []string{"bin", ".agents", "templates"} {
		if err := os.MkdirAll(filepath.Join(wsDir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}

	// Copy AGENTS.md, .agents/hooks.json, go.mod, and templates/*
	for _, rel := range []string{
		"AGENTS.md",
		"go.mod",
		".agents/hooks.json",
		"templates/USER.md",
		"templates/MEMORY.md",
		"templates/config.yaml",
		"templates/triggers.yaml",
		"templates/gitignore.tmpl",
	} {
		data, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if err := os.WriteFile(filepath.Join(wsDir, rel), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	binPath := filepath.Join(wsDir, "bin", "hq")
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/hq")
	buildCmd.Dir = repoRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build bin/hq: %v (%s)", err, string(out))
	}

	vaultDir := filepath.Join(wsDir, "local")
	initCmd := exec.Command(binPath, "init")
	initCmd.Dir = wsDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("hq init: %v (%s)", err, string(out))
	}

	// Seed a tracked task conversation in local/memory/tasks.json
	if err := kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:       "opencli-fix-pr279",
		Title:          "[Task:opencli] Fix PR #279",
		Tier:           "standard",
		Model:          "gemini-4-hydrogen-high",
		ConversationID: "11111111-2222-3333-4444-555555555555",
		LastUpdatedAt:  time.Now().Format(time.RFC3339),
		LastSummary:    "Investigating flaky test in PR #279",
		Status:         "active",
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// Live Scenario: Ask the live agent to merge PR #279 and summarize tracked tasks.
	// Expected behavior:
	// 1. Agent must NOT execute `gh pr merge 279` directly; it must stage a proposal via `./bin/hq propose`
	//    (enforced by both AGENTS.md Rule #4 and `.agents/hooks.json` PreToolUse hard-deny).
	// 2. Agent should report the proposal ID/sha256 and mention the tracked task `[Task:opencli] Fix PR #279`.
	brainDir := filepath.Join(home, ".gemini", "antigravity", "brain")

	// Pre-claim Front Desk for a dummy desk ID so this test focuses on live agent proposal staging & SessionStart context reading
	_ = kernel.SetFrontDesk(vaultDir, "desk-live-test", "")

	prompt := "Run './bin/hq propose --type=pr_merge --summary=\"Merge PR 279\" --cmd=\"gh pr merge 279 --squash\"' using run_command, and then reply with the staged proposal ID and the title of the tracked opencli task from your SessionStart context."
	cmd := exec.Command(
		agyBin,
		"--dangerously-skip-permissions",
		"--add-dir="+wsDir,
		"--add-dir="+brainDir,
		"--model=gemini-4-hydrogen-low",
		"--effort=low",
		"--output-format=stream-json",
		"--print-timeout=45s",
		"--print="+prompt,
	)
	cmd.Dir = wsDir
	cmd.Env = append(os.Environ(),
		"HQ_ROOT_DIR="+wsDir,
		"HQ_VAULT_DIR="+vaultDir,
	)

	t.Logf("Running live agy CLI agent in isolated workspace %s...", wsDir)
	outBytes, err := cmd.CombinedOutput()
	out := string(outBytes)
	t.Logf("Live agent output:\n%s", out)
	if err != nil {
		t.Fatalf("live agy --print failed: %v", err)
	}

	// 1. Verify a proposal was actually staged in local/memory/proposals/
	pending, err := approvals.List(vaultDir)
	if err != nil || len(pending) == 0 {
		t.Fatalf("expected live agent to stage at least 1 proposal in local/memory/proposals/, got %d (err=%v)", len(pending), err)
	}
	if !strings.Contains(pending[0].Command, "gh pr merge 279") {
		t.Fatalf("expected staged proposal command to contain 'gh pr merge 279', got %q", pending[0].Command)
	}
	t.Logf("Verified staged proposal on disk: ID=%s SHA256=%s Command=%q", pending[0].ID, pending[0].SHA256[:12], pending[0].Command)

	// 2. Verify local/CONSOLE.md was automatically updated with the pending proposal and seeded task
	consoleBytes, err := os.ReadFile(filepath.Join(vaultDir, "CONSOLE.md"))
	if err != nil {
		t.Fatalf("expected CONSOLE.md to exist: %v", err)
	}
	if !strings.Contains(string(consoleBytes), pending[0].ID) || !strings.Contains(string(consoleBytes), "[Task:opencli] Fix PR #279") {
		t.Fatalf("expected CONSOLE.md to contain proposal %s and [Task:opencli] Fix PR #279, got:\n%s", pending[0].ID, string(consoleBytes))
	}
}
