package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/connector"
	"github.com/zjshen14/agy-hq/internal/extension"
)

// Protocol verbs (RFC-0025 §6).
const (
	OpProbe  = "probe"
	OpList   = "list"
	OpCreate = "create"
	OpStatus = "status"
	OpRemove = "remove"
)

const (
	// DefaultProviderTimeout applies when a provider manifest leaves timeout_ms at 0.
	DefaultProviderTimeout = 20 * time.Second
	// MaxProviderTimeout caps any provider call.
	MaxProviderTimeout = time.Duration(extension.MaxWorkspaceProviderTimeoutMs) * time.Millisecond
	// MaxListResponseBytes caps a `list` response on stdout.
	MaxListResponseBytes = 256 * 1024
	// MaxResponseBytes caps every other response on stdout.
	MaxResponseBytes = 64 * 1024
	// MaxListLimit caps ListRequest.Limit.
	MaxListLimit = 200

	minProviderTimeout     = 100 * time.Millisecond
	maxCursorBytes         = 1024
	maxBaseRequestBytes    = 256
	maxErrorMessageRunes   = 300
	maxIdempotencyKeyBytes = 160
)

var reIdempotencyKey = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,160}$`)

// ExternalProvider adapts a trusted, pack-declared workspace provider binary speaking hq-workspace-provider/v1.
//
// Every call re-verifies the pack manifest and the provider binary against <vault>/private/trust.json
// (extension.VerifyTrustedFile / VerifyTrustedBinary), executes the staged exec-cache copy through
// connector.RunSubprocess (scrubbed environment plus env_allow, own process group, timeout), passes the request
// only on stdin, strictly decodes a size-capped response, and sanitizes every returned Info. It is immutable and
// safe for concurrent use.
type ExternalProvider struct {
	vaultDir string
	packID   string
	packDir  string
	spec     extension.WorkspaceProviderSpec
	fqid     string
}

var _ Provider = (*ExternalProvider)(nil)

// NewExternalProvider builds an adapter for spec declared by packID (installed at packDir). Callers normally
// obtain one through Resolve, which also checks trust and manifest validity up front.
func NewExternalProvider(vaultDir, packID, packDir string, spec extension.WorkspaceProviderSpec) *ExternalProvider {
	packID = strings.TrimSpace(packID)
	return &ExternalProvider{
		vaultDir: vaultDir,
		packID:   packID,
		packDir:  packDir,
		spec:     spec,
		fqid:     packID + "/" + strings.TrimSpace(spec.ID),
	}
}

// ID returns "<pack>/<id>".
func (p *ExternalProvider) ID() string { return p.fqid }

// Spec returns a copy of the provider's manifest entry (for skill guides, refresh cadence, and display).
func (p *ExternalProvider) Spec() extension.WorkspaceProviderSpec {
	s := p.spec
	s.Command = append([]string(nil), p.spec.Command...)
	s.EnvAllow = append([]string(nil), p.spec.EnvAllow...)
	s.RootPatterns = append([]string(nil), p.spec.RootPatterns...)
	s.DestructiveCommands = append([]string(nil), p.spec.DestructiveCommands...)
	return s
}

// Capabilities reports the capabilities declared in the pack manifest.
func (p *ExternalProvider) Capabilities() Capabilities {
	return capabilitiesFromSpec(p.spec)
}

func capabilitiesFromSpec(spec extension.WorkspaceProviderSpec) Capabilities {
	return Capabilities{
		Create:        spec.Capabilities.Create,
		Remove:        spec.Capabilities.Remove,
		Changes:       spec.Capabilities.Changes,
		SharedHistory: spec.Capabilities.SharedHistory,
	}
}

// Probe asks the provider whether it owns absolute path. The returned root must contain path.
func (p *ExternalProvider) Probe(ctx context.Context, path string) (Info, bool, error) {
	clean := strings.TrimSpace(path)
	if clean == "" || !filepath.IsAbs(clean) || len(clean) > MaxPathLength || hasControlOrFormatRune(clean) {
		return Info{}, false, &ProviderError{Code: CodeInvalid, Message: "probe path must be a clean absolute path"}
	}
	clean = filepath.Clean(clean)
	raw, err := p.invoke(ctx, OpProbe, probeRequest{wireHeader: p.header(OpProbe), Path: clean}, MaxResponseBytes)
	if err != nil {
		return Info{}, false, err
	}
	var resp probeResponse
	if err := p.decodeResponse(OpProbe, raw, &resp, &resp.wireBase); err != nil {
		return Info{}, false, err
	}
	if !resp.Owned {
		return Info{}, false, nil
	}
	if resp.Workspace == nil {
		return Info{}, false, p.malformed(OpProbe, "owned:true response is missing workspace")
	}
	info, err := p.toInfo(*resp.Workspace)
	if err != nil {
		return Info{}, false, p.malformed(OpProbe, err.Error())
	}
	if !pathWithin(clean, info.Root) && !pathWithin(CanonicalPath(clean), info.Root) {
		return Info{}, false, p.malformed(OpProbe, fmt.Sprintf("returned root %q does not contain the probed path", info.Root))
	}
	return info, true, nil
}

// List pages through the provider inventory. `list` never reports changes, so Changes is always cleared.
func (p *ExternalProvider) List(ctx context.Context, req ListRequest) ([]Info, string, error) {
	limit := req.Limit
	if limit <= 0 || limit > MaxListLimit {
		limit = MaxListLimit
	}
	if len(req.Cursor) > maxCursorBytes || hasControlOrFormatRune(req.Cursor) {
		return nil, "", &ProviderError{Code: CodeInvalid, Message: "list cursor is malformed"}
	}
	wreq := listRequest{
		wireHeader:       p.header(OpList),
		Cursor:           req.Cursor,
		Limit:            limit,
		IncludeAuxiliary: req.IncludeAuxiliary,
	}
	raw, err := p.invoke(ctx, OpList, wreq, MaxListResponseBytes)
	if err != nil {
		return nil, "", err
	}
	var resp listResponse
	if err := p.decodeResponse(OpList, raw, &resp, &resp.wireBase); err != nil {
		return nil, "", err
	}
	if len(resp.Workspaces) > limit {
		return nil, "", p.malformed(OpList, fmt.Sprintf("returned %d workspaces for limit %d", len(resp.Workspaces), limit))
	}
	if len(resp.NextCursor) > maxCursorBytes || hasControlOrFormatRune(resp.NextCursor) {
		return nil, "", p.malformed(OpList, "next_cursor is malformed")
	}
	out := make([]Info, 0, len(resp.Workspaces))
	for i, w := range resp.Workspaces {
		info, err := p.toInfo(w)
		if err != nil {
			return nil, "", p.malformed(OpList, fmt.Sprintf("entry %d: %v", i, err))
		}
		info.Changes = nil
		out = append(out, info)
	}
	return out, resp.NextCursor, nil
}

// Create asks the provider for an isolated workspace. Name must be slug-derived (ValidateCreateName); ShareFrom
// must name a ledger-owned workspace of this provider; IdempotencyKey defaults to "create:<fqid>:<name>".
func (p *ExternalProvider) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if !p.spec.Capabilities.Create {
		return CreateResult{}, &ProviderError{Code: CodeUnsupported, Message: p.fqid + " does not declare capabilities.create"}
	}
	if err := ValidateCreateName(req.Name); err != nil {
		return CreateResult{}, err
	}
	if len(req.Base) > maxBaseRequestBytes || hasControlOrFormatRune(req.Base) {
		return CreateResult{}, &ProviderError{Code: CodeInvalid, Message: "create base is malformed"}
	}
	if req.ShareFrom != "" {
		if !p.spec.Capabilities.SharedHistory {
			return CreateResult{}, &ProviderError{Code: CodeUnsupported, Message: p.fqid + " does not declare capabilities.shared_history"}
		}
		if !reWorkspaceID.MatchString(req.ShareFrom) {
			return CreateResult{}, &ProviderError{Code: CodeInvalid, Message: "share_from is not a valid workspace id"}
		}
		if e, ok := GetEntry(p.vaultDir, LedgerKey(p.fqid, req.ShareFrom)); !ok || e.Ownership != OwnershipOwned {
			return CreateResult{}, &ProviderError{Code: CodeInvalid, Message: fmt.Sprintf("share_from %q must name a ledger-owned workspace of %s", req.ShareFrom, p.fqid)}
		}
	}
	key := req.IdempotencyKey
	if key == "" {
		key = "create:" + p.fqid + ":" + req.Name
	}
	if len(key) > maxIdempotencyKeyBytes || !reIdempotencyKey.MatchString(key) {
		return CreateResult{}, &ProviderError{Code: CodeInvalid, Message: "idempotency_key is malformed"}
	}
	wreq := createRequest{
		wireHeader:     p.header(OpCreate),
		Name:           req.Name,
		Base:           req.Base,
		ShareFrom:      req.ShareFrom,
		IdempotencyKey: key,
	}
	raw, err := p.invoke(ctx, OpCreate, wreq, MaxResponseBytes)
	if err != nil {
		return CreateResult{}, err
	}
	var resp createResponse
	if err := p.decodeResponse(OpCreate, raw, &resp, &resp.wireBase); err != nil {
		return CreateResult{}, err
	}
	if resp.Workspace == nil {
		return CreateResult{}, p.malformed(OpCreate, "response is missing workspace")
	}
	info, err := p.toInfo(*resp.Workspace)
	if err != nil {
		return CreateResult{}, p.malformed(OpCreate, err.Error())
	}
	return CreateResult{Info: info, Existed: resp.Existed}, nil
}

// Status returns fresh state and changes for workspaceID; the provider must echo the same workspace_id.
func (p *ExternalProvider) Status(ctx context.Context, workspaceID string) (Info, error) {
	if !reWorkspaceID.MatchString(workspaceID) {
		return Info{}, &ProviderError{Code: CodeInvalid, Message: "status workspace_id is not a valid workspace id"}
	}
	raw, err := p.invoke(ctx, OpStatus, statusRequest{wireHeader: p.header(OpStatus), WorkspaceID: workspaceID}, MaxResponseBytes)
	if err != nil {
		return Info{}, err
	}
	var resp statusResponse
	if err := p.decodeResponse(OpStatus, raw, &resp, &resp.wireBase); err != nil {
		return Info{}, err
	}
	if resp.Workspace == nil {
		return Info{}, p.malformed(OpStatus, "response is missing workspace")
	}
	info, err := p.toInfo(*resp.Workspace)
	if err != nil {
		return Info{}, p.malformed(OpStatus, err.Error())
	}
	if info.WorkspaceID != workspaceID {
		return Info{}, p.malformed(OpStatus, fmt.Sprintf("returned workspace_id %q for requested %q", info.WorkspaceID, workspaceID))
	}
	return info, nil
}

// Remove asks the provider to delete a workspace. As defense in depth (RFC-0025 §6.5, §13) the adapter refuses
// before spawning anything unless the ledger records the workspace as owned by hq with the same root as
// ExpectedRoot; the provider must still re-check RequireClean / RequireNoOpenChanges atomically.
func (p *ExternalProvider) Remove(ctx context.Context, req RemoveRequest) error {
	if !p.spec.Capabilities.Remove {
		return &ProviderError{Code: CodeUnsupported, Message: p.fqid + " does not declare capabilities.remove"}
	}
	if !reWorkspaceID.MatchString(req.WorkspaceID) {
		return &ProviderError{Code: CodeInvalid, Message: "remove workspace_id is not a valid workspace id"}
	}
	expectedRoot, err := sanitizeAbsPath("expected_root", req.ExpectedRoot)
	if err != nil {
		return &ProviderError{Code: CodeInvalid, Message: err.Error()}
	}
	entry, ok := GetEntry(p.vaultDir, LedgerKey(p.fqid, req.WorkspaceID))
	if !ok || entry.Ownership != OwnershipOwned {
		return &ProviderError{Code: CodeInvalid, Message: fmt.Sprintf("refusing to remove %s: only workspaces recorded as owned in the ledger can be removed", LedgerKey(p.fqid, req.WorkspaceID))}
	}
	if filepath.Clean(entry.Root) != expectedRoot {
		return &ProviderError{Code: CodeConflict, Message: fmt.Sprintf("expected_root %q does not match the ledger root %q", expectedRoot, entry.Root)}
	}
	wreq := removeRequest{
		wireHeader:           p.header(OpRemove),
		WorkspaceID:          req.WorkspaceID,
		ExpectedRoot:         expectedRoot,
		RequireClean:         req.RequireClean,
		RequireNoOpenChanges: req.RequireNoOpenChanges,
	}
	raw, err := p.invoke(ctx, OpRemove, wreq, MaxResponseBytes)
	if err != nil {
		return err
	}
	var resp removeResponse
	return p.decodeResponse(OpRemove, raw, &resp, &resp.wireBase)
}

func (p *ExternalProvider) timeout() time.Duration {
	if p.spec.TimeoutMs <= 0 {
		return DefaultProviderTimeout
	}
	d := time.Duration(p.spec.TimeoutMs) * time.Millisecond
	if d > MaxProviderTimeout {
		return MaxProviderTimeout
	}
	if d < minProviderTimeout {
		return minProviderTimeout
	}
	return d
}

func (p *ExternalProvider) header(op string) wireHeader {
	return wireHeader{
		Protocol:   ProtocolV1,
		Op:         op,
		ProviderID: p.fqid,
		RequestID:  newRequestID(),
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + hex.EncodeToString(b[:])
}

// invoke verifies trust, runs the provider binary once with payload on stdin, and returns its stdout.
func (p *ExternalProvider) invoke(ctx context.Context, op string, payload interface{}, maxBytes int) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.vaultDir) == "" {
		return nil, fmt.Errorf("workspace provider %s: vaultDir is required", p.fqid)
	}
	if len(p.spec.Command) == 0 || strings.TrimSpace(p.spec.Command[0]) == "" {
		return nil, fmt.Errorf("workspace provider %s: missing command[0]", p.fqid)
	}
	cmd0 := strings.TrimSpace(p.spec.Command[0])
	if extension.IsBlockedComponentCommand0(cmd0) {
		return nil, fmt.Errorf("workspace provider %s: refusing to execute shell, interpreter, or hq command[0] %q", p.fqid, cmd0)
	}
	sourceID := "pack:" + p.packID
	if _, err := extension.VerifyTrustedFile(p.vaultDir, sourceID, filepath.Join(p.packDir, "hq-extension.json")); err != nil {
		return nil, fmt.Errorf("workspace provider %s: verify pack manifest trust: %w", p.fqid, err)
	}
	execPath, err := extension.VerifyTrustedBinary(p.vaultDir, sourceID, cmd0)
	if err != nil {
		return nil, fmt.Errorf("workspace provider %s: %w", p.fqid, err)
	}
	stdin, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("workspace provider %s: marshal %s request: %w", p.fqid, op, err)
	}
	stdout, _, runErr := connector.RunSubprocess(ctx, connector.SubprocessSpec{
		Binary:     execPath,
		Args:       append([]string(nil), p.spec.Command[1:]...),
		WorkDir:    p.packDir,
		EnvAllow:   append([]string(nil), p.spec.EnvAllow...),
		StdinBytes: append(stdin, '\n'),
		Timeout:    p.timeout(),
	})
	if len(stdout) > maxBytes {
		return nil, fmt.Errorf("workspace provider %s %s response exceeded %d-byte limit", p.fqid, op, maxBytes)
	}
	if runErr != nil {
		if pe := decodeFailureResponse(stdout); pe != nil {
			return nil, pe
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(runErr, context.DeadlineExceeded) || strings.Contains(strings.ToLower(runErr.Error()), "timed out") {
			return nil, fmt.Errorf("workspace provider %s %s failed: timed out", p.fqid, op)
		}
		return nil, fmt.Errorf("workspace provider %s %s failed: subprocess exited with error", p.fqid, op)
	}
	return stdout, nil
}

// decodeResponse strictly decodes raw into v and applies the common protocol / ok / error checks via base.
func (p *ExternalProvider) decodeResponse(op string, raw []byte, v interface{}, base *wireBase) error {
	if err := decodeStrict(raw, v); err != nil {
		return p.malformed(op, err.Error())
	}
	if base.Protocol != ProtocolV1 {
		return p.malformed(op, fmt.Sprintf("protocol %q (expected %q)", clipForError(base.Protocol), ProtocolV1))
	}
	if !base.OK {
		if pe := providerErrorFromWire(base.Error); pe != nil {
			return pe
		}
		return p.malformed(op, "ok:false response without a valid error code")
	}
	if base.Error != nil {
		return p.malformed(op, "ok:true response must not carry an error")
	}
	return nil
}

func (p *ExternalProvider) malformed(op, detail string) error {
	return fmt.Errorf("workspace provider %s %s: invalid response: %s", p.fqid, op, detail)
}

// toInfo applies work_subdir defaulting and SanitizeInfo to one wire workspace.
func (p *ExternalProvider) toInfo(w wireInfo) (Info, error) {
	in := Info{
		WorkspaceID: w.WorkspaceID,
		Name:        w.Name,
		Root:        w.Root,
		WorkDir:     w.WorkDir,
		Flavor:      w.Flavor,
		Kind:        w.Kind,
		State:       w.State,
		Changes:     w.Changes,
		Base:        w.Base,
	}
	if in.WorkDir == "" && p.spec.WorkSubdir != "" && filepath.IsAbs(in.Root) && filepath.Clean(in.Root) == in.Root {
		in.WorkDir = filepath.Join(in.Root, filepath.FromSlash(p.spec.WorkSubdir))
	}
	return SanitizeInfo(p.fqid, in)
}

func decodeStrict(raw []byte, v interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("empty response")
		}
		return err
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("unexpected data after the response object")
	}
	return nil
}

// decodeFailureResponse recovers a well-formed ok:false response from a provider that also exited non-zero.
func decodeFailureResponse(stdout []byte) *ProviderError {
	if len(bytes.TrimSpace(stdout)) == 0 {
		return nil
	}
	var base wireBase
	if decodeStrict(stdout, &base) != nil || base.Protocol != ProtocolV1 || base.OK {
		return nil
	}
	return providerErrorFromWire(base.Error)
}

func providerErrorFromWire(we *wireError) *ProviderError {
	if we == nil || !ValidErrorCode(we.Code) {
		return nil
	}
	return &ProviderError{Code: we.Code, Message: sanitizeDisplayText(we.Message, maxErrorMessageRunes)}
}

// Wire types (RFC-0025 §6, frozen by §16.7). Requests carry exactly the documented fields; responses are
// decoded with DisallowUnknownFields, so a provider can never inject extra fields (including `provider`).

type wireHeader struct {
	Protocol   string `json:"protocol"`
	Op         string `json:"op"`
	ProviderID string `json:"provider_id"`
	RequestID  string `json:"request_id"`
	Timestamp  string `json:"timestamp"`
}

type probeRequest struct {
	wireHeader
	Path string `json:"path"`
}

type listRequest struct {
	wireHeader
	Cursor           string `json:"cursor"`
	Limit            int    `json:"limit"`
	IncludeAuxiliary bool   `json:"include_auxiliary"`
}

type createRequest struct {
	wireHeader
	Name           string `json:"name"`
	Base           string `json:"base"`
	ShareFrom      string `json:"share_from"`
	IdempotencyKey string `json:"idempotency_key"`
}

type statusRequest struct {
	wireHeader
	WorkspaceID string `json:"workspace_id"`
}

type removeRequest struct {
	wireHeader
	WorkspaceID          string `json:"workspace_id"`
	ExpectedRoot         string `json:"expected_root"`
	RequireClean         bool   `json:"require_clean"`
	RequireNoOpenChanges bool   `json:"require_no_open_changes"`
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type wireBase struct {
	Protocol string     `json:"protocol"`
	OK       bool       `json:"ok"`
	Error    *wireError `json:"error,omitempty"`
}

type wireInfo struct {
	WorkspaceID string      `json:"workspace_id"`
	Name        string      `json:"name"`
	Root        string      `json:"root"`
	WorkDir     string      `json:"work_dir"`
	Flavor      string      `json:"flavor"`
	Kind        string      `json:"kind"`
	State       State       `json:"state"`
	Changes     []ChangeRef `json:"changes"`
	Base        string      `json:"base"`
}

type probeResponse struct {
	wireBase
	Owned     bool      `json:"owned"`
	Workspace *wireInfo `json:"workspace"`
}

type listResponse struct {
	wireBase
	Workspaces []wireInfo `json:"workspaces"`
	NextCursor string     `json:"next_cursor"`
}

type createResponse struct {
	wireBase
	Existed   bool      `json:"existed"`
	Workspace *wireInfo `json:"workspace"`
}

type statusResponse struct {
	wireBase
	Workspace *wireInfo `json:"workspace"`
}

type removeResponse struct {
	wireBase
}
