package store

import (
	"context"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

// PruneBodies must clear the body (and body-derived artifacts) of messages
// older than the cutoff while keeping metadata, header search, and newer
// messages fully intact.
func TestPruneBodies(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	seed := func(gmailID string, date int64) int64 {
		t.Helper()
		row, err := s.UpsertMessage(ctx, model.Message{
			AccountID: acc, GmailID: gmailID, ThreadID: gmailID,
			Subject: "subject " + gmailID, InternalDate: time.Unix(date, 0),
		})
		if err != nil {
			t.Fatalf("upsert %s: %v", gmailID, err)
		}
		if err := s.UpsertBody(ctx, model.MessageBody{MessageRowID: row, Text: "searchable body " + gmailID, HTML: "<p>hi</p>"}); err != nil {
			t.Fatalf("upsert body %s: %v", gmailID, err)
		}
		if err := s.ReplaceAttachments(ctx, row, []model.Attachment{{GmailAttID: "a-" + gmailID, Filename: "f.pdf"}}); err != nil {
			t.Fatalf("attachments %s: %v", gmailID, err)
		}
		if err := s.SetTranslation(ctx, acc, gmailID, "English", "hoi"); err != nil {
			t.Fatalf("translation %s: %v", gmailID, err)
		}
		if err := s.SetAnalysis(ctx, acc, gmailID, "legit"); err != nil {
			t.Fatalf("analysis %s: %v", gmailID, err)
		}
		return row
	}
	oldRow := seed("old", 1000)
	newRow := seed("new", 3000)

	n, err := s.PruneBodies(ctx, 2000)
	if err != nil {
		t.Fatalf("PruneBodies: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	// Idempotent: nothing left to prune at the same cutoff.
	if n, err = s.PruneBodies(ctx, 2000); err != nil || n != 0 {
		t.Fatalf("second prune = %d, %v; want 0", n, err)
	}

	// The old message lost its body and reads as never-fetched (so opening it
	// re-fetches); the new one is untouched.
	old, err := s.GetMessage(ctx, acc, "old")
	if err != nil {
		t.Fatalf("get old: %v", err)
	}
	if old.BodyFetched {
		t.Fatal("old message still marked body-fetched after prune")
	}
	if b, err := s.GetBody(ctx, oldRow); err == nil && (b.Text != "" || b.HTML != "") {
		t.Fatalf("old body still present: %+v", b)
	}
	fresh, err := s.GetBody(ctx, newRow)
	if err != nil || fresh.Text == "" {
		t.Fatalf("new body damaged by prune: %+v, %v", fresh, err)
	}

	// Body-derived artifacts of the pruned message are gone; the new one's stay.
	if tr, err := s.Translations(ctx, acc, []string{"old", "new"}, "English"); err != nil || len(tr) != 1 || tr["new"] == "" {
		t.Fatalf("translations after prune = %v, %v; want only 'new'", tr, err)
	}
	if a, ok, err := s.Analysis(ctx, acc, "old"); err == nil && ok {
		t.Fatalf("analysis survived prune: %q", a)
	}
	if atts, err := s.ListAttachments(ctx, oldRow); err != nil || len(atts) != 0 {
		t.Fatalf("attachments survived prune: %v, %v", atts, err)
	}

	// Header search still finds the pruned message; body search no longer does.
	bySubject, err := s.Search(ctx, acc, "subject", 10)
	if err != nil || len(bySubject) != 2 {
		t.Fatalf("subject search after prune = %d hits (%v), want 2", len(bySubject), err)
	}
	byBody, err := s.Search(ctx, acc, "searchable", 10)
	if err != nil || len(byBody) != 1 || byBody[0].GmailID != "new" {
		t.Fatalf("body search after prune = %v (%v), want only 'new'", byBody, err)
	}
}
