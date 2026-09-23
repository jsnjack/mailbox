package syncer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

type checkpointBackend struct {
	fakeIncBackend
	mu      sync.Mutex
	calls   map[string]int
	changes int
	cancel  context.CancelFunc
	subject string
}

func (b *checkpointBackend) Changes(ctx context.Context, cursor string) ([]string, []string, string, error) {
	b.mu.Lock()
	b.changes++
	b.mu.Unlock()
	return b.fakeIncBackend.Changes(ctx, cursor)
}

func (b *checkpointBackend) FetchMetadata(ctx context.Context, id string) (model.Message, error) {
	b.mu.Lock()
	b.calls[id]++
	cancel := b.cancel
	b.mu.Unlock()
	if id == "m20" && cancel != nil {
		cancel()
	}
	m, err := b.fakeIncBackend.FetchMetadata(ctx, id)
	m.Subject = b.subject
	return m, err
}

func TestIncrementalCheckpointRestart(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(fmt.Sprintf("vanished=%v", gone), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "test.db")
			s, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			}()
			account, err := s.UpsertAccount(ctx, model.Account{Email: "a@example.com", Type: model.AccountGmail})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetSyncCursor(ctx, account, "old"); err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 45)
			for i := range ids {
				ids[i] = fmt.Sprintf("m%d", i)
			}
			interrupted, cancel := context.WithCancel(ctx)
			defer cancel()
			b := &checkpointBackend{fakeIncBackend: fakeIncBackend{upserts: ids, next: "next", fetchErr: map[string]error{}}, calls: map[string]int{}, cancel: cancel}
			if gone {
				b.fetchErr["m0"] = backend.ErrNotFound
			}
			e := &Engine{Store: s}
			n, err := e.Incremental(interrupted, b, account)
			want := 20
			if gone {
				want--
			}
			if !errors.Is(err, context.Canceled) || n != want {
				t.Fatalf("interrupted count=%d err=%v", n, err)
			}
			batch, err := s.LoadSyncBatch(ctx, account)
			if err != nil {
				t.Fatal(err)
			}
			if batch == nil || len(batch.Upserts) != 25 {
				t.Fatalf("pending=%+v", batch)
			}
			if _, err := s.GetMessage(ctx, account, "m19"); err != nil {
				t.Fatalf("completed batch not stored: %v", err)
			}
			acc, err := s.GetAccountByID(ctx, account)
			if err != nil {
				t.Fatal(err)
			}
			if acc.SyncCursor != "old" {
				t.Fatalf("advanced cursor=%s", acc.SyncCursor)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			b.cancel = nil
			b.calls = map[string]int{}
			// New provider changes must wait for the saved range to finish.
			b.upserts = []string{"m19"}
			b.next = "newer"
			b.subject = "edited later"
			e = &Engine{Store: s}
			if n, err := e.Incremental(ctx, b, account); err != nil || n != 25 {
				t.Fatalf("resume=%d,%v", n, err)
			}
			if b.changes != 1 || len(b.calls) != 25 {
				t.Fatalf("relisted history or repeated completed fetches: changes=%d calls=%v", b.changes, b.calls)
			}
			for i := 0; i < 20; i++ {
				if b.calls[fmt.Sprintf("m%d", i)] != 0 {
					t.Fatal("fetched completed message again")
				}
			}
			batch, err = s.LoadSyncBatch(ctx, account)
			if err != nil || batch != nil {
				t.Fatalf("completed batch=%+v,%v", batch, err)
			}
			acc, err = s.GetAccountByID(ctx, account)
			if err != nil || acc.SyncCursor != "next" {
				t.Fatalf("resume cursor=%+v,%v", acc, err)
			}
			if _, err := e.Incremental(ctx, b, account); err != nil {
				t.Fatal(err)
			}
			m, err := s.GetMessage(ctx, account, "m19")
			if err != nil || m.Subject != "edited later" {
				t.Fatalf("later edit lost: %+v,%v", m, err)
			}
		})
	}
}

func TestIncrementalCheckpointRetriesOnlyFailures(t *testing.T) {
	t.Run("successful ids are not fetched on retry", func(t *testing.T) {
		ctx := context.Background()
		s, account := newIncStore(t, "old")
		b := &checkpointBackend{fakeIncBackend: fakeIncBackend{upserts: []string{"ok", "bad"}, next: "next", fetchErr: map[string]error{"bad": errors.New("offline")}}, calls: map[string]int{}}
		e := &Engine{Store: s}
		if _, err := e.Incremental(ctx, b, account); err != nil {
			t.Fatal(err)
		}
		delete(b.fetchErr, "bad")
		if _, err := e.Incremental(ctx, b, account); err != nil {
			t.Fatal(err)
		}
		if b.calls["ok"] != 1 || b.calls["bad"] != 2 || b.changes != 1 {
			t.Fatalf("unexpected requests: %v changes=%d", b.calls, b.changes)
		}
	})
}
