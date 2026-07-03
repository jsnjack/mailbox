package imapbackend

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/jsnjack/mailbox/internal/model"
)

func TestParseSearchQuery(t *testing.T) {
	cases := []struct {
		query     string
		wantLabel string
		wantHdrs  int
		wantText  int
		wantErr   bool
	}{
		{query: "", wantLabel: ""},
		{query: "in:trash", wantLabel: model.LabelTrash},
		{query: "in:spam", wantLabel: model.LabelSpam},
		{query: "in:junk", wantLabel: model.LabelSpam},
		{query: "in:INBOX", wantLabel: model.LabelInbox},
		{query: "in:Work", wantLabel: "Work"},
		{query: "from:bob@example.com", wantHdrs: 1},
		{query: "subject:hello from:bob@x.com", wantHdrs: 2},
		{query: "quarterly report", wantText: 2},
		{query: "in:Work budget", wantLabel: "Work", wantText: 1},
		{query: "http://example.com/x", wantText: 1}, // colon but not an operator → free text
		{query: "is:unread", wantErr: true},
		{query: "rfc822msgid:<x@y>", wantErr: true},
		{query: "newer_than:7d", wantErr: true},
		{query: "in:trash in:spam", wantErr: true}, // conflicting scopes
		{query: "in:", wantErr: true},
		{query: "from:", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			q, err := parseSearchQuery(c.query)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseSearchQuery(%q) = %+v, want error", c.query, q)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSearchQuery(%q): %v", c.query, err)
			}
			if q.label != c.wantLabel {
				t.Errorf("label = %q, want %q", q.label, c.wantLabel)
			}
			if len(q.criteria.Header) != c.wantHdrs {
				t.Errorf("headers = %d, want %d", len(q.criteria.Header), c.wantHdrs)
			}
			if len(q.criteria.Text) != c.wantText {
				t.Errorf("text terms = %d, want %d", len(q.criteria.Text), c.wantText)
			}
		})
	}
}

// startSearchServer stands up a memserver with INBOX (2 msgs from bob) and a
// "Work" folder (1 msg from carol), plus an alphabetically-early "AAA" folder
// (2 msgs) to prove backfill priority.
func startSearchServer(t *testing.T) *Backend {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("alice@example.com", "secret")
	appendMsg := func(mailbox, from, subject string) {
		raw := fmt.Sprintf("From: %s\r\nSubject: %s\r\n"+
			"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\nContent-Type: text/plain\r\n\r\nbody\r\n", from, subject)
		if _, err := user.Append(mailbox, bytes.NewReader([]byte(raw)), &imap.AppendOptions{}); err != nil {
			t.Fatalf("append to %s: %v", mailbox, err)
		}
	}
	for _, f := range []string{"INBOX", "Work", "AAA"} {
		if err := user.Create(f, nil); err != nil {
			t.Fatalf("create %s: %v", f, err)
		}
	}
	appendMsg("INBOX", "bob@example.com", "inbox one")
	appendMsg("INBOX", "bob@example.com", "inbox two")
	appendMsg("Work", "carol@example.com", "work stuff")
	appendMsg("AAA", "dave@example.com", "aaa one")
	appendMsg("AAA", "dave@example.com", "aaa two")
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
	return b
}

func TestSearchIDsScopedByLabel(t *testing.T) {
	b := startSearchServer(t)
	ctx := context.Background()

	// in:<user folder> restricts to that folder only.
	ids, err := b.SearchIDs(ctx, "in:Work", 0)
	if err != nil {
		t.Fatalf("SearchIDs(in:Work): %v", err)
	}
	if len(ids) != 1 || !strings.HasSuffix(ids[0], ":Work") {
		t.Fatalf("in:Work = %v, want exactly the one Work message", ids)
	}

	// in:trash with no \Trash folder must ERROR — never "all messages" (this is
	// the Empty Trash query; falling back would delete the whole account).
	if ids, err := b.SearchIDs(ctx, "in:trash", 0); err == nil {
		t.Fatalf("SearchIDs(in:trash) with no Trash folder = %v, want error", ids)
	}

	// An unsupported operator must error, not degrade to all messages.
	if ids, err := b.SearchIDs(ctx, "is:unread", 0); err == nil {
		t.Fatalf("SearchIDs(is:unread) = %v, want error", ids)
	}
}

func TestSearchIDsFromCriteria(t *testing.T) {
	b := startSearchServer(t)
	ctx := context.Background()

	ids, err := b.SearchIDs(ctx, "from:carol@example.com", 0)
	if err != nil {
		t.Fatalf("SearchIDs(from:): %v", err)
	}
	if len(ids) != 1 || !strings.HasSuffix(ids[0], ":Work") {
		t.Fatalf("from:carol = %v, want the single Work message", ids)
	}
}

// A capped backfill must fill with INBOX before alphabetically-earlier folders,
// or a fresh account's cap can be eaten entirely by an archive folder and the
// inbox never appears.
func TestSearchIDsPriorityOrderINBOXFirst(t *testing.T) {
	b := startSearchServer(t)
	ctx := context.Background()

	ids, err := b.SearchIDs(ctx, "", 2)
	if err != nil {
		t.Fatalf("SearchIDs capped: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d ids, want 2", len(ids))
	}
	for _, id := range ids {
		if !strings.HasSuffix(id, ":INBOX") {
			t.Fatalf("capped backfill returned non-INBOX id %q first (ids=%v); INBOX must have priority", id, ids)
		}
	}
}
