package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Mode controls whether OS subprocess credential masking is applied.
const (
	ModeAuto     = "auto"
	ModeRequired = "required"
	ModeOff      = "off"
)

// Config parses the top-level `sandbox:` block in local/config.yaml.
type Config struct {
	Mode string `json:"mode"` // auto | required | off
}

// Capability reports whether native zero-dependency OS sandboxing is available on the host.
type Capability struct {
	Available bool   `json:"available"`
	Backend   string `json:"backend"` // bwrap | sandbox-exec | none
	Binary    string `json:"binary,omitempty"`
	Detail    string `json:"detail"`
}

var (
	capOnce   sync.Once
	cachedCap Capability
)

// LoadConfig reads `sandbox.mode` from local/config.yaml (defaulting to ModeAuto).
func LoadConfig(vaultDir string) Config {
	cfg := Config{Mode: ModeAuto}
	if strings.TrimSpace(vaultDir) == "" {
		return cfg
	}
	raw, err := os.ReadFile(filepath.Join(vaultDir, "config.yaml"))
	if err != nil {
		return cfg
	}
	inSandbox := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent == 0 {
			inSandbox = strings.HasPrefix(trimmed, "sandbox:")
			continue
		}
		if !inSandbox {
			continue
		}
		if idx := strings.Index(trimmed, ":"); idx > 0 {
			key := strings.TrimSpace(trimmed[:idx])
			val := strings.TrimSpace(trimmed[idx+1:])
			if cIdx := strings.Index(val, " #"); cIdx >= 0 {
				val = strings.TrimSpace(val[:cIdx])
			}
			val = strings.Trim(val, `"'`)
			if key == "mode" {
				switch strings.ToLower(val) {
				case ModeAuto, ModeRequired, ModeOff:
					cfg.Mode = strings.ToLower(val)
				}
			}
		}
	}
	return cfg
}

// DetectCapability probes whether bwrap (Linux) or sandbox-exec (macOS) can run unprivileged on this host.
func DetectCapability() Capability {
	capOnce.Do(func() {
		cachedCap = probeCapabilityUncached()
	})
	return cachedCap
}

func probeCapabilityUncached() Capability {
	switch runtime.GOOS {
	case "linux":
		bin, err := exec.LookPath("bwrap")
		if err != nil || bin == "" {
			return Capability{
				Available: false,
				Backend:   "none",
				Detail:    "bubblewrap (`bwrap`) not installed; PreToolUse regex + SanitizedEnv active (install bubblewrap for kernel credential masking)",
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "--ro-bind", "/", "/", "--", "true")
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = runErr.Error()
			}
			return Capability{
				Available: false,
				Backend:   "bwrap",
				Binary:    bin,
				Detail:    fmt.Sprintf("bwrap present (%s) but unprivileged namespace probe failed (%s); falling back to SanitizedEnv + PreToolUse guard", bin, msg),
			}
		}
		return Capability{
			Available: true,
			Backend:   "bwrap",
			Binary:    bin,
			Detail:    fmt.Sprintf("Linux bubblewrap active (%s) — masks local/private/ and host SSH/cloud keys", bin),
		}
	case "darwin":
		bin, err := exec.LookPath("sandbox-exec")
		if err != nil || bin == "" {
			return Capability{
				Available: false,
				Backend:   "none",
				Detail:    "sandbox-exec not found on macOS; PreToolUse regex + SanitizedEnv active",
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "-p", "(version 1)(allow default)", "/usr/bin/true")
		if runErr := cmd.Run(); runErr != nil {
			return Capability{
				Available: false,
				Backend:   "sandbox-exec",
				Binary:    bin,
				Detail:    fmt.Sprintf("sandbox-exec probe failed (%v); falling back to SanitizedEnv", runErr),
			}
		}
		return Capability{
			Available: true,
			Backend:   "sandbox-exec",
			Binary:    bin,
			Detail:    fmt.Sprintf("macOS Seatbelt active (%s) — denies file-read*/file-write* on local/private/ and ~/.ssh", bin),
		}
	default:
		return Capability{
			Available: false,
			Backend:   "none",
			Detail:    fmt.Sprintf("OS credential namespace not supported on %s; PreToolUse regex + SanitizedEnv active", runtime.GOOS),
		}
	}
}

var allowedEnvPrefixes = []string{
	"HOME=",
	"USER=",
	"LOGNAME=",
	"PATH=",
	"SHELL=",
	"LANG=",
	"LC_",
	"TZ=",
	"TERM=",
	"TMPDIR=",
	"GOPATH=",
	"GOROOT=",
	"GOCACHE=",
	"GOMODCACHE=",
	"XDG_CACHE_HOME=",
	"HQ_VAULT_DIR=",
	"HQ_ROOT_DIR=",
}

// SanitizedEnv constructs a scrubbed environment stripping ambient tokens (ANTIGRAVITY_CSRF_TOKEN, cloud/API keys).
func SanitizedEnv(vaultDir string, extra ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		keep := false
		for _, pref := range allowedEnvPrefixes {
			if strings.HasPrefix(kv, pref) {
				keep = true
				break
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	if strings.TrimSpace(vaultDir) != "" {
		hasVault := false
		for _, kv := range out {
			if strings.HasPrefix(kv, "HQ_VAULT_DIR=") {
				hasVault = true
				break
			}
		}
		if !hasVault {
			out = append(out, "HQ_VAULT_DIR="+vaultDir)
		}
	}
	out = append(out, extra...)
	return out
}

func sensitiveMaskDirs(vaultDir string) []string {
	var dirs []string
	if strings.TrimSpace(vaultDir) != "" {
		if absV, err := filepath.Abs(vaultDir); err == nil {
			dirs = append(dirs, filepath.Join(absV, "private"))
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, rel := range []string{
			".ssh",
			".gnupg",
			".aws",
			filepath.Join(".config", "gcloud"),
			filepath.Join(".config", "gh"),
		} {
			dirs = append(dirs, filepath.Join(home, rel))
		}
	}
	return dirs
}

// BuildBwrapArgs constructs deterministic Bubblewrap arguments masking local/private/ and host credential directories.
func BuildBwrapArgs(vaultDir, workDir string, networkIsolated bool, argv []string) []string {
	args := []string{
		"--die-with-parent",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	}
	if networkIsolated {
		args = append(args, "--unshare-net")
	}
	if strings.TrimSpace(workDir) != "" {
		if absW, err := filepath.Abs(workDir); err == nil {
			if st, sErr := os.Stat(absW); sErr == nil && st.IsDir() {
				args = append(args, "--bind", absW, absW)
			}
		}
	}
	if strings.TrimSpace(vaultDir) != "" {
		if absV, err := filepath.Abs(vaultDir); err == nil {
			for _, writableSub := range []string{"notes", "memory", "inbox"} {
				p := filepath.Join(absV, writableSub)
				if st, sErr := os.Stat(p); sErr == nil && st.IsDir() {
					args = append(args, "--bind", p, p)
				}
			}
		}
	}
	// Mount empty tmpfs over sensitive directories AFTER any broader binds so credentials are invisible.
	for _, maskDir := range sensitiveMaskDirs(vaultDir) {
		if st, err := os.Stat(maskDir); err == nil && st.IsDir() {
			args = append(args, "--tmpfs", maskDir)
		}
	}
	if strings.TrimSpace(workDir) != "" {
		if absW, err := filepath.Abs(workDir); err == nil {
			args = append(args, "--chdir", absW)
		}
	}
	args = append(args, "--")
	args = append(args, argv...)
	return args
}

// BuildSeatbeltProfile generates an inline macOS sandbox-exec SBPL profile denying reads/writes to credential paths.
func BuildSeatbeltProfile(vaultDir string, networkIsolated bool) string {
	var sb strings.Builder
	sb.WriteString("(version 1)\n(allow default)\n")
	if networkIsolated {
		sb.WriteString("(deny network*)\n")
	}
	for _, maskDir := range sensitiveMaskDirs(vaultDir) {
		escaped := strings.ReplaceAll(maskDir, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		sb.WriteString(fmt.Sprintf("(deny file-read* file-write* (subpath %q))\n", escaped))
	}
	return sb.String()
}

// BuildCommand wraps argv in the host's OS credential sandbox (when available and enabled) and applies SanitizedEnv.
// Returns (cmd, sandboxed, error).
func BuildCommand(ctx context.Context, vaultDir, workDir string, networkIsolated bool, argv []string) (*exec.Cmd, bool, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil, false, fmt.Errorf("sandbox BuildCommand requires at least 1 command argument")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := LoadConfig(vaultDir)
	cleanEnv := SanitizedEnv(vaultDir)

	if cfg.Mode == ModeOff {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		if workDir != "" {
			cmd.Dir = workDir
		}
		cmd.Env = cleanEnv
		return cmd, false, nil
	}

	cap := DetectCapability()
	if !cap.Available {
		if cfg.Mode == ModeRequired {
			return nil, false, fmt.Errorf("sandbox.mode=required, but OS credential sandbox is unavailable: %s", cap.Detail)
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		if workDir != "" {
			cmd.Dir = workDir
		}
		cmd.Env = cleanEnv
		return cmd, false, nil
	}

	switch cap.Backend {
	case "bwrap":
		bwrapArgs := BuildBwrapArgs(vaultDir, workDir, networkIsolated, argv)
		cmd := exec.CommandContext(ctx, cap.Binary, bwrapArgs...)
		if workDir != "" {
			cmd.Dir = workDir
		}
		cmd.Env = cleanEnv
		return cmd, true, nil
	case "sandbox-exec":
		profile := BuildSeatbeltProfile(vaultDir, networkIsolated)
		sbArgs := append([]string{"-p", profile}, argv...)
		cmd := exec.CommandContext(ctx, cap.Binary, sbArgs...)
		if workDir != "" {
			cmd.Dir = workDir
		}
		cmd.Env = cleanEnv
		return cmd, true, nil
	default:
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		if workDir != "" {
			cmd.Dir = workDir
		}
		cmd.Env = cleanEnv
		return cmd, false, nil
	}
}
