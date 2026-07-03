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

	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gomail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-smtp"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// folderKey identifies the epoch a batch of UIDs belongs to: a UID is only
// meaningful within its mailbox's UIDVALIDITY.
type folderKey struct {
	mailbox string
	uidv    uint32
}

// groupByFolder parses provider ids and buckets their UIDs by (mailbox,
// UIDVALIDITY). Ids minted under different epochs of the same mailbox land in
// separate groups, so after a server-side renumber a stale-epoch UID can never
// be flag-stored/moved/expunged against whatever message currently holds that
// number — the executing code compares each group's uidv to the freshly
// SELECTed mailbox and skips mismatches.
func groupByFolder(ids []string) map[folderKey][]imap.UID {
	out := map[folderKey][]imap.UID{}
	for _, id := range ids {
		mailbox, uidv, uid, err := parseMsgID(id)
		if err != nil {
			logging.Trace("imapbackend: group by folder skipping unparseable id", "id", id, "err", err)
			continue
		}
		k := folderKey{mailbox: mailbox, uidv: uidv}
		out[k] = append(out[k], uid)
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
	logging.TraceContext(ctx, "imapbackend: apply labels", "account", b.cfg.Email, "ids", len(ids), "add", add, "remove", remove)
	return b.withConn(func(c *conn) error {
		if err := b.ensureFolders(c); err != nil {
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

		dest := b.moveDest(add, remove)
		logging.Trace("imapbackend: apply labels resolved", "addFlags", addFlags, "delFlags", delFlags, "moveDest", dest)

		for key, uids := range groupByFolder(ids) {
			sel, err := c.reselect(key.mailbox, false) // fresh SELECT: a move is destructive
			if err != nil {
				return err
			}
			if sel.UIDValidity != key.uidv {
				logging.Trace("imapbackend: apply labels skip stale group", "folder", key.mailbox, "uidvalidity", key.uidv, "current", sel.UIDValidity, "n", len(uids))
				continue // stale-epoch ids; the next incremental reconciles
			}
			set := uidSetOf(uids)
			if len(addFlags) > 0 {
				logging.Trace("imapbackend: store +flags", "folder", key.mailbox, "n", len(uids), "flags", addFlags)
				if err := c.cl.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: addFlags}, nil).Close(); err != nil {
					return fmt.Errorf("imap store +flags: %w", err)
				}
			}
			if len(delFlags) > 0 {
				logging.Trace("imapbackend: store -flags", "folder", key.mailbox, "n", len(uids), "flags", delFlags)
				if err := c.cl.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsDel, Flags: delFlags}, nil).Close(); err != nil {
					return fmt.Errorf("imap store -flags: %w", err)
				}
			}
			if dest != "" && dest != key.mailbox {
				logging.Trace("imapbackend: move", "folder", key.mailbox, "dest", dest, "n", len(uids))
				if _, err := c.cl.Move(set, dest).Wait(); err != nil {
					return fmt.Errorf("imap move to %q: %w", dest, err)
				}
			}
		}
		return nil
	})
}

// moveDest resolves the destination folder for a label change (trash/spam/inbox/
// archive), reading the folder maps under the guard. "" means no move.
func (b *Backend) moveDest(add, remove []string) string {
	b.folderMu.Lock()
	defer b.folderMu.Unlock()
	switch {
	case has(add, model.LabelTrash):
		return b.labelToFolder[model.LabelTrash]
	case has(add, model.LabelSpam):
		return b.labelToFolder[model.LabelSpam]
	case has(add, model.LabelInbox):
		if d := b.labelToFolder[model.LabelInbox]; d != "" {
			return d
		}
		return "INBOX"
	case has(remove, model.LabelInbox):
		return b.archiveFolder // archive; "" = no Archive folder → leave in place
	}
	return ""
}

// Delete permanently removes messages: \Deleted + EXPUNGE per folder.
func (b *Backend) Delete(ctx context.Context, ids []string) error {
	logging.TraceContext(ctx, "imapbackend: delete", "account", b.cfg.Email, "ids", len(ids))
	return b.withConn(func(c *conn) error {
		for key, uids := range groupByFolder(ids) {
			sel, err := c.reselect(key.mailbox, false) // fresh SELECT: EXPUNGE is irreversible
			if err != nil {
				return err
			}
			if sel.UIDValidity != key.uidv {
				logging.Trace("imapbackend: delete skip stale group", "folder", key.mailbox, "uidvalidity", key.uidv, "current", sel.UIDValidity, "n", len(uids))
				continue
			}
			set := uidSetOf(uids)
			logging.Trace("imapbackend: store \\Deleted + expunge", "folder", key.mailbox, "n", len(uids))
			if err := c.cl.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
				return fmt.Errorf("imap store \\Deleted: %w", err)
			}
			if err := expunge(c.cl, set); err != nil {
				return err
			}
		}
		return nil
	})
}

// expunge removes \Deleted messages — by UID (UIDPLUS) so only the targeted ones
// go, else a plain EXPUNGE of the folder's \Deleted set.
func expunge(cl *imapclient.Client, set imap.UIDSet) error {
	if cl.Caps().Has(imap.CapUIDPlus) {
		logging.Trace("imapbackend: expunge", "cmd", "UID EXPUNGE")
		if _, err := cl.UIDExpunge(set).Collect(); err != nil {
			return fmt.Errorf("imap uid expunge: %w", err)
		}
		return nil
	}
	logging.Trace("imapbackend: expunge", "cmd", "EXPUNGE (no UIDPLUS)")
	if _, err := cl.Expunge().Collect(); err != nil {
		return fmt.Errorf("imap expunge: %w", err)
	}
	return nil
}

// Send submits a message over SMTP, then APPENDs a copy to the Sent folder (SMTP
// delivery doesn't file it in IMAP the way Gmail's API does). The returned id is
// empty — the Sent copy surfaces through the next incremental.
func (b *Backend) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	logging.TraceContext(ctx, "imapbackend: send", "account", b.cfg.Email, "bytes", len(raw), "threadID", threadID)
	from, to, cleaned, err := smtpEnvelope(raw)
	if err != nil {
		return "", err
	}
	if len(to) == 0 {
		return "", fmt.Errorf("imap send: no recipients")
	}
	logging.Trace("imapbackend: send envelope", "from", from, "to", to, "recipients", len(to))
	if err := b.smtpSend(ctx, from, to, cleaned); err != nil {
		logging.TraceContext(ctx, "imapbackend: send failed", "account", b.cfg.Email, "err", err)
		return "", err
	}
	logging.TraceContext(ctx, "imapbackend: send ok", "account", b.cfg.Email)
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

func (b *Backend) smtpSend(ctx context.Context, from string, to []string, msg []byte) error {
	start := time.Now()
	addr := net.JoinHostPort(b.cfg.SMTPHost, strconv.Itoa(b.cfg.SMTPPort))
	tlsCfg := &tls.Config{ServerName: b.cfg.SMTPHost}
	logging.Trace("imapbackend: smtp dial", "addr", addr, "security", string(b.cfg.SMTPSecurity), "dialTimeout", dialTimeout)
	// Dial raw + count (below TLS), then build the SMTP client over the wrapped
	// conn so SMTP traffic is included in the byte stats. The dialer bounds the
	// connect with a timeout and honors ctx (like the IMAP dial), so a wrong or
	// unreachable SMTP host fails fast instead of hanging a send on the OS TCP
	// timeout.
	dialer := &net.Dialer{Timeout: dialTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	cc := &countingConn{Conn: raw, stats: b.stats}
	var c *smtp.Client
	switch b.cfg.SMTPSecurity {
	case SecurityTLS:
		c = smtp.NewClient(tls.Client(cc, tlsCfg))
	case SecuritySTARTTLS:
		c, err = smtp.NewClientStartTLS(cc, tlsCfg)
	case SecurityNone:
		c = smtp.NewClient(cc)
	default:
		_ = raw.Close()
		return fmt.Errorf("imap: unknown smtp security %q", b.cfg.SMTPSecurity)
	}
	if err != nil {
		_ = raw.Close()
		return fmt.Errorf("smtp connect %s: %w", addr, err)
	}
	defer func() { _ = c.Close() }()
	sc, err := b.cred.smtpSASL()
	if err != nil {
		return err
	}
	if err := c.Auth(sc); err != nil {
		logging.Trace("imapbackend: smtp auth failed", "addr", addr, "err", err)
		return fmt.Errorf("smtp auth: %w", err)
	}
	if err := c.SendMail(from, to, bytes.NewReader(msg)); err != nil {
		logging.Trace("imapbackend: smtp sendmail failed", "addr", addr, "dur", time.Since(start), "err", err)
		return fmt.Errorf("smtp send: %w", err)
	}
	logging.Trace("imapbackend: smtp sent", "addr", addr, "bytes", len(msg), "recipients", len(to), "dur", time.Since(start))
	return nil
}

// appendToSent files a sent message in the Sent folder (best-effort).
func (b *Backend) appendToSent(msg []byte) {
	_ = b.withConn(func(c *conn) error {
		if err := b.ensureFolders(c); err != nil {
			return err
		}
		b.folderMu.Lock()
		sent := b.labelToFolder[model.LabelSent]
		b.folderMu.Unlock()
		if sent != "" {
			logging.Trace("imapbackend: append to sent", "folder", sent, "bytes", len(msg))
			if _, err := appendMessage(c.cl, sent, msg, imap.FlagSeen); err != nil {
				logging.Trace("imapbackend: append to sent failed", "folder", sent, "err", err)
			}
		} else {
			logging.Trace("imapbackend: append to sent skipped (no Sent folder)", "account", b.cfg.Email)
		}
		return nil
	})
}

// appendMessage APPENDs msg to a mailbox with the given flags.
func appendMessage(cl *imapclient.Client, mailbox string, msg []byte, flags ...imap.Flag) (*imap.AppendData, error) {
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
	logging.TraceContext(ctx, "imapbackend: save draft", "account", b.cfg.Email, "bytes", len(raw), "threadID", threadID)
	var draftID string
	err := b.withConn(func(c *conn) error {
		if err := b.ensureFolders(c); err != nil {
			return err
		}
		b.folderMu.Lock()
		drafts := b.labelToFolder[model.LabelDraft]
		b.folderMu.Unlock()
		if drafts == "" {
			return fmt.Errorf("imap: no Drafts folder")
		}
		logging.Trace("imapbackend: append draft", "folder", drafts, "bytes", len(raw))
		ad, err := appendMessage(c.cl, drafts, raw, imap.FlagDraft)
		if err != nil {
			return fmt.Errorf("imap append draft: %w", err)
		}
		if ad != nil && ad.UID != 0 {
			draftID = msgID(drafts, ad.UIDValidity, ad.UID)
		}
		logging.Trace("imapbackend: save draft ok", "id", draftID)
		return nil
	})
	return draftID, err
}

// UpdateDraft replaces an existing draft: IMAP has no in-place edit, so append
// the replacement FIRST and only then delete the old message — if the append
// fails the user still has the old draft, and if the delete fails the worst
// case is a duplicate draft, never a lost one.
func (b *Backend) UpdateDraft(ctx context.Context, draftID string, raw []byte, threadID string) (string, error) {
	logging.TraceContext(ctx, "imapbackend: update draft", "account", b.cfg.Email, "draftID", draftID, "bytes", len(raw))
	newID, err := b.SaveDraft(ctx, raw, threadID)
	if err != nil {
		logging.TraceContext(ctx, "imapbackend: update draft append failed (old draft kept)", "draftID", draftID, "err", err)
		return "", err
	}
	if err := b.DeleteDraft(ctx, draftID); err != nil {
		// Non-fatal: the new draft is safely stored; a stale duplicate may remain
		// until the user deletes it or the next update succeeds.
		logging.TraceContext(ctx, "imapbackend: update draft: delete of old draft failed (duplicate may remain)", "draftID", draftID, "newID", newID, "err", err)
	}
	return newID, nil
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
	logging.TraceContext(ctx, "imapbackend: fetch attachment", "id", msgIDArg, "attID", attID)
	raw, err := b.fetchRaw(msgIDArg)
	if err != nil {
		return nil, err
	}
	idx, err := strconv.Atoi(attID)
	if err != nil {
		return nil, fmt.Errorf("imap: bad attachment id %q", attID)
	}
	data, err := attachmentBytes(raw, idx)
	if err != nil {
		logging.TraceContext(ctx, "imapbackend: fetch attachment failed", "id", msgIDArg, "attID", attID, "err", err)
		return nil, err
	}
	logging.TraceContext(ctx, "imapbackend: fetch attachment ok", "id", msgIDArg, "attID", attID, "bytes", len(data))
	return data, nil
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
