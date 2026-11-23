package gmail

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zjshen14/agy-hq/internal/connector"
)

// ForwardOptions configures `hq gmail forward` message fetching, HTML composition, and staging.
type ForwardOptions struct {
	Account   string
	MessageID string
	To        string
	Cc        string
	Subject   string
	Note      string
}

// ForwardPlan holds the prepared HTML body file, downloaded attachments, and deterministic send command.
type ForwardPlan struct {
	Account         string   `json:"account"`
	From            string   `json:"from"`
	MessageID       string   `json:"messageId"`
	To              string   `json:"to"`
	Cc              string   `json:"cc,omitempty"`
	Subject         string   `json:"subject"`
	TempDir         string   `json:"tempDir"`
	BodyFile        string   `json:"bodyFile"`
	AttachmentFiles []string `json:"attachmentFiles,omitempty"`
	HTMLBody        string   `json:"-"`
	CommandArgv     []string `json:"commandArgv"`
	Command         string   `json:"command"`
	Summary         string   `json:"summary"`
}

var (
	reFwdBodyInner    = regexp.MustCompile(`(?is)<body[^>]*>(.*?)</body>`)
	reFwdDoctype      = regexp.MustCompile(`(?is)<!doctype[^>]*>`)
	reFwdHTMLTags     = regexp.MustCompile(`(?is)</?html[^>]*>`)
	reFwdHeadBlock    = regexp.MustCompile(`(?is)<head[^>]*>.*?</head>`)
	reFwdBodyTags     = regexp.MustCompile(`(?is)</?body[^>]*>`)
	reFwdImgTag       = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	reFwdAttrWidth01  = regexp.MustCompile(`(?i)\bwidth\s*=\s*["']?0*[01](?:px)?["']?`)
	reFwdAttrHeight01 = regexp.MustCompile(`(?i)\bheight\s*=\s*["']?0*[01](?:px)?["']?`)
	reFwdStyleWidth01 = regexp.MustCompile(`(?i)\bwidth\s*:\s*0*[01](?:px)?\b`)
	reFwdStyleH01     = regexp.MustCompile(`(?i)\bheight\s*:\s*0*[01](?:px)?\b`)
	reFwdDisplayNone  = regexp.MustCompile(`(?i)\bdisplay\s*:\s*none\b`)
	reFwdMultiNewline = regexp.MustCompile(`\n{2,}`)
)

// PrepareForward fetches the source message via `gmail readonly get --json`, composes the
// HTML forwarded body, downloads all attachments into a unique temp directory with deduplicated
// filenames, and builds the deterministic `gmail mutate send` / `send-with-attachment` command.
func PrepareForward(vaultDir string, opts ForwardOptions) (ForwardPlan, error) {
	messageID := strings.TrimSpace(opts.MessageID)
	if messageID == "" {
		return ForwardPlan{}, fmt.Errorf("--message is required")
	}
	to := strings.TrimSpace(opts.To)
	if to == "" {
		return ForwardPlan{}, fmt.Errorf("--to is required")
	}
	cc := strings.TrimSpace(opts.Cc)

	cfg, err := ResolveAccount(vaultDir, opts.Account)
	if err != nil {
		if rc, cErr := connector.Resolve(vaultDir, "gmail"); cErr == nil && len(rc.Instance.Accounts) > 0 {
			want := strings.ToLower(strings.TrimSpace(opts.Account))
			if want == "" {
				want = rc.Instance.DefaultAccount
			}
			for alias, acc := range rc.Instance.Accounts {
				if want == "" || strings.EqualFold(alias, want) || strings.EqualFold(acc.Email, want) || strings.EqualFold(acc.AgyAccount, want) {
					cfg = AccountConfig{
						Alias:      alias,
						Email:      acc.Email,
						AGYAccount: acc.AgyAccount,
						CLIPath:    rc.Manifest.Transport.CLI.Path,
					}
					err = nil
					break
				}
			}
		}
		if err != nil {
			if strings.TrimSpace(opts.Account) == "" && resolveGmailBinary(AccountConfig{}) != "" {
				cfg = AccountConfig{Alias: "default"}
			} else {
				return ForwardPlan{}, err
			}
		}
	}

	rawOut, err := runGmailCLI(vaultDir, cfg, "readonly", "get", "--json", "--", messageID)
	if err != nil {
		return ForwardPlan{}, fmt.Errorf("gmail readonly get %s failed: %w (%s)", messageID, err, strings.TrimSpace(string(rawOut)))
	}
	var rm connector.RawMailMessage
	if err := json.Unmarshal(rawOut, &rm); err != nil {
		return ForwardPlan{}, fmt.Errorf("parse gmail readonly get JSON for %s: %w", messageID, err)
	}
	_, det := parseRawGmailMessage(rm, cfg.Alias)

	subject := strings.TrimSpace(opts.Subject)
	if subject == "" {
		origSubj := strings.TrimSpace(det.Subject)
		switch {
		case origSubj == "":
			subject = "Fwd:"
		case strings.HasPrefix(strings.ToLower(origSubj), "fwd:"):
			subject = origSubj
		default:
			subject = "Fwd: " + origSubj
		}
	}

	forwardedHTML := BuildForwardedHTML(det, opts.Note)

	tmpDir, err := os.MkdirTemp("", "hq-gmail-forward-*")
	if err != nil {
		return ForwardPlan{}, fmt.Errorf("create temp dir for forward: %w", err)
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	bodyFilePath := filepath.Join(tmpDir, "forward-body.html")
	if err := os.WriteFile(bodyFilePath, []byte(forwardedHTML), 0o600); err != nil {
		return ForwardPlan{}, fmt.Errorf("write forward HTML body: %w", err)
	}

	usedNames := make(map[string]int)
	var attachmentPaths []string
	for _, att := range det.Attachments {
		dedupName := DeduplicateAttachmentFilename(att.Filename, usedNames)
		destPath := filepath.Join(tmpDir, dedupName)
		if out, err := runGmailCLI(vaultDir, cfg, "readonly", "download-attachment", messageID, att.AttachmentID, destPath); err != nil {
			return ForwardPlan{}, fmt.Errorf("download attachment %q (%s): %w (%s)", att.Filename, att.AttachmentID, err, strings.TrimSpace(string(out)))
		}
		attachmentPaths = append(attachmentPaths, destPath)
	}

	gmailBin := resolveGmailBinary(cfg)
	if gmailBin == "" {
		gmailBin = "gmail"
	}
	cmdArgv, cmdStr := BuildForwardSendCommand(gmailBin, cfg.AGYAccount, to, cc, subject, bodyFilePath, attachmentPaths)

	fromDisplay := cfg.Email
	if fromDisplay == "" {
		fromDisplay = cfg.Alias
	}
	summary := fmt.Sprintf("Forward Gmail %s [From: %s -> To: %s | Subject: %s | Attachments: %d]",
		messageID, fromDisplay, to, subject, len(attachmentPaths))

	cleanupTmp = false
	return ForwardPlan{
		Account:         cfg.Alias,
		From:            cfg.Email,
		MessageID:       messageID,
		To:              to,
		Cc:              cc,
		Subject:         subject,
		TempDir:         tmpDir,
		BodyFile:        bodyFilePath,
		AttachmentFiles: attachmentPaths,
		HTMLBody:        forwardedHTML,
		CommandArgv:     cmdArgv,
		Command:         cmdStr,
		Summary:         summary,
	}, nil
}

// BuildForwardedHTML composes a clean HTML email body with an optional note, a standard
// forwarded-message metadata block, and the cleaned original HTML (or escaped plain-text fallback).
func BuildForwardedHTML(det MessageDetail, note string) string {
	var bodyContent string
	if strings.TrimSpace(det.HTMLBody) != "" {
		bodyContent = cleanForwardedHTML(det.HTMLBody)
	} else {
		bodyContent = `<pre style="white-space:pre-wrap;font-family:inherit;margin:0;">` +
			html.EscapeString(det.PlainBody) + `</pre>`
	}

	noteHTML := formatNoteHTML(note)

	var b strings.Builder
	b.WriteString(`<div dir="ltr">`)
	if noteHTML != "" {
		b.WriteString("\n")
		b.WriteString(noteHTML)
	}
	b.WriteString("\n<hr/>\n")
	b.WriteString(`<div class="gmail_quote">`)
	b.WriteString("\n<div dir=\"ltr\" class=\"gmail_attr\">\n")
	b.WriteString("---------- Forwarded message ---------<br/>\n")
	if strings.TrimSpace(det.From) != "" {
		fmt.Fprintf(&b, "From: %s<br/>\n", html.EscapeString(det.From))
	}
	if strings.TrimSpace(det.Date) != "" {
		fmt.Fprintf(&b, "Date: %s<br/>\n", html.EscapeString(det.Date))
	}
	if strings.TrimSpace(det.Subject) != "" {
		fmt.Fprintf(&b, "Subject: %s<br/>\n", html.EscapeString(det.Subject))
	}
	if strings.TrimSpace(det.To) != "" {
		fmt.Fprintf(&b, "To: %s<br/>\n", html.EscapeString(det.To))
	}
	b.WriteString("</div>\n<br/>\n")
	b.WriteString(bodyContent)
	b.WriteString("\n</div>\n</div>\n")
	return b.String()
}

func formatNoteHTML(note string) string {
	normalized := strings.ReplaceAll(note, "\r\n", "\n")
	normalized = strings.TrimSpace(normalized)
	if normalized == "" {
		return ""
	}
	paras := reFwdMultiNewline.Split(normalized, -1)
	var out []string
	for _, p := range paras {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		escaped := html.EscapeString(p)
		escaped = strings.ReplaceAll(escaped, "\n", "<br/>\n")
		out = append(out, "<p>"+escaped+"</p>")
	}
	return strings.Join(out, "\n")
}

func cleanForwardedHTML(raw string) string {
	cleaned := raw
	if m := reFwdBodyInner.FindStringSubmatch(cleaned); len(m) == 2 {
		cleaned = m[1]
	} else {
		cleaned = reFwdDoctype.ReplaceAllString(cleaned, "")
		cleaned = reFwdHTMLTags.ReplaceAllString(cleaned, "")
		cleaned = reFwdHeadBlock.ReplaceAllString(cleaned, "")
		cleaned = reFwdBodyTags.ReplaceAllString(cleaned, "")
	}
	cleaned = stripTrackingPixels(cleaned)
	return strings.TrimSpace(cleaned)
}

func stripTrackingPixels(raw string) string {
	return reFwdImgTag.ReplaceAllStringFunc(raw, func(tag string) string {
		hasZeroOneWidth := reFwdAttrWidth01.MatchString(tag) || reFwdStyleWidth01.MatchString(tag)
		hasZeroOneHeight := reFwdAttrHeight01.MatchString(tag) || reFwdStyleH01.MatchString(tag)
		if (hasZeroOneWidth && hasZeroOneHeight) || reFwdDisplayNone.MatchString(tag) {
			return ""
		}
		return tag
	})
}

// DeduplicateAttachmentFilename sanitizes a filename against path traversal and appends
// `_part2`, `_part3`, ... before the extension when duplicate names appear in the same message.
func DeduplicateAttachmentFilename(filename string, used map[string]int) string {
	clean := strings.TrimSpace(strings.ReplaceAll(filename, "\\", "/"))
	base := filepath.Base(clean)
	if base == "" || base == "." || base == ".." || base == "/" {
		base = "attachment"
	}

	key := strings.ToLower(base)
	if used[key] == 0 {
		used[key] = 1
		return base
	}

	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" {
		stem = base
		ext = ""
	}

	n := used[key] + 1
	for {
		cand := fmt.Sprintf("%s_part%d%s", stem, n, ext)
		candKey := strings.ToLower(cand)
		if used[candKey] == 0 {
			used[key] = n
			used[candKey] = 1
			return cand
		}
		n++
	}
}

// BuildForwardSendCommand constructs the deterministic argv and shell-quoted display command
// using `gmail mutate send-with-attachment` when attachments exist, or `gmail mutate send` otherwise.
func BuildForwardSendCommand(gmailBin, agyAccount, to, cc, subject, bodyFilePath string, attachmentPaths []string) ([]string, string) {
	var gmailArgs []string
	if len(attachmentPaths) > 0 {
		gmailArgs = []string{"mutate", "send-with-attachment"}
	} else {
		gmailArgs = []string{"mutate", "send"}
	}
	gmailArgs = append(gmailArgs, "--to", to)
	if cc != "" {
		gmailArgs = append(gmailArgs, "--cc", cc)
	}
	gmailArgs = append(gmailArgs,
		"--subject", subject,
		"--html",
		"--body-file", bodyFilePath,
	)
	for _, p := range attachmentPaths {
		gmailArgs = append(gmailArgs, "--file", p)
	}

	var cmdArgv []string
	if strings.TrimSpace(agyAccount) != "" {
		cmdArgv = append([]string{"/usr/bin/env", "AGY_ACCOUNT=" + agyAccount, gmailBin}, gmailArgs...)
	} else {
		cmdArgv = append([]string{gmailBin}, gmailArgs...)
	}
	return cmdArgv, formatShellArgv(cmdArgv)
}

func formatShellArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`!*?()[]{}<>|;&") {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			quoted[i] = a
		}
	}
	return strings.Join(quoted, " ")
}
