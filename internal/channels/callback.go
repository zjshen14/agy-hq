package channels

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/store"
)

const (
	ApproveTokenPrefix = "bca_"
	RejectTokenPrefix  = "bcr_"
)

// Exact 36-byte compact token format: "bca_" or "bcr_" + 32 base64url chars (192 bits CSPRNG entropy),
// fitting comfortably inside Telegram's 64-byte InlineKeyboardButton.callback_data limit.
var reValidCallbackToken = regexp.MustCompile(`^bc[ar]_[A-Za-z0-9_-]{32}$`)

// BridgeCapRecord persists ONLY SHA-256 hashes of one-shot button tokens in <vault>/private/approval_bridge_caps/<token-sha256>.json.
// Plaintext `bca_...` / `bcr_...` tokens are NEVER written to disk.
type BridgeCapRecord struct {
	ProposalID string `json:"proposal_id"`
	BridgeID   string `json:"bridge_id"`
	Decision   string `json:"decision"` // "approve" or "reject"
	FullSHA256 string `json:"full_sha256"`
	TokenHash  string `json:"token_hash"`
	PairHash   string `json:"pair_hash,omitempty"`
	VaultCanon string `json:"vault_canon"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at"`
}

// CallbackRequest is sent by a trusted companion sidecar to POST /api/channels/callback or `hq channel callback`.
type CallbackRequest struct {
	BridgeID       string `json:"bridge_id"`
	CallbackToken  string `json:"callback_token"`
	Reason         string `json:"reason,omitempty"`
	SuppressResume bool   `json:"suppress_resume,omitempty"`
}

// CallbackResult summarizes 0-token button callback execution.
type CallbackResult struct {
	OK         bool   `json:"ok"`
	BridgeID   string `json:"bridge_id"`
	ProposalID string `json:"proposal_id"`
	Decision   string `json:"decision"`
	Output     string `json:"output,omitempty"`
}

func bridgeApprovalCapsDir(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "approval_bridge_caps")
}

func canonicalVaultDir(vaultDir string) string {
	abs, err := filepath.Abs(vaultDir)
	if err != nil {
		return filepath.Clean(vaultDir)
	}
	if real, rErr := filepath.EvalSymlinks(abs); rErr == nil && real != "" {
		return filepath.Clean(real)
	}
	return filepath.Clean(abs)
}

func generateCallbackToken(prefix string) (string, string, error) {
	var raw [24]byte // 192 bits -> 32 base64url chars without padding
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	tok := prefix + base64.RawURLEncoding.EncodeToString(raw[:])
	return tok, sha256HexBytes([]byte(tok)), nil
}

// MintProposalActionTokens mints single-use 36-byte `bca_...` / `bcr_...` button tokens for each unexpired
// pending proposal in proposalIDs, writing only SHA-256(token) records to <vault>/private/approval_bridge_caps/.
func MintProposalActionTokens(vaultDir, bridgeFQID string, proposalIDs []string, now time.Time) ([]NotificationAction, error) {
	if len(proposalIDs) == 0 {
		return nil, nil
	}
	if _, _, err := ParseBridgeFQID(bridgeFQID); err != nil {
		return nil, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	capsDir := bridgeApprovalCapsDir(vaultDir)
	if st, lErr := os.Lstat(capsDir); lErr == nil && st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing symlinked private/approval_bridge_caps directory")
	}
	if err := os.MkdirAll(capsDir, 0o700); err != nil {
		return nil, err
	}

	vaultCanon := canonicalVaultDir(vaultDir)
	seen := make(map[string]bool)
	var actions []NotificationAction

	for _, rawID := range proposalIDs {
		propID := strings.TrimSpace(rawID)
		if propID == "" || seen[propID] {
			continue
		}
		seen[propID] = true
		if approvals.ValidateProposalID(propID) != nil {
			continue
		}
		pendingPath := filepath.Join(vaultDir, "memory", "pending", propID+".json")
		rawProp, err := os.ReadFile(pendingPath)
		if err != nil {
			continue
		}
		var prop approvals.Proposal
		if json.Unmarshal(rawProp, &prop) != nil || prop.ID != propID || prop.IsExpired(now) {
			continue
		}
		if approvals.VerifyProposalHash(prop) != nil {
			continue
		}

		expTime := now.Add(approvals.DefaultAskTTL)
		if pExp, pErr := time.Parse(time.RFC3339, prop.ExpiresAt); pErr == nil && pExp.Before(expTime) {
			expTime = pExp
		}
		nowStr := now.UTC().Format(time.RFC3339)
		expStr := expTime.UTC().Format(time.RFC3339)
		fullSHA := strings.ToLower(strings.TrimSpace(prop.SHA256))
		shaPrefix := fullSHA
		if len(shaPrefix) > 12 {
			shaPrefix = shaPrefix[:12]
		}

		appTok, appHash, err := generateCallbackToken(ApproveTokenPrefix)
		if err != nil {
			return nil, err
		}
		rejTok, rejHash, err := generateCallbackToken(RejectTokenPrefix)
		if err != nil {
			return nil, err
		}

		appRec := BridgeCapRecord{
			ProposalID: propID,
			BridgeID:   bridgeFQID,
			Decision:   "approve",
			FullSHA256: fullSHA,
			TokenHash:  appHash,
			PairHash:   rejHash,
			VaultCanon: vaultCanon,
			CreatedAt:  nowStr,
			ExpiresAt:  expStr,
		}
		rejRec := BridgeCapRecord{
			ProposalID: propID,
			BridgeID:   bridgeFQID,
			Decision:   "reject",
			FullSHA256: fullSHA,
			TokenHash:  rejHash,
			PairHash:   appHash,
			VaultCanon: vaultCanon,
			CreatedAt:  nowStr,
			ExpiresAt:  expStr,
		}

		appBytes, err := json.MarshalIndent(appRec, "", "  ")
		if err != nil {
			return nil, err
		}
		rejBytes, err := json.MarshalIndent(rejRec, "", "  ")
		if err != nil {
			return nil, err
		}

		appPath := filepath.Join(capsDir, appHash+".json")
		rejPath := filepath.Join(capsDir, rejHash+".json")
		if err := store.WriteAtomic(appPath, append(appBytes, '\n'), 0o600); err != nil {
			return nil, err
		}
		if err := store.WriteAtomic(rejPath, append(rejBytes, '\n'), 0o600); err != nil {
			_ = os.Remove(appPath)
			return nil, err
		}

		actions = append(actions, NotificationAction{
			ProposalID:           propID,
			ActionType:           prop.ActionType,
			Summary:              truncateUTF8Runes(prop.Summary, 160),
			SHA256Prefix:         shaPrefix,
			ApproveCallbackToken: appTok,
			RejectCallbackToken:  rejTok,
			ExpiresAt:            expStr,
		})
	}
	return actions, nil
}

// HandleCallback verifies a single-use `bca_...` / `bcr_...` button token from a trusted `callback_buttons` bridge
// via atomic `os.Rename`, invalidates its sibling pair token, and executes 0-token ApproveFromTrustedDaemon / RejectProposalWithOptions.
func HandleCallback(ctx context.Context, vaultDir string, req CallbackRequest, now time.Time) (CallbackResult, error) {
	start := time.Now()
	if now.IsZero() {
		now = start
	}
	fqid := strings.TrimSpace(req.BridgeID)
	if _, _, err := ParseBridgeFQID(fqid); err != nil {
		return CallbackResult{}, err
	}
	tok := strings.TrimSpace(req.CallbackToken)
	if !reValidCallbackToken.MatchString(tok) {
		return CallbackResult{}, fmt.Errorf("invalid callback_token format")
	}

	rb, err := requireActiveTrustedBridge(vaultDir, fqid)
	if err != nil {
		return CallbackResult{}, err
	}
	if !rb.Spec.Capabilities.CallbackButtons {
		return CallbackResult{}, fmt.Errorf("channel bridge %s does not declare capabilities.callback_buttons=true", fqid)
	}

	tokHash := sha256HexBytes([]byte(tok))
	capsDir := bridgeApprovalCapsDir(vaultDir)
	if st, lErr := os.Lstat(capsDir); lErr == nil && st.Mode()&os.ModeSymlink != 0 {
		return CallbackResult{}, fmt.Errorf("refusing symlinked private/approval_bridge_caps directory")
	}
	capPath := filepath.Join(capsDir, tokHash+".json")
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	claimedPath := filepath.Join(capsDir, fmt.Sprintf(".claimed-%s-%s.json", tokHash[:16], hex.EncodeToString(rnd[:])))
	if err := os.Rename(capPath, claimedPath); err != nil {
		return CallbackResult{}, fmt.Errorf("invalid, expired, or already-consumed callback token")
	}
	defer func() { _ = os.Remove(claimedPath) }()

	rawRec, err := os.ReadFile(claimedPath)
	if err != nil {
		return CallbackResult{}, fmt.Errorf("read callback capability: %w", err)
	}
	var rec BridgeCapRecord
	if err := json.Unmarshal(rawRec, &rec); err != nil {
		return CallbackResult{}, fmt.Errorf("corrupt callback capability")
	}
	// Immediately invalidate the sibling button token so Approve and Reject can never both fire.
	if pair := strings.TrimSpace(rec.PairHash); len(pair) == 64 {
		_ = os.Remove(filepath.Join(capsDir, pair+".json"))
	}

	if len(rec.TokenHash) != len(tokHash) || subtle.ConstantTimeCompare([]byte(rec.TokenHash), []byte(tokHash)) != 1 {
		return CallbackResult{}, fmt.Errorf("callback token hash mismatch")
	}
	if rec.BridgeID != fqid {
		return CallbackResult{}, fmt.Errorf("callback token bridge mismatch (%s != %s)", rec.BridgeID, fqid)
	}
	if rec.VaultCanon != canonicalVaultDir(vaultDir) {
		return CallbackResult{}, fmt.Errorf("callback token vault mismatch")
	}
	exp, err := time.Parse(time.RFC3339, rec.ExpiresAt)
	if err != nil || now.After(exp) {
		return CallbackResult{}, fmt.Errorf("callback token for %s expired at %s", rec.ProposalID, rec.ExpiresAt)
	}
	if err := approvals.ValidateProposalID(rec.ProposalID); err != nil {
		return CallbackResult{}, err
	}

	var outStr string
	var execErr error
	auditKind := "callback_approve"

	switch rec.Decision {
	case "approve":
		if strings.HasPrefix(tok, RejectTokenPrefix) {
			return CallbackResult{}, fmt.Errorf("callback token decision prefix mismatch")
		}
		outStr, execErr = approvals.ApproveFromTrustedDaemon(vaultDir, rec.ProposalID, rec.FullSHA256)
	case "reject":
		auditKind = "callback_reject"
		if strings.HasPrefix(tok, ApproveTokenPrefix) {
			return CallbackResult{}, fmt.Errorf("callback token decision prefix mismatch")
		}
		// Verify the pending proposal hash matches before rejecting so superseded proposals fail cleanly.
		pendingPath := filepath.Join(vaultDir, "memory", "pending", rec.ProposalID+".json")
		pData, pErr := os.ReadFile(pendingPath)
		if pErr != nil {
			execErr = fmt.Errorf("read pending proposal %s: %w", rec.ProposalID, pErr)
		} else {
			var prop approvals.Proposal
			if uErr := json.Unmarshal(pData, &prop); uErr != nil || !strings.EqualFold(strings.TrimSpace(prop.SHA256), rec.FullSHA256) {
				execErr = fmt.Errorf("proposal %s hash changed or invalid", rec.ProposalID)
			} else {
				reason := strings.TrimSpace(req.Reason)
				if reason == "" {
					reason = "Rejected via channel bridge button (" + fqid + ")"
				}
				execErr = approvals.RejectProposalWithOptions(vaultDir, rec.ProposalID, approvals.RejectOptions{
					Reason:         reason,
					SuppressResume: req.SuppressResume,
				})
				if execErr == nil {
					outStr = "rejected"
				}
			}
		}
	default:
		return CallbackResult{}, fmt.Errorf("unsupported callback decision %q", rec.Decision)
	}

	outcome := "delivered"
	errCode := ""
	if execErr != nil {
		outcome = "failed"
		errCode = "execution_error"
	}
	_ = WithChannelsLock(ctx, vaultDir, func(_ context.Context) error {
		return appendAuditEntry(vaultDir, AuditEntry{
			TS:             now.UTC().Format(time.RFC3339),
			BridgeID:       fqid,
			Op:             "callback",
			Kind:           auditKind,
			IdempotencyKey: "callback:" + rec.ProposalID + ":" + rec.Decision,
			DestSHA256:     rb.PrivateCfg.DestSHA256Short,
			PayloadSHA256:  tokHash,
			Outcome:        outcome,
			Attempt:        1,
			LatencyMs:      time.Since(start).Milliseconds(),
			ErrorCode:      errCode,
		})
	})
	if execErr != nil {
		return CallbackResult{}, execErr
	}
	return CallbackResult{
		OK:         true,
		BridgeID:   fqid,
		ProposalID: rec.ProposalID,
		Decision:   rec.Decision,
		Output:     outStr,
	}, nil
}
