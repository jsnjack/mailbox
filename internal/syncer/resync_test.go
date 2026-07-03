package syncer

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

// seedingBackend is a fake with a capped SearchIDs and a CursorSeeder
// implementation that records exactly which ids it was asked to cover.
type seedingBackend struct {
	countingBackend
	ids       []string
	seededIDs []string
}

func (f *seedingBackend) Profile(context.Context) (backend.Profile, error) {
	return backend.Profile{Email: "a@example.com", Cursor: "profile-cursor-all-uids"}, nil
}

func (f *seedingBackend) SearchIDs(_ context.Context, _ string, max int) ([]string, error) {
	if max > 0 && max < len(f.ids) {
		return f.ids[:max], nil
	}
	return f.ids, nil
}

func (f *seedingBackend) FetchMetadata(_ context.Context, id string) (model.Message, error) {
	return model.Message{AccountID: 1, GmailID: id, ThreadID: "t-" + id}, nil
}

func (f *seedingBackend) SeedCursor(_ context.Context, backfilledIDs []string) (string, error) {
	f.seededIDs = append([]string(nil), backfilledIDs...)
	return "seeded:" + strings.Join(backfilledIDs, ","), nil
}

func newResyncStore(t *testing.T) (*store.Store, int64) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	acct, err := s.UpsertAccount(context.Background(), model.Account{Email: "a@example.com", Type: model.AccountIMAP})
	if err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	return s, acct
}

// A capped Resync against a CursorSeeder backend must persist a cursor built
// from ONLY the backfilled ids — seeding the full pre-backfill snapshot would
// mark the skipped messages as already-seen and hide them forever.
func TestResyncSeedsCursorFromBackfilledIDsOnly(t *testing.T) {
	ctx := context.Background()
	s, acct := newResyncStore(t)
	var ids []string
	for i := 0; i < 10; i++ {
		ids = append(ids, fmt.Sprintf("m%d", i))
	}
	be := &seedingBackend{ids: ids}
	e := &Engine{Store: s}

	n, err := e.Resync(ctx, be, acct, 4) // cap below the mailbox size
	if err != nil {
		t.Fatalf("Resync: %v", err)
	}
	if n != 4 {
		t.Fatalf("Resync stored %d, want 4 (capped)", n)
	}
	if len(be.seededIDs) != 4 {
		t.Fatalf("SeedCursor got %d ids, want the 4 backfilled: %v", len(be.seededIDs), be.seededIDs)
	}
	acc, err := s.GetAccountByID(ctx, acct)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if !strings.HasPrefix(acc.SyncCursor, "seeded:") {
		t.Fatalf("cursor = %q, want the seeded cursor, not the profile snapshot", acc.SyncCursor)
	}
	for _, id := range be.seededIDs {
		if !strings.Contains(acc.SyncCursor, id) {
			t.Errorf("seeded cursor %q missing backfilled id %s", acc.SyncCursor, id)
		}
	}
}

// TestResyncKeepsProfileCursorWithoutSeeder pins the Gmail path: a backend that
// doesn't implement backend.CursorSeeder stores the Profile cursor unchanged
// (Gmail's history log replays changes regardless of what was backfilled).
func TestResyncKeepsProfileCursorWithoutSeeder(t *testing.T) {
	ctx := context.Background()
	s, acct := newResyncStore(t)
	// The interface-embedding wrapper hides SeedCursor from the method set.
	be := struct{ backend.Backend }{&seedingBackend{ids: []string{"m1", "m2"}}}

	if _, err := (&Engine{Store: s}).Resync(ctx, be, acct, 0); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	acc, err := s.GetAccountByID(ctx, acct)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if acc.SyncCursor != "profile-cursor-all-uids" {
		t.Fatalf("cursor = %q, want the profile watermark", acc.SyncCursor)
	}
}
