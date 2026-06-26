package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Prefs holds general user preferences that don't belong in the [ai] config or
// the per-window state. Zero values are the defaults, so a missing file behaves
// like the out-of-the-box behaviour (remote images load).
type Prefs struct {
	// BlockRemoteImages, when true, stops the reader loading remote images by
	// default (the per-message toggle can still override). Default false.
	BlockRemoteImages bool `json:"block_remote_images"`
}

func prefsPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "prefs.json"), nil
}

// LoadPrefs reads the general preferences. A missing or unparseable file returns
// the zero value (defaults), not an error.
func LoadPrefs() (Prefs, error) {
	path, err := prefsPath()
	if err != nil {
		return Prefs{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Prefs{}, nil
		}
		return Prefs{}, fmt.Errorf("read prefs: %w", err)
	}
	var p Prefs
	if err := json.Unmarshal(data, &p); err != nil {
		return Prefs{}, nil // ignore a corrupt file
	}
	return p, nil
}

// SavePrefs persists the general preferences, creating the config dir if needed.
func SavePrefs(p Prefs) error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal prefs: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prefs.json"), data, 0o600); err != nil {
		return fmt.Errorf("write prefs: %w", err)
	}
	return nil
}
