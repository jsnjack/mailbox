package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestMessageCategories(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	// Empty input returns an empty map without error.
	got, err := s.MessageCategories(ctx, acc, nil)
	if err != nil {
		t.Fatalf("MessageCategories(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty input: got %v, want empty", got)
	}

	// Set a couple, including an empty "no tag" category (which must round-trip,
	// so it isn't re-classified).
	if err := s.SetMessageCategory(ctx, acc, "m1", "Needs reply"); err != nil {
		t.Fatalf("SetMessageCategory m1: %v", err)
	}
	if err := s.SetMessageCategory(ctx, acc, "m2", ""); err != nil {
		t.Fatalf("SetMessageCategory m2: %v", err)
	}

	got, err = s.MessageCategories(ctx, acc, []string{"m1", "m2", "m3"})
	if err != nil {
		t.Fatalf("MessageCategories: %v", err)
	}
	if got["m1"] != "Needs reply" {
		t.Fatalf("m1 = %q, want %q", got["m1"], "Needs reply")
	}
	if v, ok := got["m2"]; !ok || v != "" {
		t.Fatalf(`m2 = (%q, %v), want ("", true)`, v, ok)
	}
	if _, ok := got["m3"]; ok {
		t.Fatalf("m3 should be absent (never classified), got %q", got["m3"])
	}

	// Upsert overwrites the previous category.
	if err := s.SetMessageCategory(ctx, acc, "m1", "Receipt"); err != nil {
		t.Fatalf("SetMessageCategory m1 update: %v", err)
	}
	got, _ = s.MessageCategories(ctx, acc, []string{"m1"})
	if got["m1"] != "Receipt" {
		t.Fatalf("after update m1 = %q, want %q", got["m1"], "Receipt")
	}

	// ClearMessageCategory removes just one message's tag, leaving others.
	if err := s.SetMessageCategory(ctx, acc, "m2", "Newsletter"); err != nil {
		t.Fatalf("SetMessageCategory m2: %v", err)
	}
	if err := s.ClearMessageCategory(ctx, acc, "m1"); err != nil {
		t.Fatalf("ClearMessageCategory m1: %v", err)
	}
	got, _ = s.MessageCategories(ctx, acc, []string{"m1", "m2"})
	if _, ok := got["m1"]; ok {
		t.Fatalf("m1 should be cleared, got %q", got["m1"])
	}
	if got["m2"] != "Newsletter" {
		t.Fatalf("m2 should remain, got %q", got["m2"])
	}

	// ClearCategories wipes the account's cache so the inbox re-classifies.
	if err := s.ClearCategories(ctx, acc); err != nil {
		t.Fatalf("ClearCategories: %v", err)
	}
	got, _ = s.MessageCategories(ctx, acc, []string{"m1", "m2"})
	if len(got) != 0 {
		t.Fatalf("after clear, got %v, want empty", got)
	}
}

// TestDeleteMessageClearsCategory verifies that deleting a message also removes
// its persisted category, so no orphan category row is left behind (the row is
// keyed by gmail_id with its FK on accounts, so it doesn't cascade on its own).
func TestDeleteMessageClearsCategory(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	if _, err := s.UpsertMessage(ctx, model.Message{
		AccountID: acc, GmailID: "g1", ThreadID: "t1", Subject: "hi", Labels: []string{"INBOX"},
	}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	if err := s.SetMessageCategory(ctx, acc, "g1", "Receipt"); err != nil {
		t.Fatalf("SetMessageCategory: %v", err)
	}

	if err := s.DeleteMessages(ctx, acc, []string{"g1"}); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}
	got, err := s.MessageCategories(ctx, acc, []string{"g1"})
	if err != nil {
		t.Fatalf("MessageCategories: %v", err)
	}
	if _, ok := got["g1"]; ok {
		t.Fatalf("category for deleted message should be gone, got %q", got["g1"])
	}
}

// TestMessageCategoriesChunking verifies the IN-clause is chunked: querying more
// ids than the chunk size (500) returns them all, across the boundary.
func TestMessageCategoriesChunking(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	const n = 1100 // > 2 chunks
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = fmt.Sprintf("g%04d", i)
		if err := s.SetMessageCategory(ctx, acc, ids[i], "Newsletter"); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	got, err := s.MessageCategories(ctx, acc, ids)
	if err != nil {
		t.Fatalf("MessageCategories: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d categories, want %d (chunking lost rows)", len(got), n)
	}
}
