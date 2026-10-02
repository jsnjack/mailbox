package syncer

import (
	"context"
	"fmt"

	"github.com/jsnjack/mailbox/internal/backend"
)

type bodyFetchKey struct {
	accountID int64
	id        string
}

type bodyFetch struct {
	done chan struct{}
	err  error
}

// EnsureBody caches a missing body, sharing any download already in progress.
// FetchBody also shares in-flight downloads, but can refresh a cached body.
func (e *Engine) EnsureBody(ctx context.Context, b backend.Backend, accountID int64, id string) error {
	return e.fetchBody(ctx, b, accountID, id, true)
}

func (e *Engine) fetchBody(ctx context.Context, b backend.Backend, accountID int64, id string, onlyMissing bool) (err error) {
	key := bodyFetchKey{accountID, id}
	e.bodyFetchMu.Lock()
	if active := e.bodyFetches[key]; active != nil {
		e.bodyFetchMu.Unlock()
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for message body %q: %w", id, ctx.Err())
		case <-active.done:
			return active.err
		}
	}
	if e.bodyFetches == nil {
		e.bodyFetches = make(map[bodyFetchKey]*bodyFetch)
	}
	active := &bodyFetch{done: make(chan struct{})}
	e.bodyFetches[key] = active
	e.bodyFetchMu.Unlock()
	defer func() {
		e.bodyFetchMu.Lock()
		active.err = err
		delete(e.bodyFetches, key)
		close(active.done)
		e.bodyFetchMu.Unlock()
	}()

	m, err := e.Store.GetMessage(ctx, accountID, id)
	if err != nil {
		return fmt.Errorf("load message for body %q: %w", id, err)
	}
	if onlyMissing && m.BodyFetched {
		return nil
	}
	if err := e.downloadBody(ctx, b, m); err != nil {
		return fmt.Errorf("fetch message body %q: %w", id, err)
	}
	return nil
}
