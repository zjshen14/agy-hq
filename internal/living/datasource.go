package living

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

type trustedSourcePayload struct {
	Columns []string    `json:"columns,omitempty"`
	Rows    [][]string  `json:"rows,omitempty"`
	Metrics []KPIMetric `json:"metrics,omitempty"`
}

// RefreshDataSources evaluates all declared 0-token `dataSources` on a Living Space (`hq_builtin` or
// `trusted_argv` verified against <vault>/private/trust.json) and updates bound blocks atomically.
func RefreshDataSources(ctx context.Context, vaultDir, slug string, now time.Time) (SpaceDoc, error) {
	if now.IsZero() {
		now = time.Now()
	}
	doc, err := Load(vaultDir, slug)
	if err != nil {
		return SpaceDoc{}, err
	}
	if len(doc.DataSources) == 0 {
		return doc, nil
	}

	nowStr := now.UTC().Format(time.RFC3339)
	for i := range doc.DataSources {
		ds := &doc.DataSources[i]
		ds.LastRefreshedAt = nowStr
		ds.LastError = ""

		var cols []LocalizedText
		var rows []TableRow
		var metrics []KPIMetric
		var rErr error

		switch strings.ToLower(strings.TrimSpace(ds.Kind)) {
		case "hq_builtin":
			cols, rows, metrics, rErr = evalBuiltinDataSource(vaultDir, ds.Target)
		case "trusted_argv":
			cols, rows, metrics, rErr = evalTrustedArgvDataSource(ctx, vaultDir, *ds)
		default:
			rErr = fmt.Errorf("unsupported dataSource kind %q", ds.Kind)
		}
		if rErr != nil {
			ds.LastError = rErr.Error()
			continue
		}

		// Apply refreshed rows/metrics to every block referencing this DataSourceID.
		for bi := range doc.Blocks {
			if doc.Blocks[bi].DataSourceID != ds.ID {
				continue
			}
			switch doc.Blocks[bi].Type {
			case BlockComparisonTable:
				if len(cols) > 0 {
					doc.Blocks[bi].Columns = cols
				}
				doc.Blocks[bi].Rows = rows
			case BlockKPIStrip:
				if len(metrics) > 0 {
					doc.Blocks[bi].Metrics = metrics
				}
			}
		}
	}
	return Save(vaultDir, doc, now)
}

func evalBuiltinDataSource(vaultDir, target string) ([]LocalizedText, []TableRow, []KPIMetric, error) {
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "workspaces":
		ledger, err := workspace.LoadLedger(vaultDir)
		if err != nil {
			return nil, nil, nil, err
		}
		cols := []LocalizedText{Text("Workspace"), Text("Provider"), Text("State"), Text("Open Changes"), Text("Needs Owner")}
		var keys []string
		for k := range ledger.Entries {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var rows []TableRow
		totalNeeds := 0
		for _, k := range keys {
			e := ledger.Entries[k]
			openCh := workspace.CountOpenChanges(e.Status.Changes)
			needs := 0
			for _, c := range e.Status.Changes {
				if c.NeedsOwner {
					needs++
				}
			}
			totalNeeds += needs
			badge := "ok"
			if needs > 0 {
				badge = "warn"
			}
			rows = append(rows, TableRow{
				Cells: []LocalizedText{
					Text(e.Name),
					Text(e.Provider),
					Text(string(e.Status.State)),
					Text(fmt.Sprintf("%d", openCh)),
					Text(fmt.Sprintf("%d", needs)),
				},
				Badge: badge,
			})
		}
		metrics := []KPIMetric{
			{Label: Text("Workspaces"), Value: Text(fmt.Sprintf("%d", len(rows))), Tone: "neutral"},
			{Label: Text("Needs Owner"), Value: Text(fmt.Sprintf("%d", totalNeeds)), Tone: toneIfPos(totalNeeds)},
		}
		return cols, rows, metrics, nil

	case "goals":
		goals, err := kernel.ListGoals(vaultDir)
		if err != nil {
			return nil, nil, nil, err
		}
		cols := []LocalizedText{Text("Goal"), Text("Scope"), Text("Status"), Text("Readiness"), Text("Target")}
		var rows []TableRow
		activeCount := 0
		for _, g := range goals {
			if g.Status == "completed" {
				continue
			}
			activeCount++
			p := g.Readiness()
			readiness := fmt.Sprintf("%d/%d (%d%%)", p.Confirmed, p.Active, p.Percent)
			if p.Skipped > 0 {
				readiness += fmt.Sprintf(" · %d skipped", p.Skipped)
			}
			targetDate := g.WindowStartAt
			if targetDate == "" {
				targetDate = g.TargetDate
			}
			status := g.Status
			if g.ParkedUntil != "" {
				status = "parked (" + g.ParkedUntil + ")"
			}
			rows = append(rows, TableRow{
				Cells: []LocalizedText{Text(g.Title), Text(g.Scope), Text(status), Text(readiness), Text(targetDate)},
			})
		}
		metrics := []KPIMetric{{Label: Text("Active Goals"), Value: Text(fmt.Sprintf("%d", activeCount)), Tone: "accent"}}
		return cols, rows, metrics, nil

	case "delegates":
		qDir := filepath.Join(vaultDir, "memory", "delegates_queues")
		entries, _ := os.ReadDir(qDir)
		cols := []LocalizedText{Text("Queue"), Text("Pending Files")}
		var rows []TableRow
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			rows = append(rows, TableRow{Cells: []LocalizedText{Text(strings.TrimSuffix(e.Name(), ".jsonl")), Text("active")}})
		}
		metrics := []KPIMetric{{Label: Text("Delegate Queues"), Value: Text(fmt.Sprintf("%d", len(rows))), Tone: "neutral"}}
		return cols, rows, metrics, nil

	case "obs":
		tasks, _ := kernel.ListTasks(vaultDir)
		activeTasks := 0
		for _, t := range tasks {
			if t.Status == "active" {
				activeTasks++
			}
		}
		cols := []LocalizedText{Text("Metric"), Text("Value")}
		rows := []TableRow{{Cells: []LocalizedText{Text("Active Tasks"), Text(fmt.Sprintf("%d", activeTasks))}}}
		metrics := []KPIMetric{{Label: Text("Active Tasks"), Value: Text(fmt.Sprintf("%d", activeTasks)), Tone: "neutral"}}
		return cols, rows, metrics, nil

	default:
		return nil, nil, nil, fmt.Errorf("unknown hq_builtin target %q", target)
	}
}

func toneIfPos(n int) string {
	if n > 0 {
		return "warn"
	}
	return "ok"
}

func evalTrustedArgvDataSource(ctx context.Context, vaultDir string, ds DataSourceSpec) ([]LocalizedText, []TableRow, []KPIMetric, error) {
	if len(ds.Argv) == 0 || strings.TrimSpace(ds.Argv[0]) == "" {
		return nil, nil, nil, fmt.Errorf("trusted_argv dataSource %q requires non-empty argv", ds.ID)
	}
	bin0 := strings.TrimSpace(ds.Argv[0])
	if extension.IsBlockedComponentCommand0(bin0) {
		return nil, nil, nil, fmt.Errorf("blocked shell/interpreter %q in trusted_argv dataSource %q", bin0, ds.ID)
	}
	verifiedBin, err := extension.VerifyTrustedBinary(vaultDir, "vault", bin0)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("trusted_argv binary %q failed trust verification: %w", bin0, err)
	}
	timeout := 5 * time.Second
	if ds.TimeoutMs > 0 {
		timeout = time.Duration(ds.TimeoutMs) * time.Millisecond
		if timeout > 15*time.Second {
			timeout = 15 * time.Second
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, verifiedBin, ds.Argv[1:]...)
	cmd.Env = extension.SanitizedDaemonEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("trusted_argv %q execution failed: %w", ds.ID, err)
	}
	var payload trustedSourcePayload
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, nil, nil, fmt.Errorf("trusted_argv %q returned invalid JSON: %w", ds.ID, err)
	}
	var cols []LocalizedText
	for _, c := range payload.Columns {
		cols = append(cols, Text(c))
	}
	var rows []TableRow
	for _, r := range payload.Rows {
		cells := make([]LocalizedText, 0, len(r))
		for _, cell := range r {
			cells = append(cells, Text(cell))
		}
		rows = append(rows, TableRow{Cells: cells})
	}
	return cols, rows, payload.Metrics, nil
}
