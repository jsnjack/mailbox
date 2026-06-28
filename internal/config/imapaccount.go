package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// IMAPAccount is the persisted connection config for one IMAP account (the
// secret — app password or OAuth refresh token — lives in the keyring, not
// here). Stored in imap-accounts.json keyed by email.
type IMAPAccount struct {
	Email        string   `json:"email"`
	Username     string   `json:"username"` // usually the email; some providers differ
	IMAPHost     string   `json:"imap_host"`
	IMAPPort     int      `json:"imap_port"`
	IMAPSecurity string   `json:"imap_security"`
	SMTPHost     string   `json:"smtp_host"`
	SMTPPort     int      `json:"smtp_port"`
	SMTPSecurity string   `json:"smtp_security"`
	Auth         AuthKind `json:"auth"`
}

func imapAccountsPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "imap-accounts.json"), nil
}

// LoadIMAPAccounts reads the IMAP connection configs, keyed by email. A missing
// file yields an empty map.
func LoadIMAPAccounts() (map[string]IMAPAccount, error) {
	path, err := imapAccountsPath()
	if err != nil {
		return map[string]IMAPAccount{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]IMAPAccount{}, nil
		}
		return map[string]IMAPAccount{}, fmt.Errorf("read imap accounts: %w", err)
	}
	var m map[string]IMAPAccount
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]IMAPAccount{}, nil // ignore a corrupt file
	}
	if m == nil {
		m = map[string]IMAPAccount{}
	}
	return m, nil
}

// LoadIMAPAccount returns the config for one email (ok=false if absent).
func LoadIMAPAccount(email string) (IMAPAccount, bool, error) {
	all, err := LoadIMAPAccounts()
	if err != nil {
		return IMAPAccount{}, false, err
	}
	a, ok := all[email]
	return a, ok, nil
}

// SaveIMAPAccount adds or replaces an account's connection config.
func SaveIMAPAccount(a IMAPAccount) error {
	all, err := LoadIMAPAccounts()
	if err != nil {
		return err
	}
	all[a.Email] = a
	return writeIMAPAccounts(all)
}

// DeleteIMAPAccount removes an account's connection config.
func DeleteIMAPAccount(email string) error {
	all, err := LoadIMAPAccounts()
	if err != nil {
		return err
	}
	delete(all, email)
	return writeIMAPAccounts(all)
}

func writeIMAPAccounts(all map[string]IMAPAccount) error {
	dir, err := DataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal imap accounts: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "imap-accounts.json"), data, 0o600); err != nil {
		return fmt.Errorf("write imap accounts: %w", err)
	}
	return nil
}
