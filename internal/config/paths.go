// Package config resolves XDG base directories and loads application settings.
// It uses the stdlib os helpers (and the XDG env vars they honor) rather than
// hardcoding "~", and imports no GTK code.
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// appName is the directory segment used under each XDG base directory.
const appName = "mailbox"

// ConfigDir returns ~/.config/mailbox (honoring $XDG_CONFIG_HOME).
func ConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config dir: %w", err)
	}
	return filepath.Join(base, appName), nil
}

// CacheDir returns ~/.cache/mailbox (honoring $XDG_CACHE_HOME).
func CacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve cache dir: %w", err)
	}
	return filepath.Join(base, appName), nil
}

// DataDir returns ~/.local/share/mailbox (honoring $XDG_DATA_HOME). The stdlib
// has no os.UserDataDir, so resolve XDG_DATA_HOME with the spec's fallback.
func DataDir() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve data dir: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, appName), nil
}

// ConfigFilePath returns the path to the TOML config file.
func ConfigFilePath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// DBPath returns the SQLite database path under the data directory.
func DBPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mailbox.db"), nil
}

// AttachmentsDir returns the content-addressed attachment cache directory.
func AttachmentsDir() (string, error) {
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "attachments"), nil
}

// EnsureDirs creates the config, data, and attachment directories if missing.
func EnsureDirs() error {
	cfg, err := ConfigDir()
	if err != nil {
		return err
	}
	data, err := DataDir()
	if err != nil {
		return err
	}
	att, err := AttachmentsDir()
	if err != nil {
		return err
	}
	for _, d := range []string{cfg, data, att} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}
