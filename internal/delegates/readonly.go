package delegates

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/authz"
	"github.com/zjshen14/agy-hq/internal/connector"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/redact"
	"github.com/zjshen14/agy-hq/internal/taint"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

const MaxPinnedCommandStdoutBytes = 64 * 1024 // 64 KiB

// ProjectedChangeView is the minimal change projection exposed to authorized scoped delegates.
type ProjectedChangeView struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	NeedsOwner bool   `json:"needs_owner,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

// ProjectedWorkspaceView exposes ONLY safe workspace status fields (no host paths, no BoundTasks,
// no WriterTask, no raw Status.Error).
type ProjectedWorkspaceView struct {
	WorkspaceID string                `json:"workspace_id"`
	Name        string                `json:"name,omitempty"`
	State       string                `json:"state"`
	Changes     []ProjectedChangeView `json:"changes,omitempty"`
}

// CuratedTaskView exposes ONLY explicitly curated Public Status and granted projected workspaces
// (never ActiveTask.LastSummary, private Goal/Completed/NextSteps, transcripts, or thinking).
type CuratedTaskView struct {
	TaskSlug      string                   `json:"task_slug"`
	Status        string                   `json:"status"`
	UpdatedAt     string                   `json:"updated_at,omitempty"`
	PublicStatus  string                   `json:"public_status,omitempty"`
	PublicUpdated string                   `json:"public_updated_at,omitempty"`
	Workspaces    []ProjectedWorkspaceView `json:"workspaces,omitempty"`
}

func resolveCallerGrant(vaultDir, convID string) (authz.SessionPolicy, *taint.SessionGrant, error) {
	cid, _ := taint.ResolveCallerConvID(vaultDir, strings.TrimSpace(convID))
	pol := authz.Resolve(vaultDir, cid, authz.ResolveHints{})
	if pol.Profile != authz.ProfileScopedDelegate && pol.Profile != authz.ProfileClassifier {
		return pol, nil, fmt.Errorf("unauthorized caller profile %q (requires active scoped_delegate or classifier grant)", pol.Profile)
	}
	if pol.Grant == nil {
		return pol, nil, fmt.Errorf("missing active session grant")
	}
	return pol, pol.Grant, nil
}

func stringSliceContains(list []string, target string) bool {
	for _, s := range list {
		if strings.TrimSpace(s) == target {
			return true
		}
	}
	return false
}

func projectWorkspaceEntry(e workspace.LedgerEntry) ProjectedWorkspaceView {
	se := workspace.NewSurfaceEntry(e)
	out := ProjectedWorkspaceView{
		WorkspaceID: se.WorkspaceID,
		Name:        se.Name,
		State:       string(se.State),
	}
	for _, c := range se.Changes {
		out.Changes = append(out.Changes, ProjectedChangeView{
			ID:         c.ID,
			State:      string(c.State),
			NeedsOwner: c.NeedsOwner,
			UpdatedAt:  c.UpdatedAt,
		})
	}
	return out
}

// ReadRequest returns the nonce-fenced untrusted peer request body bound to the caller's active grant.
func ReadRequest(vaultDir, convID, reqID string) (string, error) {
	_, grant, err := resolveCallerGrant(vaultDir, convID)
	if err != nil {
		return "", err
	}
	reqID = strings.TrimSpace(reqID)
	if reqID == "" {
		reqID = grant.RequestID
	}
	if reqID == "" || reqID != grant.RequestID {
		return "", fmt.Errorf("request %q is not bound to this session grant", reqID)
	}
	stored, err := LoadStoredRequest(vaultDir, reqID)
	if err != nil {
		return "", fmt.Errorf("request %q unavailable", reqID)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("request_id: %s\n", stored.RequestID))
	sb.WriteString(fmt.Sprintf("verified_caller: %s\n", stored.CallerID))
	if stored.OriginChannel != "" {
		sb.WriteString(fmt.Sprintf("origin_channel: %s\n", stored.OriginChannel))
	}
	if stored.ReceivedAt != "" {
		sb.WriteString(fmt.Sprintf("received_at: %s\n", stored.ReceivedAt))
	}
	if stored.Subject != "" {
		sb.WriteString(fmt.Sprintf("subject: %s\n", stored.Subject))
	}
	sb.WriteString("\n")
	sb.WriteString(stored.Body)
	return fenceUntrustedPayload("PEER_REQUEST:"+stored.RequestID, sb.String()), nil
}

// ReadTask returns curated public status for a single explicitly granted task slug.
func ReadTask(vaultDir, convID, taskSlug string) (CuratedTaskView, error) {
	pol, grant, err := resolveCallerGrant(vaultDir, convID)
	if err != nil {
		return CuratedTaskView{}, err
	}
	if pol.Profile != authz.ProfileScopedDelegate || !pol.Reads.GrantedTasksPublic {
		return CuratedTaskView{}, fmt.Errorf("task read not permitted in profile %q", pol.Profile)
	}
	taskSlug = strings.TrimSpace(taskSlug)
	if !stringSliceContains(grant.Tasks, taskSlug) {
		return CuratedTaskView{}, fmt.Errorf("task not granted")
	}

	view := buildCuratedTaskView(vaultDir, taskSlug, grant.Workspaces)
	AppendAudit(vaultDir, AuditEntry{
		Action:    "read_task",
		PolicyID:  grant.PolicyID,
		RequestID: grant.RequestID,
		CallerID:  grant.CallerID,
		Outcome:   "ok",
		Detail:    taskSlug,
	})
	return view, nil
}

func buildCuratedTaskView(vaultDir, taskSlug string, allowedWorkspaces []string) CuratedTaskView {
	view := CuratedTaskView{
		TaskSlug: taskSlug,
		Status:   "unknown",
	}
	if tasks, err := kernel.ListTasks(vaultDir); err == nil {
		for _, t := range tasks {
			if strings.EqualFold(strings.TrimSpace(t.TaskSlug), taskSlug) {
				view.Status = strings.TrimSpace(t.Status)
				view.UpdatedAt = strings.TrimSpace(t.LastUpdatedAt)
				break
			}
		}
	}
	if pub, pubUpd, ok, err := vault.LoadPublicStatus(vaultDir, taskSlug); err == nil && ok {
		view.PublicStatus = pub
		view.PublicUpdated = pubUpd
	}
	for _, wsKey := range allowedWorkspaces {
		if e, found := workspace.GetEntry(vaultDir, wsKey); found {
			// Include workspace if explicitly granted and bound to this task (or single-task grant).
			bound := false
			for _, bt := range e.BoundTasks {
				if strings.EqualFold(strings.TrimSpace(bt), taskSlug) {
					bound = true
					break
				}
			}
			if bound || len(allowedWorkspaces) == 1 {
				view.Workspaces = append(view.Workspaces, projectWorkspaceEntry(e))
			}
		}
	}
	return view
}

// ReadWorkspace returns the projected safe view for a single explicitly granted workspace key.
func ReadWorkspace(vaultDir, convID, wsKey string) (ProjectedWorkspaceView, error) {
	pol, grant, err := resolveCallerGrant(vaultDir, convID)
	if err != nil {
		return ProjectedWorkspaceView{}, err
	}
	if pol.Profile != authz.ProfileScopedDelegate || !pol.Reads.GrantedWorkspacesProj {
		return ProjectedWorkspaceView{}, fmt.Errorf("workspace read not permitted in profile %q", pol.Profile)
	}
	wsKey = strings.TrimSpace(wsKey)
	if !stringSliceContains(grant.Workspaces, wsKey) {
		return ProjectedWorkspaceView{}, fmt.Errorf("workspace not granted")
	}
	e, ok := workspace.GetEntry(vaultDir, wsKey)
	if !ok {
		return ProjectedWorkspaceView{}, fmt.Errorf("workspace unavailable")
	}
	view := projectWorkspaceEntry(e)
	AppendAudit(vaultDir, AuditEntry{
		Action:    "read_workspace",
		PolicyID:  grant.PolicyID,
		RequestID: grant.RequestID,
		CallerID:  grant.CallerID,
		Outcome:   "ok",
		Detail:    wsKey,
	})
	return view, nil
}

func buildValidatedCommandArgs(spec CommandSpec, provided map[string]string) ([]string, error) {
	allowedNames := make(map[string]bool, len(spec.Args))
	for _, a := range spec.Args {
		allowedNames[a.Name] = true
	}
	for k := range provided {
		if !allowedNames[k] {
			return nil, fmt.Errorf("unknown command argument %q", k)
		}
	}
	argv := append([]string(nil), spec.Command[1:]...)
	for _, a := range spec.Args {
		val, err := ValidateTypedArgValue(a, provided[a.Name])
		if err != nil {
			return nil, err
		}
		if val == "" {
			continue
		}
		if a.Flag != "" {
			argv = append(argv, a.Flag, val)
		} else {
			argv = append(argv, val)
		}
	}
	return argv, nil
}

func execVerifiedPinnedCommand(ctx context.Context, vaultDir string, spec CommandSpec, argValues map[string]string) ([]byte, error) {
	if len(spec.Command) == 0 || extension.IsBlockedComponentCommand0(spec.Command[0]) {
		return nil, fmt.Errorf("command binary %q is blocked", strings.Join(spec.Command, " "))
	}
	trustSrc := TrustSourceDelegates
	if strings.TrimSpace(spec.SourcePack) != "" {
		trustSrc = "pack:" + strings.TrimSpace(spec.SourcePack)
	}
	execBin, err := extension.VerifyTrustedBinary(vaultDir, trustSrc, spec.Command[0])
	if err != nil {
		return nil, fmt.Errorf("verify pinned binary (%s): %w", trustSrc, err)
	}
	args, err := buildValidatedCommandArgs(spec, argValues)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(effectiveInt(spec.TimeoutMs, DefaultCommandTimeoutMs)) * time.Millisecond
	if timeout > time.Duration(MaxCommandTimeoutMs)*time.Millisecond {
		timeout = time.Duration(MaxCommandTimeoutMs) * time.Millisecond
	}
	stdout, stderr, err := connector.RunSubprocess(ctx, connector.SubprocessSpec{
		Binary:   execBin,
		Args:     args,
		EnvAllow: spec.EnvAllow,
		Timeout:  timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("command failed: %w (stderr: %s)", err, connector.SanitizeStderr(stderr))
	}
	if len(stdout) > MaxPinnedCommandStdoutBytes {
		stdout = append(stdout[:MaxPinnedCommandStdoutBytes], []byte("\n...[truncated to 64 KiB]")...)
	}
	return stdout, nil
}

// RunPinnedCommand executes one SHA-256-pinned readonly command granted to the caller's scoped delegate
// session, enforcing per-request run caps, scrubbing stdout via internal/redact, and nonce-fencing output.
func RunPinnedCommand(ctx context.Context, vaultDir, convID, cmdID string, argPairs map[string]string) (string, error) {
	pol, grant, err := resolveCallerGrant(vaultDir, convID)
	if err != nil {
		return "", err
	}
	if pol.Profile != authz.ProfileScopedDelegate || !pol.Reads.GrantedPinnedCmds {
		return "", fmt.Errorf("command execution not permitted in profile %q", pol.Profile)
	}
	cmdID = strings.TrimSpace(cmdID)
	if !stringSliceContains(grant.Commands, cmdID) {
		return "", fmt.Errorf("command %q not granted", cmdID)
	}
	cfg, _, exists, err := LoadConfig(vaultDir)
	if err != nil || !exists {
		return "", fmt.Errorf("delegate policy unavailable")
	}
	cmdSpec, ok := cfg.Commands[cmdID]
	if !ok {
		return "", fmt.Errorf("command %q not found in policy", cmdID)
	}

	maxRuns := effectiveInt(cfg.Defaults.MaxCommandRunsPerRequest, DefaultMaxCmdRunsPerRequest)
	for _, p := range cfg.Policies {
		if p.ID == grant.PolicyID && p.MaxCommandRunsPerRequest > 0 {
			maxRuns = p.MaxCommandRunsPerRequest
			break
		}
	}
	if maxRuns > MaxCmdRunsPerRequestCeiling {
		maxRuns = MaxCmdRunsPerRequestCeiling
	}

	now := time.Now().UTC()
	var runCount int
	if err := withStateMutate(vaultDir, now, func(st *State) error {
		cur := st.RequestCommandRuns[grant.RequestID]
		if cur >= maxRuns {
			return fmt.Errorf("per-request command run cap reached (%d/%d)", cur, maxRuns)
		}
		st.RequestCommandRuns[grant.RequestID] = cur + 1
		runCount = cur + 1
		return nil
	}); err != nil {
		AppendAudit(vaultDir, AuditEntry{
			Action:    "run",
			PolicyID:  grant.PolicyID,
			RequestID: grant.RequestID,
			CallerID:  grant.CallerID,
			Outcome:   "rate_limited",
			Detail:    fmt.Sprintf("%s: %v", cmdID, err),
		})
		return "", err
	}

	stdout, err := execVerifiedPinnedCommand(ctx, vaultDir, cmdSpec, argPairs)
	if err != nil {
		AppendAudit(vaultDir, AuditEntry{
			Action:    "run",
			PolicyID:  grant.PolicyID,
			RequestID: grant.RequestID,
			CallerID:  grant.CallerID,
			Outcome:   "error",
			Detail:    fmt.Sprintf("%s (run %d/%d): %v", cmdID, runCount, maxRuns, err),
		})
		return "", err
	}

	scrubbed := redact.ScrubForUntrustedOutput(vaultDir, string(stdout))

	// Record arg summary deterministically for audit trail.
	var argKeys []string
	for k := range argPairs {
		argKeys = append(argKeys, k)
	}
	sort.Strings(argKeys)
	AppendAudit(vaultDir, AuditEntry{
		Action:    "run",
		PolicyID:  grant.PolicyID,
		RequestID: grant.RequestID,
		CallerID:  grant.CallerID,
		Outcome:   "ok",
		Detail:    fmt.Sprintf("%s (run %d/%d, args=%s)", cmdID, runCount, maxRuns, strings.Join(argKeys, ",")),
	})
	return fenceUntrustedPayload("COMMAND_OUTPUT:"+cmdID, scrubbed), nil
}
