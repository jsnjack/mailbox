// Package syncer keeps the local SQLite cache in step with Gmail: initial
// backfill, incremental history sync, and lazy body fetches. It publishes
// id-only change events over a Hub that the UI subscribes to (marshalling each
// onto the GTK main loop). It imports no GTK code.
package syncer

import "sync"

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
)

// Change is a lightweight notification carrying ids only; subscribers re-query
// the store for details so the channel stays cheap and non-blocking.
type Change struct {
	Kind      ChangeKind
	AccountID int64
	GmailID   string
	Count     int
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
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c, ok := h.subs[id]; ok {
			close(c)
			delete(h.subs, id)
		}
	}
}

// Publish delivers c to all subscribers. A slow subscriber whose buffer is full
// drops the event rather than blocking the publisher; subscribers re-query the
// store on any event, so a dropped one is recovered by the next.
func (h *Hub) Publish(c Change) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- c:
		default:
		}
	}
}
