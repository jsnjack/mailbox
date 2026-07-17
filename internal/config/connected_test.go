package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestConnectedTimesRoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))

	// Missing file → empty map, no error.
	if m, err := LoadConnectedTimes(); err != nil || len(m) != 0 {
		t.Fatalf("absent times: %v err=%v, want empty", m, err)
	}

	when := time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)
	if err := SaveConnectedTime("a@x.com", when); err != nil {
		t.Fatalf("SaveConnectedTime: %v", err)
	}
	if err := SaveConnectedTime("b@x.com", when.Add(24*time.Hour)); err != nil {
		t.Fatalf("SaveConnectedTime: %v", err)
	}
	got, err := LoadConnectedTimes()
	if err != nil {
		t.Fatalf("LoadConnectedTimes: %v", err)
	}
	if !got["a@x.com"].Equal(when) || !got["b@x.com"].Equal(when.Add(24*time.Hour)) {
		t.Fatalf("round-trip = %+v", got)
	}

	// Zero time clears the entry.
	if err := SaveConnectedTime("a@x.com", time.Time{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ := LoadConnectedTimes(); len(got) != 1 || !got["a@x.com"].IsZero() {
		t.Fatalf("after clear = %+v, want only b@x.com", got)
	}
}
