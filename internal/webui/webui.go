package webui

import (
	"context"
	"crypto/subtle"
	_ "embed"
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
	"sync"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/channels"
	"github.com/zjshen14/agy-hq/internal/delegates"
	"github.com/zjshen14/agy-hq/internal/doctor"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/gchat"
	"github.com/zjshen14/agy-hq/internal/hooks"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/internal/timers"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/version"
	"github.com/zjshen14/agy-hq/internal/watcher"
	"github.com/zjshen14/agy-hq/internal/workspace"
	"github.com/zjshen14/agy-hq/internal/worktree"
)

// ScheduleSummaryRow surfaces both hq daemon declarative triggers and active conversation crons in the Console UI.
type ScheduleSummaryRow struct {
	ID              string `json:"id"`
	FQID            string `json:"fqid,omitempty"`
	Kind            string `json:"kind"` // "daemon" or "conversation_cron"
	Enabled         bool   `json:"enabled"`
	Cron            string `json:"cron"`
	Runner          string `json:"runner"`
	Skill           string `json:"skill,omitempty"`
	Summary         string `json:"summary,omitempty"`
	LastCheckedAt   string `json:"lastCheckedAt,omitempty"`
	LastTriggeredAt string `json:"lastTriggeredAt,omitempty"`
	LastStatus      string `json:"lastStatus,omitempty"`
	SkipsCount      int    `json:"skipsCount,omitempty"`
	LastError       string `json:"lastError,omitempty"`
	LatestConvID    string `json:"latestConvId,omitempty"`
	NextRunHuman    string `json:"nextRunHuman,omitempty"`
}

// DashboardState aggregates live Vault, approval, worktree, provider workspace, task, schedule, journal, quota, observability, spaces, goals, ideas, and doctor state.
type DashboardState struct {
	VaultDir       string                         `json:"vaultDir"`
	UpdatedAt      string                         `json:"updatedAt"`
	Quota          kernel.QuotaState              `json:"quota"`
	Observability  obs.DashboardSummary           `json:"observability"`
	Pending        []approvals.Proposal           `json:"pending"`
	Schedules      []ScheduleSummaryRow           `json:"schedules,omitempty"`
	ActiveSpaces   []ConsoleSpace                 `json:"activeSpaces,omitempty"`
	Goals          []kernel.Goal                  `json:"goals,omitempty"`
	OverdueWaits   []kernel.OverdueWaitingSummary `json:"overdueWaits,omitempty"`
	Ideas          []outbox.OutboxItem            `json:"ideas,omitempty"`
	Worktrees      []string                       `json:"worktrees"`
	Workspaces     []workspace.SurfaceEntry       `json:"workspaces,omitempty"` // RFC-0025 provider ledger view (read-only)
	Tasks          []kernel.ActiveTask            `json:"tasks"`
	ChannelBridges []channels.BridgeSummary       `json:"channelBridges,omitempty"`
	Delegates      delegates.DashboardSummary     `json:"delegates,omitempty"`
	TodayTurns     []hooks.JournalEntry           `json:"todayTurns"`
	Health         []doctor.CheckResult           `json:"health"`
	ProactiveV3    ProactiveV3State               `json:"proactiveV3,omitempty"`
	FlashResult    string                         `json:"flashResult,omitempty"`
	CSRFToken      string                         `json:"-"`
	StaticMode     bool                           `json:"-"`
	TaskSelection  kernel.ConsoleTaskSelection    `json:"-"`
	Layer1Count    int                            `json:"-"`
	ColdTaskCount  int                            `json:"-"`
	frontDesk      kernel.FrontDeskLease
	hasFrontDesk   bool
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

func validCSRFToken(expected, candidate string) bool {
	c := strings.TrimSpace(candidate)
	if expected == "" || len(c) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(c)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type postActionPayload struct {
	ID        string `json:"id"`
	SHA256    string `json:"sha256"`
	Slug      string `json:"slug"`
	Mode      string `json:"mode"`
	Action    string `json:"action"`
	Reason    string `json:"reason"`
	Duration  string `json:"duration"`
	Resume    bool   `json:"resume"`
	CSRFToken string `json:"csrf_token"`
}

func decodeCSRFPostAction(w http.ResponseWriter, r *http.Request, csrfToken string) (postActionPayload, bool) {
	var p postActionPayload
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return p, false
	}
	if !isAllowedSameOriginRequest(r) {
		http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
		return p, false
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		_ = json.NewDecoder(r.Body).Decode(&p)
	} else {
		_ = r.ParseForm()
		p.ID = r.FormValue("id")
		p.SHA256 = r.FormValue("sha256")
		p.Slug = r.FormValue("slug")
		p.Mode = r.FormValue("mode")
		p.Action = r.FormValue("action")
		p.Reason = r.FormValue("reason")
		p.Duration = r.FormValue("duration")
		p.CSRFToken = r.FormValue("csrf_token")
		rv := strings.ToLower(strings.TrimSpace(r.FormValue("resume")))
		p.Resume = rv == "1" || rv == "true" || rv == "yes"
	}
	p.ID = strings.TrimSpace(p.ID)
	p.SHA256 = strings.TrimSpace(p.SHA256)
	p.Slug = strings.TrimSpace(p.Slug)
	p.Mode = strings.TrimSpace(p.Mode)
	p.Action = strings.ToLower(strings.TrimSpace(p.Action))
	p.Reason = strings.TrimSpace(p.Reason)
	p.Duration = strings.TrimSpace(p.Duration)
	p.CSRFToken = strings.TrimSpace(p.CSRFToken)
	if hdr := strings.TrimSpace(r.Header.Get("X-HQ-CSRF-Token")); hdr != "" {
		p.CSRFToken = hdr
	}
	if !validCSRFToken(csrfToken, p.CSRFToken) {
		http.Error(w, "forbidden: missing or invalid CSRF token", http.StatusForbidden)
		return p, false
	}
	return p, true
}

func wantsJSONResponse(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json") ||
		strings.Contains(r.Header.Get("Content-Type"), "application/json")
}

// NewHandler creates the HTTP mux for the HQ Console side pane and API.
func NewHandler(vaultDir string) http.Handler {
	mux := http.NewServeMux()
	csrfToken := store.RandomHex(16)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]interface{}{
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
		state := loadStateCached(vaultDir)
		state.CSRFToken = ""
		writeJSON(w, http.StatusOK, state)
	})

	mux.HandleFunc("/api/obs", func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedSameOriginRequest(r) {
			http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
			return
		}
		writeJSON(w, http.StatusOK, obs.BuildDashboardSummary(vaultDir, time.Now()))
	})

	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if !isAllowedSameOriginRequest(r) {
				http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
				return
			}
			now := time.Now()
			incArch := r.URL.Query().Get("archived") == "1" || r.URL.Query().Get("archived") == "true"
			list, _ := events.List(vaultDir, r.URL.Query().Get("status"), incArch, 50)
			stats := events.GetSummaryStats(vaultDir, now)
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"stats":  stats,
				"events": list,
			})
		case http.MethodPost:
			authHdr := strings.TrimSpace(r.Header.Get("Authorization"))
			authed := false
			if strings.HasPrefix(strings.ToLower(authHdr), "bearer ") {
				authed = events.ValidateDaemonToken(vaultDir, strings.TrimSpace(authHdr[7:]))
			} else if isAllowedSameOriginRequest(r) && validCSRFToken(csrfToken, r.Header.Get("X-HQ-CSRF-Token")) {
				authed = true
			}
			if !authed {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized: valid Authorization: Bearer <daemon_token> or X-HQ-CSRF-Token required")
				return
			}

			r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
			var reqBody struct {
				Source      string            `json:"source"`
				DedupKey    string            `json:"dedupKey"`
				ScopeKey    string            `json:"scopeKey"`
				SessionID   string            `json:"sessionId"`
				Skill       string            `json:"skill"`
				Model       string            `json:"model"`
				Title       string            `json:"title"`
				Prompt      string            `json:"prompt"`
				AvailableAt string            `json:"availableAt"`
				Metadata    map[string]string `json:"metadata"`
			}
			if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
				status := http.StatusBadRequest
				if strings.Contains(strings.ToLower(err.Error()), "request body too large") {
					status = http.StatusRequestEntityTooLarge
				}
				writeJSONError(w, status, err.Error())
				return
			}
			if strings.TrimSpace(reqBody.Title) == "" && strings.TrimSpace(reqBody.Prompt) == "" {
				writeJSONError(w, http.StatusBadRequest, "event requires non-empty title or prompt")
				return
			}
			src := strings.TrimSpace(reqBody.Source)
			if src == "" {
				src = "webhook"
			}
			ev, deduped, err := events.Enqueue(vaultDir, events.Event{
				Source:      src,
				DedupKey:    strings.TrimSpace(reqBody.DedupKey),
				ScopeKey:    strings.TrimSpace(reqBody.ScopeKey),
				SessionID:   strings.TrimSpace(reqBody.SessionID),
				Skill:       strings.TrimSpace(reqBody.Skill),
				Model:       strings.TrimSpace(reqBody.Model),
				Title:       strings.TrimSpace(reqBody.Title),
				Prompt:      strings.TrimSpace(reqBody.Prompt),
				AvailableAt: strings.TrimSpace(reqBody.AvailableAt),
				Metadata:    reqBody.Metadata,
			}, time.Now())
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]interface{}{
				"status":       "accepted",
				"deduplicated": deduped,
				"event":        ev,
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/timers", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !isAllowedSameOriginRequest(r) {
			http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
			return
		}
		incAll := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
		list, _ := timers.List(vaultDir, incAll)
		writeJSON(w, http.StatusOK, map[string]interface{}{"timers": list})
	})

	requireDaemonBearer := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return false
		}
		authHdr := strings.TrimSpace(r.Header.Get("Authorization"))
		if !strings.HasPrefix(strings.ToLower(authHdr), "bearer ") || !events.ValidateDaemonToken(vaultDir, strings.TrimSpace(authHdr[7:])) {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized: valid Authorization: Bearer <daemon_token> required")
			return false
		}
		return true
	}

	mux.HandleFunc("/api/channels/inbound", func(w http.ResponseWriter, r *http.Request) {
		if !requireDaemonBearer(w, r) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		var req channels.InboundRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		res, err := channels.HandleInbound(r.Context(), vaultDir, req, time.Now())
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		events.TriggerWake()
		writeJSON(w, http.StatusAccepted, res)
	})

	mux.HandleFunc("/api/channels/callback", func(w http.ResponseWriter, r *http.Request) {
		if !requireDaemonBearer(w, r) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		var req channels.CallbackRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		res, err := channels.HandleCallback(r.Context(), vaultDir, req, time.Now())
		_ = SyncMarkdownConsole(vaultDir)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		events.TriggerWake()
		writeJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("/api/channels/flush", func(w http.ResponseWriter, r *http.Request) {
		if !requireDaemonBearer(w, r) {
			return
		}
		channels.InvalidateSyncMemo()
		rep, err := channels.SyncAndFlush(r.Context(), vaultDir, time.Now(), false)
		events.TriggerWake()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rep)
	})

	handleCSRFPost := func(path string, allowHTML bool, fn func(postActionPayload) (bool, string, *string)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			p, ok := decodeCSRFPostAction(w, r, csrfToken)
			if !ok {
				return
			}
			succeeded, msg, extraOut := fn(p)
			_ = SyncMarkdownConsole(vaultDir)
			if allowHTML && !wantsJSONResponse(r) {
				state := loadState(vaultDir, msg)
				state.CSRFToken = csrfToken
				renderHTML(w, state)
				return
			}
			resp := map[string]interface{}{"ok": succeeded, "message": msg}
			if extraOut != nil {
				resp["output"] = *extraOut
			}
			writeJSON(w, http.StatusOK, resp)
		})
	}

	handleCSRFPost("/api/approve", true, func(p postActionPayload) (bool, string, *string) {
		out, err := approvals.ApproveFromTrustedDaemon(vaultDir, p.ID, p.SHA256)
		msg := fmt.Sprintf("Approved %s", p.ID)
		if out != "" {
			msg += ": " + strings.TrimSpace(out)
		}
		if err != nil {
			msg = fmt.Sprintf("Error approving %s: %v (%s)", p.ID, err, strings.TrimSpace(out))
		}
		return err == nil, msg, &out
	})

	handleCSRFPost("/api/reject", true, func(p postActionPayload) (bool, string, *string) {
		reason := p.Reason
		if reason == "" {
			reason = "Rejected via HQ Console"
		}
		err := approvals.RejectProposalWithOptions(vaultDir, p.ID, approvals.RejectOptions{
			Reason:         reason,
			SuppressResume: !p.Resume,
		})
		msg := fmt.Sprintf("Rejected %s", p.ID)
		if err != nil {
			msg = fmt.Sprintf("Error rejecting %s: %v", p.ID, err)
		}
		return err == nil, msg, nil
	})

	handleCSRFPost("/api/tasks/complete", true, func(p postActionPayload) (bool, string, *string) {
		err := kernel.CompleteTask(vaultDir, p.Slug, "")
		msg := fmt.Sprintf("Completed task %s", p.Slug)
		if err != nil {
			msg = fmt.Sprintf("Error completing %s: %v", p.Slug, err)
		}
		return err == nil, msg, nil
	})

	handleCSRFPost("/api/tasks/archive", true, func(p postActionPayload) (bool, string, *string) {
		err := kernel.ArchiveTask(vaultDir, p.Slug, false)
		msg := fmt.Sprintf("Archived task %s", p.Slug)
		if err != nil {
			msg = fmt.Sprintf("Error archiving %s: %v", p.Slug, err)
		}
		return err == nil, msg, nil
	})

	mux.HandleFunc("/api/console-spaces", func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedSameOriginRequest(r) {
			http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
			return
		}
		spaces, _ := ListSpaces(vaultDir, time.Now())
		writeJSON(w, http.StatusOK, spaces)
	})

	handleCSRFPost("/api/console-spaces/set", false, func(p postActionPayload) (bool, string, *string) {
		err := SetSpaceOverride(vaultDir, p.ID, p.Mode)
		msg := fmt.Sprintf("Updated space %s (%s)", p.ID, p.Mode)
		if err != nil {
			msg = fmt.Sprintf("Error updating space %s: %v", p.ID, err)
		}
		return err == nil, msg, nil
	})

	handleCSRFPost("/api/ideas/action", false, func(p postActionPayload) (bool, string, *string) {
		now := time.Now()
		var err error
		switch p.Action {
		case "promote":
			_, err = outbox.PromoteIdea(vaultDir, p.ID, now)
		case "snooze":
			dur := 24 * time.Hour
			if p.Duration != "" {
				if parsed, dErr := time.ParseDuration(p.Duration); dErr == nil && parsed > 0 {
					dur = parsed
				}
			}
			_, err = outbox.SnoozeIdea(vaultDir, p.ID, dur, now)
		case "dismiss":
			_, err = outbox.DismissIdea(vaultDir, p.ID, p.Reason, now)
		default:
			err = fmt.Errorf("unsupported action %q", p.Action)
		}
		msg := fmt.Sprintf("Idea %s: %s", p.ID, p.Action)
		if err != nil {
			msg = fmt.Sprintf("Error updating idea %s: %v", p.ID, err)
		}
		return err == nil, msg, nil
	})

	handleCSRFPost("/api/triggers/action", false, func(p postActionPayload) (bool, string, *string) {
		now := time.Now()
		var err error
		switch p.Action {
		case "pause":
			_, _, err = extension.SetSchedulePaused(context.Background(), vaultDir, p.ID, true, p.Reason, "", now)
		case "resume":
			_, _, err = extension.SetSchedulePaused(context.Background(), vaultDir, p.ID, false, p.Reason, "", now)
		case "fire":
			_, err = kernel.FireScheduleOnce(vaultDir, p.ID, false, false, now)
		default:
			err = fmt.Errorf("unsupported trigger action %q", p.Action)
		}
		msg := fmt.Sprintf("Schedule %s: %s", p.ID, p.Action)
		if err != nil {
			msg = fmt.Sprintf("Error updating schedule %s: %v", p.ID, err)
		}
		return err == nil, msg, nil
	})

	mux.HandleFunc("/favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte(faviconSVG))
	})
	registerProactiveV3Routes(mux, vaultDir, csrfToken)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		state := loadStateCached(vaultDir)
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

//go:embed assets/preload.js
var preloadBridgeJS string

//go:embed assets/favicon.svg
var faviconSVG string

//go:embed assets/console.html
var consoleHTML string

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
				prev := store.TruncateRunes(strings.Join(strings.Fields(strings.TrimSpace(payload)), " "), 120)
				p.Summary = fmt.Sprintf("Send Google Chat to %s: %q", target, prev)
			}
		}
	}
}

type vaultStateFingerprint [16]int64

func computeVaultStateFingerprint(vaultDir string) vaultStateFingerprint {
	var fp vaultStateFingerprint
	statAt := func(idx int, p string) {
		st, err := os.Stat(p)
		if err != nil {
			fp[idx] = -1
			return
		}
		fp[idx] = st.ModTime().UnixNano() ^ (st.Size() << 13)
	}
	mem := filepath.Join(vaultDir, "memory")
	today := time.Now().Format("2006-01-02")
	statAt(0, filepath.Join(mem, "pending"))
	statAt(1, filepath.Join(mem, "tasks.json"))
	statAt(2, filepath.Join(mem, "quota_state.json"))
	statAt(3, filepath.Join(mem, "outbox", "items.jsonl"))
	statAt(4, filepath.Join(mem, "goals.json"))
	statAt(5, filepath.Join(mem, "front_desk.json"))
	statAt(6, filepath.Join(mem, "trigger_cursors.json"))
	statAt(7, filepath.Join(mem, "space_overrides.json"))
	statAt(8, filepath.Join(vaultDir, "extensions.json"))
	statAt(9, filepath.Join(vaultDir, "worktrees"))
	statAt(10, filepath.Join(vaultDir, "config.yaml"))
	statAt(11, filepath.Join(mem, "journal", today))
	statAt(12, filepath.Join(mem, "channels"))
	statAt(13, filepath.Join(vaultDir, "private", "channels"))
	statAt(14, filepath.Join(mem, "workspaces.json"))
	fp[15] = computeObsStateStamp(vaultDir)
	return fp
}

type stateCacheSlot struct {
	state       DashboardState
	fingerprint vaultStateFingerprint
	expiresAt   time.Time
	inflight    chan struct{}
}

var (
	stateCacheMu sync.Mutex
	stateCache   = make(map[string]*stateCacheSlot)
)

func invalidateStateCache(vaultDir string) {
	key := filepath.Clean(vaultDir)
	stateCacheMu.Lock()
	if slot, ok := stateCache[key]; ok {
		slot.expiresAt = time.Time{}
	}
	stateCacheMu.Unlock()
}

// loadStateCached coalesces concurrent GET / and GET /api/state requests per vaultDir
// (singleflight) and serves a short 2s read-through cache while Vault mutation stamps match.
func loadStateCached(vaultDir string) DashboardState {
	key := filepath.Clean(vaultDir)
	fp := computeVaultStateFingerprint(vaultDir)
	now := time.Now()

	for {
		stateCacheMu.Lock()
		slot, ok := stateCache[key]
		if !ok {
			slot = &stateCacheSlot{}
			stateCache[key] = slot
		}
		if slot.inflight == nil && now.Before(slot.expiresAt) && slot.fingerprint == fp {
			cached := slot.state
			stateCacheMu.Unlock()
			return cached
		}
		if slot.inflight != nil {
			waitCh := slot.inflight
			stateCacheMu.Unlock()
			<-waitCh
			stateCacheMu.Lock()
			res := slot.state
			stateCacheMu.Unlock()
			return res
		}
		ch := make(chan struct{})
		slot.inflight = ch
		stateCacheMu.Unlock()

		computed := loadState(vaultDir, "")
		computed.CSRFToken = ""
		postFP := computeVaultStateFingerprint(vaultDir)

		stateCacheMu.Lock()
		slot.state = computed
		slot.fingerprint = postFP
		slot.expiresAt = time.Now().Add(15 * time.Second)
		slot.inflight = nil
		close(ch)
		stateCacheMu.Unlock()
		return computed
	}
}

func loadState(vaultDir, flashMsg string) DashboardState {
	return loadStateWithObsMaxAge(vaultDir, flashMsg, 0)
}

func loadStateWithObsMaxAge(vaultDir, flashMsg string, obsCacheMaxAge time.Duration) DashboardState {
	pending, _ := approvals.List(vaultDir)
	for i := range pending {
		enrichPendingDraftPayload(vaultDir, &pending[i])
	}
	now := time.Now()
	wts, _ := worktree.List(vaultDir)
	wsEntries := kernel.UnifiedSurfaceEntries(vaultDir, now)
	tasks, _ := kernel.ListTasks(vaultDir)
	layer1Tasks := kernel.FilterLayer1Tasks(vaultDir, tasks)
	coldCount := kernel.CountColdArchivedTasks(vaultDir)
	sel := kernel.SelectConsoleTasks(layer1Tasks, coldCount, now)
	turns := loadTodayJournals(vaultDir)
	quota := trimQuotaForDashboard(kernel.LoadQuota(vaultDir))
	var obsSum obs.DashboardSummary
	if obsCacheMaxAge > 0 {
		obsSum = obs.BuildDashboardSummaryCached(vaultDir, now, obsCacheMaxAge)
	} else {
		obsSum = obs.BuildDashboardSummary(vaultDir, now)
	}
	health := doctor.RunWithObs(vaultDir, &obsSum)
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
	activeSpaces, _ := activeSpacesWithCache(vaultDir, now, &spaceWidgetCache{
		goals:          allGoals,
		goalsLoaded:    true,
		ideas:          ideas,
		ideasLoaded:    true,
		surfaceEntries: wsEntries,
		surfaceLoaded:  true,
		layer1Tasks:    layer1Tasks,
		tasksLoaded:    true,
	})
	fd, hasFD := kernel.GetFrontDesk(vaultDir)
	schedules := buildScheduleSummaries(vaultDir, tasks, fd, hasFD)
	bridgeSummaries, _ := channels.ListBridgeSummaries(vaultDir)
	st := DashboardState{
		VaultDir:       vaultDir,
		UpdatedAt:      now.Format("15:04:05"),
		Quota:          quota,
		Observability:  obsSum,
		Pending:        pending,
		Schedules:      schedules,
		ActiveSpaces:   activeSpaces,
		Goals:          activeGoals,
		OverdueWaits:   overdueWaits,
		Ideas:          ideas,
		Worktrees:      wts,
		Workspaces:     wsEntries,
		Tasks:          sel.VisibleRows,
		ChannelBridges: bridgeSummaries,
		Delegates:      delegates.LoadDashboardSummary(vaultDir),
		TodayTurns:     turns,
		Health:         health,
		FlashResult:    flashMsg,
		TaskSelection:  sel,
		Layer1Count:    len(layer1Tasks),
		ColdTaskCount:  coldCount,
		frontDesk:      fd,
		hasFrontDesk:   hasFD,
	}
	enrichProactiveV3State(vaultDir, &st, now)
	return st
}

func buildScheduleSummaries(vaultDir string, allTasks []kernel.ActiveTask, fd kernel.FrontDeskLease, hasFD bool) []ScheduleSummaryRow {
	var rows []ScheduleSummaryRow
	cfg, _ := kernel.LoadEffectiveTriggers(vaultDir)
	cursors := kernel.LoadTriggerCursors(vaultDir)
	daemonActive := kernel.IsDaemonActiveFast(vaultDir)

	findLatestConv := func(schedID, mode string) string {
		var bestConv, bestTS string
		gkPrefix := "auto-gatekeeper-" + schedID + "-"
		for _, t := range allTasks {
			if strings.TrimSpace(t.ConversationID) == "" {
				continue
			}
			matched := strings.HasPrefix(t.TaskSlug, gkPrefix)
			if !matched && schedID == "gmail-inbox-watch" && strings.HasPrefix(t.TaskSlug, "auto-watch-gmail-") {
				matched = true
			}
			if matched && (bestConv == "" || t.LastUpdatedAt > bestTS) {
				bestConv = t.ConversationID
				bestTS = t.LastUpdatedAt
			}
		}
		if bestConv == "" && mode == "front_desk_cron" && hasFD {
			return fd.ConversationID
		}
		return bestConv
	}

	watchRep, _ := watcher.InspectStatus(context.Background(), vaultDir, "", "", time.Now())
	watcherWarnByConn := make(map[string]string)
	for _, wa := range watchRep.Accounts {
		if wa.Warning != "" {
			watcherWarnByConn[wa.Connector] = wa.Warning
		} else if strings.HasPrefix(wa.LastStatus, "paused") || strings.HasPrefix(wa.LastStatus, "error:") {
			watcherWarnByConn[wa.Connector] = wa.LastStatus
		}
	}

	if cfg != nil {
		for _, s := range cfg.Schedules {
			cur := cursors[s.ID]
			mode := s.EffectiveMode()
			runner := fmt.Sprintf("Front Desk fallback (%s)", mode)
			if !s.IsEnabled() {
				runner = "hq daemon (paused)"
			} else if daemonActive {
				runner = fmt.Sprintf("hq daemon (%s)", mode)
			}
			lastStatus := cur.LastStatus
			if s.ID == "gmail-inbox-watch" {
				if wWarn := watcherWarnByConn["gmail"]; wWarn != "" {
					lastStatus = wWarn
				}
			}
			rows = append(rows, ScheduleSummaryRow{
				ID:              s.ID,
				FQID:            s.FQID(),
				Kind:            "daemon",
				Enabled:         s.IsEnabled(),
				Cron:            s.Cron,
				Runner:          runner,
				Skill:           s.Skill,
				LastCheckedAt:   cur.LastCheckedAt,
				LastTriggeredAt: cur.LastTriggeredAt,
				LastStatus:      lastStatus,
				SkipsCount:      cur.SkipsCount,
				LastError:       cur.LastError,
				LatestConvID:    findLatestConv(s.ID, mode),
			})
		}
	}

	if hasFD {
		for _, ac := range fd.AdHocCrons {
			label := ac.TaskID
			if idx := strings.LastIndex(label, "/"); idx >= 0 {
				label = label[idx+1:]
			}
			if label == "" {
				label = "ad-hoc-cron"
			}
			rows = append(rows, ScheduleSummaryRow{
				ID:            label,
				Kind:          "conversation_cron",
				Enabled:       true,
				Cron:          ac.Cron,
				Runner:        "Antigravity schedule ([HQ] Front Desk)",
				Summary:       ac.Summary,
				LastCheckedAt: ac.ArmedAt,
				LatestConvID:  fd.ConversationID,
			})
		}
	}
	return rows
}

const (
	maxDashboardJournalEntries = 25
	maxDashboardQuotaEvents    = 15
)

func trimQuotaForDashboard(q kernel.QuotaState) kernel.QuotaState {
	if len(q.Events) == 0 {
		return q
	}
	start := 0
	if len(q.Events) > maxDashboardQuotaEvents {
		start = len(q.Events) - maxDashboardQuotaEvents
	}
	trimmed := make([]kernel.SpawnEvent, 0, len(q.Events)-start)
	for _, ev := range q.Events[start:] {
		ev.Outcome = store.TruncateRunes(ev.Outcome, 240)
		trimmed = append(trimmed, ev)
	}
	q.Events = trimmed
	return q
}

type todayJournalCacheEntry struct {
	dateDir    string
	dirModNano int64
	entries    []hooks.JournalEntry
}

var (
	todayJournalMu    sync.Mutex
	todayJournalCache = make(map[string]todayJournalCacheEntry)
)

func loadTodayJournals(vaultDir string) []hooks.JournalEntry {
	cleanVault := filepath.Clean(vaultDir)
	dateDir := filepath.Join(vaultDir, "memory", "journal", time.Now().Format("2006-01-02"))
	st, err := os.Stat(dateDir)
	if err != nil {
		return nil
	}
	dirMod := st.ModTime().UnixNano()
	todayJournalMu.Lock()
	if cached, ok := todayJournalCache[cleanVault]; ok && cached.dateDir == dateDir && cached.dirModNano == dirMod {
		cp := append([]hooks.JournalEntry(nil), cached.entries...)
		todayJournalMu.Unlock()
		return cp
	}
	todayJournalMu.Unlock()

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
			j.UserPrompt = store.TruncateRunes(strings.Join(strings.Fields(j.UserPrompt), " "), 240)
			j.ResponseSummary = store.TruncateRunes(strings.Join(strings.Fields(j.ResponseSummary), " "), 240)
			j.TranscriptPath = ""
			j.ToolsUsed = nil
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp > out[j].Timestamp
	})
	if len(out) > maxDashboardJournalEntries {
		out = out[:maxDashboardJournalEntries]
	}

	todayJournalMu.Lock()
	todayJournalCache[cleanVault] = todayJournalCacheEntry{
		dateDir:    dateDir,
		dirModNano: dirMod,
		entries:    append([]hooks.JournalEntry(nil), out...),
	}
	todayJournalMu.Unlock()
	return out
}

var pageTmpl = template.Must(template.New("console").Funcs(consoleTemplateFuncs()).Parse(consoleHTML))

func renderHTML(w http.ResponseWriter, state DashboardState) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTmpl.Execute(w, state)
}
