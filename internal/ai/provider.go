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
