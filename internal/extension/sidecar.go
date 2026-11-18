package extension

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/store"
)

const defaultDaemonTargetAddr = "127.0.0.1:4242"

const fallbackPreloadJS = `(function() {
  const params = new URLSearchParams(window.location.search);
  const token = params.get('token') || '';
  window.addEventListener('message', (event) => {
    if (event.data?.type === 'theme-change' && event.data.payload?.cssVariables) {
      for (const [k, v] of Object.entries(event.data.payload.cssVariables)) {
        document.documentElement.style.setProperty(k, v);
      }
    }
  });
  window.sidecar = {
    token: token,
    fetch: (url, opts = {}) => {
      opts.headers = opts.headers || {};
      if (token && !opts.headers['x-sidecar-token']) opts.headers['x-sidecar-token'] = token;
      return fetch(url, opts);
    }
  };
})();`

// ConsoleSidecarOptions configures the pure-Go `hq sidecar console` server (`RFC-0017d` §5.3).
type ConsoleSidecarOptions struct {
	ListenAddr      string
	TargetAddr      string
	UIToken         string
	VaultDir        string
	SuperviseDaemon bool
}

// SanitizedDaemonEnv strips session-scoped Antigravity/sidecar variables before spawning `hq daemon`.
func SanitizedDaemonEnv(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	blocked := map[string]bool{
		"ANTIGRAVITY_PROJECT_ID":       true,
		"ANTIGRAVITY_CONVERSATION_ID":  true,
		"ANTIGRAVITY_TRAJECTORY_ID":    true,
		"ANTIGRAVITY_SOURCE_METADATA":  true,
		"ANTIGRAVITY_SIDECAR_WEB_PORT": true,
		"ANTIGRAVITY_SIDECAR_UI_TOKEN": true,
		"ANTIGRAVITY_CREDENTIAL_SCOPE": true,
		"HQ_MANAGED_DAEMON":            true,
		"PORT":                         true,
	}
	out := make([]string, 0, len(base))
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if ok && blocked[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func resolvePreloadJS(vaultDir string) string {
	roots := hostpaths.Resolve(vaultDir)
	candidates := []string{
		filepath.Join(roots.ConfigDir, "node_modules", "sidecar_sdk", "preload.js"),
		filepath.Join(roots.AppDataDir, "lib", "node_modules", "sidecar_sdk", "preload.js"),
	}
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return string(b)
		}
	}
	return fallbackPreloadJS
}

func mintSessionSecret() string {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

func ensureDaemonAlive(targetAddr string) {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + targetAddr + "/healthz")
	if err == nil {
		_ = resp.Body.Close()
		return
	}
	exe := ResolveSelfHQExecutable()
	cmd := exec.Command(exe, "daemon", "--addr="+targetAddr)
	cmd.Env = SanitizedDaemonEnv(os.Environ())
	store.SetDetachedSession(cmd)
	devNull, oErr := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if oErr == nil {
		cmd.Stdin = devNull
		cmd.Stdout = devNull
		cmd.Stderr = devNull
		defer devNull.Close()
	}
	if sErr := cmd.Start(); sErr == nil && cmd.Process != nil {
		_ = cmd.Process.Release()
	}
}

// NewConsoleSidecarHandler builds the authenticated reverse-proxy HTTP handler for `hq sidecar console`.
func NewConsoleSidecarHandler(opts ConsoleSidecarOptions) (http.Handler, error) {
	targetAddr := strings.TrimSpace(opts.TargetAddr)
	if targetAddr == "" {
		targetAddr = strings.TrimSpace(os.Getenv("HQ_DAEMON_ADDR"))
	}
	if targetAddr == "" {
		targetAddr = defaultDaemonTargetAddr
	}
	targetURL, err := url.Parse("http://" + targetAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid target daemon address %q: %w", targetAddr, err)
	}

	preload := resolvePreloadJS(opts.VaultDir)
	uiToken := opts.UIToken
	sessionSecret := mintSessionSecret()

	rp := httputil.NewSingleHostReverseProxy(targetURL)
	origDirector := rp.Director
	rp.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = targetAddr
	}
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><body style="background:#0b0f19;color:#e6edf3;font-family:sans-serif;padding:24px;">
      <h2>Connecting to agy-hq daemon...</h2>
      <p style="color:#8b949e">Daemon is launching on %s. Connecting...</p>
      <script>setTimeout(() => location.reload(), 1500);</script>
    </body></html>`, targetAddr)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/preload.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(preload))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		providedTok := r.Header.Get("X-Sidecar-Token")
		if providedTok == "" {
			providedTok = r.URL.Query().Get("token")
		}
		tokOK := uiToken == "" || (len(providedTok) == len(uiToken) && subtle.ConstantTimeCompare([]byte(providedTok), []byte(uiToken)) == 1)

		cookieOK := false
		if c, cErr := r.Cookie("hq_sidecar_session"); cErr == nil && len(c.Value) == len(sessionSecret) {
			cookieOK = subtle.ConstantTimeCompare([]byte(c.Value), []byte(sessionSecret)) == 1
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if uiToken != "" && !tokOK && !cookieOK {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
		} else if tokOK {
			http.SetCookie(w, &http.Cookie{
				Name:     "hq_sidecar_session",
				Value:    sessionSecret,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
		}
		rp.ServeHTTP(w, r)
	})
	return mux, nil
}

// RunConsoleSidecar starts the pure-Go `hq sidecar console` HTTP proxy and optional daemon supervisor.
func RunConsoleSidecar(ctx context.Context, opts ConsoleSidecarOptions) error {
	targetAddr := strings.TrimSpace(opts.TargetAddr)
	if targetAddr == "" {
		targetAddr = strings.TrimSpace(os.Getenv("HQ_DAEMON_ADDR"))
	}
	if targetAddr == "" {
		targetAddr = defaultDaemonTargetAddr
	}
	opts.TargetAddr = targetAddr
	if opts.UIToken == "" {
		opts.UIToken = os.Getenv("ANTIGRAVITY_SIDECAR_UI_TOKEN")
	}

	if opts.SuperviseDaemon {
		ensureDaemonAlive(targetAddr)
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					ensureDaemonAlive(targetAddr)
				}
			}
		}()
	}

	listenAddr := strings.TrimSpace(opts.ListenAddr)
	if listenAddr == "" {
		rawPort := strings.TrimSpace(os.Getenv("ANTIGRAVITY_SIDECAR_WEB_PORT"))
		if rawPort == "" {
			rawPort = strings.TrimSpace(os.Getenv("PORT"))
		}
		if rawPort != "" {
			p, err := strconv.Atoi(rawPort)
			if err != nil || p <= 0 || p > 65535 {
				return fmt.Errorf("invalid sidecar port %q", rawPort)
			}
			listenAddr = fmt.Sprintf("127.0.0.1:%d", p)
		}
	}

	if listenAddr == "" || listenAddr == targetAddr {
		fmt.Printf("[sidecar] agy-hq-console initialized (daemon target %s)\n", targetAddr)
		<-ctx.Done()
		return nil
	}

	handler, err := NewConsoleSidecarHandler(opts)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	fmt.Printf("[sidecar] agy-hq-console listening on %s (proxying to %s)\n", ln.Addr().String(), targetAddr)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
