package config

import (
	"path/filepath"
	"testing"
)

func TestWindowStateRoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))

	// Missing file → zero value, no error.
	if s, err := LoadWindowState(); err != nil || (s != WindowState{}) {
		t.Fatalf("absent state: %+v err=%v, want zero", s, err)
	}

	want := WindowState{Width: 1024, Height: 720}
	if err := SaveWindowState(want); err != nil {
		t.Fatalf("SaveWindowState: %v", err)
	}
	got, err := LoadWindowState()
	if err != nil {
		t.Fatalf("LoadWindowState: %v", err)
	}
	if got != want {
		t.Fatalf("round-trip = %+v, want %+v", got, want)
	}
}
