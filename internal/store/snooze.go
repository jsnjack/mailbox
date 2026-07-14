package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
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

// SnoozeState is one snoozes row with its bookkeeping flags — the shape the
// label reconciler works over (see internal/snooze).
type SnoozeState struct {
	ThreadID string
	Until    int64
	Notified bool
	Mirrored bool
}

// ListSnoozes returns every snooze row for an account, pending and woken alike.
func (s *Store) ListSnoozes(ctx context.Context, accountID int64) ([]SnoozeState, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT thread_id, until, notified, mirrored FROM snoozes WHERE account_id = ?`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list snoozes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SnoozeState
	for rows.Next() {
		var st SnoozeState
		if err := rows.Scan(&st.ThreadID, &st.Until, &st.Notified, &st.Mirrored); err != nil {
			return nil, fmt.Errorf("scan snooze state: %w", err)
		}
		out = append(out, st)
	}
	logging.TraceContext(ctx, "store: list snoozes", "account", accountID, "count", len(out))
	return out, rows.Err()
}

// MarkSnoozeMirrored records that a snooze's label state has been handed to
// the provider (directly or adopted from labels another machine pushed).
func (s *Store) MarkSnoozeMirrored(ctx context.Context, accountID int64, threadID string) error {
	logging.TraceContext(ctx, "store: mark snooze mirrored", "account", accountID, "thread", threadID)
	if _, err := s.writer.ExecContext(ctx,
		`UPDATE snoozes SET mirrored = 1 WHERE account_id = ? AND thread_id = ?`, accountID, threadID); err != nil {
		return fmt.Errorf("mark snooze mirrored: %w", err)
	}
	return nil
}

// SnoozeLabelState maps each thread that carries a snooze mirror label (the
// "Snoozed" root or a "Snoozed/<wake time>" child) to those labels — the
// provider-truth side of snooze reconciliation.
func (s *Store) SnoozeLabelState(ctx context.Context, accountID int64) (map[string][]model.Label, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT DISTINCT m.thread_id, l.gmail_id, l.name
		FROM labels l
		JOIN message_labels ml ON ml.account_id = l.account_id AND ml.label_id = l.gmail_id
		JOIN messages m ON m.rowid = ml.message_rowid
		WHERE l.account_id = ? AND (l.name = ? OR l.name LIKE ?)`,
		accountID, model.SnoozeLabelRoot, model.SnoozeLabelPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("snooze label state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]model.Label{}
	for rows.Next() {
		var (
			tid string
			l   model.Label
		)
		if err := rows.Scan(&tid, &l.GmailID, &l.Name); err != nil {
			return nil, fmt.Errorf("scan snooze label: %w", err)
		}
		l.AccountID = accountID
		out[tid] = append(out[tid], l)
	}
	logging.TraceContext(ctx, "store: snooze label state", "account", accountID, "threads", len(out))
	return out, rows.Err()
}

// ThreadSnoozeLabels returns the snooze mirror labels present on one thread.
func (s *Store) ThreadSnoozeLabels(ctx context.Context, accountID int64, threadID string) ([]model.Label, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT DISTINCT l.gmail_id, l.name
		FROM labels l
		JOIN message_labels ml ON ml.account_id = l.account_id AND ml.label_id = l.gmail_id
		JOIN messages m ON m.rowid = ml.message_rowid
		WHERE l.account_id = ? AND m.thread_id = ? AND (l.name = ? OR l.name LIKE ?)`,
		accountID, threadID, model.SnoozeLabelRoot, model.SnoozeLabelPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("thread snooze labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Label
	for rows.Next() {
		var l model.Label
		if err := rows.Scan(&l.GmailID, &l.Name); err != nil {
			return nil, fmt.Errorf("scan thread snooze label: %w", err)
		}
		l.AccountID = accountID
		out = append(out, l)
	}
	return out, rows.Err()
}

// ThreadsWithInbox reports which of the given threads have at least one message
// still carrying INBOX.
func (s *Store) ThreadsWithInbox(ctx context.Context, accountID int64, threadIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(threadIDs))
	const chunk = 500
	for start := 0; start < len(threadIDs); start += chunk {
		end := min(start+chunk, len(threadIDs))
		batch := threadIDs[start:end]
		args := make([]any, 0, len(batch)+2)
		args = append(args, accountID, model.LabelInbox)
		for _, id := range batch {
			args = append(args, id)
		}
		rows, err := s.reader.QueryContext(ctx, `
			SELECT DISTINCT m.thread_id
			FROM messages m
			JOIN message_labels ml ON ml.message_rowid = m.rowid
			WHERE m.account_id = ? AND ml.label_id = ? AND m.thread_id IN (`+placeholders(len(batch))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("threads with inbox: %w", err)
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
			return nil, fmt.Errorf("threads with inbox: %w", err)
		}
	}
	return out, nil
}

// DeleteLabel removes a label row and any message_labels rows referencing it —
// the local shadow of a provider-side label deletion (a snooze wake-time label
// whose last thread woke).
func (s *Store) DeleteLabel(ctx context.Context, accountID int64, gmailID string) error {
	logging.TraceContext(ctx, "store: delete label", "account", accountID, "id", gmailID)
	if _, err := s.writer.ExecContext(ctx,
		`DELETE FROM message_labels WHERE account_id = ? AND label_id = ?`, accountID, gmailID); err != nil {
		return fmt.Errorf("delete label refs: %w", err)
	}
	if _, err := s.writer.ExecContext(ctx,
		`DELETE FROM labels WHERE account_id = ? AND gmail_id = ?`, accountID, gmailID); err != nil {
		return fmt.Errorf("delete label: %w", err)
	}
	return nil
}
