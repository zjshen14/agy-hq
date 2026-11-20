package extension

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/hostpaths"
)

func init() {
	approvals.RegisteredExtensionApprovalRulesProvider = func(vaultDir string) []approvals.ExtensionPackRules {
		packs := LoadActiveTrustedPacks(vaultDir)
		if len(packs) == 0 {
			return nil
		}
		out := make([]approvals.ExtensionPackRules, 0, len(packs))
		for _, dp := range packs {
			if len(dp.Manifest.Components.ApprovalRules) == 0 && len(dp.Manifest.Components.OwnedRepos) == 0 {
				continue
			}
			out = append(out, approvals.ExtensionPackRules{
				PackID:     dp.ID,
				OwnedRepos: dp.Manifest.Components.OwnedRepos,
				Rules:      dp.Manifest.Components.ApprovalRules,
			})
		}
		// RFC-0025 W4: every workspace provider's destructive_commands become one always-gated approval rule.
		// They are appended after all pack-declared rules so no pack can weaken them by reusing the synthetic
		// rule ID (packs can add gates, never remove them; only the user's Vault config.yaml may override).
		for _, dp := range packs {
			var wsRules []approvals.Rule
			for _, wp := range dp.Manifest.Components.WorkspaceProviders {
				rule, ok := WorkspaceProviderApprovalRule(dp.ID, wp)
				if !ok || approvals.ValidateRule(rule, false) != nil {
					continue
				}
				wsRules = append(wsRules, rule)
			}
			if len(wsRules) > 0 {
				out = append(out, approvals.ExtensionPackRules{PackID: dp.ID, Rules: wsRules})
			}
		}
		return out
	}
}

const (
	PackSchemaVersion       = 1
	MaxOverlayBytes         = 4096
	MaxTotalOverlaysBytes   = 16384
	ChannelBridgeProtocolV1 = "hq-channel-bridge/v1"

	// WorkspaceProviderProtocolV1 is the frozen JSON-over-stdio protocol for pack workspace providers (RFC-0025 §6).
	WorkspaceProviderProtocolV1 = "hq-workspace-provider/v1"
	// MaxWorkspaceProviderTimeoutMs caps a provider's per-call timeout_ms (workspace creation can take tens of seconds).
	MaxWorkspaceProviderTimeoutMs = 60000
	// MinWorkspaceStatusRefreshSeconds and MaxWorkspaceStatusRefreshSeconds bound a non-zero status_refresh_seconds.
	MinWorkspaceStatusRefreshSeconds = 120
	MaxWorkspaceStatusRefreshSeconds = 86400
	// WorkspaceDestructiveRulePrefix prefixes the synthetic approval rule IDs generated from destructive_commands.
	// Pack-declared approval_rules may not use it.
	WorkspaceDestructiveRulePrefix = "wsd-"
)

var (
	reValidPackID          = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	reValidPackSkillDir    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	reValidWorkspaceSubdir = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)
)

// RequiredBin describes an external CLI binary prerequisite declared in hq-extension.json.
type RequiredBin struct {
	Name       string   `json:"name"`
	ProbeArgv  []string `json:"probe_argv,omitempty"`
	MinVersion string   `json:"min_version,omitempty"`
}

// PackRequires declares host binary and desktop-session prerequisites for a companion pack.
type PackRequires struct {
	Bins           []RequiredBin `json:"bins,omitempty"`
	DesktopSession bool          `json:"desktop_session,omitempty"`
}

// PackPrecheck specifies an argv precheck for a pack-contributed schedule.
type PackPrecheck struct {
	Argv      []string `json:"argv"`
	DedupMode string   `json:"dedup_mode,omitempty"`
	TimeoutMs int      `json:"timeout_ms,omitempty"`
}

// PackSchedule defines a declarative schedule inside hq-extension.json.
type PackSchedule struct {
	ID       string        `json:"id"`
	Cron     string        `json:"cron"`
	TZ       string        `json:"tz,omitempty"`
	Tier     string        `json:"tier,omitempty"`
	Mode     string        `json:"mode,omitempty"`
	Skill    string        `json:"skill,omitempty"`
	Prompt   string        `json:"prompt,omitempty"`
	Delivery string        `json:"delivery,omitempty"`
	Enabled  *bool         `json:"enabled,omitempty"`
	CatchUp  string        `json:"catch_up,omitempty"`
	Precheck *PackPrecheck `json:"precheck,omitempty"`
}

// PackInboxRule defines a file-drop routing rule contributed by a companion pack.
type PackInboxRule struct {
	ID            string   `json:"id"`
	Extensions    []string `json:"extensions,omitempty"`
	FilenameRegex string   `json:"filename_regex,omitempty"`
	Pattern       string   `json:"pattern,omitempty"`
	Skill         string   `json:"skill"`
}

// SkillOverrideSpec maps a logical core skill to a companion pack skill.
type SkillOverrideSpec struct {
	Skill   string `json:"skill"`
	SkillMD string `json:"skill_md,omitempty"`
}

// OverlaySpec declares a role-scoped Front Desk / Task markdown policy overlay (<= 4 KB).
type OverlaySpec struct {
	ID       string   `json:"id"`
	Path     string   `json:"path"`
	Scope    []string `json:"scope,omitempty"`
	MaxBytes int      `json:"max_bytes,omitempty"`
}

// PackTierModel holds a model target inside routing_defaults.tiers.
type PackTierModel struct {
	Model         string `json:"model"`
	SubagentModel string `json:"subagent_model,omitempty"`
}

// PackRoutingDefaults provides opt-in tier->model defaults for enterprise hosts.
type PackRoutingDefaults struct {
	Tiers map[string]PackTierModel `json:"tiers,omitempty"`
}

// BridgeCapabilitiesSpec declares optional capabilities supported by a channel bridge.
type BridgeCapabilitiesSpec struct {
	DeskBinding     bool `json:"desk_binding,omitempty"`
	ItemThreads     bool `json:"item_threads,omitempty"`
	TaskDigests     bool `json:"task_digests,omitempty"`
	NativeAskCards  bool `json:"native_ask_cards,omitempty"`
	CallbackButtons bool `json:"callback_buttons,omitempty"`
}

// ChannelBridgeSpec declares a vendor-neutral Front Desk channel bridge in hq-extension.json.
type ChannelBridgeSpec struct {
	ID                     string                 `json:"id"`
	Protocol               string                 `json:"protocol"`
	Description            string                 `json:"description,omitempty"`
	Command                []string               `json:"command"`
	EnvAllow               []string               `json:"env_allow,omitempty"`
	RequiresDesktopSession bool                   `json:"requires_desktop_session,omitempty"`
	TimeoutMs              int                    `json:"timeout_ms,omitempty"`
	MaxBodyRunes           int                    `json:"max_body_runes,omitempty"`
	Capabilities           BridgeCapabilitiesSpec `json:"capabilities,omitempty"`
}

var (
	reValidBridgeEnvKey = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	blockedBridgeEnv    = map[string]bool{
		"ANTIGRAVITY_CSRF_TOKEN":      true,
		"ANTIGRAVITY_LS_ADDRESS":      true,
		"ANTIGRAVITY_AGENTAPI_EXE":    true,
		"ANTIGRAVITY_CONVERSATION_ID": true,
		"ANTIGRAVITY_TRAJECTORY_ID":   true,
		"ANTIGRAVITY_PROJECT_ID":      true,
		"ANTIGRAVITY_SOURCE_METADATA": true,
	}
	blockedBridgeShellBinaries = map[string]bool{
		"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "csh": true, "tcsh": true, "fish": true,
		"pwsh": true, "powershell": true, "busybox": true,
		"ssh": true, "env": true, "xargs": true, "sudo": true, "su": true,
		"go": true, "npx": true, "pnpx": true, "bunx": true, "uv": true, "uvx": true, "pipx": true,
		"python": true, "python2": true, "python3": true,
		"node": true, "nodejs": true, "deno": true, "bun": true,
		"perl": true, "ruby": true, "php": true, "lua": true, "awk": true, "gawk": true, "sed": true,
	}
)

func isBlockedBridgeCommand0(raw string) bool {
	if IsSelfHQBinary(raw) {
		return true
	}
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(strings.TrimSpace(raw)), ".exe"))
	if base == "" || base == "." || base == ".." {
		return true
	}
	if blockedBridgeShellBinaries[base] {
		return true
	}
	return strings.HasPrefix(base, "python2.") || strings.HasPrefix(base, "python3.")
}

// IsBlockedComponentCommand0 reports whether raw (a pack component's command[0]) is a shell, generic interpreter,
// or the hq binary itself. Runtime executors re-check it before every invocation as defense in depth.
func IsBlockedComponentCommand0(raw string) bool {
	return isBlockedBridgeCommand0(raw)
}

// WorkspaceProviderCapabilitiesSpec declares the optional verbs a workspace provider supports (RFC-0025 §5.1).
type WorkspaceProviderCapabilitiesSpec struct {
	Create           bool `json:"create,omitempty"`
	Remove           bool `json:"remove,omitempty"`
	Changes          bool `json:"changes,omitempty"`
	SharedHistory    bool `json:"shared_history,omitempty"`
	ForeignPlacement bool `json:"foreign_placement,omitempty"` // deferred in v1; LintPack rejects true
}

// WorkspaceProviderSpec declares a vendor-neutral workspace provider in hq-extension.json (RFC-0025 §5.1).
// Its runtime FQID is "<pack>/<id>"; command[0] is SHA-256 pinned and executed exactly like a channel bridge.
type WorkspaceProviderSpec struct {
	ID                   string                            `json:"id"`
	Protocol             string                            `json:"protocol"`
	Description          string                            `json:"description,omitempty"`
	Command              []string                          `json:"command"`
	EnvAllow             []string                          `json:"env_allow,omitempty"`
	TimeoutMs            int                               `json:"timeout_ms,omitempty"`
	RootPatterns         []string                          `json:"root_patterns,omitempty"`
	WorkSubdir           string                            `json:"work_subdir,omitempty"`
	Capabilities         WorkspaceProviderCapabilitiesSpec `json:"capabilities,omitempty"`
	StatusRefreshSeconds int                               `json:"status_refresh_seconds,omitempty"`
	DestructiveCommands  []string                          `json:"destructive_commands,omitempty"`
	Skill                string                            `json:"skill,omitempty"`
}

// PackComponents indexes all hq-mediated surfaces inside a companion extension plugin.
type PackComponents struct {
	Connectors         []string                     `json:"connectors,omitempty"`
	BriefExtensions    []string                     `json:"brief_extensions,omitempty"`
	Schedules          []PackSchedule               `json:"schedules,omitempty"`
	InboxRules         []PackInboxRule              `json:"inbox_rules,omitempty"`
	SkillOverrides     map[string]SkillOverrideSpec `json:"skill_overrides,omitempty"`
	FrontDeskOverlays  []OverlaySpec                `json:"front_desk_overlays,omitempty"`
	RoutingDefaults    *PackRoutingDefaults         `json:"routing_defaults,omitempty"`
	ConnectorDefaults  map[string]string            `json:"connector_defaults,omitempty"`
	ApprovalRules      []approvals.Rule             `json:"approval_rules,omitempty"`
	OwnedRepos         []string                     `json:"owned_repos,omitempty"`
	ChannelBridges     []ChannelBridgeSpec          `json:"channel_bridges,omitempty"`
	WorkspaceProviders []WorkspaceProviderSpec      `json:"workspace_providers,omitempty"`
}

// PackManifest represents <plugin>/hq-extension.json.
type PackManifest struct {
	SchemaVersion int            `json:"schema_version"`
	ID            string         `json:"id"`
	Version       string         `json:"version"`
	MinHQVersion  string         `json:"min_hq_version,omitempty"`
	Description   string         `json:"description,omitempty"`
	Requires      PackRequires   `json:"requires,omitempty"`
	Components    PackComponents `json:"components,omitempty"`
}

// strictPluginManifest validates untouched Antigravity plugin.json compatibility (`agy plugin validate --strict`).
type strictPluginManifest struct {
	Name        string      `json:"name"`
	Version     string      `json:"version"`
	Description string      `json:"description,omitempty"`
	Author      interface{} `json:"author,omitempty"`
	License     string      `json:"license,omitempty"`
	Homepage    string      `json:"homepage,omitempty"`
	Repository  interface{} `json:"repository,omitempty"`
	Keywords    []string    `json:"keywords,omitempty"`
}

// DiscoveredPack summarizes a discovered companion extension pack and its activation/trust status.
type DiscoveredPack struct {
	ID       string       `json:"id"`
	Version  string       `json:"version"`
	Path     string       `json:"path"`
	Enabled  bool         `json:"enabled"`
	Trusted  bool         `json:"trusted"`
	Manifest PackManifest `json:"manifest"`
}

// LoadPackManifest strictly parses <packDir>/hq-extension.json using DisallowUnknownFields().
func LoadPackManifest(packDir string) (PackManifest, error) {
	idxPath := filepath.Join(packDir, "hq-extension.json")
	raw, err := os.ReadFile(idxPath)
	if err != nil {
		return PackManifest{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m PackManifest
	if err := dec.Decode(&m); err != nil {
		return PackManifest{}, fmt.Errorf("parse %s: %w", idxPath, err)
	}
	if m.SchemaVersion != PackSchemaVersion {
		return PackManifest{}, fmt.Errorf("%s unsupported schema_version %d (expected %d)", idxPath, m.SchemaVersion, PackSchemaVersion)
	}
	m.ID = strings.TrimSpace(m.ID)
	if m.ID == "core" || m.ID == "vault" {
		return PackManifest{}, fmt.Errorf("%s uses reserved pack id %q", idxPath, m.ID)
	}
	if !reValidPackID.MatchString(m.ID) {
		return PackManifest{}, fmt.Errorf("%s invalid pack id %q", idxPath, m.ID)
	}
	if strings.TrimSpace(m.Version) == "" {
		return PackManifest{}, fmt.Errorf("%s missing version", idxPath)
	}
	return m, nil
}

// LintPack validates a companion extension pack directory against strict schema, `plugin.json` parity,
// and the monotonic Security Lattice (`RFC-0017d` §2.2).
func LintPack(packDir string) ([]string, error) {
	var warnings []string
	m, err := LoadPackManifest(packDir)
	if err != nil {
		return nil, err
	}

	// 1. Validate untouched plugin.json compatibility when present
	plugJSONPath := filepath.Join(packDir, "plugin.json")
	if rawPlug, pErr := os.ReadFile(plugJSONPath); pErr == nil {
		dec := json.NewDecoder(bytes.NewReader(rawPlug))
		dec.DisallowUnknownFields()
		var pm strictPluginManifest
		if err := dec.Decode(&pm); err != nil {
			return nil, fmt.Errorf("plugin.json failed strict Antigravity schema validation (keep plugin.json untouched and place HQ metadata in hq-extension.json): %w", err)
		}
		if strings.TrimSpace(pm.Name) != m.ID {
			return nil, fmt.Errorf("hq-extension.json id %q must equal plugin.json name %q", m.ID, pm.Name)
		}
	} else {
		warnings = append(warnings, "plugin.json not found (recommended for standard Antigravity plugin distribution)")
	}

	// 2. Reject overriding non-overridable quarantine skills
	for coreSkill, so := range m.Components.SkillOverrides {
		if NonOverridableCoreSkills[strings.TrimSpace(coreSkill)] {
			return nil, fmt.Errorf("skill_overrides cannot shadow security-critical skill %q", coreSkill)
		}
		rel := so.SkillMD
		if strings.TrimSpace(rel) == "" && strings.TrimSpace(so.Skill) != "" {
			rel = filepath.Join("skills", so.Skill, "SKILL.md")
		}
		if _, sErr := os.Stat(filepath.Join(packDir, filepath.FromSlash(rel))); sErr != nil {
			return nil, fmt.Errorf("skill_override %q target missing: %s", coreSkill, rel)
		}
	}

	// 3. Monotonic Security Lattice checks on connectors, brief_extensions, and hooks.json
	checkConnectorSec := func(rel string) error {
		full := filepath.Join(packDir, filepath.FromSlash(rel))
		raw, err := os.ReadFile(full)
		if err != nil {
			return fmt.Errorf("missing connector manifest %s: %w", rel, err)
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("invalid JSON in %s: %w", rel, err)
		}
		if pl, exists := obj["participant_lock"].(bool); exists && !pl {
			return fmt.Errorf("%s sets participant_lock=false (companion packs cannot loosen security lattice defaults; configure Vault security_loosening instead)", rel)
		}
		return nil
	}
	for _, c := range m.Components.Connectors {
		if err := checkConnectorSec(c); err != nil {
			return nil, err
		}
	}
	for _, c := range m.Components.ConnectorDefaults {
		if err := checkConnectorSec(c); err != nil {
			return nil, err
		}
	}

	for _, b := range m.Components.BriefExtensions {
		if !strings.HasSuffix(strings.ToLower(strings.TrimSpace(b)), ".json") {
			return nil, fmt.Errorf("brief_extension %q must be a mode=argv .json file (free-form .md prompts are forbidden in companion packs)", b)
		}
		full := filepath.Join(packDir, filepath.FromSlash(b))
		raw, err := os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("missing brief_extension %s: %w", b, err)
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("invalid JSON in %s: %w", b, err)
		}
		if mode, _ := obj["mode"].(string); mode != "" && mode != "argv" {
			return nil, fmt.Errorf("%s invalid mode %q (companion packs only permit mode=\"argv\")", b, mode)
		}
		if q, exists := obj["quarantined"].(bool); exists && !q {
			return nil, fmt.Errorf("%s sets quarantined=false (companion packs cannot loosen brief provenance; use Vault security_loosening)", b)
		}
	}

	if hooksRaw, hErr := os.ReadFile(filepath.Join(packDir, "hooks.json")); hErr == nil {
		hs := string(hooksRaw)
		if strings.Contains(hs, "PreToolUse") || strings.Contains(hs, "PreInvocation") {
			return nil, fmt.Errorf("companion pack hooks.json registers PreToolUse/PreInvocation hooks (rejected by monotonic security lattice)")
		}
	}

	// 4. Validate front_desk_overlays size, scope patterns, and safety-bypass phrases
	totalOvBytes := 0
	for _, ov := range m.Components.FrontDeskOverlays {
		if err := validateOverlayScopes(ov.Scope); err != nil {
			return nil, fmt.Errorf("front_desk_overlay %s invalid scope: %w", ov.Path, err)
		}
		full := filepath.Join(packDir, filepath.FromSlash(ov.Path))
		raw, err := os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("missing front_desk_overlay %s: %w", ov.Path, err)
		}
		capBytes := MaxOverlayBytes
		if ov.MaxBytes > 0 && ov.MaxBytes < capBytes {
			capBytes = ov.MaxBytes
		}
		if len(raw) > capBytes {
			return nil, fmt.Errorf("front_desk_overlay %s is %d bytes (exceeds %d-byte cap)", ov.Path, len(raw), capBytes)
		}
		totalOvBytes += len(raw)
		if totalOvBytes > MaxTotalOverlaysBytes {
			return nil, fmt.Errorf("total front_desk_overlays size %d exceeds %d-byte cap", totalOvBytes, MaxTotalOverlaysBytes)
		}
		lower := strings.ToLower(string(raw))
		for _, forbidden := range []string{"bypass hq propose", "skip ask_question", "send email directly without approval", "ignore quarantine"} {
			if strings.Contains(lower, forbidden) {
				return nil, fmt.Errorf("front_desk_overlay %s contains forbidden safety-bypass phrase %q", ov.Path, forbidden)
			}
		}
	}

	// 5. Validate approval_rules schema, regex compilation, and monotonic security lattice
	seenApprovalRuleIDs := make(map[string]bool)
	for _, ar := range m.Components.ApprovalRules {
		rid := strings.TrimSpace(ar.ID)
		if seenApprovalRuleIDs[rid] {
			return nil, fmt.Errorf("duplicate approval_rules id %q in hq-extension.json", rid)
		}
		seenApprovalRuleIDs[rid] = true
		isBuiltinOverride := approvals.CoreNonDisableablePackRules[rid] || rid == "pr_merge" || rid == "forge_git"
		if err := approvals.ValidateRule(ar, isBuiltinOverride); err != nil {
			return nil, fmt.Errorf("approval_rules: %w", err)
		}
		if approvals.CoreNonDisableablePackRules[rid] && strings.ToLower(strings.TrimSpace(ar.Mode)) != approvals.ModeAlways {
			return nil, fmt.Errorf("approval_rules %q sets mode=%q (companion packs cannot weaken core always-gated rules; configure Vault config.yaml instead)", rid, ar.Mode)
		}
	}

	// 6. Validate channel_bridges schema, protocol, non-shell binary, and env_allow safety
	seenBridgeIDs := make(map[string]bool)
	for _, cb := range m.Components.ChannelBridges {
		bid := strings.TrimSpace(cb.ID)
		if !reValidPackID.MatchString(bid) {
			return nil, fmt.Errorf("channel_bridges invalid id %q (expected lowercase kebab-case)", cb.ID)
		}
		if seenBridgeIDs[bid] {
			return nil, fmt.Errorf("duplicate channel_bridges id %q in hq-extension.json", bid)
		}
		seenBridgeIDs[bid] = true
		if strings.TrimSpace(cb.Protocol) != ChannelBridgeProtocolV1 {
			return nil, fmt.Errorf("channel_bridges %q unsupported protocol %q (expected %q)", bid, cb.Protocol, ChannelBridgeProtocolV1)
		}
		if len(cb.Command) == 0 || strings.TrimSpace(cb.Command[0]) == "" {
			return nil, fmt.Errorf("channel_bridges %q requires non-empty command", bid)
		}
		if isBlockedBridgeCommand0(cb.Command[0]) {
			return nil, fmt.Errorf("channel_bridges %q command[0] %q cannot be a shell or generic interpreter wrapper (must point to a compiled SHA-256-pinnable executable)", bid, cb.Command[0])
		}
		if cb.TimeoutMs < 0 || cb.TimeoutMs > 15000 {
			return nil, fmt.Errorf("channel_bridges %q timeout_ms=%d out of range [0, 15000]", bid, cb.TimeoutMs)
		}
		if cb.MaxBodyRunes != 0 && (cb.MaxBodyRunes < 80 || cb.MaxBodyRunes > 4000) {
			return nil, fmt.Errorf("channel_bridges %q max_body_runes=%d out of range (expected 0 or [80, 4000])", bid, cb.MaxBodyRunes)
		}
		for _, envKey := range cb.EnvAllow {
			ek := strings.TrimSpace(envKey)
			if !reValidBridgeEnvKey.MatchString(ek) {
				return nil, fmt.Errorf("channel_bridges %q invalid env_allow entry %q", bid, envKey)
			}
			if blockedBridgeEnv[ek] {
				return nil, fmt.Errorf("channel_bridges %q env_allow cannot expose protected host variable %q", bid, ek)
			}
		}
	}

	// 7. Validate workspace_providers (RFC-0025 §5.2): id, protocol, non-shell pinned binary, env_allow,
	// timeouts, root_patterns territory, destructive_commands approval rules, and the optional pack skill guide.
	for _, ar := range m.Components.ApprovalRules {
		if strings.HasPrefix(strings.TrimSpace(ar.ID), WorkspaceDestructiveRulePrefix) {
			return nil, fmt.Errorf("approval_rules id %q uses the %q prefix reserved for workspace_providers destructive_commands", ar.ID, WorkspaceDestructiveRulePrefix)
		}
	}
	seenWorkspaceProviderIDs := make(map[string]bool)
	for _, wp := range m.Components.WorkspaceProviders {
		if err := ValidateWorkspaceProviderSpec(m.ID, wp); err != nil {
			return nil, err
		}
		if seenWorkspaceProviderIDs[wp.ID] {
			return nil, fmt.Errorf("duplicate workspace_providers id %q in hq-extension.json", wp.ID)
		}
		seenWorkspaceProviderIDs[wp.ID] = true
		if skill := wp.Skill; skill != "" {
			rel := filepath.Join("skills", skill, "SKILL.md")
			if st, sErr := os.Stat(filepath.Join(packDir, rel)); sErr != nil || !st.Mode().IsRegular() {
				return nil, fmt.Errorf("workspace_providers %q skill %q must resolve to %s inside the pack", wp.ID, skill, filepath.ToSlash(rel))
			}
		}
	}

	return warnings, nil
}

// ValidateWorkspaceProviderSpec checks a single workspace_providers entry against RFC-0025 §5.2 rules 1-8
// (except pack-level id uniqueness and the skill file's existence, which LintPack checks against the pack
// directory). Runtime registries re-run it on every load, so a manifest that was re-trusted without being
// re-linted still fails closed.
func ValidateWorkspaceProviderSpec(packID string, wp WorkspaceProviderSpec) error {
	pid := wp.ID
	if pid != strings.TrimSpace(pid) || !reValidPackID.MatchString(pid) {
		return fmt.Errorf("workspace_providers invalid id %q (expected lowercase kebab-case)", wp.ID)
	}
	if pid == "core" {
		return fmt.Errorf("workspace_providers id %q is reserved", pid)
	}
	if strings.TrimSpace(wp.Protocol) != WorkspaceProviderProtocolV1 {
		return fmt.Errorf("workspace_providers %q unsupported protocol %q (expected %q)", pid, wp.Protocol, WorkspaceProviderProtocolV1)
	}
	if len(wp.Command) == 0 || strings.TrimSpace(wp.Command[0]) == "" {
		return fmt.Errorf("workspace_providers %q requires non-empty command", pid)
	}
	if isBlockedBridgeCommand0(wp.Command[0]) {
		return fmt.Errorf("workspace_providers %q command[0] %q cannot be a shell, generic interpreter wrapper, or hq itself (must point to a compiled SHA-256-pinnable executable)", pid, wp.Command[0])
	}
	for _, envKey := range wp.EnvAllow {
		ek := strings.TrimSpace(envKey)
		if !reValidBridgeEnvKey.MatchString(ek) {
			return fmt.Errorf("workspace_providers %q invalid env_allow entry %q", pid, envKey)
		}
		if blockedBridgeEnv[ek] {
			return fmt.Errorf("workspace_providers %q env_allow cannot expose protected host variable %q", pid, ek)
		}
	}
	if wp.TimeoutMs < 0 || wp.TimeoutMs > MaxWorkspaceProviderTimeoutMs {
		return fmt.Errorf("workspace_providers %q timeout_ms=%d out of range [0, %d]", pid, wp.TimeoutMs, MaxWorkspaceProviderTimeoutMs)
	}
	if wp.StatusRefreshSeconds != 0 && (wp.StatusRefreshSeconds < MinWorkspaceStatusRefreshSeconds || wp.StatusRefreshSeconds > MaxWorkspaceStatusRefreshSeconds) {
		return fmt.Errorf("workspace_providers %q status_refresh_seconds=%d out of range (expected 0 or [%d, %d])", pid, wp.StatusRefreshSeconds, MinWorkspaceStatusRefreshSeconds, MaxWorkspaceStatusRefreshSeconds)
	}
	guardDirs := workspaceRootGuardDirs()
	for _, pat := range wp.RootPatterns {
		if err := validateWorkspaceRootPattern(pat, guardDirs); err != nil {
			return fmt.Errorf("workspace_providers %q root_patterns entry %q %v", pid, pat, err)
		}
	}
	if sub := wp.WorkSubdir; sub != "" {
		clean := filepath.ToSlash(filepath.Clean(sub))
		if !reValidWorkspaceSubdir.MatchString(sub) || filepath.IsAbs(sub) || clean != sub || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("workspace_providers %q work_subdir %q must be a clean relative path inside the workspace root", pid, sub)
		}
	}
	if wp.Capabilities.ForeignPlacement {
		return fmt.Errorf("workspace_providers %q sets capabilities.foreign_placement=true, but Foreign placement is deferred in v1 (RFC-0025 §16.5; only Home placement ships)", pid)
	}
	for _, pat := range wp.DestructiveCommands {
		if strings.TrimSpace(pat) == "" {
			return fmt.Errorf("workspace_providers %q destructive_commands contains an empty pattern", pid)
		}
	}
	if rule, ok := WorkspaceProviderApprovalRule(packID, wp); ok {
		if err := approvals.ValidateRule(rule, false); err != nil {
			return fmt.Errorf("workspace_providers %q destructive_commands: %w", pid, err)
		}
	}
	if skill := wp.Skill; skill != "" && !reValidPackSkillDir.MatchString(skill) {
		return fmt.Errorf("workspace_providers %q skill %q must be a plain skill directory name under skills/", pid, skill)
	}
	return nil
}

// WorkspaceDestructiveRuleID returns the synthetic approval rule ID that gates a workspace provider's
// destructive_commands: "wsd-<pack>-<id>", shortened with a stable hash suffix when it would exceed the
// 63-character approval rule ID limit.
func WorkspaceDestructiveRuleID(packID, providerID string) string {
	packID = strings.ToLower(strings.TrimSpace(packID))
	providerID = strings.ToLower(strings.TrimSpace(providerID))
	id := WorkspaceDestructiveRulePrefix + packID + "-" + providerID
	if len(id) <= 63 {
		return id
	}
	sum := sha256.Sum256([]byte(packID + "/" + providerID))
	return id[:54] + "-" + hex.EncodeToString(sum[:4])
}

// WorkspaceProviderApprovalRule converts a provider's destructive_commands into an always-gated approval rule.
// It returns false when the provider declares no destructive commands.
func WorkspaceProviderApprovalRule(packID string, wp WorkspaceProviderSpec) (approvals.Rule, bool) {
	var cmds []string
	for _, c := range wp.DestructiveCommands {
		if s := strings.TrimSpace(c); s != "" {
			cmds = append(cmds, s)
		}
	}
	if len(cmds) == 0 {
		return approvals.Rule{}, false
	}
	fqid := strings.TrimSpace(packID) + "/" + strings.TrimSpace(wp.ID)
	return approvals.Rule{
		ID:          WorkspaceDestructiveRuleID(packID, wp.ID),
		Description: "Destructive workspace commands declared by workspace provider " + fqid,
		Mode:        approvals.ModeAlways,
		Commands:    cmds,
	}, true
}

// workspaceRootGuardDirs returns the directories root_patterns may never match, contain, or sit under:
// the user's home directory (literal and symlink-resolved). The Vault normally lives under it; runtime path
// matching additionally ignores the Vault wherever it lives.
func workspaceRootGuardDirs() []string {
	var dirs []string
	add := func(d string) {
		d = filepath.Clean(strings.TrimSpace(d))
		if d == "" || d == "." || d == string(filepath.Separator) || !filepath.IsAbs(d) {
			return
		}
		for _, existing := range dirs {
			if existing == d {
				return
			}
		}
		dirs = append(dirs, d)
	}
	if home, err := hostpaths.ResolveHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		add(home)
		if resolved, rErr := filepath.EvalSymlinks(home); rErr == nil {
			add(resolved)
		}
	}
	return dirs
}

// validateWorkspaceRootPattern enforces RFC-0025 §5.2 rule 6 on one root_patterns entry.
func validateWorkspaceRootPattern(pat string, guardDirs []string) error {
	if pat == "" || pat != strings.TrimSpace(pat) {
		return fmt.Errorf("must be a non-empty pattern without surrounding whitespace")
	}
	for _, r := range pat {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("must not contain control characters")
		}
	}
	if !filepath.IsAbs(pat) {
		return fmt.Errorf("must be an absolute path pattern")
	}
	for _, seg := range strings.Split(pat, "/") {
		if seg == ".." {
			return fmt.Errorf("must not contain '..'")
		}
	}
	if filepath.Clean(pat) != pat {
		return fmt.Errorf("must be a clean path pattern (no '.', '//' or trailing '/')")
	}
	if _, err := filepath.Match(pat, "/"); err != nil {
		return fmt.Errorf("has invalid glob syntax: %v", err)
	}
	if pat == "/" || pat == "/*" {
		return fmt.Errorf("must not claim the filesystem root")
	}
	if ok, _ := filepath.Match(pat, "/"); ok {
		return fmt.Errorf("must not match the filesystem root")
	}
	for _, dir := range guardDirs {
		if workspacePatternOverlapsDir(pat, dir) {
			return fmt.Errorf("must not match, contain, or sit under the user's home directory %s", dir)
		}
	}
	return nil
}

// workspacePatternOverlapsDir reports whether pattern could match dir, one of dir's ancestors, or any path
// beneath dir (every segment the two have in common matches).
func workspacePatternOverlapsDir(pattern, dir string) bool {
	pSegs := splitAbsPathSegments(pattern)
	dSegs := splitAbsPathSegments(dir)
	if len(dSegs) == 0 {
		return true
	}
	n := len(pSegs)
	if len(dSegs) < n {
		n = len(dSegs)
	}
	for i := 0; i < n; i++ {
		ok, err := filepath.Match(pSegs[i], dSegs[i])
		if err != nil || !ok {
			return false
		}
	}
	return true
}

func splitAbsPathSegments(p string) []string {
	trimmed := strings.Trim(filepath.ToSlash(p), "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// ResolvePackDirectory finds the filesystem directory for packID via explicitPath, VaultState, or host plugin roots.
func ResolvePackDirectory(vaultDir, packID, explicitPath string) (string, error) {
	if strings.TrimSpace(explicitPath) != "" {
		return canonicalDir(explicitPath)
	}
	packID = strings.TrimSpace(packID)
	if packID == "" {
		return "", fmt.Errorf("pack ID is required")
	}
	if st, err := LoadVaultState(vaultDir); err == nil {
		if act, ok := st.Packs[packID]; ok && strings.TrimSpace(act.Path) != "" {
			if c, cErr := canonicalDir(act.Path); cErr == nil {
				return c, nil
			}
		}
	}
	roots := hostpaths.Resolve(vaultDir)
	candidates := []string{
		filepath.Join(roots.PluginsDir, packID),
		filepath.Join(roots.AppDataDir, "hq", "plugins", packID),
	}
	if strings.TrimSpace(vaultDir) != "" {
		candidates = append(candidates, filepath.Join(vaultDir, "packs", packID))
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "hq-extension.json")); err == nil && !st.IsDir() {
			return canonicalDir(c)
		}
	}
	return "", fmt.Errorf("companion extension pack %q not found in %s", packID, roots.PluginsDir)
}

// DiscoverPacks discovers all installed or registered companion packs containing hq-extension.json.
func DiscoverPacks(vaultDir string) ([]DiscoveredPack, error) {
	roots := hostpaths.Resolve(vaultDir)
	st, _ := LoadVaultState(vaultDir)
	reg, _ := LoadTrustRegistry(vaultDir)

	seenDirs := make(map[string]bool)
	seenIDs := make(map[string]DiscoveredPack)

	tryDir := func(dir string) {
		canon, err := canonicalDir(dir)
		if err != nil || seenDirs[canon] {
			return
		}
		if info, sErr := os.Stat(filepath.Join(canon, "hq-extension.json")); sErr != nil || info.IsDir() {
			return
		}
		seenDirs[canon] = true
		m, err := LoadPackManifest(canon)
		if err != nil {
			return
		}
		act := st.Packs[m.ID]
		_, vErr := VerifyTrustedFileReadOnly(vaultDir, "pack:"+m.ID, filepath.Join(canon, "hq-extension.json"))
		if tt, hasT := reg.Targets["pack:"+m.ID]; !hasT || tt.ManifestSHA256 == "" {
			vErr = fmt.Errorf("untrusted")
		}
		seenIDs[m.ID] = DiscoveredPack{
			ID:       m.ID,
			Version:  m.Version,
			Path:     canon,
			Enabled:  act.Enabled,
			Trusted:  vErr == nil,
			Manifest: m,
		}
	}

	for _, act := range st.Packs {
		if strings.TrimSpace(act.Path) != "" {
			tryDir(act.Path)
		}
	}
	for _, parent := range []string{
		roots.PluginsDir,
		filepath.Join(roots.AppDataDir, "hq", "plugins"),
		filepath.Join(vaultDir, "packs"),
	} {
		if strings.TrimSpace(parent) == "" {
			continue
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				tryDir(filepath.Join(parent, e.Name()))
			}
		}
	}

	out := make([]DiscoveredPack, 0, len(seenIDs))
	for _, dp := range seenIDs {
		out = append(out, dp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func normalizePackRoutingKey(k string) string {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "deep-medium", "deep_medium", "deep-routine", "deep_routine":
		return "deep_routine"
	default:
		return strings.ToLower(strings.TrimSpace(k))
	}
}

func connectorBaseID(packDir, rel string) string {
	raw, err := os.ReadFile(filepath.Join(packDir, filepath.FromSlash(rel)))
	if err == nil {
		var obj struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &obj) == nil && strings.TrimSpace(obj.ID) != "" {
			return strings.TrimSpace(obj.ID)
		}
	}
	return strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
}

// EnablePack validates, checks unqualified component ID collisions against already-enabled packs,
// and activates packID in <vault>/extensions.json.
func EnablePack(ctx context.Context, vaultDir, packID, explicitPath string, usePackRouting bool) (PackManifest, error) {
	packDir, err := ResolvePackDirectory(vaultDir, packID, explicitPath)
	if err != nil {
		return PackManifest{}, err
	}
	if _, lErr := LintPack(packDir); lErr != nil {
		return PackManifest{}, lErr
	}
	m, err := LoadPackManifest(packDir)
	if err != nil {
		return PackManifest{}, err
	}

	err = UpdateVaultState(ctx, vaultDir, func(st *VaultState) error {
		// Detect unqualified connector ID collisions across enabled packs unless WinnerOverrides disambiguates
		myConnIDs := make(map[string]bool)
		for _, c := range m.Components.Connectors {
			myConnIDs[connectorBaseID(packDir, c)] = true
		}
		myBridgeIDs := make(map[string]bool)
		for _, cb := range m.Components.ChannelBridges {
			myBridgeIDs[strings.TrimSpace(cb.ID)] = true
		}
		for otherID, act := range st.Packs {
			if otherID == m.ID || !act.Enabled || strings.TrimSpace(act.Path) == "" {
				continue
			}
			om, oErr := LoadPackManifest(act.Path)
			if oErr != nil {
				continue
			}
			for _, oc := range om.Components.Connectors {
				cid := connectorBaseID(act.Path, oc)
				if myConnIDs[cid] && strings.TrimSpace(st.WinnerOverrides[cid]) == "" {
					return fmt.Errorf("unqualified connector ID collision on %q between enabled pack %q and %q (set winner_overrides[%q] in extensions.json)", cid, otherID, m.ID, cid)
				}
			}
			for _, ob := range om.Components.ChannelBridges {
				bid := strings.TrimSpace(ob.ID)
				if myBridgeIDs[bid] && strings.TrimSpace(st.WinnerOverrides[bid]) == "" {
					return fmt.Errorf("unqualified channel_bridges ID collision on %q between enabled pack %q and %q (set winner_overrides[%q] in extensions.json)", bid, otherID, m.ID, bid)
				}
			}
		}

		rtMap := make(map[string]string)
		if m.Components.RoutingDefaults != nil {
			for k, v := range m.Components.RoutingDefaults.Tiers {
				if strings.TrimSpace(v.Model) != "" {
					rtMap[normalizePackRoutingKey(k)] = strings.TrimSpace(v.Model)
				}
			}
		}
		sourcePath := ""
		if trimmed := strings.TrimSpace(explicitPath); trimmed != "" {
			if abs, aErr := filepath.Abs(trimmed); aErr == nil && filepath.Clean(abs) != packDir {
				sourcePath = filepath.Clean(abs)
			}
		} else if existing, ok := st.Packs[m.ID]; ok {
			sourcePath = strings.TrimSpace(existing.SourcePath)
		}
		st.Packs[m.ID] = PackActivation{
			Enabled:        true,
			Version:        m.Version,
			Path:           packDir,
			SourcePath:     sourcePath,
			UsePackRouting: usePackRouting,
			RoutingTiers:   rtMap,
			UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
		}
		found := false
		for _, existing := range st.EnabledPacks {
			if existing == m.ID {
				found = true
				break
			}
		}
		if !found {
			st.EnabledPacks = append(st.EnabledPacks, m.ID)
		}
		for coreSkill, so := range m.Components.SkillOverrides {
			if NonOverridableCoreSkills[coreSkill] {
				continue
			}
			// Respect explicit Vault pin `"core"` if user opted out of this skill override
			if cur := strings.TrimSpace(st.SkillOverrides[coreSkill]); cur == "core" || cur == "core/"+coreSkill {
				continue
			}
			st.SkillOverrides[coreSkill] = m.ID + "/" + strings.TrimSpace(so.Skill)
		}
		return nil
	})
	return m, err
}

// RefreshPack re-resolves packID from explicitPath (or its persisted SourcePath / Path), validates and
// re-enables it in extensions.json (preserving UsePackRouting unless overridden), and stages a fresh
// SHA-256 trust proposal in one atomic workflow.
func RefreshPack(ctx context.Context, vaultDir, packID, explicitPath string, usePackRoutingOverride *bool, convID string) (PackManifest, approvals.Proposal, TrustBundle, error) {
	packID = strings.TrimSpace(packID)
	if packID == "" {
		return PackManifest{}, approvals.Proposal{}, TrustBundle{}, fmt.Errorf("pack ID is required")
	}
	targetPath := strings.TrimSpace(explicitPath)
	usePackRouting := false
	if st, err := LoadVaultState(vaultDir); err == nil {
		if act, ok := st.Packs[packID]; ok {
			usePackRouting = act.UsePackRouting
			if targetPath == "" {
				if strings.TrimSpace(act.SourcePath) != "" {
					targetPath = strings.TrimSpace(act.SourcePath)
				} else {
					targetPath = strings.TrimSpace(act.Path)
				}
			}
		}
	}
	if usePackRoutingOverride != nil {
		usePackRouting = *usePackRoutingOverride
	}
	m, err := EnablePack(ctx, vaultDir, packID, targetPath, usePackRouting)
	if err != nil {
		return PackManifest{}, approvals.Proposal{}, TrustBundle{}, err
	}
	prop, bundle, err := StageTrustProposal(vaultDir, m.ID, "", convID)
	if err != nil {
		return m, approvals.Proposal{}, TrustBundle{}, err
	}
	return m, prop, bundle, nil
}

// DisablePack disables packID in <vault>/extensions.json without deleting schedule cursors.
func DisablePack(ctx context.Context, vaultDir, packID string) error {
	packID = strings.TrimSpace(packID)
	return UpdateVaultState(ctx, vaultDir, func(st *VaultState) error {
		act, ok := st.Packs[packID]
		if ok {
			act.Enabled = false
			act.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			st.Packs[packID] = act
		}
		var kept []string
		for _, id := range st.EnabledPacks {
			if id != packID {
				kept = append(kept, id)
			}
		}
		st.EnabledPacks = kept
		for k, v := range st.SkillOverrides {
			if strings.HasPrefix(v, packID+"/") {
				delete(st.SkillOverrides, k)
			}
		}
		return nil
	})
}

// LoadActiveTrustedPacks returns enabled AND trust-verified PackManifests in deterministic priority order.
func LoadActiveTrustedPacks(vaultDir string) []DiscoveredPack {
	st, err := LoadVaultState(vaultDir)
	if err != nil {
		return nil
	}
	var out []DiscoveredPack
	for _, packID := range st.OrderedEnabledPacks() {
		act := st.Packs[packID]
		if !act.Enabled || strings.TrimSpace(act.Path) == "" {
			continue
		}
		idxFile := filepath.Join(act.Path, "hq-extension.json")
		if _, vErr := VerifyTrustedFile(vaultDir, "pack:"+packID, idxFile); vErr != nil {
			continue
		}
		m, mErr := LoadPackManifest(act.Path)
		if mErr != nil || m.ID != packID {
			continue
		}
		out = append(out, DiscoveredPack{
			ID:       m.ID,
			Version:  m.Version,
			Path:     act.Path,
			Enabled:  true,
			Trusted:  true,
			Manifest: m,
		})
	}
	return out
}

func validateOverlayScopes(patterns []string) error {
	if len(patterns) == 0 {
		return nil
	}
	hasPositive := false
	for _, raw := range patterns {
		sc := strings.TrimSpace(raw)
		if sc == "" {
			return fmt.Errorf("empty scope pattern")
		}
		if strings.HasPrefix(sc, "!") {
			if strings.TrimSpace(strings.TrimPrefix(sc, "!")) == "" {
				return fmt.Errorf("negated scope pattern %q is missing a target", raw)
			}
			continue
		}
		hasPositive = true
	}
	if !hasPositive {
		return fmt.Errorf("scope list must include at least one non-negated pattern (e.g. \"task:*\")")
	}
	return nil
}

// matchScopeWildcard matches a simple '*' wildcard pattern against val (crossing '/' if present).
func matchScopeWildcard(pattern, val string) bool {
	if pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == val
	}
	if !strings.HasPrefix(val, parts[0]) {
		return false
	}
	rest := val[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		if parts[i] == "" {
			continue
		}
		idx := strings.Index(rest, parts[i])
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(parts[i]):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}

// matchOverlayScope evaluates an overlay's scope list against roleScope ("front_desk" or "task:<scope>").
// Empty scope defaults to "front_desk". Supports exact matches, '*' wildcards (e.g. "task:*"), and
// '!' exclusions (e.g. "!task:repo-core"); any matching exclusion overrides positive matches.
func matchOverlayScope(patterns []string, roleScope string) bool {
	if len(patterns) == 0 {
		return roleScope == "front_desk"
	}
	matchedPositive := false
	for _, raw := range patterns {
		sc := strings.TrimSpace(raw)
		if sc == "" {
			continue
		}
		if strings.HasPrefix(sc, "!") {
			excl := strings.TrimSpace(strings.TrimPrefix(sc, "!"))
			if excl != "" && matchScopeWildcard(excl, roleScope) {
				return false
			}
			continue
		}
		if matchScopeWildcard(sc, roleScope) {
			matchedPositive = true
		}
	}
	return matchedPositive
}

// LoadActiveFrontDeskOverlays loads role-scoped markdown overlays from enabled & trusted companion packs,
// enforcing per-overlay (<= 4 KB) and aggregate (<= 16 KB) budgets.
func LoadActiveFrontDeskOverlays(vaultDir, roleScope string) []string {
	roleScope = strings.TrimSpace(roleScope)
	if roleScope == "" {
		return nil
	}
	packs := LoadActiveTrustedPacks(vaultDir)
	var rendered []string
	totalBytes := 0
	for _, dp := range packs {
		for _, ov := range dp.Manifest.Components.FrontDeskOverlays {
			if !matchOverlayScope(ov.Scope, roleScope) {
				continue
			}
			full := filepath.Join(dp.Path, filepath.FromSlash(ov.Path))
			if _, vErr := VerifyTrustedFile(vaultDir, "pack:"+dp.ID, full); vErr != nil {
				continue
			}
			capBytes := MaxOverlayBytes
			if ov.MaxBytes > 0 && ov.MaxBytes < capBytes {
				capBytes = ov.MaxBytes
			}
			raw, rErr := os.ReadFile(full)
			if rErr != nil || len(raw) == 0 || len(raw) > capBytes {
				continue
			}
			if totalBytes+len(raw) > MaxTotalOverlaysBytes {
				break
			}
			totalBytes += len(raw)
			rendered = append(rendered, fmt.Sprintf("<!-- overlay:%s/%s -->\n%s", dp.ID, ov.ID, strings.TrimSpace(string(raw))))
		}
	}
	return rendered
}

// PackDoctorItem reports health for a single pack or binary check (`hq extension doctor`).
type PackDoctorItem struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "PASS" | "WARN" | "FAIL"
	Detail string `json:"detail"`
}

type binProbeCacheKey struct {
	resolvedPath string
	size         int64
	modNano      int64
	joinedArgv   string
}

type binProbeCacheVal struct {
	status    string
	detail    string
	expiresAt time.Time
}

var (
	binProbeMu    sync.Mutex
	binProbeCache = make(map[binProbeCacheKey]binProbeCacheVal)
)

func probeRequiredPackBinary(packID, binName string, argv []string) PackDoctorItem {
	itemName := fmt.Sprintf("pack:%s:bin:%s", packID, binName)
	resolved := argv[0]
	if lp, err := exec.LookPath(argv[0]); err == nil && lp != "" {
		resolved = lp
	}
	var fSize, fMod int64 = -1, -1
	if st, err := os.Stat(resolved); err == nil {
		fSize = st.Size()
		fMod = st.ModTime().UnixNano()
	}
	key := binProbeCacheKey{
		resolvedPath: resolved,
		size:         fSize,
		modNano:      fMod,
		joinedArgv:   strings.Join(argv, "\x00"),
	}
	now := time.Now()
	binProbeMu.Lock()
	if cached, ok := binProbeCache[key]; ok && now.Before(cached.expiresAt) {
		binProbeMu.Unlock()
		return PackDoctorItem{Name: itemName, Status: cached.status, Detail: cached.detail}
	}
	binProbeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, pErr := cmd.CombinedOutput()
	cancel()

	res := PackDoctorItem{Name: itemName}
	ttl := 5 * time.Minute
	if pErr != nil {
		res.Status = "WARN"
		res.Detail = fmt.Sprintf("probe %v failed: %v", argv, pErr)
		ttl = 15 * time.Second
	} else {
		res.Status = "PASS"
		res.Detail = strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	}

	binProbeMu.Lock()
	binProbeCache[key] = binProbeCacheVal{
		status:    res.Status,
		detail:    res.Detail,
		expiresAt: time.Now().Add(ttl),
	}
	binProbeMu.Unlock()
	return res
}

// DoctorPacks checks trust integrity, binary prerequisites, and competing hooks across discovered packs.
func DoctorPacks(vaultDir string) []PackDoctorItem {
	packs, err := DiscoverPacks(vaultDir)
	if err != nil {
		return []PackDoctorItem{{Name: "extension.discover", Status: "FAIL", Detail: err.Error()}}
	}
	if len(packs) == 0 {
		return []PackDoctorItem{{Name: "extension.packs", Status: "PASS", Detail: "0 companion extension packs configured"}}
	}
	var items []PackDoctorItem
	for _, dp := range packs {
		warnings, lErr := LintPack(dp.Path)
		if lErr != nil {
			items = append(items, PackDoctorItem{
				Name:   "pack:" + dp.ID,
				Status: "FAIL",
				Detail: lErr.Error(),
			})
			continue
		}
		status := "PASS"
		detail := fmt.Sprintf("v%s enabled=%v trusted=%v (%s)", dp.Version, dp.Enabled, dp.Trusted, dp.Path)
		if dp.Enabled && !dp.Trusted {
			status = "WARN"
			detail += " — enabled in extensions.json but untrusted in private/trust.json (inactive until `hq extension trust " + dp.ID + "`)"
		}
		if len(warnings) > 0 && status == "PASS" {
			detail += " [" + strings.Join(warnings, "; ") + "]"
		}
		items = append(items, PackDoctorItem{Name: "pack:" + dp.ID, Status: status, Detail: detail})

		for _, b := range dp.Manifest.Requires.Bins {
			argv := b.ProbeArgv
			if len(argv) == 0 && b.Name != "" {
				argv = []string{b.Name, "--version"}
			}
			if len(argv) == 0 {
				continue
			}
			items = append(items, probeRequiredPackBinary(dp.ID, b.Name, argv))
		}
	}
	return items
}
