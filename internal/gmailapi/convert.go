package gmailapi

import (
	"html"
	"strings"

	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
	gmail "google.golang.org/api/gmail/v1"
)

// ToMessage converts a Gmail message (metadata or full format) into the domain
// model. Headers come from the payload; flags are derived from label ids.
func ToMessage(accountID int64, m *gmail.Message) model.Message {
	var headers []*gmail.MessagePartHeader
	if m.Payload != nil {
		headers = m.Payload.Headers
	}
	name, addr := parseFromHeader(headerValue(headers, "From"))
	unread, starred := decodeFlags(m.LabelIds)
	logging.Trace("gmailapi: toMessage", "id", m.Id, "thread_id", m.ThreadId, "from", addr, "subject", logging.Body(headerValue(headers, "Subject")), "unread", unread, "starred", starred, "labels", m.LabelIds, "has_attachments", m.Payload != nil && hasAttachments(m.Payload))
	return model.Message{
		AccountID:    accountID,
		GmailID:      m.Id,
		ThreadID:     m.ThreadId,
		InternalDate: internalDate(m.InternalDate),
		FromName:     name,
		FromAddr:     addr,
		ReplyTo:      headerValue(headers, "Reply-To"),
		ToAddrs:      headerValue(headers, "To"),
		CcAddrs:      headerValue(headers, "Cc"),
		BccAddrs:     headerValue(headers, "Bcc"),
		Subject:      headerValue(headers, "Subject"),
		// Gmail's snippet is HTML-escaped (e.g. "it&#39;s"); store it as plain text.
		Snippet:        html.UnescapeString(m.Snippet),
		RFC822MsgID:    headerValue(headers, "Message-ID"),
		InReplyTo:      headerValue(headers, "In-Reply-To"),
		References:     headerValue(headers, "References"),
		IsUnread:       unread,
		IsStarred:      starred,
		HasAttachments: m.Payload != nil && hasAttachments(m.Payload),
		SizeEstimate:   m.SizeEstimate,
		// Unsubscribe support: RFC 8058 one-click needs the exact token
		// "List-Unsubscribe=One-Click" in List-Unsubscribe-Post.
		ListUnsubscribe:   headerValue(headers, "List-Unsubscribe"),
		ListUnsubOneClick: strings.Contains(strings.ToLower(headerValue(headers, "List-Unsubscribe-Post")), "one-click"),
		Labels:            m.LabelIds,
	}
}

// ToBody extracts the text and HTML body parts from a full-format Gmail message.
func ToBody(m *gmail.Message) model.MessageBody {
	text, html := extractBody(m.Payload)
	// Capture Gmail's SPF/DKIM/DMARC verdict (added on receipt) so the reader can
	// show a sender-authenticity badge. Stored in the otherwise-unused
	// raw_headers column.
	var auth string
	if m.Payload != nil {
		auth = headerValue(m.Payload.Headers, "Authentication-Results")
	}
	logging.Trace("gmailapi: toBody", "id", m.Id, "text_bytes", len(text), "html_bytes", len(html), "has_html", html != "", "has_text", text != "", "has_auth", auth != "")
	return model.MessageBody{Text: text, HTML: html, RawHeaders: auth}
}

// AttachmentsFromMessage extracts attachment metadata (named parts with an
// attachmentId) from a full-format Gmail message. Bytes are fetched separately.
func AttachmentsFromMessage(m *gmail.Message) []model.Attachment {
	if m == nil || m.Payload == nil {
		return nil
	}
	var out []model.Attachment
	var walk func(p *gmail.MessagePart)
	walk = func(p *gmail.MessagePart) {
		if p == nil {
			return
		}
		// A normal attachment (named part with bytes), or an inline image
		// referenced by a cid: URL (a Content-ID part, which may have no filename).
		cid := strings.Trim(headerValue(p.Headers, "Content-ID"), "<>")
		named := p.Filename != ""
		if (named || cid != "") && p.Body != nil && p.Body.AttachmentId != "" {
			out = append(out, model.Attachment{
				GmailAttID: p.Body.AttachmentId,
				Filename:   p.Filename,
				MimeType:   p.MimeType,
				SizeBytes:  p.Body.Size,
				ContentID:  cid,
			})
		}
		for _, c := range p.Parts {
			walk(c)
		}
	}
	walk(m.Payload)
	logging.Trace("gmailapi: attachmentsFromMessage", "id", m.Id, "count", len(out))
	return out
}

// ToLabel converts a Gmail label into the domain model.
func ToLabel(accountID int64, l *gmail.Label) model.Label {
	typ := model.LabelUser
	if l.Type == "system" {
		typ = model.LabelSystem
	}
	return model.Label{
		AccountID:   accountID,
		GmailID:     l.Id,
		Name:        l.Name,
		Type:        typ,
		UnreadTotal: int(l.MessagesUnread),
	}
}
