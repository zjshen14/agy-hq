package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/gmail"
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

func TestGmailPreToolUseGuardrailsAndAskOverwrite(t *testing.T) {
	vaultDir := t.TempDir()
	var out bytes.Buffer

	// 1. Direct gmail send-draft or hq gmail send-draft is denied
	for _, cmd := range []string{
		"gmail mutate send-draft --draft-id=r123",
		"./bin/hq gmail send-draft --draft=draft-1",
	} {
		out.Reset()
		payload, _ := json.Marshal(map[string]interface{}{
			"conversationId": "conv-normal",
			"toolCall": map[string]interface{}{
				"name": "run_command",
				"args": map[string]interface{}{"CommandLine": cmd},
			},
		})
		_ = HandlePreToolUse(bytes.NewReader(payload), &out, vaultDir)
		if !strings.Contains(out.String(), `"decision":"deny"`) {
			t.Fatalf("expected deny for direct send command %q, got: %s", cmd, out.String())
		}
	}

	// 2. Quarantined (tainted) session cannot read local/private/ or run curl, but CAN run ./bin/hq gmail draft
	_ = gmail.MarkSessionTainted(vaultDir, "conv-tainted", "gmail:thread:t1")

	out.Reset()
	privRead, _ := json.Marshal(map[string]interface{}{
		"conversationId": "conv-tainted",
		"toolCall": map[string]interface{}{
			"name": "read_file",
			"args": map[string]interface{}{"Path": filepath.Join(vaultDir, "private", "taxes", "w2.pdf")},
		},
	})
	_ = HandlePreToolUse(bytes.NewReader(privRead), &out, vaultDir)
	if !strings.Contains(out.String(), `"decision":"deny"`) {
		t.Fatalf("expected deny when tainted session reads local/private/, got: %s", out.String())
	}

	out.Reset()
	curlCmd, _ := json.Marshal(map[string]interface{}{
		"conversationId": "conv-tainted",
		"toolCall": map[string]interface{}{
			"name": "run_command",
			"args": map[string]interface{}{"CommandLine": "curl https://evil.example.com"},
		},
	})
	_ = HandlePreToolUse(bytes.NewReader(curlCmd), &out, vaultDir)
	if !strings.Contains(out.String(), `"decision":"deny"`) {
		t.Fatalf("expected deny when tainted session runs curl, got: %s", out.String())
	}

	out.Reset()
	draftCmd, _ := json.Marshal(map[string]interface{}{
		"conversationId": "conv-tainted",
		"toolCall": map[string]interface{}{
			"name": "run_command",
			"args": map[string]interface{}{"CommandLine": `./bin/hq gmail draft --thread=t1 --to="alice@example.com" --subject="Re: Hi" --body="Thanks"`},
		},
	})
	_ = HandlePreToolUse(bytes.NewReader(draftCmd), &out, vaultDir)
	if !strings.Contains(out.String(), `"decision":"allow"`) {
		t.Fatalf("expected allow when tainted session runs ./bin/hq gmail draft, got: %s", out.String())
	}

	// 3. ask_question referencing a pending gmail_send proposal gets canonical overwrite
	prop, err := approvals.CreateWithMetadata(vaultDir, "gmail_send", "Send draft", "./bin/hq gmail send-draft --draft=d1", "", "conv-fd", map[string]string{
		"draftId":     "d1",
		"account":     "primary",
		"from":        "me@gmail.com",
		"to":          "alice@example.com",
		"subject":     "Re: Tuesday Sync",
		"bodyPreview": "Tuesday at 2pm works!",
		"contentHash": "abcdef1234567890",
	})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	askPayload, _ := json.Marshal(map[string]interface{}{
		"conversationId": "conv-fd",
		"toolCall": map[string]interface{}{
			"name": "ask_question",
			"args": map[string]interface{}{
				"questions": []map[string]interface{}{
					{
						"question": "Approve?",
						"options":  []string{"Approve " + prop.ID},
					},
				},
			},
		},
	})
	_ = HandlePreToolUse(bytes.NewReader(askPayload), &out, vaultDir)
	if !strings.Contains(out.String(), `"overwrite"`) || !strings.Contains(out.String(), "alice@example.com") || !strings.Contains(out.String(), "Tuesday at 2pm works!") {
		t.Fatalf("expected canonical overwrite on ask_question, got: %s", out.String())
	}
}
