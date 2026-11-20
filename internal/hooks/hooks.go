package hooks

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/taint"
)

const PreCompactionFlushTokens int64 = 120000

// CommonPayload contains the camelCase fields passed by Antigravity on hook stdin.
type CommonPayload struct {
	ConversationID        string   `json:"conversationId"`
	WorkspacePaths        []string `json:"workspacePaths"`
	TranscriptPath        string   `json:"transcriptPath"`
	ArtifactDirectoryPath string   `json:"artifactDirectoryPath"`
	ModelName             string   `json:"modelName"`
	LastUserInput         string   `json:"lastUserInput"`
}

// ToolCallPayload represents PreToolUse stdin JSON.
type ToolCallPayload struct {
	CommonPayload
	StepIdx  int `json:"stepIdx"`
	ToolCall struct {
		Name string                 `json:"name"`
		Args map[string]interface{} `json:"args"`
	} `json:"toolCall"`
}

// StopPayload represents Stop stdin JSON.
type StopPayload struct {
	CommonPayload
	ExecutionNum      int    `json:"executionNum"`
	TerminationReason string `json:"terminationReason"`
	Error             string `json:"error"`
	FullyIdle         *bool  `json:"fullyIdle,omitempty"`
	LastResponse      string `json:"lastResponse,omitempty"`
}

// JournalEntry is the immutable per-turn summary written by hq hook stop (0 LLM tokens).
type JournalEntry struct {
	Timestamp            string   `json:"timestamp"`
	ConversationID       string   `json:"conversationId"`
	ParentConvID         string   `json:"parentConversationId,omitempty"`
	ExecutionNum         int      `json:"executionNum"`
	TranscriptPath       string   `json:"transcriptPath,omitempty"`
	WorkspacePaths       []string `json:"workspacePaths,omitempty"`
	ModelName            string   `json:"modelName,omitempty"`
	StepCount            int      `json:"stepCount,omitempty"`
	ToolErrorCount       int      `json:"toolErrorCount,omitempty"`
	StepStartIdx         int      `json:"stepStartIdx,omitempty"`
	TurnStepCount        int      `json:"turnStepCount,omitempty"`
	TurnErrorCount       int      `json:"turnErrorCount,omitempty"`
	TransientErrors      int      `json:"transientErrors,omitempty"`
	PreToolDenials       int      `json:"preToolDenials,omitempty"`
	TaintedSession       bool     `json:"taintedSession,omitempty"`
	TurnInputTokens      int64    `json:"turnInputTokens,omitempty"`
	TurnCacheReadTokens  int64    `json:"turnCacheReadTokens,omitempty"`
	TurnOutputTokens     int64    `json:"turnOutputTokens,omitempty"`
	TurnUncachedTokens   int64    `json:"turnUncachedTokens,omitempty"`
	TurnGrossTokens      int64    `json:"turnGrossTokens,omitempty"`
	TurnCacheHitRatio    float64  `json:"turnCacheHitRatio,omitempty"`
	PeakContextTokens    int64    `json:"peakContextTokens,omitempty"`
	CurrentContextTokens int64    `json:"currentContextTokens,omitempty"`
	CompactionCount      int      `json:"compactionCount,omitempty"`
	ActiveLatencyMs      int64    `json:"activeLatencyMs,omitempty"`
	HumanWaitMs          int64    `json:"humanWaitMs,omitempty"`
	UserPrompt           string   `json:"userPrompt,omitempty"`
	ToolsUsed            []string `json:"toolsUsed,omitempty"`
	FilesModified        []string `json:"filesModified,omitempty"`
	TerminationReason    string   `json:"terminationReason"`
	ResponseSummary      string   `json:"responseSummary,omitempty"`
}

// TranscriptStepError captures a single failed tool or command execution step from transcript.jsonl.
type TranscriptStepError struct {
	StepIndex int    `json:"stepIndex"`
	Type      string `json:"type,omitempty"`
	Summary   string `json:"summary"`
}

// TranscriptVerificationDigest is the 0-token ground-truth digest of a conversation's raw transcript.jsonl.
type TranscriptVerificationDigest struct {
	ConversationID      string                `json:"conversationId,omitempty"`
	TranscriptPath      string                `json:"transcriptPath"`
	TotalSteps          int                   `json:"totalSteps"`
	UserInputs          []string              `json:"userInputs,omitempty"`
	ToolsUsed           []string              `json:"toolsUsed,omitempty"`
	FilesModified       []string              `json:"filesModified,omitempty"`
	CommandsExecuted    []string              `json:"commandsExecuted,omitempty"`
	ToolErrorCount      int                   `json:"toolErrorCount"`
	Errors              []TranscriptStepError `json:"errors,omitempty"`
	FinalResponse       string                `json:"finalResponse,omitempty"`
	DiscrepancyWarnings []string              `json:"discrepancyWarnings,omitempty"`
}

func pathHasPrefix(target, base string) bool {
	if target == "" || base == "" {
		return false
	}
	return target == base || strings.HasPrefix(target, base+string(filepath.Separator))
}

func isPathInsideVaultWorktrees(targetPath, vaultDir string) bool {
	if strings.TrimSpace(targetPath) == "" || strings.TrimSpace(vaultDir) == "" {
		return false
	}
	wtRoot := filepath.Clean(filepath.Join(vaultDir, "worktrees"))
	return pathHasPrefix(filepath.Clean(targetPath), wtRoot)
}

func canonicalizeHookPath(vaultDir, rawPath string) string {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return ""
	}
	p := trimmed
	if !filepath.IsAbs(p) {
		rootDir := filepath.Dir(filepath.Clean(vaultDir))
		p = filepath.Join(rootDir, p)
	}
	clean := filepath.Clean(p)
	cur := clean
	var tail []string
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return clean
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

func resolveHookLexicalAbs(vaultDir, rawPath string) string {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return ""
	}
	p := trimmed
	if !filepath.IsAbs(p) {
		rootDir := filepath.Dir(filepath.Clean(vaultDir))
		p = filepath.Join(rootDir, p)
	}
	return filepath.Clean(p)
}

func pathHasSymlink(rawPath string) bool {
	cur := filepath.Clean(rawPath)
	for {
		if st, err := os.Lstat(cur); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

func isSystemGeneratedTranscriptPath(cleanSlash string) bool {
	return strings.Contains(cleanSlash, "/.system_generated/") || strings.HasSuffix(cleanSlash, "/.system_generated")
}

// isSafePathUnderBase verifies that rawTargetPath resolves canonically inside rawBaseDir,
// matches rawBaseDir lexically (accommodating OS/temp/home/vault root symlinks above rawBaseDir,
// such as macOS /var -> /private/var or enterprise symlinked $HOME), and traverses zero
// symlinks (live or dangling) at or below rawBaseDir.
func isSafePathUnderBase(vaultDir, rawBaseDir, rawTargetPath string) (string, bool) {
	absBase := resolveHookLexicalAbs(vaultDir, rawBaseDir)
	absTarget := resolveHookLexicalAbs(vaultDir, rawTargetPath)
	if absBase == "" || absTarget == "" {
		return "", false
	}
	canonBase := canonicalizeHookPath(vaultDir, absBase)
	canonTarget := canonicalizeHookPath(vaultDir, absTarget)
	if canonBase == "" || canonBase == "." || canonTarget == "" || canonTarget == "." {
		return "", false
	}
	if pathHasSymlink(canonBase) || pathHasSymlink(canonTarget) {
		return "", false
	}
	if !pathHasPrefix(canonTarget, canonBase) {
		return "", false
	}

	sep := string(filepath.Separator)
	candidates := []string{absBase, canonBase}
	trustedRoots := []string{
		filepath.Clean(os.TempDir()),
		"/var",
		"/tmp",
	}
	if strings.TrimSpace(vaultDir) != "" {
		cleanV := filepath.Clean(vaultDir)
		trustedRoots = append(trustedRoots, cleanV, filepath.Dir(cleanV))
	}
	if home, err := hostpaths.ResolveHomeDir(); err == nil && home != "" {
		trustedRoots = append(trustedRoots, filepath.Clean(home))
	}
	for _, tr := range trustedRoots {
		if tr == "" || tr == "." || tr == sep {
			continue
		}
		canonTR := canonicalizeHookPath(vaultDir, tr)
		if canonTR != "" && canonTR != tr {
			if canonBase == canonTR {
				candidates = append(candidates, tr)
			} else if strings.HasPrefix(canonBase, canonTR+sep) {
				candidates = append(candidates, tr+strings.TrimPrefix(canonBase, canonTR))
			}
		}
	}

	matchedRel := ""
	matched := false
	for _, cand := range candidates {
		if cand == "" || cand == "." {
			continue
		}
		rel, err := filepath.Rel(cand, absTarget)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+sep)) {
			matchedRel = rel
			matched = true
			break
		}
	}
	if !matched {
		return "", false
	}

	if matchedRel != "." {
		cur := canonBase
		for _, seg := range strings.Split(filepath.ToSlash(matchedRel), "/") {
			if seg == "" || seg == "." {
				continue
			}
			cur = filepath.Join(cur, seg)
			if st, err := os.Lstat(cur); err == nil && st.Mode()&os.ModeSymlink != 0 {
				return "", false
			}
		}
	}
	return filepath.ToSlash(matchedRel), true
}

func allowedArtifactDirs(payload CommonPayload, extraArtDir string, roots hostpaths.Roots) []string {
	var dirs []string
	if d := strings.TrimSpace(payload.ArtifactDirectoryPath); d != "" {
		dirs = append(dirs, d)
	}
	if d := strings.TrimSpace(extraArtDir); d != "" {
		dirs = append(dirs, d)
	}
	if roots.BrainDir != "" && strings.TrimSpace(payload.ConversationID) != "" {
		dirs = append(dirs, filepath.Join(roots.BrainDir, strings.TrimSpace(payload.ConversationID)))
	}
	return dirs
}

func isPathInAnyBase(vaultDir string, bases []string, rawPath string) bool {
	for _, b := range bases {
		if _, ok := isSafePathUnderBase(vaultDir, b, rawPath); ok {
			return true
		}
	}
	return false
}

func isAutoConversation(vaultDir, convID string) bool {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return false
	}
	if t, _, _, ok := kernel.FindTaskAttemptAnywhere(vaultDir, convID); ok {
		return t.EffectiveKind() == "auto"
	}
	if hasSpawnMarker(vaultDir, convID) {
		return true
	}
	if rec, ok := taint.Check(vaultDir, convID); ok && strings.HasPrefix(rec.Source, "spawn_batch") {
		return true
	}
	return false
}

func resetFrontDeskTurnState(vaultDir, convID string) {
	turnFile := filepath.Join(vaultDir, "memory", "front_desk_turn.json")
	resetState := map[string]interface{}{
		"conversationId": convID,
		"count":          0,
		"updatedAt":      time.Now().Format(time.RFC3339),
	}
	_ = os.MkdirAll(filepath.Dir(turnFile), 0o755)
	if data, err := json.MarshalIndent(resetState, "", "  "); err == nil {
		_ = os.WriteFile(turnFile, data, 0o644)
	}
}

func writeActiveArtifactDir(vaultDir, artDir string) {
	trimmed := strings.TrimSpace(artDir)
	if trimmed == "" {
		return
	}
	p := filepath.Join(vaultDir, "memory", "active_artifact_dir.txt")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(trimmed), 0o644)
}

func writePreToolAllow(w io.Writer) error {
	_, err := w.Write([]byte(`{"decision":"allow"}` + "\n"))
	return err
}

func writePreToolAllowOverwrite(w io.Writer, overwrite interface{}) error {
	return json.NewEncoder(w).Encode(map[string]interface{}{
		"decision":  "allow",
		"overwrite": overwrite,
	})
}

func writePreToolDeny(w io.Writer, reason string) error {
	return json.NewEncoder(w).Encode(map[string]interface{}{
		"decision": "deny",
		"reason":   reason,
	})
}

func trimInlineRunes(s string, maxRunes int) string {
	clean := strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	runes := []rune(clean)
	if len(runes) <= maxRunes {
		return clean
	}
	if maxRunes <= 1 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-1]) + "…"
}

func trimBytesPreservingLines(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	lines := strings.Split(s, "\n")
	var out []string
	used := 0
	const suffix = "\n… *(trimmed to session budget)*"
	limit := maxBytes - len(suffix)
	if limit < 64 {
		return capSessionEnvelope(s, maxBytes)
	}
	for _, line := range lines {
		add := len(line)
		if len(out) > 0 {
			add++
		}
		if used+add > limit {
			if len(out) == 0 {
				return capSessionEnvelope(line, limit) + suffix
			}
			break
		}
		out = append(out, line)
		used += add
	}
	return strings.Join(out, "\n") + suffix
}

func capSessionEnvelope(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	const marker = "\n… *(capped to budget)*"
	cut := maxBytes - len(marker)
	if cut < 32 {
		cut = maxBytes
	}
	// Avoid splitting UTF-8 multi-byte rune
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	if maxBytes-len(marker) < 32 {
		return s[:cut]
	}
	return strings.TrimRight(s[:cut], "\n ") + marker
}
