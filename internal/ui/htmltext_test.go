package ui

import (
	"strings"
	"testing"
)

func TestTranslateHTMLTextPreservesMarkup(t *testing.T) {
	in := `<table width="600" bgcolor="#fff"><tr><td style="color:red">Hello <b>Bob</b></td></tr></table>` +
		`<a href="https://x.com" style="color:blue">Visit</a><script>evil()</script>`

	var got []string
	out, err := translateHTMLText(in, func(segs []string) ([]string, error) {
		got = segs
		// Pretend to translate by upper-casing each segment.
		tr := make([]string, len(segs))
		for i, s := range segs {
			tr[i] = strings.ToUpper(s)
		}
		return tr, nil
	})
	if err != nil {
		t.Fatalf("translateHTMLText: %v", err)
	}

	// Only visible text was offered for translation (not tags, not script).
	want := map[string]bool{"Hello": true, "Bob": true, "Visit": true}
	for _, s := range got {
		if !want[s] {
			t.Fatalf("unexpected segment %q (got all: %v)", s, got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("got %d segments, want 3: %v", len(got), got)
	}

	// Markup (attributes, styles, structure) is preserved; text is translated.
	for _, frag := range []string{`width="600"`, `bgcolor="#fff"`, `style="color:red"`, `href="https://x.com"`, "<b>", "HELLO", "BOB", "VISIT"} {
		if !strings.Contains(out, frag) {
			t.Fatalf("output missing %q:\n%s", frag, out)
		}
	}
	// Script text is never offered for translation (so it stays verbatim, not
	// upper-cased); the caller's sanitizer is what removes the <script> itself.
	if strings.Contains(out, "EVIL()") {
		t.Fatalf("script content should not be translated: %s", out)
	}
}

func TestTranslateHTMLTextLengthMismatchKeepsOriginal(t *testing.T) {
	in := `<p>One</p><p>Two</p>`
	out, err := translateHTMLText(in, func(segs []string) ([]string, error) {
		return []string{"Uno"}, nil // fewer than the 2 segments
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "Uno") || !strings.Contains(out, "Two") {
		t.Fatalf("expected first translated, second kept: %s", out)
	}
}
