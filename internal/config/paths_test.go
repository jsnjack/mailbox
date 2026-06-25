package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestXDGPaths(t *testing.T) {
	root := t.TempDir()
	cfgHome := filepath.Join(root, "config")
	cacheHome := filepath.Join(root, "cache")
	dataHome := filepath.Join(root, "data")
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	tests := []struct {
		name string
		fn   func() (string, error)
		want string
	}{
		{"config dir", ConfigDir, filepath.Join(cfgHome, "mailbox")},
		{"cache dir", CacheDir, filepath.Join(cacheHome, "mailbox")},
		{"data dir", DataDir, filepath.Join(dataHome, "mailbox")},
		{"config file", ConfigFilePath, filepath.Join(cfgHome, "mailbox", "config.toml")},
		{"db path", DBPath, filepath.Join(dataHome, "mailbox", "mailbox.db")},
		{"attachments", AttachmentsDir, filepath.Join(cacheHome, "mailbox", "attachments")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.fn()
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnsureDirs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))

	if err := EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	for _, fn := range []func() (string, error){ConfigDir, DataDir, AttachmentsDir} {
		dir, _ := fn()
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			t.Fatalf("expected dir %q to exist: %v", dir, err)
		}
	}
}
