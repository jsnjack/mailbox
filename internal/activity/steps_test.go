package activity

import (
	"context"
	"sync"
	"testing"
)

func drain(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

// A sized operation shows its bar at zero from the start and always reports
// its last unit, whatever the throttle dropped in between.
func TestStepsBarFromStartToEnd(t *testing.T) {
	h := NewHub()
	ch, unsubscribe := h.Subscribe()
	defer unsubscribe()
	s := h.BeginSteps("fetch", "a@example.com", "Prefetching message bodies", 10, "bodies")
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.Add(1) }()
	}
	wg.Wait()
	s.Done("10 bodies cached")
	s.Done("again") // a second end is ignored
	events := drain(ch)
	if len(events) < 4 {
		t.Fatalf("events = %+v, want start, 0/10, …, 10/10, done", events)
	}
	if events[0].Phase != Start || events[len(events)-1].Phase != Done || events[len(events)-1].Note != "10 bodies cached" {
		t.Fatalf("not bracketed by start and one done: %+v", events)
	}
	first, last := events[1], events[len(events)-2]
	if first.Phase != Progress || first.Done != 0 || first.Total != 10 || first.Unit != "bodies" {
		t.Fatalf("first progress = %+v, want 0 of 10 bodies", first)
	}
	if last.Phase != Progress || last.Done != 10 || last.Total != 10 {
		t.Fatalf("last progress = %+v, want 10 of 10", last)
	}
	prev := -1
	for _, e := range events[1 : len(events)-1] {
		if e.Done < prev {
			t.Fatalf("bar ran backwards: %+v", events)
		}
		prev = e.Done
	}
}

// A single unit, or a size not known yet, stays a spinner; a size learned
// later shows the bar from then on.
func TestStepsSpinnerUntilSized(t *testing.T) {
	h := NewHub()
	ch, unsubscribe := h.Subscribe()
	defer unsubscribe()
	one := h.BeginSteps("send", "", "Sending message", 1, "messages")
	one.Add(1)
	one.Done("")
	for _, e := range drain(ch) {
		if e.Phase == Progress {
			t.Fatalf("one unit drew a bar: %+v", e)
		}
	}
	late := h.BeginSteps("search", "", "Searching all mail", 0, "messages")
	late.Add(1)
	if got := drain(ch); len(got) != 1 || got[0].Phase != Start {
		t.Fatalf("unsized events = %+v, want just the start", got)
	}
	late.Set(0, 5)
	late.Set(5, 5)
	got := drain(ch)
	if len(got) != 2 || got[0].Done != 0 || got[0].Total != 5 || got[1].Done != 5 {
		t.Fatalf("sized events = %+v, want 0/5 then 5/5", got)
	}
	var nilSteps *Steps
	nilSteps.Add(1)
	nilSteps.Set(1, 2)
	nilSteps.Done("")
}

// A hook that would report its own work skips it when the caller already does.
func TestWithRow(t *testing.T) {
	if HasRow(context.Background()) {
		t.Fatal("a plain context claims a row")
	}
	if !HasRow(WithRow(context.Background())) {
		t.Fatal("a marked context lost its row")
	}
}
