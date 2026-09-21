package backend

import (
	"bytes"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestParseOutgoingMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  model.OutgoingMessage
	}{
		{"unicode and attachment", model.OutgoingMessage{From: "sender@example.com", To: "recipient@example.com", Cc: "cc@example.com", Bcc: "hidden@example.com", Subject: "Réunion demain", Body: "Bonjour à tous.\n\nMerci.", HTMLBody: "<p>Bonjour à tous.</p>", InReplyTo: "<parent@example.com>", References: "<root@example.com>", Attachments: []model.OutgoingAttachment{{Filename: "résumé.bin", MimeType: "application/octet-stream", Data: []byte{0, 255, 1, 2}}}}},
		{"calendar", model.OutgoingMessage{From: "sender@example.com", To: "recipient@example.com", Body: "Accepted", Calendar: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"), CalendarMethod: "REPLY"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := BuildMIME(tc.msg)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseOutgoingMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.Subject != tc.msg.Subject || got.InReplyTo != tc.msg.InReplyTo || got.References != tc.msg.References {
				t.Fatalf("headers changed: %#v", got)
			}
			// MIME canonicalizes line endings.
			if !bytes.Equal(bytes.ReplaceAll([]byte(got.Body), []byte("\r\n"), []byte("\n")), []byte(tc.msg.Body)) {
				t.Errorf("body = %q; want %q", got.Body, tc.msg.Body)
			}
			if len(got.Attachments) != len(tc.msg.Attachments) {
				t.Fatal("attachments lost")
			}
			for i, a := range got.Attachments {
				if a.Filename != tc.msg.Attachments[i].Filename || !bytes.Equal(a.Data, tc.msg.Attachments[i].Data) {
					t.Errorf("attachment changed: %#v", a)
				}
			}
			if !bytes.Equal(got.Calendar, tc.msg.Calendar) || got.CalendarMethod != tc.msg.CalendarMethod {
				t.Errorf("calendar changed")
			}
		})
	}
}
