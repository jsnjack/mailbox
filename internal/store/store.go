// Package store is the local SQLite cache that serves as the single source of
// truth for the UI. It imports no GTK code and is fully testable without a
// display. Writes go through a single-connection handle (SQLite permits one
// writer under WAL); reads use a separate connection pool.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

//go:embed schema.sql
var schemaSQL string

// pragmas configures WAL (so readers and the single writer don't block each
// other), a busy timeout, foreign keys, and relaxed-but-safe sync.
const pragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"

// Store holds the database handles for the local cache.
type Store struct {
	writer *sql.DB
	reader *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies the
// schema. path may be a filename or ":memory:"-style DSN fragment.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?%s", path, pragmas)

	writer, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open writer: %w", err)
	}
	// A single writer connection serializes all writes, avoiding "database is
	// locked" under WAL.
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec(schemaSQL); err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(writer); err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	reader, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("open reader: %w", err)
	}
	// WAL lets reads run concurrently; cap the pool so a burst can't open an
	// unbounded number of connections (each re-applies the DSN pragmas), and keep
	// a few idle so steady-state reads reuse warm connections instead of
	// reopening. Plenty for the handful of concurrent readers (UI + sync).
	reader.SetMaxOpenConns(8)
	reader.SetMaxIdleConns(4)

	return &Store{writer: writer, reader: reader}, nil
}

// Close closes both database handles.
func (s *Store) Close() error {
	return errors.Join(s.reader.Close(), s.writer.Close())
}

// migrate applies schema changes that `CREATE TABLE IF NOT EXISTS` cannot make to
// a pre-existing table (it is a no-op on a fresh DB, where schema.sql already has
// the final shape). Each statement is idempotent: a column that already exists
// ("duplicate column name") or a rename whose source column is already gone ("no
// such column" — fresh DB or a prior migrate) means the step is already applied,
// so the error is ignored. Safe to run on every open.
func migrate(db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE messages ADD COLUMN reply_to TEXT`,
		// Multi-provider groundwork: tag each account with its backend, and rename
		// the Gmail-specific historyId watermark to an opaque cursor (IMAP stores a
		// per-folder UIDVALIDITY/MODSEQ summary here instead).
		`ALTER TABLE accounts ADD COLUMN account_type TEXT NOT NULL DEFAULT 'gmail'`,
		`ALTER TABLE accounts RENAME COLUMN last_history_id TO sync_cursor`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "duplicate column") || strings.Contains(msg, "no such column") {
				continue // already applied on a prior open (or fresh DB)
			}
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

// Vacuum rebuilds the database file, returning pages freed by deleted rows
// (messages, bodies, FTS, the AI caches) to the OS — WAL keeps that space until
// a VACUUM, so the file only grows after large deletions (emptying Trash/Spam,
// clearing categories) otherwise. The trailing checkpoint truncates the WAL so
// the on-disk size actually shrinks. It briefly takes the write lock, so callers
// should run it off the UI thread.
func (s *Store) Vacuum(ctx context.Context) error {
	if _, err := s.writer.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	if _, err := s.writer.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("checkpoint after vacuum: %w", err)
	}
	return nil
}

// withTx runs fn inside a write transaction, rolling back on error. All writes
// go through the single writer connection, so transactions never contend.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
