package webui

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/doctor"
	"github.com/zjshen14/agy-hq/internal/hooks"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/worktree"
)

// DashboardState aggregates live Vault, approval, worktree, task, journal, quota, and doctor state.
type DashboardState struct {
	VaultDir    string               `json:"vaultDir"`
	UpdatedAt   string               `json:"updatedAt"`
	Quota       kernel.QuotaState    `json:"quota"`
	Pending     []approvals.Proposal `json:"pending"`
	Worktrees   []string             `json:"worktrees"`
	Tasks       []kernel.ActiveTask  `json:"tasks"`
	TodayTurns  []hooks.JournalEntry `json:"todayTurns"`
	Health      []doctor.CheckResult `json:"health"`
	FlashResult string               `json:"flashResult,omitempty"`
}

// NewHandler creates the HTTP mux for the HQ Console side pane and API.
func NewHandler(vaultDir string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		state := loadState(vaultDir, "")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state)
	})

	mux.HandleFunc("/api/approve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		id := strings.TrimSpace(r.FormValue("id"))
		sha := strings.TrimSpace(r.FormValue("sha256"))
		out, err := approvals.Approve(vaultDir, id, sha)
		msg := fmt.Sprintf("Approved %s", id)
		if out != "" {
			msg += ": " + strings.TrimSpace(out)
		}
		if err != nil {
			msg = fmt.Sprintf("Error approving %s: %v (%s)", id, err, strings.TrimSpace(out))
		}
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      err == nil,
				"message": msg,
			})
			return
		}
		state := loadState(vaultDir, msg)
		renderHTML(w, state)
	})

	mux.HandleFunc("/api/reject", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		id := strings.TrimSpace(r.FormValue("id"))
		reason := strings.TrimSpace(r.FormValue("reason"))
		if reason == "" {
			reason = "Rejected via HQ Console"
		}
		err := approvals.Reject(vaultDir, id, reason)
		msg := fmt.Sprintf("Rejected %s", id)
		if err != nil {
			msg = fmt.Sprintf("Error rejecting %s: %v", id, err)
		}
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      err == nil,
				"message": msg,
			})
			return
		}
		state := loadState(vaultDir, msg)
		renderHTML(w, state)
	})

	mux.HandleFunc("/api/tasks/complete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		slug := strings.TrimSpace(r.FormValue("slug"))
		err := kernel.CompleteTask(vaultDir, slug, "")
		_ = SyncMarkdownConsole(vaultDir)
		msg := fmt.Sprintf("Completed task %s", slug)
		if err != nil {
			msg = fmt.Sprintf("Error completing %s: %v", slug, err)
		}
		state := loadState(vaultDir, msg)
		renderHTML(w, state)
	})

	mux.HandleFunc("/api/tasks/archive", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		slug := strings.TrimSpace(r.FormValue("slug"))
		err := kernel.ArchiveTask(vaultDir, slug, false)
		_ = SyncMarkdownConsole(vaultDir)
		msg := fmt.Sprintf("Archived task %s", slug)
		if err != nil {
			msg = fmt.Sprintf("Error archiving %s: %v", slug, err)
		}
		state := loadState(vaultDir, msg)
		renderHTML(w, state)
	})

	mux.HandleFunc("/favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte(faviconSVG))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		state := loadState(vaultDir, "")
		renderHTML(w, state)
	})

	return mux
}

const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024" width="64" height="64"><defs><linearGradient id="bg" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="#1C202B"/><stop offset="100%" stop-color="#0E1017"/></linearGradient><radialGradient id="wl" cx="286" cy="398" r="240" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#8AE05C" stop-opacity="0.95"/><stop offset="60%" stop-color="#8AE05C" stop-opacity="0.35"/><stop offset="100%" stop-color="#378DFC" stop-opacity="0"/></radialGradient><radialGradient id="wc" cx="764" cy="462" r="230" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#F85852" stop-opacity="0.95"/><stop offset="65%" stop-color="#F85852" stop-opacity="0.35"/><stop offset="100%" stop-color="#378DFC" stop-opacity="0"/></radialGradient><radialGradient id="wo" cx="672" cy="344" r="210" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#FF9A26" stop-opacity="0.98"/><stop offset="60%" stop-color="#FF9A26" stop-opacity="0.40"/><stop offset="100%" stop-color="#F85852" stop-opacity="0"/></radialGradient><radialGradient id="ol" cx="383" cy="347" r="135" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#9CD64A" stop-opacity="0.95"/><stop offset="65%" stop-color="#9CD64A" stop-opacity="0.30"/><stop offset="100%" stop-color="#378DFC" stop-opacity="0"/></radialGradient><radialGradient id="oc" cx="553" cy="341" r="135" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#F65A4C" stop-opacity="0.95"/><stop offset="65%" stop-color="#F65A4C" stop-opacity="0.30"/><stop offset="100%" stop-color="#378DFC" stop-opacity="0"/></radialGradient><radialGradient id="oo" cx="468" cy="290" r="140" gradientUnits="userSpaceOnUse"><stop offset="0%" stop-color="#FC8E26" stop-opacity="1"/><stop offset="58%" stop-color="#FC8E26" stop-opacity="0.45"/><stop offset="100%" stop-color="#F65A4C" stop-opacity="0"/></radialGradient></defs><rect x="94" y="94" width="836" height="836" rx="192" fill="url(#bg)"/><path d="M 455 768 C 625 768, 745 535, 816 312 C 828 274, 790 262, 770 298 C 700 435, 605 616, 465 616 C 365 616, 295 578, 242 518 C 218 492, 186 518, 210 552 C 268 668, 345 768, 455 768 Z" fill="#378DFC"/><path d="M 455 768 C 625 768, 745 535, 816 312 C 828 274, 790 262, 770 298 C 700 435, 605 616, 465 616 C 365 616, 295 578, 242 518 C 218 492, 186 518, 210 552 C 268 668, 345 768, 455 768 Z" fill="url(#wl)"/><path d="M 455 768 C 625 768, 745 535, 816 312 C 828 274, 790 262, 770 298 C 700 435, 605 616, 465 616 C 365 616, 295 578, 242 518 C 218 492, 186 518, 210 552 C 268 668, 345 768, 455 768 Z" fill="url(#wc)"/><path d="M 455 768 C 625 768, 745 535, 816 312 C 828 274, 790 262, 770 298 C 700 435, 605 616, 465 616 C 365 616, 295 578, 242 518 C 218 492, 186 518, 210 552 C 268 668, 345 768, 455 768 Z" fill="url(#wo)"/><circle cx="468" cy="398" r="142" fill="#378DFC"/><circle cx="468" cy="398" r="142" fill="url(#ol)"/><circle cx="468" cy="398" r="142" fill="url(#oc)"/><circle cx="468" cy="398" r="142" fill="url(#oo)"/></svg>`

func loadState(vaultDir, flashMsg string) DashboardState {
	pending, _ := approvals.List(vaultDir)
	wts, _ := worktree.List(vaultDir)
	tasks, _ := kernel.ListTasks(vaultDir)
	layer1Tasks := kernel.FilterLayer1Tasks(vaultDir, tasks)
	now := time.Now()
	sel := kernel.SelectConsoleTasks(layer1Tasks, kernel.CountColdArchivedTasks(vaultDir), now)
	turns := loadTodayJournals(vaultDir)
	health := doctor.Run(vaultDir)
	quota := kernel.LoadQuota(vaultDir)
	return DashboardState{
		VaultDir:    vaultDir,
		UpdatedAt:   now.Format("15:04:05"),
		Quota:       quota,
		Pending:     pending,
		Worktrees:   wts,
		Tasks:       sel.VisibleRows,
		TodayTurns:  turns,
		Health:      health,
		FlashResult: flashMsg,
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
	"base": filepath.Base,
}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="icon" type="image/svg+xml" href="/favicon.svg">
<title>agy-hq Console</title>
<style>
  :root {
    --bg: #0f1117;
    --card: #181b24;
    --border: #2a2f3d;
    --text: #e6edf3;
    --muted: #8b949e;
    --accent: #58a6ff;
    --ok: #2ea043;
    --warn: #d29922;
    --danger: #f85149;
  }
  body {
    margin: 0;
    padding: 16px;
    background: var(--bg);
    color: var(--text);
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
    font-size: 13px;
    line-height: 1.45;
  }
  header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    border-bottom: 1px solid var(--border);
    padding-bottom: 10px;
    margin-bottom: 14px;
  }
  h1 { font-size: 15px; margin: 0; }
  h2 { font-size: 12px; text-transform: uppercase; letter-spacing: 0.06em; color: var(--muted); margin: 16px 0 8px; }
  .card {
    background: var(--card);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 10px 12px;
    margin-bottom: 8px;
  }
  .flash {
    background: #1f2d1f;
    border: 1px solid var(--ok);
    border-radius: 6px;
    padding: 8px 12px;
    margin-bottom: 12px;
  }
  .badge {
    display: inline-block;
    padding: 2px 6px;
    border-radius: 4px;
    font-size: 11px;
    font-weight: 600;
  }
  .PASS { background: rgba(46,160,67,0.2); color: #56d364; }
  .WARN { background: rgba(210,153,34,0.2); color: #e3b341; }
  pre {
    background: #090b0f;
    padding: 6px 8px;
    border-radius: 4px;
    overflow-x: auto;
    margin: 6px 0;
    font-size: 12px;
  }
  .actions { display: flex; gap: 8px; margin-top: 8px; }
  button {
    cursor: pointer;
    border: none;
    border-radius: 4px;
    padding: 5px 10px;
    font-size: 12px;
    font-weight: 600;
  }
  .btn-approve { background: var(--ok); color: #fff; }
  .btn-reject { background: transparent; border: 1px solid var(--danger); color: var(--danger); }
  .muted { color: var(--muted); font-size: 12px; }
</style>
</head>
<body>
<header>
  <h1 style="display:flex;align-items:center;gap:8px;">
    <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024" width="22" height="22" style="flex-shrink:0">
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
    agy-hq Console
  </h1>
  <span class="muted">Updated {{.UpdatedAt}} · <a href="/" style="color:var(--accent)">Refresh</a></span>
</header>

{{if .FlashResult}}
<div class="flash">{{.FlashResult}}</div>
{{end}}

<h2>Proactive Gatekeeper & Daily Quota</h2>
<div class="card">
  <div>
    <strong>Background Flash Spawns Today:</strong> {{.Quota.SpawnsToday}} / {{.Quota.MaxDailySpawns}}
    {{if .Quota.CircuitOpen}}
      <span class="badge WARN">CIRCUIT OPEN ({{.Quota.CircuitReason}})</span>
    {{else}}
      <span class="badge PASS">BUDGET OK</span>
    {{end}}
  </div>
  <div class="muted">Last Heartbeat: {{if .Quota.LastStatus}}{{.Quota.LastStatus}}{{else}}Not run yet today{{end}}</div>
</div>

<h2>Pending Approvals ({{len .Pending}})</h2>
{{if .Pending}}
  {{range .Pending}}
  <div class="card">
    <div><strong>{{.Summary}}</strong> <span class="badge WARN">{{.ActionType}}</span></div>
    <div class="muted">ID: {{.ID}} · SHA-256: {{shortSHA .SHA256}}</div>
    <pre>{{.Command}}</pre>
    <div class="actions">
      <form method="POST" action="/api/approve">
        <input type="hidden" name="id" value="{{.ID}}">
        <input type="hidden" name="sha256" value="{{shortSHA .SHA256}}">
        <button type="submit" class="btn-approve">Approve ({{shortSHA .SHA256}})</button>
      </form>
      <form method="POST" action="/api/reject">
        <input type="hidden" name="id" value="{{.ID}}">
        <button type="submit" class="btn-reject">Reject</button>
      </form>
    </div>
  </div>
  {{end}}
{{else}}
  <div class="card muted">No pending outbound actions.</div>
{{end}}

<h2>Live Task Board ({{len .Tasks}})</h2>
{{if .Tasks}}
  {{range .Tasks}}
  <div class="card">
    <div><strong>{{.Title}}</strong> <span class="badge {{if eq .Status "active"}}PASS{{else}}WARN{{end}}">{{.Status}}</span></div>
    <div class="muted">Slug: <code>{{.TaskSlug}}</code> · Model: {{.Model}} · Conv: {{.ConversationID}}</div>
    {{if .LastSummary}}<div class="muted" style="margin-top:4px">{{.LastSummary}}</div>{{end}}
    <div class="actions">
      {{if eq .Status "active"}}
      <form method="POST" action="/api/tasks/complete">
        <input type="hidden" name="slug" value="{{.TaskSlug}}">
        <button type="submit" class="btn-approve">Complete</button>
      </form>
      {{end}}
      {{if ne .Status "archived"}}
      <form method="POST" action="/api/tasks/archive">
        <input type="hidden" name="slug" value="{{.TaskSlug}}">
        <button type="submit" class="btn-reject">Archive</button>
      </form>
      {{end}}
    </div>
  </div>
  {{end}}
{{else}}
  <div class="card muted">No active tasks on the board.</div>
{{end}}

<h2>Active Worktrees ({{len .Worktrees}})</h2>
{{if .Worktrees}}
  {{range .Worktrees}}
  <div class="card"><code>{{base .}}</code> <span class="muted">({{.}})</span></div>
  {{end}}
{{else}}
  <div class="card muted">No active git worktrees in local/worktrees/.</div>
{{end}}

<h2>Today's Turn Journal ({{len .TodayTurns}})</h2>
{{if .TodayTurns}}
  {{range .TodayTurns}}
  <div class="card">
    <div><strong>{{.UserPrompt}}</strong></div>
    <div class="muted">{{.Timestamp}} · conv: {{.ConversationID}}</div>
  </div>
  {{end}}
{{else}}
  <div class="card muted">No turns logged today yet.</div>
{{end}}

<h2>System Health</h2>
{{range .Health}}
<div class="card">
  <span class="badge {{.Status}}">{{.Status}}</span>
  <strong>{{.Name}}</strong>
  <div class="muted">{{.Detail}}</div>
</div>
{{end}}
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

	if len(state.Pending) > 0 {
		fmt.Fprintf(&b, "> [!IMPORTANT]\n> **Pending Approvals (`%d` item(s) waiting):**\n", len(state.Pending))
		for _, p := range state.Pending {
			short := p.SHA256
			if len(short) > 12 {
				short = short[:12]
			}
			fmt.Fprintf(&b, "> - **`%s`** (`%s` · `sha256:%s`): %s — `%s`\n", p.ID, p.ActionType, short, p.Summary, p.Command)
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
	fmt.Fprintf(&b, "- **Background Flash Spawns Today:** `%d / %d` (`%s`)\n", state.Quota.SpawnsToday, state.Quota.MaxDailySpawns, circuitBadge)
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
