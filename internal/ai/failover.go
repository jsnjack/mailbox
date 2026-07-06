package ai

import (
	"context"

	"github.com/jsnjack/mailbox/internal/logging"
)

// failoverProvider tries a priority-ordered list of providers (one per
// configured model). It moves to the next model when a request fails outright
// or its stream errors before yielding any content; once content has flowed,
// errors propagate — partial output can't be un-emitted, and retrying would
// duplicate it.
type failoverProvider struct {
	ps     []Provider
	models []string // parallel to ps, for tracing
}

func newFailoverProvider(ps []Provider, models []string) *failoverProvider {
	return &failoverProvider{ps: ps, models: models}
}

func (f *failoverProvider) Name() string { return f.ps[0].Name() }

func (f *failoverProvider) Stream(ctx context.Context, system string, msgs []Msg) (<-chan Chunk, error) {
	return f.StreamOpts(ctx, system, msgs, Options{})
}

func (f *failoverProvider) StreamOpts(ctx context.Context, system string, msgs []Msg, o Options) (<-chan Chunk, error) {
	var lastErr error
	for i, p := range f.ps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ch, err := streamWith(p, ctx, system, msgs, o)
		if err != nil {
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
			if i > 0 {
				logging.Trace("ai: failover succeeded on backup", "model", f.models[i], "index", i)
			}
			return out, nil
		}
		if first.Err != nil && i < len(f.ps)-1 {
			logging.Trace("ai: failover on pre-content stream error", "model", f.models[i], "index", i,
				"remaining", len(f.ps)-i-1, "err", first.Err)
			lastErr = first.Err
			continue
		}
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
