package syncer

import (
	"context"
	"slices"
	"testing"

	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

// fullBackend is an inboxBackend that can fetch a message's metadata and body
// in one request, as Gmail's format=full does.
type fullBackend struct {
	inboxBackend
}

func (f *fullBackend) FetchFull(ctx context.Context, id string) (backend.FullMessage, error) {
	f.record("full:" + id + "@" + backend.PriorityOf(ctx).String())
	if id == "vanished" {
		return backend.FullMessage{}, backend.ErrNotFound
	}
	return backend.FullMessage{
		Message: model.Message{AccountID: 1, GmailID: id, ThreadID: "t-" + id, Labels: f.labels[id]},
		Body:    model.MessageBody{Text: "body of " + id},
	}, nil
}

// bodyState reports whether id is cached with its body, and the body's text.
func bodyState(t *testing.T, s *store.Store, account int64, id string) (bool, string) {
	t.Helper()
	m, err := s.GetMessage(context.Background(), account, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	if !m.BodyFetched {
		return false, ""
	}
	body, err := s.GetBody(context.Background(), m.RowID)
	if err != nil {
		t.Fatalf("body of %s: %v", id, err)
	}
	return true, body.Text
}

// New Inbox mail is stored with its body, fetched in the same request as its
// metadata, so it never appears in the list before it can be opened without a
// download. The rest of the change log keeps the cheaper metadata fetch.
func TestIncrementalStoresInboxMailWithBody(t *testing.T) {
	ctx := context.Background()
	s, acct := newIncStore(t, "c1")
	b := &fullBackend{inboxBackend{logBackend: logBackend{
		next: "c2",
		changes: []backend.MessageChange{
			{ID: "newest", Labels: []string{model.LabelInbox, model.LabelUnread}, Inbox: true},
			{ID: "vanished", Labels: []string{model.LabelInbox}, Inbox: true},
			{ID: "archived-list", Labels: []string{"CATEGORY_UPDATES"}},
		},
		labels: map[string][]string{"newest": {model.LabelInbox, model.LabelUnread}},
	}}}
	hub := NewHub()
	changes, unsubscribe := hub.Subscribe()
	defer unsubscribe()
	// Every listener sees new Inbox mail only once its body is cached.
	checked := make(chan []string)
	go func() {
		var seen []string
		for c := range changes {
			if c.Kind != MessageUpserted || c.GmailID == "" {
				continue
			}
			m, err := s.GetMessage(ctx, acct, c.GmailID)
			if err == nil && slices.Contains(m.Labels, model.LabelInbox) && !m.BodyFetched {
				seen = append(seen, c.GmailID)
			}
		}
		checked <- seen
	}()
	e := NewEngine(s, hub)
	if _, err := e.Incremental(ctx, b, acct); err != nil {
		t.Fatal(err)
	}
	unsubscribe()
	if early := <-checked; len(early) > 0 {
		t.Fatalf("Inbox mail announced before its body was cached: %v", early)
	}

	calls := b.log()
	if !slices.Contains(calls, "full:newest@inbox") {
		t.Fatalf("new Inbox mail not fetched in full at inbox priority: %v", calls)
	}
	if i := fetchIndex(calls, "newest"); i >= 0 {
		t.Fatalf("new Inbox mail fetched twice (metadata as well): %v", calls)
	}
	if !slices.Contains(calls, "fetch:archived-list@background") {
		t.Fatalf("non-Inbox mail not fetched as metadata at background priority: %v", calls)
	}
	if fetched, text := bodyState(t, s, acct, "newest"); !fetched || text != "body of newest" {
		t.Fatalf("newest: body fetched=%v text=%q, want the body from the same request", fetched, text)
	}
	if fetched, _ := bodyState(t, s, acct, "archived-list"); fetched {
		t.Fatal("archived-list has a body; only Inbox mail is fetched in full")
	}
	// "vanished" was deleted before it was fetched: skipped, not held for retry.
	acc, err := s.GetAccountByID(ctx, acct)
	if err != nil || acc.SyncCursor != "c2" {
		t.Fatalf("cursor = %q, %v; want c2", acc.SyncCursor, err)
	}
}

// After a gap, the inbox refresh stores the mail it was missing with bodies too.
func TestInboxRefreshStoresBodies(t *testing.T) {
	ctx := context.Background()
	s, acct := newIncStore(t, "c1")
	b := &fullBackend{inboxBackend{
		logBackend: logBackend{next: "c1", labels: map[string][]string{"fresh": {model.LabelInbox, model.LabelUnread}}},
		inbox:      backend.InboxSnapshot{IDs: []string{"fresh"}, Unread: []string{"fresh"}, Complete: true},
	}}
	e := &Engine{Store: s}
	if _, err := e.Incremental(ctx, b, acct); err != nil {
		t.Fatal(err)
	}
	if calls := b.log(); !slices.Equal(calls, []string{"list-inbox", "full:fresh@inbox", "changelog"}) {
		t.Fatalf("calls = %v, want the listing, one full fetch, then the change log", calls)
	}
	if fetched, text := bodyState(t, s, acct, "fresh"); !fetched || text != "body of fresh" {
		t.Fatalf("fresh: body fetched=%v text=%q", fetched, text)
	}
}
