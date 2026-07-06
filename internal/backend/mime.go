package backend

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// BuildMIME renders an OutgoingMessage as an RFC 5322 message (UTF-8). A
// plain-text-only message goes out text/plain; one with an HTMLBody or a
// Calendar payload goes out multipart/alternative (text first, HTML preferred
// by capable clients, inline text/calendar last for iTIP processing); one
// with attachments wraps either shape in multipart/mixed. The body's newlines
// are normalized to CRLF. Threading headers are included when present so
// replies/forwards thread correctly.
func BuildMIME(m model.OutgoingMessage) ([]byte, error) {
	logging.Trace("backend: BuildMIME",
		"from", m.From, "to", m.To, "cc", m.Cc, "bcc", m.Bcc,
		"subject", m.Subject, "attachments", len(m.Attachments),
		"threaded", m.InReplyTo != "" || m.References != "", "threadID", m.ThreadID)
	var b bytes.Buffer
	// Header values must not carry their own line breaks. Strip any CR/LF from
	// the value so input sourced from an untrusted place — a crafted mailto:
	// link the app is registered to handle, or a reply header echoed from a
	// malicious sender — can't smuggle extra headers (e.g. a hidden Bcc) into
	// the message the user sends. Folding (below) re-introduces only our own
	// controlled CRLF+WSP breaks.
	stripCRLF := strings.NewReplacer("\r", "", "\n", "")
	header := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, stripCRLF.Replace(v)) }
	// foldedHeader folds a structured header at its space-separated boundaries
	// (RFC 5322 §2.2.3: CRLF + WSP). References grows without bound on long
	// threads and reply-all recipient lists grow with participants; unfolded,
	// either can exceed the 998-byte line limit (RFC 5322 §2.1.1) that strict
	// MTAs enforce.
	foldedHeader := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, foldValue(stripCRLF.Replace(v), len(k)+2)) }

	foldedHeader("From", encodeAddressList(m.From))
	if strings.TrimSpace(m.To) != "" {
		foldedHeader("To", encodeAddressList(m.To))
	}
	if strings.TrimSpace(m.Cc) != "" {
		foldedHeader("Cc", encodeAddressList(m.Cc))
	}
	// Gmail honors a Bcc header for delivery and strips it from recipients' copies.
	if strings.TrimSpace(m.Bcc) != "" {
		foldedHeader("Bcc", encodeAddressList(m.Bcc))
	}
	// Q-encoded words are ≤75 chars and space-separated, so folding between
	// them is fold-safe (RFC 2047 §2).
	foldedHeader("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("Message-ID", generateMessageID(m.From))
	if m.InReplyTo != "" {
		header("In-Reply-To", m.InReplyTo)
	}
	if m.References != "" {
		foldedHeader("References", m.References)
	}
	header("MIME-Version", "1.0")

	if len(m.Attachments) == 0 {
		if m.HTMLBody != "" || len(m.Calendar) > 0 {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			if err := writeAlternative(mw, m); err != nil {
				logging.Trace("backend: BuildMIME done", "kind", "multipart/alternative", "err", err)
				return nil, err
			}
			if err := mw.Close(); err != nil {
				return nil, fmt.Errorf("close multipart: %w", err)
			}
			header("Content-Type", "multipart/alternative; boundary=\""+mw.Boundary()+"\"")
			b.WriteString("\r\n")
			b.Write(body.Bytes())
			logging.Trace("backend: BuildMIME done", "kind", "multipart/alternative", "bytes", b.Len())
			return b.Bytes(), nil
		}
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

// writeAlternative writes the message body as multipart/alternative parts into
// mw: text/plain first, then text/html (clients render the last part they
// support, so HTML is preferred by capable ones), then an inline text/calendar
// iTIP part when present — servers auto-process attendee responses only from
// an inline calendar part, and calendar-aware clients render it as the RSVP.
// The HTML part is quoted-printable — quoted original HTML routinely carries
// lines far beyond SMTP's 998-byte limit, which 8bit would put on the wire
// verbatim.
func writeAlternative(mw *multipart.Writer, m model.OutgoingMessage) error {
	text, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=\"utf-8\""},
		"Content-Transfer-Encoding": {"8bit"},
	})
	if err != nil {
		return fmt.Errorf("create text part: %w", err)
	}
	if _, err := text.Write([]byte(normalizeNewlines(m.Body))); err != nil {
		return fmt.Errorf("write text part: %w", err)
	}
	if m.HTMLBody != "" {
		htmlPart, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {"text/html; charset=\"utf-8\""},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return fmt.Errorf("create html part: %w", err)
		}
		qp := quotedprintable.NewWriter(htmlPart)
		if _, err := qp.Write([]byte(normalizeNewlines(m.HTMLBody))); err != nil {
			return fmt.Errorf("write html part: %w", err)
		}
		if err := qp.Close(); err != nil {
			return fmt.Errorf("close html part: %w", err)
		}
	}
	if len(m.Calendar) > 0 {
		ctype := "text/calendar; charset=\"utf-8\""
		if m.CalendarMethod != "" {
			ctype += "; method=" + m.CalendarMethod
		}
		cal, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {ctype},
			"Content-Transfer-Encoding": {"base64"},
		})
		if err != nil {
			return fmt.Errorf("create calendar part: %w", err)
		}
		if err := writeWrappedBase64(cal, m.Calendar); err != nil {
			return fmt.Errorf("encode calendar part: %w", err)
		}
	}
	return nil
}

// buildMultipart writes a multipart/mixed body (the text or alternative body
// part + base64 attachments) after the already-written top headers.
func buildMultipart(b *bytes.Buffer, header func(k, v string), m model.OutgoingMessage) ([]byte, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	if m.HTMLBody != "" || len(m.Calendar) > 0 {
		// Nest the text+HTML(+calendar) set as one multipart/alternative part,
		// so clients pick a body independently of the attachments.
		var alt bytes.Buffer
		aw := multipart.NewWriter(&alt)
		if err := writeAlternative(aw, m); err != nil {
			return nil, err
		}
		if err := aw.Close(); err != nil {
			return nil, fmt.Errorf("close alternative: %w", err)
		}
		altPart, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type": {"multipart/alternative; boundary=\"" + aw.Boundary() + "\""},
		})
		if err != nil {
			return nil, fmt.Errorf("create alternative part: %w", err)
		}
		if _, err := altPart.Write(alt.Bytes()); err != nil {
			return nil, fmt.Errorf("write alternative part: %w", err)
		}
	} else {
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

// foldTarget is the preferred maximum length of a header line (RFC 5322 §2.1.1
// recommends 78 excluding CRLF); a single token longer than this stays on one
// line — message-ids and addresses aren't splittable — which is fine as long as
// it stays under the hard 998 limit.
const foldTarget = 78

// foldValue folds a header value at space boundaries so each output line stays
// within foldTarget where possible. startCol is the width already used by the
// header name and ": ". Folds are CRLF + one space of WSP; the space that
// separated the tokens is consumed by the fold (its WSP), so unfolding
// reproduces the original value (RFC 5322 §2.2.3).
func foldValue(v string, startCol int) string {
	if startCol+len(v) <= foldTarget {
		return v
	}
	var out strings.Builder
	col := startCol
	for i, tok := range strings.Split(v, " ") {
		if i > 0 {
			if col+1+len(tok) > foldTarget {
				out.WriteString("\r\n ")
				col = 1
			} else {
				out.WriteString(" ")
				col++
			}
		}
		out.WriteString(tok)
		col += len(tok)
	}
	return out.String()
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
