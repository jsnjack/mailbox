package store

import (
	"testing"
	"time"
)

func TestBuildContacts(t *testing.T) {
	t1 := time.Unix(1000, 0)
	t2 := time.Unix(2000, 0)
	in := []contactInput{
		{FromName: "Alice", FromAddr: "alice@x.com", To: "me@x.com", When: t1},
		{FromName: "Bob", FromAddr: "bob@x.com", To: "Alice <alice@x.com>, carol@x.com", When: t2},
		{FromAddr: "me@x.com", To: "alice@x.com", When: t2}, // I wrote to Alice again
	}
	got := buildContacts(in, "me@x.com", 0)

	// Self is excluded.
	for _, c := range got {
		if c.Address == "me@x.com" {
			t.Fatalf("self address should be excluded: %+v", got)
		}
	}
	// Alice appears 3 times → ranked first; her name is captured.
	if len(got) == 0 || got[0].Address != "alice@x.com" || got[0].Count != 3 {
		t.Fatalf("expected alice first with count 3, got %+v", got)
	}
	if got[0].Name != "Alice" {
		t.Fatalf("expected Alice's name captured, got %q", got[0].Name)
	}
	if !got[0].LastSeen.Equal(t2) {
		t.Fatalf("expected LastSeen=t2, got %v", got[0].LastSeen)
	}
	// Bob and Carol each appear once.
	if len(got) != 3 {
		t.Fatalf("expected 3 contacts (alice, bob, carol), got %d: %+v", len(got), got)
	}

	// limit caps the result.
	if g := buildContacts(in, "me@x.com", 1); len(g) != 1 {
		t.Fatalf("limit=1 should return 1, got %d", len(g))
	}
}
