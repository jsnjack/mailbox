package gmailapi

import (
	"context"
	"testing"
	"time"
)

func TestRateBudgetReserve(t *testing.T) {
	ctx := context.Background()
	clock := time.Unix(0, 0)
	var sleeps []time.Duration
	now := func() time.Time { return clock }
	sleep := func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		clock = clock.Add(d) // simulate time passing so the bucket refills
		return nil
	}
	// 6000 units/min => 100 units/sec, starts full.
	b := newRateBudget(6000, now, sleep)

	if err := b.Reserve(ctx, 5000); err != nil {
		t.Fatalf("reserve 5000: %v", err)
	}
	if len(sleeps) != 0 {
		t.Fatalf("expected no sleep with tokens available, got %v", sleeps)
	}

	// Only 1000 left; reserving 2000 needs 1000 more units => 10s at 100/s.
	if err := b.Reserve(ctx, 2000); err != nil {
		t.Fatalf("reserve 2000: %v", err)
	}
	if len(sleeps) != 1 {
		t.Fatalf("expected one sleep, got %d: %v", len(sleeps), sleeps)
	}
	if sleeps[0] != 10*time.Second {
		t.Fatalf("wait = %v, want 10s", sleeps[0])
	}
}

func TestRateBudgetRejectsOversizedReserve(t *testing.T) {
	b := newRateBudget(6000, func() time.Time { return time.Unix(0, 0) }, sleepCtx)
	if err := b.Reserve(context.Background(), 7000); err == nil {
		t.Fatal("expected error reserving more than capacity")
	}
}

func TestRateBudgetReserveCancel(t *testing.T) {
	clock := time.Unix(0, 0)
	now := func() time.Time { return clock }
	// sleep that never advances the clock and reports cancellation.
	sleep := func(ctx context.Context, _ time.Duration) error { return context.Canceled }
	b := newRateBudget(60, now, sleep) // 1 unit/sec, capacity 60

	if err := b.Reserve(context.Background(), 30); err != nil {
		t.Fatalf("reserve 30: %v", err)
	}
	// Now only 30 left; reserving 50 must wait, and our sleep cancels.
	if err := b.Reserve(context.Background(), 50); err != context.Canceled {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
