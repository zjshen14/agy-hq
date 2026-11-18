package offer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/connector"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/taint"
)

func init() {
	RegisterPreconditions()
}

// RegisterPreconditions wires the `connector_offer` remote thread-head check into `approvals.ApproveProposal`.
func RegisterPreconditions() {
	prev := approvals.RegisteredApplyPrecondition
	approvals.RegisteredApplyPrecondition = func(vaultDir string, prop approvals.Proposal) error {
		if prev != nil {
			if err := prev(vaultDir, prop); err != nil {
				return err
			}
		}
		if prop.ActionType != "connector_offer" {
			return nil
		}
		expectedHead := ""
		if prop.Metadata != nil {
			expectedHead = strings.TrimSpace(prop.Metadata["expectedHeadItemId"])
		}
		if prop.TargetThreadKey == "" || expectedHead == "" {
			return nil
		}
		rc, err := connector.Resolve(vaultDir, prop.TargetConnector)
		if err != nil {
			return err
		}
		if rc.Manifest.Capabilities.InspectThread == nil || !rc.Manifest.Capabilities.InspectThread.Enabled {
			return nil
		}
		allowDesktop := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CREDENTIAL_SCOPE")) != "" || connector.HasActiveDesktopPluginScope(vaultDir)
		insp, err := connector.InspectThread(context.Background(), vaultDir, rc.FQID, prop.TargetAccount, prop.TargetThreadKey, expectedHead, allowDesktop)
		if err != nil {
			return fmt.Errorf("failed to inspect thread %s before applying offer: %w", prop.TargetThreadKey, err)
		}
		if len(insp.NewerItems) > 0 || (strings.TrimSpace(insp.HeadItemID) != "" && strings.TrimSpace(insp.HeadItemID) != expectedHead) {
			return fmt.Errorf("stale offer on thread %s: remote head advanced from %s to %s (%d newer item(s)); re-read thread before sending",
				prop.TargetThreadKey, expectedHead, insp.HeadItemID, len(insp.NewerItems))
		}
		return nil
	}
}

// StageRequest configures `hq offer stage`.
type StageRequest struct {
	Connector        string
	Account          string
	Action           string
	ThreadKey        string
	Params           map[string]string
	Body             string
	Summary          string
	IdempotencyKey   string
	SupersedePrior   *bool // defaults to true when ThreadKey != ""
	ResumeOnDecide   *bool // defaults to true when ConversationID != ""
	ContinuationNote string
	OriginSkill      string
	FlagConv         string
}

func resolveOfferCallerConvID(vaultDir, flagConv string) (string, error) {
	if strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")) != "" || strings.TrimSpace(flagConv) != "" {
		return taint.ResolveCallerConvID(vaultDir, flagConv)
	}
	if os.Getenv("HQ_STRICT_CALLER_ENV") != "1" && hostpaths.IsTempPath(vaultDir) {
		return "", nil
	}
	return taint.ResolveCallerConvID(vaultDir, flagConv)
}

func verifyTaintedOfferScope(connID, acct, threadKey string, leased []events.Event, rec taint.Record) error {
	if len(leased) > 0 {
		hasWatcherLease := false
		for _, ev := range leased {
			if ev.Metadata == nil {
				continue
			}
			evConn := strings.TrimSpace(ev.Metadata["connector"])
			if evConn == "" {
				continue
			}
			hasWatcherLease = true
			evAcct := strings.TrimSpace(ev.Metadata["account"])
			if evConn != connID || (evAcct != "" && acct != "" && evAcct != acct) {
				continue
			}
			if ev.Metadata["thread_key"] == threadKey || ev.Metadata["item_id"] == threadKey || ev.Metadata["item_key"] == threadKey {
				return nil
			}
		}
		if hasWatcherLease {
			return fmt.Errorf("quarantined offer scope violation: thread %q on %s:%s is outside leased batch", threadKey, connID, acct)
		}
	}
	if rec.Connector != "" && rec.Connector != connID {
		return fmt.Errorf("quarantined offer scope violation: locked to connector %q (got %q)", rec.Connector, connID)
	}
	if rec.Account != "" && acct != "" && rec.Account != acct {
		return fmt.Errorf("quarantined offer scope violation: locked to account %q (got %q)", rec.Account, acct)
	}
	if len(rec.ItemKeys) > 0 {
		for _, k := range rec.ItemKeys {
			if k == threadKey || k == connID+":thread:"+threadKey || k == connID+":space:"+threadKey {
				return nil
			}
		}
		return fmt.Errorf("quarantined offer scope violation: thread %q is not in bound keys %v", threadKey, rec.ItemKeys)
	}
	return fmt.Errorf("quarantined offer scope violation: session has no bound thread keys")
}

// StageOffer (`hq offer stage`) validates provenance taint scope, RFC 5322 `AuthenticatedParticipants`,
// and remote `HeadItemID`, then stages a side-effect-free `HashVersion: 2` proposal whose immutable payload
// is stored in `<vault>/approvals/payloads/<id>.json` and piped via `/dev/fd/3` at `hq approve` time.
func StageOffer(ctx context.Context, vaultDir string, req StageRequest) (approvals.Proposal, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(req.Body) == "" {
		return approvals.Proposal{}, fmt.Errorf("offer --body is required")
	}
	action := strings.TrimSpace(req.Action)
	if action == "" {
		action = "reply"
	}

	convID, err := resolveOfferCallerConvID(vaultDir, req.FlagConv)
	if err != nil {
		return approvals.Proposal{}, err
	}

	rc, err := connector.Resolve(vaultDir, req.Connector)
	if err != nil {
		return approvals.Proposal{}, err
	}
	oaSpec, ok := rc.Manifest.Capabilities.Offers[action]
	if !ok {
		return approvals.Proposal{}, fmt.Errorf("connector %q does not support offer action %q", rc.ID, action)
	}

	acct := strings.TrimSpace(req.Account)
	var accCfg connector.AccountInstance
	if acct != "" {
		accCfg = rc.Instance.Accounts[acct]
	} else if rc.Instance.DefaultAccount != "" {
		acct = rc.Instance.DefaultAccount
		accCfg = rc.Instance.Accounts[acct]
	} else if len(rc.Instance.Accounts) == 1 {
		for k, v := range rc.Instance.Accounts {
			acct = k
			accCfg = v
		}
	} else {
		acct = "default"
	}

	threadKey := strings.TrimSpace(req.ThreadKey)
	if threadKey == "" && req.Params != nil {
		threadKey = strings.TrimSpace(req.Params["thread_key"])
	}

	requiresReply := oaSpec.RequiresReplyThread == nil || *oaSpec.RequiresReplyThread
	if requiresReply && threadKey == "" {
		return approvals.Proposal{}, fmt.Errorf("offer action %q on connector %q requires --thread", action, rc.ID)
	}

	taintRec, isTainted := taint.Check(vaultDir, convID)
	leased, _ := events.ListLeasedByConversation(vaultDir, convID)

	if isTainted {
		if !requiresReply || threadKey == "" {
			if !rc.EffectiveAllowTaintedCreate(vaultDir) {
				return approvals.Proposal{}, fmt.Errorf("quarantined session blocked from creating new outbound thread (%s.%s; allow_tainted_create=false)", rc.ID, action)
			}
		}
		if threadKey != "" {
			if err := verifyTaintedOfferScope(rc.ID, acct, threadKey, leased, taintRec); err != nil {
				return approvals.Proposal{}, err
			}
		}
	}

	canonicalParams := make(map[string]string, len(req.Params))
	for k, v := range req.Params {
		canonicalParams[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if threadKey != "" {
		canonicalParams["thread_key"] = threadKey
	}

	allowDesktop := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CREDENTIAL_SCOPE")) != ""
	expectedHeadItemID := ""
	var authParticipants []string
	if threadKey != "" && rc.Manifest.Capabilities.InspectThread != nil && rc.Manifest.Capabilities.InspectThread.Enabled {
		insp, iErr := connector.InspectThread(ctx, vaultDir, rc.FQID, acct, threadKey, "", allowDesktop)
		if iErr != nil {
			return approvals.Proposal{}, fmt.Errorf("failed to inspect thread %q on %s: %w", threadKey, rc.ID, iErr)
		}
		expectedHeadItemID = strings.TrimSpace(insp.HeadItemID)
		authParticipants = insp.AuthenticatedParticipants
	}

	// Enforce RFC 5322 AuthenticatedParticipants lock (display-name smuggling immune)
	enforceLock := rc.EffectiveParticipantLock(vaultDir)
	authEmailSet := make(map[string]bool)
	authChannelSet := make(map[string]bool)
	if ownerEmail := strings.ToLower(strings.TrimSpace(accCfg.Email)); ownerEmail != "" {
		authEmailSet[ownerEmail] = true
	}
	for _, ap := range authParticipants {
		apTrim := strings.TrimSpace(ap)
		if apTrim == "" {
			continue
		}
		authChannelSet[strings.ToLower(apTrim)] = true
		if parsed, pErr := connector.ParseRFC5322Emails(apTrim); pErr == nil {
			for _, em := range parsed {
				authEmailSet[em] = true
			}
		}
	}

	var displayRecipients []string
	for paramKey, kind := range oaSpec.RecipientParams {
		rawVal := strings.TrimSpace(canonicalParams[paramKey])
		if rawVal == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(kind)) {
		case "email":
			addrs, pErr := connector.ParseRFC5322Emails(rawVal)
			if pErr != nil {
				return approvals.Proposal{}, fmt.Errorf("invalid recipient in --%s: %w", paramKey, pErr)
			}
			if enforceLock && (requiresReply || isTainted) {
				for _, addr := range addrs {
					if !authEmailSet[addr] {
						return approvals.Proposal{}, fmt.Errorf("participant_lock violation on %s thread %s: recipient %q is not an authenticated DKIM/SPF-aligned or owner-addressed participant", rc.ID, threadKey, addr)
					}
				}
			}
			// Always canonicalize parameter to bare RFC 5322 addr.Address values (stripping display names)
			canonicalParams[paramKey] = strings.Join(addrs, ", ")
			displayRecipients = append(displayRecipients, addrs...)
		case "channel":
			if enforceLock && threadKey != "" && rawVal != threadKey && !authChannelSet[strings.ToLower(rawVal)] {
				return approvals.Proposal{}, fmt.Errorf("participant_lock violation on %s: channel %q does not match thread_key %q", rc.ID, rawVal, threadKey)
			}
			displayRecipients = append(displayRecipients, rawVal)
		}
	}

	deliveryMode := strings.TrimSpace(accCfg.DeliveryMode)
	if deliveryMode == "" {
		deliveryMode = strings.TrimSpace(oaSpec.DeliveryMode)
	}
	verb := strings.TrimSpace(oaSpec.Verb)
	if verb == "" {
		verb = action
	}

	envelope := connector.OfferApplyEnvelope{
		SchemaVersion:      1,
		Connector:          rc.ID,
		Account:            acct,
		Action:             action,
		Verb:               verb,
		DeliveryMode:       deliveryMode,
		ThreadKey:          threadKey,
		ExpectedHeadItemID: expectedHeadItemID,
		Params:             canonicalParams,
		Body:               req.Body,
	}
	payloadBytes, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return approvals.Proposal{}, err
	}
	payloadBytes = append(payloadBytes, '\n')

	idemKey := strings.TrimSpace(req.IdempotencyKey)
	if idemKey == "" && threadKey != "" {
		sum := sha256.Sum256(payloadBytes)
		idemKey = fmt.Sprintf("offer:%s:%s:%s:%s:%s", rc.ID, acct, action, threadKey, hex.EncodeToString(sum[:12]))
	}

	supersede := threadKey != ""
	if req.SupersedePrior != nil {
		supersede = *req.SupersedePrior
	}
	resumeOnDecide := convID != ""
	if req.ResumeOnDecide != nil {
		resumeOnDecide = *req.ResumeOnDecide
	}
	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		recipLabel := threadKey
		if len(displayRecipients) > 0 {
			recipLabel = strings.Join(displayRecipients, ", ")
		}
		summary = fmt.Sprintf("%s:%s %s -> %s", rc.ID, acct, action, recipLabel)
	}
	originSkill := strings.TrimSpace(req.OriginSkill)
	if originSkill == "" {
		originSkill = "inbox-triage"
	}

	exe := extension.ResolveSelfHQExecutable()
	argv := []string{exe, "connector", "apply-offer", "--payload-file={{payload_file}}"}

	meta := map[string]string{
		"connector":    rc.ID,
		"account":      acct,
		"action":       action,
		"deliveryMode": deliveryMode,
		"bodyPreview":  req.Body,
	}
	if threadKey != "" {
		meta["threadKey"] = threadKey
	}
	if expectedHeadItemID != "" {
		meta["expectedHeadItemId"] = expectedHeadItemID
	}
	if len(displayRecipients) > 0 {
		meta["recipients"] = strings.Join(displayRecipients, ", ")
	}

	return approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:       "connector_offer",
		Summary:          summary,
		Command:          fmt.Sprintf("hq connector apply-offer (%s:%s %s thread=%s)", rc.ID, acct, action, threadKey),
		CommandArgv:      argv,
		ConversationID:   convID,
		IdempotencyKey:   idemKey,
		TargetConnector:  rc.ID,
		TargetAccount:    acct,
		TargetThreadKey:  threadKey,
		PayloadBytes:     payloadBytes,
		SupersedePrior:   supersede,
		ResumeOnDecide:   resumeOnDecide,
		ContinuationNote: req.ContinuationNote,
		OriginSkill:      originSkill,
		Metadata:         meta,
	})
}
