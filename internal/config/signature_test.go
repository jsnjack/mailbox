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

func TestPerAccountSignature(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "cfg"))

	// With nothing set, SignatureFor falls back to the global default.
	if err := SaveSignature("global sig"); err != nil {
		t.Fatalf("SaveSignature: %v", err)
	}
	if s, _ := SignatureFor("a@x.com"); s != "global sig" {
		t.Fatalf("fallback = %q, want global sig", s)
	}

	// A per-account signature overrides the global default for that account only.
	if err := SaveAccountSignature("a@x.com", "alice sig"); err != nil {
		t.Fatalf("SaveAccountSignature: %v", err)
	}
	if s, _ := SignatureFor("a@x.com"); s != "alice sig" {
		t.Fatalf("a@x.com = %q, want alice sig", s)
	}
	if s, _ := SignatureFor("b@x.com"); s != "global sig" {
		t.Fatalf("b@x.com = %q, want global fallback", s)
	}

	// Clearing a per-account signature (blank) removes the override, so the
	// account falls back to the global default again.
	if err := SaveAccountSignature("a@x.com", ""); err != nil {
		t.Fatalf("SaveAccountSignature clear: %v", err)
	}
	if s, _ := SignatureFor("a@x.com"); s != "global sig" {
		t.Fatalf("a@x.com after clear = %q, want global fallback", s)
	}
}
