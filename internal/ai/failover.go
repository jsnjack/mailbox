package ai

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
)

// failoverCooldown is how long a chain entry is skipped after failing at
// request time or before yielding content. A VPN-only endpoint used while off
// VPN fails every request; without the cooldown each AI call would pay that
// endpoint's connect timeout before falling over to the working model. After
// the cooldown the entry is probed again, so coming back on VPN restores the
// primary within a minute.
const failoverCooldown = time.Minute

// failoverProvider tries a priority-ordered list of providers (one per
// configured model). It moves to the next model when a request fails outright
// or its stream errors before yielding any content; once content has flowed,
// errors propagate — partial output can't be un-emitted, and retrying would
// duplicate it. A circuit breaker skips recently failed entries for
// failoverCooldown (unless every entry is cooling down — then all are tried
// anyway, since guessing beats certain failure).
type failoverProvider struct {
	ps     []Provider
	models []string // parallel to ps, for tracing and activeModel

	mu       sync.Mutex
	failedAt []time.Time  // parallel to ps; zero = healthy
	served   atomic.Int32 // index of the entry the last request committed to
}

func newFailoverProvider(ps []Provider, models []string) *failoverProvider {
	return &failoverProvider{ps: ps, models: models, failedAt: make([]time.Time, len(ps))}
}

// activeModel names the chain entry the most recent request committed to (the
// primary before any request), so callers can log which model actually served —
// the interesting fact when the chain silently falls back.
func (f *failoverProvider) activeModel() string {
	return f.models[f.served.Load()]
}

func (f *failoverProvider) Name() string { return f.ps[0].Name() }

// cooling reports which entries are inside their post-failure cooldown, and
// whether that is all of them.
func (f *failoverProvider) cooling() (skip []bool, all bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	skip = make([]bool, len(f.ps))
	all = true
	for i, at := range f.failedAt {
		if !at.IsZero() && time.Since(at) < failoverCooldown {
			skip[i] = true
		} else {
			all = false
		}
	}
	return skip, all
}

func (f *failoverProvider) markFailed(i int) {
	f.mu.Lock()
	f.failedAt[i] = time.Now()
	f.mu.Unlock()
}

func (f *failoverProvider) markHealthy(i int) {
	f.mu.Lock()
	f.failedAt[i] = time.Time{}
	f.mu.Unlock()
}

func (f *failoverProvider) Stream(ctx context.Context, system string, msgs []Msg) (<-chan Chunk, error) {
	return f.StreamOpts(ctx, system, msgs, Options{})
}

func (f *failoverProvider) StreamOpts(ctx context.Context, system string, msgs []Msg, o Options) (<-chan Chunk, error) {
	skip, allCooling := f.cooling()
	var lastErr error
	for i, p := range f.ps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if skip[i] && !allCooling {
			logging.Trace("ai: failover skipping cooling endpoint", "model", f.models[i], "index", i)
			continue
		}
		ch, err := streamWith(p, ctx, system, msgs, o)
		if err != nil {
			if ctx.Err() == nil { // a user cancel is not the endpoint's fault
				f.markFailed(i)
			}
			logging.Trace("ai: failover request failed", "model", f.models[i], "index", i,
				"remaining", len(f.ps)-i-1, "err", err)
			lastErr = err
			continue
		}
		first, ok := <-ch
		if !ok {
			// A successful stream with no content — done.
			out := make(chan Chunk)
			close(out)
			f.markHealthy(i)
			f.served.Store(int32(i))
			if i > 0 {
				logging.Trace("ai: failover succeeded on backup", "model", f.models[i], "index", i)
			}
			return out, nil
		}
		if first.Err != nil {
			// The breaker cools this entry either way; with entries left the next
			// one is tried, on the last the error chunk is delivered as before.
			if ctx.Err() == nil {
				f.markFailed(i)
			}
			if i < len(f.ps)-1 {
				logging.Trace("ai: failover on pre-content stream error", "model", f.models[i], "index", i,
					"remaining", len(f.ps)-i-1, "err", first.Err)
				lastErr = first.Err
				continue
			}
		} else {
			f.markHealthy(i)
		}
		f.served.Store(int32(i))
		if i > 0 {
			logging.Trace("ai: failover succeeded on backup", "model", f.models[i], "index", i)
		}
		// Committed to this model: replay the first chunk, then pipe the rest.
		out := make(chan Chunk)
		go func() {
			defer close(out)
			select {
			case out <- first:
			case <-ctx.Done():
				return
			}
			for c := range ch {
				select {
				case out <- c:
				case <-ctx.Done():
					return
				}
			}
		}()
		return out, nil
	}
	return nil, lastErr
}

// transfer sums the byte counters of every child that tracks them, so the
// status bar reports AI traffic across all models.
func (f *failoverProvider) transfer() (in, out int64) {
	for _, p := range f.ps {
		if r, ok := p.(interface{ transfer() (int64, int64) }); ok {
			i, o := r.transfer()
			in += i
			out += o
		}
	}
	return in, out
}
