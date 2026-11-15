package hooks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/internal/taint"
)

var (
	reSpawnTokenTag = regexp.MustCompile(`\[hq-spawn:([0-9a-f]{32,64})\]`)
	reHQApproveAny  = regexp.MustCompile(`(?i)(?:^|[\s;&|("'` + "`" + `])(?:\S+/)?hq\s+approve\b`)
)

func spawnCheckMarkerPath(vaultDir, convID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(convID)))
	return filepath.Join(vaultDir, "private", "taint", ".checked-"+hex.EncodeToString(sum[:16]))
}

// bindSpawnTokenFromTranscript inspects lastUserInput and the first 64 KiB of transcriptPath for `[hq-spawn:<token>]`,
// atomically CAS-binds the leased event batch to convID, and marks convID tainted BEFORE any tool executes
// if any bound event carries TaintOnSpawn=true. Once scanned, a per-conversation marker skips redundant I/O on tool calls 2+.
func bindSpawnTokenFromTranscript(vaultDir, convID, lastUserInput, transcriptPath string) {
	convID = strings.TrimSpace(convID)
	lastUserInput = strings.TrimSpace(lastUserInput)
	transcriptPath = strings.TrimSpace(transcriptPath)
	if convID == "" || strings.TrimSpace(vaultDir) == "" {
		return
	}

	checkMarker := spawnCheckMarkerPath(vaultDir, convID)
	if st, err := os.Lstat(checkMarker); err == nil && st.Mode().IsRegular() {
		return
	}

	seenTokens := make(map[string]bool)
	var candidateTokens []string
	addMatches := func(raw []byte) {
		for _, m := range reSpawnTokenTag.FindAllSubmatch(raw, 4) {
			tok := string(bytes.TrimSpace(m[1]))
			if tok != "" && !seenTokens[tok] {
				seenTokens[tok] = true
				candidateTokens = append(candidateTokens, tok)
			}
		}
	}

	if lastUserInput != "" {
		addMatches([]byte(lastUserInput))
	}

	scannedTranscript := false
	if transcriptPath != "" {
		if st, err := os.Lstat(transcriptPath); err == nil && st.Mode()&os.ModeSymlink == 0 && st.Mode().IsRegular() {
			if f, oErr := os.Open(transcriptPath); oErr == nil {
				head, rErr := io.ReadAll(io.LimitReader(f, 64*1024))
				_ = f.Close()
				if rErr == nil && len(head) > 0 {
					scannedTranscript = true
					addMatches(head)
				}
			}
		}
	}

	boundAny := false
	for _, rawTok := range candidateTokens {
		bound, ok, bErr := events.BindBatchByToken(vaultDir, rawTok, convID, time.Now(), events.DefaultLeaseTTL)
		if bErr != nil || !ok || len(bound) == 0 {
			continue
		}
		boundAny = true
		needsTaint := false
		var evIDs []string
		var itemKeys []string
		connector := ""
		account := ""
		batchID := ""
		source := "spawn_batch"
		for _, ev := range bound {
			evIDs = append(evIDs, ev.ID)
			if ev.LeaseID != "" && batchID == "" {
				batchID = ev.LeaseID
			}
			if ev.Source != "" && source == "spawn_batch" {
				source = "spawn_batch:" + ev.Source
			}
			if ev.TaintOnSpawn {
				needsTaint = true
			}
			if ev.Metadata != nil {
				if ik := strings.TrimSpace(ev.Metadata["item_key"]); ik != "" {
					itemKeys = append(itemKeys, ik)
				}
				if tk := strings.TrimSpace(ev.Metadata["thread_key"]); tk != "" {
					itemKeys = append(itemKeys, tk)
				}
				if connector == "" {
					connector = strings.TrimSpace(ev.Metadata["connector"])
				}
				if account == "" {
					account = strings.TrimSpace(ev.Metadata["account"])
				}
			}
		}
		if needsTaint {
			_, _ = taint.Mark(context.Background(), vaultDir, taint.Record{
				ConvID:    convID,
				Source:    source,
				Connector: connector,
				Account:   account,
				BatchID:   batchID,
				EventIDs:  evIDs,
				ItemKeys:  itemKeys,
			})
		}
	}

	if boundAny || scannedTranscript {
		_ = store.WriteAtomic(checkMarker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
	}
}

// strictArgv parses a single simple command line into literal argv tokens without invoking a shell,
// rejecting any shell control operators, variable/command expansions, redirects, globs, or unclosed quotes.
func strictArgv(cmdLine string) ([]string, error) {
	s := strings.TrimSpace(cmdLine)
	if s == "" {
		return nil, fmt.Errorf("empty command line")
	}
	var argv []string
	var cur strings.Builder
	inToken := false
	inSingle := false
	inDouble := false

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inSingle {
			if ch == '\n' || ch == '\r' || ch == 0 {
				return nil, fmt.Errorf("control characters forbidden in strict argv")
			}
			if ch == '\'' {
				inSingle = false
				continue
			}
			cur.WriteByte(ch)
			continue
		}
		if inDouble {
			if ch == '\n' || ch == '\r' || ch == 0 {
				return nil, fmt.Errorf("control characters forbidden in strict argv")
			}
			// Forbid $ and ` inside double quotes (command/parameter expansion)
			if ch == '$' || ch == '`' {
				return nil, fmt.Errorf("shell expansion %q forbidden inside double quotes", ch)
			}
			if ch == '\\' {
				if i+1 >= len(s) {
					return nil, fmt.Errorf("trailing backslash in double quotes")
				}
				next := s[i+1]
				if next == '"' || next == '\\' {
					cur.WriteByte(next)
					i++
					continue
				}
				return nil, fmt.Errorf("unsupported escape \\%c in strict argv", next)
			}
			if ch == '"' {
				inDouble = false
				continue
			}
			cur.WriteByte(ch)
			continue
		}

		switch ch {
		case ' ', '\t':
			if inToken {
				argv = append(argv, cur.String())
				cur.Reset()
				inToken = false
			}
		case '\'':
			inSingle = true
			inToken = true
		case '"':
			inDouble = true
			inToken = true
		case '\n', '\r', 0, ';', '|', '&', '`', '$', '<', '>', '(', ')', '*', '?', '[', ']', '{', '}', '~', '\\':
			return nil, fmt.Errorf("forbidden shell metacharacter %q in strict argv", ch)
		default:
			cur.WriteByte(ch)
			inToken = true
		}
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unclosed quote in command line")
	}
	if inToken {
		argv = append(argv, cur.String())
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty argv")
	}
	return argv, nil
}

func isHQBinaryToken(bin string) bool {
	slash := filepath.ToSlash(strings.TrimSpace(bin))
	return slash == "hq" || slash == "./bin/hq" || slash == "bin/hq" || strings.HasSuffix(slash, "/hq")
}

// shellQuoteSingle wraps s in POSIX single quotes (`'...'`), escaping embedded single quotes safely.
func shellQuoteSingle(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func resolveTrustedHQExecutable(fallback string) string {
	if exe, err := os.Executable(); err == nil && strings.TrimSpace(exe) != "" {
		base := filepath.Base(exe)
		if base == "hq" || strings.HasPrefix(base, "hq.") {
			return exe
		}
	}
	return fallback
}

func wrapScrubbedArgvCommand(argv []string) string {
	quoted := make([]string, 0, len(argv)+7)
	quoted = append(quoted,
		"env",
		"-u", "ANTIGRAVITY_CSRF_TOKEN",
		"-u", "ANTIGRAVITY_LS_ADDRESS",
		"-u", "ANTIGRAVITY_AGENTAPI_EXE",
		"ANTIGRAVITY_PLUGIN_SHIM_DIR=/nonexistent-hq-sentinel",
		"--",
	)
	if len(argv) > 0 {
		quoted = append(quoted, shellQuoteSingle(resolveTrustedHQExecutable(argv[0])))
		for _, a := range argv[1:] {
			quoted = append(quoted, shellQuoteSingle(a))
		}
	}
	return strings.Join(quoted, " ")
}

func wrapScrubbedAutoShellCommand(cmdLine string) string {
	return "env -u ANTIGRAVITY_CSRF_TOKEN -u ANTIGRAVITY_LS_ADDRESS -u ANTIGRAVITY_AGENTAPI_EXE " +
		"ANTIGRAVITY_PLUGIN_SHIM_DIR=/nonexistent-hq-sentinel -- sh -c " + shellQuoteSingle(cmdLine)
}

// validateTaintedHQArgv enforces the strict Quarantined Worker CLI allowlist (RFC-0017b §3.2):
// ONLY `watch read|query|ack`, `offer stage`, `outbox add`, `timers set`, `goals mark-replied`,
// `memory lookup`, `brief format-quarantined`, `status`, and `gmail|gchat read-quarantined|triage-result` are allowed.
// `propose`, `approve`, `gmail draft`, `gchat draft`, `spawn`, `subagent`, and `exec-sandboxed` are DENIED.
func validateTaintedHQArgv(argv []string) error {
	if len(argv) < 2 || !isHQBinaryToken(argv[0]) {
		return fmt.Errorf("only single canonical `hq` commands are permitted in quarantined sessions")
	}
	for _, tok := range argv[1:] {
		if strings.HasPrefix(tok, "@") {
			return fmt.Errorf("leading `@` file-inclusion token %q is forbidden in quarantined sessions", tok)
		}
		if _, val, ok := strings.Cut(tok, "="); ok && strings.HasPrefix(val, "@") {
			return fmt.Errorf("leading `@` flag value in %q is forbidden in quarantined sessions", tok)
		}
	}
	top := argv[1]
	sub := ""
	if len(argv) >= 3 && !strings.HasPrefix(argv[2], "-") {
		sub = argv[2]
	}
	switch top {
	case "status":
		return nil
	case "watch":
		if sub == "read" || sub == "query" || sub == "ack" {
			return nil
		}
		return fmt.Errorf("quarantined `hq watch` allows only read, query, or ack (got %q)", sub)
	case "offer":
		if sub == "stage" {
			return nil
		}
		return fmt.Errorf("quarantined `hq offer` allows only stage (got %q)", sub)
	case "outbox":
		if sub == "add" {
			return nil
		}
		return fmt.Errorf("quarantined `hq outbox` allows only add (got %q)", sub)
	case "timers":
		if sub == "set" {
			return nil
		}
		return fmt.Errorf("quarantined `hq timers` allows only set (got %q)", sub)
	case "goals":
		if sub == "mark-replied" {
			return nil
		}
		return fmt.Errorf("quarantined `hq goals` allows only mark-replied (got %q)", sub)
	case "memory":
		if sub == "lookup" {
			return nil
		}
		return fmt.Errorf("quarantined `hq memory` allows only lookup (got %q)", sub)
	case "brief":
		if sub == "format-quarantined" {
			return nil
		}
		return fmt.Errorf("quarantined `hq brief` allows only format-quarantined (got %q)", sub)
	case "gmail", "gchat":
		if sub == "read-quarantined" || sub == "read" || sub == "triage-result" {
			return nil
		}
		return fmt.Errorf("quarantined `hq %s` allows only read-quarantined or triage-result; use `hq offer stage` instead of %q", top, sub)
	default:
		return fmt.Errorf("subcommand `hq %s` is forbidden in quarantined sessions; stage actions via `hq offer stage` or `hq outbox add`", top)
	}
}

func isAllowedTaintedReadPath(vaultDir string, payload ToolCallPayload, rawPath string) bool {
	if strings.TrimSpace(rawPath) == "" || pathHasSymlink(rawPath) {
		return false
	}
	canonPath := canonicalizeHookPath(vaultDir, rawPath)
	canonSlash := filepath.ToSlash(canonPath)
	if isSystemGeneratedTranscriptPath(canonSlash) || isSystemGeneratedTranscriptPath(filepath.ToSlash(filepath.Clean(rawPath))) {
		return false
	}

	// 1. Own conversation artifact directory (excluding .system_generated)
	var allowedArtDirs []string
	if strings.TrimSpace(payload.ArtifactDirectoryPath) != "" {
		allowedArtDirs = append(allowedArtDirs, canonicalizeHookPath(vaultDir, payload.ArtifactDirectoryPath))
	}
	if roots := hostpaths.Resolve(vaultDir); roots.BrainDir != "" && strings.TrimSpace(payload.ConversationID) != "" {
		allowedArtDirs = append(allowedArtDirs, canonicalizeHookPath(vaultDir, filepath.Join(roots.BrainDir, strings.TrimSpace(payload.ConversationID))))
	}
	for _, ad := range allowedArtDirs {
		if ad != "" && ad != "." && (canonPath == ad || strings.HasPrefix(canonPath, ad+string(filepath.Separator))) {
			return true
		}
	}

	// Never allow reading any file inside <vault>/ even if a directory inside <vault>/ mimics /.agents/plugins/.../skills/
	if strings.TrimSpace(vaultDir) != "" {
		canonVault := canonicalizeHookPath(vaultDir, vaultDir)
		if canonVault != "" && (canonPath == canonVault || strings.HasPrefix(canonPath, canonVault+string(filepath.Separator))) {
			return false
		}
	}

	// 2. Installed or repo plugin skills/ directories (read-only SKILL.md instructions)
	if plugRoot := strings.TrimSpace(os.Getenv("PLUGIN_ROOT")); plugRoot != "" {
		skillsDir := canonicalizeHookPath(vaultDir, filepath.Join(plugRoot, "skills"))
		if skillsDir != "" && (canonPath == skillsDir || strings.HasPrefix(canonPath, skillsDir+string(filepath.Separator))) {
			return true
		}
	}
	roots := hostpaths.Resolve(vaultDir)
	if roots.PluginsDir != "" {
		plugBase := canonicalizeHookPath(vaultDir, roots.PluginsDir)
		if rel, err := filepath.Rel(plugBase, canonPath); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if len(parts) >= 2 && parts[1] == "skills" {
				return true
			}
		}
	}
	if strings.Contains(canonSlash, "/.agents/plugins/") && strings.Contains(canonSlash, "/skills/") {
		return true
	}
	return false
}

func parseCanonicalHQApprove(cmdLine string) (bin, propID, shaPrefix string, err error) {
	argv, err := strictArgv(cmdLine)
	if err != nil {
		return "", "", "", fmt.Errorf("`hq approve` must be a single unchained command without shell metacharacters: %w", err)
	}
	if len(argv) < 3 || !isHQBinaryToken(argv[0]) || argv[1] != "approve" {
		return "", "", "", fmt.Errorf("canonical `hq approve` syntax required: `hq approve <proposal-id> --sha256=<prefix>`")
	}
	for _, a := range argv[2:] {
		if strings.HasPrefix(a, "--sha256=") {
			shaPrefix = strings.TrimPrefix(a, "--sha256=")
		} else if strings.HasPrefix(a, "-sha256=") {
			shaPrefix = strings.TrimPrefix(a, "-sha256=")
		} else if strings.HasPrefix(a, "-") {
			return "", "", "", fmt.Errorf("unexpected flag %q on canonical `hq approve`", a)
		} else if propID == "" {
			propID = a
		} else {
			return "", "", "", fmt.Errorf("unexpected positional argument %q on canonical `hq approve`", a)
		}
	}
	if propID == "" || shaPrefix == "" {
		return "", "", "", fmt.Errorf("canonical `hq approve` requires both `<proposal-id>` and `--sha256=<12+ hex prefix>`")
	}
	return argv[0], propID, shaPrefix, nil
}
