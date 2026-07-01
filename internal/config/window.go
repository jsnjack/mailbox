package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jsnjack/mailbox/internal/logging"
)

// WindowState is the persisted main-window geometry, remembered across launches.
type WindowState struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func windowStatePath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "window.json"), nil
}

// LoadWindowState reads the saved window geometry. A missing or unparseable
// file is not an error: it returns the zero value so the caller falls back to a
// default size.
func LoadWindowState() (WindowState, error) {
	path, err := windowStatePath()
	if err != nil {
		return WindowState{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logging.Trace("config: load window state (defaults)", "path", path)
			return WindowState{}, nil
		}
		logging.Trace("config: load window state failed", "path", path, "err", err)
		return WindowState{}, fmt.Errorf("read window state: %w", err)
	}
	var s WindowState
	if err := json.Unmarshal(data, &s); err != nil {
		logging.Trace("config: load window state corrupt (ignored)", "path", path, "err", err)
		return WindowState{}, nil // ignore a corrupt state file
	}
	logging.Trace("config: load window state", "path", path, "width", s.Width, "height", s.Height)
	return s, nil
}

// SaveWindowState persists the window geometry, creating the data dir if needed.
func SaveWindowState(s WindowState) error {
	dir, err := DataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal window state: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "window.json"), data, 0o600); err != nil {
		logging.Trace("config: save window state failed", "err", err)
		return fmt.Errorf("write window state: %w", err)
	}
	logging.Trace("config: save window state", "width", s.Width, "height", s.Height)
	return nil
}
