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

// TestInlineEmailCSSSurvivesSanitize covers the Checkly-style regression: a
// newsletter whose layout lives in <style> classes (not inline) collapsed to
// nothing because the sanitizer strips <style>. Inlining first moves those
// declarations onto the elements, where the sanitizer keeps the safe ones.
func TestInlineEmailCSSSurvivesSanitize(t *testing.T) {
	in := `<html><head><style>` +
		`.grid{display:flex;gap:8px}` +
		`.dot{display:inline-block;width:16px;height:16px;background:#fbbf24}` +
		`</style></head><body>` +
		`<div class="grid"><span class="dot">a</span><span class="dot">b</span></div>` +
		`</body></html>`

	// Without inlining, the class-based layout is lost (sanitizer drops <style>).
	bare := emailPolicy().Sanitize(in)
	if strings.Contains(bare, "display:flex") {
		t.Fatalf("precondition wrong: <style> survived bare sanitize:\n%s", bare)
	}

	// With inlining, the layout-critical declarations land on the elements and
	// survive sanitization. (Normalize spacing — the inliner emits "prop: value".)
	out := strings.ReplaceAll(emailPolicy().Sanitize(inlineEmailCSS(in)), " ", "")
	for _, want := range []string{"display:flex", "display:inline-block", "width:16px", "#fbbf24"} {
		if !strings.Contains(out, want) {
			t.Errorf("inlined style missing %q in:\n%s", want, out)
		}
	}
	// The raw <style> block itself is still gone (only inlined attrs remain).
	if strings.Contains(out, "<style") {
		t.Errorf("<style> block should not survive sanitize:\n%s", out)
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
