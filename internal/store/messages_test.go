package store

import (
	"context"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

func seedAccount(t *testing.T, s *Store) int64 {
	t.Helper()
	id, err := s.UpsertAccount(context.Background(), model.Account{Email: "me@example.com"})
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return id
}

func TestUpsertMessageAndListByLabel(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	msgs := []model.Message{
		{AccountID: acc, GmailID: "m1", ThreadID: "t1", InternalDate: time.Unix(1000, 0),
			FromName: "Alice", FromAddr: "alice@x.com", Subject: "Invoice March",
			IsUnread: true, Labels: []string{"INBOX", "UNREAD"}},
		{AccountID: acc, GmailID: "m2", ThreadID: "t2", InternalDate: time.Unix(2000, 0),
			FromName: "Bob", FromAddr: "bob@x.com", Subject: "Lunch Friday",
			Labels: []string{"INBOX"}},
		{AccountID: acc, GmailID: "m3", ThreadID: "t3", InternalDate: time.Unix(3000, 0),
			FromName: "Carol", FromAddr: "carol@x.com", Subject: "Archived note",
			Labels: []string{"Label_5"}},
	}
	for _, m := range msgs {
		if _, err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.GmailID, err)
		}
	}

	n, err := s.CountByLabel(ctx, acc, "INBOX")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("INBOX count = %d, want 2", n)
	}

	got, err := s.ListByLabel(ctx, acc, "INBOX", 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d INBOX messages, want 2", len(got))
	}
	// Newest first: m2 (date 2000) before m1 (date 1000).
	if got[0].GmailID != "m2" || got[1].GmailID != "m1" {
		t.Fatalf("order wrong: %s, %s", got[0].GmailID, got[1].GmailID)
	}
	if !got[1].IsUnread {
		t.Fatal("m1 should be unread")
	}
}

func TestUpsertMessageIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	m := model.Message{AccountID: acc, GmailID: "m1", ThreadID: "t1", Subject: "Hi",
		Labels: []string{"INBOX", "UNREAD"}}
	id1, err := s.UpsertMessage(ctx, m)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Re-upsert with a label removed (read).
	m.Labels = []string{"INBOX"}
	id2, err := s.UpsertMessage(ctx, m)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("rowid changed: %d != %d", id1, id2)
	}
	if n, _ := s.CountByLabel(ctx, acc, "UNREAD"); n != 0 {
		t.Fatalf("UNREAD count = %d, want 0 after relabel", n)
	}
	got, err := s.GetMessage(ctx, acc, "m1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "INBOX" {
		t.Fatalf("labels = %v, want [INBOX]", got.Labels)
	}
}

func TestSearchMultiWordAndPrefix(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	if _, err := s.UpsertMessage(ctx, model.Message{
		AccountID: acc, GmailID: "m1", ThreadID: "t1",
		Subject: "Quarterly invoice", FromName: "Dana", Labels: []string{"INBOX"}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Prefix, multi-word (implicit AND), and case-insensitive.
	for _, q := range []string{"quart", "quarterly invoice", "INVOICE", "quar inv"} {
		res, err := s.Search(ctx, acc, q, 10)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(res) != 1 {
			t.Fatalf("search %q: got %d, want 1", q, len(res))
		}
	}
	// A term that doesn't match.
	if res, _ := s.Search(ctx, acc, "zzz", 10); len(res) != 0 {
		t.Fatalf("expected no match for zzz, got %d", len(res))
	}
	// Special characters must not break the query (quotes/parens).
	if _, err := s.Search(ctx, acc, `"(weird] query`, 10); err != nil {
		t.Fatalf("special-char query errored: %v", err)
	}
	// A quote-only query must not produce an invalid FTS5 expression.
	for _, q := range []string{`"`, `""`, `" "`} {
		if _, err := s.Search(ctx, acc, q, 10); err != nil {
			t.Fatalf("quote-only query %q errored: %v", q, err)
		}
	}
	// Blank query returns nothing without error.
	if res, err := s.Search(ctx, acc, "   ", 10); err != nil || len(res) != 0 {
		t.Fatalf("blank query: res=%d err=%v", len(res), err)
	}
}

func TestCountAll(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	for _, m := range []model.Message{
		{AccountID: acc, GmailID: "m1", ThreadID: "t1", Subject: "inbox", Labels: []string{"INBOX"}},
		{AccountID: acc, GmailID: "m2", ThreadID: "t2", Subject: "sent", Labels: []string{"SENT"}},
		{AccountID: acc, GmailID: "m3", ThreadID: "t3", Subject: "trashed", Labels: []string{"TRASH"}},
		{AccountID: acc, GmailID: "m4", ThreadID: "t4", Subject: "spammy", Labels: []string{"SPAM"}},
	} {
		if _, err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.GmailID, err)
		}
	}

	n, err := s.CountAll(ctx, acc)
	if err != nil {
		t.Fatalf("CountAll: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountAll = %d, want 2 (Spam and Trash excluded)", n)
	}
}

func TestModifyLabels(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	if _, err := s.UpsertMessage(ctx, model.Message{
		AccountID: acc, GmailID: "m1", ThreadID: "t1", Subject: "Hi",
		IsUnread: true, Labels: []string{"INBOX", "UNREAD"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Mark read (remove UNREAD) and star (add STARRED).
	if err := s.ModifyLabels(ctx, acc, "m1", []string{"STARRED"}, []string{"UNREAD"}); err != nil {
		t.Fatalf("ModifyLabels: %v", err)
	}

	got, err := s.GetMessage(ctx, acc, "m1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.IsUnread {
		t.Fatal("expected read after removing UNREAD")
	}
	if !got.IsStarred {
		t.Fatal("expected starred after adding STARRED")
	}
	if n, _ := s.CountByLabel(ctx, acc, "UNREAD"); n != 0 {
		t.Fatalf("UNREAD count = %d, want 0", n)
	}
	if n, _ := s.CountByLabel(ctx, acc, "STARRED"); n != 1 {
		t.Fatalf("STARRED count = %d, want 1", n)
	}

	// Archive (remove INBOX).
	if err := s.ModifyLabels(ctx, acc, "m1", nil, []string{"INBOX"}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if n, _ := s.CountByLabel(ctx, acc, "INBOX"); n != 0 {
		t.Fatalf("INBOX count = %d, want 0 after archive", n)
	}
}

func TestMarkLabelRead(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	mk := func(id string, unread bool, labels ...string) model.Message {
		return model.Message{AccountID: acc, GmailID: id, ThreadID: id, IsUnread: unread, Labels: labels}
	}
	for _, m := range []model.Message{
		mk("u1", true, "INBOX", "UNREAD"),
		mk("u2", true, "INBOX", "UNREAD"),
		mk("r1", false, "INBOX"),
		mk("o1", true, "Label_3", "UNREAD"), // unread but not in INBOX
	} {
		if _, err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.GmailID, err)
		}
	}

	ids, err := s.UnreadIDsByLabel(ctx, acc, "INBOX")
	if err != nil || len(ids) != 2 {
		t.Fatalf("unread INBOX ids: %v (err %v)", ids, err)
	}

	if err := s.MarkLabelReadLocal(ctx, acc, "INBOX"); err != nil {
		t.Fatalf("MarkLabelReadLocal: %v", err)
	}
	if ids, _ := s.UnreadIDsByLabel(ctx, acc, "INBOX"); len(ids) != 0 {
		t.Fatalf("expected 0 unread in INBOX after mark-read, got %d", len(ids))
	}
	// The non-INBOX unread message is untouched.
	if n, _ := s.CountByLabel(ctx, acc, "UNREAD"); n != 1 {
		t.Fatalf("UNREAD count = %d, want 1 (the non-INBOX message)", n)
	}
}

func TestModifyLabelsMissing(t *testing.T) {
	s := openTestStore(t)
	acc := seedAccount(t, s)
	if err := s.ModifyLabels(context.Background(), acc, "nope", nil, []string{"INBOX"}); err == nil {
		t.Fatal("expected error for missing message")
	}
}

func TestSearchSubjectAndBody(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	rowID, err := s.UpsertMessage(ctx, model.Message{
		AccountID: acc, GmailID: "m1", ThreadID: "t1", Subject: "Quarterly report",
		FromName: "Dana", Labels: []string{"INBOX"}})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Found by subject term before body is present.
	if res, err := s.Search(ctx, acc, "quarterly", 10); err != nil || len(res) != 1 {
		t.Fatalf("subject search: res=%d err=%v", len(res), err)
	}
	// Not found by a body-only term yet.
	if res, _ := s.Search(ctx, acc, "asparagus", 10); len(res) != 0 {
		t.Fatalf("expected no match before body indexed, got %d", len(res))
	}

	if err := s.UpsertBody(ctx, model.MessageBody{MessageRowID: rowID,
		Text: "The asparagus harvest exceeded projections."}); err != nil {
		t.Fatalf("upsert body: %v", err)
	}
	// Now found by the body term.
	if res, err := s.Search(ctx, acc, "asparagus", 10); err != nil || len(res) != 1 {
		t.Fatalf("body search: res=%d err=%v", len(res), err)
	}

	body, err := s.GetBody(ctx, rowID)
	if err != nil {
		t.Fatalf("get body: %v", err)
	}
	if body.Text == "" {
		t.Fatal("body text empty")
	}
}
