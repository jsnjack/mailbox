// Package syncer keeps the local SQLite cache in step with Gmail: initial
// backfill, incremental history sync, and lazy body fetches. It publishes
// id-only change events over a Hub that the UI subscribes to (marshalling each
// onto the GTK main loop). It imports no GTK code.
package syncer

import (
	"sync"

	"github.com/jsnjack/mailbox/internal/logging"
)

// ChangeKind identifies what changed so a subscriber can react.
type ChangeKind int

const (
	// MessageUpserted means a message's metadata was inserted or updated.
	MessageUpserted ChangeKind = iota
	// MessageDeleted means a message was removed.
	MessageDeleted
	// LabelsSynced means the account's label set was refreshed.
	LabelsSynced
	// BackfillProgress reports incremental backfill progress (Count = done so far).
	BackfillProgress
	// BackfillComplete means initial backfill finished.
	BackfillComplete
	// AuthExpired means the account needs re-authentication.
	AuthExpired
	// SendStateChanged means an outbox item was sent or its state changed.
	SendStateChanged
	// SnoozeWoke means a snoozed conversation's wake time passed — it is back
	// in the inbox (ThreadID identifies it).
	SnoozeWoke
)

// kindName returns a human-readable name for a ChangeKind, used only in trace
// logs so a published change reads as its name rather than an opaque int.
func kindName(k ChangeKind) string {
	switch k {
	case MessageUpserted:
		return "MessageUpserted"
	case MessageDeleted:
		return "MessageDeleted"
	case LabelsSynced:
		return "LabelsSynced"
	case BackfillProgress:
		return "BackfillProgress"
	case BackfillComplete:
		return "BackfillComplete"
	case AuthExpired:
		return "AuthExpired"
	case SendStateChanged:
		return "SendStateChanged"
	case SnoozeWoke:
		return "SnoozeWoke"
	default:
		return "unknown"
	}
}

// Change is a lightweight notification carrying ids only; subscribers re-query
// the store for details so the channel stays cheap and non-blocking.
type Change struct {
	Kind      ChangeKind
	AccountID int64
	GmailID   string
	// ThreadID, when set on a MessageUpserted, lets the UI re-render the open
	// conversation if the change belongs to it (a sent reply, or a synced message).
	ThreadID string
	Count    int
}

// Hub is a fan-out publish/subscribe bus for sync changes.
type Hub struct {
	mu   sync.Mutex
	subs map[int]chan Change
	next int
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[int]chan Change)}
}

// Subscribe returns a buffered channel of changes and an unsubscribe function.
func (h *Hub) Subscribe() (<-chan Change, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	ch := make(chan Change, 128)
	h.subs[id] = ch
	logging.Trace("syncer: hub subscribe", "sub", id, "subs", len(h.subs))
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c, ok := h.subs[id]; ok {
			close(c)
			delete(h.subs, id)
			logging.Trace("syncer: hub unsubscribe", "sub", id, "subs", len(h.subs))
		}
	}
}

// Publish delivers c to all subscribers. A slow subscriber whose buffer is full
// drops the event rather than blocking the publisher; subscribers re-query the
// store on any event, so a dropped one is recovered by the next.
func (h *Hub) Publish(c Change) {
	h.mu.Lock()
	defer h.mu.Unlock()
	logging.Trace("syncer: hub publish", "kind", kindName(c.Kind), "account", c.AccountID, "id", c.GmailID, "thread", c.ThreadID, "count", c.Count, "subs", len(h.subs))
	for _, ch := range h.subs {
		select {
		case ch <- c:
		default:
		}
	}
}
