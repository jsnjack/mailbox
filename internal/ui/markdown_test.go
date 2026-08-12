package ui

import "testing"

func TestMarkdownToPango(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"bold", "**Proposed change**: move it", "<b>Proposed change</b>: move it"},
		{"inline code", "reference the `Pod=ss.pod` file", "reference the <tt>Pod=ss.pod</tt> file"},
		{"italic", "this is *really* fine", "this is <i>really</i> fine"},
		{"bullet", "- first item", "•  first item"},
		{"asterisk bullet", "* first item", "•  first item"},
		{"already bulleted", "•  from an older stored summary", "•  from an older stored summary"},
		{"heading", "### Open issues", "<b>Open issues</b>"},
		{"bold inside a bullet", "- **Status**: done", "•  <b>Status</b>: done"},
		{"code wins over italic", "every `ss-*` quadlet", "every <tt>ss-*</tt> quadlet"},
		// Markup characters in the text are escaped, or Pango refuses the whole
		// string and the label renders empty.
		{"escapes", "R&D <notifications@github.com>", "R&amp;D &lt;notifications@github.com&gt;"},
		{"escapes inside bold", "**A & B**", "<b>A &amp; B</b>"},
		// Half-streamed text must stay literal rather than open a tag.
		{"unclosed bold", "**Proposed chan", "**Proposed chan"},
		{"unclosed code", "a `path/to", "a `path/to"},
		{"lone asterisk", "match ss-* only", "match ss-* only"},
		{"empty", "", ""},
		{"multiline", "- one\n- two", "•  one\n•  two"},
	}
	for _, c := range cases {
		if got := markdownToPango(c.in); got != c.want {
			t.Errorf("%s: markdownToPango(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestMarkdownToHTML(t *testing.T) {
	if got, want := markdownToHTML("**Kirill** closed `#7891`"),
		"<b>Kirill</b> closed <code>#7891</code>"; got != want {
		t.Errorf("markdownToHTML = %q, want %q", got, want)
	}
	if got, want := markdownToHTML("one\ntwo"), "one<br>two"; got != want {
		t.Errorf("line break = %q, want %q", got, want)
	}
	// The gist is inserted with innerHTML, so anything the model wrote has to
	// arrive escaped.
	if got := markdownToHTML(`<img src=x onerror=alert(1)>`); got != "&lt;img src=x onerror=alert(1)&gt;" {
		t.Errorf("unescaped markup survived: %q", got)
	}
}
