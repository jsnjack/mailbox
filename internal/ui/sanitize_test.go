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
	// Unparseable / breakout CSS yields no styles rather than corrupting output.
	if got := scopeCSS(`x{}</style><script>`, ".m1"); got != "" {
		t.Errorf("breakout CSS should be rejected, got %q", got)
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
