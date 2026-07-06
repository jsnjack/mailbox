package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestStripTrailingSignoff(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			"best regards + name",
			"Thank you for processing this. I accept the code.\n\nBest regards,\nYauhen",
			"Thank you for processing this. I accept the code.",
		},
		{
			"salutation + name + title",
			"Sounds good, see you then.\n\nKind regards,\nYauhen Shulitski\nSoftware Engineer",
			"Sounds good, see you then.",
		},
		{
			"bare thanks, no name",
			"Got it, will do.\n\nThanks!",
			"Got it, will do.",
		},
		{
			"no sign-off is left untouched",
			"Here are the details you asked for. Let me know if that works.",
			"Here are the details you asked for. Let me know if that works.",
		},
		{
			"thanks mid-sentence is not a sign-off",
			"Thank you for the quick turnaround on this.\n\nThe report looks complete.",
			"Thank you for the quick turnaround on this.\n\nThe report looks complete.",
		},
		{
			"closing-word as sentence start is not stripped",
			"Best of luck with the launch — let me know how it goes.",
			"Best of luck with the launch — let me know how it goes.",
		},
		{
			"sign-off deep in body (>4 lines from end) is left alone",
			"Best,\nMe\n\nActually, one more thing:\nline a\nline b\nline c",
			"Best,\nMe\n\nActually, one more thing:\nline a\nline b\nline c",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripTrailingSignoff(tc.in); got != tc.want {
				t.Errorf("stripTrailingSignoff:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestEditableBoundary(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // the editable prefix (body[:boundary])
	}{
		{"no markers", "Hello there, how are you?", "Hello there, how are you?"},
		{
			// A bare "-- " (e.g. a user types it in their own signature) still marks
			// the boundary defensively.
			"explicit delimiter then quote",
			"Hi Sam,\n\nThanks!\n\n-- \nYauhen\n\nOn Jan 2, 2026, X wrote:\n> old\n",
			"Hi Sam,\n\nThanks!",
		},
		{
			// The plain sign-off composeBodyWithSignature now inserts has no
			// delimiter, so it stays in the editable region; only the quote is
			// preserved.
			"plain sign-off then quote",
			"Thanks!\n\nBest,\nYauhen\n\nOn Jan 2, 2026, X wrote:\n> old\n",
			"Thanks!\n\nBest,\nYauhen",
		},
		{
			"quote, no signature",
			"My reply here.\n\nOn Jan 2, 2026, X wrote:\n> quoted\n> more\n",
			"My reply here.",
		},
		{
			"bare quoted lines",
			"See below.\n> quoted bit\n",
			"See below.",
		},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.body[:editableBoundary(tt.body)]
			if got != tt.want {
				t.Fatalf("editable prefix = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseMailto(t *testing.T) {
	tests := []struct {
		name                       string
		uri                        string
		ok                         bool
		to, cc, bcc, subject, body string
	}{
		{"not mailto", "https://example.com", false, "", "", "", "", ""},
		{"bare address", "mailto:alice@example.com", true, "alice@example.com", "", "", "", ""},
		{
			"subject and body",
			"mailto:alice@example.com?subject=Hi%20there&body=Line%20one",
			true, "alice@example.com", "", "", "Hi there", "Line one",
		},
		{
			"multiple recipients plus cc/bcc",
			"mailto:a@x.com,b@y.com?cc=c@z.com&bcc=d@w.com",
			true, "a@x.com, b@y.com", "c@z.com", "d@w.com", "", "",
		},
		{
			"percent-encoded address",
			"mailto:bob%40example.com?subject=Re%3A%20hi",
			true, "bob@example.com", "", "", "Re: hi", "",
		},
		{
			"empty recipient with subject",
			"mailto:?subject=No%20one",
			true, "", "", "", "No one", "",
		},
		{"uppercase scheme", "MAILTO:eve@example.com", true, "eve@example.com", "", "", "", ""},
		{
			// RFC 6068 uses percent-encoding only: "+" is a literal plus, so a
			// plus-addressed recipient must survive intact.
			"plus-addressed recipient",
			"mailto:user+news@example.com",
			true, "user+news@example.com", "", "", "", "",
		},
		{
			// "+" in query values (cc addresses, subject, body) is literal too —
			// url.Values-style decoding would corrupt these into spaces.
			"plus preserved in query values",
			"mailto:a@x.com?cc=user+tag@example.com&subject=1+1%3D2&body=x+y%20z",
			true, "a@x.com", "user+tag@example.com", "", "1+1=2", "x+y z",
		},
		{
			"percent-encoded plus recipient",
			"mailto:user%2Bnews@example.com?to=other+one@y.com",
			true, "user+news@example.com, other+one@y.com", "", "", "", "",
		},
		{
			// Header names are case-insensitive per RFC 6068.
			"case-insensitive header names",
			"mailto:a@x.com?Subject=Hi&CC=b@y.com",
			true, "a@x.com", "b@y.com", "", "Hi", "",
		},
		{
			// GIO normalises a command-line mailto into this hierarchical form; the
			// recipient is in the path, not the opaque part.
			"gio-normalised triple-slash form",
			"mailto:///alice@example.com?subject=Hi&cc=bob@example.com",
			true, "alice@example.com", "bob@example.com", "", "Hi", "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, ok := parseMailto(tt.uri)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if msg.To != tt.to || msg.Cc != tt.cc || msg.Bcc != tt.bcc ||
				msg.Subject != tt.subject || msg.Body != tt.body {
				t.Fatalf("parseMailto(%q) =\n  To=%q Cc=%q Bcc=%q Subject=%q Body=%q\nwant\n  To=%q Cc=%q Bcc=%q Subject=%q Body=%q",
					tt.uri, msg.To, msg.Cc, msg.Bcc, msg.Subject, msg.Body, tt.to, tt.cc, tt.bcc, tt.subject, tt.body)
			}
		})
	}
}

func TestMentionsAttachment(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"plain mention", "Hi, please find the report attached.", true},
		{"attachment word", "See the attachment for details.", true},
		{"enclosed", "The invoice is enclosed.", true},
		{"none", "Thanks, talk soon!", false},
		{"only in quote", "Sure.\n\n> Please find attached the file", false},
		{"mention outside quote wins", "Here it is, attached.\n\n> earlier text", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mentionsAttachment(tt.body); got != tt.want {
				t.Fatalf("mentionsAttachment(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestComposeBodyWithSignature(t *testing.T) {
	if got := composeBodyWithSignature("", ""); got != "" {
		t.Fatalf("no sig, empty body = %q", got)
	}
	if got := composeBodyWithSignature("quote", ""); got != "quote" {
		t.Fatalf("no sig keeps quote = %q", got)
	}
	if got := composeBodyWithSignature("", "Best,\nYauhen"); got != "\n\nBest,\nYauhen" {
		t.Fatalf("new message sig = %q", got)
	}
	if got := composeBodyWithSignature("> quoted", "Best,\nYauhen"); got != "\n\nBest,\nYauhen\n\n> quoted" {
		t.Fatalf("reply sig placement = %q", got)
	}
}

func TestWithOwnAccounts(t *testing.T) {
	w := &window{
		deps: Deps{Accounts: []AccountInfo{
			{ID: 1, Email: "me@gmail.com"},
			{ID: 2, Email: "me@work.com"},
		}},
		accountNames: map[string]string{"me@work.com": "Work"},
	}
	past := []model.Contact{
		{Address: "friend@x.com", Name: "Friend"},
		{Address: "ME@work.com"}, // same as account 2 (case-insensitive) → deduped
	}
	got := w.withOwnAccounts(past)

	if len(got) != 3 {
		t.Fatalf("got %d contacts, want 3: %+v", len(got), got)
	}
	// Own accounts come first, in registration order.
	if got[0].Address != "me@gmail.com" || got[1].Address != "me@work.com" {
		t.Fatalf("own accounts not first: %+v", got)
	}
	// The assigned display name is used.
	if got[1].Name != "Work" {
		t.Errorf("account 2 name = %q, want Work", got[1].Name)
	}
	// The past correspondent survives; the dup of an own account does not.
	if got[2].Address != "friend@x.com" {
		t.Errorf("third = %q, want friend@x.com", got[2].Address)
	}
}

func TestForwardOriginal(t *testing.T) {
	m := model.Message{
		FromName:     "Alice",
		FromAddr:     "alice@example.com",
		Subject:      "News",
		ToAddrs:      "bob@example.com",
		InternalDate: time.Date(2026, 7, 6, 9, 36, 0, 0, time.UTC),
	}
	got := forwardOriginal(m, "line one\nline two")
	for _, want := range []string{
		forwardMarker,
		"From: Alice <alice@example.com>",
		"Date: Mon, Jul 6, 2026 at 09:36",
		"Subject: News",
		"To: bob@example.com",
		"\n\nline one\nline two\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "> line one") {
		t.Fatal("forwarded body must not be quote-prefixed")
	}
}

func TestQuoteOriginalAttribution(t *testing.T) {
	m := model.Message{
		FromName:     "Alice",
		FromAddr:     "alice@example.com",
		InternalDate: time.Date(2026, 7, 6, 9, 36, 0, 0, time.UTC),
	}
	got := quoteOriginal(m, "hello")
	if !strings.Contains(got, "On Mon, Jul 6, 2026 at 09:36, Alice <alice@example.com> wrote:\n> hello\n") {
		t.Fatalf("unexpected quote:\n%s", got)
	}
	// The compose splitter must still recognize the attribution.
	if quoteBoundary("reply\n"+got) >= len("reply\n"+got) {
		t.Fatal("quoteBoundary did not find the attribution")
	}
}

func TestQuoteBoundaryForward(t *testing.T) {
	body := "my note\n\n" + forwardMarker + "\nFrom: X <x@y>\n\nbody"
	qb := quoteBoundary(body)
	if !strings.HasPrefix(body[qb:], forwardMarker) {
		t.Fatalf("boundary at %d, want start of forward marker; got %q", qb, body[qb:])
	}
	if eb := editableBoundary(body); eb > qb {
		t.Fatalf("editableBoundary %d should not exceed the forward marker at %d", eb, qb)
	}
}

func TestBuildHTMLBody(t *testing.T) {
	m := model.Message{FromName: "Alice", FromAddr: "a@x.com", InternalDate: time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)}
	quote := quoteOriginal(m, "original text")
	quoteHTML := "<div><b>original</b> text</div>"

	t.Run("unedited quote embeds original HTML", func(t *testing.T) {
		body := "my reply\n" + quote
		got := buildHTMLBody(body, quote, quoteHTML)
		if !strings.Contains(got, "<blockquote") || !strings.Contains(got, quoteHTML) {
			t.Fatalf("expected blockquoted original HTML:\n%s", got)
		}
		if !strings.Contains(got, "my reply") || !strings.Contains(got, "wrote:") {
			t.Fatalf("missing user text or attribution:\n%s", got)
		}
		if strings.Contains(got, "&gt; original text") {
			t.Fatal("plain quote lines leaked into the rich-quote rendering")
		}
	})
	t.Run("edited quote falls back to text rendering", func(t *testing.T) {
		body := "my reply\n" + strings.Replace(quote, "original", "edited", 1)
		got := buildHTMLBody(body, quote, quoteHTML)
		if strings.Contains(got, "<blockquote") {
			t.Fatalf("edited quote must not use the original HTML:\n%s", got)
		}
		if !strings.Contains(got, "&gt; edited text") {
			t.Fatalf("expected escaped text quote:\n%s", got)
		}
	})
	t.Run("no quote renders whole body", func(t *testing.T) {
		got := buildHTMLBody("just text\nwith https://example.com link", "", "")
		if !strings.Contains(got, `<a href="https://example.com">`) {
			t.Fatalf("expected linkified body:\n%s", got)
		}
		if !strings.Contains(got, "<br>") {
			t.Fatalf("expected <br> line breaks:\n%s", got)
		}
	})
	t.Run("forward embeds original HTML unquoted", func(t *testing.T) {
		fwd := forwardOriginal(m, "original text")
		body := "FYI\n" + fwd
		got := buildHTMLBody(body, fwd, quoteHTML)
		if !strings.Contains(got, quoteHTML) {
			t.Fatalf("expected original HTML:\n%s", got)
		}
		if strings.Contains(got, "<blockquote") {
			t.Fatalf("forward must not blockquote:\n%s", got)
		}
		if !strings.Contains(got, "Forwarded message") {
			t.Fatalf("missing forwarded header block:\n%s", got)
		}
	})
}

func TestHTMLToTextPreservesLinks(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"link with text", `<p>see <a href="https://example.com/x">the docs</a> now</p>`, "see the docs (https://example.com/x) now"},
		{"link text is url", `<a href="https://example.com">https://example.com</a>`, "https://example.com"},
		{"non-http scheme dropped", `<a href="mailto:x@y">mail me</a>`, "mail me"},
		{"blocks become newlines", "<div>one</div><div>two</div>", "one\ntwo"},
		{"style stripped", "<style>.x{color:red}</style><p>body</p>", "body"},
		{"plain text unharmed", "already plain\ntext", "already plain\ntext"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := htmlToText(tc.in); got != tc.want {
				t.Fatalf("htmlToText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestUndoTitle(t *testing.T) {
	one := []model.Message{{ThreadID: "t1", Subject: "Quarterly report and a very long subject line that should be truncated nicely"}}
	if got := undoTitle("Archived", one); !strings.HasPrefix(got, "Archived “Quarterly report") || !strings.HasSuffix(got, "…”") {
		t.Fatalf("single: %q", got)
	}
	burst := []model.Message{{ThreadID: "t1", Subject: "a"}, {ThreadID: "t2", Subject: "b"}, {ThreadID: "t2", Subject: "b2"}, {ThreadID: "t3", Subject: "c"}}
	if got := undoTitle("Archived", burst); got != "Archived 3 conversations" {
		t.Fatalf("burst: %q", got)
	}
	if got := undoTitle("Archived", []model.Message{{ThreadID: "t1"}}); got != "Archived" {
		t.Fatalf("no subject: %q", got)
	}
}
