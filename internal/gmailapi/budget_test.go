package gmailapi

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/backend"
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

func TestProductionBudgetPacesMessageReads(t *testing.T) {
	t.Run("first minute including startup burst stays below quota", func(t *testing.T) {
		clock := time.Unix(0, 0)
		b := NewRateBudget()
		b.last = clock
		b.now = func() time.Time { return clock }
		b.sleep = func(_ context.Context, d time.Duration) error {
			clock = clock.Add(d)
			return nil
		}
		for range 300 {
			if err := b.Reserve(context.Background(), costMessageGet); err != nil {
				t.Fatal(err)
			}
		}
		// Google charges 20 units per read: 300 reads consume the full minute.
		if elapsed := clock.Sub(time.Unix(0, 0)); elapsed < time.Minute {
			t.Fatalf("300 reads consumed a minute's quota in %v", elapsed)
		}
	})
}

// When the quota runs short, a request someone is waiting for goes ahead of
// background requests that queued before it, which keep their own order.
func TestRateBudgetServesUrgentWaitersFirst(t *testing.T) {
	var mu sync.Mutex
	clock := time.Unix(0, 0)
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	// Time only moves when the test says so; a sleeping head just re-checks.
	sleep := func(ctx context.Context, _ time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
			return nil
		}
	}
	b := newRateBudget(60, now, sleep) // 1 unit/s
	ctx := context.Background()
	if err := b.Reserve(ctx, 60); err != nil {
		t.Fatal(err)
	}
	queued := func() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.queue) }
	waitQueued := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for queued() != n {
			if time.Now().After(deadline) {
				t.Fatalf("queue length %d, want %d", queued(), n)
			}
			time.Sleep(time.Millisecond)
		}
	}
	granted := make(chan string, 3)
	reserve := func(name string, ctx context.Context) {
		go func() {
			if err := b.Reserve(ctx, 10); err != nil {
				t.Error(err)
			}
			granted <- name
		}()
	}
	bg := backend.WithPriority(ctx, backend.PriorityBackground)
	reserve("bg1", bg)
	waitQueued(1)
	reserve("bg2", bg)
	waitQueued(2)
	reserve("user", ctx)
	waitQueued(3)
	for _, want := range []string{"user", "bg1", "bg2"} {
		advance(10 * time.Second)
		select {
		case got := <-granted:
			if got != want {
				t.Fatalf("granted %s, want %s", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("nothing granted; want %s", want)
		}
	}
}

// A cancelled waiter leaves the queue, so the ones behind it are not stuck.
func TestRateBudgetCancelledWaiterLeavesQueue(t *testing.T) {
	b := newRateBudget(60, time.Now, sleepCtx)
	ctx := context.Background()
	if err := b.Reserve(ctx, 60); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- b.Reserve(cctx, 30) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled reserve succeeded")
	}
	b.mu.Lock()
	n := len(b.queue)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("queue length %d after cancel, want 0", n)
	}
}
