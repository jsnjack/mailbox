package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
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

func TestVacuum(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	// Insert then delete a chunk of messages so VACUUM has freed pages to reclaim.
	var msgs []model.Message
	for i := 0; i < 200; i++ {
		msgs = append(msgs, model.Message{
			AccountID: acc, GmailID: fmt.Sprintf("g%d", i), ThreadID: fmt.Sprintf("t%d", i),
			Subject: "subject for vacuum test", Snippet: "some snippet text to take up space",
			Labels: []string{"INBOX"},
		})
	}
	if err := s.UpsertMessages(ctx, msgs); err != nil {
		t.Fatalf("UpsertMessages: %v", err)
	}
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.GmailID
	}
	if err := s.DeleteMessages(ctx, acc, ids); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}

	if err := s.Vacuum(ctx); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	// The store still works after a vacuum.
	if n, err := s.CountByLabel(ctx, acc, "INBOX"); err != nil || n != 0 {
		t.Fatalf("after vacuum CountByLabel = %d err=%v, want 0", n, err)
	}
}
