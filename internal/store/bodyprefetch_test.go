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
