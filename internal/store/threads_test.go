package store

import (
	"context"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestListThreadsByLabel(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	// Thread A: two INBOX messages (one unread). Thread B: one INBOX message.
	msgs := []model.Message{
		{AccountID: acc, GmailID: "a1", ThreadID: "A", InternalDate: time.Unix(100, 0), Subject: "A first", IsUnread: false, Labels: []string{"INBOX"}},
		{AccountID: acc, GmailID: "a2", ThreadID: "A", InternalDate: time.Unix(300, 0), Subject: "A latest", IsUnread: true, Labels: []string{"INBOX", "UNREAD"}},
		{AccountID: acc, GmailID: "b1", ThreadID: "B", InternalDate: time.Unix(200, 0), Subject: "B only", Labels: []string{"INBOX"}},
		{AccountID: acc, GmailID: "c1", ThreadID: "C", InternalDate: time.Unix(400, 0), Subject: "C archived", Labels: []string{"Label_9"}},
	}
	for _, m := range msgs {
		if _, err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.GmailID, err)
		}
	}

	threads, err := s.ListThreadsByLabel(ctx, acc, "INBOX", 50, 0)
	if err != nil {
		t.Fatalf("ListThreadsByLabel: %v", err)
	}
	if len(threads) != 2 {
		t.Fatalf("got %d threads, want 2 (A, B)", len(threads))
	}
	// Newest first: thread A (latest date 300) before B (200).
	if threads[0].ThreadID != "A" || threads[1].ThreadID != "B" {
		t.Fatalf("order: %s, %s", threads[0].ThreadID, threads[1].ThreadID)
	}
	if threads[0].Latest.GmailID != "a2" || threads[0].Count != 2 || threads[0].UnreadCount != 1 {
		t.Fatalf("thread A summary wrong: %+v", threads[0])
	}

	// All messages in thread A, oldest first.
	tm, err := s.ListThreadMessages(ctx, acc, "A")
	if err != nil {
		t.Fatalf("ListThreadMessages: %v", err)
	}
	if len(tm) != 2 || tm[0].GmailID != "a1" || tm[1].GmailID != "a2" {
		t.Fatalf("thread messages wrong: %+v", tm)
	}

	sum, err := s.GetThreadSummary(ctx, acc, "A")
	if err != nil {
		t.Fatalf("GetThreadSummary: %v", err)
	}
	if sum.Latest.GmailID != "a2" || sum.Count != 2 {
		t.Fatalf("GetThreadSummary wrong: %+v", sum)
	}
}
