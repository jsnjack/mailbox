package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ThreadHydrated reports whether the complete provider-side membership of a
// conversation has already been cached.
func (s *Store) ThreadHydrated(ctx context.Context, accountID int64, threadID string) (bool, error) {
	var one int
	err := s.reader.QueryRowContext(ctx,
		`SELECT 1 FROM thread_hydrations WHERE account_id = ? AND thread_id = ?`,
		accountID, threadID).Scan(&one)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, fmt.Errorf("query thread hydration: %w", err)
}

// MarkThreadHydrated records that a conversation's complete provider-side
// membership has been cached.
func (s *Store) MarkThreadHydrated(ctx context.Context, accountID int64, threadID string) error {
	if _, err := s.writer.ExecContext(ctx,
		`INSERT OR IGNORE INTO thread_hydrations (account_id, thread_id) VALUES (?, ?)`,
		accountID, threadID); err != nil {
		return fmt.Errorf("mark thread hydrated: %w", err)
	}
	return nil
}
