package delegates

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/authz"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/store"
)

const (
	MaxZone2NudgeBannerBytes = 800
	MaxZone2NudgeItems       = 5
)

// QueuedNudge is a 100% static Go-rendered priority nudge stored at
// <vault>/memory/delegates_queues/<targetSlug>.jsonl (delivery_mode: queue_only, RFC-0026 §7.4).
type QueuedNudge struct {
	ID         string            `json:"id"`
	PolicyID   string            `json:"policy_id"`
	NudgeID    string            `json:"nudge_id"`
	CallerID   string            `json:"caller_id"`
	RequestID  string            `json:"request_id"`
	TargetSlug string            `json:"target_slug"`
	Params     map[string]string `json:"params,omitempty"`
	Body       string            `json:"body"`
	CreatedAt  string            `json:"created_at"`
	AckedAt    string            `json:"acked_at,omitempty"`
}

// NudgeResult reports the outcome of ExecuteNudge (both 0-token fast path and LLM-extracted nudge).
type NudgeResult struct {
	Queued        bool         `json:"queued"`
	PreflightFail bool         `json:"preflight_fail,omitempty"`
	Escalated     bool         `json:"escalated,omitempty"`
	AckSent       bool         `json:"ack_sent,omitempty"`
	TargetSlug    string       `json:"target_slug,omitempty"`
	Nudge         *QueuedNudge `json:"nudge,omitempty"`
	Detail        string       `json:"detail"`
}

func queueFilePath(vaultDir, slug string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(slug))
	if !reValidTaskGrant.MatchString(s) || strings.Contains(s, "..") || strings.ContainsAny(s, `/\`) {
		return "", fmt.Errorf("invalid nudge queue task slug %q", slug)
	}
	return filepath.Join(delegatesQueuesDir(vaultDir), s+".jsonl"), nil
}

func findNudgeSpec(pol PolicySpec, nudgeID string) (NudgeSpec, bool) {
	for _, n := range pol.Grants.Nudges {
		if n.ID == nudgeID {
			return n, true
		}
	}
	return NudgeSpec{}, false
}

func resolveNudgeTargetTask(vaultDir string, pol PolicySpec, nudge NudgeSpec, overrideSlug string) (string, error) {
	tasks, err := kernel.ListTasks(vaultDir)
	if err != nil {
		tasks = nil
	}
	isLive := func(t kernel.ActiveTask) bool {
		st := strings.ToLower(strings.TrimSpace(t.Status))
		return st == "" || st == "active" || st == "idle" || st == "waiting" || st == "running"
	}

	explicit := strings.TrimSpace(overrideSlug)
	if explicit == "" {
		explicit = strings.TrimSpace(nudge.TargetTaskSlug)
	}
	if explicit != "" {
		for _, t := range tasks {
			if strings.EqualFold(strings.TrimSpace(t.TaskSlug), explicit) {
				if !isLive(t) {
					return "", fmt.Errorf("target task %q is %s (not active/idle)", explicit, t.Status)
				}
				return strings.TrimSpace(t.TaskSlug), nil
			}
		}
		// Allow explicit target_task_slug when explicitly bound/configured even before first task turn if no archived match exists.
		return explicit, nil
	}

	scope := strings.TrimSpace(nudge.TargetTaskScope)
	if scope == "" {
		return "", fmt.Errorf("nudge %q has no target_task_slug or target_task_scope", nudge.ID)
	}
	var pinnedMatches []string
	var liveMatches []string
	for _, t := range tasks {
		if !isLive(t) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(t.Scope), scope) || strings.EqualFold(strings.TrimSpace(t.TaskSlug), scope) {
			if t.Pinned {
				pinnedMatches = append(pinnedMatches, t.TaskSlug)
			}
			liveMatches = append(liveMatches, t.TaskSlug)
		}
	}
	if len(pinnedMatches) == 1 {
		return pinnedMatches[0], nil
	}
	if len(pinnedMatches) > 1 {
		return "", fmt.Errorf("ambiguous target_task_scope %q (%d pinned tasks match); run `hq delegate bind-target %s <slug>`", scope, len(pinnedMatches), pol.ID)
	}
	if len(liveMatches) == 1 {
		return liveMatches[0], nil
	}
	if len(liveMatches) == 0 {
		return "", fmt.Errorf("no active/idle task matches target_task_scope %q", scope)
	}
	return "", fmt.Errorf("ambiguous target_task_scope %q (%d live tasks match); run `hq delegate bind-target %s <slug>`", scope, len(liveMatches), pol.ID)
}

// ExecuteNudge validates typed params, enforces pre-preflight rate limits & cooldowns, runs optional
// SHA-256-pinned preflight (discarding output), renders a 100% static Go template with ZERO raw peer prose,
// appends to protected memory/delegates_queues/<slug>.jsonl, and delivers a static ack via Ring-0 egress.
func ExecuteNudge(ctx context.Context, vaultDir, convID, reqID, nudgeID string, params map[string]string, isFastPath bool) (NudgeResult, error) {
	now := time.Now().UTC()
	var policyID string
	if !isFastPath {
		pol, grant, err := resolveCallerGrant(vaultDir, convID)
		if err != nil {
			return NudgeResult{}, err
		}
		if pol.Profile != authz.ProfileScopedDelegate || !pol.Writes.TypedNudgeQueue {
			return NudgeResult{}, fmt.Errorf("nudge not permitted in profile %q", pol.Profile)
		}
		if strings.TrimSpace(reqID) == "" {
			reqID = grant.RequestID
		}
		if reqID != grant.RequestID {
			return NudgeResult{}, fmt.Errorf("request %q is not bound to this session grant", reqID)
		}
		if !stringSliceContains(grant.NudgeIDs, nudgeID) {
			return NudgeResult{}, fmt.Errorf("nudge %q not granted", nudgeID)
		}
		policyID = grant.PolicyID
	}

	stored, err := LoadStoredRequest(vaultDir, reqID)
	if err != nil {
		return NudgeResult{}, err
	}
	if policyID == "" {
		policyID = stored.PolicyID
	}
	cfg, _, exists, err := LoadConfig(vaultDir)
	if err != nil || !exists {
		return NudgeResult{}, fmt.Errorf("delegate policy unavailable")
	}
	polSpec, ok := findPolicyByID(cfg, policyID)
	if !ok {
		return NudgeResult{}, fmt.Errorf("policy %q not found", policyID)
	}
	nudgeSpec, ok := findNudgeSpec(polSpec, nudgeID)
	if !ok {
		return NudgeResult{}, fmt.Errorf("nudge %q not found in policy %q", nudgeID, polSpec.ID)
	}
	if nudgeSpec.DeliveryMode != "queue_only" {
		return NudgeResult{}, fmt.Errorf("unsupported delivery_mode %q (v1 supports queue_only exclusively)", nudgeSpec.DeliveryMode)
	}

	// Validate typed parameters strictly before touching preflight or queues.
	allowedParamNames := make(map[string]bool, len(nudgeSpec.Params))
	for _, p := range nudgeSpec.Params {
		allowedParamNames[p.Name] = true
	}
	for k := range params {
		if !allowedParamNames[k] {
			return NudgeResult{}, fmt.Errorf("undeclared nudge param %q", k)
		}
	}
	validated := make(map[string]string, len(nudgeSpec.Params))
	for _, p := range nudgeSpec.Params {
		v, vErr := ValidateTypedArgValue(p, params[p.Name])
		if vErr != nil {
			return NudgeResult{}, vErr
		}
		validated[p.Name] = v
	}

	// PRE-PREFLIGHT rate limit & cooldown enforcement under store.RankDelegates (prevents existence oracle probing!).
	nudgeKey := polSpec.ID + "/" + nudgeSpec.ID
	coolKey := nudgeKey + "/" + stored.CallerID
	cutoff := now.Add(-1 * time.Hour)
	var overrideSlug string
	if err := withStateMutate(vaultDir, now, func(st *State) error {
		if prev, done := st.CompletedRequests[stored.RequestID]; done && prev != "" {
			return fmt.Errorf("request %q already finalized (%s)", stored.RequestID, prev)
		}
		overrideSlug = st.TargetOverrides[polSpec.ID]
		if untilStr := st.NudgeCooldownUntil[coolKey]; untilStr != "" {
			if untilT, tErr := time.Parse(time.RFC3339, untilStr); tErr == nil && now.Before(untilT) {
				return fmt.Errorf("nudge cooldown active until %s", untilStr)
			}
		}
		recent := pruneTimestampsWithin(st.NudgeHistory[nudgeKey], cutoff)
		st.NudgeHistory[nudgeKey] = recent
		if nudgeSpec.MaxNudgesPerHour > 0 && len(recent) >= nudgeSpec.MaxNudgesPerHour {
			return fmt.Errorf("nudge hourly rate limit reached (%d/%d)", len(recent), nudgeSpec.MaxNudgesPerHour)
		}
		// Reserve cooldown & hourly slot BEFORE executing preflight so probing burns budget!
		st.NudgeHistory[nudgeKey] = append(recent, now.Format(time.RFC3339))
		if nudgeSpec.CooldownSeconds > 0 {
			st.NudgeCooldownUntil[coolKey] = now.Add(time.Duration(nudgeSpec.CooldownSeconds) * time.Second).Format(time.RFC3339)
		}
		return nil
	}); err != nil {
		AppendAudit(vaultDir, AuditEntry{
			Action:    "nudge",
			PolicyID:  polSpec.ID,
			RequestID: stored.RequestID,
			CallerID:  stored.CallerID,
			Outcome:   "rate_limited",
			Detail:    err.Error(),
		})
		return NudgeResult{}, err
	}

	// Optional SHA-256-pinned preflight command (discard stdout/stderr; uniform opaque handling on failure).
	if nudgeSpec.PreflightCommand != "" {
		cmdSpec, hasCmd := cfg.Commands[nudgeSpec.PreflightCommand]
		if !hasCmd {
			return NudgeResult{}, fmt.Errorf("preflight_command %q not found", nudgeSpec.PreflightCommand)
		}
		preflightArgs := make(map[string]string)
		for _, a := range cmdSpec.Args {
			if v, ok := validated[a.Name]; ok {
				preflightArgs[a.Name] = v
			}
		}
		_, pfErr := execVerifiedPinnedCommand(ctx, vaultDir, cmdSpec, preflightArgs)
		if pfErr != nil {
			AppendAudit(vaultDir, AuditEntry{
				Action:    "nudge",
				PolicyID:  polSpec.ID,
				RequestID: stored.RequestID,
				CallerID:  stored.CallerID,
				Outcome:   "preflight_failed",
				Detail:    fmt.Sprintf("preflight %s non-zero exit", nudgeSpec.PreflightCommand),
			})
			if nudgeSpec.OnPreflightFail == "reply_uniform" && strings.TrimSpace(string(nudgeSpec.PreflightFailReplyTemplate)) != "" {
				uniformBody, rErr := RenderSafeTemplate(string(nudgeSpec.PreflightFailReplyTemplate), TemplateData{
					CallerID:  stored.CallerID,
					PolicyID:  polSpec.ID,
					RequestID: stored.RequestID,
				})
				if rErr == nil && uniformBody != "" {
					_, _ = SendReply(ctx, vaultDir, convID, stored.RequestID, uniformBody, nil, true)
					return NudgeResult{
						PreflightFail: true,
						AckSent:       true,
						Detail:        "Preflight check failed; delivered uniform opaque reply.",
					}, nil
				}
			}
			_, _ = EscalateRequest(ctx, vaultDir, convID, stored.RequestID, "missing_info", "Delegated nudge preflight check did not match an open assigned item.", true)
			return NudgeResult{
				PreflightFail: true,
				Escalated:     true,
				Detail:        "Preflight check failed; escalated to Front Desk.",
			}, nil
		}
	}

	// Resolve target task (fail closed to Front Desk escalation if missing/archived/ambiguous).
	targetSlug, targetErr := resolveNudgeTargetTask(vaultDir, polSpec, nudgeSpec, overrideSlug)
	if targetErr != nil {
		_, _ = EscalateRequest(ctx, vaultDir, convID, stored.RequestID, "ambiguous", fmt.Sprintf("Could not resolve target task for nudge %s: %v", nudgeSpec.ID, targetErr), true)
		return NudgeResult{
			Escalated: true,
			Detail:    fmt.Sprintf("Target resolution failed (%v); escalated to Front Desk.", targetErr),
		}, nil
	}

	// Render 100% static Go template (ZERO raw peer prose!).
	rendered, err := RenderSafeTemplate(string(nudgeSpec.Template), TemplateData{
		CallerID:  stored.CallerID,
		PolicyID:  polSpec.ID,
		RequestID: stored.RequestID,
		Params:    validated,
	})
	if err != nil {
		return NudgeResult{}, fmt.Errorf("render nudge template: %w", err)
	}

	qPath, err := queueFilePath(vaultDir, targetSlug)
	if err != nil {
		return NudgeResult{}, err
	}
	qn := QueuedNudge{
		ID:         "nudge-" + randomHexToken(6),
		PolicyID:   polSpec.ID,
		NudgeID:    nudgeSpec.ID,
		CallerID:   stored.CallerID,
		RequestID:  stored.RequestID,
		TargetSlug: targetSlug,
		Params:     validated,
		Body:       rendered,
		CreatedAt:  now.Format(time.RFC3339),
	}
	lineBytes, err := json.Marshal(qn)
	if err != nil {
		return NudgeResult{}, err
	}

	// Single leaf lock critical section under store.RankDelegates (66): append queue item.
	if err := store.With(context.Background(), store.RankDelegates, delegatesLockPath(vaultDir), func(_ context.Context) error {
		if mkErr := os.MkdirAll(filepath.Dir(qPath), 0o700); mkErr != nil {
			return mkErr
		}
		f, oErr := os.OpenFile(qPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if oErr != nil {
			return oErr
		}
		_, wErr := f.Write(append(lineBytes, '\n'))
		cErr := f.Close()
		if wErr != nil {
			return wErr
		}
		return cErr
	}); err != nil {
		return NudgeResult{}, err
	}

	// Deliver optional static ack reply via Ring-0 Egress Gateway (which also marks request completed).
	ackSent := false
	if strings.TrimSpace(string(nudgeSpec.AckReplyTemplate)) != "" {
		ackBody, aErr := RenderSafeTemplate(string(nudgeSpec.AckReplyTemplate), TemplateData{
			CallerID:  stored.CallerID,
			PolicyID:  polSpec.ID,
			RequestID: stored.RequestID,
			Params:    validated,
		})
		if aErr == nil && ackBody != "" {
			res, sErr := SendReply(ctx, vaultDir, convID, stored.RequestID, ackBody, nil, true)
			if sErr == nil && res.Sent {
				ackSent = true
			}
		}
	}
	if !ackSent {
		_ = withStateMutate(vaultDir, now, func(st *State) error {
			st.CompletedRequests[stored.RequestID] = "nudged"
			return nil
		})
		stored.Status = "nudged"
		_ = SaveStoredRequest(vaultDir, stored)
		_, _, _ = outbox.Add(vaultDir, outbox.OutboxItem{
			SourceSkill: "scoped-delegate",
			Scope:       polSpec.ID,
			Tier:        outbox.TierBriefRollup,
			Title:       fmt.Sprintf("Queued priority nudge for %s (%s)", targetSlug, stored.CallerID),
			Summary:     fmt.Sprintf("Queued %s (%s) from verified caller %s for task %s.", qn.ID, nudgeSpec.ID, stored.CallerID, targetSlug),
			Tainted:     true,
			Connector:   stored.SourceFQID,
			ThreadKey:   stored.OriginThread,
		}, now)
	}

	AppendAudit(vaultDir, AuditEntry{
		Action:    "nudge",
		PolicyID:  polSpec.ID,
		RequestID: stored.RequestID,
		CallerID:  stored.CallerID,
		Outcome:   "queued",
		Detail:    fmt.Sprintf("%s -> task %s (fast_path=%v, ack_sent=%v)", qn.ID, targetSlug, isFastPath, ackSent),
	})
	return NudgeResult{
		Queued:     true,
		AckSent:    ackSent,
		TargetSlug: targetSlug,
		Nudge:      &qn,
		Detail:     fmt.Sprintf("Queued static priority nudge %s for task %q (delivery_mode=queue_only).", qn.ID, targetSlug),
	}, nil
}

func readQueueFileUnlocked(path string) ([]QueuedNudge, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []QueuedNudge
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8192), 256*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var q QueuedNudge
		if json.Unmarshal([]byte(line), &q) == nil && q.ID != "" {
			out = append(out, q)
		}
	}
	return out, nil
}

// ListPendingNudges returns all unacknowledged QueuedNudge items for taskSlug (or all tasks if taskSlug == "").
func ListPendingNudges(vaultDir, taskSlug string) ([]QueuedNudge, error) {
	if strings.TrimSpace(taskSlug) != "" {
		p, err := queueFilePath(vaultDir, taskSlug)
		if err != nil {
			return nil, err
		}
		all, err := readQueueFileUnlocked(p)
		if err != nil {
			return nil, err
		}
		var pending []QueuedNudge
		for _, q := range all {
			if strings.TrimSpace(q.AckedAt) == "" {
				pending = append(pending, q)
			}
		}
		return pending, nil
	}
	entries, err := os.ReadDir(delegatesQueuesDir(vaultDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var pending []QueuedNudge
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".jsonl") {
			continue
		}
		items, _ := readQueueFileUnlocked(filepath.Join(delegatesQueuesDir(vaultDir), ent.Name()))
		for _, q := range items {
			if strings.TrimSpace(q.AckedAt) == "" {
				pending = append(pending, q)
			}
		}
	}
	return pending, nil
}

// AckNudge acknowledges one nudge by ID or all nudges for taskSlug (interactive owner only).
func AckNudge(vaultDir, nudgeID, taskSlug string, all bool) (int, error) {
	nudgeID = strings.TrimSpace(nudgeID)
	taskSlug = strings.TrimSpace(taskSlug)
	if nudgeID == "" && (!all || taskSlug == "") {
		return 0, fmt.Errorf("specify <nudge-id> or --task=<slug> --all")
	}
	nowStr := time.Now().UTC().Format(time.RFC3339)
	acked := 0

	err := store.With(context.Background(), store.RankDelegates, delegatesLockPath(vaultDir), func(_ context.Context) error {
		var files []string
		if taskSlug != "" {
			p, pErr := queueFilePath(vaultDir, taskSlug)
			if pErr != nil {
				return pErr
			}
			files = []string{p}
		} else {
			entries, rErr := os.ReadDir(delegatesQueuesDir(vaultDir))
			if rErr != nil {
				if os.IsNotExist(rErr) {
					return nil
				}
				return rErr
			}
			for _, ent := range entries {
				if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".jsonl") {
					files = append(files, filepath.Join(delegatesQueuesDir(vaultDir), ent.Name()))
				}
			}
		}

		for _, fpath := range files {
			items, rErr := readQueueFileUnlocked(fpath)
			if rErr != nil || len(items) == 0 {
				continue
			}
			changed := false
			var kept [][]byte
			for _, it := range items {
				if strings.TrimSpace(it.AckedAt) == "" && ((all && taskSlug != "") || (nudgeID != "" && it.ID == nudgeID)) {
					it.AckedAt = nowStr
					acked++
					changed = true
					continue // compact acknowledged entries out of the live queue file
				}
				if strings.TrimSpace(it.AckedAt) == "" {
					if b, mErr := json.Marshal(it); mErr == nil {
						kept = append(kept, b)
					}
				}
			}
			if !changed {
				continue
			}
			if len(kept) == 0 {
				_ = os.Remove(fpath)
			} else {
				var out []byte
				for _, b := range kept {
					out = append(out, b...)
					out = append(out, '\n')
				}
				if wErr := store.WriteAtomic(fpath, out, 0o600); wErr != nil {
					return wErr
				}
			}
		}
		return nil
	})
	return acked, err
}

// SweepOrphanQueues deletes queue files in memory/delegates_queues/ whose task slug is not in activeSlugs.
func SweepOrphanQueues(vaultDir string, activeSlugs map[string]bool) int {
	swept := 0
	_ = store.With(context.Background(), store.RankDelegates, delegatesLockPath(vaultDir), func(_ context.Context) error {
		entries, err := os.ReadDir(delegatesQueuesDir(vaultDir))
		if err != nil {
			return nil
		}
		for _, ent := range entries {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".jsonl") {
				continue
			}
			slug := strings.ToLower(strings.TrimSuffix(ent.Name(), ".jsonl"))
			if !activeSlugs[slug] {
				if os.Remove(filepath.Join(delegatesQueuesDir(vaultDir), ent.Name())) == nil {
					swept++
				}
			}
		}
		return nil
	})
	return swept
}

// FormatZone2NudgeBanner formats pending queue_only nudges for SessionStart Zone 2
// (<= 800B, max 5 items + overflow footer; never injected in leanAuto or tainted sessions).
func FormatZone2NudgeBanner(vaultDir, taskSlug string) string {
	if strings.TrimSpace(vaultDir) == "" || strings.TrimSpace(taskSlug) == "" {
		return ""
	}
	pending, err := ListPendingNudges(vaultDir, taskSlug)
	if err != nil || len(pending) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## 📬 Queued Peer Priority Nudges (`%s` · %d pending)\n", taskSlug, len(pending)))
	shown := 0
	for _, q := range pending {
		if shown >= MaxZone2NudgeItems {
			break
		}
		oneLine := strings.Join(strings.Fields(q.Body), " ")
		entry := fmt.Sprintf("- **`%s`** (`%s`): %s\n", q.ID, q.CallerID, oneLine)
		if sb.Len()+len(entry)+90 > MaxZone2NudgeBannerBytes {
			break
		}
		sb.WriteString(entry)
		shown++
	}
	if rem := len(pending) - shown; rem > 0 {
		sb.WriteString(fmt.Sprintf("- *(+%d more queued nudge(s))*\n", rem))
	}
	sb.WriteString(fmt.Sprintf("- Clear after reviewing: `hq delegate ack-nudge --task=%s --all`", taskSlug))
	res := sb.String()
	if len(res) > MaxZone2NudgeBannerBytes {
		res = res[:MaxZone2NudgeBannerBytes]
	}
	return res
}
