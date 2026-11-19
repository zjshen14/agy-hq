package hostpaths

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/store"
)

// Roots holds canonical resolved Antigravity host directories across standard and enterprise layouts.
type Roots struct {
	HomeDir       string `json:"homeDir"`
	GeminiDir     string `json:"geminiDir"`
	ConfigDir     string `json:"configDir"`
	AppDataDir    string `json:"appDataDir"`
	BrainDir      string `json:"brainDir"`
	PluginsDir    string `json:"pluginsDir"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
	FromPersisted bool   `json:"-"`
}

// IsTempPath reports whether p resolves (after symlink evaluation) inside a real OS temporary directory
// (/tmp, /private/tmp, /var/tmp, /var/folders, etc.). It intentionally ignores caller-controlled TMPDIR
// overrides pointing at non-temp locations so an untrusted process cannot spoof temp-vault status.
func IsTempPath(p string) bool {
	if strings.TrimSpace(p) == "" {
		return false
	}
	cleanP := filepath.Clean(p)
	if !filepath.IsAbs(cleanP) {
		if abs, err := filepath.Abs(cleanP); err == nil {
			cleanP = filepath.Clean(abs)
		}
	}
	resolved := evalExistingAncestor(cleanP)
	sysRoots := []string{
		"/tmp",
		"/private/tmp",
		"/var/tmp",
		"/private/var/tmp",
		"/var/folders",
		"/private/var/folders",
	}
	for _, root := range sysRoots {
		if resolved == root || strings.HasPrefix(resolved, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func evalExistingAncestor(p string) string {
	if realP, err := filepath.EvalSymlinks(p); err == nil && realP != "" {
		return filepath.Clean(realP)
	}
	var tail []string
	cur := p
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		tail = append(tail, filepath.Base(cur))
		if realParent, err := filepath.EvalSymlinks(parent); err == nil && realParent != "" {
			out := filepath.Clean(realParent)
			for i := len(tail) - 1; i >= 0; i-- {
				out = filepath.Join(out, tail[i])
			}
			return out
		}
		cur = parent
	}
}

// ResolveHomeDir resolves the effective user home directory honoring HQ_HOME_DIR first.
func ResolveHomeDir() (string, error) {
	if h := strings.TrimSpace(os.Getenv("HQ_HOME_DIR")); h != "" {
		return filepath.Abs(h)
	}
	if h := strings.TrimSpace(os.Getenv("HOME")); h != "" {
		return filepath.Clean(h), nil
	}
	return os.UserHomeDir()
}

// PersistedRootsPath returns $XDG_CONFIG_HOME/agy-hq/roots.json (or <home>/.config/agy-hq/roots.json).
func PersistedRootsPath() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		abs, err := filepath.Abs(xdg)
		if err != nil {
			return "", err
		}
		return filepath.Join(abs, "agy-hq", "roots.json"), nil
	}
	home, err := ResolveHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "agy-hq", "roots.json"), nil
}

func isHermeticTestEnv(vaultDir, home string) bool {
	if IsTempPath(vaultDir) || IsTempPath(home) {
		return true
	}
	if envV := strings.TrimSpace(os.Getenv("HQ_VAULT_DIR")); envV != "" && IsTempPath(envV) {
		return true
	}
	if strings.TrimSpace(os.Getenv("HQ_HOME_DIR")) != "" {
		return true
	}
	return false
}

func loadPersistedRoots(vaultDir, home string) *Roots {
	if isHermeticTestEnv(vaultDir, home) {
		// Allow explicit XDG_CONFIG_HOME temp tests to verify roots.json persistence only when XDG_CONFIG_HOME is explicitly set to a non-empty path and HQ_ALLOW_TEST_ROOTS_CACHE=1
		if os.Getenv("HQ_ALLOW_TEST_ROOTS_CACHE") != "1" || strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")) == "" {
			return nil
		}
	}
	p, err := PersistedRootsPath()
	if err != nil || p == "" {
		return nil
	}
	if st, lErr := os.Lstat(p); lErr != nil || !store.VerifyOwnedPrivateFile(st) {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var cached Roots
	if json.Unmarshal(data, &cached) != nil {
		return nil
	}
	if IsTempPath(cached.AppDataDir) || IsTempPath(cached.ConfigDir) || IsTempPath(cached.GeminiDir) {
		if os.Getenv("HQ_ALLOW_TEST_ROOTS_CACHE") != "1" {
			return nil
		}
	}
	cached.FromPersisted = true
	return &cached
}

// Resolve computes the canonical Antigravity host directory layout for the active environment.
// Precedence: explicit env vars -> persisted roots.json (non-temp sessions only) -> <home>/.gemini/... defaults.
func Resolve(vaultDir string) Roots {
	home, _ := ResolveHomeDir()
	cached := loadPersistedRoots(vaultDir, home)

	geminiDir := strings.TrimSpace(os.Getenv("HQ_GEMINI_DIR"))
	if geminiDir == "" && cached != nil && cached.GeminiDir != "" {
		geminiDir = cached.GeminiDir
	}
	if geminiDir == "" && home != "" {
		geminiDir = filepath.Join(home, ".gemini")
	}
	if geminiDir != "" {
		geminiDir = filepath.Clean(geminiDir)
	}

	hermetic := IsTempPath(home) || IsTempPath(vaultDir) || strings.TrimSpace(os.Getenv("HQ_HOME_DIR")) != ""

	configEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONFIG_DIR"))
	if hermetic && configEnv != "" && !IsTempPath(configEnv) {
		configEnv = ""
	}
	configDir := configEnv
	if configDir == "" {
		configDir = strings.TrimSpace(os.Getenv("HQ_CONFIG_DIR"))
	}
	if configDir == "" && cached != nil && cached.ConfigDir != "" {
		configDir = cached.ConfigDir
	}
	if configDir == "" && geminiDir != "" {
		configDir = filepath.Join(geminiDir, "config")
	}
	if configDir != "" {
		configDir = filepath.Clean(configDir)
	}

	appDataEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_APP_DATA_DIR"))
	if hermetic && appDataEnv != "" && !IsTempPath(appDataEnv) {
		appDataEnv = ""
	}
	appDataDir := appDataEnv
	if appDataDir == "" && cached != nil && cached.AppDataDir != "" {
		appDataDir = cached.AppDataDir
	}
	if appDataDir == "" && geminiDir != "" {
		appDataDir = filepath.Join(geminiDir, "antigravity")
	}
	if appDataDir != "" {
		appDataDir = filepath.Clean(appDataDir)
	}

	pluginsDir := strings.TrimSpace(os.Getenv("HQ_PLUGINS_DIR"))
	if pluginsDir == "" && cached != nil && cached.PluginsDir != "" && configEnv == "" && os.Getenv("HQ_CONFIG_DIR") == "" {
		pluginsDir = cached.PluginsDir
	}
	if pluginsDir == "" && configDir != "" {
		pluginsDir = filepath.Join(configDir, "plugins")
	}
	if pluginsDir != "" {
		pluginsDir = filepath.Clean(pluginsDir)
	}

	brainDir := strings.TrimSpace(os.Getenv("HQ_BRAIN_DIR"))
	if brainDir == "" {
		effVault := strings.TrimSpace(vaultDir)
		if effVault == "" {
			effVault = strings.TrimSpace(os.Getenv("HQ_VAULT_DIR"))
		}
		if effVault != "" && IsTempPath(effVault) && (appDataEnv == "" || !IsTempPath(appDataEnv)) {
			brainDir = filepath.Join(filepath.Dir(filepath.Clean(effVault)), ".brain")
		} else if appDataDir != "" {
			brainDir = filepath.Join(appDataDir, "brain")
		}
	}
	if brainDir != "" {
		brainDir = filepath.Clean(brainDir)
	}

	return Roots{
		HomeDir:       home,
		GeminiDir:     geminiDir,
		ConfigDir:     configDir,
		AppDataDir:    appDataDir,
		BrainDir:      brainDir,
		PluginsDir:    pluginsDir,
		FromPersisted: cached != nil,
	}
}

// PersistFromLiveEnv records relocated Antigravity roots to $XDG_CONFIG_HOME/agy-hq/roots.json (0600)
// when explicit host overrides (ANTIGRAVITY_APP_DATA_DIR / ANTIGRAVITY_CONFIG_DIR / HQ_GEMINI_DIR) are set.
// It is a strict no-op in hermetic temp-vault test runs unless HQ_ALLOW_TEST_ROOTS_CACHE=1.
func PersistFromLiveEnv(vaultDir string) error {
	if os.Getenv("ANTIGRAVITY_APP_DATA_DIR") == "" &&
		os.Getenv("ANTIGRAVITY_CONFIG_DIR") == "" &&
		os.Getenv("HQ_GEMINI_DIR") == "" &&
		os.Getenv("HQ_CONFIG_DIR") == "" &&
		os.Getenv("HQ_PLUGINS_DIR") == "" {
		return nil
	}
	home, _ := ResolveHomeDir()
	if isHermeticTestEnv(vaultDir, home) && os.Getenv("HQ_ALLOW_TEST_ROOTS_CACHE") != "1" {
		return nil
	}
	roots := Resolve(vaultDir)
	if (IsTempPath(roots.AppDataDir) || IsTempPath(roots.ConfigDir)) && os.Getenv("HQ_ALLOW_TEST_ROOTS_CACHE") != "1" {
		return nil
	}
	p, err := PersistedRootsPath()
	if err != nil || p == "" {
		return err
	}
	if st, lErr := os.Lstat(filepath.Dir(p)); lErr == nil && st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("hostpaths: refusing symlinked config dir %s", filepath.Dir(p))
	}
	roots.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(roots, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(p, append(data, '\n'), 0o600)
}
