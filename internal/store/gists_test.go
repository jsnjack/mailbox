package store

import (
	"context"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestMessageGists(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	// Empty input returns an empty map without error.
	got, err := s.MessageGists(ctx, acc, nil)
	if err != nil {
		t.Fatalf("MessageGists(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty input: got %v, want empty", got)
	}

	if err := s.SetMessageGist(ctx, acc, "m1", "CI failed on the frame-ancestors test"); err != nil {
		t.Fatalf("SetMessageGist m1: %v", err)
	}
	got, err = s.MessageGists(ctx, acc, []string{"m1", "m2"})
	if err != nil {
		t.Fatalf("MessageGists: %v", err)
	}
	if got["m1"] != "CI failed on the frame-ancestors test" {
		t.Fatalf("m1 = %q", got["m1"])
	}
	if _, ok := got["m2"]; ok {
		t.Fatalf("m2 should be absent (never summarized), got %q", got["m2"])
	}

	// Upsert overwrites.
	if err := s.SetMessageGist(ctx, acc, "m1", "updated"); err != nil {
		t.Fatalf("SetMessageGist m1 update: %v", err)
	}
	got, _ = s.MessageGists(ctx, acc, []string{"m1"})
	if got["m1"] != "updated" {
		t.Fatalf("after update m1 = %q, want %q", got["m1"], "updated")
	}
}

// TestDeleteMessageClearsGist verifies that deleting a message also removes its
// persisted gist (keyed by gmail_id with its FK on accounts, so it doesn't
// cascade on its own).
func TestDeleteMessageClearsGist(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	if _, err := s.UpsertMessage(ctx, model.Message{
		AccountID: acc, GmailID: "g1", ThreadID: "t1", Subject: "hi", Labels: []string{"INBOX"},
	}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	if err := s.SetMessageGist(ctx, acc, "g1", "a gist"); err != nil {
		t.Fatalf("SetMessageGist: %v", err)
	}
	if err := s.DeleteMessages(ctx, acc, []string{"g1"}); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}
	got, err := s.MessageGists(ctx, acc, []string{"g1"})
	if err != nil {
		t.Fatalf("MessageGists: %v", err)
	}
	if _, ok := got["g1"]; ok {
		t.Fatalf("gist for deleted message should be gone, got %q", got["g1"])
	}
}
