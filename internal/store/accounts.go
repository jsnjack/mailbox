package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// UpsertAccount inserts the account or updates the existing row matched by email,
// returning the account's local id.
func (s *Store) UpsertAccount(ctx context.Context, a model.Account) (int64, error) {
	start := time.Now()
	logging.TraceContext(ctx, "store: upsert account", "account", a.Email, "type", a.Type)
	var expiry, backfilled any
	if !a.TokenExpiry.IsZero() {
		expiry = a.TokenExpiry.Unix()
	}
	if !a.BackfilledAt.IsZero() {
		backfilled = a.BackfilledAt.Unix()
	}
	atype := a.Type
	if atype == "" {
		atype = model.AccountGmail
	}
	var id int64
	err := s.writer.QueryRowContext(ctx, `
		INSERT INTO accounts (email, display_name, account_type, token_expiry, scopes, sync_cursor, backfilled_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET
			display_name = excluded.display_name,
			account_type = excluded.account_type,
			token_expiry = excluded.token_expiry,
			scopes       = excluded.scopes,
			sync_cursor  = excluded.sync_cursor,
			backfilled_at = excluded.backfilled_at
		RETURNING id`,
		a.Email, a.DisplayName, atype, expiry, strings.Join(a.Scopes, " "), a.SyncCursor, backfilled,
	).Scan(&id)
	if err != nil {
		logging.TraceContext(ctx, "store: upsert account", "account", a.Email, "err", err)
		return 0, fmt.Errorf("upsert account %q: %w", a.Email, err)
	}
	logging.TraceContext(ctx, "store: upsert account done", "account", a.Email, "id", id, "dur", time.Since(start))
	return id, nil
}

// GetAccountByEmail returns the account with the given email, or ErrNotFound.
func (s *Store) GetAccountByEmail(ctx context.Context, email string) (model.Account, error) {
	logging.TraceContext(ctx, "store: get account by email", "account", email)
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, email, display_name, account_type, token_expiry, scopes, sync_cursor, backfilled_at
		FROM accounts WHERE email = ?`, email)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		logging.TraceContext(ctx, "store: get account by email", "account", email, "found", false)
		return model.Account{}, ErrNotFound
	}
	if err != nil {
		logging.TraceContext(ctx, "store: get account by email", "account", email, "err", err)
		return model.Account{}, fmt.Errorf("get account %q: %w", email, err)
	}
	return a, nil
}

// GetAccountByID returns the account with the given local id, or ErrNotFound.
func (s *Store) GetAccountByID(ctx context.Context, id int64) (model.Account, error) {
	logging.TraceContext(ctx, "store: get account by id", "account", id)
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, email, display_name, account_type, token_expiry, scopes, sync_cursor, backfilled_at
		FROM accounts WHERE id = ?`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		logging.TraceContext(ctx, "store: get account by id", "account", id, "found", false)
		return model.Account{}, ErrNotFound
	}
	if err != nil {
		logging.TraceContext(ctx, "store: get account by id", "account", id, "err", err)
		return model.Account{}, fmt.Errorf("get account %d: %w", id, err)
	}
	return a, nil
}

// ListAccounts returns all connected accounts ordered by id.
func (s *Store) ListAccounts(ctx context.Context) ([]model.Account, error) {
	start := time.Now()
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, email, display_name, account_type, token_expiry, scopes, sync_cursor, backfilled_at
		FROM accounts ORDER BY id`)
	if err != nil {
		logging.TraceContext(ctx, "store: list accounts", "err", err)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: list accounts", "count", len(out), "dur", time.Since(start))
	return out, nil
}

// SetSyncCursor updates the opaque incremental-sync cursor for an account.
func (s *Store) SetSyncCursor(ctx context.Context, accountID int64, cursor string) error {
	logging.TraceContext(ctx, "store: set sync cursor", "account", accountID, "cursor", cursor)
	if _, err := s.writer.ExecContext(ctx,
		`UPDATE accounts SET sync_cursor = ? WHERE id = ?`, cursor, accountID); err != nil {
		logging.TraceContext(ctx, "store: set sync cursor", "account", accountID, "err", err)
		return fmt.Errorf("set sync_cursor: %w", err)
	}
	return nil
}

// DeleteAccount removes an account and all of its cached data. The account row's
// ON DELETE CASCADE drops the messages, labels, threads, outbox, and per-message
// AI tables; the FTS index is not a foreign-key child, so its rows are cleared
// explicitly first (else they orphan and corrupt search).
func (s *Store) DeleteAccount(ctx context.Context, accountID int64) error {
	start := time.Now()
	logging.TraceContext(ctx, "store: delete account", "account", accountID)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM messages_fts WHERE rowid IN (SELECT rowid FROM messages WHERE account_id = ?)`,
			accountID); err != nil {
			return fmt.Errorf("delete fts rows: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, accountID); err != nil {
			return fmt.Errorf("delete account %d: %w", accountID, err)
		}
		return nil
	})
	if err != nil {
		logging.TraceContext(ctx, "store: delete account", "account", accountID, "err", err)
		return err
	}
	logging.TraceContext(ctx, "store: delete account done", "account", accountID, "dur", time.Since(start))
	return nil
}

// SetBackfilledAt marks the account's initial backfill as complete at t.
func (s *Store) SetBackfilledAt(ctx context.Context, accountID int64, t time.Time) error {
	logging.TraceContext(ctx, "store: set backfilled at", "account", accountID, "at", t)
	if _, err := s.writer.ExecContext(ctx,
		`UPDATE accounts SET backfilled_at = ? WHERE id = ?`, t.Unix(), accountID); err != nil {
		logging.TraceContext(ctx, "store: set backfilled at", "account", accountID, "err", err)
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
		atype      sql.NullString
		cursor     sql.NullString
	)
	if err := sc.Scan(&a.ID, &a.Email, &display, &atype, &expiry, &scopes, &cursor, &backfilled); err != nil {
		return model.Account{}, err
	}
	a.DisplayName = display.String
	a.Type = atype.String
	if a.Type == "" {
		a.Type = model.AccountGmail
	}
	a.SyncCursor = cursor.String
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
