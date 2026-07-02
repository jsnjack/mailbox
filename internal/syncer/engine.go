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

	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/config"
	"github.com/jsnjack/mailbox/internal/logging"
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

// backfillBatch is how many fetched messages are committed per transaction
// during backfill and search hydration — large enough to amortize commit/fsync
// overhead, small enough to keep memory and lock-hold time bounded.
const backfillBatch = 200

// Engine runs sync operations against the store and publishes changes.
type Engine struct {
	Store *store.Store
	Hub   *Hub

	// sweepMu serializes SweepOutbox so overlapping sweeps (the background
	// timer, the user's "Send now", and per-item "retry" all trigger one) can't
	// both claim and send the same queued item — which would deliver the message
	// to the recipient twice. Sends are infrequent, so serializing them is free.
	sweepMu sync.Mutex
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

// NotifyAuthExpired publishes an AuthExpired change so the UI can tell the user
// to reconnect the account. Used when a sync fails because the refresh token was
// revoked or expired — a state that cannot be recovered without re-login.
func (e *Engine) NotifyAuthExpired(accountID int64) {
	logging.Trace("syncer: auth expired", "account", accountID)
	e.publish(Change{Kind: AuthExpired, AccountID: accountID})
}

// SyncLabels refreshes the account's label set from Gmail.
func (e *Engine) SyncLabels(ctx context.Context, b backend.Backend, accountID int64) (int, error) {
	start := time.Now()
	logging.TraceContext(ctx, "syncer: SyncLabels", "account", accountID)
	labels, err := b.Labels(ctx)
	if err != nil {
		logging.TraceContext(ctx, "syncer: SyncLabels fetch failed", "account", accountID, "dur", time.Since(start), "err", err)
		return 0, err
	}
	for _, l := range labels {
		if err := e.Store.UpsertLabel(ctx, l); err != nil {
			logging.TraceContext(ctx, "syncer: SyncLabels upsert failed", "account", accountID, "label", l.GmailID, "err", err)
			return 0, err
		}
	}
	e.publish(Change{Kind: LabelsSynced, AccountID: accountID, Count: len(labels)})
	logging.TraceContext(ctx, "syncer: SyncLabels ok", "account", accountID, "count", len(labels), "dur", time.Since(start))
	return len(labels), nil
}

// Backfill lists message ids matching query (empty = all), newest first up to
// max (0 = all), fetches each message's metadata concurrently, and upserts it.
// Individual fetch failures are logged and skipped; it returns the number stored.
func (e *Engine) Backfill(ctx context.Context, b backend.Backend, accountID int64, query string, max int) (int, error) {
	start := time.Now()
	logging.TraceContext(ctx, "syncer: Backfill start", "account", accountID, "query", query, "max", max, "workers", backfillWorkers)
	ids, err := b.SearchIDs(ctx, query, max)
	if err != nil {
		logging.TraceContext(ctx, "syncer: Backfill list ids failed", "account", accountID, "query", query, "err", err)
		return 0, fmt.Errorf("list message ids: %w", err)
	}
	logging.TraceContext(ctx, "syncer: Backfill listed ids", "account", accountID, "count", len(ids))

	// Fetch metadata concurrently, but write in batches: each worker only does
	// network work and hands the converted message to a single collector that
	// commits ~backfillBatch rows per transaction. This keeps the network
	// parallelism while turning N transactions/fsyncs into N/batch.
	//
	// A derived context lets the collector stop the pipeline on a write error:
	// cancel() unblocks the feeder (select on ctx.Done) and aborts in-flight
	// fetches, and the collector keeps draining resCh so no worker blocks on a
	// full buffer — guaranteeing every goroutine unwinds.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		idCh  = make(chan string)
		resCh = make(chan model.Message, backfillWorkers)
	)
	for i := 0; i < backfillWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range idCh {
				msg, err := b.FetchMetadata(ctx, id)
				if err != nil {
					slog.Default().Warn("backfill: fetch metadata", "id", id, "err", err)
					continue
				}
				resCh <- msg
			}
		}()
	}
	go func() { wg.Wait(); close(resCh) }() // close results once all fetchers done

	// Feed ids on a goroutine so the collector below runs concurrently and the
	// bounded resCh never deadlocks.
	go func() {
		defer close(idCh)
		for _, id := range ids {
			select {
			case <-ctx.Done():
				return
			case idCh <- id:
			}
		}
	}()

	stored := 0
	batch := make([]model.Message, 0, backfillBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		bstart := time.Now()
		if err := e.Store.UpsertMessages(ctx, batch); err != nil {
			return fmt.Errorf("backfill upsert: %w", err)
		}
		stored += len(batch)
		logging.TraceContext(ctx, "syncer: Backfill batch committed", "account", accountID, "batch", len(batch), "stored", stored, "dur", time.Since(bstart))
		e.publish(Change{Kind: BackfillProgress, AccountID: accountID, Count: stored})
		batch = batch[:0]
		return nil
	}
	var flushErr error
	for m := range resCh {
		if flushErr != nil {
			continue // keep draining so workers and the feeder can finish
		}
		batch = append(batch, m)
		if len(batch) >= backfillBatch {
			if flushErr = flush(); flushErr != nil {
				cancel() // stop feeding; let the pipeline wind down
			}
		}
	}
	if flushErr != nil {
		logging.TraceContext(ctx, "syncer: Backfill aborted", "account", accountID, "stored", stored, "dur", time.Since(start), "err", flushErr)
		return stored, flushErr
	}
	if err := flush(); err != nil {
		logging.TraceContext(ctx, "syncer: Backfill final flush failed", "account", accountID, "stored", stored, "dur", time.Since(start), "err", err)
		return stored, err
	}

	e.publish(Change{Kind: BackfillComplete, AccountID: accountID, Count: stored})
	logging.TraceContext(ctx, "syncer: Backfill complete", "account", accountID, "total", stored, "dur", time.Since(start), "ctxErr", ctx.Err())
	return stored, ctx.Err()
}

// SearchServer runs a Gmail server-side search (query is Gmail's q= syntax),
// caches the matching messages' metadata that isn't already local, and returns
// the matching message ids (Gmail's relevance order). This lets the user find
// mail beyond the local cache.
func (e *Engine) SearchServer(ctx context.Context, b backend.Backend, accountID int64, query string, max int) ([]string, error) {
	ids, err := b.SearchIDs(ctx, query, max)
	if err != nil {
		return nil, fmt.Errorf("search list ids: %w", err)
	}
	var fetched []model.Message
	for _, id := range ids {
		if _, err := e.Store.GetMessage(ctx, accountID, id); err == nil {
			continue // already cached
		}
		msg, err := b.FetchMetadata(ctx, id)
		if err != nil {
			slog.Default().Warn("search: fetch metadata", "id", id, "err", err)
			continue
		}
		fetched = append(fetched, msg)
	}
	if len(fetched) > 0 {
		if err := e.Store.UpsertMessages(ctx, fetched); err != nil {
			return nil, fmt.Errorf("search upsert: %w", err)
		}
		e.publish(Change{Kind: MessageUpserted, AccountID: accountID})
	}
	return ids, nil
}

// DeletePermanently removes messages for good (server batchDelete + local
// delete). Used for "Delete forever" from Trash/Spam; cannot be undone.
func (e *Engine) DeletePermanently(ctx context.Context, b backend.Backend, accountID int64, gmailIDs []string) error {
	if len(gmailIDs) == 0 {
		return nil
	}
	if err := b.Delete(ctx, gmailIDs); err != nil {
		return fmt.Errorf("delete permanently: %w", err)
	}
	for _, id := range gmailIDs {
		if err := e.Store.DeleteMessage(ctx, accountID, id); err != nil {
			slog.Default().Warn("delete permanently: local", "id", id, "err", err)
		}
	}
	e.publish(Change{Kind: MessageDeleted, AccountID: accountID})
	return nil
}

// EmptyLabel permanently deletes every message in Trash or Spam (server-side
// list + batchDelete + local delete), returning how many were removed. Used by
// "Empty Trash/Spam"; cannot be undone.
func (e *Engine) EmptyLabel(ctx context.Context, b backend.Backend, accountID int64, labelID string) (int, error) {
	var query string
	switch labelID {
	case model.LabelTrash:
		query = "in:trash"
	case model.LabelSpam:
		query = "in:spam"
	default:
		return 0, fmt.Errorf("can only empty Trash or Spam")
	}
	ids, err := b.SearchIDs(ctx, query, 0)
	if err != nil {
		return 0, fmt.Errorf("empty %s: list: %w", labelID, err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if err := b.Delete(ctx, ids); err != nil {
		return 0, fmt.Errorf("empty %s: delete: %w", labelID, err)
	}
	for _, id := range ids {
		if err := e.Store.DeleteMessage(ctx, accountID, id); err != nil {
			slog.Default().Warn("empty label: local delete", "id", id, "err", err)
		}
	}
	e.publish(Change{Kind: MessageDeleted, AccountID: accountID})
	return len(ids), nil
}

// FetchBody downloads a message's full body and caches it, marking it fetched.
func (e *Engine) FetchBody(ctx context.Context, b backend.Backend, accountID int64, gmailID string) error {
	start := time.Now()
	m, err := e.Store.GetMessage(ctx, accountID, gmailID)
	if err != nil {
		return err
	}
	netStart := time.Now()
	body, atts, err := b.FetchBody(ctx, gmailID)
	if err != nil {
		return err
	}
	netDur := time.Since(netStart)
	defer func() {
		slog.Default().Debug("engine: FetchBody", "id", gmailID, "network", netDur, "total", time.Since(start))
	}()
	body.MessageRowID = m.RowID
	if err := e.Store.UpsertBody(ctx, body); err != nil {
		return err
	}
	if err := e.Store.ReplaceAttachments(ctx, m.RowID, atts); err != nil {
		slog.Default().Warn("store attachments", "id", gmailID, "err", err)
	}
	e.publish(Change{Kind: MessageUpserted, AccountID: accountID, GmailID: gmailID})
	return nil
}

// htmlBackfillWorkers bounds the concurrency of the one-time HTML backfill. Kept
// low so re-fetching a large cache stays a gentle background trickle rather than
// a startup spike; the Gmail client throttles requests/quota on top of this.
const htmlBackfillWorkers = 4

// BackfillHTMLBodies re-fetches cached messages that have no HTML body and were
// fetched before externalized-HTML support, recovering the HTML that an older
// build dropped when Gmail served a large HTML part via an attachment id. It
// re-fetches at most max messages (newest first), low-concurrency, and stores
// them quietly — no per-message change is published, so a bulk pass can't flood
// the UI; the recovered body is read on the message's next open. Returns the
// number actually re-fetched. A no-op once every body is at the current version.
func (e *Engine) BackfillHTMLBodies(ctx context.Context, b backend.Backend, accountID int64, max int) (int, error) {
	ids, err := e.Store.MessagesMissingHTML(ctx, accountID, max)
	if err != nil {
		return 0, fmt.Errorf("list missing-html: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	var (
		sem = make(chan struct{}, htmlBackfillWorkers)
		wg  sync.WaitGroup
		n   atomic.Int64
	)
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			m, err := e.Store.GetMessage(ctx, accountID, id)
			if err != nil {
				slog.Default().Warn("html backfill: get message", "id", id, "err", err)
				return
			}
			body, atts, err := b.FetchBody(ctx, id)
			if err != nil {
				slog.Default().Warn("html backfill: fetch body", "id", id, "err", err)
				return
			}
			body.MessageRowID = m.RowID
			if err := e.Store.UpsertBody(ctx, body); err != nil { // stamps body_fetched = 2
				slog.Default().Warn("html backfill: store body", "id", id, "err", err)
				return
			}
			if err := e.Store.ReplaceAttachments(ctx, m.RowID, atts); err != nil {
				slog.Default().Warn("html backfill: store attachments", "id", id, "err", err)
			}
			n.Add(1)
		}(id)
	}
	wg.Wait()
	return int(n.Load()), nil
}

// OpenAttachment ensures an attachment's bytes are cached on disk (downloading
// them if needed) and returns the local file path. The file is content-addressed
// by SHA-256 under the attachment cache directory.
func (e *Engine) OpenAttachment(ctx context.Context, b backend.Backend, gmailID string, attID int64) (string, error) {
	a, err := e.Store.GetAttachmentByID(ctx, attID)
	if err != nil {
		return "", err
	}
	if a.DiskPath != "" {
		if _, err := os.Stat(a.DiskPath); err == nil {
			return a.DiskPath, nil
		}
	}
	data, err := b.FetchAttachment(ctx, gmailID, a.GmailAttID)
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
func (e *Engine) Send(ctx context.Context, b backend.Backend, accountID int64, msg model.OutgoingMessage) error {
	if strings.TrimSpace(msg.To) == "" {
		return fmt.Errorf("message has no recipient")
	}
	raw, err := backend.BuildMIME(msg)
	if err != nil {
		return err
	}
	sentID, err := b.Send(ctx, raw, msg.ThreadID)
	if err != nil {
		if qerr := e.Store.EnqueueOutbox(ctx, accountID, msg.ThreadID, raw); qerr != nil {
			return fmt.Errorf("send failed (%v) and could not queue: %w", err, qerr)
		}
		// The message left the drafts for the outbox; drop the source draft so it
		// doesn't linger and duplicate the queued send.
		if msg.DraftID != "" {
			if derr := b.DeleteDraft(ctx, msg.DraftID); derr != nil {
				slog.Default().Warn("send: delete source draft after queue", "id", msg.DraftID, "err", derr)
			}
		}
		e.publish(Change{Kind: SendStateChanged, AccountID: accountID})
		return fmt.Errorf("send failed, queued for retry: %w", err)
	}
	// Sending an edited draft creates a new message; remove the original draft.
	if msg.DraftID != "" {
		if err := b.DeleteDraft(ctx, msg.DraftID); err != nil {
			slog.Default().Warn("send: delete source draft", "id", msg.DraftID, "err", err)
		}
	}
	// Reflect the sent message in the local cache so it joins the conversation
	// immediately, rather than only after the next incremental sync.
	e.storeSentMessage(ctx, b, accountID, sentID)
	return nil
}

// storeSentMessage fetches a just-sent message's metadata and upserts it, then
// publishes a MessageUpserted carrying its thread id so the UI can drop it into
// the open conversation. Best-effort: the next sync would reconcile it anyway,
// so failures only delay its appearance, they don't lose it.
func (e *Engine) storeSentMessage(ctx context.Context, b backend.Backend, accountID int64, id string) {
	if id == "" {
		return
	}
	m, err := b.FetchMetadata(ctx, id)
	if err != nil {
		slog.Default().Warn("send: fetch sent message", "id", id, "err", err)
		return
	}
	if err := e.Store.UpsertMessages(ctx, []model.Message{m}); err != nil {
		slog.Default().Warn("send: store sent message", "id", id, "err", err)
		return
	}
	e.publish(Change{Kind: MessageUpserted, AccountID: accountID, GmailID: id, ThreadID: m.ThreadID})
}

// RetryOutbox requeues a single outbox item (clearing its failed state and
// attempt count) and immediately attempts to send everything sendable for the
// account — used by the user's manual "retry" action.
func (e *Engine) RetryOutbox(ctx context.Context, b backend.Backend, accountID, id int64) error {
	if err := e.Store.RequeueOutbox(ctx, id); err != nil {
		return err
	}
	e.publish(Change{Kind: SendStateChanged, AccountID: accountID})
	_, err := e.SweepOutbox(ctx, b, accountID)
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
func (e *Engine) SaveDraft(ctx context.Context, b backend.Backend, accountID int64, msg model.OutgoingMessage) error {
	raw, err := backend.BuildMIME(msg)
	if err != nil {
		return err
	}
	if msg.DraftID != "" {
		_, err = b.UpdateDraft(ctx, msg.DraftID, raw, msg.ThreadID)
		return err
	}
	if _, err := b.SaveDraft(ctx, raw, msg.ThreadID); err != nil {
		return err
	}
	return nil
}

// SweepOutbox retries queued/failed messages for an account, returning how many
// were sent. It is run periodically in the background.
func (e *Engine) SweepOutbox(ctx context.Context, b backend.Backend, accountID int64) (int, error) {
	// Serialize sweeps: the lock spans the list→send→mark loop so a second sweep
	// blocks until the first finishes, then re-lists and sees the items already
	// sent (no duplicate delivery). See sweepMu.
	// Serialize sweeps: the lock spans the list→send→mark loop so a second sweep
	// blocks until the first finishes, then re-lists and sees the items already
	// sent (no duplicate delivery). See sweepMu.
	e.sweepMu.Lock()
	defer e.sweepMu.Unlock()

	items, err := e.Store.ListSendableOutbox(ctx, accountID, maxOutboxAttempts)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, it := range items {
		if _, err := b.Send(ctx, it.RFC822, it.ThreadID); err != nil {
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
func (e *Engine) ModifyLabelsBatch(ctx context.Context, b backend.Backend, accountID int64, gmailIDs []string, add, remove []string) error {
	if len(gmailIDs) == 0 {
		return nil
	}
	start := time.Now()
	for _, id := range gmailIDs {
		if err := e.Store.ModifyLabels(ctx, accountID, id, add, remove); err != nil {
			return fmt.Errorf("modify labels (local): %w", err)
		}
	}
	// One coarse event (no GmailID) so the UI refreshes once, not per message.
	e.publish(Change{Kind: MessageUpserted, AccountID: accountID})
	slog.Default().Debug("engine: ModifyLabelsBatch (local)",
		"n", len(gmailIDs), "add", add, "remove", remove, "dur", time.Since(start))

	// Mirror to Gmail in the background so the UI updates instantly off the local
	// change instead of waiting on the round-trip (e.g. unstarring should leave the
	// Starred folder at once). A failure is logged; the next incremental sync
	// reconciles any divergence.
	if b != nil {
		go func() {
			if err := b.ApplyLabels(context.Background(), gmailIDs, add, remove); err != nil {
				slog.Default().Warn("modify labels: mirror to provider", "n", len(gmailIDs), "err", err)
			}
		}()
	}
	return nil
}

// MarkLabelRead marks every unread message in a label as read: optimistically
// in the store, then mirrored to Gmail with a single batch call.
func (e *Engine) MarkLabelRead(ctx context.Context, b backend.Backend, accountID int64, labelID string) error {
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
	if b != nil {
		if err := b.ApplyLabels(ctx, ids, nil, []string{model.LabelUnread}); err != nil {
			return fmt.Errorf("mark label read on server: %w", err)
		}
	}
	return nil
}

// Incremental applies provider changes since the account's stored cursor:
// additions and label changes are re-fetched and upserted, deletions removed.
// It returns ErrHistoryExpired if the cursor is too old to use.
func (e *Engine) Incremental(ctx context.Context, b backend.Backend, accountID int64) (int, error) {
	defer func(start time.Time) {
		slog.Default().Debug("engine: Incremental", "account", accountID, "dur", time.Since(start))
	}(time.Now())
	logging.TraceContext(ctx, "syncer: Incremental start", "account", accountID)
	acc, err := e.Store.GetAccountByID(ctx, accountID)
	if err != nil {
		return 0, err
	}
	if acc.SyncCursor == "" {
		logging.TraceContext(ctx, "syncer: Incremental no cursor; backfill first", "account", accountID)
		return 0, fmt.Errorf("account %d has no sync cursor; backfill first", accountID)
	}

	upserts, deletes, next, err := b.Changes(ctx, acc.SyncCursor)
	if err != nil {
		if errors.Is(err, backend.ErrCursorExpired) {
			// Cursor fell out of the provider's history window: signal the caller to
			// self-heal via Resync (a full re-backfill) rather than retry incremental.
			logging.TraceContext(ctx, "syncer: Incremental cursor expired -> resync required", "account", accountID, "cursor", acc.SyncCursor, "err", err)
			return 0, ErrHistoryExpired
		}
		logging.TraceContext(ctx, "syncer: Incremental changes failed", "account", accountID, "cursor", acc.SyncCursor, "err", err)
		return 0, err
	}
	logging.TraceContext(ctx, "syncer: Incremental changes", "account", accountID, "cursor", acc.SyncCursor, "next", next, "upserts", len(upserts), "deletes", len(deletes))

	changed := 0
	if len(deletes) > 0 {
		dstart := time.Now()
		if err := e.Store.DeleteMessages(ctx, accountID, deletes); err != nil {
			return changed, err
		}
		logging.TraceContext(ctx, "syncer: Incremental deleted", "account", accountID, "count", len(deletes), "dur", time.Since(dstart))
		// One event per id so per-message consumers still see each deletion; the
		// UI coalesces the resulting refresh.
		for _, id := range deletes {
			e.publish(Change{Kind: MessageDeleted, AccountID: accountID, GmailID: id})
		}
		changed += len(deletes)
	}

	// Fetch all changed messages concurrently, write them in one transaction,
	// then publish a per-id event so new-mail notifications (which need the id)
	// still fire. Concurrency makes catching up a burst of external changes
	// (e.g. a bulk archive done on another device) N/workers round-trips, not N.
	msgs, fetchedIDs, transientFail := e.fetchMetadataConcurrent(ctx, b, upserts)
	if len(msgs) > 0 {
		ustart := time.Now()
		if err := e.Store.UpsertMessages(ctx, msgs); err != nil {
			return changed, err
		}
		logging.TraceContext(ctx, "syncer: Incremental upserted", "account", accountID, "count", len(msgs), "dur", time.Since(ustart))
		for i, id := range fetchedIDs {
			tid := ""
			if i < len(msgs) {
				tid = msgs[i].ThreadID // msgs and fetchedIDs are parallel
			}
			e.publish(Change{Kind: MessageUpserted, AccountID: accountID, GmailID: id, ThreadID: tid})
		}
		changed += len(msgs)
	}

	// Only advance the cursor once every changed message has been durably applied
	// (or is genuinely gone). If a fetch failed transiently — a network blip or a
	// sustained outage that outlasted the client's own retries — advancing past it
	// would skip that message forever (its history record is behind the new
	// cursor). Holding the cursor makes the next incremental re-walk the same
	// range and retry; the deletes and successful upserts already applied are
	// idempotent, so re-processing is harmless. A vanished message (ErrNotFound)
	// is not a transient failure, so it doesn't stall the cursor.
	if transientFail {
		logging.TraceContext(ctx, "syncer: Incremental holding cursor (transient fetch failure)", "account", accountID, "cursor", acc.SyncCursor, "fetched", len(fetchedIDs), "wanted", len(upserts))
		return changed, nil
	}

	if err := e.Store.SetSyncCursor(ctx, accountID, next); err != nil {
		return changed, err
	}
	logging.TraceContext(ctx, "syncer: Incremental done", "account", accountID, "changed", changed, "next", next)
	return changed, nil
}

// Resync recovers from an expired history watermark — the ErrHistoryExpired that
// Incremental returns once an account has been offline past Gmail's history
// retention window. It captures the current historyId, re-backfills the newest
// max messages (upserting — it never truncates the cache), then advances the
// watermark so incremental sync resumes from a valid point. Without this an
// expired watermark leaves the account stuck failing every incremental forever,
// so new mail never appears.
//
// The watermark is captured before backfilling (so mail arriving during the
// backfill isn't missed) but stored only after it succeeds (so a failed backfill
// never advances past un-fetched history). Deletions made while the watermark was
// expired aren't reconciled — incremental handles deletions going forward, and a
// full-mailbox id diff isn't worth its cost here.
func (e *Engine) Resync(ctx context.Context, b backend.Backend, accountID int64, max int) (int, error) {
	start := time.Now()
	logging.TraceContext(ctx, "syncer: Resync start", "account", accountID, "max", max)
	prof, err := b.Profile(ctx)
	if err != nil {
		logging.TraceContext(ctx, "syncer: Resync profile failed", "account", accountID, "err", err)
		return 0, fmt.Errorf("resync: profile: %w", err)
	}
	watermark := prof.Cursor
	logging.TraceContext(ctx, "syncer: Resync captured watermark", "account", accountID, "historyId", watermark)
	n, err := e.Backfill(ctx, b, accountID, "", max)
	if err != nil {
		logging.TraceContext(ctx, "syncer: Resync backfill failed", "account", accountID, "stored", n, "err", err)
		return n, fmt.Errorf("resync: backfill: %w", err)
	}
	if err := e.Store.SetSyncCursor(ctx, accountID, watermark); err != nil {
		logging.TraceContext(ctx, "syncer: Resync set cursor failed", "account", accountID, "historyId", watermark, "err", err)
		return n, fmt.Errorf("resync: set cursor: %w", err)
	}
	logging.TraceContext(ctx, "syncer: Resync done", "account", accountID, "stored", n, "historyId", watermark, "dur", time.Since(start))
	return n, nil
}

// fetchMetadataConcurrent fetches each id's metadata in parallel (bounded by
// backfillWorkers) and returns the converted messages and their ids in input
// order. An id that is genuinely gone (backend.ErrNotFound) is skipped silently.
// Any other fetch error is transient (network blip, exhausted retries) and sets
// the returned transientFail flag so the caller can decline to advance the sync
// cursor past a message it hasn't actually stored yet. Each goroutine writes its
// own slot, so there is no shared-state contention.
func (e *Engine) fetchMetadataConcurrent(ctx context.Context, b backend.Backend, ids []string) (msgs []model.Message, fetchedIDs []string, transientFail bool) {
	type slot struct {
		msg       model.Message
		ok        bool
		transient bool
	}
	slots := make([]slot, len(ids))
	sem := make(chan struct{}, backfillWorkers)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id string) {
			defer wg.Done()
			defer func() { <-sem }()
			msg, err := b.FetchMetadata(ctx, id)
			if err != nil {
				gone := errors.Is(err, backend.ErrNotFound)
				slog.Default().Warn("incremental: fetch metadata", "id", id, "gone", gone, "err", err)
				slots[i] = slot{transient: !gone}
				return
			}
			slots[i] = slot{msg: msg, ok: true}
		}(i, id)
	}
	wg.Wait()

	msgs = make([]model.Message, 0, len(ids))
	fetchedIDs = make([]string, 0, len(ids))
	for i, s := range slots {
		if s.ok {
			msgs = append(msgs, s.msg)
			fetchedIDs = append(fetchedIDs, ids[i])
		}
		if s.transient {
			transientFail = true
		}
	}
	return msgs, fetchedIDs, transientFail
}
