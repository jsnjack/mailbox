package store

import (
	"context"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestThreadSummary(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	// Absent summary: ok is false, no error.
	if fp, sum, ok, err := s.ThreadSummary(ctx, acc, "t1"); err != nil || ok || fp != "" || sum != "" {
		t.Fatalf("absent = (%q, %q, %v, %v), want empty/false/nil", fp, sum, ok, err)
	}

	if err := s.SetThreadSummary(ctx, acc, "t1", "t1|m1|m2", "• point one\n• point two"); err != nil {
		t.Fatalf("SetThreadSummary: %v", err)
	}
	fp, sum, ok, err := s.ThreadSummary(ctx, acc, "t1")
	if err != nil || !ok {
		t.Fatalf("ThreadSummary: ok=%v err=%v", ok, err)
	}
	if fp != "t1|m1|m2" || sum != "• point one\n• point two" {
		t.Fatalf("got (%q, %q), want fingerprint+summary", fp, sum)
	}

	// A new message changes the fingerprint; upsert replaces both.
	if err := s.SetThreadSummary(ctx, acc, "t1", "t1|m1|m2|m3", "• updated"); err != nil {
		t.Fatalf("SetThreadSummary update: %v", err)
	}
	fp, sum, _, _ = s.ThreadSummary(ctx, acc, "t1")
	if fp != "t1|m1|m2|m3" || sum != "• updated" {
		t.Fatalf("after update got (%q, %q)", fp, sum)
	}
}

// TestDeleteMessageClearsAICaches verifies that deleting a message drops its
// persisted translation (by gmail_id) and its thread's summary (by thread_id),
// leaving no orphan rows.
func TestDeleteMessageClearsAICaches(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	if _, err := s.UpsertMessage(ctx, model.Message{
		AccountID: acc, GmailID: "g1", ThreadID: "th1", Subject: "hi", Labels: []string{"INBOX"},
	}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	if err := s.SetTranslation(ctx, acc, "g1", "English", "<p>hi</p>"); err != nil {
		t.Fatalf("SetTranslation: %v", err)
	}
	if err := s.SetThreadSummary(ctx, acc, "th1", "th1|g1", "• summary"); err != nil {
		t.Fatalf("SetThreadSummary: %v", err)
	}

	if err := s.DeleteMessages(ctx, acc, []string{"g1"}); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}

	if tr, _ := s.Translations(ctx, acc, []string{"g1"}, "English"); len(tr) != 0 {
		t.Fatalf("translation should be gone, got %v", tr)
	}
	if _, _, ok, _ := s.ThreadSummary(ctx, acc, "th1"); ok {
		t.Fatalf("thread summary should be gone")
	}
}

// TestClearSummaries verifies the summary-language reset: both AI summary caches
// go — thread summaries and per-message gists — while unrelated AI caches (a
// translation, which is language-explicit already) stay.
func TestClearSummaries(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	if _, err := s.UpsertMessage(ctx, model.Message{
		AccountID: acc, GmailID: "g1", ThreadID: "th1", Subject: "hi", Labels: []string{"INBOX"},
	}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	if err := s.SetThreadSummary(ctx, acc, "th1", "th1|g1", "• summary"); err != nil {
		t.Fatalf("SetThreadSummary: %v", err)
	}
	if err := s.SetMessageGist(ctx, acc, "g1", "a gist"); err != nil {
		t.Fatalf("SetMessageGist: %v", err)
	}
	if err := s.SetTranslation(ctx, acc, "g1", "English", "<p>hi</p>"); err != nil {
		t.Fatalf("SetTranslation: %v", err)
	}

	n, err := s.ClearSummaries(ctx)
	if err != nil {
		t.Fatalf("ClearSummaries: %v", err)
	}
	if n != 2 {
		t.Fatalf("rows cleared = %d, want 2", n)
	}
	if _, _, ok, _ := s.ThreadSummary(ctx, acc, "th1"); ok {
		t.Fatalf("thread summary should be gone")
	}
	if g, _ := s.MessageGists(ctx, acc, []string{"g1"}); len(g) != 0 {
		t.Fatalf("gist should be gone, got %v", g)
	}
	if tr, _ := s.Translations(ctx, acc, []string{"g1"}, "English"); len(tr) != 1 {
		t.Fatalf("translation should survive, got %v", tr)
	}

	// Idempotent: clearing an empty cache is not an error.
	if n, err := s.ClearSummaries(ctx); err != nil || n != 0 {
		t.Fatalf("second ClearSummaries = (%d, %v), want (0, nil)", n, err)
	}
}
