package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
)

// SetMessageCategory persists the AI-assigned category for a message (keyed by
// its Gmail id). An empty category is stored deliberately — it records that the
// message was classified and matched no bucket, so it isn't re-classified.
func (s *Store) SetMessageCategory(ctx context.Context, accountID int64, gmailID, category string) error {
	logging.TraceContext(ctx, "store: set message category", "account", accountID, "id", gmailID, "category", category)
	_, err := s.writer.ExecContext(ctx,
		`INSERT INTO message_categories (account_id, gmail_id, category)
		 VALUES (?, ?, ?)
		 ON CONFLICT(account_id, gmail_id) DO UPDATE SET category = excluded.category`,
		accountID, gmailID, category)
	if err != nil {
		logging.TraceContext(ctx, "store: set message category", "account", accountID, "id", gmailID, "err", err)
		return fmt.Errorf("set message category: %w", err)
	}
	return nil
}

// ClearMessageCategory removes the cached category for a single message, so its
// thread is re-classified on the next pass (used by the per-conversation
// "Re-categorize" action).
func (s *Store) ClearMessageCategory(ctx context.Context, accountID int64, gmailID string) error {
	logging.TraceContext(ctx, "store: clear message category", "account", accountID, "id", gmailID)
	if _, err := s.writer.ExecContext(ctx,
		`DELETE FROM message_categories WHERE account_id = ? AND gmail_id = ?`,
		accountID, gmailID); err != nil {
		logging.TraceContext(ctx, "store: clear message category", "account", accountID, "id", gmailID, "err", err)
		return fmt.Errorf("clear message category: %w", err)
	}
	return nil
}

// ClearCategories removes all cached categories for an account, so the inbox is
// re-classified from scratch on the next categorize pass (used by the manual
// "Re-categorize inbox" action, e.g. after the category prompt changes).
func (s *Store) ClearCategories(ctx context.Context, accountID int64) error {
	logging.TraceContext(ctx, "store: clear categories", "account", accountID)
	res, err := s.writer.ExecContext(ctx,
		`DELETE FROM message_categories WHERE account_id = ?`, accountID)
	if err != nil {
		logging.TraceContext(ctx, "store: clear categories", "account", accountID, "err", err)
		return fmt.Errorf("clear categories: %w", err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil {
		logging.TraceContext(ctx, "store: clear categories done", "account", accountID, "rows", n)
	}
	return nil
}

// MessageCategories returns the cached categories for the given message ids, as
// a gmail_id → category map. Ids with no stored category are absent from the
// map (a present-but-empty value means "classified, no tag"). An empty input
// returns an empty map without querying.
func (s *Store) MessageCategories(ctx context.Context, accountID int64, gmailIDs []string) (map[string]string, error) {
	start := time.Now()
	logging.TraceContext(ctx, "store: message categories", "account", accountID, "n", len(gmailIDs))
	out := make(map[string]string, len(gmailIDs))
	const chunk = 500 // stay well under SQLite's bound-variable limit
	for start := 0; start < len(gmailIDs); start += chunk {
		end := start + chunk
		if end > len(gmailIDs) {
			end = len(gmailIDs)
		}
		ids := gmailIDs[start:end]
		args := make([]any, 0, len(ids)+1)
		args = append(args, accountID)
		for _, id := range ids {
			args = append(args, id)
		}
		rows, err := s.reader.QueryContext(ctx,
			`SELECT gmail_id, category FROM message_categories
			   WHERE account_id = ? AND gmail_id IN (`+placeholders(len(ids))+`)`,
			args...)
		if err != nil {
			return nil, fmt.Errorf("query message categories: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id, cat string
				if err := rows.Scan(&id, &cat); err != nil {
					return err
				}
				out[id] = cat
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	logging.TraceContext(ctx, "store: message categories done", "account", accountID, "n", len(gmailIDs), "count", len(out), "dur", time.Since(start))
	return out, nil
}
