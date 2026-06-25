package auth

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadClientConfig reads a Google Cloud OAuth client credentials JSON file (the
// "Desktop app" download, whose top-level key is "installed") and returns the
// client ID and secret. The "web" key is also accepted as a fallback.
func LoadClientConfig(path string) (ClientConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ClientConfig{}, fmt.Errorf("read credentials file: %w", err)
	}
	var f struct {
		Installed *clientCreds `json:"installed"`
		Web       *clientCreds `json:"web"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return ClientConfig{}, fmt.Errorf("parse credentials JSON: %w", err)
	}
	creds := f.Installed
	if creds == nil {
		creds = f.Web
	}
	if creds == nil || creds.ClientID == "" || creds.ClientSecret == "" {
		return ClientConfig{}, fmt.Errorf("credentials file %q missing installed/web client_id and client_secret", path)
	}
	return ClientConfig{ClientID: creds.ClientID, ClientSecret: creds.ClientSecret}, nil
}

type clientCreds struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}
