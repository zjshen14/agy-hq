package channels

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/store"
)

const (
	BridgeConfigSchemaVersion = 1

	PrivacyModeSanitizedDigest = "sanitized_digest"
	PrivacyModeZeroBody        = "zero_body"

	ConfigThreadModeGrouped  = "grouped"
	ConfigThreadModeDeskOnly = "desk_only"

	DefaultMaxNonUrgentPerHour = 6
	DefaultMaxBodyRunes        = 1600
	HardMaxBodyRunes           = 4000
)

var (
	reValidSlugSegment = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	forbiddenDestKeys  = map[string]bool{
		"command": true,
		"argv":    true,
		"path":    true,
		"env":     true,
		"binary":  true,
		"script":  true,
	}
)

// BridgePolicyConfig defines per-bridge notification and quiet-hours behavior in private/channels/<safe-id>.json.
type BridgePolicyConfig struct {
	DeliverInterrupts   *bool  `json:"deliver_interrupts,omitempty"`
	DeliverTaskDone     *bool  `json:"deliver_task_done,omitempty"`
	DeliverTaskBlocked  *bool  `json:"deliver_task_blocked,omitempty"`
	MirrorAllDeskTurns  *bool  `json:"mirror_all_desk_turns,omitempty"`
	RespectQuietHours   *bool  `json:"respect_quiet_hours,omitempty"`
	AllowCriticalBreach *bool  `json:"allow_critical_breach,omitempty"`
	MaxNonUrgentPerHour *int   `json:"max_non_urgent_per_hour,omitempty"`
	MaxBodyRunes        *int   `json:"max_body_runes,omitempty"`
	PrivacyMode         string `json:"privacy_mode,omitempty"`
}

// ResolvedPolicy holds concrete defaults after merging BridgePolicyConfig.
type ResolvedPolicy struct {
	DeliverInterrupts   bool
	DeliverTaskDone     bool
	DeliverTaskBlocked  bool
	MirrorAllDeskTurns  bool
	RespectQuietHours   bool
	AllowCriticalBreach bool
	MaxNonUrgentPerHour int
	MaxBodyRunes        int
	PrivacyMode         string
}

// BridgePrivateConfig models <vault>/private/channels/<safe-id>.json (0600, LLM-invisible).
type BridgePrivateConfig struct {
	SchemaVersion  int                    `json:"schema_version"`
	BridgeID       string                 `json:"bridge_id"`
	Enabled        *bool                  `json:"enabled,omitempty"`
	CredentialFile string                 `json:"credential_file,omitempty"`
	Destination    map[string]interface{} `json:"destination"`
	Policy         BridgePolicyConfig     `json:"policy,omitempty"`
}

// LoadedPrivateConfig bundles a parsed BridgePrivateConfig with deterministic digests for pause recovery & redacted UIs.
type LoadedPrivateConfig struct {
	Config          BridgePrivateConfig
	ResolvedPolicy  ResolvedPolicy
	ThreadMode      string
	DestSHA256Short string
	ConfigSHA256    string
	CredFingerprint string
	FileMode        os.FileMode
}

// ResolvedBridge joins an extension pack's ChannelBridgeSpec with Vault activation, trust, private config, and runtime state.
type ResolvedBridge struct {
	FQID               string
	PackID             string
	BridgeID           string
	PackDir            string
	PackManifestSHA256 string
	PackEnabled        bool
	PackTrusted        bool
	BinaryTrusted      bool
	OverrideOn         bool
	Spec               extension.ChannelBridgeSpec
	HasPrivateCfg      bool
	PrivateCfg         LoadedPrivateConfig
	PrivateCfgErr      error
}

// EffectiveEnabled returns true only when pack, Vault override, trust pins, and private config all enable dispatch.
func (rb ResolvedBridge) EffectiveEnabled() bool {
	if !rb.PackEnabled || !rb.PackTrusted || !rb.BinaryTrusted || !rb.OverrideOn || !rb.HasPrivateCfg || rb.PrivateCfgErr != nil {
		return false
	}
	if rb.PrivateCfg.Config.Enabled != nil && !*rb.PrivateCfg.Config.Enabled {
		return false
	}
	return true
}

// BridgeSummary exposes strictly redacted channel bridge health for `hq channel list`, `hq status`, and `/api/state`.
// Zero raw room IDs, webhook URLs, or credentials are ever included.
type BridgeSummary struct {
	BridgeID        string `json:"bridgeId"`
	PackID          string `json:"packId"`
	Enabled         bool   `json:"enabled"`
	Trusted         bool   `json:"trusted"`
	Configured      bool   `json:"configured"`
	HealthStatus    string `json:"healthStatus"`
	DestSHA256Short string `json:"destSha256Short,omitempty"`
	LastDeskCID     string `json:"lastDeskCid,omitempty"`
	QueuedCount     int    `json:"queuedCount"`
	LastSuccessAt   string `json:"lastSuccessAt,omitempty"`
	LastError       string `json:"lastError,omitempty"`
}

// ParseBridgeFQID validates and splits "<pack>/<id>".
func ParseBridgeFQID(fqid string) (string, string, error) {
	trimmed := strings.TrimSpace(fqid)
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid channel bridge ID %q (expected <pack>/<id>)", fqid)
	}
	packID := strings.TrimSpace(parts[0])
	bridgeID := strings.TrimSpace(parts[1])
	if packID == "core" || packID == "vault" || !reValidSlugSegment.MatchString(packID) || !reValidSlugSegment.MatchString(bridgeID) {
		return "", "", fmt.Errorf("invalid channel bridge ID %q (expected lowercase kebab-case <pack>/<id>)", fqid)
	}
	return packID, bridgeID, nil
}

// SafeBridgeFileStem converts validated "<pack>/<id>" to injective "<pack>__<id>"
// (`reValidSlugSegment` forbids '_', preventing any collision when IDs contain '--').
func SafeBridgeFileStem(fqid string) (string, error) {
	packID, bridgeID, err := ParseBridgeFQID(fqid)
	if err != nil {
		return "", err
	}
	return packID + "__" + bridgeID, nil
}

func privateChannelsDir(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "channels")
}

func privateConfigPath(vaultDir, fqid string) (string, error) {
	stem, err := SafeBridgeFileStem(fqid)
	if err != nil {
		return "", err
	}
	return filepath.Join(privateChannelsDir(vaultDir), stem+".json"), nil
}

// ResolvePolicy applies RFC-0023 defaults (action-required out of the box: deliver_task_done=false).
func ResolvePolicy(pol BridgePolicyConfig, specMaxRunes int) (ResolvedPolicy, error) {
	boolOr := func(p *bool, def bool) bool {
		if p == nil {
			return def
		}
		return *p
	}
	maxNonUrg := DefaultMaxNonUrgentPerHour
	if pol.MaxNonUrgentPerHour != nil {
		if *pol.MaxNonUrgentPerHour < 0 || *pol.MaxNonUrgentPerHour > 120 {
			return ResolvedPolicy{}, fmt.Errorf("policy.max_non_urgent_per_hour=%d out of range [0, 120]", *pol.MaxNonUrgentPerHour)
		}
		maxNonUrg = *pol.MaxNonUrgentPerHour
	}
	maxRunes := DefaultMaxBodyRunes
	if pol.MaxBodyRunes != nil {
		if *pol.MaxBodyRunes < 80 || *pol.MaxBodyRunes > HardMaxBodyRunes {
			return ResolvedPolicy{}, fmt.Errorf("policy.max_body_runes=%d out of range [80, %d]", *pol.MaxBodyRunes, HardMaxBodyRunes)
		}
		maxRunes = *pol.MaxBodyRunes
	}
	if specMaxRunes > 0 {
		if specMaxRunes < 80 {
			specMaxRunes = 80
		}
		if specMaxRunes < maxRunes {
			maxRunes = specMaxRunes
		}
	}
	if maxRunes > HardMaxBodyRunes {
		maxRunes = HardMaxBodyRunes
	}
	priv := strings.TrimSpace(pol.PrivacyMode)
	if priv == "" {
		priv = PrivacyModeSanitizedDigest
	}
	if priv != PrivacyModeSanitizedDigest && priv != PrivacyModeZeroBody {
		return ResolvedPolicy{}, fmt.Errorf("policy.privacy_mode %q must be %q or %q", priv, PrivacyModeSanitizedDigest, PrivacyModeZeroBody)
	}
	return ResolvedPolicy{
		DeliverInterrupts:   boolOr(pol.DeliverInterrupts, true),
		DeliverTaskDone:     boolOr(pol.DeliverTaskDone, false),
		DeliverTaskBlocked:  boolOr(pol.DeliverTaskBlocked, true),
		MirrorAllDeskTurns:  boolOr(pol.MirrorAllDeskTurns, false),
		RespectQuietHours:   boolOr(pol.RespectQuietHours, true),
		AllowCriticalBreach: boolOr(pol.AllowCriticalBreach, true),
		MaxNonUrgentPerHour: maxNonUrg,
		MaxBodyRunes:        maxRunes,
		PrivacyMode:         priv,
	}, nil
}

func validateDestinationObject(dest map[string]interface{}) (string, string, error) {
	if len(dest) == 0 {
		return "", "", fmt.Errorf("destination object must be non-empty")
	}
	threadMode := ConfigThreadModeGrouped
	for k, v := range dest {
		lk := strings.ToLower(strings.TrimSpace(k))
		if forbiddenDestKeys[lk] {
			return "", "", fmt.Errorf("destination contains forbidden executable key %q", k)
		}
		if lk == "thread_mode" {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				tm := strings.ToLower(strings.TrimSpace(s))
				if tm != ConfigThreadModeGrouped && tm != ConfigThreadModeDeskOnly {
					return "", "", fmt.Errorf("destination.thread_mode %q must be %q or %q", s, ConfigThreadModeGrouped, ConfigThreadModeDeskOnly)
				}
				threadMode = tm
			}
		}
	}
	canonDest, err := json.Marshal(dest)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(canonDest)
	return threadMode, hex.EncodeToString(sum[:])[:12], nil
}

// ValidatePrivateConfigBytes strictly validates raw JSON for <vault>/private/channels/<safe-id>.json.
func ValidatePrivateConfigBytes(expectedFQID string, raw []byte, specMaxRunes int) (LoadedPrivateConfig, error) {
	if _, _, err := ParseBridgeFQID(expectedFQID); err != nil {
		return LoadedPrivateConfig{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var cfg BridgePrivateConfig
	if err := dec.Decode(&cfg); err != nil {
		return LoadedPrivateConfig{}, fmt.Errorf("parse private channel config for %s: %w", expectedFQID, err)
	}
	if cfg.SchemaVersion != BridgeConfigSchemaVersion {
		return LoadedPrivateConfig{}, fmt.Errorf("unsupported schema_version %d in channel config for %s (expected %d)", cfg.SchemaVersion, expectedFQID, BridgeConfigSchemaVersion)
	}
	if strings.TrimSpace(cfg.BridgeID) != strings.TrimSpace(expectedFQID) {
		return LoadedPrivateConfig{}, fmt.Errorf("bridge_id mismatch in channel config (%q != %q)", cfg.BridgeID, expectedFQID)
	}
	if cred := strings.TrimSpace(cfg.CredentialFile); cred != "" {
		cleanRel := filepath.ToSlash(filepath.Clean(cred))
		if filepath.IsAbs(cred) || cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, "../") || !strings.HasPrefix(cleanRel, "private/") {
			return LoadedPrivateConfig{}, fmt.Errorf("credential_file %q must reside under private/", cred)
		}
	}
	threadMode, destShort, err := validateDestinationObject(cfg.Destination)
	if err != nil {
		return LoadedPrivateConfig{}, err
	}
	resPol, err := ResolvePolicy(cfg.Policy, specMaxRunes)
	if err != nil {
		return LoadedPrivateConfig{}, err
	}
	cfgSum := sha256.Sum256(bytes.TrimSpace(raw))
	return LoadedPrivateConfig{
		Config:          cfg,
		ResolvedPolicy:  resPol,
		ThreadMode:      threadMode,
		DestSHA256Short: destShort,
		ConfigSHA256:    hex.EncodeToString(cfgSum[:]),
	}, nil
}

func credentialFileFingerprint(vaultDir, credRel string) string {
	credRel = strings.TrimSpace(credRel)
	if credRel == "" {
		return ""
	}
	cleanRel := filepath.ToSlash(filepath.Clean(credRel))
	if filepath.IsAbs(credRel) || !strings.HasPrefix(cleanRel, "private/") {
		return "invalid"
	}
	full := filepath.Join(vaultDir, filepath.FromSlash(cleanRel))
	st, err := os.Lstat(full)
	if err != nil {
		return "missing"
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return "irregular"
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())))
	return hex.EncodeToString(sum[:8])
}

// LoadBridgePrivateConfig safely reads <vault>/private/channels/<safe-id>.json using O_NOFOLLOW.
func LoadBridgePrivateConfig(vaultDir, fqid string, specMaxRunes int) (LoadedPrivateConfig, bool, error) {
	cfgPath, err := privateConfigPath(vaultDir, fqid)
	if err != nil {
		return LoadedPrivateConfig{}, false, err
	}
	st, lErr := os.Lstat(cfgPath)
	if lErr != nil {
		if os.IsNotExist(lErr) {
			return LoadedPrivateConfig{}, false, nil
		}
		return LoadedPrivateConfig{}, false, lErr
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return LoadedPrivateConfig{}, true, fmt.Errorf("refusing non-regular or symlinked channel config %s", filepath.Base(cfgPath))
	}
	f, err := store.OpenReadNoFollow(cfgPath)
	if err != nil {
		return LoadedPrivateConfig{}, true, err
	}
	raw, rErr := io.ReadAll(io.LimitReader(f, 128*1024))
	_ = f.Close()
	if rErr != nil {
		return LoadedPrivateConfig{}, true, rErr
	}
	loaded, vErr := ValidatePrivateConfigBytes(fqid, raw, specMaxRunes)
	if vErr != nil {
		return LoadedPrivateConfig{}, true, vErr
	}
	loaded.FileMode = st.Mode().Perm()
	loaded.CredFingerprint = credentialFileFingerprint(vaultDir, loaded.Config.CredentialFile)
	return loaded, true, nil
}

// SaveBridgePrivateConfig validates and atomically writes <vault>/private/channels/<safe-id>.json with mode 0600.
func SaveBridgePrivateConfig(vaultDir, fqid string, raw []byte) (LoadedPrivateConfig, error) {
	cfgPath, err := privateConfigPath(vaultDir, fqid)
	if err != nil {
		return LoadedPrivateConfig{}, err
	}
	dir := filepath.Dir(cfgPath)
	if st, lErr := os.Lstat(dir); lErr == nil && st.Mode()&os.ModeSymlink != 0 {
		return LoadedPrivateConfig{}, fmt.Errorf("refusing symlinked private/channels directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return LoadedPrivateConfig{}, err
	}
	loaded, err := ValidatePrivateConfigBytes(fqid, raw, 0)
	if err != nil {
		return LoadedPrivateConfig{}, err
	}
	pretty, err := json.MarshalIndent(loaded.Config, "", "  ")
	if err != nil {
		return LoadedPrivateConfig{}, err
	}
	if err := store.WriteAtomic(cfgPath, append(pretty, '\n'), 0o600); err != nil {
		return LoadedPrivateConfig{}, err
	}
	loaded.FileMode = 0o600
	loaded.CredFingerprint = credentialFileFingerprint(vaultDir, loaded.Config.CredentialFile)
	return loaded, nil
}

// DiscoverBridges enumerates all channel_bridges across companion extension packs.
func DiscoverBridges(vaultDir string) ([]ResolvedBridge, error) {
	packs, err := extension.DiscoverPacks(vaultDir)
	if err != nil {
		return nil, err
	}
	vaultState, _ := extension.LoadVaultState(vaultDir)
	var out []ResolvedBridge
	for _, dp := range packs {
		packManifestSHA, _ := extension.ComputeStatCachedSHA256(filepath.Join(dp.Path, "hq-extension.json"))
		for _, spec := range dp.Manifest.Components.ChannelBridges {
			fqid := dp.ID + "/" + strings.TrimSpace(spec.ID)
			binTrusted := false
			if dp.Trusted && len(spec.Command) > 0 {
				if _, bErr := extension.VerifyTrustedBinaryReadOnly(vaultDir, "pack:"+dp.ID, spec.Command[0]); bErr == nil {
					binTrusted = true
				}
			}
			overrideOn := vaultState.IsChannelBridgeEnabled(fqid, true)
			if winner := strings.TrimSpace(vaultState.WinnerOverrides[strings.TrimSpace(spec.ID)]); winner != "" && winner != dp.ID {
				overrideOn = false
			}
			loadedCfg, hasCfg, cfgErr := LoadBridgePrivateConfig(vaultDir, fqid, spec.MaxBodyRunes)
			out = append(out, ResolvedBridge{
				FQID:               fqid,
				PackID:             dp.ID,
				BridgeID:           strings.TrimSpace(spec.ID),
				PackDir:            dp.Path,
				PackManifestSHA256: packManifestSHA,
				PackEnabled:        dp.Enabled,
				PackTrusted:        dp.Trusted,
				BinaryTrusted:      binTrusted,
				OverrideOn:         overrideOn,
				Spec:               spec,
				HasPrivateCfg:      hasCfg,
				PrivateCfg:         loadedCfg,
				PrivateCfgErr:      cfgErr,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FQID < out[j].FQID })
	return out, nil
}
