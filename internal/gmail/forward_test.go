package gmail

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildForwardedHTML_WithHTMLBodyAndTrackingPixel(t *testing.T) {
	det := MessageDetail{
		From:    "Alice <alice@example.com>",
		Date:    "Tue, 17 Nov 2026 10:00:00 -0800",
		Subject: "Q4 Report & Forecast",
		To:      "bob@example.com",
		HTMLBody: `<!DOCTYPE html><html><head><title>Ignored</title><style>.x{}</style></head>` +
			`<body><p>Hello <strong>Team</strong>,</p>` +
			`<img src="https://example.com/logo.png" width="200" height="50">` +
			`<img src="https://track.example/open.gif" width="1" height="1">` +
			`<img src="https://track.example/px.png" style="width:1px;height:1px">` +
			`</body></html>`,
	}

	got := BuildForwardedHTML(det, "FYI — please review.\n\nThanks & <regards>!")

	for _, want := range []string{
		"<p>FYI — please review.</p>",
		"<p>Thanks &amp; &lt;regards&gt;!</p>",
		"<hr/>",
		"---------- Forwarded message ---------",
		"From: Alice &lt;alice@example.com&gt;",
		"Date: Tue, 17 Nov 2026 10:00:00 -0800",
		"Subject: Q4 Report &amp; Forecast",
		"To: bob@example.com",
		"<p>Hello <strong>Team</strong>,</p>",
		`<img src="https://example.com/logo.png" width="200" height="50">`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected output to contain %q\ngot:\n%s", want, got)
		}
	}

	for _, unwanted := range []string{
		"<!DOCTYPE",
		"<head>",
		"<body",
		"track.example/open.gif",
		"track.example/px.png",
	} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("expected output NOT to contain %q\ngot:\n%s", unwanted, got)
		}
	}
}

func TestBuildForwardedHTML_PlainTextFallback(t *testing.T) {
	det := MessageDetail{
		From:      "sender@example.com",
		Date:      "Mon, 16 Nov 2026 09:00:00 -0800",
		Subject:   "Plain Only",
		To:        "me@example.com",
		PlainBody: "Line 1\n<script>alert(1)</script>\nLine 3",
	}

	got := BuildForwardedHTML(det, "")
	if !strings.Contains(got, "<pre ") {
		t.Fatalf("expected <pre> fallback, got:\n%s", got)
	}
	if !strings.Contains(got, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("expected escaped plain text, got:\n%s", got)
	}
	if strings.Contains(got, "<p></p>") {
		t.Fatalf("did not expect empty <p></p> when note is empty")
	}
}

func TestDeduplicateAttachmentFilename(t *testing.T) {
	used := make(map[string]int)

	cases := []struct {
		in   string
		want string
	}{
		{"invoice.pdf", "invoice.pdf"},
		{"invoice.pdf", "invoice_part2.pdf"},
		{"invoice.pdf", "invoice_part3.pdf"},
		{"README", "README"},
		{"README", "README_part2"},
		{"../../etc/passwd", "passwd"},
		{"../passwd", "passwd_part2"},
		{"doc_part2.pdf", "doc_part2.pdf"},
		{"doc.pdf", "doc.pdf"},
		{"doc.pdf", "doc_part3.pdf"},
		{"", "attachment"},
		{"..", "attachment_part2"},
	}

	for _, tc := range cases {
		got := DeduplicateAttachmentFilename(tc.in, used)
		if got != tc.want {
			t.Errorf("DeduplicateAttachmentFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildForwardSendCommand(t *testing.T) {
	argvNoAtt, strNoAtt := BuildForwardSendCommand("/usr/bin/gmail", "", "alice@example.com", "", "Fwd: Hi", "/tmp/body.html", nil)
	if len(argvNoAtt) < 3 || argvNoAtt[1] != "mutate" || argvNoAtt[2] != "send" {
		t.Fatalf("expected `mutate send` for 0 attachments, got %v", argvNoAtt)
	}
	if strings.Contains(strNoAtt, "--file") || strings.Contains(strNoAtt, "--cc") {
		t.Fatalf("unexpected --file or --cc in 0-attachment command: %s", strNoAtt)
	}

	argvWithAtt, strWithAtt := BuildForwardSendCommand(
		"/usr/bin/gmail",
		"work@acme.com",
		"alice@example.com,bob@example.com",
		"cc@example.com",
		"Fwd: Report",
		"/tmp/dir/body.html",
		[]string{"/tmp/dir/r.pdf", "/tmp/dir/r_part2.pdf"},
	)
	if argvWithAtt[0] != "/usr/bin/env" || argvWithAtt[1] != "AGY_ACCOUNT=work@acme.com" {
		t.Fatalf("expected env AGY_ACCOUNT wrapper, got %v", argvWithAtt)
	}
	if argvWithAtt[3] != "mutate" || argvWithAtt[4] != "send-with-attachment" {
		t.Fatalf("expected `mutate send-with-attachment`, got %v", argvWithAtt)
	}
	if !strings.Contains(strWithAtt, "--cc cc@example.com") {
		t.Fatalf("expected --cc in command: %s", strWithAtt)
	}
	if !strings.Contains(strWithAtt, "--file /tmp/dir/r.pdf --file /tmp/dir/r_part2.pdf") {
		t.Fatalf("expected both --file flags in command: %s", strWithAtt)
	}
}

func TestPrepareForward_MockCLI(t *testing.T) {
	vaultDir := t.TempDir()
	binDir := t.TempDir()
	mockGmail := filepath.Join(binDir, "gmail")

	htmlPayload := base64.RawURLEncoding.EncodeToString([]byte(`<html><body><p>Original HTML</p></body></html>`))
	rawMsg := rawGmailMessage{
		ID:       "msg-123",
		ThreadID: "thr-123",
	}
	rawMsg.Payload.MimeType = "multipart/mixed"
	rawMsg.Payload.Headers = []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}{
		{Name: "From", Value: "alice@example.com"},
		{Name: "To", Value: "me@example.com"},
		{Name: "Subject", Value: "Monthly Invoice"},
		{Name: "Date", Value: "Tue, 17 Nov 2026 08:00:00 -0800"},
	}
	var htmlPart, att1, att2 rawGmailPart
	htmlPart.MimeType = "text/html"
	htmlPart.Body.Data = htmlPayload
	att1.MimeType = "application/pdf"
	att1.Filename = "receipt.pdf"
	att1.Body.AttachmentID = "att-1"
	att2.MimeType = "application/pdf"
	att2.Filename = "receipt.pdf"
	att2.Body.AttachmentID = "att-2"
	rawMsg.Payload.Parts = []rawGmailPart{htmlPart, att1, att2}

	rawJSON, err := json.Marshal(rawMsg)
	if err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(binDir, "msg.json")
	if err := os.WriteFile(jsonPath, rawJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	script := `#!/bin/sh
if [ "$1" = "readonly" ] && [ "$2" = "get" ]; then
  cat "` + jsonPath + `"
  exit 0
fi
if [ "$1" = "readonly" ] && [ "$2" = "download-attachment" ]; then
  att_id="$4"
  out_file="$5"
  printf "content-for-%s" "$att_id" > "$out_file"
  exit 0
fi
echo "unexpected args: $@" >&2
exit 1
`
	if err := os.WriteFile(mockGmail, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := InitOrAddAccountWithOptions(vaultDir, InitAccountOptions{
		Alias:      "primary",
		Email:      "me@example.com",
		CLIPath:    mockGmail,
		SetDefault: true,
	}); err != nil {
		t.Fatalf("InitOrAddAccountWithOptions: %v", err)
	}

	plan, err := PrepareForward(vaultDir, ForwardOptions{
		MessageID: "msg-123",
		To:        "finance@example.com",
		Note:      "Please process.",
	})
	if err != nil {
		t.Fatalf("PrepareForward failed: %v", err)
	}
	defer os.RemoveAll(plan.TempDir)

	if plan.Subject != "Fwd: Monthly Invoice" {
		t.Fatalf("expected Subject 'Fwd: Monthly Invoice', got %q", plan.Subject)
	}
	if len(plan.AttachmentFiles) != 2 {
		t.Fatalf("expected 2 attachment files, got %d: %v", len(plan.AttachmentFiles), plan.AttachmentFiles)
	}
	if filepath.Base(plan.AttachmentFiles[0]) != "receipt.pdf" || filepath.Base(plan.AttachmentFiles[1]) != "receipt_part2.pdf" {
		t.Fatalf("unexpected deduplicated filenames: %v", plan.AttachmentFiles)
	}
	b1, _ := os.ReadFile(plan.AttachmentFiles[0])
	b2, _ := os.ReadFile(plan.AttachmentFiles[1])
	if string(b1) != "content-for-att-1" || string(b2) != "content-for-att-2" {
		t.Fatalf("unexpected attachment contents: %q, %q", string(b1), string(b2))
	}
	bodyBytes, err := os.ReadFile(plan.BodyFile)
	if err != nil {
		t.Fatalf("read body file: %v", err)
	}
	if !strings.Contains(string(bodyBytes), "<p>Original HTML</p>") || !strings.Contains(string(bodyBytes), "<p>Please process.</p>") {
		t.Fatalf("unexpected body HTML:\n%s", string(bodyBytes))
	}
	if len(plan.CommandArgv) < 3 || plan.CommandArgv[2] != "send-with-attachment" {
		t.Fatalf("expected send-with-attachment command, got %v", plan.CommandArgv)
	}
}
