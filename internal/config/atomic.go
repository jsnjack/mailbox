package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic durably replaces path without ever exposing a partially
// written file. The caller creates the parent directory; data is written and
// synced to a same-directory temporary file, renamed atomically, then the
// directory entry is synced. This is exported for the AI TOML writer, which
// owns the encoding but shares the application's configuration durability.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("set temporary file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open parent directory: %w", err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("sync parent directory: %w", err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("close parent directory: %w", err)
	}
	return nil
}
