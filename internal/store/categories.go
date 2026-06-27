package store

import (
	"context"
	"fmt"
)

// SetMessageCategory persists the AI-assigned category for a message (keyed by
// its Gmail id). An empty category is stored deliberately — it records that the
// message was classified and matched no bucket, so it isn't re-classified.
func (s *Store) SetMessageCategory(ctx context.Context, accountID int64, gmailID, category string) error {
	_, err := s.writer.ExecContext(ctx,
		`INSERT INTO message_categories (account_id, gmail_id, category)
		 VALUES (?, ?, ?)
		 ON CONFLICT(account_id, gmail_id) DO UPDATE SET category = excluded.category`,
		accountID, gmailID, category)
	if err != nil {
		return fmt.Errorf("set message category: %w", err)
	}
	return nil
}

// ClearCategories removes all cached categories for an account, so the inbox is
// re-classified from scratch on the next categorize pass (used by the manual
// "Re-categorize inbox" action, e.g. after the category prompt changes).
func (s *Store) ClearCategories(ctx context.Context, accountID int64) error {
	if _, err := s.writer.ExecContext(ctx,
		`DELETE FROM message_categories WHERE account_id = ?`, accountID); err != nil {
		return fmt.Errorf("clear categories: %w", err)
	}
	return nil
}

// MessageCategories returns the cached categories for the given message ids, as
// a gmail_id → category map. Ids with no stored category are absent from the
// map (a present-but-empty value means "classified, no tag"). An empty input
// returns an empty map without querying.
func (s *Store) MessageCategories(ctx context.Context, accountID int64, gmailIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(gmailIDs))
	if len(gmailIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(gmailIDs)+1)
	args = append(args, accountID)
	for _, id := range gmailIDs {
		args = append(args, id)
	}
	rows, err := s.reader.QueryContext(ctx,
		`SELECT gmail_id, category FROM message_categories
		   WHERE account_id = ? AND gmail_id IN (`+placeholders(len(gmailIDs))+`)`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("query message categories: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, cat string
		if err := rows.Scan(&id, &cat); err != nil {
			return nil, err
		}
		out[id] = cat
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
