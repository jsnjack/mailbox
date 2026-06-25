package gmailapi

import (
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
