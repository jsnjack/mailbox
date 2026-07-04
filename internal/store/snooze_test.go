package store

import (
	"context"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

// A snoozed thread must vanish from the inbox list (labels untouched), show in
// the snoozed set, surface as due once its wake time passes, and reappear in
// the inbox after unsnooze.
func TestSnoozeLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	seed := func(gmailID string) {
		t.Helper()
		row, err := s.UpsertMessage(ctx, model.Message{
			AccountID: acc, GmailID: gmailID, ThreadID: gmailID,
			Subject: gmailID, InternalDate: time.Unix(1000, 0),
			Labels: []string{model.LabelInbox},
		})
		if err != nil || row == 0 {
			t.Fatalf("upsert %s: %v", gmailID, err)
		}
	}
	seed("stays")
	seed("snoozed")

	inbox := func() int {
		t.Helper()
		out, err := s.ListThreadsByLabel(ctx, acc, model.LabelInbox, 10, 0)
		if err != nil {
			t.Fatalf("list inbox: %v", err)
		}
		return len(out)
	}
	if n := inbox(); n != 2 {
		t.Fatalf("inbox before snooze = %d, want 2", n)
	}

	future := time.Now().Add(time.Hour).Unix()
	if err := s.SnoozeThread(ctx, acc, "snoozed", future); err != nil {
		t.Fatalf("snooze: %v", err)
	}
	if n := inbox(); n != 1 {
		t.Fatalf("inbox with active snooze = %d, want 1", n)
	}
	sn, err := s.SnoozedThreads(ctx, acc)
	if err != nil || len(sn) != 1 || sn[0].ThreadID != "snoozed" || sn[0].Until != future {
		t.Fatalf("SnoozedThreads = %+v, %v", sn, err)
	}
	// Not due yet.
	if due, _ := s.DueSnoozes(ctx, time.Now().Unix()); len(due) != 0 {
		t.Fatalf("due before wake time = %+v, want none", due)
	}
	// Due once the clock passes the wake time.
	due, err := s.DueSnoozes(ctx, future+1)
	if err != nil || len(due) != 1 || due[0].ThreadID != "snoozed" {
		t.Fatalf("due after wake time = %+v, %v", due, err)
	}
	// An elapsed snooze no longer hides the thread, even before unsnooze runs.
	if err := s.SnoozeThread(ctx, acc, "snoozed", time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatalf("re-snooze: %v", err)
	}
	if n := inbox(); n != 2 {
		t.Fatalf("inbox with elapsed snooze = %d, want 2", n)
	}

	if err := s.UnsnoozeThread(ctx, acc, "snoozed"); err != nil {
		t.Fatalf("unsnooze: %v", err)
	}
	if sn, _ := s.SnoozedThreads(ctx, acc); len(sn) != 0 {
		t.Fatalf("snoozed after unsnooze = %+v, want none", sn)
	}
	if n := inbox(); n != 2 {
		t.Fatalf("inbox after unsnooze = %d, want 2", n)
	}
}
