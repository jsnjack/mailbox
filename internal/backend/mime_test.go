package backend

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/mail"
	"strings"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestBuildMIME(t *testing.T) {
	raw, err := BuildMIME(model.OutgoingMessage{
		From:       "me@example.com",
		To:         "you@example.com",
		Cc:         "cc@example.com",
		Subject:    "Re: Lunch",
		Body:       "Sounds good.\nSee you then.",
		InReplyTo:  "<orig@mail.gmail.com>",
		References: "<a@x> <orig@mail.gmail.com>",
	})
	if err != nil {
		t.Fatalf("BuildMIME: %v", err)
	}
	s := string(raw)

	for _, want := range []string{
		"From: me@example.com\r\n",
		"To: you@example.com\r\n",
		"Cc: cc@example.com\r\n",
		"Subject: Re: Lunch\r\n",
		"In-Reply-To: <orig@mail.gmail.com>\r\n",
		"References: <a@x> <orig@mail.gmail.com>\r\n",
		"Content-Type: text/plain; charset=\"utf-8\"\r\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing header %q", want)
		}
	}

	// Headers and body are separated by a blank line; body uses CRLF.
	if !strings.Contains(s, "\r\n\r\nSounds good.\r\nSee you then.") {
		t.Errorf("body not found after header separator:\n%s", s)
	}
	if !strings.Contains(s, "Message-ID: <") || !strings.Contains(s, "@example.com>") {
		t.Errorf("message-id not derived from sender domain:\n%s", s)
	}
}

func TestBuildMIMEAllowsEmptyRecipient(t *testing.T) {
	// Drafts may have no recipient yet; BuildMIME must not require one (the
	// recipient check lives in the send path).
	raw, err := BuildMIME(model.OutgoingMessage{From: "me@example.com", Subject: "draft"})
	if err != nil {
		t.Fatalf("BuildMIME with empty To: %v", err)
	}
	if strings.Contains(string(raw), "\r\nTo:") {
		t.Errorf("did not expect a To header:\n%s", raw)
	}
}

func TestBuildMIMEWithAttachment(t *testing.T) {
	raw, err := BuildMIME(model.OutgoingMessage{
		From:    "me@example.com",
		To:      "you@example.com",
		Subject: "Files",
		Body:    "see attached",
		Attachments: []model.OutgoingAttachment{
			{Filename: "a.txt", MimeType: "text/plain", Data: []byte("hello world")},
		},
	})
	if err != nil {
		t.Fatalf("BuildMIME: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, "Content-Type: multipart/mixed; boundary=") {
		t.Errorf("missing multipart content type:\n%s", s)
	}
	if !strings.Contains(s, `Content-Disposition: attachment; filename="a.txt"`) {
		t.Errorf("missing attachment disposition:\n%s", s)
	}
	if !strings.Contains(s, "Content-Transfer-Encoding: base64") {
		t.Errorf("attachment not base64 encoded:\n%s", s)
	}
	// "hello world" base64 is "aGVsbG8gd29ybGQ=".
	if !strings.Contains(s, "aGVsbG8gd29ybGQ=") {
		t.Errorf("attachment bytes not present:\n%s", s)
	}
}

// An iTIP RSVP goes out with the calendar payload as an inline
// text/calendar; method=REPLY body part inside multipart/alternative — the
// only shape Exchange/Google auto-process — alongside the .ics attachment.
func TestBuildMIMEWithCalendarPart(t *testing.T) {
	ics := []byte("BEGIN:VCALENDAR\r\nMETHOD:REPLY\r\nEND:VCALENDAR\r\n")
	raw, err := BuildMIME(model.OutgoingMessage{
		From:           "me@example.com",
		To:             "organizer@example.com",
		Subject:        "Accepted: Sync",
		Body:           "Me has accepted the invitation: Sync",
		Calendar:       ics,
		CalendarMethod: "REPLY",
		Attachments: []model.OutgoingAttachment{
			{Filename: "response.ics", MimeType: `application/ics; name="response.ics"`, Data: ics},
		},
	})
	if err != nil {
		t.Fatalf("BuildMIME: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, "Content-Type: multipart/mixed; boundary=") {
		t.Errorf("missing mixed content type:\n%s", s)
	}
	if !strings.Contains(s, "Content-Type: multipart/alternative; boundary=") {
		t.Errorf("calendar body must be nested as multipart/alternative:\n%s", s)
	}
	if !strings.Contains(s, `Content-Type: text/calendar; charset="utf-8"; method=REPLY`) {
		t.Errorf("missing inline text/calendar part:\n%s", s)
	}
	// The inline calendar part carries the payload (base64 of the ics bytes).
	enc := base64.StdEncoding.EncodeToString(ics)
	if strings.Count(s, enc[:20]) < 2 { // once inline, once as the attachment
		t.Errorf("calendar payload missing from inline part or attachment:\n%s", s)
	}
	// A calendar part without HTML must not emit an empty text/html part.
	if strings.Contains(s, "text/html") {
		t.Errorf("unexpected html part:\n%s", s)
	}
}

// A calendar part on an attachment-less message still produces
// multipart/alternative (not bare text/plain).
func TestBuildMIMECalendarNoAttachments(t *testing.T) {
	raw, err := BuildMIME(model.OutgoingMessage{
		From: "me@example.com", To: "o@example.com", Subject: "Accepted",
		Body: "ok", Calendar: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"), CalendarMethod: "REPLY",
	})
	if err != nil {
		t.Fatalf("BuildMIME: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, "Content-Type: multipart/alternative; boundary=") {
		t.Errorf("missing alternative content type:\n%s", s)
	}
	if !strings.Contains(s, `Content-Type: text/calendar; charset="utf-8"; method=REPLY`) {
		t.Errorf("missing text/calendar part:\n%s", s)
	}
}

func TestBuildMIMESubjectEncoding(t *testing.T) {
	raw, err := BuildMIME(model.OutgoingMessage{To: "a@b.com", Subject: "Schöne Grüße", Body: "hi"})
	if err != nil {
		t.Fatalf("BuildMIME: %v", err)
	}
	// Non-ASCII subjects must be RFC 2047 encoded.
	if !strings.Contains(string(raw), "Subject: =?utf-8?") {
		t.Errorf("subject not encoded:\n%s", raw)
	}
}

// A CR/LF smuggled into a header value (e.g. from a crafted mailto: link) must
// not inject an extra header line into the sent message.
func TestBuildMIMENoHeaderInjection(t *testing.T) {
	raw, err := BuildMIME(model.OutgoingMessage{
		From:    "me@example.com",
		To:      "you@example.com",
		Cc:      "cc@example.com\r\nBcc: attacker@evil.com",
		Subject: "Hi\r\nBcc: attacker2@evil.com",
		Body:    "hello",
	})
	if err != nil {
		t.Fatalf("BuildMIME: %v", err)
	}
	// The headers end at the first blank line. Injection would place a Bcc on its
	// own header line; the message has no legitimate Bcc, so any Bcc header line
	// (or any header line at all mentioning an attacker address) is a leak. The
	// stripped value stays on its original Cc/Subject line, which is not injection.
	headers := string(raw)
	if i := strings.Index(headers, "\r\n\r\n"); i >= 0 {
		headers = headers[:i]
	}
	for _, line := range strings.Split(headers, "\r\n") {
		key, _, _ := strings.Cut(line, ":")
		if strings.EqualFold(strings.TrimSpace(key), "Bcc") {
			t.Fatalf("injected Bcc header line: %q\nfull headers:\n%s", line, headers)
		}
	}
	// The legitimate Cc must still be present (value kept, just single-lined).
	if !strings.Contains(headers, "cc@example.com") {
		t.Fatalf("legitimate Cc dropped:\n%s", headers)
	}
}

func TestBuildMIMEEncodesNonASCIIDisplayNames(t *testing.T) {
	raw, err := BuildMIME(model.OutgoingMessage{
		From:    `Jürgen Müller <j@example.de>`,
		To:      `Ünal Ö <u@example.com>, Plain Name <p@example.com>`,
		Cc:      `Zoë <z@example.com>`,
		Subject: "hi",
		Body:    "b",
	})
	if err != nil {
		t.Fatalf("BuildMIME: %v", err)
	}
	s := string(raw)
	head := s[:strings.Index(s, "\r\n\r\n")]
	// No raw non-ASCII may appear in the address headers (RFC 5322 headers are
	// ASCII; non-ASCII display names must be RFC-2047 encoded-words).
	for _, r := range head {
		if r > 127 {
			t.Fatalf("raw non-ASCII rune %q in headers:\n%s", r, head)
		}
	}
	for _, want := range []string{
		"From: =?utf-8?", // Jürgen Müller encoded
		"<j@example.de>",
		"<u@example.com>",
		"Cc: =?utf-8?", // Zoë encoded
	} {
		if !strings.Contains(head, want) {
			t.Errorf("missing %q in headers:\n%s", want, head)
		}
	}
	// The ASCII display name survives readable (net/mail quotes it at most).
	if !strings.Contains(head, "Plain Name") && !strings.Contains(head, `"Plain Name"`) {
		t.Errorf("ASCII display name mangled:\n%s", head)
	}
}

func TestEncodeAddressListPassthroughOnUnparseable(t *testing.T) {
	in := "totally --not-- an address list"
	if got := encodeAddressList(in); got != in {
		t.Errorf("unparseable input rewritten: %q -> %q", in, got)
	}
	// A bare address stays a bare address (historical wire format).
	if got := encodeAddressList("a@b.com"); got != "a@b.com" {
		t.Errorf("bare address mangled: %q", got)
	}
}

func TestBuildMIMEAlternative(t *testing.T) {
	raw, err := BuildMIME(model.OutgoingMessage{
		From:     "me@example.com",
		To:       "you@example.com",
		Subject:  "hi",
		Body:     "plain text reply",
		HTMLBody: "<div>html reply</div><blockquote>quoted</blockquote>",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "Content-Type: multipart/alternative;") {
		t.Fatalf("expected multipart/alternative, got:\n%s", s)
	}
	for _, want := range []string{"text/plain", "text/html", "plain text reply", "quoted-printable"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	// The text part must come first so HTML-capable clients prefer the later part.
	if strings.Index(s, "text/plain") > strings.Index(s, "text/html") {
		t.Fatal("text/plain part must precede text/html")
	}
}

func TestBuildMIMEAlternativeWithAttachment(t *testing.T) {
	raw, err := BuildMIME(model.OutgoingMessage{
		From:        "me@example.com",
		To:          "you@example.com",
		Subject:     "hi",
		Body:        "plain",
		HTMLBody:    "<div>html</div>",
		Attachments: []model.OutgoingAttachment{{Filename: "a.txt", MimeType: "text/plain", Data: []byte("x")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "Content-Type: multipart/mixed;") {
		t.Fatalf("expected multipart/mixed, got:\n%s", s)
	}
	if !strings.Contains(s, "multipart/alternative") {
		t.Fatalf("expected nested multipart/alternative, got:\n%s", s)
	}
	if !strings.Contains(s, `attachment; filename="a.txt"`) {
		t.Fatalf("missing attachment part:\n%s", s)
	}
}

// TestBuildMIMEAlternativeLongLines: quoted original HTML routinely has lines
// far past SMTP's 998-byte limit; the quoted-printable HTML part must keep
// every wire line within it.
func TestBuildMIMEAlternativeLongLines(t *testing.T) {
	raw, err := BuildMIME(model.OutgoingMessage{
		From: "me@example.com", To: "you@example.com", Subject: "hi",
		Body:     "plain",
		HTMLBody: "<div>" + strings.Repeat("x", 5000) + "</div>",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("wire line %d is %d bytes (limit 998)", i, len(line))
		}
	}
}

// TestBuildMIMEFoldsLongHeaders: References grows without bound on long
// threads and reply-all To lists grow with participants; every header line
// must stay within RFC 5322's 998-byte limit (target 78), and the folded
// headers must still parse back to their original values.
func TestBuildMIMEFoldsLongHeaders(t *testing.T) {
	refs := make([]string, 60)
	for i := range refs {
		refs[i] = fmt.Sprintf("<%040d@mail.gmail.com>", i)
	}
	tos := make([]string, 30)
	for i := range tos {
		tos[i] = fmt.Sprintf("recipient-number-%02d@example-company.com", i)
	}
	raw, err := BuildMIME(model.OutgoingMessage{
		From:       "me@example.com",
		To:         strings.Join(tos, ", "),
		Subject:    strings.Repeat("Ünïcode subject needing many encoded words ", 6),
		Body:       "hi",
		References: strings.Join(refs, " "),
		InReplyTo:  refs[len(refs)-1],
	})
	if err != nil {
		t.Fatal(err)
	}
	headerEnd := strings.Index(string(raw), "\r\n\r\n")
	for i, line := range strings.Split(string(raw[:headerEnd]), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("header line %d is %d bytes (hard limit 998): %.80s…", i, len(line), line)
		}
	}
	// The folded message must parse, and unfolding must reproduce the values.
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("folded message does not parse: %v", err)
	}
	if got := msg.Header.Get("References"); got != strings.Join(refs, " ") {
		t.Fatalf("References mangled by folding:\n got %q\nwant %q", got, strings.Join(refs, " "))
	}
	if addrs, err := msg.Header.AddressList("To"); err != nil || len(addrs) != len(tos) {
		t.Fatalf("To list mangled by folding: %d addrs, err=%v", len(addrs), err)
	}
}
