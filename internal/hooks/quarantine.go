package hooks

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/authz"
	"github.com/zjshen14/agy-hq/internal/events"
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

func spawnMarkerPath(vaultDir, convID string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(convID))))
	return filepath.Join(vaultDir, "private", "taint", ".spawn-"+hex.EncodeToString(sum[:16]))
}

func hasSpawnMarker(vaultDir, convID string) bool {
	convID = strings.TrimSpace(convID)
	if convID == "" || strings.TrimSpace(vaultDir) == "" {
		return false
	}
	st, err := os.Lstat(spawnMarkerPath(vaultDir, convID))
	return err == nil && st.Mode().IsRegular()
}

// readFirstUserInputContent reads the untruncated cleaned content of the first USER_INPUT step
// from the initial 64 KiB of transcriptPath so tail tags like `[hq-spawn:<token>]` are preserved.
func readFirstUserInputContent(transcriptPath string) string {
	transcriptPath = strings.TrimSpace(transcriptPath)
	if transcriptPath == "" {
		return ""
	}
	st, err := os.Lstat(transcriptPath)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	lr := io.LimitReader(f, 64*1024)
	scanner := bufio.NewScanner(lr)
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)
	for scanner.Scan() {
		var step struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		}
		if json.Unmarshal(scanner.Bytes(), &step) != nil {
			continue
		}
		if step.Type == "USER_INPUT" && strings.TrimSpace(step.Content) != "" {
			return cleanUserPrompt(step.Content)
		}
	}
	return ""
}

func hasAutoOrSpawnEvidence(vaultDir, convID, firstUserInput, fallbackPrompt string) bool {
	for _, s := range []string{firstUserInput, fallbackPrompt} {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(trimmed), "[auto]") || reSpawnTokenTag.MatchString(s) {
			return true
		}
	}
	if hasSpawnMarker(vaultDir, convID) {
		return true
	}
	if strings.TrimSpace(convID) != "" && strings.TrimSpace(vaultDir) != "" {
		if rec, ok := taint.Check(vaultDir, convID); ok && strings.HasPrefix(rec.Source, "spawn_batch") {
			return true
		}
	}
	return false
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

	userInputHasSpawn := false
	if lastUserInput != "" {
		addMatches([]byte(lastUserInput))
		if reSpawnTokenTag.MatchString(lastUserInput) {
			userInputHasSpawn = true
		}
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
					for _, rawLine := range bytes.Split(head, []byte("\n")) {
						var step struct {
							Type    string `json:"type"`
							Content string `json:"content"`
						}
						if json.Unmarshal(rawLine, &step) == nil && step.Type == "USER_INPUT" && reSpawnTokenTag.MatchString(step.Content) {
							userInputHasSpawn = true
							break
						}
					}
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
		_, _, _ = authz.BindSessionFromEvents(context.Background(), vaultDir, convID, bound)
	}

	if boundAny || userInputHasSpawn {
		_ = store.WriteAtomic(spawnMarkerPath(vaultDir, convID), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
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
	quoted := make([]string, 0, len(argv)+6)
	quoted = append(quoted,
		"env",
		"-u", "ANTIGRAVITY_CSRF_TOKEN",
		"-u", "ANTIGRAVITY_LS_ADDRESS",
		"-u", "ANTIGRAVITY_AGENTAPI_EXE",
		"ANTIGRAVITY_PLUGIN_SHIM_DIR=/nonexistent-hq-sentinel",
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
		"ANTIGRAVITY_PLUGIN_SHIM_DIR=/nonexistent-hq-sentinel sh -c " + shellQuoteSingle(cmdLine)
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
