package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/jsnjack/mailbox/internal/gmailapi"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

// ErrHistoryExpired means the stored historyId is older than Gmail's retention
// window; the caller should run a full backfill to recover.
var ErrHistoryExpired = errors.New("gmail historyId expired; full resync required")

// backfillWorkers bounds how many message fetches run concurrently from the
// engine. The Gmail client additionally caps in-flight requests and quota use,
// so this only needs to keep the pipeline full.
const backfillWorkers = 20

// Engine runs sync operations against the store and publishes changes.
type Engine struct {
	Store *store.Store
	Hub   *Hub
}

// NewEngine returns an engine writing to st and publishing to hub (hub may be nil).
func NewEngine(st *store.Store, hub *Hub) *Engine {
	return &Engine{Store: st, Hub: hub}
}

func (e *Engine) publish(c Change) {
	if e.Hub != nil {
		e.Hub.Publish(c)
	}
}

// SyncLabels refreshes the account's label set from Gmail.
func (e *Engine) SyncLabels(ctx context.Context, c *gmailapi.Client, accountID int64) (int, error) {
	labels, err := c.ListLabels(ctx)
	if err != nil {
		return 0, err
	}
	for _, l := range labels {
		if err := e.Store.UpsertLabel(ctx, gmailapi.ToLabel(accountID, l)); err != nil {
			return 0, err
		}
	}
	e.publish(Change{Kind: LabelsSynced, AccountID: accountID, Count: len(labels)})
	return len(labels), nil
}

// Backfill lists message ids matching query (empty = all), newest first up to
// max (0 = all), fetches each message's metadata concurrently, and upserts it.
// Individual fetch failures are logged and skipped; it returns the number stored.
func (e *Engine) Backfill(ctx context.Context, c *gmailapi.Client, accountID int64, query string, max int) (int, error) {
	ids, err := c.ListMessageIDs(ctx, query, max)
	if err != nil {
		return 0, fmt.Errorf("list message ids: %w", err)
	}

	var (
		wg   sync.WaitGroup
		done atomic.Int64
		idCh = make(chan string)
	)
	for i := 0; i < backfillWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range idCh {
				msg, err := c.GetMessageMetadata(ctx, id)
				if err != nil {
					slog.Default().Warn("backfill: fetch metadata", "id", id, "err", err)
					continue
				}
				if _, err := e.Store.UpsertMessage(ctx, gmailapi.ToMessage(accountID, msg)); err != nil {
					slog.Default().Warn("backfill: upsert", "id", id, "err", err)
					continue
				}
				n := done.Add(1)
				e.publish(Change{Kind: BackfillProgress, AccountID: accountID, Count: int(n)})
			}
		}()
	}

	var feedErr error
feed:
	for _, id := range ids {
		select {
		case <-ctx.Done():
			feedErr = ctx.Err()
			break feed
		case idCh <- id:
		}
	}
	close(idCh)
	wg.Wait()

	e.publish(Change{Kind: BackfillComplete, AccountID: accountID, Count: int(done.Load())})
	return int(done.Load()), feedErr
}

// FetchBody downloads a message's full body and caches it, marking it fetched.
func (e *Engine) FetchBody(ctx context.Context, c *gmailapi.Client, accountID int64, gmailID string) error {
	m, err := e.Store.GetMessage(ctx, accountID, gmailID)
	if err != nil {
		return err
	}
	full, err := c.GetMessageFull(ctx, gmailID)
	if err != nil {
		return err
	}
	body := gmailapi.ToBody(full)
	body.MessageRowID = m.RowID
	if err := e.Store.UpsertBody(ctx, body); err != nil {
		return err
	}
	e.publish(Change{Kind: MessageUpserted, AccountID: accountID, GmailID: gmailID})
	return nil
}

// Send builds and transmits an outgoing message via Gmail. After it returns, the
// sent message arrives in the local cache through the next incremental sync.
func (e *Engine) Send(ctx context.Context, c *gmailapi.Client, accountID int64, msg model.OutgoingMessage) error {
	raw, err := gmailapi.BuildMIME(msg)
	if err != nil {
		return err
	}
	if _, err := c.Send(ctx, raw, msg.ThreadID); err != nil {
		return err
	}
	return nil
}

// ModifyLabels applies a label change locally first (instant, optimistic) and
// then mirrors it to Gmail. On the next incremental sync the server state
// reconciles any divergence if the API call failed.
func (e *Engine) ModifyLabels(ctx context.Context, c *gmailapi.Client, accountID int64, gmailID string, add, remove []string) error {
	if err := e.Store.ModifyLabels(ctx, accountID, gmailID, add, remove); err != nil {
		return err
	}
	e.publish(Change{Kind: MessageUpserted, AccountID: accountID, GmailID: gmailID})
	if c != nil {
		if err := c.ModifyLabels(ctx, gmailID, add, remove); err != nil {
			return fmt.Errorf("modify labels on server: %w", err)
		}
	}
	return nil
}

// Incremental applies Gmail history since the account's stored watermark:
// additions and label changes are re-fetched and upserted, deletions removed.
// It returns ErrHistoryExpired if the watermark is too old to use.
func (e *Engine) Incremental(ctx context.Context, c *gmailapi.Client, accountID int64) (int, error) {
	acc, err := e.Store.GetAccountByID(ctx, accountID)
	if err != nil {
		return 0, err
	}
	if acc.LastHistoryID == "" {
		return 0, fmt.Errorf("account %d has no history watermark; backfill first", accountID)
	}

	records, newest, err := c.ListHistory(ctx, acc.LastHistoryID)
	if err != nil {
		if gmailapi.IsHistoryExpired(err) {
			return 0, ErrHistoryExpired
		}
		return 0, err
	}

	refetch := make(map[string]bool)
	deletes := make(map[string]bool)
	for _, r := range records {
		for _, x := range r.MessagesAdded {
			if x.Message != nil {
				refetch[x.Message.Id] = true
			}
		}
		for _, x := range r.LabelsAdded {
			if x.Message != nil {
				refetch[x.Message.Id] = true
			}
		}
		for _, x := range r.LabelsRemoved {
			if x.Message != nil {
				refetch[x.Message.Id] = true
			}
		}
		for _, x := range r.MessagesDeleted {
			if x.Message != nil {
				deletes[x.Message.Id] = true
				delete(refetch, x.Message.Id)
			}
		}
	}

	changed := 0
	for id := range deletes {
		if err := e.Store.DeleteMessage(ctx, accountID, id); err != nil {
			return changed, err
		}
		e.publish(Change{Kind: MessageDeleted, AccountID: accountID, GmailID: id})
		changed++
	}
	for id := range refetch {
		msg, err := c.GetMessageMetadata(ctx, id)
		if err != nil {
			// The message may have been removed between the history record and
			// this fetch; log and move on rather than aborting the sync.
			slog.Default().Warn("incremental: fetch metadata", "id", id, "err", err)
			continue
		}
		if _, err := e.Store.UpsertMessage(ctx, gmailapi.ToMessage(accountID, msg)); err != nil {
			return changed, err
		}
		e.publish(Change{Kind: MessageUpserted, AccountID: accountID, GmailID: id})
		changed++
	}

	if err := e.Store.SetLastHistoryID(ctx, accountID, newest); err != nil {
		return changed, err
	}
	return changed, nil
}
