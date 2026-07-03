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
	sends        atomic.Int32
	draftDeletes atomic.Int32
	searchHit    bool // when true, SearchIDs reports the message already exists
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
func (c *countingBackend) DeleteDraft(context.Context, string) error {
	c.draftDeletes.Add(1)
	return nil
}
func (c *countingBackend) FindDraftID(context.Context, string) (string, error) { return "", nil }

// TestEnqueueSendUndoWindow verifies the outbox-first send path: a message
// queued with a future not_before is durable but not swept during its undo
// window, and once the window elapses the sweep delivers it and deletes the
// source draft.
func TestEnqueueSendUndoWindow(t *testing.T) {
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

	base := time.Unix(1_000_000, 0)
	be := &countingBackend{}
	e := &Engine{Store: s, Now: func() time.Time { return base }}

	msg := model.OutgoingMessage{From: "a@example.com", To: "b@example.com", Subject: "hi", Body: "there", DraftID: "draft-1"}
	if _, err := e.EnqueueSend(ctx, acct, msg, base.Add(5*time.Second).Unix()); err != nil {
		t.Fatalf("enqueue send: %v", err)
	}

	// Within the undo window: a sweep must not deliver it.
	if n, err := e.SweepOutbox(ctx, be, acct); err != nil || n != 0 {
		t.Fatalf("in-window sweep sent %d (err %v), want 0", n, err)
	}
	if got := be.sends.Load(); got != 0 {
		t.Fatalf("in-window Send called %d times, want 0", got)
	}

	// After the window: the sweep delivers it and removes the source draft.
	e.Now = func() time.Time { return base.Add(6 * time.Second) }
	if n, err := e.SweepOutbox(ctx, be, acct); err != nil || n != 1 {
		t.Fatalf("post-window sweep sent %d (err %v), want 1", n, err)
	}
	if got := be.sends.Load(); got != 1 {
		t.Fatalf("post-window Send called %d times, want 1", got)
	}
	if got := be.draftDeletes.Load(); got != 1 {
		t.Fatalf("source draft deleted %d times, want 1", got)
	}
	if left, _ := s.ListSendableOutbox(ctx, acct, maxOutboxAttempts, base.Add(6*time.Second).Unix()); len(left) != 0 {
		t.Fatalf("outbox has %d sendable after send, want 0", len(left))
	}

	// A recipient-less message is rejected synchronously, not queued.
	if _, err := e.EnqueueSend(ctx, acct, model.OutgoingMessage{From: "a@example.com", Body: "x"}, 0); err == nil {
		t.Fatalf("EnqueueSend with no recipient: want error, got nil")
	}
}

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
	if _, err := s.EnqueueOutbox(ctx, acct, "thread-1", "", []byte("From: a@example.com\r\n\r\nhi"), 0); err != nil {
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
	left, err := s.ListSendableOutbox(ctx, acct, maxOutboxAttempts, 2_000_000)
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
	if _, err := s.EnqueueOutbox(ctx, acct, "thread-1", "", rfc, 0); err != nil {
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
	left, err := s.ListSendableOutbox(ctx, acct, maxOutboxAttempts, 2_000_000)
	if err != nil {
		t.Fatalf("list sendable: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("outbox still has %d sendable items, want 0 (should be marked sent)", len(left))
	}
}
