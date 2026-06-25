package ai

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractOpenAIDelta(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		wantText string
		wantDone bool
	}{
		{"content", `{"choices":[{"delta":{"content":"hello"}}]}`, "hello", false},
		{"finish", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "", true},
		{"empty choices", `{"choices":[]}`, "", false},
		{"garbage", `not json`, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, done := extractOpenAIDelta([]byte(tc.data))
			if text != tc.wantText || done != tc.wantDone {
				t.Fatalf("got (%q,%v), want (%q,%v)", text, done, tc.wantText, tc.wantDone)
			}
		})
	}
}

func TestExtractAnthropicDelta(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		wantText string
		wantDone bool
	}{
		{"text delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`, "hi", false},
		{"stop", `{"type":"message_stop"}`, "", true},
		{"ping", `{"type":"ping"}`, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, done := extractAnthropicDelta([]byte(tc.data))
			if text != tc.wantText || done != tc.wantDone {
				t.Fatalf("got (%q,%v), want (%q,%v)", text, done, tc.wantText, tc.wantDone)
			}
		})
	}
}

func TestOpenAIMessagesPrependsSystem(t *testing.T) {
	msgs := openAIMessages("sys", []Msg{{Role: RoleUser, Content: "hi"}})
	if len(msgs) != 2 || msgs[0]["role"] != "system" || msgs[1]["role"] != "user" {
		t.Fatalf("unexpected messages: %v", msgs)
	}
}

func TestAnthropicMessagesDropsSystem(t *testing.T) {
	msgs := anthropicMessages([]Msg{{Role: RoleSystem, Content: "sys"}, {Role: RoleUser, Content: "hi"}})
	if len(msgs) != 1 || msgs[0]["role"] != "user" {
		t.Fatalf("expected system dropped, got %v", msgs)
	}
}

func TestLoadConfigAndOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[ai]\nprovider=\"litellm\"\nendpoint=\"http://argus:4000/v1\"\nmodel=\"gpt-4o\"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Configured() || cfg.Provider != "litellm" || cfg.Model != "gpt-4o" {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}

	t.Setenv("MAILBOX_AI_MODEL", "claude-x")
	cfg, _ = LoadConfig(path)
	if cfg.Model != "claude-x" {
		t.Fatalf("env override not applied: %+v", cfg)
	}
}

func TestNewProvider(t *testing.T) {
	for _, p := range []string{"openai", "litellm", "anthropic"} {
		if _, err := NewProvider(Config{Provider: p, Endpoint: "http://x/v1", Model: "m"}, "k"); err != nil {
			t.Fatalf("provider %q: %v", p, err)
		}
	}
	if _, err := NewProvider(Config{Provider: "bogus"}, "k"); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestSaveConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml") // dir created by SaveConfig
	want := Config{Provider: "anthropic", Endpoint: "https://api.anthropic.com/v1", Model: "claude-sonnet-4-6"}
	if err := SaveConfig(path, want); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got != want {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestMissingConfigFileNotAnError(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("absent file should not error: %v", err)
	}
	if cfg.Configured() {
		t.Fatal("absent config should not be Configured")
	}
}
