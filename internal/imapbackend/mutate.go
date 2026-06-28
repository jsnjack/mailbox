package imapbackend

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"strconv"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gomail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-smtp"
	"github.com/jsnjack/mailbox/internal/model"
)

// folderUIDs groups a folder's UIDs with the UIDVALIDITY the ids were minted at.
type folderUIDs struct {
	uidv uint32
	uids []imap.UID
}

// groupByFolder parses provider ids and buckets their UIDs by mailbox.
func groupByFolder(ids []string) map[string]*folderUIDs {
	out := map[string]*folderUIDs{}
	for _, id := range ids {
		mailbox, uidv, uid, err := parseMsgID(id)
		if err != nil {
			continue
		}
		g := out[mailbox]
		if g == nil {
			g = &folderUIDs{uidv: uidv}
			out[mailbox] = g
		}
		g.uids = append(g.uids, uid)
	}
	return out
}

func has(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func uidSetOf(uids []imap.UID) imap.UIDSet {
	var s imap.UIDSet
	s.AddNum(uids...)
	return s
}

// ApplyLabels maps label deltas to IMAP flag changes (\Seen from UNREAD,
// \Flagged from STARRED) and folder moves (TRASH/SPAM/INBOX/archive). A move
// changes a message's UID, so the optimistic local change is reconciled by the
// next incremental (old id vanishes from the source, new id appears in dest).
func (b *Backend) ApplyLabels(ctx context.Context, ids []string, add, remove []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	cl, err := b.conn()
	if err != nil {
		return err
	}
	if err := b.ensureFolders(cl); err != nil {
		return err
	}

	var addFlags, delFlags []imap.Flag
	if has(remove, model.LabelUnread) {
		addFlags = append(addFlags, imap.FlagSeen) // mark read
	}
	if has(add, model.LabelUnread) {
		delFlags = append(delFlags, imap.FlagSeen) // mark unread
	}
	if has(add, model.LabelStarred) {
		addFlags = append(addFlags, imap.FlagFlagged)
	}
	if has(remove, model.LabelStarred) {
		delFlags = append(delFlags, imap.FlagFlagged)
	}

	dest := ""
	switch {
	case has(add, model.LabelTrash):
		dest = b.labelToFolder[model.LabelTrash]
	case has(add, model.LabelSpam):
		dest = b.labelToFolder[model.LabelSpam]
	case has(add, model.LabelInbox):
		if dest = b.labelToFolder[model.LabelInbox]; dest == "" {
			dest = "INBOX"
		}
	case has(remove, model.LabelInbox):
		dest = b.archiveFolder // archive; "" = no Archive folder → leave in place
	}

	for folder, g := range groupByFolder(ids) {
		sel, err := b.selectMailbox(cl, folder)
		if err != nil {
			b.reset()
			return err
		}
		if sel.UIDValidity != g.uidv {
			continue // stale ids; the next incremental reconciles
		}
		set := uidSetOf(g.uids)
		if len(addFlags) > 0 {
			if err := cl.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: addFlags}, nil).Close(); err != nil {
				b.reset()
				return fmt.Errorf("imap store +flags: %w", err)
			}
		}
		if len(delFlags) > 0 {
			if err := cl.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsDel, Flags: delFlags}, nil).Close(); err != nil {
				b.reset()
				return fmt.Errorf("imap store -flags: %w", err)
			}
		}
		if dest != "" && dest != folder {
			if _, err := cl.Move(set, dest).Wait(); err != nil {
				b.reset()
				return fmt.Errorf("imap move to %q: %w", dest, err)
			}
		}
	}
	return nil
}

// Delete permanently removes messages: \Deleted + EXPUNGE per folder.
func (b *Backend) Delete(ctx context.Context, ids []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	cl, err := b.conn()
	if err != nil {
		return err
	}
	for folder, g := range groupByFolder(ids) {
		sel, err := b.selectMailbox(cl, folder)
		if err != nil {
			b.reset()
			return err
		}
		if sel.UIDValidity != g.uidv {
			continue
		}
		set := uidSetOf(g.uids)
		if err := cl.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
			b.reset()
			return fmt.Errorf("imap store \\Deleted: %w", err)
		}
		if err := b.expunge(cl, set); err != nil {
			b.reset()
			return err
		}
	}
	return nil
}

// expunge removes \Deleted messages — by UID (UIDPLUS) so only the targeted ones
// go, else a plain EXPUNGE of the folder's \Deleted set.
func (b *Backend) expunge(cl *imapclient.Client, set imap.UIDSet) error {
	if cl.Caps().Has(imap.CapUIDPlus) {
		if _, err := cl.UIDExpunge(set).Collect(); err != nil {
			return fmt.Errorf("imap uid expunge: %w", err)
		}
		return nil
	}
	if _, err := cl.Expunge().Collect(); err != nil {
		return fmt.Errorf("imap expunge: %w", err)
	}
	return nil
}

// Send submits a message over SMTP, then APPENDs a copy to the Sent folder (SMTP
// delivery doesn't file it in IMAP the way Gmail's API does). The returned id is
// empty — the Sent copy surfaces through the next incremental.
func (b *Backend) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	from, to, cleaned, err := smtpEnvelope(raw)
	if err != nil {
		return "", err
	}
	if len(to) == 0 {
		return "", fmt.Errorf("imap send: no recipients")
	}
	if err := b.smtpSend(from, to, cleaned); err != nil {
		return "", err
	}
	b.appendToSent(cleaned) // best-effort; a failure just means no local Sent copy yet
	return "", nil
}

// smtpEnvelope extracts the MAIL FROM, the full RCPT TO list (To+Cc+Bcc), and the
// message to transmit with the Bcc header stripped (recipients must not see it).
func smtpEnvelope(raw []byte) (from string, to []string, cleaned []byte, err error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", nil, nil, fmt.Errorf("parse outgoing message: %w", err)
	}
	if froms, e := msg.Header.AddressList("From"); e == nil && len(froms) > 0 {
		from = froms[0].Address
	}
	seen := map[string]bool{}
	for _, hdr := range []string{"To", "Cc", "Bcc"} {
		addrs, _ := msg.Header.AddressList(hdr)
		for _, a := range addrs {
			if !seen[a.Address] {
				seen[a.Address] = true
				to = append(to, a.Address)
			}
		}
	}
	return from, to, stripHeader(raw, "Bcc"), nil
}

// stripHeader removes a header (and its folded continuation lines) from the
// header block of an RFC 5322 message, leaving the body untouched.
func stripHeader(raw []byte, name string) []byte {
	sep := bytes.Index(raw, []byte("\r\n\r\n"))
	if sep < 0 {
		return raw
	}
	head, body := raw[:sep], raw[sep:]
	prefix := []byte(strings.ToLower(name) + ":")
	var kept [][]byte
	skipping := false
	for _, ln := range bytes.Split(head, []byte("\r\n")) {
		if skipping {
			if len(ln) > 0 && (ln[0] == ' ' || ln[0] == '\t') {
				continue // folded continuation of the stripped header
			}
			skipping = false
		}
		if bytes.HasPrefix(bytes.ToLower(ln), prefix) {
			skipping = true
			continue
		}
		kept = append(kept, ln)
	}
	return append(bytes.Join(kept, []byte("\r\n")), body...)
}

func (b *Backend) smtpSend(from string, to []string, msg []byte) error {
	addr := net.JoinHostPort(b.cfg.SMTPHost, strconv.Itoa(b.cfg.SMTPPort))
	tlsCfg := &tls.Config{ServerName: b.cfg.SMTPHost}
	var (
		c   *smtp.Client
		err error
	)
	switch b.cfg.SMTPSecurity {
	case SecurityTLS:
		c, err = smtp.DialTLS(addr, tlsCfg)
	case SecuritySTARTTLS:
		c, err = smtp.DialStartTLS(addr, tlsCfg)
	case SecurityNone:
		c, err = smtp.Dial(addr)
	default:
		return fmt.Errorf("imap: unknown smtp security %q", b.cfg.SMTPSecurity)
	}
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	defer func() { _ = c.Close() }()
	sc, err := b.cred.smtpSASL()
	if err != nil {
		return err
	}
	if err := c.Auth(sc); err != nil {
		return fmt.Errorf("smtp auth: %w", err)
	}
	if err := c.SendMail(from, to, bytes.NewReader(msg)); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}

// appendToSent files a sent message in the Sent folder (best-effort).
func (b *Backend) appendToSent(msg []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cl, err := b.conn()
	if err != nil {
		return
	}
	if err := b.ensureFolders(cl); err != nil {
		return
	}
	if sent := b.labelToFolder[model.LabelSent]; sent != "" {
		_, _ = b.appendLocked(cl, sent, msg, imap.FlagSeen)
	}
}

// appendLocked APPENDs msg to a mailbox with the given flags. Caller holds mu.
func (b *Backend) appendLocked(cl *imapclient.Client, mailbox string, msg []byte, flags ...imap.Flag) (*imap.AppendData, error) {
	cmd := cl.Append(mailbox, int64(len(msg)), &imap.AppendOptions{Flags: flags})
	if _, err := cmd.Write(msg); err != nil {
		_ = cmd.Close()
		return nil, err
	}
	if err := cmd.Close(); err != nil {
		return nil, err
	}
	return cmd.Wait()
}

// SaveDraft APPENDs raw to the Drafts folder (with \Draft) and returns the new
// draft's provider id (empty when the server lacks UIDPLUS, so no stable id).
func (b *Backend) SaveDraft(ctx context.Context, raw []byte, threadID string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cl, err := b.conn()
	if err != nil {
		return "", err
	}
	if err := b.ensureFolders(cl); err != nil {
		return "", err
	}
	drafts := b.labelToFolder[model.LabelDraft]
	if drafts == "" {
		return "", fmt.Errorf("imap: no Drafts folder")
	}
	ad, err := b.appendLocked(cl, drafts, raw, imap.FlagDraft)
	if err != nil {
		b.reset()
		return "", fmt.Errorf("imap append draft: %w", err)
	}
	if ad == nil || ad.UID == 0 {
		return "", nil
	}
	return msgID(drafts, ad.UIDValidity, ad.UID), nil
}

// UpdateDraft replaces an existing draft: IMAP has no in-place edit, so delete
// the old message and append the new one.
func (b *Backend) UpdateDraft(ctx context.Context, draftID string, raw []byte, threadID string) (string, error) {
	if err := b.DeleteDraft(ctx, draftID); err != nil {
		return "", err
	}
	return b.SaveDraft(ctx, raw, threadID)
}

// DeleteDraft removes a draft by its provider id (the draft is the message).
func (b *Backend) DeleteDraft(ctx context.Context, draftID string) error {
	if draftID == "" {
		return nil
	}
	return b.Delete(ctx, []string{draftID})
}

// FindDraftID is the identity for IMAP: a draft has no id separate from its
// message provider id.
func (b *Backend) FindDraftID(ctx context.Context, id string) (string, error) {
	return id, nil
}

// FetchAttachment re-fetches the message body and returns the bytes of the
// attachment at the 1-based ordinal recorded during parsing.
func (b *Backend) FetchAttachment(ctx context.Context, msgIDArg, attID string) ([]byte, error) {
	raw, err := b.fetchRaw(msgIDArg)
	if err != nil {
		return nil, err
	}
	idx, err := strconv.Atoi(attID)
	if err != nil {
		return nil, fmt.Errorf("imap: bad attachment id %q", attID)
	}
	return attachmentBytes(raw, idx)
}

// attachmentBytes returns the idx-th attachment part's decoded bytes.
func attachmentBytes(raw []byte, idx int) ([]byte, error) {
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("imap: parse message: %w", err)
	}
	n := 0
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
		if _, ok := part.Header.(*gomail.AttachmentHeader); ok {
			n++
			if n == idx {
				return io.ReadAll(part.Body)
			}
		}
		_, _ = io.Copy(io.Discard, part.Body)
	}
	return nil, fmt.Errorf("imap: attachment %d not found", idx)
}
