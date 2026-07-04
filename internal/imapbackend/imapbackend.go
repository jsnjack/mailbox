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
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gomail "github.com/emersion/go-message/mail"
	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/logging"
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

// dialTimeout/loginTimeout bound connection setup so a wrong or unreachable host
// fails fast (e.g. in the Test & Add dialog) instead of hanging on the OS TCP
// timeout. The deadline is cleared after login, so a minutes-long IDLE read on a
// pooled connection is unaffected.
const (
	dialTimeout  = 30 * time.Second
	loginTimeout = 30 * time.Second
)

// pooledOpTimeout bounds one pooled operation (a withConn fn — a SELECT+FETCH
// group, a folder sweep, an APPEND). Without it a server that stalls mid-reply
// after login would wedge a pool slot forever (dial clears its deadline after
// login). Generous, because a single fn may sweep every folder of a large
// account; it only needs to beat "forever". The dedicated IDLE connection is
// NOT pooled and never gets this deadline — IDLE legitimately blocks for
// minutes (see idle.go).
const pooledOpTimeout = 5 * time.Minute

// conn is one pooled IMAP connection plus the mailbox it currently has SELECTed
// (cached so a fan-out of fetches against the same folder skips re-SELECTing).
// raw is the underlying net.Conn so withConn can bound each pooled operation
// with a deadline.
type conn struct {
	cl       *imapclient.Client
	raw      net.Conn
	selected string
	selData  *imap.SelectData
}

// Backend implements backend.Backend over one IMAP account, with a small
// connection pool for concurrency.
type Backend struct {
	cfg       Config
	accountID int64
	cred      Credential

	sem    chan struct{} // bounds live connections to poolSize
	idle   chan *conn    // reusable idle connections
	closed atomic.Bool   // set by Close so in-flight releases don't repool
	// closeMu makes release's closed-check + repool atomic against Close's
	// set-closed + drain. Without it a release racing Close can push a live,
	// authenticated connection into the already-drained pool, leaking it (and
	// its server-side session) for the life of the process.
	closeMu sync.Mutex
	stats   *Stats // wire bytes transferred (IMAP + SMTP)

	folderMu      sync.Mutex        // guards the folder caches below
	folderToLabel map[string]string // mailbox name → label id (special-use mapped)
	labelToFolder map[string]string // system label id → mailbox name (for moves)
	archiveFolder string            // the \Archive mailbox, if any (for archive)
	labels        []model.Label     // cached LIST → domain labels
	synced        []string          // mailboxes to sync, derived once from LIST
	foldersLoaded bool              // LIST done
}

// Compile-time assertion: the engine's metadata fan-out uses the batch path when
// a backend implements this.
var _ backend.BatchMetadataFetcher = (*Backend)(nil)

// New builds an IMAP backend. cred authenticates both the IMAP and SMTP
// connections (PasswordAuth or OAuthAuth).
func New(cfg Config, accountID int64, cred Credential) *Backend {
	return &Backend{
		cfg: cfg, accountID: accountID, cred: cred,
		sem:           make(chan struct{}, poolSize),
		idle:          make(chan *conn, poolSize),
		stats:         &Stats{},
		folderToLabel: map[string]string{},
	}
}

var _ backend.Backend = (*Backend)(nil)

// --- connection pool ---

// dial opens and logs in a new connection, returning the client and the raw
// net.Conn beneath it (deadline-bindable by the caller; nil only on error).
// handler, when non-nil, receives unsolicited server data (used by Watch for
// IDLE — which must NOT set deadlines on the returned conn).
func (b *Backend) dial(handler *imapclient.UnilateralDataHandler) (*imapclient.Client, net.Conn, error) {
	start := time.Now()
	addr := net.JoinHostPort(b.cfg.Host, strconv.Itoa(b.cfg.Port))
	tlsCfg := &tls.Config{ServerName: b.cfg.Host}
	opts := &imapclient.Options{TLSConfig: tlsCfg, UnilateralDataHandler: handler}
	logging.Trace("imapbackend: dial", "account", b.cfg.Email, "addr", addr, "security", string(b.cfg.Security), "dialTimeout", dialTimeout)

	// Dial the raw TCP conn ourselves for every mode and wrap it in a byte counter
	// (below TLS, so it counts wire bytes), then build the client over it. Owning
	// the conn lets us bound the dial (DialTimeout) and set a login deadline below
	// — including for STARTTLS, whose handshake + LOGIN would otherwise be
	// unbounded and could hang a pool connection forever.
	var (
		cl  *imapclient.Client
		raw net.Conn // the conn we own; deadline-bindable
		err error
	)
	switch b.cfg.Security {
	case SecurityTLS:
		var tcp net.Conn
		if tcp, err = net.DialTimeout("tcp", addr, dialTimeout); err == nil {
			raw = &countingConn{Conn: tcp, stats: b.stats}
			cl = imapclient.New(tls.Client(raw, tlsCfg), opts)
		}
	case SecurityNone:
		var tcp net.Conn
		if tcp, err = net.DialTimeout("tcp", addr, dialTimeout); err == nil {
			raw = &countingConn{Conn: tcp, stats: b.stats}
			cl = imapclient.New(raw, opts)
		}
	case SecuritySTARTTLS:
		var tcp net.Conn
		if tcp, err = net.DialTimeout("tcp", addr, dialTimeout); err == nil {
			raw = &countingConn{Conn: tcp, stats: b.stats}
			// The STARTTLS handshake does I/O, so bound it with the login deadline
			// before it runs (the block below refreshes the deadline for LOGIN, then
			// clears it). NewStartTLS closes the conn itself on failure.
			_ = raw.SetDeadline(time.Now().Add(loginTimeout))
			cl, err = imapclient.NewStartTLS(raw, opts)
		}
	default:
		return nil, nil, fmt.Errorf("imap: unknown security %q", b.cfg.Security)
	}
	if err != nil {
		logging.Trace("imapbackend: dial failed", "account", b.cfg.Email, "addr", addr, "dur", time.Since(start), "err", err)
		return nil, nil, fmt.Errorf("imap dial %s: %w", addr, err)
	}
	// Bound login (greeting + LOGIN/AUTHENTICATE) with a deadline, then clear it so
	// long-lived pooled/IDLE reads aren't affected.
	if raw != nil {
		_ = raw.SetDeadline(time.Now().Add(loginTimeout))
	}
	loginErr := b.cred.imapLogin(cl)
	if raw != nil {
		_ = raw.SetDeadline(time.Time{})
	}
	if loginErr != nil {
		_ = cl.Close()
		return nil, nil, loginError(loginErr)
	}
	logging.Trace("imapbackend: connected",
		"account", b.cfg.Email, "addr", addr, "dur", time.Since(start),
		"idle", cl.Caps().Has(imap.CapIdle),
		"condstore", cl.Caps().Has(imap.CapCondStore),
		"qresync", cl.Caps().Has(imap.CapQResync),
		"uidplus", cl.Caps().Has(imap.CapUIDPlus))
	return cl, raw, nil
}

// loginError wraps an IMAP login failure, tagging credential rejections with
// backend.ErrAuth so the launcher can prompt the user to reconnect rather than
// retrying a doomed login every sync tick.
func loginError(err error) error {
	if isAuthFailure(err) {
		logging.Trace("imapbackend: login classified as auth failure", "err", err, "wrapped", "ErrAuth")
		return fmt.Errorf("imap login: %w: %v", backend.ErrAuth, err)
	}
	logging.Trace("imapbackend: login failed (non-auth)", "err", err)
	return fmt.Errorf("imap login: %w", err)
}

// isAuthFailure reports whether an IMAP error is a credential rejection. It
// prefers the structured AUTHENTICATIONFAILED response code, falling back to the
// text for the many servers that return a bare "NO" with a human-readable reason.
func isAuthFailure(err error) bool {
	var ie *imap.Error
	if errors.As(err, &ie) && ie.Code == imap.ResponseCodeAuthenticationFailed {
		return true
	}
	low := strings.ToLower(err.Error())
	for _, s := range []string{
		"authenticationfailed",
		"authentication failed",
		"authentication unsuccessful",
		"invalid credentials",
		"login failed",
		"username and password not accepted",
	} {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

// acquire takes a connection from the pool (reusing an idle one or dialing a new
// one), blocking until a slot is free.
func (b *Backend) acquire() (*conn, error) {
	if b.closed.Load() {
		return nil, fmt.Errorf("imap: backend closed")
	}
	b.sem <- struct{}{}
	// Re-check after taking the slot: Close may have run between the check above
	// and here. Without this, a teardown (stopAccount → Close) wouldn't be a
	// barrier — we'd dial a fresh connection on an already-closed backend.
	if b.closed.Load() {
		<-b.sem
		return nil, fmt.Errorf("imap: backend closed")
	}
	select {
	case c := <-b.idle:
		logging.Trace("imapbackend: pool acquire (reused)", "account", b.cfg.Email, "selected", c.selected)
		return c, nil
	default:
	}
	cl, raw, err := b.dial(nil)
	if err != nil {
		<-b.sem
		return nil, err
	}
	logging.Trace("imapbackend: pool acquire (new conn)", "account", b.cfg.Email)
	return &conn{cl: cl, raw: raw}, nil
}

// release returns a healthy connection to the pool, or closes a failed one (so
// the next acquire re-dials). A connection released after Close is closed too,
// not leaked into a drained pool.
func (b *Backend) release(c *conn, healthy bool) {
	if healthy {
		// The closed-check and the repool must be one atomic step with respect
		// to Close (see closeMu): a push that lands before Close takes the lock
		// is seen by its drain; after Close, the check below fails and the
		// connection is closed here instead of leaking into a drained pool.
		b.closeMu.Lock()
		if !b.closed.Load() {
			select {
			case b.idle <- c:
				b.closeMu.Unlock()
				logging.Trace("imapbackend: pool release (repooled)", "account", b.cfg.Email, "selected", c.selected)
				<-b.sem
				return
			default:
			}
		}
		b.closeMu.Unlock()
	}
	logging.Trace("imapbackend: pool release (closing conn)", "account", b.cfg.Email, "healthy", healthy, "closed", b.closed.Load())
	_ = c.cl.Close()
	<-b.sem
}

// withConn runs fn on a pooled connection. The connection is returned to the pool
// on success and closed on any error (conservative — an error may have left it in
// a bad state). release runs via defer so a panic in fn still returns the pool
// token (otherwise repeated panics would starve the pool and deadlock all I/O).
//
// Each fn runs under a conn deadline (pooledOpTimeout) so a server that stalls
// mid-command can't wedge a pool slot forever; the deadline is cleared on
// success before the conn is repooled, so an idle pooled connection doesn't
// expire while waiting for its next use. (The dedicated IDLE connection never
// passes through here.)
func (b *Backend) withConn(fn func(*conn) error) (err error) {
	c, aerr := b.acquire()
	if aerr != nil {
		return aerr
	}
	if c.raw != nil {
		_ = c.raw.SetDeadline(time.Now().Add(pooledOpTimeout))
	}
	healthy := false
	defer func() { b.release(c, healthy) }()
	err = fn(c)
	healthy = err == nil
	if healthy && c.raw != nil {
		_ = c.raw.SetDeadline(time.Time{})
	}
	return err
}

// selectMailbox SELECTs mailbox on this connection (skipping a redundant SELECT
// when it's already current) and returns its status. condStore requests
// CONDSTORE when the server supports it.
func (c *conn) selectMailbox(mailbox string, condStore bool) (*imap.SelectData, error) {
	if c.selected == mailbox && c.selData != nil {
		logging.Trace("imapbackend: select (cached)", "mailbox", mailbox)
		return c.selData, nil
	}
	cs := condStore && c.cl.Caps().Has(imap.CapCondStore)
	opts := &imap.SelectOptions{CondStore: cs}
	start := time.Now()
	data, err := c.cl.Select(mailbox, opts).Wait()
	if err != nil {
		logging.Trace("imapbackend: select failed", "mailbox", mailbox, "condstore", cs, "dur", time.Since(start), "err", err)
		return nil, fmt.Errorf("imap select %q: %w", mailbox, err)
	}
	logging.Trace("imapbackend: select",
		"mailbox", mailbox, "condstore", cs, "uidvalidity", data.UIDValidity,
		"exists", data.NumMessages, "highestmodseq", data.HighestModSeq, "dur", time.Since(start))
	c.selected, c.selData = mailbox, data
	return data, nil
}

// reselect forces a fresh SELECT (bypassing the cache) so the current
// UIDVALIDITY is observed. Destructive ops (move/delete) and the sync snapshot
// use it — trusting a stale cached UIDVALIDITY could act on the wrong messages
// after a server-side folder renumber.
func (c *conn) reselect(mailbox string, condStore bool) (*imap.SelectData, error) {
	c.selected, c.selData = "", nil
	return c.selectMailbox(mailbox, condStore)
}

// Close shuts down the pool: it marks the backend closed (so in-flight operations
// close their connection on release rather than returning it here) and drains the
// idle connections. The dedicated IDLE connection from Watch is owned by that
// goroutine and closes when its context is cancelled.
func (b *Backend) Close() {
	logging.Trace("imapbackend: close (draining pool)", "account", b.cfg.Email)
	// Set closed under closeMu so no release can repool after the drain below
	// starts — see release.
	b.closeMu.Lock()
	b.closed.Store(true)
	b.closeMu.Unlock()
	drained := 0
	for {
		select {
		case c := <-b.idle:
			_ = c.cl.Close()
			drained++
		default:
			logging.Trace("imapbackend: closed", "account", b.cfg.Email, "drained", drained)
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
	logging.TraceContext(ctx, "imapbackend: profile", "account", b.cfg.Email)
	var cur string
	err := b.withConn(func(c *conn) error {
		var e error
		cur, e = b.buildProfileCursor(c)
		return e
	})
	if err != nil {
		logging.TraceContext(ctx, "imapbackend: profile failed", "account", b.cfg.Email, "err", err)
		return backend.Profile{}, err
	}
	logging.TraceContext(ctx, "imapbackend: profile ok", "account", b.cfg.Email, "cursor_bytes", len(cur))
	return backend.Profile{Email: b.cfg.Email, Cursor: cur}, nil
}

// Labels lists the server's folders as domain labels, mapping IMAP special-use
// attributes (\Sent \Drafts \Trash \Junk) and INBOX to the app's system label
// ids so the existing folder views work. It also records the mailbox→label
// mapping for FetchMetadata.
func (b *Backend) Labels(ctx context.Context) ([]model.Label, error) {
	logging.TraceContext(ctx, "imapbackend: labels", "account", b.cfg.Email)
	if err := b.withConn(b.ensureFolders); err != nil {
		logging.TraceContext(ctx, "imapbackend: labels failed", "account", b.cfg.Email, "err", err)
		return nil, err
	}
	b.folderMu.Lock()
	defer b.folderMu.Unlock()
	logging.TraceContext(ctx, "imapbackend: labels ok", "account", b.cfg.Email, "count", len(b.labels))
	return b.labels, nil
}

// ensureFolders runs LIST once and derives, in one pass: the domain label list,
// the folder→label and (system) label→folder maps, and the syncable folder set
// (excluding \All/\Flagged/\Important virtuals so Gmail's All Mail doesn't
// duplicate everything). Idempotent; guarded so fan-out readers see a consistent
// cache.
func (b *Backend) ensureFolders(c *conn) error {
	b.folderMu.Lock()
	loaded := b.foldersLoaded
	b.folderMu.Unlock()
	if loaded {
		return nil
	}
	// LIST without the lock held so a slow round-trip doesn't block label lookups
	// (labelFor is called per message during backfill). A rare concurrent double
	// LIST is harmless — both compute the same maps and the last write wins.
	start := time.Now()
	data, err := c.cl.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		logging.Trace("imapbackend: list failed", "account", b.cfg.Email, "dur", time.Since(start), "err", err)
		return fmt.Errorf("imap list: %w", err)
	}
	logging.Trace("imapbackend: list", "account", b.cfg.Email, "count", len(data), "dur", time.Since(start))
	folderToLabel := map[string]string{}
	labelToFolder := map[string]string{}
	archive := ""
	var labels []model.Label
	var synced []string
	prio := map[string]int{} // sync/backfill priority per mailbox (lower = first)
	for _, d := range data {
		if hasAttr(d.Attrs, imap.MailboxAttrNonExistent) || hasAttr(d.Attrs, imap.MailboxAttrNoSelect) {
			logging.Trace("imapbackend: list folder skipped (non-selectable)", "folder", d.Mailbox)
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
		syncable := !hasAttr(d.Attrs, imap.MailboxAttrAll) &&
			!hasAttr(d.Attrs, imap.MailboxAttrFlagged) &&
			!hasAttr(d.Attrs, imap.MailboxAttrImportant)
		if syncable {
			synced = append(synced, d.Mailbox)
			prio[d.Mailbox] = folderPriority(d.Mailbox, id)
		}
		logging.Trace("imapbackend: list folder mapped", "folder", d.Mailbox, "id", id, "type", ltype, "syncable", syncable, "priority", prio[d.Mailbox])
	}
	// Order folders by priority (INBOX, then special-use, then the rest,
	// alphabetical within a tier): a capped backfill (SearchIDs with max) fills
	// with INBOX mail first instead of whatever sorts alphabetically earliest.
	sort.Slice(synced, func(i, j int) bool {
		if prio[synced[i]] != prio[synced[j]] {
			return prio[synced[i]] < prio[synced[j]]
		}
		return synced[i] < synced[j]
	})
	logging.Trace("imapbackend: folders ready", "account", b.cfg.Email, "labels", len(labels), "synced", len(synced), "archive", archive)
	b.folderMu.Lock()
	defer b.folderMu.Unlock()
	if b.foldersLoaded { // lost a race with another LIST; keep the first result
		return nil
	}
	b.folderToLabel, b.labelToFolder, b.labels, b.synced = folderToLabel, labelToFolder, labels, synced
	b.archiveFolder = archive
	b.foldersLoaded = true
	return nil
}

// SearchIDs lists message ids matching query, newest first within each folder
// (highest UID first), capped to max total. An empty query lists every synced
// folder (backfill); otherwise the query is parsed (see parseSearchQuery) into a
// folder scope (in:) plus IMAP SEARCH criteria. An unsupported query is an error
// — it must never fall back to "all messages" (Empty Trash deletes the result).
// Folders are visited in priority order (INBOX, special-use, rest — see
// ensureFolders), so a capped backfill fills up with the mail that matters.
func (b *Backend) SearchIDs(ctx context.Context, query string, max int) ([]string, error) {
	logging.TraceContext(ctx, "imapbackend: search ids", "account", b.cfg.Email, "query", query, "max", max)
	q, err := parseSearchQuery(query)
	if err != nil {
		logging.TraceContext(ctx, "imapbackend: search ids unsupported query", "account", b.cfg.Email, "query", query, "err", err)
		return nil, err
	}
	var ids []string
	err = b.withConn(func(c *conn) error {
		folders, err := b.folders(c)
		if err != nil {
			return err
		}
		if q.label != "" {
			folders, err = b.foldersForLabel(q.label)
			if err != nil {
				return err
			}
		}
		for _, f := range folders {
			sel, err := c.selectMailbox(f, false)
			if err != nil {
				return err
			}
			start := time.Now()
			crit := q.criteria // copy: UIDSearch may not mutate, but keep folders independent
			sd, err := c.cl.UIDSearch(&crit, nil).Wait()
			if err != nil {
				logging.Trace("imapbackend: uid search failed", "folder", f, "dur", time.Since(start), "err", err)
				return fmt.Errorf("imap uid search %q: %w", f, err)
			}
			uids := sd.AllUIDs()
			logging.Trace("imapbackend: uid search", "folder", f, "uidvalidity", sel.UIDValidity, "count", len(uids), "dur", time.Since(start))
			sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] }) // newest first
			for _, u := range uids {
				ids = append(ids, msgID(f, sel.UIDValidity, u))
				if max > 0 && len(ids) >= max {
					logging.TraceContext(ctx, "imapbackend: search ids capped", "account", b.cfg.Email, "count", len(ids), "max", max)
					return nil
				}
			}
		}
		return nil
	})
	if err != nil {
		return ids, err
	}
	logging.TraceContext(ctx, "imapbackend: search ids ok", "account", b.cfg.Email, "count", len(ids))
	return ids, nil
}

// FetchMetadata fetches one message's envelope + flags and converts it.
func (b *Backend) FetchMetadata(ctx context.Context, id string) (model.Message, error) {
	mailbox, uidv, uid, err := parseMsgID(id)
	if err != nil {
		logging.TraceContext(ctx, "imapbackend: fetch metadata bad id", "id", id, "err", err)
		return model.Message{}, err
	}
	logging.TraceContext(ctx, "imapbackend: fetch metadata", "id", id, "mailbox", mailbox, "uid", uint32(uid), "uidvalidity", uidv)
	var out model.Message
	err = b.withConn(func(c *conn) error {
		sel, err := c.selectMailbox(mailbox, false)
		if err != nil {
			return err
		}
		if sel.UIDValidity != uidv {
			// A stale-epoch id names a message that no longer exists under that
			// number: ErrNotFound so the engine skips it instead of holding the
			// cursor forever on a "transient" failure.
			logging.Trace("imapbackend: fetch metadata stale id", "id", id, "uidvalidity", uidv, "current", sel.UIDValidity)
			return fmt.Errorf("%w: stale id %q (uidvalidity %d != %d)", backend.ErrNotFound, id, uidv, sel.UIDValidity)
		}
		// References isn't part of the IMAP ENVELOPE, so fetch that one header too
		// — it carries the thread's ancestry (used to compute a stable thread root).
		refSection := &imap.FetchItemBodySection{
			Specifier: imap.PartSpecifierHeader, HeaderFields: metaHeaderFields, Peek: true,
		}
		start := time.Now()
		bufs, err := c.cl.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
			Envelope: true, Flags: true, InternalDate: true, RFC822Size: true, UID: true,
			BodySection: []*imap.FetchItemBodySection{refSection},
		}).Collect()
		if err != nil {
			logging.Trace("imapbackend: fetch metadata failed", "id", id, "dur", time.Since(start), "err", err)
			return fmt.Errorf("imap fetch metadata: %w", err)
		}
		if len(bufs) == 0 {
			// The message vanished between listing and fetch: genuinely gone, not a
			// transient failure — don't stall the engine's cursor on it.
			logging.Trace("imapbackend: fetch metadata not found", "mailbox", mailbox, "uid", uint32(uid))
			return fmt.Errorf("%w: uid %d in %q", backend.ErrNotFound, uid, mailbox)
		}
		mh := parseMetaHeaders(bufs[0].FindBodySection(refSection))
		out = b.toMessage(mailbox, uidv, bufs[0], mh)
		logging.Trace("imapbackend: fetch metadata ok",
			"id", id, "subject", out.Subject, "from", out.FromAddr, "unread", out.IsUnread,
			"starred", out.IsStarred, "size", out.SizeEstimate, "dur", time.Since(start))
		return nil
	})
	return out, err
}

// metadataFetchChunk bounds how many UIDs one metadata FETCH requests. IMAP
// servers accept large UID sets, but a bound keeps a single command (and its
// response buffering) reasonable; the engine typically calls with ~200 already.
const metadataFetchChunk = 200

// FetchMetadataBatch implements backend.BatchMetadataFetcher: it fetches metadata
// for many messages in far fewer round-trips than one FetchMetadata per id by
// grouping the ids by (mailbox, uidvalidity) — one SELECT + UID-set FETCH per
// group, chunked to metadataFetchChunk UIDs. A UID missing from the response (or a
// whole stale-epoch group) is skipped and counted, never failing the batch; a
// transport error fails the whole call so the caller can hold its sync cursor. The
// returned messages are in no guaranteed order.
func (b *Backend) FetchMetadataBatch(ctx context.Context, ids []string) ([]model.Message, error) {
	logging.TraceContext(ctx, "imapbackend: fetch metadata batch", "account", b.cfg.Email, "n", len(ids))
	if len(ids) == 0 {
		return nil, nil
	}
	// Group by (mailbox, uidvalidity), preserving first-seen order for stable tracing.
	type groupKey struct {
		mailbox string
		uidv    uint32
	}
	groups := map[groupKey][]imap.UID{}
	var order []groupKey
	skipped := 0
	for _, id := range ids {
		mailbox, uidv, uid, err := parseMsgID(id)
		if err != nil {
			logging.TraceContext(ctx, "imapbackend: fetch metadata batch bad id", "id", id, "err", err)
			skipped++
			continue
		}
		k := groupKey{mailbox, uidv}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], uid)
	}

	out := make([]model.Message, 0, len(ids))
	err := b.withConn(func(c *conn) error {
		for _, k := range order {
			uids := groups[k]
			sel, err := c.selectMailbox(k.mailbox, false)
			if err != nil {
				return err
			}
			if sel.UIDValidity != k.uidv {
				// Stale-epoch ids name messages that no longer exist under that number:
				// skip the group (counted) rather than fetch the wrong messages.
				logging.Trace("imapbackend: fetch metadata batch stale group",
					"mailbox", k.mailbox, "uidvalidity", k.uidv, "current", sel.UIDValidity, "n", len(uids))
				skipped += len(uids)
				continue
			}
			for start := 0; start < len(uids); start += metadataFetchChunk {
				end := start + metadataFetchChunk
				if end > len(uids) {
					end = len(uids)
				}
				chunk := uids[start:end]
				got, ferr := b.fetchMetaChunk(c, k.mailbox, k.uidv, chunk)
				if ferr != nil {
					return ferr
				}
				out = append(out, got...)
				skipped += len(chunk) - len(got) // UIDs absent from the response are gone
			}
		}
		return nil
	})
	if err != nil {
		logging.TraceContext(ctx, "imapbackend: fetch metadata batch failed", "account", b.cfg.Email, "err", err)
		return nil, err
	}
	logging.TraceContext(ctx, "imapbackend: fetch metadata batch ok",
		"account", b.cfg.Email, "requested", len(ids), "fetched", len(out), "skipped", skipped)
	return out, nil
}

// fetchMetaChunk issues one UID-set metadata FETCH (envelope, flags, dates,
// References header) against the already-selected mailbox and converts each
// returned buffer. The caller guarantees the connection has mailbox SELECTed at
// uidv. Fewer buffers than uids means some UIDs vanished between listing and fetch.
func (b *Backend) fetchMetaChunk(c *conn, mailbox string, uidv uint32, uids []imap.UID) ([]model.Message, error) {
	refSection := &imap.FetchItemBodySection{
		Specifier: imap.PartSpecifierHeader, HeaderFields: metaHeaderFields, Peek: true,
	}
	set := uidSetOf(uids)
	start := time.Now()
	bufs, err := c.cl.Fetch(set, &imap.FetchOptions{
		Envelope: true, Flags: true, InternalDate: true, RFC822Size: true, UID: true,
		BodySection: []*imap.FetchItemBodySection{refSection},
	}).Collect()
	if err != nil {
		logging.Trace("imapbackend: fetch metadata batch chunk failed", "mailbox", mailbox, "n", len(uids), "dur", time.Since(start), "err", err)
		return nil, fmt.Errorf("imap fetch metadata batch %q: %w", mailbox, err)
	}
	out := make([]model.Message, 0, len(bufs))
	for _, buf := range bufs {
		mh := parseMetaHeaders(buf.FindBodySection(refSection))
		out = append(out, b.toMessage(mailbox, uidv, buf, mh))
	}
	logging.Trace("imapbackend: fetch metadata batch chunk", "mailbox", mailbox, "requested", len(uids), "fetched", len(out), "dur", time.Since(start))
	return out, nil
}

// FetchBody fetches and parses a message's full body + attachment metadata.
func (b *Backend) FetchBody(ctx context.Context, id string) (model.MessageBody, []model.Attachment, error) {
	logging.TraceContext(ctx, "imapbackend: fetch body", "id", id)
	raw, err := b.fetchRaw(id)
	if err != nil {
		return model.MessageBody{}, nil, err
	}
	body, atts, err := parseBody(raw)
	logging.TraceContext(ctx, "imapbackend: fetch body parsed",
		"id", id, "raw_bytes", len(raw), "html_bytes", len(body.HTML), "text_bytes", len(body.Text), "attachments", len(atts))
	return body, atts, err
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
			logging.Trace("imapbackend: fetch raw stale id", "id", id, "uidvalidity", uidv, "current", sel.UIDValidity)
			return fmt.Errorf("imap: stale id %q", id)
		}
		section := &imap.FetchItemBodySection{Peek: true}
		start := time.Now()
		bufs, err := c.cl.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
			BodySection: []*imap.FetchItemBodySection{section},
		}).Collect()
		if err != nil {
			logging.Trace("imapbackend: fetch raw failed", "id", id, "dur", time.Since(start), "err", err)
			return fmt.Errorf("imap fetch body: %w", err)
		}
		if len(bufs) == 0 {
			logging.Trace("imapbackend: fetch raw not found", "mailbox", mailbox, "uid", uint32(uid))
			return fmt.Errorf("imap: uid %d not found", uid)
		}
		raw = bufs[0].FindBodySection(section)
		logging.Trace("imapbackend: fetch raw ok", "id", id, "bytes", len(raw), "dur", time.Since(start))
		return nil
	})
	return raw, err
}

// Changes diffs every synced folder against the cursor (a per-folder UID-set +
// modseq snapshot) and returns the message ids to upsert (new + flag-changed)
// and delete (vanished), plus the next cursor. A UIDVALIDITY change re-syncs that
// folder wholesale. (Mutations, send, and drafts live in mutate.go.)
func (b *Backend) Changes(ctx context.Context, cur string) (upserts, deletes []string, next string, err error) {
	logging.TraceContext(ctx, "imapbackend: changes", "account", b.cfg.Email, "cursor_bytes", len(cur))
	var nextCur cursor
	err = b.withConn(func(c *conn) error {
		var e error
		upserts, deletes, nextCur, e = b.computeChanges(c, decodeCursor(cur))
		return e
	})
	if err != nil {
		logging.TraceContext(ctx, "imapbackend: changes failed", "account", b.cfg.Email, "err", err)
		return nil, nil, "", err
	}
	logging.TraceContext(ctx, "imapbackend: changes ok", "account", b.cfg.Email, "upserts", len(upserts), "deletes", len(deletes))
	return upserts, deletes, nextCur.encode(), nil
}

// --- conversions / helpers ---

// toMessage converts a fetched message into the domain model. Caller holds mu
// (it reads folderToLabel).
func (b *Backend) toMessage(mailbox string, uidv uint32, buf *imapclient.FetchMessageBuffer, mh metaHeaders) model.Message {
	id := msgID(mailbox, uidv, buf.UID)
	refs := mh.refs
	m := model.Message{
		AccountID:         b.accountID,
		GmailID:           id,
		ThreadID:          id, // overridden below once the reference chain is known
		InternalDate:      buf.InternalDate,
		SizeEstimate:      buf.RFC822Size,
		ListUnsubscribe:   mh.unsub,
		ListUnsubOneClick: mh.oneClick,
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

// folderPriority ranks a syncable folder for capped backfills: INBOX first,
// then the special-use folders (Sent/Drafts/Trash/Junk — those that mapped to a
// system label), then everything else.
func folderPriority(mailbox, labelID string) int {
	switch {
	case strings.EqualFold(mailbox, "INBOX"):
		return 0
	case isSystemLabel(labelID):
		return 1
	default:
		return 2
	}
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

// metaHeaderFields are the headers the metadata FETCH requests beyond the
// envelope: threading ancestry plus the unsubscribe capability.
var metaHeaderFields = []string{"References", "List-Unsubscribe", "List-Unsubscribe-Post"}

// metaHeaders is the parsed form of the metaHeaderFields section.
type metaHeaders struct {
	refs     []string // bracket-stripped References message-ids, oldest-first
	unsub    string   // List-Unsubscribe value ("" = none)
	oneClick bool     // List-Unsubscribe-Post offered RFC 8058 one-click
}

// parseMetaHeaders parses the fetched HEADER.FIELDS block (multiple headers,
// possibly folded across lines) into its typed parts.
func parseMetaHeaders(headerBytes []byte) metaHeaders {
	var mh metaHeaders
	// Unfold: a line starting with space/tab continues the previous header.
	raw := strings.Split(strings.ReplaceAll(string(headerBytes), "\r\n", "\n"), "\n")
	var lines []string
	for _, l := range raw {
		if (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) && len(lines) > 0 {
			lines[len(lines)-1] += " " + strings.TrimSpace(l)
			continue
		}
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	for _, l := range lines {
		name, val, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "references":
			for _, tok := range strings.Fields(val) {
				if id := strings.Trim(tok, "<>"); id != "" {
					mh.refs = append(mh.refs, id)
				}
			}
		case "list-unsubscribe":
			mh.unsub = val
		case "list-unsubscribe-post":
			mh.oneClick = strings.Contains(strings.ToLower(val), "one-click")
		}
	}
	return mh
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
