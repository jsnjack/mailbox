package syncer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/model"
)

type bodyBackend struct {
	backend.Backend
	fetch func(context.Context, string) (model.MessageBody, []model.Attachment, error)
}

func (b *bodyBackend) FetchBody(ctx context.Context, id string) (model.MessageBody, []model.Attachment, error) {
	return b.fetch(ctx, id)
}

func TestEnsureBody(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cached   bool
		force    bool
		fail     bool
		wantHits int32
	}{
		{"missing body", false, false, false, 1},
		{"cached body", true, false, false, 0},
		{"explicit refresh", true, true, false, 1},
		{"failed body remains retryable", false, false, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, accountID := newResyncStore(t)
			rowID, err := s.UpsertMessage(ctx, model.Message{AccountID: accountID, GmailID: "m", ThreadID: "t"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.cached {
				if err := s.UpsertBody(ctx, model.MessageBody{MessageRowID: rowID, Text: "old"}); err != nil {
					t.Fatal(err)
				}
			}
			var hits atomic.Int32
			failure := errors.New("offline")
			b := &bodyBackend{fetch: func(context.Context, string) (model.MessageBody, []model.Attachment, error) {
				hits.Add(1)
				if tc.fail {
					return model.MessageBody{}, nil, failure
				}
				return model.MessageBody{Text: "new", HTML: "<p>new</p>"}, []model.Attachment{{GmailAttID: "att", Filename: "test.txt", MimeType: "text/plain"}}, nil
			}}
			e := NewEngine(s, nil)
			fetch := e.EnsureBody
			if tc.force {
				fetch = e.FetchBody
			}
			for i := 0; i < 2; i++ {
				err := fetch(ctx, b, accountID, "m")
				if tc.fail && !errors.Is(err, failure) || !tc.fail && err != nil {
					t.Fatalf("fetch: %v", err)
				}
				if tc.force {
					break
				}
			}
			if hits.Load() != tc.wantHits {
				t.Fatalf("body requests = %d, want %d", hits.Load(), tc.wantHits)
			}
			m, err := s.GetMessage(ctx, accountID, "m")
			if err != nil || m.BodyFetched == tc.fail {
				t.Fatalf("cached message = %+v, err %v", m, err)
			}
			if !tc.fail && tc.wantHits > 0 {
				atts, err := s.ListAttachments(ctx, rowID)
				if err != nil || len(atts) != 1 {
					t.Fatalf("cached attachments = %v, err %v", atts, err)
				}
			}
		})
	}
}

func TestBodyFetchSharing(t *testing.T) {
	for _, cancelWaiter := range []bool{false, true} {
		name := "reader shares prefetch"
		if cancelWaiter {
			name = "canceling reader leaves prefetch running"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s, accountID := newResyncStore(t)
			if _, err := s.UpsertMessage(ctx, model.Message{AccountID: accountID, GmailID: "m", ThreadID: "t"}); err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			release := make(chan struct{})
			var hits atomic.Int32
			b := &bodyBackend{fetch: func(ctx context.Context, _ string) (model.MessageBody, []model.Attachment, error) {
				if hits.Add(1) == 1 {
					close(started)
				}
				select {
				case <-ctx.Done():
					return model.MessageBody{}, nil, ctx.Err()
				case <-release:
					return model.MessageBody{Text: "prefetched"}, nil, nil
				}
			}}
			e := NewEngine(s, nil)
			owner := make(chan error, 1)
			go func() { owner <- e.EnsureBody(ctx, b, accountID, "m") }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("prefetch did not start")
			}
			waitCtx, cancelReader := context.WithCancel(ctx)
			defer cancelReader()
			waiting := &bodyWaitContext{Context: waitCtx, entered: make(chan struct{})}
			reader := make(chan error, 1)
			go func() { reader <- e.FetchBody(waiting, b, accountID, "m") }()
			select {
			case <-waiting.entered:
			case <-ctx.Done():
				t.Fatal("reader did not join prefetch")
			}
			if cancelWaiter {
				cancelReader()
				if err := <-reader; !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled reader: %v", err)
				}
			}
			close(release)
			if err := <-owner; err != nil {
				t.Fatal(err)
			}
			if !cancelWaiter {
				if err := <-reader; err != nil {
					t.Fatal(err)
				}
			}
			if hits.Load() != 1 {
				t.Fatalf("overlapping requests fetched %d times", hits.Load())
			}
		})
	}
}

type bodyWaitContext struct {
	context.Context
	entered chan struct{}
}

func (c *bodyWaitContext) Done() <-chan struct{} {
	close(c.entered)
	return c.Context.Done()
}
