package activity

import (
	"context"
	"sync"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
)

// StepsMin is the smallest size that shows a bar. One unit finishes in a single
// step, which the spinner already says.
const StepsMin = 2

// stepsEvery throttles progress updates: a pass can finish dozens of units a
// second, and the status bar needs a few repaints a second, not hundreds.
const stepsEvery = 250 * time.Millisecond

// Steps is a running operation of known size — so many requests, messages or
// conversations — that the status bar draws as a bar filling as the units
// finish, instead of a spinner. The size may come late (a search learns how
// many results to fetch only after listing them); until then the row is a
// spinner. A nil *Steps reports nothing. Safe for concurrent use.
type Steps struct {
	hub                      *Hub
	op, account, label, unit string

	mu          sync.Mutex
	done, total int
	shown       int // the last position published, so a bar never runs backwards
	last        time.Time
	end         func(note string)
}

// BeginSteps starts an operation like Begin, sized total units of unit
// ("messages", "bodies"); pass 0 when the size is not known yet and call Set
// once it is. A total of StepsMin or more shows the bar at once, at zero.
func (h *Hub) BeginSteps(op, account, label string, total int, unit string) *Steps {
	s := &Steps{hub: h, op: op, account: account, label: label, unit: unit}
	s.end = h.Begin(op, account, label)
	s.Set(0, total)
	return s
}

// Add records n more finished units, failed ones included: the bar shows how
// far the work has got, the closing note says how it went.
func (s *Steps) Add(n int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done += n
	s.publishLocked(false)
}

// Set records the position as done of total, replacing the size when it
// changes (a new size restarts the bar).
func (s *Steps) Set(done, total int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resized := total != s.total
	if resized {
		s.shown = 0
		logging.Trace("activity: steps planned", "op", s.op, "label", s.label, "account", s.account, "total", total, "unit", s.unit)
	}
	s.done, s.total = done, total
	s.publishLocked(resized)
}

// Done ends the operation with note, exactly like the function Begin returns.
// Later calls do nothing.
func (s *Steps) Done(note string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.end == nil {
		return
	}
	s.end(note)
	s.end = nil
}

// publishLocked reports the position, throttled to stepsEvery; the first
// position of a size and the last always show.
func (s *Steps) publishLocked(force bool) {
	if s.end == nil || s.total < StepsMin {
		return
	}
	done := max(0, min(s.done, s.total))
	if done < s.shown {
		return
	}
	now := time.Now()
	if !force && done < s.total && now.Sub(s.last) < stepsEvery {
		return
	}
	s.last, s.shown = now, done
	s.hub.Publish(Event{Op: s.op, Phase: Progress, Account: s.account, Label: s.label, Done: done, Total: s.total, Unit: s.unit})
}

type hasRowKey struct{}

// WithRow marks ctx as work that the caller already reports in a row of its
// own, typically one unit of a Steps: a hook that would open its own row for
// the work (a body fetch's "Fetching message") leaves it out, so a batch reads
// as one row with a bar rather than a spinner row per item.
func WithRow(ctx context.Context) context.Context {
	return context.WithValue(ctx, hasRowKey{}, true)
}

// HasRow reports whether ctx was marked by WithRow.
func HasRow(ctx context.Context) bool {
	v, _ := ctx.Value(hasRowKey{}).(bool)
	return v
}
