package store

import (
	"context"
	"fmt"

	"github.com/jsnjack/mailbox/internal/model"
)

// MessagesMissingBodies pages through unfetched Inbox messages, most recently cached
// first. A positive cutoff excludes mail older than that Unix time; undated mail
// remains eligible. beforeRowID is the previous page's last RowID, or 0 to start.
func (s *Store) MessagesMissingBodies(ctx context.Context, accountID, cutoff, beforeRowID int64, limit int) ([]model.Message, error) {
	query := `SELECT ` + msgCols + ` FROM messages m
		WHERE m.account_id = ? AND m.body_fetched = 0
		AND EXISTS (SELECT 1 FROM message_labels ml
			WHERE ml.message_rowid = m.rowid AND ml.label_id = ?)`
	args := []any{accountID, model.LabelInbox}
	if cutoff > 0 {
		query += ` AND (m.internal_date IS NULL OR m.internal_date >= ?)`
		args = append(args, cutoff)
	}
	if beforeRowID > 0 {
		query += ` AND m.rowid < ?`
		args = append(args, beforeRowID)
	}
	query += ` ORDER BY m.rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query missing message bodies: %w", err)
	}
	messages, err := scanMessagesAndClose(rows)
	if err != nil {
		return nil, fmt.Errorf("scan missing message bodies: %w", err)
	}
	return messages, nil
}
