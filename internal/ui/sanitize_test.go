package ui

import (
	"strings"
	"testing"
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
