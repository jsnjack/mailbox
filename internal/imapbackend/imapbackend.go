// Package imapbackend implements backend.Backend over an IMAP server (read path:
// connect, list folders, backfill, fetch bodies). A single connection is guarded
// by a mutex — IMAP is stateful, one SELECT at a time; a connection pool for
// concurrency is a later optimization. Incremental sync (QRESYNC/CONDSTORE),
// mutations, SMTP send, and threading land in later phases.
package imapbackend

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gomail "github.com/emersion/go-message/mail"
	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/model"
)

// errNotImplemented marks Backend methods reserved for later phases (mutations,
// send, drafts). They satisfy the interface without pretending to work.
var errNotImplemented = errors.New("imap: not implemented yet")

// Security selects the transport for the IMAP connection.
type Security string

const (
	SecurityTLS      Security = "tls"      // implicit TLS (typically port 993)
	SecuritySTARTTLS Security = "starttls" // upgrade a plaintext port 143 connection
	SecurityNone     Security = "none"     // plaintext — tests/localhost only
)

// Config describes how to reach an IMAP server.
type Config struct {
	Host     string
	Port     int
	Security Security
	Username string // usually the email address
	Email    string // the account's address (for Profile)
}

// Backend implements backend.Backend over one IMAP account.
type Backend struct {
	cfg       Config
	accountID int64
	password  string

	mu            sync.Mutex
	cl            *imapclient.Client
	selected      string            // currently SELECTed mailbox ("" = none)
	folderToLabel map[string]string // mailbox name → label id (special-use mapped)
	synced        []string          // mailboxes to sync, derived once from LIST
}

// New builds an IMAP backend. password authenticates with LOGIN/PLAIN; OAuth
// (XOAUTH2) is wired in a later phase via a separate credential path.
func New(cfg Config, accountID int64, password string) *Backend {
	return &Backend{cfg: cfg, accountID: accountID, password: password, folderToLabel: map[string]string{}}
}

var _ backend.Backend = (*Backend)(nil)

// --- connection management (caller holds mu) ---

func (b *Backend) dial() (*imapclient.Client, error) {
	addr := net.JoinHostPort(b.cfg.Host, strconv.Itoa(b.cfg.Port))
	opts := &imapclient.Options{TLSConfig: &tls.Config{ServerName: b.cfg.Host}}
	var (
		cl  *imapclient.Client
		err error
	)
	switch b.cfg.Security {
	case SecurityTLS:
		cl, err = imapclient.DialTLS(addr, opts)
	case SecuritySTARTTLS:
		cl, err = imapclient.DialStartTLS(addr, opts)
	case SecurityNone:
		cl, err = imapclient.DialInsecure(addr, nil)
	default:
		return nil, fmt.Errorf("imap: unknown security %q", b.cfg.Security)
	}
	if err != nil {
		return nil, fmt.Errorf("imap dial %s: %w", addr, err)
	}
	if err := cl.Login(b.cfg.Username, b.password).Wait(); err != nil {
		_ = cl.Close()
		return nil, fmt.Errorf("imap login: %w", err)
	}
	return cl, nil
}

// conn returns a live, logged-in client, dialing on first use.
func (b *Backend) conn() (*imapclient.Client, error) {
	if b.cl != nil {
		return b.cl, nil
	}
	cl, err := b.dial()
	if err != nil {
		return nil, err
	}
	b.cl, b.selected = cl, ""
	return cl, nil
}

// reset drops the connection after an error so the next op reconnects cleanly.
func (b *Backend) reset() {
	if b.cl != nil {
		_ = b.cl.Close()
		b.cl, b.selected = nil, ""
	}
}

// selectMailbox SELECTs mailbox (idempotent on the already-selected box would
// still reset state, so re-SELECT only when changing) and returns its status —
// callers need UIDVALIDITY to build/verify message ids.
func (b *Backend) selectMailbox(cl *imapclient.Client, mailbox string) (*imap.SelectData, error) {
	data, err := cl.Select(mailbox, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap select %q: %w", mailbox, err)
	}
	b.selected = mailbox
	return data, nil
}

// Close releases the connection.
func (b *Backend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reset()
}

// --- backend.Backend: read path ---

// Profile verifies connectivity and seeds the incremental-sync cursor with the
// current state of every synced folder, so the first incremental diffs against
// the post-backfill baseline (mail arriving during backfill is then caught as a
// change rather than missed).
func (b *Backend) Profile(ctx context.Context) (backend.Profile, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cl, err := b.conn()
	if err != nil {
		return backend.Profile{}, err
	}
	cur, err := b.buildProfileCursor(cl)
	if err != nil {
		b.reset()
		return backend.Profile{}, err
	}
	return backend.Profile{Email: b.cfg.Email, Cursor: cur}, nil
}

// Labels lists the server's folders as domain labels, mapping IMAP special-use
// attributes (\Sent \Drafts \Trash \Junk) and INBOX to the app's system label
// ids so the existing folder views work. It also records the mailbox→label
// mapping for FetchMetadata.
func (b *Backend) Labels(ctx context.Context) ([]model.Label, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cl, err := b.conn()
	if err != nil {
		return nil, err
	}
	data, err := cl.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		b.reset()
		return nil, fmt.Errorf("imap list: %w", err)
	}
	mapping := map[string]string{}
	out := make([]model.Label, 0, len(data))
	for _, d := range data {
		if hasAttr(d.Attrs, imap.MailboxAttrNonExistent) {
			continue // a hierarchy node with no messages
		}
		id := folderLabelID(d)
		mapping[d.Mailbox] = id
		ltype := model.LabelUser
		if isSystemLabel(id) {
			ltype = model.LabelSystem
		}
		out = append(out, model.Label{
			AccountID: b.accountID,
			GmailID:   id,
			Name:      displayName(d.Mailbox, d.Delim),
			Type:      ltype,
		})
	}
	b.folderToLabel = mapping
	return out, nil
}

// SearchIDs lists message ids for backfill across all synced folders, newest
// first within each (highest UID first), capped to max total. query is ignored
// (provider search is a later addition).
func (b *Backend) SearchIDs(ctx context.Context, query string, max int) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cl, err := b.conn()
	if err != nil {
		return nil, err
	}
	folders, err := b.folders(cl)
	if err != nil {
		b.reset()
		return nil, err
	}
	var ids []string
	for _, f := range folders {
		sel, err := b.selectMailbox(cl, f)
		if err != nil {
			b.reset()
			return nil, err
		}
		sd, err := cl.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
		if err != nil {
			b.reset()
			return nil, fmt.Errorf("imap uid search %q: %w", f, err)
		}
		uids := sd.AllUIDs()
		sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] }) // newest first
		for _, u := range uids {
			ids = append(ids, msgID(f, sel.UIDValidity, u))
			if max > 0 && len(ids) >= max {
				return ids, nil
			}
		}
	}
	return ids, nil
}

// FetchMetadata fetches one message's envelope + flags and converts it.
func (b *Backend) FetchMetadata(ctx context.Context, id string) (model.Message, error) {
	mailbox, uidv, uid, err := parseMsgID(id)
	if err != nil {
		return model.Message{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cl, err := b.conn()
	if err != nil {
		return model.Message{}, err
	}
	sel, err := b.selectMailbox(cl, mailbox)
	if err != nil {
		b.reset()
		return model.Message{}, err
	}
	if sel.UIDValidity != uidv {
		return model.Message{}, fmt.Errorf("imap: stale id %q (uidvalidity %d != %d)", id, uidv, sel.UIDValidity)
	}
	// References isn't part of the IMAP ENVELOPE, so fetch that one header too —
	// it carries the thread's ancestry (used to compute a stable thread root).
	refSection := &imap.FetchItemBodySection{
		Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"References"}, Peek: true,
	}
	bufs, err := cl.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
		Envelope: true, Flags: true, InternalDate: true, RFC822Size: true, UID: true,
		BodySection: []*imap.FetchItemBodySection{refSection},
	}).Collect()
	if err != nil {
		b.reset()
		return model.Message{}, fmt.Errorf("imap fetch metadata: %w", err)
	}
	if len(bufs) == 0 {
		return model.Message{}, fmt.Errorf("imap: uid %d not found in %q", uid, mailbox)
	}
	refs := parseReferences(bufs[0].FindBodySection(refSection))
	return b.toMessage(mailbox, uidv, bufs[0], refs), nil
}

// FetchBody fetches and parses a message's full body + attachment metadata.
func (b *Backend) FetchBody(ctx context.Context, id string) (model.MessageBody, []model.Attachment, error) {
	mailbox, uidv, uid, err := parseMsgID(id)
	if err != nil {
		return model.MessageBody{}, nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cl, err := b.conn()
	if err != nil {
		return model.MessageBody{}, nil, err
	}
	sel, err := b.selectMailbox(cl, mailbox)
	if err != nil {
		b.reset()
		return model.MessageBody{}, nil, err
	}
	if sel.UIDValidity != uidv {
		return model.MessageBody{}, nil, fmt.Errorf("imap: stale id %q", id)
	}
	section := &imap.FetchItemBodySection{Peek: true} // full message, don't set \Seen
	bufs, err := cl.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		b.reset()
		return model.MessageBody{}, nil, fmt.Errorf("imap fetch body: %w", err)
	}
	if len(bufs) == 0 {
		return model.MessageBody{}, nil, fmt.Errorf("imap: uid %d not found", uid)
	}
	return parseBody(bufs[0].FindBodySection(section))
}

// --- backend.Backend: stubs for later phases ---

// Changes diffs every synced folder against the cursor (a per-folder UID-set +
// modseq snapshot) and returns the message ids to upsert (new + flag-changed)
// and delete (vanished), plus the next cursor. A UIDVALIDITY change re-syncs that
// folder wholesale.
func (b *Backend) Changes(ctx context.Context, cur string) (upserts, deletes []string, next string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cl, err := b.conn()
	if err != nil {
		return nil, nil, "", err
	}
	up, del, nextCur, err := b.computeChanges(cl, decodeCursor(cur))
	if err != nil {
		b.reset()
		return nil, nil, "", err
	}
	return up, del, nextCur.encode(), nil
}

func (b *Backend) FetchAttachment(ctx context.Context, msgID, attID string) ([]byte, error) {
	return nil, errNotImplemented
}
func (b *Backend) ApplyLabels(ctx context.Context, ids []string, add, remove []string) error {
	return errNotImplemented
}
func (b *Backend) Delete(ctx context.Context, ids []string) error { return errNotImplemented }
func (b *Backend) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	return "", errNotImplemented
}
func (b *Backend) SaveDraft(ctx context.Context, raw []byte, threadID string) (string, error) {
	return "", errNotImplemented
}
func (b *Backend) UpdateDraft(ctx context.Context, draftID string, raw []byte, threadID string) (string, error) {
	return "", errNotImplemented
}
func (b *Backend) DeleteDraft(ctx context.Context, draftID string) error { return errNotImplemented }
func (b *Backend) FindDraftID(ctx context.Context, msgID string) (string, error) {
	return "", errNotImplemented
}

// --- conversions / helpers ---

// toMessage converts a fetched message into the domain model. Caller holds mu
// (it reads folderToLabel).
func (b *Backend) toMessage(mailbox string, uidv uint32, buf *imapclient.FetchMessageBuffer, refs []string) model.Message {
	id := msgID(mailbox, uidv, buf.UID)
	m := model.Message{
		AccountID:    b.accountID,
		GmailID:      id,
		ThreadID:     id, // overridden below once the reference chain is known
		InternalDate: buf.InternalDate,
		SizeEstimate: buf.RFC822Size,
	}
	if env := buf.Envelope; env != nil {
		m.Subject = env.Subject
		m.RFC822MsgID = bracket(env.MessageID)
		m.InReplyTo = bracketAll(env.InReplyTo)
		m.References = bracketAll(refs)
		// Group the conversation under the root ancestor's Message-ID (References
		// is oldest-first), so every reply in a chain shares one thread id —
		// across folders too (a sent reply files with the original it answers).
		m.ThreadID = threadRoot(refs, env.InReplyTo, env.MessageID, id)
		if len(env.From) > 0 {
			m.FromName = env.From[0].Name
			m.FromAddr = env.From[0].Addr()
		}
		if len(env.ReplyTo) > 0 {
			m.ReplyTo = env.ReplyTo[0].Addr()
		}
		m.ToAddrs = addrList(env.To)
		m.CcAddrs = addrList(env.Cc)
		if !env.Date.IsZero() && m.InternalDate.IsZero() {
			m.InternalDate = env.Date
		}
	}
	seen, flagged := false, false
	for _, f := range buf.Flags {
		switch f {
		case imap.FlagSeen:
			seen = true
		case imap.FlagFlagged:
			flagged = true
		}
	}
	m.IsUnread = !seen
	m.IsStarred = flagged

	label := b.folderToLabel[mailbox]
	if label == "" {
		label = labelForMailbox(mailbox)
	}
	m.Labels = []string{label}
	if m.IsUnread {
		m.Labels = append(m.Labels, model.LabelUnread)
	}
	if m.IsStarred {
		m.Labels = append(m.Labels, model.LabelStarred)
	}
	return m
}

// parseBody extracts text/html bodies and attachment metadata from a raw RFC 5322
// message using go-message. On a parse failure it falls back to treating the whole
// payload as plain text, so a malformed message still renders something.
func parseBody(raw []byte) (model.MessageBody, []model.Attachment, error) {
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return model.MessageBody{Text: string(raw)}, nil, nil
	}
	var (
		body model.MessageBody
		atts []model.Attachment
	)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
		switch h := part.Header.(type) {
		case *gomail.InlineHeader:
			ct, _, _ := h.ContentType()
			data, _ := io.ReadAll(part.Body)
			if strings.EqualFold(ct, "text/html") {
				body.HTML = string(data)
			} else if body.Text == "" {
				body.Text = string(data)
			}
		case *gomail.AttachmentHeader:
			filename, _ := h.Filename()
			ct, _, _ := h.ContentType()
			n, _ := io.Copy(io.Discard, part.Body)
			atts = append(atts, model.Attachment{Filename: filename, MimeType: ct, SizeBytes: n})
		}
	}
	return body, atts, nil
}

// msgID encodes a stable provider id for an IMAP message: its mailbox,
// UIDVALIDITY, and UID. The mailbox is last so colons in folder names survive.
func msgID(mailbox string, uidValidity uint32, uid imap.UID) string {
	return fmt.Sprintf("imap:%d:%d:%s", uidValidity, uint32(uid), mailbox)
}

// parseMsgID is the inverse of msgID.
func parseMsgID(id string) (mailbox string, uidValidity uint32, uid imap.UID, err error) {
	rest, ok := strings.CutPrefix(id, "imap:")
	if !ok {
		return "", 0, 0, fmt.Errorf("imap: not an imap message id: %q", id)
	}
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) != 3 {
		return "", 0, 0, fmt.Errorf("imap: malformed message id: %q", id)
	}
	uv, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return "", 0, 0, fmt.Errorf("imap: bad uidvalidity in %q: %w", id, err)
	}
	u, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return "", 0, 0, fmt.Errorf("imap: bad uid in %q: %w", id, err)
	}
	return parts[2], uint32(uv), imap.UID(u), nil
}

// folderLabelID maps a folder to a stable label id: special-use → the app's
// system ids, INBOX → INBOX, anything else → its own name.
func folderLabelID(d *imap.ListData) string {
	for _, a := range d.Attrs {
		switch a {
		case imap.MailboxAttrSent:
			return model.LabelSent
		case imap.MailboxAttrDrafts:
			return model.LabelDraft
		case imap.MailboxAttrTrash:
			return model.LabelTrash
		case imap.MailboxAttrJunk:
			return model.LabelSpam
		}
	}
	return labelForMailbox(d.Mailbox)
}

// labelForMailbox maps a bare mailbox name (no special-use info) to a label id.
func labelForMailbox(mailbox string) string {
	if strings.EqualFold(mailbox, "INBOX") {
		return model.LabelInbox
	}
	return mailbox
}

func isSystemLabel(id string) bool {
	switch id {
	case model.LabelInbox, model.LabelSent, model.LabelDraft, model.LabelTrash,
		model.LabelSpam, model.LabelStarred, model.LabelUnread, model.LabelImportant:
		return true
	}
	return false
}

func hasAttr(attrs []imap.MailboxAttr, want imap.MailboxAttr) bool {
	for _, a := range attrs {
		if a == want {
			return true
		}
	}
	return false
}

// displayName is the leaf of a hierarchical mailbox path (e.g. "Work/Projects"
// with delim '/' → "Projects"); INBOX keeps its conventional name.
func displayName(mailbox string, delim rune) string {
	if strings.EqualFold(mailbox, "INBOX") {
		return "Inbox"
	}
	if delim != 0 {
		if i := strings.LastIndexByte(mailbox, byte(delim)); i >= 0 && i+1 < len(mailbox) {
			return mailbox[i+1:]
		}
	}
	return mailbox
}

func addrList(as []imap.Address) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		if a.Name != "" {
			parts = append(parts, fmt.Sprintf("%s <%s>", a.Name, a.Addr()))
		} else if e := a.Addr(); e != "" {
			parts = append(parts, e)
		}
	}
	return strings.Join(parts, ", ")
}

// parseReferences extracts the (bracket-stripped) message-ids from a raw
// "References: <a@x> <b@y>" header section, oldest-first.
func parseReferences(headerBytes []byte) []string {
	s := string(headerBytes)
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[i+1:] // drop the "References:" name
	}
	var out []string
	for _, tok := range strings.Fields(s) {
		if id := strings.Trim(tok, "<>"); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// threadRoot returns the conversation's root id: the oldest References ancestor
// if any, else the immediate parent (In-Reply-To), else the message's own
// Message-ID, else its provider id. All inputs are bracket-stripped so messages
// in one chain resolve to the same root. fallback is the provider id.
func threadRoot(refs, inReplyTo []string, messageID, fallback string) string {
	if len(refs) > 0 {
		return refs[0]
	}
	if len(inReplyTo) > 0 && inReplyTo[0] != "" {
		return inReplyTo[0]
	}
	if messageID != "" {
		return messageID
	}
	return fallback
}

// bracket restores the angle brackets go-imap strips from a Message-ID.
func bracket(id string) string {
	if id == "" {
		return ""
	}
	return "<" + id + ">"
}

func bracketAll(ids []string) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, bracket(id))
	}
	return strings.Join(parts, " ")
}
