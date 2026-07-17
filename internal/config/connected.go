package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
)

// connectedPath is the JSON file mapping account email → the moment its
// credential was last saved (an interactive sign-in or reconnect), RFC 3339.
// It exists so the UI can say how old a sign-in is — Google OAuth apps left in
// "Testing" publishing status expire refresh tokens after 7 days, and without
// the sign-in date an expiry looks arbitrary.
func connectedPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "connected.json"), nil
}

// LoadConnectedTimes reads the per-account sign-in times, keyed by email. A
// missing or unparseable file yields an empty map, not an error, so callers can
// always treat "no entry" as "unknown".
func LoadConnectedTimes() (map[string]time.Time, error) {
	path, err := connectedPath()
	if err != nil {
		return map[string]time.Time{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logging.Trace("config: load connected times (none)", "path", path)
			return map[string]time.Time{}, nil
		}
		logging.Trace("config: load connected times failed", "path", path, "err", err)
		return map[string]time.Time{}, fmt.Errorf("read connected times: %w", err)
	}
	var m map[string]time.Time
	if err := json.Unmarshal(data, &m); err != nil {
		logging.Trace("config: load connected times corrupt (ignored)", "path", path, "err", err)
		return map[string]time.Time{}, nil // ignore a corrupt file
	}
	if m == nil {
		m = map[string]time.Time{}
	}
	logging.Trace("config: load connected times", "path", path, "count", len(m))
	return m, nil
}

// SaveConnectedTime records when an account's credential was saved (or, when t
// is the zero time, clears the entry) and persists the whole map.
func SaveConnectedTime(email string, t time.Time) error {
	times, err := LoadConnectedTimes()
	if err != nil {
		return err
	}
	cleared := t.IsZero()
	if cleared {
		delete(times, email)
	} else {
		times[email] = t
	}

	dir, err := DataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	data, err := json.Marshal(times)
	if err != nil {
		return fmt.Errorf("marshal connected times: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "connected.json"), data, 0o600); err != nil {
		logging.Trace("config: save connected time failed", "email", email, "err", err)
		return fmt.Errorf("write connected times: %w", err)
	}
	logging.Trace("config: save connected time", "email", email, "cleared", cleared, "count", len(times))
	return nil
}
