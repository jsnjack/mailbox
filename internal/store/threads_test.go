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

func TestThreadLabels(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	for _, m := range []model.Message{
		{AccountID: acc, GmailID: "a1", ThreadID: "A", Subject: "one", Labels: []string{"INBOX", "UNREAD"}},
		{AccountID: acc, GmailID: "a2", ThreadID: "A", Subject: "two", Labels: []string{"INBOX", "Label_7"}},
		{AccountID: acc, GmailID: "b1", ThreadID: "B", Subject: "other", Labels: []string{"SENT"}},
	} {
		if _, err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.GmailID, err)
		}
	}

	got, err := s.ThreadLabels(ctx, acc, "A")
	if err != nil {
		t.Fatalf("ThreadLabels: %v", err)
	}
	// Union across both messages in thread A, deduplicated.
	for _, want := range []string{"INBOX", "UNREAD", "Label_7"} {
		if !got[want] {
			t.Fatalf("thread A missing label %q in %v", want, got)
		}
	}
	if got["SENT"] {
		t.Fatalf("thread A should not carry SENT (that's thread B): %v", got)
	}
}

func TestListAllThreads(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	// A: two inbox messages. B: one sent message. T: latest in Trash (excluded).
	// S: latest in Spam (excluded).
	for _, m := range []model.Message{
		{AccountID: acc, GmailID: "a1", ThreadID: "A", InternalDate: time.Unix(100, 0), Subject: "A first", Labels: []string{"INBOX"}},
		{AccountID: acc, GmailID: "a2", ThreadID: "A", InternalDate: time.Unix(300, 0), Subject: "A latest", Labels: []string{"INBOX"}},
		{AccountID: acc, GmailID: "b1", ThreadID: "B", InternalDate: time.Unix(500, 0), Subject: "B sent", Labels: []string{"SENT"}},
		{AccountID: acc, GmailID: "t1", ThreadID: "T", InternalDate: time.Unix(700, 0), Subject: "trashed", Labels: []string{"TRASH"}},
		{AccountID: acc, GmailID: "s1", ThreadID: "S", InternalDate: time.Unix(900, 0), Subject: "spammy", Labels: []string{"SPAM"}},
	} {
		if _, err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.GmailID, err)
		}
	}

	threads, err := s.ListAllThreads(ctx, acc, 50, 0)
	if err != nil {
		t.Fatalf("ListAllThreads: %v", err)
	}
	if len(threads) != 2 {
		t.Fatalf("got %d threads, want 2 (A, B); Spam/Trash must be excluded", len(threads))
	}
	// Newest first: B (500) before A (300).
	if threads[0].ThreadID != "B" || threads[1].ThreadID != "A" {
		t.Fatalf("order: %s, %s", threads[0].ThreadID, threads[1].ThreadID)
	}
	if threads[1].Latest.GmailID != "a2" || threads[1].Count != 2 {
		t.Fatalf("thread A summary wrong: %+v", threads[1])
	}
}

func TestListThreadsByLabelTiesAndNullDates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	// Thread T: two messages with the SAME internal_date (whole-second tie).
	// Thread N: messages with no date at all (NULL internal_date).
	for _, m := range []model.Message{
		{AccountID: acc, GmailID: "t1", ThreadID: "T", InternalDate: time.Unix(500, 0), Subject: "tie one", Labels: []string{"INBOX"}},
		{AccountID: acc, GmailID: "t2", ThreadID: "T", InternalDate: time.Unix(500, 0), Subject: "tie two", Labels: []string{"INBOX"}},
		{AccountID: acc, GmailID: "n1", ThreadID: "N", Subject: "no date a", Labels: []string{"INBOX"}},
		{AccountID: acc, GmailID: "n2", ThreadID: "N", Subject: "no date b", Labels: []string{"INBOX"}},
	} {
		if _, err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.GmailID, err)
		}
	}

	threads, err := s.ListThreadsByLabel(ctx, acc, "INBOX", 50, 0)
	if err != nil {
		t.Fatalf("ListThreadsByLabel: %v", err)
	}
	// Exactly one row per thread — no duplicate for the tie, and the NULL-date
	// thread is not dropped.
	seen := map[string]int{}
	for _, th := range threads {
		seen[th.ThreadID]++
	}
	if seen["T"] != 1 {
		t.Fatalf("tie thread T appeared %d times, want 1", seen["T"])
	}
	if seen["N"] != 1 {
		t.Fatalf("NULL-date thread N appeared %d times, want 1", seen["N"])
	}
}
