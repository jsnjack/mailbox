package ai

import "context"

// Assistant builds task-specific prompts on top of a Provider.
type Assistant struct {
	p Provider
}

// NewAssistant wraps a provider.
func NewAssistant(p Provider) *Assistant { return &Assistant{p: p} }

// ProviderName returns the underlying provider's name.
func (a *Assistant) ProviderName() string { return a.p.Name() }

// Translate streams a translation of body into targetLang.
func (a *Assistant) Translate(ctx context.Context, body, targetLang string) (<-chan Chunk, error) {
	system := "You are a translation engine. Translate the user's email into " + targetLang +
		". Preserve meaning, tone, and formatting. Output only the translation, with no preamble or explanation."
	return a.p.Stream(ctx, system, []Msg{{Role: RoleUser, Content: body}})
}

// DraftReply streams a reply drafted from the thread context. instruction is an
// optional steer (e.g. "decline politely"); empty for a neutral reply.
func (a *Assistant) DraftReply(ctx context.Context, threadContext, instruction string) (<-chan Chunk, error) {
	system := "You are an email assistant. Draft a concise, professional reply in the same language as the " +
		"email thread. Output only the reply body — no subject line and no preamble such as 'Here is'."
	user := "Email thread to reply to:\n\n" + threadContext
	if instruction != "" {
		user += "\n\nAdditional instruction: " + instruction
	}
	return a.p.Stream(ctx, system, []Msg{{Role: RoleUser, Content: user}})
}
