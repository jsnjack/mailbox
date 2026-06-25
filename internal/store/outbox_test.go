package store

import (
	"context"
	"testing"
)

func TestOutboxLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	if err := s.EnqueueOutbox(ctx, acc, "thread-1", []byte("raw message bytes")); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	items, err := s.ListSendableOutbox(ctx, acc, 5)
	if err != nil {
		t.Fatalf("ListSendableOutbox: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d sendable, want 1", len(items))
	}
	it := items[0]
	if it.ThreadID != "thread-1" || string(it.RFC822) != "raw message bytes" || it.LocalUUID == "" {
		t.Fatalf("unexpected item: %+v", it)
	}

	// A failed attempt increments attempts but keeps it sendable until the cap.
	if err := s.MarkOutboxFailed(ctx, it.ID, "network down"); err != nil {
		t.Fatalf("MarkOutboxFailed: %v", err)
	}
	items, _ = s.ListSendableOutbox(ctx, acc, 5)
	if len(items) != 1 || items[0].Attempts != 1 || items[0].LastError != "network down" {
		t.Fatalf("after failure: %+v", items)
	}

	// Beyond the attempt cap it is no longer sendable.
	if items, _ := s.ListSendableOutbox(ctx, acc, 1); len(items) != 0 {
		t.Fatalf("expected none sendable at cap, got %d", len(items))
	}

	// Marking sent removes it.
	if err := s.MarkOutboxSent(ctx, it.ID); err != nil {
		t.Fatalf("MarkOutboxSent: %v", err)
	}
	if items, _ := s.ListSendableOutbox(ctx, acc, 5); len(items) != 0 {
		t.Fatalf("expected empty outbox after send, got %d", len(items))
	}
}
