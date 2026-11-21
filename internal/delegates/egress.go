package delegates

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/authz"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/redact"
	"github.com/zjshen14/agy-hq/internal/vault"
)

const MaxEscalationReasonRunes = 240

var (
	reProposalTokenAntiCue = regexp.MustCompile(`\bprop-[A-Za-z0-9_-]+\b`)
	reMarkdownLinkAntiCue  = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	validEscalateCats      = map[string]bool{
		"out_of_scope":   true,
		"missing_info":   true,
		"needs_approval": true,
		"uncertain":      true,
		"ambiguous":      true,
	}
)

// EgressScanResult describes the outcome of Ring-0 defense-in-depth egress inspection.
type EgressScanResult struct {
	Passed      bool     `json:"passed"`
	CleanedText string   `json:"cleaned_text"`
	Violations  []string `json:"violations,omitempty"`
}

// ReplyResult reports the outcome of SendReply (`hq delegate reply`).
type ReplyResult struct {
	Sent       bool                `json:"sent"`
	Staged     bool                `json:"staged"`
	Escalated  bool                `json:"escalated"`
	RequestID  string              `json:"request_id"`
	Violations []string            `json:"violations,omitempty"`
	Proposal   *approvals.Proposal `json:"proposal,omitempty"`
	Detail     string              `json:"detail"`
}

// ReplyApplyBundle is piped via /dev/fd/3 into `hq delegate apply-reply` after human approval.
type ReplyApplyBundle struct {
	SchemaVersion int    `json:"schema_version"`
	RequestID     string `json:"request_id"`
	PolicyID      string `json:"policy_id"`
	SourceFQID    string `json:"source_fqid"`
	CallerID      string `json:"caller_id"`
	OriginThread  string `json:"origin_thread"`
	Body          string `json:"body"`
}

// DecodeReplyBody resolves --body, repeatable --line, or RFC-4648 --body-b64 (std, URL, or unpadded)
// without requiring write_file in tainted delegate sessions (RFC-0026 §7.5).
func DecodeReplyBody(body string, lines []string, bodyB64 string) (string, error) {
	modes := 0
	if strings.TrimSpace(body) != "" {
		modes++
	}
	if len(lines) > 0 {
		modes++
	}
	if strings.TrimSpace(bodyB64) != "" {
		modes++
	}
	if modes == 0 {
		return "", fmt.Errorf("reply body is required (--body, --line, or --body-b64)")
	}
	if modes > 1 {
		return "", fmt.Errorf("specify only one of --body, --line, or --body-b64")
	}
	if len(lines) > 0 {
		return strings.Join(lines, "\n"), nil
	}
	if strings.TrimSpace(bodyB64) != "" {
		raw := strings.TrimSpace(bodyB64)
		for _, enc := range []*base64.Encoding{
			base64.StdEncoding,
			base64.RawStdEncoding,
			base64.URLEncoding,
			base64.RawURLEncoding,
		} {
			if dec, err := enc.DecodeString(raw); err == nil {
				if !utf8.Valid(dec) {
					return "", fmt.Errorf("--body-b64 decoded payload is not valid UTF-8")
				}
				return string(dec), nil
			}
		}
		return "", fmt.Errorf("invalid base64 payload in --body-b64")
	}
	return body, nil
}

func wordBoundaryMatchCI(haystack, needle string) bool {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return false
	}
	re, err := regexp.Compile(`(?i)(?:^|[^a-z0-9_-])` + regexp.QuoteMeta(needle) + `(?:$|[^a-z0-9_-])`)
	if err != nil {
		return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
	}
	return re.MatchString(haystack)
}

// ScanEgressText executes the Ring-0 defense-in-depth egress gateway checks (RFC-0026 §7.5):
//  1. Strip Unicode Cf / bidi overrides & control chars.
//  2. Enforce max_body_runes cap.
//  3. Check internal/redact violations (secrets, harness tokens, home/vault paths, pii-denylist.txt).
//  4. Reject high-entropy / base64 encoded blobs not present verbatim in grantedInputs.
//  5. Reject non-granted task slugs (high-precision word-boundary check) + defaults.sensitive_terms.
//  6. Reject defaults.blocked_patterns + policy.egress.blocked_patterns commitments.
func ScanEgressText(vaultDir string, cfg Config, pol PolicySpec, rawText string, grantedInputs []string) EgressScanResult {
	cleaned := strings.TrimSpace(redact.StripInvisibleUnicode(rawText))
	if cleaned == "" {
		return EgressScanResult{Passed: false, Violations: []string{"empty_body"}}
	}

	var violations []string
	maxRunes := effectiveInt(pol.Egress.MaxBodyRunes, DefaultMaxBodyRunes)
	if utf8.RuneCountInString(cleaned) > maxRunes {
		violations = append(violations, fmt.Sprintf("body_too_long:%d>%d", utf8.RuneCountInString(cleaned), maxRunes))
	}

	for _, v := range redact.FindViolations(vaultDir, cleaned, nil) {
		violations = append(violations, v.Kind+":"+v.Detail)
	}

	if blob, ok := redact.HasHighEntropyBlob(cleaned, grantedInputs); ok {
		violations = append(violations, "high_entropy_blob:"+blob)
	}

	// Sensitive terms
	for _, term := range cfg.Defaults.SensitiveTerms {
		t := strings.TrimSpace(term)
		if t != "" && wordBoundaryMatchCI(cleaned, t) {
			violations = append(violations, "sensitive_term:"+t)
		}
	}

	// High-precision non-granted task slug check
	grantedTaskSet := make(map[string]bool, len(pol.Grants.Tasks))
	grantedTokens := make(map[string]bool)
	for _, gt := range pol.Grants.Tasks {
		low := strings.ToLower(strings.TrimSpace(gt))
		grantedTaskSet[low] = true
		for _, tok := range strings.FieldsFunc(low, func(r rune) bool { return r == '-' || r == '_' }) {
			if len(tok) >= 4 {
				grantedTokens[tok] = true
			}
		}
	}
	if strings.TrimSpace(vaultDir) != "" {
		if tasks, err := kernel.ListTasks(vaultDir); err == nil {
			for _, t := range tasks {
				slug := strings.ToLower(strings.TrimSpace(t.TaskSlug))
				if slug == "" || grantedTaskSet[slug] || grantedTokens[slug] {
					continue
				}
				if len(slug) < 6 && !strings.ContainsAny(slug, "-_") {
					continue
				}
				if wordBoundaryMatchCI(cleaned, slug) {
					violations = append(violations, "non_granted_task:"+slug)
				}
			}
		}
	}

	// Commitment / blocked_patterns
	allBlocked := make([]string, 0, len(cfg.Defaults.BlockedPatterns)+len(pol.Egress.BlockedPatterns))
	allBlocked = append(allBlocked, cfg.Defaults.BlockedPatterns...)
	allBlocked = append(allBlocked, pol.Egress.BlockedPatterns...)
	for _, pat := range allBlocked {
		re, err := regexp.Compile(pat)
		if err != nil {
			violations = append(violations, "invalid_blocked_pattern")
			continue
		}
		if re.MatchString(cleaned) {
			violations = append(violations, "blocked_pattern:"+pat)
		}
	}

	return EgressScanResult{
		Passed:      len(violations) == 0,
		CleanedText: cleaned,
		Violations:  violations,
	}
}

func findPolicyByID(cfg Config, id string) (PolicySpec, bool) {
	for _, p := range cfg.Policies {
		if p.ID == id {
			return p, true
		}
	}
	return PolicySpec{}, false
}

// resolveBoundWorkerRequest checks whether convID is an active `spawn_sandboxed_worker`
// unattended_auto session leased to a `delegate_action` event for reqID (or binds reqID
// when omitted), granting that worker a single Ring-0-filtered reply/escalation strictly
// for its own originating peer request.
func resolveBoundWorkerRequest(vaultDir, convID, reqID string) (boundReqID, policyID, callerID string, ok bool) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return "", "", "", false
	}
	leased, err := events.ListLeasedByConversation(vaultDir, convID)
	if err != nil || len(leased) == 0 {
		return "", "", "", false
	}
	wantReq := strings.TrimSpace(reqID)
	for _, ev := range leased {
		if ev.Source != "delegate_action" || ev.Metadata["delivery_mode"] != "spawn_sandboxed_worker" {
			continue
		}
		evReq := strings.TrimSpace(ev.Metadata["request_id"])
		evPol := strings.TrimSpace(ev.Metadata["policy_id"])
		if evReq == "" || evPol == "" {
			continue
		}
		if wantReq != "" && wantReq != evReq {
			continue
		}
		return evReq, evPol, strings.TrimSpace(ev.Metadata["caller_id"]), true
	}
	return "", "", "", false
}

// SendWorkerAck delivers a static initial acknowledgement for a `spawn_sandboxed_worker`
// request without closing CompletedRequests, keeping the single final Ring-0-filtered reply
// slot open for the spawned worker conversation.
func SendWorkerAck(ctx context.Context, vaultDir, convID, reqID, rawBody string) (ReplyResult, error) {
	return sendReplyInternal(ctx, vaultDir, convID, reqID, rawBody, nil, true, true)
}

// SendReply enforces single-reply-per-request, sliding-window rate limits, structured_template rendering,
// and Ring-0 Egress Gateway scanning, then delivers via origin-bound transport.reply or fails closed.
// Callable by a bound `scoped_delegate` session OR by the exact `unattended_auto` worker spawned for reqID.
func SendReply(ctx context.Context, vaultDir, convID, reqID, rawBody string, structuredParams map[string]string, isInternalAuto bool) (ReplyResult, error) {
	return sendReplyInternal(ctx, vaultDir, convID, reqID, rawBody, structuredParams, isInternalAuto, false)
}

func sendReplyInternal(ctx context.Context, vaultDir, convID, reqID, rawBody string, structuredParams map[string]string, isInternalAuto, isWorkerAck bool) (ReplyResult, error) {
	now := time.Now().UTC()
	var callerPolicyID, callerID string
	if !isInternalAuto {
		pol, grant, err := resolveCallerGrant(vaultDir, convID)
		if err == nil {
			if pol.Profile != authz.ProfileScopedDelegate || !pol.Egress.EgressFilteredReply {
				return ReplyResult{}, fmt.Errorf("reply not permitted in profile %q (egress=%s)", pol.Profile, grant.EgressMode)
			}
			if strings.TrimSpace(reqID) == "" {
				reqID = grant.RequestID
			}
			if reqID != grant.RequestID {
				return ReplyResult{}, fmt.Errorf("request %q is not bound to this session grant", reqID)
			}
			callerPolicyID = grant.PolicyID
			callerID = grant.CallerID
		} else if boundReq, boundPol, boundCaller, ok := resolveBoundWorkerRequest(vaultDir, convID, reqID); ok {
			reqID = boundReq
			callerPolicyID = boundPol
			callerID = boundCaller
		} else {
			return ReplyResult{}, err
		}
	}

	stored, err := LoadStoredRequest(vaultDir, reqID)
	if err != nil {
		return ReplyResult{}, fmt.Errorf("load request %q: %w", reqID, err)
	}
	if callerPolicyID == "" {
		callerPolicyID = stored.PolicyID
	}
	if callerID == "" {
		callerID = stored.CallerID
	}
	cfg, _, exists, err := LoadConfig(vaultDir)
	if err != nil || !exists {
		return ReplyResult{}, fmt.Errorf("delegate policy unavailable")
	}
	polSpec, ok := findPolicyByID(cfg, callerPolicyID)
	if !ok {
		return ReplyResult{}, fmt.Errorf("policy %q not found", callerPolicyID)
	}

	// Render structured_template if required (LLM cannot inject arbitrary prose).
	bodyToSend := rawBody
	if !isInternalAuto && polSpec.Egress.Mode == "structured_template" {
		validated := make(map[string]string, len(polSpec.Egress.StructuredFields))
		for _, f := range polSpec.Egress.StructuredFields {
			v, vErr := ValidateTypedArgValue(f, structuredParams[f.Name])
			if vErr != nil {
				return ReplyResult{}, fmt.Errorf("structured_template field %q: %w", f.Name, vErr)
			}
			validated[f.Name] = v
		}
		rendered, rErr := RenderSafeTemplate(string(polSpec.Egress.StructuredTemplate), TemplateData{
			CallerID:  stored.CallerID,
			PolicyID:  polSpec.ID,
			RequestID: stored.RequestID,
			Params:    validated,
		})
		if rErr != nil {
			return ReplyResult{}, fmt.Errorf("render structured_template: %w", rErr)
		}
		bodyToSend = rendered
	}

	// Gather granted inputs for high-entropy allowlist (e.g. curated Public Status strings).
	var grantedInputs []string
	for _, slug := range polSpec.Grants.Tasks {
		grantedInputs = append(grantedInputs, slug)
		if pub, _, ok, _ := vault.LoadPublicStatus(vaultDir, slug); ok {
			grantedInputs = append(grantedInputs, pub)
		}
	}

	scan := ScanEgressText(vaultDir, cfg, polSpec, bodyToSend, grantedInputs)

	// Check single-reply & sliding-window rate limits under RankDelegates = 66.
	rateViolation := ""
	alreadyDone := ""
	globalMax := effectiveInt(cfg.Defaults.GlobalMaxRepliesPerHour, DefaultGlobalMaxRepliesPerHour)
	polMax := effectiveInt(polSpec.Egress.MaxRepliesPerHour, DefaultPolicyMaxRepliesPerHour)
	cutoff := now.Add(-1 * time.Hour)

	if err := withStateMutate(vaultDir, now, func(st *State) error {
		if prev, done := st.CompletedRequests[stored.RequestID]; done && prev != "" {
			alreadyDone = prev
			return nil
		}
		st.GlobalReplyTimes = pruneTimestampsWithin(st.GlobalReplyTimes, cutoff)
		st.PolicyReplyTimes[polSpec.ID] = pruneTimestampsWithin(st.PolicyReplyTimes[polSpec.ID], cutoff)
		if len(st.GlobalReplyTimes) >= globalMax {
			rateViolation = fmt.Sprintf("global_rate_limit:%d/%d", len(st.GlobalReplyTimes), globalMax)
			return nil
		}
		if len(st.PolicyReplyTimes[polSpec.ID]) >= polMax {
			rateViolation = fmt.Sprintf("policy_rate_limit:%d/%d", len(st.PolicyReplyTimes[polSpec.ID]), polMax)
			return nil
		}
		return nil
	}); err != nil {
		return ReplyResult{}, err
	}
	if alreadyDone != "" {
		return ReplyResult{}, fmt.Errorf("request %q already finalized (%s); single reply per request enforced", stored.RequestID, alreadyDone)
	}
	if rateViolation != "" {
		scan.Passed = false
		scan.Violations = append(scan.Violations, rateViolation)
	}

	// Decide delivery vs fail-closed staging/escalation.
	canAutoSend := scan.Passed && (isInternalAuto || polSpec.Egress.Mode == "auto_reply" || polSpec.Egress.Mode == "structured_template")
	if !canAutoSend {
		failMode := polSpec.Egress.FailMode
		if failMode == "" {
			failMode = "stage_approval"
		}
		reasonSummary := "policy requires stage_approval"
		if len(scan.Violations) > 0 {
			reasonSummary = "egress guard tripped: " + strings.Join(scan.Violations, ", ")
		}

		if failMode == "stage_approval" || polSpec.Egress.Mode == "stage_approval" {
			bundle := ReplyApplyBundle{
				SchemaVersion: PolicySchemaVersion,
				RequestID:     stored.RequestID,
				PolicyID:      polSpec.ID,
				SourceFQID:    stored.SourceFQID,
				CallerID:      stored.CallerID,
				OriginThread:  stored.OriginThread,
				Body:          scan.CleanedText,
			}
			payloadBytes, err := json.MarshalIndent(bundle, "", "  ")
			if err != nil {
				return ReplyResult{}, err
			}
			exe := extension.ResolveSelfHQExecutable()
			prop, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
				ActionType:     "rsi_apply",
				Summary:        fmt.Sprintf("Send delegate reply to %s (%s, %s)", stored.CallerID, polSpec.ID, reasonSummary),
				CommandArgv:    []string{exe, "delegate", "apply-reply", "--payload-file={{payload_file}}"},
				ConversationID: convID,
				PayloadBytes:   append(payloadBytes, '\n'),
				Metadata: map[string]string{
					"bodyPreview":  scan.CleanedText,
					"draftPayload": scan.CleanedText,
					"requestId":    stored.RequestID,
					"policyId":     polSpec.ID,
				},
			})
			if err != nil {
				return ReplyResult{}, err
			}
			_ = withStateMutate(vaultDir, now, func(st *State) error {
				st.CompletedRequests[stored.RequestID] = "staged_approval"
				return nil
			})
			stored.Status = "staged_approval"
			_ = SaveStoredRequest(vaultDir, stored)
			_, _, _ = outbox.Add(vaultDir, outbox.OutboxItem{
				SourceSkill: "scoped-delegate",
				Scope:       polSpec.ID,
				Tier:        outbox.TierInterrupt,
				Title:       fmt.Sprintf("Delegate reply held for approval (%s · %s)", polSpec.ID, stored.CallerID),
				Summary:     fmt.Sprintf("[DELEGATE REPLY HELD — %s] Staged %s (sha256:%s) for request %s.", reasonSummary, prop.ID, prop.SHA256[:12], stored.RequestID),
				ProposalIDs: []string{prop.ID},
				Tainted:     true,
				Connector:   stored.SourceFQID,
				ThreadKey:   stored.OriginThread,
			}, now)
			AppendAudit(vaultDir, AuditEntry{
				Action:    "reply_blocked",
				PolicyID:  polSpec.ID,
				RequestID: stored.RequestID,
				CallerID:  stored.CallerID,
				Outcome:   "staged_approval",
				Detail:    fmt.Sprintf("%s -> %s", reasonSummary, prop.ID),
			})
			return ReplyResult{
				Staged:     true,
				RequestID:  stored.RequestID,
				Violations: scan.Violations,
				Proposal:   &prop,
				Detail:     fmt.Sprintf("Reply held (%s); staged 1-tap approval %s.", reasonSummary, prop.ID),
			}, nil
		}

		// fail_mode == "escalate"
		_, _, _ = outbox.Add(vaultDir, outbox.OutboxItem{
			SourceSkill: "scoped-delegate",
			Scope:       polSpec.ID,
			Tier:        outbox.TierInterrupt,
			Title:       fmt.Sprintf("Delegate reply blocked (%s · %s)", polSpec.ID, stored.CallerID),
			Summary:     fmt.Sprintf("[UNTRUSTED PEER ESCALATION — verified caller: %s] Reply blocked by Ring-0 egress gateway (%s) for request %s.", stored.CallerID, reasonSummary, stored.RequestID),
			Tainted:     true,
			Connector:   stored.SourceFQID,
			ThreadKey:   stored.OriginThread,
		}, now)
		_ = withStateMutate(vaultDir, now, func(st *State) error {
			st.CompletedRequests[stored.RequestID] = "escalated"
			return nil
		})
		stored.Status = "escalated"
		_ = SaveStoredRequest(vaultDir, stored)
		AppendAudit(vaultDir, AuditEntry{
			Action:    "reply_blocked",
			PolicyID:  polSpec.ID,
			RequestID: stored.RequestID,
			CallerID:  stored.CallerID,
			Outcome:   "escalated",
			Detail:    reasonSummary,
		})
		return ReplyResult{
			Escalated:  true,
			RequestID:  stored.RequestID,
			Violations: scan.Violations,
			Detail:     fmt.Sprintf("Reply blocked (%s); escalated to Front Desk.", reasonSummary),
		}, nil
	}

	// PASS -> invoke pinned transport.reply bound to (request_id, origin_thread) with idempotency_key.
	idemKey := "dlg:" + stored.RequestID
	if isWorkerAck {
		idemKey = "dlg-ack:" + stored.RequestID
	}
	if _, err := InvokeTransport(ctx, vaultDir, stored.SourceFQID, TransportRequest{
		Op:             "reply",
		RequestID:      stored.RequestID,
		OriginThread:   stored.OriginThread,
		Body:           scan.CleanedText,
		IdempotencyKey: idemKey,
	}); err != nil {
		AppendAudit(vaultDir, AuditEntry{
			Action:    "reply",
			PolicyID:  polSpec.ID,
			RequestID: stored.RequestID,
			CallerID:  stored.CallerID,
			Outcome:   "transport_error",
			Detail:    err.Error(),
		})
		return ReplyResult{}, err
	}

	tsStr := now.Format(time.RFC3339)
	_ = withStateMutate(vaultDir, now, func(st *State) error {
		st.GlobalReplyTimes = append(pruneTimestampsWithin(st.GlobalReplyTimes, cutoff), tsStr)
		st.PolicyReplyTimes[polSpec.ID] = append(pruneTimestampsWithin(st.PolicyReplyTimes[polSpec.ID], cutoff), tsStr)
		if !isWorkerAck {
			st.CompletedRequests[stored.RequestID] = "replied"
		}
		return nil
	})
	if isWorkerAck {
		stored.Status = "worker_running"
		_ = SaveStoredRequest(vaultDir, stored)
		AppendAudit(vaultDir, AuditEntry{
			Action:    "ack",
			PolicyID:  polSpec.ID,
			RequestID: stored.RequestID,
			CallerID:  stored.CallerID,
			Outcome:   "sent",
			Detail:    fmt.Sprintf("sent worker ack (%d runes) to thread %s", utf8.RuneCountInString(scan.CleanedText), stored.OriginThread),
		})
		return ReplyResult{
			Sent:      true,
			RequestID: stored.RequestID,
			Detail:    fmt.Sprintf("Delivered initial worker ack for %s to %s.", stored.RequestID, stored.CallerID),
		}, nil
	}
	stored.Status = "replied"
	_ = SaveStoredRequest(vaultDir, stored)

	auditTier := strings.TrimSpace(cfg.Defaults.AuditTier)
	if auditTier == "" {
		auditTier = outbox.TierBriefRollup
	}
	_, _, _ = outbox.Add(vaultDir, outbox.OutboxItem{
		SourceSkill: "scoped-delegate",
		Scope:       polSpec.ID,
		Tier:        auditTier,
		Title:       fmt.Sprintf("Delegate auto-replied to %s (%s)", stored.CallerID, polSpec.ID),
		Summary:     fmt.Sprintf("Verified peer %s (%s): delivered sanitized reply for request %s.", stored.CallerID, polSpec.ID, stored.RequestID),
		Tainted:     true,
		Connector:   stored.SourceFQID,
		ThreadKey:   stored.OriginThread,
	}, now)
	AppendAudit(vaultDir, AuditEntry{
		Action:    "reply",
		PolicyID:  polSpec.ID,
		RequestID: stored.RequestID,
		CallerID:  stored.CallerID,
		Outcome:   "sent",
		Detail:    fmt.Sprintf("sent %d runes to thread %s", utf8.RuneCountInString(scan.CleanedText), stored.OriginThread),
	})
	return ReplyResult{
		Sent:      true,
		RequestID: stored.RequestID,
		Detail:    fmt.Sprintf("Delivered sanitized reply for %s to %s.", stored.RequestID, stored.CallerID),
	}, nil
}

// ApplyReplyFD3 executes a human-approved ReplyApplyBundle delivered via /dev/fd/3 (`hq delegate apply-reply`).
func ApplyReplyFD3(ctx context.Context, vaultDir string, rawBundle []byte) error {
	var bundle ReplyApplyBundle
	dec := json.NewDecoder(strings.NewReader(string(rawBundle)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&bundle); err != nil {
		return fmt.Errorf("invalid delegate reply bundle: %w", err)
	}
	if bundle.SchemaVersion != PolicySchemaVersion || bundle.RequestID == "" || bundle.SourceFQID == "" || bundle.OriginThread == "" || strings.TrimSpace(bundle.Body) == "" {
		return fmt.Errorf("incomplete delegate reply bundle")
	}
	if _, err := InvokeTransport(ctx, vaultDir, bundle.SourceFQID, TransportRequest{
		Op:             "reply",
		RequestID:      bundle.RequestID,
		OriginThread:   bundle.OriginThread,
		Body:           bundle.Body,
		IdempotencyKey: "dlg-approved:" + bundle.RequestID,
	}); err != nil {
		return err
	}
	now := time.Now().UTC()
	_ = withStateMutate(vaultDir, now, func(st *State) error {
		st.CompletedRequests[bundle.RequestID] = "replied_approved"
		return nil
	})
	if stored, err := LoadStoredRequest(vaultDir, bundle.RequestID); err == nil {
		stored.Status = "replied"
		_ = SaveStoredRequest(vaultDir, stored)
	}
	AppendAudit(vaultDir, AuditEntry{
		Action:    "reply",
		PolicyID:  bundle.PolicyID,
		RequestID: bundle.RequestID,
		CallerID:  bundle.CallerID,
		Outcome:   "sent_approved",
		Detail:    fmt.Sprintf("human-approved reply delivered to thread %s", bundle.OriginThread),
	})
	return nil
}

func sanitizeEscalationReason(vaultDir, raw string) string {
	s := redact.ScrubForUntrustedOutput(vaultDir, raw)
	s = reProposalTokenAntiCue.ReplaceAllString(s, "[REDACTED_PROPOSAL_ID]")
	s = reMarkdownLinkAntiCue.ReplaceAllString(s, "$1")
	var out strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			out.WriteByte(' ')
			continue
		}
		if r == '#' || r == '`' || unicode.IsControl(r) {
			continue
		}
		out.WriteRune(r)
	}
	cleaned := strings.Join(strings.Fields(out.String()), " ")
	runes := []rune(cleaned)
	if len(runes) > MaxEscalationReasonRunes {
		cleaned = string(runes[:MaxEscalationReasonRunes-1]) + "…"
	}
	return cleaned
}

// EscalateRequest implements hardened first-class escalation (`hq delegate escalate`, RFC-0026 §7.5):
// enforces category enum, <=240 redacted single-line runes, per-caller rate limit, Tainted outbox item,
// optional static escalated_ack_template, and closes request_id.
func EscalateRequest(ctx context.Context, vaultDir, convID, reqID, category, reason string, isInternal bool) (outbox.OutboxItem, error) {
	cat := strings.ToLower(strings.TrimSpace(category))
	if !validEscalateCats[cat] {
		return outbox.OutboxItem{}, fmt.Errorf("invalid escalation --category %q (expected out_of_scope, missing_info, needs_approval, uncertain, or ambiguous)", category)
	}
	if !isInternal {
		pol, grant, err := resolveCallerGrant(vaultDir, convID)
		if err == nil {
			if !pol.Writes.DelegateEscalate {
				return outbox.OutboxItem{}, fmt.Errorf("escalate not permitted in profile %q", pol.Profile)
			}
			if strings.TrimSpace(reqID) == "" {
				reqID = grant.RequestID
			}
			if reqID != grant.RequestID {
				return outbox.OutboxItem{}, fmt.Errorf("request %q is not bound to this session grant", reqID)
			}
		} else if boundReq, _, _, ok := resolveBoundWorkerRequest(vaultDir, convID, reqID); ok {
			reqID = boundReq
		} else {
			return outbox.OutboxItem{}, err
		}
	}
	stored, err := LoadStoredRequest(vaultDir, reqID)
	if err != nil {
		return outbox.OutboxItem{}, err
	}

	now := time.Now().UTC()
	cfg, _, _, _ := LoadConfig(vaultDir)
	maxEsc := effectiveInt(cfg.Defaults.MaxEscalationsPerHour, DefaultMaxEscalationsPerHour)
	cutoff := now.Add(-1 * time.Hour)

	if err := withStateMutate(vaultDir, now, func(st *State) error {
		if prev, done := st.CompletedRequests[stored.RequestID]; done && prev != "" {
			return fmt.Errorf("request %q already finalized (%s)", stored.RequestID, prev)
		}
		recent := pruneTimestampsWithin(st.CallerEscalateTimes[stored.CallerID], cutoff)
		if len(recent) >= maxEsc {
			return fmt.Errorf("per-caller escalation rate limit reached (%d/%d per hour)", len(recent), maxEsc)
		}
		st.CallerEscalateTimes[stored.CallerID] = append(recent, now.Format(time.RFC3339))
		st.CompletedRequests[stored.RequestID] = "escalated"
		return nil
	}); err != nil {
		return outbox.OutboxItem{}, err
	}

	safeReason := sanitizeEscalationReason(vaultDir, reason)
	if safeReason == "" {
		safeReason = "Peer request requires owner attention."
	}
	scope := stored.PolicyID
	if scope == "" {
		scope = "delegates"
	}

	item, _, err := outbox.Add(vaultDir, outbox.OutboxItem{
		SourceSkill: "scoped-delegate",
		Scope:       scope,
		Tier:        outbox.TierInterrupt,
		Title:       fmt.Sprintf("Peer escalation (%s · %s)", cat, stored.CallerID),
		Summary:     fmt.Sprintf("[UNTRUSTED PEER ESCALATION — verified caller: %s · category: %s · request: %s] %s", stored.CallerID, cat, stored.RequestID, safeReason),
		Tainted:     true,
		Connector:   stored.SourceFQID,
		ThreadKey:   stored.OriginThread,
	}, now)
	if err != nil {
		return outbox.OutboxItem{}, err
	}

	stored.Status = "escalated"
	_ = SaveStoredRequest(vaultDir, stored)

	// Optional static escalated_ack_template delivery.
	if polSpec, ok := findPolicyByID(cfg, stored.PolicyID); ok && strings.TrimSpace(string(polSpec.Egress.EscalatedAckTemplate)) != "" {
		if ackBody, rErr := RenderSafeTemplate(string(polSpec.Egress.EscalatedAckTemplate), TemplateData{
			CallerID:  stored.CallerID,
			PolicyID:  polSpec.ID,
			RequestID: stored.RequestID,
		}); rErr == nil && ackBody != "" {
			_, _ = InvokeTransport(ctx, vaultDir, stored.SourceFQID, TransportRequest{
				Op:             "reply",
				RequestID:      stored.RequestID,
				OriginThread:   stored.OriginThread,
				Body:           ackBody,
				IdempotencyKey: "dlg-esc-ack:" + stored.RequestID,
			})
		}
	}

	AppendAudit(vaultDir, AuditEntry{
		Action:    "escalate",
		PolicyID:  stored.PolicyID,
		RequestID: stored.RequestID,
		CallerID:  stored.CallerID,
		Outcome:   cat,
		Detail:    safeReason,
	})
	return item, nil
}
