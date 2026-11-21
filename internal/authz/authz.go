package authz

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/taint"
)

// ProfileKind names a coordinate in the unified 8-profile Session Capability Lattice (RFC-0026 §3.1).
type ProfileKind string

const (
	ProfileFailClosed        ProfileKind = "fail_closed"
	ProfileClassifier        ProfileKind = "classifier"
	ProfileQuarantinedReader ProfileKind = "quarantined_reader"
	ProfileScopedDelegate    ProfileKind = "scoped_delegate"
	ProfileSubagent          ProfileKind = "subagent"
	ProfileUnattendedAuto    ProfileKind = "unattended_auto"
	ProfileFrontDesk         ProfileKind = "front_desk"
	ProfileInteractiveTask   ProfileKind = "interactive_task"
)

// IngressTrust classifies the provenance of the session's primary instructions.
type IngressTrust string

const (
	IngressOwnerInteractive IngressTrust = "owner_interactive"
	IngressOwnerAuto        IngressTrust = "owner_auto"
	IngressParentSubagent   IngressTrust = "parent_subagent"
	IngressUntrustedInbox   IngressTrust = "untrusted_inbox"
	IngressUntrustedPeer    IngressTrust = "untrusted_peer"
)

// ContextProjection controls how much Vault context SessionStart injects.
type ContextProjection string

const (
	ContextNone        ContextProjection = "none"
	ContextGrantBanner ContextProjection = "grant_banner"
	ContextLeanVault   ContextProjection = "lean_vault"
	ContextFullVault   ContextProjection = "full_vault"
)

// ExecMode classifies what run_command may execute.
type ExecMode string

const (
	ExecStrictHQOnly     ExecMode = "strict_hq_only"
	ExecScrubbedShell    ExecMode = "scrubbed_shell"
	ExecInteractiveShell ExecMode = "interactive_shell"
)

// HostPermissionPreset maps a lattice profile onto a host per-conversation permission preset (RFC-0026 §3.7).
type HostPermissionPreset string

const (
	HostPresetRequestReview  HostPermissionPreset = "request_review"
	HostPresetDefaultSandbox HostPermissionPreset = "default_sandbox"
	HostPresetAuto           HostPermissionPreset = "auto"
	HostPresetTurbo          HostPermissionPreset = "turbo"
)

// ProtoEnum returns the Antigravity LanguageServer annotation enum for this preset.
func (h HostPermissionPreset) ProtoEnum() string {
	switch h {
	case HostPresetRequestReview:
		return "AGENT_PERMISSION_PRESET_REQUEST_REVIEW"
	case HostPresetDefaultSandbox:
		return "AGENT_PERMISSION_PRESET_DEFAULT"
	case HostPresetAuto:
		return "AGENT_PERMISSION_PRESET_AUTO"
	case HostPresetTurbo:
		return "AGENT_PERMISSION_PRESET_TURBO"
	default:
		return ""
	}
}

// ResolveHints carries kernel-derived session facts into internal/authz so authz never imports internal/kernel.
type ResolveHints struct {
	IsFrontDesk  bool
	IsSubagent   bool
	IsAutoWorker bool
}

// ReadPolicy describes what a session may read.
type ReadPolicy struct {
	HostFS                bool
	VaultMemory           bool
	InboxBoundBatch       bool
	InboxLowQuery         bool
	EntityRouting         bool
	PendingProposals      bool
	DelegateRequest       bool
	GrantedTasksPublic    bool
	GrantedWorkspacesProj bool
	GrantedPinnedCmds     bool
	AllowArtifactRead     bool
	AllowTrustedSkills    bool
	AllowedSkillPaths     []string
}

// WritePolicy describes what a session may mutate.
type WritePolicy struct {
	WorkspaceEditsLeased bool
	VaultMemoryWrite     bool
	FrontDeskCoordinator bool
	TriageLedger         bool
	TypedNudgeQueue      bool
	DelegateEscalate     bool
	DelegateRoute        bool
	BlockPolicyFileEdits bool
}

// ExecPolicy describes run_command constraints.
type ExecPolicy struct {
	Mode                 ExecMode
	AllowedHQVerbs       []string // canonical "top sub" pairs when Mode == ExecStrictHQOnly
	PreToolAutoApproveHQ bool     // emit "auto_approve" for verified HQ commands under restricted host presets
}

// EgressPolicy describes approval, escalation, and outbound communication boundaries.
type EgressPolicy struct {
	HumanPresenceApprove      bool
	InteractiveApprovalPolicy bool
	AutoProposeOnly           bool
	OfferStageBound           bool
	OutboxTainted             bool
	EgressFilteredReply       bool
	CanClaimDesk              bool
	CanAskQuestion            bool
	CanProposalAsk            bool
}

// SessionPolicy is the unified 5-axis authorization descriptor for any Antigravity session.
type SessionPolicy struct {
	Profile           ProfileKind
	Tainted           bool
	TaintReason       string
	IngressTrust      IngressTrust
	ContextProjection ContextProjection
	HostPreset        HostPermissionPreset
	Reads             ReadPolicy
	Writes            WritePolicy
	Exec              ExecPolicy
	Egress            EgressPolicy
	Grant             *taint.SessionGrant
}

var (
	quarantinedReaderVerbs = []string{
		"watch read",
		"watch query",
		"watch ack",
		"offer stage",
		"outbox add",
		"timers set",
		"goals mark-replied",
		"memory lookup",
		"brief format-quarantined",
		"gmail read-quarantined",
		"gmail read",
		"gmail triage-result",
		"gchat read-quarantined",
		"gchat read",
		"gchat triage-result",
	}

	forbiddenDelegateVerbPrefixes = map[string]bool{
		"status": true,
		"watch":  true,
		"gmail":  true,
		"gchat":  true,
		"memory": true,
		"offer":  true,
		"timers": true,
		"goals":  true,
		"brief":  true,
	}
)

// IsUntrustedIngress reports whether the session ingests untrusted inbox or peer data.
func (p SessionPolicy) IsUntrustedIngress() bool {
	return p.IngressTrust == IngressUntrustedInbox || p.IngressTrust == IngressUntrustedPeer
}

// ValidateProfile mechanically enforces Ring-0 Non-Interference Invariants NI-1..NI-4 (RFC-0026 §3.3).
func ValidateProfile(p SessionPolicy) error {
	// NI-1: Lethal Trifecta Exclusion — untrusted ingress may never combine private reads with auto egress.
	if p.IsUntrustedIngress() {
		hasPrivateRead := p.Reads.HostFS ||
			p.Reads.VaultMemory ||
			p.Reads.InboxBoundBatch ||
			p.Reads.InboxLowQuery ||
			p.Reads.EntityRouting ||
			p.Reads.PendingProposals
		hasAutoEgress := p.Egress.EgressFilteredReply || p.Egress.InteractiveApprovalPolicy
		if hasPrivateRead && hasAutoEgress {
			return fmt.Errorf("authz NI-1 violation: profile %q combines private reads with automated egress under untrusted ingress", p.Profile)
		}
		if p.Profile == ProfileScopedDelegate || p.Profile == ProfileClassifier || p.Profile == ProfileFailClosed {
			for _, verb := range p.Exec.AllowedHQVerbs {
				top := strings.Fields(strings.ToLower(strings.TrimSpace(verb)))
				if len(top) == 0 {
					continue
				}
				if forbiddenDelegateVerbPrefixes[top[0]] {
					return fmt.Errorf("authz NI-1 violation: delegate/classifier profile %q may not hold inbox/vault verb %q", p.Profile, verb)
				}
			}
		}
	}

	// NI-2: Untrusted Execution Containment.
	if p.IsUntrustedIngress() {
		if !p.Tainted {
			return fmt.Errorf("authz NI-2 violation: untrusted profile %q must be monotonically tainted", p.Profile)
		}
		if p.ContextProjection != ContextNone && p.ContextProjection != ContextGrantBanner {
			return fmt.Errorf("authz NI-2 violation: untrusted profile %q cannot project Vault memory (%s)", p.Profile, p.ContextProjection)
		}
		if p.Writes.WorkspaceEditsLeased || p.Writes.VaultMemoryWrite {
			return fmt.Errorf("authz NI-2 violation: untrusted profile %q cannot hold workspace or Vault write authority", p.Profile)
		}
		if p.Exec.Mode != ExecStrictHQOnly {
			return fmt.Errorf("authz NI-2 violation: untrusted profile %q must enforce strict_hq_only exec mode", p.Profile)
		}
		if p.Egress.CanClaimDesk || p.Egress.CanAskQuestion || p.Egress.CanProposalAsk {
			return fmt.Errorf("authz NI-2 violation: untrusted profile %q cannot claim desk or prompt interactively", p.Profile)
		}
	}

	// NI-3: Human-Presence Top-of-Lattice Divider.
	if p.IngressTrust != IngressOwnerInteractive {
		if p.Egress.HumanPresenceApprove || p.Egress.InteractiveApprovalPolicy {
			return fmt.Errorf("authz NI-3 violation: profile %q (ingress=%s) cannot hold human-presence approval or interactive bypass authority", p.Profile, p.IngressTrust)
		}
	}

	return nil
}

// BuildFailClosed constructs the absolute bottom profile (⊥_deny: 0 verbs, 0 file reads).
func BuildFailClosed(reason string) SessionPolicy {
	if strings.TrimSpace(reason) == "" {
		reason = "fail_closed"
	}
	return SessionPolicy{
		Profile:           ProfileFailClosed,
		Tainted:           true,
		TaintReason:       reason,
		IngressTrust:      IngressUntrustedPeer,
		ContextProjection: ContextNone,
		HostPreset:        HostPresetRequestReview,
		Reads:             ReadPolicy{},
		Writes:            WritePolicy{BlockPolicyFileEdits: true},
		Exec:              ExecPolicy{Mode: ExecStrictHQOnly, AllowedHQVerbs: nil},
		Egress:            EgressPolicy{},
		Grant:             &taint.SessionGrant{Profile: taint.ProfileFailClosed},
	}
}

// BuildClassifier constructs the 0-grant classifier profile (⊥).
func BuildClassifier(rec taint.Record) SessionPolicy {
	g := rec.Grant
	p := SessionPolicy{
		Profile:           ProfileClassifier,
		Tainted:           true,
		TaintReason:       formatTaintReason(rec),
		IngressTrust:      IngressUntrustedPeer,
		ContextProjection: ContextNone,
		HostPreset:        HostPresetRequestReview,
		Reads: ReadPolicy{
			DelegateRequest:    true,
			AllowArtifactRead:  true,
			AllowTrustedSkills: true,
		},
		Writes: WritePolicy{
			DelegateRoute:        true,
			BlockPolicyFileEdits: true,
		},
		Exec: ExecPolicy{
			Mode:                 ExecStrictHQOnly,
			AllowedHQVerbs:       []string{"delegate read-request", "delegate route"},
			PreToolAutoApproveHQ: true,
		},
		Egress: EgressPolicy{},
		Grant:  g,
	}
	if ValidateProfile(p) != nil {
		return BuildFailClosed("invalid_classifier_profile")
	}
	return p
}

// BuildQuarantinedReader constructs the RFC-0017b Quarantined Reader sibling profile.
func BuildQuarantinedReader(rec taint.Record) SessionPolicy {
	verbs := make([]string, len(quarantinedReaderVerbs))
	copy(verbs, quarantinedReaderVerbs)
	p := SessionPolicy{
		Profile:           ProfileQuarantinedReader,
		Tainted:           true,
		TaintReason:       formatTaintReason(rec),
		IngressTrust:      IngressUntrustedInbox,
		ContextProjection: ContextNone,
		HostPreset:        HostPresetRequestReview,
		Reads: ReadPolicy{
			InboxBoundBatch:    true,
			InboxLowQuery:      true,
			EntityRouting:      true,
			AllowArtifactRead:  true,
			AllowTrustedSkills: true,
		},
		Writes: WritePolicy{
			TriageLedger:         true,
			BlockPolicyFileEdits: true,
		},
		Exec: ExecPolicy{
			Mode:                 ExecStrictHQOnly,
			AllowedHQVerbs:       verbs,
			PreToolAutoApproveHQ: true,
		},
		Egress: EgressPolicy{
			OfferStageBound: true,
			OutboxTainted:   true,
		},
	}
	if ValidateProfile(p) != nil {
		return BuildFailClosed("invalid_quarantined_reader_profile")
	}
	return p
}

// CompileDelegateVerbs derives the exact AllowedHQVerbs for a Scoped Delegate grant.
func CompileDelegateVerbs(g taint.SessionGrant) []string {
	if g.Profile == string(ProfileClassifier) {
		return []string{"delegate read-request", "delegate route"}
	}
	verbs := []string{"delegate read-request", "delegate escalate"}
	if len(g.Tasks) > 0 {
		verbs = append(verbs, "delegate read-task")
	}
	if len(g.Workspaces) > 0 {
		verbs = append(verbs, "delegate read-workspace")
	}
	if len(g.Commands) > 0 {
		verbs = append(verbs, "delegate run")
	}
	if len(g.NudgeIDs) > 0 {
		verbs = append(verbs, "delegate nudge")
	}
	switch strings.ToLower(strings.TrimSpace(g.EgressMode)) {
	case "auto_reply", "stage_approval", "structured_template":
		verbs = append(verbs, "delegate reply")
	}
	return verbs
}

// BuildScopedDelegate constructs the RFC-0026 Scoped Delegate sibling profile from a SessionGrant.
func BuildScopedDelegate(rec taint.Record) SessionPolicy {
	if rec.Grant == nil {
		return BuildFailClosed("missing_delegate_grant")
	}
	g := *rec.Grant
	verbs := g.AllowedHQVerbs
	if len(verbs) == 0 {
		verbs = CompileDelegateVerbs(g)
	}
	egressAuto := false
	switch strings.ToLower(strings.TrimSpace(g.EgressMode)) {
	case "auto_reply", "structured_template":
		egressAuto = true
	}
	p := SessionPolicy{
		Profile:           ProfileScopedDelegate,
		Tainted:           true,
		TaintReason:       formatTaintReason(rec),
		IngressTrust:      IngressUntrustedPeer,
		ContextProjection: ContextGrantBanner,
		HostPreset:        HostPresetRequestReview,
		Reads: ReadPolicy{
			DelegateRequest:       true,
			GrantedTasksPublic:    len(g.Tasks) > 0,
			GrantedWorkspacesProj: len(g.Workspaces) > 0,
			GrantedPinnedCmds:     len(g.Commands) > 0,
			AllowArtifactRead:     true,
			AllowTrustedSkills:    len(g.Skills) > 0,
			AllowedSkillPaths:     append([]string(nil), g.Skills...),
		},
		Writes: WritePolicy{
			TypedNudgeQueue:      len(g.NudgeIDs) > 0,
			DelegateEscalate:     true,
			BlockPolicyFileEdits: true,
		},
		Exec: ExecPolicy{
			Mode:                 ExecStrictHQOnly,
			AllowedHQVerbs:       append([]string(nil), verbs...),
			PreToolAutoApproveHQ: true,
		},
		Egress: EgressPolicy{
			OutboxTainted:       true,
			EgressFilteredReply: egressAuto,
		},
		Grant: &g,
	}
	if err := ValidateProfile(p); err != nil {
		return BuildFailClosed(err.Error())
	}
	return p
}

// BuildSubagent constructs the parent-spawned subagent profile.
func BuildSubagent() SessionPolicy {
	return SessionPolicy{
		Profile:           ProfileSubagent,
		IngressTrust:      IngressParentSubagent,
		ContextProjection: ContextLeanVault,
		HostPreset:        HostPresetDefaultSandbox,
		Reads: ReadPolicy{
			HostFS:             true,
			VaultMemory:        true,
			AllowArtifactRead:  true,
			AllowTrustedSkills: true,
		},
		Writes: WritePolicy{
			WorkspaceEditsLeased: true,
		},
		Exec: ExecPolicy{
			Mode: ExecInteractiveShell,
		},
		Egress: EgressPolicy{
			AutoProposeOnly: true,
			CanAskQuestion:  true,
		},
	}
}

// BuildUnattendedAuto constructs the unattended [Auto] background worker profile.
func BuildUnattendedAuto() SessionPolicy {
	return SessionPolicy{
		Profile:           ProfileUnattendedAuto,
		IngressTrust:      IngressOwnerAuto,
		ContextProjection: ContextLeanVault,
		HostPreset:        HostPresetDefaultSandbox,
		Reads: ReadPolicy{
			HostFS:             true,
			VaultMemory:        true,
			AllowArtifactRead:  true,
			AllowTrustedSkills: true,
		},
		Writes: WritePolicy{
			WorkspaceEditsLeased: true,
			BlockPolicyFileEdits: true,
		},
		Exec: ExecPolicy{
			Mode:                 ExecScrubbedShell,
			PreToolAutoApproveHQ: true,
		},
		Egress: EgressPolicy{
			AutoProposeOnly: true,
		},
	}
}

// BuildFrontDesk constructs the interactive [HQ] Front Desk coordinator profile.
func BuildFrontDesk() SessionPolicy {
	return SessionPolicy{
		Profile:           ProfileFrontDesk,
		IngressTrust:      IngressOwnerInteractive,
		ContextProjection: ContextFullVault,
		HostPreset:        HostPresetAuto,
		Reads: ReadPolicy{
			VaultMemory:        true,
			PendingProposals:   true,
			AllowArtifactRead:  true,
			AllowTrustedSkills: true,
		},
		Writes: WritePolicy{
			FrontDeskCoordinator: true,
			VaultMemoryWrite:     true,
		},
		Exec: ExecPolicy{
			Mode: ExecInteractiveShell,
		},
		Egress: EgressPolicy{
			HumanPresenceApprove:      true,
			InteractiveApprovalPolicy: true,
			CanClaimDesk:              true,
			CanAskQuestion:            true,
			CanProposalAsk:            true,
		},
	}
}

// BuildInteractiveTask constructs the top-of-lattice (⊤) interactive task profile.
func BuildInteractiveTask() SessionPolicy {
	return SessionPolicy{
		Profile:           ProfileInteractiveTask,
		IngressTrust:      IngressOwnerInteractive,
		ContextProjection: ContextFullVault,
		HostPreset:        HostPresetAuto,
		Reads: ReadPolicy{
			HostFS:             true,
			VaultMemory:        true,
			PendingProposals:   true,
			AllowArtifactRead:  true,
			AllowTrustedSkills: true,
		},
		Writes: WritePolicy{
			WorkspaceEditsLeased: true,
			VaultMemoryWrite:     true,
		},
		Exec: ExecPolicy{
			Mode: ExecInteractiveShell,
		},
		Egress: EgressPolicy{
			HumanPresenceApprove:      true,
			InteractiveApprovalPolicy: true,
			CanClaimDesk:              true,
			CanAskQuestion:            true,
			CanProposalAsk:            true,
		},
	}
}

func formatTaintReason(rec taint.Record) string {
	if rec.TaintedAt != "" {
		return fmt.Sprintf("%s (%s)", rec.Source, rec.TaintedAt)
	}
	return rec.Source
}

// Resolve computes the single authoritative SessionPolicy for convID at time.Now().
func Resolve(vaultDir, convID string, hints ResolveHints) SessionPolicy {
	return ResolveAt(vaultDir, convID, hints, time.Now())
}

// ResolveAt computes the authoritative SessionPolicy for convID at reference time now.
func ResolveAt(vaultDir, convID string, hints ResolveHints, now time.Time) SessionPolicy {
	if rec, tainted := taint.Check(vaultDir, convID); tainted {
		if strings.HasPrefix(rec.Source, "fail_closed:") {
			return BuildFailClosed(formatTaintReason(rec))
		}
		if rec.Grant != nil {
			if rec.Grant.Profile == taint.ProfileFailClosed {
				return BuildFailClosed(formatTaintReason(rec))
			}
			if exp := strings.TrimSpace(rec.Grant.ExpiresAt); exp != "" {
				if ts, err := time.Parse(time.RFC3339, exp); err != nil || now.After(ts) {
					return BuildFailClosed("grant_expired")
				}
			}
			switch ProfileKind(strings.TrimSpace(rec.Grant.Profile)) {
			case ProfileClassifier:
				return BuildClassifier(rec)
			case ProfileScopedDelegate:
				return BuildScopedDelegate(rec)
			default:
				return BuildFailClosed("unknown_grant_profile:" + rec.Grant.Profile)
			}
		}
		if rec.SchemaVersion >= taint.SchemaVersion {
			return BuildFailClosed("v2_missing_grant")
		}
		return BuildQuarantinedReader(rec)
	}
	if hints.IsFrontDesk {
		return BuildFrontDesk()
	}
	if hints.IsAutoWorker {
		return BuildUnattendedAuto()
	}
	if hints.IsSubagent {
		return BuildSubagent()
	}
	return BuildInteractiveTask()
}

// Attenuate computes the monotonic downward meet (current ∩ ceiling) in the capability lattice.
// Capabilities can only narrow, never widen; if the result violates NI-1..NI-4, clamps to ProfileFailClosed.
func Attenuate(current, ceiling SessionPolicy) SessionPolicy {
	out := current
	out.Tainted = current.Tainted || ceiling.Tainted
	if ceiling.Tainted {
		out.Profile = ceiling.Profile
		out.IngressTrust = ceiling.IngressTrust
		if out.TaintReason == "" {
			out.TaintReason = ceiling.TaintReason
		}
	}
	if contextRank(ceiling.ContextProjection) < contextRank(current.ContextProjection) {
		out.ContextProjection = ceiling.ContextProjection
	}
	if presetRank(ceiling.HostPreset) < presetRank(current.HostPreset) {
		out.HostPreset = ceiling.HostPreset
	}
	out.Reads = ReadPolicy{
		HostFS:                current.Reads.HostFS && ceiling.Reads.HostFS,
		VaultMemory:           current.Reads.VaultMemory && ceiling.Reads.VaultMemory,
		InboxBoundBatch:       current.Reads.InboxBoundBatch && ceiling.Reads.InboxBoundBatch,
		InboxLowQuery:         current.Reads.InboxLowQuery && ceiling.Reads.InboxLowQuery,
		EntityRouting:         current.Reads.EntityRouting && ceiling.Reads.EntityRouting,
		PendingProposals:      current.Reads.PendingProposals && ceiling.Reads.PendingProposals,
		DelegateRequest:       current.Reads.DelegateRequest && ceiling.Reads.DelegateRequest,
		GrantedTasksPublic:    current.Reads.GrantedTasksPublic && ceiling.Reads.GrantedTasksPublic,
		GrantedWorkspacesProj: current.Reads.GrantedWorkspacesProj && ceiling.Reads.GrantedWorkspacesProj,
		GrantedPinnedCmds:     current.Reads.GrantedPinnedCmds && ceiling.Reads.GrantedPinnedCmds,
		AllowArtifactRead:     current.Reads.AllowArtifactRead && ceiling.Reads.AllowArtifactRead,
		AllowTrustedSkills:    current.Reads.AllowTrustedSkills && ceiling.Reads.AllowTrustedSkills,
		AllowedSkillPaths:     intersectStrings(current.Reads.AllowedSkillPaths, ceiling.Reads.AllowedSkillPaths),
	}
	out.Writes = WritePolicy{
		WorkspaceEditsLeased: current.Writes.WorkspaceEditsLeased && ceiling.Writes.WorkspaceEditsLeased,
		VaultMemoryWrite:     current.Writes.VaultMemoryWrite && ceiling.Writes.VaultMemoryWrite,
		FrontDeskCoordinator: current.Writes.FrontDeskCoordinator && ceiling.Writes.FrontDeskCoordinator,
		TriageLedger:         current.Writes.TriageLedger && ceiling.Writes.TriageLedger,
		TypedNudgeQueue:      current.Writes.TypedNudgeQueue && ceiling.Writes.TypedNudgeQueue,
		DelegateEscalate:     current.Writes.DelegateEscalate && ceiling.Writes.DelegateEscalate,
		DelegateRoute:        current.Writes.DelegateRoute && ceiling.Writes.DelegateRoute,
		BlockPolicyFileEdits: current.Writes.BlockPolicyFileEdits || ceiling.Writes.BlockPolicyFileEdits,
	}
	if execRank(ceiling.Exec.Mode) < execRank(current.Exec.Mode) {
		out.Exec.Mode = ceiling.Exec.Mode
	}
	if out.Exec.Mode == ExecStrictHQOnly {
		if current.Exec.Mode == ExecStrictHQOnly && ceiling.Exec.Mode == ExecStrictHQOnly {
			out.Exec.AllowedHQVerbs = intersectStrings(current.Exec.AllowedHQVerbs, ceiling.Exec.AllowedHQVerbs)
		} else if ceiling.Exec.Mode == ExecStrictHQOnly {
			out.Exec.AllowedHQVerbs = append([]string(nil), ceiling.Exec.AllowedHQVerbs...)
		}
	}
	out.Egress = EgressPolicy{
		HumanPresenceApprove:      current.Egress.HumanPresenceApprove && ceiling.Egress.HumanPresenceApprove,
		InteractiveApprovalPolicy: current.Egress.InteractiveApprovalPolicy && ceiling.Egress.InteractiveApprovalPolicy,
		AutoProposeOnly:           current.Egress.AutoProposeOnly || ceiling.Egress.AutoProposeOnly,
		OfferStageBound:           current.Egress.OfferStageBound && ceiling.Egress.OfferStageBound,
		OutboxTainted:             current.Egress.OutboxTainted && ceiling.Egress.OutboxTainted,
		EgressFilteredReply:       current.Egress.EgressFilteredReply && ceiling.Egress.EgressFilteredReply,
		CanClaimDesk:              current.Egress.CanClaimDesk && ceiling.Egress.CanClaimDesk,
		CanAskQuestion:            current.Egress.CanAskQuestion && ceiling.Egress.CanAskQuestion,
		CanProposalAsk:            current.Egress.CanProposalAsk && ceiling.Egress.CanProposalAsk,
	}
	if err := ValidateProfile(out); err != nil {
		return BuildFailClosed(err.Error())
	}
	return out
}

func contextRank(c ContextProjection) int {
	switch c {
	case ContextNone:
		return 0
	case ContextGrantBanner:
		return 1
	case ContextLeanVault:
		return 2
	case ContextFullVault:
		return 3
	default:
		return 0
	}
}

func execRank(e ExecMode) int {
	switch e {
	case ExecStrictHQOnly:
		return 0
	case ExecScrubbedShell:
		return 1
	case ExecInteractiveShell:
		return 2
	default:
		return 0
	}
}

func presetRank(h HostPermissionPreset) int {
	switch h {
	case HostPresetRequestReview:
		return 0
	case HostPresetDefaultSandbox:
		return 1
	case HostPresetAuto:
		return 2
	case HostPresetTurbo:
		return 3
	default:
		return 0
	}
}

func intersectStrings(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	set := make(map[string]bool, len(b))
	for _, s := range b {
		set[s] = true
	}
	var out []string
	for _, s := range a {
		if set[s] {
			out = append(out, s)
		}
	}
	return out
}

// EvalHQArgv enforces strict_hq_only against the session policy's compiled AllowedHQVerbs.
func (p SessionPolicy) EvalHQArgv(argv []string) error {
	if p.Profile == ProfileFailClosed {
		return fmt.Errorf("session is clamped to fail_closed (0 commands permitted)")
	}
	if len(argv) < 2 || !isHQBinaryToken(argv[0]) {
		return fmt.Errorf("only single canonical `hq` commands are permitted in %s sessions", p.Profile)
	}
	for _, tok := range argv[1:] {
		if strings.HasPrefix(tok, "@") {
			return fmt.Errorf("leading `@` file-inclusion token %q is forbidden in %s sessions", tok, p.Profile)
		}
		if _, val, ok := strings.Cut(tok, "="); ok && strings.HasPrefix(val, "@") {
			return fmt.Errorf("leading `@` flag value in %q is forbidden in %s sessions", tok, p.Profile)
		}
	}
	top := argv[1]
	sub := ""
	if len(argv) >= 3 && !strings.HasPrefix(argv[2], "-") {
		sub = argv[2]
	}
	candidate := strings.TrimSpace(top + " " + sub)
	for _, allowed := range p.Exec.AllowedHQVerbs {
		if allowed == candidate {
			return nil
		}
	}
	return fmt.Errorf("command `hq %s` is forbidden in %s sessions", candidate, p.Profile)
}

func isHQBinaryToken(bin string) bool {
	slash := filepath.ToSlash(strings.TrimSpace(bin))
	return slash == "hq" || slash == "./bin/hq" || slash == "bin/hq" || strings.HasSuffix(slash, "/hq")
}

// BindSessionFromEvents is the single atomic Ring-0 taint & SessionGrant binder called by both
// internal/kernel/events_dispatch.go (OnCascadeCreated) and internal/hooks/quarantine.go (SessionStart/PreToolUse).
func BindSessionFromEvents(ctx context.Context, vaultDir, convID string, bound []events.Event) (taint.Record, bool, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" || strings.TrimSpace(vaultDir) == "" || len(bound) == 0 {
		return taint.Record{}, false, nil
	}

	needsTaint := false
	grantID := ""
	var evIDs []string
	var itemKeys []string
	connector := ""
	account := ""
	batchID := ""
	source := "spawn_batch"

	for _, ev := range bound {
		evIDs = append(evIDs, ev.ID)
		if ev.LeaseID != "" && batchID == "" {
			batchID = ev.LeaseID
		}
		if ev.Source != "" && source == "spawn_batch" {
			source = "spawn_batch:" + ev.Source
		}
		if ev.TaintOnSpawn {
			needsTaint = true
		}
		if ev.Metadata != nil {
			if gid := strings.TrimSpace(ev.Metadata["authz_grant_id"]); gid != "" {
				grantID = gid
				needsTaint = true
			}
			if ik := strings.TrimSpace(ev.Metadata["item_key"]); ik != "" {
				itemKeys = append(itemKeys, ik)
			}
			if tk := strings.TrimSpace(ev.Metadata["thread_key"]); tk != "" {
				itemKeys = append(itemKeys, tk)
			}
			for _, cid := range strings.Split(ev.Metadata["coalesced_item_ids"], ",") {
				if cid = strings.TrimSpace(cid); cid != "" {
					itemKeys = append(itemKeys, cid)
				}
			}
			if connector == "" {
				connector = strings.TrimSpace(ev.Metadata["connector"])
			}
			if account == "" {
				account = strings.TrimSpace(ev.Metadata["account"])
			}
		}
	}

	if !needsTaint {
		return taint.Record{}, false, nil
	}

	// If already bound with a grant, preserve immutably (idempotent across OnCascadeCreated + SessionStart).
	if existing, ok := taint.Check(vaultDir, convID); ok && existing.Grant != nil {
		return existing, true, nil
	}

	rec := taint.Record{
		ConvID:   convID,
		Source:   source,
		BatchID:  batchID,
		EventIDs: evIDs,
	}
	if grantID != "" {
		g, found, err := taint.ConsumePendingGrant(ctx, vaultDir, grantID)
		if err != nil || !found {
			// P0-1 fail-closed: missing/corrupt pending grant MUST NEVER degrade into quarantined_reader.
			failGrant := taint.SessionGrant{
				GrantID: grantID,
				Profile: taint.ProfileFailClosed,
			}
			rec.Source = "fail_closed:missing_pending_grant"
			rec.Grant = &failGrant
		} else {
			if len(g.AllowedHQVerbs) == 0 {
				g.AllowedHQVerbs = CompileDelegateVerbs(g)
			}
			rec.Grant = &g
			// Verify NI-1..NI-4 before persisting; clamp to fail_closed on any violation.
			var compiled SessionPolicy
			if ProfileKind(g.Profile) == ProfileClassifier {
				compiled = BuildClassifier(rec)
			} else {
				compiled = BuildScopedDelegate(rec)
			}
			if compiled.Profile == ProfileFailClosed {
				failGrant := taint.SessionGrant{
					GrantID: g.GrantID,
					Profile: taint.ProfileFailClosed,
				}
				rec.Source = "fail_closed:invalid_grant_policy"
				rec.Grant = &failGrant
			}
		}
	} else {
		rec.Connector = connector
		rec.Account = account
		rec.ItemKeys = itemKeys
	}

	persisted, err := taint.Mark(ctx, vaultDir, rec)
	return persisted, true, err
}
