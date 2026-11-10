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
		"modelName":         "gemini-3.8-flash-high",
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
	subStartPayload := `{"conversationId":"conv-sub-child-999","modelName":"gemini-3.8-flash-high"}`
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
