package store

import (
	"context"
	"slices"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestApplyLabelSets(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	acct, err := s.UpsertAccount(ctx, model.Account{Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMessages(ctx, []model.Message{
		{AccountID: acct, GmailID: "read", ThreadID: "t1", Labels: []string{model.LabelInbox, model.LabelUnread}, IsUnread: true},
		{AccountID: acct, GmailID: "same", ThreadID: "t2", Labels: []string{"CATEGORY_UPDATES", model.LabelInbox}},
		{AccountID: acct, GmailID: "mine", ThreadID: "t3", Labels: []string{model.LabelInbox}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ModifyLabelsBatchAndEnqueue(ctx, acct, []string{"mine"}, []string{model.LabelStarred}, nil); err != nil {
		t.Fatal(err)
	}
	res, err := s.ApplyLabelSets(ctx, acct, []MessageLabels{
		{GmailID: "read", Labels: []string{model.LabelInbox, model.LabelStarred}},
		{GmailID: "same", Labels: []string{model.LabelInbox, "CATEGORY_UPDATES"}},
		{GmailID: "mine", Labels: nil},
		{GmailID: "uncached", Labels: []string{model.LabelInbox}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 1 || res.Changed[0].GmailID != "read" || res.Changed[0].ThreadID != "t1" {
		t.Fatalf("changed = %+v, want just read (thread t1)", res.Changed)
	}
	if !slices.Equal(res.Unchanged, []string{"same"}) || !slices.Equal(res.Pending, []string{"mine"}) {
		t.Fatalf("unchanged = %v, pending = %v", res.Unchanged, res.Pending)
	}
	m, err := s.GetMessage(ctx, acct, "read")
	if err != nil {
		t.Fatal(err)
	}
	if m.IsUnread || !m.IsStarred {
		t.Fatalf("flags not recomputed: unread=%v starred=%v", m.IsUnread, m.IsStarred)
	}
	if m, err := s.GetMessage(ctx, acct, "mine"); err != nil || !m.IsStarred {
		t.Fatalf("pending message changed: %+v, %v", m, err)
	}
}

func TestReconcileInbox(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	acct, err := s.UpsertAccount(ctx, model.Account{Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMessages(ctx, []model.Message{
		{AccountID: acct, GmailID: "gone", ThreadID: "t1", Labels: []string{model.LabelInbox}},
		{AccountID: acct, GmailID: "back", ThreadID: "t2", Labels: []string{"CATEGORY_UPDATES"}},
		{AccountID: acct, GmailID: "unread-elsewhere", ThreadID: "t3", Labels: []string{model.LabelInbox}},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := s.ReconcileInbox(ctx, acct, InboxState{
		IDs:      []string{"new2", "back", "new1", "unread-elsewhere"},
		Unread:   []string{"unread-elsewhere"},
		Complete: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Missing, []string{"new2", "new1"}) {
		t.Fatalf("missing = %v, want newest first", res.Missing)
	}
	if len(res.Changed) != 3 {
		t.Fatalf("changed = %+v, want gone, back, unread-elsewhere", res.Changed)
	}
	inbox, err := s.MessageIDsWithLabel(ctx, acct, model.LabelInbox, []string{"gone", "back", "unread-elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	if inbox["gone"] || !inbox["back"] || !inbox["unread-elsewhere"] {
		t.Fatalf("inbox membership = %v", inbox)
	}
	if m, err := s.GetMessage(ctx, acct, "unread-elsewhere"); err != nil || !m.IsUnread {
		t.Fatalf("unread state not taken: %+v, %v", m, err)
	}
}
