package config

import (
	"path/filepath"
	"testing"
)

func TestPrefsRemoteImages(t *testing.T) {
	tests := []struct {
		name string
		save *Prefs
		want bool
	}{
		{name: "fresh profile loads automatically", want: false},
		{name: "automatic loading round trips", save: &Prefs{BlockRemoteImages: false}, want: false},
		{name: "explicit blocking round trips", save: &Prefs{BlockRemoteImages: true}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "cfg"))
			if tt.save != nil {
				if err := SavePrefs(*tt.save); err != nil {
					t.Fatalf("SavePrefs: %v", err)
				}
			}
			got, err := LoadPrefs()
			if err != nil {
				t.Fatalf("LoadPrefs: %v", err)
			}
			if got.BlockRemoteImages != tt.want {
				t.Fatalf("BlockRemoteImages = %v, want %v", got.BlockRemoteImages, tt.want)
			}
		})
	}
}

func TestPrefsAILanguage(t *testing.T) {
	tests := []struct {
		name string
		save *Prefs
		want string
	}{
		{name: "fresh profile defaults to the zero value (English)", want: ""},
		{name: "a language round trips", save: &Prefs{AILanguage: "Portuguese"}, want: "Portuguese"},
		{name: "match round trips", save: &Prefs{AILanguage: "match"}, want: "match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "cfg"))
			if tt.save != nil {
				if err := SavePrefs(*tt.save); err != nil {
					t.Fatalf("SavePrefs: %v", err)
				}
			}
			got, err := LoadPrefs()
			if err != nil {
				t.Fatalf("LoadPrefs: %v", err)
			}
			if got.AILanguage != tt.want {
				t.Fatalf("AILanguage = %q, want %q", got.AILanguage, tt.want)
			}
		})
	}
}
