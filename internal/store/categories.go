package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
)

// SetMessageCategory persists the AI-assigned category for a message (keyed by
// its Gmail id), marking it status='ok'. An empty category is stored
// deliberately — it records that the message was classified and matched no
// bucket, so it isn't re-classified. Overwrites any prior 'failed' status: a
// successful classification always heals a previous failed attempt.
func (s *Store) SetMessageCategory(ctx context.Context, accountID int64, gmailID, category string) error {
	logging.TraceContext(ctx, "store: set message category", "account", accountID, "id", gmailID, "category", category)
	_, err := s.writer.ExecContext(ctx,
		`INSERT INTO message_categories (account_id, gmail_id, category, status)
		 VALUES (?, ?, ?, 'ok')
		 ON CONFLICT(account_id, gmail_id) DO UPDATE SET category = excluded.category, status = 'ok'`,
		accountID, gmailID, category)
	if err != nil {
		logging.TraceContext(ctx, "store: set message category", "account", accountID, "id", gmailID, "err", err)
		return fmt.Errorf("set message category: %w", err)
	}
	return nil
}

// SetMessageCategoryFailed records that classifying this message errored (an
// AI request/network/parse failure), as opposed to SetMessageCategory's
// "tried, no match" (empty category with status='ok'). The row keeps
// category=” but status='failed', so MessageCategories excludes it from
// "already done" — the thread remains an AI retry candidate — while the UI can
// still show a distinct "categorization failed" indicator instead of it
// silently looking identical to "no category". Never downgrades an existing
// 'ok' row (a settled classification always wins over a later failed retry of
// something else touching the same message, which shouldn't happen but is
// cheap to guard).
func (s *Store) SetMessageCategoryFailed(ctx context.Context, accountID int64, gmailID string) error {
	logging.TraceContext(ctx, "store: set message category failed", "account", accountID, "id", gmailID)
	_, err := s.writer.ExecContext(ctx,
		`INSERT INTO message_categories (account_id, gmail_id, category, status)
		 VALUES (?, ?, '', 'failed')
		 ON CONFLICT(account_id, gmail_id) DO UPDATE SET status = 'failed'
		 WHERE message_categories.status != 'ok'`,
		accountID, gmailID)
	if err != nil {
		logging.TraceContext(ctx, "store: set message category failed", "account", accountID, "id", gmailID, "err", err)
		return fmt.Errorf("set message category failed: %w", err)
	}
	return nil
}

// SetManualCategory persists a category the user picked by hand, marking it
// manual so it outranks the automatic "Replied" tag in the list and survives a
// restart. Like SetMessageCategory but sets manual = 1.
func (s *Store) SetManualCategory(ctx context.Context, accountID int64, gmailID, category string) error {
	logging.TraceContext(ctx, "store: set manual category", "account", accountID, "id", gmailID, "category", category)
	_, err := s.writer.ExecContext(ctx,
		`INSERT INTO message_categories (account_id, gmail_id, category, manual, status)
		 VALUES (?, ?, ?, 1, 'ok')
		 ON CONFLICT(account_id, gmail_id) DO UPDATE SET category = excluded.category, manual = 1, status = 'ok'`,
		accountID, gmailID, category)
	if err != nil {
		logging.TraceContext(ctx, "store: set manual category", "account", accountID, "id", gmailID, "err", err)
		return fmt.Errorf("set manual category: %w", err)
	}
	return nil
}

// ManualCategoryIDs returns, for the given message ids, the set of ids whose
// category the user set manually (manual = 1). Non-manual and absent ids are not
// in the map. An empty input returns an empty map without querying.
func (s *Store) ManualCategoryIDs(ctx context.Context, accountID int64, gmailIDs []string) (map[string]bool, error) {
	logging.TraceContext(ctx, "store: manual category ids", "account", accountID, "n", len(gmailIDs))
	out := make(map[string]bool, len(gmailIDs))
	const chunk = 500
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
			`SELECT gmail_id FROM message_categories
			   WHERE account_id = ? AND manual = 1 AND gmail_id IN (`+placeholders(len(ids))+`)`,
			args...)
		if err != nil {
			return nil, fmt.Errorf("query manual category ids: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				out[id] = true
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	logging.TraceContext(ctx, "store: manual category ids done", "account", accountID, "count", len(out))
	return out, nil
}

// FailedCategoryIDs returns, for the given message ids, the set of ids whose
// last categorization attempt errored (status = 'failed') rather than settling
// with a real category or a deliberate "no tag". These ids are absent from
// MessageCategories and remain AI retry candidates; this lets the UI show a
// distinct "categorization failed" indicator instead of it looking identical
// to "no category" while retries are pending. An empty input returns an empty
// map without querying.
func (s *Store) FailedCategoryIDs(ctx context.Context, accountID int64, gmailIDs []string) (map[string]bool, error) {
	logging.TraceContext(ctx, "store: failed category ids", "account", accountID, "n", len(gmailIDs))
	out := make(map[string]bool, len(gmailIDs))
	const chunk = 500
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
			`SELECT gmail_id FROM message_categories
			   WHERE account_id = ? AND status = 'failed' AND gmail_id IN (`+placeholders(len(ids))+`)`,
			args...)
		if err != nil {
			return nil, fmt.Errorf("query failed category ids: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				out[id] = true
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	logging.TraceContext(ctx, "store: failed category ids done", "account", accountID, "count", len(out))
	return out, nil
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

// MessageCategories returns the settled (status='ok') categories for the given
// message ids, as a gmail_id → category map. Ids with no stored category, and
// ids whose only row is status='failed', are absent from the map (a
// present-but-empty value means "classified, no tag"); both cases keep the
// message a candidate for another AI attempt — see FailedCategoryIDs to tell
// them apart. An empty input returns an empty map without querying.
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
			   WHERE account_id = ? AND status = 'ok' AND gmail_id IN (`+placeholders(len(ids))+`)`,
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
