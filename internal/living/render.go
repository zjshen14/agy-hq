package living

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"
)

// RenderOptions configures HTML compilation for either `:4242` sandboxed WebUI or Antigravity `generative_ui` artifact.
type RenderOptions struct {
	Surface string // "webui" | "artifact" | "standalone"
}

type i18nCollector struct {
	locales []string
	primary string
	dicts   map[string]map[string]string
	seq     int
}

func newI18nCollector(locales []string) *i18nCollector {
	if len(locales) == 0 {
		locales = []string{"en"}
	}
	dicts := make(map[string]map[string]string, len(locales))
	for _, loc := range locales {
		dicts[loc] = make(map[string]string)
	}
	return &i18nCollector{
		locales: locales,
		primary: locales[0],
		dicts:   dicts,
	}
}

func (c *i18nCollector) add(lt LocalizedText) (key string, primaryText string) {
	c.seq++
	key = fmt.Sprintf("k%d", c.seq)
	primaryText = lt.Resolve(c.primary, "en")
	for _, loc := range c.locales {
		c.dicts[loc][key] = lt.Resolve(loc, c.primary)
	}
	return key, primaryText
}

func (c *i18nCollector) span(lt LocalizedText) string {
	key, primary := c.add(lt)
	if len(c.locales) <= 1 {
		return html.EscapeString(primary)
	}
	return fmt.Sprintf(`<span data-i18n="%s">%s</span>`, key, html.EscapeString(primary))
}

func safeJSONForScript(v interface{}) string {
	b, _ := json.Marshal(v)
	// Neutralize </script> breakout sequences inside JSON script tags.
	return strings.ReplaceAll(string(b), "</", `<\/`)
}

func attrList(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	cleaned := make([]string, 0, len(vals))
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			cleaned = append(cleaned, strings.ToLower(s))
		}
	}
	return strings.Join(cleaned, ",")
}

func safeHref(raw string) string {
	u := strings.TrimSpace(raw)
	low := strings.ToLower(u)
	if strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "http://") {
		return html.EscapeString(u)
	}
	if strings.HasPrefix(low, "conversation://") {
		rest := u[len("conversation://"):]
		if rest != "" && !strings.ContainsAny(rest, " \t\n\r\"'<>") {
			return html.EscapeString(u)
		}
	}
	return ""
}

// RenderHTML compiles a canonical SpaceDoc into a self-contained, XSS-safe, CSP-clean HTML5 micro-app.
func RenderHTML(doc SpaceDoc, opts RenderOptions) ([]byte, error) {
	locales := doc.Locales
	if len(locales) == 0 {
		locales = []string{"en"}
	}
	col := newI18nCollector(locales)
	primary := locales[0]
	pageTitle := doc.Title.Resolve(primary, "en")
	if pageTitle == "" {
		pageTitle = doc.Slug
	}
	theme := normalizeTheme(doc.Theme)

	var body strings.Builder

	// Editorial Hero Header
	body.WriteString(`<header class="hdr"><div class="hdr-main">`)
	if !doc.HeroBadge.IsEmpty() {
		body.WriteString(`<div class="hero-badge">` + col.span(doc.HeroBadge) + `</div>`)
	}
	body.WriteString(`<h1>` + col.span(doc.Title) + `</h1>`)
	if !doc.Subtitle.IsEmpty() {
		body.WriteString(`<p class="sub">` + col.span(doc.Subtitle) + `</p>`)
	}
	body.WriteString(`</div><div class="hdr-tools">`)

	if len(locales) > 1 {
		body.WriteString(`<div class="lang-switch" role="group" aria-label="Language">`)
		for idx, loc := range locales {
			lbl := doc.LocaleLabels[loc]
			if lbl == "" {
				lbl = strings.ToUpper(loc)
			}
			activeCls := ""
			if idx == 0 {
				activeCls = " active"
			}
			body.WriteString(fmt.Sprintf(`<button type="button" class="pill lang-btn%s" data-lang="%s">%s</button>`,
				activeCls, html.EscapeString(loc), html.EscapeString(lbl)))
		}
		body.WriteString(`</div>`)
	}
	body.WriteString(`<button type="button" class="pill mode-btn" id="hq-mode-btn" title="Toggle light / dark mode" aria-label="Toggle color mode">◐</button>`)
	body.WriteString(`</div></header>`)

	// Sticky Navigation & Filter Controls
	hasControls := len(doc.Perspectives) > 0 || len(doc.Groups) > 0 || len(doc.Blocks) >= 2
	if hasControls {
		body.WriteString(`<nav class="controls">`)
		if len(doc.Perspectives) > 0 || len(doc.Groups) > 0 {
			body.WriteString(`<div class="ctrl-row">`)
			if len(doc.Perspectives) > 0 {
				body.WriteString(`<div class="ctrl-group" role="tablist">`)
				if !doc.HideAllPerspective {
					body.WriteString(`<button type="button" class="pill persp-btn active" data-persp="all">All Views</button>`)
				}
				for idx, p := range doc.Perspectives {
					activeCls := ""
					if doc.HideAllPerspective && idx == 0 {
						activeCls = " active"
					}
					body.WriteString(fmt.Sprintf(`<button type="button" class="pill persp-btn%s" data-persp="%s">%s</button>`,
						activeCls, html.EscapeString(strings.ToLower(p.ID)), col.span(p.Label)))
				}
				body.WriteString(`</div>`)
			}
			if len(doc.Groups) > 0 {
				grpPerspAttr := ""
				if len(doc.GroupPerspectives) > 0 {
					grpPerspAttr = fmt.Sprintf(` data-perspectives="%s"`, html.EscapeString(attrList(doc.GroupPerspectives)))
				}
				body.WriteString(`<div class="ctrl-group"` + grpPerspAttr + `>`)
				if !doc.HideAllGroup {
					body.WriteString(`<button type="button" class="pill grp-btn active" data-grp="all">All</button>`)
				}
				for idx, g := range doc.Groups {
					activeCls := ""
					if doc.HideAllGroup && idx == 0 {
						activeCls = " active"
					}
					body.WriteString(fmt.Sprintf(`<button type="button" class="pill grp-btn%s" data-grp="%s">%s</button>`,
						activeCls, html.EscapeString(strings.ToLower(g)), html.EscapeString(g)))
				}
				body.WriteString(`</div>`)
			}
			body.WriteString(`</div>`)
		}

		// Section quick-jump TOC + client-side search when Space has multiple blocks
		if len(doc.Blocks) >= 2 {
			body.WriteString(`<div class="ctrl-row ctrl-utility">`)
			if len(doc.Blocks) >= 3 {
				body.WriteString(`<div class="toc-strip" aria-label="Jump to section">`)
				for idx, blk := range doc.Blocks {
					if blk.Title.IsEmpty() {
						continue
					}
					anchor := blockAnchorID(blk, idx)
					perspAttr := attrList(blk.Perspectives)
					grpAttr := strings.ToLower(strings.TrimSpace(blk.Group))
					body.WriteString(fmt.Sprintf(`<a class="toc-pill" href="#%s" data-perspectives="%s" data-group="%s">%s</a>`,
						anchor, html.EscapeString(perspAttr), html.EscapeString(grpAttr), col.span(blk.Title)))
				}
				body.WriteString(`</div>`)
			}
			body.WriteString(`<input type="search" id="hq-space-search" class="space-search" placeholder="Filter this space…" aria-label="Filter items">`)
			body.WriteString(`</div>`)
		}
		body.WriteString(`</nav>`)
	}

	// Render each composable block
	for idx, blk := range doc.Blocks {
		anchor := blockAnchorID(blk, idx)
		perspAttr := attrList(blk.Perspectives)
		grpAttr := strings.ToLower(strings.TrimSpace(blk.Group))
		body.WriteString(fmt.Sprintf(`<section id="%s" class="block block-%s" data-perspectives="%s" data-group="%s">`,
			anchor, html.EscapeString(blk.Type), html.EscapeString(perspAttr), html.EscapeString(grpAttr)))
		if !blk.Title.IsEmpty() {
			body.WriteString(`<div class="blk-hdr"><h2>` + col.span(blk.Title) + `</h2>`)
			if blk.Type == BlockTimelineAccordion && len(blk.Entries) > 1 {
				body.WriteString(`<button type="button" class="mini-btn tl-toggle-all" data-expanded="0">Expand all</button>`)
			}
			body.WriteString(`</div>`)
		}
		switch blk.Type {
		case BlockKPIStrip:
			renderKPIStrip(&body, blk, doc.Timezone, col)
		case BlockTimelineAccordion:
			renderTimelineAccordion(&body, blk, col)
		case BlockComparisonTable:
			renderComparisonTable(&body, blk, col)
		case BlockChecklistLedger:
			renderChecklistLedger(&body, blk, doc.GoalSlug, col)
		case BlockCalloutNotes:
			renderCalloutNotes(&body, blk, col)
		case BlockMetricBars:
			renderMetricBars(&body, blk, col)
		case BlockInteractiveMap:
			renderInteractiveMap(&body, blk, col)
		case BlockStoryCards:
			renderStoryCards(&body, blk, col)
		}
		body.WriteString(`</section>`)
	}

	body.WriteString(fmt.Sprintf(`<footer class="ftr">Living Space <code>%s</code> · Updated %s · 0-token local build</footer>`,
		html.EscapeString(doc.Slug), html.EscapeString(doc.UpdatedAt)))

	defPersp := strings.ToLower(strings.TrimSpace(doc.DefaultPerspective))
	if defPersp == "" && doc.HideAllPerspective && len(doc.Perspectives) > 0 {
		defPersp = strings.ToLower(strings.TrimSpace(doc.Perspectives[0].ID))
	}
	defGrp := strings.ToLower(strings.TrimSpace(doc.DefaultGroup))
	if defGrp == "" && doc.HideAllGroup && len(doc.Groups) > 0 {
		defGrp = strings.ToLower(strings.TrimSpace(doc.Groups[0]))
	}
	i18nPayload := map[string]interface{}{
		"slug":               doc.Slug,
		"goalSlug":           doc.GoalSlug,
		"locales":            locales,
		"default":            primary,
		"defaultPerspective": defPersp,
		"defaultGroup":       defGrp,
	}
	if len(locales) > 1 {
		i18nPayload["dicts"] = col.dicts
	}
	i18nJSON := safeJSONForScript(i18nPayload)

	surfaceClass := "surface-webui"
	if opts.Surface == "artifact" {
		surfaceClass = "surface-artifact"
	}

	var docHTML strings.Builder
	docHTML.WriteString("<!DOCTYPE html>\n<html lang=\"" + html.EscapeString(primary) + "\" data-theme=\"" + html.EscapeString(theme) + "\">\n<head>\n<meta charset=\"utf-8\">\n")
	docHTML.WriteString("<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">\n")
	docHTML.WriteString("<title>" + html.EscapeString(pageTitle) + "</title>\n")
	docHTML.WriteString("<style>\n" + livingSpaceCSS + "\n</style>\n</head>\n<body class=\"" + surfaceClass + "\">\n<main class=\"wrap\">\n")
	docHTML.WriteString(body.String())
	docHTML.WriteString("\n</main>\n<script id=\"hq-i18n\" type=\"application/json\">" + i18nJSON + "</script>\n")
	docHTML.WriteString("<script>\n" + livingSpaceJS + "\n</script>\n</body>\n</html>\n")
	return []byte(docHTML.String()), nil
}

func blockAnchorID(blk BlockSpec, idx int) string {
	raw := strings.ToLower(strings.TrimSpace(blk.ID))
	if raw == "" {
		return fmt.Sprintf("blk-%d", idx+1)
	}
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return "blk-" + b.String()
}

func renderKPIStrip(sb *strings.Builder, blk BlockSpec, tz string, col *i18nCollector) {
	sb.WriteString(`<div class="kpi-grid">`)
	if blk.CountdownAt != "" {
		lbl := blk.CountdownLabel
		if lbl.IsEmpty() {
			lbl = Text("Countdown")
		}
		sb.WriteString(fmt.Sprintf(`<div class="kpi-card tone-accent"><div class="kpi-lbl">%s</div><div class="kpi-val hq-countdown" data-target="%s" data-tz="%s">%s</div></div>`,
			col.span(lbl), html.EscapeString(blk.CountdownAt), html.EscapeString(tz), html.EscapeString(blk.CountdownAt)))
	}
	if blk.ShowReadiness || blk.ConfirmedCount > 0 || blk.OpenCount > 0 || blk.SkippedCount > 0 {
		active := blk.ConfirmedCount + blk.OpenCount
		pct := 0
		if active > 0 {
			pct = (blk.ConfirmedCount * 100) / active
		}
		sb.WriteString(fmt.Sprintf(`<div class="kpi-card tone-ok" data-readiness-card="1" data-base-confirmed="%d" data-base-total="%d" data-base-skipped="%d"><div class="kpi-lbl">Readiness</div><div class="kpi-val" data-readiness-val="1">%d / %d (%d%%)</div><div class="bar"><span data-readiness-bar="1" style="width:%d%%"></span></div><div class="kpi-sub" data-readiness-sub="1">%d open · %d skipped</div></div>`,
			blk.ConfirmedCount, active, blk.SkippedCount, blk.ConfirmedCount, active, pct, pct, blk.OpenCount, blk.SkippedCount))
	}
	for _, m := range blk.Metrics {
		tone := safeTone(m.Tone, "neutral")
		sb.WriteString(fmt.Sprintf(`<div class="kpi-card tone-%s"><div class="kpi-lbl">%s</div><div class="kpi-val">%s</div></div>`,
			tone, col.span(m.Label), col.span(m.Value)))
	}
	sb.WriteString(`</div>`)
}

func renderTimelineAccordion(sb *strings.Builder, blk BlockSpec, col *i18nCollector) {
	sb.WriteString(`<div class="timeline">`)
	for idx, e := range blk.Entries {
		openAttr := ""
		if idx == 0 {
			openAttr = " open"
		}
		sb.WriteString(fmt.Sprintf(`<details class="tl-item" data-group="%s" data-perspectives="%s" data-searchable="1"%s><summary><span class="tl-node">%d</span><span class="tl-date">%s</span><strong class="tl-title">%s</strong>`,
			html.EscapeString(strings.ToLower(e.Group)), html.EscapeString(attrList(e.Perspectives)), openAttr, idx+1, col.span(e.DateLabel), col.span(e.Title)))
		if !e.Summary.IsEmpty() {
			sb.WriteString(`<span class="tl-sum">` + col.span(e.Summary) + `</span>`)
		}
		sb.WriteString(`</summary><div class="tl-body">`)
		if len(e.Details) > 0 {
			sb.WriteString(`<ul>`)
			for _, d := range e.Details {
				sb.WriteString(`<li>` + col.span(d) + `</li>`)
			}
			sb.WriteString(`</ul>`)
		}
		if !e.Callout.IsEmpty() {
			sb.WriteString(`<div class="tl-callout">` + col.span(e.Callout) + `</div>`)
		}
		sb.WriteString(`</div></details>`)
	}
	sb.WriteString(`</div>`)
}

func renderComparisonTable(sb *strings.Builder, blk BlockSpec, col *i18nCollector) {
	sb.WriteString(`<div class="tbl-wrap"><table class="cmp-tbl" data-sortable="1">`)
	if len(blk.Columns) > 0 {
		sb.WriteString(`<thead><tr>`)
		for i, c := range blk.Columns {
			sb.WriteString(fmt.Sprintf(`<th data-sort-col="%d" tabindex="0" role="button">%s <span class="sort-ind">↕</span></th>`, i, col.span(c)))
		}
		sb.WriteString(`</tr></thead>`)
	}
	sb.WriteString(`<tbody>`)
	for _, r := range blk.Rows {
		sb.WriteString(fmt.Sprintf(`<tr class="row-%s" data-group="%s" data-perspectives="%s" data-searchable="1">`,
			safeTone(r.Badge, "default"), html.EscapeString(strings.ToLower(r.Group)), html.EscapeString(attrList(r.Perspectives))))
		for _, cell := range r.Cells {
			sb.WriteString(`<td>` + col.span(cell) + `</td>`)
		}
		sb.WriteString(`</tr>`)
	}
	sb.WriteString(`</tbody></table></div>`)
}

func renderChecklistLedger(sb *strings.Builder, blk BlockSpec, goalSlug string, col *i18nCollector) {
	var openRows, confirmedRows, skippedRows []ChecklistRow
	for _, it := range blk.Items {
		switch strings.ToLower(strings.TrimSpace(it.Status)) {
		case "confirmed":
			confirmedRows = append(confirmedRows, it)
		case "skipped":
			skippedRows = append(skippedRows, it)
		default:
			openRows = append(openRows, it)
		}
	}
	sb.WriteString(`<div class="chk-sync-bar hidden" data-chk-sync="1"><span>✓ Local checklist updated</span><button type="button" class="copy-btn primary-btn" data-chk-copy-cmd="1">📋 Copy Sync Cmd</button><button type="button" class="mini-btn" data-chk-reset="1">Reset</button></div>`)
	renderGroup := func(title, badgeCls string, interactive bool, list []ChecklistRow) {
		if len(list) == 0 {
			return
		}
		sb.WriteString(fmt.Sprintf(`<div class="chk-group"><h3>%s (%d)</h3><ul class="chk-list">`, html.EscapeString(title), len(list)))
		for _, it := range list {
			isDone := strings.EqualFold(strings.TrimSpace(it.Status), "confirmed")
			doneCls := ""
			checkedAttr := ""
			if isDone {
				doneCls = " is-done"
				checkedAttr = " checked"
			}
			sb.WriteString(fmt.Sprintf(`<li class="chk-row%s" data-group="%s" data-chk-id="%s" data-orig-status="%s" data-searchable="1">`,
				doneCls, html.EscapeString(strings.ToLower(it.Group)), html.EscapeString(it.ID), html.EscapeString(strings.ToLower(strings.TrimSpace(it.Status)))))
			if interactive && strings.TrimSpace(it.ID) != "" {
				sb.WriteString(fmt.Sprintf(`<input type="checkbox" class="chk-box" data-chk-toggle="%s" aria-label="Toggle item"%s>`,
					html.EscapeString(it.ID), checkedAttr))
			}
			sb.WriteString(fmt.Sprintf(`<span class="badge %s" data-chk-badge="1">%s</span><div class="chk-main"><div class="chk-title">%s`,
				badgeCls, html.EscapeString(it.Status), col.span(it.Title)))
			if it.ExternalRef != "" {
				sb.WriteString(` <code>` + html.EscapeString(it.ExternalRef) + `</code>`)
			}
			if href := safeHref(it.ActionURL); href != "" {
				sb.WriteString(fmt.Sprintf(` <a href="%s" target="_blank" rel="noopener noreferrer">Open ↗</a>`, href))
			}
			sb.WriteString(`</div>`)
			if !it.StatusNote.IsEmpty() {
				sb.WriteString(`<div class="chk-meta">` + col.span(it.StatusNote) + `</div>`)
			}
			if !it.SkipReason.IsEmpty() {
				sb.WriteString(`<div class="chk-meta">Skipped: ` + col.span(it.SkipReason) + `</div>`)
			}
			if it.CutoffAt != "" {
				sb.WriteString(`<div class="chk-meta">Cutoff: ` + html.EscapeString(it.CutoffAt) + `</div>`)
			}
			if strings.TrimSpace(it.CopyText) != "" {
				lbl := it.CopyLabel
				if lbl.IsEmpty() {
					lbl = Text("Copy Action")
				}
				sb.WriteString(fmt.Sprintf(`<div class="act-row"><button type="button" class="copy-btn" data-copy="%s">📋 %s</button></div>`,
					html.EscapeString(it.CopyText), col.span(lbl)))
			}
			sb.WriteString(`</div></li>`)
		}
		sb.WriteString(`</ul></div>`)
	}
	renderGroup("Open", "b-open", true, openRows)
	renderGroup("Confirmed", "b-ok", true, confirmedRows)
	if len(skippedRows) > 0 {
		sb.WriteString(`<details class="skipped-sec"><summary>Skipped (` + fmt.Sprintf("%d", len(skippedRows)) + ` — won't re-suggest)</summary>`)
		renderGroup("Skipped", "b-skip", false, skippedRows)
		sb.WriteString(`</details>`)
	}
	_ = goalSlug
}

func renderCalloutNotes(sb *strings.Builder, blk BlockSpec, col *i18nCollector) {
	sb.WriteString(`<div class="notes-grid">`)
	for _, n := range blk.Notes {
		tone := safeTone(n.Tone, "info")
		sb.WriteString(fmt.Sprintf(`<article class="note tone-%s" data-group="%s" data-perspectives="%s" data-searchable="1"><h3>%s</h3><p>%s</p>`,
			tone, html.EscapeString(strings.ToLower(n.Group)), html.EscapeString(attrList(n.Perspectives)), col.span(n.Title), col.span(n.Body)))
		hasActions := strings.TrimSpace(n.CopyText) != "" || strings.TrimSpace(n.CopyAltText) != "" || safeHref(n.LinkURL) != ""
		if hasActions {
			sb.WriteString(`<div class="act-row">`)
			if href := safeHref(n.LinkURL); href != "" {
				lbl := n.LinkLabel
				if lbl.IsEmpty() {
					lbl = Text("Open ↗")
				}
				sb.WriteString(fmt.Sprintf(`<a class="copy-btn link-btn" href="%s" target="_blank" rel="noopener noreferrer">%s</a>`, href, col.span(lbl)))
			}
			if strings.TrimSpace(n.CopyText) != "" {
				lbl := n.CopyLabel
				if lbl.IsEmpty() {
					lbl = Text("Copy")
				}
				sb.WriteString(fmt.Sprintf(`<button type="button" class="copy-btn primary-btn" data-copy="%s">📋 %s</button>`,
					html.EscapeString(n.CopyText), col.span(lbl)))
			}
			if strings.TrimSpace(n.CopyAltText) != "" {
				lbl := n.CopyAltLabel
				if lbl.IsEmpty() {
					lbl = Text("Copy Alt")
				}
				sb.WriteString(fmt.Sprintf(`<button type="button" class="copy-btn" data-copy="%s">📋 %s</button>`,
					html.EscapeString(n.CopyAltText), col.span(lbl)))
			}
			sb.WriteString(`</div>`)
		}
		sb.WriteString(`</article>`)
	}
	sb.WriteString(`</div>`)
}

func clampBarPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func renderMetricBars(sb *strings.Builder, blk BlockSpec, col *i18nCollector) {
	sb.WriteString(`<div class="mbar-list">`)
	for _, r := range blk.Bars {
		tone := safeTone(r.Tone, "accent")
		pct := clampBarPct(r.Pct)
		sb.WriteString(fmt.Sprintf(`<div class="mbar-row" data-group="%s" data-perspectives="%s" data-searchable="1"><div class="mbar-top"><span class="mbar-lbl">%s</span><span class="mbar-val">%s</span></div><div class="mbar-track"><span class="mbar-fill tone-%s" style="width:%.1f%%"></span>`,
			html.EscapeString(strings.ToLower(r.Group)), html.EscapeString(attrList(r.Perspectives)), col.span(r.Label), col.span(r.Value), tone, pct))
		if r.WarnMarkerPct > 0 && r.WarnMarkerPct < 100 {
			sb.WriteString(fmt.Sprintf(`<i class="mbar-tick tick-warn" style="left:%.1f%%" title="Warn"></i>`, clampBarPct(r.WarnMarkerPct)))
		}
		if r.CritMarkerPct > 0 && r.CritMarkerPct < 100 {
			sb.WriteString(fmt.Sprintf(`<i class="mbar-tick tick-crit" style="left:%.1f%%" title="Critical"></i>`, clampBarPct(r.CritMarkerPct)))
		}
		sb.WriteString(`</div>`)
		if !r.Sub.IsEmpty() {
			sb.WriteString(`<div class="mbar-sub">` + col.span(r.Sub) + `</div>`)
		}
		sb.WriteString(`</div>`)
	}
	sb.WriteString(`</div>`)
}

const livingSpaceCSS = `
:root{--bg:#f8fafc;--card-bg:#ffffff;--elev:#f1f5f9;--fg:#0f172a;--muted:#475569;--bdr:#e2e8f0;--acc:#2563eb;--acc2:#7c3aed;--acc-soft:rgba(37,99,235,.10);--ok:#15803d;--warn:#d97706;--danger:#dc2626;--gold:#b45309;--hero-g:linear-gradient(135deg,rgba(37,99,235,.09),rgba(124,58,237,.05));--shadow:0 4px 20px rgba(15,23,42,.05);--font-body:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;--font-disp:var(--font-body)}
html[data-theme="editorial"]{--bg:#faf7f2;--card-bg:#fffdf9;--elev:#f4eee3;--fg:#1c1917;--muted:#57534e;--bdr:#e7dfd3;--acc:#b45309;--acc2:#9a3412;--acc-soft:rgba(180,83,9,.11);--hero-g:linear-gradient(135deg,rgba(180,83,9,.10),rgba(120,53,15,.04));--font-disp:"Iowan Old Style","Palatino Linotype","Book Antiqua",Georgia,serif}
html[data-theme="heritage"]{--bg:#f8f4ea;--card-bg:#fffcf5;--elev:#efe7d6;--fg:#1f1916;--muted:#5c5147;--bdr:#dfd3bc;--acc:#881337;--acc2:#b45309;--acc-soft:rgba(136,19,55,.10);--gold:#a16207;--hero-g:linear-gradient(135deg,rgba(136,19,55,.11),rgba(180,83,9,.09));--font-disp:"Iowan Old Style","Palatino Linotype",Georgia,serif}
html[data-theme="ocean"]{--bg:#f2f8fb;--card-bg:#ffffff;--elev:#e6f2f8;--fg:#0c1b2a;--muted:#456075;--bdr:#d2e4ee;--acc:#0284c7;--acc2:#0f766e;--acc-soft:rgba(2,132,199,.10);--hero-g:linear-gradient(135deg,rgba(2,132,199,.10),rgba(15,118,110,.07))}
html[data-theme="forest"]{--bg:#f4f8f4;--card-bg:#ffffff;--elev:#e7f0e8;--fg:#102216;--muted:#44604d;--bdr:#d3e2d6;--acc:#15803d;--acc2:#0f766e;--acc-soft:rgba(21,128,61,.10);--hero-g:linear-gradient(135deg,rgba(21,128,61,.10),rgba(15,118,110,.06))}
@media(prefers-color-scheme:dark){html:not([data-mode="light"]){--bg:#0b1120;--card-bg:#111827;--elev:#0f172a;--fg:#f1f5f9;--muted:#94a3b8;--bdr:#1e293b;--acc:#60a5fa;--acc2:#a78bfa;--acc-soft:rgba(96,165,250,.14);--ok:#4ade80;--warn:#fbbf24;--danger:#f87171;--gold:#fbbf24;--shadow:0 8px 28px rgba(0,0,0,.35)}
html[data-theme="editorial"]:not([data-mode="light"]){--bg:#141210;--card-bg:#1c1916;--elev:#25201c;--bdr:#2e2822;--acc:#f59e0b;--acc2:#fb923c;--acc-soft:rgba(245,158,11,.15)}
html[data-theme="heritage"]:not([data-mode="light"]){--bg:#141011;--card-bg:#1d1618;--elev:#271d20;--bdr:#35272b;--acc:#fb7185;--acc2:#fbbf24;--acc-soft:rgba(251,113,133,.15)}
html[data-theme="ocean"]:not([data-mode="light"]){--bg:#08131d;--card-bg:#0e1d2b;--elev:#132638;--bdr:#1d354b;--acc:#38bdf8;--acc2:#2dd4bf;--acc-soft:rgba(56,189,248,.15)}
html[data-theme="forest"]:not([data-mode="light"]){--bg:#0a150e;--card-bg:#112017;--elev:#172b1f;--bdr:#213a2a;--acc:#4ade80;--acc2:#2dd4bf;--acc-soft:rgba(74,222,128,.14)}}
html[data-mode="dark"]{--bg:#0b1120;--card-bg:#111827;--elev:#0f172a;--fg:#f1f5f9;--muted:#94a3b8;--bdr:#1e293b;--acc:#60a5fa;--acc2:#a78bfa;--acc-soft:rgba(96,165,250,.14);--ok:#4ade80;--warn:#fbbf24;--danger:#f87171;--gold:#fbbf24;--shadow:0 8px 28px rgba(0,0,0,.35)}
html[data-theme="editorial"][data-mode="dark"]{--bg:#141210;--card-bg:#1c1916;--elev:#25201c;--bdr:#2e2822;--acc:#f59e0b;--acc2:#fb923c;--acc-soft:rgba(245,158,11,.15)}
html[data-theme="heritage"][data-mode="dark"]{--bg:#141011;--card-bg:#1d1618;--elev:#271d20;--bdr:#35272b;--acc:#fb7185;--acc2:#fbbf24;--acc-soft:rgba(251,113,133,.15)}
html[data-theme="ocean"][data-mode="dark"]{--bg:#08131d;--card-bg:#0e1d2b;--elev:#132638;--bdr:#1d354b;--acc:#38bdf8;--acc2:#2dd4bf;--acc-soft:rgba(56,189,248,.15)}
html[data-theme="forest"][data-mode="dark"]{--bg:#0a150e;--card-bg:#112017;--elev:#172b1f;--bdr:#213a2a;--acc:#4ade80;--acc2:#2dd4bf;--acc-soft:rgba(74,222,128,.14)}
*{box-sizing:border-box}html{scroll-behavior:smooth}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.6 var(--font-body);-webkit-font-smoothing:antialiased}
.wrap{max-width:980px;margin:0 auto;padding:22px 16px 48px}
.hdr{display:flex;flex-wrap:wrap;justify-content:space-between;align-items:flex-start;gap:14px;padding:22px 24px;border-radius:16px;border:1px solid var(--bdr);background:var(--hero-g),var(--card-bg);box-shadow:var(--shadow);margin-bottom:14px}
.hdr-main{min-width:0;flex:1 1 320px}.hdr-tools{display:flex;align-items:center;gap:8px;flex-wrap:wrap}
.hero-badge{display:inline-block;font-size:11px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;color:var(--acc);background:var(--acc-soft);padding:3px 10px;border-radius:999px;margin-bottom:8px}
h1{margin:0;font-family:var(--font-disp);font-size:28px;line-height:1.18;letter-spacing:-.015em}
h2{margin:0;font-family:var(--font-disp);font-size:18px;line-height:1.25}
h3{margin:0 0 6px;font-size:14px}
.sub{margin:6px 0 0;color:var(--muted);font-size:14px;max-width:68ch}
.controls{position:sticky;top:8px;z-index:20;display:flex;flex-direction:column;gap:8px;padding:10px 12px;margin-bottom:16px;border-radius:14px;border:1px solid var(--bdr);background:color-mix(in srgb,var(--card-bg) 88%,transparent);backdrop-filter:blur(10px);box-shadow:var(--shadow)}
.ctrl-row{display:flex;flex-wrap:wrap;justify-content:space-between;align-items:center;gap:8px}
.ctrl-utility{padding-top:6px;border-top:1px dashed var(--bdr)}
.ctrl-group,.lang-switch,.toc-strip{display:flex;flex-wrap:wrap;gap:6px;align-items:center}
.pill{border:1px solid var(--bdr);background:var(--card-bg);color:var(--fg);padding:5px 12px;border-radius:999px;font-size:12px;cursor:pointer;font-weight:600;transition:all .15s ease}
.pill:hover{border-color:var(--acc);color:var(--acc)}
.pill.active{background:var(--acc);color:#fff;border-color:var(--acc)}
.mode-btn{padding:5px 10px;font-size:13px}
.toc-pill{font-size:11px;font-weight:600;color:var(--muted);text-decoration:none;padding:3px 9px;border-radius:999px;background:var(--elev);border:1px solid transparent;transition:all .15s ease}
.toc-pill:hover{color:var(--acc);border-color:var(--acc)}
.space-search{flex:0 1 220px;min-width:150px;margin-left:auto;background:var(--bg);color:var(--fg);border:1px solid var(--bdr);border-radius:999px;padding:5px 12px;font-size:12px;outline:none}
.space-search:focus{border-color:var(--acc)}
.block{background:var(--card-bg);border:1px solid var(--bdr);border-radius:14px;padding:18px 20px;margin-bottom:16px;box-shadow:var(--shadow);scroll-margin-top:96px}
.blk-hdr{display:flex;justify-content:space-between;align-items:center;gap:10px;margin-bottom:12px;padding-bottom:8px;border-bottom:1px solid var(--bdr)}
.mini-btn{border:1px solid var(--bdr);background:var(--elev);color:var(--muted);padding:3px 9px;border-radius:999px;font-size:11px;font-weight:600;cursor:pointer}
.mini-btn:hover{color:var(--fg);border-color:var(--acc)}
.kpi-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(175px,1fr));gap:10px}
.kpi-card{border:1px solid var(--bdr);border-top:3px solid var(--bdr);border-radius:12px;padding:13px 14px;background:var(--bg)}
.kpi-card.tone-accent{border-top-color:var(--acc)}.kpi-card.tone-warn{border-top-color:var(--warn)}.kpi-card.tone-ok{border-top-color:var(--ok)}.kpi-card.tone-danger{border-top-color:var(--danger)}
.kpi-lbl{font-size:11px;text-transform:uppercase;letter-spacing:.05em;color:var(--muted);margin-bottom:4px;font-weight:600}
.kpi-val{font-family:var(--font-disp);font-size:20px;font-weight:700;line-height:1.2}.kpi-sub{font-size:12px;color:var(--muted);margin-top:4px}
.bar{height:6px;background:var(--bdr);border-radius:999px;overflow:hidden;margin-top:7px}.bar span{display:block;height:100%;background:var(--acc);transition:width .25s ease}
.mbar-list{display:flex;flex-direction:column;gap:10px}
.mbar-row{border:1px solid var(--bdr);border-radius:10px;padding:11px 13px;background:var(--bg)}
.mbar-top{display:flex;justify-content:space-between;align-items:baseline;gap:8px;font-size:13px}.mbar-lbl{font-weight:600}.mbar-val{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px}
.mbar-track{position:relative;height:8px;background:var(--bdr);border-radius:999px;margin-top:6px;overflow:visible}
.mbar-fill{display:block;height:100%;border-radius:999px;background:var(--acc)}.mbar-fill.tone-ok{background:var(--ok)}.mbar-fill.tone-warn{background:var(--warn)}.mbar-fill.tone-danger{background:var(--danger)}.mbar-fill.tone-neutral{background:var(--muted)}
.mbar-tick{position:absolute;top:-3px;bottom:-3px;width:2px;border-radius:1px}.mbar-tick.tick-warn{background:var(--warn)}.mbar-tick.tick-crit{background:var(--danger)}
.mbar-sub{font-size:12px;color:var(--muted);margin-top:5px}
.timeline{position:relative;display:flex;flex-direction:column;gap:10px;padding-left:14px}
.timeline::before{content:"";position:absolute;left:5px;top:14px;bottom:14px;width:2px;background:var(--bdr);border-radius:2px}
.tl-item{position:relative;border:1px solid var(--bdr);border-radius:12px;padding:12px 14px;background:var(--bg);transition:border-color .15s ease}
.tl-item[open]{border-color:color-mix(in srgb,var(--acc) 45%,var(--bdr))}
.tl-item summary{cursor:pointer;list-style:none;display:flex;flex-wrap:wrap;gap:8px;align-items:center}
.tl-item summary::-webkit-details-marker{display:none}
.tl-node{width:22px;height:22px;border-radius:50%;background:var(--acc);color:#fff;font-size:11px;font-weight:700;display:inline-flex;align-items:center;justify-content:center;flex-shrink:0}
.tl-date{font-size:11px;font-weight:700;color:var(--acc);background:var(--acc-soft);padding:2px 8px;border-radius:6px}
.tl-title{font-size:14px}.tl-sum{font-size:12px;color:var(--muted);margin-left:auto}
.tl-body{margin-top:10px;padding:10px 4px 2px 30px;border-top:1px dashed var(--bdr)}.tl-body ul{margin:0;padding-left:18px}.tl-body li{margin:4px 0}
.tl-callout{margin-top:10px;padding:9px 12px;border-left:3px solid var(--acc);background:var(--card-bg);border-radius:0 8px 8px 0;font-size:13px}
.tbl-wrap{overflow-x:auto}.cmp-tbl{width:100%;border-collapse:collapse;font-size:13px}
.cmp-tbl th,.cmp-tbl td{text-align:left;padding:9px 11px;border-bottom:1px solid var(--bdr)}
.cmp-tbl th{font-size:11px;text-transform:uppercase;letter-spacing:.04em;color:var(--muted);cursor:pointer;user-select:none;white-space:nowrap}
.cmp-tbl th:hover{color:var(--acc)}.sort-ind{opacity:.45;font-size:10px}
.cmp-tbl tbody tr:hover td{background:var(--acc-soft)}
.cmp-tbl tr.row-warn td{background:rgba(217,119,6,.08)}.cmp-tbl tr.row-ok td,.cmp-tbl tr.row-recommended td{background:rgba(22,163,74,.08)}
.chk-sync-bar{display:flex;flex-wrap:wrap;align-items:center;gap:8px;padding:8px 12px;margin-bottom:10px;border-radius:10px;border:1px solid var(--acc);background:var(--acc-soft);font-size:12px;font-weight:600}
.chk-group h3{font-size:12px;text-transform:uppercase;letter-spacing:.05em;color:var(--muted);margin:10px 0 6px}
.chk-list{list-style:none;margin:0 0 10px;padding:0;display:flex;flex-direction:column;gap:7px}
.chk-row{display:flex;gap:10px;align-items:flex-start;padding:10px 12px;border:1px solid var(--bdr);border-radius:10px;background:var(--bg);transition:all .15s ease}
.chk-row.is-done .chk-title{text-decoration:line-through;opacity:.75}
.chk-box{width:16px;height:16px;margin-top:2px;accent-color:var(--ok);cursor:pointer;flex-shrink:0}
.badge{font-size:10px;padding:2px 8px;border-radius:999px;font-weight:700;text-transform:uppercase;letter-spacing:.04em;flex-shrink:0}
.b-ok{background:rgba(22,163,74,.16);color:var(--ok)}.b-open{background:rgba(217,119,6,.16);color:var(--warn)}.b-skip{background:var(--elev);color:var(--muted)}.b-info{background:var(--acc-soft);color:var(--acc)}
.chk-title{font-weight:600}.chk-meta{font-size:12px;color:var(--muted);margin-top:2px}
.skipped-sec{margin-top:10px;font-size:13px;color:var(--muted)}.skipped-sec summary{cursor:pointer}
.notes-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:12px}
.note{border:1px solid var(--bdr);border-left:4px solid var(--acc);border-radius:10px;padding:12px 14px;background:var(--bg)}
.note.tone-warn{border-left-color:var(--warn)}.note.tone-ok{border-left-color:var(--ok)}.note.tone-danger{border-left-color:var(--danger)}.note p{margin:4px 0 0;font-size:13px;white-space:pre-line}
.imap-shell{display:flex;flex-direction:column;gap:12px}
.imap-canvas{background:var(--bg);border:1px solid var(--bdr);border-radius:12px;padding:10px;overflow:hidden}
.imap-svg{width:100%;height:auto;display:block;max-height:420px}
.imap-zone rect{fill:var(--elev);stroke:var(--bdr);stroke-width:1.5}
.imap-zone.tone-accent rect{fill:var(--acc-soft);stroke:var(--acc)}.imap-zone.tone-gold rect{fill:rgba(180,83,9,.12);stroke:var(--gold)}.imap-zone.tone-ok rect{fill:rgba(22,163,74,.10);stroke:var(--ok)}.imap-zone.tone-warn rect{fill:rgba(217,119,6,.10);stroke:var(--warn)}
.imap-zone text{fill:var(--muted);font-size:11px;font-weight:700;letter-spacing:.05em;text-transform:uppercase}
.imap-path{stroke:var(--acc);stroke-width:2.5;stroke-linecap:round;stroke-linejoin:round;opacity:.75}
.imap-path.tone-gold{stroke:var(--gold)}.imap-path.tone-ok{stroke:var(--ok)}.imap-path.tone-warn{stroke:var(--warn)}
.imap-pin{cursor:pointer;outline:none}
.imap-ring{fill:var(--acc);opacity:0;transition:opacity .18s ease}
.imap-dot{fill:var(--card-bg);stroke:var(--acc);stroke-width:2.5;transition:all .18s ease}
.imap-pin.tone-gold .imap-dot{stroke:var(--gold)}.imap-pin.tone-ok .imap-dot{stroke:var(--ok)}.imap-pin.tone-warn .imap-dot{stroke:var(--warn)}
.imap-num{fill:var(--fg);font-size:11px;font-weight:800;pointer-events:none}
.imap-pin:hover .imap-ring,.imap-pin.active .imap-ring{opacity:.22}
.imap-pin.active .imap-dot{fill:var(--acc);stroke:var(--acc)}.imap-pin.active .imap-num{fill:#fff}
.imap-legend{display:flex;flex-wrap:wrap;gap:6px}.imap-leg-pill{font-size:11px;color:var(--muted);background:var(--elev);padding:3px 9px;border-radius:999px}
.imap-bar{display:flex;flex-wrap:wrap;justify-content:space-between;align-items:center;gap:8px}
.imap-pills,.imap-nav{display:flex;flex-wrap:wrap;gap:6px}
.imap-pill-num{display:inline-flex;align-items:center;justify-content:center;width:18px;height:18px;border-radius:50%;background:var(--acc-soft);color:var(--acc);font-size:10px;font-weight:800;margin-right:3px}
.imap-pill.active .imap-pill-num{background:rgba(255,255,255,.22);color:#fff}
.imap-detail{display:none;border:1px solid color-mix(in srgb,var(--acc) 35%,var(--bdr));border-radius:12px;padding:15px 16px;background:var(--bg)}
.imap-detail.active{display:block}
.imap-detail-hdr{display:flex;flex-wrap:wrap;align-items:flex-start;gap:10px;margin-bottom:8px}
.imap-badge{background:var(--acc);color:#fff;font-size:11px;font-weight:700;padding:3px 9px;border-radius:999px}
.imap-detail-titles{flex:1 1 200px}.imap-detail-titles h3{margin:0;font-family:var(--font-disp);font-size:17px}.imap-detail-sub{font-size:12px;color:var(--muted)}
.imap-meta{font-size:11px;font-weight:600;color:var(--acc);background:var(--acc-soft);padding:3px 9px;border-radius:999px}
.imap-body{margin:0;font-size:14px;line-height:1.65;white-space:pre-line}
.story-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(260px,1fr));gap:14px}
.story-card{border:1px solid var(--bdr);border-radius:14px;background:var(--bg);overflow:hidden;display:flex;flex-direction:column;transition:transform .15s ease,border-color .15s ease}
.story-card:hover{border-color:color-mix(in srgb,var(--acc) 45%,var(--bdr))}
.story-card.tone-accent{border-top:3px solid var(--acc)}.story-card.tone-ok{border-top:3px solid var(--ok)}.story-card.tone-warn{border-top:3px solid var(--warn)}
.story-media img{display:block;width:100%;height:160px;object-fit:cover;border-bottom:1px solid var(--bdr)}
.story-pad{padding:15px 16px;display:flex;flex-direction:column;gap:6px;flex:1}
.story-top{display:flex;justify-content:space-between;align-items:center;gap:8px}
.story-eye{font-size:11px;font-weight:700;text-transform:uppercase;letter-spacing:.06em;color:var(--acc);display:inline-flex;align-items:center;gap:5px}
.story-ico{font-size:15px}.story-title{margin:0;font-family:var(--font-disp);font-size:17px;line-height:1.25}
.story-sub{font-size:12px;color:var(--muted)}.story-body{margin:2px 0 0;font-size:13px;line-height:1.6;white-space:pre-line}
.story-list{margin:4px 0 0;padding-left:18px;font-size:13px}.story-list li{margin:3px 0}
.story-deep{margin-top:6px;padding-top:6px;border-top:1px dashed var(--bdr);font-size:13px}
.story-deep summary{cursor:pointer;color:var(--acc);font-weight:600;font-size:12px}.story-deep p{margin:6px 0 0;color:var(--muted);white-space:pre-line}
.story-meta{font-size:11px;color:var(--muted);margin-top:auto;padding-top:6px}
.act-row{display:flex;flex-wrap:wrap;gap:6px;margin-top:8px}
.copy-btn{border:1px solid var(--bdr);background:var(--card-bg);color:var(--fg);padding:5px 11px;border-radius:8px;font-size:12px;font-weight:600;cursor:pointer;text-decoration:none;display:inline-flex;align-items:center;gap:4px;min-height:30px}
.copy-btn:hover{border-color:var(--acc);color:var(--acc)}
.copy-btn.primary-btn{background:var(--acc);color:#fff;border-color:var(--acc)}.copy-btn.primary-btn:hover{opacity:.92;color:#fff}
.copy-btn.copied{background:var(--ok);color:#fff;border-color:var(--ok)}
.ftr{text-align:center;color:var(--muted);font-size:12px;margin-top:22px}
.hidden{display:none!important}
@media(max-width:600px){.wrap{padding:12px 10px 36px}.hdr{padding:16px}.block{padding:14px}h1{font-size:22px}.space-search{flex:1 1 100%;margin-left:0}}
`

const livingSpaceJS = `
(function(){
  var dataEl=document.getElementById('hq-i18n');
  var i18n=dataEl?JSON.parse(dataEl.textContent):{locales:['en'],default:'en',dicts:{}};
  var slug=i18n.slug||'default';

  // Light / Dark color mode toggle (persisted per space)
  var modeKey='hq_space_mode_'+slug;
  var modeBtn=document.getElementById('hq-mode-btn');
  function applyMode(m){
    if(m==='light'||m==='dark'){document.documentElement.setAttribute('data-mode',m);}
    else{document.documentElement.removeAttribute('data-mode');}
    if(modeBtn){modeBtn.textContent=m==='dark'?'☀️':(m==='light'?'🌙':'◐');}
  }
  try{var savedM=localStorage.getItem(modeKey);if(savedM)applyMode(savedM);}catch(e){}
  if(modeBtn){
    modeBtn.addEventListener('click',function(){
      var cur=document.documentElement.getAttribute('data-mode');
      var prefersDark=window.matchMedia&&window.matchMedia('(prefers-color-scheme: dark)').matches;
      var next=(cur==='dark'||(!cur&&prefersDark))?'light':'dark';
      applyMode(next);
      try{localStorage.setItem(modeKey,next);}catch(e){}
    });
  }

  // Multi-locale i18n switcher
  function setLang(loc){
    if(!i18n.dicts||!i18n.dicts[loc])return;
    var dict=i18n.dicts[loc];
    document.documentElement.lang=loc;
    document.querySelectorAll('[data-i18n]').forEach(function(el){
      var k=el.getAttribute('data-i18n');
      if(dict[k]!==undefined)el.textContent=dict[k];
    });
    document.querySelectorAll('.lang-btn').forEach(function(b){
      b.classList.toggle('active',b.getAttribute('data-lang')===loc);
    });
    try{var u=new URL(window.location.href);u.searchParams.set('lang',loc);window.history.replaceState(null,'',u.toString());}catch(e){}
  }
  document.querySelectorAll('.lang-btn').forEach(function(b){
    b.addEventListener('click',function(){setLang(b.getAttribute('data-lang'));});
  });
  try{
    var p=new URLSearchParams(window.location.search).get('lang');
    if(p&&i18n.dicts&&i18n.dicts[p])setLang(p);
  }catch(e){}

  // Perspective, Group & Live Search filters
  var perspStoreKey='hq_space_persp_'+slug;
  var activePersp=(i18n.defaultPerspective||'all'),activeGrp=(i18n.defaultGroup||'all'),searchQ='';
  try{
    var savedP=sessionStorage.getItem(perspStoreKey);
    if(savedP&&document.querySelector('.persp-btn[data-persp="'+savedP+'"]'))activePersp=savedP;
  }catch(e){}
  function applyFilters(){
    document.querySelectorAll('[data-perspectives],[data-group],[data-searchable]').forEach(function(el){
      var ps=(el.getAttribute('data-perspectives')||'').split(',').filter(Boolean);
      var gr=(el.getAttribute('data-group')||'').trim();
      var okP=(activePersp==='all'||ps.length===0||ps.indexOf(activePersp)!==-1);
      var okG=(activeGrp==='all'||!gr||gr===activeGrp);
      var okQ=true;
      if(searchQ&&el.getAttribute('data-searchable')==='1'){
        okQ=(el.textContent||'').toLowerCase().indexOf(searchQ)!==-1;
      }
      el.classList.toggle('hidden',!(okP&&okG&&okQ));
    });
  }
  document.querySelectorAll('.persp-btn').forEach(function(b){
    b.classList.toggle('active',b.getAttribute('data-persp')===activePersp);
    b.addEventListener('click',function(){
      activePersp=b.getAttribute('data-persp');
      try{sessionStorage.setItem(perspStoreKey,activePersp);}catch(e){}
      document.querySelectorAll('.persp-btn').forEach(function(x){x.classList.toggle('active',x===b);});
      applyFilters();
    });
  });
  document.querySelectorAll('.grp-btn').forEach(function(b){
    b.classList.toggle('active',b.getAttribute('data-grp')===activeGrp);
    b.addEventListener('click',function(){
      activeGrp=b.getAttribute('data-grp');
      document.querySelectorAll('.grp-btn').forEach(function(x){x.classList.toggle('active',x===b);});
      applyFilters();
    });
  });
  var searchEl=document.getElementById('hq-space-search');
  if(searchEl){
    searchEl.addEventListener('input',function(){
      searchQ=(searchEl.value||'').trim().toLowerCase();
      applyFilters();
    });
  }
  applyFilters();

  // Timeline Expand All / Collapse All
  document.querySelectorAll('.tl-toggle-all').forEach(function(btn){
    btn.addEventListener('click',function(){
      var sec=btn.closest('.block');
      if(!sec)return;
      var expand=btn.getAttribute('data-expanded')!=='1';
      sec.querySelectorAll('details.tl-item').forEach(function(d){d.open=expand;});
      btn.setAttribute('data-expanded',expand?'1':'0');
      btn.textContent=expand?'Collapse all':'Expand all';
    });
  });

  // Sortable Comparison Tables
  document.querySelectorAll('table.cmp-tbl[data-sortable]').forEach(function(tbl){
    tbl.querySelectorAll('th[data-sort-col]').forEach(function(th){
      function doSort(){
        var col=parseInt(th.getAttribute('data-sort-col'),10);
        var asc=th.getAttribute('data-sort-dir')!=='asc';
        tbl.querySelectorAll('th[data-sort-col]').forEach(function(h){
          h.removeAttribute('data-sort-dir');
          var ind=h.querySelector('.sort-ind');if(ind)ind.textContent='↕';
        });
        th.setAttribute('data-sort-dir',asc?'asc':'desc');
        var ind=th.querySelector('.sort-ind');if(ind)ind.textContent=asc?'↑':'↓';
        var tb=tbl.querySelector('tbody');if(!tb)return;
        var rows=Array.prototype.slice.call(tb.querySelectorAll('tr'));
        rows.sort(function(a,b){
          var ta=((a.children[col]&&a.children[col].textContent)||'').trim();
          var tbv=((b.children[col]&&b.children[col].textContent)||'').trim();
          var na=parseFloat(ta.replace(/[^0-9.\-]/g,'')),nb=parseFloat(tbv.replace(/[^0-9.\-]/g,''));
          if(!isNaN(na)&&!isNaN(nb)&&ta.match(/[0-9]/)&&tbv.match(/[0-9]/)){return asc?(na-nb):(nb-na);}
          return asc?ta.localeCompare(tbv):tbv.localeCompare(ta);
        });
        rows.forEach(function(r){tb.appendChild(r);});
      }
      th.addEventListener('click',doSort);
      th.addEventListener('keydown',function(e){if(e.key==='Enter'||e.key===' '){e.preventDefault();doSort();}});
    });
  });

  // Interactive SVG Maps (pin click, keyboard, pill click, Prev/Next stepper)
  document.querySelectorAll('.imap-shell').forEach(function(shell){
    var details=shell.querySelectorAll('.imap-detail');
    if(!details.length)return;
    var cur=0;
    function selectPin(idx){
      if(idx<0)idx=details.length-1;
      if(idx>=details.length)idx=0;
      cur=idx;
      shell.querySelectorAll('[data-map-pin]').forEach(function(el){
        el.classList.toggle('active',parseInt(el.getAttribute('data-map-pin'),10)===cur);
      });
      details.forEach(function(d){
        d.classList.toggle('active',parseInt(d.getAttribute('data-map-detail'),10)===cur);
      });
    }
    shell.querySelectorAll('[data-map-pin]').forEach(function(el){
      var idx=parseInt(el.getAttribute('data-map-pin'),10);
      el.addEventListener('click',function(){selectPin(idx);});
      el.addEventListener('keydown',function(e){if(e.key==='Enter'||e.key===' '){e.preventDefault();selectPin(idx);}});
    });
    var prev=shell.querySelector('.imap-prev'),next=shell.querySelector('.imap-next');
    if(prev)prev.addEventListener('click',function(){selectPin(cur-1);});
    if(next)next.addEventListener('click',function(){selectPin(cur+1);});
  });

  // Clipboard helper
  function copyToClipboard(txt,btn){
    var done=function(){
      var orig=btn.textContent;
      btn.classList.add('copied');
      btn.textContent='✓ Copied — paste in chat';
      setTimeout(function(){btn.classList.remove('copied');btn.textContent=orig;},1800);
    };
    if(navigator.clipboard&&navigator.clipboard.writeText){
      navigator.clipboard.writeText(txt).then(done).catch(function(){
        var ta=document.createElement('textarea');ta.value=txt;ta.style.position='fixed';ta.style.opacity='0';
        document.body.appendChild(ta);ta.select();try{document.execCommand('copy');done();}catch(e){}document.body.removeChild(ta);
      });
    }else{
      var ta=document.createElement('textarea');ta.value=txt;ta.style.position='fixed';ta.style.opacity='0';
      document.body.appendChild(ta);ta.select();try{document.execCommand('copy');done();}catch(e){}document.body.removeChild(ta);
    }
  }
  document.querySelectorAll('.copy-btn[data-copy]').forEach(function(b){
    b.addEventListener('click',function(){copyToClipboard(b.getAttribute('data-copy')||'',b);});
  });

  // Interactive Checklist Toggles (localStorage persisted + Live Readiness + Goal Sync Cmd)
  var chkKey='hq_space_chk_'+slug;
  var chkState={};
  try{chkState=JSON.parse(localStorage.getItem(chkKey)||'{}')||{};}catch(e){chkState={};}
  function updateChecklistUI(){
    var boxes=document.querySelectorAll('input.chk-box[data-chk-toggle]');
    if(!boxes.length)return;
    var confirmed=0,total=0,changed=[];
    boxes.forEach(function(cb){
      var id=cb.getAttribute('data-chk-toggle');
      var row=cb.closest('.chk-row');
      var orig=row?(row.getAttribute('data-orig-status')||'open'):'open';
      var isDone=(chkState[id]!==undefined)?!!chkState[id]:(orig==='confirmed');
      cb.checked=isDone;
      total++;
      if(isDone)confirmed++;
      if(row){
        row.classList.toggle('is-done',isDone);
        var b=row.querySelector('[data-chk-badge]');
        if(b){b.textContent=isDone?'confirmed':'open';b.className='badge '+(isDone?'b-ok':'b-open');}
      }
      if((isDone&&orig!=='confirmed')||(!isDone&&orig==='confirmed')){
        changed.push({id:id,done:isDone});
      }
    });
    document.querySelectorAll('[data-readiness-card]').forEach(function(card){
      var skipped=parseInt(card.getAttribute('data-base-skipped')||'0',10);
      var pct=total>0?Math.floor((confirmed*100)/total):0;
      var v=card.querySelector('[data-readiness-val]');if(v)v.textContent=confirmed+' / '+total+' ('+pct+'%)';
      var bar=card.querySelector('[data-readiness-bar]');if(bar)bar.style.width=pct+'%';
      var sub=card.querySelector('[data-readiness-sub]');if(sub)sub.textContent=(total-confirmed)+' open · '+skipped+' skipped';
    });
    document.querySelectorAll('[data-chk-sync]').forEach(function(bar){
      bar.classList.toggle('hidden',changed.length===0);
      var copyBtn=bar.querySelector('[data-chk-copy-cmd]');
      if(copyBtn&&changed.length>0){
        var cmds=changed.filter(function(x){return x.done;}).map(function(x){
          return i18n.goalSlug?('hq goals check-off '+i18n.goalSlug+' '+x.id):('Mark checklist item '+x.id+' confirmed in space '+slug);
        });
        if(!cmds.length){cmds=changed.map(function(x){return 'Re-open checklist item '+x.id+' in space '+slug;});}
        copyBtn.setAttribute('data-sync-cmd',cmds.join(' && '));
      }
    });
  }
  document.querySelectorAll('input.chk-box[data-chk-toggle]').forEach(function(cb){
    cb.addEventListener('change',function(){
      chkState[cb.getAttribute('data-chk-toggle')]=cb.checked;
      try{localStorage.setItem(chkKey,JSON.stringify(chkState));}catch(e){}
      updateChecklistUI();
    });
  });
  document.querySelectorAll('[data-chk-copy-cmd]').forEach(function(btn){
    btn.addEventListener('click',function(){copyToClipboard(btn.getAttribute('data-sync-cmd')||'',btn);});
  });
  document.querySelectorAll('[data-chk-reset]').forEach(function(btn){
    btn.addEventListener('click',function(){
      chkState={};
      try{localStorage.removeItem(chkKey);}catch(e){}
      updateChecklistUI();
    });
  });
  updateChecklistUI();

  // Live Countdowns
  function updateCountdowns(){
    var now=Date.now();
    document.querySelectorAll('.hq-countdown').forEach(function(el){
      var raw=el.getAttribute('data-target');
      if(!raw)return;
      var t=Date.parse(raw.length===10?raw+'T00:00:00Z':raw);
      if(isNaN(t))return;
      var diff=t-now;
      if(diff<=0){el.textContent='Live now';return;}
      var d=Math.floor(diff/86400000),h=Math.floor((diff%86400000)/3600000),m=Math.floor((diff%3600000)/60000);
      el.textContent=(d>0?d+'d ':'')+h+'h '+m+'m';
    });
  }
  updateCountdowns();
  setInterval(updateCountdowns,60000);
})();
`
