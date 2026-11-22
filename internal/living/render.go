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

	var body strings.Builder

	// Header + interactive toolbar (locale toggle hidden when single-locale; perspectives/groups hidden when empty)
	body.WriteString(`<header class="hdr"><div class="hdr-main">`)
	body.WriteString(`<h1>` + col.span(doc.Title) + `</h1>`)
	if !doc.Subtitle.IsEmpty() {
		body.WriteString(`<p class="sub">` + col.span(doc.Subtitle) + `</p>`)
	}
	body.WriteString(`</div>`)

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
	body.WriteString(`</header>`)

	if len(doc.Perspectives) > 0 || len(doc.Groups) > 0 {
		body.WriteString(`<nav class="controls">`)
		if len(doc.Perspectives) > 0 {
			body.WriteString(`<div class="ctrl-group" role="tablist"><button type="button" class="pill persp-btn active" data-persp="all">All Views</button>`)
			for _, p := range doc.Perspectives {
				body.WriteString(fmt.Sprintf(`<button type="button" class="pill persp-btn" data-persp="%s">%s</button>`,
					html.EscapeString(strings.ToLower(p.ID)), col.span(p.Label)))
			}
			body.WriteString(`</div>`)
		}
		if len(doc.Groups) > 0 {
			body.WriteString(`<div class="ctrl-group"><button type="button" class="pill grp-btn active" data-grp="all">All</button>`)
			for _, g := range doc.Groups {
				body.WriteString(fmt.Sprintf(`<button type="button" class="pill grp-btn" data-grp="%s">%s</button>`,
					html.EscapeString(strings.ToLower(g)), html.EscapeString(g)))
			}
			body.WriteString(`</div>`)
		}
		body.WriteString(`</nav>`)
	}

	// Render each composable block
	for _, blk := range doc.Blocks {
		perspAttr := attrList(blk.Perspectives)
		body.WriteString(fmt.Sprintf(`<section class="block block-%s" data-perspectives="%s">`,
			html.EscapeString(blk.Type), html.EscapeString(perspAttr)))
		if !blk.Title.IsEmpty() {
			body.WriteString(`<h2>` + col.span(blk.Title) + `</h2>`)
		}
		switch blk.Type {
		case BlockKPIStrip:
			renderKPIStrip(&body, blk, doc.Timezone, col)
		case BlockTimelineAccordion:
			renderTimelineAccordion(&body, blk, col)
		case BlockComparisonTable:
			renderComparisonTable(&body, blk, col)
		case BlockChecklistLedger:
			renderChecklistLedger(&body, blk, col)
		case BlockCalloutNotes:
			renderCalloutNotes(&body, blk, col)
		}
		body.WriteString(`</section>`)
	}

	body.WriteString(fmt.Sprintf(`<footer class="ftr">Living Space <code>%s</code> · Updated %s · 0-token local build</footer>`,
		html.EscapeString(doc.Slug), html.EscapeString(doc.UpdatedAt)))

	i18nPayload := map[string]interface{}{
		"locales":            locales,
		"default":            primary,
		"defaultPerspective": strings.ToLower(strings.TrimSpace(doc.DefaultPerspective)),
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
	docHTML.WriteString("<!DOCTYPE html>\n<html lang=\"" + html.EscapeString(primary) + "\">\n<head>\n<meta charset=\"utf-8\">\n")
	docHTML.WriteString("<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">\n")
	docHTML.WriteString("<title>" + html.EscapeString(pageTitle) + "</title>\n")
	docHTML.WriteString("<style>\n" + livingSpaceCSS + "\n</style>\n</head>\n<body class=\"" + surfaceClass + "\">\n<main class=\"wrap\">\n")
	docHTML.WriteString(body.String())
	docHTML.WriteString("\n</main>\n<script id=\"hq-i18n\" type=\"application/json\">" + i18nJSON + "</script>\n")
	docHTML.WriteString("<script>\n" + livingSpaceJS + "\n</script>\n</body>\n</html>\n")
	return []byte(docHTML.String()), nil
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
		sb.WriteString(fmt.Sprintf(`<div class="kpi-card"><div class="kpi-lbl">Readiness</div><div class="kpi-val">%d / %d (%d%%)</div><div class="bar"><span style="width:%d%%"></span></div><div class="kpi-sub">%d open · %d skipped</div></div>`,
			blk.ConfirmedCount, active, pct, pct, blk.OpenCount, blk.SkippedCount))
	}
	for _, m := range blk.Metrics {
		tone := m.Tone
		if tone == "" {
			tone = "neutral"
		}
		sb.WriteString(fmt.Sprintf(`<div class="kpi-card tone-%s"><div class="kpi-lbl">%s</div><div class="kpi-val">%s</div></div>`,
			html.EscapeString(tone), col.span(m.Label), col.span(m.Value)))
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
		sb.WriteString(fmt.Sprintf(`<details class="tl-item" data-group="%s" data-perspectives="%s"%s><summary><span class="tl-date">%s</span><strong>%s</strong>`,
			html.EscapeString(strings.ToLower(e.Group)), html.EscapeString(attrList(e.Perspectives)), openAttr, col.span(e.DateLabel), col.span(e.Title)))
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
	sb.WriteString(`<div class="tbl-wrap"><table class="cmp-tbl">`)
	if len(blk.Columns) > 0 {
		sb.WriteString(`<thead><tr>`)
		for _, c := range blk.Columns {
			sb.WriteString(`<th>` + col.span(c) + `</th>`)
		}
		sb.WriteString(`</tr></thead>`)
	}
	sb.WriteString(`<tbody>`)
	for _, r := range blk.Rows {
		sb.WriteString(fmt.Sprintf(`<tr class="row-%s" data-group="%s" data-perspectives="%s">`,
			html.EscapeString(r.Badge), html.EscapeString(strings.ToLower(r.Group)), html.EscapeString(attrList(r.Perspectives))))
		for _, cell := range r.Cells {
			sb.WriteString(`<td>` + col.span(cell) + `</td>`)
		}
		sb.WriteString(`</tr>`)
	}
	sb.WriteString(`</tbody></table></div>`)
}

func renderChecklistLedger(sb *strings.Builder, blk BlockSpec, col *i18nCollector) {
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
	renderGroup := func(title, badgeCls string, list []ChecklistRow) {
		if len(list) == 0 {
			return
		}
		sb.WriteString(fmt.Sprintf(`<div class="chk-group"><h3>%s (%d)</h3><ul class="chk-list">`, html.EscapeString(title), len(list)))
		for _, it := range list {
			sb.WriteString(fmt.Sprintf(`<li class="chk-row" data-group="%s"><span class="badge %s">%s</span><div class="chk-main"><div class="chk-title">%s`,
				html.EscapeString(strings.ToLower(it.Group)), badgeCls, html.EscapeString(it.Status), col.span(it.Title)))
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
	renderGroup("Open", "b-open", openRows)
	renderGroup("Confirmed", "b-ok", confirmedRows)
	if len(skippedRows) > 0 {
		sb.WriteString(`<details class="skipped-sec"><summary>Skipped (` + fmt.Sprintf("%d", len(skippedRows)) + ` — won't re-suggest)</summary>`)
		renderGroup("Skipped", "b-skip", skippedRows)
		sb.WriteString(`</details>`)
	}
}

func renderCalloutNotes(sb *strings.Builder, blk BlockSpec, col *i18nCollector) {
	sb.WriteString(`<div class="notes-grid">`)
	for _, n := range blk.Notes {
		tone := n.Tone
		if tone == "" {
			tone = "info"
		}
		sb.WriteString(fmt.Sprintf(`<article class="note tone-%s" data-group="%s" data-perspectives="%s"><h3>%s</h3><p>%s</p>`,
			html.EscapeString(tone), html.EscapeString(strings.ToLower(n.Group)), html.EscapeString(attrList(n.Perspectives)), col.span(n.Title), col.span(n.Body)))
		hasActions := strings.TrimSpace(n.CopyText) != "" || strings.TrimSpace(n.CopyAltText) != "" || safeHref(n.LinkURL) != ""
		if hasActions {
			sb.WriteString(`<div class="act-row">`)
			if href := safeHref(n.LinkURL); href != "" {
				lbl := n.LinkLabel
				if lbl.IsEmpty() {
					lbl = Text("Open ↗")
				}
				sb.WriteString(fmt.Sprintf(`<a class="copy-btn link-btn" href="%s">%s</a>`, href, col.span(lbl)))
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

const livingSpaceCSS = `
:root{--bg:var(--background,#f8fafc);--card-bg:var(--card,#ffffff);--fg:var(--foreground,#0f172a);--muted:var(--muted-foreground,#475569);--bdr:var(--border,#e2e8f0);--acc:var(--primary,#2563eb);--ok:#16a34a;--warn:#d97706}
@media(prefers-color-scheme:dark){:root{--bg:var(--background,#0b1120);--card-bg:var(--card,#111827);--fg:var(--foreground,#f1f5f9);--muted:var(--muted-foreground,#94a3b8);--bdr:var(--border,#1e293b);--acc:var(--primary,#60a5fa)}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif}
.wrap{max-width:960px;margin:0 auto;padding:20px 16px 40px}
.hdr{display:flex;flex-wrap:wrap;justify-content:space-between;align-items:flex-start;gap:12px;margin-bottom:16px}
h1{margin:0;font-size:22px;line-height:1.25}h2{margin:0 0 10px;font-size:16px}h3{margin:0 0 6px;font-size:14px}
.sub{margin:4px 0 0;color:var(--muted);font-size:13px}
.controls{display:flex;flex-wrap:wrap;gap:10px;margin-bottom:16px}
.ctrl-group,.lang-switch{display:flex;flex-wrap:wrap;gap:6px}
.pill{border:1px solid var(--bdr);background:var(--card-bg);color:var(--fg);padding:5px 12px;border-radius:999px;font-size:12px;cursor:pointer;font-weight:500}
.pill.active{background:var(--acc);color:#fff;border-color:var(--acc)}
.block{background:var(--card-bg);border:1px solid var(--bdr);border-radius:12px;padding:16px;margin-bottom:14px}
.kpi-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:10px}
.kpi-card{border:1px solid var(--bdr);border-radius:10px;padding:12px;background:var(--bg)}
.kpi-card.tone-accent{border-color:var(--acc)}.kpi-card.tone-warn{border-color:var(--warn)}.kpi-card.tone-ok{border-color:var(--ok)}
.kpi-lbl{font-size:12px;color:var(--muted);margin-bottom:4px}.kpi-val{font-size:18px;font-weight:700}.kpi-sub{font-size:12px;color:var(--muted);margin-top:4px}
.bar{height:6px;background:var(--bdr);border-radius:999px;overflow:hidden;margin-top:6px}.bar span{display:block;height:100%;background:var(--acc)}
.timeline{display:flex;flex-direction:column;gap:8px}
.tl-item{border:1px solid var(--bdr);border-radius:10px;padding:10px 12px;background:var(--bg)}
.tl-item summary{cursor:pointer;display:flex;flex-wrap:wrap;gap:8px;align-items:center}
.tl-date{font-size:12px;font-weight:600;color:var(--acc);background:var(--card-bg);padding:2px 8px;border-radius:6px;border:1px solid var(--bdr)}
.tl-sum{font-size:12px;color:var(--muted);margin-left:auto}.tl-body{margin-top:8px;padding-left:6px}.tl-body ul{margin:4px 0;padding-left:18px}
.tl-callout{margin-top:8px;padding:8px 10px;border-left:3px solid var(--acc);background:var(--card-bg);border-radius:6px;font-size:13px}
.tbl-wrap{overflow-x:auto}.cmp-tbl{width:100%;border-collapse:collapse;font-size:13px}
.cmp-tbl th,.cmp-tbl td{text-align:left;padding:8px 10px;border-bottom:1px solid var(--bdr)}.cmp-tbl th{font-size:12px;color:var(--muted)}
.cmp-tbl tr.row-warn td{background:rgba(217,119,6,.08)}
.chk-list{list-style:none;margin:0 0 10px;padding:0;display:flex;flex-direction:column;gap:6px}
.chk-row{display:flex;gap:10px;align-items:flex-start;padding:8px 10px;border:1px solid var(--bdr);border-radius:8px;background:var(--bg)}
.badge{font-size:12px;padding:2px 8px;border-radius:999px;font-weight:600;text-transform:uppercase}
.b-ok{background:rgba(22,163,74,.15);color:var(--ok)}.b-open{background:rgba(217,119,6,.15);color:var(--warn)}.b-skip{background:var(--bdr);color:var(--muted)}
.chk-title{font-weight:600}.chk-meta{font-size:12px;color:var(--muted);margin-top:2px}
.skipped-sec{margin-top:8px;font-size:13px;color:var(--muted)}.skipped-sec summary{cursor:pointer}
.notes-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:10px}
.note{border:1px solid var(--bdr);border-left:4px solid var(--acc);border-radius:8px;padding:10px 12px;background:var(--bg)}
.note.tone-warn{border-left-color:var(--warn)}.note.tone-ok{border-left-color:var(--ok)}.note p{margin:4px 0 0;font-size:13px;white-space:pre-line}
.act-row{display:flex;flex-wrap:wrap;gap:6px;margin-top:8px}
.copy-btn{border:1px solid var(--bdr);background:var(--card-bg);color:var(--fg);padding:5px 10px;border-radius:8px;font-size:12px;font-weight:600;cursor:pointer;text-decoration:none;display:inline-flex;align-items:center;gap:4px;min-height:30px}
.copy-btn.primary-btn{background:var(--acc);color:#fff;border-color:var(--acc)}
.copy-btn.copied{background:var(--ok);color:#fff;border-color:var(--ok)}
.ftr{text-align:center;color:var(--muted);font-size:12px;margin-top:20px}
.hidden{display:none!important}
`

const livingSpaceJS = `
(function(){
  var dataEl=document.getElementById('hq-i18n');
  var i18n=dataEl?JSON.parse(dataEl.textContent):{locales:['en'],default:'en',dicts:{}};
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

  var activePersp=(i18n.defaultPerspective||'all'),activeGrp='all';
  function applyFilters(){
    document.querySelectorAll('[data-perspectives],[data-group]').forEach(function(el){
      var ps=(el.getAttribute('data-perspectives')||'').split(',').filter(Boolean);
      var gr=(el.getAttribute('data-group')||'').trim();
      var okP=(activePersp==='all'||ps.length===0||ps.indexOf(activePersp)!==-1);
      var okG=(activeGrp==='all'||!gr||gr===activeGrp);
      el.classList.toggle('hidden',!(okP&&okG));
    });
  }
  document.querySelectorAll('.persp-btn').forEach(function(b){
    b.classList.toggle('active',b.getAttribute('data-persp')===activePersp);
    b.addEventListener('click',function(){
      activePersp=b.getAttribute('data-persp');
      document.querySelectorAll('.persp-btn').forEach(function(x){x.classList.toggle('active',x===b);});
      applyFilters();
    });
  });
  applyFilters();

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
  document.querySelectorAll('.grp-btn').forEach(function(b){
    b.addEventListener('click',function(){
      activeGrp=b.getAttribute('data-grp');
      document.querySelectorAll('.grp-btn').forEach(function(x){x.classList.toggle('active',x===b);});
      applyFilters();
    });
  });

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
