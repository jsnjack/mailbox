package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jsnjack/mailbox/internal/logging"
)

func shortcutsPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "shortcuts.json"), nil
}

// LoadShortcuts reads the user's single-key shortcut overrides (action id →
// keys; "" disables the action's keys). A missing or corrupt file returns an
// empty map — the built-in defaults then apply unchanged.
func LoadShortcuts() (map[string]string, error) {
	path, err := shortcutsPath()
	if err != nil {
		return map[string]string{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		logging.Trace("config: load shortcuts failed", "path", path, "err", err)
		return map[string]string{}, fmt.Errorf("read shortcuts: %w", err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		logging.Trace("config: load shortcuts corrupt (ignored)", "path", path, "err", err)
		return map[string]string{}, nil
	}
	logging.Trace("config: load shortcuts", "n", len(m))
	return m, nil
}

// SaveShortcuts persists the shortcut overrides.
func SaveShortcuts(m map[string]string) error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal shortcuts: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shortcuts.json"), data, 0o600); err != nil {
		return fmt.Errorf("write shortcuts: %w", err)
	}
	logging.Trace("config: save shortcuts", "n", len(m))
	return nil
}
