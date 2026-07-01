package ai

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/jsnjack/mailbox/internal/logging"
)

// Config selects and configures the AI provider. It is read from the [ai] table
// of the config file; the API key is supplied separately (from the keyring/env),
// never from the file.
type Config struct {
	Provider string `toml:"provider"` // "openai" | "litellm" | "anthropic"
	Endpoint string `toml:"endpoint"` // base URL including /v1
	Model    string `toml:"model"`
}

type fileConfig struct {
	AI Config `toml:"ai"`
}

// LoadConfig reads the [ai] table from the TOML file at path (absent file is not
// an error), then applies MAILBOX_AI_* environment overrides.
func LoadConfig(path string) (Config, error) {
	var fc fileConfig
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, &fc); err != nil {
			return Config{}, fmt.Errorf("parse ai config %s: %w", path, err)
		}
	}
	cfg := fc.AI
	providerSrc, endpointSrc, modelSrc := "config", "config", "config"
	if v := os.Getenv("MAILBOX_AI_PROVIDER"); v != "" {
		cfg.Provider = v
		providerSrc = "env"
	}
	if v := os.Getenv("MAILBOX_AI_ENDPOINT"); v != "" {
		cfg.Endpoint = v
		endpointSrc = "env"
	}
	if v := os.Getenv("MAILBOX_AI_MODEL"); v != "" {
		cfg.Model = v
		modelSrc = "env"
	}
	logging.Trace("ai: config resolved",
		"provider", cfg.Provider, "providerSrc", providerSrc,
		"endpoint", cfg.Endpoint, "endpointSrc", endpointSrc,
		"model", cfg.Model, "modelSrc", modelSrc,
		"configured", cfg.Configured())
	return cfg, nil
}

// SaveConfig writes cfg as the [ai] table of the TOML file at path, creating the
// directory if needed. The API key is never written here.
func SaveConfig(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create config: %w", err)
	}
	if err := toml.NewEncoder(f).Encode(fileConfig{AI: cfg}); err != nil {
		_ = f.Close()
		return fmt.Errorf("encode config: %w", err)
	}
	// Close error matters on a written file — it can surface a failed flush.
	if err := f.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	return nil
}

// Configured reports whether enough is set to build a provider.
func (c Config) Configured() bool {
	return c.Provider != "" && c.Endpoint != "" && c.Model != ""
}

// NewProvider builds a Provider from cfg and the API key. "openai" and "litellm"
// both use the OpenAI-compatible implementation.
func NewProvider(cfg Config, apiKey string) (Provider, error) {
	logging.Trace("ai: new provider",
		"provider", cfg.Provider, "endpoint", cfg.Endpoint, "model", cfg.Model,
		"hasKey", apiKey != "", "keyLen", len(apiKey))
	switch cfg.Provider {
	case "openai", "litellm":
		return newOpenAIProvider(cfg.Endpoint, apiKey, cfg.Model), nil
	case "anthropic":
		return newAnthropicProvider(cfg.Endpoint, apiKey, cfg.Model), nil
	default:
		logging.Trace("ai: unknown provider", "provider", cfg.Provider, "err", "unsupported")
		return nil, fmt.Errorf("unknown ai provider %q (want openai, litellm, or anthropic)", cfg.Provider)
	}
}
