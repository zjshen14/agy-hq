package delegates

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/connector"
	"github.com/zjshen14/agy-hq/internal/extension"
)

const (
	MaxTransportStdoutBytes = 256 * 1024 // 256 KiB
	DefaultTransportTimeout = 10 * time.Second
)

// TransportRequest is the JSON request written to a delegate transport binary's stdin (RFC-0026 §5.2).
type TransportRequest struct {
	Protocol       string      `json:"protocol"`
	Op             string      `json:"op"` // "poll" | "reply" | "publish_snapshot" | "status"
	Cursor         string      `json:"cursor,omitempty"`
	RequestID      string      `json:"request_id,omitempty"`
	OriginThread   string      `json:"origin_thread,omitempty"`
	Body           string      `json:"body,omitempty"`
	IdempotencyKey string      `json:"idempotency_key,omitempty"`
	SnapshotID     string      `json:"snapshot_id,omitempty"`
	Snapshot       interface{} `json:"snapshot,omitempty"`
}

// TransportPollItem represents one inbound peer request envelope returned by a transport's `poll` verb.
type TransportPollItem struct {
	RequestID     string `json:"request_id"`
	CallerID      string `json:"caller_id"`
	AuthMethod    string `json:"auth_method"`
	OriginChannel string `json:"origin_channel,omitempty"`
	OriginThread  string `json:"origin_thread"`
	ReceivedAt    string `json:"received_at,omitempty"`
	Subject       string `json:"subject,omitempty"`
	Body          string `json:"body"`
}

// TransportResponse is the single JSON object read from a delegate transport binary's stdout.
type TransportResponse struct {
	Protocol   string              `json:"protocol"`
	OK         bool                `json:"ok"`
	Error      string              `json:"error,omitempty"`
	NextCursor string              `json:"next_cursor,omitempty"`
	Items      []TransportPollItem `json:"items,omitempty"`
	Healthy    bool                `json:"healthy,omitempty"`
	AuthState  string              `json:"auth_state,omitempty"`
	Detail     string              `json:"detail,omitempty"`
}

// ResolveActiveTransport resolves "transport:<pack>/<id>" or "<pack>/<id>" against enabled trusted packs.
func ResolveActiveTransport(vaultDir, sourceSpec string) (extension.ActiveDelegateTransport, error) {
	fqid := strings.TrimPrefix(strings.TrimSpace(sourceSpec), "transport:")
	if fqid == "" {
		return extension.ActiveDelegateTransport{}, fmt.Errorf("empty delegate transport source")
	}
	for _, tr := range extension.ListActiveDelegateTransports(vaultDir) {
		if tr.FQID == fqid {
			return tr, nil
		}
	}
	return extension.ActiveDelegateTransport{}, fmt.Errorf("delegate transport %q is not registered in any enabled trusted companion pack", fqid)
}

func loadTransportPrivateConfigFD3(vaultDir, packID, transportID string) []byte {
	cfgPath := filepath.Join(vaultDir, "private", "delegates", "transports", packID+"__"+transportID+".json")
	st, err := os.Lstat(cfgPath)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return nil
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil
	}
	return raw
}

// InvokeTransport executes one hq-delegate-transport/v1 verb against a SHA-256-pinned pack transport binary.
func InvokeTransport(ctx context.Context, vaultDir, sourceSpec string, req TransportRequest) (TransportResponse, error) {
	tr, err := ResolveActiveTransport(vaultDir, sourceSpec)
	if err != nil {
		return TransportResponse{}, err
	}
	if len(tr.Spec.Command) == 0 || extension.IsBlockedComponentCommand0(tr.Spec.Command[0]) {
		return TransportResponse{}, fmt.Errorf("delegate transport %q command[0] is blocked", tr.FQID)
	}
	execBin, err := extension.VerifyTrustedBinary(vaultDir, "pack:"+tr.PackID, tr.Spec.Command[0])
	if err != nil {
		return TransportResponse{}, fmt.Errorf("verify delegate transport %q binary: %w", tr.FQID, err)
	}

	req.Protocol = extension.DelegateTransportProtocolV1
	stdinBytes, err := json.Marshal(req)
	if err != nil {
		return TransportResponse{}, err
	}

	timeout := DefaultTransportTimeout
	if tr.Spec.TimeoutMs > 0 {
		timeout = time.Duration(tr.Spec.TimeoutMs) * time.Millisecond
		if timeout > time.Duration(extension.MaxDelegateTransportTimeoutMs)*time.Millisecond {
			timeout = time.Duration(extension.MaxDelegateTransportTimeoutMs) * time.Millisecond
		}
	}

	fd3Payload := loadTransportPrivateConfigFD3(vaultDir, tr.PackID, tr.Spec.ID)
	stdout, stderr, err := connector.RunSubprocess(ctx, connector.SubprocessSpec{
		Binary:       execBin,
		Args:         tr.Spec.Command[1:],
		WorkDir:      tr.PackPath,
		EnvAllow:     tr.Spec.EnvAllow,
		StdinBytes:   append(stdinBytes, '\n'),
		PayloadBytes: fd3Payload,
		Timeout:      timeout,
	})
	if err != nil {
		return TransportResponse{}, fmt.Errorf("delegate transport %q op=%s failed: %w (stderr: %s)", tr.FQID, req.Op, err, connector.SanitizeStderr(stderr))
	}
	if len(stdout) > MaxTransportStdoutBytes {
		return TransportResponse{}, fmt.Errorf("delegate transport %q op=%s stdout exceeded %d bytes", tr.FQID, req.Op, MaxTransportStdoutBytes)
	}
	var resp TransportResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return TransportResponse{}, fmt.Errorf("delegate transport %q op=%s invalid JSON response: %w", tr.FQID, req.Op, err)
	}
	if resp.Protocol != "" && resp.Protocol != extension.DelegateTransportProtocolV1 {
		return TransportResponse{}, fmt.Errorf("delegate transport %q unexpected protocol %q", tr.FQID, resp.Protocol)
	}
	if !resp.OK {
		errMsg := strings.TrimSpace(resp.Error)
		if errMsg == "" {
			errMsg = "transport returned ok=false"
		}
		return resp, fmt.Errorf("delegate transport %q op=%s error: %s", tr.FQID, req.Op, errMsg)
	}
	return resp, nil
}
