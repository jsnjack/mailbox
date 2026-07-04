package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// ListThreadsByLabel returns one summary per thread that has a message carrying
// labelID, newest first. The summary's Latest is the newest labeled message; the
// counts cover the labeled messages in that thread.
func (s *Store) ListThreadsByLabel(ctx context.Context, accountID int64, labelID string, limit, offset int) ([]model.ThreadSummary, error) {
	start := time.Now()
	logging.TraceContext(ctx, "store: list threads by label", "account", accountID, "label", labelID, "limit", limit, "offset", offset)
	// Snoozed conversations are hidden from the inbox only (their labels are
	// untouched — snooze is pure visibility); an elapsed snooze shows even
	// before the wake sweeper fires.
	snoozeFilter := ""
	if labelID == model.LabelInbox {
		snoozeFilter = ` AND NOT EXISTS (
			SELECT 1 FROM snoozes sn
			WHERE sn.account_id = m.account_id AND sn.thread_id = m.thread_id AND sn.until > unixepoch())`
	}
	// Exactly one row per thread: the labeled message with the greatest
	// (internal_date, rowid). The rowid tiebreak avoids duplicate rows when two
	// messages share a whole-second internal_date, and ordering by rowid means a
	// thread whose dates are all NULL still resolves to a single latest message.
	// The ml join binds account_id too so the planner drives from idx_msg_label
	// (account_id, label_id, …) — visiting only labeled rows — instead of scanning
	// every message of the account and probing labels per row.
	rows, err := s.reader.QueryContext(ctx, `
		SELECT `+msgCols+`
		FROM messages m
		JOIN message_labels ml ON ml.account_id = ? AND ml.message_rowid = m.rowid AND ml.label_id = ?
		WHERE m.account_id = ? AND m.rowid = (
			SELECT m2.rowid
			FROM messages m2
			JOIN message_labels ml2 ON ml2.message_rowid = m2.rowid AND ml2.label_id = ?
			WHERE m2.account_id = m.account_id AND m2.thread_id = m.thread_id
			ORDER BY m2.internal_date DESC, m2.rowid DESC
			LIMIT 1
		)`+snoozeFilter+`
		ORDER BY m.internal_date DESC, m.rowid DESC
		LIMIT ? OFFSET ?`,
		accountID, labelID, accountID, labelID, limit, offset)
	if err != nil {
		logging.TraceContext(ctx, "store: list threads by label", "account", accountID, "label", labelID, "err", err)
		return nil, fmt.Errorf("list threads: %w", err)
	}
	latest, err := scanMessagesAndClose(rows)
	if err != nil {
		return nil, err
	}

	// Count only the threads on this page (a capped LIMIT/OFFSET), not every
	// labeled message in the account. Counts stay label-scoped (labeled messages
	// per thread) — the page ids just bound the GROUP BY instead of a full-account
	// scan.
	ids := make([]string, len(latest))
	for i, m := range latest {
		ids[i] = m.ThreadID
	}
	counts, err := s.threadCountsForIDsWithLabel(ctx, accountID, labelID, ids)
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
	if err := s.markRepliedByMe(ctx, accountID, out); err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: list threads by label done", "account", accountID, "label", labelID, "count", len(out), "dur", time.Since(start))
	return out, nil
}

// ListAllThreads returns one summary per thread (its newest message), newest
// first, across all of the account's cached mail except threads whose newest
// message is in Spam or Trash. It backs the "All Mail" folder.
func (s *Store) ListAllThreads(ctx context.Context, accountID int64, limit, offset int) ([]model.ThreadSummary, error) {
	start := time.Now()
	logging.TraceContext(ctx, "store: list all threads", "account", accountID, "limit", limit, "offset", offset)
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
		logging.TraceContext(ctx, "store: list all threads", "account", accountID, "err", err)
		return nil, fmt.Errorf("list all threads: %w", err)
	}
	latest, err := scanMessagesAndClose(rows)
	if err != nil {
		return nil, err
	}

	// Count only the threads actually being shown (a capped page), not every
	// thread in the account.
	ids := make([]string, len(latest))
	for i, m := range latest {
		ids[i] = m.ThreadID
	}
	counts, err := s.threadCountsForIDs(ctx, accountID, ids)
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
	if err := s.markRepliedByMe(ctx, accountID, out); err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: list all threads done", "account", accountID, "count", len(out), "dur", time.Since(start))
	return out, nil
}

// ListThreadMessages returns every message in a thread, oldest first.
func (s *Store) ListThreadMessages(ctx context.Context, accountID int64, threadID string) ([]model.Message, error) {
	logging.TraceContext(ctx, "store: list thread messages", "account", accountID, "thread", threadID)
	rows, err := s.reader.QueryContext(ctx,
		`SELECT `+msgCols+` FROM messages m WHERE m.account_id = ? AND m.thread_id = ? ORDER BY m.internal_date`,
		accountID, threadID)
	if err != nil {
		logging.TraceContext(ctx, "store: list thread messages", "account", accountID, "thread", threadID, "err", err)
		return nil, fmt.Errorf("list thread messages: %w", err)
	}
	out, err := scanMessagesAndClose(rows)
	if err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: list thread messages done", "account", accountID, "thread", threadID, "count", len(out))
	return out, nil
}

// ThreadLabels returns the set of label ids applied to any message in the
// thread — used to reflect which labels are currently on a conversation.
func (s *Store) ThreadLabels(ctx context.Context, accountID int64, threadID string) (map[string]bool, error) {
	logging.TraceContext(ctx, "store: thread labels", "account", accountID, "thread", threadID)
	rows, err := s.reader.QueryContext(ctx, `
		SELECT DISTINCT ml.label_id
		FROM message_labels ml
		JOIN messages m ON m.rowid = ml.message_rowid
		WHERE m.account_id = ? AND m.thread_id = ?`, accountID, threadID)
	if err != nil {
		logging.TraceContext(ctx, "store: thread labels", "account", accountID, "thread", threadID, "err", err)
		return nil, fmt.Errorf("thread labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan thread label: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: thread labels done", "account", accountID, "thread", threadID, "count", len(out))
	return out, nil
}

// GetThreadSummary returns the summary for a single thread (all messages, not
// label-scoped) — used to present search hits as threads.
func (s *Store) GetThreadSummary(ctx context.Context, accountID int64, threadID string) (model.ThreadSummary, error) {
	logging.TraceContext(ctx, "store: get thread summary", "account", accountID, "thread", threadID)
	row := s.reader.QueryRowContext(ctx,
		`SELECT `+msgCols+` FROM messages m WHERE m.account_id = ? AND m.thread_id = ? ORDER BY m.internal_date DESC LIMIT 1`,
		accountID, threadID)
	latest, err := scanMessage(row)
	if err != nil {
		logging.TraceContext(ctx, "store: get thread summary", "account", accountID, "thread", threadID, "err", err)
		return model.ThreadSummary{}, fmt.Errorf("thread summary %q: %w", threadID, err)
	}
	var total, unread int
	if err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(is_unread),0) FROM messages WHERE account_id = ? AND thread_id = ?`,
		accountID, threadID).Scan(&total, &unread); err != nil {
		logging.TraceContext(ctx, "store: get thread summary", "account", accountID, "thread", threadID, "err", err)
		return model.ThreadSummary{}, fmt.Errorf("thread counts %q: %w", threadID, err)
	}
	logging.TraceContext(ctx, "store: get thread summary done", "account", accountID, "thread", threadID, "count", total, "unread", unread)
	return model.ThreadSummary{ThreadID: threadID, Latest: latest, Count: total, UnreadCount: unread}, nil
}

// GetThreadSummaries returns a summary (latest message + total/unread counts)
// for each given thread id, in the same order, skipping ids with no cached
// message and de-duplicating repeats. It batches the latest-message and count
// lookups instead of the two-per-thread that looping GetThreadSummary would —
// turning a search of N hit threads from 2N round-trips into a small constant.
func (s *Store) GetThreadSummaries(ctx context.Context, accountID int64, threadIDs []string) ([]model.ThreadSummary, error) {
	if len(threadIDs) == 0 {
		return nil, nil
	}
	start := time.Now()
	logging.TraceContext(ctx, "store: get thread summaries", "account", accountID, "n", len(threadIDs))
	latest := make(map[string]model.Message, len(threadIDs))

	// Chunk the IN-list to stay well under SQLite's bound-variable ceiling.
	const chunk = 500
	for start := 0; start < len(threadIDs); start += chunk {
		end := start + chunk
		if end > len(threadIDs) {
			end = len(threadIDs)
		}
		ids := threadIDs[start:end]
		args := make([]any, 0, len(ids)+1)
		args = append(args, accountID)
		for _, id := range ids {
			args = append(args, id)
		}
		rows, err := s.reader.QueryContext(ctx, `
			SELECT `+msgCols+`
			FROM messages m
			WHERE m.account_id = ? AND m.thread_id IN (`+placeholders(len(ids))+`) AND m.rowid = (
				SELECT m2.rowid FROM messages m2
				WHERE m2.account_id = m.account_id AND m2.thread_id = m.thread_id
				ORDER BY m2.internal_date DESC, m2.rowid DESC
				LIMIT 1
			)`, args...)
		if err != nil {
			logging.TraceContext(ctx, "store: get thread summaries", "account", accountID, "err", err)
			return nil, fmt.Errorf("thread summaries (latest): %w", err)
		}
		msgs, err := scanMessagesAndClose(rows)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			latest[m.ThreadID] = m
		}
	}

	counts, err := s.threadCountsForIDs(ctx, accountID, threadIDs)
	if err != nil {
		return nil, err
	}

	out := make([]model.ThreadSummary, 0, len(threadIDs))
	seen := make(map[string]bool, len(threadIDs))
	for _, id := range threadIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		m, ok := latest[id]
		if !ok {
			continue // no cached message for this thread
		}
		c := counts[id]
		out = append(out, model.ThreadSummary{ThreadID: id, Latest: m, Count: c.total, UnreadCount: c.unread})
	}
	if err := s.markRepliedByMe(ctx, accountID, out); err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: get thread summaries done", "account", accountID, "n", len(threadIDs), "count", len(out), "dur", time.Since(start))
	return out, nil
}

// placeholders returns "?,?,…" with n marks for a parameterized IN-list.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

type threadCount struct{ total, unread int }

// scanThreadCountsInto scans (thread_id, total, unread) rows into out and closes
// the rows. It is shared by the per-label, all-mail, and by-id count queries.
func scanThreadCountsInto(rows *sql.Rows, out map[string]threadCount) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var tid string
		var c threadCount
		if err := rows.Scan(&tid, &c.total, &c.unread); err != nil {
			return fmt.Errorf("scan thread count: %w", err)
		}
		out[tid] = c
	}
	return rows.Err()
}

// threadCountsForIDs returns per-thread total/unread message counts for the
// given thread ids (all messages, not label-scoped), chunked to stay under the
// bind-variable ceiling. Scoping to the displayed ids avoids a full-account scan
// when the caller only shows a capped page of threads.
func (s *Store) threadCountsForIDs(ctx context.Context, accountID int64, ids []string) (map[string]threadCount, error) {
	out := make(map[string]threadCount, len(ids))
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		args := make([]any, 0, len(batch)+1)
		args = append(args, accountID)
		for _, id := range batch {
			args = append(args, id)
		}
		rows, err := s.reader.QueryContext(ctx, `
			SELECT thread_id, COUNT(*), COALESCE(SUM(is_unread),0)
			FROM messages WHERE account_id = ? AND thread_id IN (`+placeholders(len(batch))+`)
			GROUP BY thread_id`, args...)
		if err != nil {
			return nil, fmt.Errorf("thread counts for ids: %w", err)
		}
		if err := scanThreadCountsInto(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// markRepliedByMe sets RepliedByMe on each summary whose thread's most recent
// message was sent by this account (its newest message carries SENT).
func (s *Store) markRepliedByMe(ctx context.Context, accountID int64, sums []model.ThreadSummary) error {
	if len(sums) == 0 {
		return nil
	}
	ids := make([]string, len(sums))
	for i := range sums {
		ids[i] = sums[i].ThreadID
	}
	replied, err := s.threadsRepliedByMe(ctx, accountID, ids)
	if err != nil {
		return err
	}
	for i := range sums {
		sums[i].RepliedByMe = replied[sums[i].ThreadID]
	}
	return nil
}

// threadsRepliedByMe returns the subset of threadIDs whose most recent message
// (any label) carries the SENT label — i.e. this account had the last word.
func (s *Store) threadsRepliedByMe(ctx context.Context, accountID int64, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		args := make([]any, 0, len(batch)+2)
		args = append(args, model.LabelSent, accountID)
		for _, id := range batch {
			args = append(args, id)
		}
		rows, err := s.reader.QueryContext(ctx, `
			SELECT m.thread_id
			FROM messages m
			JOIN message_labels ml ON ml.message_rowid = m.rowid AND ml.label_id = ?
			WHERE m.account_id = ? AND m.thread_id IN (`+placeholders(len(batch))+`)
			  AND m.rowid = (
				SELECT m2.rowid FROM messages m2
				WHERE m2.account_id = m.account_id AND m2.thread_id = m.thread_id
				ORDER BY m2.internal_date DESC, m2.rowid DESC LIMIT 1
			  )`, args...)
		if err != nil {
			return nil, fmt.Errorf("threads replied-by-me: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var tid string
				if err := rows.Scan(&tid); err != nil {
					return err
				}
				out[tid] = true
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// threadCountsForIDsWithLabel returns per-thread total/unread counts of the
// messages carrying labelID, restricted to the given thread ids (a page), chunked
// to stay under the bind-variable ceiling. Like threadCountsForIDs but label-
// scoped: it counts only the labeled messages in each thread, so the label-view
// counts match what the pre-page-scoped threadCounts returned — just without the
// full-account GROUP BY.
func (s *Store) threadCountsForIDsWithLabel(ctx context.Context, accountID int64, labelID string, ids []string) (map[string]threadCount, error) {
	out := make(map[string]threadCount, len(ids))
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		args := make([]any, 0, len(batch)+2)
		args = append(args, labelID, accountID)
		for _, id := range batch {
			args = append(args, id)
		}
		rows, err := s.reader.QueryContext(ctx, `
			SELECT m.thread_id, COUNT(*), COALESCE(SUM(m.is_unread),0)
			FROM messages m
			JOIN message_labels ml ON ml.message_rowid = m.rowid AND ml.label_id = ?
			WHERE m.account_id = ? AND m.thread_id IN (`+placeholders(len(batch))+`)
			GROUP BY m.thread_id`, args...)
		if err != nil {
			return nil, fmt.Errorf("thread counts for ids (labeled): %w", err)
		}
		if err := scanThreadCountsInto(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}
