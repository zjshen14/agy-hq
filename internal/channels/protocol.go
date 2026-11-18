package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/connector"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/store"
)

const (
	ProtocolV1 = extension.ChannelBridgeProtocolV1

	OpBindDesk = "bind-desk"
	OpNotify   = "notify"
	OpStatus   = "status"

	LeaseStateActive  = "active"
	LeaseStateVacated = "vacated"

	ThreadModeDeskThread = "desk_thread"
	ThreadModeItemThread = "item_thread"

	KindInterrupt      = "interrupt"
	KindCriticalBreach = "critical_breach"
	KindDeskReply      = "desk_reply"
	KindTaskDone       = "task_done"
	KindTaskBlocked    = "task_blocked"
	KindTestPing       = "test_ping"

	ErrCodeRateLimited         = "rate_limited"
	ErrCodeAuthExpired         = "auth_expired"
	ErrCodeDestinationNotFound = "destination_not_found"
	ErrCodePermissionDenied    = "permission_denied"
	ErrCodeInvalidRequest      = "invalid_request"
	ErrCodePermanent           = "permanent"

	DefaultBridgeTimeout = 5 * time.Second
	MaxBridgeTimeout     = 15 * time.Second
	MaxBridgeStdoutBytes = 64 * 1024
	MaxCredentialBytes   = 64 * 1024
)

var reValidIdempotencyKey = regexp.MustCompile(`^[A-Za-z0-9._:/-]{8,160}$`)

// ErrorClass categorizes bridge execution failures for retry/pause scheduling.
type ErrorClass string

const (
	ErrClassTransient            ErrorClass = "transient"
	ErrClassRateLimited          ErrorClass = "rate_limited"
	ErrClassAuthExpired          ErrorClass = "auth_expired"
	ErrClassPermanentDestination ErrorClass = "permanent_destination"
	ErrClassPermanentItem        ErrorClass = "permanent_item"
)

// ProtocolError models structured failure details returned by a bridge on stdout.
type ProtocolError struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

// DeskLeasePayload describes the active or vacated Front Desk shift passed to bind-desk.
type DeskLeasePayload struct {
	LeaseState             string `json:"lease_state"`
	ConversationID         string `json:"conversation_id"`
	PreviousConversationID string `json:"previous_conversation_id,omitempty"`
	ProjectID              string `json:"project_id,omitempty"`
	WorkspaceName          string `json:"workspace_name,omitempty"`
	UpdatedAt              string `json:"updated_at,omitempty"`
}

// DeskThreadRef identifies the pinned external thread tracking `[HQ] Front Desk`.
type DeskThreadRef struct {
	ExternalThreadID string            `json:"external_thread_id,omitempty"`
	Permalink        string            `json:"permalink,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// OpenItemThreadRef describes an open grouped proposal/task/alert thread rebound during shift rotation.
type OpenItemThreadRef struct {
	ItemKey          string `json:"item_key"`
	Kind             string `json:"kind"`
	ExternalThreadID string `json:"external_thread_id"`
	Status           string `json:"status"`
}

// BindDeskRequest is sent on stdin for `op: "bind-desk"`.
type BindDeskRequest struct {
	Protocol         string                 `json:"protocol"`
	Op               string                 `json:"op"`
	BridgeID         string                 `json:"bridge_id"`
	IdempotencyKey   string                 `json:"idempotency_key"`
	Timestamp        string                 `json:"timestamp"`
	HasCredentialFD3 bool                   `json:"has_credential_fd3"`
	Silent           bool                   `json:"silent"`
	Destination      map[string]interface{} `json:"destination"`
	Desk             DeskLeasePayload       `json:"desk"`
	DeskThread       *DeskThreadRef         `json:"desk_thread,omitempty"`
	OpenItemThreads  []OpenItemThreadRef    `json:"open_item_threads,omitempty"`
}

// BoundDeskThread reports the external thread state returned by `bind-desk`.
type BoundDeskThread struct {
	ExternalThreadID string            `json:"external_thread_id"`
	Permalink        string            `json:"permalink,omitempty"`
	Rebound          bool              `json:"rebound,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// BoundItemThread reports an updated item thread mapping returned by `bind-desk`.
type BoundItemThread struct {
	ItemKey          string `json:"item_key"`
	ExternalThreadID string `json:"external_thread_id"`
	Rebound          bool   `json:"rebound,omitempty"`
}

// BindDeskResponse is returned on stdout by `op: "bind-desk"`.
type BindDeskResponse struct {
	Protocol     string            `json:"protocol"`
	OK           bool              `json:"ok"`
	Deduplicated bool              `json:"deduplicated,omitempty"`
	DeskThread   *BoundDeskThread  `json:"desk_thread,omitempty"`
	ItemThreads  []BoundItemThread `json:"item_threads,omitempty"`
	Error        *ProtocolError    `json:"error,omitempty"`
}

// ThreadHint advises the bridge how to thread an outbound notification.
type ThreadHint struct {
	Mode                     string `json:"mode"`
	DeskExternalThreadID     string `json:"desk_external_thread_id,omitempty"`
	ItemKey                  string `json:"item_key,omitempty"`
	ExistingExternalThreadID string `json:"existing_external_thread_id,omitempty"`
}

// NotificationAction carries compact 36-byte one-shot button tokens for `callback_buttons: true` bridges.
type NotificationAction struct {
	ProposalID           string `json:"proposal_id"`
	ActionType           string `json:"action_type"`
	Summary              string `json:"summary"`
	SHA256Prefix         string `json:"sha256_prefix"`
	ApproveCallbackToken string `json:"approve_callback_token"`
	RejectCallbackToken  string `json:"reject_callback_token"`
	ExpiresAt            string `json:"expires_at"`
}

// NotificationPayload carries the sanitized outbound notification body and metadata.
type NotificationPayload struct {
	Kind          string               `json:"kind"`
	Severity      string               `json:"severity,omitempty"`
	SourceSkill   string               `json:"source_skill,omitempty"`
	Scope         string               `json:"scope,omitempty"`
	Title         string               `json:"title"`
	BodyMarkdown  string               `json:"body_markdown"`
	ProposalIDs   []string             `json:"proposal_ids,omitempty"`
	Actions       []NotificationAction `json:"actions,omitempty"`
	TaskSlug      string               `json:"task_slug,omitempty"`
	TaintedOrigin bool                 `json:"tainted_origin,omitempty"`
}

// NotifyRequest is sent on stdin for `op: "notify"`.
type NotifyRequest struct {
	Protocol           string                 `json:"protocol"`
	Op                 string                 `json:"op"`
	BridgeID           string                 `json:"bridge_id"`
	IdempotencyKey     string                 `json:"idempotency_key"`
	Timestamp          string                 `json:"timestamp"`
	HasCredentialFD3   bool                   `json:"has_credential_fd3"`
	Destination        map[string]interface{} `json:"destination"`
	DeskConversationID string                 `json:"desk_conversation_id,omitempty"`
	ThreadHint         ThreadHint             `json:"thread_hint"`
	Notification       NotificationPayload    `json:"notification"`
}

// NotifyResponse is returned on stdout by `op: "notify"`.
type NotifyResponse struct {
	Protocol             string         `json:"protocol"`
	OK                   bool           `json:"ok"`
	DeliveryID           string         `json:"delivery_id,omitempty"`
	Deduplicated         bool           `json:"deduplicated,omitempty"`
	ExternalThreadID     string         `json:"external_thread_id,omitempty"`
	DeskExternalThreadID string         `json:"desk_external_thread_id,omitempty"`
	Error                *ProtocolError `json:"error,omitempty"`
}

// StatusRequest is sent on stdin for `op: "status"`.
type StatusRequest struct {
	Protocol             string                 `json:"protocol"`
	Op                   string                 `json:"op"`
	BridgeID             string                 `json:"bridge_id"`
	Timestamp            string                 `json:"timestamp"`
	HasCredentialFD3     bool                   `json:"has_credential_fd3"`
	Destination          map[string]interface{} `json:"destination"`
	DeskConversationID   string                 `json:"desk_conversation_id,omitempty"`
	DeskExternalThreadID string                 `json:"desk_external_thread_id,omitempty"`
}

// StatusResponse is returned on stdout by `op: "status"`.
type StatusResponse struct {
	Protocol              string         `json:"protocol"`
	OK                    bool           `json:"ok"`
	Healthy               bool           `json:"healthy"`
	AuthState             string         `json:"auth_state,omitempty"`
	InboundTunnelBoundCID string         `json:"inbound_tunnel_bound_cid,omitempty"`
	Detail                string         `json:"detail,omitempty"`
	Error                 *ProtocolError `json:"error,omitempty"`
}

// ClassifyError maps a protocol error or subprocess failure into the 5 runtime error classes.
func ClassifyError(pErr *ProtocolError, execErr error) (ErrorClass, string, int) {
	if pErr != nil {
		code := strings.ToLower(strings.TrimSpace(pErr.Code))
		msg := connector.SanitizeStderr(pErr.Message)
		if msg == "" {
			msg = code
		}
		switch code {
		case ErrCodeRateLimited:
			return ErrClassRateLimited, msg, pErr.RetryAfterSeconds
		case ErrCodeAuthExpired:
			return ErrClassAuthExpired, msg, 0
		case ErrCodeDestinationNotFound, ErrCodePermissionDenied:
			return ErrClassPermanentDestination, msg, 0
		case ErrCodeInvalidRequest, ErrCodePermanent:
			return ErrClassPermanentItem, msg, 0
		default:
			return ErrClassTransient, msg, pErr.RetryAfterSeconds
		}
	}
	if execErr != nil {
		return ErrClassTransient, connector.SanitizeStderr(execErr.Error()), 0
	}
	return ErrClassTransient, "unknown bridge error", 0
}

func sanitizeIdempotencyKey(raw string) string {
	s := strings.TrimSpace(raw)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == ':' || r == '/' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 160 {
		out = out[:160]
	}
	for len(out) < 8 {
		out += "_0"
	}
	if !reValidIdempotencyKey.MatchString(out) {
		return "hq_channel_key_default"
	}
	return out
}

func resolveBridgeTimeout(timeoutMs int) time.Duration {
	if timeoutMs <= 0 {
		return DefaultBridgeTimeout
	}
	d := time.Duration(timeoutMs) * time.Millisecond
	if d > MaxBridgeTimeout {
		return MaxBridgeTimeout
	}
	if d < 100*time.Millisecond {
		return 100 * time.Millisecond
	}
	return d
}

func loadOptionalCredentialFD3(vaultDir, credRel string) ([]byte, bool, error) {
	credRel = strings.TrimSpace(credRel)
	if credRel == "" {
		return nil, false, nil
	}
	cleanRel := filepath.ToSlash(filepath.Clean(credRel))
	if filepath.IsAbs(credRel) || cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, "../") || !strings.HasPrefix(cleanRel, "private/") {
		return nil, false, fmt.Errorf("credential_file %q must reside under private/", credRel)
	}
	fullPath := filepath.Join(vaultDir, filepath.FromSlash(cleanRel))
	f, err := store.OpenReadNoFollow(fullPath)
	if err != nil {
		return nil, false, fmt.Errorf("open credential_file %s: %w", cleanRel, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxCredentialBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > MaxCredentialBytes {
		return nil, false, fmt.Errorf("credential_file %s exceeds %d-byte limit", cleanRel, MaxCredentialBytes)
	}
	return data, true, nil
}

// InvokeBridge verifies pack manifest + SHA-256 binary pins in private/trust.json, loads optional
// /dev/fd/3 credentials, and runs a bounded scrubbed subprocess speaking `hq-channel-bridge/v1`.
func InvokeBridge(
	ctx context.Context,
	vaultDir, packID, packDir string,
	spec extension.ChannelBridgeSpec,
	credRel string,
	reqPayload interface{},
) ([]byte, string, error) {
	sourceID := "pack:" + strings.TrimSpace(packID)
	if _, err := extension.VerifyTrustedFile(vaultDir, sourceID, filepath.Join(packDir, "hq-extension.json")); err != nil {
		return nil, "", fmt.Errorf("verify pack manifest trust (%s): %w", sourceID, err)
	}
	if len(spec.Command) == 0 || strings.TrimSpace(spec.Command[0]) == "" {
		return nil, "", fmt.Errorf("channel bridge %s/%s missing command[0]", packID, spec.ID)
	}
	execPath, err := extension.VerifyTrustedBinary(vaultDir, sourceID, spec.Command[0])
	if err != nil {
		return nil, "", err
	}

	stdinBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, "", fmt.Errorf("marshal %s request: %w", ProtocolV1, err)
	}
	stdinBytes = append(stdinBytes, '\n')

	var fd3Bytes []byte
	if strings.TrimSpace(credRel) != "" {
		b, hasCred, cErr := loadOptionalCredentialFD3(vaultDir, credRel)
		if cErr != nil {
			return nil, "", cErr
		}
		if hasCred {
			fd3Bytes = b
		}
	}

	var args []string
	if len(spec.Command) > 1 {
		args = append(args, spec.Command[1:]...)
	}

	stdoutBytes, stderrStr, runErr := connector.RunSubprocess(ctx, connector.SubprocessSpec{
		Binary:            execPath,
		Args:              args,
		WorkDir:           packDir,
		EnvAllow:          spec.EnvAllow,
		AllowDesktopScope: spec.RequiresDesktopSession,
		StdinBytes:        stdinBytes,
		PayloadBytes:      fd3Bytes,
		Timeout:           resolveBridgeTimeout(spec.TimeoutMs),
	})
	if len(stdoutBytes) > MaxBridgeStdoutBytes {
		return nil, stderrStr, fmt.Errorf("bridge %s/%s stdout exceeded %d-byte limit", packID, spec.ID, MaxBridgeStdoutBytes)
	}
	return bytes.TrimSpace(stdoutBytes), stderrStr, runErr
}
