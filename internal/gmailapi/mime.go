package gmailapi

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"mime"
	"strings"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

// BuildMIME renders an OutgoingMessage as an RFC 5322 message (text/plain, UTF-8).
// The body's newlines are normalized to CRLF. Threading headers are included when
// present so replies/forwards thread correctly.
func BuildMIME(m model.OutgoingMessage) ([]byte, error) {
	var b bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }

	header("From", m.From)
	if strings.TrimSpace(m.To) != "" {
		header("To", m.To)
	}
	if strings.TrimSpace(m.Cc) != "" {
		header("Cc", m.Cc)
	}
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("Message-ID", generateMessageID(m.From))
	if m.InReplyTo != "" {
		header("In-Reply-To", m.InReplyTo)
	}
	if m.References != "" {
		header("References", m.References)
	}
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=\"utf-8\"")
	header("Content-Transfer-Encoding", "8bit")
	b.WriteString("\r\n")
	b.WriteString(normalizeNewlines(m.Body))
	return b.Bytes(), nil
}

// generateMessageID returns a unique Message-ID using the sender's domain.
func generateMessageID(from string) string {
	domain := "localhost"
	if i := strings.LastIndex(from, "@"); i >= 0 {
		domain = strings.Trim(from[i+1:], "<> ")
	}
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("<%x@%s>", buf, domain)
}

// normalizeNewlines converts any newline style to CRLF.
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}
