package ui

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEmailPolicyPreservesStyling(t *testing.T) {
	p := emailPolicy()
	in := `<table width="600" bgcolor="#f4f4f4" cellpadding="0" align="center">` +
		`<tr><td style="padding:24px;color:#333">` +
		`<font face="Arial" size="4" color="#0a7">Sale</font>` +
		`<p style="font-size:14px">Hi <b>there</b></p>` +
		`</td></tr></table>`
	out := p.Sanitize(in)

	for _, want := range []string{
		`style="padding:24px;color:#333"`,
		`bgcolor="#f4f4f4"`,
		`cellpadding="0"`,
		`align="center"`,
		`<font`,
		`face="Arial"`,
		`style="font-size:14px"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("email styling stripped: missing %q in:\n%s", want, out)
		}
	}
}

// TestEmailPolicyKeepsClass: class is presentational and must survive so a
// message's scoped <style> can target its elements (the Checkly grid regression).
func TestEmailPolicyKeepsClass(t *testing.T) {
	out := emailPolicy().Sanitize(`<div class="bar grid">x</div>`)
	if !strings.Contains(out, `class="bar grid"`) {
		t.Errorf("class stripped: %s", out)
	}
}

// TestScopeCSS covers the Checkly-style regression: a newsletter whose layout
// lives in <style> classes collapsed because the sanitizer drops <style>. We
// re-add it scoped to the message wrapper; scopeCSS must prefix selectors, map
// page-level selectors to the wrapper, and keep @media blocks.
func TestScopeCSS(t *testing.T) {
	in := `body{margin:0} .bar{height:0;background:#aaa} ` +
		`.top .weekday .bar{margin-top:auto} ` +
		`@media (min-width:480px){.col{width:50%}}`
	out := scopeCSS(in, ".m1")

	for _, want := range []string{".m1 .bar", ".m1 .top .weekday .bar", ".m1 .col", "@media"} {
		if !strings.Contains(out, want) {
			t.Errorf("scoped CSS missing %q in:\n%s", want, out)
		}
	}
	// A bare `body` selector maps to the wrapper itself, never `.m1 body`.
	if strings.Contains(out, ".m1 body") {
		t.Errorf("body should map to the wrapper, not descend into it:\n%s", out)
	}
	// !important is stripped so an element's inline style still wins (an Outlook
	// hack like ".keep-white{color:#000!important}" must not override inline white).
	if strings.Contains(scopeCSS(`.keep-white{color:#000 !important}`, ".m1"), "important") {
		t.Errorf("!important should be stripped from re-injected email CSS")
	}
	// Unparseable / breakout CSS yields no styles rather than corrupting output.
	if got := scopeCSS(`x{}</style><script>`, ".m1"); got != "" {
		t.Errorf("breakout CSS should be rejected, got %q", got)
	}
}

func TestScopeEmailCSSStripsTrackingResources(t *testing.T) {
	in := `.hidden{display:none;background-image:url("https://img.example.com/hidden.png")}` +
		`.visible{background-image:url("https://img.example.com/hero.png")}` +
		`.tracker{background-image:url("https://img.example.com/beacon.gif?id=42")}`
	out, blocked := scopeEmailCSS(in, ".m1")
	if blocked != 2 {
		t.Fatalf("blocked = %d, want 2: %s", blocked, out)
	}
	for _, bad := range []string{"hidden.png", "beacon.gif"} {
		if strings.Contains(out, bad) {
			t.Fatalf("tracking resource %q survived: %s", bad, out)
		}
	}
	if !strings.Contains(out, "hero.png") {
		t.Fatalf("visible background was removed: %s", out)
	}
}

func TestEmailPolicyStripsDangerousContent(t *testing.T) {
	p := emailPolicy()
	in := `<p onclick="steal()" style="color:red">hi</p>` +
		`<script>evil()</script>` +
		`<a href="javascript:alert(1)">x</a>` +
		`<img src="x" onerror="boom()">`
	out := p.Sanitize(in)

	for _, bad := range []string{"onclick", "<script", "evil()", "javascript:", "onerror", "boom()"} {
		if strings.Contains(out, bad) {
			t.Errorf("dangerous content survived: %q in:\n%s", bad, out)
		}
	}
	// ...while the benign inline style on the same element is kept.
	if !strings.Contains(out, "color:red") {
		t.Errorf("benign style dropped: %s", out)
	}
}

// A <style> block that ends in an Outlook conditional comment — the tail of a
// great deal of transactional mail — used to reach douceur as a rule whose
// selector was the leftover marker. Serialized back into the scoped stylesheet,
// that rule sent douceur's own parser into an infinite loop when the reader read
// the stylesheet again for its image URLs: the render goroutine spun at 100% of
// a core forever, the conversation never appeared, and whatever the reader was
// already showing stayed on screen under the new subject.
//
// A regression hangs this test rather than failing it — which is the point: the
// hang is the bug.
func TestConditionalCommentInStyleDoesNotWedgeTheCSSParser(t *testing.T) {
	const body = `<html><head><style>
.wrap { color: #123456; }
#MessageViewBody{width: 100% !important;}<!--[if (gte mso 9)|(IE)]>li {text-indent: -1em;}<![endif]-->
</style></head><body><div class="wrap" style="background-image:url(https://example.com/bg.png)">hi</div></body></html>`

	w := &window{sanitizer: emailPolicy()}
	clean, _ := w.cleanHTML(body)
	if strings.Contains(clean, "endif") || strings.Contains(clean, "[if ") {
		t.Fatalf("conditional-comment marker survived into the document: %q", clean)
	}
	// The rules around the marker are ordinary CSS and must survive it.
	if !strings.Contains(clean, "#123456") {
		t.Fatalf("stylesheet lost its rules: %q", clean)
	}
	// Reading the scoped stylesheet back is where the loop used to happen.
	if _, stats, _ := w.resolveRemoteImages(clean, true); stats.Total == 0 {
		t.Fatal("no remote image found; the image pass did not read the document")
	}
}

func TestStripCSSMarkupScaffolding(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain css is untouched", ".a { color: red; }", ".a { color: red; }"},
		{
			"downlevel-hidden conditional",
			`.a{color:red}<!--[if (gte mso 9)|(IE)]>li {text-indent:-1em;}<![endif]-->`,
			`.a{color:red} li {text-indent:-1em;} `,
		},
		{
			"downlevel-revealed conditional keeps its css",
			`<!--[if !mso]><!-->.b{color:blue}<!--<![endif]-->`,
			`  .b{color:blue}  `,
		},
		{"bare cdo/cdc tokens", `<!--.c{color:green}-->`, ` .c{color:green} `},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripCSSMarkupScaffolding(tt.in); got != tt.want {
				t.Fatalf("stripCSSMarkupScaffolding(%q)\n  = %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

// The backstop for markup this strip doesn't know: a rule whose selector is
// markup is dropped rather than serialized back into text a parser must read.
func TestScopeEmailCSSDropsMarkupRules(t *testing.T) {
	out, _ := scopeEmailCSS(`.a{color:red}<x-bogus>{color:blue}`, ".mbx")
	if strings.Contains(out, "<") {
		t.Fatalf("markup serialized back into the scoped stylesheet: %q", out)
	}
	if !strings.Contains(out, "red") {
		t.Fatalf("valid rule dropped with the markup one: %q", out)
	}
}

// hangGuard runs fn and fails if it does not return promptly. The bugs it
// guards are non-terminating loops, so a plain call would hang the test binary
// until the package timeout instead of naming the input.
func hangGuard(t *testing.T, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: did not terminate", name)
	}
}

// TestScopeEmailCSSTerminates covers the input class that used to wedge the
// render goroutine at 100% of a core for the life of the process: a ";" where
// douceur's parser expects a selector, which parseQualifiedRule and parsePrelude
// hand back and forth without either consuming it. The email that surfaced it
// was a Ryanair itinerary whose <style> had been HTML-escaped by the sender's
// template engine, so every "&gt;" in a child selector left a ";" behind.
func TestScopeEmailCSSTerminates(t *testing.T) {
	cases := map[string]string{
		"escaped child selector":  `table.sm-12&gt;tbody{display:inline-block!important}`,
		"escaped, several":        `.sm-12,table.sm-12&gt;tbody,table.sm-12&gt;tbody&gt;tr{width:100%!important}`,
		"escaped ampersand":       `.a&amp;b{color:red}`,
		"escaped nbsp":            `.a&nbsp;b{color:red}`,
		"numeric character ref":   `.a&#62;b{color:red}`,
		"escaped inside at-rule":  `@media only screen and (max-width:600px){table.sm-12&gt;tbody{display:block}}`,
		"stray semicolon":         `.a{color:red};.b{color:blue}`,
		"leading semicolon":       `;.a{color:red}`,
		"semicolons only":         `;;;;`,
		"stray inside at-rule":    `@media screen{.a{color:red};.b{color:blue}}`,
		"stray inside keyframes":  `@keyframes k{;0%{opacity:0}}`,
		"serialized endif marker": `<![endif]-->;`,
		"empty at-rule body":      `@media screen{;}`,
	}
	for name, cssText := range cases {
		cssText := cssText
		hangGuard(t, name, func() { scopeCSS(cssText, ".mbx") })
	}
}

// TestStripPreludeSemicolonsKeepsRealOnes pins the other half of the guard: the
// semicolons CSS actually needs must survive, or the pass would quietly merge
// declarations and break every styled email instead of one.
func TestStripPreludeSemicolonsKeepsRealOnes(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"declarations", `.a{color:red;top:0}`, `.a{color:red;top:0}`},
		{"nested declarations", `@media screen{.a{color:red;top:0}}`, `@media screen{.a{color:red;top:0}}`},
		{"at-statement", `@import url(https://x/y);.a{color:red}`, `@import url(https://x/y);.a{color:red}`},
		{"mixed-case at-statement", `@IMPORT "a";.a{color:red}`, `@IMPORT "a";.a{color:red}`},
		{"data uri value", `.a{background:url(data:image/png;base64,AAA)}`, `.a{background:url(data:image/png;base64,AAA)}`},
		{"semicolon in string", `.a{content:";"}`, `.a{content:";"}`},
		{"semicolon in comment", `.a{color:red/* ; */}`, `.a{color:red/* ; */}`},
		{"escaped semicolon in selector", `.a\;b{color:red}`, `.a\;b{color:red}`},
		{"font-face declarations", `@font-face{font-family:x;src:url(https://x/f.woff)}`, `@font-face{font-family:x;src:url(https://x/f.woff)}`},
		// A comment ahead of an at-rule must not disguise it as a qualified
		// rule, or the ";" its block needs would be dropped — or worse, kept
		// where douceur spins.
		{"comment before at-rule", `/*c*/@media screen{.a{x:y;z:w}}`, `/*c*/@media screen{.a{x:y;z:w}}`},
		{"comment before stray", `/*c*/;.a{color:red}`, `/*c*/.a{color:red}`},
		{"utf-8 selector", `.café&gt;b{color:red}`, `.café&gtb{color:red}`},
		{"utf-8 preserved", `.café{content:"é"}`, `.café{content:"é"}`},
		{"stray dropped", `.a{color:red};.b{color:blue}`, `.a{color:red}.b{color:blue}`},
		{"escaped gt dropped", `table.a&gt;tbody{color:red}`, `table.a&gttbody{color:red}`},
	}
	for _, c := range cases {
		got, _ := stripPreludeSemicolons(c.in)
		if got != c.want {
			t.Errorf("%s: stripPreludeSemicolons(%q)\n got %q\nwant %q", c.name, c.in, got, c.want)
		}
	}
}

// TestScopeEmailStyleBlocksIsolatesBadBlock and the per-rule recovery below are
// the difference between a malformed stylesheet costing an email one rule and
// costing it every rule. Both shapes are from the same Ryanair itinerary.
func TestScopeEmailStyleBlocksIsolatesBadBlock(t *testing.T) {
	body := `<html><head>` +
		`<style>.keep-me{color:red}</style>` +
		`<style>.nowrap{white-space:nowrap @media (max-width:480px){white-space:wrap;}}</style>` +
		`<style>.keep-me-too{color:blue}</style>` +
		`</head><body>hi</body></html>`
	scoped, _ := scopeEmailStyleBlocks(body, ".mbx")
	for _, want := range []string{"keep-me", "keep-me-too"} {
		if !strings.Contains(scoped, want) {
			t.Errorf("one malformed <style> block cost the email %q: %s", want, scoped)
		}
	}
}

func TestParseStylesheetRecoversAroundBadRule(t *testing.T) {
	cssText := `.dark a{color:#2091eb}` +
		`.nowrap{white-space:nowrap @media (max-width:480px){white-space:wrap;}}` +
		`.receipt-table__right{font-weight:700;text-align:right}`
	scoped, _ := scopeEmailCSS(cssText, ".mbx")
	// The rules on both sides of the unparsable one must survive it.
	for _, want := range []string{".mbx .dark a", ".mbx .receipt-table__right"} {
		if !strings.Contains(scoped, want) {
			t.Errorf("lost %q to a neighbouring bad rule: %s", want, scoped)
		}
	}
}

func TestSplitTopLevelRules(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"two rules", `.a{color:red}.b{color:blue}`, []string{`.a{color:red}`, `.b{color:blue}`}},
		{"at-statement then rule", `@import "x";.a{color:red}`, []string{`@import "x";`, `.a{color:red}`}},
		{"nested block stays whole", `@media screen{.a{top:0}.b{top:1px}}.c{top:2px}`,
			[]string{`@media screen{.a{top:0}.b{top:1px}}`, `.c{top:2px}`}},
		{"braces in string", `.a{content:"}"}.b{top:0}`, []string{`.a{content:"}"}`, `.b{top:0}`}},
		{"unclosed block", `.a{color:red`, []string{`.a{color:red`}},
	}
	for _, c := range cases {
		got := splitTopLevelRules(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: splitTopLevelRules(%q)\n got %#v\nwant %#v", c.name, c.in, got, c.want)
		}
	}
}

// FuzzScopeEmailCSS is the standing defence: the guard above is complete for
// douceur's parser as it stands today, and this is what would catch the next
// non-terminating input — a dependency bump included — rather than a user
// opening an email and watching a core spin. Go's fuzzer reports an input that
// exceeds its per-input deadline, which is exactly the failure mode.
func FuzzScopeEmailCSS(f *testing.F) {
	for _, seed := range []string{
		`table.sm-12&gt;tbody{display:block}`, `.a{color:red};.b{color:blue}`, `;`,
		`<![endif]-->;`, `@media screen{;}`, `.a{background:url(data:image/png;base64,A;A)}`,
		`.a{content:";"}`, `.a\;b{color:red}`, `@import url(x);`, `.a{color:red/* ; */}`,
		`@keyframes k{0%{opacity:0}100%{opacity:1}}`, `.nowrap{a:b @media (x){c:d;}}`,
		`"`, `/*`, `.a{`, `}`, `@media`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, cssText string) {
		out, _ := scopeEmailCSS(cssText, ".mbx")
		// The output is written straight into a <style> element, so it must never
		// be able to close it.
		if strings.Contains(strings.ToLower(out), "</style") {
			t.Fatalf("scoped CSS can close its <style>: %q", out)
		}
	})
}
