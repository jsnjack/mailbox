package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/jsnjack/mailbox/internal/model"
)

// EnqueueOutbox stores a built RFC 5322 message for (re)sending.
func (s *Store) EnqueueOutbox(ctx context.Context, accountID int64, threadID string, rfc822 []byte) error {
	uuid, err := randomUUID()
	if err != nil {
		return err
	}
	if _, err := s.writer.ExecContext(ctx, `
		INSERT INTO outbox (local_uuid, account_id, thread_id, rfc822, state)
		VALUES (?, ?, ?, ?, 'queued')`, uuid, accountID, threadID, rfc822); err != nil {
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
	return out, rows.Err()
}

// MarkOutboxSent removes a successfully sent message from the outbox.
func (s *Store) MarkOutboxSent(ctx context.Context, id int64) error {
	if _, err := s.writer.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, id); err != nil {
		return fmt.Errorf("mark outbox sent: %w", err)
	}
	return nil
}

// MarkOutboxFailed records a failed send attempt.
func (s *Store) MarkOutboxFailed(ctx context.Context, id int64, errMsg string) error {
	if _, err := s.writer.ExecContext(ctx,
		`UPDATE outbox SET state = 'failed', attempts = attempts + 1, last_error = ? WHERE id = ?`,
		errMsg, id); err != nil {
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
