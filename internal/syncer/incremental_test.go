package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

// fakeIncBackend drives Incremental: Changes returns a fixed upsert set, and
// FetchMetadata returns a configured per-id error (or a valid message).
type fakeIncBackend struct {
	countingBackend
	upserts  []string
	next     string
	fetchErr map[string]error // id -> error to return from FetchMetadata
}

func (f *fakeIncBackend) Changes(context.Context, string) ([]string, []string, string, error) {
	return f.upserts, nil, f.next, nil
}

func (f *fakeIncBackend) FetchMetadata(_ context.Context, id string) (model.Message, error) {
	if err := f.fetchErr[id]; err != nil {
		return model.Message{}, err
	}
	return model.Message{AccountID: 1, GmailID: id, ThreadID: "t-" + id}, nil
}

func newIncStore(t *testing.T, cursor string) (*store.Store, int64) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	acct, err := s.UpsertAccount(ctx, model.Account{Email: "a@example.com", Type: model.AccountGmail})
	if err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	if err := s.SetSyncCursor(ctx, acct, cursor); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	return s, acct
}

// A transient fetch failure must NOT advance the cursor — otherwise the message
// is skipped forever (its history record falls behind the new cursor).
func TestIncrementalHoldsCursorOnTransientFailure(t *testing.T) {
	ctx := context.Background()
	s, acct := newIncStore(t, "cursor-old")
	be := &fakeIncBackend{
		upserts:  []string{"m1", "m2"},
		next:     "cursor-new",
		fetchErr: map[string]error{"m2": errors.New("connection reset")}, // transient
	}
	e := &Engine{Store: s}
	if _, err := e.Incremental(ctx, be, acct); err != nil {
		t.Fatalf("incremental: %v", err)
	}
	acc, err := s.GetAccountByID(ctx, acct)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if acc.SyncCursor != "cursor-old" {
		t.Fatalf("cursor advanced to %q on transient failure; want held at cursor-old", acc.SyncCursor)
	}
}

// A genuinely vanished message (ErrNotFound) is not transient, so the cursor
// advances normally — the message is legitimately gone.
func TestIncrementalAdvancesCursorOnNotFound(t *testing.T) {
	ctx := context.Background()
	s, acct := newIncStore(t, "cursor-old")
	be := &fakeIncBackend{
		upserts:  []string{"m1", "m2"},
		next:     "cursor-new",
		fetchErr: map[string]error{"m2": backend.ErrNotFound}, // gone, safe to skip
	}
	e := &Engine{Store: s}
	if _, err := e.Incremental(ctx, be, acct); err != nil {
		t.Fatalf("incremental: %v", err)
	}
	acc, err := s.GetAccountByID(ctx, acct)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if acc.SyncCursor != "cursor-new" {
		t.Fatalf("cursor at %q after ErrNotFound; want advanced to cursor-new", acc.SyncCursor)
	}
}
