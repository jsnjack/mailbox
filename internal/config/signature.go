package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// signaturePath is the file holding the user's default email signature (plain
// text, possibly multi-line).
func signaturePath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "signature.txt"), nil
}

// LoadSignature reads the configured default signature. A missing file is not
// an error — it returns an empty string (no signature).
func LoadSignature() (string, error) {
	path, err := signaturePath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read signature: %w", err)
	}
	return string(data), nil
}

// SaveSignature persists the default signature (creating the config dir if
// needed). A blank signature removes the file.
func SaveSignature(sig string) error {
	path, err := signaturePath()
	if err != nil {
		return err
	}
	if sig == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove signature: %w", err)
		}
		return nil
	}
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(sig), 0o600); err != nil {
		return fmt.Errorf("write signature: %w", err)
	}
	return nil
}
