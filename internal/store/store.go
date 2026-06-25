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

	reader, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("open reader: %w", err)
	}

	return &Store{writer: writer, reader: reader}, nil
}

// Close closes both database handles.
func (s *Store) Close() error {
	return errors.Join(s.reader.Close(), s.writer.Close())
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
