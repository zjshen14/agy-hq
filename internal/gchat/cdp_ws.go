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
	"strings"
	"time"
)

type cdpTarget struct {
	ID                   string `json:"id"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	Type                 string `json:"type"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func fetchCDPTargets(cdpEndpoint string) ([]cdpTarget, error) {
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
	var chatTargets []cdpTarget
	for _, t := range targets {
		if (strings.Contains(t.URL, "chat.google.com") || strings.Contains(t.URL, "mail.google.com/chat")) && t.WebSocketDebuggerURL != "" {
			chatTargets = append(chatTargets, t)
		}
	}
	return chatTargets, nil
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
  for (const doc of docs) {
    const nodes = doc.querySelectorAll('[data-group-id], a[href*="/chat/"], a[href*="/dm/"], [role="listitem"][aria-label]');
    for (const el of nodes) {
      let id = el.getAttribute('data-group-id') || '';
      const href = el.getAttribute('href') || '';
      if (!id && href) {
        const m = href.match(/(?:chat|dm|spaces)\/([A-Za-z0-9_\-]+)/);
        if (m) id = 'spaces/' + m[1];
      }
      if (!id) continue;
      if (!id.startsWith('spaces/')) id = 'spaces/' + id.replace(/^(space|dm)\//, '');
      if (seen.has(id)) continue;
      seen.add(id);
      const name = (el.getAttribute('aria-label') || el.innerText || id).split('\n')[0].trim();
      out.push({ spaceId: id, displayName: name || id, spaceType: href.includes('/dm/') ? 'DIRECT_MESSAGE' : 'SPACE' });
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
  for (const doc of docs) {
    const items = doc.querySelectorAll('[data-message-id], [data-topic-id], [role="listitem"]');
    let idx = 0;
    for (const el of items) {
      const text = (el.innerText || '').trim();
      if (!text || text.length < 2) continue;
      idx++;
      const msgId = el.getAttribute('data-message-id') || el.getAttribute('data-topic-id') || ('dom-msg-' + idx);
      const senderEl = el.querySelector('[data-member-id], [data-name], [role="heading"]');
      const sender = senderEl ? (senderEl.getAttribute('data-name') || senderEl.innerText || 'Chat User').trim() : 'Chat Participant';
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
    const btn = doc.querySelector('button[aria-label="Send message"], button[aria-label*="Send"], [data-tooltip*="Send"]');
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

		// 1. Ensure tab is navigated to the target space
		if cleanSpaceID != "" {
			locBytes, err := cdpEvalJS(wsURL, "window.location.href")
			curLoc := ""
			if err == nil {
				curLoc = strings.Trim(string(locBytes), `"`)
			}

			if !strings.Contains(curLoc, cleanSpaceID) {
				// Try client-side routing via sidebar click first
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
						time.Sleep(500 * time.Millisecond)
						ready, _ := cdpEvalJS(wsURL, jsCheckComposer)
						if string(ready) == "true" {
							break
						}
					}
				}

				// If not yet on space, navigate URL directly
				locBytesAfter, _ := cdpEvalJS(wsURL, "window.location.href")
				if !strings.Contains(string(locBytesAfter), cleanSpaceID) {
					targetURL := fmt.Sprintf("https://chat.google.com/room/%s", cleanSpaceID)
					if strings.Contains(curLoc, "mail.google.com") || strings.Contains(t.URL, "mail.google.com") {
						targetURL = fmt.Sprintf("https://mail.google.com/mail/u/0/#chat/space/%s", cleanSpaceID)
					}
					targetURLJSON, _ := json.Marshal(targetURL)
					_, _ = cdpEvalJS(wsURL, fmt.Sprintf("window.location.href = %s", string(targetURLJSON)))
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

		// Settle
		time.Sleep(300 * time.Millisecond)

		// 3. Inject the draft text into the composer
		raw, err := cdpEvalJS(wsURL, jsFocusAndInsert)
		if err != nil || string(raw) != "true" {
			lastErr = fmt.Errorf("failed to focus or insert text into composer in tab %s: %v", t.ID, err)
			continue
		}

		// Wait 300ms for send button to update state
		time.Sleep(300 * time.Millisecond)

		// 4. Click Send button or dispatch Enter
		sendRes, err := cdpEvalJS(wsURL, jsClickSendBtn)
		if err != nil || string(sendRes) != "true" {
			// Fallback: Dispatch Enter key via CDP Input.dispatchKeyEvent
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

		time.Sleep(1 * time.Second)
		return fmt.Sprintf("delivered via Chrome CDP WebSocket (%s to %s)", t.ID, spec.SpaceID), nil
	}

	if lastErr != nil {
		return "", fmt.Errorf("CDP send failed: %w", lastErr)
	}
	return "", fmt.Errorf("no active Google Chat message composer box found in Chrome CDP tabs")
}

