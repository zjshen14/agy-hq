package approvals

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/store"
)

const (
	DefaultAskTTL          = 24 * time.Hour
	DefaultCapTTL          = 60 * time.Second
	maxTranscriptTailBytes = 2 * 1024 * 1024 // 2 MiB bounded tail scan
	nonceAlphabet          = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

var reCanonicalApproveSHA = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// AskRecord tracks a pending ask_question human-presence challenge in <vault>/private/approval_asks/<id>.json.
type AskRecord struct {
	ProposalID string `json:"proposal_id"`
	FullSHA256 string `json:"full_sha256"`
	ConvID     string `json:"conv_id"`
	NonceCode  string `json:"nonce_code"`
	AskStepMin int    `json:"ask_step_min"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at"`
}

// CapRecord represents a one-shot 60s approval capability in <vault>/private/approval_caps/<id>.json.
type CapRecord struct {
	ProposalID string `json:"proposal_id"`
	FullSHA256 string `json:"full_sha256"`
	VaultCanon string `json:"vault_canon"`
	ConvID     string `json:"conv_id"`
	CapSHA256  string `json:"cap_sha256"`
	StepIndex  int    `json:"step_index"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at"`
}

type transcriptStepLine struct {
	StepIndex       int      `json:"step_index"`
	Source          string   `json:"source"`
	Type            string   `json:"type"`
	Status          string   `json:"status"`
	Content         string   `json:"content"`
	TruncatedFields []string `json:"truncated_fields"`
}

func approvalAsksDir(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "approval_asks")
}

func approvalCapsDir(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "approval_caps")
}

func canonicalVaultPath(vaultDir string) string {
	abs, err := filepath.Abs(vaultDir)
	if err != nil {
		return filepath.Clean(vaultDir)
	}
	if real, rErr := filepath.EvalSymlinks(abs); rErr == nil && real != "" {
		return filepath.Clean(real)
	}
	return filepath.Clean(abs)
}

func generateNonceCode(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range raw {
		out[i] = nonceAlphabet[int(b)%len(nonceAlphabet)]
	}
	return string(out), nil
}

func sha256HexString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func readFileTail(path string, maxBytes int64) ([]byte, error) {
	st, lErr := os.Lstat(path)
	if lErr != nil {
		return nil, lErr
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing non-regular transcript path %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	size := st.Size()
	offset := int64(0)
	if size > maxBytes {
		offset = size - maxBytes
		if _, sErr := f.Seek(offset, io.SeekStart); sErr != nil {
			return nil, sErr
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	// If we seeked into the middle of a line, discard the partial leading line.
	if offset > 0 {
		if nl := bytes.IndexByte(data, '\n'); nl >= 0 {
			data = data[nl+1:]
		}
	}
	return data, nil
}

func highestTranscriptStepIndex(transcriptPath string) int {
	transcriptPath = strings.TrimSpace(transcriptPath)
	if transcriptPath == "" {
		return 0
	}
	data, err := readFileTail(transcriptPath, 512*1024)
	if err != nil || len(data) == 0 {
		return 0
	}
	maxStep := 0
	lines := bytes.Split(data, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var st transcriptStepLine
		if json.Unmarshal(line, &st) == nil && st.StepIndex > maxStep {
			maxStep = st.StepIndex
		}
	}
	return maxStep
}

func lookupStepContentInFullTranscript(transcriptPath string, targetStep int) string {
	dir := filepath.Dir(transcriptPath)
	fullPath := filepath.Join(dir, "transcript_full.jsonl")
	data, err := readFileTail(fullPath, maxTranscriptTailBytes)
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := bytes.Split(data, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var st transcriptStepLine
		if json.Unmarshal(line, &st) == nil && st.StepIndex == targetStep {
			if st.Status == "DONE" && st.Source == "MODEL" && st.Type == "GENERIC" {
				return st.Content
			}
			return ""
		}
	}
	return ""
}

func hasSliceItem(items []string, want string) bool {
	for _, it := range items {
		if strings.EqualFold(strings.TrimSpace(it), want) {
			return true
		}
	}
	return false
}

// CleanupApprovalArtifacts best-effort removes pending ask/cap files when a proposal settles or expires.
func CleanupApprovalArtifacts(vaultDir, propID string) {
	if ValidateProposalID(propID) != nil {
		return
	}
	_ = os.Remove(filepath.Join(approvalAsksDir(vaultDir), propID+".json"))
	_ = os.Remove(filepath.Join(approvalCapsDir(vaultDir), propID+".json"))
	cleanupBridgeApprovalCaps(vaultDir, propID)
}

func cleanupBridgeApprovalCaps(vaultDir, propID string) {
	if strings.TrimSpace(vaultDir) == "" {
		return
	}
	dir := filepath.Join(vaultDir, "private", "approval_bridge_caps")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		raw, rErr := os.ReadFile(full)
		if rErr != nil {
			continue
		}
		var rec struct {
			ProposalID string `json:"proposal_id"`
			ExpiresAt  string `json:"expires_at"`
		}
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		if rec.ProposalID == propID {
			_ = os.Remove(full)
			continue
		}
		if exp, pErr := time.Parse(time.RFC3339, rec.ExpiresAt); pErr == nil && now.After(exp) {
			_ = os.Remove(full)
		}
	}
}

// RecordAsk creates an AskRecord with a fresh 6-char challenge code for propID before ask_question renders.
func RecordAsk(vaultDir, propID, convID, transcriptPath string, now time.Time) (AskRecord, error) {
	propID = strings.TrimSpace(propID)
	convID = strings.TrimSpace(convID)
	if err := ValidateProposalID(propID); err != nil {
		return AskRecord{}, err
	}
	if convID == "" {
		return AskRecord{}, fmt.Errorf("approvals: conversation ID required to record ask challenge")
	}
	if now.IsZero() {
		now = time.Now()
	}

	pendingPath := filepath.Join(vaultDir, "memory", "pending", propID+".json")
	rawProp, err := os.ReadFile(pendingPath)
	if err != nil {
		return AskRecord{}, fmt.Errorf("approvals: read pending proposal %s: %w", propID, err)
	}
	var prop Proposal
	if err := json.Unmarshal(rawProp, &prop); err != nil || prop.ID != propID {
		return AskRecord{}, fmt.Errorf("approvals: invalid pending proposal %s", propID)
	}
	if prop.IsExpired(now) {
		return AskRecord{}, fmt.Errorf("approvals: proposal %s is expired", propID)
	}
	if err := VerifyProposalHash(prop); err != nil {
		return AskRecord{}, err
	}

	nonce, err := generateNonceCode(6)
	if err != nil {
		return AskRecord{}, err
	}
	askTTL := loadPolicyAskTTL(vaultDir)
	rec := AskRecord{
		ProposalID: propID,
		FullSHA256: strings.ToLower(strings.TrimSpace(prop.SHA256)),
		ConvID:     convID,
		NonceCode:  nonce,
		AskStepMin: highestTranscriptStepIndex(transcriptPath),
		CreatedAt:  now.UTC().Format(time.RFC3339),
		ExpiresAt:  now.Add(askTTL).UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return AskRecord{}, err
	}
	askPath := filepath.Join(approvalAsksDir(vaultDir), propID+".json")
	if err := store.WriteAtomic(askPath, append(data, '\n'), 0o600); err != nil {
		return AskRecord{}, err
	}
	return rec, nil
}

// VerifyAskAndMintCap verifies that the Antigravity conversation transcript contains a genuine
// ask_question tool-response step (source=MODEL, type=GENERIC, status=DONE, step_index > AskStepMin)
// containing both propID and `code:<NonceCode>`, enforces O_EXCL step replay protection, and mints a
// 60-second single-use capability token for `hq approve`.
func VerifyAskAndMintCap(vaultDir, propID, shaPrefix, convID, transcriptPath string, now time.Time) (string, string, error) {
	propID = strings.TrimSpace(propID)
	convID = strings.TrimSpace(convID)
	normPrefix := strings.ToLower(strings.TrimSpace(shaPrefix))
	if err := ValidateProposalID(propID); err != nil {
		return "", "", err
	}
	if !reCanonicalApproveSHA.MatchString(normPrefix) {
		return "", "", fmt.Errorf("canonical `hq approve` requires >= 12 lowercase hex sha256 characters (got %q)", shaPrefix)
	}
	if convID == "" || strings.TrimSpace(transcriptPath) == "" {
		return "", "", fmt.Errorf("missing conversation ID or transcript path for human-presence verification")
	}
	if now.IsZero() {
		now = time.Now()
	}

	pendingPath := filepath.Join(vaultDir, "memory", "pending", propID+".json")
	rawProp, err := os.ReadFile(pendingPath)
	if err != nil {
		return "", "", fmt.Errorf("pending proposal %s not found: %w", propID, err)
	}
	var prop Proposal
	if err := json.Unmarshal(rawProp, &prop); err != nil || prop.ID != propID {
		return "", "", fmt.Errorf("corrupt pending proposal %s", propID)
	}
	if prop.IsExpired(now) {
		return "", "", fmt.Errorf("proposal %s expired at %s", propID, prop.ExpiresAt)
	}
	if err := VerifyProposalHash(prop); err != nil {
		return "", "", err
	}
	fullSHA := strings.ToLower(strings.TrimSpace(prop.SHA256))
	if !strings.HasPrefix(fullSHA, normPrefix) {
		return "", "", fmt.Errorf("sha256 prefix mismatch for %s", propID)
	}

	askPath := filepath.Join(approvalAsksDir(vaultDir), propID+".json")
	rawAsk, err := os.ReadFile(askPath)
	if err != nil {
		return "", "", fmt.Errorf("no ask_question challenge recorded for %s; present the proposal via ask_question before calling `hq approve`", propID)
	}
	var ask AskRecord
	if err := json.Unmarshal(rawAsk, &ask); err != nil {
		return "", "", fmt.Errorf("corrupt ask challenge record for %s", propID)
	}
	if ask.ProposalID != propID || ask.ConvID != convID {
		return "", "", fmt.Errorf("ask_question challenge for %s belongs to a different conversation (%s); either run `hq approve` in that conversation or present a fresh `ask_question` referencing %s in this conversation first", propID, ask.ConvID, propID)
	}
	if strings.ToLower(strings.TrimSpace(ask.FullSHA256)) != fullSHA {
		return "", "", fmt.Errorf("proposal %s payload changed since ask_question challenge was shown", propID)
	}
	if exp, pErr := time.Parse(time.RFC3339, ask.ExpiresAt); pErr != nil || now.After(exp) {
		_ = os.Remove(askPath)
		return "", "", fmt.Errorf("ask_question challenge for %s expired; re-prompt the user via ask_question", propID)
	}

	tailBytes, err := readFileTail(transcriptPath, maxTranscriptTailBytes)
	if err != nil {
		return "", "", fmt.Errorf("read transcript for human-presence proof: %w", err)
	}
	wantCodeToken := "code:" + ask.NonceCode
	matchedStepIdx := -1
	lines := bytes.Split(tailBytes, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var st transcriptStepLine
		if json.Unmarshal(line, &st) != nil {
			continue
		}
		if st.StepIndex <= ask.AskStepMin {
			break
		}
		// Never trust USER_EXPLICIT / USER_INPUT steps (forgeable via SendUserCascadeMessage).
		// Real ask_question user selections are recorded as MODEL + GENERIC + DONE tool results.
		if st.Status != "DONE" || st.Source != "MODEL" || st.Type != "GENERIC" {
			continue
		}
		content := st.Content
		if (!strings.Contains(content, propID) || !strings.Contains(content, wantCodeToken)) && hasSliceItem(st.TruncatedFields, "content") {
			if full := lookupStepContentInFullTranscript(transcriptPath, st.StepIndex); full != "" {
				content = full
			}
		}
		if strings.Contains(content, propID) && strings.Contains(content, wantCodeToken) {
			matchedStepIdx = st.StepIndex
			break
		}
	}
	if matchedStepIdx < 0 {
		return "", "", fmt.Errorf("human-presence proof failed for %s: no completed ask_question user selection containing %s found after step %d", propID, wantCodeToken, ask.AskStepMin)
	}

	usedMarker := filepath.Join(approvalAsksDir(vaultDir), ".used-"+sha256HexString(convID+":"+strconv.Itoa(matchedStepIdx)+":"+propID))
	if _, uErr := os.Lstat(usedMarker); uErr == nil {
		return "", "", fmt.Errorf("human-presence proof replay rejected for %s (step %d already consumed)", propID, matchedStepIdx)
	}

	capPath := filepath.Join(approvalCapsDir(vaultDir), propID+".json")
	if rawExistingCap, rErr := os.ReadFile(capPath); rErr == nil {
		var existingCap CapRecord
		if json.Unmarshal(rawExistingCap, &existingCap) == nil {
			if cExp, cErr := time.Parse(time.RFC3339, existingCap.ExpiresAt); cErr == nil && !now.After(cExp) {
				return "", "", fmt.Errorf("human-presence proof replay rejected for %s (active capability already minted for step %d)", propID, existingCap.StepIndex)
			}
		}
		_ = os.Remove(capPath)
	}

	// Two-phase commit (RFC-0021 / Issue #28): keep askPath intact and defer `.used-*` stamping until
	// ApproveFromCLI consumes the minted capability so hook timeouts or pre-flight errors never brick the ask.
	var capBytes [32]byte
	if _, err := rand.Read(capBytes[:]); err != nil {
		return "", "", err
	}
	rawCap := hex.EncodeToString(capBytes[:])
	capRec := CapRecord{
		ProposalID: propID,
		FullSHA256: fullSHA,
		VaultCanon: canonicalVaultPath(vaultDir),
		ConvID:     convID,
		CapSHA256:  sha256HexString(rawCap),
		StepIndex:  matchedStepIdx,
		CreatedAt:  now.UTC().Format(time.RFC3339),
		ExpiresAt:  now.Add(DefaultCapTTL).UTC().Format(time.RFC3339),
	}
	capJSON, err := json.MarshalIndent(capRec, "", "  ")
	if err != nil {
		return "", "", err
	}
	cf, cErr := store.OpenCreateExclNoFollow(capPath, 0o600)
	if cErr != nil {
		return "", "", fmt.Errorf("mint approval capability for %s: %w", propID, cErr)
	}
	if _, wErr := cf.Write(append(capJSON, '\n')); wErr != nil {
		_ = cf.Close()
		_ = os.Remove(capPath)
		return "", "", wErr
	}
	_ = cf.Sync()
	_ = cf.Close()

	return rawCap, fullSHA, nil
}

// ApproveFromTrustedDaemon executes an approval verified by the local :4242 Web Console CSRF boundary.
func ApproveFromTrustedDaemon(vaultDir, id, shaPrefix string) (string, error) {
	return ApproveProposal(vaultDir, id, shaPrefix)
}

// ApproveFromCLI enforces one-shot human-presence capability verification (`--cap`) for all live Vaults
// and whenever HQ_FORCE_CAP_CHECK=1 or ANTIGRAVITY_CONVERSATION_ID is set.
func ApproveFromCLI(vaultDir, id, shaPrefix, rawCap string) (string, error) {
	id = strings.TrimSpace(id)
	if err := ValidateProposalID(id); err != nil {
		return "", err
	}
	rawCap = strings.TrimSpace(rawCap)
	if rawCap == "" {
		allowCaplessTest := os.Getenv("HQ_FORCE_CAP_CHECK") == "" &&
			(store.InGoTest() || (strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")) == "" && hostpaths.IsTempPath(vaultDir)))
		if allowCaplessTest {
			return ApproveProposal(vaultDir, id, shaPrefix)
		}
		return "", fmt.Errorf("human-presence capability token (--cap) is required to approve %s; present the proposal via ask_question or approve in the :4242 Web Console", id)
	}

	capsDir := approvalCapsDir(vaultDir)
	capPath := filepath.Join(capsDir, id+".json")
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	claimedCapPath := filepath.Join(capsDir, fmt.Sprintf(".claimed-%s-%s.json", id, hex.EncodeToString(rnd[:])))
	if err := os.Rename(capPath, claimedCapPath); err != nil {
		return "", fmt.Errorf("invalid or already-consumed approval capability for %s: %w", id, err)
	}
	defer func() { _ = os.Remove(claimedCapPath) }()

	rawRec, err := os.ReadFile(claimedCapPath)
	if err != nil {
		return "", fmt.Errorf("read approval capability for %s: %w", id, err)
	}
	var capRec CapRecord
	if err := json.Unmarshal(rawRec, &capRec); err != nil {
		return "", fmt.Errorf("corrupt approval capability for %s", id)
	}
	now := time.Now()
	exp, err := time.Parse(time.RFC3339, capRec.ExpiresAt)
	if err != nil || now.After(exp) {
		return "", fmt.Errorf("approval capability for %s expired at %s", id, capRec.ExpiresAt)
	}
	gotCapHash := sha256HexString(rawCap)
	if len(gotCapHash) != len(capRec.CapSHA256) || subtle.ConstantTimeCompare([]byte(gotCapHash), []byte(capRec.CapSHA256)) != 1 {
		return "", fmt.Errorf("approval capability token mismatch for %s", id)
	}
	if capRec.ProposalID != id {
		return "", fmt.Errorf("approval capability proposal mismatch (%s != %s)", capRec.ProposalID, id)
	}
	if capRec.VaultCanon != canonicalVaultPath(vaultDir) {
		return "", fmt.Errorf("approval capability vault mismatch for %s", id)
	}

	normPrefix := strings.ToLower(strings.TrimSpace(shaPrefix))
	if !strings.HasPrefix(strings.ToLower(capRec.FullSHA256), normPrefix) {
		return "", fmt.Errorf("approval capability sha256 mismatch for %s", id)
	}

	// Verify the pending proposal file exists before committing one-shot replay marker.
	pendingPath := filepath.Join(vaultDir, "memory", "pending", id+".json")
	if _, pErr := os.Stat(pendingPath); pErr != nil {
		return "", fmt.Errorf("read proposal %s: %w", id, pErr)
	}

	usedMarker := filepath.Join(approvalAsksDir(vaultDir), ".used-"+sha256HexString(capRec.ConvID+":"+strconv.Itoa(capRec.StepIndex)+":"+id))
	uf, uErr := store.OpenCreateExclNoFollow(usedMarker, 0o600)
	if uErr != nil {
		return "", fmt.Errorf("human-presence proof replay rejected for %s (step %d already consumed)", id, capRec.StepIndex)
	}
	_, _ = uf.WriteString(now.UTC().Format(time.RFC3339) + "\n")
	_ = uf.Sync()
	_ = uf.Close()
	_ = os.Remove(filepath.Join(approvalAsksDir(vaultDir), id+".json"))

	return ApproveProposal(vaultDir, id, capRec.FullSHA256)
}

// WorkspaceExecCapRecord is a one-shot capability minted exclusively by ApproveProposal while executing an
// approved `hq workspace remove --force` or `hq workspace own` command (RFC-0025 §16.9). Stored under
// <vault>/private/approval_caps/ (blocked from all LLM tools and shell commands), it ensures direct CLI
// invocation cannot bypass PreToolUse approval gates even under shell-quoting or flag obfuscation.
type WorkspaceExecCapRecord struct {
	ProposalID  string   `json:"proposal_id"`
	ActionType  string   `json:"action_type"`
	VaultCanon  string   `json:"vault_canon"`
	Command     string   `json:"command,omitempty"`
	CommandArgv []string `json:"command_argv,omitempty"`
	TokenSHA256 string   `json:"token_sha256"`
	CreatedAt   string   `json:"created_at"`
	ExpiresAt   string   `json:"expires_at"`
}

var reValidExecCapID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{3,95}$`)

func shouldMintWorkspaceExecCap(prop Proposal) bool {
	if prop.ActionType == "workspace_remove" || prop.ActionType == "workspace_own" {
		return true
	}
	if len(prop.CommandArgv) >= 3 && strings.Contains(strings.ToLower(filepath.Base(prop.CommandArgv[0])), "hq") && prop.CommandArgv[1] == "workspace" {
		return prop.CommandArgv[2] == "remove" || prop.CommandArgv[2] == "own"
	}
	norm := normalizeCommandForRuleMatch(prop.Command)
	return strings.Contains(norm, "workspace remove ") || strings.Contains(norm, "workspace own ")
}

func mintWorkspaceExecCap(vaultDir string, prop Proposal, ttl time.Duration) (execID, rawTok, capPath string, err error) {
	if ttl <= 0 {
		ttl = 150 * time.Second
	}
	var idRand [6]byte
	var tokBytes [32]byte
	if _, err = rand.Read(idRand[:]); err != nil {
		return "", "", "", err
	}
	if _, err = rand.Read(tokBytes[:]); err != nil {
		return "", "", "", err
	}
	execID = fmt.Sprintf("%s-%s", prop.ID, hex.EncodeToString(idRand[:]))
	rawTok = hex.EncodeToString(tokBytes[:])
	now := time.Now()
	rec := WorkspaceExecCapRecord{
		ProposalID:  prop.ID,
		ActionType:  prop.ActionType,
		VaultCanon:  canonicalVaultPath(vaultDir),
		Command:     prop.Command,
		CommandArgv: append([]string(nil), prop.CommandArgv...),
		TokenSHA256: sha256HexString(rawTok),
		CreatedAt:   now.UTC().Format(time.RFC3339),
		ExpiresAt:   now.Add(ttl).UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", "", "", err
	}
	if err := os.MkdirAll(approvalCapsDir(vaultDir), 0o700); err != nil {
		return "", "", "", err
	}
	capPath = filepath.Join(approvalCapsDir(vaultDir), ".exec-"+execID+".json")
	f, err := store.OpenCreateExclNoFollow(capPath, 0o600)
	if err != nil {
		return "", "", "", err
	}
	if _, wErr := f.Write(append(data, '\n')); wErr != nil {
		_ = f.Close()
		_ = os.Remove(capPath)
		return "", "", "", wErr
	}
	_ = f.Sync()
	_ = f.Close()
	return execID, rawTok, capPath, nil
}

// VerifyAndConsumeWorkspaceExecApproval enforces binary-level human-approval proof for destructive workspace
// mutations (`workspace_remove` with `--force`, and `workspace_own`). In live Vaults or whenever
// HQ_FORCE_CAP_CHECK=1 or ANTIGRAVITY_CONVERSATION_ID is set, the command must be running inside
// ApproveProposal with a matching one-shot exec capability token minted for this exact action and key.
func VerifyAndConsumeWorkspaceExecApproval(vaultDir, actionType, key string) error {
	actionType = strings.TrimSpace(actionType)
	key = strings.TrimSpace(key)
	execID := strings.TrimSpace(os.Getenv("HQ_APPROVAL_EXEC_ID"))
	execTok := strings.TrimSpace(os.Getenv("HQ_APPROVAL_EXEC_TOKEN"))
	if execID == "" || execTok == "" {
		allowCaplessTest := os.Getenv("HQ_FORCE_CAP_CHECK") == "" &&
			(store.InGoTest() || (strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")) == "" && hostpaths.IsTempPath(vaultDir)))
		if allowCaplessTest {
			return nil
		}
		return fmt.Errorf("human-approved execution token is required for %s on %s; stage it with `hq propose --type=%s` and execute via `hq approve`", actionType, key, actionType)
	}
	if !reValidExecCapID.MatchString(execID) {
		return fmt.Errorf("invalid approval execution token id")
	}
	capsDir := approvalCapsDir(vaultDir)
	capPath := filepath.Join(capsDir, ".exec-"+execID+".json")
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	claimedPath := filepath.Join(capsDir, fmt.Sprintf(".exec-claimed-%s-%s.json", execID, hex.EncodeToString(rnd[:])))
	if err := os.Rename(capPath, claimedPath); err != nil {
		return fmt.Errorf("missing or already-consumed approval execution token for %s", actionType)
	}
	defer func() { _ = os.Remove(claimedPath) }()

	raw, err := os.ReadFile(claimedPath)
	if err != nil {
		return fmt.Errorf("read approval execution token: %w", err)
	}
	var rec WorkspaceExecCapRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("corrupt approval execution token")
	}
	now := time.Now()
	exp, err := time.Parse(time.RFC3339, rec.ExpiresAt)
	if err != nil || now.After(exp) {
		return fmt.Errorf("approval execution token expired")
	}
	gotHash := sha256HexString(execTok)
	if len(gotHash) != len(rec.TokenSHA256) || subtle.ConstantTimeCompare([]byte(gotHash), []byte(rec.TokenSHA256)) != 1 {
		return fmt.Errorf("approval execution token mismatch")
	}
	if rec.VaultCanon != canonicalVaultPath(vaultDir) {
		return fmt.Errorf("approval execution token vault mismatch")
	}
	if !workspaceExecCommandMatches(rec, actionType, key) {
		return fmt.Errorf("approval execution token does not authorize %s on %s", actionType, key)
	}
	return nil
}

func workspaceExecCommandMatches(rec WorkspaceExecCapRecord, actionType, key string) bool {
	if key == "" {
		return false
	}
	var segments [][]string
	if len(rec.CommandArgv) > 0 {
		segments = [][]string{rec.CommandArgv}
	} else if norm := normalizeCommandForRuleMatch(rec.Command); norm != "" {
		for _, seg := range strings.Split(norm, " ; ") {
			if f := strings.Fields(seg); len(f) > 0 {
				segments = append(segments, f)
			}
		}
	}
	for _, toks := range segments {
		for i := 0; i+2 < len(toks); i++ {
			if toks[i] != "workspace" {
				continue
			}
			verb := toks[i+1]
			rest := toks[i+2:]
			switch actionType {
			case "workspace_own":
				if verb == "own" && len(rest) == 1 && rest[0] == key {
					return true
				}
			case "workspace_remove":
				if verb != "remove" || len(rest) != 2 {
					continue
				}
				hasKey, hasForce := false, false
				for _, arg := range rest {
					if arg == key {
						hasKey = true
					} else if arg == "--force" || arg == "-force" || arg == "--force=true" || arg == "-force=true" || arg == "--force=1" || arg == "-force=1" {
						hasForce = true
					}
				}
				if hasKey && hasForce {
					return true
				}
			}
		}
	}
	return false
}
