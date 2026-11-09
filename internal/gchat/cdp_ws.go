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

// InteractiveLogin opens a visible Chrome window to Google Sign-In, polls until chat.google.com loads,
// caches the user's space list, closes the visible window, and restarts in background `--headless=new` mode.
func InteractiveLogin(vaultDir, accountAlias string, timeout time.Duration) (string, error) {
	cfg, err := ResolveAccount(vaultDir, accountAlias)
	if err != nil {
		return "", err
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ep := resolveActiveCDPEndpoint(cfg)
	if ok, _ := CheckCDPEndpoint(ep); ok {
		// Check if already authenticated
		if chatTargets, err := fetchCDPTargets(ep); err == nil && len(chatTargets) > 0 {
			return ep, nil
		}
		closeCDPBrowser(ep)
		time.Sleep(400 * time.Millisecond)
	}

	if _, _, err := launchChromeWithURL(cfg, false, "https://accounts.google.com/ServiceLogin?continue=https://chat.google.com/u/0/app/home"); err != nil {
		return "", err
	}

	deadline := time.Now().Add(timeout)
	var authedTargets []cdpTarget
	for time.Now().Before(deadline) {
		time.Sleep(1 * time.Second)
		ep = resolveActiveCDPEndpoint(cfg)
		targets, err := fetchCDPTargets(ep)
		if err == nil && len(targets) > 0 {
			authedTargets = targets
			break
		}
	}
	if len(authedTargets) == 0 {
		return "", fmt.Errorf("timed out after %s waiting for Google Chat sign-in in Chrome", timeout)
	}

	// Let sidebar render and cache spaces before switching to background headless mode
	time.Sleep(2 * time.Second)
	if spaces, err := evalSpacesViaWebSocket(authedTargets, cfg.Alias, "", 50); err == nil && len(spaces) > 0 {
		_ = saveCachedSpaces(vaultDir, cfg.Alias, spaces)
	}

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
	return ep, nil
}

// PreWarmSpace navigates the background headless Chrome tab to spaceID so that when the user taps
// Approve on the 1-tap prompt seconds later, the composer box is already loaded and ready.
func PreWarmSpace(vaultDir string, cfg AccountConfig, spaceID string) {
	if mockDir(vaultDir) != "" || cfg.Transport != "cdp" {
		return
	}
	if cfg.CDPEndpoint != "" && cfg.CDPEndpoint != DefaultCDPEndpoint {
		return
	}
	ep, err := EnsureCDPSession(vaultDir, cfg.Alias, false)
	if err != nil {
		return
	}
	targets, err := fetchCDPTargets(ep)
	if err != nil || len(targets) == 0 {
		return
	}
	cleanSpaceID := strings.TrimPrefix(strings.TrimSpace(spaceID), "spaces/")
	cleanSpaceID = strings.TrimPrefix(cleanSpaceID, "space/")
	cleanSpaceID = strings.TrimPrefix(cleanSpaceID, "dm/")
	if cleanSpaceID == "" || strings.HasPrefix(cleanSpaceID, "gmail-chat-") {
		return
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
		return
	}
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
        if (cleaned && !statusWords.test(cleaned) && cleaned.length > 1 && !cleaned.startsWith('spaces/')) {
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
