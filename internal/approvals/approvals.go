package approvals

import (
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
)

// DefaultProposalTTL is the standard expiration window for pending outbound approval proposals.
const DefaultProposalTTL = 24 * time.Hour

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
	ID             string            `json:"id"`
	CreatedAt      string            `json:"createdAt"`
	ExpiresAt      string            `json:"expiresAt,omitempty"`
	ConversationID string            `json:"conversationId,omitempty"`
	ActionType     string            `json:"actionType"`
	Summary        string            `json:"summary"`
	Command        string            `json:"command"`
	WorkDir        string            `json:"workDir,omitempty"`
	SHA256         string            `json:"sha256"`
	Status         string            `json:"status"`
	RiskTier       string            `json:"riskTier,omitempty"`
	RiskFlags      []string          `json:"riskFlags,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
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
	pendingDir := filepath.Join(vaultDir, "memory", "pending")
	if err := os.MkdirAll(pendingDir, 0o700); err != nil {
		return Proposal{}, err
	}
	now := time.Now()
	id := generateID()
	expiresAt := now.Add(DefaultProposalTTL).UTC().Format(time.RFC3339)
	if meta != nil && strings.TrimSpace(meta["expiresAt"]) != "" {
		expiresAt = strings.TrimSpace(meta["expiresAt"])
	}

	var scanBuf strings.Builder
	scanBuf.WriteString(summary)
	scanBuf.WriteByte('\n')
	scanBuf.WriteString(command)
	if meta != nil {
		for _, k := range []string{"draftPayload", "bodyPreview", "subject"} {
			if v := strings.TrimSpace(meta[k]); v != "" {
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
		ID:             id,
		CreatedAt:      now.Format(time.RFC3339),
		ExpiresAt:      expiresAt,
		ConversationID: convID,
		ActionType:     actionType,
		Summary:        summary,
		Command:        command,
		WorkDir:        workDir,
		SHA256:         ComputeHash(actionType, command),
		Status:         "pending",
		RiskTier:       riskTier,
		RiskFlags:      riskFlags,
		Metadata:       meta,
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

// Approve verifies the SHA-256 prefix and TTL, executes the staged command deterministically in Go,
// removes the pending file, and appends an audit entry to local/memory/interventions.jsonl.
func Approve(vaultDir, id, shaPrefix string) (string, error) {
	return ApproveProposal(vaultDir, id, shaPrefix)
}

// ApproveProposal verifies the SHA-256 prefix and TTL, executes the staged command deterministically in Go,
// removes the pending file, and appends an audit entry to local/memory/interventions.jsonl.
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
	if prop.IsExpired(time.Now()) {
		_ = os.Remove(pendingPath)
		prop.Status = "expired"
		appendIntervention(vaultDir, InterventionRecord{
			Timestamp: time.Now().Format(time.RFC3339),
			Decision:  "expired",
			Proposal:  prop,
			Error:     "attempted approval after TTL expiration",
		})
		return "", fmt.Errorf("proposal %s expired at %s; re-stage with ./bin/hq propose", id, prop.ExpiresAt)
	}
	expected := ComputeHash(prop.ActionType, prop.Command)
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

	cmd := exec.Command("sh", "-c", prop.Command)
	if prop.WorkDir != "" {
		cmd.Dir = prop.WorkDir
	}
	outBytes, execErr := cmd.CombinedOutput()
	output := string(outBytes)

	prop.Status = "approved"

	rec := InterventionRecord{
		Timestamp: time.Now().Format(time.RFC3339),
		Decision:  "approved",
		Proposal:  prop,
		Output:    output,
	}
	if execErr != nil {
		rec.Error = execErr.Error()
	}
	appendIntervention(vaultDir, rec)
	return output, execErr
}

// Reject removes the proposal from pending/ and logs the rejection to interventions.jsonl.
func Reject(vaultDir, id, reason string) error {
	return RejectProposal(vaultDir, id, reason)
}

// RejectProposal removes the proposal from pending/ and logs the rejection to interventions.jsonl.
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
	prop.Status = "rejected"
	_ = os.Remove(pendingPath)

	rec := InterventionRecord{
		Timestamp: time.Now().Format(time.RFC3339),
		Decision:  "rejected",
		Proposal:  prop,
		Output:    reason,
	}
	appendIntervention(vaultDir, rec)
	return nil
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
