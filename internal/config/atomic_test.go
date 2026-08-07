package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomicReplacesAndCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("read = %q, %v; want new", got, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	left, err := filepath.Glob(filepath.Join(dir, ".prefs.json.tmp-*"))
	if err != nil || len(left) != 0 {
		t.Fatalf("temporary files = %v, %v; want none", left, err)
	}
}
