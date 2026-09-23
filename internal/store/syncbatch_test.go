package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestSyncBatch(t *testing.T) {
	for _, tc := range []string{"atomic rollback", "cursor reset"} {
		t.Run(tc, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t)
			id, err := s.UpsertAccount(ctx, model.Account{Email: "a@example.com"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetSyncCursor(ctx, id, "old"); err != nil {
				t.Fatal(err)
			}
			initial := SyncBatch{Cursor: "old", Next: "next", Upserts: []string{"a", "b"}}
			if err := s.SaveSyncBatch(ctx, id, initial, nil); err != nil {
				t.Fatal(err)
			}
			if tc == "atomic rollback" {
				updated := initial
				updated.Upserts = nil
				err = s.SaveSyncBatch(ctx, id, updated, []model.Message{{AccountID: id, GmailID: "a", ThreadID: "t"}, {AccountID: id + 1, GmailID: "b", ThreadID: "t"}})
				if err == nil {
					t.Fatal("accepted message from another account")
				}
				if _, err := s.GetMessage(ctx, id, "a"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("partial commit: %v", err)
				}
				batch, err := s.LoadSyncBatch(ctx, id)
				if err != nil || batch == nil || len(batch.Upserts) != 2 {
					t.Fatalf("checkpoint changed after failure: %+v,%v", batch, err)
				}
			} else {
				if err := s.SetSyncCursor(ctx, id, "resynced"); err != nil {
					t.Fatal(err)
				}
				batch, err := s.LoadSyncBatch(ctx, id)
				if err != nil || batch != nil {
					t.Fatalf("stale checkpoint: %+v,%v", batch, err)
				}
				if err := s.SaveSyncBatch(ctx, id, initial, nil); err == nil {
					t.Fatal("accepted stale cursor")
				}
			}
		})
	}
}
