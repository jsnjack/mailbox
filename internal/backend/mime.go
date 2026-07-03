package backend

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// BuildMIME renders an OutgoingMessage as an RFC 5322 message (text/plain, UTF-8).
// The body's newlines are normalized to CRLF. Threading headers are included when
// present so replies/forwards thread correctly.
func BuildMIME(m model.OutgoingMessage) ([]byte, error) {
	logging.Trace("backend: BuildMIME",
		"from", m.From, "to", m.To, "cc", m.Cc, "bcc", m.Bcc,
		"subject", m.Subject, "attachments", len(m.Attachments),
		"threaded", m.InReplyTo != "" || m.References != "", "threadID", m.ThreadID)
	var b bytes.Buffer
	// Header values must be single-line. Strip any CR/LF from the value so input
	// sourced from an untrusted place — a crafted mailto: link the app is
	// registered to handle, or a reply header echoed from a malicious sender —
	// can't smuggle extra headers (e.g. a hidden Bcc) into the message the user
	// sends. Nothing we build here legitimately needs a raw newline in a value.
	stripCRLF := strings.NewReplacer("\r", "", "\n", "")
	header := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, stripCRLF.Replace(v)) }

	header("From", encodeAddressList(m.From))
	if strings.TrimSpace(m.To) != "" {
		header("To", encodeAddressList(m.To))
	}
	if strings.TrimSpace(m.Cc) != "" {
		header("Cc", encodeAddressList(m.Cc))
	}
	// Gmail honors a Bcc header for delivery and strips it from recipients' copies.
	if strings.TrimSpace(m.Bcc) != "" {
		header("Bcc", encodeAddressList(m.Bcc))
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

	if len(m.Attachments) == 0 {
		header("Content-Type", "text/plain; charset=\"utf-8\"")
		header("Content-Transfer-Encoding", "8bit")
		b.WriteString("\r\n")
		b.WriteString(normalizeNewlines(m.Body))
		logging.Trace("backend: BuildMIME done", "kind", "text/plain", "bytes", b.Len())
		return b.Bytes(), nil
	}
	out, err := buildMultipart(&b, header, m)
	if err != nil {
		logging.Trace("backend: BuildMIME done", "kind", "multipart/mixed", "err", err)
		return out, err
	}
	logging.Trace("backend: BuildMIME done", "kind", "multipart/mixed", "attachments", len(m.Attachments), "bytes", len(out))
	return out, nil
}

// buildMultipart writes a multipart/mixed body (text part + base64 attachments)
// after the already-written top headers.
func buildMultipart(b *bytes.Buffer, header func(k, v string), m model.OutgoingMessage) ([]byte, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	text, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=\"utf-8\""},
		"Content-Transfer-Encoding": {"8bit"},
	})
	if err != nil {
		return nil, fmt.Errorf("create text part: %w", err)
	}
	if _, err := text.Write([]byte(normalizeNewlines(m.Body))); err != nil {
		return nil, fmt.Errorf("write text part: %w", err)
	}

	for _, a := range m.Attachments {
		mtype := a.MimeType
		if mtype == "" {
			mtype = "application/octet-stream"
		}
		part, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {mtype},
			"Content-Transfer-Encoding": {"base64"},
			"Content-Disposition":       {fmt.Sprintf("attachment; filename=%q", a.Filename)},
		})
		if err != nil {
			return nil, fmt.Errorf("create attachment part %q: %w", a.Filename, err)
		}
		if err := writeWrappedBase64(part, a.Data); err != nil {
			return nil, fmt.Errorf("encode attachment %q: %w", a.Filename, err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart: %w", err)
	}

	header("Content-Type", "multipart/mixed; boundary=\""+mw.Boundary()+"\"")
	b.WriteString("\r\n")
	b.Write(body.Bytes())
	return b.Bytes(), nil
}

// writeWrappedBase64 writes data as base64 wrapped at 76 columns (RFC 2045).
func writeWrappedBase64(w io.Writer, data []byte) error {
	const cols = 76
	enc := base64.StdEncoding.EncodeToString(data)
	for i := 0; i < len(enc); i += cols {
		end := i + cols
		if end > len(enc) {
			end = len(enc)
		}
		if _, err := io.WriteString(w, enc[i:end]+"\r\n"); err != nil {
			return err
		}
	}
	return nil
}

// encodeAddressList re-renders a comma-separated address list with each display
// name RFC-2047-encoded when it contains non-ASCII (net/mail's Address.String
// does the encoding), so "Jürgen <j@x.de>" doesn't put raw UTF-8 on the wire in
// a structured header. Input that net/mail can't parse is passed through
// unchanged — better an oddly-encoded name than a dropped recipient (the
// address part of unparseable input was never separable anyway).
func encodeAddressList(list string) string {
	addrs, err := mail.ParseAddressList(list)
	if err != nil {
		logging.Trace("backend: address list unparseable; passing through", "list", list, "err", err)
		return list
	}
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Name == "" {
			// Keep a bare address bare ("me@x.com", not "<me@x.com>") — both are
			// valid, but this preserves the historical wire format.
			parts = append(parts, a.Address)
			continue
		}
		parts = append(parts, a.String())
	}
	return strings.Join(parts, ", ")
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
