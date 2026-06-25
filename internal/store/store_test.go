package store

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestOpenAppliesSchema(t *testing.T) {
	s := openTestStore(t)

	wantTables := []string{
		"accounts", "labels", "threads", "messages",
		"message_labels", "message_bodies", "attachments",
		"outbox", "messages_fts",
	}
	for _, name := range wantTables {
		t.Run(name, func(t *testing.T) {
			var got string
			err := s.reader.QueryRow(
				`SELECT name FROM sqlite_master WHERE name = ?`, name,
			).Scan(&got)
			if err != nil {
				t.Fatalf("table %q not found: %v", name, err)
			}
			if got != name {
				t.Fatalf("got %q, want %q", got, name)
			}
		})
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	for i := 0; i < 2; i++ {
		s, err := Open(dbPath)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}
}

func TestWALEnabled(t *testing.T) {
	s := openTestStore(t)
	var mode string
	if err := s.writer.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}
