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

func TestSmartReplies(t *testing.T) {
	fp := &fakeProvider{chunks: []Chunk{{Text: `["Sounds good!", `}, {Text: `"Can we reschedule?", "I'll take a look."]`}}}
	got, err := NewAssistant(fp).SmartReplies(context.Background(), "From: a@x.com\nSubject: lunch?\n\nLunch tomorrow?")
	if err != nil {
		t.Fatalf("SmartReplies: %v", err)
	}
	if len(got) != 3 || got[0] != "Sounds good!" || got[2] != "I'll take a look." {
		t.Fatalf("replies = %#v", got)
	}
	if !strings.Contains(fp.gotMsgs[0].Content, "Lunch tomorrow?") {
		t.Fatalf("thread context not passed: %+v", fp.gotMsgs)
	}
}

func TestCategorize(t *testing.T) {
	fp := &fakeProvider{chunks: []Chunk{{Text: `["Needs reply", "Receipt", `}, {Text: `"Newsletter"]`}}}
	got, err := NewAssistant(fp).Categorize(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Categorize: %v", err)
	}
	if len(got) != 3 || got[0] != "Needs reply" || got[2] != "Newsletter" {
		t.Fatalf("categories = %#v", got)
	}
	if !strings.Contains(fp.gotSystem, "Needs reply") {
		t.Fatalf("system prompt missing categories: %q", fp.gotSystem)
	}
}

func TestCategorizeSingleSalvage(t *testing.T) {
	// Small models often answer a single-item classify with a bare scalar
	// instead of a one-element JSON array; parseCategories salvages it.
	cases := []struct {
		reply string
		want  string
	}{
		{`""`, ""},                       // JSON empty string → no tag
		{`Notification`, "Notification"}, // bare word
		{"`Needs reply`", "Needs reply"}, // code-fenced
		{`"Receipt"`, "Receipt"},         // quoted scalar
		{`something else`, ""},           // unknown → no tag
		// Real replies from Ministral-3B at temperature 0: a nested array, and
		// near-misses the canonical set must still absorb.
		{"[[\"Notification\"]]", "Notification"},
		{`["Notifications"]`, "Notification"},
		{`Category: Needs reply`, "Needs reply"},
		{`Newsletter or Notification`, ""}, // ambiguous → no tag
		// Off-list labels models emit (seen live) map to their canonical bucket.
		{`["Marketing"]`, "Newsletter"},
		{`Invitation`, "Calendar"},
	}
	for _, tc := range cases {
		fp := &fakeProvider{chunks: []Chunk{{Text: tc.reply}}}
		got, err := NewAssistant(fp).Categorize(context.Background(), []string{"a"})
		if err != nil {
			t.Fatalf("Categorize(%q): %v", tc.reply, err)
		}
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("Categorize(%q) = %#v, want [%q]", tc.reply, got, tc.want)
		}
	}
}

// Categorize pins temperature 0 — sampled classification flips between the
// right category and "" run-to-run on small models.
func TestCategorizeTemperatureZero(t *testing.T) {
	sp := &scriptedProvider{chunks: []Chunk{{Text: `["Receipt"]`}}}
	if _, err := NewAssistant(sp).Categorize(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Categorize: %v", err)
	}
	if sp.gotOpts == nil || sp.gotOpts.Temperature == nil || *sp.gotOpts.Temperature != 0 {
		t.Fatalf("temperature not pinned to 0: %+v", sp.gotOpts)
	}
}

// A truncated batch reply (the model emitted EOS mid-array — seen from
// Ministral-3B on 20-item batches) salvages the answered prefix instead of
// failing the whole batch; the unanswered tail is left for the next pass.
func TestCategorizeTruncatedBatch(t *testing.T) {
	fp := &fakeProvider{chunks: []Chunk{{Text: "[\n\"Calendar\",\n\"\",\n\"Notifications\",\n\""}}}
	got, err := NewAssistant(fp).Categorize(context.Background(), make([]string, 20))
	if err != nil {
		t.Fatalf("Categorize: %v", err)
	}
	// Three complete elements; the fourth was cut mid-string and is dropped.
	if len(got) != 3 || got[0] != "Calendar" || got[1] != "" || got[2] != "Notification" {
		t.Fatalf("salvaged prefix = %#v", got)
	}
}

// A multi-item nested reply maps element-wise.
func TestCategorizeNestedMulti(t *testing.T) {
	fp := &fakeProvider{chunks: []Chunk{{Text: `[["Receipt"],[""],["Notifications"]]`}}}
	got, err := NewAssistant(fp).Categorize(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Categorize: %v", err)
	}
	if len(got) != 3 || got[0] != "Receipt" || got[1] != "" || got[2] != "Notification" {
		t.Fatalf("categories = %#v", got)
	}
}

func TestSmartRepliesSalvage(t *testing.T) {
	// Model ignored the JSON-array instruction and returned a bulleted list.
	fp := &fakeProvider{chunks: []Chunk{{Text: "- Sounds good!\n"}, {Text: "2. Can we reschedule?\n* I'll take a look.\n"}}}
	got, err := NewAssistant(fp).SmartReplies(context.Background(), "ctx")
	if err != nil {
		t.Fatalf("SmartReplies: %v", err)
	}
	want := []string{"Sounds good!", "Can we reschedule?", "I'll take a look."}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("replies = %#v, want %#v", got, want)
	}
}

func TestSmartRepliesMalformedArrayNoGarbage(t *testing.T) {
	// A malformed JSON array (trailing comma) fails to parse; line-splitting its
	// syntax would yield garbage, so we surface the error instead of bad replies.
	fp := &fakeProvider{chunks: []Chunk{{Text: "[\n  \"Sounds good\",\n  \"On my way\",\n]"}}}
	got, err := NewAssistant(fp).SmartReplies(context.Background(), "ctx")
	if err == nil {
		t.Fatalf("expected error for malformed array, got replies %#v", got)
	}
}

func TestTranslateSegmentsSingleSalvage(t *testing.T) {
	// A single segment often comes back as a bare string, not a 1-element array.
	fp := &fakeProvider{chunks: []Chunk{{Text: `"Hola"`}}}
	got, err := NewAssistant(fp).TranslateSegments(context.Background(), []string{"Hello"}, "Spanish")
	if err != nil {
		t.Fatalf("TranslateSegments: %v", err)
	}
	if len(got) != 1 || got[0] != "Hola" {
		t.Fatalf("segments = %#v, want [\"Hola\"]", got)
	}
	// A multi-segment reply that isn't an array must still error (no positional
	// salvage possible), so the caller keeps the originals.
	fp2 := &fakeProvider{chunks: []Chunk{{Text: "Hola"}}}
	if _, err := NewAssistant(fp2).TranslateSegments(context.Background(), []string{"a", "b"}, "Spanish"); err == nil {
		t.Fatal("multi-segment non-array reply should error")
	}
}

func TestProofread(t *testing.T) {
	fp := &fakeProvider{chunks: []Chunk{{Text: "Hi there,\n"}, {Text: "Thanks for your help."}}}
	ch, err := NewAssistant(fp).Proofread(context.Background(), "hi their, thanks for you're help")
	if err != nil {
		t.Fatalf("Proofread: %v", err)
	}
	var b strings.Builder
	for c := range ch {
		b.WriteString(c.Text)
	}
	if b.String() != "Hi there,\nThanks for your help." {
		t.Fatalf("got %q", b.String())
	}
	if !strings.Contains(fp.gotMsgs[0].Content, "you're help") || !strings.Contains(fp.gotSystem, "grammar") {
		t.Fatalf("text/system not passed: %+v / %q", fp.gotMsgs, fp.gotSystem)
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
	ch, err := a.DraftNew(context.Background(), "Project sync", "Propose a meeting", false)
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
		wantErr  bool
	}{
		{"content", `{"choices":[{"delta":{"content":"hello"}}]}`, "hello", false, false},
		{"finish", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "", true, false},
		{"empty choices", `{"choices":[]}`, "", false, false},
		{"garbage", `not json`, "", false, false},
		{"mid-stream error", `{"error":{"message":"server overloaded"}}`, "", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, done, err := extractOpenAIDelta([]byte(tc.data))
			if text != tc.wantText || done != tc.wantDone || (err != nil) != tc.wantErr {
				t.Fatalf("got (%q,%v,err=%v), want (%q,%v,err=%v)", text, done, err, tc.wantText, tc.wantDone, tc.wantErr)
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
		wantErr  bool
	}{
		{"text delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`, "hi", false, false},
		{"stop", `{"type":"message_stop"}`, "", true, false},
		{"ping", `{"type":"ping"}`, "", false, false},
		{"garbage", `not json`, "", false, false},
		{"mid-stream error", `{"type":"error","error":{"message":"overloaded_error"}}`, "", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, done, err := extractAnthropicDelta([]byte(tc.data))
			if text != tc.wantText || done != tc.wantDone || (err != nil) != tc.wantErr {
				t.Fatalf("got (%q,%v,err=%v), want (%q,%v,err=%v)", text, done, err, tc.wantText, tc.wantDone, tc.wantErr)
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
	if got.Provider != want.Provider || got.Endpoint != want.Endpoint || got.Model != want.Model || len(got.Models) != 0 {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}

// A multi-model config round-trips, mirrors the primary into the single-model
// field (downgrade compatibility), and resolves the priority list.
func TestSaveConfigModelsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	in := Config{Provider: "litellm", Endpoint: "http://argus:4000/v1", Models: []string{"big-cloud", "small-local"}}
	if err := SaveConfig(path, in); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if list := got.ModelList(); len(list) != 2 || list[0] != "big-cloud" || list[1] != "small-local" {
		t.Fatalf("ModelList = %#v", got.ModelList())
	}
	if got.Model != "big-cloud" {
		t.Fatalf("single-model mirror = %q, want primary", got.Model)
	}
	if !got.Configured() {
		t.Fatal("multi-model config should be Configured")
	}

	// The env override pins a single model with no fallbacks.
	t.Setenv("MAILBOX_AI_MODEL", "pinned")
	got, _ = LoadConfig(path)
	if list := got.ModelList(); len(list) != 1 || list[0] != "pinned" {
		t.Fatalf("env override ModelList = %#v", list)
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
		{"multiple arrays", `["a","b"], ["c"]`, []string{"a", "b"}, true}, // regression: only the first
		{"bracket inside string", `["a [1]","b"]`, []string{"a [1]", "b"}, true},
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

func TestCleanSubject(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Quarterly report ready", "Quarterly report ready"},
		{"Subject: Lunch on Friday", "Lunch on Friday"},
		{"subject:  Trimmed  ", "Trimmed"},
		{"\"Quoted subject\"", "Quoted subject"},
		{"First line\nSecond line", "First line"},
		{"  spaced out  ", "spaced out"},
		{"", ""},
	}
	for _, c := range cases {
		if got := cleanSubject(c.in); got != c.want {
			t.Errorf("cleanSubject(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
