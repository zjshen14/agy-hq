package webui

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/living"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/pulse"
	"github.com/zjshen14/agy-hq/internal/timers"
)

// ProactiveV3State holds RFC-0027 Living Spaces, Expired Proposals, Active Decision Cards, and Activity Reversals.
type ProactiveV3State struct {
	LivingSpaces     []living.SpaceSummary          `json:"livingSpaces,omitempty"`
	ExpiredProposals []approvals.InterventionRecord `json:"expiredProposals,omitempty"`
	DecisionCards    []outbox.OutboxItem            `json:"decisionCards,omitempty"`
	RecentActivity   []kernel.ActivityRecord        `json:"recentActivity,omitempty"`
}

const livingSpaceCSP = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'"

// enrichProactiveV3State populates RFC-0027 state and human-readable Schedule next-run times.
func enrichProactiveV3State(vaultDir string, state *DashboardState, now time.Time) {
	if state == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	spaces, _ := living.List(vaultDir)
	expired, _ := approvals.ListExpiredProposals(vaultDir, 7*24*time.Hour, now)
	allInterrupts, _ := outbox.List(vaultDir, outbox.TierInterrupt, "", now)
	var decisionCards []outbox.OutboxItem
	for _, it := range allInterrupts {
		if len(it.Decisions) == 0 {
			continue
		}
		if it.Status == outbox.StatusPending || it.Status == outbox.StatusDelivered {
			decisionCards = append(decisionCards, it)
			if len(decisionCards) >= 10 {
				break
			}
		}
	}
	state.ProactiveV3 = ProactiveV3State{
		LivingSpaces:     spaces,
		ExpiredProposals: expired,
		DecisionCards:    decisionCards,
		RecentActivity:   kernel.LoadRecentActivity(vaultDir, 10),
	}
	for i := range state.Schedules {
		if !state.Schedules[i].Enabled || state.Schedules[i].Stale || strings.TrimSpace(state.Schedules[i].Cron) == "" {
			continue
		}
		if nextAt, err := timers.NextCronOccurrence(state.Schedules[i].Cron, now); err == nil {
			state.Schedules[i].NextRunHuman = formatHumanNextRun(nextAt, now)
		}
	}
}

func formatHumanNextRun(nextAt, now time.Time) string {
	nextAt = nextAt.In(now.Location())
	diff := nextAt.Sub(now)
	if diff < 0 {
		return "due now"
	}
	var rel string
	switch {
	case diff < time.Hour:
		rel = fmt.Sprintf("in %dm", int(diff.Minutes())+1)
	case diff < 24*time.Hour:
		h := int(diff.Hours())
		m := int(diff.Minutes()) % 60
		rel = fmt.Sprintf("in %dh %dm", h, m)
	default:
		d := int(diff.Hours()) / 24
		h := int(diff.Hours()) % 24
		rel = fmt.Sprintf("in %dd %dh", d, h)
	}
	dayLabel := nextAt.Format("Mon 3:04 PM")
	if nextAt.YearDay() == now.YearDay() && nextAt.Year() == now.Year() {
		dayLabel = "Today " + nextAt.Format("3:04 PM")
	} else if nextAt.YearDay() == now.Add(24*time.Hour).YearDay() && nextAt.Year() == now.Add(24*time.Hour).Year() {
		dayLabel = "Tomorrow " + nextAt.Format("3:04 PM")
	}
	return fmt.Sprintf("%s · %s", dayLabel, rel)
}

// registerProactiveV3Routes mounts `/living-spaces/<slug>` (strict CSP sandbox), `/api/outbox/act`, and `/api/approvals/restage`.
func registerProactiveV3Routes(mux *http.ServeMux, vaultDir, csrfToken string) {
	mux.HandleFunc("/living-spaces/", func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/living-spaces/")
		rel = strings.Trim(rel, "/")
		if rel == "" {
			spaces, _ := living.List(vaultDir)
			writeJSON(w, http.StatusOK, map[string]interface{}{"livingSpaces": spaces})
			return
		}
		isRaw := false
		slug := rel
		if strings.HasSuffix(rel, "/raw") {
			isRaw = true
			slug = strings.TrimSuffix(rel, "/raw")
		} else if r.URL.Query().Get("raw") == "1" {
			isRaw = true
		}
		if err := living.ValidateSlug(slug); err != nil {
			http.Error(w, "invalid living space slug", http.StatusBadRequest)
			return
		}

		// Ensure compiled index.html exists (auto-build from space.json if needed in 0 tokens)
		htmlPath := filepath.Join(living.SpaceDir(vaultDir, slug), "index.html")
		rawHTML, rErr := os.ReadFile(htmlPath)
		if rErr != nil {
			if _, _, bErr := living.Build(vaultDir, slug); bErr != nil {
				http.Error(w, "living space not found", http.StatusNotFound)
				return
			}
			rawHTML, rErr = os.ReadFile(htmlPath)
			if rErr != nil {
				http.Error(w, "living space unreadable", http.StatusInternalServerError)
				return
			}
		}

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")

		if isRaw {
			w.Header().Set("Content-Security-Policy", livingSpaceCSP)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(rawHTML)
			return
		}

		// Wrapper shell renders the Living Space inside an isolated <iframe sandbox="allow-scripts">
		// (explicitly omitting allow-same-origin so generated UI cannot touch :4242 APIs or CSRF tokens).
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; frame-src 'self'; base-uri 'none'; form-action 'none'")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		escapedSlug := html.EscapeString(slug)
		rawSrc := "/living-spaces/" + escapedSlug + "/raw"
		if lang := strings.TrimSpace(r.URL.Query().Get("lang")); lang != "" {
			rawSrc += "?lang=" + html.EscapeString(lang)
		}
		shell := fmt.Sprintf(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Living Space: %s</title><style>html,body{margin:0;height:100%%;background:#0f172a;font-family:system-ui,sans-serif}.bar{display:flex;align-items:center;justify-content:space-between;padding:8px 14px;background:#111827;color:#e2e8f0;font-size:13px;border-bottom:1px solid #1e293b}.bar a{color:#60a5fa;text-decoration:none;font-weight:600}iframe{border:0;width:100%%;height:calc(100vh - 38px);display:block}</style></head><body><div class="bar"><a href="/">← Back to HQ Console</a><span>Living Space: <strong>%s</strong> (CSP Sandboxed)</span></div><iframe sandbox="allow-scripts" src="%s" title="Living Space %s"></iframe></body></html>`,
			escapedSlug, escapedSlug, rawSrc, escapedSlug)
		_, _ = w.Write([]byte(shell))
	})

	mux.HandleFunc("/api/outbox/act", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !isAllowedSameOriginRequest(r) {
			http.Error(w, "forbidden cross-origin request", http.StatusForbidden)
			return
		}
		var req struct {
			ID        string `json:"id"`
			Option    string `json:"option"`
			CSRFToken string `json:"csrf_token"`
		}
		if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			_ = json.NewDecoder(r.Body).Decode(&req)
		} else {
			_ = r.ParseForm()
			req.ID = r.FormValue("id")
			req.Option = r.FormValue("option")
			req.CSRFToken = r.FormValue("csrf_token")
		}
		if hdr := strings.TrimSpace(r.Header.Get("X-HQ-CSRF-Token")); hdr != "" {
			req.CSRFToken = hdr
		}
		if !validCSRFToken(csrfToken, req.CSRFToken) {
			http.Error(w, "forbidden: missing or invalid CSRF token", http.StatusForbidden)
			return
		}
		res, err := pulse.ExecuteDecision(vaultDir, req.ID, req.Option, time.Now())
		invalidateStateCache(vaultDir)
		if err != nil {
			if wantsJSONResponse(r) {
				writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			state := loadState(vaultDir, "Decision failed: "+err.Error())
			state.CSRFToken = csrfToken
			renderHTML(w, state)
			return
		}
		_ = SyncMarkdownConsole(vaultDir)
		if wantsJSONResponse(r) {
			writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "result": res})
			return
		}
		state := loadState(vaultDir, res.Summary)
		state.CSRFToken = csrfToken
		renderHTML(w, state)
	})

	mux.HandleFunc("/api/approvals/restage", func(w http.ResponseWriter, r *http.Request) {
		p, ok := decodeCSRFPostAction(w, r, csrfToken)
		if !ok {
			return
		}
		fresh, err := approvals.RestageExpiredProposal(vaultDir, p.ID, time.Now())
		invalidateStateCache(vaultDir)
		if err != nil {
			if wantsJSONResponse(r) {
				writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			state := loadState(vaultDir, "Restage failed: "+err.Error())
			state.CSRFToken = csrfToken
			renderHTML(w, state)
			return
		}
		_ = SyncMarkdownConsole(vaultDir)
		if wantsJSONResponse(r) {
			writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "proposal": fresh})
			return
		}
		state := loadState(vaultDir, fmt.Sprintf("Restaged %s as %s", p.ID, fresh.ID))
		state.CSRFToken = csrfToken
		renderHTML(w, state)
	})

	mux.HandleFunc("/api/approvals/dismiss-expired", func(w http.ResponseWriter, r *http.Request) {
		p, ok := decodeCSRFPostAction(w, r, csrfToken)
		if !ok {
			return
		}
		if err := approvals.DismissExpiredProposal(vaultDir, p.ID, p.Reason, time.Now()); err != nil {
			if wantsJSONResponse(r) {
				writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			state := loadState(vaultDir, "Dismiss expired failed: "+err.Error())
			state.CSRFToken = csrfToken
			renderHTML(w, state)
			return
		}
		invalidateStateCache(vaultDir)
		_ = SyncMarkdownConsole(vaultDir)
		if wantsJSONResponse(r) {
			writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": p.ID})
			return
		}
		state := loadState(vaultDir, fmt.Sprintf("Dismissed expired proposal %s", p.ID))
		state.CSRFToken = csrfToken
		renderHTML(w, state)
	})
}
