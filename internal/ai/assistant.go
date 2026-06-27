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
	arr := firstJSONArray(raw)
	if arr == "" {
		return nil, fmt.Errorf("no JSON array in reply")
	}
	var out []string
	if err := json.Unmarshal([]byte(arr), &out); err != nil {
		return nil, fmt.Errorf("parse array: %w", err)
	}
	return out, nil
}

// firstJSONArray returns the first balanced [...] substring of s, tracking
// string literals so brackets inside strings don't confuse the match. This is
// robust to models that wrap the array in prose or emit more than one array
// (e.g. `["a","b"], ["c"]` — only the first is taken).
func firstJSONArray(s string) string {
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		switch c := s[i]; {
		case inStr:
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '[':
			depth++
		case c == ']':
			if depth--; depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
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

// EmailCategories are the fixed action buckets Categorize assigns. A message
// that fits none gets an empty category (no tag), so there is no "Other".
var EmailCategories = []string{
	"Needs reply", "Calendar", "Travel", "Receipt", "Finance", "Security",
	"Discount", "Newsletter", "Notification",
}

// Categorize classifies each email (a short "From / Subject / Snippet" string)
// into exactly one of EmailCategories, returning a category per input in order.
// Inputs and outputs are JSON arrays so many messages classify in one call.
func (a *Assistant) Categorize(ctx context.Context, items []string) ([]string, error) {
	payload, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("encode items: %w", err)
	}
	system := "You classify emails into exactly one category each, using these definitions:\n" +
		"- \"Needs reply\": a real person is asking you something or clearly expects a response.\n" +
		"- \"Calendar\": meetings or events — invitations, announcements or notices (incl. minutes/agendas), reminders, scheduling, or RSVP requests.\n" +
		"- \"Travel\": flights, hotels, car/train bookings, itineraries, boarding passes.\n" +
		"- \"Receipt\": confirmation of an order/payment ALREADY made — invoices paid, order/shipping/delivery updates.\n" +
		"- \"Finance\": money you still owe or account info — bills or payments DUE, bank/card statements, taxes.\n" +
		"- \"Security\": sign-in alerts, password resets, 2FA/verification codes, account or security changes.\n" +
		"- \"Discount\": marketing that contains a usable promo/coupon/discount code or a specific limited-time offer.\n" +
		"- \"Newsletter\": general marketing, promotions, or digests WITHOUT a specific redeemable code.\n" +
		"- \"Notification\": automated alerts or status updates from a service (social, app, or system).\n" +
		"If none of these clearly applies, use an empty string \"\" for that email. " +
		"The user message is a JSON array of emails, each a short 'From / Subject / Snippet' string. " +
		"Reply with ONLY a JSON array of the same length and order; each element is exactly one of the category " +
		"strings above or \"\". No commentary, no code fences."
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

// Proofread streams a grammar- and spelling-corrected version of the user's
// email text, preserving meaning, language, line breaks, quoted lines (starting
// with '>'), and any signature.
func (a *Assistant) Proofread(ctx context.Context, text string) (<-chan Chunk, error) {
	system := "You are a proofreader for email. Correct only spelling, grammar, and punctuation in the user's " +
		"text. Preserve the meaning, tone, language, line breaks, any quoted lines (those starting with '>'), " +
		"and any signature, exactly. Return only the corrected text — no commentary, no surrounding quotes, no " +
		"code fences."
	return a.p.Stream(ctx, system, []Msg{{Role: RoleUser, Content: text}})
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

// GenerateSubject returns a concise subject line for the given email body. The
// model is told to reply with only the subject; cleanSubject defends against a
// stray "Subject:" prefix, surrounding quotes, or extra lines.
func (a *Assistant) GenerateSubject(ctx context.Context, body string) (string, error) {
	system := "You write a concise, specific email subject line for the email body the user provides. " +
		"Reply with ONLY the subject line: a short noun phrase (ideally under 8 words), in the body's " +
		"language, with no surrounding quotes, no 'Subject:' prefix, and no commentary."
	ch, err := a.p.Stream(ctx, system, []Msg{{Role: RoleUser, Content: body}})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for c := range ch {
		if c.Err != nil {
			return "", c.Err
		}
		b.WriteString(c.Text)
	}
	return cleanSubject(b.String()), nil
}

// cleanSubject reduces a model reply to a single bare subject line.
func cleanSubject(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(s), "subject:") {
		s = strings.TrimSpace(s[len("subject:"):])
	}
	return strings.TrimSpace(strings.Trim(s, `"'`))
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
