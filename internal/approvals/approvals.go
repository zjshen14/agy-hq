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

var (
	reValidProposalID = regexp.MustCompile(`^prop-[a-z0-9][a-z0-9\-]{3,63}$`)
	reValidSHAPrefix  = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
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
	ConversationID string            `json:"conversationId,omitempty"`
	ActionType     string            `json:"actionType"`
	Summary        string            `json:"summary"`
	Command        string            `json:"command"`
	WorkDir        string            `json:"workDir,omitempty"`
	SHA256         string            `json:"sha256"`
	Status         string            `json:"status"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// InterventionRecord logs an approved or rejected proposal in local/memory/interventions.jsonl.
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

// Create stages a new pending proposal in local/memory/pending/<id>.json.
func Create(vaultDir, actionType, summary, command, workDir, convID string) (Proposal, error) {
	return CreateWithMetadata(vaultDir, actionType, summary, command, workDir, convID, nil)
}

// CreateWithMetadata stages a new pending proposal with optional structured metadata (e.g., Gmail draft fields).
func CreateWithMetadata(vaultDir, actionType, summary, command, workDir, convID string, meta map[string]string) (Proposal, error) {
	pendingDir := filepath.Join(vaultDir, "memory", "pending")
	if err := os.MkdirAll(pendingDir, 0o700); err != nil {
		return Proposal{}, err
	}
	id := generateID()
	prop := Proposal{
		ID:             id,
		CreatedAt:      time.Now().Format(time.RFC3339),
		ConversationID: convID,
		ActionType:     actionType,
		Summary:        summary,
		Command:        command,
		WorkDir:        workDir,
		SHA256:         ComputeHash(actionType, command),
		Status:         "pending",
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

// List returns all pending proposals sorted by creation time.
func List(vaultDir string) ([]Proposal, error) {
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

// Approve verifies the SHA-256 prefix, executes the staged command deterministically in Go,
// removes the pending file, and appends an audit entry to local/memory/interventions.jsonl.
func Approve(vaultDir, id, shaPrefix string) (string, error) {
	return ApproveProposal(vaultDir, id, shaPrefix)
}

// ApproveProposal verifies the SHA-256 prefix, executes the staged command deterministically in Go,
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
