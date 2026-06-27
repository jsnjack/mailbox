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

func TestCleanEmailHTMLStripsTrackers(t *testing.T) {
	in := `<p>Hi</p>` +
		`<img src="https://cdn.example.com/logo.png" width="200" height="50">` + // legit
		`<img src="https://t.example.com/o.gif" width="1" height="1">` + // 1x1 pixel
		`<img src="https://esp.example.com/wf/open?u=123">` + // tracker pattern
		`<img src="https://x.example.com/p.gif" style="width:1px;height:1px">` // styled pixel
	out, blocked := cleanEmailHTML(in)

	if !strings.Contains(out, "logo.png") {
		t.Fatalf("legit image was removed: %s", out)
	}
	for _, bad := range []string{"o.gif", "/wf/open", "p.gif"} {
		if strings.Contains(out, bad) {
			t.Fatalf("tracker %q survived: %s", bad, out)
		}
	}
	if blocked != 3 {
		t.Fatalf("blocked count = %d, want 3", blocked)
	}
	// No trackers and no quotes → returned unchanged, zero count.
	clean := `<p>Just text and a <img src="a.png" width="100" height="100"></p>`
	if got, n := cleanEmailHTML(clean); got != clean || n != 0 {
		t.Fatalf("clean HTML changed: %q (n=%d)", got, n)
	}
}

func TestCleanEmailHTMLCollapsesQuotes(t *testing.T) {
	// A blockquote is wrapped in a <details> with a summary; its content survives.
	in := `<p>My reply.</p><blockquote>On Mon, X wrote: original text</blockquote>`
	out, _ := cleanEmailHTML(in)
	for _, frag := range []string{"<details>", "<summary", "Show quoted text", "original text", "My reply."} {
		if !strings.Contains(out, frag) {
			t.Fatalf("output missing %q:\n%s", frag, out)
		}
	}

	// No blockquote and no tracker → unchanged.
	plain := `<p>Just a note, no quote.</p>`
	if got, n := cleanEmailHTML(plain); got != plain || n != 0 {
		t.Fatalf("plain HTML changed: %q (n=%d)", got, n)
	}

	// A nested blockquote is not double-wrapped (only one <details>).
	nested := `<blockquote>outer <blockquote>inner</blockquote></blockquote>`
	if out, _ := cleanEmailHTML(nested); strings.Count(out, "<details>") != 1 {
		t.Fatalf("nested blockquote produced %d <details>, want 1", strings.Count(out, "<details>"))
	}
}

func TestCleanEmailHTMLStripsAndCollapsesTogether(t *testing.T) {
	// Both passes apply in one go: the tracker inside the quote is removed and the
	// quote is collapsed.
	in := `<p>Reply</p><blockquote>old <img src="https://t.example.com/o.gif" width="1" height="1"> text</blockquote>`
	out, blocked := cleanEmailHTML(in)
	if blocked != 1 {
		t.Fatalf("blocked = %d, want 1", blocked)
	}
	if strings.Contains(out, "o.gif") {
		t.Fatalf("tracker survived: %s", out)
	}
	if !strings.Contains(out, "<details>") || !strings.Contains(out, "old") {
		t.Fatalf("quote not collapsed or content lost: %s", out)
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
