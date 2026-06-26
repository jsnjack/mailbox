package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jsnjack/mailbox/internal/config"
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

// SearchServer runs a Gmail server-side search (query is Gmail's q= syntax),
// caches the matching messages' metadata that isn't already local, and returns
// the matching message ids (Gmail's relevance order). This lets the user find
// mail beyond the local cache.
func (e *Engine) SearchServer(ctx context.Context, c *gmailapi.Client, accountID int64, query string, max int) ([]string, error) {
	ids, err := c.ListMessageIDs(ctx, query, max)
	if err != nil {
		return nil, fmt.Errorf("search list ids: %w", err)
	}
	fetched := 0
	for _, id := range ids {
		if _, err := e.Store.GetMessage(ctx, accountID, id); err == nil {
			continue // already cached
		}
		msg, err := c.GetMessageMetadata(ctx, id)
		if err != nil {
			slog.Default().Warn("search: fetch metadata", "id", id, "err", err)
			continue
		}
		if _, err := e.Store.UpsertMessage(ctx, gmailapi.ToMessage(accountID, msg)); err != nil {
			slog.Default().Warn("search: upsert", "id", id, "err", err)
			continue
		}
		fetched++
	}
	if fetched > 0 {
		e.publish(Change{Kind: MessageUpserted, AccountID: accountID})
	}
	return ids, nil
}

// FetchBody downloads a message's full body and caches it, marking it fetched.
func (e *Engine) FetchBody(ctx context.Context, c *gmailapi.Client, accountID int64, gmailID string) error {
	start := time.Now()
	m, err := e.Store.GetMessage(ctx, accountID, gmailID)
	if err != nil {
		return err
	}
	netStart := time.Now()
	full, err := c.GetMessageFull(ctx, gmailID)
	if err != nil {
		return err
	}
	netDur := time.Since(netStart)
	defer func() {
		slog.Default().Debug("engine: FetchBody", "id", gmailID, "network", netDur, "total", time.Since(start))
	}()
	body := gmailapi.ToBody(full)
	body.MessageRowID = m.RowID
	if err := e.Store.UpsertBody(ctx, body); err != nil {
		return err
	}
	if err := e.Store.ReplaceAttachments(ctx, m.RowID, gmailapi.AttachmentsFromMessage(full)); err != nil {
		slog.Default().Warn("store attachments", "id", gmailID, "err", err)
	}
	e.publish(Change{Kind: MessageUpserted, AccountID: accountID, GmailID: gmailID})
	return nil
}

// OpenAttachment ensures an attachment's bytes are cached on disk (downloading
// them if needed) and returns the local file path. The file is content-addressed
// by SHA-256 under the attachment cache directory.
func (e *Engine) OpenAttachment(ctx context.Context, c *gmailapi.Client, gmailID string, attID int64) (string, error) {
	a, err := e.Store.GetAttachmentByID(ctx, attID)
	if err != nil {
		return "", err
	}
	if a.DiskPath != "" {
		if _, err := os.Stat(a.DiskPath); err == nil {
			return a.DiskPath, nil
		}
	}
	data, err := c.GetAttachment(ctx, gmailID, a.GmailAttID)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	dir, err := config.AttachmentsDir()
	if err != nil {
		return "", err
	}
	sub := filepath.Join(dir, sha[:2])
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return "", fmt.Errorf("create attachment dir: %w", err)
	}
	path := filepath.Join(sub, sha+filepath.Ext(a.Filename))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write attachment: %w", err)
	}
	if err := e.Store.SetAttachmentDownloaded(ctx, attID, sha, path); err != nil {
		return "", err
	}
	return path, nil
}

// maxOutboxAttempts bounds how many times the sweeper retries a queued message.
const maxOutboxAttempts = 5

// Send builds and transmits an outgoing message via Gmail. On a transient
// failure it queues the message to the outbox for the background sweeper to
// retry, and returns the error so the caller can inform the user. After a
// successful send the message arrives in the local cache via incremental sync.
func (e *Engine) Send(ctx context.Context, c *gmailapi.Client, accountID int64, msg model.OutgoingMessage) error {
	if strings.TrimSpace(msg.To) == "" {
		return fmt.Errorf("message has no recipient")
	}
	raw, err := gmailapi.BuildMIME(msg)
	if err != nil {
		return err
	}
	if _, err := c.Send(ctx, raw, msg.ThreadID); err != nil {
		if qerr := e.Store.EnqueueOutbox(ctx, accountID, msg.ThreadID, raw); qerr != nil {
			return fmt.Errorf("send failed (%v) and could not queue: %w", err, qerr)
		}
		// The message left the drafts for the outbox; drop the source draft so it
		// doesn't linger and duplicate the queued send.
		if msg.DraftID != "" {
			if derr := c.DeleteDraft(ctx, msg.DraftID); derr != nil {
				slog.Default().Warn("send: delete source draft after queue", "id", msg.DraftID, "err", derr)
			}
		}
		e.publish(Change{Kind: SendStateChanged, AccountID: accountID})
		return fmt.Errorf("send failed, queued for retry: %w", err)
	}
	// Sending an edited draft creates a new message; remove the original draft.
	if msg.DraftID != "" {
		if err := c.DeleteDraft(ctx, msg.DraftID); err != nil {
			slog.Default().Warn("send: delete source draft", "id", msg.DraftID, "err", err)
		}
	}
	return nil
}

// RetryOutbox requeues a single outbox item (clearing its failed state and
// attempt count) and immediately attempts to send everything sendable for the
// account — used by the user's manual "retry" action.
func (e *Engine) RetryOutbox(ctx context.Context, c *gmailapi.Client, accountID, id int64) error {
	if err := e.Store.RequeueOutbox(ctx, id); err != nil {
		return err
	}
	e.publish(Change{Kind: SendStateChanged, AccountID: accountID})
	_, err := e.SweepOutbox(ctx, c, accountID)
	return err
}

// DiscardOutbox removes a queued/failed message without sending it.
func (e *Engine) DiscardOutbox(ctx context.Context, accountID, id int64) error {
	if err := e.Store.DeleteOutbox(ctx, id); err != nil {
		return err
	}
	e.publish(Change{Kind: SendStateChanged, AccountID: accountID})
	return nil
}

// SaveDraft builds an outgoing message and stores it as a Gmail draft. When the
// message carries an existing DraftID it updates that draft in place rather than
// creating a new one (so editing a draft doesn't leave a duplicate).
func (e *Engine) SaveDraft(ctx context.Context, c *gmailapi.Client, accountID int64, msg model.OutgoingMessage) error {
	raw, err := gmailapi.BuildMIME(msg)
	if err != nil {
		return err
	}
	if msg.DraftID != "" {
		_, err = c.UpdateDraft(ctx, msg.DraftID, raw, msg.ThreadID)
		return err
	}
	if _, err := c.SaveDraft(ctx, raw, msg.ThreadID); err != nil {
		return err
	}
	return nil
}

// SweepOutbox retries queued/failed messages for an account, returning how many
// were sent. It is run periodically in the background.
func (e *Engine) SweepOutbox(ctx context.Context, c *gmailapi.Client, accountID int64) (int, error) {
	items, err := e.Store.ListSendableOutbox(ctx, accountID, maxOutboxAttempts)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, it := range items {
		if _, err := c.Send(ctx, it.RFC822, it.ThreadID); err != nil {
			if mErr := e.Store.MarkOutboxFailed(ctx, it.ID, err.Error()); mErr != nil {
				return sent, mErr
			}
			slog.Default().Warn("outbox: retry failed", "id", it.ID, "attempt", it.Attempts+1, "err", err)
			continue
		}
		if err := e.Store.MarkOutboxSent(ctx, it.ID); err != nil {
			return sent, err
		}
		e.publish(Change{Kind: SendStateChanged, AccountID: accountID})
		sent++
	}
	return sent, nil
}

// ModifyLabelsBatch applies the same label change to many messages: it updates
// every message locally first (instant, optimistic), publishes a single change,
// then mirrors the change to Gmail in one BatchModify call. This keeps archiving
// or marking a whole conversation to O(1) network round-trips and one UI refresh
// instead of one per message. Next incremental sync reconciles any divergence.
func (e *Engine) ModifyLabelsBatch(ctx context.Context, c *gmailapi.Client, accountID int64, gmailIDs []string, add, remove []string) error {
	if len(gmailIDs) == 0 {
		return nil
	}
	start := time.Now()
	for _, id := range gmailIDs {
		if err := e.Store.ModifyLabels(ctx, accountID, id, add, remove); err != nil {
			return fmt.Errorf("modify labels (local): %w", err)
		}
	}
	localDur := time.Since(start)
	// One coarse event (no GmailID) so the UI refreshes once, not per message.
	e.publish(Change{Kind: MessageUpserted, AccountID: accountID})

	var netDur time.Duration
	if c != nil {
		netStart := time.Now()
		if err := c.BatchModify(ctx, gmailIDs, add, remove); err != nil {
			return fmt.Errorf("modify labels on server: %w", err)
		}
		netDur = time.Since(netStart)
	}
	slog.Default().Debug("engine: ModifyLabelsBatch",
		"n", len(gmailIDs), "add", add, "remove", remove,
		"local", localDur, "network", netDur, "total", time.Since(start))
	return nil
}

// MarkLabelRead marks every unread message in a label as read: optimistically
// in the store, then mirrored to Gmail with a single batch call.
func (e *Engine) MarkLabelRead(ctx context.Context, c *gmailapi.Client, accountID int64, labelID string) error {
	defer func(start time.Time) {
		slog.Default().Debug("engine: MarkLabelRead", "label", labelID, "dur", time.Since(start))
	}(time.Now())
	ids, err := e.Store.UnreadIDsByLabel(ctx, accountID, labelID)
	if err != nil {
		return fmt.Errorf("mark label read: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	if err := e.Store.MarkLabelReadLocal(ctx, accountID, labelID); err != nil {
		return fmt.Errorf("mark label read: %w", err)
	}
	e.publish(Change{Kind: LabelsSynced, AccountID: accountID})
	if c != nil {
		if err := c.BatchModify(ctx, ids, nil, []string{model.LabelUnread}); err != nil {
			return fmt.Errorf("mark label read on server: %w", err)
		}
	}
	return nil
}

// Incremental applies Gmail history since the account's stored watermark:
// additions and label changes are re-fetched and upserted, deletions removed.
// It returns ErrHistoryExpired if the watermark is too old to use.
func (e *Engine) Incremental(ctx context.Context, c *gmailapi.Client, accountID int64) (int, error) {
	defer func(start time.Time) {
		slog.Default().Debug("engine: Incremental", "account", accountID, "dur", time.Since(start))
	}(time.Now())
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
