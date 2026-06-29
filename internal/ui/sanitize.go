package ui

import (
	"github.com/aymerick/douceur/inliner"
	"github.com/microcosm-cc/bluemonday"
)

// inlineEmailCSS resolves an email's <style> rules into per-element style
// attributes before sanitizing. The sanitizer strips <style> blocks (they can't
// be safely validated), so newsletters that lay out via classes — Checkly's
// monitoring grid uses .d-flex/.bar/widths defined only in <style> — would
// otherwise collapse. Inlining moves those declarations onto the elements, where
// the sanitizer keeps the safe ones (display/width/background/…), and confines
// each email's styles to its own elements (no cross-message bleed in a thread).
// Media-query/pseudo rules that can't be inlined are dropped — fine for the
// fixed-width reader. Falls back to the original HTML if inlining fails.
func inlineEmailCSS(htmlStr string) string {
	out, err := inliner.Inline(htmlStr)
	if err != nil {
		return htmlStr
	}
	return out
}

// emailPolicy returns an HTML sanitizer tuned for rendering real email. It keeps
// UGCPolicy's safety guarantees (no <script>, no on* event handlers, only safe
// URL schemes, and CSS values validated against bluemonday's safe set) but
// additionally permits the inline styling and table-based layout that HTML email
// relies on — UGCPolicy strips those, leaving messages looking broken.
//
// The reader's WebView runs with JavaScript disabled and remote images blocked
// by default, so permitting presentational CSS/markup here does not reintroduce
// a meaningful script-execution surface.
func emailPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()

	// Inline CSS is how virtually all HTML email is styled. bluemonday still
	// validates the declarations and drops unsafe ones (e.g. expression(),
	// javascript: urls).
	p.AllowAttrs("style").Globally()

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
