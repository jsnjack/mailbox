package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jsnjack/mailbox/internal/logging"
)

// SnoozeThread hides a conversation from the inbox until the given unix time.
// Snoozing again overwrites the previous wake time and resets notified, so a
// thread re-snoozed after already waking behaves like a fresh snooze.
func (s *Store) SnoozeThread(ctx context.Context, accountID int64, threadID string, until int64) error {
	logging.TraceContext(ctx, "store: snooze thread", "account", accountID, "thread", threadID, "until", until)
	if _, err := s.writer.ExecContext(ctx, `
		INSERT INTO snoozes (account_id, thread_id, until) VALUES (?,?,?)
		ON CONFLICT(account_id, thread_id) DO UPDATE SET until = excluded.until, notified = 0`,
		accountID, threadID, until); err != nil {
		return fmt.Errorf("snooze thread: %w", err)
	}
	return nil
}

// UnsnoozeThread wakes a conversation immediately (no-op if it wasn't
// snoozed), removing all trace of the snooze — used for the user's explicit
// "Unsnooze" action, unlike the background sweeper's MarkSnoozeNotified which
// keeps the row so the list can show a "Snoozed" tag.
func (s *Store) UnsnoozeThread(ctx context.Context, accountID int64, threadID string) error {
	logging.TraceContext(ctx, "store: unsnooze thread", "account", accountID, "thread", threadID)
	if _, err := s.writer.ExecContext(ctx,
		`DELETE FROM snoozes WHERE account_id = ? AND thread_id = ?`, accountID, threadID); err != nil {
		return fmt.Errorf("unsnooze thread: %w", err)
	}
	return nil
}

// MarkSnoozeNotified records that a due snooze has been woken and the user
// notified, without deleting the row: the thread stays visible to
// threadsWokeFromSnooze (its "Snoozed" tag in the list) until the user
// re-snoozes it or picks a category by hand. DueSnoozes only matches rows
// with notified = 0, so this also stops the sweeper from re-notifying the
// same wake every pass.
func (s *Store) MarkSnoozeNotified(ctx context.Context, accountID int64, threadID string) error {
	logging.TraceContext(ctx, "store: mark snooze notified", "account", accountID, "thread", threadID)
	if _, err := s.writer.ExecContext(ctx,
		`UPDATE snoozes SET notified = 1 WHERE account_id = ? AND thread_id = ?`, accountID, threadID); err != nil {
		return fmt.Errorf("mark snooze notified: %w", err)
	}
	return nil
}

// Snooze identifies one snoozed conversation.
type Snooze struct {
	AccountID int64
	ThreadID  string
	Until     int64
}

// DueSnoozes returns every not-yet-notified snooze (all accounts) whose wake
// time has passed — the background sweeper marks these notified and notifies
// the UI. A row already marked notified is excluded so a wake is announced
// exactly once, even though the row itself lingers for the "Snoozed" tag.
func (s *Store) DueSnoozes(ctx context.Context, now int64) ([]Snooze, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT account_id, thread_id, until FROM snoozes WHERE until <= ? AND notified = 0 ORDER BY until`, now)
	if err != nil {
		return nil, fmt.Errorf("due snoozes: %w", err)
	}
	return scanSnoozes(rows)
}

// SnoozedThreads returns an account's still-pending snoozed conversations,
// soonest wake first — the "Snoozed" virtual folder. A woken (notified) row
// is excluded even though it lingers in the table for the list's "Snoozed" tag.
func (s *Store) SnoozedThreads(ctx context.Context, accountID int64) ([]Snooze, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT account_id, thread_id, until FROM snoozes WHERE account_id = ? AND until > unixepoch() ORDER BY until`, accountID)
	if err != nil {
		return nil, fmt.Errorf("snoozed threads: %w", err)
	}
	return scanSnoozes(rows)
}

func scanSnoozes(rows *sql.Rows) ([]Snooze, error) {
	defer func() { _ = rows.Close() }()
	var out []Snooze
	for rows.Next() {
		var sn Snooze
		if err := rows.Scan(&sn.AccountID, &sn.ThreadID, &sn.Until); err != nil {
			return nil, fmt.Errorf("scan snooze: %w", err)
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// SnoozedCount returns how many conversations an account has still pending
// (not yet woken) — the Snoozed folder's badge. A woken row is excluded even
// though it lingers in the table for the list's "Snoozed" tag.
func (s *Store) SnoozedCount(ctx context.Context, accountID int64) (int, error) {
	var n int
	if err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM snoozes WHERE account_id = ? AND until > unixepoch()`, accountID).Scan(&n); err != nil {
		return 0, fmt.Errorf("snoozed count: %w", err)
	}
	return n, nil
}
