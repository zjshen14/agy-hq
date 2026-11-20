package hooks

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/gchat"
	"github.com/zjshen14/agy-hq/internal/gmail"
)

var (
	reHQPropose    = regexp.MustCompile(`(?i)(?:^|&&|;)\s*(?:\S+/)?hq\s+propose\b`)
	reCmdFlagValue = regexp.MustCompile(`--cmd=(?:"(?:\\.|[^"\\])*"|'[^']*'|[^\s;&|$` + "`" + `<>()#'"]+)`)

	reChannelAPIRoute = regexp.MustCompile(`(?i)/api/channels(?:/|%2f|%5c|$|[\s"'])`)

	reDoubleQuotedCmdSubst = regexp.MustCompile(`--cmd="(?:\\.|[^"\\])*(?:\$\(|` + "`" + `)`)
	reHQOutboundBodyCmd    = regexp.MustCompile(`(?i)(?:^|[\s;&|])(?:go\s+run\s+\./cmd/hq|(?:\S+/)?hq)\s+(?:gchat\s+(?:propose|stage-send|draft)|propose|gmail\s+(?:draft|propose))\b`)
	reDoubleQuotedBodyFlag = regexp.MustCompile(`(?:^|\s)--(?:body|message)(?:=|\s+)"((?:\\.|[^"\\])*)"`)
	reSingleShellVarOnly   = regexp.MustCompile(`^\$(?:[A-Za-z_][A-Za-z0-9_]*|\{[A-Za-z_][A-Za-z0-9_]*\})$`)
)

// isSingleHQCommand returns true when cmdLine is a single non-chained invocation of `hq`
// (via PATH, ./bin/hq, bin/hq, or an absolute path) or scripts/bootstrap.sh.
func isSingleHQCommand(cmdLine string) bool {
	trimmed := strings.TrimSpace(cmdLine)
	if trimmed == "" || strings.ContainsAny(trimmed, "\n\r;|&`") {
		return false
	}
	fields := approvals.StripLeadingEnv(strings.Fields(trimmed), false)
	if len(fields) == 0 {
		return false
	}
	if isHQBinaryToken(fields[0]) {
		if len(fields) >= 2 && fields[1] == "exec-sandboxed" {
			return false
		}
		return true
	}
	first := filepath.ToSlash(fields[0])
	return first == "./scripts/bootstrap.sh" || first == "scripts/bootstrap.sh"
}

func splitShellSegmentsUnquoted(cmd string) []string {
	var out []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				inSingle = false
			}
			cur.WriteByte(ch)
			continue
		}
		if inDouble {
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inDouble = false
			}
			cur.WriteByte(ch)
			continue
		}
		switch ch {
		case '\\':
			escaped = true
			cur.WriteByte(ch)
		case '\'':
			inSingle = true
			cur.WriteByte(ch)
		case '"':
			inDouble = true
			cur.WriteByte(ch)
		case ';', '|', '&', '\n', '\r', '(', ')', '`':
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// isBlockedChannelGatewayShell structurally detects quote/env/`go run`-wrapped invocations of
// `hq channel inbound|callback|flush|apply-config` or `/api/channels/*` across all LLM sessions.
func isBlockedChannelGatewayShell(cmdLine string) bool {
	if reChannelAPIRoute.MatchString(cmdLine) {
		return true
	}
	for _, seg := range splitShellSegmentsUnquoted(cmdLine) {
		toks, _ := approvals.TokenizeShellWords(seg, false)
		// Strip leading env assignments (`FOO=bar`), `env [-u X / --unset=X / -i / --]`, `command`, `exec`, `nohup`, `nice`, `timeout <dur>`
		i := 0
		for i < len(toks) {
			t := toks[i]
			if strings.Contains(t, "=") && !strings.HasPrefix(t, "-") {
				i++
				continue
			}
			switch t {
			case "env":
				i++
				for i < len(toks) {
					et := toks[i]
					if et == "--" {
						i++
						break
					}
					if et == "-u" || et == "--unset" || et == "-C" || et == "--chdir" || et == "-S" {
						i += 2
						continue
					}
					if strings.HasPrefix(et, "-") || strings.Contains(et, "=") {
						i++
						continue
					}
					break
				}
				continue
			case "command", "exec", "nohup", "builtin":
				i++
				for i < len(toks) && strings.HasPrefix(toks[i], "-") {
					i++
				}
				continue
			case "nice":
				i++
				if i < len(toks) && (toks[i] == "-n" || strings.HasPrefix(toks[i], "--adjustment")) {
					if toks[i] == "-n" {
						i += 2
					} else {
						i++
					}
				}
				continue
			case "timeout":
				i++
				for i < len(toks) && strings.HasPrefix(toks[i], "-") {
					if toks[i] == "-k" || toks[i] == "-s" {
						i += 2
					} else {
						i++
					}
				}
				if i < len(toks) {
					i++ // duration arg
				}
				continue
			}
			break
		}
		if i >= len(toks) {
			continue
		}
		rest := toks[i:]
		// Normalize `go run [flags] ./cmd/hq[/] <args...>` -> `hq <args...>`
		base0 := strings.ToLower(strings.TrimSuffix(filepath.Base(rest[0]), ".exe"))
		if base0 == "go" && len(rest) >= 3 && rest[1] == "run" {
			j := 2
			for j < len(rest) && strings.HasPrefix(rest[j], "-") {
				j++
			}
			if j < len(rest) {
				pkgClean := strings.TrimSuffix(filepath.ToSlash(strings.TrimSpace(rest[j])), "/")
				if strings.HasSuffix(pkgClean, "cmd/hq") {
					rest = append([]string{"hq"}, rest[j+1:]...)
					base0 = "hq"
				}
			}
		}
		if base0 != "hq" {
			continue
		}
		// Find first positional subcommands after `hq`
		var pos []string
		for _, a := range rest[1:] {
			if a == "--" {
				break
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			pos = append(pos, strings.ToLower(strings.TrimSpace(a)))
			if len(pos) >= 2 {
				break
			}
		}
		if len(pos) >= 2 && pos[0] == "channel" {
			switch pos[1] {
			case "inbound", "callback", "flush", "apply-config":
				return true
			}
		}
	}
	return false
}

func containsUnescapedDollar(s string) bool {
	escaped := false
	for i := 0; i < len(s); i++ {
		if escaped {
			escaped = false
			continue
		}
		if s[i] == '\\' {
			escaped = true
			continue
		}
		if s[i] == '$' {
			return true
		}
	}
	return false
}

// hasUnsafeDoubleQuotedDollar returns true if a double-quoted --body or --message
// argument on an hq outbound/draft command contains an unescaped '$' that bash will
// expand before hq receives it (e.g. "$382.34" -> "82.34"). An exact single-variable
// reference like "$MSG" or "${MSG}" is permitted.
func hasUnsafeDoubleQuotedDollar(cmdLine string) bool {
	if !reHQOutboundBodyCmd.MatchString(cmdLine) {
		return false
	}
	for _, m := range reDoubleQuotedBodyFlag.FindAllStringSubmatch(cmdLine, -1) {
		if len(m) < 2 {
			continue
		}
		inner := m[1]
		if reSingleShellVarOnly.MatchString(inner) {
			continue
		}
		if containsUnescapedDollar(inner) {
			return true
		}
	}
	return false
}

func extractProposeCmdForValidation(cmdLine string) (hasCmd bool, cmdVal string, argv []string) {
	if strings.Contains(cmdLine, "--draft") || strings.Contains(cmdLine, "--rsi-id") {
		return false, "", nil
	}
	if m := reCmdFlagValue.FindString(cmdLine); m != "" {
		raw := strings.TrimPrefix(m, "--cmd=")
		if len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'' {
			raw = raw[1 : len(raw)-1]
		} else if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
			raw = strings.ReplaceAll(strings.ReplaceAll(raw[1:len(raw)-1], `\"`, `"`), `\\`, `\`)
		}
		return true, raw, nil
	}
	if idx := strings.Index(cmdLine, " -- "); idx >= 0 {
		rest := strings.TrimSpace(cmdLine[idx+4:])
		if rest != "" {
			return true, "", strings.Fields(rest)
		}
	}
	return false, "", nil
}

// hasUnquotedShellChainingOrSubst reports whether s contains any shell chaining/piping operator outside quotes
// (;, &, |, newline), process substitution (<(...), >(...)), command substitution ($(...) or backtick) outside
// single quotes, bash ANSI-C/locale quoting ($'...', $"..."), unquoted '#' comments (which can mask quote
// desynchronization), or an unclosed quote / trailing escape (fail-closed).
func hasUnquotedShellChainingOrSubst(s string) bool {
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if escaped {
			if ch == '\n' || ch == '\r' {
				return true // bash strips \<LF> before tokenization (can splice $( or <()
			}
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inDouble = false
				continue
			}
			if ch == '`' || (ch == '$' && i+1 < len(s) && s[i+1] == '(') {
				return true
			}
			continue
		}
		switch ch {
		case '\\':
			escaped = true
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case ';', '&', '|', '\n', '\r', '`', '#':
			return true
		case '<', '>':
			if i+1 < len(s) && s[i+1] == '(' {
				return true
			}
		case '$':
			if i+1 < len(s) && (s[i+1] == '(' || s[i+1] == '\'' || s[i+1] == '"') {
				return true
			}
		}
	}
	return inSingle || inDouble || escaped
}

// stripSafeProposeCmdFlag replaces `--cmd=<val>` with `--cmd=""` only when `<val>` is cleanly bounded
// (single-quoted, double-quoted without command substitution or \<LF> continuation, or unquoted without shell
// metacharacters) and immediately followed by whitespace or end-of-string. Anything suspicious (including
// bash $'...' / $"..." quoting, \<LF> line continuations, unclosed quotes, `--cmd=hq;evil`, or `--cmd=$(evil)`)
// causes the entire command line to be returned unstripped so EvaluateCommand inspects and blocks it fail-closed.
func stripSafeProposeCmdFlag(cmdLine string) string {
	var out strings.Builder
	out.Grow(len(cmdLine))
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(cmdLine); {
		ch := cmdLine[i]
		if escaped {
			if ch == '\n' || ch == '\r' {
				return cmdLine
			}
			out.WriteByte(ch)
			escaped = false
			i++
			continue
		}
		if inSingle {
			out.WriteByte(ch)
			if ch == '\'' {
				inSingle = false
			}
			i++
			continue
		}
		if inDouble {
			out.WriteByte(ch)
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inDouble = false
			}
			i++
			continue
		}
		if ch == '$' && i+1 < len(cmdLine) && (cmdLine[i+1] == '\'' || cmdLine[i+1] == '"') {
			return cmdLine // fail closed on bash ANSI-C $'...' or locale $"..." quoting
		}
		if strings.HasPrefix(cmdLine[i:], "--cmd=") && (i == 0 || cmdLine[i-1] == ' ' || cmdLine[i-1] == '\t') {
			valStart := i + len("--cmd=")
			if valStart < len(cmdLine) {
				switch cmdLine[valStart] {
				case '\'':
					end := strings.IndexByte(cmdLine[valStart+1:], '\'')
					if end >= 0 {
						after := valStart + 1 + end + 1
						if after == len(cmdLine) || cmdLine[after] == ' ' || cmdLine[after] == '\t' {
							out.WriteString(`--cmd=""`)
							i = after
							continue
						}
					}
				case '"':
					j := valStart + 1
					safe := true
					closed := false
					for j < len(cmdLine) {
						c := cmdLine[j]
						if c == '\\' && j+1 < len(cmdLine) {
							if cmdLine[j+1] == '\n' || cmdLine[j+1] == '\r' {
								safe = false
								break
							}
							j += 2
							continue
						}
						if c == '`' || (c == '$' && j+1 < len(cmdLine) && cmdLine[j+1] == '(') {
							safe = false
							break
						}
						if c == '"' {
							closed = true
							j++
							break
						}
						j++
					}
					if safe && closed && (j == len(cmdLine) || cmdLine[j] == ' ' || cmdLine[j] == '\t') {
						out.WriteString(`--cmd=""`)
						i = j
						continue
					}
				default:
					j := valStart
					safe := true
					for j < len(cmdLine) && cmdLine[j] != ' ' && cmdLine[j] != '\t' {
						c := cmdLine[j]
						if strings.ContainsRune(";&|$`<>()#'\"\\\n\r", rune(c)) {
							safe = false
							break
						}
						j++
					}
					if safe && j > valStart && (j == len(cmdLine) || cmdLine[j] == ' ' || cmdLine[j] == '\t') {
						out.WriteString(`--cmd=""`)
						i = j
						continue
					}
				}
			}
		}
		switch ch {
		case '\\':
			escaped = true
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		}
		out.WriteByte(ch)
		i++
	}
	if inSingle || inDouble || escaped {
		return cmdLine
	}
	return out.String()
}

func stripProposeInnerCommand(cmdLine string) string {
	stripped := stripSafeProposeCmdFlag(cmdLine)
	if idx := strings.Index(stripped, " -- "); idx >= 0 {
		prefix := stripped[:idx]
		tail := stripped[idx+4:]
		if !hasUnquotedShellChainingOrSubst(prefix) && !hasUnquotedShellChainingOrSubst(tail) {
			stripped = prefix
		}
	}
	return stripped
}

func isStandardOrSpoofedApprovalOption(opt string) bool {
	lower := strings.ToLower(strings.TrimSpace(opt))
	if lower == "" || lower == "keep pending" {
		return true
	}
	for _, pref := range []string{
		"approve", "(recommended) approve", "✅ approve",
		"reject", "❌ reject",
	} {
		if lower == pref || strings.HasPrefix(lower, pref+" ") || strings.HasPrefix(lower, pref+"(") || strings.HasPrefix(lower, pref+":") {
			return true
		}
	}
	return false
}

func extractQuestionStringOptions(qMap map[string]interface{}) []string {
	var opts []string
	if rawOpts, hasOpts := qMap["options"].([]interface{}); hasOpts {
		for _, o := range rawOpts {
			if s, ok := o.(string); ok {
				opts = append(opts, s)
			}
		}
	} else if strOpts, hasStrOpts := qMap["options"].([]string); hasStrOpts {
		opts = append(opts, strOpts...)
	}
	return opts
}

func findMatchingPendingProposal(qText string, opts []string, pending []approvals.Proposal) *approvals.Proposal {
	for idx := range pending {
		pid := pending[idx].ID
		if strings.Contains(qText, pid) {
			return &pending[idx]
		}
		for _, o := range opts {
			if strings.Contains(o, pid) {
				return &pending[idx]
			}
		}
	}
	return nil
}

func buildCanonicalProposalQuestion(vaultDir string, prop approvals.Proposal, convID, transcriptPath string, origOpts []string) map[string]interface{} {
	hasPIIWarning := len(prop.RiskFlags) > 0
	meta := prop.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	hashShort := meta["contentHash"]
	if hashShort == "" {
		hashShort = meta["diffSha256"]
	}
	bodyPreview := meta["bodyPreview"]
	fromVal := meta["from"]
	toVal := meta["to"]
	subjVal := meta["subject"]
	spaceName := meta["spaceName"]
	spaceID := meta["spaceId"]
	if prop.ActionType == "gchat_send" && meta["draftId"] != "" {
		if spec, dErr := gchat.GetDraft(vaultDir, meta["account"], meta["draftId"]); dErr == nil {
			bodyPreview = spec.Body
			hashShort = spec.ContentHash
			if spec.From != "" {
				fromVal = spec.From
			}
			if spec.SpaceName != "" {
				spaceName = spec.SpaceName
			}
			if spec.SpaceID != "" {
				spaceID = spec.SpaceID
			}
		}
	} else if prop.ActionType == "gmail_send" && meta["draftId"] != "" {
		if spec, dErr := gmail.GetDraft(vaultDir, meta["account"], meta["draftId"]); dErr == nil {
			bodyPreview = spec.Body
			hashShort = spec.ContentHash
			if spec.From != "" {
				fromVal = spec.From
			}
			if spec.To != "" {
				toVal = spec.To
			}
			if spec.Subject != "" {
				subjVal = spec.Subject
			}
		}
	}
	if len(hashShort) > 12 {
		hashShort = hashShort[:12]
	}
	shaShort := prop.SHA256
	if len(shaShort) > 12 {
		shaShort = shaShort[:12]
	}
	var canonicalQ string
	switch prop.ActionType {
	case "gchat_send":
		threadLabel := meta["threadId"]
		if threadLabel == "" {
			threadLabel = "(new message)"
		}
		canonicalQ = fmt.Sprintf(
			"Approve sending Google Chat Message %s?\n• Account: %s (%s)\n• Space: %s [%s]\n• Thread: %s\n• Message: %s\n• Draft Hash: %s",
			meta["draftId"], meta["account"], fromVal, spaceName, spaceID, threadLabel, bodyPreview, hashShort,
		)
	case "rsi_patch", "rsi_skill_patch", "rsi_overlay":
		canonicalQ = fmt.Sprintf(
			"Approve RSI Self-Improvement Patch %s (%s)?\n• Hypothesis: %s (%s)\n• Fault Layer: %s\n• Target Files: %s\n• Verification Scorecard: %s\n• Diff SHA-256: %s\n• Diff Preview:\n%s",
			prop.ID, meta["ringLabel"], meta["rsiId"], meta["title"], meta["faultLayer"], meta["targetFiles"], meta["gateSummary"], hashShort, meta["canonicalDiffPreview"],
		)
	case "gmail_send":
		canonicalQ = fmt.Sprintf(
			"Approve sending Gmail Draft %s?\n• From: %s\n• To: %s\n• Subject: %s\n• Body: %s\n• Draft Hash: %s",
			meta["draftId"], fromVal, toVal, subjVal, bodyPreview, hashShort,
		)
	default:
		canonicalQ = fmt.Sprintf(
			"Approve outbound action %s (%s)?\n• Summary: %s\n• Command: %s",
			prop.ID, prop.ActionType, prop.Summary, prop.Command,
		)
	}
	if hasPIIWarning {
		canonicalQ = fmt.Sprintf("⚠️ [PII WARNING: %s]\n%s", strings.Join(prop.RiskFlags, ", "), canonicalQ)
	}
	approveOpt := fmt.Sprintf("(Recommended) Approve (%s · sha256:%s)", prop.ID, shaShort)
	if askRec, aErr := approvals.RecordAsk(vaultDir, prop.ID, convID, transcriptPath, time.Now()); aErr == nil && askRec.NonceCode != "" {
		approveOpt = fmt.Sprintf("(Recommended) Approve (%s · sha256:%s · code:%s)", prop.ID, shaShort, askRec.NonceCode)
	}

	opts := []string{
		approveOpt,
		fmt.Sprintf("Reject (%s)", prop.ID),
	}
	var customOpts []string
	seenCustom := make(map[string]bool)
	for _, rawOpt := range origOpts {
		clean := strings.TrimSpace(rawOpt)
		if isStandardOrSpoofedApprovalOption(clean) || strings.Contains(strings.ToLower(clean), "code:") || strings.Contains(clean, prop.ID) {
			continue
		}
		key := strings.ToLower(clean)
		if !seenCustom[key] {
			seenCustom[key] = true
			customOpts = append(customOpts, clean)
		}
	}
	if len(customOpts) > 0 {
		opts = append(opts, customOpts...)
	} else {
		opts = append(opts, "Keep pending")
	}

	return map[string]interface{}{
		"question":        canonicalQ,
		"is_multi_select": false,
		"options":         opts,
	}
}

func askQuestionsReferenceProposal(args map[string]interface{}, pending []approvals.Proposal) string {
	rawList, ok := args["questions"].([]interface{})
	if !ok || len(rawList) == 0 || len(pending) == 0 {
		return ""
	}
	for _, item := range rawList {
		qMap, isMap := item.(map[string]interface{})
		if !isMap {
			continue
		}
		qText, _ := qMap["question"].(string)
		if matched := findMatchingPendingProposal(qText, extractQuestionStringOptions(qMap), pending); matched != nil {
			return matched.ID
		}
	}
	return ""
}

func rewriteAskQuestionsSurgical(vaultDir string, args map[string]interface{}, pending []approvals.Proposal, convID, transcriptPath string) (map[string]interface{}, bool) {
	rawList, ok := args["questions"].([]interface{})
	if !ok || len(rawList) == 0 {
		return nil, false
	}
	updatedList := make([]interface{}, len(rawList))
	modifiedAny := false

	for i, item := range rawList {
		qMap, isMap := item.(map[string]interface{})
		if !isMap {
			updatedList[i] = item
			continue
		}
		qText, _ := qMap["question"].(string)
		origOpts := extractQuestionStringOptions(qMap)
		if matchedProp := findMatchingPendingProposal(qText, origOpts, pending); matchedProp != nil {
			updatedList[i] = buildCanonicalProposalQuestion(vaultDir, *matchedProp, convID, transcriptPath, origOpts)
			modifiedAny = true
		} else {
			updatedList[i] = qMap
		}
	}

	if !modifiedAny {
		return nil, false
	}
	overwriteArgs := make(map[string]interface{}, len(args))
	for k, v := range args {
		overwriteArgs[k] = v
	}
	overwriteArgs["questions"] = updatedList
	return overwriteArgs, true
}
