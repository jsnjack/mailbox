package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DBSize returns the size in bytes of the SQLite database file (the main file,
// excluding the WAL/shm sidecars). A missing file reports 0, not an error.
func DBSize() (int64, error) {
	path, err := DBPath()
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat database: %w", err)
	}
	return fi.Size(), nil
}

// ClearAttachmentsCache deletes every cached attachment file, returning the
// number of bytes freed. Attachments are content-addressed and re-downloadable,
// so this is always safe. A missing cache dir is not an error.
func ClearAttachmentsCache() (int64, error) {
	dir, err := AttachmentsDir()
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read attachments dir: %w", err)
	}
	var freed int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			freed += info.Size()
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return freed, fmt.Errorf("remove cached attachment: %w", err)
		}
	}
	return freed, nil
}
