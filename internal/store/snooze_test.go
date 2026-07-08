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

// A woken snooze is marked notified (not deleted) so the inbox listing can
// flag where the thread came from; a notified row is announced only once and
// drops out of the Snoozed folder and its count, and re-snoozing resets it.
func TestSnoozeWokeTag(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	if _, err := s.UpsertMessage(ctx, model.Message{
		AccountID: acc, GmailID: "woke", ThreadID: "woke",
		Subject: "woke", InternalDate: time.Unix(1000, 0),
		Labels: []string{model.LabelInbox},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	past := time.Now().Add(-time.Minute).Unix()
	if err := s.SnoozeThread(ctx, acc, "woke", past); err != nil {
		t.Fatalf("snooze: %v", err)
	}

	due, err := s.DueSnoozes(ctx, time.Now().Unix())
	if err != nil || len(due) != 1 || due[0].ThreadID != "woke" {
		t.Fatalf("due before notify = %+v, %v", due, err)
	}
	if err := s.MarkSnoozeNotified(ctx, acc, "woke"); err != nil {
		t.Fatalf("mark notified: %v", err)
	}

	// Announced once: no longer due, so the sweeper won't re-notify every pass.
	if due, _ := s.DueSnoozes(ctx, time.Now().Unix()); len(due) != 0 {
		t.Fatalf("due after notify = %+v, want none", due)
	}
	// Dropped from the Snoozed folder and its count — it's back in the inbox.
	if sn, _ := s.SnoozedThreads(ctx, acc); len(sn) != 0 {
		t.Fatalf("SnoozedThreads after notify = %+v, want none", sn)
	}
	if n, _ := s.SnoozedCount(ctx, acc); n != 0 {
		t.Fatalf("SnoozedCount after notify = %d, want 0", n)
	}
	// The row lingers so the inbox listing can flag where the thread came from.
	out, err := s.ListThreadsByLabel(ctx, acc, model.LabelInbox, 10, 0)
	if err != nil || len(out) != 1 || !out[0].WokeFromSnooze {
		t.Fatalf("ListThreadsByLabel = %+v, %v, want one WokeFromSnooze thread", out, err)
	}

	// Re-snoozing resets notified, so the tag goes away until it wakes again.
	future := time.Now().Add(time.Hour).Unix()
	if err := s.SnoozeThread(ctx, acc, "woke", future); err != nil {
		t.Fatalf("re-snooze: %v", err)
	}
	out, err = s.ListThreadsByLabel(ctx, acc, model.LabelInbox, 10, 0)
	if err != nil || len(out) != 0 {
		t.Fatalf("ListThreadsByLabel after re-snooze = %+v, %v, want none (hidden again)", out, err)
	}
	if due, _ := s.DueSnoozes(ctx, time.Now().Unix()); len(due) != 0 {
		t.Fatalf("due right after re-snooze = %+v, want none", due)
	}
}
