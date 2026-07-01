package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jsnjack/mailbox/internal/logging"
)

// accountNamesPath is the JSON file mapping account email → user-assigned
// display name (e.g. "Home", "Work").
func accountNamesPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "accounts.json"), nil
}

// LoadAccountNames reads the user-assigned account display names, keyed by
// email. A missing or unparseable file yields an empty map, not an error, so the
// caller can always fall back to the email itself.
func LoadAccountNames() (map[string]string, error) {
	path, err := accountNamesPath()
	if err != nil {
		return map[string]string{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logging.Trace("config: load account names (none)", "path", path)
			return map[string]string{}, nil
		}
		logging.Trace("config: load account names failed", "path", path, "err", err)
		return map[string]string{}, fmt.Errorf("read account names: %w", err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		logging.Trace("config: load account names corrupt (ignored)", "path", path, "err", err)
		return map[string]string{}, nil // ignore a corrupt file
	}
	if m == nil {
		m = map[string]string{}
	}
	logging.Trace("config: load account names", "path", path, "count", len(m))
	return m, nil
}

// SaveAccountName sets (or, when name is blank, clears) the display name for an
// account email and persists the whole map.
func SaveAccountName(email, name string) error {
	names, err := LoadAccountNames()
	if err != nil {
		return err
	}
	cleared := false
	if name = strings.TrimSpace(name); name == "" {
		delete(names, email)
		cleared = true
	} else {
		names[email] = name
	}

	dir, err := DataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	data, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("marshal account names: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), data, 0o600); err != nil {
		logging.Trace("config: save account name failed", "email", email, "err", err)
		return fmt.Errorf("write account names: %w", err)
	}
	logging.Trace("config: save account name", "email", email, "cleared", cleared, "count", len(names))
	return nil
}
