package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

type labelCall struct {
	ids    []string
	add    []string
	remove []string
}

type labelBackend struct {
	countingBackend
	mu       sync.Mutex
	calls    []labelCall
	failNext bool
}

func (b *labelBackend) ApplyLabels(_ context.Context, ids, add, remove []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, labelCall{append([]string(nil), ids...), append([]string(nil), add...), append([]string(nil), remove...)})
	if b.failNext {
		b.failNext = false
		return errors.New("offline")
	}
	return nil
}

func TestPendingLabelOpsSurviveRestartAndPreserveOrder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mailbox.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := s.UpsertAccount(ctx, model.Account{Email: "offline@example.com", Type: model.AccountGmail})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMessages(ctx, []model.Message{{
		AccountID: acct, GmailID: "m1", ThreadID: "t1", Labels: []string{model.LabelInbox},
	}}); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, nil)
	if err := e.ModifyLabelsBatch(ctx, nil, acct, []string{"m1"}, []string{model.LabelStarred}, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.ModifyLabelsBatch(ctx, nil, acct, []string{"m1"}, nil, []string{model.LabelStarred}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	e = NewEngine(s, nil)
	be := &labelBackend{failNext: true}
	if n, err := e.SweepPendingLabelOps(ctx, be, acct); err == nil || n != 0 {
		t.Fatalf("failed sweep = %d, %v", n, err)
	}
	if ops, err := s.PendingLabelOps(ctx, acct, 10); err != nil || len(ops) != 2 || ops[0].Attempts != 1 {
		t.Fatalf("pending after failure = %+v, %v", ops, err)
	}
	if n, err := e.SweepPendingLabelOps(ctx, be, acct); err != nil || n != 2 {
		t.Fatalf("retry sweep = %d, %v", n, err)
	}
	be.mu.Lock()
	calls := append([]labelCall(nil), be.calls...)
	be.mu.Unlock()
	// First attempt failed; the retry repeats it before applying the Undo.
	if len(calls) != 3 || len(calls[1].add) != 1 || calls[1].add[0] != model.LabelStarred || len(calls[2].remove) != 1 || calls[2].remove[0] != model.LabelStarred {
		t.Fatalf("provider calls out of order: %+v", calls)
	}
}
