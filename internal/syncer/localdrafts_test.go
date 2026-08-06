package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

type draftMirrorBackend struct {
	countingBackend
	mu         sync.Mutex
	findID     string
	updatedID  string
	updates    int
	deletes    int
	failUpdate bool
}

func (b *draftMirrorBackend) FindDraftID(context.Context, string) (string, error) {
	return b.findID, nil
}

func (b *draftMirrorBackend) SaveDraft(context.Context, []byte, string) (model.DraftRef, error) {
	return model.DraftRef{DraftID: "created-draft", MessageID: "created-message"}, nil
}

func (b *draftMirrorBackend) UpdateDraft(context.Context, string, []byte, string) (model.DraftRef, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.updates++
	if b.failUpdate {
		return model.DraftRef{}, errors.New("offline")
	}
	return model.DraftRef{DraftID: b.updatedID, MessageID: "replacement-message"}, nil
}

func (b *draftMirrorBackend) DeleteDraft(context.Context, string) error {
	b.mu.Lock()
	b.deletes++
	b.mu.Unlock()
	return nil
}

func TestSweepLocalDraftsUpdatesThenDeletesProviderDraft(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	acct, err := s.UpsertAccount(ctx, model.Account{Email: "drafts@example.com", Type: model.AccountGmail})
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, nil)
	localID, err := e.SaveDraftLocal(ctx, acct, model.OutgoingMessage{
		From: "drafts@example.com", To: "to@example.com", Body: "offline body",
		SourceMessageID: "old-provider-message", ThreadID: "thread-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	be := &draftMirrorBackend{findID: "old-draft", updatedID: "new-draft"}
	if n, err := e.SweepLocalDrafts(ctx, be, acct); err != nil || n != 1 {
		t.Fatalf("sync sweep = %d, %v", n, err)
	}
	d, err := s.LocalDraft(ctx, localID)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != store.LocalDraftSynced || d.ProviderDraftID != "new-draft" || d.ProviderMessageID != "replacement-message" {
		t.Fatalf("synced draft = %+v", d)
	}
	if err := s.MarkLocalDraftDeleting(ctx, localID); err != nil {
		t.Fatal(err)
	}
	if n, err := e.SweepLocalDrafts(ctx, be, acct); err != nil || n != 1 {
		t.Fatalf("delete sweep = %d, %v", n, err)
	}
	if _, err := s.LocalDraft(ctx, localID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("local draft after delete = %v", err)
	}
	be.mu.Lock()
	updates, deletes := be.updates, be.deletes
	be.mu.Unlock()
	if updates != 1 || deletes != 1 {
		t.Fatalf("provider updates=%d deletes=%d", updates, deletes)
	}
}
