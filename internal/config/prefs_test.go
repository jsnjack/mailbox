package config

import (
	"path/filepath"
	"testing"
)

func TestPrefsRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "cfg"))

	// Missing file → privacy-safe default (external images blocked).
	if p, err := LoadPrefs(); err != nil || !p.BlockRemoteImages {
		t.Fatalf("absent prefs: %+v err=%v, want images blocked", p, err)
	}

	if err := SavePrefs(Prefs{BlockRemoteImages: false, RemoteImagesConfigured: true}); err != nil {
		t.Fatalf("SavePrefs: %v", err)
	}
	got, err := LoadPrefs()
	if err != nil {
		t.Fatalf("LoadPrefs: %v", err)
	}
	if got.BlockRemoteImages {
		t.Fatalf("round-trip = %+v, want BlockRemoteImages false", got)
	}
}

func TestPrefsMigratesOldRemoteImageDefaultToBlocked(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "cfg"))
	if err := SavePrefs(Prefs{BlockRemoteImages: false}); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPrefs()
	if err != nil || !p.BlockRemoteImages {
		t.Fatalf("old prefs migration = %+v, %v", p, err)
	}
}
