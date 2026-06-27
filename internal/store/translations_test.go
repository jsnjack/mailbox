package store

import (
	"context"
	"testing"
)

func TestTranslations(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	// Empty input is a no-op, not an error.
	got, err := s.Translations(ctx, acc, nil, "English")
	if err != nil {
		t.Fatalf("Translations(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty input: got %v, want empty", got)
	}

	if err := s.SetTranslation(ctx, acc, "m1", "English", "<p>Hello</p>"); err != nil {
		t.Fatalf("SetTranslation m1: %v", err)
	}
	// A different target language is a distinct row.
	if err := s.SetTranslation(ctx, acc, "m1", "French", "<p>Bonjour</p>"); err != nil {
		t.Fatalf("SetTranslation m1 French: %v", err)
	}

	got, err = s.Translations(ctx, acc, []string{"m1", "m2"}, "English")
	if err != nil {
		t.Fatalf("Translations: %v", err)
	}
	if got["m1"] != "<p>Hello</p>" {
		t.Fatalf("m1 English = %q, want %q", got["m1"], "<p>Hello</p>")
	}
	if _, ok := got["m2"]; ok {
		t.Fatalf("m2 should be absent, got %q", got["m2"])
	}
	if fr, _ := s.Translations(ctx, acc, []string{"m1"}, "French"); fr["m1"] != "<p>Bonjour</p>" {
		t.Fatalf("m1 French = %q, want %q", fr["m1"], "<p>Bonjour</p>")
	}

	// Upsert overwrites the same (id, lang).
	if err := s.SetTranslation(ctx, acc, "m1", "English", "<p>Hi</p>"); err != nil {
		t.Fatalf("SetTranslation m1 update: %v", err)
	}
	got, _ = s.Translations(ctx, acc, []string{"m1"}, "English")
	if got["m1"] != "<p>Hi</p>" {
		t.Fatalf("after update m1 = %q, want %q", got["m1"], "<p>Hi</p>")
	}
}
