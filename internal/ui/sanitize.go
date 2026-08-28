package ui

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

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

// extractStyleBlocks returns the contents of each <style> block in raw email
// HTML, separately. The sanitizer drops <style> (its CSS can't be validated the
// way inline declarations are), so the body's layout — newsletters routinely
// style via classes defined only here — would collapse. We capture the CSS here
// and re-add it scoped (see scopeCSS) so it renders without affecting anything
// outside the message.
//
// Keeping the blocks apart is what stops one malformed block from costing an
// email every style it has: a real message carries several (a framework's reset,
// the template's layout, a hand-written footer), they are independent stylesheets
// to a browser, and concatenating them makes a single unbalanced brace anywhere
// abort the parse for all of them. A Ryanair itinerary ships eight blocks, the
// last of which nests an @media inside a declaration; joined, that one took the
// other seven down and the email rendered with no CSS at all.
func extractStyleBlocks(htmlStr string) []string {
	matches := styleBlockRe.FindAllStringSubmatch(htmlStr, -1)
	blocks := make([]string, 0, len(matches))
	for _, m := range matches {
		blocks = append(blocks, stripCSSMarkupScaffolding(m[1]))
	}
	return blocks
}

// scopeEmailStyleBlocks scopes every <style> block of an email to scopeSel,
// independently, and returns the concatenated result plus the number of tracking
// resources removed. A block that cannot be parsed or scoped contributes nothing
// and the rest still render.
func scopeEmailStyleBlocks(htmlStr, scopeSel string) (string, int) {
	var b strings.Builder
	trackers := 0
	for i, block := range extractStyleBlocks(htmlStr) {
		if strings.TrimSpace(block) == "" {
			continue
		}
		scoped, n := scopeEmailCSS(block, scopeSel)
		trackers += n
		if scoped == "" {
			logging.Trace("ui: style block dropped", "block", i, "bytes", len(block))
			continue
		}
		b.WriteString(scoped)
		b.WriteByte('\n')
	}
	return b.String(), trackers
}

// cssMarkupScaffoldingRe matches the markup scaffolding email stylesheets carry
// inside <style>: the legacy CDO/CDC tokens that once hid CSS from browsers
// without stylesheet support, and the Outlook conditional comments built on
// them ("<!--[if (gte mso 9)|(IE)]>li{…}<![endif]-->" — the tail of a Ziggo
// invoice, and of a great deal of transactional mail).
var cssMarkupScaffoldingRe = regexp.MustCompile(`(?is)<!--\[if[^\]]*\]>|<!\[endif\]-->|<!\[endif\]|<!-->|<!--|-->`)

// stripCSSMarkupScaffolding removes that scaffolding before any CSS parse. A
// browser's parser skips these tokens and error-recovers from the selector they
// corrupt; douceur does neither, and would keep a rule whose selector is the
// leftover markup. The scaffolding carries no style, so dropping it costs
// nothing and keeps the declarations around it.
//
// It is no longer what stands between the app and a hung render: serializing
// such a rule produces "<![endif]-->;", and it was the trailing ";" — sitting
// where a selector belongs when the reader parses the scoped stylesheet again —
// that spun douceur forever, not the markup. stripPreludeSemicolons removes that
// token wherever it comes from, so this pass is back to being about rendering.
func stripCSSMarkupScaffolding(cssText string) string {
	if !strings.Contains(cssText, "<!") && !strings.Contains(cssText, "-->") {
		return cssText
	}
	return cssMarkupScaffoldingRe.ReplaceAllString(cssText, " ")
}

// atRulesWithRulesBlock mirrors douceur's own list of at-rules whose block holds
// nested rules rather than declarations. stripPreludeSemicolons has to agree with
// douceur about which kind of block it is inside, because that decides whether a
// ";" is a declaration separator (legal, load-bearing) or garbage sitting where a
// selector belongs (the hang below). Matching is case-insensitive while douceur's
// is exact, and deliberately so: the two classifications can only disagree on a
// mixed-case at-rule, and disagreeing in this direction drops a ";" douceur wanted
// (two declarations merge into one bogus one, in CSS that was already malformed)
// where the other direction hands douceur the token it spins on.
var atRulesWithRulesBlock = map[string]bool{
	"@document": true, "@font-feature-values": true, "@keyframes": true,
	"@media": true, "@supports": true,
}

// preludeEmbedsRules reports whether the block introduced by prelude holds rules
// rather than declarations.
func preludeEmbedsRules(prelude string) bool {
	p := strings.TrimSpace(prelude)
	if !strings.HasPrefix(p, "@") {
		return false // a qualified rule: selectors, so the block holds declarations
	}
	name := p
	if i := strings.IndexAny(p, " \t\r\n(\""); i > 0 {
		name = p[:i]
	}
	return atRulesWithRulesBlock[strings.ToLower(name)]
}

// stripPreludeSemicolons removes every ";" that sits where a selector belongs,
// and is the reason a malformed email stylesheet can no longer wedge the app.
//
// douceur's parser has exactly one input it cannot make progress on. Reading a
// rule list, an unrecognised token is handed to parseQualifiedRule, which loops
// "if the next token is '{' parse the block, else parse a prelude" — and
// parsePrelude consumes tokens only until it reaches ";" or "{". So a ";" in
// prelude position is consumed by neither: parsePrelude returns having shifted
// nothing, the caller asks again, forever, at 100% of a core. Every other token
// is shifted by someone. That makes this pass a complete guard for the parser as
// it stands, not a blocklist of the strings we happened to meet:
//
//   - ";.b{}" or ".a{}\n;" — a stray semicolon between rules, which browsers
//     simply ignore;
//   - "table.sm-12&gt;tbody{...}" — a <style> whose CSS was HTML-escaped by the
//     sender's template engine. Character references are not decoded inside
//     <style>, so the ";" ending each entity lands in a selector. This is what a
//     Ryanair itinerary ships, and opening one hung the render goroutine
//     permanently: the conversation never appeared and the reader sat empty; and
//   - "<![endif]-->;" — douceur's own serialization of a rule whose selector is
//     leftover Outlook conditional-comment markup (see stripCSSMarkupScaffolding,
//     which removes that scaffolding for rendering's sake; this is what makes the
//     re-parse safe regardless).
//
// The escaped ";" is dropped rather than the malformed rule around it: the rule's
// declarations are usually fine and its selector was never going to match in a
// browser either, so dropping only the token keeps the rest of the stylesheet
// parsing as it does everywhere else.
//
// Semicolons are kept inside a declaration block (where they separate
// declarations — a "url(data:image/png;base64,…)" value included) and in an
// at-rule prelude, where "@import url(…);" ends the statement and douceur's
// parseAtRule consumes it itself.
func stripPreludeSemicolons(cssText string) (string, int) {
	if !strings.Contains(cssText, ";") {
		return cssText, 0
	}
	var out strings.Builder
	out.Grow(len(cssText))
	// declBlock records, for each open "{", whether its body holds declarations
	// rather than nested rules. An empty stack is the top level, itself a rule
	// list.
	var declBlock []bool
	inDeclarations := func() bool { return len(declBlock) > 0 && declBlock[len(declBlock)-1] }
	// The prelude — the selector or at-rule prelude being accumulated — is the
	// input between the last boundary ("{", "}" or ";") and here, so it is a
	// slice rather than a copy. preludeBlank tracks whether it is still all
	// whitespace, which is the only case where a comment has to move its start:
	// "/*c*/@media{…}" is an at-rule to douceur, and misreading it as a
	// qualified rule would keep a ";" inside that douceur spins on.
	preludeStart, preludeBlank := 0, true
	boundary := func(i int) { preludeStart, preludeBlank = i+1, true }
	dropped := 0
	for i := 0; i < len(cssText); {
		if end, comment, ok := cssOpaqueEnd(cssText, i); ok {
			out.WriteString(cssText[i:end])
			if comment && preludeBlank {
				preludeStart = end
			} else {
				preludeBlank = false
			}
			i = end
			continue
		}
		c := cssText[i]
		switch c {
		case '{':
			declBlock = append(declBlock, !preludeEmbedsRules(cssText[preludeStart:i]))
			out.WriteByte(c)
			boundary(i)
		case '}':
			if len(declBlock) > 0 {
				declBlock = declBlock[:len(declBlock)-1]
			}
			out.WriteByte(c)
			boundary(i)
		case ';':
			if inDeclarations() || strings.HasPrefix(strings.TrimSpace(cssText[preludeStart:i]), "@") {
				out.WriteByte(c)
			} else {
				dropped++
			}
			boundary(i)
		default:
			out.WriteByte(c)
			if preludeBlank && c != ' ' && c != '\t' && c != '\r' && c != '\n' {
				preludeBlank = false
			}
		}
		i++
	}
	if dropped > 0 {
		logging.Trace("ui: css prelude semicolons dropped", "n", dropped, "bytes", len(cssText))
	}
	return out.String(), dropped
}

// cssOpaqueEnd reports whether a region whose contents carry no CSS structure
// starts at i — a comment, a quoted string, or a backslash escape — and returns
// the index just past it. Braces and semicolons inside such a region are text,
// not syntax ("url(data:…;base64,…)" quoted, or content:"{"), so both passes
// below step over them as a unit. comment distinguishes the one form that is not
// part of a selector.
func cssOpaqueEnd(cssText string, i int) (end int, comment bool, ok bool) {
	switch c := cssText[i]; {
	case c == '\\' && i+1 < len(cssText):
		return i + 2, false, true // an escape can carry any character, ";" included
	case c == '/' && i+1 < len(cssText) && cssText[i+1] == '*':
		if j := strings.Index(cssText[i+2:], "*/"); j >= 0 {
			return i + 2 + j + 2, true, true
		}
		return len(cssText), true, true // unterminated comment runs to the end
	case c == '"' || c == '\'':
		quote := c
		for j := i + 1; j < len(cssText); j++ {
			switch cssText[j] {
			case '\\':
				j++
			case quote:
				return j + 1, false, true
			case '\n', '\r':
				return j, false, true // a newline ends a bad string
			}
		}
		return len(cssText), false, true
	}
	return i, false, false
}

// splitTopLevelRules cuts a stylesheet into its top-level rules — one qualified
// rule or at-rule per element, each with its block, nested rules included.
func splitTopLevelRules(cssText string) []string {
	var rules []string
	depth, start := 0, 0
	emit := func(end int) {
		if chunk := strings.TrimSpace(cssText[start:end]); chunk != "" {
			rules = append(rules, chunk)
		}
		start = end
	}
	for i := 0; i < len(cssText); {
		if end, _, ok := cssOpaqueEnd(cssText, i); ok {
			i = end
			continue
		}
		switch cssText[i] {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
			if depth == 0 {
				emit(i + 1)
			}
		case ';':
			if depth == 0 {
				emit(i + 1) // an at-statement: "@import …;"
			}
		}
		i++
	}
	emit(len(cssText)) // trailing text with no closing brace
	return rules
}

// parseRulesIndividually is the recovery path for a stylesheet douceur cannot
// parse as a whole, and it is what a browser does with the same input: a rule it
// cannot make sense of is skipped, and the rules after it still apply. douceur
// has no way to resume — it abandons a stylesheet at the first error and returns
// none of the rules it had already built, so one malformed rule anywhere costs an
// email every rule below it. Parsing each top-level rule on its own puts that
// cost back where it belongs: the malformed rule alone is lost.
//
// A Ryanair itinerary is the case in hand. One block writes
//
//	.nowrap { white-space: nowrap @media (max-width: 480px) { white-space: wrap; } }
//
// — an @media opened inside a declaration, never closed as one — and the receipt
// table's own styling is written below it.
func parseRulesIndividually(cssText string) []*css.Rule {
	chunks := splitTopLevelRules(cssText)
	if len(chunks) <= 1 {
		return nil // nothing to isolate: the single rule is the one that fails
	}
	var rules []*css.Rule
	dropped := 0
	for _, chunk := range chunks {
		ss, err := parser.NewParser(chunk).ParseStylesheet()
		if err != nil || ss == nil || len(ss.Rules) == 0 {
			dropped++
			logging.Trace("ui: css rule dropped", "reason", "unparsable in isolation",
				"err", err, "rule", logging.Body(chunk))
			continue
		}
		rules = append(rules, ss.Rules...)
	}
	logging.Trace("ui: css parsed rule by rule", "kept", len(rules), "dropped", dropped)
	return rules
}

// cssParseTimeout bounds one stylesheet parse. Reaching it means douceur found a
// new way not to terminate, so the value only has to be far above a real parse
// (single-digit milliseconds for the largest stylesheets email carries).
const cssParseTimeout = 2 * time.Second

// parseStylesheet is the only place email CSS is handed to douceur. Both callers
// — scoping a message's <style> for render, and re-reading the scoped result to
// find its image URLs — go through here so neither can be hardened while the
// other is forgotten, which is how the re-parse ended up unprotected before.
//
// Three layers, outermost last: stripCSSMarkupScaffolding removes markup that
// leaked into the stylesheet, stripPreludeSemicolons removes the token douceur
// cannot make progress on, and the timeout is the backstop for a pathology we
// have not seen. The abandoned goroutine keeps spinning — a parse cannot be
// cancelled from outside — so the timeout is a way to keep the app usable (this
// message renders unstyled, everything else works), not a substitute for the two
// passes that make it unreachable. A panic is recovered for the same reason.
func parseStylesheet(cssText string) (*css.Stylesheet, error) {
	hardened, _ := stripPreludeSemicolons(stripCSSMarkupScaffolding(cssText))
	type parsed struct {
		ss  *css.Stylesheet
		err error
	}
	done := make(chan parsed, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- parsed{nil, fmt.Errorf("css parse panicked: %v", r)}
			}
		}()
		ss, err := parser.NewParser(hardened).ParseStylesheet()
		if err != nil {
			// Salvage the rules around the one that broke it, the way a browser
			// would (see parseRulesIndividually).
			if rules := parseRulesIndividually(hardened); len(rules) > 0 {
				recovered := css.NewStylesheet()
				recovered.Rules = rules
				done <- parsed{recovered, err}
				return
			}
		}
		done <- parsed{ss, err}
	}()
	select {
	case p := <-done:
		return p.ss, p.err
	case <-time.After(cssParseTimeout):
		logging.Trace("ui: css parse abandoned", "reason", "timeout",
			"timeout", cssParseTimeout, "bytes", len(hardened), "css", logging.Body(hardened))
		return nil, errors.New("css parse did not terminate")
	}
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
	out, _ := scopeEmailCSS(cssText, scopeSel)
	return out
}

// scopeEmailCSS scopes email styles and removes remote resources from rules
// that explicitly conceal them or use a known tracking endpoint.
func scopeEmailCSS(cssText, scopeSel string) (string, int) {
	// The breakout test runs on the input as well as the output below: a
	// "</style><script>" rides in as a selector, and dropMarkupRules would now
	// remove that rule before the output test could ever see it. A stylesheet
	// carrying a closing style tag has nothing legitimate to offer anyway.
	if strings.Contains(strings.ToLower(cssText), "</style") {
		logging.Trace("ui: scope css rejected", "reason", "closes style tag", "where", "input")
		return "", 0
	}
	ss, err := parseStylesheet(cssText)
	if ss == nil || len(ss.Rules) == 0 {
		logging.Trace("ui: scope css parse failed", "err", err, "bytes", len(cssText))
		return "", 0
	}
	if err != nil {
		// Partial: the rules above the error are usable, the remainder is lost.
		logging.Trace("ui: scope css parsed partially", "err", err,
			"rules", len(ss.Rules), "bytes", len(cssText))
	}
	ss.Rules = dropMarkupRules(ss.Rules)
	scopeRules(ss.Rules, scopeSel)
	trackers := stripTrackerCSSRules(ss.Rules)
	out := ss.String()
	if strings.Contains(strings.ToLower(out), "</style") {
		logging.Trace("ui: scope css rejected", "reason", "closes style tag")
		return "", trackers // never let serialized CSS terminate the <style> tag early
	}
	logging.Trace("ui: scope css", "scope", scopeSel, "in_bytes", len(cssText), "out_bytes", len(out), "trackers", trackers)
	return out, trackers
}

// dropMarkupRules removes rules whose selector or at-rule prelude still holds
// markup rather than CSS. stripCSSMarkupScaffolding removes the forms we know;
// this is the backstop for the ones we don't, and it is what makes the scoped
// stylesheet safe to parse again: "<" cannot begin a selector, so a rule
// carrying one is markup that leaked into the stylesheet, and serializing it
// would put markup back into text a parser has to read (see
// stripCSSMarkupScaffolding for what that costs).
func dropMarkupRules(rules []*css.Rule) []*css.Rule {
	kept := rules[:0]
	for _, r := range rules {
		if strings.Contains(r.Prelude, "<") || strings.Contains(r.Name, "<") {
			logging.Trace("ui: css rule dropped", "reason", "markup in prelude", "prelude", logging.Body(r.Prelude))
			continue
		}
		markup := false
		for _, sel := range r.Selectors {
			if strings.Contains(sel, "<") {
				markup = true
				break
			}
		}
		if markup {
			logging.Trace("ui: css rule dropped", "reason", "markup in selector", "selectors", strings.Join(r.Selectors, ","))
			continue
		}
		r.Rules = dropMarkupRules(r.Rules)
		kept = append(kept, r)
	}
	return kept
}

func stripTrackerCSSRules(rules []*css.Rule) int {
	removed := 0
	for _, rule := range rules {
		removed += stripTrackerCSSRules(rule.Rules)
		concealed := false
		for _, decl := range rule.Declarations {
			if cssDeclarationConcealsResource(decl.Property, decl.Value) {
				concealed = true
				break
			}
		}
		for _, decl := range rule.Declarations {
			var n int
			decl.Value, n = stripTrackerCSSURLs(decl.Value, concealed)
			removed += n
		}
	}
	return removed
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
