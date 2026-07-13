package ai

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/jsnjack/mailbox/internal/logging"
)

// Config selects and configures the AI provider. It is read from the [ai] table
// of the config file; API keys are supplied separately (from the keyring/env),
// never from the file.
type Config struct {
	Provider string `toml:"provider"` // "openai" | "litellm" | "anthropic"
	Endpoint string `toml:"endpoint"` // base URL including /v1
	// Model is the single-model form, kept for existing config files; Models
	// (priority order — first is primary, the rest are fallbacks) wins when set.
	Model  string   `toml:"model"`
	Models []string `toml:"models,omitempty"`
	// Chain is the fullest form: priority-ordered [[ai.chain]] entries, each
	// optionally on its own provider/endpoint (a VPN-only proxy first, a local
	// model as fallback). When set it wins over Models/Model.
	Chain []ModelConfig `toml:"chain,omitempty"`
}

// ModelConfig is one entry of the failover chain: a model plus, optionally, its
// own provider kind and endpoint. Blank fields inherit the top-level [ai]
// values, so same-endpoint chains stay terse.
type ModelConfig struct {
	Model    string `toml:"model"`
	Provider string `toml:"provider,omitempty"`
	Endpoint string `toml:"endpoint,omitempty"`
}

// ModelList returns the models in priority order: Models when set, else the
// single Model, else empty. Legacy accessor — ResolvedChain is the full form.
func (c Config) ModelList() []string {
	if len(c.Models) > 0 {
		return c.Models
	}
	if c.Model != "" {
		return []string{c.Model}
	}
	return nil
}

// ResolvedChain returns the failover chain in priority order with inherited
// fields filled in: the [[ai.chain]] entries when present (blank provider/
// endpoint inheriting the top-level values, entries without a model dropped),
// else the legacy models list, all on the top-level provider/endpoint.
func (c Config) ResolvedChain() []ModelConfig {
	var out []ModelConfig
	if len(c.Chain) > 0 {
		for _, e := range c.Chain {
			if e.Model == "" {
				continue
			}
			if e.Provider == "" {
				e.Provider = c.Provider
			}
			if e.Endpoint == "" {
				e.Endpoint = c.Endpoint
			}
			out = append(out, e)
		}
		return out
	}
	for _, m := range c.ModelList() {
		out = append(out, ModelConfig{Model: m, Provider: c.Provider, Endpoint: c.Endpoint})
	}
	return out
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
		cfg.Models = nil // the env override pins a single model, no fallbacks
		cfg.Chain = nil  // ...on the top-level (possibly env-overridden) endpoint
		modelSrc = "env"
	}
	logging.Trace("ai: config resolved",
		"provider", cfg.Provider, "providerSrc", providerSrc,
		"endpoint", cfg.Endpoint, "endpointSrc", endpointSrc,
		"chain", chainSummary(cfg.ResolvedChain()), "modelSrc", modelSrc,
		"configured", cfg.Configured())
	return cfg, nil
}

// chainSummary renders a resolved chain compactly for trace logs.
func chainSummary(chain []ModelConfig) []string {
	out := make([]string, len(chain))
	for i, e := range chain {
		out[i] = e.Model + " @ " + e.Endpoint + " (" + e.Provider + ")"
	}
	return out
}

// SaveConfig writes cfg as the [ai] table of the TOML file at path, creating the
// directory if needed. The API key is never written here.
func SaveConfig(path string, cfg Config) error {
	// A chain whose entries all sit on the top-level provider/endpoint carries
	// no per-entry information — collapse it to the plain models list so the
	// common single-endpoint config stays terse.
	if len(cfg.Chain) > 0 {
		plain := true
		var models []string
		for _, e := range cfg.ResolvedChain() {
			if e.Provider != cfg.Provider || e.Endpoint != cfg.Endpoint {
				plain = false
				break
			}
			models = append(models, e.Model)
		}
		if plain {
			cfg.Chain = nil
			cfg.Models = models
		}
	}
	// Keep the single-model (and top-level provider/endpoint) fields mirroring
	// the primary entry, so a config written by this version still works if the
	// binary is downgraded.
	if chain := cfg.ResolvedChain(); len(chain) > 0 {
		cfg.Model = chain[0].Model
		if len(cfg.Chain) > 0 {
			cfg.Provider = chain[0].Provider
			cfg.Endpoint = chain[0].Endpoint
			cfg.Models = nil // the chain is authoritative; don't disagree
		} else if len(chain) == 1 {
			cfg.Models = nil
		}
	}
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

// Configured reports whether enough is set to build a provider: at least one
// chain entry, and every entry resolving to a full provider/endpoint/model.
func (c Config) Configured() bool {
	chain := c.ResolvedChain()
	if len(chain) == 0 {
		return false
	}
	for _, e := range chain {
		if e.Provider == "" || e.Endpoint == "" || e.Model == "" {
			return false
		}
	}
	return true
}

// KeyFunc supplies the API key for a chain entry's provider+endpoint (empty for
// keyless local endpoints). Keys come from the keyring/env, so lookup lives with
// the caller.
type KeyFunc func(provider, endpoint string) string

// StaticKey is a KeyFunc that returns the same key for every entry — the
// single-endpoint case.
func StaticKey(key string) KeyFunc {
	return func(_, _ string) string { return key }
}

// NewProvider builds a Provider from cfg, with keyFor supplying each chain
// entry's API key (nil means keyless). "openai" and "litellm" both use the
// OpenAI-compatible implementation. With more than one entry configured the
// result is a failover chain: the primary is tried first and backups take over
// when it is down or errors before producing content.
func NewProvider(cfg Config, keyFor KeyFunc) (Provider, error) {
	chain := cfg.ResolvedChain()
	logging.Trace("ai: new provider", "chain", chainSummary(chain))
	if len(chain) == 0 {
		return nil, fmt.Errorf("no ai model configured")
	}
	build := func(e ModelConfig) (Provider, error) {
		key := ""
		if keyFor != nil {
			key = keyFor(e.Provider, e.Endpoint)
		}
		switch e.Provider {
		case "openai", "litellm":
			return newOpenAIProvider(e.Endpoint, key, e.Model), nil
		case "anthropic":
			return newAnthropicProvider(e.Endpoint, key, e.Model), nil
		default:
			logging.Trace("ai: unknown provider", "provider", e.Provider, "err", "unsupported")
			return nil, fmt.Errorf("unknown ai provider %q (want openai, litellm, or anthropic)", e.Provider)
		}
	}
	if len(chain) == 1 {
		return build(chain[0])
	}
	ps := make([]Provider, len(chain))
	labels := make([]string, len(chain))
	for i, e := range chain {
		p, err := build(e)
		if err != nil {
			return nil, err
		}
		ps[i] = p
		labels[i] = e.Model + " @ " + e.Endpoint
	}
	return newFailoverProvider(ps, labels), nil
}
