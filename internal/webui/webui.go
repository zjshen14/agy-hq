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

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		state := loadState(vaultDir, "")
		renderHTML(w, state)
	})

	return mux
}

func loadState(vaultDir, flashMsg string) DashboardState {
	pending, _ := approvals.List(vaultDir)
	wts, _ := worktree.List(vaultDir)
	tasks, _ := kernel.ListTasks(vaultDir)
	turns := loadTodayJournals(vaultDir)
	health := doctor.Run(vaultDir)
	quota := kernel.LoadQuota(vaultDir)
	return DashboardState{
		VaultDir:    vaultDir,
		UpdatedAt:   time.Now().Format("15:04:05"),
		Quota:       quota,
		Pending:     pending,
		Worktrees:   wts,
		Tasks:       tasks,
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
  <h1>🏛️ agy-hq Console</h1>
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

	fmt.Fprintf(&b, "## 📋 2. Task Board (`%d` Tracked Conversations)\n", len(state.Tasks))
	if len(state.Tasks) == 0 {
		b.WriteString("- None registered in `local/memory/tasks.json`.\n\n---\n\n")
	} else {
		b.WriteString("| Status | Task Conversation | Slug | Latest Progress |\n| :--- | :--- | :--- | :--- |\n")
		for _, t := range state.Tasks {
			oneLine := strings.Join(strings.Fields(t.LastSummary), " ")
			oneLine = strings.ReplaceAll(oneLine, "|", "\\|")
			if runes := []rune(oneLine); len(runes) > 130 {
				oneLine = string(runes[:130]) + "..."
			}
			badge := "🟢 `active`"
			if strings.EqualFold(t.Status, "completed") {
				badge = "✅ `completed`"
			}
			fmt.Fprintf(&b, "| %s | **[%s](conversation://%s)** | `%s` | %s |\n", badge, t.Title, t.ConversationID, t.TaskSlug, oneLine)
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
			prompt := strings.Join(strings.Fields(j.UserPrompt), " ")
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
		if _, err := os.Stat(artDir); err == nil {
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
	}
	return nil
}
