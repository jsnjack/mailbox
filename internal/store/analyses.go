package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jsnjack/mailbox/internal/logging"
)

// SetAnalysis persists the AI security analysis (verdict + reasons) for a
// message. The message and its auth/heuristic signals are immutable, so this is
// keyed by the message's Gmail id and never needs invalidation.
func (s *Store) SetAnalysis(ctx context.Context, accountID int64, gmailID, analysis string) error {
	logging.TraceContext(ctx, "store: set analysis", "account", accountID, "id", gmailID, "bytes", len(analysis))
	_, err := s.writer.ExecContext(ctx,
		`INSERT INTO message_analyses (account_id, gmail_id, analysis)
		 VALUES (?, ?, ?)
		 ON CONFLICT(account_id, gmail_id) DO UPDATE SET analysis = excluded.analysis`,
		accountID, gmailID, analysis)
	if err != nil {
		logging.TraceContext(ctx, "store: set analysis", "account", accountID, "id", gmailID, "err", err)
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
		logging.TraceContext(ctx, "store: analysis", "account", accountID, "id", gmailID, "hit", false)
		return "", false, nil
	case scanErr != nil:
		logging.TraceContext(ctx, "store: analysis", "account", accountID, "id", gmailID, "err", scanErr)
		return "", false, fmt.Errorf("query analysis: %w", scanErr)
	}
	logging.TraceContext(ctx, "store: analysis", "account", accountID, "id", gmailID, "hit", true, "bytes", len(analysis))
	return analysis, true, nil
}
