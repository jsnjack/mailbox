package backend

import "context"

// Priority orders provider requests that share a rate limit (Gmail's per-user
// quota). When the quota runs short, a waiting request of a more urgent
// priority goes first, so opening a message during a long catch-up does not
// queue behind hundreds of background fetches.
type Priority int

const (
	// PriorityUser is the default: someone is waiting for the result (opening a
	// message, searching, sending, archiving).
	PriorityUser Priority = iota
	// PriorityInbox brings the visible inbox up to date: its new messages and
	// their bodies.
	PriorityInbox
	// PriorityBackground is bulk catch-up nobody is looking at yet.
	PriorityBackground
)

func (p Priority) String() string {
	switch p {
	case PriorityUser:
		return "user"
	case PriorityInbox:
		return "inbox"
	case PriorityBackground:
		return "background"
	}
	return "unknown"
}

type priorityKey struct{}

// WithPriority returns ctx carrying p for the provider requests made under it.
func WithPriority(ctx context.Context, p Priority) context.Context {
	return context.WithValue(ctx, priorityKey{}, p)
}

// PriorityOf returns the priority ctx carries, PriorityUser when none.
func PriorityOf(ctx context.Context) Priority {
	if p, ok := ctx.Value(priorityKey{}).(Priority); ok {
		return p
	}
	return PriorityUser
}
