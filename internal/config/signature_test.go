package config

import (
	"path/filepath"
	"testing"
)

func TestSignatureRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "cfg"))

	// Missing file → empty, no error.
	if s, err := LoadSignature(); err != nil || s != "" {
		t.Fatalf("absent signature: %q err=%v, want empty", s, err)
	}

	want := "Yauhen Shulitski\njsnjack\n+00 000"
	if err := SaveSignature(want); err != nil {
		t.Fatalf("SaveSignature: %v", err)
	}
	got, err := LoadSignature()
	if err != nil {
		t.Fatalf("LoadSignature: %v", err)
	}
	if got != want {
		t.Fatalf("round-trip = %q, want %q", got, want)
	}

	// Blank clears it.
	if err := SaveSignature(""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if s, _ := LoadSignature(); s != "" {
		t.Fatalf("after clear = %q, want empty", s)
	}
}
