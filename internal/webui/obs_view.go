package webui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/living"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/store"
)

// ConsoleLivingSpaceSlug is the reserved Living Space slug for the self-contained
// mobile/artifact HTML snapshot rendered from the unified console.html template.
const ConsoleLivingSpaceSlug = "hq-console"

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
func StateBootstrapJSON(state DashboardState) template.JS {
	var payload interface{}
	if state.StaticMode {
		obsCopy := state.Observability
		obsCopy.TaskBadges = nil
		payload = map[string]interface{}{
			"observability": obsCopy,
			"memory":        state.Memory,
		}
	} else {
		cp := state
		cp.CSRFToken = ""
		cp.TodayTurns = nil
		payload = cp
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	safe := strings.ReplaceAll(string(b), "</", `<\/`)
	return template.JS(safe)
}

// RenderStaticConsoleHTML renders the unified 4-tab console.html template in offline/snapshot mode
// (zero external assets, zero /api/state polling, 1-tap Copy CLI command buttons, strict CSP compatible).
func RenderStaticConsoleHTML(state DashboardState) ([]byte, error) {
	staticState := state
	staticState.StaticMode = true
	staticState.CSRFToken = ""
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, staticState); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SyncConsoleLiveArtifact compiles DashboardState using the unified console.html template into
// `<vault>/living_spaces/hq-console/index.html` and `<frontDesk.ArtifactDir>/hq_console_live.html`
// (+ `.metadata.json`) in 0 LLM tokens so Antigravity Mobile / Remote Control users can open the
// exact same 4-tab interactive Console inside the Artifact Pane.
func SyncConsoleLiveArtifact(vaultDir string, state DashboardState, unchangedBody bool) error {
	spaceDir := living.SpaceDir(vaultDir, ConsoleLivingSpaceSlug)
	indexPath := filepath.Join(spaceDir, "index.html")
	_, errIdx := os.Stat(indexPath)

	artDir := ""
	var artHTML, artMeta string
	var errArt, errMeta error
	if state.hasFrontDesk && strings.TrimSpace(state.frontDesk.ArtifactDir) != "" {
		artDir = strings.TrimSpace(state.frontDesk.ArtifactDir)
		artHTML = filepath.Join(artDir, "hq_console_live.html")
		artMeta = filepath.Join(artDir, "hq_console_live.html.metadata.json")
		_, errArt = os.Stat(artHTML)
		_, errMeta = os.Stat(artMeta)
	}
	if unchangedBody && errIdx == nil && (artDir == "" || (errArt == nil && errMeta == nil)) {
		if artDir != "" {
			syncUserLivingSpaceArtifacts(vaultDir, artDir, state.ProactiveV3.LivingSpaces)
		}
		return nil
	}

	rawHTML, err := RenderStaticConsoleHTML(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(spaceDir, 0o755); err != nil {
		return err
	}
	if err := store.WriteAtomic(indexPath, rawHTML, 0o644); err != nil {
		return err
	}
	if artDir != "" {
		_ = os.MkdirAll(artDir, 0o755)
		meta := map[string]interface{}{
			"summary":         "Live Interactive [HQ] Console (Mobile & Artifact Pane) — Needs Me, Work, Memory, Tokens, and Ops",
			"updatedAt":       time.Now().UTC().Format(time.RFC3339Nano),
			"requestFeedback": false,
			"userFacing":      true,
		}
		metaBytes, _ := json.MarshalIndent(meta, "", "  ")
		_ = store.WriteAtomic(artHTML, rawHTML, 0o644)
		_ = store.WriteAtomic(artMeta, metaBytes, 0o644)
		syncUserLivingSpaceArtifacts(vaultDir, artDir, state.ProactiveV3.LivingSpaces)
	}
	return nil
}

// syncUserLivingSpaceArtifacts mirrors custom Living Spaces into Front Desk's ArtifactDir as
// `space_<slug>.html` (+ `.metadata.json`) when missing or updated so Mobile Remote Control users
// can open every Living Space directly in the Artifact Pane without needing localhost:4242.
func syncUserLivingSpaceArtifacts(vaultDir, artDir string, spaces []living.SpaceSummary) {
	if artDir == "" || len(spaces) == 0 {
		return
	}
	for _, ls := range spaces {
		if ls.Slug == "" || ls.Slug == ConsoleLivingSpaceSlug {
			continue
		}
		srcHTML := filepath.Join(living.SpaceDir(vaultDir, ls.Slug), "index.html")
		raw, err := os.ReadFile(srcHTML)
		if err != nil || len(raw) == 0 {
			continue
		}
		dstHTML := filepath.Join(artDir, "space_"+ls.Slug+".html")
		dstMeta := dstHTML + ".metadata.json"
		if prev, rErr := os.ReadFile(dstHTML); rErr == nil && bytes.Equal(prev, raw) {
			if _, mErr := os.Stat(dstMeta); mErr == nil {
				continue
			}
		}
		_ = os.MkdirAll(artDir, 0o755)
		title := strings.TrimSpace(ls.Title)
		if title == "" {
			title = ls.Slug
		}
		meta := map[string]interface{}{
			"summary":         fmt.Sprintf("Living Space: %s (Mobile & Artifact Pane ready)", title),
			"updatedAt":       time.Now().UTC().Format(time.RFC3339Nano),
			"requestFeedback": false,
			"userFacing":      true,
		}
		metaBytes, _ := json.MarshalIndent(meta, "", "  ")
		_ = store.WriteAtomic(dstHTML, raw, 0o644)
		_ = store.WriteAtomic(dstMeta, metaBytes, 0o644)
	}
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
