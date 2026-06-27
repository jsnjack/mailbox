package store

import (
	"context"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestAnalysis(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	// Absent: ok is false, no error.
	if a, ok, err := s.Analysis(ctx, acc, "m1"); err != nil || ok || a != "" {
		t.Fatalf("absent = (%q, %v, %v), want empty/false/nil", a, ok, err)
	}

	if err := s.SetAnalysis(ctx, acc, "m1", "Likely safe.\n• SPF/DKIM pass"); err != nil {
		t.Fatalf("SetAnalysis: %v", err)
	}
	a, ok, err := s.Analysis(ctx, acc, "m1")
	if err != nil || !ok {
		t.Fatalf("Analysis: ok=%v err=%v", ok, err)
	}
	if a != "Likely safe.\n• SPF/DKIM pass" {
		t.Fatalf("got %q", a)
	}

	// Upsert overwrites.
	if err := s.SetAnalysis(ctx, acc, "m1", "Suspicious."); err != nil {
		t.Fatalf("SetAnalysis update: %v", err)
	}
	if a, _, _ := s.Analysis(ctx, acc, "m1"); a != "Suspicious." {
		t.Fatalf("after update = %q", a)
	}
}

// TestDeleteMessageClearsAnalysis verifies the analysis is dropped when its
// message is deleted (keyed by gmail_id, FK on accounts, so it needs explicit
// cleanup like the other AI caches).
func TestDeleteMessageClearsAnalysis(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	if _, err := s.UpsertMessage(ctx, model.Message{
		AccountID: acc, GmailID: "g1", ThreadID: "t1", Subject: "hi", Labels: []string{"INBOX"},
	}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	if err := s.SetAnalysis(ctx, acc, "g1", "Likely safe."); err != nil {
		t.Fatalf("SetAnalysis: %v", err)
	}
	if err := s.DeleteMessages(ctx, acc, []string{"g1"}); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}
	if _, ok, _ := s.Analysis(ctx, acc, "g1"); ok {
		t.Fatalf("analysis should be gone after delete")
	}
}
