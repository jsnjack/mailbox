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

func TestMessagesMissingHTML(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	// Helper: insert a message and store a body, then force a fetch-version so the
	// test can recreate both the legacy (1) and current (2) states.
	store := func(gmailID string, date int64, text, html string, version int) {
		t.Helper()
		row, err := s.UpsertMessage(ctx, model.Message{AccountID: acc, GmailID: gmailID, ThreadID: gmailID, InternalDate: time.Unix(date, 0)})
		if err != nil {
			t.Fatalf("upsert %s: %v", gmailID, err)
		}
		if err := s.UpsertBody(ctx, model.MessageBody{MessageRowID: row, Text: text, HTML: html}); err != nil {
			t.Fatalf("upsert body %s: %v", gmailID, err)
		}
		// UpsertBody always stamps version 2; rewrite to the case under test.
		if _, err := s.writer.ExecContext(ctx, `UPDATE messages SET body_fetched = ? WHERE rowid = ?`, version, row); err != nil {
			t.Fatalf("set version %s: %v", gmailID, err)
		}
	}

	store("legacy-textonly", 300, "plain only", "", 1)     // candidate
	store("legacy-hashtml", 200, "plain", "<p>hi</p>", 1)  // has HTML — skip
	store("current-textonly", 400, "plain only", "", 2)    // already re-checked — skip
	store("legacy-textonly-old", 100, "plain only", "", 1) // candidate (older)
	// A metadata-only message (body never fetched) must not be selected.
	if _, err := s.UpsertMessage(ctx, model.Message{AccountID: acc, GmailID: "nobody", ThreadID: "nobody"}); err != nil {
		t.Fatalf("upsert nobody: %v", err)
	}

	got, err := s.MessagesMissingHTML(ctx, acc, 10)
	if err != nil {
		t.Fatalf("MessagesMissingHTML: %v", err)
	}
	// Only the two legacy text-only messages, newest first.
	want := []string{"legacy-textonly", "legacy-textonly-old"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	// The limit caps the batch (newest first).
	if one, err := s.MessagesMissingHTML(ctx, acc, 1); err != nil || len(one) != 1 || one[0] != "legacy-textonly" {
		t.Fatalf("limit=1 got %v err %v", one, err)
	}

	// Re-fetching a candidate (UpsertBody stamps version 2) removes it from the set.
	row, _ := s.GetMessage(ctx, acc, "legacy-textonly")
	if err := s.UpsertBody(ctx, model.MessageBody{MessageRowID: row.RowID, Text: "plain", HTML: "<p>recovered</p>"}); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if after, _ := s.MessagesMissingHTML(ctx, acc, 10); len(after) != 1 || after[0] != "legacy-textonly-old" {
		t.Fatalf("after re-fetch got %v, want [legacy-textonly-old]", after)
	}
}

func TestUpsertMessagesBatch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	msgs := []model.Message{
		{AccountID: acc, GmailID: "m1", ThreadID: "t1", InternalDate: time.Unix(100, 0), Subject: "alpha report", Labels: []string{"INBOX", "UNREAD"}, IsUnread: true},
		{AccountID: acc, GmailID: "m2", ThreadID: "t2", InternalDate: time.Unix(200, 0), Subject: "beta memo", Labels: []string{"INBOX"}},
		{AccountID: acc, GmailID: "m3", ThreadID: "t2", InternalDate: time.Unix(300, 0), Subject: "beta follow-up", Labels: []string{"INBOX"}},
	}
	if err := s.UpsertMessages(ctx, msgs); err != nil {
		t.Fatalf("UpsertMessages: %v", err)
	}

	// All three are stored and label-indexed.
	got, err := s.ListByLabel(ctx, acc, "INBOX", 50, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	// FTS was written in the same transaction, so search finds a batched row.
	hits, err := s.Search(ctx, acc, "beta", 50)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("search 'beta' got %d, want 2", len(hits))
	}

	// Empty input is a no-op, not an error.
	if err := s.UpsertMessages(ctx, nil); err != nil {
		t.Fatalf("UpsertMessages(nil): %v", err)
	}
}

func TestDeleteMessagesBatch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	for _, m := range []model.Message{
		{AccountID: acc, GmailID: "m1", ThreadID: "t1", Subject: "one", Labels: []string{"INBOX"}},
		{AccountID: acc, GmailID: "m2", ThreadID: "t2", Subject: "two", Labels: []string{"INBOX"}},
		{AccountID: acc, GmailID: "m3", ThreadID: "t3", Subject: "three", Labels: []string{"INBOX"}},
	} {
		if _, err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatalf("seed %s: %v", m.GmailID, err)
		}
	}

	// Delete two present ids plus one missing id (skipped, not an error).
	if err := s.DeleteMessages(ctx, acc, []string{"m1", "m3", "missing"}); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}
	got, err := s.ListByLabel(ctx, acc, "INBOX", 50, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(got) != 1 || got[0].GmailID != "m2" {
		t.Fatalf("after delete want only m2, got %+v", got)
	}
	// The deleted rows are gone from FTS too.
	if hits, err := s.Search(ctx, acc, "three", 50); err != nil || len(hits) != 0 {
		t.Fatalf("search 'three' after delete: %d hits, err %v", len(hits), err)
	}
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

func TestUpsertMessageReplyToRoundTrips(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	m := model.Message{
		AccountID: acc, GmailID: "m1", ThreadID: "t1", Subject: "Hi",
		FromAddr: "no-reply@x.com", ReplyTo: "List <list@x.com>", Labels: []string{"INBOX"},
	}
	if _, err := s.UpsertMessage(ctx, m); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetMessage(ctx, acc, "m1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ReplyTo != "List <list@x.com>" {
		t.Fatalf("ReplyTo = %q, want %q", got.ReplyTo, "List <list@x.com>")
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

func TestCountUnreadByLabel(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	for _, m := range []model.Message{
		{AccountID: acc, GmailID: "u1", ThreadID: "t1", Subject: "a", IsUnread: true, Labels: []string{"INBOX", "UNREAD"}},
		{AccountID: acc, GmailID: "u2", ThreadID: "t2", Subject: "b", IsUnread: true, Labels: []string{"INBOX", "UNREAD"}},
		{AccountID: acc, GmailID: "r1", ThreadID: "t3", Subject: "c", IsUnread: false, Labels: []string{"INBOX"}},
		{AccountID: acc, GmailID: "o1", ThreadID: "t4", Subject: "d", IsUnread: true, Labels: []string{"SENT", "UNREAD"}},
	} {
		if _, err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.GmailID, err)
		}
	}

	if n, err := s.CountUnreadByLabel(ctx, acc, "INBOX"); err != nil || n != 2 {
		t.Fatalf("INBOX unread = %d (err %v), want 2", n, err)
	}
	if n, _ := s.CountUnreadByLabel(ctx, acc, "SENT"); n != 1 {
		t.Fatalf("SENT unread = %d, want 1", n)
	}
}

func TestUnreadCountByLabelForAccounts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a1 := seedAccount(t, s)
	a2, err := s.UpsertAccount(ctx, model.Account{Email: "other@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	for _, m := range []model.Message{
		{AccountID: a1, GmailID: "u1", ThreadID: "t1", IsUnread: true, Labels: []string{"INBOX", "UNREAD"}},
		{AccountID: a1, GmailID: "u2", ThreadID: "t2", IsUnread: true, Labels: []string{"INBOX", "UNREAD"}},
		{AccountID: a1, GmailID: "r1", ThreadID: "t3", IsUnread: false, Labels: []string{"INBOX"}},
		{AccountID: a2, GmailID: "x1", ThreadID: "t4", IsUnread: true, Labels: []string{"INBOX", "UNREAD"}},
	} {
		if _, err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.GmailID, err)
		}
	}

	got, err := s.UnreadCountByLabelForAccounts(ctx, []int64{a1, a2}, "INBOX")
	if err != nil {
		t.Fatalf("UnreadCountByLabelForAccounts: %v", err)
	}
	if got[a1] != 2 || got[a2] != 1 {
		t.Fatalf("counts = %v, want {a1:2, a2:1}", got)
	}
	// Empty input → empty map, no error.
	if m, err := s.UnreadCountByLabelForAccounts(ctx, nil, "INBOX"); err != nil || len(m) != 0 {
		t.Fatalf("empty input: %v, %v", m, err)
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

// TestSearchMalformedInputDoesNotError guards the FTS5 query builder: arbitrary
// user input (operators, punctuation, smart quotes) must produce a valid MATCH
// expression, never an FTS syntax error.
func TestSearchMalformedInputDoesNotError(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)
	if _, err := s.UpsertMessage(ctx, model.Message{
		AccountID: acc, GmailID: "m1", ThreadID: "t1", Subject: "hello world", Snippet: "body text", Labels: []string{"INBOX"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, q := range []string{"*", ":", "()", "a*b", "-foo", `""`, "***", "AND", "OR", "NOT", "^", "foo:bar", "a OR b", "“smart”"} {
		if _, err := s.Search(ctx, acc, q, 20); err != nil {
			t.Errorf("Search(%q) errored: %v", q, err)
		}
	}
}
