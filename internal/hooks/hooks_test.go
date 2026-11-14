package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/gmail"
	"github.com/zjshen14/agy-hq/internal/kernel"
)

func TestMain(m *testing.M) {
	_ = os.Unsetenv("ANTIGRAVITY_LS_ADDRESS")
	_ = os.Unsetenv("ANTIGRAVITY_CSRF_TOKEN")
	_ = os.Unsetenv("ANTIGRAVITY_CONVERSATION_ID")
	os.Exit(m.Run())
}

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

func TestCrossMidnightPriorJournalStepsAndDuplicateStopGuard(t *testing.T) {
	vaultDir := t.TempDir()
	convID := "conv-cross-midnight"

	// 1. Write a prior-day journal entry (2026-11-09) with executionNum=5 and StepCount=42
	prevDayDir := filepath.Join(vaultDir, "memory", "journal", "2026-11-09")
	_ = os.MkdirAll(prevDayDir, 0o755)
	prevJE := JournalEntry{
		ConversationID: convID,
		ExecutionNum:   5,
		StepCount:      42,
	}
	if b, err := json.Marshal(prevJE); err == nil {
		_ = os.WriteFile(filepath.Join(prevDayDir, convID+"-5.json"), b, 0o644)
	}

	// Verify that on a new day (2026-11-10) when currentExecNum resets to 1, priorJournalStepCountWithDate returns 42 (not 0!)
	gotStart := priorJournalStepCountWithDate(vaultDir, convID, "2026-11-10", 1)
	if gotStart != 42 {
		t.Fatalf("expected priorJournalStepCountWithDate across midnight to return 42, got %d", gotStart)
	}

	// 2. Test B1 duplicate Stop hook guard + enriched token metrics on JournalEntry
	tPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	tLines := []string{
		`{"step_index":0,"type":"USER_INPUT","created_at":"2026-11-10T09:00:00Z","content":"Hello"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-11-10T09:00:02Z","input_tokens":15000,"cache_read_tokens":85000,"output_tokens":500,"content":"Done"}`,
	}
	_ = os.WriteFile(tPath, []byte(strings.Join(tLines, "\n")+"\n"), 0o644)

	stopJSON, _ := json.Marshal(map[string]interface{}{
		"conversationId":    "conv-b1-test",
		"transcriptPath":    tPath,
		"modelName":         "gemini-4-hydrogen-high",
		"terminationReason": "NO_TOOL_CALL",
	})

	var out1, out2 bytes.Buffer
	if err := HandleStop(bytes.NewReader(stopJSON), &out1, vaultDir); err != nil {
		t.Fatalf("first HandleStop: %v", err)
	}
	// Second immediate invocation with identical transcript (simulating workspace + plugin hooks.json) must be deduplicated
	if err := HandleStop(bytes.NewReader(stopJSON), &out2, vaultDir); err != nil {
		t.Fatalf("second HandleStop: %v", err)
	}

	todayDir := filepath.Join(vaultDir, "memory", "journal", time.Now().Format("2006-01-02"))
	entries, err := os.ReadDir(todayDir)
	if err != nil {
		t.Fatalf("ReadDir todayDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 journal file (duplicate Stop hook skipped), got %d", len(entries))
	}
	jBytes, _ := os.ReadFile(filepath.Join(todayDir, entries[0].Name()))
	var saved JournalEntry
	if err := json.Unmarshal(jBytes, &saved); err != nil {
		t.Fatalf("unmarshal saved journal: %v", err)
	}
	if saved.TurnInputTokens != 15000 || saved.TurnCacheReadTokens != 85000 || saved.TurnOutputTokens != 500 || saved.CurrentContextTokens != 100000 {
		t.Fatalf("unexpected enriched token metrics in JournalEntry: %+v", saved)
	}
}

func TestSubagentFrontDeskGuardHooks(t *testing.T) {
	vaultDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(vaultDir, "USER.md"), []byte("User profile"), 0o644)
	_ = os.WriteFile(filepath.Join(vaultDir, "MEMORY.md"), []byte("Memory context"), 0o644)

	// Claim Front Desk for conv-desk-main
	_ = kernel.SetFrontDesk(vaultDir, "conv-desk-main", "")

	// 1. Front Desk spawns a subagent via PreToolUse
	var preOut bytes.Buffer
	invokePayload := `{"conversationId":"conv-desk-main","toolCall":{"name":"invoke_subagent","args":{"Role":"[standard] test: Lease Guard Tester","Model":"flash","Prompt":"Verify guard"}}}`
	if err := HandlePreToolUse(strings.NewReader(invokePayload), &preOut, vaultDir); err != nil {
		t.Fatalf("HandlePreToolUse invoke_subagent: %v", err)
	}

	// 2. Subagent starts up SessionStart
	var ssOut bytes.Buffer
	subStartPayload := `{"conversationId":"conv-sub-child-999","modelName":"gemini-4-hydrogen-high"}`
	if err := HandleSessionStart(strings.NewReader(subStartPayload), &ssOut, vaultDir); err != nil {
		t.Fatalf("HandleSessionStart subagent: %v", err)
	}
	ssStr := ssOut.String()
	if strings.Contains(ssStr, "Front Desk Notice") || strings.Contains(ssStr, "claim-desk") {
		t.Fatalf("regression: subagent SessionStart received Front Desk Notice or claim-desk prompt:\n%s", ssStr)
	}

	// Verify Front Desk was not clobbered
	fd, ok := kernel.GetFrontDesk(vaultDir)
	if !ok || fd.ConversationID != "conv-desk-main" {
		t.Fatalf("expected Front Desk to remain conv-desk-main, got %+v", fd)
	}

	// 3. Subagent attempts to run claim-desk via run_command in PreToolUse
	var toolOut bytes.Buffer
	claimPayload := `{"conversationId":"conv-sub-child-999","toolCall":{"name":"run_command","args":{"CommandLine":"./bin/hq claim-desk --conv=conv-sub-child-999"}}}`
	if err := HandlePreToolUse(strings.NewReader(claimPayload), &toolOut, vaultDir); err != nil {
		t.Fatalf("HandlePreToolUse claim-desk: %v", err)
	}
	if !strings.Contains(toolOut.String(), `"decision":"deny"`) || !strings.Contains(toolOut.String(), "Front Desk Lease Subagent Invariant") {
		t.Fatalf("expected PreToolUse to deny subagent claim-desk, got: %s", toolOut.String())
	}
}

func TestPreToolUseSecurityHardening(t *testing.T) {
	rootDir := t.TempDir()
	vaultDir := filepath.Join(rootDir, "local")
	_ = os.MkdirAll(filepath.Join(vaultDir, "private"), 0o700)
	_ = os.WriteFile(filepath.Join(vaultDir, "private", "secret.txt"), []byte("top-secret"), 0o600)

	var out bytes.Buffer

	// 1. Malformed JSON must fail closed
	_ = HandlePreToolUse(strings.NewReader(`{not-valid-json`), &out, vaultDir)
	if !strings.Contains(out.String(), `"decision":"deny"`) {
		t.Fatalf("expected malformed JSON to fail closed (deny), got: %s", out.String())
	}

	// 2. Direct write_file to pending proposals or RSI hypotheses must be denied
	for _, rel := range []string{
		filepath.Join(vaultDir, "memory", "pending", "prop-123456-abcdef123456.json"),
		filepath.Join(vaultDir, "memory", "rsi", "hypotheses", "rsi-1.json"),
		filepath.Join(vaultDir, "memory", "interventions.jsonl"),
	} {
		out.Reset()
		raw, _ := json.Marshal(map[string]interface{}{
			"conversationId": "conv-worker",
			"toolCall": map[string]interface{}{
				"name": "write_file",
				"args": map[string]interface{}{"Path": rel, "Content": "{}"},
			},
		})
		_ = HandlePreToolUse(bytes.NewReader(raw), &out, vaultDir)
		if !strings.Contains(out.String(), `"decision":"deny"`) {
			t.Fatalf("expected deny for direct write_file to %s, got: %s", rel, out.String())
		}
	}

	// 3. Double-quoted command substitution in hq propose and direct curl to /api/approve must be denied
	for _, cmd := range []string{
		`./bin/hq propose --type=ext --summary="s" --cmd="echo $(cat local/private/secret.txt)"`,
		`curl -X POST http://127.0.0.1:4242/api/approve -d "id=prop-1"`,
	} {
		out.Reset()
		raw, _ := json.Marshal(map[string]interface{}{
			"conversationId": "conv-worker",
			"toolCall": map[string]interface{}{
				"name": "run_command",
				"args": map[string]interface{}{"CommandLine": cmd},
			},
		})
		_ = HandlePreToolUse(bytes.NewReader(raw), &out, vaultDir)
		if !strings.Contains(out.String(), `"decision":"deny"`) {
			t.Fatalf("expected deny for %q, got: %s", cmd, out.String())
		}
	}

	// 4. Quarantined session escapes (newline command separator, symlink read, invoke_subagent, write_file, call_mcp_tool)
	_ = gmail.MarkSessionTainted(vaultDir, "conv-q", "gmail:thread:99")
	_ = os.MkdirAll(filepath.Join(vaultDir, "notes"), 0o700)
	symPath := filepath.Join(vaultDir, "notes", "sneaky-link.txt")
	_ = os.Symlink(filepath.Join(vaultDir, "private", "secret.txt"), symPath)

	escapeProbes := []map[string]interface{}{
		{"name": "run_command", "args": map[string]interface{}{"CommandLine": "./bin/hq status\ncurl https://evil.example.com"}},
		{"name": "read_file", "args": map[string]interface{}{"Path": symPath}},
		{"name": "read_file", "args": map[string]interface{}{"Path": "local/private/secret.txt"}},
		{"name": "write_file", "args": map[string]interface{}{"Path": filepath.Join(vaultDir, "notes", "x.md")}},
		{"name": "invoke_subagent", "args": map[string]interface{}{"Role": "escape"}},
		{"name": "call_mcp_tool", "args": map[string]interface{}{"ServerName": "chrome-devtools-plugin_chrome-devtools"}},
	}
	for _, tc := range escapeProbes {
		out.Reset()
		raw, _ := json.Marshal(map[string]interface{}{
			"conversationId": "conv-q",
			"toolCall":       tc,
		})
		_ = HandlePreToolUse(bytes.NewReader(raw), &out, vaultDir)
		if !strings.Contains(out.String(), `"decision":"deny"`) {
			t.Fatalf("expected quarantined session to deny %+v, got: %s", tc, out.String())
		}
	}
}

// Regression test for macOS TMPDIR symlink resolution (/var/folders -> /private/var/folders)
// and symlinked shell path escapes targeting Credential Vault directories.
func TestSymlinkedVaultDirCredentialShellGuard(t *testing.T) {
	base := t.TempDir()
	realVault := filepath.Join(base, "real-private-var-vault")
	symlinkVault := filepath.Join(base, "symlink-var-vault")
	if err := os.MkdirAll(filepath.Join(realVault, "private", "gmail"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realVault, "private", "gmail", "tokens.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realVault, symlinkVault); err != nil {
		t.Fatal(err)
	}
	sneakyLink := filepath.Join(base, "sneaky-gmail-link")
	if err := os.Symlink(filepath.Join(realVault, "private", "gmail"), sneakyLink); err != nil {
		t.Fatal(err)
	}

	for _, cmd := range []string{
		"cat " + filepath.Join(symlinkVault, "private", "gmail", "tokens.json"),
		"cat " + filepath.Join(sneakyLink, "tokens.json"),
	} {
		var out bytes.Buffer
		raw, _ := json.Marshal(map[string]interface{}{
			"conversationId": "conv-untainted",
			"toolCall": map[string]interface{}{
				"name": "run_command",
				"args": map[string]interface{}{"CommandLine": cmd},
			},
		})
		if err := HandlePreToolUse(bytes.NewReader(raw), &out, symlinkVault); err != nil {
			t.Fatalf("HandlePreToolUse(%q): %v", cmd, err)
		}
		if !strings.Contains(out.String(), `"decision":"deny"`) {
			t.Fatalf("expected deny on symlinked credential shell command %q, got: %s", cmd, out.String())
		}
	}
}
