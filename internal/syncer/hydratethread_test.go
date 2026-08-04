package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

type threadBackend struct {
	countingBackend
	msgs  []model.Message
	err   error
	calls int
}

func (b *threadBackend) FetchThreadMetadata(context.Context, string) ([]model.Message, error) {
	b.calls++
	return b.msgs, b.err
}

func TestHydrateThreadCachesMissingMessagesOnce(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	acct, err := s.UpsertAccount(ctx, model.Account{Email: "a@example.com", Type: model.AccountGmail})
	if err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	reply := model.Message{AccountID: acct, GmailID: "m2", ThreadID: "t1", Subject: "Re: quote", InternalDate: time.Unix(2, 0)}
	if err := s.UpsertMessages(ctx, []model.Message{reply}); err != nil {
		t.Fatalf("seed reply: %v", err)
	}
	root := model.Message{AccountID: acct, GmailID: "m1", ThreadID: "t1", Subject: "quote", HasAttachments: true, InternalDate: time.Unix(1, 0)}
	be := &threadBackend{msgs: []model.Message{root, reply}}
	e := &Engine{Store: s}

	added, err := e.HydrateThread(ctx, be, acct, "t1")
	if err != nil {
		t.Fatalf("HydrateThread: %v", err)
	}
	if added != 1 || be.calls != 1 {
		t.Fatalf("first hydrate: added=%d calls=%d, want 1/1", added, be.calls)
	}
	msgs, err := s.ListThreadMessages(ctx, acct, "t1")
	if err != nil || len(msgs) != 2 || msgs[0].GmailID != "m1" {
		t.Fatalf("cached thread = %+v, err %v", msgs, err)
	}

	added, err = e.HydrateThread(ctx, be, acct, "t1")
	if err != nil || added != 0 || be.calls != 1 {
		t.Fatalf("second hydrate: added=%d calls=%d err=%v, want 0/1/nil", added, be.calls, err)
	}
}

func TestHydrateThreadRetriesAfterProviderFailure(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	acct, err := s.UpsertAccount(ctx, model.Account{Email: "a@example.com", Type: model.AccountGmail})
	if err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	be := &threadBackend{err: errors.New("offline")}
	e := &Engine{Store: s}

	if _, err := e.HydrateThread(ctx, be, acct, "t1"); err == nil {
		t.Fatal("HydrateThread error = nil, want provider failure")
	}
	be.err = nil
	be.msgs = []model.Message{{AccountID: acct, GmailID: "m1", ThreadID: "t1"}}
	if added, err := e.HydrateThread(ctx, be, acct, "t1"); err != nil || added != 1 || be.calls != 2 {
		t.Fatalf("retry: added=%d calls=%d err=%v, want 1/2/nil", added, be.calls, err)
	}
}
