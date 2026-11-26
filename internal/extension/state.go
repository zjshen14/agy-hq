package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/store"
)

const VaultExtensionsSchemaVersion = 1

// ScheduleOverride persists runtime pause/resume, cron, tier, or catch_up overrides in <vault>/extensions.json.
type ScheduleOverride struct {
	Enabled      *bool  `json:"enabled,omitempty"`
	Cron         string `json:"cron,omitempty"`
	Tier         string `json:"tier,omitempty"`
	CatchUp      string `json:"catch_up,omitempty"` // "none" (default) | "last"
	PausedAt     string `json:"paused_at,omitempty"`
	PausedReason string `json:"paused_reason,omitempty"`
	ResumedAt    string `json:"resumed_at,omitempty"`
}

// ConnectorOverride records Vault-level activation or loosening overrides for a connector.
type ConnectorOverride struct {
	Enabled       *bool    `json:"enabled,omitempty"`
	LoosenedFlags []string `json:"loosened_flags,omitempty"`
	Justification string   `json:"justification,omitempty"`
	UpdatedAt     string   `json:"updated_at,omitempty"`
}

// SecurityLooseningRule records a Vault-level opt-in relaxation of a monotonic security boolean
// (must also be pinned in <vault>/private/trust.json against the exact component SHA-256).
type SecurityLooseningRule struct {
	Target        string `json:"target"`
	Value         bool   `json:"value"`
	Justification string `json:"justification"`
}

// PackActivation records an activated companion extension pack in <vault>/extensions.json.
type PackActivation struct {
	Enabled        bool              `json:"enabled"`
	Version        string            `json:"version,omitempty"`
	Path           string            `json:"path,omitempty"`
	SourcePath     string            `json:"source_path,omitempty"`
	UsePackRouting bool              `json:"use_pack_routing,omitempty"`
	RoutingTiers   map[string]string `json:"routing_tiers,omitempty"`
	UpdatedAt      string            `json:"updated_at,omitempty"`
}

// ChannelBridgeOverride records Vault-level pause/resume overrides for a companion channel bridge.
type ChannelBridgeOverride struct {
	Enabled      *bool  `json:"enabled,omitempty"`
	PausedAt     string `json:"paused_at,omitempty"`
	PausedReason string `json:"paused_reason,omitempty"`
	ResumedAt    string `json:"resumed_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// VaultState represents the locked JSON control plane at <vault>/extensions.json.
type VaultState struct {
	SchemaVersion          int                              `json:"schema_version"`
	EnabledPacks           []string                         `json:"enabled_packs,omitempty"`
	Packs                  map[string]PackActivation        `json:"packs,omitempty"`
	SkillOverrides         map[string]string                `json:"skill_overrides,omitempty"` // coreSkill -> "<pack>/<skill>" or "core"
	ScheduleOverrides      map[string]ScheduleOverride      `json:"schedule_overrides,omitempty"`
	ConnectorOverrides     map[string]ConnectorOverride     `json:"connector_overrides,omitempty"`
	ChannelBridgeOverrides map[string]ChannelBridgeOverride `json:"channel_bridge_overrides,omitempty"`
	SecurityLoosening      []SecurityLooseningRule          `json:"security_loosening,omitempty"`
	WinnerOverrides        map[string]string                `json:"winner_overrides,omitempty"` // bare component id -> winning pack id
	UpdatedAt              string                           `json:"updated_at,omitempty"`
}

// OrderedEnabledPacks returns the deterministic ordered list of enabled pack IDs.
func (s VaultState) OrderedEnabledPacks() []string {
	seen := make(map[string]bool)
	var out []string
	for _, id := range s.EnabledPacks {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if act, ok := s.Packs[id]; ok && act.Enabled {
			seen[id] = true
			out = append(out, id)
		}
	}
	var extra []string
	for id, act := range s.Packs {
		if act.Enabled && !seen[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

func stateFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "extensions.json")
}

func stateLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", ".extensions.lock")
}

// NormalizeScheduleFQID maps bare schedule IDs ("morning-brief") to canonical "core/<id>",
// preserving already-namespaced "<pack>/<id>" FQIDs.
func NormalizeScheduleFQID(id string) string {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, "/") {
		return trimmed
	}
	return "core/" + trimmed
}

// BareCoreScheduleID strips a leading "core/" prefix while leaving "<pack>/<id>" intact.
func BareCoreScheduleID(fqid string) string {
	trimmed := strings.TrimSpace(fqid)
	if strings.HasPrefix(trimmed, "core/") {
		return strings.TrimPrefix(trimmed, "core/")
	}
	return trimmed
}

func emptyVaultState() VaultState {
	return VaultState{
		SchemaVersion:          VaultExtensionsSchemaVersion,
		Packs:                  make(map[string]PackActivation),
		SkillOverrides:         make(map[string]string),
		ScheduleOverrides:      make(map[string]ScheduleOverride),
		ConnectorOverrides:     make(map[string]ConnectorOverride),
		ChannelBridgeOverrides: make(map[string]ChannelBridgeOverride),
	}
}

func loadStateUnlocked(vaultDir string) (VaultState, error) {
	st := emptyVaultState()
	if strings.TrimSpace(vaultDir) == "" {
		return st, nil
	}
	p := stateFilePath(vaultDir)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return st, nil
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("parse %s: %w", p, err)
	}
	if st.SchemaVersion == 0 {
		st.SchemaVersion = VaultExtensionsSchemaVersion
	}
	if st.Packs == nil {
		st.Packs = make(map[string]PackActivation)
	}
	if st.SkillOverrides == nil {
		st.SkillOverrides = make(map[string]string)
	}
	if st.ScheduleOverrides == nil {
		st.ScheduleOverrides = make(map[string]ScheduleOverride)
	}
	if st.ConnectorOverrides == nil {
		st.ConnectorOverrides = make(map[string]ConnectorOverride)
	}
	if st.ChannelBridgeOverrides == nil {
		st.ChannelBridgeOverrides = make(map[string]ChannelBridgeOverride)
	}
	if st.WinnerOverrides == nil {
		st.WinnerOverrides = make(map[string]string)
	}
	return st, nil
}

// LoadVaultState reads <vault>/extensions.json safely (returning empty defaults if absent).
func LoadVaultState(vaultDir string) (VaultState, error) {
	return loadStateUnlocked(vaultDir)
}

// UpdateVaultState atomically mutates <vault>/extensions.json under RankExtensions lock.
func UpdateVaultState(ctx context.Context, vaultDir string, fn func(*VaultState) error) error {
	if strings.TrimSpace(vaultDir) == "" {
		return fmt.Errorf("vaultDir is required")
	}
	return store.With(ctx, store.RankExtensions, stateLockPath(vaultDir), func(_ context.Context) error {
		st, err := loadStateUnlocked(vaultDir)
		if err != nil {
			return err
		}
		if err := fn(&st); err != nil {
			return err
		}
		st.SchemaVersion = VaultExtensionsSchemaVersion
		st.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		out, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return err
		}
		return store.WriteAtomic(stateFilePath(vaultDir), append(out, '\n'), 0o600)
	})
}

// LookupScheduleOverride resolves a schedule override accepting either bare "<id>" or "core/<id>".
func (s VaultState) LookupScheduleOverride(id string) (ScheduleOverride, bool) {
	if len(s.ScheduleOverrides) == 0 {
		return ScheduleOverride{}, false
	}
	fqid := NormalizeScheduleFQID(id)
	if ov, ok := s.ScheduleOverrides[fqid]; ok {
		return ov, true
	}
	bare := BareCoreScheduleID(fqid)
	if ov, ok := s.ScheduleOverrides[bare]; ok {
		return ov, true
	}
	return ScheduleOverride{}, false
}

// IsScheduleEnabled returns whether schedule id is enabled given its declared default and Vault override.
func (s VaultState) IsScheduleEnabled(id string, declaredDefault bool) bool {
	if ov, ok := s.LookupScheduleOverride(id); ok && ov.Enabled != nil {
		return *ov.Enabled
	}
	return declaredDefault
}

// SetSchedulePaused sets enabled=false (or true on resume) for canonical FQID in <vault>/extensions.json.
func SetSchedulePaused(ctx context.Context, vaultDir, scheduleID string, paused bool, reason, catchUp string, now time.Time) (string, ScheduleOverride, error) {
	fqid := NormalizeScheduleFQID(scheduleID)
	if fqid == "" {
		return "", ScheduleOverride{}, fmt.Errorf("schedule id is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	var updated ScheduleOverride
	err := UpdateVaultState(ctx, vaultDir, func(st *VaultState) error {
		ov, _ := st.LookupScheduleOverride(fqid)
		// Remove legacy bare duplicate if present so canonical FQID is single source of truth
		delete(st.ScheduleOverrides, BareCoreScheduleID(fqid))
		enabled := !paused
		ov.Enabled = &enabled
		if catchUp != "" {
			if catchUp != "none" && catchUp != "last" {
				return fmt.Errorf("invalid catch_up %q (expected none or last)", catchUp)
			}
			ov.CatchUp = catchUp
		} else if ov.CatchUp == "" {
			ov.CatchUp = "none"
		}
		if paused {
			ov.PausedAt = now.UTC().Format(time.RFC3339)
			ov.PausedReason = strings.TrimSpace(reason)
		} else {
			ov.ResumedAt = now.UTC().Format(time.RFC3339)
			ov.PausedReason = ""
		}
		st.ScheduleOverrides[fqid] = ov
		updated = ov
		return nil
	})
	return fqid, updated, err
}

// LookupChannelBridgeOverride returns a Vault-level pause/resume override for bridgeFQID ("<pack>/<id>").
func (s VaultState) LookupChannelBridgeOverride(bridgeFQID string) (ChannelBridgeOverride, bool) {
	if len(s.ChannelBridgeOverrides) == 0 {
		return ChannelBridgeOverride{}, false
	}
	ov, ok := s.ChannelBridgeOverrides[strings.TrimSpace(bridgeFQID)]
	return ov, ok
}

// IsChannelBridgeEnabled returns whether bridgeFQID is enabled in extensions.json (defaulting to declaredDefault).
func (s VaultState) IsChannelBridgeEnabled(bridgeFQID string, declaredDefault bool) bool {
	if ov, ok := s.LookupChannelBridgeOverride(bridgeFQID); ok && ov.Enabled != nil {
		return *ov.Enabled
	}
	return declaredDefault
}

// SetChannelBridgePaused sets enabled=false (or true on resume) for bridgeFQID in <vault>/extensions.json.
func SetChannelBridgePaused(ctx context.Context, vaultDir, bridgeFQID string, paused bool, reason string, now time.Time) (ChannelBridgeOverride, error) {
	fqid := strings.TrimSpace(bridgeFQID)
	if fqid == "" || !strings.Contains(fqid, "/") {
		return ChannelBridgeOverride{}, fmt.Errorf("channel bridge FQID (<pack>/<id>) is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	nowStr := now.UTC().Format(time.RFC3339)
	var updated ChannelBridgeOverride
	err := UpdateVaultState(ctx, vaultDir, func(st *VaultState) error {
		ov, _ := st.LookupChannelBridgeOverride(fqid)
		enabled := !paused
		ov.Enabled = &enabled
		ov.UpdatedAt = nowStr
		if paused {
			ov.PausedAt = nowStr
			ov.PausedReason = strings.TrimSpace(reason)
		} else {
			ov.ResumedAt = nowStr
			ov.PausedReason = ""
		}
		st.ChannelBridgeOverrides[fqid] = ov
		updated = ov
		return nil
	})
	return updated, err
}

// NonOverridableCoreSkills lists security-boundary skills that extension packs
// and personal RSI overlays can never shadow or mutate.
var NonOverridableCoreSkills = map[string]bool{
	"inbox-triage":              true,
	"quarantined-digest-reader": true,
}

// DispatchResolution reports the active target for a logical core skill or schedule (`hq dispatch resolve`).
type DispatchResolution struct {
	FQID         string   `json:"fqid,omitempty"`
	LogicalSkill string   `json:"logical_skill,omitempty"`
	ActiveTarget string   `json:"active_target,omitempty"` // "core/<skill>", "vault/<skill>", or "<pack>/<skill>"
	Skill        string   `json:"skill,omitempty"`
	SkillOrigin  string   `json:"skill_origin,omitempty"` // "core", "vault", or "pack:<id>"
	Overridden   bool     `json:"overridden"`
	SkillPath    string   `json:"skill_path,omitempty"`
	HasOverlay   bool     `json:"has_overlay,omitempty"`
	OverlayPath  string   `json:"overlay_path,omitempty"`
	ActiveRules  int      `json:"active_rules,omitempty"`
	OverlayRules []string `json:"overlay_rules,omitempty"`
	Enabled      bool     `json:"enabled"`
	Tier         string   `json:"tier,omitempty"`
	Model        string   `json:"model,omitempty"`
	Trust        string   `json:"trust,omitempty"` // "ok" | "untrusted" | "builtin"
	Reason       string   `json:"reason,omitempty"`
}

// MaxOverlayRulesPerSkill caps the number of active RSI overlay rules injected for a single skill.
const MaxOverlayRulesPerSkill = 5

var (
	reRSIRuleComment = regexp.MustCompile(`<!--\s*rsi:rule\s+([^>]+)-->`)
	reRSIRuleKV      = regexp.MustCompile(`(\w+)="([^"]*)"`)
)

// ParsedRSIRule represents a parsed `<!-- rsi:rule ... -->` metadata block in a SKILL.md or overlay file.
type ParsedRSIRule struct {
	ID         string
	Source     string
	ModelScope string
	Added      string
	Helpful    int
	Harmful    int
	Origin     string
	SkillFile  string
}

// ParseRSIRules extracts all `<!-- rsi:rule ... -->` metadata headers from a skill or overlay document.
func ParseRSIRules(skillFile, content string) []ParsedRSIRule {
	matches := reRSIRuleComment.FindAllStringSubmatch(content, -1)
	var out []ParsedRSIRule
	for _, m := range matches {
		attrs := m[1]
		kvs := reRSIRuleKV.FindAllStringSubmatch(attrs, -1)
		r := ParsedRSIRule{SkillFile: skillFile}
		for _, kv := range kvs {
			switch kv[1] {
			case "id":
				r.ID = kv[2]
			case "source":
				r.Source = kv[2]
			case "model_scope":
				r.ModelScope = kv[2]
			case "added":
				r.Added = kv[2]
			case "helpful":
				r.Helpful, _ = strconv.Atoi(kv[2])
			case "harmful":
				r.Harmful, _ = strconv.Atoi(kv[2])
			case "origin":
				r.Origin = kv[2]
			}
		}
		if r.ID != "" {
			out = append(out, r)
		}
	}
	return out
}

// ExtractActiveRSIRuleBlocks extracts all non-disabled `<!-- rsi:rule ... -->` blocks from markdown content.
func ExtractActiveRSIRuleBlocks(content string) []string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		m := reRSIRuleComment.FindStringSubmatch(lines[i])
		if len(m) < 2 {
			continue
		}
		attrs := strings.ToLower(m[1])
		disabled := strings.Contains(attrs, `enabled="false"`) ||
			strings.Contains(attrs, `status="disabled"`) ||
			strings.Contains(attrs, `ablation="retired"`)

		block := []string{lines[i]}
		closedAt := -1
		nextStop := len(lines)
		for j := i + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if strings.HasPrefix(trimmed, "<!-- /rsi:rule") {
				block = append(block, lines[j])
				closedAt = j
				break
			}
			if trimmed == "" || strings.HasPrefix(trimmed, "<!-- rsi:rule") || strings.HasPrefix(trimmed, "## ") {
				nextStop = j
				break
			}
			block = append(block, lines[j])
		}
		if closedAt >= 0 {
			i = closedAt
		} else {
			i = nextStop - 1
		}
		if disabled {
			continue
		}
		if blk := strings.TrimSpace(strings.Join(block, "\n")); blk != "" {
			out = append(out, blk)
		}
	}
	return out
}

// ExtractRSIRuleBlock returns the full markdown block for a specific ruleID, if present.
func ExtractRSIRuleBlock(content, ruleID string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	marker := fmt.Sprintf(`<!-- rsi:rule id=%q`, ruleID)
	for i := 0; i < len(lines); i++ {
		if strings.Contains(lines[i], marker) {
			var block []string
			block = append(block, lines[i])
			for j := i + 1; j < len(lines); j++ {
				trimmed := strings.TrimSpace(lines[j])
				if trimmed == "" || strings.HasPrefix(trimmed, "<!-- rsi:rule") || strings.HasPrefix(trimmed, "## ") {
					break
				}
				block = append(block, lines[j])
			}
			return strings.Join(block, "\n"), true
		}
	}
	return "", false
}

// ResolveActivePackSkillOverrides resolves all trusted, enabled companion pack skill overrides
// in deterministic alphabetical order while reading extensions.json and private/trust.json at most once.
func ResolveActivePackSkillOverrides(vaultDir string) []DispatchResolution {
	st, err := LoadVaultState(vaultDir)
	if err != nil || len(st.SkillOverrides) == 0 {
		return nil
	}
	reg, regErr := LoadTrustRegistry(vaultDir)
	if regErr != nil {
		return nil
	}
	keys := make([]string, 0, len(st.SkillOverrides))
	for k := range st.SkillOverrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []DispatchResolution
	for _, k := range keys {
		res := resolvePackSkillOverride(vaultDir, st, reg, strings.TrimSpace(strings.TrimPrefix(k, "/")))
		if res.Overridden && res.Trust == "ok" && res.SkillPath != "" {
			out = append(out, res)
		}
	}
	return out
}

func resolvePackSkillOverride(vaultDir string, st VaultState, reg TrustRegistry, skill string) DispatchResolution {
	res := DispatchResolution{
		FQID:         "core/" + skill,
		LogicalSkill: skill,
		ActiveTarget: "core/" + skill,
		Skill:        skill,
		SkillOrigin:  "core",
		Enabled:      true,
		Trust:        "builtin",
	}
	if skill == "" || NonOverridableCoreSkills[skill] {
		if NonOverridableCoreSkills[skill] {
			res.Reason = "non_overridable_security_skill"
		}
		return res
	}
	target := strings.TrimSpace(st.SkillOverrides[skill])
	if target == "" || target == "core" || target == "core/"+skill {
		return res
	}
	parts := strings.SplitN(target, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return res
	}
	packID, targetSkill := parts[0], parts[1]
	packAct, ok := st.Packs[packID]
	if !ok || !packAct.Enabled || strings.TrimSpace(packAct.Path) == "" {
		return res
	}
	cand := filepath.Join(packAct.Path, "skills", targetSkill, "SKILL.md")
	if _, vErr := verifyTrustedFileWithRegistry(vaultDir, reg, "pack:"+packID, cand, false); vErr != nil {
		res.Trust = "untrusted"
		res.Reason = vErr.Error()
		return res
	}
	res.ActiveTarget = target
	res.Skill = targetSkill
	res.SkillOrigin = "pack:" + packID
	res.SkillPath = cand
	res.Overridden = true
	res.Trust = "ok"
	return res
}

// ResolveSkillDispatch resolves whether logicalSkill should execute its core SKILL.md, a Vault SKILL.md,
// or an enabled, trust-verified companion pack override (failing closed back to core if untrusted or tampered),
// and composes active Tier 1B RSI overlay rules from <vault>/memory/rsi/rules/<skill>.md.
func ResolveSkillDispatch(vaultDir, logicalSkill string) DispatchResolution {
	skill := strings.TrimSpace(strings.TrimPrefix(logicalSkill, "/"))
	res := DispatchResolution{
		FQID:         "core/" + skill,
		LogicalSkill: skill,
		ActiveTarget: "core/" + skill,
		Skill:        skill,
		SkillOrigin:  "core",
		Overridden:   false,
		Enabled:      true,
		Trust:        "builtin",
	}
	if skill == "" || NonOverridableCoreSkills[skill] {
		if NonOverridableCoreSkills[skill] {
			res.Reason = "non_overridable_security_skill"
		}
		return res
	}

	packResolved := false
	if st, err := LoadVaultState(vaultDir); err == nil {
		if target := strings.TrimSpace(st.SkillOverrides[skill]); target != "" && target != "core" && target != "core/"+skill {
			if reg, regErr := LoadTrustRegistry(vaultDir); regErr == nil {
				packRes := resolvePackSkillOverride(vaultDir, st, reg, skill)
				if packRes.Trust == "untrusted" {
					return packRes
				}
				if packRes.Overridden {
					res = packRes
					packResolved = true
				}
			}
		}
	}

	if !packResolved && strings.TrimSpace(vaultDir) != "" {
		vaultSkillPath := filepath.Join(vaultDir, "skills", skill, "SKILL.md")
		if b, rErr := os.ReadFile(vaultSkillPath); rErr == nil {
			res.FQID = "vault/" + skill
			res.ActiveTarget = "vault/" + skill
			res.SkillOrigin = "vault"
			res.SkillPath = vaultSkillPath
			res.Trust = "ok"
			res.ActiveRules += len(ExtractActiveRSIRuleBlocks(string(b)))
		}
	}

	if strings.TrimSpace(vaultDir) != "" {
		overlayPath := filepath.Join(vaultDir, "memory", "rsi", "rules", skill+".md")
		if b, oErr := os.ReadFile(overlayPath); oErr == nil {
			blocks := ExtractActiveRSIRuleBlocks(string(b))
			if len(blocks) > MaxOverlayRulesPerSkill {
				blocks = blocks[:MaxOverlayRulesPerSkill]
			}
			if len(blocks) > 0 {
				res.HasOverlay = true
				res.OverlayPath = overlayPath
				res.OverlayRules = blocks
				res.ActiveRules += len(blocks)
			}
		}
	}
	return res
}
