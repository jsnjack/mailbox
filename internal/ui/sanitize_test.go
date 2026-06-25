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
