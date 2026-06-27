package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SetAnalysis persists the AI security analysis (verdict + reasons) for a
// message. The message and its auth/heuristic signals are immutable, so this is
// keyed by the message's Gmail id and never needs invalidation.
func (s *Store) SetAnalysis(ctx context.Context, accountID int64, gmailID, analysis string) error {
	_, err := s.writer.ExecContext(ctx,
		`INSERT INTO message_analyses (account_id, gmail_id, analysis)
		 VALUES (?, ?, ?)
		 ON CONFLICT(account_id, gmail_id) DO UPDATE SET analysis = excluded.analysis`,
		accountID, gmailID, analysis)
	if err != nil {
		return fmt.Errorf("set analysis: %w", err)
	}
	return nil
}

// Analysis returns the cached security analysis for a message; ok is false when
// none is stored.
func (s *Store) Analysis(ctx context.Context, accountID int64, gmailID string) (analysis string, ok bool, err error) {
	row := s.reader.QueryRowContext(ctx,
		`SELECT analysis FROM message_analyses WHERE account_id = ? AND gmail_id = ?`,
		accountID, gmailID)
	switch scanErr := row.Scan(&analysis); {
	case errors.Is(scanErr, sql.ErrNoRows):
		return "", false, nil
	case scanErr != nil:
		return "", false, fmt.Errorf("query analysis: %w", scanErr)
	}
	return analysis, true, nil
}
