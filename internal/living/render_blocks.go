package living

import (
	"fmt"
	"html"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var (
	reDataImageURI = regexp.MustCompile(`^data:image/(png|jpeg|jpg|webp|svg\+xml);base64,[A-Za-z0-9+/=\s]+$`)
	reSafeTone     = regexp.MustCompile(`^[a-z0-9_-]+$`)
)

func normalizeTheme(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ThemeEditorial:
		return ThemeEditorial
	case ThemeHeritage:
		return ThemeHeritage
	case ThemeOcean:
		return ThemeOcean
	case ThemeForest:
		return ThemeForest
	default:
		return ThemeModern
	}
}

func safeTone(raw, fallback string) string {
	t := strings.ToLower(strings.TrimSpace(raw))
	if t != "" && reSafeTone.MatchString(t) {
		return t
	}
	return fallback
}

func safeViewBox(raw string) string {
	fields := strings.Fields(strings.ReplaceAll(strings.TrimSpace(raw), ",", " "))
	if len(fields) != 4 {
		return "0 0 800 400"
	}
	var vals [4]float64
	for i, f := range fields {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return "0 0 800 400"
		}
		vals[i] = v
	}
	if vals[2] <= 0 || vals[3] <= 0 {
		return "0 0 800 400"
	}
	return fmt.Sprintf("%.1f %.1f %.1f %.1f", vals[0], vals[1], vals[2], vals[3])
}

func safeSVGPoints(raw string) string {
	tokens := strings.Fields(strings.TrimSpace(raw))
	if len(tokens) == 0 {
		return ""
	}
	var pts []string
	for _, tok := range tokens {
		parts := strings.Split(tok, ",")
		if len(parts) != 2 {
			continue
		}
		x, errX := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		y, errY := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if errX != nil || errY != nil || math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) {
			continue
		}
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	return strings.Join(pts, " ")
}

func safeImageDataURI(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || !reDataImageURI.MatchString(s) {
		return ""
	}
	return html.EscapeString(s)
}

func renderInteractiveMap(sb *strings.Builder, blk BlockSpec, col *i18nCollector) {
	if blk.Map == nil {
		return
	}
	m := blk.Map
	vb := safeViewBox(m.ViewBox)
	mapID := html.EscapeString(strings.TrimSpace(blk.ID))
	if mapID == "" {
		mapID = "map"
	}

	sb.WriteString(fmt.Sprintf(`<div class="imap-shell" data-imap="%s">`, mapID))
	sb.WriteString(`<div class="imap-canvas">`)
	sb.WriteString(fmt.Sprintf(`<svg class="imap-svg" viewBox="%s" role="img" aria-label="Interactive Map">`, vb))

	// Architectural / geographical zones
	for _, z := range m.Zones {
		tone := safeTone(z.Tone, "neutral")
		sb.WriteString(fmt.Sprintf(`<g class="imap-zone tone-%s"><rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="12" ry="12"/>`,
			tone, z.X, z.Y, z.W, z.H))
		if !z.Label.IsEmpty() {
			cx := z.X + z.W/2
			cy := z.Y + 20
			key, primary := col.add(z.Label)
			if len(col.locales) > 1 {
				sb.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" data-i18n="%s">%s</text>`,
					cx, cy, key, html.EscapeString(primary)))
			} else {
				sb.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle">%s</text>`,
					cx, cy, html.EscapeString(primary)))
			}
		}
		sb.WriteString(`</g>`)
	}

	// Connecting route polylines
	for _, p := range m.Paths {
		pts := safeSVGPoints(p.Points)
		if pts == "" {
			continue
		}
		tone := safeTone(p.Tone, "accent")
		dash := ""
		if p.Dashed {
			dash = ` stroke-dasharray="7 5"`
		}
		sb.WriteString(fmt.Sprintf(`<polyline class="imap-path tone-%s" points="%s"%s fill="none"/>`, tone, pts, dash))
	}

	// Tappable numbered stop pins
	for idx, pin := range m.Pins {
		num := pin.Number
		if num <= 0 {
			num = idx + 1
		}
		tone := safeTone(pin.Tone, "accent")
		activeCls := ""
		if idx == 0 {
			activeCls = " active"
		}
		sb.WriteString(fmt.Sprintf(`<g class="imap-pin tone-%s%s" tabindex="0" role="button" data-map-pin="%d" data-group="%s" data-perspectives="%s" transform="translate(%.1f,%.1f)">`,
			tone, activeCls, idx, html.EscapeString(strings.ToLower(pin.Group)), html.EscapeString(attrList(pin.Perspectives)), pin.X, pin.Y))
		sb.WriteString(`<circle class="imap-ring" r="19"/>`)
		sb.WriteString(`<circle class="imap-dot" r="13"/>`)
		sb.WriteString(fmt.Sprintf(`<text class="imap-num" y="4" text-anchor="middle">%d</text>`, num))
		sb.WriteString(`</g>`)
	}

	sb.WriteString(`</svg></div>`)

	// Optional legend pills
	if len(m.Legend) > 0 {
		sb.WriteString(`<div class="imap-legend">`)
		for _, item := range m.Legend {
			sb.WriteString(`<span class="imap-leg-pill">` + col.span(item) + `</span>`)
		}
		sb.WriteString(`</div>`)
	}

	// Interactive stop selector rail + Prev/Next guided tour inspector
	if len(m.Pins) > 0 {
		sb.WriteString(`<div class="imap-bar">`)
		sb.WriteString(`<div class="imap-pills">`)
		for idx, pin := range m.Pins {
			num := pin.Number
			if num <= 0 {
				num = idx + 1
			}
			activeCls := ""
			if idx == 0 {
				activeCls = " active"
			}
			sb.WriteString(fmt.Sprintf(`<button type="button" class="pill imap-pill%s" data-map-pin="%d" data-group="%s" data-perspectives="%s"><span class="imap-pill-num">%d</span> %s</button>`,
				activeCls, idx, html.EscapeString(strings.ToLower(pin.Group)), html.EscapeString(attrList(pin.Perspectives)), num, col.span(pin.Title)))
		}
		sb.WriteString(`</div>`)
		if len(m.Pins) > 1 {
			sb.WriteString(`<div class="imap-nav"><button type="button" class="pill imap-prev">← Prev</button><button type="button" class="pill imap-next">Next →</button></div>`)
		}
		sb.WriteString(`</div>`)

		sb.WriteString(`<div class="imap-details">`)
		for idx, pin := range m.Pins {
			num := pin.Number
			if num <= 0 {
				num = idx + 1
			}
			activeCls := ""
			if idx == 0 {
				activeCls = " active"
			}
			sb.WriteString(fmt.Sprintf(`<article class="imap-detail%s" data-map-detail="%d" data-searchable="1">`, activeCls, idx))
			sb.WriteString(`<div class="imap-detail-hdr">`)
			sb.WriteString(fmt.Sprintf(`<span class="imap-badge">%d / %d</span>`, num, len(m.Pins)))
			sb.WriteString(`<div class="imap-detail-titles"><h3>` + col.span(pin.Title) + `</h3>`)
			if !pin.Subtitle.IsEmpty() {
				sb.WriteString(`<div class="imap-detail-sub">` + col.span(pin.Subtitle) + `</div>`)
			}
			sb.WriteString(`</div>`)
			if !pin.Meta.IsEmpty() {
				sb.WriteString(`<span class="imap-meta">` + col.span(pin.Meta) + `</span>`)
			}
			sb.WriteString(`</div>`)
			if !pin.Body.IsEmpty() {
				sb.WriteString(`<p class="imap-body">` + col.span(pin.Body) + `</p>`)
			}
			if !pin.Tip.IsEmpty() {
				sb.WriteString(`<div class="tl-callout">💡 ` + col.span(pin.Tip) + `</div>`)
			}
			sb.WriteString(`</article>`)
		}
		sb.WriteString(`</div>`)
	}

	sb.WriteString(`</div>`)
}

func renderStoryCards(sb *strings.Builder, blk BlockSpec, col *i18nCollector) {
	sb.WriteString(`<div class="story-grid">`)
	for _, c := range blk.Cards {
		tone := safeTone(c.Tone, "neutral")
		sb.WriteString(fmt.Sprintf(`<article class="story-card tone-%s" data-group="%s" data-perspectives="%s" data-searchable="1">`,
			tone, html.EscapeString(strings.ToLower(c.Group)), html.EscapeString(attrList(c.Perspectives))))

		if imgURI := safeImageDataURI(c.ImageDataURI); imgURI != "" {
			sb.WriteString(fmt.Sprintf(`<div class="story-media"><img src="%s" alt="" loading="lazy"/></div>`, imgURI))
		}
		sb.WriteString(`<div class="story-pad">`)
		if !c.Eyebrow.IsEmpty() || c.Icon != "" || c.Badge != "" {
			sb.WriteString(`<div class="story-top">`)
			sb.WriteString(`<span class="story-eye">`)
			if c.Icon != "" {
				sb.WriteString(`<span class="story-ico">` + html.EscapeString(c.Icon) + `</span>`)
			}
			if !c.Eyebrow.IsEmpty() {
				sb.WriteString(col.span(c.Eyebrow))
			}
			sb.WriteString(`</span>`)
			if c.Badge != "" {
				sb.WriteString(`<span class="badge b-info">` + html.EscapeString(c.Badge) + `</span>`)
			}
			sb.WriteString(`</div>`)
		}

		sb.WriteString(`<h3 class="story-title">` + col.span(c.Title) + `</h3>`)
		if !c.Subtitle.IsEmpty() {
			sb.WriteString(`<div class="story-sub">` + col.span(c.Subtitle) + `</div>`)
		}
		if !c.Body.IsEmpty() {
			sb.WriteString(`<p class="story-body">` + col.span(c.Body) + `</p>`)
		}
		if len(c.Highlights) > 0 {
			sb.WriteString(`<ul class="story-list">`)
			for _, h := range c.Highlights {
				sb.WriteString(`<li>` + col.span(h) + `</li>`)
			}
			sb.WriteString(`</ul>`)
		}
		if !c.ExpandBody.IsEmpty() {
			expLbl := c.ExpandLabel
			if expLbl.IsEmpty() {
				expLbl = Text("Read deeper")
			}
			sb.WriteString(`<details class="story-deep"><summary>` + col.span(expLbl) + `</summary><p>` + col.span(c.ExpandBody) + `</p></details>`)
		}
		if !c.Meta.IsEmpty() {
			sb.WriteString(`<div class="story-meta">` + col.span(c.Meta) + `</div>`)
		}

		hasActions := strings.TrimSpace(c.CopyText) != "" || safeHref(c.LinkURL) != ""
		if hasActions {
			sb.WriteString(`<div class="act-row">`)
			if href := safeHref(c.LinkURL); href != "" {
				lbl := c.LinkLabel
				if lbl.IsEmpty() {
					lbl = Text("Open ↗")
				}
				sb.WriteString(fmt.Sprintf(`<a class="copy-btn link-btn" href="%s" target="_blank" rel="noopener noreferrer">%s</a>`, href, col.span(lbl)))
			}
			if strings.TrimSpace(c.CopyText) != "" {
				lbl := c.CopyLabel
				if lbl.IsEmpty() {
					lbl = Text("Copy")
				}
				sb.WriteString(fmt.Sprintf(`<button type="button" class="copy-btn primary-btn" data-copy="%s">📋 %s</button>`,
					html.EscapeString(c.CopyText), col.span(lbl)))
			}
			sb.WriteString(`</div>`)
		}
		sb.WriteString(`</div></article>`)
	}
	sb.WriteString(`</div>`)
}
