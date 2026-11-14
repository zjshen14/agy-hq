package kernel

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Default timeouts, batching limits, and retry bounds for Antigravity Sidebar Archive Sync (RFC-0016).
const (
	DefaultSidebarUnaryTimeout    = 1500 * time.Millisecond
	DefaultSidebarBatchTimeout    = 3000 * time.Millisecond
	DefaultSidebarStopHookTimeout = 500 * time.Millisecond
	DefaultSidebarBatchSize       = 50
	DefaultSidebarBackfillLimit   = 500
	MaxSidebarSyncAttempts        = 20
	MaxSidebarSyncBackoff         = 6 * time.Hour
	MaxSidebarSyncAge             = 14 * 24 * time.Hour
)

var (
	reCanonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	sidebarQueueMu  sync.Mutex
)

// IsCanonicalUUID returns true if id is a standard 36-character hex UUID.
func IsCanonicalUUID(id string) bool {
	return reCanonicalUUID.MatchString(strings.ToLower(strings.TrimSpace(id)))
}

func normalizeUUID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

func boolPtr(v bool) *bool {
	b := v
	return &b
}

// LSTrajectorySummary models the subset of exa.jetski_cortex_pb.CascadeTrajectorySummary
// returned by LanguageServerService/GetAllCascadeTrajectories and JetboxSubscribeToSummaries needed for safety gates S2-S7.
type LSTrajectorySummary struct {
	Summary             string                    `json:"summary,omitempty"`
	StepCount           uint32                    `json:"stepCount,omitempty"`
	Status              string                    `json:"status,omitempty"`
	Annotations         LSConversationAnnotations `json:"annotations"`
	TrajectoryMetadata  LSTrajectoryMetadata      `json:"trajectoryMetadata,omitempty"`
	WaitingSteps        []json.RawMessage         `json:"waitingSteps,omitempty"`
	NotFullyIdle        bool                      `json:"notFullyIdle,omitempty"`
	BackgroundTaskCount uint32                    `json:"backgroundTaskCount,omitempty"`
	HasActiveChildren   bool                      `json:"hasActiveChildren,omitempty"`
	HasRunningSubagents bool                      `json:"hasRunningSubagents,omitempty"`
}

// LSTrajectoryMetadata carries parent/depth lineage so JetboxSubscribeToSummaries
// excludes nested child subagent subtrajectories while retaining top-level sidebar threads.
type LSTrajectoryMetadata struct {
	ParentConversationID string `json:"parentConversationId,omitempty"`
	RootConversationID   string `json:"rootConversationId,omitempty"`
	NestingDepth         int    `json:"nestingDepth,omitempty"`
}

type lsJetboxSummariesFrame struct {
	Updates map[string]LSTrajectorySummary `json:"updates"`
}

// LSConversationAnnotations models exa.jetski_cortex_pb.ConversationAnnotations on read responses.
type LSConversationAnnotations struct {
	Title                   string `json:"title,omitempty"`
	Archived                bool   `json:"archived"`
	ArchivalStatusTimestamp string `json:"archivalStatusTimestamp,omitempty"`
	Starred                 bool   `json:"starred"`
	Pinned                  bool   `json:"pinned"`
}

type lsGetAllTrajectoriesRequest struct {
	ExcludeSubtrajectories bool `json:"excludeSubtrajectories"`
}

type lsGetAllTrajectoriesResponse struct {
	TrajectorySummaries map[string]LSTrajectorySummary `json:"trajectorySummaries"`
}

type lsUpdateAnnotationsRequest struct {
	CascadeID        string              `json:"cascadeId,omitempty"`
	CascadeIDs       []string            `json:"cascadeIds,omitempty"`
	Annotations      lsConversationPatch `json:"annotations"`
	MergeAnnotations bool                `json:"mergeAnnotations"`
}

// lsConversationPatch uses *bool pointers so encoding/json emits explicit `"archived": false`
// when unarchiving (setting proto3_optional synthetic oneof `_archived` under mergeAnnotations=true)
// while omitting unset fields when nil.
type lsConversationPatch struct {
	Archived                *bool  `json:"archived,omitempty"`
	ArchivalStatusTimestamp string `json:"archivalStatusTimestamp,omitempty"`
	Pinned                  *bool  `json:"pinned,omitempty"`
}

type sidebarRPCError struct {
	Method     string
	StatusCode int
	Body       string
	Err        error
}

func (e *sidebarRPCError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s RPC failed: %v", e.Method, e.Err)
	}
	return fmt.Sprintf("%s HTTP %d: %s", e.Method, e.StatusCode, e.Body)
}

func isTerminalSidebarTargetError(err error) bool {
	var rpcErr *sidebarRPCError
	if !errors.As(err, &rpcErr) || rpcErr.Err != nil {
		return false
	}
	if rpcErr.StatusCode == http.StatusNotFound || rpcErr.StatusCode == http.StatusBadRequest {
		return true
	}
	lower := strings.ToLower(rpcErr.Body)
	return strings.Contains(lower, `"not_found"`) || strings.Contains(lower, `"invalid_argument"`)
}

// SidebarSyncConfig configures ConnectRPC access to Antigravity's LanguageServerService.
// Zero values fall back to ANTIGRAVITY_LS_ADDRESS, ANTIGRAVITY_CSRF_TOKEN, and ANTIGRAVITY_CONVERSATION_ID.
type SidebarSyncConfig struct {
	BaseURL        string
	CSRFToken      string
	CallerConvID   string
	CallerOverride bool
	HTTPClient     *http.Client
	Now            func() time.Time
}

// SidebarSyncClient communicates with LanguageServerService over sanctioned ConnectRPC HTTP/JSON.
type SidebarSyncClient struct {
	baseURL      string
	csrfToken    string
	callerConvID string
	httpClient   *http.Client
	nowFn        func() time.Time
}

// NewSidebarSyncClient constructs a SidebarSyncClient from cfg and standard Antigravity env vars.
func NewSidebarSyncClient(cfg SidebarSyncConfig) *SidebarSyncClient {
	addr := strings.TrimSpace(cfg.BaseURL)
	if addr == "" {
		addr = strings.TrimSpace(os.Getenv("ANTIGRAVITY_LS_ADDRESS"))
	}
	if addr != "" && !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	addr = strings.TrimRight(addr, "/")

	csrf := strings.TrimSpace(cfg.CSRFToken)
	if csrf == "" {
		csrf = strings.TrimSpace(os.Getenv("ANTIGRAVITY_CSRF_TOKEN"))
	}

	caller := strings.TrimSpace(cfg.CallerConvID)
	if !cfg.CallerOverride && caller == "" {
		caller = strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	}

	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	return &SidebarSyncClient{
		baseURL:      addr,
		csrfToken:    csrf,
		callerConvID: normalizeUUID(caller),
		httpClient:   httpClient,
		nowFn:        nowFn,
	}
}

// Available reports whether both LanguageServerService address and CSRF token are configured.
func (c *SidebarSyncClient) Available() bool {
	return c != nil && c.baseURL != "" && c.csrfToken != ""
}

func (c *SidebarSyncClient) doJSONRPC(ctx context.Context, method string, reqPayload interface{}, respPayload interface{}, defaultTimeout time.Duration) error {
	if !c.Available() {
		return &sidebarRPCError{Method: method, Err: errors.New("LanguageServerService credentials unavailable")}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if defaultTimeout > 0 {
		if deadline, hasDeadline := ctx.Deadline(); !hasDeadline || time.Until(deadline) > defaultTimeout {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
			defer cancel()
		}
	}

	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return &sidebarRPCError{Method: method, Err: err}
	}

	endpoint := fmt.Sprintf("%s/exa.language_server_pb.LanguageServerService/%s", c.baseURL, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return &sidebarRPCError{Method: method, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("x-codeium-csrf-token", c.csrfToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &sidebarRPCError{Method: method, Err: err}
	}
	defer resp.Body.Close()

	respBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return &sidebarRPCError{Method: method, Err: readErr}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &sidebarRPCError{
			Method:     method,
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(respBytes)),
		}
	}
	if respPayload != nil && len(bytes.TrimSpace(respBytes)) > 0 {
		if err := json.Unmarshal(respBytes, respPayload); err != nil {
			return &sidebarRPCError{Method: method, Err: fmt.Errorf("decode response: %w", err)}
		}
	}
	return nil
}

// GetAllCascadeTrajectories fetches currently loaded in-memory top-level conversation summaries from LanguageServerService.
func (c *SidebarSyncClient) GetAllCascadeTrajectories(ctx context.Context) (map[string]LSTrajectorySummary, error) {
	var raw lsGetAllTrajectoriesResponse
	err := c.doJSONRPC(
		ctx,
		"GetAllCascadeTrajectories",
		lsGetAllTrajectoriesRequest{ExcludeSubtrajectories: true},
		&raw,
		DefaultSidebarBatchTimeout,
	)
	if err != nil {
		return nil, err
	}
	out := make(map[string]LSTrajectorySummary, len(raw.TrajectorySummaries))
	for k, v := range raw.TrajectorySummaries {
		out[normalizeUUID(k)] = v
	}
	return out, nil
}

// GetJetboxTrajectorySummaries reads the initial snapshot burst from LanguageServerService/JetboxSubscribeToSummaries
// (Connect streaming protocol `application/connect+json`), which includes historical unloaded conversations persisted
// on disk across Antigravity restarts that are omitted by in-memory GetAllCascadeTrajectories.
// Nested subtrajectories (parentConversationId != "" or nestingDepth > 0) are filtered out.
func (c *SidebarSyncClient) GetJetboxTrajectorySummaries(ctx context.Context) (map[string]LSTrajectorySummary, error) {
	const (
		jetboxSnapshotChunkSize = 100
		maxConnectFrameBytes    = 32 * 1024 * 1024
		jetboxIdleCutoff        = 120 * time.Millisecond
	)
	if !c.Available() {
		return nil, &sidebarRPCError{Method: "JetboxSubscribeToSummaries", Err: errors.New("LanguageServerService credentials unavailable")}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	boundedCtx := ctx
	var boundCancel context.CancelFunc
	if deadline, hasDeadline := ctx.Deadline(); !hasDeadline || time.Until(deadline) > DefaultSidebarBatchTimeout {
		boundedCtx, boundCancel = context.WithTimeout(ctx, DefaultSidebarBatchTimeout)
		defer boundCancel()
	}
	streamCtx, streamCancel := context.WithCancel(boundedCtx)
	defer streamCancel()

	// Connect streaming envelope: 1-byte flags (0x00) + 4-byte big-endian payload length (2) + "{}"
	frameReq := []byte{0x00, 0x00, 0x00, 0x00, 0x02, '{', '}'}
	endpoint := fmt.Sprintf("%s/exa.language_server_pb.LanguageServerService/JetboxSubscribeToSummaries", c.baseURL)
	req, err := http.NewRequestWithContext(streamCtx, http.MethodPost, endpoint, bytes.NewReader(frameReq))
	if err != nil {
		return nil, &sidebarRPCError{Method: "JetboxSubscribeToSummaries", Err: err}
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("x-codeium-csrf-token", c.csrfToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &sidebarRPCError{Method: "JetboxSubscribeToSummaries", Err: err}
	}
	defer resp.Body.Close()

	// Graceful fallback if a minimal mock server only implements GetAllCascadeTrajectories.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &sidebarRPCError{
			Method:     "JetboxSubscribeToSummaries",
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(bodyBytes)),
		}
	}

	out := make(map[string]LSTrajectorySummary)
	var idleCutoff atomic.Bool
	var idleTimer *time.Timer
	defer func() {
		if idleTimer != nil {
			idleTimer.Stop()
		}
	}()

	framesRead := 0
	for {
		var hdr [5]byte
		if _, rErr := io.ReadFull(resp.Body, hdr[:]); rErr != nil {
			if errors.Is(rErr, io.EOF) || (idleCutoff.Load() && framesRead > 0) {
				break
			}
			return nil, &sidebarRPCError{Method: "JetboxSubscribeToSummaries", Err: rErr}
		}
		flags := hdr[0]
		length := binary.BigEndian.Uint32(hdr[1:5])
		if length > maxConnectFrameBytes {
			return nil, &sidebarRPCError{Method: "JetboxSubscribeToSummaries", Err: fmt.Errorf("connect frame size %d exceeds cap", length)}
		}
		payload := make([]byte, length)
		if length > 0 {
			if _, rErr := io.ReadFull(resp.Body, payload); rErr != nil {
				if idleCutoff.Load() && framesRead > 0 {
					break
				}
				return nil, &sidebarRPCError{Method: "JetboxSubscribeToSummaries", Err: rErr}
			}
		}
		// Connect EndStreamResponse flag (0x02)
		if flags&0x02 != 0 {
			break
		}
		var frame lsJetboxSummariesFrame
		if uErr := json.Unmarshal(payload, &frame); uErr != nil {
			return nil, &sidebarRPCError{Method: "JetboxSubscribeToSummaries", Err: fmt.Errorf("decode frame: %w", uErr)}
		}
		framesRead++
		for k, v := range frame.Updates {
			if strings.TrimSpace(v.TrajectoryMetadata.ParentConversationID) != "" || v.TrajectoryMetadata.NestingDepth > 0 {
				continue
			}
			out[normalizeUUID(k)] = v
		}
		if len(frame.Updates) < jetboxSnapshotChunkSize {
			streamCancel()
			break
		}
		if idleTimer == nil {
			idleTimer = time.AfterFunc(jetboxIdleCutoff, func() {
				idleCutoff.Store(true)
				streamCancel()
			})
		} else {
			idleTimer.Reset(jetboxIdleCutoff)
		}
	}
	return out, nil
}

// ResolveTopLevelTrajectories fetches in-memory live trajectories via GetAllCascadeTrajectories and,
// if any requested candidate UUID is unloaded from RAM (e.g., historical conversations across IDE restarts),
// enriches the map from JetboxSubscribeToSummaries while keeping live executor fields authoritative.
func (c *SidebarSyncClient) ResolveTopLevelTrajectories(ctx context.Context, candidateIDs []string) (map[string]LSTrajectorySummary, error) {
	live, err := c.GetAllCascadeTrajectories(ctx)
	if err != nil {
		return nil, err
	}
	needJetbox := false
	for _, rawID := range candidateIDs {
		cid := normalizeUUID(rawID)
		if IsCanonicalUUID(cid) {
			if _, ok := live[cid]; !ok {
				needJetbox = true
				break
			}
		}
	}
	if !needJetbox {
		return live, nil
	}
	jetbox, jbErr := c.GetJetboxTrajectorySummaries(ctx)
	if jbErr != nil || len(jetbox) == 0 {
		// Fall back cleanly to live summaries if Jetbox stream is unavailable or times out.
		return live, nil
	}
	merged := make(map[string]LSTrajectorySummary, len(jetbox)+len(live))
	for k, v := range jetbox {
		merged[k] = v
	}
	for k, v := range live {
		if jb, ok := merged[k]; ok && v.Annotations.Title == "" && jb.Annotations.Title != "" {
			v.Annotations.Title = jb.Annotations.Title
		}
		merged[k] = v
	}
	return merged, nil
}

// UpdateConversationAnnotations patches annotations on one or multiple cascade IDs with mergeAnnotations=true.
func (c *SidebarSyncClient) UpdateConversationAnnotations(ctx context.Context, cascadeID string, cascadeIDs []string, patch lsConversationPatch, timeout time.Duration) error {
	req := lsUpdateAnnotationsRequest{
		CascadeID:        cascadeID,
		CascadeIDs:       cascadeIDs,
		Annotations:      patch,
		MergeAnnotations: true,
	}
	return c.doJSONRPC(ctx, "UpdateConversationAnnotations", req, nil, timeout)
}

// SidebarSafetyAction classifies the outcome of EvaluateSidebarArchiveSafety.
type SidebarSafetyAction string

const (
	SafetyAllow       SidebarSafetyAction = "allow"
	SafetyNoOpSynced  SidebarSafetyAction = "already_synced"
	SafetyNoOpMissing SidebarSafetyAction = "not_top_level_or_missing"
	SafetyDeferQueue  SidebarSafetyAction = "defer_queue"
	SafetyBlockSkip   SidebarSafetyAction = "block_skip"
)

// SidebarSafetyInput supplies Vault and live LanguageServerService signals for Gates S1-S7.
type SidebarSafetyInput struct {
	ConversationID     string
	DesiredArchived    bool
	ExplicitSingleTask bool
	Force              bool
	FrontDeskConvID    string
	CallerConvID       string
	VaultTitle         string
	VaultPinned        bool
	ActiveInHotVault   bool
	Summary            LSTrajectorySummary
	HasSummary         bool
}

// SidebarSafetyDecision reports the deterministic gate decision and reason.
type SidebarSafetyDecision struct {
	Action SidebarSafetyAction
	Gate   string
	Reason string
}

func isTrajectoryBusy(s LSTrajectorySummary) bool {
	st := strings.TrimSpace(s.Status)
	if st != "" && st != "CASCADE_RUN_STATUS_UNSPECIFIED" && st != "CASCADE_RUN_STATUS_IDLE" {
		return true
	}
	if s.NotFullyIdle || s.HasActiveChildren || s.HasRunningSubagents || s.BackgroundTaskCount > 0 || len(s.WaitingSteps) > 0 {
		return true
	}
	return false
}

// EvaluateSidebarArchiveSafety enforces the RFC-0016 7-Gate Defense-in-Depth Safety Model.
func EvaluateSidebarArchiveSafety(in SidebarSafetyInput) SidebarSafetyDecision {
	cid := normalizeUUID(in.ConversationID)
	// Gate S1: Canonical UUID check
	if !IsCanonicalUUID(cid) {
		return SidebarSafetyDecision{Action: SafetyBlockSkip, Gate: "S1", Reason: "invalid_uuid"}
	}

	// Unarchiving (hq reopen): restoring visibility is always safe once S1 and S7 pass.
	if !in.DesiredArchived {
		if !in.HasSummary {
			return SidebarSafetyDecision{Action: SafetyNoOpMissing, Gate: "S7", Reason: "not_top_level_or_missing"}
		}
		if !in.Summary.Annotations.Archived {
			return SidebarSafetyDecision{Action: SafetyNoOpSynced, Gate: "S7", Reason: "already_synced"}
		}
		return SidebarSafetyDecision{Action: SafetyAllow}
	}

	// Gate S2: Unconditional [HQ] Front Desk lease & title guard (never archive even with --force)
	if fd := normalizeUUID(in.FrontDeskConvID); fd != "" && cid == fd {
		return SidebarSafetyDecision{Action: SafetyBlockSkip, Gate: "S2", Reason: "front_desk"}
	}
	if strings.HasPrefix(strings.TrimSpace(in.VaultTitle), "[HQ] Front Desk") {
		return SidebarSafetyDecision{Action: SafetyBlockSkip, Gate: "S2", Reason: "front_desk"}
	}
	if in.HasSummary && strings.HasPrefix(strings.TrimSpace(in.Summary.Annotations.Title), "[HQ] Front Desk") {
		return SidebarSafetyDecision{Action: SafetyBlockSkip, Gate: "S2", Reason: "front_desk"}
	}

	// Gate S3: Caller self-preservation guard
	if caller := normalizeUUID(in.CallerConvID); caller != "" && cid == caller {
		if in.ExplicitSingleTask {
			return SidebarSafetyDecision{Action: SafetyDeferQueue, Gate: "S3", Reason: "caller_self_deferred"}
		}
		return SidebarSafetyDecision{Action: SafetyBlockSkip, Gate: "S3", Reason: "caller_self"}
	}

	// Gate S4: Vault & Antigravity UI pin/star protection
	allowForcedPin := in.ExplicitSingleTask && in.Force
	if in.VaultPinned && !allowForcedPin {
		return SidebarSafetyDecision{Action: SafetyBlockSkip, Gate: "S4", Reason: "vault_pinned"}
	}
	if in.HasSummary && in.Summary.Annotations.Pinned && !allowForcedPin {
		return SidebarSafetyDecision{Action: SafetyBlockSkip, Gate: "S4", Reason: "ui_pinned"}
	}
	if in.HasSummary && in.Summary.Annotations.Starred && !allowForcedPin {
		return SidebarSafetyDecision{Action: SafetyBlockSkip, Gate: "S4", Reason: "ui_starred"}
	}

	// Gate S5: Hot Vault active/idle/stalled state guard
	if !in.ExplicitSingleTask && in.ActiveInHotVault {
		return SidebarSafetyDecision{Action: SafetyBlockSkip, Gate: "S5", Reason: "active_in_vault"}
	}

	if !in.HasSummary {
		return SidebarSafetyDecision{Action: SafetyNoOpMissing, Gate: "S7", Reason: "not_top_level_or_missing"}
	}

	// Gate S6: Live trajectory execution / waiting-step guard
	if isTrajectoryBusy(in.Summary) {
		if in.ExplicitSingleTask {
			return SidebarSafetyDecision{Action: SafetyDeferQueue, Gate: "S6", Reason: "trajectory_busy"}
		}
		return SidebarSafetyDecision{Action: SafetyBlockSkip, Gate: "S6", Reason: "trajectory_busy"}
	}

	// Gate S7: Idempotency guard
	if in.Summary.Annotations.Archived {
		return SidebarSafetyDecision{Action: SafetyNoOpSynced, Gate: "S7", Reason: "already_synced"}
	}

	return SidebarSafetyDecision{Action: SafetyAllow}
}

// SidebarSyncQueue persists unsynced sidebar transitions in <vault>/memory/sidebar_sync_queue.json.
type SidebarSyncQueue struct {
	UpdatedAt string                          `json:"updatedAt"`
	Items     map[string]SidebarSyncQueueItem `json:"items"`
}

// SidebarSyncQueueItem tracks a single conversation's pending desired sidebar archive state.
type SidebarSyncQueueItem struct {
	ConversationID  string `json:"conversationId"`
	TaskSlug        string `json:"taskSlug,omitempty"`
	DesiredArchived bool   `json:"desiredArchived"`
	Force           bool   `json:"force,omitempty"`
	EnqueuedAt      string `json:"enqueuedAt"`
	UpdatedAt       string `json:"updatedAt"`
	Attempts        int    `json:"attempts"`
	NextRetryAt     string `json:"nextRetryAt"`
	LastError       string `json:"lastError,omitempty"`
}

// SidebarSyncQueueStatus summarizes queue health for `hq doctor` and `hq tasks`.
type SidebarSyncQueueStatus struct {
	PendingCount     int    `json:"pendingCount"`
	HighAttemptCount int    `json:"highAttemptCount"`
	OldestEnqueuedAt string `json:"oldestEnqueuedAt,omitempty"`
	LastErrorSample  string `json:"lastErrorSample,omitempty"`
}

// SidebarQueueFilePath returns <vault>/memory/sidebar_sync_queue.json.
func SidebarQueueFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "sidebar_sync_queue.json")
}

// HasPendingSidebarSyncQueue performs an O(1) os.Stat check so hooks incur 0ms overhead when empty.
func HasPendingSidebarSyncQueue(vaultDir string) bool {
	if strings.TrimSpace(vaultDir) == "" {
		return false
	}
	st, err := os.Stat(SidebarQueueFilePath(vaultDir))
	return err == nil && !st.IsDir() && st.Size() > 2
}

func sidebarBackoffDuration(attempts int) time.Duration {
	switch {
	case attempts <= 1:
		return 30 * time.Second
	case attempts == 2:
		return 2 * time.Minute
	case attempts == 3:
		return 10 * time.Minute
	case attempts == 4:
		return 30 * time.Minute
	case attempts == 5:
		return 2 * time.Hour
	default:
		return MaxSidebarSyncBackoff
	}
}

func parseSidebarTimestamp(raw string) time.Time {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return ts
	}
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts
	}
	return time.Time{}
}

func loadSidebarSyncQueueUnlocked(vaultDir string) SidebarSyncQueue {
	q := SidebarSyncQueue{
		Items: make(map[string]SidebarSyncQueueItem),
	}
	data, err := os.ReadFile(SidebarQueueFilePath(vaultDir))
	if err != nil {
		return q
	}
	var parsed SidebarSyncQueue
	if json.Unmarshal(data, &parsed) == nil && len(parsed.Items) > 0 {
		for k, v := range parsed.Items {
			cid := normalizeUUID(k)
			if cid == "" {
				cid = normalizeUUID(v.ConversationID)
			}
			if !IsCanonicalUUID(cid) {
				continue
			}
			v.ConversationID = cid
			q.Items[cid] = v
		}
		q.UpdatedAt = parsed.UpdatedAt
	}
	return q
}

func saveSidebarSyncQueueUnlocked(vaultDir string, q SidebarSyncQueue, now time.Time) error {
	path := SidebarQueueFilePath(vaultDir)
	if len(q.Items) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now()
	}
	q.UpdatedAt = now.UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// LoadSidebarSyncQueue returns a snapshot of the durable sidebar retry queue.
func LoadSidebarSyncQueue(vaultDir string) SidebarSyncQueue {
	sidebarQueueMu.Lock()
	defer sidebarQueueMu.Unlock()
	return loadSidebarSyncQueueUnlocked(vaultDir)
}

// InspectSidebarSyncQueue computes queue diagnostics for `hq doctor`.
func InspectSidebarSyncQueue(vaultDir string) SidebarSyncQueueStatus {
	if !HasPendingSidebarSyncQueue(vaultDir) {
		return SidebarSyncQueueStatus{}
	}
	q := LoadSidebarSyncQueue(vaultDir)
	st := SidebarSyncQueueStatus{
		PendingCount: len(q.Items),
	}
	var oldest time.Time
	for _, it := range q.Items {
		if it.Attempts >= 5 {
			st.HighAttemptCount++
		}
		if st.LastErrorSample == "" && it.LastError != "" {
			st.LastErrorSample = it.LastError
		}
		if ts := parseSidebarTimestamp(it.EnqueuedAt); !ts.IsZero() && (oldest.IsZero() || ts.Before(oldest)) {
			oldest = ts
			st.OldestEnqueuedAt = it.EnqueuedAt
		}
	}
	return st
}

// EnqueueSidebarSync inserts or supersedes a conversation's desired sidebar state in the retry queue.
func EnqueueSidebarSync(vaultDir string, item SidebarSyncQueueItem, now time.Time) error {
	cid := normalizeUUID(item.ConversationID)
	if !IsCanonicalUUID(cid) {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	nowUTC := now.UTC().Format(time.RFC3339)

	sidebarQueueMu.Lock()
	defer sidebarQueueMu.Unlock()

	q := loadSidebarSyncQueueUnlocked(vaultDir)
	prev, existed := q.Items[cid]

	item.ConversationID = cid
	item.UpdatedAt = nowUTC
	if existed && prev.DesiredArchived == item.DesiredArchived {
		if item.TaskSlug == "" {
			item.TaskSlug = prev.TaskSlug
		}
		if prev.EnqueuedAt != "" {
			item.EnqueuedAt = prev.EnqueuedAt
		} else {
			item.EnqueuedAt = nowUTC
		}
		if item.Force || prev.Force {
			item.Force = true
		}
		if item.Attempts == 0 {
			item.Attempts = prev.Attempts + 1
		}
		if item.NextRetryAt == "" {
			item.NextRetryAt = now.Add(sidebarBackoffDuration(item.Attempts)).UTC().Format(time.RFC3339)
		}
	} else {
		// New item or state supersession (e.g., hq reopen overriding pending archive)
		item.EnqueuedAt = nowUTC
		if item.NextRetryAt == "" {
			item.NextRetryAt = nowUTC
		}
	}
	q.Items[cid] = item
	return saveSidebarSyncQueueUnlocked(vaultDir, q, now)
}

// RemoveSidebarSyncQueueItem deletes convID from the retry queue if present.
func RemoveSidebarSyncQueueItem(vaultDir, convID string, now time.Time) {
	cid := normalizeUUID(convID)
	if cid == "" || !HasPendingSidebarSyncQueue(vaultDir) {
		return
	}
	sidebarQueueMu.Lock()
	defer sidebarQueueMu.Unlock()
	q := loadSidebarSyncQueueUnlocked(vaultDir)
	if _, ok := q.Items[cid]; ok {
		delete(q.Items, cid)
		_ = saveSidebarSyncQueueUnlocked(vaultDir, q, now)
	}
}

// SidebarSingleSyncResult describes the sidebar sync outcome for a single ArchiveTask or ReopenTask call.
type SidebarSingleSyncResult struct {
	Synced        bool   `json:"synced"`
	AlreadySynced bool   `json:"alreadySynced"`
	Queued        bool   `json:"queued"`
	SkippedReason string `json:"skippedReason,omitempty"`
}

func syncSingleTaskArchiveState(ctx context.Context, vaultDir string, task *ActiveTask, desiredArchived bool, explicitSingle bool, force bool, cfg SidebarSyncConfig) SidebarSingleSyncResult {
	if task == nil {
		return SidebarSingleSyncResult{SkippedReason: "nil_task"}
	}
	cid := normalizeUUID(task.ConversationID)
	if !IsCanonicalUUID(cid) {
		return SidebarSingleSyncResult{SkippedReason: "invalid_uuid"}
	}

	client := NewSidebarSyncClient(cfg)
	now := client.nowFn()
	nowUTC := now.UTC().Format(time.RFC3339Nano)

	var fdID string
	if fd, ok := GetFrontDesk(vaultDir); ok {
		fdID = normalizeUUID(fd.ConversationID)
	}

	if desiredArchived {
		if (fdID != "" && cid == fdID) || strings.HasPrefix(strings.TrimSpace(task.Title), "[HQ] Front Desk") {
			RemoveSidebarSyncQueueItem(vaultDir, cid, now)
			return SidebarSingleSyncResult{SkippedReason: "front_desk"}
		}
		if task.Pinned && !(explicitSingle && force) {
			RemoveSidebarSyncQueueItem(vaultDir, cid, now)
			return SidebarSingleSyncResult{SkippedReason: "vault_pinned"}
		}
		if client.callerConvID != "" && cid == client.callerConvID {
			if explicitSingle {
				_ = EnqueueSidebarSync(vaultDir, SidebarSyncQueueItem{
					ConversationID:  cid,
					TaskSlug:        task.TaskSlug,
					DesiredArchived: true,
					Force:           force,
					NextRetryAt:     now.Add(15 * time.Second).UTC().Format(time.RFC3339),
					LastError:       "caller_self_deferred",
				}, now)
				return SidebarSingleSyncResult{Queued: true, SkippedReason: "caller_self_deferred"}
			}
			return SidebarSingleSyncResult{SkippedReason: "caller_self"}
		}
	}

	if !client.Available() {
		_ = EnqueueSidebarSync(vaultDir, SidebarSyncQueueItem{
			ConversationID:  cid,
			TaskSlug:        task.TaskSlug,
			DesiredArchived: desiredArchived,
			Force:           force && explicitSingle,
			NextRetryAt:     now.UTC().Format(time.RFC3339),
			LastError:       "ls_offline",
		}, now)
		return SidebarSingleSyncResult{Queued: true, SkippedReason: "ls_offline"}
	}

	summaries, err := client.ResolveTopLevelTrajectories(ctx, []string{cid})
	if err != nil {
		_ = EnqueueSidebarSync(vaultDir, SidebarSyncQueueItem{
			ConversationID:  cid,
			TaskSlug:        task.TaskSlug,
			DesiredArchived: desiredArchived,
			Force:           force && explicitSingle,
			Attempts:        1,
			NextRetryAt:     now.Add(sidebarBackoffDuration(1)).UTC().Format(time.RFC3339),
			LastError:       err.Error(),
		}, now)
		return SidebarSingleSyncResult{Queued: true, SkippedReason: "rpc_read_error"}
	}

	sum, hasSum := summaries[cid]
	dec := EvaluateSidebarArchiveSafety(SidebarSafetyInput{
		ConversationID:     cid,
		DesiredArchived:    desiredArchived,
		ExplicitSingleTask: explicitSingle,
		Force:              force,
		FrontDeskConvID:    fdID,
		CallerConvID:       client.callerConvID,
		VaultTitle:         task.Title,
		VaultPinned:        task.Pinned,
		Summary:            sum,
		HasSummary:         hasSum,
	})

	switch dec.Action {
	case SafetyNoOpSynced:
		RemoveSidebarSyncQueueItem(vaultDir, cid, now)
		task.SidebarArchived = desiredArchived
		task.SidebarSyncedAt = nowUTC
		return SidebarSingleSyncResult{AlreadySynced: true}

	case SafetyNoOpMissing:
		RemoveSidebarSyncQueueItem(vaultDir, cid, now)
		return SidebarSingleSyncResult{SkippedReason: dec.Reason}

	case SafetyDeferQueue:
		_ = EnqueueSidebarSync(vaultDir, SidebarSyncQueueItem{
			ConversationID:  cid,
			TaskSlug:        task.TaskSlug,
			DesiredArchived: desiredArchived,
			Force:           force && explicitSingle,
			NextRetryAt:     now.Add(15 * time.Second).UTC().Format(time.RFC3339),
			LastError:       dec.Reason,
		}, now)
		return SidebarSingleSyncResult{Queued: true, SkippedReason: dec.Reason}

	case SafetyBlockSkip:
		RemoveSidebarSyncQueueItem(vaultDir, cid, now)
		return SidebarSingleSyncResult{SkippedReason: dec.Reason}
	}

	patch := lsConversationPatch{
		Archived:                boolPtr(desiredArchived),
		ArchivalStatusTimestamp: nowUTC,
	}
	if wErr := client.UpdateConversationAnnotations(ctx, cid, nil, patch, DefaultSidebarUnaryTimeout); wErr != nil {
		if isTerminalSidebarTargetError(wErr) {
			RemoveSidebarSyncQueueItem(vaultDir, cid, now)
			return SidebarSingleSyncResult{SkippedReason: "not_in_ls"}
		}
		_ = EnqueueSidebarSync(vaultDir, SidebarSyncQueueItem{
			ConversationID:  cid,
			TaskSlug:        task.TaskSlug,
			DesiredArchived: desiredArchived,
			Force:           force && explicitSingle,
			Attempts:        1,
			NextRetryAt:     now.Add(sidebarBackoffDuration(1)).UTC().Format(time.RFC3339),
			LastError:       wErr.Error(),
		}, now)
		return SidebarSingleSyncResult{Queued: true, SkippedReason: "rpc_write_error"}
	}

	RemoveSidebarSyncQueueItem(vaultDir, cid, now)
	task.SidebarArchived = desiredArchived
	task.SidebarSyncedAt = nowUTC
	return SidebarSingleSyncResult{Synced: true}
}

// SyncTaskPinnedBestEffort mirrors Vault pin/unpin state onto Antigravity's sidebar when online.
func SyncTaskPinnedBestEffort(vaultDir, convID string, pinned bool, cfg SidebarSyncConfig) {
	cid := normalizeUUID(convID)
	if !IsCanonicalUUID(cid) {
		return
	}
	client := NewSidebarSyncClient(cfg)
	if !client.Available() {
		return
	}
	_ = client.UpdateConversationAnnotations(context.Background(), cid, nil, lsConversationPatch{
		Pinned: boolPtr(pinned),
	}, DefaultSidebarUnaryTimeout)
}

// SyncSidebarOptions configures batch sidebar reconciliation, compaction sync, queue draining, and backfill.
type SyncSidebarOptions struct {
	Now                time.Time
	DryRun             bool
	Backfill           bool
	QueueOnly          bool
	ExplicitArchiveIDs []string
	Limit              int
	BatchSize          int
	MaxDuration        time.Duration
	ClientConfig       SidebarSyncConfig
}

// SidebarSkipDetail records why a candidate conversation was skipped by Safety Gates S1-S6.
type SidebarSkipDetail struct {
	ConversationID string `json:"conversationId"`
	TaskSlug       string `json:"taskSlug,omitempty"`
	Title          string `json:"title,omitempty"`
	Gate           string `json:"gate"`
	Reason         string `json:"reason"`
}

// SidebarCandidate represents an evaluated conversation candidate in SyncSidebar.
type SidebarCandidate struct {
	ConversationID  string `json:"conversationId"`
	TaskSlug        string `json:"taskSlug,omitempty"`
	Title           string `json:"title,omitempty"`
	DesiredArchived bool   `json:"desiredArchived"`
	Source          string `json:"source"`
	Force           bool   `json:"force,omitempty"`
}

// SyncSidebarReport summarizes dry-run or live sidebar synchronization outcomes.
type SyncSidebarReport struct {
	DryRun                      bool                `json:"dryRun"`
	Backfill                    bool                `json:"backfill"`
	LSOnline                    bool                `json:"lsOnline"`
	CandidatesEvaluated         int                 `json:"candidatesEvaluated"`
	ArchivedCount               int                 `json:"archivedCount"`
	UnarchivedCount             int                 `json:"unarchivedCount"`
	AlreadySyncedCount          int                 `json:"alreadySyncedCount"`
	MissingOrSubtrajectoryCount int                 `json:"missingOrSubtrajectoryCount"`
	QueuedCount                 int                 `json:"queuedCount"`
	PendingQueueRemaining       int                 `json:"pendingQueueRemaining"`
	WouldArchive                []SidebarCandidate  `json:"wouldArchive,omitempty"`
	WouldUnarchive              []SidebarCandidate  `json:"wouldUnarchive,omitempty"`
	ArchivedIDs                 []string            `json:"archivedIds,omitempty"`
	UnarchivedIDs               []string            `json:"unarchivedIds,omitempty"`
	ProtectedSkipped            []SidebarSkipDetail `json:"protectedSkipped,omitempty"`
	LastError                   string              `json:"lastError,omitempty"`
}

// DrainSidebarSyncQueue flushes due items in <vault>/memory/sidebar_sync_queue.json within maxDuration.
func DrainSidebarSyncQueue(vaultDir string, maxDuration time.Duration, cfgs ...SidebarSyncConfig) (*SyncSidebarReport, error) {
	if !HasPendingSidebarSyncQueue(vaultDir) {
		return &SyncSidebarReport{}, nil
	}
	var cfg SidebarSyncConfig
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	return SyncSidebar(vaultDir, SyncSidebarOptions{
		QueueOnly:    true,
		MaxDuration:  maxDuration,
		ClientConfig: cfg,
	})
}

// SyncSidebar reconciles queued, hot-archived, and (when Backfill is true) cold-archived tasks
// with Antigravity's LanguageServerService under the 7-Gate Safety Model.
func SyncSidebar(vaultDir string, opts SyncSidebarOptions) (*SyncSidebarReport, error) {
	if !opts.Now.IsZero() && opts.ClientConfig.Now == nil {
		fixedNow := opts.Now
		opts.ClientConfig.Now = func() time.Time { return fixedNow }
	}
	client := NewSidebarSyncClient(opts.ClientConfig)
	now := client.nowFn()
	if !opts.Now.IsZero() {
		now = opts.Now
	}
	nowUTC := now.UTC().Format(time.RFC3339Nano)
	nowSecUTC := now.UTC().Format(time.RFC3339)

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultSidebarBatchSize
	}

	rep := &SyncSidebarReport{
		DryRun:   opts.DryRun,
		Backfill: opts.Backfill,
		LSOnline: client.Available(),
	}

	var fdID string
	if fd, ok := GetFrontDesk(vaultDir); ok {
		fdID = normalizeUUID(fd.ConversationID)
	}

	hotTasks, _ := ListTasks(vaultDir)
	hotIdxByConv := make(map[string]int, len(hotTasks))
	activeInHot := make(map[string]bool)
	pinnedInVault := make(map[string]bool)
	slugByConv := make(map[string]string)
	titleByConv := make(map[string]string)

	for i, t := range hotTasks {
		cid := normalizeUUID(t.ConversationID)
		if !IsCanonicalUUID(cid) {
			continue
		}
		hotIdxByConv[cid] = i
		if t.TaskSlug != "" {
			slugByConv[cid] = t.TaskSlug
		}
		if t.Title != "" {
			titleByConv[cid] = t.Title
		}
		if t.Pinned {
			pinnedInVault[cid] = true
		}
		eff := t.EffectiveStatus(now)
		if eff == "active" || eff == "idle" || eff == "stalled" {
			activeInHot[cid] = true
		}
	}

	sidebarQueueMu.Lock()
	queue := loadSidebarSyncQueueUnlocked(vaultDir)
	queueMutated := false
	for cid, it := range queue.Items {
		enqTime := parseSidebarTimestamp(it.EnqueuedAt)
		if it.Attempts >= MaxSidebarSyncAttempts || (!enqTime.IsZero() && now.After(enqTime) && now.Sub(enqTime) > MaxSidebarSyncAge) {
			if !opts.DryRun {
				delete(queue.Items, cid)
				queueMutated = true
			}
			continue
		}
		if it.DesiredArchived && activeInHot[cid] {
			if !opts.DryRun {
				delete(queue.Items, cid)
				queueMutated = true
			}
		}
	}
	sidebarQueueMu.Unlock()

	var orderedIDs []string
	candMap := make(map[string]SidebarCandidate)
	addCandidate := func(c SidebarCandidate) {
		cid := normalizeUUID(c.ConversationID)
		if cid == "" {
			return
		}
		c.ConversationID = cid
		if c.TaskSlug == "" {
			c.TaskSlug = slugByConv[cid]
		}
		if c.Title == "" {
			c.Title = titleByConv[cid]
		}
		if existing, exists := candMap[cid]; exists {
			// Queue state takes precedence on DesiredArchived because it reflects the latest user action (e.g. reopen).
			if existing.Source == "queue" {
				return
			}
			candMap[cid] = c
			return
		}
		candMap[cid] = c
		orderedIDs = append(orderedIDs, cid)
	}

	// 1. Due retry queue items (sorted deterministically by ConversationID)
	var queueKeys []string
	for cid := range queue.Items {
		queueKeys = append(queueKeys, cid)
	}
	sort.Strings(queueKeys)
	for _, cid := range queueKeys {
		it := queue.Items[cid]
		if it.DesiredArchived && activeInHot[cid] {
			continue
		}
		nextAt := parseSidebarTimestamp(it.NextRetryAt)
		if !nextAt.IsZero() && now.Before(nextAt) {
			continue
		}
		addCandidate(SidebarCandidate{
			ConversationID:  cid,
			TaskSlug:        it.TaskSlug,
			DesiredArchived: it.DesiredArchived,
			Source:          "queue",
			Force:           it.Force,
		})
	}

	// 2. Explicit newly compacted/rotated IDs
	for _, rawID := range opts.ExplicitArchiveIDs {
		cid := normalizeUUID(rawID)
		if !IsCanonicalUUID(cid) {
			continue
		}
		addCandidate(SidebarCandidate{
			ConversationID:  cid,
			DesiredArchived: true,
			Source:          "compact",
		})
	}

	// 3. Hot archived tasks when running reconciliation (`hq tasks sync-sidebar`)
	if !opts.QueueOnly && len(opts.ExplicitArchiveIDs) == 0 {
		for _, t := range hotTasks {
			if strings.ToLower(strings.TrimSpace(t.Status)) == "archived" && IsCanonicalUUID(t.ConversationID) {
				addCandidate(SidebarCandidate{
					ConversationID:  t.ConversationID,
					TaskSlug:        t.TaskSlug,
					Title:           t.Title,
					DesiredArchived: true,
					Source:          "hot_archived",
				})
			}
		}
	}

	// 4. Cold historical archive (`tasks_archive.jsonl`) when `--backfill` is enabled
	if opts.Backfill {
		coldTasks, _, _ := ListArchivedTasks(vaultDir, 0)
		for _, ct := range coldTasks {
			cid := normalizeUUID(ct.ConversationID)
			if !IsCanonicalUUID(cid) {
				continue
			}
			if ct.Pinned {
				pinnedInVault[cid] = true
			}
			if slugByConv[cid] == "" && ct.TaskSlug != "" {
				slugByConv[cid] = ct.TaskSlug
			}
			if titleByConv[cid] == "" && ct.Title != "" {
				titleByConv[cid] = ct.Title
			}
			addCandidate(SidebarCandidate{
				ConversationID:  cid,
				TaskSlug:        ct.TaskSlug,
				Title:           ct.Title,
				DesiredArchived: true,
				Source:          "cold_backfill",
			})
		}
	}

	rep.CandidatesEvaluated = len(orderedIDs)
	if len(orderedIDs) == 0 {
		if queueMutated && !opts.DryRun {
			sidebarQueueMu.Lock()
			_ = saveSidebarSyncQueueUnlocked(vaultDir, queue, now)
			sidebarQueueMu.Unlock()
		}
		rep.PendingQueueRemaining = len(queue.Items)
		return rep, nil
	}

	enqueueOfflineOrTransient := func(c SidebarCandidate, errMsg string) {
		if opts.DryRun {
			return
		}
		sidebarQueueMu.Lock()
		defer sidebarQueueMu.Unlock()
		prev, existed := queue.Items[c.ConversationID]
		attempts := 1
		enqAt := nowSecUTC
		if existed && prev.DesiredArchived == c.DesiredArchived {
			attempts = prev.Attempts + 1
			if prev.EnqueuedAt != "" {
				enqAt = prev.EnqueuedAt
			}
		}
		nextRetry := now.UTC().Format(time.RFC3339)
		if errMsg != "ls_offline" {
			nextRetry = now.Add(sidebarBackoffDuration(attempts)).UTC().Format(time.RFC3339)
		}
		queue.Items[c.ConversationID] = SidebarSyncQueueItem{
			ConversationID:  c.ConversationID,
			TaskSlug:        c.TaskSlug,
			DesiredArchived: c.DesiredArchived,
			Force:           c.Force && c.Source == "queue",
			EnqueuedAt:      enqAt,
			UpdatedAt:       nowSecUTC,
			Attempts:        attempts,
			NextRetryAt:     nextRetry,
			LastError:       errMsg,
		}
		queueMutated = true
	}

	removeQueued := func(cid string) {
		if opts.DryRun {
			return
		}
		sidebarQueueMu.Lock()
		if _, ok := queue.Items[cid]; ok {
			delete(queue.Items, cid)
			queueMutated = true
		}
		sidebarQueueMu.Unlock()
	}

	checkLocalProtection := func(cid string, c SidebarCandidate) (SidebarSafetyDecision, bool) {
		explicitQueuedForce := c.Source == "queue" && c.Force
		dec := EvaluateSidebarArchiveSafety(SidebarSafetyInput{
			ConversationID:     cid,
			DesiredArchived:    c.DesiredArchived,
			ExplicitSingleTask: explicitQueuedForce,
			Force:              explicitQueuedForce,
			FrontDeskConvID:    fdID,
			CallerConvID:       client.callerConvID,
			VaultTitle:         c.Title,
			VaultPinned:        pinnedInVault[cid],
			ActiveInHotVault:   activeInHot[cid],
			HasSummary:         false,
		})
		if dec.Action == SafetyBlockSkip {
			return dec, true
		}
		return dec, false
	}

	if !client.Available() {
		rep.LSOnline = false
		rep.LastError = "ANTIGRAVITY_LS_ADDRESS or ANTIGRAVITY_CSRF_TOKEN not set"
		for _, cid := range orderedIDs {
			c := candMap[cid]
			if dec, blocked := checkLocalProtection(cid, c); blocked {
				rep.ProtectedSkipped = append(rep.ProtectedSkipped, SidebarSkipDetail{
					ConversationID: cid,
					TaskSlug:       c.TaskSlug,
					Title:          c.Title,
					Gate:           dec.Gate,
					Reason:         dec.Reason,
				})
				if dec.Reason != "caller_self" {
					removeQueued(cid)
				}
				continue
			}
			if c.Source == "compact" {
				enqueueOfflineOrTransient(c, "ls_offline")
				rep.QueuedCount++
			}
		}
		if queueMutated && !opts.DryRun {
			sidebarQueueMu.Lock()
			_ = saveSidebarSyncQueueUnlocked(vaultDir, queue, now)
			sidebarQueueMu.Unlock()
		}
		rep.PendingQueueRemaining = len(queue.Items)
		return rep, nil
	}

	ctx := context.Background()
	var cancel context.CancelFunc
	if opts.MaxDuration > 0 {
		ctx, cancel = context.WithTimeout(ctx, opts.MaxDuration)
		defer cancel()
	}

	summaries, err := client.ResolveTopLevelTrajectories(ctx, orderedIDs)
	if err != nil {
		rep.LSOnline = false
		rep.LastError = err.Error()
		for _, cid := range orderedIDs {
			c := candMap[cid]
			if dec, blocked := checkLocalProtection(cid, c); blocked {
				rep.ProtectedSkipped = append(rep.ProtectedSkipped, SidebarSkipDetail{
					ConversationID: cid,
					TaskSlug:       c.TaskSlug,
					Title:          c.Title,
					Gate:           dec.Gate,
					Reason:         dec.Reason,
				})
				if dec.Reason != "caller_self" {
					removeQueued(cid)
				}
				continue
			}
			if c.Source == "compact" || c.Source == "queue" {
				enqueueOfflineOrTransient(c, err.Error())
				if c.Source == "compact" {
					rep.QueuedCount++
				}
			}
		}
		if queueMutated && !opts.DryRun {
			sidebarQueueMu.Lock()
			_ = saveSidebarSyncQueueUnlocked(vaultDir, queue, now)
			sidebarQueueMu.Unlock()
		}
		rep.PendingQueueRemaining = len(queue.Items)
		return rep, nil
	}
	rep.LSOnline = true

	var toArchive []SidebarCandidate
	var toUnarchive []SidebarCandidate
	hotMutated := false
	markHotSynced := func(cid string, archived bool) {
		if opts.DryRun {
			return
		}
		if idx, ok := hotIdxByConv[cid]; ok && idx >= 0 && idx < len(hotTasks) {
			if hotTasks[idx].SidebarArchived != archived || hotTasks[idx].SidebarSyncedAt == "" {
				hotTasks[idx].SidebarArchived = archived
				hotTasks[idx].SidebarSyncedAt = nowUTC
				hotMutated = true
			}
		}
	}

	for _, cid := range orderedIDs {
		cand := candMap[cid]
		sum, hasSum := summaries[cid]
		explicitQueuedForce := cand.Source == "queue" && cand.Force
		dec := EvaluateSidebarArchiveSafety(SidebarSafetyInput{
			ConversationID:     cid,
			DesiredArchived:    cand.DesiredArchived,
			ExplicitSingleTask: explicitQueuedForce,
			Force:              explicitQueuedForce,
			FrontDeskConvID:    fdID,
			CallerConvID:       client.callerConvID,
			VaultTitle:         cand.Title,
			VaultPinned:        pinnedInVault[cid],
			ActiveInHotVault:   activeInHot[cid],
			Summary:            sum,
			HasSummary:         hasSum,
		})

		switch dec.Action {
		case SafetyAllow:
			if opts.Limit > 0 && (len(toArchive)+len(toUnarchive)) >= opts.Limit {
				continue
			}
			if cand.DesiredArchived {
				toArchive = append(toArchive, cand)
			} else {
				toUnarchive = append(toUnarchive, cand)
			}

		case SafetyNoOpSynced:
			rep.AlreadySyncedCount++
			removeQueued(cid)
			markHotSynced(cid, cand.DesiredArchived)

		case SafetyNoOpMissing:
			rep.MissingOrSubtrajectoryCount++
			removeQueued(cid)

		case SafetyDeferQueue, SafetyBlockSkip:
			rep.ProtectedSkipped = append(rep.ProtectedSkipped, SidebarSkipDetail{
				ConversationID: cid,
				TaskSlug:       cand.TaskSlug,
				Title:          cand.Title,
				Gate:           dec.Gate,
				Reason:         dec.Reason,
			})
			switch dec.Reason {
			case "trajectory_busy":
				if cand.Source == "compact" || cand.Source == "queue" {
					enqueueOfflineOrTransient(cand, "trajectory_busy")
					if cand.Source == "compact" {
						rep.QueuedCount++
					}
				}
			case "caller_self", "caller_self_deferred":
				if cand.Source == "queue" && !opts.DryRun {
					// Reschedule without burning Attempts so another session/heartbeat drains it once idle.
					sidebarQueueMu.Lock()
					if it, ok := queue.Items[cid]; ok {
						it.NextRetryAt = now.Add(30 * time.Second).UTC().Format(time.RFC3339)
						it.UpdatedAt = nowSecUTC
						queue.Items[cid] = it
						queueMutated = true
					}
					sidebarQueueMu.Unlock()
				}
			default:
				removeQueued(cid)
			}
		}
	}

	if opts.DryRun {
		rep.WouldArchive = toArchive
		rep.WouldUnarchive = toUnarchive
		rep.PendingQueueRemaining = len(queue.Items)
		return rep, nil
	}

	applyChunks := func(cands []SidebarCandidate, desired bool) {
		for start := 0; start < len(cands); start += batchSize {
			end := start + batchSize
			if end > len(cands) {
				end = len(cands)
			}
			chunk := cands[start:end]
			patch := lsConversationPatch{
				Archived:                boolPtr(desired),
				ArchivalStatusTimestamp: nowUTC,
			}

			var singleID string
			var batchIDs []string
			if len(chunk) == 1 {
				singleID = chunk[0].ConversationID
			} else {
				batchIDs = make([]string, len(chunk))
				for i, c := range chunk {
					batchIDs[i] = c.ConversationID
				}
			}

			timeout := DefaultSidebarBatchTimeout
			if len(chunk) == 1 {
				timeout = DefaultSidebarUnaryTimeout
			}

			err := client.UpdateConversationAnnotations(ctx, singleID, batchIDs, patch, timeout)
			if err == nil {
				for _, c := range chunk {
					removeQueued(c.ConversationID)
					markHotSynced(c.ConversationID, desired)
					if desired {
						rep.ArchivedCount++
						rep.ArchivedIDs = append(rep.ArchivedIDs, c.ConversationID)
					} else {
						rep.UnarchivedCount++
						rep.UnarchivedIDs = append(rep.UnarchivedIDs, c.ConversationID)
					}
				}
				continue
			}

			// If a multi-ID chunk failed with a terminal 400/404, isolate per-ID so valid siblings still succeed.
			if len(chunk) > 1 && isTerminalSidebarTargetError(err) {
				for _, c := range chunk {
					uErr := client.UpdateConversationAnnotations(ctx, c.ConversationID, nil, patch, DefaultSidebarUnaryTimeout)
					if uErr == nil {
						removeQueued(c.ConversationID)
						markHotSynced(c.ConversationID, desired)
						if desired {
							rep.ArchivedCount++
							rep.ArchivedIDs = append(rep.ArchivedIDs, c.ConversationID)
						} else {
							rep.UnarchivedCount++
							rep.UnarchivedIDs = append(rep.UnarchivedIDs, c.ConversationID)
						}
					} else if isTerminalSidebarTargetError(uErr) {
						rep.MissingOrSubtrajectoryCount++
						removeQueued(c.ConversationID)
					} else {
						rep.LastError = uErr.Error()
						enqueueOfflineOrTransient(c, uErr.Error())
						rep.QueuedCount++
					}
				}
				continue
			}

			if isTerminalSidebarTargetError(err) {
				for _, c := range chunk {
					rep.MissingOrSubtrajectoryCount++
					removeQueued(c.ConversationID)
				}
				continue
			}

			rep.LastError = err.Error()
			for _, c := range chunk {
				enqueueOfflineOrTransient(c, err.Error())
				rep.QueuedCount++
			}
		}
	}

	applyChunks(toArchive, true)
	applyChunks(toUnarchive, false)

	if hotMutated {
		if data, mErr := json.MarshalIndent(hotTasks, "", "  "); mErr == nil {
			_ = os.WriteFile(tasksFilePath(vaultDir), data, 0o644)
		}
	}
	if queueMutated {
		sidebarQueueMu.Lock()
		_ = saveSidebarSyncQueueUnlocked(vaultDir, queue, now)
		sidebarQueueMu.Unlock()
	}
	rep.PendingQueueRemaining = len(queue.Items)
	return rep, nil
}
