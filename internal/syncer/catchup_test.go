package syncer

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/activity"
	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

// logBackend is a ChangeLogger whose every provider call is recorded in order,
// with the priority each fetch ran at.
type logBackend struct {
	countingBackend
	mu      sync.Mutex
	calls   []string
	changes []backend.MessageChange
	deletes []string
	next    string
	labels  map[string][]string // labels a fetched message carries
	search  []string
	profile string
}

func (f *logBackend) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *logBackend) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *logBackend) ChangeLog(_ context.Context, _ string, progress func(done, total int)) ([]backend.MessageChange, []string, string, error) {
	f.record("changelog")
	if progress != nil {
		progress(1, 2)
	}
	return f.changes, f.deletes, f.next, nil
}

func (f *logBackend) FetchMetadata(ctx context.Context, id string) (model.Message, error) {
	f.record("fetch:" + id + "@" + backend.PriorityOf(ctx).String())
	return model.Message{AccountID: 1, GmailID: id, ThreadID: "t-" + id, Labels: f.labels[id], IsUnread: slices.Contains(f.labels[id], model.LabelUnread)}, nil
}

func (f *logBackend) Profile(context.Context) (backend.Profile, error) {
	return backend.Profile{Cursor: f.profile}, nil
}

func (f *logBackend) SearchIDs(context.Context, string, int) ([]string, error) {
	f.record("search")
	return f.search, nil
}

// inboxBackend adds the inbox listing.
type inboxBackend struct {
	logBackend
	inbox backend.InboxSnapshot
}

func (f *inboxBackend) ListInbox(context.Context, int) (backend.InboxSnapshot, error) {
	f.record("list-inbox")
	return f.inbox, nil
}

func cache(t *testing.T, s *store.Store, account int64, id string, labels ...string) {
	t.Helper()
	m := model.Message{AccountID: account, GmailID: id, ThreadID: "t-" + id, Subject: "s-" + id, Labels: labels, IsUnread: slices.Contains(labels, model.LabelUnread)}
	if err := s.UpsertMessages(context.Background(), []model.Message{m}); err != nil {
		t.Fatal(err)
	}
}

func labelsOf(t *testing.T, s *store.Store, account int64, id string) []string {
	t.Helper()
	ids, err := s.MessageIDsWithLabel(context.Background(), account, model.LabelInbox, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	if ids[id] {
		out = append(out, model.LabelInbox)
	}
	m, err := s.GetMessage(context.Background(), account, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.IsUnread {
		out = append(out, model.LabelUnread)
	}
	return out
}

func fetchIndex(calls []string, id string) int {
	return slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "fetch:"+id+"@") })
}

// A long catch-up applies label changes to cached messages without fetching
// them, fetches new inbox mail before anything else and at inbox priority, and
// leaves the rest to background priority.
func TestIncrementalInboxFirstAndLabelsWithoutFetching(t *testing.T) {
	ctx := context.Background()
	s, acct := newIncStore(t, "c1")
	cache(t, s, acct, "read-elsewhere", model.LabelInbox, model.LabelUnread)
	b := &logBackend{
		next: "c2",
		changes: []backend.MessageChange{
			{ID: "newest", Labels: []string{model.LabelInbox, model.LabelUnread}, Inbox: true},
			{ID: "archived-list", Labels: []string{"CATEGORY_UPDATES"}},
			{ID: "read-elsewhere", Labels: []string{model.LabelInbox}, Inbox: true},
			{ID: "older", Labels: []string{model.LabelInbox}, Inbox: true},
		},
		labels: map[string][]string{"newest": {model.LabelInbox, model.LabelUnread}, "older": {model.LabelInbox}},
	}
	e := &Engine{Store: s}
	n, err := e.Incremental(ctx, b, acct)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("changed = %d, want 4 (1 relabelled + 3 fetched)", n)
	}
	calls := b.log()
	if i := fetchIndex(calls, "read-elsewhere"); i >= 0 {
		t.Fatalf("cached message was fetched again: %v", calls)
	}
	if got := labelsOf(t, s, acct, "read-elsewhere"); !slices.Equal(got, []string{model.LabelInbox}) {
		t.Fatalf("read-elsewhere labels = %v, want just INBOX (read)", got)
	}
	for _, id := range []string{"newest", "older"} {
		if !slices.Contains(calls, "fetch:"+id+"@inbox") {
			t.Fatalf("%s not fetched at inbox priority: %v", id, calls)
		}
		if fetchIndex(calls, id) > fetchIndex(calls, "archived-list") {
			t.Fatalf("inbox mail %s fetched after background mail: %v", id, calls)
		}
	}
	if !slices.Contains(calls, "fetch:archived-list@background") {
		t.Fatalf("non-inbox mail not fetched at background priority: %v", calls)
	}
	acc, err := s.GetAccountByID(ctx, acct)
	if err != nil || acc.SyncCursor != "c2" {
		t.Fatalf("cursor = %q, %v; want c2", acc.SyncCursor, err)
	}
}

// A label change the user made here and that has not reached the provider yet
// must not be undone by the provider's older state; the message is fetched
// instead, after everything else.
func TestIncrementalLeavesPendingLabelOpsAlone(t *testing.T) {
	ctx := context.Background()
	s, acct := newIncStore(t, "c1")
	cache(t, s, acct, "mine", model.LabelInbox)
	if err := s.ModifyLabelsBatchAndEnqueue(ctx, acct, []string{"mine"}, nil, []string{model.LabelInbox}); err != nil {
		t.Fatal(err)
	}
	b := &logBackend{next: "c2", changes: []backend.MessageChange{{ID: "mine", Labels: []string{model.LabelInbox, model.LabelUnread}, Inbox: true}}}
	e := &Engine{Store: s}
	if _, err := e.Incremental(ctx, b, acct); err != nil {
		t.Fatal(err)
	}
	if calls := b.log(); !slices.Contains(calls, "fetch:mine@background") {
		t.Fatalf("pending message not deferred to a background fetch: %v", calls)
	}
}

// After a gap the inbox is listed and corrected before the change log is read:
// archived and read elsewhere, new mail fetched first.
func TestIncrementalRefreshesInboxBeforeChangeLog(t *testing.T) {
	ctx := context.Background()
	s, acct := newIncStore(t, "c1")
	cache(t, s, acct, "archived-elsewhere", model.LabelInbox, model.LabelUnread)
	cache(t, s, acct, "read-elsewhere", model.LabelInbox, model.LabelUnread)
	cache(t, s, acct, "mine", model.LabelInbox)
	// Starred here, not yet mirrored: the inbox refresh must leave it alone.
	if err := s.ModifyLabelsBatchAndEnqueue(ctx, acct, []string{"mine"}, []string{model.LabelStarred}, nil); err != nil {
		t.Fatal(err)
	}
	b := &inboxBackend{
		logBackend: logBackend{next: "c2", labels: map[string][]string{"fresh": {model.LabelInbox, model.LabelUnread}}},
		inbox:      backend.InboxSnapshot{IDs: []string{"fresh", "read-elsewhere"}, Unread: []string{"fresh"}, Complete: true},
	}
	e := &Engine{Store: s}
	if _, err := e.Incremental(ctx, b, acct); err != nil {
		t.Fatal(err)
	}
	calls := b.log()
	want := []string{"list-inbox", "fetch:fresh@inbox", "changelog"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for id, want := range map[string][]string{
		"archived-elsewhere": {model.LabelUnread}, // archived, still unread
		"read-elsewhere":     {model.LabelInbox},
		"mine":               {model.LabelInbox},
		"fresh":              {model.LabelInbox, model.LabelUnread},
	} {
		if got := labelsOf(t, s, acct, id); !slices.Equal(got, want) {
			t.Errorf("%s labels = %v, want %v", id, got, want)
		}
	}

	// The next pass a minute later is not a catch-up: no inbox listing.
	b.calls = nil
	if _, err := e.Incremental(ctx, b, acct); err != nil {
		t.Fatal(err)
	}
	if calls := b.log(); slices.Contains(calls, "list-inbox") {
		t.Fatalf("listed the inbox again right after a pass: %v", calls)
	}

	// After a long gap it is again.
	e.Now = func() time.Time { return time.Now().Add(time.Hour) }
	b.calls = nil
	if _, err := e.Incremental(ctx, b, acct); err != nil {
		t.Fatal(err)
	}
	if calls := b.log(); !slices.Contains(calls, "list-inbox") {
		t.Fatalf("no inbox listing after a long gap: %v", calls)
	}
}

// A partial inbox listing only adds: a message missing from it may simply be
// past the cap.
func TestInboxRefreshPartialListingRemovesNothing(t *testing.T) {
	ctx := context.Background()
	s, acct := newIncStore(t, "c1")
	cache(t, s, acct, "old", model.LabelInbox, model.LabelUnread)
	cache(t, s, acct, "listed", model.LabelInbox, model.LabelUnread)
	b := &inboxBackend{inbox: backend.InboxSnapshot{IDs: []string{"listed"}, Complete: false}}
	e := &Engine{Store: s}
	if _, _, err := e.refreshInbox(ctx, b, acct, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old", "listed"} {
		if got := labelsOf(t, s, acct, id); !slices.Equal(got, []string{model.LabelInbox, model.LabelUnread}) {
			t.Errorf("%s labels = %v; a partial listing must not archive or mark read", id, got)
		}
	}
}

// A resync after an expired cursor reuses the inbox refresh the failed
// incremental just did, and its backfill skips what that refresh fetched.
func TestResyncReusesInboxRefresh(t *testing.T) {
	ctx := context.Background()
	s, acct := newIncStore(t, "expired")
	b := &inboxBackend{
		logBackend: logBackend{profile: "p1", search: []string{"inbox-new", "a1", "a2"}},
		inbox:      backend.InboxSnapshot{IDs: []string{"inbox-new"}, Complete: true},
	}
	e := &Engine{Store: s}
	if _, _, err := e.refreshInbox(backend.WithPriority(ctx, backend.PriorityInbox), b, acct, nil); err != nil {
		t.Fatal(err)
	}
	b.calls = nil
	n, err := e.Resync(ctx, b, acct, 500)
	if err != nil {
		t.Fatal(err)
	}
	calls := b.log()
	if slices.Contains(calls, "list-inbox") || fetchIndex(calls, "inbox-new") >= 0 {
		t.Fatalf("resync repeated the inbox refresh: %v", calls)
	}
	for _, id := range []string{"a1", "a2"} {
		if !slices.Contains(calls, "fetch:"+id+"@background") {
			t.Fatalf("%s not backfilled at background priority: %v", id, calls)
		}
	}
	if n != 2 {
		t.Fatalf("resync stored %d, want 2", n)
	}
	acc, err := s.GetAccountByID(ctx, acct)
	if err != nil || acc.SyncCursor != "p1" {
		t.Fatalf("cursor = %q, %v; want p1", acc.SyncCursor, err)
	}
}

// A long catch-up shows its bulk as its own row with a count, so the status bar
// says what is happening instead of one "Checking mail" spinner for minutes.
func TestIncrementalProgressRows(t *testing.T) {
	ctx := context.Background()
	s, acct := newIncStore(t, "c1")
	b := &logBackend{next: "c2"}
	for i := range 30 {
		b.changes = append(b.changes, backend.MessageChange{ID: fmt.Sprintf("m%d", i), Labels: []string{"CATEGORY_UPDATES"}})
	}
	hub := activity.NewHub()
	events, unsubscribe := hub.Subscribe()
	defer unsubscribe()
	e := &Engine{Store: s}
	p := NewProgress(hub, "a@example.com")
	n, err := e.IncrementalWithProgress(ctx, b, acct, p)
	p.Finish(err, "fallback")
	if err != nil || n != 30 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var got []string
	var last activity.Event
	for len(events) > 0 {
		ev := <-events
		switch ev.Phase {
		case activity.Start:
			got = append(got, "start "+ev.Label)
		case activity.Done:
			got = append(got, "done "+ev.Label+" · "+ev.Note)
		case activity.Progress:
			last = ev
		}
	}
	want := []string{"start Checking mail", "done Checking mail · ", "start Syncing other mail", "done Syncing other mail · 30 messages"}
	if !slices.Equal(got, want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	if last.Label != LabelSyncingOther || last.Done != 30 || last.Total != 30 || last.Unit != "messages" {
		t.Fatalf("last progress = %+v, want 30 of 30 messages", last)
	}
}
