package hooks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/taint"
)

var (
	reSpawnTokenTag = regexp.MustCompile(`\[hq-spawn:([0-9a-f]{32,64})\]`)
	reHQApproveAny  = regexp.MustCompile(`(?i)(?:^|[\s;&|("'` + "`" + `])(?:\S+/)?hq\s+approve\b`)
)

// bindSpawnTokenFromTranscript inspects the first 64 KiB of transcriptPath for `[hq-spawn:<token>]`,
// atomically CAS-binds the leased event batch to convID, and marks convID tainted BEFORE any tool executes
// if any bound event carries TaintOnSpawn=true.
func bindSpawnTokenFromTranscript(vaultDir, convID, transcriptPath string) {
	convID = strings.TrimSpace(convID)
	transcriptPath = strings.TrimSpace(transcriptPath)
	if convID == "" || transcriptPath == "" {
		return
	}
	st, err := os.Lstat(transcriptPath)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return
	}
	defer f.Close()

	head, err := io.ReadAll(io.LimitReader(f, 64*1024))
	if err != nil || len(head) == 0 {
		return
	}
	matches := reSpawnTokenTag.FindAllSubmatch(head, 4)
	for _, m := range matches {
		rawTok := string(bytes.TrimSpace(m[1]))
		bound, ok, bErr := events.BindBatchByToken(vaultDir, rawTok, convID, time.Now(), events.DefaultLeaseTTL)
		if bErr != nil || !ok || len(bound) == 0 {
			continue
		}
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
	if strings.HasPrefix(strings.TrimSpace(cmdLine), "env -u ANTIGRAVITY_CSRF_TOKEN ") {
		return cmdLine
	}
	return "env -u ANTIGRAVITY_CSRF_TOKEN -u ANTIGRAVITY_LS_ADDRESS -u ANTIGRAVITY_AGENTAPI_EXE -- sh -c " + shellQuoteSingle(cmdLine)
}

// validateTaintedHQArgv enforces the strict Quarantined Worker CLI allowlist (RFC-0017b §3.2):
// ONLY `watch read|query|ack`, `offer stage`, `outbox add`, `timers set`, `memory lookup`,
// `status`, and `gmail|gchat read-quarantined|triage-result` are allowed.
// `propose`, `approve`, `gmail draft`, `gchat draft`, `spawn`, `subagent`, and `exec-sandboxed` are DENIED.
func validateTaintedHQArgv(argv []string) error {
	if len(argv) < 2 || !isHQBinaryToken(argv[0]) {
		return fmt.Errorf("only single canonical `hq` commands are permitted in quarantined sessions")
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
	case "memory":
		if sub == "lookup" {
			return nil
		}
		return fmt.Errorf("quarantined `hq memory` allows only lookup (got %q)", sub)
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
		if strings.HasPrefix(canonPath, plugBase+string(filepath.Separator)) && strings.Contains(canonSlash, "/skills/") {
			return true
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
