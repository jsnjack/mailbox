// Package activity is a tiny pub/sub for transient "what is the app doing"
// events — sync, AI calls, search, body/attachment fetches. It is headless
// (imports no GTK) so the background layers can report into it; the UI
// subscribes and renders a status bar and activity log.
package activity

import (
	"fmt"
	"sync"

	"github.com/jsnjack/mailbox/internal/logging"
)

// Plural renders a count with its noun ("1 change", "6 changes") so labels and
// notes never show "(s)" constructions. Pass both forms — English plurals
// aren't all regular ("bodies").
func Plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Phase marks where an operation is in its lifecycle.
type Phase int

const (
	Start    Phase = iota // work began
	Progress              // bounded progress update (Done/Total)
	Done                  // work finished (Note may carry a result/error summary)
)

// Event is one unit of reported activity.
type Event struct {
	Op      string // category: "sync", "ai", "search", "fetch", "send", "attach", "draft", "mail"
	Phase   Phase
	Account string // email of the account the op ran for ("" = app-wide)
	Label   string // terse object, e.g. "categorize 3" or "body" ("" when the account says it all)
	Done    int    // progress numerator (Progress phase); 0 otherwise
	Total   int    // progress denominator; 0 means indeterminate
	Note    string // extra detail for the log (counts, timing, errors)
}

// Hub fans out activity events to all subscribers. The zero value is unusable;
// use NewHub. A nil *Hub is a safe no-op so callers needn't nil-check.
type Hub struct {
	mu   sync.Mutex
	subs map[int]chan Event
	next int
}

// NewHub returns a ready hub.
func NewHub() *Hub { return &Hub{subs: make(map[int]chan Event)} }

// Publish delivers e to every subscriber, dropping it for any subscriber whose
// buffer is full (activity is advisory — never block a worker on the UI).
func (h *Hub) Publish(e Event) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	logging.Trace("activity: publish", "op", e.Op, "phase", e.Phase, "label", e.Label, "done", e.Done, "total", e.Total, "note", logging.Body(e.Note), "subs", len(h.subs))
	for _, ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Begin reports a Start and returns a function that reports the matching Done;
// pass it a note (e.g. "+3 messages", "240 tok", an error string). account is
// the email the op runs for ("" for app-wide work). Typical use:
//
//	done := hub.Begin("ai", email, "translate")
//	defer func() { done("240 tok") }()
func (h *Hub) Begin(op, account, label string) func(note string) {
	h.Publish(Event{Op: op, Phase: Start, Account: account, Label: label})
	return func(note string) {
		h.Publish(Event{Op: op, Phase: Done, Account: account, Label: label, Note: note})
	}
}

// Report publishes an already-completed operation (a Done with no matching
// Start, so no duration): work that is effectively instant or only worth
// logging when it did something — a label mirror, an outbox sweep that
// delivered, a retention prune, a woken snooze.
func (h *Hub) Report(op, account, label, note string) {
	h.Publish(Event{Op: op, Phase: Done, Account: account, Label: label, Note: note})
}

// Subscribe returns a channel of events and an unsubscribe function. The channel
// is buffered; events are dropped (not blocked) when it is full.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	ch := make(chan Event, 64)
	h.subs[id] = ch
	logging.Trace("activity: subscribe", "id", id, "subs", len(h.subs))
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
			logging.Trace("activity: unsubscribe", "id", id, "subs", len(h.subs))
		}
	}
}
