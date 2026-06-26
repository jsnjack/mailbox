package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Assistant builds task-specific prompts on top of a Provider.
type Assistant struct {
	p Provider
}

// NewAssistant wraps a provider.
func NewAssistant(p Provider) *Assistant { return &Assistant{p: p} }

// ProviderName returns the underlying provider's name.
func (a *Assistant) ProviderName() string { return a.p.Name() }

// TranslateSegments translates each text snippet into targetLang and returns the
// translations in the same order. It sends only the text (as a compact JSON
// array), never the surrounding markup, so the model generates a small fraction
// of the tokens it would for whole-HTML translation — far faster. The caller
// reinserts the results into the original markup, preserving styling.
func (a *Assistant) TranslateSegments(ctx context.Context, segments []string, targetLang string) ([]string, error) {
	payload, err := json.Marshal(segments)
	if err != nil {
		return nil, fmt.Errorf("encode segments: %w", err)
	}
	system := "You are a translation engine. The user message is a JSON array of short text snippets from " +
		"an email. Translate each snippet into " + targetLang + " and reply with ONLY a JSON array of the " +
		"same length and order, where each element is the translation of the corresponding input snippet. " +
		"Leave snippets that are URLs, email addresses, numbers, or pure symbols unchanged. Do not merge or " +
		"split snippets. No commentary and no code fences."
	ch, err := a.p.Stream(ctx, system, []Msg{{Role: RoleUser, Content: string(payload)}})
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for c := range ch {
		if c.Err != nil {
			return nil, c.Err
		}
		b.WriteString(c.Text)
	}
	return parseTranslatedSegments(b.String())
}

// parseTranslatedSegments extracts a JSON array of strings from a model reply,
// tolerating code fences or surrounding prose by salvaging the outermost array.
func parseTranslatedSegments(raw string) ([]string, error) {
	start := strings.IndexByte(raw, '[')
	end := strings.LastIndexByte(raw, ']')
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON array in translation reply")
	}
	var out []string
	if err := json.Unmarshal([]byte(raw[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("parse translation array: %w", err)
	}
	return out, nil
}

// SummarizeThread streams a short bullet-point summary of an email thread, for
// someone catching up quickly. threadContext is the thread rendered as plain
// text (oldest message first). The reply is plain text — a few "- " bullets.
func (a *Assistant) SummarizeThread(ctx context.Context, threadContext string) (<-chan Chunk, error) {
	system := "You are an email assistant. Summarize the following email thread for someone catching up " +
		"quickly. Reply with 2-5 short bullet points, one per line, each starting with '- ', covering the key " +
		"points, decisions, and any open questions or action items awaiting a response. Be concise and " +
		"factual. Write the summary in the same language as the thread. Output only the bullet points — no " +
		"heading, no preamble such as 'Here is', and no code fences."
	user := "Email thread to summarize:\n\n" + threadContext
	return a.p.Stream(ctx, system, []Msg{{Role: RoleUser, Content: user}})
}

// DraftNew streams a brand-new email body from an instruction (what the user
// wants to say); subject is an optional hint. There is no thread to reply to,
// so this is used when composing from scratch.
func (a *Assistant) DraftNew(ctx context.Context, subject, instruction string) (<-chan Chunk, error) {
	system := "You are an email assistant. Write a clear, concise, professional email body from the user's " +
		"instruction. Output only the body — no subject line and no preamble such as 'Here is'. Match the " +
		"language of the instruction."
	user := strings.TrimSpace(instruction)
	if user == "" {
		user = "Write a brief, friendly email."
	}
	if s := strings.TrimSpace(subject); s != "" {
		user = "The email subject is: " + s + "\n\n" + user
	}
	return a.p.Stream(ctx, system, []Msg{{Role: RoleUser, Content: user}})
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
