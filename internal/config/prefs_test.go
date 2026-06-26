package config

import (
	"path/filepath"
	"testing"
)

func TestPrefsRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "cfg"))

	// Missing file → defaults (images load).
	if p, err := LoadPrefs(); err != nil || p.BlockRemoteImages {
		t.Fatalf("absent prefs: %+v err=%v, want default (load images)", p, err)
	}

	if err := SavePrefs(Prefs{BlockRemoteImages: true}); err != nil {
		t.Fatalf("SavePrefs: %v", err)
	}
	got, err := LoadPrefs()
	if err != nil {
		t.Fatalf("LoadPrefs: %v", err)
	}
	if !got.BlockRemoteImages {
		t.Fatalf("round-trip = %+v, want BlockRemoteImages true", got)
	}
}
