package store

import (
	"context"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestRecoverOutboxDraft(t *testing.T) {
	for _, tc := range []struct {
		name           string
		claim, invalid bool
	}{{name: "recover"}, {name: "already sending", claim: true}, {name: "save failure rolls back", invalid: true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			acct := seedAccount(t, s)
			id, err := s.EnqueueOutbox(ctx, acct, "thread", "", []byte("raw"), 0)
			if err != nil {
				t.Fatal(err)
			}
			items, err := s.ListPendingOutbox(ctx, acct, nowT)
			if err != nil {
				t.Fatal(err)
			}
			it := items[0]
			if tc.claim {
				if ok, err := s.ClaimOutbox(ctx, id); err != nil || !ok {
					t.Fatalf("claim: %v %v", ok, err)
				}
			}
			msg := model.OutgoingMessage{From: "sender@example.com", Body: "Keep my writing"}
			if tc.invalid {
				it.LocalDraftID = LocalDraftPrefix + "conflicting"
				other, err := s.UpsertAccount(ctx, model.Account{Email: "other@example.com"})
				if err != nil {
					t.Fatal(err)
				}
				_, err = s.SaveLocalDraft(ctx, other, model.OutgoingMessage{LocalDraftID: it.LocalDraftID})
				if err != nil {
					t.Fatal(err)
				}
			}
			localID, ok, err := s.RecoverOutboxDraft(ctx, it, msg)
			if tc.invalid {
				if err == nil || ok {
					t.Fatal("expected rollback")
				}
				remaining, err := s.ListPendingOutbox(ctx, acct, nowT)
				if err != nil || len(remaining) != 1 {
					t.Fatalf("send lost after failed recovery: %v %v", remaining, err)
				}
			} else if tc.claim {
				if err != nil || ok {
					t.Fatalf("claimed send recovered: %v %v", ok, err)
				}
			} else {
				if err != nil || !ok {
					t.Fatalf("recover: %v %v", ok, err)
				}
				d, err := s.LocalDraft(ctx, localID)
				if err != nil || d.Message.Body != msg.Body {
					t.Fatalf("draft: %#v %v", d, err)
				}
				pending, err := s.ListPendingOutbox(ctx, acct, nowT)
				if err != nil || len(pending) != 0 {
					t.Fatalf("send still queued: %v %v", pending, err)
				}
			}
		})
	}
}

func TestRestoreOutboxPreservesUncertainty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acct := seedAccount(t, s)
	_, err := s.EnqueueOutbox(ctx, acct, "thread", "", []byte("original"), 0)
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.ListPendingOutbox(ctx, acct, nowT)
	if err != nil {
		t.Fatal(err)
	}
	it := items[0]
	it.State = "uncertain"
	it.Attempts = 3
	it.LastError = "delivery unconfirmed"
	if ok, err := s.DeleteOutbox(ctx, it.ID); err != nil || !ok {
		t.Fatalf("discard: %v %v", ok, err)
	}
	// A newer send can reuse the deleted numeric id; Undo uses the stable UUID.
	if _, err := s.EnqueueOutbox(ctx, acct, "", "", []byte("new send"), 0); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.RestoreOutbox(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	items, err = s.ListPendingOutbox(ctx, acct, nowT)
	if err != nil || len(items) != 2 {
		t.Fatalf("restoration duplicated or lost send: %v %v", items, err)
	}
	for _, got := range items {
		if got.LocalUUID == it.LocalUUID && (got.State != "uncertain" || got.Attempts != 3 || got.LastError != it.LastError) {
			t.Fatalf("delivery state changed: %#v", got)
		}
	}
}
