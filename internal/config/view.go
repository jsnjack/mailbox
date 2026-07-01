package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jsnjack/mailbox/internal/logging"
)

// ViewState remembers the last-used folder and unread filter so the app reopens
// where the user left off.
type ViewState struct {
	Folder        string  `json:"folder"`
	UnreadOnly    bool    `json:"unread_only"`
	Zoom          float64 `json:"zoom"` // reader zoom level (0 = default 1.0)
	ComposeWidth  int     `json:"compose_width"`
	ComposeHeight int     `json:"compose_height"`
}

func viewStatePath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "view.json"), nil
}

// LoadViewState reads the saved view state. A missing or unparseable file
// returns the zero value (no error), so the caller falls back to defaults.
func LoadViewState() (ViewState, error) {
	path, err := viewStatePath()
	if err != nil {
		return ViewState{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logging.Trace("config: load view state (defaults)", "path", path)
			return ViewState{}, nil
		}
		logging.Trace("config: load view state failed", "path", path, "err", err)
		return ViewState{}, fmt.Errorf("read view state: %w", err)
	}
	var s ViewState
	if err := json.Unmarshal(data, &s); err != nil {
		logging.Trace("config: load view state corrupt (ignored)", "path", path, "err", err)
		return ViewState{}, nil // ignore a corrupt file
	}
	logging.Trace("config: load view state", "path", path, "folder", s.Folder, "unreadOnly", s.UnreadOnly, "zoom", s.Zoom, "composeW", s.ComposeWidth, "composeH", s.ComposeHeight)
	return s, nil
}

// SaveViewState persists the view state, creating the data dir if needed.
func SaveViewState(s ViewState) error {
	dir, err := DataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal view state: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "view.json"), data, 0o600); err != nil {
		logging.Trace("config: save view state failed", "err", err)
		return fmt.Errorf("write view state: %w", err)
	}
	logging.Trace("config: save view state", "folder", s.Folder, "unreadOnly", s.UnreadOnly, "zoom", s.Zoom, "composeW", s.ComposeWidth, "composeH", s.ComposeHeight)
	return nil
}
