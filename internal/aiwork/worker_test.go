package aiwork

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
	"github.com/jsnjack/mailbox/internal/syncer"
)

// fakeProvider answers every request with a fixed reply (or error) and counts
// calls.
type fakeProvider struct {
	reply string
	err   error
	calls atomic.Int64
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Stream(_ context.Context, _ string, _ []ai.Msg) (<-chan ai.Chunk, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	ch := make(chan ai.Chunk, 1)
	ch <- ai.Chunk{Text: f.reply}
	close(ch)
	return ch, nil
}

func testStore(t *testing.T) (*store.Store, int64) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	acct, err := s.UpsertAccount(context.Background(), model.Account{Email: "a@example.com"})
	if err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	return s, acct
}

// A pass classifies every uncategorized inbox thread, persists the results, and
// announces them; a second pass finds everything cached and calls no AI.
func TestPassCategorizesAndCaches(t *testing.T) {
	s, acct := testStore(t)
	ctx := context.Background()
	msgs := []model.Message{
		{AccountID: acct, GmailID: "g1", ThreadID: "t1", Subject: "Your receipt", Snippet: "Order #1", Labels: []string{model.LabelInbox}},
		{AccountID: acct, GmailID: "g2", ThreadID: "t2", Subject: "Weekly digest", Snippet: "News", Labels: []string{model.LabelInbox}},
		{AccountID: acct, GmailID: "g3", ThreadID: "t3", Subject: "Archived thing", Snippet: "Old", Labels: []string{}},
	}
	if err := s.UpsertMessages(ctx, msgs); err != nil {
		t.Fatalf("UpsertMessages: %v", err)
	}

	p := &fakeProvider{reply: `["Receipt"]`}
	hub := syncer.NewHub()
	events, unsub := hub.Subscribe()
	defer unsub()
	w := New(s, ai.NewAssistant(p), hub, nil, nil)

	remaining, err := w.pass(ctx, acct)
	if err != nil || remaining != 0 {
		t.Fatalf("pass = %d, %v", remaining, err)
	}
	if got := p.calls.Load(); got != 2 {
		t.Fatalf("AI calls = %d, want 2 (only the inbox threads)", got)
	}
	cats, err := s.MessageCategories(ctx, acct, []string{"g1", "g2", "g3"})
	if err != nil {
		t.Fatalf("MessageCategories: %v", err)
	}
	if cats["g1"] != "Receipt" || cats["g2"] != "Receipt" {
		t.Fatalf("persisted categories = %v", cats)
	}
	if _, ok := cats["g3"]; ok {
		t.Fatal("non-inbox message must not be categorized")
	}
	select {
	case c := <-events:
		if c.Kind != syncer.AIUpdated || c.AccountID != acct || c.Count != 2 {
			t.Fatalf("event = %+v", c)
		}
	case <-time.After(time.Second):
		t.Fatal("no AIUpdated event published")
	}

	// Second pass: all cached, no AI, no event.
	remaining, err = w.pass(ctx, acct)
	if err != nil || remaining != 0 {
		t.Fatalf("second pass = %d, %v", remaining, err)
	}
	if got := p.calls.Load(); got != 2 {
		t.Fatalf("AI calls after cached pass = %d, want still 2", got)
	}
}

// The enabled gate (the Preferences toggle) short-circuits a pass entirely.
func TestPassRespectsDisabled(t *testing.T) {
	s, acct := testStore(t)
	ctx := context.Background()
	if err := s.UpsertMessages(ctx, []model.Message{
		{AccountID: acct, GmailID: "g1", ThreadID: "t1", Subject: "s", Snippet: "x", Labels: []string{model.LabelInbox}},
	}); err != nil {
		t.Fatalf("UpsertMessages: %v", err)
	}
	p := &fakeProvider{reply: `["Receipt"]`}
	w := New(s, ai.NewAssistant(p), syncer.NewHub(), nil, func() bool { return false })
	if remaining, err := w.pass(ctx, acct); err != nil || remaining != 0 {
		t.Fatalf("pass = %d, %v", remaining, err)
	}
	if p.calls.Load() != 0 {
		t.Fatal("disabled worker must not call the AI")
	}
}

// An AI failure is persisted as a failed marker (so the UI can show it) and
// reported, letting Run's cooldown pace retries.
func TestPassMarksFailures(t *testing.T) {
	s, acct := testStore(t)
	ctx := context.Background()
	if err := s.UpsertMessages(ctx, []model.Message{
		{AccountID: acct, GmailID: "g1", ThreadID: "t1", Subject: "s", Snippet: "x", Labels: []string{model.LabelInbox}},
	}); err != nil {
		t.Fatalf("UpsertMessages: %v", err)
	}
	p := &fakeProvider{err: context.DeadlineExceeded}
	w := New(s, ai.NewAssistant(p), syncer.NewHub(), nil, nil)
	if _, err := w.pass(ctx, acct); err == nil {
		t.Fatal("expected the pass to report the failure")
	}
	failed, err := s.FailedCategoryIDs(ctx, acct, []string{"g1"})
	if err != nil {
		t.Fatalf("FailedCategoryIDs: %v", err)
	}
	if !failed["g1"] {
		t.Fatal("failed classification must be marked in the store")
	}
}
