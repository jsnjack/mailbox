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
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
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
	start := time.Now()
	logging.Trace("store: open", "path", path)
	dsn := fmt.Sprintf("file:%s?%s", path, pragmas)

	writer, err := sql.Open("sqlite", dsn)
	if err != nil {
		logging.Trace("store: open", "path", path, "err", err)
		return nil, fmt.Errorf("open writer: %w", err)
	}
	// A single writer connection serializes all writes, avoiding "database is
	// locked" under WAL.
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec(schemaSQL); err != nil {
		_ = writer.Close()
		logging.Trace("store: apply schema", "path", path, "err", err)
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	logging.Trace("store: schema applied", "path", path)
	if err := migrate(writer); err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	reader, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = writer.Close()
		logging.Trace("store: open reader", "path", path, "err", err)
		return nil, fmt.Errorf("open reader: %w", err)
	}
	// WAL lets reads run concurrently; cap the pool so a burst can't open an
	// unbounded number of connections (each re-applies the DSN pragmas), and keep
	// a few idle so steady-state reads reuse warm connections instead of
	// reopening. Plenty for the handful of concurrent readers (UI + sync).
	reader.SetMaxOpenConns(8)
	reader.SetMaxIdleConns(4)

	logging.Trace("store: opened", "path", path, "dur", time.Since(start))
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
		// Unsubscribe support: the List-Unsubscribe header (captured at metadata
		// fetch) and whether List-Unsubscribe-Post offered RFC 8058 one-click.
		"ALTER TABLE messages ADD COLUMN list_unsubscribe TEXT NOT NULL DEFAULT ''",
		`ALTER TABLE messages ADD COLUMN list_unsub_post INTEGER NOT NULL DEFAULT 0`,
		// Multi-provider groundwork: tag each account with its backend, and rename
		// the Gmail-specific historyId watermark to an opaque cursor (IMAP stores a
		// per-folder UIDVALIDITY/MODSEQ summary here instead).
		`ALTER TABLE accounts ADD COLUMN account_type TEXT NOT NULL DEFAULT 'gmail'`,
		`ALTER TABLE accounts RENAME COLUMN last_history_id TO sync_cursor`,
		// Inline-image support: remember each part's Content-ID so a cid: <img> in
		// the body can be resolved to its bytes and rendered.
		`ALTER TABLE attachments ADD COLUMN content_id TEXT NOT NULL DEFAULT ''`,
		// Manual category override: a category the user picked by hand outranks the
		// automatic "Replied" tag in the list, and must survive a restart.
		`ALTER TABLE message_categories ADD COLUMN manual INTEGER NOT NULL DEFAULT 0`,
		// Outbox-first sending: persist the source draft to delete post-send, and a
		// not_before watermark so a message held for its undo window can't be swept
		// (or lost on quit) before the window elapses.
		`ALTER TABLE outbox ADD COLUMN draft_id TEXT`,
		`ALTER TABLE outbox ADD COLUMN not_before INTEGER NOT NULL DEFAULT 0`,
		// Snoozed-tag support: a woken snooze row is kept (not deleted) so the
		// list can show where a thread came from; notified distinguishes an
		// announced wake from a still-pending one.
		`ALTER TABLE snoozes ADD COLUMN notified INTEGER NOT NULL DEFAULT 0`,
		// Distinguish "AI attempt failed" from "AI legitimately found no
		// category": a status='failed' row is excluded from MessageCategories'
		// "already done" set, so it stays a retry candidate instead of being
		// indistinguishable from a settled category=''.
		`ALTER TABLE message_categories ADD COLUMN status TEXT NOT NULL DEFAULT 'ok'`,
		// Snooze label mirroring: whether this snooze has been handed to the
		// provider (labels applied). The reconciler must treat a never-mirrored
		// row as "push it out" even though its thread still has INBOX — the
		// same INBOX that, on a mirrored row, means "unsnoozed elsewhere".
		`ALTER TABLE snoozes ADD COLUMN mirrored INTEGER NOT NULL DEFAULT 0`,
		// Bcc capture (only ever present on the user's own sent/draft copies):
		// shown in the reader and preserved when a draft is re-edited.
		"ALTER TABLE messages ADD COLUMN bcc_addrs TEXT NOT NULL DEFAULT ''",
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "duplicate column") || strings.Contains(msg, "no such column") {
				logging.Trace("store: migrate step already applied", "stmt", s)
				continue // already applied on a prior open (or fresh DB)
			}
			logging.Trace("store: migrate step", "stmt", s, "err", err)
			return fmt.Errorf("%s: %w", s, err)
		}
		logging.Trace("store: migrate step applied", "stmt", s)
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
	start := time.Now()
	logging.TraceContext(ctx, "store: vacuum")
	if _, err := s.writer.ExecContext(ctx, "VACUUM"); err != nil {
		logging.TraceContext(ctx, "store: vacuum", "err", err)
		return fmt.Errorf("vacuum: %w", err)
	}
	logging.TraceContext(ctx, "store: wal-truncate")
	if _, err := s.writer.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		logging.TraceContext(ctx, "store: wal-truncate", "err", err)
		return fmt.Errorf("checkpoint after vacuum: %w", err)
	}
	logging.TraceContext(ctx, "store: vacuum done", "dur", time.Since(start))
	return nil
}

// withTx runs fn inside a write transaction, rolling back on error. All writes
// go through the single writer connection, so transactions never contend.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		logging.TraceContext(ctx, "store: begin tx", "err", err)
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		logging.TraceContext(ctx, "store: tx rollback", "err", err)
		return err
	}
	if err := tx.Commit(); err != nil {
		logging.TraceContext(ctx, "store: tx commit", "err", err)
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
