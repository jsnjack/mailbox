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
	uuid, err := randomUUID()
	if err != nil {
		return 0, err
	}
	logging.TraceContext(ctx, "store: enqueue outbox", "account", accountID, "thread", threadID, "uuid", uuid, "bytes", len(rfc822), "not_before", notBefore)
	res, err := s.writer.ExecContext(ctx, `
		INSERT INTO outbox (local_uuid, account_id, thread_id, draft_id, rfc822, state, not_before)
		VALUES (?, ?, ?, ?, ?, 'queued', ?)`, uuid, accountID, threadID, draftID, rfc822, notBefore)
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

// ListSendableOutbox returns an account's queued/failed messages that have not
// exceeded maxAttempts and whose undo window (not_before) has elapsed, oldest
// first. now is unix seconds (passed in so the caller controls the clock).
func (s *Store) ListSendableOutbox(ctx context.Context, accountID int64, maxAttempts int, now int64) ([]model.OutboxItem, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, local_uuid, account_id, thread_id, draft_id, rfc822, state, attempts, last_error, not_before
		FROM outbox
		WHERE account_id = ? AND state IN ('queued','failed') AND attempts < ? AND not_before <= ?
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
			it       model.OutboxItem
			threadID sql.NullString
			draftID  sql.NullString
			lastErr  sql.NullString
		)
		if err := rows.Scan(&it.ID, &it.LocalUUID, &it.AccountID, &threadID, &draftID, &it.RFC822, &it.State, &it.Attempts, &lastErr, &it.NotBefore); err != nil {
			return nil, fmt.Errorf("scan outbox: %w", err)
		}
		it.ThreadID = threadID.String
		it.DraftID = draftID.String
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
		`SELECT COUNT(*) FROM outbox WHERE account_id = ? AND state IN ('queued','failed') AND not_before <= ?`,
		accountID, now).Scan(&n); err != nil {
		logging.TraceContext(ctx, "store: count pending outbox", "account", accountID, "err", err)
		return 0, fmt.Errorf("count pending outbox: %w", err)
	}
	logging.TraceContext(ctx, "store: count pending outbox", "account", accountID, "count", n)
	return n, nil
}

// ListPendingOutbox returns all of an account's queued/failed messages past their
// undo window, oldest first — including ones that have exhausted their retry
// budget (those are the stuck sends the user most needs to see). now is unix
// seconds; a send still in its undo window is omitted.
func (s *Store) ListPendingOutbox(ctx context.Context, accountID int64, now int64) ([]model.OutboxItem, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, local_uuid, account_id, thread_id, draft_id, rfc822, state, attempts, last_error, not_before
		FROM outbox
		WHERE account_id = ? AND state IN ('queued','failed') AND not_before <= ?
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

// DeleteOutbox discards a queued/failed message without sending it.
func (s *Store) DeleteOutbox(ctx context.Context, id int64) error {
	logging.TraceContext(ctx, "store: delete outbox", "id", id)
	if _, err := s.writer.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, id); err != nil {
		logging.TraceContext(ctx, "store: delete outbox", "id", id, "err", err)
		return fmt.Errorf("delete outbox: %w", err)
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
