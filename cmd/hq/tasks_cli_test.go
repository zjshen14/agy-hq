package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zjshen14/agy-hq/internal/kernel"
)

func TestTasksCLIRetiredAndUnknownSubcommands(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "hq-test-bin")
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build cmd/hq failed: %v (%s)", err, string(out))
	}

	wsRoot := t.TempDir()
	vaultDir := filepath.Join(wsRoot, "local")
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		t.Fatalf("mkdir memory: %v", err)
	}
	const demoConvID = "11111111-1111-1111-1111-111111111111"
	if err := kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:       "demo-arch",
		Title:          "[Task:demo] Archived Demo",
		ConversationID: demoConvID,
		Status:         "archived",
		LastUpdatedAt:  time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("RegisterTask: %v", err)
	}

	baseEnv := make([]string, 0, len(os.Environ())+5)
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "HQ_VAULT_DIR="),
			strings.HasPrefix(kv, "HQ_ROOT_DIR="),
			strings.HasPrefix(kv, "ANTIGRAVITY_CONVERSATION_ID="),
			strings.HasPrefix(kv, "ANTIGRAVITY_LS_ADDRESS="),
			strings.HasPrefix(kv, "ANTIGRAVITY_CSRF_TOKEN="):
			continue
		default:
			baseEnv = append(baseEnv, kv)
		}
	}
	baseEnv = append(baseEnv,
		"HQ_VAULT_DIR="+vaultDir,
		"HQ_ROOT_DIR="+wsRoot,
		"ANTIGRAVITY_CONVERSATION_ID=",
		"ANTIGRAVITY_LS_ADDRESS=",
		"ANTIGRAVITY_CSRF_TOKEN=",
	)

	runCLI := func(args ...string) (string, string, int) {
		cmd := exec.Command(binPath, args...)
		cmd.Dir = wsRoot
		cmd.Env = baseEnv
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		exitCode := 0
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				t.Fatalf("exec %v unexpected error: %v", args, err)
			}
		}
		return stdout.String(), stderr.String(), exitCode
	}

	assertDemoStatus := func(want string) {
		t.Helper()
		tasks, err := kernel.ListTasks(vaultDir)
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		for _, task := range tasks {
			if task.TaskSlug == "demo-arch" {
				if task.Status != want {
					t.Fatalf("demo-arch status = %q, want %q", task.Status, want)
				}
				return
			}
		}
		t.Fatalf("demo-arch task not found in %+v", tasks)
	}

	// 1. Retired `hq tasks reopen` and unknown positional subcommands/args must reject loudly with exit 1,
	// empty stdout (no Task Board), stderr mentioning 'hq reopen', and without mutating task state.
	rejectedCases := []struct {
		name           string
		args           []string
		wantStderrFrag string
	}{
		{name: "tasks_reopen_slug", args: []string{"tasks", "reopen", "demo-arch"}, wantStderrFrag: "unknown tasks subcommand"},
		{name: "tasks_reopen_slug_no_worktree", args: []string{"tasks", "reopen", "demo-arch", "--no-worktree"}, wantStderrFrag: "unknown tasks subcommand"},
		{name: "tasks_reopen_no_args", args: []string{"tasks", "reopen"}, wantStderrFrag: "unknown tasks subcommand"},
		{name: "tasks_unknown_subcmd", args: []string{"tasks", "unknown-subcmd"}, wantStderrFrag: "unknown tasks subcommand"},
		{name: "tasks_help_word", args: []string{"tasks", "help"}, wantStderrFrag: "unknown tasks subcommand"},
		{name: "tasks_flag_then_reopen", args: []string{"tasks", "--json", "reopen"}, wantStderrFrag: "unknown tasks argument"},
	}
	for _, tc := range rejectedCases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, exitCode := runCLI(tc.args...)
			if exitCode != 1 {
				t.Fatalf("%v exitCode = %d, want 1 (stdout=%q stderr=%q)", tc.args, exitCode, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("%v stdout = %q, want empty", tc.args, stdout)
			}
			if !strings.Contains(stderr, tc.wantStderrFrag) || !strings.Contains(stderr, "'hq reopen'") {
				t.Fatalf("%v stderr = %q, want fragments %q and %q", tc.args, stderr, tc.wantStderrFrag, "'hq reopen'")
			}
			assertDemoStatus("archived")
		})
	}

	// 2. Valid `hq tasks` flag invocations and help flags remain working with exit 0.
	for _, args := range [][]string{
		{"tasks"},
		{"tasks", "--all"},
		{"tasks", "--search=demo-arch"},
		{"tasks", "--json"},
	} {
		stdout, stderr, exitCode := runCLI(args...)
		if exitCode != 0 {
			t.Fatalf("%v exitCode = %d, want 0 (stdout=%q stderr=%q)", args, exitCode, stdout, stderr)
		}
	}
	for _, args := range [][]string{
		{"tasks", "-h"},
		{"tasks", "--help"},
	} {
		stdout, stderr, exitCode := runCLI(args...)
		if exitCode != 0 {
			t.Fatalf("%v exitCode = %d, want 0 (stdout=%q stderr=%q)", args, exitCode, stdout, stderr)
		}
		if !strings.Contains(stdout+stderr, "Usage of tasks:") {
			t.Fatalf("%v output missing 'Usage of tasks:': stdout=%q stderr=%q", args, stdout, stderr)
		}
	}

	// 3. Replacement command `hq reopen` succeeds and transitions `demo-arch` to active.
	stdout, stderr, exitCode := runCLI("reopen", "demo-arch", "--no-worktree", "--no-sidebar")
	if exitCode != 0 {
		t.Fatalf("hq reopen exitCode = %d, want 0 (stdout=%q stderr=%q)", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, `Reopened task "demo-arch"`) {
		t.Fatalf("hq reopen stdout = %q, want Reopened task \"demo-arch\"", stdout)
	}
	assertDemoStatus("active")
}

func TestShortPrefix(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{name: "empty", s: "", n: 8, want: ""},
		{name: "zero_n", s: "abcdef", n: 0, want: ""},
		{name: "negative_n", s: "abcdef", n: -1, want: ""},
		{name: "negative_large_n", s: "abcdef", n: -10, want: ""},
		{name: "ascii_shorter", s: "abc", n: 8, want: "abc"},
		{name: "ascii_exact", s: "abcdefgh", n: 8, want: "abcdefgh"},
		{name: "ascii_longer", s: "0123456789abcdef", n: 12, want: "0123456789ab"},
		{name: "utf8_rune_count_fits", s: "日本", n: 4, want: "日本"},
		{name: "utf8_truncated", s: "日本語テスト", n: 4, want: "日本語テ"},
		{name: "mixed_ascii_utf8_truncated", s: "a日本語テスト", n: 4, want: "a日本語"},
		{name: "utf8_negative_n", s: "日本語", n: -2, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shortPrefix(tc.s, tc.n)
			if got != tc.want {
				t.Fatalf("shortPrefix(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("shortPrefix(%q, %d) returned invalid UTF-8: %q", tc.s, tc.n, got)
			}
		})
	}
}
