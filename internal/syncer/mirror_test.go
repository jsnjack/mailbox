package syncer

import (
	"sync"
	"testing"
	"time"
)

// The per-account mirror queue must apply operations in submission order even
// when a later op's work is faster than an earlier one's — otherwise an action's
// provider mirror could be overtaken by its Undo's, leaving the provider (and,
// after the next sync, the cache) in the wrong final state.
func TestMirrorAsyncPreservesOrder(t *testing.T) {
	e := &Engine{}
	var mu sync.Mutex
	var order []int
	done := make(chan struct{}, 2)

	// First op is slow; second is fast. FIFO ordering must still yield [1, 2].
	e.mirrorAsync(1, func() {
		time.Sleep(60 * time.Millisecond)
		mu.Lock()
		order = append(order, 1)
		mu.Unlock()
		done <- struct{}{}
	})
	e.mirrorAsync(1, func() {
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
		done <- struct{}{}
	})

	<-done
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("mirror applied out of order: %v (want [1 2])", order)
	}
}

// Different accounts must not block each other (independent FIFO queues).
func TestMirrorAsyncPerAccountConcurrency(t *testing.T) {
	e := &Engine{}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	// Both wait on the same barrier; if they shared one queue, the second could
	// not start until the first returned, and this would deadlock/time out.
	e.mirrorAsync(1, func() { <-start; wg.Done() })
	e.mirrorAsync(2, func() { <-start; wg.Done() })
	close(start)
	waitCh := make(chan struct{})
	go func() { wg.Wait(); close(waitCh) }()
	select {
	case <-waitCh:
	case <-time.After(2 * time.Second):
		t.Fatal("per-account mirror queues did not run concurrently")
	}
}

// StopAccount must close the account's queue (already-queued mirrors still run,
// then the drain goroutine exits) and a later mirrorAsync for the same account
// must get a fresh queue — no panic, no closure leaking the old backend.
func TestStopAccountClosesAndRecreatesMirrorQueue(t *testing.T) {
	e := &Engine{}
	ran := make(chan int, 2)
	e.mirrorAsync(1, func() { ran <- 1 })
	drained := e.StopAccount(1)
	// The queued op still runs to completion before the drainer exits.
	select {
	case got := <-ran:
		if got != 1 {
			t.Fatalf("queued op = %d, want 1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued mirror op was dropped by StopAccount")
	}
	// The returned channel closes once the drain goroutine exits, so callers can
	// hold the backend open until queued mirrors have finished.
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("StopAccount's drained channel never closed")
	}
	// A new op after StopAccount starts a fresh queue and runs.
	e.mirrorAsync(1, func() { ran <- 2 })
	select {
	case got := <-ran:
		if got != 2 {
			t.Fatalf("post-stop op = %d, want 2", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mirror op after StopAccount did not run")
	}
	// Stopping an account with no queue is a no-op with an already-closed signal.
	select {
	case <-e.StopAccount(42):
	default:
		t.Fatal("StopAccount for an unknown account must return a closed channel")
	}
}
