package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// EnqueueOutbox stores a built RFC 5322 message for (re)sending and returns its
// new row id. notBefore is a unix-seconds watermark (0 = send ASAP): a send held
// for its undo window is enqueued with notBefore in the near future, so it is
// persisted immediately (surviving a quit) yet invisible to the sweeper until the
// window elapses. draftID, when non-empty, is the source draft to delete once the
// send succeeds.
func (s *Store) EnqueueOutbox(ctx context.Context, accountID int64, threadID, draftID string, rfc822 []byte, notBefore int64) (int64, error) {
	return s.EnqueueOutboxFromDraft(ctx, accountID, threadID, draftID, "", rfc822, notBefore)
}

// EnqueueOutboxFromDraft is EnqueueOutbox plus the durable local draft whose
// cached copy must be removed only after delivery succeeds. Keeping it through
// Undo Send and failed delivery leaves a recoverable editable copy.
func (s *Store) EnqueueOutboxFromDraft(ctx context.Context, accountID int64, threadID, draftID, localDraftID string, rfc822 []byte, notBefore int64) (int64, error) {
	uuid, err := randomUUID()
	if err != nil {
		return 0, err
	}
	logging.TraceContext(ctx, "store: enqueue outbox", "account", accountID, "thread", threadID, "uuid", uuid, "bytes", len(rfc822), "not_before", notBefore)
	res, err := s.writer.ExecContext(ctx, `
		INSERT INTO outbox (local_uuid, account_id, thread_id, draft_id, local_draft_id, rfc822, state, not_before)
		VALUES (?, ?, ?, ?, ?, ?, 'queued', ?)`, uuid, accountID, threadID, draftID, localDraftID, rfc822, notBefore)
	if err != nil {
		logging.TraceContext(ctx, "store: enqueue outbox", "account", accountID, "uuid", uuid, "err", err)
		return 0, fmt.Errorf("enqueue outbox: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("enqueue outbox: last id: %w", err)
	}
	return id, nil
}

// ListSendableOutbox returns queued/failed messages eligible for delivery plus
// uncertain messages eligible only for Message-ID reconciliation. Uncertain
// delivery is never attempted automatically, regardless of attempt count.
func (s *Store) ListSendableOutbox(ctx context.Context, accountID int64, maxAttempts int, now int64) ([]model.OutboxItem, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, local_uuid, account_id, thread_id, draft_id, local_draft_id, rfc822, state, attempts, last_error, not_before
		FROM outbox
		WHERE account_id = ?
		  AND ((state IN ('queued','failed') AND attempts < ?) OR state = 'uncertain')
		  AND not_before <= ?
		ORDER BY id`, accountID, maxAttempts, now)
	if err != nil {
		logging.TraceContext(ctx, "store: list sendable outbox", "account", accountID, "err", err)
		return nil, fmt.Errorf("list outbox: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out, err := scanOutbox(rows)
	if err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: list sendable outbox", "account", accountID, "count", len(out))
	return out, nil
}

// scanOutbox reads outbox rows selected with the standard column list.
func scanOutbox(rows *sql.Rows) ([]model.OutboxItem, error) {
	var out []model.OutboxItem
	for rows.Next() {
		var (
			it           model.OutboxItem
			threadID     sql.NullString
			draftID      sql.NullString
			localDraftID sql.NullString
			lastErr      sql.NullString
		)
		if err := rows.Scan(&it.ID, &it.LocalUUID, &it.AccountID, &threadID, &draftID, &localDraftID, &it.RFC822, &it.State, &it.Attempts, &lastErr, &it.NotBefore); err != nil {
			return nil, fmt.Errorf("scan outbox: %w", err)
		}
		it.ThreadID = threadID.String
		it.DraftID = draftID.String
		it.LocalDraftID = localDraftID.String
		it.LastError = lastErr.String
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// CountPendingOutbox returns how many of an account's messages are awaiting
// send (queued or failed) and are past their undo window, regardless of attempt
// count. now is unix seconds; a send still in its undo window is not "pending".
func (s *Store) CountPendingOutbox(ctx context.Context, accountID int64, now int64) (int, error) {
	var n int
	if err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outbox WHERE account_id = ? AND state IN ('queued','failed','uncertain') AND not_before <= ?`,
		accountID, now).Scan(&n); err != nil {
		logging.TraceContext(ctx, "store: count pending outbox", "account", accountID, "err", err)
		return 0, fmt.Errorf("count pending outbox: %w", err)
	}
	logging.TraceContext(ctx, "store: count pending outbox", "account", accountID, "count", n)
	return n, nil
}

// CountUncertainOutbox returns messages whose provider acceptance could not be
// confirmed. They need user attention rather than another automatic send.
func (s *Store) CountUncertainOutbox(ctx context.Context, accountID int64, now int64) (int, error) {
	var n int
	if err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outbox WHERE account_id = ? AND state = 'uncertain' AND not_before <= ?`,
		accountID, now).Scan(&n); err != nil {
		logging.TraceContext(ctx, "store: count uncertain outbox", "account", accountID, "err", err)
		return 0, fmt.Errorf("count uncertain outbox: %w", err)
	}
	return n, nil
}

// ListPendingOutbox returns all of an account's queued/failed messages past their
// undo window, oldest first — including ones that have exhausted their retry
// budget (those are the stuck sends the user most needs to see). now is unix
// seconds; a send still in its undo window is omitted.
func (s *Store) ListPendingOutbox(ctx context.Context, accountID int64, now int64) ([]model.OutboxItem, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, local_uuid, account_id, thread_id, draft_id, local_draft_id, rfc822, state, attempts, last_error, not_before
		FROM outbox
		WHERE account_id = ? AND state IN ('queued','failed','uncertain') AND not_before <= ?
		ORDER BY id`, accountID, now)
	if err != nil {
		logging.TraceContext(ctx, "store: list pending outbox", "account", accountID, "err", err)
		return nil, fmt.Errorf("list pending outbox: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out, err := scanOutbox(rows)
	if err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: list pending outbox", "account", accountID, "count", len(out))
	return out, nil
}

// RequeueOutbox resets an item to queued, clears its failure state, and clears
// its undo window (not_before = 0) so the next sweep retries it immediately, even
// if it had exhausted its attempts.
func (s *Store) RequeueOutbox(ctx context.Context, id int64) error {
	logging.TraceContext(ctx, "store: requeue outbox", "id", id)
	if _, err := s.writer.ExecContext(ctx,
		`UPDATE outbox SET state = 'queued', attempts = 0, last_error = NULL, not_before = 0 WHERE id = ?`,
		id); err != nil {
		logging.TraceContext(ctx, "store: requeue outbox", "id", id, "err", err)
		return fmt.Errorf("requeue outbox: %w", err)
	}
	return nil
}

// DeleteOutbox discards a queued, failed, or uncertain message. It reports
// whether the row was actually cancelled: false means a sweep had already
// claimed (state 'sending') or completed it — the message is going out (or has
// gone out), and the caller must not present it as unsent.
func (s *Store) DeleteOutbox(ctx context.Context, id int64) (bool, error) {
	logging.TraceContext(ctx, "store: delete outbox", "id", id)
	res, err := s.writer.ExecContext(ctx,
		`DELETE FROM outbox WHERE id = ? AND state IN ('queued','failed','uncertain')`, id)
	if err != nil {
		logging.TraceContext(ctx, "store: delete outbox", "id", id, "err", err)
		return false, fmt.Errorf("delete outbox: %w", err)
	}
	n, _ := res.RowsAffected()
	logging.TraceContext(ctx, "store: delete outbox done", "id", id, "cancelled", n > 0)
	return n > 0, nil
}

// ClaimOutbox atomically moves a pending row to 'sending', reporting
// whether the claim won. A false return means the row vanished (an undo
// discarded it) — the caller must not send it. Claiming before the network send
// is what makes undo-vs-sweep authoritative: once claimed, DeleteOutbox refuses
// the row; before the claim, a successful delete reliably stops the send.
func (s *Store) ClaimOutbox(ctx context.Context, id int64) (bool, error) {
	res, err := s.writer.ExecContext(ctx,
		`UPDATE outbox SET state = 'sending' WHERE id = ? AND state IN ('queued','failed','uncertain')`, id)
	if err != nil {
		logging.TraceContext(ctx, "store: claim outbox", "id", id, "err", err)
		return false, fmt.Errorf("claim outbox: %w", err)
	}
	n, _ := res.RowsAffected()
	logging.TraceContext(ctx, "store: claim outbox", "id", id, "claimed", n > 0)
	return n > 0, nil
}

// FailInterruptedSends converts leftover 'sending' rows to 'uncertain'. A crash
// may have happened after provider acceptance but before local acknowledgement,
// so automatic retry would risk duplicate delivery.
func (s *Store) FailInterruptedSends(ctx context.Context, accountID int64) error {
	res, err := s.writer.ExecContext(ctx, `
		UPDATE outbox SET state = 'uncertain', attempts = attempts + 1,
			last_error = 'Mailbox closed before delivery could be confirmed'
		WHERE account_id = ? AND state = 'sending'`, accountID)
	if err != nil {
		logging.TraceContext(ctx, "store: fail interrupted sends", "account", accountID, "err", err)
		return fmt.Errorf("fail interrupted sends: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		logging.TraceContext(ctx, "store: fail interrupted sends", "account", accountID, "count", n)
	}
	return nil
}

// MarkOutboxUncertain records a send whose provider acknowledgement was lost.
// It remains visible and is eligible for Message-ID reconciliation, but never
// for automatic delivery; RetryOutbox is the user's explicit override.
func (s *Store) MarkOutboxUncertain(ctx context.Context, id int64, errMsg string) error {
	logging.TraceContext(ctx, "store: mark outbox uncertain", "id", id, "reason", errMsg)
	if _, err := s.writer.ExecContext(ctx, `
		UPDATE outbox SET state = 'uncertain', attempts = attempts + 1, last_error = ? WHERE id = ?`,
		errMsg, id); err != nil {
		return fmt.Errorf("mark outbox uncertain: %w", err)
	}
	return nil
}

// KeepOutboxUncertain releases a reconciliation claim without counting another
// delivery attempt. Background Message-ID checks may run repeatedly while the
// item waits for provider indexing or a user's decision.
func (s *Store) KeepOutboxUncertain(ctx context.Context, id int64, note string) error {
	if _, err := s.writer.ExecContext(ctx, `
		UPDATE outbox SET state = 'uncertain', last_error = ? WHERE id = ? AND state = 'sending'`,
		note, id); err != nil {
		return fmt.Errorf("keep outbox uncertain: %w", err)
	}
	return nil
}

// MarkOutboxSent removes a successfully sent message from the outbox.
func (s *Store) MarkOutboxSent(ctx context.Context, id int64) error {
	logging.TraceContext(ctx, "store: mark outbox sent", "id", id)
	if _, err := s.writer.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, id); err != nil {
		logging.TraceContext(ctx, "store: mark outbox sent", "id", id, "err", err)
		return fmt.Errorf("mark outbox sent: %w", err)
	}
	return nil
}

// MarkOutboxFailed records a failed send attempt.
func (s *Store) MarkOutboxFailed(ctx context.Context, id int64, errMsg string) error {
	logging.TraceContext(ctx, "store: mark outbox failed", "id", id, "reason", errMsg)
	if _, err := s.writer.ExecContext(ctx,
		`UPDATE outbox SET state = 'failed', attempts = attempts + 1, last_error = ? WHERE id = ?`,
		errMsg, id); err != nil {
		logging.TraceContext(ctx, "store: mark outbox failed", "id", id, "err", err)
		return fmt.Errorf("mark outbox failed: %w", err)
	}
	return nil
}

func randomUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	return hex.EncodeToString(b), nil
}
