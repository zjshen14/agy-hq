package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// PackActivation records an activated companion extension pack in <vault>/extensions.json.
type PackActivation struct {
	Enabled        bool              `json:"enabled"`
	Version        string            `json:"version,omitempty"`
	Path           string            `json:"path,omitempty"`
	UsePackRouting bool              `json:"use_pack_routing,omitempty"`
	RoutingTiers   map[string]string `json:"routing_tiers,omitempty"`
	UpdatedAt      string            `json:"updated_at,omitempty"`
}

// VaultState represents the locked JSON control plane at <vault>/extensions.json.
type VaultState struct {
	SchemaVersion      int                          `json:"schema_version"`
	Packs              map[string]PackActivation    `json:"packs,omitempty"`
	SkillOverrides     map[string]string            `json:"skill_overrides,omitempty"` // coreSkill -> "<pack>/<skill>" or "core"
	ScheduleOverrides  map[string]ScheduleOverride  `json:"schedule_overrides,omitempty"`
	ConnectorOverrides map[string]ConnectorOverride `json:"connector_overrides,omitempty"`
	UpdatedAt          string                       `json:"updated_at,omitempty"`
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
		SchemaVersion:      VaultExtensionsSchemaVersion,
		Packs:              make(map[string]PackActivation),
		SkillOverrides:     make(map[string]string),
		ScheduleOverrides:  make(map[string]ScheduleOverride),
		ConnectorOverrides: make(map[string]ConnectorOverride),
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

// NonOverridableCoreSkills lists security-boundary skills that extension packs can never shadow.
var NonOverridableCoreSkills = map[string]bool{
	"inbox-triage": true,
}

// DispatchResolution reports the active target for a logical core skill (`hq dispatch resolve <skill>`).
type DispatchResolution struct {
	LogicalSkill string `json:"logical_skill"`
	ActiveTarget string `json:"active_target"` // "core/<skill>" or "<pack>/<skill>"
	Overridden   bool   `json:"overridden"`
	SkillPath    string `json:"skill_path,omitempty"`
}

// ResolveSkillDispatch resolves whether logicalSkill should execute its core SKILL.md or an enabled pack override.
func ResolveSkillDispatch(vaultDir, logicalSkill string) DispatchResolution {
	skill := strings.TrimSpace(strings.TrimPrefix(logicalSkill, "/"))
	res := DispatchResolution{
		LogicalSkill: skill,
		ActiveTarget: "core/" + skill,
		Overridden:   false,
	}
	if skill == "" || NonOverridableCoreSkills[skill] {
		return res
	}
	st, err := LoadVaultState(vaultDir)
	if err != nil {
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
	packAct, ok := st.Packs[parts[0]]
	if !ok || !packAct.Enabled {
		return res
	}
	if packAct.Path != "" {
		cand := filepath.Join(packAct.Path, "skills", parts[1], "SKILL.md")
		if info, sErr := os.Stat(cand); sErr == nil && !info.IsDir() {
			res.SkillPath = cand
		} else {
			// Fail closed back to core if override target SKILL.md is missing on disk.
			return res
		}
	}
	res.ActiveTarget = target
	res.Overridden = true
	return res
}
