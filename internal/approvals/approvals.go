package approvals

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/events"
)

// DefaultProposalTTL is the standard expiration window for pending outbound approval proposals.
const DefaultProposalTTL = 24 * time.Hour

const DefaultRejectReason = "Rejected by user"

var (
	reValidProposalID  = regexp.MustCompile(`^prop-[a-z0-9][a-z0-9\-]{3,63}$`)
	reValidSHAPrefix   = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
	reOutboundSSN      = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	reOutboundFinAcct  = regexp.MustCompile(`(?i)\b(?:acct|account|routing|iban)\s*[:#]?\s*[0-9]{6,}\b`)
	reOutboundCardNum  = regexp.MustCompile(`\b(?:\d[ -]?){13,16}\b`)
	reOutboundStreetAd = regexp.MustCompile(`\b\d{2,5}\s+[A-Z][a-zA-Z0-9.\s]{2,28}\s+(?:Street|St|Avenue|Ave|Road|Rd|Boulevard|Blvd|Drive|Dr|Lane|Ln|Court|Ct|Way)\b`)
)

// ValidateProposalID rejects path traversal, quotes, or malformed proposal identifiers.
func ValidateProposalID(id string) error {
	trimmed := strings.TrimSpace(id)
	if !reValidProposalID.MatchString(trimmed) {
		return fmt.Errorf("invalid proposal id %q (expected prop-<ts>-<hex>)", id)
	}
	return nil
}

// Proposal represents an approval-bound action staged in local/memory/pending/<id>.json.
type Proposal struct {
	ID               string            `json:"id"`
	CreatedAt        string            `json:"createdAt"`
	ExpiresAt        string            `json:"expiresAt,omitempty"`
	ConversationID   string            `json:"conversationId,omitempty"`
	ActionType       string            `json:"actionType"`
	Summary          string            `json:"summary"`
	Command          string            `json:"command"`
	CommandArgv      []string          `json:"commandArgv,omitempty"`
	WorkDir          string            `json:"workDir,omitempty"`
	SHA256           string            `json:"sha256"`
	Status           string            `json:"status"`
	RiskTier         string            `json:"riskTier,omitempty"`
	RiskFlags        []string          `json:"riskFlags,omitempty"`
	ResumeOnDecide   bool              `json:"resumeOnDecide,omitempty"`
	ContinuationNote string            `json:"continuationNote,omitempty"`
	OriginSkill      string            `json:"originSkill,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// CreateOptions configures structured argv, deferred session resumption, and skill attribution.
type CreateOptions struct {
	ActionType       string
	Summary          string
	Command          string
	CommandArgv      []string
	WorkDir          string
	ConversationID   string
	ResumeOnDecide   bool
	ContinuationNote string
	OriginSkill      string
	Metadata         map[string]string
}

// IsExpired returns true if ExpiresAt is set and strictly in the past relative to now.
// Legacy or demo proposals with empty ExpiresAt do not expire automatically.
func (p Proposal) IsExpired(now time.Time) bool {
	exp := strings.TrimSpace(p.ExpiresAt)
	if exp == "" {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	t, err := time.Parse(time.RFC3339Nano, exp)
	if err != nil {
		t, err = time.Parse(time.RFC3339, exp)
		if err != nil {
			return false
		}
	}
	return now.After(t)
}

// InterventionRecord logs an approved, rejected, or expired proposal in local/memory/interventions.jsonl.
type InterventionRecord struct {
	Timestamp string   `json:"timestamp"`
	Decision  string   `json:"decision"`
	Proposal  Proposal `json:"proposal"`
	Output    string   `json:"output,omitempty"`
	Error     string   `json:"error,omitempty"`
}

func generateID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("prop-%d-%s", time.Now().Unix(), hex.EncodeToString(b))
}

// ComputeHash computes the deterministic SHA-256 checksum over actionType and command.
func ComputeHash(actionType, command string) string {
	sum := sha256.Sum256([]byte(actionType + "\n" + command))
	return hex.EncodeToString(sum[:])
}

// ComputeProposalHash binds CommandArgv canonically when populated, falling back to ComputeHash for string commands.
func ComputeProposalHash(actionType, command string, argv []string) string {
	if len(argv) == 0 {
		return ComputeHash(actionType, command)
	}
	canonicalArgv, _ := json.Marshal(argv)
	sum := sha256.Sum256([]byte(actionType + "\nargv:" + string(canonicalArgv)))
	return hex.EncodeToString(sum[:])
}

// ScanOutboundContextualPII deterministically checks outbound draft/command payloads against
// local/private/pii-denylist.txt and high-sensitivity PII patterns (SSN, street address,
// financial account numbers, private Vault paths) so warning badges are surfaced before human approval.
func ScanOutboundContextualPII(vaultDir, text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	seen := make(map[string]bool)
	var flags []string
	addFlag := func(f string) {
		if !seen[f] {
			seen[f] = true
			flags = append(flags, f)
		}
	}

	if vaultDir != "" {
		denylistPath := filepath.Join(vaultDir, "private", "pii-denylist.txt")
		if raw, err := os.ReadFile(denylistPath); err == nil {
			lowerText := strings.ToLower(text)
			for _, line := range strings.Split(string(raw), "\n") {
				term := strings.TrimSpace(line)
				if term == "" || strings.HasPrefix(term, "#") || len(term) < 3 {
					continue
				}
				if strings.Contains(lowerText, strings.ToLower(term)) {
					addFlag("denylist_pii_match")
					break
				}
			}
		}
	}
	if reOutboundSSN.MatchString(text) {
		addFlag("ssn_pattern")
	}
	if reOutboundFinAcct.MatchString(text) || reOutboundCardNum.MatchString(text) {
		addFlag("financial_account_number")
	}
	if reOutboundStreetAd.MatchString(text) {
		addFlag("street_address")
	}
	if strings.Contains(text, "local/private/") {
		addFlag("private_vault_reference")
	}
	return flags
}

// Create stages a new pending proposal in local/memory/pending/<id>.json.
func Create(vaultDir, actionType, summary, command, workDir, convID string) (Proposal, error) {
	return CreateWithMetadata(vaultDir, actionType, summary, command, workDir, convID, nil)
}

// CreateWithMetadata stages a new pending proposal with optional structured metadata, 24h TTL,
// and automatic outbound contextual-PII warning inspection.
func CreateWithMetadata(vaultDir, actionType, summary, command, workDir, convID string, meta map[string]string) (Proposal, error) {
	return CreateWithOptions(vaultDir, CreateOptions{
		ActionType:     actionType,
		Summary:        summary,
		Command:        command,
		WorkDir:        workDir,
		ConversationID: convID,
		Metadata:       meta,
	})
}

// CreateWithOptions stages a pending proposal supporting structured CommandArgv, deferred resumption, and skill attribution.
func CreateWithOptions(vaultDir string, opts CreateOptions) (Proposal, error) {
	pendingDir := filepath.Join(vaultDir, "memory", "pending")
	if err := os.MkdirAll(pendingDir, 0o700); err != nil {
		return Proposal{}, err
	}
	now := time.Now()
	id := generateID()
	expiresAt := now.Add(DefaultProposalTTL).UTC().Format(time.RFC3339)
	if opts.Metadata != nil && strings.TrimSpace(opts.Metadata["expiresAt"]) != "" {
		expiresAt = strings.TrimSpace(opts.Metadata["expiresAt"])
	}
	cmdDisplay := strings.TrimSpace(opts.Command)
	if cmdDisplay == "" && len(opts.CommandArgv) > 0 {
		cmdDisplay = strings.Join(opts.CommandArgv, " ")
	}
	originSkill := strings.TrimSpace(opts.OriginSkill)
	if originSkill == "" && opts.Metadata != nil {
		originSkill = strings.TrimSpace(opts.Metadata["originSkill"])
	}

	var scanBuf strings.Builder
	scanBuf.WriteString(opts.Summary)
	scanBuf.WriteByte('\n')
	scanBuf.WriteString(cmdDisplay)
	if len(opts.CommandArgv) > 0 {
		scanBuf.WriteByte('\n')
		scanBuf.WriteString(strings.Join(opts.CommandArgv, " "))
	}
	if opts.Metadata != nil {
		for _, k := range []string{"draftPayload", "bodyPreview", "subject"} {
			if v := strings.TrimSpace(opts.Metadata[k]); v != "" {
				scanBuf.WriteByte('\n')
				scanBuf.WriteString(v)
			}
		}
	}
	riskFlags := ScanOutboundContextualPII(vaultDir, scanBuf.String())
	riskTier := "standard"
	if len(riskFlags) > 0 {
		riskTier = "pii_warning"
	}

	prop := Proposal{
		ID:               id,
		CreatedAt:        now.Format(time.RFC3339),
		ExpiresAt:        expiresAt,
		ConversationID:   strings.TrimSpace(opts.ConversationID),
		ActionType:       strings.TrimSpace(opts.ActionType),
		Summary:          strings.TrimSpace(opts.Summary),
		Command:          cmdDisplay,
		CommandArgv:      opts.CommandArgv,
		WorkDir:          strings.TrimSpace(opts.WorkDir),
		SHA256:           ComputeProposalHash(strings.TrimSpace(opts.ActionType), cmdDisplay, opts.CommandArgv),
		Status:           "pending",
		RiskTier:         riskTier,
		RiskFlags:        riskFlags,
		ResumeOnDecide:   opts.ResumeOnDecide,
		ContinuationNote: strings.TrimSpace(opts.ContinuationNote),
		OriginSkill:      originSkill,
		Metadata:         opts.Metadata,
	}
	data, err := json.MarshalIndent(prop, "", "  ")
	if err != nil {
		return Proposal{}, err
	}
	dst := filepath.Join(pendingDir, id+".json")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return Proposal{}, err
	}
	return prop, nil
}

// ExpireStaleProposals sweeps local/memory/pending/*.json for proposals past ExpiresAt,
// removing them from the pending queue and recording an `"expired"` audit record in interventions.jsonl.
func ExpireStaleProposals(vaultDir string, now time.Time) ([]Proposal, error) {
	if now.IsZero() {
		now = time.Now()
	}
	pendingDir := filepath.Join(vaultDir, "memory", "pending")
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var expired []Proposal
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		fileID := strings.TrimSuffix(e.Name(), ".json")
		if ValidateProposalID(fileID) != nil {
			continue
		}
		fullPath := filepath.Join(pendingDir, e.Name())
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		var p Proposal
		if json.Unmarshal(data, &p) != nil || p.ID != fileID {
			continue
		}
		if p.IsExpired(now) {
			if os.Remove(fullPath) == nil {
				p.Status = "expired"
				appendIntervention(vaultDir, InterventionRecord{
					Timestamp: now.Format(time.RFC3339),
					Decision:  "expired",
					Proposal:  p,
					Output:    fmt.Sprintf("Proposal auto-expired at %s (TTL exceeded)", p.ExpiresAt),
				})
				expired = append(expired, p)
			}
		}
	}
	return expired, nil
}

// List returns all active (non-expired) pending proposals sorted by creation time.
func List(vaultDir string) ([]Proposal, error) {
	_, _ = ExpireStaleProposals(vaultDir, time.Now())
	pendingDir := filepath.Join(vaultDir, "memory", "pending")
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Proposal
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		fileID := strings.TrimSuffix(e.Name(), ".json")
		if ValidateProposalID(fileID) != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(pendingDir, e.Name()))
		if err != nil {
			continue
		}
		var p Proposal
		if err := json.Unmarshal(data, &p); err == nil {
			if ValidateProposalID(p.ID) != nil || p.ID != fileID {
				continue
			}
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out, nil
}

// SanitizedApprovalEnv builds a scrubbed environment allowlist for executing approved actions,
// preventing ANTIGRAVITY_CSRF_TOKEN or unrelated developer secrets from leaking into child processes.
func SanitizedApprovalEnv(vaultDir string) []string {
	allowedPrefixes := []string{
		"HOME=",
		"USER=",
		"LOGNAME=",
		"SHELL=",
		"LANG=",
		"LC_",
		"TZ=",
		"TERM=",
		"TMPDIR=",
		"SSH_AUTH_SOCK=",
		"DISPLAY=",
		"XDG_RUNTIME_DIR=",
		"XDG_CONFIG_HOME=",
		"XDG_CACHE_HOME=",
		"HQ_ROOT_DIR=",
		"AGY_ACCOUNT=",
	}
	var env []string
	for _, kv := range os.Environ() {
		for _, pref := range allowedPrefixes {
			if strings.HasPrefix(kv, pref) {
				env = append(env, kv)
				break
			}
		}
	}

	pathVal := os.Getenv("PATH")
	var extraDirs []string
	if exe, eErr := os.Executable(); eErr == nil && exe != "" {
		extraDirs = append(extraDirs, filepath.Dir(exe))
	}
	if home, hErr := os.UserHomeDir(); hErr == nil && home != "" {
		extraDirs = append(extraDirs, filepath.Join(home, ".local", "bin"))
	}
	if len(extraDirs) > 0 {
		env = append(env, "PATH="+strings.Join(extraDirs, string(os.PathListSeparator))+string(os.PathListSeparator)+pathVal)
	} else if pathVal != "" {
		env = append(env, "PATH="+pathVal)
	}

	if v := strings.TrimSpace(os.Getenv("HQ_VAULT_DIR")); v != "" {
		env = append(env, "HQ_VAULT_DIR="+v)
	} else if strings.TrimSpace(vaultDir) != "" {
		env = append(env, "HQ_VAULT_DIR="+vaultDir)
	}
	return env
}

// Approve verifies the SHA-256 prefix and TTL, executes the staged command deterministically in Go,
// removes the pending file, and appends an audit entry to local/memory/interventions.jsonl.
func Approve(vaultDir, id, shaPrefix string) (string, error) {
	return ApproveProposal(vaultDir, id, shaPrefix)
}

// ApproveProposal verifies the SHA-256 prefix and TTL, executes the staged command deterministically in Go
// (using CommandArgv without sh -c when available and a scrubbed environment), logs to interventions.jsonl,
// and enqueues a continuation event if ResumeOnDecide is enabled.
func ApproveProposal(vaultDir, id, shaPrefix string) (string, error) {
	id = strings.TrimSpace(id)
	if err := ValidateProposalID(id); err != nil {
		return "", err
	}
	normPrefix := strings.ToLower(strings.TrimSpace(shaPrefix))
	if !reValidSHAPrefix.MatchString(normPrefix) {
		return "", fmt.Errorf("sha256 prefix required (minimum 8 lowercase hex characters, got %q)", shaPrefix)
	}

	pendingDir := filepath.Join(vaultDir, "memory", "pending")
	pendingPath := filepath.Join(pendingDir, id+".json")
	data, err := os.ReadFile(pendingPath)
	if err != nil {
		return "", fmt.Errorf("read proposal %s: %w", id, err)
	}
	var prop Proposal
	if err := json.Unmarshal(data, &prop); err != nil {
		return "", err
	}
	if prop.ID != id {
		return "", fmt.Errorf("proposal ID mismatch: file=%s json=%s", id, prop.ID)
	}
	now := time.Now()
	if prop.IsExpired(now) {
		_ = os.Remove(pendingPath)
		prop.Status = "expired"
		appendIntervention(vaultDir, InterventionRecord{
			Timestamp: now.Format(time.RFC3339),
			Decision:  "expired",
			Proposal:  prop,
			Error:     "attempted approval after TTL expiration",
		})
		return "", fmt.Errorf("proposal %s expired at %s; re-stage with ./bin/hq propose", id, prop.ExpiresAt)
	}
	expected := ComputeProposalHash(prop.ActionType, prop.Command, prop.CommandArgv)
	if prop.SHA256 != expected {
		return "", fmt.Errorf("tampered proposal: stored sha256 %s != computed %s", prop.SHA256, expected)
	}
	if !strings.HasPrefix(strings.ToLower(prop.SHA256), normPrefix) {
		return "", fmt.Errorf("sha256 prefix mismatch: got %s, expected prefix of %s", normPrefix, prop.SHA256)
	}

	// Atomically claim the pending file prior to execution to prevent concurrent double-approval races.
	claimPath := filepath.Join(pendingDir, ".executing-"+id+".json")
	if err := os.Rename(pendingPath, claimPath); err != nil {
		return "", fmt.Errorf("claim proposal %s (already approved or removed): %w", id, err)
	}
	defer func() { _ = os.Remove(claimPath) }()

	var cmd *exec.Cmd
	if len(prop.CommandArgv) > 0 && strings.TrimSpace(prop.CommandArgv[0]) != "" {
		cmd = exec.Command(prop.CommandArgv[0], prop.CommandArgv[1:]...)
	} else {
		cmd = exec.Command("sh", "-c", prop.Command)
	}
	if prop.WorkDir != "" {
		cmd.Dir = prop.WorkDir
	}
	cmd.Env = SanitizedApprovalEnv(vaultDir)

	outBytes, execErr := cmd.CombinedOutput()
	output := string(outBytes)

	prop.Status = "approved"
	rec := InterventionRecord{
		Timestamp: now.Format(time.RFC3339),
		Decision:  "approved",
		Proposal:  prop,
		Output:    output,
	}
	if execErr != nil {
		rec.Error = execErr.Error()
	}
	appendIntervention(vaultDir, rec)

	if prop.ResumeOnDecide && strings.TrimSpace(prop.ConversationID) != "" {
		enqueueApprovalDecisionEvent(vaultDir, prop, "approved", output, execErr, now)
	}
	return output, execErr
}

// Reject removes the proposal from pending/ and logs the rejection to interventions.jsonl.
func Reject(vaultDir, id, reason string) error {
	return RejectProposal(vaultDir, id, reason)
}

// RejectProposal removes the proposal from pending/, logs the rejection to interventions.jsonl,
// and enqueues a steering resumption event to prop.ConversationID when the user provides feedback or ResumeOnDecide is set.
func RejectProposal(vaultDir, id, reason string) error {
	id = strings.TrimSpace(id)
	if err := ValidateProposalID(id); err != nil {
		return err
	}
	pendingPath := filepath.Join(vaultDir, "memory", "pending", id+".json")
	data, err := os.ReadFile(pendingPath)
	if err != nil {
		return fmt.Errorf("read proposal %s: %w", id, err)
	}
	var prop Proposal
	if err := json.Unmarshal(data, &prop); err != nil {
		return err
	}
	now := time.Now()
	prop.Status = "rejected"
	_ = os.Remove(pendingPath)

	rec := InterventionRecord{
		Timestamp: now.Format(time.RFC3339),
		Decision:  "rejected",
		Proposal:  prop,
		Output:    reason,
	}
	appendIntervention(vaultDir, rec)

	trimmedReason := strings.TrimSpace(reason)
	hasCustomSteering := trimmedReason != "" && !strings.EqualFold(trimmedReason, DefaultRejectReason)
	if strings.TrimSpace(prop.ConversationID) != "" && (prop.ResumeOnDecide || hasCustomSteering) {
		enqueueApprovalDecisionEvent(vaultDir, prop, "rejected", trimmedReason, nil, now)
	}
	return nil
}

func enqueueApprovalDecisionEvent(vaultDir string, prop Proposal, decision, detail string, execErr error, now time.Time) {
	convID := strings.TrimSpace(prop.ConversationID)
	if convID == "" {
		return
	}
	snippet := strings.TrimSpace(detail)
	if runes := []rune(snippet); len(runes) > 600 {
		snippet = string(runes[:600]) + "..."
	}
	var prompt strings.Builder
	if decision == "approved" {
		prompt.WriteString(fmt.Sprintf("[Deferred Proposal Approved & Executed: `%s` (%s)]\n", prop.ID, prop.ActionType))
		prompt.WriteString(fmt.Sprintf("- Summary: %s\n", prop.Summary))
		if execErr != nil {
			prompt.WriteString(fmt.Sprintf("- Execution Error: %v\n", execErr))
		} else {
			prompt.WriteString("- Execution Status: succeeded\n")
		}
		if snippet != "" {
			prompt.WriteString(fmt.Sprintf("- Output:\n```\n%s\n```\n", snippet))
		}
		if prop.ContinuationNote != "" {
			prompt.WriteString(fmt.Sprintf("- Next Continuation Step: %s\n", prop.ContinuationNote))
		}
	} else {
		prompt.WriteString(fmt.Sprintf("[Deferred Proposal Rejected by User: `%s` (%s)]\n", prop.ID, prop.ActionType))
		prompt.WriteString(fmt.Sprintf("- Original Summary: %s\n", prop.Summary))
		if snippet != "" {
			prompt.WriteString(fmt.Sprintf("- User Steering / Rejection Feedback: %q\n", snippet))
			prompt.WriteString("\nRevise the draft/action according to the user's feedback above and stage an updated proposal via `hq propose` if still appropriate.")
		} else if prop.ContinuationNote != "" {
			prompt.WriteString(fmt.Sprintf("- Continuation Note: %s\n", prop.ContinuationNote))
		}
	}

	decisionLabel := "Rejected"
	if decision == "approved" {
		decisionLabel = "Approved"
	}
	_, _, _ = events.Enqueue(vaultDir, events.Event{
		Source:    "approval_decided",
		DedupKey:  fmt.Sprintf("approval_decided:%s:%s", prop.ID, decision),
		ScopeKey:  "session:" + convID,
		SessionID: convID,
		Skill:     prop.OriginSkill,
		Title:     fmt.Sprintf("[Approval %s] %s", decisionLabel, prop.ID),
		Prompt:    strings.TrimSpace(prompt.String()),
		Metadata: map[string]string{
			"proposal_id": prop.ID,
			"decision":    decision,
			"action_type": prop.ActionType,
		},
	}, now)
}

func appendIntervention(vaultDir string, rec InterventionRecord) {
	logPath := filepath.Join(vaultDir, "memory", "interventions.jsonl")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o700)
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// LoadInterventions reads intervention audit records from local/memory/interventions.jsonl.
func LoadInterventions(vaultDir string) ([]InterventionRecord, error) {
	p := filepath.Join(vaultDir, "memory", "interventions.jsonl")
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []InterventionRecord
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec InterventionRecord
		if json.Unmarshal([]byte(line), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out, scanner.Err()
}
