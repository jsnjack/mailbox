package syncer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

// wideSearchBackend simulates the historical IMAP bug: SearchIDs ignores its
// query and returns every message it knows, and Delete records what it was told
// to remove.
type wideSearchBackend struct {
	countingBackend
	allIDs  []string
	deleted []string
}

func (w *wideSearchBackend) SearchIDs(context.Context, string, int) ([]string, error) {
	return w.allIDs, nil
}

func (w *wideSearchBackend) Delete(_ context.Context, ids []string) error {
	w.deleted = append(w.deleted, ids...)
	return nil
}

// EmptyLabel must never delete a cached message that doesn't carry the target
// label, even when the backend's search returns out-of-scope ids — the guard is
// the last line of defence against "Empty Trash" wiping the whole account.
func TestEmptyLabelGuardsAgainstUnscopedSearch(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = s.Close() }()
	acct, err := s.UpsertAccount(ctx, model.Account{Email: "a@example.com", Type: model.AccountIMAP})
	if err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	seed := []model.Message{
		{AccountID: acct, GmailID: "trash-1", ThreadID: "t1", Labels: []string{model.LabelTrash}},
		{AccountID: acct, GmailID: "inbox-1", ThreadID: "t2", Labels: []string{model.LabelInbox}},
		{AccountID: acct, GmailID: "inbox-2", ThreadID: "t3", Labels: []string{model.LabelInbox, model.LabelUnread}},
	}
	if err := s.UpsertMessages(ctx, seed); err != nil {
		t.Fatalf("seed messages: %v", err)
	}

	be := &wideSearchBackend{allIDs: []string{"trash-1", "inbox-1", "inbox-2", "uncached-1"}}
	e := &Engine{Store: s}
	n, err := e.EmptyLabel(ctx, be, acct, model.LabelTrash)
	if err != nil {
		t.Fatalf("EmptyLabel: %v", err)
	}
	// trash-1 (labeled) and uncached-1 (unknown locally, so trusted) survive the
	// guard; the two cached inbox messages must not.
	if n != 2 {
		t.Fatalf("EmptyLabel removed %d, want 2 (trash-1 + uncached-1)", n)
	}
	got := map[string]bool{}
	for _, id := range be.deleted {
		got[id] = true
	}
	if !got["trash-1"] || !got["uncached-1"] || got["inbox-1"] || got["inbox-2"] {
		t.Fatalf("backend Delete got %v; want exactly trash-1 + uncached-1", be.deleted)
	}
	// The inbox messages are still cached.
	for _, id := range []string{"inbox-1", "inbox-2"} {
		if _, err := s.GetMessage(ctx, acct, id); err != nil {
			t.Fatalf("guarded message %s was deleted locally: %v", id, err)
		}
	}
	if _, err := s.GetMessage(ctx, acct, "trash-1"); err == nil {
		t.Fatal("trash-1 should have been deleted locally")
	}
}
