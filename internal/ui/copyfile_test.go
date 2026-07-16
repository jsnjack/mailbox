package ui

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.pdf")
	want := []byte("%PDF-1.4\n...binary attachment bytes...\n%%EOF")
	if err := os.WriteFile(src, want, 0o600); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "chosen", "invoice.pdf")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	// Copy into a fresh destination.
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content mismatch: got %q want %q", got, want)
	}

	// Overwrite an existing destination (Save-as to an existing file).
	if err := os.WriteFile(dst, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile overwrite: %v", err)
	}
	got, _ = os.ReadFile(dst)
	if !bytes.Equal(got, want) {
		t.Fatalf("overwrite mismatch: got %q want %q", got, want)
	}

	// A missing source is reported, not silently ignored.
	if err := copyFile(filepath.Join(dir, "nope"), dst); err == nil {
		t.Fatal("expected error for missing source")
	}
}
