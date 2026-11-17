package connector

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zjshen14/agy-hq/internal/store"
)

const (
	MaxSubprocessStdoutBytes = 8 * 1024 * 1024 // 8 MiB
	MaxSubprocessStderrBytes = 64 * 1024       // 64 KiB
	DefaultMaxWorkers        = 4
	DefaultSubprocessTimeout = 10 * time.Second
)

var (
	rePluginCardPair = regexp.MustCompile(`(?is)<plugin-card\b[^>]*>.*?</plugin-card>`)
	rePluginCardSelf = regexp.MustCompile(`(?is)<plugin-card\b[^>]*/?>`)
)

var safeBaseEnvKeys = map[string]bool{
	"PATH":                       true,
	"HOME":                       true,
	"USER":                       true,
	"LOGNAME":                    true,
	"LANG":                       true,
	"LC_ALL":                     true,
	"TZ":                         true,
	"TMPDIR":                     true,
	"SSL_CERT_FILE":              true,
	"SSL_CERT_DIR":               true,
	"SYSTEMROOT":                 true,
	"XDG_CONFIG_HOME":            true,
	"HQ_ROOT_DIR":                true,
	"HQ_VAULT_DIR":               true,
	"HQ_HOME_DIR":                true,
	"HQ_GEMINI_DIR":              true,
	"HQ_CONFIG_DIR":              true,
	"HQ_APP_DATA_DIR":            true,
	"HQ_BRAIN_DIR":               true,
	"HQ_GMAIL_MOCK_DIR":          true,
	"HQ_GCHAT_MOCK_DIR":          true,
	"HQ_GMAIL_API_BASE":          true,
	"HQ_GCHAT_API_BASE":          true,
	"ANTIGRAVITY_APP_DATA_DIR":   true,
	"ANTIGRAVITY_GEMINI_DIR":     true,
	"ANTIGRAVITY_CONFIG_REL_DIR": true,
	"HQ_PLUGIN_CLI_HOST":         true,
}

var unconditionallyBlockedEnv = map[string]bool{
	"ANTIGRAVITY_CSRF_TOKEN":      true,
	"ANTIGRAVITY_LS_ADDRESS":      true,
	"ANTIGRAVITY_AGENTAPI_EXE":    true,
	"ANTIGRAVITY_CONVERSATION_ID": true,
	"ANTIGRAVITY_TRAJECTORY_ID":   true,
	"ANTIGRAVITY_PROJECT_ID":      true,
	"ANTIGRAVITY_SOURCE_METADATA": true,
}

// SanitizedSubprocessEnv builds a strict allowlisted environment for connector/precheck subprocesses:
// - Unconditionally strips ANTIGRAVITY_CSRF_TOKEN and ANTIGRAVITY_LS_ADDRESS (preventing Front Desk turn forgery).
// - Sets ANTIGRAVITY_PLUGIN_SHIM_DIR=/nonexistent-hq-sentinel unless allowDesktopScope is explicitly enabled.
func SanitizedSubprocessEnv(base, envAllow []string, extra map[string]string, allowDesktopScope bool) []string {
	if base == nil {
		base = os.Environ()
	}
	allowSet := make(map[string]bool, len(envAllow))
	for _, k := range envAllow {
		k = strings.TrimSpace(k)
		if k != "" && !unconditionallyBlockedEnv[k] {
			allowSet[k] = true
		}
	}

	desktopAllowed := allowDesktopScope && (allowSet["AGY_ACCOUNT"] || allowSet["ANTIGRAVITY_CREDENTIAL_SCOPE"])
	envMap := make(map[string]string)
	var keyOrder []string
	setKV := func(k, v string) {
		if unconditionallyBlockedEnv[k] {
			return
		}
		if _, exists := envMap[k]; !exists {
			keyOrder = append(keyOrder, k)
		}
		envMap[k] = v
	}

	var origShimDir string
	for _, kv := range base {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || unconditionallyBlockedEnv[k] {
			continue
		}
		if k == "ANTIGRAVITY_PLUGIN_SHIM_DIR" {
			origShimDir = strings.TrimSpace(v)
			if desktopAllowed && origShimDir != "" {
				if info, err := os.Stat(origShimDir); err == nil && info.IsDir() {
					setKV(k, origShimDir)
				}
			}
			continue
		}
		if k == "ANTIGRAVITY_CREDENTIAL_SCOPE" {
			if desktopAllowed {
				setKV(k, v)
			}
			continue
		}
		if safeBaseEnvKeys[k] || allowSet[k] {
			setKV(k, v)
		}
	}

	const shimSentinel = "/nonexistent-hq-sentinel"
	for k, v := range extra {
		k = strings.TrimSpace(k)
		if k == "" || unconditionallyBlockedEnv[k] {
			continue
		}
		if k == "ANTIGRAVITY_PLUGIN_SHIM_DIR" {
			v = strings.TrimSpace(v)
			if !desktopAllowed || v == "" || v == shimSentinel {
				continue
			}
			info, err := os.Stat(v)
			if err != nil || !info.IsDir() {
				continue
			}
		} else if k == "ANTIGRAVITY_CREDENTIAL_SCOPE" {
			v = strings.TrimSpace(v)
			if !desktopAllowed || v == "" {
				continue
			}
		}
		setKV(k, v)
	}

	if !desktopAllowed {
		delete(envMap, "ANTIGRAVITY_CREDENTIAL_SCOPE")
		setKV("ANTIGRAVITY_PLUGIN_SHIM_DIR", shimSentinel)
	} else if strings.TrimSpace(envMap["ANTIGRAVITY_PLUGIN_SHIM_DIR"]) == "" {
		setKV("ANTIGRAVITY_PLUGIN_SHIM_DIR", shimSentinel)
	}

	if envMap["ANTIGRAVITY_PLUGIN_SHIM_DIR"] == shimSentinel {
		if pathVal, ok := envMap["PATH"]; ok && pathVal != "" {
			normOrig := strings.TrimRight(origShimDir, `/\`)
			parts := strings.Split(pathVal, string(os.PathListSeparator))
			filtered := make([]string, 0, len(parts))
			for _, part := range parts {
				normPart := strings.TrimRight(strings.TrimSpace(part), `/\`)
				if normPart == "" {
					continue
				}
				if (normOrig != "" && normPart == normOrig) ||
					strings.Contains(normPart, "/agy-plugin-cli-") ||
					strings.Contains(normPart, `\agy-plugin-cli-`) {
					continue
				}
				filtered = append(filtered, part)
			}
			setKV("PATH", strings.Join(filtered, string(os.PathListSeparator)))
		}
	}

	if desktopAllowed {
		appDataDir := strings.TrimSpace(envMap["ANTIGRAVITY_APP_DATA_DIR"])
		if appDataDir == "" {
			if h := strings.TrimSpace(envMap["HOME"]); h != "" {
				appDataDir = filepath.Join(h, ".gemini", "antigravity")
			}
		}
		if appDataDir != "" {
			appBin := filepath.Clean(filepath.Join(appDataDir, "bin"))
			if st, err := os.Stat(appBin); err == nil && st.IsDir() {
				curPath := envMap["PATH"]
				hasAppBin := false
				for _, p := range strings.Split(curPath, string(os.PathListSeparator)) {
					if filepath.Clean(strings.TrimSpace(p)) == appBin {
						hasAppBin = true
						break
					}
				}
				if !hasAppBin {
					if curPath == "" {
						setKV("PATH", appBin)
					} else {
						setKV("PATH", appBin+string(os.PathListSeparator)+curPath)
					}
				}
			}
		}
	}

	out := make([]string, 0, len(keyOrder))
	for _, k := range keyOrder {
		if v, ok := envMap[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// SanitizeStderr strips `<plugin-card>` UI tags and control characters from subprocess stderr before logging.
func SanitizeStderr(raw string) string {
	cleaned := rePluginCardPair.ReplaceAllString(raw, "")
	cleaned = rePluginCardSelf.ReplaceAllString(cleaned, "")
	cleaned = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, cleaned)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if runes := []rune(cleaned); len(runes) > 400 {
		cleaned = string(runes[:400]) + "..."
	}
	return cleaned
}

// SubprocessSpec configures a single sandboxed connector subprocess execution.
type SubprocessSpec struct {
	Binary            string
	Args              []string
	WorkDir           string
	EnvAllow          []string
	ExtraEnv          map[string]string
	AllowDesktopScope bool
	StdinBytes        []byte
	PayloadBytes      []byte // passed via /dev/fd/3 ExtraFiles when non-nil
	Timeout           time.Duration
}

type limitedBuffer struct {
	buf       bytes.Buffer
	maxBytes  int
	truncated bool
}

func (lb *limitedBuffer) Write(p []byte) (int, error) {
	origLen := len(p)
	rem := lb.maxBytes - lb.buf.Len()
	if rem <= 0 {
		lb.truncated = true
		return origLen, nil
	}
	if len(p) > rem {
		lb.truncated = true
		p = p[:rem]
	}
	_, _ = lb.buf.Write(p)
	return origLen, nil
}

// RunSubprocess executes a connector binary in its own process group (`Setpgid: true`),
// killing `-pid` (`SIGKILL`) on timeout, enforcing an 8 MiB stdout cap, and piping `PayloadBytes` via `/dev/fd/3`.
func RunSubprocess(parentCtx context.Context, spec SubprocessSpec) ([]byte, string, error) {
	if strings.TrimSpace(spec.Binary) == "" {
		return nil, "", fmt.Errorf("subprocess binary is required")
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultSubprocessTimeout
	}
	effectiveTimeout := timeout
	if dl, hasDL := parentCtx.Deadline(); hasDL {
		if rem := time.Until(dl); rem > 0 && rem < effectiveTimeout {
			effectiveTimeout = rem.Round(10 * time.Millisecond)
			if effectiveTimeout <= 0 {
				effectiveTimeout = rem
			}
		}
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, spec.Binary, spec.Args...)
	if strings.TrimSpace(spec.WorkDir) != "" {
		cmd.Dir = spec.WorkDir
	}
	cmd.Env = SanitizedSubprocessEnv(os.Environ(), spec.EnvAllow, spec.ExtraEnv, spec.AllowDesktopScope)
	store.SetProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			store.KillProcessGroup(cmd.Process.Pid)
			return cmd.Process.Kill()
		}
		return nil
	}

	if len(spec.StdinBytes) > 0 {
		cmd.Stdin = bytes.NewReader(spec.StdinBytes)
	}

	var pipeReader, pipeWriter *os.File
	if spec.PayloadBytes != nil {
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, "", fmt.Errorf("open /dev/fd/3 payload pipe: %w", err)
		}
		pipeReader, pipeWriter = pr, pw
		cmd.ExtraFiles = []*os.File{pipeReader}
	}

	stdoutBuf := &limitedBuffer{maxBytes: MaxSubprocessStdoutBytes}
	stderrBuf := &limitedBuffer{maxBytes: MaxSubprocessStderrBytes}
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		if pipeReader != nil {
			_ = pipeReader.Close()
			_ = pipeWriter.Close()
		}
		return nil, "", err
	}

	if pipeReader != nil {
		_ = pipeReader.Close()
		payload := spec.PayloadBytes
		go func(w *os.File, data []byte) {
			_, _ = io.Copy(w, bytes.NewReader(data))
			_ = w.Close()
		}(pipeWriter, payload)
	}

	waitErr := cmd.Wait()
	sanitizedErr := SanitizeStderr(stderrBuf.buf.String())
	if stdoutBuf.truncated {
		return nil, sanitizedErr, fmt.Errorf("subprocess stdout exceeded %d-byte limit", MaxSubprocessStdoutBytes)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return stdoutBuf.buf.Bytes(), sanitizedErr, fmt.Errorf("subprocess timed out after %s (%s)", effectiveTimeout, sanitizedErr)
	}
	if waitErr != nil {
		if sanitizedErr != "" {
			return stdoutBuf.buf.Bytes(), sanitizedErr, fmt.Errorf("%w: %s", waitErr, sanitizedErr)
		}
		return stdoutBuf.buf.Bytes(), sanitizedErr, waitErr
	}
	return stdoutBuf.buf.Bytes(), sanitizedErr, nil
}

// RunPool executes jobs concurrently with at most maxWorkers goroutines and returns results in input order.
func RunPool[T any](ctx context.Context, maxWorkers int, jobs []func(context.Context) T) []T {
	if len(jobs) == 0 {
		return nil
	}
	if maxWorkers <= 0 {
		maxWorkers = DefaultMaxWorkers
	}
	if maxWorkers > len(jobs) {
		maxWorkers = len(jobs)
	}
	results := make([]T, len(jobs))
	ch := make(chan int, len(jobs))
	for i := range jobs {
		ch <- i
	}
	close(ch)

	var wg sync.WaitGroup
	for w := 0; w < maxWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range ch {
				results[idx] = jobs[idx](ctx)
			}
		}()
	}
	wg.Wait()
	return results
}
