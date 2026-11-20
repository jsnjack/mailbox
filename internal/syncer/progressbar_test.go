package syncer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/activity"
	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/model"
)

// progressEvents drains ch and returns its Progress events for label.
func progressEvents(ch <-chan activity.Event, label string) []activity.Event {
	var out []activity.Event
	for {
		select {
		case e := <-ch:
			if e.Phase == activity.Progress && e.Label == label {
				out = append(out, e)
			}
		default:
			return out
		}
	}
}

// checkBar fails unless events draw one bar of total units: from zero,
// never backwards, to the end.
func checkBar(t *testing.T, events []activity.Event, total int, unit string) {
	t.Helper()
	var bar []activity.Event
	for _, e := range events {
		if e.Total == total && e.Unit == unit {
			bar = append(bar, e)
		}
	}
	if len(bar) < 2 || bar[0].Done != 0 || bar[len(bar)-1].Done != total {
		t.Fatalf("bar of %d %s = %+v, want 0 … %d", total, unit, events, total)
	}
	for i := 1; i < len(bar); i++ {
		if bar[i].Done < bar[i-1].Done {
			t.Fatalf("bar ran backwards: %+v", bar)
		}
	}
}

// A pass that fetches a few new messages shows them as a bar on its
// "Checking mail" row, not just a spinner.
func TestIncrementalShowsFetchProgress(t *testing.T) {
	ctx := context.Background()
	s, acct := newIncStore(t, "c1")
	var changes []backend.MessageChange
	labels := map[string][]string{}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("new%d", i)
		changes = append(changes, backend.MessageChange{ID: id, Labels: []string{model.LabelInbox}, Inbox: true})
		labels[id] = []string{model.LabelInbox}
	}
	changes = append(changes, backend.MessageChange{ID: "list-mail", Labels: []string{"CATEGORY_UPDATES"}})
	b := &logBackend{next: "c2", changes: changes, labels: labels}
	hub := activity.NewHub()
	ch, unsubscribe := hub.Subscribe()
	defer unsubscribe()
	p := NewProgress(hub, "a@example.com")
	if _, err := (&Engine{Store: s}).IncrementalWithProgress(ctx, b, acct, p); err != nil {
		t.Fatal(err)
	}
	p.Finish(nil, "")
	// Inbox and the rest share one bar: the rest is too small for its own row.
	checkBar(t, progressEvents(ch, LabelCheckingMail), 4, "messages")
}

// A prefetch batch fills a bar body by body.
func TestBodyPrefetchShowsProgress(t *testing.T) {
	ctx := context.Background()
	s, acct := newResyncStore(t)
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("m%d", i)
		if _, err := s.UpsertMessage(ctx, model.Message{AccountID: acct, GmailID: id, ThreadID: id, Labels: []string{model.LabelInbox}}); err != nil {
			t.Fatal(err)
		}
	}
	queued, err := s.MessagesMissingBodies(ctx, acct, 0, 0, bodyPrefetchBatch)
	if err != nil {
		t.Fatal(err)
	}
	b := &bodyBackend{fetch: func(context.Context, string) (model.MessageBody, []model.Attachment, error) {
		time.Sleep(time.Millisecond)
		return model.MessageBody{Text: "body"}, nil, nil
	}}
	hub := activity.NewHub()
	ch, unsubscribe := hub.Subscribe()
	defer unsubscribe()
	NewEngine(s, nil).prefetchBodyBatch(ctx, b, hub, acct, "a@example.com", queued, make(map[int64]time.Time))
	checkBar(t, progressEvents(ch, "Prefetching message bodies"), 5, "bodies")
}

// A server search shows how many of its results it has fetched.
func TestSearchShowsFetchProgress(t *testing.T) {
	ctx := context.Background()
	s, acct := newIncStore(t, "c1")
	b := &logBackend{search: []string{"r1", "r2", "r3"}}
	var (
		mu  sync.Mutex
		got [][2]int
	)
	if _, err := (&Engine{Store: s}).SearchServer(ctx, b, acct, "from:a", 10, func(done, total int) {
		mu.Lock() // called from the fetching goroutines
		defer mu.Unlock()
		got = append(got, [2]int{done, total})
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0] != [2]int{0, 3} {
		t.Fatalf("progress = %v, want 0/3 then one call per fetched result", got)
	}
	reached := 0
	for _, g := range got {
		reached = max(reached, g[0])
	}
	if reached != 3 {
		t.Fatalf("progress = %v, never reached 3 of 3", got)
	}
}
