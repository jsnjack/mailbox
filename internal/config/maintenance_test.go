package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClearAttachmentsCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))

	// Missing cache dir → no error, nothing freed.
	if freed, err := ClearAttachmentsCache(); err != nil || freed != 0 {
		t.Fatalf("absent cache: freed=%d err=%v, want 0/nil", freed, err)
	}

	dir, err := AttachmentsDir()
	if err != nil {
		t.Fatalf("AttachmentsDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), make([]byte, 100), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b"), make([]byte, 50), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	freed, err := ClearAttachmentsCache()
	if err != nil {
		t.Fatalf("ClearAttachmentsCache: %v", err)
	}
	if freed != 150 {
		t.Fatalf("freed = %d, want 150", freed)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("cache not empty after clear: %d entries", len(entries))
	}
}
