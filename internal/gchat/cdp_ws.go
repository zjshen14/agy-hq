package gchat

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HostResolverJailRule restricts headless Chrome DNS resolution strictly to Google domains,
// blocking third-party network egress / image beacon exfiltration at the browser network layer.
const HostResolverJailRule = "MAP * 127.0.0.1, EXCLUDE chat.google.com, EXCLUDE *.chat.google.com, EXCLUDE mail.google.com, EXCLUDE *.google.com, EXCLUDE google.com, EXCLUDE *.gstatic.com, EXCLUDE gstatic.com, EXCLUDE *.googleapis.com, EXCLUDE googleapis.com, EXCLUDE *.googleusercontent.com"

var blockedNonChatGoogleURLs = []string{
	"*://myaccount.google.com/*",
	"*://drive.google.com/*",
	"*://docs.google.com/*",
	"*://pay.google.com/*",
	"*://takeout.google.com/*",
	"*://myactivity.google.com/*",
	"*://passwords.google.com/*",
	"*://contacts.google.com/*",
}

type cdpTarget struct {
	ID                   string `json:"id"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	Type                 string `json:"type"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// isAuthenticatedChatURL returns true ONLY when rawURL's host is actually chat.google.com
// or mail.google.com/chat (never matching accounts.google.com/?continue=https://chat.google.com).
func isAuthenticatedChatURL(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "chat.google.com" {
		return true
	}
	if host == "mail.google.com" && (strings.HasPrefix(u.Path, "/chat") || strings.Contains(u.Fragment, "chat/")) {
		return true
	}
	return false
}

// isGoogleLoginURL returns true when Chrome was redirected to Google Accounts sign-in
// or the unauthenticated Google Workspace Chat marketing landing page.
func isGoogleLoginURL(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "accounts.google.com" {
		return true
	}
	if host == "workspace.google.com" && strings.Contains(u.Path, "/products/chat") {
		return true
	}
	return false
}

func fetchAllCDPTargets(cdpEndpoint string) ([]cdpTarget, error) {
	base := strings.TrimRight(cdpEndpoint, "/")
	if base == "" {
		base = DefaultCDPEndpoint
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/json/list")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CDP /json/list HTTP %d", resp.StatusCode)
	}
	var targets []cdpTarget
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, err
	}
	return targets, nil
}

func fetchCDPTargets(cdpEndpoint string) ([]cdpTarget, error) {
	targets, err := fetchAllCDPTargets(cdpEndpoint)
	if err != nil {
		return nil, err
	}
	var chatTargets []cdpTarget
	for _, t := range targets {
		if isAuthenticatedChatURL(t.URL) && t.WebSocketDebuggerURL != "" {
			chatTargets = append(chatTargets, t)
		}
	}
	return chatTargets, nil
}

// resolveActiveCDPEndpoint checks the configured endpoint first, then falls back to reading
// Chrome's DevToolsActivePort file inside local/private/gchat/chrome-profile.
func resolveActiveCDPEndpoint(cfg AccountConfig) string {
	ep := strings.TrimSpace(cfg.CDPEndpoint)
	if ep == "" {
		ep = DefaultCDPEndpoint
	}
	if ok, _ := CheckCDPEndpoint(ep); ok {
		return ep
	}
	if cfg.ChromeProfileDir != "" {
		activePortFile := filepath.Join(cfg.ChromeProfileDir, "DevToolsActivePort")
		if data, err := os.ReadFile(activePortFile); err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) > 0 && strings.TrimSpace(lines[0]) != "" {
				cand := "http://127.0.0.1:" + strings.TrimSpace(lines[0])
				if ok, _ := CheckCDPEndpoint(cand); ok {
					return cand
				}
			}
		}
	}
	return ep
}

func hasSavedProfileCookies(profileDir string) bool {
	if strings.TrimSpace(profileDir) == "" {
		return false
	}
	for _, rel := range []string{
		filepath.Join("Default", "Cookies"),
		filepath.Join("Default", "Network", "Cookies"),
	} {
		if info, err := os.Stat(filepath.Join(profileDir, rel)); err == nil && info.Size() > 0 {
			return true
		}
	}
	return false
}

func closeCDPBrowser(cdpEndpoint string) {
	targets, err := fetchAllCDPTargets(cdpEndpoint)
	if err != nil {
		return
	}
	for _, t := range targets {
		if t.WebSocketDebuggerURL != "" {
			_, _ = cdpCall(t.WebSocketDebuggerURL, "Browser.close", map[string]interface{}{})
			time.Sleep(300 * time.Millisecond)
			return
		}
	}
}

func applyCDPNetworkJail(wsURL string) {
	_, _ = cdpCall(wsURL, "Network.enable", map[string]interface{}{})
	_, _ = cdpCall(wsURL, "Network.setBlockedURLs", map[string]interface{}{
		"urls": blockedNonChatGoogleURLs,
	})
}

// EnsureCDPSession ensures that Chrome CDP is running and authenticated at chat.google.com.
// - If saved cookies exist in local/private/gchat/chrome-profile, it auto-starts Chrome in
//   invisible `--headless=new` mode (0 popup windows).
// - If cookies are missing or expired (redirected to accounts.google.com) and allowInteractiveLogin
//   is true, it runs the guided InteractiveLogin flow (visible sign-in -> cache spaces -> switch to headless).
func EnsureCDPSession(vaultDir, accountAlias string, allowInteractiveLogin bool) (string, error) {
	cfg, err := ResolveAccount(vaultDir, accountAlias)
	if err != nil {
		return "", err
	}
	if mockDir(vaultDir) != "" || cfg.Transport != "cdp" {
		return cfg.CDPEndpoint, nil
	}
	// If a custom non-default CDP endpoint is configured (e.g. unit test httptest server), do not spawn Chrome.
	if cfg.CDPEndpoint != "" && cfg.CDPEndpoint != DefaultCDPEndpoint {
		return cfg.CDPEndpoint, nil
	}

	ep := resolveActiveCDPEndpoint(cfg)
	ok, _ := CheckCDPEndpoint(ep)
	if !ok {
		if hasSavedProfileCookies(cfg.ChromeProfileDir) {
			if _, _, err := LaunchChromeProfile(vaultDir, cfg.Alias, true); err == nil {
				for i := 0; i < 20; i++ {
					time.Sleep(250 * time.Millisecond)
					ep = resolveActiveCDPEndpoint(cfg)
					if ok, _ = CheckCDPEndpoint(ep); ok {
						break
					}
				}
			}
		} else if allowInteractiveLogin {
			return InteractiveLogin(vaultDir, cfg.Alias, 120*time.Second)
		} else {
			return "", fmt.Errorf("Google Chat profile is not logged in yet; run `./bin/hq gchat login` once")
		}
	}

	// Wait briefly for initial navigation to settle and verify auth state
	var chatTargets []cdpTarget
	needsLogin := false
	for i := 0; i < 8; i++ {
		all, err := fetchAllCDPTargets(ep)
		if err != nil {
			break
		}
		chatTargets = nil
		loginTabs := 0
		for _, t := range all {
			if isAuthenticatedChatURL(t.URL) && t.WebSocketDebuggerURL != "" {
				chatTargets = append(chatTargets, t)
			} else if isGoogleLoginURL(t.URL) {
				loginTabs++
			}
		}
		if len(chatTargets) > 0 {
			for _, ct := range chatTargets {
				if ct.Type == "page" {
					applyCDPNetworkJail(ct.WebSocketDebuggerURL)
				}
			}
			return ep, nil
		}
		if loginTabs > 0 && i >= 2 {
			needsLogin = true
			break
		}
		time.Sleep(350 * time.Millisecond)
	}

	if needsLogin {
		if allowInteractiveLogin {
			return InteractiveLogin(vaultDir, cfg.Alias, 120*time.Second)
		}
		return "", fmt.Errorf("Google Chat session on %s requires sign-in (redirected to accounts.google.com); run `./bin/hq gchat login`", ep)
	}
	return ep, nil
}

// LoginOptions configures InteractiveLoginWithOptions.
type LoginOptions struct {
	Reset          bool
	RemoteHeadless bool
	ScreenshotPath string
	Timeout        time.Duration
}

// LogoutOrResetProfile closes the active Chrome CDP session, backs up existing session cookies
// to Cookies.bak inside local/private/gchat/chrome-profile, and removes the active Cookies file
// so the user can test a fresh first-time login (and restore via RestoreProfileCookies if desired).
func LogoutOrResetProfile(vaultDir, accountAlias string) (string, error) {
	cfg, err := ResolveAccount(vaultDir, accountAlias)
	if err != nil {
		return "", err
	}
	ep := resolveActiveCDPEndpoint(cfg)
	if ok, _ := CheckCDPEndpoint(ep); ok {
		closeCDPBrowser(ep)
		time.Sleep(500 * time.Millisecond)
	}
	var backedUp []string
	for _, rel := range []string{
		filepath.Join("Default", "Cookies"),
		filepath.Join("Default", "Network", "Cookies"),
	} {
		p := filepath.Join(cfg.ChromeProfileDir, rel)
		if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
			bak := p + ".bak"
			if st, bErr := os.Stat(bak); bErr != nil || int64(len(data)) >= st.Size() {
				_ = os.WriteFile(bak, data, 0o600)
			}
			_ = os.Remove(p)
			_ = os.Remove(p + "-journal")
			backedUp = append(backedUp, rel)
		}
	}
	if len(backedUp) == 0 {
		return fmt.Sprintf("Closed Chrome CDP session for %q (profile was already logged out)", cfg.Alias), nil
	}
	return fmt.Sprintf("Logged out Google Chat profile %q (backed up %s to *.bak; run `./bin/hq gchat login` to test login or `./bin/hq gchat restore-session` to restore)", cfg.Alias, strings.Join(backedUp, ", ")), nil
}

// RestoreProfileCookies restores Cookies.bak in local/private/gchat/chrome-profile and restarts headless Chrome.
func RestoreProfileCookies(vaultDir, accountAlias string) (string, error) {
	cfg, err := ResolveAccount(vaultDir, accountAlias)
	if err != nil {
		return "", err
	}
	ep := resolveActiveCDPEndpoint(cfg)
	if ok, _ := CheckCDPEndpoint(ep); ok {
		closeCDPBrowser(ep)
		time.Sleep(500 * time.Millisecond)
	}
	restored := 0
	for _, rel := range []string{
		filepath.Join("Default", "Cookies"),
		filepath.Join("Default", "Network", "Cookies"),
	} {
		p := filepath.Join(cfg.ChromeProfileDir, rel)
		bak := p + ".bak"
		if data, err := os.ReadFile(bak); err == nil && len(data) > 0 {
			if err := os.WriteFile(p, data, 0o600); err == nil {
				restored++
			}
		}
	}
	if restored == 0 {
		return "", fmt.Errorf("no Cookies.bak backup found in %s", cfg.ChromeProfileDir)
	}
	newEp, err := EnsureCDPSession(vaultDir, cfg.Alias, false)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Restored Google Chat session cookies for %q and restarted headless Chrome on %s", cfg.Alias, newEp), nil
}

func captureCDPScreenshot(wsURL, destPath string) error {
	if strings.TrimSpace(destPath) == "" {
		return nil
	}
	raw, err := cdpCall(wsURL, "Page.captureScreenshot", map[string]interface{}{
		"format": "png",
	})
	if err != nil {
		return err
	}
	var resp struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Data == "" {
		return fmt.Errorf("empty screenshot data")
	}
	pngBytes, err := base64.StdEncoding.DecodeString(resp.Data)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(destPath, pngBytes, 0o644)
}

func assistGoogleLoginStep(wsURL, email, vaultDir, screenshotPath string, emailSubmitted, passSubmitted *bool, lastChallenge *string) {
	if !*emailSubmitted && strings.TrimSpace(email) != "" {
		emailJSON, _ := json.Marshal(strings.TrimSpace(email))
		jsFillEmail := fmt.Sprintf(`(() => {
  const input = document.querySelector('input[type="email"], input#identifierId');
  if (!input || input.offsetParent === null) return false;
  if (!input.value) {
    input.focus();
    input.value = %s;
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.dispatchEvent(new Event('change', { bubbles: true }));
  }
  const nextBtn = document.querySelector('#identifierNext button, #identifierNext');
  if (nextBtn) nextBtn.click();
  return true;
})()`, string(emailJSON))
		if raw, err := cdpEvalJS(wsURL, jsFillEmail); err == nil && string(raw) == "true" {
			*emailSubmitted = true
			fmt.Printf("  -> Auto-filled Google account email (%s) and clicked Next\n", email)
			return
		}
	}

	if !*passSubmitted {
		passFile := filepath.Join(vaultDir, "private", "gchat", ".pass")
		if passBytes, err := os.ReadFile(passFile); err == nil {
			pass := strings.TrimSpace(string(passBytes))
			if pass != "" {
				passJSON, _ := json.Marshal(pass)
				jsFillPass := fmt.Sprintf(`(() => {
  const input = document.querySelector('input[type="password"][name="Passwd"], input[type="password"]');
  if (!input || input.offsetParent === null) return false;
  input.focus();
  input.value = %s;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  input.dispatchEvent(new Event('change', { bubbles: true }));
  const nextBtn = document.querySelector('#passwordNext button, #passwordNext');
  if (nextBtn) nextBtn.click();
  return true;
})()`, string(passJSON))
				if raw, err := cdpEvalJS(wsURL, jsFillPass); err == nil && string(raw) == "true" {
					*passSubmitted = true
					fmt.Println("  -> Auto-submitted password from local/private/gchat/.pass (0 LLM tokens)")
					return
				}
			}
		}
	}

	const jsInspectChallenge = `(() => {
  const url = window.location.href;
  const heading = (document.querySelector('h1, #headingText') || {}).innerText || '';
  const samp = (document.querySelector('samp, [data-challenge-number], .vxx8jf') || {}).innerText || '';
  const hasPass = !!document.querySelector('input[type="password"]')?.offsetParent;
  const bodyText = (document.body ? document.body.innerText : '').split('\n').map(s => s.trim()).filter(Boolean).slice(0, 12).join(' | ');
  return { url, heading: heading.trim(), number: samp.trim(), hasPass, summary: bodyText.slice(0, 240) };
})()`
	if raw, err := cdpEvalJS(wsURL, jsInspectChallenge); err == nil && len(raw) > 0 {
		var info struct {
			URL     string `json:"url"`
			Heading string `json:"heading"`
			Number  string `json:"number"`
			HasPass bool   `json:"hasPass"`
			Summary string `json:"summary"`
		}
		if json.Unmarshal(raw, &info) == nil && info.Heading != "" {
			sig := info.Heading + "|" + info.Number + "|" + fmt.Sprintf("%v", info.HasPass)
			if sig != *lastChallenge {
				*lastChallenge = sig
				if info.Number != "" {
					fmt.Printf("  📱 GOOGLE PROMPT ON PHONE: %s — Tap \"Yes\" and select number [%s]\n", info.Heading, info.Number)
				} else if info.HasPass {
					fmt.Printf("  🔑 Google Sign-In Step: %s (complete in window, or write password to local/private/gchat/.pass with 0600 perms for headless auto-fill)\n", info.Heading)
				} else {
					fmt.Printf("  🔐 Google Sign-In Step: %s (%s)\n", info.Heading, info.Summary)
				}
				_ = captureCDPScreenshot(wsURL, screenshotPath)
			}
		}
	}
}

// InteractiveLogin opens a Chrome window to Google Sign-In, polls until chat.google.com loads,
// caches the user's space list, closes the visible window, and restarts in background `--headless=new` mode.
func InteractiveLogin(vaultDir, accountAlias string, timeout time.Duration) (string, error) {
	return InteractiveLoginWithOptions(vaultDir, accountAlias, LoginOptions{Timeout: timeout})
}

// InteractiveLoginWithOptions performs a guided Google Chat sign-in with optional cookie reset,
// remote headless mode (with email auto-fill, Google Prompt 2-digit number readout, and live PNG screenshot),
// and automatic transition into `--headless=new` background mode.
func InteractiveLoginWithOptions(vaultDir, accountAlias string, opts LoginOptions) (string, error) {
	cfg, err := ResolveAccount(vaultDir, accountAlias)
	if err != nil {
		return "", err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 120 * time.Second
	}
	if opts.ScreenshotPath == "" {
		opts.ScreenshotPath = filepath.Join(vaultDir, "memory", "gchat", "login-status.png")
	}
	if opts.Reset {
		msg, err := LogoutOrResetProfile(vaultDir, cfg.Alias)
		if err != nil {
			return "", err
		}
		fmt.Println(msg)
	}

	ep := resolveActiveCDPEndpoint(cfg)
	if ok, _ := CheckCDPEndpoint(ep); ok {
		if !opts.Reset {
			if chatTargets, err := fetchCDPTargets(ep); err == nil && len(chatTargets) > 0 {
				return ep, nil
			}
		}
		closeCDPBrowser(ep)
		time.Sleep(400 * time.Millisecond)
	}

	loginURL := "https://accounts.google.com/ServiceLogin?Email=" + url.QueryEscape(cfg.Email) + "&continue=https://chat.google.com/u/0/app/home"
	if _, _, err := launchChromeWithURL(cfg, opts.RemoteHeadless, loginURL); err != nil {
		return "", err
	}

	deadline := time.Now().Add(opts.Timeout)
	var authedTargets []cdpTarget
	emailSubmitted := false
	passSubmitted := false
	lastChallenge := ""
	for time.Now().Before(deadline) {
		time.Sleep(1 * time.Second)
		ep = resolveActiveCDPEndpoint(cfg)
		targets, err := fetchCDPTargets(ep)
		if err == nil && len(targets) > 0 {
			authedTargets = targets
			break
		}
		if allTargets, err := fetchAllCDPTargets(ep); err == nil {
			for _, t := range allTargets {
				if t.Type == "page" && isGoogleLoginURL(t.URL) && t.WebSocketDebuggerURL != "" {
					assistGoogleLoginStep(t.WebSocketDebuggerURL, cfg.Email, vaultDir, opts.ScreenshotPath, &emailSubmitted, &passSubmitted, &lastChallenge)
					break
				}
			}
		}
	}
	if len(authedTargets) == 0 {
		return "", fmt.Errorf("timed out after %s waiting for Google Chat sign-in in Chrome (screenshot saved to %s; run `./bin/hq gchat restore-session` to restore previous cookies)", opts.Timeout, opts.ScreenshotPath)
	}

	// Let sidebar render and cache spaces before switching to background headless mode
	time.Sleep(2 * time.Second)
	if spaces, err := evalSpacesViaWebSocket(authedTargets, cfg.Alias, "", 50); err == nil && len(spaces) > 0 {
		_ = saveCachedSpaces(vaultDir, cfg.Alias, spaces)
	}

	if !opts.RemoteHeadless {
		// Close visible login window so cookies flush to disk, then restart silently in --headless=new mode
		closeCDPBrowser(ep)
		time.Sleep(500 * time.Millisecond)
		if _, _, err := LaunchChromeProfile(vaultDir, cfg.Alias, true); err == nil {
			for i := 0; i < 20; i++ {
				time.Sleep(250 * time.Millisecond)
				ep = resolveActiveCDPEndpoint(cfg)
				if ok, _ := CheckCDPEndpoint(ep); ok {
					break
				}
			}
		}
	}
	return ep, nil
}

// PreWarmSpace ensures the Google Chat CDP session is authenticated (launching guided login if logged out)
// and navigates the background headless Chrome tab to spaceID so that when the user taps
// Approve on the 1-tap prompt seconds later, the composer box is already loaded and ready.
func PreWarmSpace(vaultDir string, cfg AccountConfig, spaceID string) error {
	if mockDir(vaultDir) != "" || cfg.Transport != "cdp" {
		return nil
	}
	if cfg.CDPEndpoint != "" && cfg.CDPEndpoint != DefaultCDPEndpoint {
		return nil
	}
	ep, err := EnsureCDPSession(vaultDir, cfg.Alias, true)
	if err != nil {
		return err
	}
	targets, err := fetchCDPTargets(ep)
	if err != nil || len(targets) == 0 {
		return nil
	}
	cleanSpaceID := strings.TrimPrefix(strings.TrimSpace(spaceID), "spaces/")
	cleanSpaceID = strings.TrimPrefix(cleanSpaceID, "space/")
	cleanSpaceID = strings.TrimPrefix(cleanSpaceID, "dm/")
	if cleanSpaceID == "" || strings.HasPrefix(cleanSpaceID, "gmail-chat-") {
		return nil
	}
	for _, t := range targets {
		if t.Type != "page" || t.WebSocketDebuggerURL == "" {
			continue
		}
		applyCDPNetworkJail(t.WebSocketDebuggerURL)
		locBytes, _ := cdpEvalJS(t.WebSocketDebuggerURL, "window.location.href")
		if !strings.Contains(string(locBytes), cleanSpaceID) {
			targetURL := fmt.Sprintf("https://chat.google.com/app/chat/%s", cleanSpaceID)
			targetURLJSON, _ := json.Marshal(targetURL)
			_, _ = cdpEvalJS(t.WebSocketDebuggerURL, fmt.Sprintf("window.location.href = %s", string(targetURLJSON)))
		}
		return nil
	}
	return nil
}

// cdpEvalJS connects to a Chrome DevTools WebSocket endpoint and runs Runtime.evaluate(expression),
// returning the JSON-serialized result.value. Uses zero external dependencies (RFC 6455 client).
func cdpEvalJS(wsURL, expression string) ([]byte, error) {
	res, err := cdpCall(wsURL, "Runtime.evaluate", map[string]interface{}{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return nil, err
	}
	var evalResp struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &evalResp); err != nil {
		return nil, err
	}
	if evalResp.ExceptionDetails != nil {
		return nil, fmt.Errorf("CDP JS exception: %s", evalResp.ExceptionDetails.Text)
	}
	return evalResp.Result.Value, nil
}

func cdpCall(wsURL, method string, params map[string]interface{}) (json.RawMessage, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	keyBytes := make([]byte, 16)
	_, _ = rand.Read(keyBytes)
	wsKey := base64.StdEncoding.EncodeToString(keyBytes)

	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		path, u.Host, wsKey)
	if _, err := conn.Write([]byte(req)); err != nil {
		return nil, err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("websocket handshake HTTP %d", resp.StatusCode)
	}
	expectedAccept := computeWSAccept(wsKey)
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != "" && got != expectedAccept {
		return nil, fmt.Errorf("invalid Sec-WebSocket-Accept")
	}

	payload, err := json.Marshal(map[string]interface{}{
		"id":     1,
		"method": method,
		"params": params,
	})
	if err != nil {
		return nil, err
	}
	if err := writeWSFrame(conn, payload); err != nil {
		return nil, err
	}

	for {
		msg, opcode, err := readWSFrame(br)
		if err != nil {
			return nil, err
		}
		if opcode == 0x8 { // close
			return nil, fmt.Errorf("websocket closed by peer")
		}
		if opcode == 0x9 { // ping
			_ = writeWSControlFrame(conn, 0xA, msg)
			continue
		}
		if opcode != 0x1 && opcode != 0x2 {
			continue
		}
		var env struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(msg, &env); err != nil {
			continue
		}
		if env.ID != 1 {
			continue
		}
		if env.Error != nil {
			return nil, fmt.Errorf("CDP error %d: %s", env.Error.Code, env.Error.Message)
		}
		return env.Result, nil
	}
}

func computeWSAccept(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+"258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func writeWSFrame(w io.Writer, payload []byte) error {
	return writeWSControlFrame(w, 0x1, payload)
}

func writeWSControlFrame(w io.Writer, opcode byte, payload []byte) error {
	var hdr []byte
	hdr = append(hdr, 0x80|opcode) // FIN + opcode
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 65535:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		var b8 [8]byte
		binary.BigEndian.PutUint64(b8[:], uint64(n))
		hdr = append(hdr, b8[:]...)
	}
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	hdr = append(hdr, mask...)
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(masked)
	return err
}

func readWSFrame(r *bufio.Reader) ([]byte, byte, error) {
	var b2 [2]byte
	if _, err := io.ReadFull(r, b2[:]); err != nil {
		return nil, 0, err
	}
	opcode := b2[0] & 0x0F
	masked := (b2[1] & 0x80) != 0
	length := uint64(b2[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, 0, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, 0, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > 16*1024*1024 {
		return nil, 0, fmt.Errorf("websocket frame too large (%d bytes)", length)
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return nil, 0, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, 0, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return payload, opcode, nil
}

const jsExtractSpaces = `(() => {
  const docs = [document];
  for (const f of document.querySelectorAll('iframe')) {
    try { if (f.contentDocument) docs.push(f.contentDocument); } catch (e) {}
  }
  const seen = new Set();
  const out = [];
  const skipIds = new Set(['spaces/pinned-item', 'spaces/direct-messages', 'spaces/spaces', 'spaces/shortcut', 'spaces/browse-spaces']);
  const statusWords = /^(Away|Active|Do not disturb|In a meeting|Out of office|Offline|Unread|Pinned|Muted|Space|Direct message|External|\d+\s+unread.*|\d+:\d+\s*(AM|PM)?|Yesterday|Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec.*)$/i;
  for (const doc of docs) {
    const nodes = doc.querySelectorAll('[data-group-id], a[href*="/chat/"], a[href*="/room/"], a[href*="/dm/"], [role="listitem"][aria-label]');
    for (const el of nodes) {
      let id = el.getAttribute('data-group-id') || '';
      const href = el.getAttribute('href') || '';
      if (!id && href) {
        const m = href.match(/(?:chat|room|dm|spaces)\/([A-Za-z0-9_\-]+)/);
        if (m) id = 'spaces/' + m[1];
      }
      if (!id) continue;
      if (!id.startsWith('spaces/')) id = 'spaces/' + id.replace(/^(space|room|dm)\//, '');
      if (skipIds.has(id) || seen.has(id)) continue;
      seen.add(id);
      const aria = (el.getAttribute('aria-label') || '').trim();
      const lines = (aria ? [aria] : []).concat((el.innerText || '').split('\n')).map(s => s.trim()).filter(Boolean);
      let name = '';
      for (const line of lines) {
        const cleaned = line
          .replace(/^(Unread\s+|Pinned\s+|Space\s+|Direct message\s+|Conversation with\s+)/i, '')
          .replace(/,\s*(Away|Active|Do not disturb|Offline|Unread).*$/i, '')
          .trim();
        if (cleaned && !statusWords.test(cleaned) && !/^Google Account:/i.test(cleaned) && !/Google membership/i.test(cleaned) && cleaned.length > 1 && !cleaned.startsWith('spaces/')) {
          name = cleaned;
          break;
        }
      }
      if (!name) continue;
      out.push({ spaceId: id, displayName: name, spaceType: (id.endsWith('AAAAE') || href.includes('/dm/')) ? 'DIRECT_MESSAGE' : 'SPACE' });
    }
  }
  return out;
})()`

const jsExtractMessages = `(() => {
  const docs = [document];
  for (const f of document.querySelectorAll('iframe')) {
    try { if (f.contentDocument) docs.push(f.contentDocument); } catch (e) {}
  }
  const out = [];
  const seen = new Set();
  for (const doc of docs) {
    const items = doc.querySelectorAll('div[data-id][data-message-multiplat-item-id], [data-message-id], [data-topic-id]');
    let idx = 0;
    for (const el of items) {
      const msgId = el.getAttribute('data-id') || el.getAttribute('data-message-id') || el.getAttribute('data-topic-id') || ('dom-msg-' + (++idx));
      if (seen.has(msgId)) continue;
      const bodyEl = el.querySelector('div[jsname="bgckF"], div.DTp27d.QIJiHb.Zc1Emd');
      const text = (bodyEl ? bodyEl.innerText : (el.innerText || '')).trim();
      if (!text) continue;
      seen.add(msgId);
      const senderEl = el.querySelector('span.ZTmjQb.Z4BnXb, span[jscontroller="AkvWld"], [data-member-id], [data-name], [role="heading"]');
      const sender = senderEl ? (senderEl.getAttribute('data-name') || senderEl.innerText || 'Chat Participant').trim() : 'Chat Participant';
      out.push({
        id: msgId,
        sender: sender.split('\n')[0],
        plainText: text,
        snippet: text.slice(0, 200),
        createTime: new Date().toISOString()
      });
    }
  }
  return out;
})()`

func evalSpacesViaWebSocket(targets []cdpTarget, alias, query string, maxResults int) ([]SpaceSummary, error) {
	qLower := strings.ToLower(strings.TrimSpace(query))
	seen := make(map[string]bool)
	var out []SpaceSummary
	for _, t := range targets {
		if t.WebSocketDebuggerURL == "" {
			continue
		}
		if t.Type == "page" {
			applyCDPNetworkJail(t.WebSocketDebuggerURL)
		}
		raw, err := cdpEvalJS(t.WebSocketDebuggerURL, jsExtractSpaces)
		if err != nil || len(raw) == 0 {
			continue
		}
		var extracted []SpaceSummary
		if err := json.Unmarshal(raw, &extracted); err != nil {
			continue
		}
		for _, s := range extracted {
			if seen[s.SpaceID] {
				continue
			}
			if qLower != "" && !strings.Contains(strings.ToLower(s.DisplayName), qLower) && !strings.Contains(strings.ToLower(s.SpaceID), qLower) {
				continue
			}
			seen[s.SpaceID] = true
			s.Account = alias
			s.Source = "cdp"
			out = append(out, s)
			if len(out) >= maxResults {
				return out, nil
			}
		}
	}
	return out, nil
}

func evalMessagesViaWebSocket(targets []cdpTarget, alias, spaceID, query string, maxResults int) ([]MessageSummary, error) {
	qLower := strings.ToLower(strings.TrimSpace(query))
	if qLower == "is:unread" {
		qLower = ""
	}
	var out []MessageSummary
	for _, t := range targets {
		if t.WebSocketDebuggerURL == "" {
			continue
		}
		if t.Type == "page" {
			applyCDPNetworkJail(t.WebSocketDebuggerURL)
		}
		spID := spaceID
		if spID == "" {
			spID = extractSpaceIDFromURL(t.URL, t.ID)
		}
		raw, err := cdpEvalJS(t.WebSocketDebuggerURL, jsExtractMessages)
		if err != nil || len(raw) == 0 {
			continue
		}
		var msgs []struct {
			ID         string `json:"id"`
			Sender     string `json:"sender"`
			PlainText  string `json:"plainText"`
			Snippet    string `json:"snippet"`
			CreateTime string `json:"createTime"`
		}
		if err := json.Unmarshal(raw, &msgs); err != nil {
			continue
		}
		for _, m := range msgs {
			if qLower != "" && !strings.Contains(strings.ToLower(m.PlainText), qLower) && !strings.Contains(strings.ToLower(m.Sender), qLower) {
				continue
			}
			out = append(out, MessageSummary{
				ID:         m.ID,
				SpaceID:    spID,
				SpaceName:  strings.TrimSpace(strings.TrimSuffix(t.Title, " - Google Chat")),
				ThreadID:   m.ID,
				Account:    alias,
				Sender:     m.Sender,
				CreateTime: m.CreateTime,
				Snippet:    m.Snippet,
				Source:     "cdp",
			})
			if len(out) >= maxResults {
				return out, nil
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no messages extracted from active CDP WebSocket targets")
	}
	return out, nil
}

func evalThreadViaWebSocket(targets []cdpTarget, alias, spaceID, threadID string, maxMessages int) (ThreadDetail, error) {
	for _, t := range targets {
		if t.WebSocketDebuggerURL == "" {
			continue
		}
		if t.Type == "page" {
			applyCDPNetworkJail(t.WebSocketDebuggerURL)
		}
		raw, err := cdpEvalJS(t.WebSocketDebuggerURL, jsExtractMessages)
		if err != nil || len(raw) == 0 {
			continue
		}
		var msgs []MessageDetail
		if err := json.Unmarshal(raw, &msgs); err != nil || len(msgs) == 0 {
			continue
		}
		for i := range msgs {
			msgs[i].SpaceID = spaceID
			if msgs[i].ThreadID == "" {
				msgs[i].ThreadID = threadID
			}
		}
		if len(msgs) > maxMessages {
			msgs = msgs[len(msgs)-maxMessages:]
		}
		return ThreadDetail{
			SpaceID:   spaceID,
			SpaceName: strings.TrimSpace(strings.TrimSuffix(t.Title, " - Google Chat")),
			ThreadID:  threadID,
			Account:   alias,
			Messages:  msgs,
			Source:    "cdp",
		}, nil
	}
	return ThreadDetail{}, fmt.Errorf("no thread messages found via CDP WebSocket")
}

func sendDraftViaWebSocket(targets []cdpTarget, spec DraftSpec) (string, error) {
	cleanSpaceID := strings.TrimPrefix(spec.SpaceID, "spaces/")
	cleanSpaceID = strings.TrimPrefix(cleanSpaceID, "space/")
	cleanSpaceID = strings.TrimPrefix(cleanSpaceID, "dm/")

	// Prioritize page targets over subframe/iframe targets
	var sortedTargets []cdpTarget
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			sortedTargets = append(sortedTargets, t)
		}
	}
	for _, t := range targets {
		if t.Type != "page" && t.WebSocketDebuggerURL != "" {
			sortedTargets = append(sortedTargets, t)
		}
	}
	if len(sortedTargets) == 0 {
		return "", fmt.Errorf("no active Google Chat WebSocket targets found")
	}

	bodyJSON, _ := json.Marshal(spec.Body)
	jsFocusAndInsert := fmt.Sprintf(`(() => {
  const docs = [document];
  for (const f of document.querySelectorAll('iframe')) {
    try { if (f.contentDocument) docs.push(f.contentDocument); } catch (e) {}
  }
  for (const doc of docs) {
    const box = doc.querySelector('div[role="textbox"][contenteditable="true"]');
    if (box) {
      box.focus();
      doc.execCommand('selectAll', false, null);
      doc.execCommand('insertText', false, %s);
      return true;
    }
  }
  return false;
})()`, string(bodyJSON))

	const jsClickSendBtn = `(() => {
  const docs = [document];
  for (const f of document.querySelectorAll('iframe')) {
    try { if (f.contentDocument) docs.push(f.contentDocument); } catch (e) {}
  }
  for (const doc of docs) {
    const btn = doc.querySelector('button[aria-label="Send message"], div[role="button"][aria-label="Send message"], [data-tooltip="Send message"]');
    if (btn && !btn.disabled && btn.getAttribute('aria-disabled') !== 'true') {
      btn.click();
      return true;
    }
  }
  return false;
})()`

	const jsCheckComposer = `(() => {
  const docs = [document];
  for (const f of document.querySelectorAll('iframe')) {
    try { if (f.contentDocument) docs.push(f.contentDocument); } catch (e) {}
  }
  for (const doc of docs) {
    if (doc.querySelector('div[role="textbox"][contenteditable="true"]')) return true;
  }
  return false;
})()`

	var lastErr error
	for _, t := range sortedTargets {
		wsURL := t.WebSocketDebuggerURL
		if t.Type == "page" {
			applyCDPNetworkJail(wsURL)
		}

		// 1. Ensure tab is navigated to the target space
		if cleanSpaceID != "" {
			locBytes, err := cdpEvalJS(wsURL, "window.location.href")
			curLoc := ""
			if err == nil {
				curLoc = strings.Trim(string(locBytes), `"`)
			}

			if !strings.Contains(curLoc, cleanSpaceID) {
				cleanSpaceJSON, _ := json.Marshal(cleanSpaceID)
				jsClickSidebar := fmt.Sprintf(`(() => {
  const id = %s;
  const selectors = [
    `+"`"+`[data-group-id="space/${id}"]`+"`"+`,
    `+"`"+`[data-group-id="${id}"]`+"`"+`,
    `+"`"+`[data-group-id="dm/${id}"]`+"`"+`,
    `+"`"+`a[href*="/chat/${id}"]`+"`"+`,
    `+"`"+`a[href*="/room/${id}"]`+"`"+`,
    `+"`"+`a[href*="/dm/${id}"]`+"`"+`,
    `+"`"+`a[href*="/spaces/${id}"]`+"`"+`
  ];
  for (const sel of selectors) {
    const el = document.querySelector(sel);
    if (el) { el.click(); return true; }
  }
  return false;
})()`, string(cleanSpaceJSON))

				clickedSidebar, _ := cdpEvalJS(wsURL, jsClickSidebar)
				if string(clickedSidebar) == "true" {
					for i := 0; i < 4; i++ {
						time.Sleep(400 * time.Millisecond)
						ready, _ := cdpEvalJS(wsURL, jsCheckComposer)
						if string(ready) == "true" {
							break
						}
					}
				}

				locBytesAfter, _ := cdpEvalJS(wsURL, "window.location.href")
				if !strings.Contains(string(locBytesAfter), cleanSpaceID) {
					targetURL := fmt.Sprintf("https://chat.google.com/app/chat/%s", cleanSpaceID)
					if strings.Contains(curLoc, "mail.google.com") || strings.Contains(t.URL, "mail.google.com") {
						targetURL = fmt.Sprintf("https://mail.google.com/mail/u/0/#chat/space/%s", cleanSpaceID)
					}
					targetURLJSON, _ := json.Marshal(targetURL)
					_, _ = cdpEvalJS(wsURL, fmt.Sprintf("window.location.href = %s", string(targetURLJSON)))
					time.Sleep(800 * time.Millisecond)
				}
			}
		}

		// 2. Wait up to 10 seconds for the composer element to appear
		composerReady := false
		for i := 0; i < 20; i++ {
			raw, err := cdpEvalJS(wsURL, jsCheckComposer)
			if err == nil && string(raw) == "true" {
				composerReady = true
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !composerReady {
			lastErr = fmt.Errorf("timeout waiting for composer in tab %s", t.ID)
			continue
		}

		time.Sleep(250 * time.Millisecond)

		// 3. Inject the draft text into the composer
		raw, err := cdpEvalJS(wsURL, jsFocusAndInsert)
		if err != nil || string(raw) != "true" {
			lastErr = fmt.Errorf("failed to focus or insert text into composer in tab %s: %v", t.ID, err)
			continue
		}

		time.Sleep(250 * time.Millisecond)

		// 4. Click Send button or dispatch Enter
		sendRes, err := cdpEvalJS(wsURL, jsClickSendBtn)
		if err != nil || string(sendRes) != "true" {
			_, _ = cdpCall(wsURL, "Input.dispatchKeyEvent", map[string]interface{}{
				"type":                  "keyDown",
				"key":                   "Enter",
				"code":                  "Enter",
				"windowsVirtualKeyCode": 13,
				"nativeVirtualKeyCode":  13,
			})
			_, _ = cdpCall(wsURL, "Input.dispatchKeyEvent", map[string]interface{}{
				"type":                  "keyUp",
				"key":                   "Enter",
				"code":                  "Enter",
				"windowsVirtualKeyCode": 13,
				"nativeVirtualKeyCode":  13,
			})
		}

		time.Sleep(800 * time.Millisecond)
		return fmt.Sprintf("delivered via Chrome CDP WebSocket (%s to %s)", t.ID, spec.SpaceID), nil
	}

	if lastErr != nil {
		return "", fmt.Errorf("CDP send failed: %w", lastErr)
	}
	return "", fmt.Errorf("no active Google Chat message composer box found in Chrome CDP tabs")
}

// resolveOrCreateDMViaCDP uses Google Chat's "New chat" PeopleKit UI in headless Chrome to resolve
// or start a Direct Message with an email address, username handle, or contact not currently visible
// in the top 20 sidebar items.
func resolveOrCreateDMViaCDP(vaultDir string, cfg AccountConfig, rawQuery string) (SpaceSummary, error) {
	if mockDir(vaultDir) != "" || cfg.Transport != "cdp" {
		return SpaceSummary{}, fmt.Errorf("live CDP DM resolution unavailable in non-CDP mode")
	}
	if cfg.CDPEndpoint != "" && cfg.CDPEndpoint != DefaultCDPEndpoint {
		return SpaceSummary{}, fmt.Errorf("live CDP DM resolution skipped for custom test endpoint")
	}
	ep, err := EnsureCDPSession(vaultDir, cfg.Alias, true)
	if err != nil {
		return SpaceSummary{}, err
	}
	targets, err := fetchCDPTargets(ep)
	if err != nil || len(targets) == 0 {
		return SpaceSummary{}, fmt.Errorf("no active Google Chat tabs found")
	}

	var wsURL string
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			wsURL = t.WebSocketDebuggerURL
			break
		}
	}
	if wsURL == "" {
		return SpaceSummary{}, fmt.Errorf("no page WebSocket target found in Chrome")
	}
	applyCDPNetworkJail(wsURL)

	// Build candidate search strings: if bare username without '@' or whitespace (e.g. "alex_dev"),
	// try "<username>@gmail.com" first so PeopleKit immediately surfaces the exact email chip.
	q := strings.TrimSpace(rawQuery)
	var candidates []string
	if !strings.Contains(q, "@") && !strings.Contains(q, " ") {
		candidates = []string{q + "@gmail.com", q}
	} else {
		candidates = []string{q}
	}

	const jsClickNewChat = `(() => {
  const fire = (el) => {
    for (const t of ['pointerdown', 'mousedown', 'pointerup', 'mouseup', 'click']) {
      el.dispatchEvent(new MouseEvent(t, { bubbles: true, cancelable: true, view: window }));
    }
  };
  const btn = document.querySelector('button[jsname="TrXBg"]') ||
    Array.from(document.querySelectorAll('button, [role="button"]')).find(el => (el.innerText || '').trim().includes('New chat'));
  if (!btn) return false;
  fire(btn);
  return true;
})()`

	for _, searchStr := range candidates {
		_, _ = cdpEvalJS(wsURL, jsClickNewChat)

		// Wait up to 2.5s for PeopleKit input `input[aria-label="Add 1 or more people"]`
		searchJSON, _ := json.Marshal(searchStr)
		jsTypeSearch := fmt.Sprintf(`(() => {
  const input = document.querySelector('input[aria-label="Add 1 or more people"], input.I4p4db.XdSAtd');
  if (!input) return false;
  input.focus();
  input.value = '';
  document.execCommand('selectAll', false, null);
  document.execCommand('insertText', false, %s);
  if (!input.value) input.value = %s;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  input.dispatchEvent(new Event('change', { bubbles: true }));
  return true;
})()`, string(searchJSON), string(searchJSON))

		typed := false
		for i := 0; i < 10; i++ {
			time.Sleep(250 * time.Millisecond)
			if raw, err := cdpEvalJS(wsURL, jsTypeSearch); err == nil && string(raw) == "true" {
				typed = true
				break
			}
		}
		if !typed {
			continue
		}

		// Poll up to 2.5s for PeopleKit autocomplete option `[role="option"][pkd-target]`
		const jsPickOption = `(() => {
  const fire = (el) => {
    for (const t of ['pointerdown', 'mousedown', 'pointerup', 'mouseup', 'click']) {
      el.dispatchEvent(new MouseEvent(t, { bubbles: true, cancelable: true, view: window }));
    }
  };
  const opt = document.querySelector('[role="option"][pkd-target], .AZW99.XdSAtd[role="option"]');
  if (!opt || opt.offsetParent === null) return '';
  const text = (opt.innerText || '').split('\n').map(s => s.trim()).filter(Boolean).slice(0, 2).join(' — ');
  fire(opt);
  return text || 'matched';
})()`

		pickedLabel := ""
		for i := 0; i < 10; i++ {
			time.Sleep(280 * time.Millisecond)
			if raw, err := cdpEvalJS(wsURL, jsPickOption); err == nil {
				val := strings.Trim(string(raw), `"`)
				if val != "" && val != "null" {
					pickedLabel = val
					break
				}
			}
		}
		if pickedLabel == "" {
			continue
		}

		// Click "Start chat" button if visible and wait for URL to navigate to /app/chat/<id> or /app/dm/<id>
		const jsStartAndCheckURL = `(() => {
  const fire = (el) => {
    for (const t of ['pointerdown', 'mousedown', 'pointerup', 'mouseup', 'click']) {
      el.dispatchEvent(new MouseEvent(t, { bubbles: true, cancelable: true, view: window }));
    }
  };
  const startBtn = document.querySelector('button[jsname="OCpkoe"]') ||
    Array.from(document.querySelectorAll('button, [role="button"]')).find(el => /^Start chat$/i.test((el.innerText || '').trim()));
  if (startBtn && !startBtn.disabled && startBtn.getAttribute('aria-disabled') !== 'true') {
    fire(startBtn);
  }
  const m = window.location.href.match(/\/(?:chat|dm|room|spaces)\/([A-Za-z0-9_\-]+)/);
  if (m && m[1] && m[1] !== 'home') return m[1];
  return '';
})()`

		for i := 0; i < 12; i++ {
			time.Sleep(300 * time.Millisecond)
			if raw, err := cdpEvalJS(wsURL, jsStartAndCheckURL); err == nil {
				spID := strings.Trim(string(raw), `"`)
				if spID != "" && spID != "null" && spID != "home" {
					displayName := q
					if pickedLabel != "matched" && pickedLabel != "" {
						displayName = pickedLabel
					}
					summary := SpaceSummary{
						SpaceID:     "spaces/" + spID,
						Account:     cfg.Alias,
						DisplayName: displayName,
						SpaceType:   "DIRECT_MESSAGE",
						Source:      "cdp",
					}
					_ = saveCachedSpaces(vaultDir, cfg.Alias, []SpaceSummary{summary})
					return summary, nil
				}
			}
		}
	}

	return SpaceSummary{}, fmt.Errorf("could not find Google Chat contact matching %q via New Chat search", rawQuery)
}
