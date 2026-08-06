package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jsnjack/mailbox/internal/logging"
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
	return clearCacheDir(dir, "attachments")
}

// ClearMailCache removes all re-downloadable mail files: attachments and
// external images that were explicitly allowed and cached for offline use.
func ClearMailCache() (int64, error) {
	attachments, err := AttachmentsDir()
	if err != nil {
		return 0, err
	}
	remote, err := RemoteImagesDir()
	if err != nil {
		return 0, err
	}
	var freed int64
	for _, dir := range []string{attachments, remote} {
		n, err := clearCacheDir(dir, "mail")
		freed += n
		if err != nil {
			return freed, err
		}
	}
	return freed, nil
}

func clearCacheDir(dir, kind string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logging.Trace("config: clear cache (none)", "kind", kind, "path", dir)
			return 0, nil
		}
		logging.Trace("config: clear cache read failed", "kind", kind, "path", dir, "err", err)
		return 0, fmt.Errorf("read cache dir: %w", err)
	}
	var freed int64
	removed := 0
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		freed += pathSize(path)
		if err := os.RemoveAll(path); err != nil {
			logging.Trace("config: clear cache remove failed", "kind", kind, "path", dir, "removed", removed, "bytes", freed, "err", err)
			return freed, fmt.Errorf("remove cached file: %w", err)
		}
		removed++
	}
	logging.Trace("config: clear cache", "kind", kind, "path", dir, "removed", removed, "bytes", freed)
	return freed, nil
}

func pathSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			if info, infoErr := entry.Info(); infoErr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
