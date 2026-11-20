package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// MessagesMissingBodies pages through unfetched messages of Inbox conversations,
// most recently cached first. A conversation is in the Inbox when any of its
// messages is, and the reader shows all of them, so the older messages that left
// the Inbox (archived, filtered past it) qualify as well: opening the
// conversation would otherwise download them. A positive cutoff excludes mail
// older than that Unix time; undated mail remains eligible. beforeRowID is the
// previous page's last RowID, or 0 to start.
func (s *Store) MessagesMissingBodies(ctx context.Context, accountID, cutoff, beforeRowID int64, limit int) ([]model.Message, error) {
	start := time.Now()
	// Driven from the Inbox label index: the Inbox is small, so this reads a few
	// hundred rows where a scan of every unfetched message would read the cache.
	query := `SELECT ` + msgCols + ` FROM messages m
		WHERE m.account_id = ? AND m.body_fetched = 0
		AND m.thread_id IN (SELECT t.thread_id FROM message_labels ml
			JOIN messages t ON t.rowid = ml.message_rowid
			WHERE ml.account_id = ? AND ml.label_id = ?)`
	args := []any{accountID, accountID, model.LabelInbox}
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
	logging.TraceContext(ctx, "store: messages missing bodies", "account", accountID, "cutoff", cutoff, "before", beforeRowID, "limit", limit, "count", len(messages), "dur", time.Since(start))
	return messages, nil
}
