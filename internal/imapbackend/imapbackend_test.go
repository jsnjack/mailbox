package imapbackend

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
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
	}, 1, "secret")
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
