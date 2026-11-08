package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandleSessionStartAndPreToolUseAndStop(t *testing.T) {
	vaultDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(vaultDir, "USER.md"), []byte("Concise style"), 0o644)
	_ = os.WriteFile(filepath.Join(vaultDir, "MEMORY.md"), []byte("Active goal: ship M1"), 0o644)

	// 1. SessionStart
	var out bytes.Buffer
	err := HandleSessionStart(strings.NewReader(`{"conversationId":"c1"}`), &out, vaultDir)
	if err != nil {
		t.Fatalf("SessionStart error: %v", err)
	}
	if !strings.Contains(out.String(), "Concise style") || !strings.Contains(out.String(), "Active goal: ship M1") {
		t.Fatalf("unexpected SessionStart output: %s", out.String())
	}

	// 2. PreToolUse (allow safe command, deny outbound command)
	out.Reset()
	_ = HandlePreToolUse(strings.NewReader(`{"toolCall":{"name":"run_command","args":{"CommandLine":"go test ./..."}}}`), &out, vaultDir)
	if !strings.Contains(out.String(), `"decision":"allow"`) {
		t.Fatalf("expected allow, got: %s", out.String())
	}

	out.Reset()
	_ = HandlePreToolUse(strings.NewReader(`{"toolCall":{"name":"run_command","args":{"CommandLine":"gh pr merge 10"}}}`), &out, vaultDir)
	var denyResp map[string]interface{}
	_ = json.Unmarshal(out.Bytes(), &denyResp)
	if denyResp["decision"] != "deny" {
		t.Fatalf("expected deny for gh pr merge, got: %s", out.String())
	}

	// 3. Stop
	out.Reset()
	_ = HandleStop(strings.NewReader(`{"conversationId":"c1","executionNum":2,"terminationReason":"NO_TOOL_CALL","lastUserInput":"hello"}`), &out, vaultDir)
	if !strings.Contains(out.String(), `"decision":"stop"`) {
		t.Fatalf("expected stop, got: %s", out.String())
	}
}
