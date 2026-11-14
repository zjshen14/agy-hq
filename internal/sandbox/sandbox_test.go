package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizedEnvAndSandboxProfileGeneration(t *testing.T) {
	vaultDir := t.TempDir()
	privDir := filepath.Join(vaultDir, "private", "gmail")
	if err := os.MkdirAll(privDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(privDir, "tokens.json")
	if err := os.WriteFile(secretFile, []byte(`{"access_token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "do-not-leak-csrf")
	t.Setenv("OPENAI_API_KEY", "sk-do-not-leak")
	env := SanitizedEnv(vaultDir)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "ANTIGRAVITY_CSRF_TOKEN=") || strings.Contains(joined, "OPENAI_API_KEY=") {
		t.Fatalf("SanitizedEnv leaked sensitive env vars:\n%s", joined)
	}
	if !strings.Contains(joined, "HQ_VAULT_DIR="+vaultDir) {
		t.Fatalf("expected HQ_VAULT_DIR in SanitizedEnv, got:\n%s", joined)
	}

	bwrapArgs := BuildBwrapArgs(vaultDir, vaultDir, true, []string{"sh", "-c", "ls"})
	bwrapJoined := strings.Join(bwrapArgs, " ")
	if !strings.Contains(bwrapJoined, "--unshare-net") || !strings.Contains(bwrapJoined, "--tmpfs "+filepath.Join(vaultDir, "private")) {
		t.Fatalf("expected --unshare-net and --tmpfs private mask in bwrap args, got:\n%s", bwrapJoined)
	}

	sbpl := BuildSeatbeltProfile(vaultDir, true)
	if !strings.Contains(sbpl, "(deny network*)") || !strings.Contains(sbpl, filepath.Join(vaultDir, "private")) {
		t.Fatalf("expected network deny and private subpath deny in Seatbelt profile, got:\n%s", sbpl)
	}

	// If OS sandbox is active on this runner, verify child command truly cannot see tokens.json!
	cmd, sandboxed, err := BuildCommand(context.Background(), vaultDir, vaultDir, false, []string{"sh", "-c", "test -f " + secretFile + " && echo LEAKED || echo MASKED"})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed command execution failed (sandboxed=%v): %v (%s)", sandboxed, err, string(out))
	}
	if sandboxed && !strings.Contains(string(out), "MASKED") {
		t.Fatalf("expected private/gmail/tokens.json to be masked inside OS sandbox, got: %s", string(out))
	}
}
