package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jsnjack/mailbox/internal/logging"
)

// SnoozeThread hides a conversation from the inbox until the given unix time.
// Snoozing again overwrites the previous wake time.
func (s *Store) SnoozeThread(ctx context.Context, accountID int64, threadID string, until int64) error {
	logging.TraceContext(ctx, "store: snooze thread", "account", accountID, "thread", threadID, "until", until)
	if _, err := s.writer.ExecContext(ctx, `
		INSERT INTO snoozes (account_id, thread_id, until) VALUES (?,?,?)
		ON CONFLICT(account_id, thread_id) DO UPDATE SET until = excluded.until`,
		accountID, threadID, until); err != nil {
		return fmt.Errorf("snooze thread: %w", err)
	}
	return nil
}

// UnsnoozeThread wakes a conversation (no-op if it wasn't snoozed).
func (s *Store) UnsnoozeThread(ctx context.Context, accountID int64, threadID string) error {
	logging.TraceContext(ctx, "store: unsnooze thread", "account", accountID, "thread", threadID)
	if _, err := s.writer.ExecContext(ctx,
		`DELETE FROM snoozes WHERE account_id = ? AND thread_id = ?`, accountID, threadID); err != nil {
		return fmt.Errorf("unsnooze thread: %w", err)
	}
	return nil
}

// Snooze identifies one snoozed conversation.
type Snooze struct {
	AccountID int64
	ThreadID  string
	Until     int64
}

// DueSnoozes returns every snooze (all accounts) whose wake time has passed —
// the background sweeper unsnoozes these and notifies the UI.
func (s *Store) DueSnoozes(ctx context.Context, now int64) ([]Snooze, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT account_id, thread_id, until FROM snoozes WHERE until <= ? ORDER BY until`, now)
	if err != nil {
		return nil, fmt.Errorf("due snoozes: %w", err)
	}
	return scanSnoozes(rows)
}

// SnoozedThreads returns an account's snoozed conversations, soonest wake
// first — the "Snoozed" virtual folder.
func (s *Store) SnoozedThreads(ctx context.Context, accountID int64) ([]Snooze, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT account_id, thread_id, until FROM snoozes WHERE account_id = ? ORDER BY until`, accountID)
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

// SnoozedCount returns how many conversations an account has snoozed (active
// or elapsed-but-unswept) — the Snoozed folder's badge.
func (s *Store) SnoozedCount(ctx context.Context, accountID int64) (int, error) {
	var n int
	if err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM snoozes WHERE account_id = ?`, accountID).Scan(&n); err != nil {
		return 0, fmt.Errorf("snoozed count: %w", err)
	}
	return n, nil
}
