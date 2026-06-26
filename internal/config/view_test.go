package config

import (
	"path/filepath"
	"testing"
)

func TestViewStateRoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))

	if s, err := LoadViewState(); err != nil || (s != ViewState{}) {
		t.Fatalf("absent state: %+v err=%v, want zero", s, err)
	}

	want := ViewState{Folder: "SENT", UnreadOnly: true}
	if err := SaveViewState(want); err != nil {
		t.Fatalf("SaveViewState: %v", err)
	}
	got, err := LoadViewState()
	if err != nil {
		t.Fatalf("LoadViewState: %v", err)
	}
	if got != want {
		t.Fatalf("round-trip = %+v, want %+v", got, want)
	}
}
