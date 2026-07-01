package gmailapi

import (
	"encoding/base64"
	"net/mail"
	"strings"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
	gmail "google.golang.org/api/gmail/v1"
)

// decodeFlags derives the unread/starred flags from a message's Gmail label ids.
func decodeFlags(labelIDs []string) (unread, starred bool) {
	for _, l := range labelIDs {
		switch l {
		case model.LabelUnread:
			unread = true
		case model.LabelStarred:
			starred = true
		}
	}
	return unread, starred
}

// parseFromHeader splits a From header into display name and address. On a parse
// failure it returns the raw value as the address so nothing is lost.
func parseFromHeader(value string) (name, addr string) {
	if value == "" {
		return "", ""
	}
	a, err := mail.ParseAddress(value)
	if err != nil {
		return "", value
	}
	return a.Name, a.Address
}

// headerValue returns the first header matching name (case-insensitive).
func headerValue(headers []*gmail.MessagePartHeader, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// internalDate converts Gmail's internalDate (unix milliseconds) to time.Time.
func internalDate(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// extractBody walks the MIME tree and returns the first text/plain and text/html
// parts it finds. Gmail encodes part bodies as web-safe base64.
func extractBody(part *gmail.MessagePart) (text, html string) {
	if part == nil {
		return "", ""
	}
	if len(part.Parts) == 0 {
		data := ""
		if part.Body != nil && part.Body.Data != "" {
			if b, err := decodeWebSafeB64(part.Body.Data); err == nil {
				data = string(b)
			}
		}
		switch {
		case strings.HasPrefix(part.MimeType, "text/plain"):
			return data, ""
		case strings.HasPrefix(part.MimeType, "text/html"):
			return "", data
		}
		return "", ""
	}
	for _, p := range part.Parts {
		t, h := extractBody(p)
		if text == "" {
			text = t
		}
		if html == "" {
			html = h
		}
	}
	return text, html
}

// ExternalBodyParts returns the attachment ids of the first text/plain and
// text/html parts whose bytes Gmail did NOT inline (Data empty, AttachmentId
// set). Gmail externalizes large part bodies this way, so a big HTML body (e.g.
// a Dependabot weekly digest) is fetched separately rather than dropped, which
// would otherwise render the message text-only. Empty when bodies are inline.
func ExternalBodyParts(m *gmail.Message) (textAttID, htmlAttID string) {
	if m == nil {
		return "", ""
	}
	var walk func(p *gmail.MessagePart)
	walk = func(p *gmail.MessagePart) {
		if p == nil {
			return
		}
		if len(p.Parts) == 0 && p.Body != nil && p.Body.AttachmentId != "" && p.Body.Data == "" {
			switch {
			case strings.HasPrefix(p.MimeType, "text/plain") && textAttID == "":
				textAttID = p.Body.AttachmentId
			case strings.HasPrefix(p.MimeType, "text/html") && htmlAttID == "":
				htmlAttID = p.Body.AttachmentId
			}
		}
		for _, c := range p.Parts {
			walk(c)
		}
	}
	walk(m.Payload)
	if textAttID != "" || htmlAttID != "" {
		logging.Trace("gmailapi: externalBodyParts", "id", m.Id, "text_att_id", textAttID, "html_att_id", htmlAttID)
	}
	return textAttID, htmlAttID
}

// hasAttachments reports whether any MIME part is a named attachment.
func hasAttachments(part *gmail.MessagePart) bool {
	if part == nil {
		return false
	}
	if part.Filename != "" && part.Body != nil && part.Body.AttachmentId != "" {
		return true
	}
	for _, p := range part.Parts {
		if hasAttachments(p) {
			return true
		}
	}
	return false
}

// decodeWebSafeB64 decodes Gmail's URL-safe base64, tolerating missing padding.
func decodeWebSafeB64(s string) ([]byte, error) {
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}
