package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// accountSignaturesPath is the JSON file mapping account email → that account's
// signature, which overrides the global default (signature.txt).
func accountSignaturesPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "signatures.json"), nil
}

// LoadAccountSignatures reads per-account signatures keyed by email. A missing or
// unparseable file yields an empty map (callers fall back to the global default).
func LoadAccountSignatures() (map[string]string, error) {
	path, err := accountSignaturesPath()
	if err != nil {
		return map[string]string{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return map[string]string{}, fmt.Errorf("read signatures: %w", err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]string{}, nil // ignore a corrupt file
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}

// SaveAccountSignature sets the per-account signature for an email and persists
// the whole map. A blank signature removes the per-account override, so the
// account falls back to the global default (SignatureFor) — i.e. "blank means
// use the global signature".
func SaveAccountSignature(email, sig string) error {
	sigs, err := LoadAccountSignatures()
	if err != nil {
		return err
	}
	if strings.TrimSpace(sig) == "" {
		delete(sigs, email)
	} else {
		sigs[email] = sig
	}

	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.Marshal(sigs)
	if err != nil {
		return fmt.Errorf("marshal signatures: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "signatures.json"), data, 0o600); err != nil {
		return fmt.Errorf("write signatures: %w", err)
	}
	return nil
}

// SignatureFor returns the signature to use for an account: its own per-account
// signature when one has been set, otherwise the global default (signature.txt).
func SignatureFor(email string) (string, error) {
	sigs, err := LoadAccountSignatures()
	if err != nil {
		return "", err
	}
	if sig, ok := sigs[email]; ok {
		return sig, nil
	}
	return LoadSignature()
}
