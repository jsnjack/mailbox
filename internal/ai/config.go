package ai

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
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
	if v := os.Getenv("MAILBOX_AI_PROVIDER"); v != "" {
		cfg.Provider = v
	}
	if v := os.Getenv("MAILBOX_AI_ENDPOINT"); v != "" {
		cfg.Endpoint = v
	}
	if v := os.Getenv("MAILBOX_AI_MODEL"); v != "" {
		cfg.Model = v
	}
	return cfg, nil
}

// Configured reports whether enough is set to build a provider.
func (c Config) Configured() bool {
	return c.Provider != "" && c.Endpoint != "" && c.Model != ""
}

// NewProvider builds a Provider from cfg and the API key. "openai" and "litellm"
// both use the OpenAI-compatible implementation.
func NewProvider(cfg Config, apiKey string) (Provider, error) {
	switch cfg.Provider {
	case "openai", "litellm":
		return newOpenAIProvider(cfg.Endpoint, apiKey, cfg.Model), nil
	case "anthropic":
		return newAnthropicProvider(cfg.Endpoint, apiKey, cfg.Model), nil
	default:
		return nil, fmt.Errorf("unknown ai provider %q (want openai, litellm, or anthropic)", cfg.Provider)
	}
}
