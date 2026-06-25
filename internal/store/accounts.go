package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// UpsertAccount inserts the account or updates the existing row matched by email,
// returning the account's local id.
func (s *Store) UpsertAccount(ctx context.Context, a model.Account) (int64, error) {
	var expiry, backfilled any
	if !a.TokenExpiry.IsZero() {
		expiry = a.TokenExpiry.Unix()
	}
	if !a.BackfilledAt.IsZero() {
		backfilled = a.BackfilledAt.Unix()
	}
	var id int64
	err := s.writer.QueryRowContext(ctx, `
		INSERT INTO accounts (email, display_name, token_expiry, scopes, last_history_id, backfilled_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET
			display_name    = excluded.display_name,
			token_expiry    = excluded.token_expiry,
			scopes          = excluded.scopes,
			last_history_id = excluded.last_history_id,
			backfilled_at   = excluded.backfilled_at
		RETURNING id`,
		a.Email, a.DisplayName, expiry, strings.Join(a.Scopes, " "), a.LastHistoryID, backfilled,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert account %q: %w", a.Email, err)
	}
	return id, nil
}

// GetAccountByEmail returns the account with the given email, or ErrNotFound.
func (s *Store) GetAccountByEmail(ctx context.Context, email string) (model.Account, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, email, display_name, token_expiry, scopes, last_history_id, backfilled_at
		FROM accounts WHERE email = ?`, email)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Account{}, ErrNotFound
	}
	if err != nil {
		return model.Account{}, fmt.Errorf("get account %q: %w", email, err)
	}
	return a, nil
}

// GetAccountByID returns the account with the given local id, or ErrNotFound.
func (s *Store) GetAccountByID(ctx context.Context, id int64) (model.Account, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, email, display_name, token_expiry, scopes, last_history_id, backfilled_at
		FROM accounts WHERE id = ?`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Account{}, ErrNotFound
	}
	if err != nil {
		return model.Account{}, fmt.Errorf("get account %d: %w", id, err)
	}
	return a, nil
}

// ListAccounts returns all connected accounts ordered by id.
func (s *Store) ListAccounts(ctx context.Context) ([]model.Account, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, email, display_name, token_expiry, scopes, last_history_id, backfilled_at
		FROM accounts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetLastHistoryID updates the incremental-sync watermark for an account.
func (s *Store) SetLastHistoryID(ctx context.Context, accountID int64, historyID string) error {
	if _, err := s.writer.ExecContext(ctx,
		`UPDATE accounts SET last_history_id = ? WHERE id = ?`, historyID, accountID); err != nil {
		return fmt.Errorf("set last_history_id: %w", err)
	}
	return nil
}

// SetBackfilledAt marks the account's initial backfill as complete at t.
func (s *Store) SetBackfilledAt(ctx context.Context, accountID int64, t time.Time) error {
	if _, err := s.writer.ExecContext(ctx,
		`UPDATE accounts SET backfilled_at = ? WHERE id = ?`, t.Unix(), accountID); err != nil {
		return fmt.Errorf("set backfilled_at: %w", err)
	}
	return nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanAccount(sc rowScanner) (model.Account, error) {
	var (
		a          model.Account
		expiry     sql.NullInt64
		backfilled sql.NullInt64
		scopes     sql.NullString
		display    sql.NullString
		history    sql.NullString
	)
	if err := sc.Scan(&a.ID, &a.Email, &display, &expiry, &scopes, &history, &backfilled); err != nil {
		return model.Account{}, err
	}
	a.DisplayName = display.String
	a.LastHistoryID = history.String
	if scopes.Valid && scopes.String != "" {
		a.Scopes = strings.Fields(scopes.String)
	}
	if expiry.Valid {
		a.TokenExpiry = time.Unix(expiry.Int64, 0)
	}
	if backfilled.Valid {
		a.BackfilledAt = time.Unix(backfilled.Int64, 0)
	}
	return a, nil
}
