package ui

import (
	"regexp"
	"strings"

	"github.com/aymerick/douceur/css"
	"github.com/aymerick/douceur/parser"
	"github.com/microcosm-cc/bluemonday"

	"github.com/jsnjack/mailbox/internal/logging"
)

// emailPolicy returns an HTML sanitizer tuned for rendering real email. It keeps
// UGCPolicy's safety guarantees (no <script>, no on* event handlers, only safe
// URL schemes, and CSS values validated against bluemonday's safe set) but
// additionally permits the inline styling, presentational markup, and class
// hooks that HTML email relies on — UGCPolicy strips those, leaving messages
// looking broken.
//
// The reader's WebView runs with JavaScript disabled and a strict per-render CSP
// (default-src 'none', script-src locked to a nonce), so permitting
// presentational CSS/markup here does not reintroduce a meaningful
// script-execution surface.
func emailPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()

	// Inline CSS is how virtually all HTML email is styled. bluemonday still
	// validates the declarations and drops unsafe ones (e.g. expression(),
	// javascript: urls).
	p.AllowAttrs("style").Globally()

	// Allow cid: image references through (in addition to the default
	// http/https/mailto) so inline images survive sanitizing; inlineCIDImages then
	// rewrites them to embedded data: URIs for rendering. cid: in any other context
	// is inert (no handler), and the data: it becomes is injected after this pass.
	p.AllowURLSchemes("http", "https", "mailto", "cid")

	// class is purely presentational; it lets each message's scoped <style> rules
	// (see scopeCSS) match the elements they were written for — many newsletters
	// lay out via classes rather than inline styles.
	p.AllowAttrs("class").Globally()

	// Legacy presentational attributes used heavily in table-based layouts.
	p.AllowAttrs(
		"align", "valign", "bgcolor", "color", "background",
		"width", "height", "border", "cellpadding", "cellspacing", "dir",
	).Globally()

	// Presentational elements emails depend on that UGCPolicy omits.
	p.AllowElements("center", "font")
	p.AllowAttrs("face", "size", "color").OnElements("font")
	p.AllowAttrs("colspan", "rowspan", "nowrap").OnElements("td", "th")

	return p
}

var styleBlockRe = regexp.MustCompile(`(?is)<style[^>]*>(.*?)</style>`)

// extractStyleCSS returns the concatenated contents of every <style> block in
// raw email HTML. The sanitizer drops <style> (its CSS can't be validated the
// way inline declarations are), so the body's layout — newsletters routinely
// style via classes defined only here — would collapse. We capture the CSS here
// and re-add it scoped (see scopeCSS) so it renders without affecting anything
// outside the message.
func extractStyleCSS(htmlStr string) string {
	var b strings.Builder
	for _, m := range styleBlockRe.FindAllStringSubmatch(htmlStr, -1) {
		b.WriteString(m[1])
		b.WriteByte('\n')
	}
	return b.String()
}

// scopeCSS prefixes every selector in an email's CSS with scopeSel so the rules
// apply only inside that message's wrapper. This preserves the email's own
// cascade — an element's inline style still beats a class, unlike CSS inlining,
// which flattens both into one attribute and can clobber the intended value —
// while stopping one message's styles from bleeding onto another in a
// multi-message thread. Page-level selectors (html/body/:root/*) map to the
// wrapper itself. Returns "" if the CSS can't be parsed or could break out of
// the <style> element.
func scopeCSS(cssText, scopeSel string) string {
	ss, err := parser.Parse(cssText)
	if err != nil {
		logging.Trace("ui: scope css parse failed", "err", err, "bytes", len(cssText))
		return ""
	}
	scopeRules(ss.Rules, scopeSel)
	out := ss.String()
	if strings.Contains(strings.ToLower(out), "</style") {
		logging.Trace("ui: scope css rejected", "reason", "closes style tag")
		return "" // never let serialized CSS terminate the <style> tag early
	}
	logging.Trace("ui: scope css", "scope", scopeSel, "in_bytes", len(cssText), "out_bytes", len(out))
	return out
}

// scopeRules rewrites each rule's selectors in place (recursing into @media /
// @supports blocks) and strips !important. Dropping !important makes the email's
// <style> behave as defaults that an element's own inline style overrides — the
// correct cascade for email, and what Gmail effectively does. Without it, an
// Outlook-targeted hack like ".keep-white { color:#000 !important }" (paired with
// an mso gradient WebKit ignores) would override an inline color:#fff and render
// white-on-dark banners as black.
func scopeRules(rules []*css.Rule, scopeSel string) {
	for _, r := range rules {
		scopeRules(r.Rules, scopeSel)
		for _, d := range r.Declarations {
			d.Important = false
		}
		for i, sel := range r.Selectors {
			r.Selectors[i] = scopeSelector(sel, scopeSel)
		}
	}
}

func scopeSelector(sel, scopeSel string) string {
	switch strings.TrimSpace(sel) {
	case "html", "body", ":root", "*", "":
		return scopeSel // page-level rules apply to the message wrapper
	}
	return scopeSel + " " + strings.TrimSpace(sel)
}
