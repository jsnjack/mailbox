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

func TestBuildMIMENoRecipient(t *testing.T) {
	if _, err := BuildMIME(model.OutgoingMessage{From: "me@example.com", Subject: "x"}); err == nil {
		t.Fatal("expected error with no recipient")
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
