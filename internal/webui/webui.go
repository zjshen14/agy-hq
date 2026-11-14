package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/doctor"
	"github.com/zjshen14/agy-hq/internal/gchat"
	"github.com/zjshen14/agy-hq/internal/gmail"
	"github.com/zjshen14/agy-hq/internal/hooks"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/version"
	"github.com/zjshen14/agy-hq/internal/worktree"
)

// DashboardState aggregates live Vault, approval, worktree, task, journal, quota, observability, spaces, goals, ideas, and doctor state.
type DashboardState struct {
	VaultDir      string                         `json:"vaultDir"`
	UpdatedAt     string                         `json:"updatedAt"`
	Quota         kernel.QuotaState              `json:"quota"`
	Observability obs.DashboardSummary           `json:"observability"`
	Pending       []approvals.Proposal           `json:"pending"`
	ActiveSpaces  []ConsoleSpace                 `json:"activeSpaces,omitempty"`
	Goals         []kernel.Goal                  `json:"goals,omitempty"`
	OverdueWaits  []kernel.OverdueWaitingSummary `json:"overdueWaits,omitempty"`
	Ideas         []outbox.OutboxItem            `json:"ideas,omitempty"`
	Worktrees     []string                       `json:"worktrees"`
	Tasks         []kernel.ActiveTask            `json:"tasks"`
	TodayTurns    []hooks.JournalEntry           `json:"todayTurns"`
	Health        []doctor.CheckResult           `json:"health"`
	FlashResult   string                         `json:"flashResult,omitempty"`
	CSRFToken     string                         `json:"-"`
}

// IsLoopbackAddr returns true if listenAddr binds strictly to a loopback interface (127.0.0.1, ::1, or localhost).
func IsLoopbackAddr(listenAddr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil {
		host = strings.TrimSpace(listenAddr)
	}
	return isLoopbackHostOnly(host) && host != ""
}

func isLoopbackHostHeader(hostHeader string) bool {
	trimmed := strings.TrimSpace(hostHeader)
	if trimmed == "" {
		return true
	}
	host := trimmed
	if h, _, err := net.SplitHostPort(trimmed); err == nil {
		host = h
	}
	return isLoopbackHostOnly(host)
}

func isLoopbackHostOnly(host string) bool {
	h := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func isAllowedSameOriginRequest(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !isLoopbackHostOnly(u.Hostname()) {
			return false
		}
	}
	return true
}

func generateCSRFToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func validCSRFToken(expected, candidate string) bool {
	c := strings.TrimSpace(candidate)
	if expected == "" || len(c) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(c)) == 1
}

// NewHandler creates the HTTP mux for the HQ Console side pane and API.
func NewHandler(vaultDir string) http.Handler {
	mux := http.NewServeMux()
	csrfToken := generateCSRFToken()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":             "ok",
			"version":            version.Version,
			"commit":             version.EffectiveCommit(),
			"vaultSchemaVersion": vault.CurrentSchemaVersion,
		})
	})

	mux.HandleFunc("/preload.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		_, _ = w.Write([]byte(preloadBridgeJS))
	})

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedSameOriginRequest(r) {
			http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
			return
		}
		state := loadState(vaultDir, "")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state)
	})

	mux.HandleFunc("/api/obs", func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedSameOriginRequest(r) {
			http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
			return
		}
		sum := obs.BuildDashboardSummary(vaultDir, time.Now())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sum)
	})

	mux.HandleFunc("/api/approve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !isAllowedSameOriginRequest(r) {
			http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
			return
		}

		var id, sha, reqCSRF string
		if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			var body struct {
				ID        string `json:"id"`
				SHA256    string `json:"sha256"`
				CSRFToken string `json:"csrf_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id = strings.TrimSpace(body.ID)
			sha = strings.TrimSpace(body.SHA256)
			reqCSRF = strings.TrimSpace(body.CSRFToken)
		} else {
			_ = r.ParseForm()
			id = strings.TrimSpace(r.FormValue("id"))
			sha = strings.TrimSpace(r.FormValue("sha256"))
			reqCSRF = strings.TrimSpace(r.FormValue("csrf_token"))
		}
		if hdr := strings.TrimSpace(r.Header.Get("X-HQ-CSRF-Token")); hdr != "" {
			reqCSRF = hdr
		}
		if !validCSRFToken(csrfToken, reqCSRF) {
			http.Error(w, "forbidden: missing or invalid CSRF token", http.StatusForbidden)
			return
		}

		out, err := approvals.ApproveProposal(vaultDir, id, sha)
		_ = SyncMarkdownConsole(vaultDir)

		msg := fmt.Sprintf("Approved %s", id)
		if out != "" {
			msg += ": " + strings.TrimSpace(out)
		}
		if err != nil {
			msg = fmt.Sprintf("Error approving %s: %v (%s)", id, err, strings.TrimSpace(out))
		}

		if strings.Contains(r.Header.Get("Accept"), "application/json") || strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      err == nil,
				"message": msg,
				"output":  out,
			})
			return
		}
		state := loadState(vaultDir, msg)
		state.CSRFToken = csrfToken
		renderHTML(w, state)
	})

	mux.HandleFunc("/api/reject", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !isAllowedSameOriginRequest(r) {
			http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
			return
		}

		var id, reason, reqCSRF string
		if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			var body struct {
				ID        string `json:"id"`
				Reason    string `json:"reason"`
				CSRFToken string `json:"csrf_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id = strings.TrimSpace(body.ID)
			reason = strings.TrimSpace(body.Reason)
			reqCSRF = strings.TrimSpace(body.CSRFToken)
		} else {
			_ = r.ParseForm()
			id = strings.TrimSpace(r.FormValue("id"))
			reason = strings.TrimSpace(r.FormValue("reason"))
			reqCSRF = strings.TrimSpace(r.FormValue("csrf_token"))
		}
		if hdr := strings.TrimSpace(r.Header.Get("X-HQ-CSRF-Token")); hdr != "" {
			reqCSRF = hdr
		}
		if !validCSRFToken(csrfToken, reqCSRF) {
			http.Error(w, "forbidden: missing or invalid CSRF token", http.StatusForbidden)
			return
		}
		if reason == "" {
			reason = "Rejected via HQ Console"
		}

		err := approvals.RejectProposal(vaultDir, id, reason)
		_ = SyncMarkdownConsole(vaultDir)

		msg := fmt.Sprintf("Rejected %s", id)
		if err != nil {
			msg = fmt.Sprintf("Error rejecting %s: %v", id, err)
		}

		if strings.Contains(r.Header.Get("Accept"), "application/json") || strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      err == nil,
				"message": msg,
			})
			return
		}
		state := loadState(vaultDir, msg)
		state.CSRFToken = csrfToken
		renderHTML(w, state)
	})

	mux.HandleFunc("/api/tasks/complete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !isAllowedSameOriginRequest(r) {
			http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
			return
		}

		var slug, reqCSRF string
		if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			var body struct {
				Slug      string `json:"slug"`
				CSRFToken string `json:"csrf_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			slug = strings.TrimSpace(body.Slug)
			reqCSRF = strings.TrimSpace(body.CSRFToken)
		} else {
			_ = r.ParseForm()
			slug = strings.TrimSpace(r.FormValue("slug"))
			reqCSRF = strings.TrimSpace(r.FormValue("csrf_token"))
		}
		if hdr := strings.TrimSpace(r.Header.Get("X-HQ-CSRF-Token")); hdr != "" {
			reqCSRF = hdr
		}
		if !validCSRFToken(csrfToken, reqCSRF) {
			http.Error(w, "forbidden: missing or invalid CSRF token", http.StatusForbidden)
			return
		}

		err := kernel.CompleteTask(vaultDir, slug, "")
		_ = SyncMarkdownConsole(vaultDir)
		msg := fmt.Sprintf("Completed task %s", slug)
		if err != nil {
			msg = fmt.Sprintf("Error completing %s: %v", slug, err)
		}

		if strings.Contains(r.Header.Get("Accept"), "application/json") || strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      err == nil,
				"message": msg,
			})
			return
		}
		state := loadState(vaultDir, msg)
		state.CSRFToken = csrfToken
		renderHTML(w, state)
	})

	mux.HandleFunc("/api/tasks/archive", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !isAllowedSameOriginRequest(r) {
			http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
			return
		}

		var slug, reqCSRF string
		if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			var body struct {
				Slug      string `json:"slug"`
				CSRFToken string `json:"csrf_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			slug = strings.TrimSpace(body.Slug)
			reqCSRF = strings.TrimSpace(body.CSRFToken)
		} else {
			_ = r.ParseForm()
			slug = strings.TrimSpace(r.FormValue("slug"))
			reqCSRF = strings.TrimSpace(r.FormValue("csrf_token"))
		}
		if hdr := strings.TrimSpace(r.Header.Get("X-HQ-CSRF-Token")); hdr != "" {
			reqCSRF = hdr
		}
		if !validCSRFToken(csrfToken, reqCSRF) {
			http.Error(w, "forbidden: missing or invalid CSRF token", http.StatusForbidden)
			return
		}

		err := kernel.ArchiveTask(vaultDir, slug, false)
		_ = SyncMarkdownConsole(vaultDir)
		msg := fmt.Sprintf("Archived task %s", slug)
		if err != nil {
			msg = fmt.Sprintf("Error archiving %s: %v", slug, err)
		}

		if strings.Contains(r.Header.Get("Accept"), "application/json") || strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      err == nil,
				"message": msg,
			})
			return
		}
		state := loadState(vaultDir, msg)
		state.CSRFToken = csrfToken
		renderHTML(w, state)
	})

	mux.HandleFunc("/api/console-spaces", func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedSameOriginRequest(r) {
			http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
			return
		}
		spaces, _ := ListSpaces(vaultDir, time.Now())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(spaces)
	})

	mux.HandleFunc("/api/console-spaces/set", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !isAllowedSameOriginRequest(r) {
			http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
			return
		}
		var id, mode, reqCSRF string
		if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			var body struct {
				ID        string `json:"id"`
				Mode      string `json:"mode"`
				CSRFToken string `json:"csrf_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id = strings.TrimSpace(body.ID)
			mode = strings.TrimSpace(body.Mode)
			reqCSRF = strings.TrimSpace(body.CSRFToken)
		} else {
			_ = r.ParseForm()
			id = strings.TrimSpace(r.FormValue("id"))
			mode = strings.TrimSpace(r.FormValue("mode"))
			reqCSRF = strings.TrimSpace(r.FormValue("csrf_token"))
		}
		if hdr := strings.TrimSpace(r.Header.Get("X-HQ-CSRF-Token")); hdr != "" {
			reqCSRF = hdr
		}
		if !validCSRFToken(csrfToken, reqCSRF) {
			http.Error(w, "forbidden: missing or invalid CSRF token", http.StatusForbidden)
			return
		}
		err := SetSpaceOverride(vaultDir, id, mode)
		_ = SyncMarkdownConsole(vaultDir)
		msg := fmt.Sprintf("Updated space %s (%s)", id, mode)
		if err != nil {
			msg = fmt.Sprintf("Error updating space %s: %v", id, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": err == nil, "message": msg})
	})

	mux.HandleFunc("/api/ideas/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !isAllowedSameOriginRequest(r) {
			http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
			return
		}
		var id, action, reason, durationStr, reqCSRF string
		if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			var body struct {
				ID        string `json:"id"`
				Action    string `json:"action"`
				Reason    string `json:"reason"`
				Duration  string `json:"duration"`
				CSRFToken string `json:"csrf_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id = strings.TrimSpace(body.ID)
			action = strings.ToLower(strings.TrimSpace(body.Action))
			reason = strings.TrimSpace(body.Reason)
			durationStr = strings.TrimSpace(body.Duration)
			reqCSRF = strings.TrimSpace(body.CSRFToken)
		} else {
			_ = r.ParseForm()
			id = strings.TrimSpace(r.FormValue("id"))
			action = strings.ToLower(strings.TrimSpace(r.FormValue("action")))
			reason = strings.TrimSpace(r.FormValue("reason"))
			durationStr = strings.TrimSpace(r.FormValue("duration"))
			reqCSRF = strings.TrimSpace(r.FormValue("csrf_token"))
		}
		if hdr := strings.TrimSpace(r.Header.Get("X-HQ-CSRF-Token")); hdr != "" {
			reqCSRF = hdr
		}
		if !validCSRFToken(csrfToken, reqCSRF) {
			http.Error(w, "forbidden: missing or invalid CSRF token", http.StatusForbidden)
			return
		}

		now := time.Now()
		var err error
		switch action {
		case "promote":
			_, err = outbox.PromoteIdea(vaultDir, id, now)
		case "snooze":
			dur := 24 * time.Hour
			if durationStr != "" {
				if parsed, dErr := time.ParseDuration(durationStr); dErr == nil && parsed > 0 {
					dur = parsed
				}
			}
			_, err = outbox.SnoozeIdea(vaultDir, id, dur, now)
		case "dismiss":
			_, err = outbox.DismissIdea(vaultDir, id, reason, now)
		default:
			err = fmt.Errorf("unsupported action %q", action)
		}
		_ = SyncMarkdownConsole(vaultDir)
		msg := fmt.Sprintf("Idea %s: %s", id, action)
		if err != nil {
			msg = fmt.Sprintf("Error updating idea %s: %v", id, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": err == nil, "message": msg})
	})

	mux.HandleFunc("/favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte(faviconSVG))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		state := loadState(vaultDir, "")
		state.CSRFToken = csrfToken
		renderHTML(w, state)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHostHeader(r.Host) {
			http.Error(w, "forbidden: non-loopback Host header rejected (DNS rebinding guard)", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

const preloadBridgeJS = `/**
 * Frontend bridge for Antigravity Sidecar & Standalone agy-hq Web UI.
 */
(function() {
  const params = new URLSearchParams(window.location.search);
  const token = params.get('token') || '';
  const conversationId = params.get('conversationId') || '';

  window.addEventListener('message', (event) => {
    if (event.data && event.data.type === 'theme-change') {
      const vars = event.data.payload && event.data.payload.cssVariables;
      if (vars) {
        for (const [key, value] of Object.entries(vars)) {
          document.documentElement.style.setProperty(key, value);
        }
      }
    }
  });

  window.sidecar = {
    token: token,
    conversationId: conversationId,
    fetch: function(url, options) {
      options = options || {};
      options.headers = options.headers || {};
      if (token && !options.headers['x-sidecar-token']) {
        options.headers['x-sidecar-token'] = token;
      }
      return fetch(url, options);
    }
  };
})();`

const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024" width="64" height="64"><defs><linearGradient id="bg" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="#1C202B"/><stop offset="100%" stop-color="#0E1017"/></linearGradient><radialGradient id="wl" cx="286" cy="398" r="240" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#8AE05C" stop-opacity="0.95"/><stop offset="60%" stop-color="#8AE05C" stop-opacity="0.35"/><stop offset="100%" stop-color="#378DFC" stop-opacity="0"/></radialGradient><radialGradient id="wc" cx="764" cy="462" r="230" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#F85852" stop-opacity="0.95"/><stop offset="65%" stop-color="#F85852" stop-opacity="0.35"/><stop offset="100%" stop-color="#378DFC" stop-opacity="0"/></radialGradient><radialGradient id="wo" cx="672" cy="344" r="210" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#FF9A26" stop-opacity="0.98"/><stop offset="60%" stop-color="#FF9A26" stop-opacity="0.40"/><stop offset="100%" stop-color="#F85852" stop-opacity="0"/></radialGradient><radialGradient id="ol" cx="383" cy="347" r="135" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#9CD64A" stop-opacity="0.95"/><stop offset="65%" stop-color="#9CD64A" stop-opacity="0.30"/><stop offset="100%" stop-color="#378DFC" stop-opacity="0"/></radialGradient><radialGradient id="oc" cx="553" cy="341" r="135" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#F65A4C" stop-opacity="0.95"/><stop offset="65%" stop-color="#F65A4C" stop-opacity="0.30"/><stop offset="100%" stop-color="#378DFC" stop-opacity="0"/></radialGradient><radialGradient id="oo" cx="468" cy="290" r="140" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#FC8E26" stop-opacity="1"/><stop offset="58%" stop-color="#FC8E26" stop-opacity="0.45"/><stop offset="100%" stop-color="#F65A4C" stop-opacity="0"/></radialGradient></defs><rect x="94" y="94" width="836" height="836" rx="192" fill="url(#bg)"/><path d="M 455 768 C 625 768, 745 535, 816 312 C 828 274, 790 262, 770 298 C 700 435, 605 616, 465 616 C 365 616, 295 578, 242 518 C 218 492, 186 518, 210 552 C 268 668, 345 768, 455 768 Z" fill="#378DFC"/><path d="M 455 768 C 625 768, 745 535, 816 312 C 828 274, 790 262, 770 298 C 700 435, 605 616, 465 616 C 365 616, 295 578, 242 518 C 218 492, 186 518, 210 552 C 268 668, 345 768, 455 768 Z" fill="url(#wl)"/><path d="M 455 768 C 625 768, 745 535, 816 312 C 828 274, 790 262, 770 298 C 700 435, 605 616, 465 616 C 365 616, 295 578, 242 518 C 218 492, 186 518, 210 552 C 268 668, 345 768, 455 768 Z" fill="url(#wc)"/><path d="M 455 768 C 625 768, 745 535, 816 312 C 828 274, 790 262, 770 298 C 700 435, 605 616, 465 616 C 365 616, 295 578, 242 518 C 218 492, 186 518, 210 552 C 268 668, 345 768, 455 768 Z" fill="url(#wo)"/><circle cx="468" cy="398" r="142" fill="#378DFC"/><circle cx="468" cy="398" r="142" fill="url(#ol)"/><circle cx="468" cy="398" r="142" fill="url(#oc)"/><circle cx="468" cy="398" r="142" fill="url(#oo)"/></svg>`

func extractFlagValue(cmd, flag string) string {
	parts := strings.Fields(cmd)
	prefix := flag + "="
	for _, p := range parts {
		if strings.HasPrefix(p, prefix) {
			val := strings.TrimPrefix(p, prefix)
			return strings.Trim(val, `"'`)
		}
	}
	for i, p := range parts {
		if p == flag && i+1 < len(parts) {
			return strings.Trim(parts[i+1], `"'`)
		}
	}
	return ""
}

func enrichPendingDraftPayload(vaultDir string, p *approvals.Proposal) {
	if p == nil {
		return
	}
	if p.Metadata == nil {
		p.Metadata = make(map[string]string)
	}

	draftID := p.Metadata["draftId"]
	account := p.Metadata["account"]
	if draftID == "" {
		draftID = extractFlagValue(p.Command, "--draft")
	}
	if account == "" {
		account = extractFlagValue(p.Command, "--account")
	}

	isGChat := p.ActionType == "gchat_send" || strings.Contains(p.Command, "gchat") || strings.HasPrefix(draftID, "gchat-draft-")
	isGmail := p.ActionType == "gmail_send" || strings.Contains(p.Command, "gmail") || strings.HasPrefix(draftID, "draft-")

	var payload string
	loadedFromDraft := false
	if isGChat && draftID != "" {
		if spec, err := gchat.GetDraft(vaultDir, account, draftID); err == nil && spec.Body != "" {
			payload = spec.Body
			loadedFromDraft = true
			if p.Metadata["spaceName"] == "" {
				p.Metadata["spaceName"] = spec.SpaceName
			}
			if p.Metadata["spaceId"] == "" {
				p.Metadata["spaceId"] = spec.SpaceID
			}
		}
	} else if isGmail && draftID != "" {
		if spec, err := gmail.GetDraft(vaultDir, account, draftID); err == nil && (spec.Body != "" || spec.Subject != "") {
			loadedFromDraft = true
			if spec.Subject != "" && !strings.Contains(spec.Body, spec.Subject) {
				payload = fmt.Sprintf("Subject: %s\n\n%s", spec.Subject, spec.Body)
			} else {
				payload = spec.Body
			}
		}
	}

	if payload == "" {
		if dp := p.Metadata["draftPayload"]; dp != "" {
			payload = dp
		} else if bp := p.Metadata["bodyPreview"]; bp != "" {
			payload = bp
		}
	}

	if payload != "" {
		p.Metadata["draftPayload"] = payload
		if loadedFromDraft || p.Metadata["bodyPreview"] == "" {
			p.Metadata["bodyPreview"] = payload
		}
		if isGChat && strings.HasPrefix(p.Summary, "Send Google Chat Draft ") {
			target := p.Metadata["spaceName"]
			if target == "" {
				target = p.Metadata["spaceId"]
			}
			if target != "" {
				prev := strings.Join(strings.Fields(strings.TrimSpace(payload)), " ")
				if runes := []rune(prev); len(runes) > 120 {
					prev = string(runes[:120]) + "..."
				}
				p.Summary = fmt.Sprintf("Send Google Chat to %s: %q", target, prev)
			}
		}
	}
}

func loadState(vaultDir, flashMsg string) DashboardState {
	pending, _ := approvals.List(vaultDir)
	for i := range pending {
		enrichPendingDraftPayload(vaultDir, &pending[i])
	}
	wts, _ := worktree.List(vaultDir)
	tasks, _ := kernel.ListTasks(vaultDir)
	layer1Tasks := kernel.FilterLayer1Tasks(vaultDir, tasks)
	now := time.Now()
	sel := kernel.SelectConsoleTasks(layer1Tasks, kernel.CountColdArchivedTasks(vaultDir), now)
	turns := loadTodayJournals(vaultDir)
	health := doctor.Run(vaultDir)
	quota := kernel.LoadQuota(vaultDir)
	obsSum := obs.BuildDashboardSummary(vaultDir, now)
	activeSpaces, _ := ActiveSpaces(vaultDir, now)
	allGoals, _ := kernel.ListGoals(vaultDir)
	var activeGoals []kernel.Goal
	var overdueWaits []kernel.OverdueWaitingSummary
	for _, g := range allGoals {
		if g.Status != "active" {
			continue
		}
		activeGoals = append(activeGoals, g)
		for _, wItem := range g.WaitingOn {
			if wItem.Status == "replied" {
				continue
			}
			isOverdue := wItem.Status == "nudge_queued" || wItem.Status == "overdue"
			if wItem.FollowUpAfter != "" {
				if ft, pErr := time.Parse(time.RFC3339, wItem.FollowUpAfter); pErr == nil && !now.Before(ft) {
					isOverdue = true
				}
			}
			if isOverdue {
				overdueWaits = append(overdueWaits, kernel.OverdueWaitingSummary{
					GoalSlug:  g.Slug,
					GoalTitle: g.Title,
					Scope:     g.Scope,
					Waiting:   wItem,
				})
			}
		}
	}
	ideas, _ := outbox.List(vaultDir, outbox.TierIdeasFeed, outbox.StatusPending, now)
	return DashboardState{
		VaultDir:      vaultDir,
		UpdatedAt:     now.Format("15:04:05"),
		Quota:         quota,
		Observability: obsSum,
		Pending:       pending,
		ActiveSpaces:  activeSpaces,
		Goals:         activeGoals,
		OverdueWaits:  overdueWaits,
		Ideas:         ideas,
		Worktrees:     wts,
		Tasks:         sel.VisibleRows,
		TodayTurns:    turns,
		Health:        health,
		FlashResult:   flashMsg,
	}
}

func loadTodayJournals(vaultDir string) []hooks.JournalEntry {
	dateDir := filepath.Join(vaultDir, "memory", "journal", time.Now().Format("2006-01-02"))
	entries, err := os.ReadDir(dateDir)
	if err != nil {
		return nil
	}
	var out []hooks.JournalEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dateDir, e.Name()))
		if err != nil {
			continue
		}
		var j hooks.JournalEntry
		if json.Unmarshal(data, &j) == nil {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp > out[j].Timestamp
	})
	return out
}

var pageTmpl = template.Must(template.New("console").Funcs(template.FuncMap{
	"shortSHA": func(s string) string {
		if len(s) > 12 {
			return s[:12]
		}
		return s
	},
	"draftPayload": func(p approvals.Proposal) string {
		if p.Metadata != nil {
			if dp := p.Metadata["draftPayload"]; dp != "" {
				return dp
			}
			if bp := p.Metadata["bodyPreview"]; bp != "" {
				return bp
			}
		}
		return ""
	},
	"base": filepath.Base,
	"mulf": func(a, b float64) float64 {
		return a * b
	},
	"divf": func(a, b int) float64 {
		if b == 0 {
			return 0
		}
		return float64(a) / float64(b)
	},
	"fmtTokens": obs.FormatTokensCompact,
	"pctClamp": func(p float64) float64 {
		if p < 0 {
			return 0
		}
		if p > 100 {
			return 100
		}
		return p
	},
	"ctxPct": func(cur int64) float64 {
		p := (float64(cur) / 256000.0) * 100.0
		if p > 100 {
			return 100
		}
		return p
	},
	"taskBadge": func(badges map[string]obs.TaskTokenBadge, convID string) string {
		if badges == nil {
			return ""
		}
		if b, ok := badges[strings.ToLower(strings.TrimSpace(convID))]; ok && b.GrossTokens > 0 {
			return b.CompactLabel
		}
		return ""
	},
}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="hq-csrf-token" content="{{.CSRFToken}}">
<link rel="icon" type="image/svg+xml" href="/favicon.svg">
<title>agy-hq Console</title>
<script src="/preload.js"></script>
<style>
  :root {
    --bg: var(--background, #0b0f19);
    --card: var(--card-bg, #111625);
    --card-hover: #161e31;
    --border: var(--border-color, #1e293b);
    --text: var(--foreground, #f1f5f9);
    --muted: #94a3b8;
    --accent: #38bdf8;
    --accent-glow: rgba(56, 189, 248, 0.15);
    --ok: #22c55e;
    --ok-glow: rgba(34, 197, 94, 0.18);
    --warn: #f59e0b;
    --warn-glow: rgba(245, 158, 11, 0.18);
    --danger: #ef4444;
    --danger-glow: rgba(239, 68, 68, 0.18);
    --font: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "JetBrains Mono", monospace;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    padding: 16px;
    background: var(--bg);
    color: var(--text);
    font-family: var(--font);
    font-size: 13px;
    line-height: 1.45;
  }
  header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    border-bottom: 1px solid var(--border);
    padding-bottom: 12px;
    margin-bottom: 16px;
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .brand h1 {
    font-size: 16px;
    font-weight: 700;
    letter-spacing: -0.01em;
    margin: 0;
    color: var(--text);
  }
  .header-actions {
    display: flex;
    align-items: center;
    gap: 12px;
  }
  .live-pill {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 3px 8px;
    border-radius: 999px;
    font-size: 11px;
    font-weight: 600;
    background: rgba(34, 197, 94, 0.12);
    border: 1px solid rgba(34, 197, 94, 0.35);
    color: #4ade80;
  }
  .live-dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: #22c55e;
    box-shadow: 0 0 8px #22c55e;
    animation: pulse 2s infinite;
  }
  @keyframes pulse {
    0%, 100% { opacity: 1; transform: scale(1); }
    50% { opacity: 0.4; transform: scale(0.85); }
  }
  h2 {
    font-size: 11px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--muted);
    margin: 18px 0 8px;
    display: flex;
    align-items: center;
    justify-content: space-between;
  }
  .card {
    background: var(--card);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 12px 14px;
    margin-bottom: 10px;
    transition: border-color 0.15s ease, background 0.15s ease;
  }
  .card:hover {
    border-color: rgba(56, 189, 248, 0.35);
  }
  .card.pending-hero {
    border-color: rgba(245, 158, 11, 0.45);
    background: linear-gradient(180deg, rgba(245, 158, 11, 0.04) 0%, var(--card) 100%);
  }
  .flash-banner {
    background: rgba(34, 197, 94, 0.12);
    border: 1px solid rgba(34, 197, 94, 0.4);
    color: #86efac;
    border-radius: 8px;
    padding: 10px 14px;
    margin-bottom: 14px;
    font-size: 12px;
    display: flex;
    align-items: center;
    justify-content: space-between;
  }
  .badge {
    display: inline-flex;
    align-items: center;
    padding: 2px 7px;
    border-radius: 4px;
    font-size: 11px;
    font-weight: 600;
    letter-spacing: 0.02em;
  }
  .badge-warn { background: var(--warn-glow); color: #fbbf24; border: 1px solid rgba(245, 158, 11, 0.3); }
  .badge-pass { background: var(--ok-glow); color: #4ade80; border: 1px solid rgba(34, 197, 94, 0.3); }
  .badge-info { background: var(--accent-glow); color: #38bdf8; border: 1px solid rgba(56, 189, 248, 0.3); }
  .badge-danger { background: var(--danger-glow); color: #f87171; border: 1px solid rgba(239, 68, 68, 0.3); }
  
  .preview-box {
    margin: 8px 0;
  }
  .preview-title {
    font-size: 11px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: #38bdf8;
    margin-bottom: 4px;
    display: flex;
    align-items: center;
    gap: 4px;
  }
  .preview-quote {
    background: #06080e;
    border-left: 3px solid #38bdf8;
    color: #f1f5f9;
    margin: 0;
    padding: 8px 12px;
    border-radius: 0 6px 6px 0;
    font-size: 12px;
    line-height: 1.45;
    white-space: pre-wrap;
    word-break: break-word;
    font-family: var(--font);
  }
  pre.code-block {
    background: #06080e;
    border: 1px solid rgba(30, 41, 59, 0.8);
    color: #38bdf8;
    padding: 8px 10px;
    border-radius: 6px;
    overflow-x: auto;
    margin: 8px 0;
    font-family: "JetBrains Mono", Consolas, monospace;
    font-size: 12px;
    line-height: 1.4;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    margin-top: 10px;
  }
  button, .btn {
    cursor: pointer;
    border: none;
    border-radius: 6px;
    padding: 6px 12px;
    font-size: 12px;
    font-weight: 600;
    display: inline-flex;
    align-items: center;
    gap: 6px;
    transition: transform 0.08s ease, opacity 0.15s ease, background 0.15s ease;
    text-decoration: none;
  }
  button:active, .btn:active {
    transform: scale(0.97);
  }
  .btn-approve {
    background: #16a34a;
    color: #ffffff;
    box-shadow: 0 0 10px rgba(34, 197, 94, 0.25);
  }
  .btn-approve:hover { background: #22c55e; }
  .btn-reject {
    background: transparent;
    border: 1px solid rgba(239, 68, 68, 0.5);
    color: #f87171;
  }
  .btn-reject:hover { background: rgba(239, 68, 68, 0.15); border-color: #ef4444; }
  .btn-secondary {
    background: #1e293b;
    border: 1px solid #334155;
    color: #cbd5e1;
  }
  .btn-secondary:hover { background: #334155; color: #ffffff; }
  .muted { color: var(--muted); font-size: 12px; }
  .meta-row { display: flex; align-items: center; justify-content: space-between; gap: 8px; font-size: 12px; }
  .progress-bar {
    width: 100%;
    height: 6px;
    background: #1e293b;
    border-radius: 3px;
    overflow: hidden;
    margin: 8px 0 4px;
  }
  .progress-fill {
    height: 100%;
    background: linear-gradient(90deg, #38bdf8 0%, #22c55e 100%);
    border-radius: 3px;
  }
  .kpi-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
    gap: 8px;
    margin-bottom: 10px;
  }
  .kpi-tile {
    background: #0c111d;
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 8px 10px;
  }
  .kpi-label {
    font-size: 10px;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--muted);
    margin-bottom: 2px;
  }
  .kpi-val {
    font-size: 14px;
    font-weight: 700;
    color: var(--text);
  }
  .kpi-sub {
    font-size: 11px;
    color: var(--muted);
    margin-top: 2px;
  }
  a { color: var(--accent); text-decoration: none; }
  a:hover { text-decoration: underline; }
</style>
</head>
<body>
<header>
  <div class="brand">
    <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024" width="24" height="24" style="flex-shrink:0">
      <defs>
        <radialGradient id="hq-wl" cx="286" cy="398" r="240" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#8AE05C" stop-opacity="0.95"/><stop offset="60%" stop-color="#8AE05C" stop-opacity="0.35"/><stop offset="100%" stop-color="#378DFC" stop-opacity="0"/></radialGradient>
        <radialGradient id="hq-wc" cx="764" cy="462" r="230" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#F85852" stop-opacity="0.95"/><stop offset="65%" stop-color="#F85852" stop-opacity="0.35"/><stop offset="100%" stop-color="#378DFC" stop-opacity="0"/></radialGradient>
        <radialGradient id="hq-wo" cx="672" cy="344" r="210" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#FF9A26" stop-opacity="0.98"/><stop offset="60%" stop-color="#FF9A26" stop-opacity="0.40"/><stop offset="100%" stop-color="#F85852" stop-opacity="0"/></radialGradient>
        <radialGradient id="hq-ol" cx="383" cy="347" r="135" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#9CD64A" stop-opacity="0.95"/><stop offset="65%" stop-color="#9CD64A" stop-opacity="0.30"/><stop offset="100%" stop-color="#378DFC" stop-opacity="0"/></radialGradient>
        <radialGradient id="hq-oc" cx="553" cy="341" r="135" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#F65A4C" stop-opacity="0.95"/><stop offset="65%" stop-color="#F65A4C" stop-opacity="0.30"/><stop offset="100%" stop-color="#378DFC" stop-opacity="0"/></radialGradient>
        <radialGradient id="hq-oo" cx="468" cy="290" r="140" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#FC8E26" stop-opacity="1"/><stop offset="58%" stop-color="#FC8E26" stop-opacity="0.45"/><stop offset="100%" stop-color="#F65A4C" stop-opacity="0"/></radialGradient>
      </defs>
      <path d="M 455 768 C 625 768, 745 535, 816 312 C 828 274, 790 262, 770 298 C 700 435, 605 616, 465 616 C 365 616, 295 578, 242 518 C 218 492, 186 518, 210 552 C 268 668, 345 768, 455 768 Z" fill="#378DFC"/>
      <path d="M 455 768 C 625 768, 745 535, 816 312 C 828 274, 790 262, 770 298 C 700 435, 605 616, 465 616 C 365 616, 295 578, 242 518 C 218 492, 186 518, 210 552 C 268 668, 345 768, 455 768 Z" fill="url(#hq-wl)"/>
      <path d="M 455 768 C 625 768, 745 535, 816 312 C 828 274, 790 262, 770 298 C 700 435, 605 616, 465 616 C 365 616, 295 578, 242 518 C 218 492, 186 518, 210 552 C 268 668, 345 768, 455 768 Z" fill="url(#hq-wc)"/>
      <path d="M 455 768 C 625 768, 745 535, 816 312 C 828 274, 790 262, 770 298 C 700 435, 605 616, 465 616 C 365 616, 295 578, 242 518 C 218 492, 186 518, 210 552 C 268 668, 345 768, 455 768 Z" fill="url(#hq-wo)"/>
      <circle cx="468" cy="398" r="142" fill="#378DFC"/>
      <circle cx="468" cy="398" r="142" fill="url(#hq-ol)"/>
      <circle cx="468" cy="398" r="142" fill="url(#hq-oc)"/>
      <circle cx="468" cy="398" r="142" fill="url(#hq-oo)"/>
    </svg>
    <h1>agy-hq Console</h1>
  </div>
  <div class="header-actions">
    <span class="live-pill"><span class="live-dot"></span>LIVE</span>
    <span class="muted" id="last-updated">Updated {{.UpdatedAt}}</span>
    <button type="button" class="btn btn-secondary" onclick="refreshState()" style="padding:4px 8px;font-size:11px">🔄 Refresh</button>
  </div>
</header>

<div id="flash-container">
  {{if .FlashResult}}
  <div class="flash-banner" id="flash-banner">
    <span>{{.FlashResult}}</span>
    <button type="button" class="btn" onclick="this.parentElement.remove()" style="background:none;color:inherit;padding:0 4px">✕</button>
  </div>
  {{end}}
</div>

<h2>
  <span>Pending Approvals (<span id="pending-count">{{len .Pending}}</span>)</span>
  <span class="muted" style="text-transform:none;font-weight:normal;font-size:11px">1-Tap Remote Authorization</span>
</h2>
<div id="pending-list">
{{if .Pending}}
  {{range .Pending}}
  <div class="card pending-hero" id="proposal-{{.ID}}">
    <div class="meta-row">
      <div><strong style="font-size:13px">{{.Summary}}</strong></div>
      <span class="badge badge-warn">{{.ActionType}}</span>
    </div>
    <div class="muted" style="margin:4px 0">
      ID: <code>{{.ID}}</code> · SHA-256: <code title="{{.SHA256}}">{{shortSHA .SHA256}}</code>{{if .ExpiresAt}} · Expires: <code>{{.ExpiresAt}}</code>{{end}}
    </div>
    {{if .RiskFlags}}
    <div style="margin:6px 0">
      <span class="badge badge-danger">⚠️ Contextual PII Warning: {{range $i, $f := .RiskFlags}}{{if $i}}, {{end}}{{$f}}{{end}}</span>
    </div>
    {{end}}
    {{if draftPayload .}}
    <div class="preview-box">
      <div class="preview-title">💬 Message Preview</div>
      <blockquote class="preview-quote">{{draftPayload .}}</blockquote>
    </div>
    {{end}}
    <pre class="code-block">{{.Command}}</pre>
    <div class="actions">
      <button type="button" class="btn btn-approve" data-hq-action="approve" data-id="{{.ID}}" data-sha="{{shortSHA .SHA256}}">
        ✓ Approve ({{shortSHA .SHA256}})
      </button>
      <button type="button" class="btn btn-reject" data-hq-action="reject" data-id="{{.ID}}">
        ✕ Reject
      </button>
    </div>
  </div>
  {{end}}
{{else}}
  <div class="card muted" id="no-pending">No pending outbound actions requiring authorization.</div>
{{end}}
</div>

<h2>Proactive Gatekeeper & Daily Quota</h2>
<div class="card" id="quota-card">
  <div class="meta-row">
    <div><strong>Background Flash Spawns Today:</strong> <span id="quota-spawns">{{.Quota.SpawnsToday}}</span> / {{.Quota.MaxDailySpawns}}</div>
    <div>
      {{if .Quota.CircuitOpen}}
        <span class="badge badge-warn">CIRCUIT OPEN ({{.Quota.CircuitReason}})</span>
      {{else}}
        <span class="badge badge-pass">BUDGET OK</span>
      {{end}}
    </div>
  </div>
  <div class="progress-bar">
    <div class="progress-fill" style="width: {{if gt .Quota.MaxDailySpawns 0}}{{printf "%.1f" (mulf (divf .Quota.SpawnsToday .Quota.MaxDailySpawns) 100.0)}}{{else}}0{{end}}%;"></div>
  </div>
  <div class="muted" style="margin-top:6px">Last Heartbeat: {{if .Quota.LastStatus}}{{.Quota.LastStatus}}{{else}}Not run yet today{{end}}</div>
</div>

<h2>
  <span>Token &amp; Context Observability</span>
  <span class="muted" style="text-transform:none;font-weight:normal;font-size:11px">RFC-0012 Telemetry</span>
</h2>
<div class="card" id="obs-card">
  <div class="kpi-grid">
    <div class="kpi-tile">
      <div class="kpi-label">Today Uncached Burn</div>
      <div class="kpi-val" id="obs-today-uncached">{{fmtTokens .Observability.TodayUncachedTokens}} / {{fmtTokens .Observability.Budget.DailyUncachedBudget}}</div>
      <div class="kpi-sub">{{printf "%.1f" .Observability.DailyBudgetPct}}% of daily budget ({{.Observability.TodayTurnCount}} turns)</div>
    </div>
    <div class="kpi-tile">
      <div class="kpi-label">Prefix Cache Hit Ratio</div>
      <div class="kpi-val" id="obs-chr">{{printf "%.1f" (mulf .Observability.TodayCacheHitRatio 100.0)}}%</div>
      <div class="kpi-sub">All-time: {{printf "%.1f" (mulf .Observability.AllTimeCacheHitRatio 100.0)}}% ({{fmtTokens .Observability.AllTimeGrossTokens}} gross)</div>
    </div>
    <div class="kpi-tile">
      <div class="kpi-label">Front Desk Context (256k Cap)</div>
      <div class="kpi-val" id="obs-fd-ctx">{{fmtTokens .Observability.FrontDeskContextTokens}} / 256k</div>
      <div class="kpi-sub">Status: {{.Observability.FrontDeskContextStatus}} · Compactions: {{.Observability.FrontDeskCompactions}}</div>
    </div>
    <div class="kpi-tile">
      <div class="kpi-label">Deep Tier Burn Today</div>
      <div class="kpi-val" id="obs-deep-burn">{{fmtTokens .Observability.TodayDeepUncachedTokens}} / {{fmtTokens .Observability.Budget.DailyDeepUncachedBudget}}</div>
      <div class="kpi-sub">{{printf "%.0f" .Observability.TodayQuotaUnits}} QBU today</div>
    </div>
  </div>
  <div class="meta-row" style="margin-top:4px">
    <span class="muted">Front Desk Context Fill ({{printf "%.1f" (ctxPct .Observability.FrontDeskContextTokens)}}% of ~256k auto-compaction ceiling)</span>
    <span class="badge {{if eq .Observability.FrontDeskContextStatus "OK"}}badge-pass{{else if eq .Observability.FrontDeskContextStatus "WARN"}}badge-warn{{else}}badge-danger{{end}}">{{.Observability.FrontDeskContextStatus}}</span>
  </div>
  <div class="progress-bar">
    <div class="progress-fill" style="width: {{printf "%.1f" (ctxPct .Observability.FrontDeskContextTokens)}}%;"></div>
  </div>
  {{if .Observability.Anomalies}}
  <div style="margin-top:8px" id="obs-anomalies">
    {{range .Observability.Anomalies}}
    <div style="margin-top:4px;font-size:11px">
      <span class="badge {{if eq .Severity "CRITICAL"}}badge-danger{{else}}badge-warn{{end}}">{{.Code}} · {{.Severity}}</span>
      <span>{{.Summary}}</span>
    </div>
    {{end}}
  </div>
  {{end}}
</div>

<h2>
  <span>Live Task Board (<span id="task-count">{{len .Tasks}}</span>)</span>
  <span class="muted" style="text-transform:none;font-weight:normal;font-size:11px">Layer-1 Coordinated Tasks</span>
</h2>
<div id="tasks-list">
{{if .Tasks}}
  {{range .Tasks}}
  <div class="card" id="task-{{.TaskSlug}}">
    <div class="meta-row">
      <strong>{{.Title}}</strong>
      <span class="badge {{if eq .Status "active"}}badge-pass{{else}}badge-warn{{end}}">{{.Status}}</span>
    </div>
    <div class="muted" style="margin:4px 0">
      Slug: <code>{{.TaskSlug}}</code> · Model: {{.Model}} · Conv: <a href="conversation://{{.ConversationID}}">{{.ConversationID}}</a>{{if taskBadge $.Observability.TaskBadges .ConversationID}} · Tokens: <code>{{taskBadge $.Observability.TaskBadges .ConversationID}}</code>{{end}}
    </div>
    {{if .LastSummary}}<div class="muted" style="margin:4px 0 8px;color:#cbd5e1">{{.LastSummary}}</div>{{end}}
    <div class="actions">
      {{if eq .Status "active"}}
      <button type="button" class="btn btn-approve" style="font-size:11px;padding:4px 8px" data-hq-action="complete-task" data-slug="{{.TaskSlug}}">Complete</button>
      {{end}}
      {{if ne .Status "archived"}}
      <button type="button" class="btn btn-secondary" style="font-size:11px;padding:4px 8px" data-hq-action="archive-task" data-slug="{{.TaskSlug}}">Archive</button>
      {{end}}
    </div>
  </div>
  {{end}}
{{else}}
  <div class="card muted">No active tasks on the live board.</div>
{{end}}
</div>

<h2>Active Parallel Git Worktrees ({{len .Worktrees}})</h2>
<div id="worktrees-list">
{{if .Worktrees}}
  {{range .Worktrees}}
  <div class="card"><code style="color:var(--accent)">{{base .}}</code> <span class="muted">({{.}})</span></div>
  {{end}}
{{else}}
  <div class="card muted">No active git worktrees in local/worktrees/.</div>
{{end}}
</div>

{{if .ActiveSpaces}}
<h2>Active Console Spaces ({{len .ActiveSpaces}})</h2>
<div id="spaces-list">
  {{range .ActiveSpaces}}
  <div class="card" id="space-{{.ID}}">
    <div class="meta-row">
      <strong>🗂️ {{.Title}}</strong>
      <div>
        {{if eq .OverrideMode "pin"}}<span class="badge badge-info">PINNED</span> {{end}}
        <span class="badge badge-pass">{{.ID}}</span>
      </div>
    </div>
    <div class="muted" style="margin:4px 0">Schedule: <code>{{.Schedule}}</code>{{if .Scope}} · Scope: <code>{{.Scope}}</code>{{end}}</div>
    {{range .RenderedCards}}
    <div style="margin-top:8px">
      <div class="kpi-label">{{.Title}}</div>
      {{range .Lines}}<div class="muted" style="color:#cbd5e1">• {{.}}</div>{{end}}
    </div>
    {{end}}
    <div class="actions">
      <button type="button" class="btn btn-secondary" style="font-size:11px;padding:4px 8px" data-hq-action="space-override" data-id="{{.ID}}" data-mode="hide">Hide Space</button>
      {{if .OverrideMode}}<button type="button" class="btn btn-secondary" style="font-size:11px;padding:4px 8px" data-hq-action="space-override" data-id="{{.ID}}" data-mode="auto">Reset Schedule</button>{{end}}
    </div>
  </div>
  {{end}}
</div>
{{end}}

<h2>Proactive Ideas Feed (<span id="ideas-count">{{len .Ideas}}</span>)</h2>
<div id="ideas-list">
{{if .Ideas}}
  {{range .Ideas}}
  <div class="card" id="idea-{{.ID}}">
    <div class="meta-row">
      <strong>💡 {{.Title}}</strong>
      <span class="badge badge-info">{{if .SourceSkill}}{{.SourceSkill}}{{else}}idea{{end}}</span>
    </div>
    {{if .Summary}}<div class="muted" style="margin:4px 0;color:#cbd5e1">{{.Summary}}</div>{{end}}
    <div class="actions">
      <button type="button" class="btn btn-approve" style="font-size:11px;padding:4px 8px" data-hq-action="idea-action" data-id="{{.ID}}" data-idea-op="promote">Promote</button>
      <button type="button" class="btn btn-secondary" style="font-size:11px;padding:4px 8px" data-hq-action="idea-action" data-id="{{.ID}}" data-idea-op="snooze">Snooze 24h</button>
      <button type="button" class="btn btn-reject" style="font-size:11px;padding:4px 8px" data-hq-action="idea-action" data-id="{{.ID}}" data-idea-op="dismiss">Dismiss</button>
    </div>
  </div>
  {{end}}
{{else}}
  <div class="card muted">No pending proactive ideas in outbox.</div>
{{end}}
</div>

<h2>Today's Turn Journal ({{len .TodayTurns}})</h2>
<div id="journal-list">
{{if .TodayTurns}}
  {{range .TodayTurns}}
  <div class="card">
    <div><strong>{{.UserPrompt}}</strong></div>
    <div class="muted">{{.Timestamp}} · Conv: <a href="conversation://{{.ConversationID}}">{{.ConversationID}}</a> · {{.ModelName}}</div>
  </div>
  {{end}}
{{else}}
  <div class="card muted">No turns logged today yet.</div>
{{end}}
</div>

<h2>System Health (Doctor)</h2>
<div id="health-grid">
{{range .Health}}
<div class="card" style="margin-bottom:6px">
  <div class="meta-row">
    <strong>{{.Name}}</strong>
    <span class="badge {{if eq .Status "PASS"}}badge-pass{{else}}badge-warn{{end}}">{{.Status}}</span>
  </div>
  <div class="muted" style="margin-top:2px">{{.Detail}}</div>
</div>
{{end}}
</div>

<script>
function showFlash(message, isError) {
  const container = document.getElementById('flash-container');
  const banner = document.createElement('div');
  banner.className = 'flash-banner';
  if (isError) {
    banner.style.borderColor = 'rgba(239, 68, 68, 0.4)';
    banner.style.background = 'rgba(239, 68, 68, 0.12)';
    banner.style.color = '#fca5a5';
  }
  banner.innerHTML = '<span>' + escapeHTML(message) + '</span><button type="button" class="btn" onclick="this.parentElement.remove()" style="background:none;color:inherit;padding:0 4px">✕</button>';
  container.innerHTML = '';
  container.appendChild(banner);
  setTimeout(() => { if (banner.parentElement) banner.remove(); }, 5000);
}

const CSRF_TOKEN = ((document.querySelector('meta[name="hq-csrf-token"]') || {}).content) || '';

function escapeHTML(s) {
  return String(s || '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function postHeaders() {
  return {
    'Content-Type': 'application/json',
    'Accept': 'application/json',
    'X-HQ-CSRF-Token': CSRF_TOKEN
  };
}

async function approveProposal(id, sha) {
  if (!confirm('Execute approved action for proposal ' + id + ' (sha256: ' + sha + ')?')) return;
  try {
    const res = await fetch('/api/approve', {
      method: 'POST',
      headers: postHeaders(),
      body: JSON.stringify({ id: id, sha256: sha, csrf_token: CSRF_TOKEN })
    });
    const data = await res.json();
    showFlash(data.message || ('Approved ' + id), !data.ok);
    await refreshState();
  } catch (err) {
    showFlash('Error approving ' + id + ': ' + err.message, true);
  }
}

async function rejectProposal(id) {
  const reason = prompt('Reason for rejecting ' + id + ' (optional):', 'Rejected via HQ Console');
  if (reason === null) return;
  try {
    const res = await fetch('/api/reject', {
      method: 'POST',
      headers: postHeaders(),
      body: JSON.stringify({ id: id, reason: reason, csrf_token: CSRF_TOKEN })
    });
    const data = await res.json();
    showFlash(data.message || ('Rejected ' + id), !data.ok);
    await refreshState();
  } catch (err) {
    showFlash('Error rejecting ' + id + ': ' + err.message, true);
  }
}

async function completeTask(slug) {
  try {
    const res = await fetch('/api/tasks/complete', {
      method: 'POST',
      headers: postHeaders(),
      body: JSON.stringify({ slug: slug, csrf_token: CSRF_TOKEN })
    });
    const data = await res.json();
    showFlash(data.message || ('Completed ' + slug), !data.ok);
    await refreshState();
  } catch (err) {
    showFlash('Error completing ' + slug + ': ' + err.message, true);
  }
}

async function archiveTask(slug) {
  try {
    const res = await fetch('/api/tasks/archive', {
      method: 'POST',
      headers: postHeaders(),
      body: JSON.stringify({ slug: slug, csrf_token: CSRF_TOKEN })
    });
    const data = await res.json();
    showFlash(data.message || ('Archived ' + slug), !data.ok);
    await refreshState();
  } catch (err) {
    showFlash('Error archiving ' + slug + ': ' + err.message, true);
  }
}

document.addEventListener('click', function(ev) {
  const btn = ev.target && ev.target.closest ? ev.target.closest('[data-hq-action]') : null;
  if (!btn) return;
  const act = btn.getAttribute('data-hq-action');
  if (act === 'approve') {
    approveProposal(btn.getAttribute('data-id') || '', btn.getAttribute('data-sha') || '');
  } else if (act === 'reject') {
    rejectProposal(btn.getAttribute('data-id') || '');
  } else if (act === 'complete-task') {
    completeTask(btn.getAttribute('data-slug') || '');
  } else if (act === 'archive-task') {
    archiveTask(btn.getAttribute('data-slug') || '');
  } else if (act === 'idea-action') {
    const id = btn.getAttribute('data-id') || '';
    const op = btn.getAttribute('data-idea-op') || '';
    let reason = '';
    if (op === 'dismiss') {
      reason = prompt('Optional reason for dismissing idea ' + id + ':', 'Not relevant right now') || '';
    }
    fetch('/api/ideas/action', {
      method: 'POST',
      headers: postHeaders(),
      body: JSON.stringify({ id: id, action: op, reason: reason, duration: '24h', csrf_token: CSRF_TOKEN })
    }).then(r => r.json()).then(d => { showFlash(d.message || ('Updated ' + id), !d.ok); refreshState(); });
  } else if (act === 'space-override') {
    const id = btn.getAttribute('data-id') || '';
    const mode = btn.getAttribute('data-mode') || 'auto';
    fetch('/api/console-spaces/set', {
      method: 'POST',
      headers: postHeaders(),
      body: JSON.stringify({ id: id, mode: mode, csrf_token: CSRF_TOKEN })
    }).then(r => r.json()).then(d => { showFlash(d.message || ('Updated space ' + id), !d.ok); location.reload(); });
  }
});

async function refreshState() {
  try {
    const res = await fetch('/api/state', { headers: { 'Accept': 'application/json' } });
    if (!res.ok) return;
    const state = await res.json();
    document.getElementById('last-updated').textContent = 'Updated ' + state.updatedAt;

    // Update pending approvals
    const pendingList = document.getElementById('pending-list');
    document.getElementById('pending-count').textContent = state.pending ? state.pending.length : 0;
    if (state.pending && state.pending.length > 0) {
      let html = '';
      for (const p of state.pending) {
        const shortSha = p.sha256 ? p.sha256.substring(0, 12) : '';
        const preview = (p.metadata && (p.metadata.draftPayload || p.metadata.bodyPreview)) ? (p.metadata.draftPayload || p.metadata.bodyPreview) : '';
        const previewHtml = preview ?
          '<div class="preview-box">' +
          '<div class="preview-title">💬 Message Preview</div>' +
          '<blockquote class="preview-quote">' + escapeHTML(preview) + '</blockquote>' +
          '</div>' : '';

        html += '<div class="card pending-hero" id="proposal-' + escapeHTML(p.id) + '">' +
          '<div class="meta-row"><div><strong style="font-size:13px">' + escapeHTML(p.summary) + '</strong></div>' +
          '<span class="badge badge-warn">' + escapeHTML(p.actionType) + '</span></div>' +
          '<div class="muted" style="margin:4px 0">ID: <code>' + escapeHTML(p.id) + '</code> · SHA-256: <code title="' + escapeHTML(p.sha256) + '">' + escapeHTML(shortSha) + '</code></div>' +
          previewHtml +
          '<pre class="code-block">' + escapeHTML(p.command) + '</pre>' +
          '<div class="actions">' +
          '<button type="button" class="btn btn-approve" data-hq-action="approve" data-id="' + escapeHTML(p.id) + '" data-sha="' + escapeHTML(shortSha) + '">✓ Approve (' + escapeHTML(shortSha) + ')</button>' +
          '<button type="button" class="btn btn-reject" data-hq-action="reject" data-id="' + escapeHTML(p.id) + '">✕ Reject</button>' +
          '</div></div>';
      }
      pendingList.innerHTML = html;
    } else {
      pendingList.innerHTML = '<div class="card muted" id="no-pending">No pending outbound actions requiring authorization.</div>';
    }

    // Update tasks
    const tasksList = document.getElementById('tasks-list');
    document.getElementById('task-count').textContent = state.tasks ? state.tasks.length : 0;
    if (state.tasks && state.tasks.length > 0) {
      let html = '';
      for (const t of state.tasks) {
        const badgeClass = t.status === 'active' ? 'badge-pass' : 'badge-warn';
        html += '<div class="card" id="task-' + escapeHTML(t.taskSlug) + '">' +
          '<div class="meta-row"><strong>' + escapeHTML(t.title) + '</strong>' +
          '<span class="badge ' + badgeClass + '">' + escapeHTML(t.status) + '</span></div>' +
          '<div class="muted" style="margin:4px 0">Slug: <code>' + escapeHTML(t.taskSlug) + '</code> · Model: ' + escapeHTML(t.model) + ' · Conv: <a href="conversation://' + escapeHTML(t.conversationId) + '">' + escapeHTML(t.conversationId) + '</a></div>' +
          (t.lastSummary ? '<div class="muted" style="margin:4px 0 8px;color:#cbd5e1">' + escapeHTML(t.lastSummary) + '</div>' : '') +
          '<div class="actions">' +
          (t.status === 'active' ? '<button type="button" class="btn btn-approve" style="font-size:11px;padding:4px 8px" data-hq-action="complete-task" data-slug="' + escapeHTML(t.taskSlug) + '">Complete</button>' : '') +
          (t.status !== 'archived' ? '<button type="button" class="btn btn-secondary" style="font-size:11px;padding:4px 8px" data-hq-action="archive-task" data-slug="' + escapeHTML(t.taskSlug) + '">Archive</button>' : '') +
          '</div></div>';
      }
      tasksList.innerHTML = html;
    } else {
      tasksList.innerHTML = '<div class="card muted">No active tasks on the live board.</div>';
    }

    // Update Quota spawns
    if (state.quota) {
      const qSpan = document.getElementById('quota-spawns');
      if (qSpan) qSpan.textContent = state.quota.spawnsToday;
    }
  } catch (err) {
    console.warn('background state refresh failed:', err);
  }
}

// Auto-refresh state every 3 seconds for real-time responsiveness
setInterval(refreshState, 3000);
</script>
</body>
</html>`))

func renderHTML(w http.ResponseWriter, state DashboardState) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTmpl.Execute(w, state)
}

// RecordArtifactDir stores the active conversation's artifactDirectoryPath so CLI commands
// (`hq propose`, `hq approve`, `hq reject`, `hq worktree`, `hq heartbeat`) can deterministically
// keep `hq_console.md` in sync on every state mutation.
func RecordArtifactDir(vaultDir, artifactDir string) {
	if artifactDir == "" {
		return
	}
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755)
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "active_artifact_dir.txt"), []byte(strings.TrimSpace(artifactDir)), 0o644)
}

// SyncMarkdownConsole deterministically regenerates `local/CONSOLE.md` and (if present)
// `<artifactDirectoryPath>/hq_console.md` from live Vault state in 0 LLM tokens.
func SyncMarkdownConsole(vaultDir string) error {
	kernel.ReconcileFrontDeskFromTranscript(vaultDir, "")
	state := loadState(vaultDir, "")
	var b strings.Builder

	fmt.Fprintf(&b, "# 🏛️ agy-hq Console & Live Status (%s)\n\n", time.Now().Format("2006-01-02 15:04:05"))
	b.WriteString("[🎛️ Open Interactive Console (Side Pane)](sidecar://agy-hq-console/) · [🌐 Browser UI](http://127.0.0.1:4242)\n\n")

	if len(state.Pending) > 0 {
		fmt.Fprintf(&b, "> [!IMPORTANT]\n> **Pending Approvals (`%d` item(s) waiting):**\n", len(state.Pending))
		for _, p := range state.Pending {
			short := p.SHA256
			if len(short) > 12 {
				short = short[:12]
			}
			piiBadge := ""
			if len(p.RiskFlags) > 0 {
				piiBadge = fmt.Sprintf(" · ⚠️ **PII WARNING [%s]**", strings.Join(p.RiskFlags, ", "))
			}
			expBadge := ""
			if p.ExpiresAt != "" {
				expBadge = fmt.Sprintf(" · `expires:%s`", p.ExpiresAt)
			}
			fmt.Fprintf(&b, "> - **`%s`** (`%s` · `sha256:%s`%s%s): %s — `%s`\n", p.ID, p.ActionType, short, expBadge, piiBadge, p.Summary, p.Command)
			payload := ""
			if p.Metadata != nil {
				if dp := p.Metadata["draftPayload"]; dp != "" {
					payload = dp
				} else if bp := p.Metadata["bodyPreview"]; bp != "" {
					payload = bp
				}
			}
			if payload != "" {
				previewText := payload
				if runes := []rune(previewText); len(runes) > 600 {
					previewText = string(runes[:600]) + "..."
				}
				b.WriteString(">   > **Message Preview:**\n")
				lines := strings.Split(strings.TrimSpace(previewText), "\n")
				for _, line := range lines {
					fmt.Fprintf(&b, ">   > %s\n", line)
				}
			}
		}
		b.WriteString("\n---\n\n")
	} else {
		b.WriteString("> [!NOTE]\n> **Pending Approvals (`0` items):** All staged actions have been resolved.\n\n---\n\n")
	}

	b.WriteString("## 📊 1. Front Desk Lease & Daily Quota\n")
	if fd, ok := kernel.GetFrontDesk(vaultDir); ok {
		fmt.Fprintf(&b, "- **Active Front Desk:** [`[HQ] Front Desk`](conversation://%s) (`%s`)\n", fd.ConversationID, fd.ConversationID)
		pinStatus := "⚠️ Pending Turn-1 `write_file` pin"
		if fd.ConsolePinned {
			pinStatus = "✅ Pinned in UI"
		}
		fmt.Fprintf(&b, "- **Console Artifact Pin:** `%s`\n", pinStatus)
		armed, unarmed := kernel.GetUnarmedTriggers(vaultDir)
		totalTriggers := len(armed) + len(unarmed)
		if totalTriggers > 0 {
			fmt.Fprintf(&b, "- **Proactive Schedules (`triggers.yaml`):** `%d / %d armed in Front Desk`\n", len(armed), totalTriggers)
		}
	} else {
		b.WriteString("- **Active Front Desk:** `VACANT` *(the next new conversation opened in `headquarter` will automatically claim `[HQ] Front Desk`)*\n")
	}
	circuitBadge := "BUDGET OK"
	if state.Quota.CircuitOpen {
		circuitBadge = "CIRCUIT OPEN: " + state.Quota.CircuitReason
	}
	gatekeeperNote := ""
	if state.Quota.GatekeeperSavesToday > 0 {
		gatekeeperNote = fmt.Sprintf(" · 0-Token Gatekeeper Skips: `%d`", state.Quota.GatekeeperSavesToday)
	}
	fmt.Fprintf(&b, "- **Background Flash Spawns Today:** `%d / %d` (`%s`%s)\n", state.Quota.SpawnsToday, state.Quota.MaxDailySpawns, circuitBadge, gatekeeperNote)
	fmt.Fprintf(&b, "- **Token Burn Today (Uncached / Budget):** `%s / %s` (`%.1f%%` · Gross: `%s` · Prefix CHR: `%.1f%%` · Deep: `%s / %s`)\n",
		obs.FormatTokensCompact(state.Observability.TodayUncachedTokens),
		obs.FormatTokensCompact(state.Observability.Budget.DailyUncachedBudget),
		state.Observability.DailyBudgetPct,
		obs.FormatTokensCompact(state.Observability.TodayGrossTokens),
		state.Observability.TodayCacheHitRatio*100.0,
		obs.FormatTokensCompact(state.Observability.TodayDeepUncachedTokens),
		obs.FormatTokensCompact(state.Observability.Budget.DailyDeepUncachedBudget),
	)
	fmt.Fprintf(&b, "- **Front Desk Context Watermark:** `%s / 256k` (`%s` · Compactions: `%d` · CHR: `%.1f%%`)\n",
		obs.FormatTokensCompact(state.Observability.FrontDeskContextTokens),
		state.Observability.FrontDeskContextStatus,
		state.Observability.FrontDeskCompactions,
		state.Observability.FrontDeskCacheHitRatio*100.0,
	)
	lastHB := state.Quota.LastStatus
	if lastHB == "" {
		lastHB = "Not run yet today"
	}
	fmt.Fprintf(&b, "- **Last Heartbeat:** `%s`\n\n---\n\n", lastHB)

	now := time.Now()
	allTasks, _ := kernel.ListTasks(vaultDir)
	layer1Tasks := kernel.FilterLayer1Tasks(vaultDir, allTasks)
	coldCount := kernel.CountColdArchivedTasks(vaultDir)
	sel := kernel.SelectConsoleTasks(layer1Tasks, coldCount, now)

	if len(layer1Tasks) == 0 && coldCount == 0 {
		fmt.Fprintf(&b, "## 📋 2. Task Board (`0` Tracked Conversations)\n")
		b.WriteString("- None registered in `local/memory/tasks.json`.\n\n---\n\n")
	} else {
		fmt.Fprintf(&b, "## 📋 2. Task Board (`%d` Active · `%d` Idle · `%d` Completed · `%d` Archived)\n",
			sel.ActiveCount, sel.IdleCount, sel.CompletedCount, sel.ArchivedHotCount+sel.ArchivedColdCount)
		if len(sel.VisibleRows) == 0 {
			b.WriteString("- No active or recently completed tasks on the live board.\n")
		} else {
			b.WriteString("| Status | Task Conversation | Slug | Latest Progress |\n| :--- | :--- | :--- | :--- |\n")
			for _, t := range sel.VisibleRows {
				cleanTitle := kernel.SanitizeTaskTitle(t.Title, t.TaskSlug)
				oneLine := strings.Join(strings.Fields(t.LastSummary), " ")
				oneLine = strings.ReplaceAll(oneLine, "|", "\\|")
				if runes := []rune(oneLine); len(runes) > 130 {
					oneLine = string(runes[:130]) + "..."
				}
				eff := t.EffectiveStatus(now)
				badge := "🟢 `active`"
				switch eff {
				case "idle":
					badge = "⏸️ `idle`"
				case "stalled":
					badge = "⚠️ `stalled`"
				case "completed":
					badge = "✅ `completed`"
				case "cancelled":
					badge = "🛑 `cancelled`"
				case "archived":
					badge = "📦 `archived`"
				}
				if t.Pinned && eff == "active" {
					badge = "📌 🟢 `active`"
				}
				slugCell := fmt.Sprintf("`%s`", t.TaskSlug)
				var metaParts []string
				if age := t.FormatRelativeAge(now); age != "" {
					metaParts = append(metaParts, age)
				}
				if t.WorktreeDir != "" {
					if info, sErr := os.Stat(t.WorktreeDir); sErr == nil && info.IsDir() {
						metaParts = append(metaParts, "🌿 wt")
					}
				}
				if tb, ok := state.Observability.TaskBadges[strings.ToLower(strings.TrimSpace(t.ConversationID))]; ok && tb.GrossTokens > 0 {
					metaParts = append(metaParts, "🪙 "+obs.FormatTokensCompact(tb.TreeUncachedTokens))
				}
				if len(metaParts) > 0 {
					slugCell += fmt.Sprintf(" · *%s*", strings.Join(metaParts, " · "))
				}
				fmt.Fprintf(&b, "| %s | **[%s](conversation://%s)** | %s | %s |\n", badge, cleanTitle, t.ConversationID, slugCell, oneLine)
			}
		}
		if sel.ArchivedHotCount > 0 {
			fmt.Fprintf(&b, "\n- *%d archived task(s) preserved in local/memory/tasks.json*\n", sel.ArchivedHotCount)
		}
		if sel.HiddenCompletedCount > 0 || sel.HiddenIdleCount > 0 || sel.ArchivedColdCount > 0 {
			fmt.Fprintf(&b, "- *%d older completed, %d idle, and %d cold-archived task(s) omitted from live console — run `./bin/hq tasks --all` or `./bin/hq tasks --archive` to inspect.*\n",
				sel.HiddenCompletedCount, sel.HiddenIdleCount, sel.ArchivedColdCount)
		}
		b.WriteString("\n---\n\n")
	}

	fmt.Fprintf(&b, "## 🌿 3. Active Parallel Git Worktrees (`%d` Active)\n", len(state.Worktrees))
	if len(state.Worktrees) == 0 {
		b.WriteString("- None active in `local/worktrees/`.\n\n---\n\n")
	} else {
		for _, wt := range state.Worktrees {
			fmt.Fprintf(&b, "- `%s` (`%s`)\n", filepath.Base(wt), wt)
		}
		b.WriteString("\n---\n\n")
	}

	if len(state.ActiveSpaces) > 0 {
		fmt.Fprintf(&b, "## 🗂️ Active Console Spaces (`%d` Active)\n", len(state.ActiveSpaces))
		for _, sp := range state.ActiveSpaces {
			pinTag := ""
			if sp.OverrideMode == "pin" {
				pinTag = " · `PINNED`"
			}
			fmt.Fprintf(&b, "- **%s** (`%s` · `%s`%s)\n", sp.Title, sp.ID, sp.Schedule, pinTag)
			for _, card := range sp.RenderedCards {
				fmt.Fprintf(&b, "  - *%s:* %s\n", card.Title, strings.Join(card.Lines, " | "))
			}
		}
		b.WriteString("\n---\n\n")
	}

	if len(state.Goals) > 0 || len(state.OverdueWaits) > 0 {
		fmt.Fprintf(&b, "## 🎯 Active Goals & Counterparty SLAs (`%d` Goal(s) · `%d` Overdue Wait(s))\n", len(state.Goals), len(state.OverdueWaits))
		for _, g := range state.Goals {
			done := 0
			for _, m := range g.Milestones {
				if m.Done {
					done++
				}
			}
			dueTag := ""
			if g.TargetDate != "" {
				dueTag = fmt.Sprintf(" · target `%s`", g.TargetDate)
			}
			fmt.Fprintf(&b, "- **`%s`** — %s (`%d/%d` milestones%s)\n", g.Slug, g.Title, done, len(g.Milestones), dueTag)
		}
		for _, ow := range state.OverdueWaits {
			fmt.Fprintf(&b, "- ⚠️ **Overdue SLA (`%s`):** Waiting on **%s** for *%s* (`%s` · deadline `%s`)\n", ow.GoalSlug, ow.Waiting.Counterparty, ow.Waiting.Summary, ow.Waiting.Channel, ow.Waiting.FollowUpAfter)
		}
		b.WriteString("\n---\n\n")
	}

	if len(state.Ideas) > 0 {
		fmt.Fprintf(&b, "## 💡 Proactive Ideas Feed (`%d` Pending)\n", len(state.Ideas))
		for _, it := range state.Ideas {
			fmt.Fprintf(&b, "- **`%s`** (`%s`): **%s** — %s *(run `./bin/hq ideas promote %s` or `dismiss %s`)*\n", it.ID, it.SourceSkill, it.Title, it.Summary, it.ID, it.ID)
		}
		b.WriteString("\n---\n\n")
	}

	b.WriteString("## 🕒 4. Recent Turn Activity (Today's Journal)\n")
	if len(state.TodayTurns) == 0 {
		b.WriteString("- No turns logged yet today.\n\n---\n\n")
	} else {
		limit := len(state.TodayTurns)
		if limit > 5 {
			limit = 5
		}
		for _, j := range state.TodayTurns[:limit] {
			rawPrompt := j.UserPrompt
			if strings.TrimSpace(rawPrompt) == "" {
				rawPrompt = j.ResponseSummary
			}
			prompt := strings.Join(strings.Fields(rawPrompt), " ")
			if runes := []rune(prompt); len(runes) > 90 {
				prompt = string(runes[:90]) + "..."
			}
			shortConv := j.ConversationID
			if len(shortConv) > 8 {
				shortConv = shortConv[:8]
			}
			fmt.Fprintf(&b, "- `%s` · [`%s`](conversation://%s) (`%s`): %s\n", j.Timestamp, shortConv, j.ConversationID, j.ModelName, prompt)
		}
		b.WriteString("\n---\n\n")
	}

	b.WriteString("## 🩺 5. System Health (`hq doctor`)\n")
	b.WriteString("| Check | Status | Detail |\n| :--- | :--- | :--- |\n")
	for _, h := range state.Health {
		icon := "✅"
		if h.Status != "PASS" {
			icon = "⚠️"
		}
		fmt.Fprintf(&b, "| **%s** | %s `%s` | %s |\n", h.Name, icon, h.Status, h.Detail)
	}

	content := []byte(b.String())
	if err := os.WriteFile(filepath.Join(vaultDir, "CONSOLE.md"), content, 0o644); err != nil {
		return err
	}

	if fd, ok := kernel.GetFrontDesk(vaultDir); ok && strings.TrimSpace(fd.ArtifactDir) != "" {
		artDir := strings.TrimSpace(fd.ArtifactDir)
		_ = os.MkdirAll(artDir, 0o755)
		meta := map[string]interface{}{
			"summary":         "Live [HQ] Front Desk Console — Pending Approvals, Task Board, Active Worktrees, Recent Turn Activity, and System Health.",
			"updatedAt":       time.Now().UTC().Format(time.RFC3339Nano),
			"requestFeedback": false,
			"userFacing":      true,
		}
		metaBytes, _ := json.MarshalIndent(meta, "", "  ")
		_ = os.Remove(filepath.Join(artDir, "hq_console_mobile.md"))
		_ = os.Remove(filepath.Join(artDir, "hq_console_mobile.md.metadata.json"))
		_ = os.WriteFile(filepath.Join(artDir, "hq_console.md"), content, 0o644)
		_ = os.WriteFile(filepath.Join(artDir, "hq_console.md.metadata.json"), metaBytes, 0o644)
	}
	return nil
}
