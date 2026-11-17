package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/events"
)

func TestResolveBodyInput(t *testing.T) {
	// 1. Inline --body
	got, err := resolveBodyInput("hello", false, "", strings.NewReader(""))
	if err != nil || got != "hello" {
		t.Fatalf("inline body: got (%q, %v)", got, err)
	}

	// 2. --body-stdin preserves '$' and multi-line content
	stdinMsg := "Total reimbursement: $382.34\nSecond line with $100 and $VAR."
	got, err = resolveBodyInput("", true, "", strings.NewReader(stdinMsg))
	if err != nil || got != stdinMsg {
		t.Fatalf("body-stdin: got (%q, %v), want %q", got, err, stdinMsg)
	}

	// 3. --body-file preserves '$' and multi-line content
	tmpFile := filepath.Join(t.TempDir(), "msg.txt")
	if err := os.WriteFile(tmpFile, []byte(stdinMsg), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	got, err = resolveBodyInput("", false, tmpFile, strings.NewReader(""))
	if err != nil || got != stdinMsg {
		t.Fatalf("body-file: got (%q, %v), want %q", got, err, stdinMsg)
	}

	// 4. Conflicting flags fail cleanly
	if _, err := resolveBodyInput("inline", true, "", strings.NewReader("stdin")); err == nil {
		t.Fatalf("expected error combining --body and --body-stdin")
	}
	if _, err := resolveBodyInput("inline", false, tmpFile, strings.NewReader("")); err == nil {
		t.Fatalf("expected error combining --body and --body-file")
	}
	if _, err := resolveBodyInput("", true, tmpFile, strings.NewReader("stdin")); err == nil {
		t.Fatalf("expected error combining --body-stdin and --body-file")
	}
}

func TestFormatBodyVerificationSummary(t *testing.T) {
	single := formatBodyVerificationSummary("Amount is $382.34")
	if !strings.Contains(single, "17 bytes, 17 runes, 1 '$' chars") || !strings.Contains(single, `line: "Amount is $382.34"`) {
		t.Fatalf("unexpected single-line summary: %s", single)
	}

	multi := formatBodyVerificationSummary("Line one $10\nLine two $20\nLine three $30")
	if !strings.Contains(multi, "3 '$' chars") || !strings.Contains(multi, `first: "Line one $10"`) || !strings.Contains(multi, `last: "Line three $30"`) {
		t.Fatalf("unexpected multi-line summary: %s", multi)
	}
}

func TestPreToolHookConcurrencyAndWatchPruneCLI(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "hq-test-bin")
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build cmd/hq failed: %v (%s)", err, string(out))
	}

	wsRoot := t.TempDir()
	vaultDir := filepath.Join(wsRoot, "local")
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory", "gmail", "mock"), 0o755); err != nil {
		t.Fatalf("mkdir mock gmail: %v", err)
	}
	mockMsgs := []map[string]interface{}{
		{
			"id":       "m-auto-1",
			"threadId": "th-bulk-1",
			"from":     "news@example.com",
			"subject":  "Weekly Digest",
			"payload": map[string]interface{}{
				"headers": []map[string]string{
					{"name": "Precedence", "value": "bulk"},
					{"name": "List-Unsubscribe", "value": "<https://example.com/unsub>"},
				},
			},
		},
		{
			"id":       "m-human-1",
			"threadId": "th-human-1",
			"from":     "alice@example.com",
			"subject":  "Review notes",
		},
		{
			"id":       "m-human-2",
			"threadId": "th-human-2",
			"from":     "bob@example.com",
			"subject":  "Launch checklist",
		},
	}
	rawMock, _ := json.Marshal(mockMsgs)
	if err := os.WriteFile(filepath.Join(vaultDir, "memory", "gmail", "mock", "messages.json"), rawMock, 0o644); err != nil {
		t.Fatalf("write mock messages: %v", err)
	}

	// 1. Run 8 concurrent `hq hook pre-tool` invocations; verify all succeed well under the 5s hook timeout
	// and do NOT regenerate CONSOLE.md on the PreToolUse hot path.
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(binPath, "hook", "pre-tool")
			cmd.Dir = wsRoot
			cmd.Env = append(os.Environ(), "HQ_VAULT_DIR="+vaultDir, "ANTIGRAVITY_CONVERSATION_ID=00000000-0000-0000-0000-000000000111")
			cmd.Stdin = strings.NewReader(`{"conversationId":"00000000-0000-0000-0000-000000000111","toolCall":{"name":"run_command","args":{"CommandLine":"echo ok"}}}`)
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			if err := cmd.Run(); err != nil || !strings.Contains(out.String(), `"decision":"allow"`) {
				errs <- os.ErrInvalid
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent hq hook pre-tool failed: %v", e)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("expected 8 concurrent hq hook pre-tool calls to finish <3s, took %s", elapsed)
	}
	if _, stErr := os.Stat(filepath.Join(vaultDir, "CONSOLE.md")); !os.IsNotExist(stErr) {
		t.Fatalf("hq hook pre-tool must not regenerate CONSOLE.md on hot path")
	}

	// 2. Seed pending backlog (2 automated + 2 duplicate human-1 + 1 human-2) and verify `hq watch prune-backlog` + multi-thread `hq watch read`.
	now := time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC)
	mkEv := func(itemID, threadID string) events.Event {
		return events.Event{
			Source:       "connector:gmail",
			DedupKey:     "watch:gmail:default:" + itemID,
			ScopeKey:     "watch:gmail:default",
			Skill:        "inbox-triage",
			Title:        "[Auto] Channel Triage",
			Prompt:       "Triage " + itemID,
			TaintOnSpawn: true,
			Metadata: map[string]string{
				"requires_ack": "1",
				"connector":    "gmail",
				"account":      "default",
				"item_id":      itemID,
				"item_key":     itemID,
				"thread_key":   threadID,
			},
		}
	}
	_, _, _ = events.EnqueueBatch(vaultDir, []events.Event{
		mkEv("m-auto-1", "th-bulk-1"),
		mkEv("m-auto-2", "th-bulk-1"),
		mkEv("m-human-1", "th-human-1"),
		mkEv("m-human-1b", "th-human-1"),
		mkEv("m-human-2", "th-human-2"),
	}, now)

	pruneCmd := exec.Command(binPath, "watch", "prune-backlog", "--connector=gmail", "--json")
	pruneCmd.Dir = wsRoot
	pruneCmd.Env = append(os.Environ(), "HQ_VAULT_DIR="+vaultDir, "ANTIGRAVITY_CONVERSATION_ID=")
	pruneOut, err := pruneCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hq watch prune-backlog failed: %v (%s)", err, string(pruneOut))
	}
	var pruneRep struct {
		AutomatedPruned     int `json:"automated_pruned"`
		DuplicatesCoalesced int `json:"duplicates_coalesced"`
		RemainingPending    int `json:"remaining_pending"`
	}
	if err := json.Unmarshal(pruneOut, &pruneRep); err != nil {
		t.Fatalf("decode prune-backlog JSON: %v (%s)", err, string(pruneOut))
	}
	if pruneRep.AutomatedPruned != 2 || pruneRep.DuplicatesCoalesced != 1 || pruneRep.RemainingPending != 2 {
		t.Fatalf("unexpected prune-backlog counts: %+v (%s)", pruneRep, string(pruneOut))
	}

	// 3. Multi-thread `hq watch read --connector=gmail --thread=th-human-1,th-human-2 --json`
	readCmd := exec.Command(binPath, "watch", "read", "--connector=gmail", "--thread=th-human-1,th-human-2", "--conv=00000000-0000-0000-0000-000000000222", "--json")
	readCmd.Dir = wsRoot
	readCmd.Env = append(os.Environ(), "HQ_VAULT_DIR="+vaultDir)
	readOut, err := readCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hq watch read multi-thread failed: %v (%s)", err, string(readOut))
	}
	var readResp struct {
		Fenced  string                   `json:"fenced"`
		Threads []map[string]interface{} `json:"threads"`
	}
	if err := json.Unmarshal(readOut, &readResp); err != nil {
		t.Fatalf("decode read JSON: %v (%s)", err, string(readOut))
	}
	if len(readResp.Threads) != 2 || strings.Count(readResp.Fenced, "<<<HQ_UNTRUSTED_NONCE_") != 2 {
		t.Fatalf("expected 2 threads and 2 nonce fences in multi-thread CLI output, got %+v", readResp)
	}
}
