package store

import (
	"context"
	"fmt"

	"github.com/jsnjack/mailbox/internal/model"
)

// ListThreadsByLabel returns one summary per thread that has a message carrying
// labelID, newest first. The summary's Latest is the newest labeled message; the
// counts cover the labeled messages in that thread.
func (s *Store) ListThreadsByLabel(ctx context.Context, accountID int64, labelID string, limit, offset int) ([]model.ThreadSummary, error) {
	// Latest labeled message per thread (the MAX(internal_date) row).
	rows, err := s.reader.QueryContext(ctx, `
		SELECT `+msgCols+`
		FROM messages m
		JOIN message_labels ml ON ml.message_rowid = m.rowid AND ml.label_id = ?
		JOIN (
			SELECT m2.thread_id AS tid, MAX(m2.internal_date) AS maxd
			FROM messages m2
			JOIN message_labels ml2 ON ml2.message_rowid = m2.rowid AND ml2.label_id = ?
			WHERE m2.account_id = ?
			GROUP BY m2.thread_id
		) agg ON agg.tid = m.thread_id AND agg.maxd = m.internal_date
		WHERE m.account_id = ?
		ORDER BY m.internal_date DESC
		LIMIT ? OFFSET ?`,
		labelID, labelID, accountID, accountID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	latest, err := scanMessagesAndClose(rows)
	if err != nil {
		return nil, err
	}

	counts, err := s.threadCounts(ctx, accountID, labelID)
	if err != nil {
		return nil, err
	}

	out := make([]model.ThreadSummary, 0, len(latest))
	for _, m := range latest {
		c := counts[m.ThreadID]
		out = append(out, model.ThreadSummary{
			ThreadID: m.ThreadID, Latest: m, Count: c.total, UnreadCount: c.unread,
		})
	}
	return out, nil
}

// ListThreadMessages returns every message in a thread, oldest first.
func (s *Store) ListThreadMessages(ctx context.Context, accountID int64, threadID string) ([]model.Message, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT `+msgCols+` FROM messages m WHERE m.account_id = ? AND m.thread_id = ? ORDER BY m.internal_date`,
		accountID, threadID)
	if err != nil {
		return nil, fmt.Errorf("list thread messages: %w", err)
	}
	return scanMessagesAndClose(rows)
}

// GetThreadSummary returns the summary for a single thread (all messages, not
// label-scoped) — used to present search hits as threads.
func (s *Store) GetThreadSummary(ctx context.Context, accountID int64, threadID string) (model.ThreadSummary, error) {
	row := s.reader.QueryRowContext(ctx,
		`SELECT `+msgCols+` FROM messages m WHERE m.account_id = ? AND m.thread_id = ? ORDER BY m.internal_date DESC LIMIT 1`,
		accountID, threadID)
	latest, err := scanMessage(row)
	if err != nil {
		return model.ThreadSummary{}, fmt.Errorf("thread summary %q: %w", threadID, err)
	}
	var total, unread int
	if err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(is_unread),0) FROM messages WHERE account_id = ? AND thread_id = ?`,
		accountID, threadID).Scan(&total, &unread); err != nil {
		return model.ThreadSummary{}, fmt.Errorf("thread counts %q: %w", threadID, err)
	}
	return model.ThreadSummary{ThreadID: threadID, Latest: latest, Count: total, UnreadCount: unread}, nil
}

type threadCount struct{ total, unread int }

func (s *Store) threadCounts(ctx context.Context, accountID int64, labelID string) (map[string]threadCount, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT m.thread_id, COUNT(*), COALESCE(SUM(m.is_unread),0)
		FROM messages m
		JOIN message_labels ml ON ml.message_rowid = m.rowid AND ml.label_id = ?
		WHERE m.account_id = ?
		GROUP BY m.thread_id`, labelID, accountID)
	if err != nil {
		return nil, fmt.Errorf("thread counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]threadCount)
	for rows.Next() {
		var tid string
		var c threadCount
		if err := rows.Scan(&tid, &c.total, &c.unread); err != nil {
			return nil, fmt.Errorf("scan thread count: %w", err)
		}
		out[tid] = c
	}
	return out, rows.Err()
}
