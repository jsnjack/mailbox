package gmailapi

import (
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
	return model.Message{
		AccountID:      accountID,
		GmailID:        m.Id,
		ThreadID:       m.ThreadId,
		InternalDate:   internalDate(m.InternalDate),
		FromName:       name,
		FromAddr:       addr,
		ToAddrs:        headerValue(headers, "To"),
		CcAddrs:        headerValue(headers, "Cc"),
		Subject:        headerValue(headers, "Subject"),
		Snippet:        m.Snippet,
		RFC822MsgID:    headerValue(headers, "Message-ID"),
		InReplyTo:      headerValue(headers, "In-Reply-To"),
		References:     headerValue(headers, "References"),
		IsUnread:       unread,
		IsStarred:      starred,
		HasAttachments: m.Payload != nil && hasAttachments(m.Payload),
		SizeEstimate:   m.SizeEstimate,
		Labels:         m.LabelIds,
	}
}

// ToBody extracts the text and HTML body parts from a full-format Gmail message.
func ToBody(m *gmail.Message) model.MessageBody {
	text, html := extractBody(m.Payload)
	return model.MessageBody{Text: text, HTML: html}
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
