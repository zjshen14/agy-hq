package workspace

import (
	"crypto/subtle"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zjshen14/agy-hq/internal/extension"
)

var (
	builtinMu    sync.RWMutex
	builtinByID  = make(map[string]Provider)
	builtinOrder []string
)

// RegisterBuiltin registers a built-in provider (normally from an init function). Built-in FQIDs must start with
// "core/", a namespace companion packs can never claim (pack id "core" is reserved). Registering the same FQID
// again replaces the earlier provider.
func RegisterBuiltin(p Provider) {
	if p == nil {
		panic("workspace: RegisterBuiltin called with a nil provider")
	}
	id := strings.TrimSpace(p.ID())
	if !strings.HasPrefix(id, "core/") || len(id) <= len("core/") || strings.Contains(id, ":") {
		panic(fmt.Sprintf("workspace: built-in provider FQID %q must look like \"core/<name>\"", id))
	}
	builtinMu.Lock()
	defer builtinMu.Unlock()
	if _, exists := builtinByID[id]; !exists {
		builtinOrder = append(builtinOrder, id)
	}
	builtinByID[id] = p
}

func registeredBuiltins() []Provider {
	builtinMu.RLock()
	defer builtinMu.RUnlock()
	out := make([]Provider, 0, len(builtinOrder))
	for _, id := range builtinOrder {
		out = append(out, builtinByID[id])
	}
	return out
}

func bindVault(p Provider, vaultDir string) Provider {
	if vs, ok := p.(VaultScoped); ok {
		if bound := vs.ForVault(vaultDir); bound != nil {
			return bound
		}
	}
	return p
}

// ProviderStatus summarizes one registered provider for `hq workspace providers`, the console, and doctor.
type ProviderStatus struct {
	FQID                 string       `json:"fqid"`
	PackID               string       `json:"pack_id,omitempty"`
	Builtin              bool         `json:"builtin"`
	Trusted              bool         `json:"trusted"`
	Reason               string       `json:"reason,omitempty"` // why an untrusted provider is disabled
	Capabilities         Capabilities `json:"capabilities"`
	RootPatterns         []string     `json:"root_patterns,omitempty"`
	StatusRefreshSeconds int          `json:"status_refresh_seconds,omitempty"`
	Skill                string       `json:"skill,omitempty"`
	Description          string       `json:"description,omitempty"`
}

type externalProviderEntry struct {
	pack extension.DiscoveredPack
	spec extension.WorkspaceProviderSpec
	fqid string
}

// loadExternalProviders returns every workspace_providers entry of the enabled, manifest-trusted packs in
// deterministic pack priority order. It reads manifests only and never spawns a subprocess.
func loadExternalProviders(vaultDir string) []externalProviderEntry {
	if strings.TrimSpace(vaultDir) == "" {
		return nil
	}
	var out []externalProviderEntry
	seen := make(map[string]bool)
	for _, dp := range extension.LoadActiveTrustedPacks(vaultDir) {
		for _, spec := range dp.Manifest.Components.WorkspaceProviders {
			fqid := dp.ID + "/" + strings.TrimSpace(spec.ID)
			if seen[fqid] {
				continue
			}
			seen[fqid] = true
			out = append(out, externalProviderEntry{pack: dp, spec: spec, fqid: fqid})
		}
	}
	return out
}

// externalTrust performs the cheap, read-only trust checks for one pack provider: manifest-entry validity, the
// SHA-256 pin of command[0] (stat-cached hash, no exec-cache staging), and the pinned skill guide. A failure
// disables only that provider.
func externalTrust(vaultDir string, reg extension.TrustRegistry, regErr error, ep externalProviderEntry) (bool, string) {
	if err := extension.ValidateWorkspaceProviderSpec(ep.pack.ID, ep.spec); err != nil {
		return false, "invalid manifest entry: " + err.Error()
	}
	if regErr != nil {
		return false, "trust registry unreadable: " + regErr.Error()
	}
	target, ok := reg.Targets["pack:"+ep.pack.ID]
	if !ok {
		return false, fmt.Sprintf("pack %s is not trusted (run `hq extension trust %s`)", ep.pack.ID, ep.pack.ID)
	}
	cmd0 := strings.TrimSpace(ep.spec.Command[0])
	pin, pinned := target.Binaries[cmd0]
	if !pinned {
		for _, cand := range target.Binaries {
			if cand.RawRef == cmd0 || cand.ResolvedPath == cmd0 {
				pin, pinned = cand, true
				break
			}
		}
	}
	if !pinned {
		return false, fmt.Sprintf("provider binary %q is not pinned (re-run `hq extension trust %s`)", cmd0, ep.pack.ID)
	}
	if pin.SelfHQ || strings.TrimSpace(pin.SHA256) == "" {
		return false, fmt.Sprintf("provider binary %q has no SHA-256 pin", cmd0)
	}
	sum, err := extension.ComputeStatCachedSHA256(pin.ResolvedPath)
	if err != nil || subtle.ConstantTimeCompare([]byte(sum), []byte(pin.SHA256)) != 1 {
		return false, fmt.Sprintf("provider binary %s changed since the trust ceremony (re-run `hq extension trust %s`)", pin.ResolvedPath, ep.pack.ID)
	}
	if skill := strings.TrimSpace(ep.spec.Skill); skill != "" {
		skillPath := filepath.Join(ep.pack.Path, "skills", skill, "SKILL.md")
		if _, err := extension.VerifyTrustedFileReadOnly(vaultDir, "pack:"+ep.pack.ID, skillPath); err != nil {
			return false, "skill guide failed trust verification: " + err.Error()
		}
	}
	return true, ""
}

// ListProviders returns the built-in providers followed by every workspace_providers entry of the enabled and
// manifest-trusted packs (FQID "<pack>/<id>"), each with its trust verdict. It never spawns a subprocess.
func ListProviders(vaultDir string) []ProviderStatus {
	var out []ProviderStatus
	for _, raw := range registeredBuiltins() {
		p := bindVault(raw, vaultDir)
		st := ProviderStatus{
			FQID:         p.ID(),
			Builtin:      true,
			Trusted:      true,
			Capabilities: p.Capabilities(),
		}
		if d, ok := p.(Describer); ok {
			st.Description = sanitizeDisplayText(d.Description(), 200)
		}
		out = append(out, st)
	}
	externals := loadExternalProviders(vaultDir)
	if len(externals) == 0 {
		return out
	}
	reg, regErr := extension.LoadTrustRegistry(vaultDir)
	for _, ep := range externals {
		st := ProviderStatus{
			FQID:                 ep.fqid,
			PackID:               ep.pack.ID,
			Capabilities:         capabilitiesFromSpec(ep.spec),
			RootPatterns:         append([]string(nil), ep.spec.RootPatterns...),
			StatusRefreshSeconds: ep.spec.StatusRefreshSeconds,
			Skill:                strings.TrimSpace(ep.spec.Skill),
			Description:          sanitizeDisplayText(ep.spec.Description, 200),
		}
		st.Trusted, st.Reason = externalTrust(vaultDir, reg, regErr, ep)
		out = append(out, st)
	}
	return out
}

// Resolve returns the provider registered under fqid, bound to vaultDir. Pack providers are returned only when
// their manifest entry is valid and their binary pin verifies; otherwise a *ProviderError with CodeUnavailable
// explains why the provider is disabled. Unknown FQIDs yield CodeNotFound.
func Resolve(vaultDir, fqid string) (Provider, error) {
	fqid = strings.TrimSpace(fqid)
	if fqid == "" {
		return nil, &ProviderError{Code: CodeInvalid, Message: "provider FQID is required"}
	}
	builtinMu.RLock()
	bp, isBuiltin := builtinByID[fqid]
	builtinMu.RUnlock()
	if isBuiltin {
		return bindVault(bp, vaultDir), nil
	}
	packID, providerID, ok := strings.Cut(fqid, "/")
	if !ok || packID == "" || providerID == "" || strings.Contains(providerID, "/") || strings.Contains(fqid, ":") {
		return nil, &ProviderError{Code: CodeInvalid, Message: fmt.Sprintf("provider FQID %q must look like <pack>/<id>", fqid)}
	}
	if strings.TrimSpace(vaultDir) == "" {
		return nil, &ProviderError{Code: CodeInvalid, Message: "vaultDir is required to resolve pack providers"}
	}
	for _, ep := range loadExternalProviders(vaultDir) {
		if ep.fqid != fqid {
			continue
		}
		reg, regErr := extension.LoadTrustRegistry(vaultDir)
		if trusted, reason := externalTrust(vaultDir, reg, regErr, ep); !trusted {
			return nil, &ProviderError{Code: CodeUnavailable, Message: fmt.Sprintf("workspace provider %s is disabled: %s", fqid, reason)}
		}
		return NewExternalProvider(vaultDir, ep.pack.ID, ep.pack.Path, ep.spec), nil
	}
	return nil, &ProviderError{Code: CodeNotFound, Message: fmt.Sprintf("workspace provider %q is not registered (built-in or enabled and trusted pack)", fqid)}
}

// MatchRootPattern finds which pack provider's static root_patterns claim absPath, returning the provider FQID
// and the matched workspace root. Every ancestor of the canonical path (deepest first) is tested with
// filepath.Match against the root_patterns of the enabled, manifest-trusted packs, in pack priority order.
// Paths inside the Vault (including <vault>/worktrees/) and roots that would contain the Vault never match.
// It reads manifests only and never spawns a subprocess, so it is safe on PreToolUse / Stop / render paths.
//
// root_patterns come from the trust-verified manifest, so matching does not require the provider binary to be
// healthy: path ownership stays recognized for hygiene even while a tampered binary is disabled.
func MatchRootPattern(vaultDir, absPath string) (fqid string, root string, ok bool) {
	raw := strings.TrimSpace(absPath)
	if raw == "" || !filepath.IsAbs(raw) {
		return "", "", false
	}
	providers := loadExternalProviders(vaultDir)
	if len(providers) == 0 {
		return "", "", false
	}
	var vaultRoots []string
	for _, v := range []string{cleanAbsPath(vaultDir), CanonicalPath(vaultDir)} {
		if v != "" && (len(vaultRoots) == 0 || vaultRoots[0] != v) {
			vaultRoots = append(vaultRoots, v)
		}
	}
	clean := filepath.Clean(raw)
	candidates := []string{CanonicalPath(clean)}
	if clean != candidates[0] {
		candidates = append(candidates, clean)
	}
	for _, p := range candidates {
		for _, v := range vaultRoots {
			if pathWithin(p, v) {
				return "", "", false
			}
		}
	}
	for _, p := range candidates {
		for anc := p; ; {
			for _, ep := range providers {
				for _, pat := range ep.spec.RootPatterns {
					if matched, err := filepath.Match(pat, anc); err != nil || !matched {
						continue
					}
					if rootContainsAny(anc, vaultRoots) {
						continue
					}
					return ep.fqid, anc, true
				}
			}
			parent := filepath.Dir(anc)
			if parent == anc {
				break
			}
			anc = parent
		}
	}
	return "", "", false
}

func rootContainsAny(root string, dirs []string) bool {
	for _, d := range dirs {
		if pathWithin(d, root) {
			return true
		}
	}
	return false
}
