package delegates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/store"
)

// ShowResult is returned by ShowPolicy (`hq delegate show`) strictly in interactive owner sessions.
type ShowResult struct {
	Exists   bool   `json:"exists"`
	Path     string `json:"path"`
	SHA256   string `json:"sha256,omitempty"`
	RawJSONC string `json:"raw_jsonc,omitempty"`
	Config   Config `json:"config"`
}

// ConfigureResult reports whether a delegate policy update applied immediately or staged 1-tap approval.
type ConfigureResult struct {
	Applied     bool                `json:"applied"`
	Staged      bool                `json:"staged"`
	Widening    bool                `json:"widening"`
	Reasons     []string            `json:"reasons,omitempty"`
	Warnings    []string            `json:"warnings,omitempty"`
	SHA256      string              `json:"sha256,omitempty"`
	PinnedBins  int                 `json:"pinned_bins"`
	Proposal    *approvals.Proposal `json:"proposal,omitempty"`
	SummaryText string              `json:"summary,omitempty"`
}

// PolicyApplyBundle is serialized into an approval payload and piped via /dev/fd/3 into `hq delegate apply-policy`.
type PolicyApplyBundle struct {
	SchemaVersion int                     `json:"schema_version"`
	BaseSHA256    string                  `json:"base_sha256,omitempty"`
	RawPolicy     string                  `json:"raw_policy"`
	TrustTarget   extension.TrustedTarget `json:"trust_target"`
	Reasons       []string                `json:"reasons,omitempty"`
}

func egressModeRank(mode string) int {
	switch strings.TrimSpace(mode) {
	case "none":
		return 0
	case "escalate":
		return 1
	case "stage_approval":
		return 2
	case "structured_template":
		return 3
	case "auto_reply":
		return 4
	default:
		return 5
	}
}

func effectiveInt(val, def int) int {
	if val <= 0 {
		return def
	}
	return val
}

func stringSetDiff(oldList, newList []string) (added []string, removed []string) {
	oldSet := make(map[string]bool, len(oldList))
	for _, s := range oldList {
		oldSet[strings.TrimSpace(s)] = true
	}
	newSet := make(map[string]bool, len(newList))
	for _, s := range newList {
		newSet[strings.TrimSpace(s)] = true
	}
	for s := range newSet {
		if s != "" && !oldSet[s] {
			added = append(added, s)
		}
	}
	for s := range oldSet {
		if s != "" && !newSet[s] {
			removed = append(removed, s)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// ClassifyPolicyDiff compares an existing Config (if any) against a validated draft Config
// and reports whether the edit widens privilege (requiring 1-tap rsi_apply from chat) or is
// non-widening (applying immediately with 0 popups in owner interactive sessions; RFC-0026 §3.6).
func ClassifyPolicyDiff(oldCfg *Config, hasOld bool, newCfg Config) (bool, []string) {
	var reasons []string
	addReason := func(r string) {
		reasons = append(reasons, r)
	}

	var base Config
	if hasOld && oldCfg != nil {
		base = *oldCfg
	}

	// 1. Defaults loosening
	if hasOld {
		if _, rem := stringSetDiff(base.Defaults.BlockedPatterns, newCfg.Defaults.BlockedPatterns); len(rem) > 0 {
			addReason(fmt.Sprintf("removed %d defaults.blocked_patterns entry(s)", len(rem)))
		}
		if _, rem := stringSetDiff(base.Defaults.SensitiveTerms, newCfg.Defaults.SensitiveTerms); len(rem) > 0 {
			addReason(fmt.Sprintf("removed defaults.sensitive_terms: %s", strings.Join(rem, ", ")))
		}
		if effectiveInt(newCfg.Defaults.GlobalMaxRepliesPerHour, DefaultGlobalMaxRepliesPerHour) > effectiveInt(base.Defaults.GlobalMaxRepliesPerHour, DefaultGlobalMaxRepliesPerHour) {
			addReason("increased defaults.global_max_replies_per_hour")
		}
		if effectiveInt(newCfg.Defaults.MaxCommandRunsPerRequest, DefaultMaxCmdRunsPerRequest) > effectiveInt(base.Defaults.MaxCommandRunsPerRequest, DefaultMaxCmdRunsPerRequest) {
			addReason("increased defaults.max_command_runs_per_request")
		}
		if effectiveInt(newCfg.Defaults.MaxEscalationsPerHour, DefaultMaxEscalationsPerHour) > effectiveInt(base.Defaults.MaxEscalationsPerHour, DefaultMaxEscalationsPerHour) {
			addReason("increased defaults.max_escalations_per_hour")
		}
	}

	// 2. Top-level pinned commands (any new or modified command binary/args widens execution surface)
	for cmdID, newCmd := range newCfg.Commands {
		oldCmd, existed := base.Commands[cmdID]
		if !existed {
			addReason(fmt.Sprintf("added pinned command %q (%s)", cmdID, newCmd.Command[0]))
			continue
		}
		if !reflect.DeepEqual(oldCmd.Command, newCmd.Command) ||
			oldCmd.SourcePack != newCmd.SourcePack ||
			!reflect.DeepEqual(oldCmd.EnvAllow, newCmd.EnvAllow) ||
			!reflect.DeepEqual(oldCmd.Args, newCmd.Args) {
			addReason(fmt.Sprintf("modified command %q definition/args", cmdID))
		}
	}

	// 3. Policies
	oldPolByID := make(map[string]PolicySpec, len(base.Policies))
	for _, p := range base.Policies {
		oldPolByID[p.ID] = p
	}
	for _, np := range newCfg.Policies {
		if !np.IsEnabled() {
			continue // Disabling a policy never widens privileges.
		}
		op, existed := oldPolByID[np.ID]
		if !existed || !op.IsEnabled() {
			if egressModeRank(np.Egress.Mode) >= egressModeRank("structured_template") ||
				len(np.Grants.Commands) > 0 || len(np.Grants.Nudges) > 0 ||
				len(np.Grants.Tasks) > 0 || len(np.Grants.Workspaces) > 0 || len(np.Grants.Skills) > 0 {
				addReason(fmt.Sprintf("enabled policy %q (egress=%s)", np.ID, np.Egress.Mode))
			}
			continue
		}

		// Caller expansion check
		oldCallers := ExpandPolicyCallers(base, op)
		newCallers := ExpandPolicyCallers(newCfg, np)
		var addedCallers []string
		for c := range newCallers {
			if !oldCallers[c] {
				addedCallers = append(addedCallers, c)
			}
		}
		if len(addedCallers) > 0 {
			sort.Strings(addedCallers)
			addReason(fmt.Sprintf("policy %q added caller(s): %s", np.ID, strings.Join(addedCallers, ", ")))
		}

		// Egress widening check
		if egressModeRank(np.Egress.Mode) > egressModeRank(op.Egress.Mode) {
			addReason(fmt.Sprintf("policy %q upgraded egress.mode (%s -> %s)", np.ID, op.Egress.Mode, np.Egress.Mode))
		}
		if _, rem := stringSetDiff(op.Egress.BlockedPatterns, np.Egress.BlockedPatterns); len(rem) > 0 {
			addReason(fmt.Sprintf("policy %q removed %d egress.blocked_patterns", np.ID, len(rem)))
		}
		if effectiveInt(np.Egress.MaxRepliesPerHour, DefaultPolicyMaxRepliesPerHour) > effectiveInt(op.Egress.MaxRepliesPerHour, DefaultPolicyMaxRepliesPerHour) {
			addReason(fmt.Sprintf("policy %q increased egress.max_replies_per_hour", np.ID))
		}
		if effectiveInt(np.Egress.MaxBodyRunes, DefaultMaxBodyRunes) > effectiveInt(op.Egress.MaxBodyRunes, DefaultMaxBodyRunes) {
			addReason(fmt.Sprintf("policy %q increased egress.max_body_runes", np.ID))
		}
		if effectiveInt(np.MaxCommandRunsPerRequest, DefaultMaxCmdRunsPerRequest) > effectiveInt(op.MaxCommandRunsPerRequest, DefaultMaxCmdRunsPerRequest) {
			addReason(fmt.Sprintf("policy %q increased max_command_runs_per_request", np.ID))
		}

		// Grants widening check:
		// Commands, skills, and nudges always widen; tasks/workspaces widen when egress is auto_reply/structured_template.
		isAutoEgress := egressModeRank(np.Egress.Mode) >= egressModeRank("structured_template")
		if added, _ := stringSetDiff(op.Grants.Tasks, np.Grants.Tasks); len(added) > 0 && isAutoEgress {
			addReason(fmt.Sprintf("policy %q added task grant(s) to %s policy: %s", np.ID, np.Egress.Mode, strings.Join(added, ", ")))
		}
		if added, _ := stringSetDiff(op.Grants.Workspaces, np.Grants.Workspaces); len(added) > 0 && isAutoEgress {
			addReason(fmt.Sprintf("policy %q added workspace grant(s) to %s policy: %s", np.ID, np.Egress.Mode, strings.Join(added, ", ")))
		}
		if added, _ := stringSetDiff(op.Grants.Skills, np.Grants.Skills); len(added) > 0 {
			addReason(fmt.Sprintf("policy %q added skill grant(s): %s", np.ID, strings.Join(added, ", ")))
		}
		if added, _ := stringSetDiff(op.Grants.Commands, np.Grants.Commands); len(added) > 0 {
			addReason(fmt.Sprintf("policy %q added command grant(s): %s", np.ID, strings.Join(added, ", ")))
		}

		// Nudges widening check
		oldNudges := make(map[string]NudgeSpec, len(op.Grants.Nudges))
		for _, on := range op.Grants.Nudges {
			oldNudges[on.ID] = on
		}
		for _, nn := range np.Grants.Nudges {
			on, had := oldNudges[nn.ID]
			if !had {
				addReason(fmt.Sprintf("policy %q added nudge %q", np.ID, nn.ID))
				continue
			}
			if nn.PreflightCommand != on.PreflightCommand ||
				nn.Template != on.Template ||
				nn.AckReplyTemplate != on.AckReplyTemplate ||
				!reflect.DeepEqual(nn.Params, on.Params) {
				addReason(fmt.Sprintf("policy %q modified nudge %q contract/template", np.ID, nn.ID))
			}
			if nn.CooldownSeconds < on.CooldownSeconds {
				addReason(fmt.Sprintf("policy %q lowered nudge %q cooldown_seconds", np.ID, nn.ID))
			}
			if nn.MaxNudgesPerHour > on.MaxNudgesPerHour && on.MaxNudgesPerHour > 0 {
				addReason(fmt.Sprintf("policy %q increased nudge %q max_nudges_per_hour", np.ID, nn.ID))
			}
		}
	}

	// 4. Snapshots
	oldSnaps := make(map[string]SnapshotSpec, len(base.Snapshots))
	for _, s := range base.Snapshots {
		oldSnaps[s.ID] = s
	}
	for _, ns := range newCfg.Snapshots {
		if !ns.IsEnabled() {
			continue
		}
		osnap, existed := oldSnaps[ns.ID]
		if !existed || !osnap.IsEnabled() {
			addReason(fmt.Sprintf("enabled offline snapshot %q", ns.ID))
			continue
		}
		if ns.Source != osnap.Source || !reflect.DeepEqual(ns.Commands, osnap.Commands) {
			addReason(fmt.Sprintf("modified snapshot %q source/commands", ns.ID))
		}
		if added, _ := stringSetDiff(osnap.Tasks, ns.Tasks); len(added) > 0 {
			addReason(fmt.Sprintf("snapshot %q added tasks: %s", ns.ID, strings.Join(added, ", ")))
		}
		if added, _ := stringSetDiff(osnap.Workspaces, ns.Workspaces); len(added) > 0 {
			addReason(fmt.Sprintf("snapshot %q added workspaces: %s", ns.ID, strings.Join(added, ", ")))
		}
	}

	sort.Strings(reasons)
	return len(reasons) > 0, reasons
}

// BuildDelegatesTrustTarget hashes all non-pack command binaries declared in cfg into
// a TrustedTarget with SourceID: "delegates" (CanonicalRoot: <vault>/private/delegates).
func BuildDelegatesTrustTarget(vaultDir string, cfg Config) (extension.TrustedTarget, []string, error) {
	privDir := filepath.Join(vaultDir, "private", "delegates")
	if err := os.MkdirAll(privDir, 0o700); err != nil {
		return extension.TrustedTarget{}, nil, err
	}
	canonRoot, err := filepath.EvalSymlinks(privDir)
	if err != nil {
		canonRoot = filepath.Clean(privDir)
	}
	tt := extension.TrustedTarget{
		SourceID:      TrustSourceDelegates,
		CanonicalRoot: canonRoot,
		Files:         make(map[string]string),
		Binaries:      make(map[string]extension.PinnedBinary),
		ApprovedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	var lines []string

	cmdIDs := make([]string, 0, len(cfg.Commands))
	for id := range cfg.Commands {
		cmdIDs = append(cmdIDs, id)
	}
	sort.Strings(cmdIDs)
	for _, id := range cmdIDs {
		spec := cfg.Commands[id]
		if strings.TrimSpace(spec.SourcePack) != "" {
			if _, err := extension.VerifyTrustedBinaryReadOnly(vaultDir, "pack:"+strings.TrimSpace(spec.SourcePack), spec.Command[0]); err != nil {
				return extension.TrustedTarget{}, nil, fmt.Errorf("command %q references source_pack %q whose binary %q is not trusted: %w", id, spec.SourcePack, spec.Command[0], err)
			}
			continue
		}
		rawBin := strings.TrimSpace(spec.Command[0])
		pb, err := extension.PinExternalBinary(rawBin)
		if err != nil {
			return extension.TrustedTarget{}, nil, fmt.Errorf("pin command %q binary %q: %w", id, rawBin, err)
		}
		tt.Binaries[rawBin] = pb
		short := pb.SHA256
		if len(short) > 12 {
			short = short[:12]
		}
		lines = append(lines, fmt.Sprintf("command %s -> %s (sha256:%s)", id, pb.ResolvedPath, short))
	}
	return tt, lines, nil
}

// ShowPolicy returns the current private/delegates/policy.json and its SHA-256 digest.
func ShowPolicy(vaultDir string) (ShowResult, error) {
	p := PolicyFilePath(vaultDir)
	st, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return ShowResult{Exists: false, Path: p}, nil
		}
		return ShowResult{}, err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return ShowResult{}, fmt.Errorf("refusing symlinked or non-regular delegate policy %s", p)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return ShowResult{}, err
	}
	sum := sha256.Sum256(raw)
	cfg, err := ParseConfig(raw)
	if err != nil {
		return ShowResult{Exists: true, Path: p, SHA256: hex.EncodeToString(sum[:]), RawJSONC: string(raw)}, err
	}
	return ShowResult{
		Exists:   true,
		Path:     p,
		SHA256:   hex.EncodeToString(sum[:]),
		RawJSONC: string(raw),
		Config:   cfg,
	}, nil
}

func persistPolicyAndTrust(vaultDir string, rawPolicy []byte, tt extension.TrustedTarget) (string, error) {
	p := PolicyFilePath(vaultDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	if err := store.WriteAtomic(p, rawPolicy, 0o600); err != nil {
		return "", err
	}
	sum := sha256.Sum256(rawPolicy)
	shaHex := hex.EncodeToString(sum[:])
	if tt.Files == nil {
		tt.Files = make(map[string]string)
	}
	tt.Files["policy.json"] = shaHex
	if err := extension.UpsertTrustTarget(vaultDir, tt); err != nil {
		return "", err
	}
	return shaHex, nil
}

// ConfigurePolicy validates draftBytes, computes Targets["delegates"] binary pins, and runs ClassifyPolicyDiff.
// Non-widening edits or direct terminal CLI invocations apply immediately (0 popups); privilege-widening edits
// from chat stage a single 1-tap rsi_apply proposal (`hq delegate apply-policy`) fusing policy + binary pins.
func ConfigurePolicy(vaultDir string, draftBytes []byte, baseSHA256, convID string, isInteractiveCLI bool) (ConfigureResult, error) {
	oldCfg, oldSHA, hasOld, loadErr := LoadConfig(vaultDir)
	if loadErr != nil && hasOld {
		// Allow replacing a broken policy if baseSHA256 matches or wasn't provided.
		oldCfg = Config{}
	}
	if strings.TrimSpace(baseSHA256) != "" && hasOld {
		if !strings.EqualFold(strings.TrimSpace(baseSHA256), oldSHA) {
			return ConfigureResult{}, fmt.Errorf("optimistic concurrency conflict: policy.json changed (expected sha256 %s, current %s); re-run `hq delegate show`", baseSHA256, oldSHA)
		}
	}

	draftCfg, err := ParseConfig(draftBytes)
	if err != nil {
		return ConfigureResult{}, err
	}
	warnings, err := ValidateConfig(vaultDir, draftCfg, ValidateOptions{CheckBinaryPins: false})
	if err != nil {
		return ConfigureResult{}, err
	}
	tt, pinLines, err := BuildDelegatesTrustTarget(vaultDir, draftCfg)
	if err != nil {
		return ConfigureResult{}, err
	}

	var oldPtr *Config
	if hasOld && loadErr == nil {
		oldPtr = &oldCfg
	}
	widening, reasons := ClassifyPolicyDiff(oldPtr, hasOld && loadErr == nil, draftCfg)

	// Also treat changed binary hashes for an existing command as widening.
	if reg, rErr := extension.LoadTrustRegistry(vaultDir); rErr == nil {
		if prevT, ok := reg.Targets[TrustSourceDelegates]; ok {
			for k, newPin := range tt.Binaries {
				if prevPin, had := prevT.Binaries[k]; !had || prevPin.SHA256 != newPin.SHA256 {
					if !widening {
						widening = true
					}
					reasons = append(reasons, fmt.Sprintf("pinned/updated command binary %s", k))
				}
			}
		}
	}

	if isInteractiveCLI || !widening {
		shaHex, err := persistPolicyAndTrust(vaultDir, draftBytes, tt)
		if err != nil {
			return ConfigureResult{}, err
		}
		outcome := "applied_direct_cli"
		if !isInteractiveCLI {
			outcome = "applied_non_widening"
		}
		AppendAudit(vaultDir, AuditEntry{
			Action:  "configure",
			Outcome: outcome,
			Detail:  fmt.Sprintf("policy.json updated (sha256:%s, %d pinned bin(s))", shaHex[:12], len(tt.Binaries)),
		})
		return ConfigureResult{
			Applied:     true,
			Widening:    widening,
			Reasons:     reasons,
			Warnings:    warnings,
			SHA256:      shaHex,
			PinnedBins:  len(tt.Binaries),
			SummaryText: fmt.Sprintf("Applied delegate policy (%s, sha256:%s, %d pinned binary(s)).", outcome, shaHex[:12], len(tt.Binaries)),
		}, nil
	}

	// Privilege-widening edit from chat: fuse policy + Targets["delegates"] pins into 1-tap rsi_apply proposal.
	bundle := PolicyApplyBundle{
		SchemaVersion: PolicySchemaVersion,
		BaseSHA256:    oldSHA,
		RawPolicy:     string(draftBytes),
		TrustTarget:   tt,
		Reasons:       reasons,
	}
	payloadBytes, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return ConfigureResult{}, err
	}
	previewLines := []string{"Privilege-widening delegate policy changes:"}
	for _, r := range reasons {
		previewLines = append(previewLines, "- "+r)
	}
	for _, pl := range pinLines {
		previewLines = append(previewLines, "- pin "+pl)
	}
	exe := extension.ResolveSelfHQExecutable()
	prop, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:     "rsi_apply",
		Summary:        fmt.Sprintf("Apply widening delegate policy (%d policy(s), %d pinned binary(s))", len(draftCfg.Policies), len(tt.Binaries)),
		CommandArgv:    []string{exe, "delegate", "apply-policy", "--payload-file={{payload_file}}"},
		ConversationID: convID,
		PayloadBytes:   append(payloadBytes, '\n'),
		Metadata: map[string]string{
			"bodyPreview":  strings.Join(previewLines, "\n"),
			"draftPayload": strings.Join(previewLines, "\n"),
		},
	})
	if err != nil {
		return ConfigureResult{}, err
	}
	AppendAudit(vaultDir, AuditEntry{
		Action:  "configure",
		Outcome: "staged_approval",
		Detail:  fmt.Sprintf("staged proposal %s (%s)", prop.ID, strings.Join(reasons, "; ")),
	})
	return ConfigureResult{
		Staged:      true,
		Widening:    true,
		Reasons:     reasons,
		Warnings:    warnings,
		PinnedBins:  len(tt.Binaries),
		Proposal:    &prop,
		SummaryText: fmt.Sprintf("Staged 1-tap approval %s (sha256:%s) for privilege-widening delegate policy.", prop.ID, prop.SHA256[:12]),
	}, nil
}

// ApplyPolicyFD3 applies an approved PolicyApplyBundle delivered via /dev/fd/3 (`hq delegate apply-policy`).
func ApplyPolicyFD3(vaultDir string, rawBundle []byte) (string, error) {
	var bundle PolicyApplyBundle
	dec := json.NewDecoder(strings.NewReader(string(rawBundle)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&bundle); err != nil {
		return "", fmt.Errorf("invalid delegate policy bundle: %w", err)
	}
	if bundle.SchemaVersion != PolicySchemaVersion || strings.TrimSpace(bundle.RawPolicy) == "" {
		return "", fmt.Errorf("invalid delegate policy bundle schema or empty policy")
	}
	if _, oldSHA, hasOld, _ := LoadConfig(vaultDir); hasOld && bundle.BaseSHA256 != "" && !strings.EqualFold(oldSHA, bundle.BaseSHA256) {
		return "", fmt.Errorf("policy.json changed since proposal was staged (expected %s, current %s)", bundle.BaseSHA256, oldSHA)
	}
	cfg, err := ParseConfig([]byte(bundle.RawPolicy))
	if err != nil {
		return "", err
	}
	if _, err := ValidateConfig(vaultDir, cfg, ValidateOptions{CheckBinaryPins: false}); err != nil {
		return "", err
	}
	if bundle.TrustTarget.SourceID != TrustSourceDelegates {
		return "", fmt.Errorf("invalid trust target source_id %q", bundle.TrustTarget.SourceID)
	}
	shaHex, err := persistPolicyAndTrust(vaultDir, []byte(bundle.RawPolicy), bundle.TrustTarget)
	if err != nil {
		return "", err
	}
	AppendAudit(vaultDir, AuditEntry{
		Action:  "configure",
		Outcome: "applied_approved",
		Detail:  fmt.Sprintf("approved delegate policy applied (sha256:%s, %d pinned bin(s))", shaHex[:12], len(bundle.TrustTarget.Binaries)),
	})
	return shaHex, nil
}

// TrustCommandsRefresh re-hashes all commands in policy.json into Targets["delegates"]
// (applying directly in human terminal CLI or staging 1-tap rsi_apply from chat).
func TrustCommandsRefresh(vaultDir, convID string, isInteractiveCLI bool) (ConfigureResult, error) {
	show, err := ShowPolicy(vaultDir)
	if err != nil {
		return ConfigureResult{}, err
	}
	if !show.Exists {
		return ConfigureResult{}, fmt.Errorf("no delegate policy found at %s", show.Path)
	}
	return ConfigurePolicy(vaultDir, []byte(show.RawJSONC), show.SHA256, convID, isInteractiveCLI)
}

// BindTargetOverride sets or clears the explicit target task slug override for policyID (`hq delegate bind-target`).
func BindTargetOverride(vaultDir, policyID, taskSlug string) error {
	policyID = strings.TrimSpace(policyID)
	taskSlug = strings.TrimSpace(taskSlug)
	if !reValidSlugID.MatchString(policyID) {
		return fmt.Errorf("invalid policy_id %q", policyID)
	}
	if taskSlug != "" && !reValidTaskGrant.MatchString(taskSlug) {
		return fmt.Errorf("invalid task_slug %q", taskSlug)
	}
	return withStateMutate(vaultDir, time.Now().UTC(), func(st *State) error {
		if taskSlug == "" {
			delete(st.TargetOverrides, policyID)
		} else {
			st.TargetOverrides[policyID] = taskSlug
		}
		return nil
	})
}
