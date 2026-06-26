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

// SmartReplies suggests up to 3 short, ready-to-send replies to the latest
// message in a thread. It returns plain strings (parsed from a JSON array).
func (a *Assistant) SmartReplies(ctx context.Context, threadContext string) ([]string, error) {
	system := "You are an email assistant. Suggest 3 short, distinct, ready-to-send replies to the latest " +
		"message in this thread — each a single natural sentence (under about 12 words), in the thread's " +
		"language. Reply with ONLY a JSON array of exactly 3 strings: no commentary, no code fences."
	ch, err := a.p.Stream(ctx, system, []Msg{{Role: RoleUser, Content: threadContext}})
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

// AnalyzeEmail streams a phishing/scam risk assessment of an email. emailContext
// is the sender, subject, body, and any automated signals (auth result,
// heuristic warnings). The reply leads with a one-line verdict, then reasons.
func (a *Assistant) AnalyzeEmail(ctx context.Context, emailContext string) (<-chan Chunk, error) {
	system := "You are a security assistant helping a user judge whether an email is a phishing, scam, or " +
		"social-engineering attempt. Weigh signals like a false sense of urgency or threats, requests for " +
		"passwords, payment, or personal information, mismatched or lookalike sender addresses, suspicious or " +
		"mismatched links, and unusual requests. You are given the email plus any automated authentication " +
		"result and warnings. Reply with a first line that is exactly one of: 'Verdict: Looks legitimate', " +
		"'Verdict: Be cautious', or 'Verdict: Likely phishing'. Then give 2-4 short bullet points (each " +
		"starting with '- ') explaining why. Be concise and factual; do not invent details."
	return a.p.Stream(ctx, system, []Msg{{Role: RoleUser, Content: emailContext}})
}

// Ping issues a tiny request to verify the provider, endpoint, and key actually
// work. It returns the first error from the stream, or nil on success.
func (a *Assistant) Ping(ctx context.Context) error {
	ch, err := a.p.Stream(ctx, "Reply with the single word OK.", []Msg{{Role: RoleUser, Content: "ping"}})
	if err != nil {
		return err
	}
	for c := range ch {
		if c.Err != nil {
			return c.Err
		}
	}
	return nil
}

// SummarizeThread streams a short bullet-point summary of an email thread, for
// someone catching up quickly. threadContext is the thread rendered as plain
// text (oldest message first). The reply is plain text — a few "- " bullets.
func (a *Assistant) SummarizeThread(ctx context.Context, threadContext string) (<-chan Chunk, error) {
	system := "You are an email assistant. Summarize the following email thread for someone catching up " +
		"quickly. Reply with 2-5 short bullet points, one per line, each starting with '- ', covering the key " +
		"points, decisions, and any open questions or action items awaiting a response. Be concise and " +
		"factual. Always write the summary in English, even when the thread is in another language. Output " +
		"only the bullet points — no heading, no preamble such as 'Here is', and no code fences."
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
