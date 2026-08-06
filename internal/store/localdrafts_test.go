package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestLocalDraftIsOfflineAuthoritativeAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mailbox.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	acct := seedAccount(t, s)
	if err := s.UpsertMessages(ctx, []model.Message{{
		AccountID: acct, GmailID: "provider-message", ThreadID: "provider-thread",
		Subject: "old server copy", Labels: []string{model.LabelDraft},
	}}); err != nil {
		t.Fatal(err)
	}
	msg := model.OutgoingMessage{
		From: "me@example.com", To: "you@example.com", Subject: "offline work",
		Body: "the complete local body", SourceMessageID: "provider-message",
		ThreadID: "provider-thread", Attachments: []model.OutgoingAttachment{{Filename: "note.txt", Data: []byte("payload")}},
	}
	localID, err := s.SaveLocalDraft(ctx, acct, msg)
	if err != nil {
		t.Fatal(err)
	}
	threads, err := s.ListThreadsByLabel(ctx, acct, model.LabelDraft, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].Latest.GmailID != localID || threads[0].Latest.Subject != msg.Subject {
		t.Fatalf("draft threads = %+v", threads)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	d, err := s.LocalDraft(ctx, localID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Message.Body != msg.Body || len(d.Message.Attachments) != 1 || string(d.Message.Attachments[0].Data) != "payload" {
		t.Fatalf("reopened local draft = %+v", d.Message)
	}
	if d.State != LocalDraftQueued || d.Revision != 1 {
		t.Fatalf("state=%q revision=%d", d.State, d.Revision)
	}
}

func TestLocalDraftRevisionDoesNotLoseNewerAutosave(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acct := seedAccount(t, s)
	localID, err := s.SaveLocalDraft(ctx, acct, model.OutgoingMessage{From: "me@example.com", Body: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveLocalDraft(ctx, acct, model.OutgoingMessage{LocalDraftID: localID, From: "me@example.com", Body: "two"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordLocalDraftSynced(ctx, localID, 1, model.DraftRef{DraftID: "provider-draft", MessageID: "provider-message"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.LocalDraft(ctx, localID)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != LocalDraftQueued || d.Revision != 2 || d.Message.Body != "two" || d.Message.DraftID != "provider-draft" {
		t.Fatalf("newer draft overwritten: %+v", d)
	}
	if err := s.MarkLocalDraftDeleting(ctx, localID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMessage(ctx, acct, localID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("synthetic draft after delete = %v", err)
	}
}
