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
	// Exactly one row per thread: the labeled message with the greatest
	// (internal_date, rowid). The rowid tiebreak avoids duplicate rows when two
	// messages share a whole-second internal_date, and ordering by rowid means a
	// thread whose dates are all NULL still resolves to a single latest message.
	rows, err := s.reader.QueryContext(ctx, `
		SELECT `+msgCols+`
		FROM messages m
		JOIN message_labels ml ON ml.message_rowid = m.rowid AND ml.label_id = ?
		WHERE m.account_id = ? AND m.rowid = (
			SELECT m2.rowid
			FROM messages m2
			JOIN message_labels ml2 ON ml2.message_rowid = m2.rowid AND ml2.label_id = ?
			WHERE m2.account_id = m.account_id AND m2.thread_id = m.thread_id
			ORDER BY m2.internal_date DESC, m2.rowid DESC
			LIMIT 1
		)
		ORDER BY m.internal_date DESC, m.rowid DESC
		LIMIT ? OFFSET ?`,
		labelID, accountID, labelID, limit, offset)
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

// ListAllThreads returns one summary per thread (its newest message), newest
// first, across all of the account's cached mail except threads whose newest
// message is in Spam or Trash. It backs the "All Mail" folder.
func (s *Store) ListAllThreads(ctx context.Context, accountID int64, limit, offset int) ([]model.ThreadSummary, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT `+msgCols+`
		FROM messages m
		WHERE m.account_id = ? AND m.rowid = (
			SELECT m2.rowid
			FROM messages m2
			WHERE m2.account_id = m.account_id AND m2.thread_id = m.thread_id
			ORDER BY m2.internal_date DESC, m2.rowid DESC
			LIMIT 1
		)
		AND NOT EXISTS (
			SELECT 1 FROM message_labels mx
			WHERE mx.message_rowid = m.rowid AND mx.label_id IN (?, ?)
		)
		ORDER BY m.internal_date DESC, m.rowid DESC
		LIMIT ? OFFSET ?`,
		accountID, model.LabelSpam, model.LabelTrash, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list all threads: %w", err)
	}
	latest, err := scanMessagesAndClose(rows)
	if err != nil {
		return nil, err
	}

	counts, err := s.threadCountsAll(ctx, accountID)
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

// threadCountsAll returns per-thread total/unread message counts across the
// whole account (not label-scoped) — used by the "All Mail" folder.
func (s *Store) threadCountsAll(ctx context.Context, accountID int64) (map[string]threadCount, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT thread_id, COUNT(*), COALESCE(SUM(is_unread),0)
		FROM messages WHERE account_id = ?
		GROUP BY thread_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("thread counts all: %w", err)
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
