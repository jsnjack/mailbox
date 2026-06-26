package ai

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProvider records the prompt it was given and replays canned chunks.
type fakeProvider struct {
	gotSystem string
	gotMsgs   []Msg
	chunks    []Chunk
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Stream(_ context.Context, system string, msgs []Msg) (<-chan Chunk, error) {
	f.gotSystem, f.gotMsgs = system, msgs
	ch := make(chan Chunk, len(f.chunks))
	for _, c := range f.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

func TestSummarizeThread(t *testing.T) {
	fp := &fakeProvider{chunks: []Chunk{{Text: "- A asked B\n"}, {Text: "- B agreed"}}}
	a := NewAssistant(fp)
	ch, err := a.SummarizeThread(context.Background(), "From: A\nSubject: Hi\n\nPlease confirm.")
	if err != nil {
		t.Fatalf("SummarizeThread: %v", err)
	}
	var b strings.Builder
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("chunk err: %v", c.Err)
		}
		b.WriteString(c.Text)
	}
	if got := b.String(); got != "- A asked B\n- B agreed" {
		t.Fatalf("summary = %q", got)
	}
	// The thread text must reach the model as the user turn; the system prompt
	// must instruct a bullet summary.
	if len(fp.gotMsgs) != 1 || !strings.Contains(fp.gotMsgs[0].Content, "Please confirm.") {
		t.Fatalf("thread context not passed as user message: %+v", fp.gotMsgs)
	}
	if !strings.Contains(fp.gotSystem, "bullet") {
		t.Fatalf("system prompt missing bullet instruction: %q", fp.gotSystem)
	}
	// Summaries must always be produced in English.
	if !strings.Contains(fp.gotSystem, "English") {
		t.Fatalf("system prompt should force English: %q", fp.gotSystem)
	}
}

func TestAnalyzeEmail(t *testing.T) {
	fp := &fakeProvider{chunks: []Chunk{{Text: "Verdict: Be cautious\n"}, {Text: "- urgent tone"}}}
	a := NewAssistant(fp)
	ch, err := a.AnalyzeEmail(context.Background(), "From: x@evil.example\nSubject: Verify now\n\nClick here")
	if err != nil {
		t.Fatalf("AnalyzeEmail: %v", err)
	}
	for range ch {
	}
	if len(fp.gotMsgs) != 1 || !strings.Contains(fp.gotMsgs[0].Content, "evil.example") {
		t.Fatalf("email context not passed: %+v", fp.gotMsgs)
	}
	if !strings.Contains(fp.gotSystem, "phishing") || !strings.Contains(fp.gotSystem, "Verdict:") {
		t.Fatalf("system prompt missing phishing/verdict instruction: %q", fp.gotSystem)
	}
}

func TestPing(t *testing.T) {
	if err := NewAssistant(&fakeProvider{chunks: []Chunk{{Text: "OK"}}}).Ping(context.Background()); err != nil {
		t.Fatalf("Ping ok: %v", err)
	}
	wantErr := &fakeProvider{chunks: []Chunk{{Err: context.DeadlineExceeded}}}
	if err := NewAssistant(wantErr).Ping(context.Background()); err == nil {
		t.Fatal("Ping should surface a stream error")
	}
}

func TestDraftNew(t *testing.T) {
	fp := &fakeProvider{chunks: []Chunk{{Text: "Hello,\n"}, {Text: "Let's meet Tuesday."}}}
	a := NewAssistant(fp)
	ch, err := a.DraftNew(context.Background(), "Project sync", "Propose a meeting")
	if err != nil {
		t.Fatalf("DraftNew: %v", err)
	}
	var b strings.Builder
	for c := range ch {
		b.WriteString(c.Text)
	}
	if got := b.String(); got != "Hello,\nLet's meet Tuesday." {
		t.Fatalf("draft = %q", got)
	}
	// Both the subject hint and the instruction must reach the model.
	if len(fp.gotMsgs) != 1 ||
		!strings.Contains(fp.gotMsgs[0].Content, "Project sync") ||
		!strings.Contains(fp.gotMsgs[0].Content, "Propose a meeting") {
		t.Fatalf("subject/instruction not passed: %+v", fp.gotMsgs)
	}
}

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

func TestParseTranslatedSegments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
		ok   bool
	}{
		{"plain array", `["Hallo","Welt"]`, []string{"Hallo", "Welt"}, true},
		{"code fence", "```json\n[\"a\",\"b\"]\n```", []string{"a", "b"}, true},
		{"prose around", `Sure! ["x"] done`, []string{"x"}, true},
		{"no array", `not json`, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseTranslatedSegments(c.in)
			if c.ok != (err == nil) {
				t.Fatalf("err=%v, ok=%v", err, c.ok)
			}
			if c.ok && (len(got) != len(c.want) || (len(got) > 0 && got[0] != c.want[0])) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}
