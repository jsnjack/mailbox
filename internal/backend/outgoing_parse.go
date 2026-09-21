package backend

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-message/mail"
	"github.com/jsnjack/mailbox/internal/model"
)

// ParseOutgoingMessage recovers the editable content of a locally built MIME
// message. Unsupported parts fail instead of silently dropping outgoing content.
func ParseOutgoingMessage(raw []byte) (model.OutgoingMessage, error) {
	reader, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return model.OutgoingMessage{}, fmt.Errorf("read queued message: %w", err)
	}
	subject, err := reader.Header.Subject()
	if err != nil {
		return model.OutgoingMessage{}, fmt.Errorf("read queued subject: %w", err)
	}
	msg := model.OutgoingMessage{Subject: subject, InReplyTo: reader.Header.Get("In-Reply-To"), References: reader.Header.Get("References")}
	for _, field := range []struct {
		name   string
		target *string
	}{{"From", &msg.From}, {"To", &msg.To}, {"Cc", &msg.Cc}, {"Bcc", &msg.Bcc}} {
		if reader.Header.Get(field.name) == "" {
			continue
		}
		addresses, err := reader.Header.AddressList(field.name)
		if err != nil {
			return msg, fmt.Errorf("read queued %s: %w", field.name, err)
		}
		values := make([]string, len(addresses))
		for i, a := range addresses {
			values[i] = a.String()
		}
		*field.target = strings.Join(values, ", ")
	}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return msg, fmt.Errorf("read queued part: %w", err)
		}
		data, err := io.ReadAll(part.Body)
		if err != nil {
			return msg, fmt.Errorf("decode queued part: %w", err)
		}
		switch header := part.Header.(type) {
		case *mail.AttachmentHeader:
			name, err := header.Filename()
			if err != nil {
				return msg, fmt.Errorf("read attachment filename: %w", err)
			}
			kind, _, err := header.ContentType()
			if err != nil {
				return msg, fmt.Errorf("read attachment type: %w", err)
			}
			msg.Attachments = append(msg.Attachments, model.OutgoingAttachment{Filename: name, MimeType: kind, Data: data})
		case *mail.InlineHeader:
			kind, params, err := header.ContentType()
			if err != nil {
				return msg, fmt.Errorf("read inline type: %w", err)
			}
			switch kind {
			case "text/plain":
				msg.Body += string(data)
			case "text/html":
				msg.HTMLBody += string(data)
			case "text/calendar":
				msg.Calendar = data
				msg.CalendarMethod = params["method"]
			default:
				return msg, fmt.Errorf("cannot edit inline part of type %s", kind)
			}
		default:
			return msg, fmt.Errorf("cannot edit unsupported MIME part")
		}
	}
	if msg.Body == "" && msg.HTMLBody != "" {
		return msg, fmt.Errorf("cannot edit an HTML-only queued message without losing formatting")
	}
	return msg, nil
}
