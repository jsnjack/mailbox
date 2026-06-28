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

// Security selects the transport for the IMAP connection.
type Security string

const (
	SecurityTLS      Security = "tls"      // implicit TLS (typically port 993)
	SecuritySTARTTLS Security = "starttls" // upgrade a plaintext port 143 connection
	SecurityNone     Security = "none"     // plaintext — tests/localhost only
)

// Config describes how to reach an IMAP server (and its SMTP submission server
// for sending).
type Config struct {
	Host     string
	Port     int
	Security Security
	Username string // usually the email address
	Email    string // the account's address (for Profile + SMTP MAIL FROM)

	SMTPHost     string
	SMTPPort     int
	SMTPSecurity Security
}

// poolSize bounds concurrent IMAP connections. IMAP is stateful (one SELECT per
// connection), so the engine's fan-out (backfill, incremental metadata fetch) is
// served by a small pool rather than serialized on one connection.
const poolSize = 4

// conn is one pooled IMAP connection plus the mailbox it currently has SELECTed
// (cached so a fan-out of fetches against the same folder skips re-SELECTing).
type conn struct {
	cl       *imapclient.Client
	selected string
	selData  *imap.SelectData
}

// Backend implements backend.Backend over one IMAP account, with a small
// connection pool for concurrency.
type Backend struct {
	cfg       Config
	accountID int64
	cred      Credential

	sem  chan struct{} // bounds live connections to poolSize
	idle chan *conn    // reusable idle connections

	folderMu      sync.Mutex        // guards the folder caches below
	folderToLabel map[string]string // mailbox name → label id (special-use mapped)
	labelToFolder map[string]string // system label id → mailbox name (for moves)
	archiveFolder string            // the \Archive mailbox, if any (for archive)
	labels        []model.Label     // cached LIST → domain labels
	synced        []string          // mailboxes to sync, derived once from LIST
	foldersLoaded bool              // LIST done
}

// New builds an IMAP backend. cred authenticates both the IMAP and SMTP
// connections (PasswordAuth or OAuthAuth).
func New(cfg Config, accountID int64, cred Credential) *Backend {
	return &Backend{
		cfg: cfg, accountID: accountID, cred: cred,
		sem:           make(chan struct{}, poolSize),
		idle:          make(chan *conn, poolSize),
		folderToLabel: map[string]string{},
	}
}

var _ backend.Backend = (*Backend)(nil)

// --- connection pool ---

// dial opens and logs in a new connection. handler, when non-nil, receives
// unsolicited server data (used by Watch for IDLE).
func (b *Backend) dial(handler *imapclient.UnilateralDataHandler) (*imapclient.Client, error) {
	addr := net.JoinHostPort(b.cfg.Host, strconv.Itoa(b.cfg.Port))
	opts := &imapclient.Options{
		TLSConfig:             &tls.Config{ServerName: b.cfg.Host},
		UnilateralDataHandler: handler,
	}
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
		cl, err = imapclient.DialInsecure(addr, opts)
	default:
		return nil, fmt.Errorf("imap: unknown security %q", b.cfg.Security)
	}
	if err != nil {
		return nil, fmt.Errorf("imap dial %s: %w", addr, err)
	}
	if err := b.cred.imapLogin(cl); err != nil {
		_ = cl.Close()
		return nil, fmt.Errorf("imap login: %w", err)
	}
	return cl, nil
}

// acquire takes a connection from the pool (reusing an idle one or dialing a new
// one), blocking until a slot is free.
func (b *Backend) acquire() (*conn, error) {
	b.sem <- struct{}{}
	select {
	case c := <-b.idle:
		return c, nil
	default:
	}
	cl, err := b.dial(nil)
	if err != nil {
		<-b.sem
		return nil, err
	}
	return &conn{cl: cl}, nil
}

// release returns a healthy connection to the pool, or closes a failed one (so
// the next acquire re-dials).
func (b *Backend) release(c *conn, healthy bool) {
	if healthy {
		select {
		case b.idle <- c:
			<-b.sem
			return
		default:
		}
	}
	_ = c.cl.Close()
	<-b.sem
}

// withConn runs fn on a pooled connection. The connection is returned to the pool
// on success and closed on any error (conservative — an error may have left it in
// a bad state).
func (b *Backend) withConn(fn func(*conn) error) error {
	c, err := b.acquire()
	if err != nil {
		return err
	}
	err = fn(c)
	b.release(c, err == nil)
	return err
}

// selectMailbox SELECTs mailbox on this connection (skipping a redundant SELECT
// when it's already current) and returns its status. condStore requests
// CONDSTORE when the server supports it.
func (c *conn) selectMailbox(mailbox string, condStore bool) (*imap.SelectData, error) {
	if c.selected == mailbox && c.selData != nil {
		return c.selData, nil
	}
	opts := &imap.SelectOptions{CondStore: condStore && c.cl.Caps().Has(imap.CapCondStore)}
	data, err := c.cl.Select(mailbox, opts).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap select %q: %w", mailbox, err)
	}
	c.selected, c.selData = mailbox, data
	return data, nil
}

// Close shuts down the idle connections in the pool. In-flight operations finish
// and close their own connections on release.
func (b *Backend) Close() {
	for {
		select {
		case c := <-b.idle:
			_ = c.cl.Close()
		default:
			return
		}
	}
}

// labelFor returns the label id a mailbox maps to (folder caches are guarded
// because fan-out fetches read them concurrently with Labels populating them).
func (b *Backend) labelFor(mailbox string) string {
	b.folderMu.Lock()
	defer b.folderMu.Unlock()
	if id := b.folderToLabel[mailbox]; id != "" {
		return id
	}
	return labelForMailbox(mailbox)
}

// --- backend.Backend: read path ---

// Profile verifies connectivity and seeds the incremental-sync cursor with the
// current state of every synced folder, so the first incremental diffs against
// the post-backfill baseline (mail arriving during backfill is then caught as a
// change rather than missed).
func (b *Backend) Profile(ctx context.Context) (backend.Profile, error) {
	var cur string
	err := b.withConn(func(c *conn) error {
		var e error
		cur, e = b.buildProfileCursor(c)
		return e
	})
	if err != nil {
		return backend.Profile{}, err
	}
	return backend.Profile{Email: b.cfg.Email, Cursor: cur}, nil
}

// Labels lists the server's folders as domain labels, mapping IMAP special-use
// attributes (\Sent \Drafts \Trash \Junk) and INBOX to the app's system label
// ids so the existing folder views work. It also records the mailbox→label
// mapping for FetchMetadata.
func (b *Backend) Labels(ctx context.Context) ([]model.Label, error) {
	if err := b.withConn(b.ensureFolders); err != nil {
		return nil, err
	}
	b.folderMu.Lock()
	defer b.folderMu.Unlock()
	return b.labels, nil
}

// ensureFolders runs LIST once and derives, in one pass: the domain label list,
// the folder→label and (system) label→folder maps, and the syncable folder set
// (excluding \All/\Flagged/\Important virtuals so Gmail's All Mail doesn't
// duplicate everything). Idempotent; guarded so fan-out readers see a consistent
// cache.
func (b *Backend) ensureFolders(c *conn) error {
	b.folderMu.Lock()
	defer b.folderMu.Unlock()
	if b.foldersLoaded {
		return nil
	}
	data, err := c.cl.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		return fmt.Errorf("imap list: %w", err)
	}
	folderToLabel := map[string]string{}
	labelToFolder := map[string]string{}
	archive := ""
	var labels []model.Label
	var synced []string
	for _, d := range data {
		if hasAttr(d.Attrs, imap.MailboxAttrNonExistent) || hasAttr(d.Attrs, imap.MailboxAttrNoSelect) {
			continue
		}
		if hasAttr(d.Attrs, imap.MailboxAttrArchive) {
			archive = d.Mailbox
		}
		id := folderLabelID(d)
		folderToLabel[d.Mailbox] = id
		ltype := model.LabelUser
		if isSystemLabel(id) {
			ltype = model.LabelSystem
			labelToFolder[id] = d.Mailbox
		}
		labels = append(labels, model.Label{
			AccountID: b.accountID, GmailID: id, Name: displayName(d.Mailbox, d.Delim), Type: ltype,
		})
		if !hasAttr(d.Attrs, imap.MailboxAttrAll) &&
			!hasAttr(d.Attrs, imap.MailboxAttrFlagged) &&
			!hasAttr(d.Attrs, imap.MailboxAttrImportant) {
			synced = append(synced, d.Mailbox)
		}
	}
	sort.Strings(synced)
	b.folderToLabel, b.labelToFolder, b.labels, b.synced = folderToLabel, labelToFolder, labels, synced
	b.archiveFolder = archive
	b.foldersLoaded = true
	return nil
}

// SearchIDs lists message ids for backfill across all synced folders, newest
// first within each (highest UID first), capped to max total. query is ignored
// (provider search is a later addition).
func (b *Backend) SearchIDs(ctx context.Context, query string, max int) ([]string, error) {
	var ids []string
	err := b.withConn(func(c *conn) error {
		folders, err := b.folders(c)
		if err != nil {
			return err
		}
		for _, f := range folders {
			sel, err := c.selectMailbox(f, false)
			if err != nil {
				return err
			}
			sd, err := c.cl.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
			if err != nil {
				return fmt.Errorf("imap uid search %q: %w", f, err)
			}
			uids := sd.AllUIDs()
			sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] }) // newest first
			for _, u := range uids {
				ids = append(ids, msgID(f, sel.UIDValidity, u))
				if max > 0 && len(ids) >= max {
					return nil
				}
			}
		}
		return nil
	})
	return ids, err
}

// FetchMetadata fetches one message's envelope + flags and converts it.
func (b *Backend) FetchMetadata(ctx context.Context, id string) (model.Message, error) {
	mailbox, uidv, uid, err := parseMsgID(id)
	if err != nil {
		return model.Message{}, err
	}
	var out model.Message
	err = b.withConn(func(c *conn) error {
		sel, err := c.selectMailbox(mailbox, false)
		if err != nil {
			return err
		}
		if sel.UIDValidity != uidv {
			return fmt.Errorf("imap: stale id %q (uidvalidity %d != %d)", id, uidv, sel.UIDValidity)
		}
		// References isn't part of the IMAP ENVELOPE, so fetch that one header too
		// — it carries the thread's ancestry (used to compute a stable thread root).
		refSection := &imap.FetchItemBodySection{
			Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"References"}, Peek: true,
		}
		bufs, err := c.cl.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
			Envelope: true, Flags: true, InternalDate: true, RFC822Size: true, UID: true,
			BodySection: []*imap.FetchItemBodySection{refSection},
		}).Collect()
		if err != nil {
			return fmt.Errorf("imap fetch metadata: %w", err)
		}
		if len(bufs) == 0 {
			return fmt.Errorf("imap: uid %d not found in %q", uid, mailbox)
		}
		refs := parseReferences(bufs[0].FindBodySection(refSection))
		out = b.toMessage(mailbox, uidv, bufs[0], refs)
		return nil
	})
	return out, err
}

// FetchBody fetches and parses a message's full body + attachment metadata.
func (b *Backend) FetchBody(ctx context.Context, id string) (model.MessageBody, []model.Attachment, error) {
	raw, err := b.fetchRaw(id)
	if err != nil {
		return model.MessageBody{}, nil, err
	}
	return parseBody(raw)
}

// fetchRaw returns a message's full raw RFC 5322 bytes (BODY[], peeked so it
// doesn't set \Seen). Shared by FetchBody and FetchAttachment.
func (b *Backend) fetchRaw(id string) ([]byte, error) {
	mailbox, uidv, uid, err := parseMsgID(id)
	if err != nil {
		return nil, err
	}
	var raw []byte
	err = b.withConn(func(c *conn) error {
		sel, err := c.selectMailbox(mailbox, false)
		if err != nil {
			return err
		}
		if sel.UIDValidity != uidv {
			return fmt.Errorf("imap: stale id %q", id)
		}
		section := &imap.FetchItemBodySection{Peek: true}
		bufs, err := c.cl.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
			BodySection: []*imap.FetchItemBodySection{section},
		}).Collect()
		if err != nil {
			return fmt.Errorf("imap fetch body: %w", err)
		}
		if len(bufs) == 0 {
			return fmt.Errorf("imap: uid %d not found", uid)
		}
		raw = bufs[0].FindBodySection(section)
		return nil
	})
	return raw, err
}

// Changes diffs every synced folder against the cursor (a per-folder UID-set +
// modseq snapshot) and returns the message ids to upsert (new + flag-changed)
// and delete (vanished), plus the next cursor. A UIDVALIDITY change re-syncs that
// folder wholesale. (Mutations, send, and drafts live in mutate.go.)
func (b *Backend) Changes(ctx context.Context, cur string) (upserts, deletes []string, next string, err error) {
	var nextCur cursor
	err = b.withConn(func(c *conn) error {
		var e error
		upserts, deletes, nextCur, e = b.computeChanges(c, decodeCursor(cur))
		return e
	})
	if err != nil {
		return nil, nil, "", err
	}
	return upserts, deletes, nextCur.encode(), nil
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

	m.Labels = []string{b.labelFor(mailbox)}
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
			// IMAP has no per-attachment id; use the ordinal so FetchAttachment can
			// re-derive the part from a re-fetched body.
			atts = append(atts, model.Attachment{
				GmailAttID: strconv.Itoa(len(atts) + 1),
				Filename:   filename, MimeType: ct, SizeBytes: n,
			})
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
