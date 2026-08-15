// Package ai is the user-configurable LLM layer behind one Provider interface.
// One implementation speaks the OpenAI-compatible Chat Completions API (covering
// the LiteLLM proxy and OpenAI); another speaks the Anthropic Messages API. Both
// stream tokens over a channel so the UI can render replies live. It imports no
// GTK code.
package ai

import "context"

// Role is a chat message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Msg is a single chat message.
type Msg struct {
	Role    Role
	Content string
}

// Chunk is an incremental piece of a streamed response. A non-nil Err is terminal.
type Chunk struct {
	Text string
	Err  error
}

// Provider streams a chat completion. system is the system prompt (may be empty);
// msgs are the conversation turns. The returned channel is closed when the stream
// ends. Cancelling ctx aborts the request.
type Provider interface {
	Stream(ctx context.Context, system string, msgs []Msg) (<-chan Chunk, error)
	Name() string
}

// Options tunes a single request. Zero values mean "provider default".
type Options struct {
	// Temperature pins the sampling temperature. Classification tasks set 0:
	// small local models sampled at their server's default flip between the
	// right answer and an empty one run-to-run.
	Temperature *float64

	// AllowReasoning lets a thinking model think. It is OFF by default, so every
	// op suppresses chain-of-thought unless it deliberately opts in — the polarity
	// is deliberate, because thinking is a large regression on every operation
	// mailbox actually performs, and 5 of 7 local models surveyed think by
	// default.
	//
	// Measured through lemonade. Classifying one email on Qwen3.5-35B-A3B with
	// thinking on: 772 completion tokens, 31.5s, and it needs a budget over
	// ~2048 tokens before it emits any visible answer at all — below that the
	// reply is EMPTY, because the model spends the whole budget in its reasoning
	// channel and never reaches the content field. With thinking off: 4 tokens,
	// 0.3s, same answer. Drafting a reply on Gemma-4-E4B: 12.7s and 324 tokens
	// with thinking, 2.4s and 55 tokens without, for an equally good draft.
	//
	// So the default protects two things: latency on interactive work (a draft
	// that takes 13s reads as broken), and correctness on structured work (a
	// silent empty reply looks like the feature is broken rather than busy).
	// Set it only where deliberation is worth seconds of visible delay.
	AllowReasoning bool
}

// OptionsStreamer is an optional Provider capability for per-request Options.
type OptionsStreamer interface {
	StreamOpts(ctx context.Context, system string, msgs []Msg, o Options) (<-chan Chunk, error)
}

// streamWith streams via p, applying o when p supports options.
func streamWith(p Provider, ctx context.Context, system string, msgs []Msg, o Options) (<-chan Chunk, error) {
	if os, ok := p.(OptionsStreamer); ok {
		return os.StreamOpts(ctx, system, msgs, o)
	}
	return p.Stream(ctx, system, msgs)
}
