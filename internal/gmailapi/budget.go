package gmailapi

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
)

// gmailPerUserUnitsPerMin is Gmail's per-user quota ceiling. The budget refills
// continuously toward this rate so the sync engine throttles itself proactively
// rather than relying on 429 retries.
const gmailPerUserUnitsPerMin = 6000

// Quota unit costs for the API methods we call (from Gmail's published costs).
const (
	costHistoryList = 2
	costMessageList = 5
	costMessageGet  = 5 // metadata or full
	costLabelsList  = 1
	costSend        = 100
)

// RateBudget is a token bucket over Gmail quota units.
type RateBudget struct {
	mu       sync.Mutex
	capacity float64
	perSec   float64
	tokens   float64
	last     time.Time
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

// NewRateBudget returns a budget sized to Gmail's per-user quota.
func NewRateBudget() *RateBudget {
	return newRateBudget(gmailPerUserUnitsPerMin, time.Now, sleepCtx)
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
	}
}

// Reserve blocks until cost units are available, then consumes them. It returns
// the context error if cancelled while waiting.
func (b *RateBudget) Reserve(ctx context.Context, cost int) error {
	if float64(cost) > b.capacity {
		logging.TraceContext(ctx, "gmailapi: budget cost exceeds capacity", "quota", cost, "capacity", b.capacity)
		return fmt.Errorf("reserve %d units exceeds budget capacity %.0f", cost, b.capacity)
	}
	waited := false
	for {
		b.mu.Lock()
		b.refillLocked()
		if b.tokens >= float64(cost) {
			b.tokens -= float64(cost)
			remaining := b.tokens
			b.mu.Unlock()
			if waited {
				logging.TraceContext(ctx, "gmailapi: budget granted after wait", "quota", cost, "remaining", remaining)
			}
			return nil
		}
		wait := time.Duration((float64(cost) - b.tokens) / b.perSec * float64(time.Second))
		tokens := b.tokens
		b.mu.Unlock()
		logging.TraceContext(ctx, "gmailapi: budget exhausted, waiting", "quota", cost, "available", tokens, "wait", wait)
		waited = true
		if err := b.sleep(ctx, wait); err != nil {
			logging.TraceContext(ctx, "gmailapi: budget wait cancelled", "quota", cost, "err", err)
			return err
		}
	}
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
