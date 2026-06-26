package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureDesktopFile(t *testing.T) {
	t.Run("writes a user entry when none exists", func(t *testing.T) {
		dataHome := t.TempDir()
		t.Setenv("XDG_DATA_HOME", dataHome)
		t.Setenv("XDG_DATA_DIRS", t.TempDir()) // empty system dir → no packaged entry

		ensureDesktopFile()

		dest := filepath.Join(dataHome, "applications", desktopFileName)
		b, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("expected desktop entry at %s: %v", dest, err)
		}
		exe, _ := os.Executable()
		if !strings.Contains(string(b), "Exec="+exe) {
			t.Errorf("Exec not rewritten to the running binary; got:\n%s", b)
		}
	})

	t.Run("no-op when a system entry already exists", func(t *testing.T) {
		dataHome := t.TempDir()
		sysData := t.TempDir()
		t.Setenv("XDG_DATA_HOME", dataHome)
		t.Setenv("XDG_DATA_DIRS", sysData)

		sysApps := filepath.Join(sysData, "applications")
		if err := os.MkdirAll(sysApps, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sysApps, desktopFileName), []byte("packaged"), 0o644); err != nil {
			t.Fatal(err)
		}

		ensureDesktopFile()

		if fileExists(filepath.Join(dataHome, "applications", desktopFileName)) {
			t.Error("should not write a user entry when a system entry exists")
		}
	})

	t.Run("does not clobber an existing user entry", func(t *testing.T) {
		dataHome := t.TempDir()
		t.Setenv("XDG_DATA_HOME", dataHome)
		t.Setenv("XDG_DATA_DIRS", t.TempDir())

		userApps := filepath.Join(dataHome, "applications")
		if err := os.MkdirAll(userApps, 0o755); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(userApps, desktopFileName)
		if err := os.WriteFile(dest, []byte("user-customized"), 0o644); err != nil {
			t.Fatal(err)
		}

		ensureDesktopFile()

		b, err := os.ReadFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "user-customized" {
			t.Errorf("existing user entry was overwritten; got %q", b)
		}
	})
}
