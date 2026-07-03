package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
)

// pruneChunk bounds how many messages one pruning transaction touches, keeping
// the write-lock hold time and the IN-list parameter count reasonable.
const pruneChunk = 500

// PruneBodies clears the cached bodies of messages older than cutoff (unix
// seconds) across all accounts, returning how many were pruned. Metadata is
// untouched — the list, threading, and header search keep working — and
// body_fetched is reset so opening a pruned message lazily re-fetches it from
// the provider (which keeps the authoritative copy). Everything derived from
// the body goes with it: the FTS body text (the row is rebuilt so header
// search still matches), extracted attachment rows (re-extracted on re-fetch),
// and the persisted per-message AI translations and phishing analyses.
func (s *Store) PruneBodies(ctx context.Context, cutoff int64) (int, error) {
	start := time.Now()
	logging.TraceContext(ctx, "store: prune bodies", "cutoff", cutoff)
	total := 0
	for {
		// Each pass claims one chunk; body_fetched = 0 afterwards keeps a row
		// from ever being selected twice, so this terminates.
		rows, err := s.reader.QueryContext(ctx, `
			SELECT rowid FROM messages
			WHERE internal_date IS NOT NULL AND internal_date < ? AND body_fetched != 0
			LIMIT ?`, cutoff, pruneChunk)
		if err != nil {
			return total, fmt.Errorf("prune bodies: select: %w", err)
		}
		ids, err := scanRowIDs(rows)
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			break
		}
		if err := s.pruneBodiesChunk(ctx, ids); err != nil {
			return total, err
		}
		total += len(ids)
	}
	logging.TraceContext(ctx, "store: prune bodies done", "count", total, "dur", time.Since(start))
	return total, nil
}

// pruneBodiesChunk clears one chunk of message bodies in a single transaction.
func (s *Store) pruneBodiesChunk(ctx context.Context, rowIDs []int64) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("prune bodies: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	marks := placeholders(len(rowIDs))
	args := make([]any, len(rowIDs))
	for i, id := range rowIDs {
		args[i] = id
	}
	// AI artifacts are keyed by (account_id, gmail_id); resolve via the rows
	// being pruned. Deleted before the body so the subquery and the intent
	// stay in one place.
	for _, stmt := range []string{
		`DELETE FROM message_translations WHERE (account_id, gmail_id) IN
			(SELECT account_id, gmail_id FROM messages WHERE rowid IN (` + marks + `))`,
		`DELETE FROM message_analyses WHERE (account_id, gmail_id) IN
			(SELECT account_id, gmail_id FROM messages WHERE rowid IN (` + marks + `))`,
		`DELETE FROM attachments WHERE message_rowid IN (` + marks + `)`,
		`DELETE FROM message_bodies WHERE message_rowid IN (` + marks + `)`,
		`UPDATE messages SET body_fetched = 0 WHERE rowid IN (` + marks + `)`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return fmt.Errorf("prune bodies: %w", err)
		}
	}
	// Rebuild each FTS row from the now-bodyless state so subject/sender/snippet
	// search keeps matching while the body text stops.
	for _, id := range rowIDs {
		if err := reindexFTS(ctx, tx, id); err != nil {
			return fmt.Errorf("prune bodies: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("prune bodies: commit: %w", err)
	}
	logging.TraceContext(ctx, "store: prune bodies chunk", "count", len(rowIDs))
	return nil
}

// scanRowIDs collects an integer rowid column and closes the rows.
func scanRowIDs(rows *sql.Rows) ([]int64, error) {
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan rowid: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
