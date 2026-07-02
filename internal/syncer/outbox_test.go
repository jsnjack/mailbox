package syncer

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

// countingBackend records how many times Send is called. Send sleeps briefly to
// widen the window in which overlapping sweeps could both send the same item.
type countingBackend struct {
	sends     atomic.Int32
	searchHit bool // when true, SearchIDs reports the message already exists
}

func (c *countingBackend) Send(_ context.Context, _ []byte, _ string) (string, error) {
	c.sends.Add(1)
	time.Sleep(20 * time.Millisecond)
	return "sent-id", nil
}

// The rest of backend.Backend is unused by SweepOutbox; stub it out.
func (c *countingBackend) Profile(context.Context) (backend.Profile, error) {
	return backend.Profile{}, nil
}
func (c *countingBackend) Labels(context.Context) ([]model.Label, error) { return nil, nil }
func (c *countingBackend) SearchIDs(context.Context, string, int) ([]string, error) {
	if c.searchHit {
		return []string{"existing-id"}, nil
	}
	return nil, nil
}
func (c *countingBackend) FetchMetadata(context.Context, string) (model.Message, error) {
	return model.Message{}, nil
}
func (c *countingBackend) FetchBody(context.Context, string) (model.MessageBody, []model.Attachment, error) {
	return model.MessageBody{}, nil, nil
}
func (c *countingBackend) FetchAttachment(context.Context, string, string) ([]byte, error) {
	return nil, nil
}
func (c *countingBackend) ApplyLabels(context.Context, []string, []string, []string) error {
	return nil
}
func (c *countingBackend) Delete(context.Context, []string) error { return nil }
func (c *countingBackend) Changes(context.Context, string) ([]string, []string, string, error) {
	return nil, nil, "", nil
}
func (c *countingBackend) SaveDraft(context.Context, []byte, string) (string, error) { return "", nil }
func (c *countingBackend) UpdateDraft(context.Context, string, []byte, string) (string, error) {
	return "", nil
}
func (c *countingBackend) DeleteDraft(context.Context, string) error           { return nil }
func (c *countingBackend) FindDraftID(context.Context, string) (string, error) { return "", nil }

// TestSweepOutboxConcurrentNoDoubleSend guards the mutex that serializes
// SweepOutbox: with three real triggers (background timer, "Send now", "retry")
// running on independent goroutines, overlapping sweeps must not send the same
// queued message twice to the recipient.
func TestSweepOutboxConcurrentNoDoubleSend(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = s.Close() }()

	acct, err := s.UpsertAccount(ctx, model.Account{Email: "a@example.com", Type: model.AccountGmail})
	if err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	if err := s.EnqueueOutbox(ctx, acct, "thread-1", []byte("From: a@example.com\r\n\r\nhi")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	be := &countingBackend{}
	e := &Engine{Store: s}

	// Fire many concurrent sweeps, mimicking the timer + Send-now + retry overlap.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.SweepOutbox(ctx, be, acct); err != nil {
				t.Errorf("sweep: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := be.sends.Load(); got != 1 {
		t.Fatalf("Send called %d times, want exactly 1 (double-send)", got)
	}
	// The item must be marked sent, so it no longer appears sendable.
	left, err := s.ListSendableOutbox(ctx, acct, maxOutboxAttempts)
	if err != nil {
		t.Fatalf("list sendable: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("outbox still has %d sendable items, want 0", len(left))
	}
}

// A queued item whose Message-ID the provider already has (a prior send that
// succeeded but returned a network error) must be marked sent, NOT resent —
// otherwise the recipient gets a duplicate.
func TestSweepOutboxSkipsAlreadySent(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = s.Close() }()

	acct, err := s.UpsertAccount(ctx, model.Account{Email: "a@example.com", Type: model.AccountGmail})
	if err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	rfc := []byte("Message-ID: <dedup-test@example.com>\r\nFrom: a@example.com\r\n\r\nhi")
	if err := s.EnqueueOutbox(ctx, acct, "thread-1", rfc); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	be := &countingBackend{searchHit: true} // provider already has this Message-ID
	e := &Engine{Store: s}
	if _, err := e.SweepOutbox(ctx, be, acct); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := be.sends.Load(); got != 0 {
		t.Fatalf("Send called %d times, want 0 (already at provider)", got)
	}
	left, err := s.ListSendableOutbox(ctx, acct, maxOutboxAttempts)
	if err != nil {
		t.Fatalf("list sendable: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("outbox still has %d sendable items, want 0 (should be marked sent)", len(left))
	}
}
