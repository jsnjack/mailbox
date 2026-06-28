package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestUpsertAccount(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name    string
		account model.Account
	}{
		{
			name: "minimal",
			account: model.Account{
				Email: "a@example.com",
			},
		},
		{
			name: "full",
			account: model.Account{
				Email:        "b@example.com",
				DisplayName:  "Bee",
				Type:         model.AccountIMAP,
				TokenExpiry:  time.Unix(1_700_000_000, 0),
				Scopes:       []string{"https://mail.google.com/", "openid"},
				SyncCursor:   "12345",
				BackfilledAt: time.Unix(1_700_000_100, 0),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, err := s.UpsertAccount(ctx, tc.account)
			if err != nil {
				t.Fatalf("UpsertAccount: %v", err)
			}
			if id == 0 {
				t.Fatal("expected non-zero id")
			}
			got, err := s.GetAccountByEmail(ctx, tc.account.Email)
			if err != nil {
				t.Fatalf("GetAccountByEmail: %v", err)
			}
			if got.Email != tc.account.Email || got.DisplayName != tc.account.DisplayName {
				t.Fatalf("got %+v, want email/name to match %+v", got, tc.account)
			}
			if got.SyncCursor != tc.account.SyncCursor {
				t.Fatalf("sync cursor: got %q, want %q", got.SyncCursor, tc.account.SyncCursor)
			}
			// An unset type defaults to Gmail; an explicit type round-trips.
			wantType := tc.account.Type
			if wantType == "" {
				wantType = model.AccountGmail
			}
			if got.Type != wantType {
				t.Fatalf("account type: got %q, want %q", got.Type, wantType)
			}
			if len(got.Scopes) != len(tc.account.Scopes) {
				t.Fatalf("scopes: got %v, want %v", got.Scopes, tc.account.Scopes)
			}
		})
	}
}

func TestUpsertAccountUpdatesExisting(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id1, err := s.UpsertAccount(ctx, model.Account{Email: "x@example.com", DisplayName: "Old"})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	id2, err := s.UpsertAccount(ctx, model.Account{Email: "x@example.com", DisplayName: "New"})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("id changed on update: %d != %d", id1, id2)
	}
	got, err := s.GetAccountByEmail(ctx, "x@example.com")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DisplayName != "New" {
		t.Fatalf("display name not updated: %q", got.DisplayName)
	}
}

func TestGetAccountNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetAccountByEmail(context.Background(), "missing@example.com")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestSetWatermarks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, err := s.UpsertAccount(ctx, model.Account{Email: "w@example.com"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.SetSyncCursor(ctx, id, "99999"); err != nil {
		t.Fatalf("SetSyncCursor: %v", err)
	}
	now := time.Unix(1_700_001_000, 0)
	if err := s.SetBackfilledAt(ctx, id, now); err != nil {
		t.Fatalf("SetBackfilledAt: %v", err)
	}
	got, err := s.GetAccountByEmail(ctx, "w@example.com")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SyncCursor != "99999" {
		t.Fatalf("sync cursor: got %q", got.SyncCursor)
	}
	if !got.BackfilledAt.Equal(now) {
		t.Fatalf("backfilled_at: got %v, want %v", got.BackfilledAt, now)
	}
}
