package store

import (
	"context"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestSearchDateAndUnreadFilters(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acct := seedAccount(t, s)
	for _, seed := range []struct {
		id, day string
		unread  bool
	}{{"before", "2025-12-31", true}, {"start", "2026-01-01", true}, {"read", "2026-01-15", false}, {"end", "2026-02-01", true}} {
		date, err := time.Parse("2006-01-02", seed.day)
		if err != nil {
			t.Fatal(err)
		}
		labels := []string{model.LabelInbox}
		if seed.unread {
			labels = append(labels, model.LabelUnread)
		}
		if _, err := s.UpsertMessage(ctx, model.Message{AccountID: acct, GmailID: seed.id, ThreadID: seed.id, FromAddr: "jane@example.com", InternalDate: date, Labels: labels, Subject: "Report"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, query string
		count       int
	}{{"combined", "after:2026-01-01 before:2026-02-01 is:unread", 1}, {"date range", "after:2026-01-01 before:2026-02-01", 2}, {"unread", "is:unread", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Search(ctx, acct, tc.query, 50)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.count {
				t.Fatalf("got %d results; want %d", len(got), tc.count)
			}
		})
	}
}
