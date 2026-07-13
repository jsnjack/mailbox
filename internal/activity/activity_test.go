package activity

import "testing"

func TestHubPublishSubscribe(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	defer cancel()

	done := h.Begin("ai", "a@example.com", "translate")
	if got := <-ch; got.Op != "ai" || got.Phase != Start || got.Account != "a@example.com" || got.Label != "translate" {
		t.Fatalf("start event wrong: %+v", got)
	}
	done("240 tok")
	if got := <-ch; got.Phase != Done || got.Note != "240 tok" {
		t.Fatalf("done event wrong: %+v", got)
	}
}

func TestHubDropsWhenFull(t *testing.T) {
	h := NewHub()
	_, cancel := h.Subscribe() // never drained
	defer cancel()
	// Far more than the buffer; Publish must not block.
	for i := 0; i < 1000; i++ {
		h.Publish(Event{Op: "sync", Phase: Progress, Done: i, Total: 1000})
	}
}

func TestNilHubIsNoop(t *testing.T) {
	var h *Hub
	h.Publish(Event{Op: "x"}) // must not panic
	done := h.Begin("x", "", "y")
	done("z")
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	cancel()
	h.Publish(Event{Op: "sync"})
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after cancel")
	}
}
