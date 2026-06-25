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

func TestOutboxPendingRequeueAndDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	if err := s.EnqueueOutbox(ctx, acc, "t1", []byte("a")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := s.EnqueueOutbox(ctx, acc, "t2", []byte("b")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Drive one item past the retry cap; it stays pending and visible.
	stuck, _ := s.ListPendingOutbox(ctx, acc)
	for i := 0; i < maxStuck; i++ {
		if err := s.MarkOutboxFailed(ctx, stuck[0].ID, "boom"); err != nil {
			t.Fatalf("mark failed: %v", err)
		}
	}

	if n, err := s.CountPendingOutbox(ctx, acc); err != nil || n != 2 {
		t.Fatalf("CountPendingOutbox = %d (err %v), want 2", n, err)
	}
	pending, err := s.ListPendingOutbox(ctx, acc)
	if err != nil || len(pending) != 2 {
		t.Fatalf("ListPendingOutbox = %d (err %v), want 2", len(pending), err)
	}
	// The stuck item is excluded from the sendable set but still listed pending.
	if sendable, _ := s.ListSendableOutbox(ctx, acc, maxStuck); len(sendable) != 1 {
		t.Fatalf("sendable = %d, want 1 (stuck one excluded)", len(sendable))
	}

	// Requeue clears the failure state so it becomes sendable again.
	if err := s.RequeueOutbox(ctx, stuck[0].ID); err != nil {
		t.Fatalf("RequeueOutbox: %v", err)
	}
	if sendable, _ := s.ListSendableOutbox(ctx, acc, maxStuck); len(sendable) != 2 {
		t.Fatalf("after requeue, sendable = %d, want 2", len(sendable))
	}

	// Delete removes an item entirely.
	if err := s.DeleteOutbox(ctx, stuck[0].ID); err != nil {
		t.Fatalf("DeleteOutbox: %v", err)
	}
	if n, _ := s.CountPendingOutbox(ctx, acc); n != 1 {
		t.Fatalf("after delete, count = %d, want 1", n)
	}
}

const maxStuck = 5
