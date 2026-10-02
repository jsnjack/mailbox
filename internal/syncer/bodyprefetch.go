package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jsnjack/mailbox/internal/activity"
	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

const (
	bodyPrefetchWorkers  = 4
	bodyPrefetchBatch    = 50
	bodyPrefetchTimeout  = 60 * time.Second
	bodyPrefetchInterval = 60 * time.Second
)

// RunBodyPrefetch caches unfetched Inbox bodies until ctx is canceled. Store queries
// recover dropped sync events and unfinished work after restart. cutoff is read
// for every batch so retention changes apply live; nil keeps all Inbox bodies.
func (e *Engine) RunBodyPrefetch(ctx context.Context, b backend.Backend, act *activity.Hub, accountID int64, email string, cutoff func() (int64, error)) {
	var changes <-chan Change
	if e.Hub != nil {
		var unsubscribe func()
		changes, unsubscribe = e.Hub.Subscribe()
		defer unsubscribe()
	}
	ticker := time.NewTicker(bodyPrefetchInterval)
	defer ticker.Stop()
	retryAfter := make(map[int64]time.Time)
	ready := make(chan struct{})
	close(ready)
	var beforeRowID int64
	for ctx.Err() == nil {
		var since int64
		var err error
		if cutoff != nil {
			since, err = cutoff()
		}
		var msgs []model.Message
		if err == nil {
			msgs, err = e.Store.MessagesMissingBodies(ctx, accountID, since, beforeRowID, bodyPrefetchBatch)
		}
		if err != nil {
			slog.Warn("body prefetch: list messages", "account", accountID, "err", err)
		} else {
			e.prefetchBodyBatch(ctx, b, act, accountID, email, msgs, retryAfter)
		}
		if ctx.Err() != nil {
			return
		}
		more := err == nil && len(msgs) == bodyPrefetchBatch
		if more {
			beforeRowID = msgs[len(msgs)-1].RowID
		} else {
			beforeRowID = 0
		}

		// A newly synced batch takes priority over the older startup backlog.
		var next <-chan struct{}
		if more {
			next = ready
		}
	waitForWork:
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				beforeRowID = 0
				break waitForWork
			case c, ok := <-changes:
				if !ok {
					return
				}
				if bodyPrefetchChange(c, accountID) {
					beforeRowID = 0
					break waitForWork
				}
			case <-next:
				break waitForWork
			}
		}
	}
}

func bodyPrefetchChange(c Change, accountID int64) bool {
	return c.AccountID == accountID && (c.Kind == MessageUpserted || c.Kind == BackfillProgress || c.Kind == BackfillComplete)
}

func (e *Engine) prefetchBodyBatch(ctx context.Context, b backend.Backend, act *activity.Hub, accountID int64, email string, msgs []model.Message, retryAfter map[int64]time.Time) {
	now := e.now()
	for rowID, retry := range retryAfter {
		if !now.Before(retry) {
			delete(retryAfter, rowID)
		}
	}
	var todo []model.Message
	for _, m := range msgs {
		if _, cooling := retryAfter[m.RowID]; !cooling {
			todo = append(todo, m)
		}
	}
	if len(todo) == 0 {
		return
	}
	done := act.Begin("fetch", email, "Prefetching message bodies")
	type result struct {
		rowID int64
		err   error
	}
	results := make(chan result, len(todo))
	sem := make(chan struct{}, bodyPrefetchWorkers)
	var wg sync.WaitGroup
feed:
	for _, m := range todo {
		select {
		case <-ctx.Done():
			break feed
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(m model.Message) {
			defer wg.Done()
			defer func() { <-sem }()
			fetchCtx, cancel := context.WithTimeout(ctx, bodyPrefetchTimeout)
			defer cancel()
			// A message may have been archived while waiting for a worker slot.
			current, err := e.Store.GetMessage(fetchCtx, accountID, m.GmailID)
			if err == nil && !slices.Contains(current.Labels, model.LabelInbox) {
				return
			}
			if err == nil {
				err = e.EnsureBody(fetchCtx, b, accountID, m.GmailID)
			}
			results <- result{m.RowID, err}
			if err != nil && ctx.Err() == nil {
				slog.Warn("body prefetch: fetch message", "account", accountID, "id", m.GmailID, "err", err)
			}
		}(m)
	}
	wg.Wait()
	close(results)
	var fetched, failed int
	for r := range results {
		if r.err == nil {
			fetched++
		} else if !errors.Is(r.err, store.ErrNotFound) {
			failed++
			retryAfter[r.rowID] = e.now().Add(bodyPrefetchInterval)
		}
	}
	note := activity.Plural(fetched, "body cached", "bodies cached")
	if failed > 0 {
		note += fmt.Sprintf("; %d failed, will retry", failed)
	}
	if ctx.Err() != nil {
		note += "; canceled"
	}
	done(note)
}
