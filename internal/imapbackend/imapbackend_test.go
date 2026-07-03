package imapbackend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/model"
)

const testMsg = "From: Bob Builder <bob@example.com>\r\n" +
	"To: alice@example.com\r\n" +
	"Subject: Hello IMAP\r\n" +
	"Message-ID: <m1@example.com>\r\n" +
	"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Hi there, this is the body.\r\n"

// startMemServer stands up an in-memory IMAP server with one user whose INBOX
// holds testMsg, and returns the host/port to dial.
func startMemServer(t *testing.T) (host string, port int) {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("alice@example.com", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	// A non-nil AppendOptions is required: the in-memory server dereferences it.
	if _, err := user.Append("INBOX", bytes.NewReader([]byte(testMsg)), &imap.AppendOptions{}); err != nil {
		t.Fatalf("append message: %v", err)
	}
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true, // tests dial plaintext
		Caps:         imap.CapSet{imap.CapIMAP4rev2: {}},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })

	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ = strconv.Atoi(p)
	return h, port
}

func TestIMAPBackendReadPath(t *testing.T) {
	host, port := startMemServer(t)
	b := New(Config{
		Host: host, Port: port, Security: SecurityNone,
		Username: "alice@example.com", Email: "alice@example.com",
	}, 1, PasswordAuth("alice@example.com", "secret"))
	t.Cleanup(b.Close)
	ctx := context.Background()

	// Profile dials + logs in.
	prof, err := b.Profile(ctx)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if prof.Email != "alice@example.com" {
		t.Errorf("Profile email = %q", prof.Email)
	}

	// Labels include a mapped INBOX.
	labels, err := b.Labels(ctx)
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	if !hasLabelID(labels, model.LabelInbox) {
		t.Errorf("INBOX label missing: %+v", labels)
	}

	// SearchIDs backfills the INBOX (one message).
	ids, err := b.SearchIDs(ctx, "", 0)
	if err != nil {
		t.Fatalf("SearchIDs: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("SearchIDs returned %d ids, want 1: %v", len(ids), ids)
	}

	// FetchMetadata parses the envelope and flags.
	m, err := b.FetchMetadata(ctx, ids[0])
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	if m.Subject != "Hello IMAP" {
		t.Errorf("Subject = %q", m.Subject)
	}
	if m.FromAddr != "bob@example.com" {
		t.Errorf("FromAddr = %q", m.FromAddr)
	}
	if m.FromName != "Bob Builder" {
		t.Errorf("FromName = %q", m.FromName)
	}
	if m.RFC822MsgID != "<m1@example.com>" {
		t.Errorf("RFC822MsgID = %q", m.RFC822MsgID)
	}
	if !m.IsUnread {
		t.Errorf("expected unread (no \\Seen flag)")
	}
	if !contains(m.Labels, model.LabelInbox) || !contains(m.Labels, model.LabelUnread) {
		t.Errorf("Labels = %v, want INBOX + UNREAD", m.Labels)
	}

	// FetchBody parses the text body.
	body, atts, err := b.FetchBody(ctx, ids[0])
	if err != nil {
		t.Fatalf("FetchBody: %v", err)
	}
	if !strings.Contains(body.Text, "Hi there, this is the body.") {
		t.Errorf("body text = %q", body.Text)
	}
	if len(atts) != 0 {
		t.Errorf("expected no attachments, got %d", len(atts))
	}

	// Byte stats are counted across the IMAP traffic above.
	in, out := b.Transferred()
	if in == 0 || out == 0 {
		t.Errorf("byte stats not counted: in=%d out=%d", in, out)
	}
}

func TestIMAPIncremental(t *testing.T) {
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("alice@example.com", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	appendMsg := func(mailbox, subject string) {
		raw := "From: bob@example.com\r\nSubject: " + subject + "\r\n" +
			"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\nContent-Type: text/plain\r\n\r\nbody\r\n"
		if _, err := user.Append(mailbox, bytes.NewReader([]byte(raw)), &imap.AppendOptions{}); err != nil {
			t.Fatalf("append to %s: %v", mailbox, err)
		}
	}
	appendMsg("INBOX", "first")
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev2: {}},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)

	b := New(Config{Host: h, Port: port, Security: SecurityNone, Username: "alice@example.com", Email: "alice@example.com"}, 1, PasswordAuth("alice@example.com", "secret"))
	t.Cleanup(b.Close)
	ctx := context.Background()

	// Baseline cursor after "backfill" (one message present).
	prof, err := b.Profile(ctx)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if prof.Cursor == "" {
		t.Fatal("Profile cursor is empty; want a seeded cursor")
	}

	// No changes yet. The steady-state cursor must round-trip byte-identically
	// (whether via the STATUS pre-check copying the old state or a fresh
	// snapshot of an unchanged folder) — the engine skips the per-tick cursor
	// DB write on that equality.
	up, del, cur1, err := b.Changes(ctx, prof.Cursor)
	if err != nil {
		t.Fatalf("Changes (steady): %v", err)
	}
	if len(up) != 0 || len(del) != 0 {
		t.Fatalf("steady-state changes: ups=%v dels=%v, want none", up, del)
	}
	if cur1 != prof.Cursor {
		t.Fatalf("steady-state cursor changed:\n was %s\n now %s", prof.Cursor, cur1)
	}

	// A new message appears → one upsert, no deletes.
	appendMsg("INBOX", "second")
	up, del, cur2, err := b.Changes(ctx, cur1)
	if err != nil {
		t.Fatalf("Changes (new msg): %v", err)
	}
	if len(up) != 1 || len(del) != 0 {
		t.Fatalf("after append: ups=%v dels=%v, want 1 up / 0 del", up, del)
	}

	// Vanished detection: feed a cursor that claims an extra (now-absent) UID.
	c := decodeCursor(cur2)
	st := c.Folders["INBOX"]
	present := decodeUIDs(st.UIDs)
	phantom := imap.UID(99999)
	st.UIDs = encodeUIDs(append(append([]imap.UID{}, present...), phantom))
	c.Folders["INBOX"] = st
	up, del, _, err = b.Changes(ctx, c.encode())
	if err != nil {
		t.Fatalf("Changes (vanished): %v", err)
	}
	wantDel := msgID("INBOX", st.UIDValidity, phantom)
	if len(up) != 0 || len(del) != 1 || del[0] != wantDel {
		t.Fatalf("vanished: ups=%v dels=%v, want 0 up / [%s]", up, del, wantDel)
	}

	// UIDVALIDITY change → whole folder re-synced: old UIDs deleted, current upserted.
	c2 := decodeCursor(cur2)
	st2 := c2.Folders["INBOX"]
	oldUIDs := decodeUIDs(st2.UIDs)
	st2.UIDValidity++ // simulate a server-side validity bump
	c2.Folders["INBOX"] = st2
	up, del, _, err = b.Changes(ctx, c2.encode())
	if err != nil {
		t.Fatalf("Changes (uidvalidity): %v", err)
	}
	if len(del) != len(oldUIDs) || len(up) != len(present) {
		t.Fatalf("uidvalidity reset: ups=%d dels=%d, want %d up / %d del", len(up), len(del), len(present), len(oldUIDs))
	}
}

func TestIMAPThreading(t *testing.T) {
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("alice@example.com", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	orig := "From: bob@example.com\r\nSubject: Plan\r\nMessage-ID: <a@x>\r\n" +
		"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\nContent-Type: text/plain\r\n\r\norig\r\n"
	reply := "From: alice@example.com\r\nSubject: Re: Plan\r\nMessage-ID: <b@x>\r\n" +
		"In-Reply-To: <a@x>\r\nReferences: <a@x>\r\n" +
		"Date: Mon, 02 Jan 2006 16:04:05 -0700\r\nContent-Type: text/plain\r\n\r\nreply\r\n"
	for _, raw := range []string{orig, reply} {
		if _, err := user.Append("INBOX", bytes.NewReader([]byte(raw)), &imap.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev2: {}},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)

	b := New(Config{Host: h, Port: port, Security: SecurityNone, Username: "alice@example.com", Email: "alice@example.com"}, 1, PasswordAuth("alice@example.com", "secret"))
	t.Cleanup(b.Close)
	ctx := context.Background()

	ids, err := b.SearchIDs(ctx, "", 0)
	if err != nil {
		t.Fatalf("SearchIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 messages, got %d", len(ids))
	}
	threads := map[string]bool{}
	for _, id := range ids {
		m, err := b.FetchMetadata(ctx, id)
		if err != nil {
			t.Fatalf("FetchMetadata: %v", err)
		}
		threads[m.ThreadID] = true
	}
	if len(threads) != 1 {
		t.Fatalf("original and reply should share one thread; got %d distinct: %v", len(threads), threads)
	}
	if !threads["a@x"] {
		t.Fatalf("thread root should be the original's Message-ID a@x; got %v", threads)
	}
}

func TestCursorAndUIDCodec(t *testing.T) {
	uids := []imap.UID{1, 2, 3, 5, 7, 8, 9, 100}
	got := decodeUIDs(encodeUIDs(uids))
	if len(got) != len(uids) {
		t.Fatalf("uid codec: got %v, want %v", got, uids)
	}
	for i := range uids {
		if got[i] != uids[i] {
			t.Fatalf("uid codec mismatch at %d: %v vs %v", i, got, uids)
		}
	}
	c := cursor{Folders: map[string]folderState{
		"INBOX": {UIDValidity: 42, ModSeq: 1000, UIDNext: 101, UIDs: encodeUIDs(uids)},
	}}
	rt := decodeCursor(c.encode())
	if rt.Folders["INBOX"].UIDValidity != 42 || rt.Folders["INBOX"].ModSeq != 1000 || rt.Folders["INBOX"].UIDNext != 101 {
		t.Fatalf("cursor round-trip lost fields: %+v", rt.Folders["INBOX"])
	}
	if decodeCursor("").Folders == nil {
		t.Fatal("empty cursor must yield a usable (non-nil) folder map")
	}

	// countUIDs must agree with a full decode without materializing the set.
	for _, tc := range []struct {
		set  string
		want int
	}{
		{"", 0},
		{"7", 1},
		{"1:5", 5},
		{encodeUIDs(uids), len(uids)},
		{"1:100000", 100000},
	} {
		if got := countUIDs(tc.set); got != tc.want {
			t.Fatalf("countUIDs(%q) = %d, want %d", tc.set, got, tc.want)
		}
	}
}

func TestIMAPMutations(t *testing.T) {
	host, port := startMemServer(t) // INBOX has one unread message
	b := New(Config{Host: host, Port: port, Security: SecurityNone, Username: "alice@example.com", Email: "alice@example.com"}, 1, PasswordAuth("alice@example.com", "secret"))
	t.Cleanup(b.Close)
	ctx := context.Background()

	ids, err := b.SearchIDs(ctx, "", 0)
	if err != nil || len(ids) != 1 {
		t.Fatalf("SearchIDs: %v, ids=%v", err, ids)
	}
	id := ids[0]

	if m, _ := b.FetchMetadata(ctx, id); !m.IsUnread {
		t.Fatal("message should start unread")
	}

	// Mark read (remove UNREAD → set \Seen).
	if err := b.ApplyLabels(ctx, []string{id}, nil, []string{model.LabelUnread}); err != nil {
		t.Fatalf("ApplyLabels mark-read: %v", err)
	}
	if m, _ := b.FetchMetadata(ctx, id); m.IsUnread {
		t.Error("still unread after mark-read")
	}

	// Star (add STARRED → set \Flagged).
	if err := b.ApplyLabels(ctx, []string{id}, []string{model.LabelStarred}, nil); err != nil {
		t.Fatalf("ApplyLabels star: %v", err)
	}
	if m, _ := b.FetchMetadata(ctx, id); !m.IsStarred {
		t.Error("not starred after star")
	}

	// Delete (\Deleted + EXPUNGE).
	if err := b.Delete(ctx, []string{id}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ids2, _ := b.SearchIDs(ctx, "", 0); len(ids2) != 0 {
		t.Errorf("after delete: %d messages remain, want 0", len(ids2))
	}
}

func TestSMTPEnvelope(t *testing.T) {
	raw := []byte("From: Me <me@x.com>\r\nTo: a@y.com, b@y.com\r\nCc: c@z.com\r\n" +
		"Bcc: secret@w.com\r\nSubject: hi\r\n\r\nbody\r\n")
	from, to, cleaned, err := smtpEnvelope(raw)
	if err != nil {
		t.Fatalf("smtpEnvelope: %v", err)
	}
	if from != "me@x.com" {
		t.Errorf("from = %q, want me@x.com", from)
	}
	if len(to) != 4 {
		t.Errorf("recipients = %v, want 4 (To+Cc+Bcc)", to)
	}
	if !contains(to, "secret@w.com") {
		t.Error("bcc recipient missing from the RCPT list")
	}
	if bytes.Contains(bytes.ToLower(cleaned), []byte("bcc:")) {
		t.Errorf("Bcc header not stripped from the transmitted message:\n%s", cleaned)
	}
	if !bytes.Contains(cleaned, []byte("To: a@y.com")) || !bytes.Contains(cleaned, []byte("\r\n\r\nbody")) {
		t.Errorf("stripping Bcc damaged the message:\n%s", cleaned)
	}
}

func TestIMAPConcurrentFetch(t *testing.T) {
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("alice@example.com", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		raw := fmt.Sprintf("From: bob@example.com\r\nSubject: msg %d\r\nMessage-ID: <m%d@x>\r\n"+
			"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\nContent-Type: text/plain\r\n\r\nbody\r\n", i, i)
		if _, err := user.Append("INBOX", bytes.NewReader([]byte(raw)), &imap.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev2: {}},
	})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)

	b := New(Config{Host: h, Port: port, Security: SecurityNone, Username: "alice@example.com", Email: "alice@example.com"}, 1, PasswordAuth("alice@example.com", "secret"))
	t.Cleanup(b.Close)
	ctx := context.Background()

	ids, err := b.SearchIDs(ctx, "", 0)
	if err != nil || len(ids) != 20 {
		t.Fatalf("SearchIDs: %v, %d ids", err, len(ids))
	}
	// Fetch all concurrently — exercises the connection pool. Run with -race to
	// catch shared-state bugs.
	var wg sync.WaitGroup
	errs := make(chan error, len(ids))
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := b.FetchMetadata(ctx, id); err != nil {
				errs <- err
			}
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent fetch failed: %v", err)
	}
}

func TestIMAPCloseBarrier(t *testing.T) {
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("alice@example.com", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		raw := fmt.Sprintf("From: bob@example.com\r\nSubject: m%d\r\nMessage-ID: <m%d@x>\r\n"+
			"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\nContent-Type: text/plain\r\n\r\nbody\r\n", i, i)
		if _, err := user.Append("INBOX", bytes.NewReader([]byte(raw)), &imap.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev2: {}},
	})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)

	b := New(Config{Host: h, Port: port, Security: SecurityNone, Username: "alice@example.com", Email: "alice@example.com"}, 4, PasswordAuth("alice@example.com", "secret"))
	ctx := context.Background()
	ids, err := b.SearchIDs(ctx, "", 0)
	if err != nil || len(ids) == 0 {
		t.Fatalf("SearchIDs: %v, %d ids", err, len(ids))
	}

	// Race fetches against a concurrent Close. Run with -race: must not panic, must
	// not corrupt the semaphore, and once closed every acquire fails cleanly.
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, _ = b.FetchMetadata(ctx, id) // error after Close is expected, not a panic
		}(ids[i%len(ids)])
	}
	b.Close()
	wg.Wait()

	// After Close the backend is a hard barrier: no new connection is dialed.
	if _, err := b.FetchMetadata(ctx, ids[0]); err == nil {
		t.Fatal("FetchMetadata succeeded after Close; expected a closed-backend error")
	}
	// And no release racing Close may have repooled a live connection into the
	// drained pool — that would leak an authenticated session for the process
	// lifetime (release/Close are serialized by closeMu).
	if n := len(b.idle); n != 0 {
		t.Fatalf("idle pool holds %d connections after Close and all releases; leaked", n)
	}
}

func TestIMAPWatchStopsOnCancel(t *testing.T) {
	host, port := startMemServer(t)
	b := New(Config{Host: host, Port: port, Security: SecurityNone, Username: "alice@example.com", Email: "alice@example.com"}, 1, PasswordAuth("alice@example.com", "secret"))
	t.Cleanup(b.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Watch(ctx, func() {}); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return promptly after context cancel")
	}
}

func TestMsgIDRoundTrip(t *testing.T) {
	cases := []struct {
		mailbox string
		uidv    uint32
		uid     imap.UID
	}{
		{"INBOX", 1, 42},
		{"[Gmail]/All Mail", 12345, 9999},
		{"Weird:Folder:Name", 7, 1}, // colons in the mailbox survive (it's last)
	}
	for _, c := range cases {
		id := msgID(c.mailbox, c.uidv, c.uid)
		mb, uv, u, err := parseMsgID(id)
		if err != nil {
			t.Fatalf("parseMsgID(%q): %v", id, err)
		}
		if mb != c.mailbox || uv != c.uidv || u != c.uid {
			t.Errorf("round-trip %q -> (%q,%d,%d), want (%q,%d,%d)", id, mb, uv, u, c.mailbox, c.uidv, c.uid)
		}
	}
	if _, _, _, err := parseMsgID("gmail-style-id"); err == nil {
		t.Error("expected error for a non-imap id")
	}
}

func TestLoginErrorClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		auth bool // expect it to be tagged backend.ErrAuth
	}{
		{"response code", &imap.Error{Code: imap.ResponseCodeAuthenticationFailed, Text: "bad"}, true},
		{"bare NO text", fmt.Errorf("imap: NO [AUTHENTICATIONFAILED] Invalid credentials"), true},
		{"gmail wording", fmt.Errorf("Username and password not accepted"), true},
		{"outlook wording", fmt.Errorf("LOGIN failed."), true},
		{"network error", fmt.Errorf("dial tcp: i/o timeout"), false},
		{"server busy", fmt.Errorf("imap: NO server unavailable"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := errors.Is(loginError(c.err), backend.ErrAuth)
			if got != c.auth {
				t.Errorf("loginError(%v): errors.Is ErrAuth = %v, want %v", c.err, got, c.auth)
			}
		})
	}
}

func TestFetchMetadataBatch(t *testing.T) {
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("alice@example.com", "secret")
	for _, f := range []string{"INBOX", "Archive"} {
		if err := user.Create(f, nil); err != nil {
			t.Fatalf("create %s: %v", f, err)
		}
	}
	appendMsg := func(mailbox, subject string) {
		raw := "From: bob@example.com\r\nSubject: " + subject + "\r\n" +
			"Message-ID: <" + subject + "@x>\r\n" +
			"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\nContent-Type: text/plain\r\n\r\nbody\r\n"
		if _, err := user.Append(mailbox, bytes.NewReader([]byte(raw)), &imap.AppendOptions{}); err != nil {
			t.Fatalf("append to %s: %v", mailbox, err)
		}
	}
	for i := 0; i < 3; i++ {
		appendMsg("INBOX", fmt.Sprintf("inbox-%d", i))
	}
	appendMsg("Archive", "arch-0")
	appendMsg("Archive", "arch-1")
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev2: {}},
	})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)

	b := New(Config{Host: h, Port: port, Security: SecurityNone, Username: "alice@example.com", Email: "alice@example.com"}, 1, PasswordAuth("alice@example.com", "secret"))
	t.Cleanup(b.Close)
	ctx := context.Background()

	ids, err := b.SearchIDs(ctx, "", 0)
	if err != nil || len(ids) != 5 {
		t.Fatalf("SearchIDs: %v, %d ids (want 5 across two folders)", err, len(ids))
	}

	// Empty input is a no-op.
	if msgs, err := b.FetchMetadataBatch(ctx, nil); err != nil || msgs != nil {
		t.Fatalf("FetchMetadataBatch(nil) = %v, %v", msgs, err)
	}

	// Derive two ids the batch must skip (not fail on): a phantom UID in a real
	// folder (absent from the FETCH response) and a stale-epoch id (wrong
	// uidvalidity → whole group skipped).
	mailbox, uidv, uid, err := parseMsgID(ids[0])
	if err != nil {
		t.Fatalf("parseMsgID: %v", err)
	}
	phantom := msgID(mailbox, uidv, 999999)
	stale := msgID(mailbox, uidv+100, uid)

	// Batch across both folders plus the two skippable ids. All five real messages
	// come back (grouped by folder), the two bad ids are silently skipped.
	req := append(append([]string{}, ids...), phantom, stale)
	msgs, err := b.FetchMetadataBatch(ctx, req)
	if err != nil {
		t.Fatalf("FetchMetadataBatch: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5 (phantom + stale skipped)", len(msgs))
	}
	subjects := map[string]bool{}
	labels := map[string]bool{}
	for _, m := range msgs {
		subjects[m.Subject] = true
		for _, l := range m.Labels {
			labels[l] = true
		}
	}
	for _, want := range []string{"inbox-0", "inbox-1", "inbox-2", "arch-0", "arch-1"} {
		if !subjects[want] {
			t.Errorf("missing subject %q in batch result %v", want, subjects)
		}
	}
	// Grouping actually spanned both folders (INBOX + a mapped Archive label).
	if !labels[model.LabelInbox] {
		t.Errorf("expected an INBOX-labeled message; labels seen: %v", labels)
	}
}

func hasLabelID(ls []model.Label, id string) bool {
	for _, l := range ls {
		if l.GmailID == id {
			return true
		}
	}
	return false
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
