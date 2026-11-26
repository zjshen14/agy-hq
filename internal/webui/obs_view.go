package webui

import (
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/obs"
)

// ConsoleActionCount returns the total number of items awaiting user action in Needs Me.
func ConsoleActionCount(state DashboardState) int {
	return len(state.Pending) + len(state.ProactiveV3.ExpiredProposals) + len(state.ProactiveV3.DecisionCards)
}

// ConsoleDefaultTab selects "needs_me" when user action is required, else "work" when tasks are active.
func ConsoleDefaultTab(state DashboardState) string {
	if ConsoleActionCount(state) > 0 {
		return "needs_me"
	}
	if state.TaskSelection.ActiveCount > 0 {
		return "work"
	}
	return "needs_me"
}

// FormatUSD formats an API-equivalent USD estimate cleanly without custom quota jargon.
func FormatUSD(v float64) string {
	if v <= 0 {
		return "$0.00"
	}
	if v < 0.01 {
		return "<$0.01"
	}
	return fmt.Sprintf("~$%.2f", v)
}

// ContextFillPct computes clamped context fill percentage against ceiling (defaulting to 512k).
func ContextFillPct(cur, ceiling int64) float64 {
	if ceiling <= 0 {
		ceiling = obs.DefaultContextCeilingTokens
	}
	if cur <= 0 {
		return 0
	}
	p := (float64(cur) / float64(ceiling)) * 100.0
	if p > 100 {
		return 100
	}
	return p
}

// StateBootstrapJSON serializes DashboardState safely for inline <script type="application/json"> hydration.
func StateBootstrapJSON(state DashboardState) template.HTML {
	cp := state
	cp.CSRFToken = ""
	b, err := json.Marshal(cp)
	if err != nil {
		return "{}"
	}
	safe := strings.ReplaceAll(string(b), "</", `<\/`)
	return template.HTML(safe)
}

func computeObsStateStamp(vaultDir string) int64 {
	_, _, fdSize, fdMod := obs.FrontDeskTranscriptStamp(vaultDir)
	stamp := fdMod ^ (fdSize << 13)
	if st, err := os.Stat(filepath.Join(vaultDir, "memory", "obs", "index.json")); err == nil {
		stamp ^= st.ModTime().UnixNano() ^ (st.Size() << 7)
	}
	return stamp
}

func consoleTemplateFuncs() template.FuncMap {
	return template.FuncMap{
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
		"fmtTokens":   obs.FormatTokensCompact,
		"fmtUSD":      FormatUSD,
		"actionCount": ConsoleActionCount,
		"defaultTab":  ConsoleDefaultTab,
		"stateJSON":   StateBootstrapJSON,
		"pctClamp": func(p float64) float64 {
			if p < 0 {
				return 0
			}
			if p > 100 {
				return 100
			}
			return p
		},
		"ctxPct": func(cur, ceiling int64) float64 {
			return ContextFillPct(cur, ceiling)
		},
		"effectiveCeiling": func(b obs.BudgetConfig) int64 {
			return b.EffectiveContextCeiling()
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
	}
}
