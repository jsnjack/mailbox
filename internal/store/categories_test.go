package store

import (
	"context"
	"testing"
)

func TestMessageCategories(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	// Empty input returns an empty map without error.
	got, err := s.MessageCategories(ctx, acc, nil)
	if err != nil {
		t.Fatalf("MessageCategories(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty input: got %v, want empty", got)
	}

	// Set a couple, including an empty "no tag" category (which must round-trip,
	// so it isn't re-classified).
	if err := s.SetMessageCategory(ctx, acc, "m1", "Needs reply"); err != nil {
		t.Fatalf("SetMessageCategory m1: %v", err)
	}
	if err := s.SetMessageCategory(ctx, acc, "m2", ""); err != nil {
		t.Fatalf("SetMessageCategory m2: %v", err)
	}

	got, err = s.MessageCategories(ctx, acc, []string{"m1", "m2", "m3"})
	if err != nil {
		t.Fatalf("MessageCategories: %v", err)
	}
	if got["m1"] != "Needs reply" {
		t.Fatalf("m1 = %q, want %q", got["m1"], "Needs reply")
	}
	if v, ok := got["m2"]; !ok || v != "" {
		t.Fatalf(`m2 = (%q, %v), want ("", true)`, v, ok)
	}
	if _, ok := got["m3"]; ok {
		t.Fatalf("m3 should be absent (never classified), got %q", got["m3"])
	}

	// Upsert overwrites the previous category.
	if err := s.SetMessageCategory(ctx, acc, "m1", "Receipt"); err != nil {
		t.Fatalf("SetMessageCategory m1 update: %v", err)
	}
	got, _ = s.MessageCategories(ctx, acc, []string{"m1"})
	if got["m1"] != "Receipt" {
		t.Fatalf("after update m1 = %q, want %q", got["m1"], "Receipt")
	}
}
