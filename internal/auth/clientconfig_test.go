package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadClientConfig(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantID  string
		wantErr bool
	}{
		{
			name:   "installed",
			json:   `{"installed":{"client_id":"id-1.apps.googleusercontent.com","client_secret":"secret-1"}}`,
			wantID: "id-1.apps.googleusercontent.com",
		},
		{
			name:   "web fallback",
			json:   `{"web":{"client_id":"id-2","client_secret":"secret-2"}}`,
			wantID: "id-2",
		},
		{
			name:    "missing secret",
			json:    `{"installed":{"client_id":"id-3"}}`,
			wantErr: true,
		},
		{
			name:    "invalid json",
			json:    `not json`,
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "creds.json")
			if err := os.WriteFile(path, []byte(tc.json), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := LoadClientConfig(path)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadClientConfig: %v", err)
			}
			if got.ClientID != tc.wantID {
				t.Fatalf("ClientID = %q, want %q", got.ClientID, tc.wantID)
			}
		})
	}
}

func TestLoadClientConfigMissingFile(t *testing.T) {
	if _, err := LoadClientConfig(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
