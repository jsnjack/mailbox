package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrateAccountsCursor verifies that opening a pre-existing (old-schema)
// database adds account_type and renames last_history_id → sync_cursor without
// losing data, and that re-running migrate is a no-op.
func TestMigrateAccountsCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// The pre-migration accounts table: last_history_id, no account_type. Created
	// before schemaSQL so its CREATE TABLE IF NOT EXISTS leaves this shape intact.
	if _, err := db.Exec(`CREATE TABLE accounts (
		id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE, display_name TEXT,
		token_expiry INTEGER, scopes TEXT, last_history_id TEXT, backfilled_at INTEGER,
		created_at INTEGER NOT NULL DEFAULT (unixepoch()))`); err != nil {
		t.Fatalf("create old accounts table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO accounts (email, last_history_id) VALUES ('old@example.com', '777')`); err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	// schemaSQL creates the remaining tables and skips the existing accounts table.
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var atype, cursor string
	if err := db.QueryRow(
		`SELECT account_type, sync_cursor FROM accounts WHERE email = 'old@example.com'`).
		Scan(&atype, &cursor); err != nil {
		t.Fatalf("query migrated row: %v", err)
	}
	if atype != "gmail" {
		t.Errorf("account_type = %q, want gmail (default)", atype)
	}
	if cursor != "777" {
		t.Errorf("sync_cursor = %q, want 777 (preserved from last_history_id)", cursor)
	}

	// Idempotent: a second pass (column already added, already renamed) is a no-op.
	if err := migrate(db); err != nil {
		t.Fatalf("migrate is not idempotent: %v", err)
	}
}
