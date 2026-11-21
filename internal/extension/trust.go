package extension

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/internal/version"
)

const TrustSchemaVersion = 1

// PinnedBinary records a trusted external CLI binary hash (or hq self-identity).
type PinnedBinary struct {
	RawRef       string `json:"raw_ref"`
	ResolvedPath string `json:"resolved_path"`
	SHA256       string `json:"sha256,omitempty"`
	SelfHQ       bool   `json:"self_hq,omitempty"`
	HQVersion    string `json:"hq_version,omitempty"`
	HQCommit     string `json:"hq_commit,omitempty"`
}

// PinnedLoosening binds a Vault security_loosening justification to an exact component SHA-256.
type PinnedLoosening struct {
	Target          string `json:"target"`
	ComponentSHA256 string `json:"component_sha256"`
	Justification   string `json:"justification"`
	ApprovedAt      string `json:"approved_at"`
}

// TrustedTarget stores the pinned file/binary manifest for "pack:<id>" or "vault" in <vault>/private/trust.json.
type TrustedTarget struct {
	SourceID               string                     `json:"source_id"`
	CanonicalRoot          string                     `json:"canonical_root"`
	Version                string                     `json:"version,omitempty"`
	ManifestSHA256         string                     `json:"manifest_sha256,omitempty"`
	Files                  map[string]string          `json:"files,omitempty"` // relSlash -> sha256
	Binaries               map[string]PinnedBinary    `json:"binaries,omitempty"`
	Loosenings             map[string]PinnedLoosening `json:"loosenings,omitempty"`
	NativeSurfaces         []string                   `json:"native_surfaces,omitempty"`
	HasCompetingPreToolUse bool                       `json:"has_competing_pre_tool_use,omitempty"`
	ApprovedAt             string                     `json:"approved_at"`
}

// TrustRegistry persists all trusted companion packs and local vault extensions at <vault>/private/trust.json (0600).
type TrustRegistry struct {
	SchemaVersion int                      `json:"schema_version"`
	Targets       map[string]TrustedTarget `json:"targets"`
	UpdatedAt     string                   `json:"updated_at"`
}

// TrustBundle is serialized into <vault>/approvals/payloads/<propID>.json and piped via /dev/fd/3 on `hq approve`.
type TrustBundle struct {
	SchemaVersion int           `json:"schema_version"`
	Target        TrustedTarget `json:"target"`
	SummaryLines  []string      `json:"summary_lines,omitempty"`
}

type statHashEntry struct {
	size    int64
	modNano int64
	sha256  string
}

var (
	statHashCache sync.Map // string(absPath) -> statHashEntry
	noticeDedup   sync.Map // string(vaultDir+"|"+key) -> bool
)

func trustFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "trust.json")
}

func trustLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "private", ".trust.lock")
}

func canonicalDir(p string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(p))
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(real), nil
}

// ComputeStatCachedSHA256 computes the SHA-256 of a regular non-symlink file, caching by (size, mtimeNano).
func ComputeStatCachedSHA256(path string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return "", fmt.Errorf("refusing non-regular or symlinked file %s", abs)
	}
	if cached, ok := statHashCache.Load(abs); ok {
		ent := cached.(statHashEntry)
		if ent.size == st.Size() && ent.modNano == st.ModTime().UnixNano() && ent.sha256 != "" {
			return ent.sha256, nil
		}
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sumHex := hex.EncodeToString(h.Sum(nil))
	statHashCache.Store(abs, statHashEntry{
		size:    st.Size(),
		modNano: st.ModTime().UnixNano(),
		sha256:  sumHex,
	})
	return sumHex, nil
}

// IsSelfHQBinary reports whether rawBin designates the hq binary itself (pinned by version/commit rather than file hash).
func IsSelfHQBinary(rawBin string) bool {
	trimmed := strings.TrimSpace(rawBin)
	if trimmed == "hq" || trimmed == "./bin/hq" || trimmed == "bin/hq" {
		return true
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		c1, e1 := canonicalDir(trimmed)
		c2, e2 := canonicalDir(exe)
		if e1 == nil && e2 == nil && c1 == c2 {
			return true
		}
	}
	return false
}

// ResolveSelfHQExecutable returns the running hq binary path (falling back to PATH lookup during go test binaries).
func ResolveSelfHQExecutable() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		base := filepath.Base(exe)
		if base == "hq" || strings.HasPrefix(base, "hq.") {
			return exe
		}
	}
	if envRoot := strings.TrimSpace(os.Getenv("HQ_ROOT_DIR")); envRoot != "" {
		cand := filepath.Join(envRoot, "bin", "hq")
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	if lp, err := exec.LookPath("hq"); err == nil && lp != "" {
		return lp
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "hq"
}

// LoadTrustRegistry reads <vault>/private/trust.json safely (returning empty registry when absent).
func LoadTrustRegistry(vaultDir string) (TrustRegistry, error) {
	reg := TrustRegistry{
		SchemaVersion: TrustSchemaVersion,
		Targets:       make(map[string]TrustedTarget),
	}
	if strings.TrimSpace(vaultDir) == "" {
		return reg, nil
	}
	p := trustFilePath(vaultDir)
	st, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return reg, err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return reg, fmt.Errorf("refusing symlinked or non-regular trust registry %s", p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return reg, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return reg, nil
	}
	if err := json.Unmarshal(data, &reg); err != nil {
		return reg, fmt.Errorf("corrupt trust registry %s: %w", p, err)
	}
	if reg.Targets == nil {
		reg.Targets = make(map[string]TrustedTarget)
	}
	return reg, nil
}

func emitTrustMismatchNoticeOnce(vaultDir, sourceID, comp, detail string) {
	if strings.TrimSpace(vaultDir) == "" {
		return
	}
	dedupKey := vaultDir + "|" + sourceID + "|" + comp + "|" + detail
	if _, loaded := noticeDedup.LoadOrStore(dedupKey, true); loaded {
		return
	}
	_, _, _ = outbox.Add(vaultDir, outbox.OutboxItem{
		SourceSkill: "extension-trust",
		Scope:       sourceID,
		Tier:        outbox.TierBriefRollup,
		Title:       fmt.Sprintf("Extension trust verification failed (%s)", sourceID),
		Summary:     fmt.Sprintf("Component %s failed SHA-256 trust check (%s); re-run `hq extension trust` to inspect diff and approve.", comp, detail),
	}, time.Now())
}

// VerifyTrustedFile verifies that absPath belongs to sourceID ("vault" or "pack:<id>") and matches its SHA-256 pin in <vault>/private/trust.json.
func VerifyTrustedFile(vaultDir, sourceID, absPath string) (string, error) {
	return verifyTrustedFileInternal(vaultDir, sourceID, absPath, true)
}

// VerifyTrustedFileReadOnly verifies SHA-256 trust pins without mutating outbox state on mismatch (for 0-write peek/list/doctor paths).
func VerifyTrustedFileReadOnly(vaultDir, sourceID, absPath string) (string, error) {
	return verifyTrustedFileInternal(vaultDir, sourceID, absPath, false)
}

func verifyTrustedFileInternal(vaultDir, sourceID, absPath string, notifyOutbox bool) (string, error) {
	reg, err := LoadTrustRegistry(vaultDir)
	if err != nil {
		return "", err
	}
	return verifyTrustedFileWithRegistry(vaultDir, reg, sourceID, absPath, notifyOutbox)
}

func verifyTrustedFileWithRegistry(vaultDir string, reg TrustRegistry, sourceID, absPath string, notifyOutbox bool) (string, error) {
	target, ok := reg.Targets[sourceID]
	if !ok {
		if notifyOutbox {
			emitTrustMismatchNoticeOnce(vaultDir, sourceID, filepath.Base(absPath), "untrusted_target")
		}
		return "", fmt.Errorf("%s is not trusted in private/trust.json (run `hq extension trust`)", sourceID)
	}
	canonRoot, err := canonicalDir(target.CanonicalRoot)
	if err != nil {
		return "", fmt.Errorf("resolve canonical root for %s: %w", sourceID, err)
	}

	// For companion packs, verify hq-extension.json itself first; if modified, disable the entire pack.
	if strings.HasPrefix(sourceID, "pack:") && target.ManifestSHA256 != "" {
		idxPath := filepath.Join(canonRoot, "hq-extension.json")
		gotIdxSHA, mErr := ComputeStatCachedSHA256(idxPath)
		if mErr != nil || subtle.ConstantTimeCompare([]byte(gotIdxSHA), []byte(target.ManifestSHA256)) != 1 {
			if notifyOutbox {
				emitTrustMismatchNoticeOnce(vaultDir, sourceID, "hq-extension.json", gotIdxSHA)
			}
			return "", fmt.Errorf("%s manifest hq-extension.json modified since trust ceremony", sourceID)
		}
	}

	canonFile, err := canonicalDir(absPath)
	if err != nil {
		return "", fmt.Errorf("resolve file %s: %w", absPath, err)
	}
	rel, err := filepath.Rel(canonRoot, canonFile)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("file %s escapes trusted root %s", canonFile, canonRoot)
	}
	relSlash := filepath.ToSlash(rel)
	wantSHA, pinned := target.Files[relSlash]
	if !pinned || wantSHA == "" {
		if notifyOutbox {
			emitTrustMismatchNoticeOnce(vaultDir, sourceID, relSlash, "unpinned_component")
		}
		return "", fmt.Errorf("component %s in %s is not pinned in private/trust.json", relSlash, sourceID)
	}
	gotSHA, err := ComputeStatCachedSHA256(canonFile)
	if err != nil {
		return "", err
	}
	if len(gotSHA) != len(wantSHA) || subtle.ConstantTimeCompare([]byte(gotSHA), []byte(wantSHA)) != 1 {
		if notifyOutbox {
			emitTrustMismatchNoticeOnce(vaultDir, sourceID, relSlash, gotSHA[:12])
		}
		return "", fmt.Errorf("SHA-256 mismatch for %s (%s): got %s, pinned %s", sourceID, relSlash, gotSHA[:12], wantSHA[:12])
	}
	return gotSHA, nil
}

// VerifyTrustedBinary verifies an external CLI binary against <vault>/private/trust.json (exempting `hq` self-verbs).
func VerifyTrustedBinary(vaultDir, sourceID, rawBin string) (string, error) {
	return verifyTrustedBinaryInternal(vaultDir, sourceID, rawBin, true)
}

// VerifyTrustedBinaryReadOnly verifies an external CLI binary without mutating outbox state on mismatch.
func VerifyTrustedBinaryReadOnly(vaultDir, sourceID, rawBin string) (string, error) {
	return verifyTrustedBinaryInternal(vaultDir, sourceID, rawBin, false)
}

func stageVerifiedExecCache(vaultDir, resolvedPath, expectedSHA string) (string, error) {
	f, err := store.OpenReadNoFollow(resolvedPath)
	if err != nil {
		return "", err
	}
	const maxBinaryBytes = 256 * 1024 * 1024 // 256 MiB safety ceiling
	rawBytes, rErr := io.ReadAll(io.LimitReader(f, maxBinaryBytes+1))
	_ = f.Close()
	if rErr != nil {
		return "", rErr
	}
	if len(rawBytes) > maxBinaryBytes {
		return "", fmt.Errorf("binary %s exceeds 256 MiB ceiling", resolvedPath)
	}
	sum := sha256.Sum256(rawBytes)
	gotSHA := hex.EncodeToString(sum[:])
	if len(gotSHA) != len(expectedSHA) || subtle.ConstantTimeCompare([]byte(gotSHA), []byte(expectedSHA)) != 1 {
		return gotSHA, fmt.Errorf("external binary %s SHA-256 changed since trust ceremony (%s != %s)", resolvedPath, gotSHA[:12], expectedSHA[:12])
	}
	if strings.TrimSpace(vaultDir) == "" {
		return resolvedPath, nil
	}

	baseName := filepath.Base(filepath.Clean(resolvedPath))
	if baseName == "" || baseName == "." || baseName == ".." {
		baseName = "bin"
	}
	cacheRoot := filepath.Join(vaultDir, "private", "exec-cache")
	if st, lErr := os.Lstat(cacheRoot); lErr == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("refusing symlinked exec-cache root %s", cacheRoot)
	}
	cacheDir := filepath.Join(cacheRoot, expectedSHA)
	if st, lErr := os.Lstat(cacheDir); lErr == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("refusing symlinked exec-cache entry %s", cacheDir)
	}
	if mkErr := os.MkdirAll(cacheDir, 0o700); mkErr != nil {
		return "", mkErr
	}
	cachedPath := filepath.Join(cacheDir, baseName)
	if cf, oErr := store.OpenReadNoFollow(cachedPath); oErr == nil {
		h := sha256.New()
		_, cpErr := io.Copy(h, cf)
		_ = cf.Close()
		if cpErr == nil {
			cachedSHA := hex.EncodeToString(h.Sum(nil))
			if subtle.ConstantTimeCompare([]byte(cachedSHA), []byte(expectedSHA)) == 1 {
				return cachedPath, nil
			}
		}
		_ = os.Chmod(cachedPath, 0o600)
		_ = os.Remove(cachedPath)
	}

	if wErr := store.WriteAtomic(cachedPath, rawBytes, 0o500); wErr != nil {
		return "", wErr
	}
	return cachedPath, nil
}

func verifyTrustedBinaryInternal(vaultDir, sourceID, rawBin string, notifyOutbox bool) (string, error) {
	if IsSelfHQBinary(rawBin) {
		return ResolveSelfHQExecutable(), nil
	}
	reg, err := LoadTrustRegistry(vaultDir)
	if err != nil {
		return "", err
	}
	target, ok := reg.Targets[sourceID]
	if !ok {
		return "", fmt.Errorf("%s is not trusted in private/trust.json", sourceID)
	}
	pin, hasPin := target.Binaries[rawBin]
	if !hasPin {
		for _, cand := range target.Binaries {
			if cand.ResolvedPath == rawBin || cand.RawRef == rawBin {
				pin = cand
				hasPin = true
				break
			}
		}
	}
	if !hasPin {
		if notifyOutbox {
			emitTrustMismatchNoticeOnce(vaultDir, sourceID, rawBin, "unpinned_binary")
		}
		return "", fmt.Errorf("external binary %q in %s is not pinned in private/trust.json", rawBin, sourceID)
	}
	if pin.SelfHQ {
		return ResolveSelfHQExecutable(), nil
	}
	execPath, err := stageVerifiedExecCache(vaultDir, pin.ResolvedPath, pin.SHA256)
	if err != nil {
		if notifyOutbox && len(execPath) >= 12 {
			emitTrustMismatchNoticeOnce(vaultDir, sourceID, rawBin, execPath[:12])
		}
		return "", fmt.Errorf("verify pinned binary %s: %w", pin.ResolvedPath, err)
	}
	return execPath, nil
}

// IsSecurityLoosened returns true only when <vault>/extensions.json declares a security_loosening rule
// with a >=16-character justification AND <vault>/private/trust.json pins that justification to componentSHA.
func IsSecurityLoosened(vaultDir, sourceID, targetKey, componentSHA string) (bool, string) {
	if strings.TrimSpace(vaultDir) == "" || strings.TrimSpace(targetKey) == "" || strings.TrimSpace(componentSHA) == "" {
		return false, ""
	}
	st, err := LoadVaultState(vaultDir)
	if err != nil {
		return false, ""
	}
	var matchedJust string
	foundInState := false
	for _, r := range st.SecurityLoosening {
		if strings.TrimSpace(r.Target) == targetKey && !r.Value && len(strings.TrimSpace(r.Justification)) >= 16 {
			matchedJust = strings.TrimSpace(r.Justification)
			foundInState = true
			break
		}
	}
	if !foundInState {
		return false, ""
	}
	reg, err := LoadTrustRegistry(vaultDir)
	if err != nil {
		return false, ""
	}
	t, ok := reg.Targets[sourceID]
	if !ok {
		if vt, vok := reg.Targets["vault"]; vok {
			t = vt
			ok = true
		}
	}
	if !ok || t.Loosenings == nil {
		return false, ""
	}
	pin, ok := t.Loosenings[targetKey]
	if !ok || len(strings.TrimSpace(pin.Justification)) < 16 {
		return false, ""
	}
	if subtle.ConstantTimeCompare([]byte(pin.ComponentSHA256), []byte(componentSHA)) != 1 {
		return false, ""
	}
	return true, matchedJust
}

func pinPackOrSystemBinaryRef(canonPack, rawBin string) (PinnedBinary, error) {
	rawBin = strings.TrimSpace(rawBin)
	if rawBin == "" {
		return PinnedBinary{}, fmt.Errorf("empty binary reference")
	}
	if !filepath.IsAbs(rawBin) && (strings.Contains(rawBin, "/") || strings.Contains(rawBin, "\\")) {
		cleanRel := filepath.ToSlash(filepath.Clean(rawBin))
		if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, "../") {
			return PinnedBinary{}, fmt.Errorf("invalid pack-relative binary path %q", rawBin)
		}
		full := filepath.Join(canonPack, filepath.FromSlash(cleanRel))
		canonFull, err := canonicalDir(full)
		if err != nil {
			return PinnedBinary{}, fmt.Errorf("resolve pack binary %q: %w", rawBin, err)
		}
		chkRel, rErr := filepath.Rel(canonPack, canonFull)
		if rErr != nil || strings.HasPrefix(chkRel, "..") {
			return PinnedBinary{}, fmt.Errorf("pack binary %q escapes pack root", rawBin)
		}
		sum, hErr := ComputeStatCachedSHA256(canonFull)
		if hErr != nil {
			return PinnedBinary{}, hErr
		}
		return PinnedBinary{
			RawRef:       rawBin,
			ResolvedPath: canonFull,
			SHA256:       sum,
		}, nil
	}
	return pinBinaryRef(rawBin)
}

func pinBinaryRef(rawBin string) (PinnedBinary, error) {
	rawBin = strings.TrimSpace(rawBin)
	if rawBin == "" {
		return PinnedBinary{}, fmt.Errorf("empty binary reference")
	}
	if IsSelfHQBinary(rawBin) {
		return PinnedBinary{
			RawRef:       rawBin,
			ResolvedPath: "hq",
			SelfHQ:       true,
			HQVersion:    version.Version,
			HQCommit:     version.EffectiveCommit(),
		}, nil
	}
	resolved := rawBin
	if !filepath.IsAbs(resolved) {
		lp, err := exec.LookPath(resolved)
		if err != nil {
			return PinnedBinary{}, fmt.Errorf("resolve external binary %q on PATH: %w", rawBin, err)
		}
		resolved = lp
	}
	canonBin, err := canonicalDir(resolved)
	if err != nil {
		return PinnedBinary{}, fmt.Errorf("canonicalize binary %q: %w", resolved, err)
	}
	sum, err := ComputeStatCachedSHA256(canonBin)
	if err != nil {
		return PinnedBinary{}, err
	}
	return PinnedBinary{
		RawRef:       rawBin,
		ResolvedPath: canonBin,
		SHA256:       sum,
	}, nil
}

// BuildTrustBundle inspects either "vault" (<vault>/connectors/* + <vault>/brief.d/*) or a companion pack
// directory and constructs a deterministic TrustBundle + human-readable diff summary.
func BuildTrustBundle(vaultDir, targetSpec, explicitPath string) (TrustBundle, error) {
	targetSpec = strings.TrimSpace(targetSpec)
	nowStr := time.Now().UTC().Format(time.RFC3339)
	vaultState, _ := LoadVaultState(vaultDir)

	if targetSpec == "vault" || targetSpec == "" {
		canonVault, err := canonicalDir(vaultDir)
		if err != nil {
			return TrustBundle{}, err
		}
		tt := TrustedTarget{
			SourceID:      "vault",
			CanonicalRoot: canonVault,
			Files:         make(map[string]string),
			Binaries:      make(map[string]PinnedBinary),
			Loosenings:    make(map[string]PinnedLoosening),
			ApprovedAt:    nowStr,
		}
		var lines []string
		// Walk <vault>/connectors/*/connector.json and <vault>/brief.d/*.{json,md}
		for _, sub := range []string{"connectors", "brief.d"} {
			dir := filepath.Join(canonVault, sub)
			if st, sErr := os.Stat(dir); sErr != nil || !st.IsDir() {
				continue
			}
			_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, wErr error) error {
				if wErr != nil || d.IsDir() {
					return nil
				}
				ext := strings.ToLower(filepath.Ext(d.Name()))
				if ext != ".json" && ext != ".md" {
					return nil
				}
				rel, rErr := filepath.Rel(canonVault, p)
				if rErr != nil {
					return nil
				}
				relSlash := filepath.ToSlash(rel)
				sha, hErr := ComputeStatCachedSHA256(p)
				if hErr != nil {
					return nil
				}
				tt.Files[relSlash] = sha
				lines = append(lines, fmt.Sprintf("file %s (sha256:%s)", relSlash, sha[:12]))
				if ext == ".json" {
					for _, binRef := range extractJSONBinaryRefs(p) {
						if pb, bErr := pinBinaryRef(binRef); bErr == nil {
							tt.Binaries[binRef] = pb
							if !pb.SelfHQ {
								lines = append(lines, fmt.Sprintf("bin  %s -> %s (sha256:%s)", binRef, pb.ResolvedPath, pb.SHA256[:12]))
							}
						}
					}
				}
				return nil
			})
		}
		attachLoosenings(&tt, vaultState, "")
		sort.Strings(lines)
		return TrustBundle{
			SchemaVersion: TrustSchemaVersion,
			Target:        tt,
			SummaryLines:  lines,
		}, nil
	}

	packID := strings.TrimPrefix(targetSpec, "pack:")
	packDir, err := ResolvePackDirectory(vaultDir, packID, explicitPath)
	if err != nil {
		return TrustBundle{}, err
	}
	canonPack, err := canonicalDir(packDir)
	if err != nil {
		return TrustBundle{}, err
	}
	manifest, err := LoadPackManifest(canonPack)
	if err != nil {
		return TrustBundle{}, err
	}
	idxSHA, err := ComputeStatCachedSHA256(filepath.Join(canonPack, "hq-extension.json"))
	if err != nil {
		return TrustBundle{}, err
	}

	tt := TrustedTarget{
		SourceID:       "pack:" + manifest.ID,
		CanonicalRoot:  canonPack,
		Version:        manifest.Version,
		ManifestSHA256: idxSHA,
		Files:          map[string]string{"hq-extension.json": idxSHA},
		Binaries:       make(map[string]PinnedBinary),
		Loosenings:     make(map[string]PinnedLoosening),
		ApprovedAt:     nowStr,
	}
	lines := []string{fmt.Sprintf("pack %s@%s (%s, hq-extension.json:%s)", manifest.ID, manifest.Version, canonPack, idxSHA[:12])}

	pinRel := func(relPath string) (string, error) {
		cleanRel := filepath.ToSlash(filepath.Clean(strings.TrimSpace(relPath)))
		if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, "../") || filepath.IsAbs(cleanRel) {
			return "", fmt.Errorf("invalid pack relative path %q", relPath)
		}
		full := filepath.Join(canonPack, filepath.FromSlash(cleanRel))
		canonFull, cErr := canonicalDir(full)
		if cErr != nil {
			return "", cErr
		}
		chkRel, rErr := filepath.Rel(canonPack, canonFull)
		if rErr != nil || strings.HasPrefix(chkRel, "..") {
			return "", fmt.Errorf("component %q escapes pack root", relPath)
		}
		sha, hErr := ComputeStatCachedSHA256(canonFull)
		if hErr != nil {
			return "", hErr
		}
		tt.Files[filepath.ToSlash(chkRel)] = sha
		lines = append(lines, fmt.Sprintf("file %s (sha256:%s)", filepath.ToSlash(chkRel), sha[:12]))
		return sha, nil
	}

	pinComponentJSON := func(rel string) error {
		if _, err := pinRel(rel); err != nil {
			return err
		}
		for _, binRef := range extractJSONBinaryRefs(filepath.Join(canonPack, filepath.FromSlash(rel))) {
			pb, bErr := pinBinaryRef(binRef)
			if bErr != nil {
				return bErr
			}
			tt.Binaries[binRef] = pb
		}
		return nil
	}
	for _, list := range [][]string{
		manifest.Components.Connectors,
		manifest.Components.BriefExtensions,
	} {
		for _, rel := range list {
			if err := pinComponentJSON(rel); err != nil {
				return TrustBundle{}, err
			}
		}
	}
	for _, rel := range manifest.Components.ConnectorDefaults {
		if err := pinComponentJSON(rel); err != nil {
			return TrustBundle{}, err
		}
	}
	for _, ov := range manifest.Components.FrontDeskOverlays {
		if _, err := pinRel(ov.Path); err != nil {
			return TrustBundle{}, err
		}
	}
	for _, so := range manifest.Components.SkillOverrides {
		relSkill := so.SkillMD
		if strings.TrimSpace(relSkill) == "" && strings.TrimSpace(so.Skill) != "" {
			relSkill = filepath.ToSlash(filepath.Join("skills", so.Skill, "SKILL.md"))
		}
		if relSkill != "" {
			if _, err := pinRel(relSkill); err != nil {
				return TrustBundle{}, err
			}
		}
	}
	for _, sc := range manifest.Components.Schedules {
		if sc.Precheck != nil && len(sc.Precheck.Argv) > 0 {
			pb, bErr := pinBinaryRef(sc.Precheck.Argv[0])
			if bErr != nil {
				return TrustBundle{}, bErr
			}
			tt.Binaries[sc.Precheck.Argv[0]] = pb
		}
	}
	pinExecComponent := func(kind, id string, cmd []string) error {
		if len(cmd) == 0 || strings.TrimSpace(cmd[0]) == "" {
			return fmt.Errorf("%s %q missing command[0]", kind, id)
		}
		rawBin := strings.TrimSpace(cmd[0])
		pb, bErr := pinPackOrSystemBinaryRef(canonPack, rawBin)
		if bErr != nil {
			return fmt.Errorf("%s %q: %w", kind, id, bErr)
		}
		tt.Binaries[rawBin] = pb
		shaShort := pb.SHA256
		if len(shaShort) > 12 {
			shaShort = shaShort[:12]
		}
		lines = append(lines, fmt.Sprintf("%s %s/%s -> %s (sha256:%s)", kind, manifest.ID, id, pb.ResolvedPath, shaShort))
		return nil
	}
	for _, cb := range manifest.Components.ChannelBridges {
		if err := pinExecComponent("channel_bridge", cb.ID, cb.Command); err != nil {
			return TrustBundle{}, err
		}
	}
	for _, wp := range manifest.Components.WorkspaceProviders {
		if err := pinExecComponent("workspace_provider", wp.ID, wp.Command); err != nil {
			return TrustBundle{}, err
		}
		if skill := strings.TrimSpace(wp.Skill); skill != "" {
			if _, err := pinRel(filepath.ToSlash(filepath.Join("skills", skill, "SKILL.md"))); err != nil {
				return TrustBundle{}, fmt.Errorf("workspace_provider %q skill: %w", wp.ID, err)
			}
		}
	}
	for _, dt := range manifest.Components.DelegateTransports {
		if err := pinExecComponent("delegate_transport", dt.ID, dt.Command); err != nil {
			return TrustBundle{}, err
		}
	}
	// RFC-0026 D3: SHA-256-pin all pack-declared skills/<name>/SKILL.md files so
	// tainted sessions may read only verified skill instructions from trusted packs.
	if skillEntries, sErr := os.ReadDir(filepath.Join(canonPack, "skills")); sErr == nil {
		for _, ent := range skillEntries {
			if !ent.IsDir() || !reValidPackSkillDir.MatchString(ent.Name()) {
				continue
			}
			relSkill := filepath.ToSlash(filepath.Join("skills", ent.Name(), "SKILL.md"))
			if _, already := tt.Files[relSkill]; already {
				continue
			}
			fullSkill := filepath.Join(canonPack, filepath.FromSlash(relSkill))
			if st, lErr := os.Lstat(fullSkill); lErr == nil && st.Mode().IsRegular() && st.Mode()&os.ModeSymlink == 0 {
				_, _ = pinRel(relSkill)
			}
		}
	}
	for _, reqBin := range manifest.Requires.Bins {
		ref := reqBin.Name
		if len(reqBin.ProbeArgv) > 0 && strings.TrimSpace(reqBin.ProbeArgv[0]) != "" {
			ref = reqBin.ProbeArgv[0]
		}
		if ref != "" {
			if pb, bErr := pinBinaryRef(ref); bErr == nil {
				tt.Binaries[ref] = pb
			}
		}
	}

	// Inventory native Antigravity surfaces in the plugin so the user sees full blast radius before approving.
	tt.NativeSurfaces, tt.HasCompetingPreToolUse = inventoryNativeSurfaces(canonPack)
	for _, ns := range tt.NativeSurfaces {
		lines = append(lines, "native "+ns)
	}
	if tt.HasCompetingPreToolUse {
		lines = append(lines, "WARNING: companion hooks.json declares a PreToolUse handler")
	}

	attachLoosenings(&tt, vaultState, manifest.ID)
	return TrustBundle{
		SchemaVersion: TrustSchemaVersion,
		Target:        tt,
		SummaryLines:  lines,
	}, nil
}

func attachLoosenings(tt *TrustedTarget, st VaultState, packID string) {
	nowStr := time.Now().UTC().Format(time.RFC3339)
	for _, r := range st.SecurityLoosening {
		if r.Value || len(strings.TrimSpace(r.Justification)) < 16 {
			continue
		}
		target := strings.TrimSpace(r.Target)
		if packID != "" && !strings.HasPrefix(target, packID+"/") {
			continue
		}
		// Bind to matching component SHA-256 (or ManifestSHA256 fallback if pack-level)
		compSHA := tt.ManifestSHA256
		for rel, fsha := range tt.Files {
			baseNoExt := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
			if strings.Contains(target, "/"+baseNoExt+".") || strings.HasPrefix(target, baseNoExt+".") {
				compSHA = fsha
				break
			}
		}
		if compSHA == "" {
			for _, fsha := range tt.Files {
				compSHA = fsha
				break
			}
		}
		if compSHA != "" {
			tt.Loosenings[target] = PinnedLoosening{
				Target:          target,
				ComponentSHA256: compSHA,
				Justification:   strings.TrimSpace(r.Justification),
				ApprovedAt:      nowStr,
			}
		}
	}
}

func extractJSONBinaryRefs(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var obj map[string]interface{}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	var refs []string
	if argv, ok := obj["argv"].([]interface{}); ok && len(argv) > 0 {
		if s, ok := argv[0].(string); ok && strings.TrimSpace(s) != "" {
			refs = append(refs, strings.TrimSpace(s))
		}
	}
	if drv, ok := obj["driver"].(map[string]interface{}); ok {
		if cmd, ok := drv["command"].([]interface{}); ok && len(cmd) > 0 {
			if s, ok := cmd[0].(string); ok && strings.TrimSpace(s) != "" {
				refs = append(refs, strings.TrimSpace(s))
			}
		}
	}
	if tr, ok := obj["transport"].(map[string]interface{}); ok {
		if cli, ok := tr["cli"].(map[string]interface{}); ok {
			if p, ok := cli["path"].(string); ok && strings.TrimSpace(p) != "" {
				refs = append(refs, strings.TrimSpace(p))
			}
		}
	}
	return refs
}

func inventoryNativeSurfaces(packDir string) ([]string, bool) {
	var surfaces []string
	hasPreTool := false
	for _, sub := range []string{"rules", "skills", "sidecars"} {
		d := filepath.Join(packDir, sub)
		if entries, err := os.ReadDir(d); err == nil && len(entries) > 0 {
			surfaces = append(surfaces, fmt.Sprintf("%s/ (%d entries)", sub, len(entries)))
		}
	}
	hooksPath := filepath.Join(packDir, "hooks.json")
	if b, err := os.ReadFile(hooksPath); err == nil {
		surfaces = append(surfaces, "hooks.json")
		if strings.Contains(string(b), "PreToolUse") || strings.Contains(string(b), "PreInvocation") {
			hasPreTool = true
		}
	}
	if _, err := os.Stat(filepath.Join(packDir, "mcp.json")); err == nil {
		surfaces = append(surfaces, "mcp.json")
	}
	return surfaces, hasPreTool
}

// StageTrustProposal stages an approval-bound `extension_trust` proposal whose immutable TrustBundle
// is piped via /dev/fd/3 into `hq extension apply-trust` only upon human-verified approval.
func StageTrustProposal(vaultDir, targetSpec, explicitPath, convID string) (approvals.Proposal, TrustBundle, error) {
	bundle, err := BuildTrustBundle(vaultDir, targetSpec, explicitPath)
	if err != nil {
		return approvals.Proposal{}, TrustBundle{}, err
	}
	payloadBytes, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return approvals.Proposal{}, TrustBundle{}, err
	}
	exe := ResolveSelfHQExecutable()
	summary := fmt.Sprintf("Trust %s in private/trust.json (%d file(s), %d binary pin(s))",
		bundle.Target.SourceID, len(bundle.Target.Files), len(bundle.Target.Binaries))
	prop, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:     "extension_trust",
		Summary:        summary,
		CommandArgv:    []string{exe, "extension", "apply-trust", "--payload-file={{payload_file}}"},
		ConversationID: convID,
		PayloadBytes:   append(payloadBytes, '\n'),
		Metadata: map[string]string{
			"sourceId":     bundle.Target.SourceID,
			"bodyPreview":  strings.Join(bundle.SummaryLines, "\n"),
			"draftPayload": string(payloadBytes),
		},
	})
	return prop, bundle, err
}

// PinExternalBinary resolves and SHA-256-hashes an external CLI binary (rejecting shells/interpreters/hq).
func PinExternalBinary(rawBin string) (PinnedBinary, error) {
	rawBin = strings.TrimSpace(rawBin)
	if rawBin == "" || IsBlockedComponentCommand0(rawBin) {
		return PinnedBinary{}, fmt.Errorf("binary %q cannot be empty, a shell/interpreter, or hq itself", rawBin)
	}
	return pinBinaryRef(rawBin)
}

// VerifyTrustedPackSkillFile reports whether absPath is a SHA-256-verified SKILL.md
// inside an active trusted companion pack's skills/ directory, returning its logical
// "<packID>/<skillName>" identifier when valid. Performs zero writes.
func VerifyTrustedPackSkillFile(vaultDir, absPath string) (string, bool) {
	if strings.TrimSpace(vaultDir) == "" || strings.TrimSpace(absPath) == "" {
		return "", false
	}
	canonFile, err := canonicalDir(absPath)
	if err != nil {
		return "", false
	}
	packs, reg := loadActiveTrustedPacksWithReg(vaultDir)
	for _, dp := range packs {
		canonPack, cErr := canonicalDir(dp.Path)
		if cErr != nil {
			continue
		}
		rel, rErr := filepath.Rel(canonPack, canonFile)
		if rErr != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		relSlash := filepath.ToSlash(rel)
		parts := strings.Split(relSlash, "/")
		if len(parts) != 3 || parts[0] != "skills" || parts[2] != "SKILL.md" {
			continue
		}
		if _, vErr := verifyTrustedFileWithRegistry(vaultDir, reg, "pack:"+dp.ID, canonFile, false); vErr == nil {
			return dp.ID + "/" + parts[1], true
		}
	}
	return "", false
}

// UpsertTrustTarget atomically updates a single TrustedTarget entry (e.g. "delegates")
// in <vault>/private/trust.json under store.RankExtensions.
func UpsertTrustTarget(vaultDir string, target TrustedTarget) error {
	if strings.TrimSpace(vaultDir) == "" {
		return fmt.Errorf("vaultDir is required")
	}
	if strings.TrimSpace(target.SourceID) == "" || strings.TrimSpace(target.CanonicalRoot) == "" {
		return fmt.Errorf("trust target missing source_id or canonical_root")
	}
	return store.With(context.Background(), store.RankExtensions, trustLockPath(vaultDir), func(_ context.Context) error {
		reg, err := LoadTrustRegistry(vaultDir)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if reg.Targets == nil {
			reg.Targets = make(map[string]TrustedTarget)
		}
		target.ApprovedAt = time.Now().UTC().Format(time.RFC3339)
		reg.Targets[target.SourceID] = target
		reg.SchemaVersion = TrustSchemaVersion
		reg.UpdatedAt = target.ApprovedAt
		out, err := json.MarshalIndent(reg, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(trustFilePath(vaultDir)), 0o700); err != nil {
			return err
		}
		return store.WriteAtomic(trustFilePath(vaultDir), append(out, '\n'), 0o600)
	})
}

// ApplyTrustBundle persists a verified TrustBundle into <vault>/private/trust.json (0600).
func ApplyTrustBundle(vaultDir string, rawBundle []byte) error {
	if strings.TrimSpace(vaultDir) == "" {
		return fmt.Errorf("vaultDir is required")
	}
	var bundle TrustBundle
	dec := json.NewDecoder(strings.NewReader(string(rawBundle)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&bundle); err != nil {
		return fmt.Errorf("invalid trust bundle JSON: %w", err)
	}
	return UpsertTrustTarget(vaultDir, bundle.Target)
}
