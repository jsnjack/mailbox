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

// EnqueueOutbox stores a built RFC 5322 message for (re)sending.
func (s *Store) EnqueueOutbox(ctx context.Context, accountID int64, threadID string, rfc822 []byte) error {
	uuid, err := randomUUID()
	if err != nil {
		return err
	}
	logging.TraceContext(ctx, "store: enqueue outbox", "account", accountID, "thread", threadID, "uuid", uuid, "bytes", len(rfc822))
	if _, err := s.writer.ExecContext(ctx, `
		INSERT INTO outbox (local_uuid, account_id, thread_id, rfc822, state)
		VALUES (?, ?, ?, ?, 'queued')`, uuid, accountID, threadID, rfc822); err != nil {
		logging.TraceContext(ctx, "store: enqueue outbox", "account", accountID, "uuid", uuid, "err", err)
		return fmt.Errorf("enqueue outbox: %w", err)
	}
	return nil
}

// ListSendableOutbox returns an account's queued/failed messages that have not
// exceeded maxAttempts, oldest first.
func (s *Store) ListSendableOutbox(ctx context.Context, accountID int64, maxAttempts int) ([]model.OutboxItem, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, local_uuid, account_id, thread_id, rfc822, state, attempts, last_error
		FROM outbox
		WHERE account_id = ? AND state IN ('queued','failed') AND attempts < ?
		ORDER BY id`, accountID, maxAttempts)
	if err != nil {
		logging.TraceContext(ctx, "store: list sendable outbox", "account", accountID, "err", err)
		return nil, fmt.Errorf("list outbox: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.OutboxItem
	for rows.Next() {
		var (
			it       model.OutboxItem
			threadID sql.NullString
			lastErr  sql.NullString
		)
		if err := rows.Scan(&it.ID, &it.LocalUUID, &it.AccountID, &threadID, &it.RFC822, &it.State, &it.Attempts, &lastErr); err != nil {
			return nil, fmt.Errorf("scan outbox: %w", err)
		}
		it.ThreadID = threadID.String
		it.LastError = lastErr.String
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: list sendable outbox", "account", accountID, "count", len(out))
	return out, nil
}

// CountPendingOutbox returns how many of an account's messages are awaiting
// send (queued or failed), regardless of attempt count.
func (s *Store) CountPendingOutbox(ctx context.Context, accountID int64) (int, error) {
	var n int
	if err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outbox WHERE account_id = ? AND state IN ('queued','failed')`,
		accountID).Scan(&n); err != nil {
		logging.TraceContext(ctx, "store: count pending outbox", "account", accountID, "err", err)
		return 0, fmt.Errorf("count pending outbox: %w", err)
	}
	logging.TraceContext(ctx, "store: count pending outbox", "account", accountID, "count", n)
	return n, nil
}

// ListPendingOutbox returns all of an account's queued/failed messages, oldest
// first — including ones that have exhausted their retry budget (those are the
// stuck sends the user most needs to see).
func (s *Store) ListPendingOutbox(ctx context.Context, accountID int64) ([]model.OutboxItem, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, local_uuid, account_id, thread_id, rfc822, state, attempts, last_error
		FROM outbox
		WHERE account_id = ? AND state IN ('queued','failed')
		ORDER BY id`, accountID)
	if err != nil {
		logging.TraceContext(ctx, "store: list pending outbox", "account", accountID, "err", err)
		return nil, fmt.Errorf("list pending outbox: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.OutboxItem
	for rows.Next() {
		var (
			it       model.OutboxItem
			threadID sql.NullString
			lastErr  sql.NullString
		)
		if err := rows.Scan(&it.ID, &it.LocalUUID, &it.AccountID, &threadID, &it.RFC822, &it.State, &it.Attempts, &lastErr); err != nil {
			return nil, fmt.Errorf("scan outbox: %w", err)
		}
		it.ThreadID = threadID.String
		it.LastError = lastErr.String
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: list pending outbox", "account", accountID, "count", len(out))
	return out, nil
}

// RequeueOutbox resets an item to queued and clears its failure state so the
// next sweep retries it, even if it had exhausted its attempts.
func (s *Store) RequeueOutbox(ctx context.Context, id int64) error {
	logging.TraceContext(ctx, "store: requeue outbox", "id", id)
	if _, err := s.writer.ExecContext(ctx,
		`UPDATE outbox SET state = 'queued', attempts = 0, last_error = NULL WHERE id = ?`,
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
