package syncer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestRunBodyPrefetch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
	}{
		{"startup and new arrivals", 2},
		{"backlog spans multiple batches", bodyPrefetchBatch + 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s, accountID := newResyncStore(t)
			for i := 0; i < tc.count; i++ {
				id := fmt.Sprintf("m%d", i)
				if _, err := s.UpsertMessage(ctx, model.Message{AccountID: accountID, GmailID: id, ThreadID: id, InternalDate: time.Unix(200, 0), Labels: []string{model.LabelInbox}}); err != nil {
					t.Fatal(err)
				}
			}
			for _, m := range []model.Message{
				{AccountID: accountID, GmailID: "too-old", ThreadID: "too-old", InternalDate: time.Unix(100, 0)},
				{AccountID: accountID, GmailID: "cached", ThreadID: "cached", InternalDate: time.Unix(200, 0)},
				{AccountID: accountID, GmailID: "offline", ThreadID: "offline", InternalDate: time.Unix(200, 0)},
				{AccountID: accountID, GmailID: "archived", ThreadID: "m0", InternalDate: time.Unix(200, 0)},
			} {
				if m.GmailID != "archived" {
					m.Labels = []string{model.LabelInbox}
				}
				rowID, err := s.UpsertMessage(ctx, m)
				if err != nil {
					t.Fatal(err)
				}
				if m.GmailID == "cached" {
					if err := s.UpsertBody(ctx, model.MessageBody{MessageRowID: rowID, Text: "cached"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			other, err := s.UpsertAccount(ctx, model.Account{Email: "other@example.com"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.UpsertMessage(ctx, model.Message{AccountID: other, GmailID: "other", ThreadID: "other", Labels: []string{model.LabelInbox}}); err != nil {
				t.Fatal(err)
			}
			var hits, failures atomic.Int32
			b := &bodyBackend{fetch: func(_ context.Context, id string) (model.MessageBody, []model.Attachment, error) {
				hits.Add(1)
				if id == "offline" {
					failures.Add(1)
					return model.MessageBody{}, nil, errors.New("temporarily offline")
				}
				return model.MessageBody{Text: "body " + id}, nil, nil
			}}
			hub := NewHub()
			changes, unsubscribe := hub.Subscribe()
			defer unsubscribe()
			e := NewEngine(s, hub)
			stopped := make(chan struct{})
			go func() {
				e.RunBodyPrefetch(ctx, b, nil, accountID, "a@example.com", func() (int64, error) { return 200, nil })
				close(stopped)
			}()
			defer func() {
				cancel()
				select {
				case <-stopped:
				case <-time.After(5 * time.Second):
					t.Error("prefetch did not stop")
				}
			}()
			waitBodies := func(n int) {
				t.Helper()
				for n > 0 {
					select {
					case c := <-changes:
						if c.Kind == MessageBodyFetched {
							m, err := s.GetMessage(ctx, c.AccountID, c.GmailID)
							if err != nil || !m.BodyFetched {
								t.Fatalf("event preceded cached body: %v", err)
							}
							n--
						}
					case <-ctx.Done():
						t.Fatalf("still waiting for %d prefetched bodies", n)
					}
				}
			}
			waitBodies(tc.count)
			if _, err := s.UpsertMessage(ctx, model.Message{AccountID: accountID, GmailID: "arrival", ThreadID: "arrival", InternalDate: time.Unix(200, 0), Labels: []string{model.LabelInbox}}); err != nil {
				t.Fatal(err)
			}
			hub.Publish(Change{Kind: MessageUpserted, AccountID: accountID, GmailID: "arrival"})
			waitBodies(1)
			cancel()
			<-stopped
			if got := hits.Load(); got != int32(tc.count+2) {
				t.Fatalf("body fetches = %d, want %d eligible messages plus one failure", got, tc.count+1)
			}
			if failures.Load() != 1 {
				t.Fatalf("failed message retried before cooldown: %d fetches", failures.Load())
			}
		})
	}
}

func TestPrefetchBodyRetry(t *testing.T) {
	ctx := context.Background()
	s, accountID := newResyncStore(t)
	if _, err := s.UpsertMessage(ctx, model.Message{AccountID: accountID, GmailID: "m", ThreadID: "t", Labels: []string{model.LabelInbox}}); err != nil {
		t.Fatal(err)
	}
	var hits int
	b := &bodyBackend{fetch: func(context.Context, string) (model.MessageBody, []model.Attachment, error) {
		hits++
		if hits == 1 {
			return model.MessageBody{}, nil, errors.New("offline")
		}
		return model.MessageBody{Text: "retried"}, nil, nil
	}}
	e := NewEngine(s, nil)
	now := time.Unix(1000, 0)
	e.Now = func() time.Time { return now }
	retries := make(map[int64]time.Time)
	for _, tc := range []struct {
		name    string
		advance time.Duration
		want    int
	}{
		{"first failure", 0, 1},
		{"cooldown", bodyPrefetchInterval - time.Second, 1},
		{"retry after cooldown", time.Second, 2},
		{"cached body stays cached", bodyPrefetchInterval, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now = now.Add(tc.advance)
			msgs, err := s.MessagesMissingBodies(ctx, accountID, 0, 0, bodyPrefetchBatch)
			if err != nil {
				t.Fatal(err)
			}
			e.prefetchBodyBatch(ctx, b, nil, accountID, "a@example.com", msgs, retries)
			if hits != tc.want {
				t.Fatalf("fetches = %d, want %d", hits, tc.want)
			}
		})
	}
}

func TestBodyPrefetchChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Change
		want bool
	}{
		{"metadata batch", Change{Kind: MessageUpserted, AccountID: 1}, true},
		{"backfill batch", Change{Kind: BackfillProgress, AccountID: 1}, true},
		{"backfill complete", Change{Kind: BackfillComplete, AccountID: 1}, true},
		{"other account", Change{Kind: MessageUpserted, AccountID: 2}, false},
		{"body fetch echo", Change{Kind: MessageBodyFetched, AccountID: 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bodyPrefetchChange(tc.c, 1); got != tc.want {
				t.Fatalf("wakes prefetch = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBodyPrefetchConcurrencyAndCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, accountID := newResyncStore(t)
	for i := 0; i < bodyPrefetchWorkers+2; i++ {
		id := fmt.Sprintf("m%d", i)
		if _, err := s.UpsertMessage(ctx, model.Message{AccountID: accountID, GmailID: id, ThreadID: id, Labels: []string{model.LabelInbox}}); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan struct{}, bodyPrefetchBatch)
	var active, peak atomic.Int32
	b := &bodyBackend{fetch: func(ctx context.Context, _ string) (model.MessageBody, []model.Attachment, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); n > previous; previous = peak.Load() {
			if peak.CompareAndSwap(previous, n) {
				break
			}
		}
		started <- struct{}{}
		<-ctx.Done()
		return model.MessageBody{}, nil, ctx.Err()
	}}
	e := NewEngine(s, nil)
	stopped := make(chan struct{})
	go func() {
		e.RunBodyPrefetch(ctx, b, nil, accountID, "a@example.com", nil)
		close(stopped)
	}()
	for i := 0; i < bodyPrefetchWorkers; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("prefetch did not fill the worker slots")
		}
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("canceling the account did not stop prefetch")
	}
	if peak.Load() != bodyPrefetchWorkers || active.Load() != 0 {
		t.Fatalf("peak concurrent fetches = %d; %d still active", peak.Load(), active.Load())
	}
}

func TestBodyPrefetchRechecksInbox(t *testing.T) {
	for _, tc := range []struct {
		name    string
		archive bool
		want    int
	}{
		{"still in Inbox", false, 1},
		{"archived after being queued", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, accountID := newResyncStore(t)
			if _, err := s.UpsertMessage(ctx, model.Message{AccountID: accountID, GmailID: "m", ThreadID: "t", Labels: []string{model.LabelInbox}}); err != nil {
				t.Fatal(err)
			}
			queued, err := s.MessagesMissingBodies(ctx, accountID, 0, 0, bodyPrefetchBatch)
			if err != nil || len(queued) != 1 {
				t.Fatalf("queued messages = %v, err %v", queued, err)
			}
			if tc.archive {
				if err := s.ModifyLabels(ctx, accountID, "m", nil, []string{model.LabelInbox}); err != nil {
					t.Fatal(err)
				}
			}
			var hits int
			b := &bodyBackend{fetch: func(context.Context, string) (model.MessageBody, []model.Attachment, error) {
				hits++
				return model.MessageBody{Text: "body"}, nil, nil
			}}
			e := NewEngine(s, nil)
			e.prefetchBodyBatch(ctx, b, nil, accountID, "a@example.com", queued, make(map[int64]time.Time))
			if hits != tc.want {
				t.Fatalf("body requests = %d, want %d", hits, tc.want)
			}
			// Opening an archived message still fetches its body on demand.
			if tc.archive {
				if err := e.FetchBody(ctx, b, accountID, "m"); err != nil {
					t.Fatal(err)
				}
				if hits != 1 {
					t.Fatalf("on-demand body requests = %d, want 1", hits)
				}
			}
		})
	}
}
