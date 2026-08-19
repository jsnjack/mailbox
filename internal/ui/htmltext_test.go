package ui

import (
	"fmt"
	"strings"
	"sync"
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

// Trackers that dodge integer-pixel detection with decimals or relative units
// must still be stripped (previously strconv.Atoi failed and they loaded).
func TestCleanEmailHTMLStripsEvasiveTrackers(t *testing.T) {
	cases := []string{
		`<img src="https://t1.example.com/a.gif" width="1.0" height="1.0">`,         // decimal attrs
		`<img src="https://t2.example.com/b.gif" style="width:0.1em;height:0.1em">`, // relative units
		`<img src="https://t3.example.com/c.gif" style="width: 1px; height: 1px">`,  // spaced style
	}
	for _, in := range cases {
		out, blocked := cleanEmailHTML(in)
		if blocked != 1 || strings.Contains(out, ".gif") {
			t.Fatalf("evasive tracker survived (blocked=%d): %s", blocked, out)
		}
	}
	// A genuinely large image with a decimal size must NOT be treated as a tracker.
	big := `<img src="https://cdn.example.com/hero.png" width="600.0" height="200.0">`
	if out, n := cleanEmailHTML(big); n != 0 || !strings.Contains(out, "hero.png") {
		t.Fatalf("large decimal-sized image wrongly stripped (n=%d): %s", n, out)
	}
}

func TestCleanEmailHTMLStripsConcealedRemoteResources(t *testing.T) {
	tests := []struct {
		name string
		html string
	}{
		{name: "one declared tiny dimension", html: `<img src="https://img.example.com/a.png" width="1">`},
		{name: "hidden ancestor", html: `<div style="display:none"><img src="https://img.example.com/b.png" width="600" height="400"></div>`},
		{name: "hidden background", html: `<div hidden style="background-image:url(https://img.example.com/c.png)"></div>`},
		{name: "tracking background endpoint", html: `<div style="background:url(https://img.example.com/beacon.gif?id=42)"></div>`},
		{name: "open event query", html: `<img src="https://img.example.com/image.png?event=open&id=42">`},
		{name: "tracking srcset", html: `<img srcset="https://img.example.com/pixel.gif?id=42 1x">`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, blocked := cleanEmailHTML(tt.html)
			if blocked != 1 || strings.Contains(out, "https://") {
				t.Fatalf("blocked = %d, output = %s", blocked, out)
			}
		})
	}
}

func TestCleanEmailHTMLKeepsVisibleRemoteArtwork(t *testing.T) {
	in := `<img src="https://img.example.com/art/pixel-art.png?size=large" width="600">` +
		`<div style="background:url(https://img.example.com/hero.png)">Hello</div>`
	out, blocked := cleanEmailHTML(in)
	if blocked != 0 || out != in {
		t.Fatalf("visible artwork changed (blocked=%d): %s", blocked, out)
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

// A plain-text body is rendered as one <pre>, so the whole email is a single
// text node. It must be offered one segment per paragraph: handed the lot as one
// snippet, a model answers with one element per paragraph, which used to leave
// only the greeting translated and drop the rest of the email.
func TestTranslateHTMLTextSplitsPreByParagraph(t *testing.T) {
	in := "<pre style=\"white-space:pre-wrap\">Hi Yauhen,\r\n\r\nJe stelt elk kwartaal doelen.\r\n\r\nBeste groeten,\r\nJurre\r\n</pre>"

	var got []string
	out, err := translateHTMLText(in, func(segs []string) ([]string, error) {
		got = segs
		return []string{"Hi Yauhen,", "You set goals every quarter.", "Best regards,\r\nJurre"}, nil
	})
	if err != nil {
		t.Fatalf("translateHTMLText: %v", err)
	}
	// The HTML tokenizer normalizes CRLF to LF, so the segments arrive with \n.
	want := []string{"Hi Yauhen,", "Je stelt elk kwartaal doelen.", "Beste groeten,\nJurre"}
	if len(got) != len(want) {
		t.Fatalf("got %d segments, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("segment %d = %q, want %q", i, got[i], want[i])
		}
	}
	// Every paragraph is translated, and the blank lines between them survive.
	for _, frag := range []string{"Hi Yauhen,", "You set goals every quarter.", "Best regards,", "Jurre", "\n\n"} {
		if !strings.Contains(out, frag) {
			t.Fatalf("output missing %q:\n%s", frag, out)
		}
	}
	if strings.Contains(out, "Je stelt") || strings.Contains(out, "Beste groeten") {
		t.Fatalf("untranslated text left behind:\n%s", out)
	}
}

// In flowed HTML a blank line is insignificant whitespace inside a paragraph, so
// splitting there would hand the translator half a sentence.
func TestTranslateHTMLTextKeepsFlowedParagraphWhole(t *testing.T) {
	in := "<p>Je stelt elk kwartaal doelen,\n\nen aan het eind blijkt de helft blijven liggen.</p>"
	var got []string
	if _, err := translateHTMLText(in, func(segs []string) ([]string, error) {
		got = segs
		return segs, nil
	}); err != nil {
		t.Fatalf("translateHTMLText: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d segments, want 1: %q", len(got), got)
	}
}

// A model that splits our single snippet into its paragraphs must not cost us
// everything past the first: the parts belong to that one snippet and re-join.
func TestTranslateHTMLTextRejoinsSplitReply(t *testing.T) {
	in := "<pre>Hi Yauhen,\r\n\r\nJe stelt doelen.\r\n\r\nBeste groeten,\r\n</pre>"
	out, err := translateHTMLText(in, func(segs []string) ([]string, error) {
		if len(segs) != 3 { // the paragraph split asks for three
			return nil, nil
		}
		// Answer the first snippet with two elements, as an over-eager model does.
		return []string{"Hi Yauhen,", "extra", "You set goals.", "Best regards,"}, nil
	})
	if err == nil {
		t.Fatalf("expected a mismatch error for 4 elements over 3 segments, got:\n%s", out)
	}

	// With a single segment the same reply is recoverable.
	out, err = translateHTMLText("<pre>Hallo daar.</pre>", func(segs []string) ([]string, error) {
		return []string{"Hello", "there."}, nil
	})
	if err != nil {
		t.Fatalf("translateHTMLText: %v", err)
	}
	if !strings.Contains(out, "Hello there.") {
		t.Fatalf("split reply not rejoined: %s", out)
	}
	if strings.Contains(out, "Hallo") {
		t.Fatalf("original kept instead of the translation: %s", out)
	}
}

// A conversation's messages quote each other, so pooling must ask for a repeated
// paragraph once and reuse the answer everywhere it appears.
func TestPoolTranslationsDedupes(t *testing.T) {
	shared := "<p>Beste groeten,</p>"
	newer := "<p>Bedankt voor je bericht.</p>" + shared
	older := "<p>Ik help MT's dat zichtbaar te maken.</p>" + shared

	var plans []*translationPlan
	for _, body := range []string{newer, older} {
		plan, err := planTranslation(body)
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, plan)
	}

	var asked []string
	byText, err := poolTranslations(plans, func(segs []string) ([]string, error) {
		asked = append(asked, segs...)
		out := make([]string, len(segs))
		for i, s := range segs {
			out[i] = "EN:" + s
		}
		return out, nil
	})
	if err != nil {
		t.Fatalf("poolTranslations: %v", err)
	}
	// Four segments across the two bodies, three of them distinct.
	if len(asked) != 3 {
		t.Fatalf("asked for %d segments, want 3 (deduped): %q", len(asked), asked)
	}
	seen := map[string]int{}
	for _, s := range asked {
		seen[s]++
		if seen[s] > 1 {
			t.Fatalf("segment asked twice: %q", s)
		}
	}
	// Both bodies render fully from the shared pool.
	for i, plan := range plans {
		out, err := plan.render(byText)
		if err != nil {
			t.Fatalf("render %d: %v", i, err)
		}
		if !strings.Contains(out, "EN:Beste groeten,") {
			t.Fatalf("body %d missing the pooled translation: %s", i, out)
		}
	}
}

// Requests stay bounded, so a big conversation doesn't become one slow request.
func TestPoolTranslationsBatchesRequests(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 95; i++ {
		fmt.Fprintf(&body, "<p>paragraaf %d</p>", i)
	}
	plan, err := planTranslation(body.String())
	if err != nil {
		t.Fatal(err)
	}
	var sizes []int
	var mu sync.Mutex
	byText, err := poolTranslations([]*translationPlan{plan}, func(segs []string) ([]string, error) {
		mu.Lock()
		sizes = append(sizes, len(segs))
		mu.Unlock()
		return segs, nil
	})
	if err != nil {
		t.Fatalf("poolTranslations: %v", err)
	}
	total := 0
	for _, n := range sizes {
		if n > translateBatch {
			t.Fatalf("batch of %d exceeds translateBatch %d", n, translateBatch)
		}
		total += n
	}
	if total != 95 || len(byText) != 95 {
		t.Fatalf("covered %d segments in %v batches, byText=%d, want 95", total, sizes, len(byText))
	}
}

// A segment the translator skipped keeps its own original text, and nothing else
// moves — the failure mode the keyed protocol exists to give us.
func TestRenderGapKeepsOnlyThatSegment(t *testing.T) {
	plan, err := planTranslation("<p>een</p><p>twee</p><p>drie</p>")
	if err != nil {
		t.Fatal(err)
	}
	byText, err := zipTranslations(plan.segments, []string{"one", "", "three"})
	if err != nil {
		t.Fatalf("zipTranslations: %v", err)
	}
	out, err := plan.render(byText)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<p>one</p>", "<p>twee</p>", "<p>three</p>"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}
