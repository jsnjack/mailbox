package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jsnjack/mailbox/internal/logging"
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
			logging.Trace("config: load signature (none)", "path", path)
			return "", nil
		}
		logging.Trace("config: load signature failed", "path", path, "err", err)
		return "", fmt.Errorf("read signature: %w", err)
	}
	logging.Trace("config: load signature", "path", path, "len", len(data))
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
		logging.Trace("config: save signature (removed, blank)", "path", path)
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
		logging.Trace("config: save signature failed", "path", path, "err", err)
		return fmt.Errorf("write signature: %w", err)
	}
	logging.Trace("config: save signature", "path", path, "len", len(sig))
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
			logging.Trace("config: load account signatures (none)", "path", path)
			return map[string]string{}, nil
		}
		logging.Trace("config: load account signatures failed", "path", path, "err", err)
		return map[string]string{}, fmt.Errorf("read signatures: %w", err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		logging.Trace("config: load account signatures corrupt (ignored)", "path", path, "err", err)
		return map[string]string{}, nil // ignore a corrupt file
	}
	if m == nil {
		m = map[string]string{}
	}
	logging.Trace("config: load account signatures", "path", path, "count", len(m))
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
	cleared := false
	if strings.TrimSpace(sig) == "" {
		delete(sigs, email)
		cleared = true
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
		logging.Trace("config: save account signature failed", "email", email, "err", err)
		return fmt.Errorf("write signatures: %w", err)
	}
	logging.Trace("config: save account signature", "email", email, "cleared", cleared, "len", len(sig), "count", len(sigs))
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
		logging.Trace("config: signature resolved", "email", email, "source", "override", "len", len(sig))
		return sig, nil
	}
	logging.Trace("config: signature resolved", "email", email, "source", "default")
	return LoadSignature()
}
