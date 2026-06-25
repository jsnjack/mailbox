package syncer

import "testing"

func TestHubDelivers(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe()
	defer unsub()

	h.Publish(Change{Kind: MessageUpserted, AccountID: 1, GmailID: "m1"})
	select {
	case c := <-ch:
		if c.GmailID != "m1" || c.AccountID != 1 {
			t.Fatalf("got %+v", c)
		}
	default:
		t.Fatal("expected an event")
	}
}

func TestHubFanOut(t *testing.T) {
	h := NewHub()
	c1, _ := h.Subscribe()
	c2, _ := h.Subscribe()

	h.Publish(Change{AccountID: 7})
	for i, ch := range []<-chan Change{c1, c2} {
		select {
		case c := <-ch:
			if c.AccountID != 7 {
				t.Fatalf("subscriber %d: got %+v", i, c)
			}
		default:
			t.Fatalf("subscriber %d: no event", i)
		}
	}
}

func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe()
	unsub()
	// Publishing after unsubscribe must not panic and must not deliver.
	h.Publish(Change{AccountID: 1})
	if _, ok := <-ch; ok {
		t.Fatal("expected closed channel after unsubscribe")
	}
}
