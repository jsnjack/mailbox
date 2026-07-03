package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
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

	// mirror holds a per-account FIFO queue (drained by one goroutine each) that
	// serializes provider label-mirror operations in submission order, so an
	// action and its Undo can't reach the provider out of order. See mirrorAsync.
	mirrorMu sync.Mutex
	mirror   map[int64]chan func()
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
	stored, err := e.backfill(ctx, b, accountID, query, max)
	return len(stored), err
}

// backfill is Backfill's body; it returns the ids actually committed so Resync
// can seed an honest cursor (see backend.CursorSeeder).
func (e *Engine) backfill(ctx context.Context, b backend.Backend, accountID int64, query string, max int) ([]string, error) {
	start := time.Now()
	logging.TraceContext(ctx, "syncer: Backfill start", "account", accountID, "query", query, "max", max, "workers", backfillWorkers)
	ids, err := b.SearchIDs(ctx, query, max)
	if err != nil {
		logging.TraceContext(ctx, "syncer: Backfill list ids failed", "account", accountID, "query", query, "err", err)
		return nil, fmt.Errorf("list message ids: %w", err)
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
	// A batch-capable backend (IMAP) fetches a whole chunk in one round-trip, so
	// feed chunks of metadataBatchSize; a per-id backend (Gmail) keeps chunkSize 1
	// so the workers stay 20-wide over individual fetches, exactly as before.
	chunkSize := 1
	if _, ok := b.(backend.BatchMetadataFetcher); ok {
		chunkSize = metadataBatchSize
	}
	var (
		wg    sync.WaitGroup
		idCh  = make(chan []string)
		resCh = make(chan model.Message, backfillWorkers)
	)
	for i := 0; i < backfillWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunk := range idCh {
				for _, m := range e.fetchChunk(ctx, b, chunk) {
					resCh <- m
				}
			}
		}()
	}
	go func() { wg.Wait(); close(resCh) }() // close results once all fetchers done

	// Feed id chunks on a goroutine so the collector below runs concurrently and
	// the bounded resCh never deadlocks.
	go func() {
		defer close(idCh)
		for start := 0; start < len(ids); start += chunkSize {
			end := start + chunkSize
			if end > len(ids) {
				end = len(ids)
			}
			select {
			case <-ctx.Done():
				return
			case idCh <- ids[start:end]:
			}
		}
	}()

	var stored []string
	batch := make([]model.Message, 0, backfillBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		bstart := time.Now()
		if err := e.Store.UpsertMessages(ctx, batch); err != nil {
			return fmt.Errorf("backfill upsert: %w", err)
		}
		for _, m := range batch {
			stored = append(stored, m.GmailID)
		}
		logging.TraceContext(ctx, "syncer: Backfill batch committed", "account", accountID, "batch", len(batch), "stored", len(stored), "dur", time.Since(bstart))
		e.publish(Change{Kind: BackfillProgress, AccountID: accountID, Count: len(stored)})
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
		logging.TraceContext(ctx, "syncer: Backfill aborted", "account", accountID, "stored", len(stored), "dur", time.Since(start), "err", flushErr)
		return stored, flushErr
	}
	if err := flush(); err != nil {
		logging.TraceContext(ctx, "syncer: Backfill final flush failed", "account", accountID, "stored", len(stored), "dur", time.Since(start), "err", err)
		return stored, err
	}

	e.publish(Change{Kind: BackfillComplete, AccountID: accountID, Count: len(stored)})
	logging.TraceContext(ctx, "syncer: Backfill complete", "account", accountID, "total", len(stored), "dur", time.Since(start), "ctxErr", ctx.Err())
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
	if len(ids) == 0 {
		return ids, nil
	}
	// Existence check in one IN-query instead of a GetMessage per hit, then fetch
	// only the misses — concurrently, and batched for a backend that supports it
	// (IMAP) — rather than a serial FetchMetadata per uncached id.
	existing, err := e.Store.ExistingMessageIDs(ctx, accountID, ids)
	if err != nil {
		return nil, fmt.Errorf("search existence check: %w", err)
	}
	missing := make([]string, 0, len(ids))
	for _, id := range ids {
		if !existing[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		fetched, _, _ := e.fetchMetadataConcurrent(ctx, b, missing)
		if len(fetched) > 0 {
			if err := e.Store.UpsertMessages(ctx, fetched); err != nil {
				return nil, fmt.Errorf("search upsert: %w", err)
			}
			e.publish(Change{Kind: MessageUpserted, AccountID: accountID})
		}
	}
	logging.TraceContext(ctx, "syncer: SearchServer done", "account", accountID, "query", query, "hits", len(ids), "cached", len(ids)-len(missing), "fetched_misses", len(missing))
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
	if err := e.Store.DeleteMessages(ctx, accountID, gmailIDs); err != nil {
		slog.Default().Warn("delete permanently: local", "n", len(gmailIDs), "err", err)
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
	// Belt and braces before an irreversible bulk delete: never trust the backend
	// to have scoped the query. Any returned id that is cached locally but does
	// NOT carry the target label is dropped (and loudly logged) instead of
	// deleted. Ids we don't have cached are kept — a server-side listing can
	// legitimately reach beyond the local cache (e.g. Gmail trash older than the
	// backfill window).
	existing, err := e.Store.ExistingMessageIDs(ctx, accountID, ids)
	if err != nil {
		return 0, fmt.Errorf("empty %s: verify: %w", labelID, err)
	}
	labeled, err := e.Store.MessageIDsWithLabel(ctx, accountID, labelID, ids)
	if err != nil {
		return 0, fmt.Errorf("empty %s: verify: %w", labelID, err)
	}
	keep := make([]string, 0, len(ids))
	skipped := 0
	for _, id := range ids {
		if existing[id] && !labeled[id] {
			skipped++
			logging.TraceContext(ctx, "syncer: EmptyLabel skipping out-of-scope id", "account", accountID, "label", labelID, "id", id)
			continue
		}
		keep = append(keep, id)
	}
	if skipped > 0 {
		slog.Default().Warn("empty label: backend returned ids outside the label; skipped them",
			"label", labelID, "skipped", skipped, "kept", len(keep))
	}
	if len(keep) == 0 {
		return 0, nil
	}
	if err := b.Delete(ctx, keep); err != nil {
		return 0, fmt.Errorf("empty %s: delete: %w", labelID, err)
	}
	if err := e.Store.DeleteMessages(ctx, accountID, keep); err != nil {
		slog.Default().Warn("empty label: local delete", "n", len(keep), "err", err)
	}
	e.publish(Change{Kind: MessageDeleted, AccountID: accountID})
	return len(keep), nil
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
	gmailAcct := e.isGmailAccount(ctx, accountID)
	for _, it := range items {
		// Guard against resending a message a prior attempt already delivered: a
		// send can succeed at the provider yet return a network error (the response
		// was lost), which enqueues it here. Before resending, check whether the
		// provider already has a message with this item's Message-ID and, if so,
		// mark it sent instead of delivering a duplicate. Gmail-only: the check uses
		// Gmail's rfc822msgid: search and the raw send preserves our Message-ID.
		if gmailAcct {
			if mid := messageIDOf(it.RFC822); mid != "" {
				if ids, serr := b.SearchIDs(ctx, "rfc822msgid:"+mid, 1); serr == nil && len(ids) > 0 {
					logging.TraceContext(ctx, "syncer: outbox already at provider; not resending", "id", it.ID, "msgid", mid)
					if err := e.Store.MarkOutboxSent(ctx, it.ID); err != nil {
						return sent, err
					}
					e.publish(Change{Kind: SendStateChanged, AccountID: accountID})
					continue
				} else if serr != nil {
					logging.TraceContext(ctx, "syncer: outbox dedup search failed; proceeding to send", "id", it.ID, "err", serr)
				}
			}
		}
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

// isGmailAccount reports whether accountID is a Gmail account, so outbox dedup
// can use Gmail's rfc822msgid: search (IMAP has no equivalent through the
// provider-agnostic SearchIDs, so it opts out and keeps the prior behavior). A
// lookup failure conservatively returns false (skip dedup, just send).
func (e *Engine) isGmailAccount(ctx context.Context, accountID int64) bool {
	acc, err := e.Store.GetAccountByID(ctx, accountID)
	if err != nil {
		return false
	}
	return acc.Type != model.AccountIMAP // "" (unset) is treated as Gmail
}

// messageIDOf extracts the Message-ID header (without angle brackets) from a raw
// RFC 5322 message, or "" if absent/unparseable. Used to dedup outbox resends.
func messageIDOf(rfc822 []byte) string {
	m, err := mail.ReadMessage(bytes.NewReader(rfc822))
	if err != nil {
		return ""
	}
	return strings.Trim(m.Header.Get("Message-ID"), "<> ")
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
	// Apply every id in one transaction (one commit/fsync) instead of a tx per
	// message; missing ids are skipped inside the store.
	if err := e.Store.ModifyLabelsBatch(ctx, accountID, gmailIDs, add, remove); err != nil {
		return fmt.Errorf("modify labels (local): %w", err)
	}
	// One coarse event (no GmailID) so the UI refreshes once, not per message.
	e.publish(Change{Kind: MessageUpserted, AccountID: accountID})
	slog.Default().Debug("engine: ModifyLabelsBatch (local)",
		"n", len(gmailIDs), "add", add, "remove", remove, "dur", time.Since(start))

	// Mirror to Gmail off the calling path so the UI updates instantly off the
	// local change instead of waiting on the round-trip (e.g. unstarring should
	// leave the Starred folder at once). Ordered per account (see mirrorAsync): an
	// action and a later Undo that reverses it must reach the provider in the order
	// the user made them — otherwise a slow first call could be overtaken by the
	// second, leaving Gmail (and, since sync is Gmail→local, the cache after the
	// next sync) in the wrong final state. A failure is logged; the next
	// incremental sync reconciles any divergence.
	if b != nil {
		e.mirrorAsync(accountID, func() {
			if err := b.ApplyLabels(context.Background(), gmailIDs, add, remove); err != nil {
				slog.Default().Warn("modify labels: mirror to provider", "n", len(gmailIDs), "err", err)
			}
		})
	}
	return nil
}

// mirrorAsync runs fn on accountID's serial mirror queue, off the caller's path
// but preserving submission order: a single goroutine per account drains a FIFO
// channel, so mirror operations apply one-at-a-time in the order they were
// enqueued. This keeps an action and its Undo from racing to the provider. The
// channel is buffered so the (already background) caller returns immediately; a
// backlog deep enough to fill it just applies backpressure.
//
// The send happens under mirrorMu so StopAccount (which closes the channel
// under the same lock) can never close it mid-send. The drain goroutine never
// takes the lock, so a full buffer still drains and the send can't deadlock.
func (e *Engine) mirrorAsync(accountID int64, fn func()) {
	e.mirrorMu.Lock()
	defer e.mirrorMu.Unlock()
	ch, ok := e.mirror[accountID]
	if !ok {
		if e.mirror == nil {
			e.mirror = make(map[int64]chan func())
		}
		ch = make(chan func(), 128)
		e.mirror[accountID] = ch
		logging.Trace("syncer: mirror queue started", "account", accountID)
		go func() {
			for f := range ch {
				f()
			}
			logging.Trace("syncer: mirror queue drained and stopped", "account", accountID)
		}()
	}
	ch <- fn
}

// StopAccount releases the engine's per-account resources — today the mirror
// queue: it is closed (the drain goroutine finishes any already-queued mirrors,
// then exits) and forgotten, so a later start for the same account gets a fresh
// queue instead of a leaked goroutine whose queued closures capture the old,
// torn-down backend across a reconnect. Callers should invoke it whenever an
// account's runtime is stopped (remove, reconnect).
func (e *Engine) StopAccount(accountID int64) {
	e.mirrorMu.Lock()
	defer e.mirrorMu.Unlock()
	ch, ok := e.mirror[accountID]
	if !ok {
		logging.Trace("syncer: stop account (no mirror queue)", "account", accountID)
		return
	}
	delete(e.mirror, accountID)
	close(ch)
	logging.Trace("syncer: stop account (mirror queue closed)", "account", accountID, "queued", len(ch))
}

// MarkLabelRead marks every unread message in a label as read: optimistically
// in the store, then mirrored to the provider with a single batch call. The
// mirror goes through the same per-account FIFO as ModifyLabelsBatch so it
// cannot overtake (or be overtaken by) an earlier label change or its Undo; a
// mirror failure is logged and reconciled by the next incremental sync.
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
		e.mirrorAsync(accountID, func() {
			if err := b.ApplyLabels(context.Background(), ids, nil, []string{model.LabelUnread}); err != nil {
				slog.Default().Warn("mark label read: mirror to provider", "label", labelID, "n", len(ids), "err", err)
			}
		})
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
//
// A backend whose cursor enumerates message state rather than a change log
// (backend.CursorSeeder — IMAP) instead gets its cursor built from exactly the
// ids the backfill stored: seeding from the pre-backfill Profile snapshot would
// mark every UID the cap skipped as already-seen, hiding them forever.
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
	storedIDs, err := e.backfill(ctx, b, accountID, "", max)
	n := len(storedIDs)
	if err != nil {
		logging.TraceContext(ctx, "syncer: Resync backfill failed", "account", accountID, "stored", n, "err", err)
		return n, fmt.Errorf("resync: backfill: %w", err)
	}
	if seeder, ok := b.(backend.CursorSeeder); ok {
		cur, serr := seeder.SeedCursor(ctx, storedIDs)
		if serr != nil {
			logging.TraceContext(ctx, "syncer: Resync seed cursor failed", "account", accountID, "err", serr)
			return n, fmt.Errorf("resync: seed cursor: %w", serr)
		}
		logging.TraceContext(ctx, "syncer: Resync using seeded cursor (backfilled ids only)", "account", accountID, "stored", n, "cursor_bytes", len(cur))
		watermark = cur
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
	if len(ids) == 0 {
		return nil, nil, false
	}
	// Prefer a batched fetch when the backend supports it (IMAP: one FETCH per
	// ~200 ids per folder instead of one round-trip per message). Gmail has no
	// batch metadata endpoint and falls through to the per-id path below.
	if bf, ok := b.(backend.BatchMetadataFetcher); ok {
		return e.fetchMetadataBatched(ctx, bf, ids)
	}
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

// fetchChunk fetches metadata for a chunk of ids for backfill, using the
// backend's batch fetcher when it has one (IMAP: one FETCH per chunk) and falling
// back to a per-id loop otherwise (Gmail). Fetch failures are logged and skipped —
// backfill is best-effort: a skipped id simply isn't stored this pass and
// resurfaces on a later sync (Resync seeds its cursor from the ids actually
// stored, so nothing is silently marked already-seen).
func (e *Engine) fetchChunk(ctx context.Context, b backend.Backend, ids []string) []model.Message {
	if bf, ok := b.(backend.BatchMetadataFetcher); ok {
		msgs, err := bf.FetchMetadataBatch(ctx, ids)
		if err != nil {
			slog.Default().Warn("backfill: fetch metadata batch", "n", len(ids), "err", err)
			return nil
		}
		return msgs
	}
	out := make([]model.Message, 0, len(ids))
	for _, id := range ids {
		msg, err := b.FetchMetadata(ctx, id)
		if err != nil {
			slog.Default().Warn("backfill: fetch metadata", "id", id, "err", err)
			continue
		}
		out = append(out, msg)
	}
	return out
}

// metadataBatchSize is how many ids each batched metadata call requests. Chunking
// lets the calls run concurrently (using the backend's connection pool) and bounds
// any single provider operation.
const metadataBatchSize = 200

// fetchMetadataBatched fetches ids via the backend's batch fetcher, splitting them
// into fixed-size chunks fetched concurrently. A chunk that fails at the transport
// layer sets transientFail (so the caller holds the sync cursor and retries),
// while ids simply absent from a successful chunk's result are treated as gone —
// mirroring the per-id path's ErrNotFound handling. fetchedIDs is built from the
// returned messages so it stays parallel to msgs.
func (e *Engine) fetchMetadataBatched(ctx context.Context, bf backend.BatchMetadataFetcher, ids []string) (msgs []model.Message, fetchedIDs []string, transientFail bool) {
	var chunks [][]string
	for start := 0; start < len(ids); start += metadataBatchSize {
		end := start + metadataBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		chunks = append(chunks, ids[start:end])
	}
	type result struct {
		msgs      []model.Message
		transient bool
	}
	results := make([]result, len(chunks))
	sem := make(chan struct{}, backfillWorkers)
	var wg sync.WaitGroup
	for i, chunk := range chunks {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, chunk []string) {
			defer wg.Done()
			defer func() { <-sem }()
			got, err := bf.FetchMetadataBatch(ctx, chunk)
			if err != nil {
				slog.Default().Warn("incremental: fetch metadata batch", "n", len(chunk), "err", err)
				results[i] = result{transient: true}
				return
			}
			results[i] = result{msgs: got}
		}(i, chunk)
	}
	wg.Wait()

	msgs = make([]model.Message, 0, len(ids))
	fetchedIDs = make([]string, 0, len(ids))
	for _, r := range results {
		if r.transient {
			transientFail = true
		}
		for _, m := range r.msgs {
			msgs = append(msgs, m)
			fetchedIDs = append(fetchedIDs, m.GmailID)
		}
	}
	return msgs, fetchedIDs, transientFail
}
