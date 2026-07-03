package store

import (
	"context"
	"testing"
)

const nowT = int64(1_000_000) // fixed "now" for outbox tests (unix seconds)

func TestOutboxLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	if _, err := s.EnqueueOutbox(ctx, acc, "thread-1", "", []byte("raw message bytes"), 0); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	items, err := s.ListSendableOutbox(ctx, acc, 5, nowT)
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
	items, _ = s.ListSendableOutbox(ctx, acc, 5, nowT)
	if len(items) != 1 || items[0].Attempts != 1 || items[0].LastError != "network down" {
		t.Fatalf("after failure: %+v", items)
	}

	// Beyond the attempt cap it is no longer sendable.
	if items, _ := s.ListSendableOutbox(ctx, acc, 1, nowT); len(items) != 0 {
		t.Fatalf("expected none sendable at cap, got %d", len(items))
	}

	// Marking sent removes it.
	if err := s.MarkOutboxSent(ctx, it.ID); err != nil {
		t.Fatalf("MarkOutboxSent: %v", err)
	}
	if items, _ := s.ListSendableOutbox(ctx, acc, 5, nowT); len(items) != 0 {
		t.Fatalf("expected empty outbox after send, got %d", len(items))
	}
}

// A message enqueued with a future not_before (a send held for its undo window)
// is durable but invisible to the sweeper and the pending banner until the window
// elapses — so a quit during the window can't lose it, yet it doesn't send early.
func TestOutboxUndoWindow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	id, err := s.EnqueueOutbox(ctx, acc, "t1", "draft-9", []byte("held"), nowT+5)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// During the window: not sendable, not counted as pending.
	if items, _ := s.ListSendableOutbox(ctx, acc, 5, nowT); len(items) != 0 {
		t.Fatalf("in-window sendable = %d, want 0", len(items))
	}
	if n, _ := s.CountPendingOutbox(ctx, acc, nowT); n != 0 {
		t.Fatalf("in-window pending = %d, want 0", n)
	}

	// After the window: sendable, and carries its draft id for post-send cleanup.
	got, _ := s.ListSendableOutbox(ctx, acc, 5, nowT+5)
	if len(got) != 1 {
		t.Fatalf("post-window sendable = %d, want 1", len(got))
	}
	if got[0].DraftID != "draft-9" {
		t.Fatalf("DraftID = %q, want draft-9", got[0].DraftID)
	}

	// Undo removes it before it can be swept, and reports the cancel won.
	if ok, err := s.DeleteOutbox(ctx, id); err != nil || !ok {
		t.Fatalf("delete = %v, %v; want cancelled", ok, err)
	}
	if got, _ := s.ListSendableOutbox(ctx, acc, 5, nowT+5); len(got) != 0 {
		t.Fatalf("after undo sendable = %d, want 0", len(got))
	}
}

// Undo-vs-sweep arbitration: a claimed row refuses the discard (the send is in
// flight — reporting "cancelled" would be a lie), a discarded row refuses the
// claim (the send must not happen), and an interrupted claim is retryable.
func TestOutboxClaimVsDiscard(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	// Claim wins: discard after a claim reports not-cancelled.
	id, _ := s.EnqueueOutbox(ctx, acc, "t1", "", []byte("a"), 0)
	if ok, err := s.ClaimOutbox(ctx, id); err != nil || !ok {
		t.Fatalf("claim = %v, %v; want claimed", ok, err)
	}
	if ok, err := s.DeleteOutbox(ctx, id); err != nil || ok {
		t.Fatalf("discard of claimed row = %v, %v; want not-cancelled", ok, err)
	}

	// Discard wins: claim after a discard reports not-claimed.
	id2, _ := s.EnqueueOutbox(ctx, acc, "t2", "", []byte("b"), 0)
	if ok, err := s.DeleteOutbox(ctx, id2); err != nil || !ok {
		t.Fatalf("discard = %v, %v; want cancelled", ok, err)
	}
	if ok, err := s.ClaimOutbox(ctx, id2); err != nil || ok {
		t.Fatalf("claim of discarded row = %v, %v; want not-claimed", ok, err)
	}

	// A leftover 'sending' row (crash mid-send) becomes a retryable failure
	// with the attempt counted.
	if err := s.FailInterruptedSends(ctx, acc); err != nil {
		t.Fatalf("fail interrupted: %v", err)
	}
	sendable, _ := s.ListSendableOutbox(ctx, acc, 5, nowT)
	if len(sendable) != 1 || sendable[0].ID != id {
		t.Fatalf("after recovery sendable = %+v, want the interrupted row", sendable)
	}
	if sendable[0].Attempts != 1 || sendable[0].State != "failed" {
		t.Fatalf("recovered row = state %q attempts %d, want failed/1", sendable[0].State, sendable[0].Attempts)
	}
}

func TestOutboxPendingRequeueAndDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	if _, err := s.EnqueueOutbox(ctx, acc, "t1", "", []byte("a"), 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s.EnqueueOutbox(ctx, acc, "t2", "", []byte("b"), 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Drive one item past the retry cap; it stays pending and visible.
	stuck, _ := s.ListPendingOutbox(ctx, acc, nowT)
	for i := 0; i < maxStuck; i++ {
		if err := s.MarkOutboxFailed(ctx, stuck[0].ID, "boom"); err != nil {
			t.Fatalf("mark failed: %v", err)
		}
	}

	if n, err := s.CountPendingOutbox(ctx, acc, nowT); err != nil || n != 2 {
		t.Fatalf("CountPendingOutbox = %d (err %v), want 2", n, err)
	}
	pending, err := s.ListPendingOutbox(ctx, acc, nowT)
	if err != nil || len(pending) != 2 {
		t.Fatalf("ListPendingOutbox = %d (err %v), want 2", len(pending), err)
	}
	// The stuck item is excluded from the sendable set but still listed pending.
	if sendable, _ := s.ListSendableOutbox(ctx, acc, maxStuck, nowT); len(sendable) != 1 {
		t.Fatalf("sendable = %d, want 1 (stuck one excluded)", len(sendable))
	}

	// Requeue clears the failure state so it becomes sendable again.
	if err := s.RequeueOutbox(ctx, stuck[0].ID); err != nil {
		t.Fatalf("RequeueOutbox: %v", err)
	}
	if sendable, _ := s.ListSendableOutbox(ctx, acc, maxStuck, nowT); len(sendable) != 2 {
		t.Fatalf("after requeue, sendable = %d, want 2", len(sendable))
	}

	// Delete removes an item entirely.
	if ok, err := s.DeleteOutbox(ctx, stuck[0].ID); err != nil || !ok {
		t.Fatalf("DeleteOutbox = %v, %v; want cancelled", ok, err)
	}
	if n, _ := s.CountPendingOutbox(ctx, acc, nowT); n != 1 {
		t.Fatalf("after delete, count = %d, want 1", n)
	}
}

const maxStuck = 5
