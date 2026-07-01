package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jsnjack/mailbox/internal/logging"
)

// SetThreadSummary persists a thread's AI summary together with the fingerprint
// it was computed for (the thread's message-id set). A later fingerprint
// mismatch tells the caller the thread changed and the summary is stale.
func (s *Store) SetThreadSummary(ctx context.Context, accountID int64, threadID, fingerprint, summary string) error {
	logging.TraceContext(ctx, "store: set thread summary", "account", accountID, "id", threadID, "fingerprint", fingerprint, "bytes", len(summary))
	_, err := s.writer.ExecContext(ctx,
		`INSERT INTO thread_summaries (account_id, thread_id, fingerprint, summary)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(account_id, thread_id) DO UPDATE SET
		   fingerprint = excluded.fingerprint, summary = excluded.summary`,
		accountID, threadID, fingerprint, summary)
	if err != nil {
		logging.TraceContext(ctx, "store: set thread summary", "account", accountID, "id", threadID, "err", err)
		return fmt.Errorf("set thread summary: %w", err)
	}
	return nil
}

// ThreadSummary returns the cached summary for a thread and the fingerprint it
// was computed for; ok is false when none is stored. The caller compares the
// fingerprint to the thread's current message set to decide whether it is still
// valid.
func (s *Store) ThreadSummary(ctx context.Context, accountID int64, threadID string) (fingerprint, summary string, ok bool, err error) {
	row := s.reader.QueryRowContext(ctx,
		`SELECT fingerprint, summary FROM thread_summaries WHERE account_id = ? AND thread_id = ?`,
		accountID, threadID)
	switch scanErr := row.Scan(&fingerprint, &summary); {
	case errors.Is(scanErr, sql.ErrNoRows):
		logging.TraceContext(ctx, "store: thread summary", "account", accountID, "id", threadID, "hit", false)
		return "", "", false, nil
	case scanErr != nil:
		logging.TraceContext(ctx, "store: thread summary", "account", accountID, "id", threadID, "err", scanErr)
		return "", "", false, fmt.Errorf("query thread summary: %w", scanErr)
	}
	logging.TraceContext(ctx, "store: thread summary", "account", accountID, "id", threadID, "hit", true, "fingerprint", fingerprint)
	return fingerprint, summary, true, nil
}
