package config

import (
	"path/filepath"
	"testing"
)

func TestAccountNamesRoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))

	// Missing file → empty map, no error.
	if m, err := LoadAccountNames(); err != nil || len(m) != 0 {
		t.Fatalf("absent names: %v err=%v, want empty", m, err)
	}

	if err := SaveAccountName("a@x.com", "Home"); err != nil {
		t.Fatalf("SaveAccountName: %v", err)
	}
	if err := SaveAccountName("b@x.com", "  Work  "); err != nil { // trimmed
		t.Fatalf("SaveAccountName: %v", err)
	}
	got, err := LoadAccountNames()
	if err != nil {
		t.Fatalf("LoadAccountNames: %v", err)
	}
	if got["a@x.com"] != "Home" || got["b@x.com"] != "Work" {
		t.Fatalf("round-trip = %+v", got)
	}

	// Blank name clears the entry.
	if err := SaveAccountName("a@x.com", "   "); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ := LoadAccountNames(); len(got) != 1 || got["a@x.com"] != "" {
		t.Fatalf("after clear = %+v, want only b@x.com", got)
	}
}
