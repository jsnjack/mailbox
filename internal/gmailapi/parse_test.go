package gmailapi

import (
	"encoding/base64"
	"testing"

	gmail "google.golang.org/api/gmail/v1"
)

func TestToMessageDecodesSnippet(t *testing.T) {
	m := ToMessage(1, &gmail.Message{
		Id:       "m1",
		ThreadId: "t1",
		Snippet:  "Here&#39;s the plan &amp; details &lt;3",
	})
	if want := "Here's the plan & details <3"; m.Snippet != want {
		t.Fatalf("Snippet = %q, want %q", m.Snippet, want)
	}
}

func TestDecodeFlags(t *testing.T) {
	tests := []struct {
		name            string
		labels          []string
		unread, starred bool
	}{
		{"none", []string{"INBOX"}, false, false},
		{"unread", []string{"INBOX", "UNREAD"}, true, false},
		{"starred", []string{"STARRED"}, false, true},
		{"both", []string{"UNREAD", "STARRED"}, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, s := decodeFlags(tc.labels)
			if u != tc.unread || s != tc.starred {
				t.Fatalf("got (%v,%v), want (%v,%v)", u, s, tc.unread, tc.starred)
			}
		})
	}
}

func TestParseFromHeader(t *testing.T) {
	tests := []struct {
		in       string
		wantName string
		wantAddr string
	}{
		{`Alice <alice@example.com>`, "Alice", "alice@example.com"},
		{`bob@example.com`, "", "bob@example.com"},
		{`"Doe, John" <john@example.com>`, "Doe, John", "john@example.com"},
		{``, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			name, addr := parseFromHeader(tc.in)
			if name != tc.wantName || addr != tc.wantAddr {
				t.Fatalf("got (%q,%q), want (%q,%q)", name, addr, tc.wantName, tc.wantAddr)
			}
		})
	}
}

func b64(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }

func TestExtractBodyMultipart(t *testing.T) {
	payload := &gmail.MessagePart{
		MimeType: "multipart/alternative",
		Parts: []*gmail.MessagePart{
			{MimeType: "text/plain", Body: &gmail.MessagePartBody{Data: b64("hello plain")}},
			{MimeType: "text/html", Body: &gmail.MessagePartBody{Data: b64("<p>hello html</p>")}},
		},
	}
	text, html := extractBody(payload)
	if text != "hello plain" {
		t.Fatalf("text = %q", text)
	}
	if html != "<p>hello html</p>" {
		t.Fatalf("html = %q", html)
	}
}

func TestExternalBodyParts(t *testing.T) {
	// A large HTML body Gmail externalized (Data empty, AttachmentId set) plus an
	// inline text/plain alternative.
	m := &gmail.Message{Payload: &gmail.MessagePart{
		MimeType: "multipart/alternative",
		Parts: []*gmail.MessagePart{
			{MimeType: "text/plain", Body: &gmail.MessagePartBody{Data: b64("plain")}},
			{MimeType: "text/html", Body: &gmail.MessagePartBody{AttachmentId: "html-att", Size: 1 << 20}},
		},
	}}
	textAtt, htmlAtt := ExternalBodyParts(m)
	if htmlAtt != "html-att" {
		t.Fatalf("htmlAtt = %q, want html-att", htmlAtt)
	}
	if textAtt != "" {
		t.Fatalf("textAtt = %q, want empty (it was inline)", textAtt)
	}
	// extractBody alone misses the externalized HTML — the gap this closes.
	if _, html := extractBody(m.Payload); html != "" {
		t.Fatalf("extractBody html = %q, want empty (externalized)", html)
	}
	// All-inline bodies report no external parts.
	if tA, hA := ExternalBodyParts(&gmail.Message{Payload: &gmail.MessagePart{
		MimeType: "multipart/alternative",
		Parts: []*gmail.MessagePart{
			{MimeType: "text/html", Body: &gmail.MessagePartBody{Data: b64("<p>hi</p>")}},
		},
	}}); tA != "" || hA != "" {
		t.Fatalf("inline body reported external parts: %q %q", tA, hA)
	}
}

func TestAttachmentsFromMessage(t *testing.T) {
	m := &gmail.Message{Payload: &gmail.MessagePart{
		MimeType: "multipart/mixed",
		Parts: []*gmail.MessagePart{
			{MimeType: "text/plain", Body: &gmail.MessagePartBody{Data: b64("hi")}},
			{Filename: "doc.pdf", MimeType: "application/pdf", Body: &gmail.MessagePartBody{AttachmentId: "att-1", Size: 4096}},
			{Filename: "pic.png", MimeType: "image/png", Body: &gmail.MessagePartBody{AttachmentId: "att-2", Size: 8192}},
		},
	}}
	atts := AttachmentsFromMessage(m)
	if len(atts) != 2 {
		t.Fatalf("got %d attachments, want 2", len(atts))
	}
	if atts[0].Filename != "doc.pdf" || atts[0].GmailAttID != "att-1" || atts[0].SizeBytes != 4096 {
		t.Fatalf("unexpected attachment: %+v", atts[0])
	}
}

func TestHasAttachments(t *testing.T) {
	withAtt := &gmail.MessagePart{
		MimeType: "multipart/mixed",
		Parts: []*gmail.MessagePart{
			{MimeType: "text/plain", Body: &gmail.MessagePartBody{Data: b64("hi")}},
			{Filename: "doc.pdf", Body: &gmail.MessagePartBody{AttachmentId: "att-1"}},
		},
	}
	if !hasAttachments(withAtt) {
		t.Fatal("expected attachments detected")
	}
	noAtt := &gmail.MessagePart{MimeType: "text/plain", Body: &gmail.MessagePartBody{Data: b64("hi")}}
	if hasAttachments(noAtt) {
		t.Fatal("did not expect attachments")
	}
}

// Bcc appears only on the user's own copies (sent, drafts); it must survive
// the metadata conversion so draft re-edit and the reader keep it.
func TestToMessageCapturesBcc(t *testing.T) {
	m := ToMessage(1, &gmail.Message{
		Id: "m1",
		Payload: &gmail.MessagePart{Headers: []*gmail.MessagePartHeader{
			{Name: "To", Value: "a@x.com"},
			{Name: "Cc", Value: "b@x.com"},
			{Name: "Bcc", Value: "Secret <c@x.com>"},
		}},
	})
	if m.ToAddrs != "a@x.com" || m.CcAddrs != "b@x.com" || m.BccAddrs != "Secret <c@x.com>" {
		t.Fatalf("to/cc/bcc = %q/%q/%q", m.ToAddrs, m.CcAddrs, m.BccAddrs)
	}
}
