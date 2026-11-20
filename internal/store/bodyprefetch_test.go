package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestMessagesMissingBodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cutoff int64
		limit  int
		want   []string
	}{
		{"Inbox only", 0, 10, []string{"undated", "new", "old"}},
		{"retention boundary", 200, 10, []string{"undated", "new"}},
		{"bounded page", 0, 1, []string{"undated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t)
			accountID := seedAccount(t, s)
			other, err := s.UpsertAccount(ctx, model.Account{Email: "other@example.com"})
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range []model.Message{
				{AccountID: accountID, GmailID: "old", InternalDate: time.Unix(100, 0), Labels: []string{model.LabelInbox}},
				{AccountID: accountID, GmailID: "new", InternalDate: time.Unix(200, 0), Labels: []string{model.LabelInbox, "custom"}},
				{AccountID: accountID, GmailID: "cached", InternalDate: time.Unix(300, 0), Labels: []string{model.LabelInbox}},
				{AccountID: accountID, GmailID: "undated", Labels: []string{model.LabelInbox}},
				{AccountID: other, GmailID: "new", Labels: []string{model.LabelInbox}},
				{AccountID: accountID, GmailID: "archived", Labels: []string{"custom"}},
				{AccountID: accountID, GmailID: "sent", Labels: []string{model.LabelSent}},
				{AccountID: accountID, GmailID: "trash", Labels: []string{model.LabelTrash}},
				{AccountID: accountID, GmailID: "spam", Labels: []string{model.LabelSpam}},
			} {
				m.ThreadID = m.GmailID
				rowID, err := s.UpsertMessage(ctx, m)
				if err != nil {
					t.Fatal(err)
				}
				if m.GmailID == "cached" {
					if err := s.UpsertBody(ctx, model.MessageBody{MessageRowID: rowID}); err != nil {
						t.Fatal(err)
					}
				}
			}
			msgs, err := s.MessagesMissingBodies(ctx, accountID, tc.cutoff, 0, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, m := range msgs {
				ids = append(ids, m.GmailID)
			}
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("missing bodies = %v, want %v", ids, tc.want)
			}
			if tc.limit == 1 {
				// Removing the first page must not make keyset paging skip a row.
				if err := s.UpsertBody(ctx, model.MessageBody{MessageRowID: msgs[0].RowID}); err != nil {
					t.Fatal(err)
				}
				next, err := s.MessagesMissingBodies(ctx, accountID, 0, msgs[0].RowID, 10)
				if err != nil || len(next) != 2 || next[0].GmailID != "new" || next[1].GmailID != "old" {
					t.Fatalf("next page = %v, err %v", next, err)
				}
			}
		})
	}
}

// Mail stored with the body downloaded alongside it is never visible without
// that body: message, body and attachment rows land in one transaction.
func TestUpsertMessagesWithBodies(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	accountID := seedAccount(t, s)
	msgs := []model.Message{
		{AccountID: accountID, GmailID: "full", ThreadID: "t1", Subject: "hello", Labels: []string{model.LabelInbox}},
		{AccountID: accountID, GmailID: "meta", ThreadID: "t2", Labels: []string{model.LabelInbox}},
	}
	bodies := map[string]FetchedBody{"full": {
		Body:        model.MessageBody{Text: "needle body", HTML: `<p>needle body</p><img src="cid:logo">`, RawHeaders: "spf=pass"},
		Attachments: []model.Attachment{{GmailAttID: "a1", Filename: "report.pdf", MimeType: "application/pdf", SizeBytes: 10}, {GmailAttID: "a2", MimeType: "image/png", ContentID: "logo"}},
	}}
	if err := s.UpsertMessagesWithBodies(ctx, msgs, bodies); err != nil {
		t.Fatal(err)
	}
	full, err := s.GetMessage(ctx, accountID, "full")
	if err != nil {
		t.Fatal(err)
	}
	if !full.BodyFetched || !full.HasAttachments {
		t.Fatalf("full: fetched=%v attachments=%v, want both", full.BodyFetched, full.HasAttachments)
	}
	body, err := s.GetBody(ctx, full.RowID)
	if err != nil || body.Text != "needle body" || body.RawHeaders != "spf=pass" {
		t.Fatalf("body = %+v, err %v", body, err)
	}
	atts, err := s.ListAttachments(ctx, full.RowID)
	if err != nil || len(atts) != 2 {
		t.Fatalf("attachments = %v, err %v", atts, err)
	}
	meta, err := s.GetMessage(ctx, accountID, "meta")
	if err != nil || meta.BodyFetched {
		t.Fatalf("meta: fetched=%v err %v, want metadata only", meta.BodyFetched, err)
	}
	// The body is searchable at once, and the prefetch has only the other left.
	found, err := s.Search(ctx, accountID, "needle", 10)
	if err != nil || len(found) != 1 || found[0].GmailID != "full" {
		t.Fatalf("search = %v, err %v; want the stored body", found, err)
	}
	missing, err := s.MessagesMissingBodies(ctx, accountID, 0, 0, 10)
	if err != nil || len(missing) != 1 || missing[0].GmailID != "meta" {
		t.Fatalf("still missing = %v, err %v; want just meta", missing, err)
	}
}
