package gmailapi

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/logging"
)

// gmailPerUserUnitsPerMin is Gmail's per-user quota ceiling. The budget refills
// continuously toward this rate so the sync engine throttles itself proactively
// rather than relying on 429 retries.
const gmailPerUserUnitsPerMin = 6000

// Quota unit costs for the API methods we call (from Gmail's published costs).
const (
	costHistoryList   = 2
	costMessageList   = 5
	costMessageGet    = 20 // metadata or full
	costThreadGet     = 40
	costProfileGet    = 1
	costDraftCreate   = 10
	costDraftUpdate   = 15
	costDraftDelete   = 10
	costAttachmentGet = 20
	costBatchMutate   = 50
	costMessageModify = 5
	costLabelsList    = 1
	costLabelsGet     = 1
	costLabelsMutate  = 5 // labels.create / labels.delete
	costSend          = 100
)

// RateBudget is a token bucket over Gmail quota units. Requests that have to
// wait are served most urgent first (backend.PriorityOf their context), in
// arrival order within a priority: without that, a long catch-up's twenty
// background fetchers would race a reader opening a message for every token,
// and the reader would usually lose.
type RateBudget struct {
	mu       sync.Mutex
	capacity float64
	perSec   float64
	tokens   float64
	last     time.Time
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error

	// queue holds the waiting requests, most urgent first; only its head may
	// take tokens. turn is closed (and replaced) whenever the queue changes, to
	// wake the waiters behind the head so they can see whether they lead now.
	queue []*budgetWaiter
	seq   uint64
	turn  chan struct{}
}

type budgetWaiter struct {
	prio backend.Priority
	seq  uint64
}

// NewRateBudget leaves headroom below Gmail's per-user quota and limits bursts
// to one send, rather than allowing a full minute's quota immediately at launch.
func NewRateBudget() *RateBudget {
	b := newRateBudget(gmailPerUserUnitsPerMin*4/5, time.Now, sleepCtx)
	b.capacity = costSend
	b.tokens = costSend
	return b
}

func newRateBudget(unitsPerMin int, now func() time.Time, sleep func(context.Context, time.Duration) error) *RateBudget {
	capacity := float64(unitsPerMin)
	return &RateBudget{
		capacity: capacity,
		perSec:   capacity / 60.0,
		tokens:   capacity,
		last:     now(),
		now:      now,
		sleep:    sleep,
		turn:     make(chan struct{}),
	}
}

// Reserve blocks until cost units are available and no more urgent request is
// waiting, then consumes them. It returns the context error if cancelled while
// waiting.
func (b *RateBudget) Reserve(ctx context.Context, cost int) error {
	if float64(cost) > b.capacity {
		logging.TraceContext(ctx, "gmailapi: budget cost exceeds capacity", "quota", cost, "capacity", b.capacity)
		return fmt.Errorf("reserve %d units exceeds budget capacity %.0f", cost, b.capacity)
	}
	prio := backend.PriorityOf(ctx)
	b.mu.Lock()
	w := b.enqueueLocked(prio)
	waited := false
	for {
		b.refillLocked()
		if b.queue[0] != w {
			// A more urgent (or earlier) request leads; wait for the queue to move.
			turn := b.turn
			ahead := b.positionLocked(w)
			b.mu.Unlock()
			if !waited {
				logging.TraceContext(ctx, "gmailapi: budget queued behind other requests", "quota", cost, "priority", prio, "ahead", ahead)
			}
			waited = true
			select {
			case <-turn:
			case <-ctx.Done():
				b.mu.Lock()
				b.dequeueLocked(w)
				b.mu.Unlock()
				logging.TraceContext(ctx, "gmailapi: budget wait cancelled", "quota", cost, "priority", prio, "err", ctx.Err())
				return ctx.Err()
			}
			b.mu.Lock()
			continue
		}
		if b.tokens >= float64(cost) {
			b.tokens -= float64(cost)
			remaining := b.tokens
			b.dequeueLocked(w)
			b.mu.Unlock()
			if waited {
				logging.TraceContext(ctx, "gmailapi: budget granted after wait", "quota", cost, "priority", prio, "remaining", remaining)
			}
			return nil
		}
		wait := time.Duration((float64(cost) - b.tokens) / b.perSec * float64(time.Second))
		tokens := b.tokens
		b.mu.Unlock()
		logging.TraceContext(ctx, "gmailapi: budget exhausted, waiting", "quota", cost, "priority", prio, "available", tokens, "wait", wait)
		waited = true
		// The head sleeps for its own refill. A more urgent request arriving
		// meanwhile takes the lead; this one notices when it wakes and yields.
		err := b.sleep(ctx, wait)
		b.mu.Lock()
		if err != nil {
			b.dequeueLocked(w)
			b.mu.Unlock()
			logging.TraceContext(ctx, "gmailapi: budget wait cancelled", "quota", cost, "priority", prio, "err", err)
			return err
		}
	}
}

// enqueueLocked adds a waiter behind every request at least as urgent.
func (b *RateBudget) enqueueLocked(prio backend.Priority) *budgetWaiter {
	b.seq++
	w := &budgetWaiter{prio: prio, seq: b.seq}
	i := len(b.queue)
	for i > 0 && b.queue[i-1].prio > prio {
		i--
	}
	b.queue = append(b.queue, nil)
	copy(b.queue[i+1:], b.queue[i:])
	b.queue[i] = w
	b.signalLocked()
	return w
}

func (b *RateBudget) dequeueLocked(w *budgetWaiter) {
	if i := b.positionLocked(w); i >= 0 {
		b.queue = append(b.queue[:i], b.queue[i+1:]...)
		b.signalLocked()
	}
}

func (b *RateBudget) positionLocked(w *budgetWaiter) int {
	for i, q := range b.queue {
		if q == w {
			return i
		}
	}
	return -1
}

func (b *RateBudget) signalLocked() {
	close(b.turn)
	b.turn = make(chan struct{})
}

func (b *RateBudget) refillLocked() {
	now := b.now()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = minFloat(b.capacity, b.tokens+elapsed*b.perSec)
		b.last = now
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
