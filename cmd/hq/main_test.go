package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
