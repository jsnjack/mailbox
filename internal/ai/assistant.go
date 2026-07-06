package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
)

// Assistant builds task-specific prompts on top of a Provider. The provider is
// swappable at runtime (SetProvider), so Preferences changes apply to a live
// Assistant without a restart. The Assistant owns the session-cumulative AI
// counters (requests, transferred-bytes baseline) so the status bar's numbers
// survive a provider swap instead of resetting with the new provider object.
type Assistant struct {
	mu sync.RWMutex
	p  Provider

	reqs            atomic.Int64 // AI requests issued this session
	baseIn, baseOut atomic.Int64 // bytes from providers swapped out earlier
}

// NewAssistant wraps a provider.
func NewAssistant(p Provider) *Assistant { return &Assistant{p: p} }

// SetProvider swaps the underlying provider. In-flight requests finish on the
// provider they started with; new requests use p. The outgoing provider's byte
// counters roll into the session baseline so cumulative stats keep counting.
func (a *Assistant) SetProvider(p Provider) {
	a.mu.Lock()
	if r, ok := a.p.(interface{ transfer() (int64, int64) }); ok {
		in, out := r.transfer()
		a.baseIn.Add(in)
		a.baseOut.Add(out)
	}
	a.p = p
	a.mu.Unlock()
	logging.Trace("ai: provider swapped", "provider", p.Name())
}

func (a *Assistant) provider() Provider {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.p
}

// stream is the single gate every Assistant op calls through: it counts the
// request (for the status bar's session stats) and dispatches with options.
func (a *Assistant) stream(ctx context.Context, system string, msgs []Msg, opts ...Options) (<-chan Chunk, error) {
	a.reqs.Add(1)
	o := Options{}
	if len(opts) > 0 {
		o = opts[0]
	}
	return streamWith(a.provider(), ctx, system, msgs, o)
}

// Requests returns how many AI requests this session has issued.
func (a *Assistant) Requests() int64 { return a.reqs.Load() }

// ProviderName returns the underlying provider's name.
func (a *Assistant) ProviderName() string { return a.provider().Name() }

// TranslateSegments translates each text snippet into targetLang and returns the
// translations in the same order. It sends only the text (as a compact JSON
// array), never the surrounding markup, so the model generates a small fraction
// of the tokens it would for whole-HTML translation — far faster. The caller
// reinserts the results into the original markup, preserving styling.
func (a *Assistant) TranslateSegments(ctx context.Context, segments []string, targetLang string) ([]string, error) {
	start := time.Now()
	logging.Trace("ai: translate segments", "op", "TranslateSegments", "provider", a.provider().Name(),
		"lang", targetLang, "segments", len(segments))
	payload, err := json.Marshal(segments)
	if err != nil {
		return nil, fmt.Errorf("encode segments: %w", err)
	}
	system := "You are a translation engine. The user message is a JSON array of short text snippets from " +
		"an email. Translate each snippet into " + targetLang + " and reply with ONLY a JSON array of the " +
		"same length and order, where each element is the translation of the corresponding input snippet. " +
		"Leave snippets that are URLs, email addresses, numbers, or pure symbols unchanged. Do not merge or " +
		"split snippets. No commentary and no code fences."
	ch, err := a.stream(ctx, system, []Msg{{Role: RoleUser, Content: string(payload)}})
	if err != nil {
		logging.Trace("ai: translate segments failed", "op", "TranslateSegments", "err", err)
		return nil, err
	}
	var b strings.Builder
	for c := range ch {
		if c.Err != nil {
			logging.Trace("ai: translate segments failed", "op", "TranslateSegments", "err", c.Err)
			return nil, c.Err
		}
		b.WriteString(c.Text)
	}
	out, err := parseTranslatedSegments(b.String())
	if err != nil && len(segments) == 1 {
		// A single segment often comes back as a bare string ("Hola") instead of a
		// 1-element array; salvage it so a short one-line email still translates.
		if v := stripScalar(b.String()); v != "" {
			out, err = []string{v}, nil
		}
	}
	logging.Trace("ai: translate segments done", "op", "TranslateSegments",
		"bytes", b.Len(), "results", len(out), "dur", time.Since(start), "err", err)
	return out, err
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

// stripScalar reduces a model reply that should have been a single JSON string
// to its bare text: it trims whitespace and surrounding code fences/backticks,
// then unquotes a JSON string ("hi") or drops stray surrounding quotes. Used to
// salvage the bare-scalar reply small models return when only one value was
// requested (see parseCategories, TranslateSegments) instead of a 1-element array.
func stripScalar(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`")
	s = strings.TrimSpace(s)
	if unq := ""; json.Unmarshal([]byte(s), &unq) == nil {
		return unq
	}
	return strings.Trim(s, `"'`)
}

// stripListMarker removes a single leading bullet ("- ", "* ", "• ") or ordinal
// ("1. ", "1) ") marker from a line, so a reply salvaged from a bulleted/numbered
// list (see SmartReplies) keeps its text without the marker.
func stripListMarker(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"- ", "* ", "• "} {
		if strings.HasPrefix(s, p) {
			return strings.TrimSpace(s[len(p):])
		}
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 && i < len(s) && (s[i] == '.' || s[i] == ')') {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// SmartReplies suggests up to 3 short, ready-to-send replies to the latest
// message in a thread. It returns plain strings (parsed from a JSON array).
func (a *Assistant) SmartReplies(ctx context.Context, threadContext string) ([]string, error) {
	start := time.Now()
	logging.Trace("ai: smart replies", "op", "SmartReplies", "provider", a.provider().Name(),
		"context", logging.Body(threadContext))
	system := "You are an email assistant. Suggest 3 short, distinct, ready-to-send replies to the latest " +
		"message in this thread — each a single natural sentence (under about 12 words), in the thread's " +
		"language. Reply with ONLY a JSON array of exactly 3 strings: no commentary, no code fences."
	ch, err := a.stream(ctx, system, []Msg{{Role: RoleUser, Content: threadContext}})
	if err != nil {
		logging.Trace("ai: smart replies failed", "op", "SmartReplies", "err", err)
		return nil, err
	}
	var b strings.Builder
	for c := range ch {
		if c.Err != nil {
			logging.Trace("ai: smart replies failed", "op", "SmartReplies", "err", c.Err)
			return nil, c.Err
		}
		b.WriteString(c.Text)
	}
	out, err := parseTranslatedSegments(b.String())
	if err != nil {
		// Small models sometimes ignore the JSON-array instruction and answer with
		// a bulleted or newline-separated list. Salvage each non-empty line (marker
		// stripped) as a reply rather than dropping the whole suggestion.
		if salv := salvageReplyLines(b.String()); len(salv) > 0 {
			out, err = salv, nil
		}
	}
	logging.Trace("ai: smart replies done", "op", "SmartReplies",
		"bytes", b.Len(), "results", len(out), "dur", time.Since(start), "err", err)
	return out, err
}

// salvageReplyLines extracts up to 3 reply strings from a non-JSON reply by
// splitting on lines, stripping list markers and quotes, and dropping empties
// and bare JSON punctuation. It returns nothing when the reply looks like an
// attempted (but malformed) JSON array — line-splitting JSON syntax yields
// garbage like `"a",`, and showing no suggestions beats showing garbage.
func salvageReplyLines(raw string) []string {
	t := strings.TrimSpace(raw)
	t = strings.TrimPrefix(t, "```json")
	t = strings.TrimPrefix(t, "```")
	if strings.HasPrefix(strings.TrimSpace(t), "[") {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(raw, "\n") {
		s := stripScalar(stripListMarker(ln))
		switch s {
		case "", "[", "]", ",", "```":
			continue
		}
		out = append(out, s)
		if len(out) == 3 {
			break
		}
	}
	return out
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
	start := time.Now()
	logging.Trace("ai: categorize", "op", "Categorize", "provider", a.provider().Name(), "items", len(items))
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
		// "suggestions" is spelled out: social networks send connection/job
		// suggestions that are neither alerts nor status updates, and small
		// models read the definition literally (a LinkedIn "add Yuri" email
		// scored "" until suggestions were named).
		"- \"Notification\": automated notices from a service or social network — alerts, status updates, " +
		"activity digests, or suggestions (e.g. \"people you may know\", job picks, commits pushed, CI results).\n" +
		"If none of these clearly applies, use an empty string \"\" for that email. " +
		"The user message is a JSON array of emails, each a short 'From / Subject / Snippet' string. " +
		"Reply with ONLY a JSON array of the same length and order; each element is exactly one of the category " +
		"strings above or \"\". No commentary, no code fences."
	// Temperature 0: classification wants the model's argmax answer. Small
	// local models sampled at the server default flip between the right
	// category and "" for the same email run-to-run.
	zero := 0.0
	ch, err := a.stream(ctx, system, []Msg{{Role: RoleUser, Content: string(payload)}}, Options{Temperature: &zero})
	if err != nil {
		logging.Trace("ai: categorize failed", "op", "Categorize", "err", err)
		return nil, err
	}
	var b strings.Builder
	for c := range ch {
		if c.Err != nil {
			logging.Trace("ai: categorize failed", "op", "Categorize", "err", c.Err)
			return nil, c.Err
		}
		b.WriteString(c.Text)
	}
	out, err := parseCategories(b.String(), len(items))
	logging.Trace("ai: categorize done", "op", "Categorize",
		"bytes", b.Len(), "reply", logging.Body(b.String()),
		"results", len(out), "dur", time.Since(start), "err", err)
	return out, err
}

// parseCategories reads the model's categorize reply into n canonical category
// strings. The normal reply is a JSON array (parseTranslatedSegments); small
// models also produce a nested array (`[["Notification"]]`) or, for a single
// email, a bare scalar (`""` or `Notification`). Every element is mapped onto
// the canonical category set via MatchCategory, so near-misses ("Notifications",
// "category: Notification") still tag rather than silently dropping to "".
func parseCategories(raw string, n int) ([]string, error) {
	out, err := parseTranslatedSegments(raw)
	if err != nil {
		// Over-bracketed reply: a nested array of one-element arrays.
		var nested [][]string
		if arr := firstJSONArray(raw); arr != "" && json.Unmarshal([]byte(arr), &nested) == nil {
			out = make([]string, len(nested))
			for i, inner := range nested {
				if len(inner) > 0 {
					out[i] = inner[0]
				}
			}
			err = nil
		}
	}
	if err != nil {
		// Truncated reply: the model emitted EOS mid-array, so the closing "]"
		// never arrived and no balanced array exists. Salvage the complete
		// prefix — the caller stores those and leaves the rest for a later pass.
		if prefix := truncatedStringArray(raw); len(prefix) > 0 {
			out, err = prefix, nil
		}
	}
	if err == nil {
		for i := range out {
			out[i] = MatchCategory(out[i])
		}
		return out, nil
	}
	if n != 1 {
		return nil, err
	}
	// Tolerate a JSON-quoted string ("Notification") or a bare word.
	return []string{MatchCategory(stripScalar(raw))}, nil
}

// truncatedStringArray reads the longest complete prefix of string elements
// from a JSON array whose closing bracket never arrived. Nil when raw doesn't
// start an array of strings.
func truncatedStringArray(raw string) []string {
	start := strings.IndexByte(raw, '[')
	if start < 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(raw[start:]))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil
	}
	var out []string
	for {
		tok, err := dec.Token()
		if err != nil {
			return out // truncation point (or clean "]" — but then parsing above succeeded)
		}
		s, ok := tok.(string)
		if !ok {
			return out
		}
		out = append(out, s)
	}
}

// MatchCategory maps a model-produced category string onto the canonical
// EmailCategories entry, tolerating the near-misses small models emit: case
// differences, surrounding quotes/punctuation, a plural ("Notifications"), or
// the category embedded in a short phrase ("Category: Needs reply"). Returns
// "" (no tag) when nothing matches unambiguously.
func MatchCategory(s string) string {
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "\"'`.,:;!"))
	if s == "" {
		return ""
	}
	for _, c := range EmailCategories {
		if strings.EqualFold(s, c) {
			return c
		}
	}
	found := ""
	ls := strings.ToLower(s)
	for _, c := range EmailCategories {
		if strings.Contains(ls, strings.ToLower(c)) {
			if found != "" {
				return "" // two categories named — ambiguous, better no tag
			}
			found = c
		}
	}
	return found
}

// Proofread streams a grammar- and spelling-corrected version of the user's
// email text, preserving meaning, language, line breaks, quoted lines (starting
// with '>'), and any signature.
func (a *Assistant) Proofread(ctx context.Context, text string) (<-chan Chunk, error) {
	logging.Trace("ai: proofread", "op", "Proofread", "provider", a.provider().Name(),
		"bytes", len(text), "text", logging.Body(text))
	system := "You are a proofreader for email. Correct only spelling, grammar, and punctuation in the user's " +
		"text. Preserve the meaning, tone, language, line breaks, any quoted lines (those starting with '>'), " +
		"and any signature, exactly. Return only the corrected text — no commentary, no surrounding quotes, no " +
		"code fences."
	return a.stream(ctx, system, []Msg{{Role: RoleUser, Content: text}})
}

// AnalyzeEmail streams a phishing/scam risk assessment of an email. emailContext
// is the sender, subject, body, and any automated signals (auth result,
// heuristic warnings). The reply leads with a one-line verdict, then reasons.
func (a *Assistant) AnalyzeEmail(ctx context.Context, emailContext string) (<-chan Chunk, error) {
	logging.Trace("ai: analyze email", "op", "AnalyzeEmail", "provider", a.provider().Name(),
		"bytes", len(emailContext), "context", logging.Body(emailContext))
	system := "You are a security assistant helping a user judge whether an email is a phishing, scam, or " +
		"social-engineering attempt. Weigh signals like a false sense of urgency or threats, requests for " +
		"passwords, payment, or personal information, mismatched or lookalike sender addresses, suspicious or " +
		"mismatched links, and unusual requests. You are given the email plus any automated authentication " +
		"result and warnings. Reply with a first line that is exactly one of: 'Verdict: Looks legitimate', " +
		"'Verdict: Be cautious', or 'Verdict: Likely phishing'. Then give 2-4 short bullet points (each " +
		"starting with '- ') explaining why. Be concise and factual; do not invent details."
	return a.stream(ctx, system, []Msg{{Role: RoleUser, Content: emailContext}})
}

// Ping issues a tiny request to verify the provider, endpoint, and key actually
// work. It returns the first error from the stream, or nil on success.
func (a *Assistant) Ping(ctx context.Context) error {
	start := time.Now()
	logging.Trace("ai: ping", "op", "Ping", "provider", a.provider().Name())
	ch, err := a.stream(ctx, "Reply with the single word OK.", []Msg{{Role: RoleUser, Content: "ping"}})
	if err != nil {
		logging.Trace("ai: ping failed", "op", "Ping", "dur", time.Since(start), "err", err)
		return err
	}
	for c := range ch {
		if c.Err != nil {
			logging.Trace("ai: ping failed", "op", "Ping", "dur", time.Since(start), "err", c.Err)
			return c.Err
		}
	}
	logging.Trace("ai: ping done", "op", "Ping", "dur", time.Since(start), "status", "ok")
	return nil
}

// SummarizeThread streams a short bullet-point summary of an email thread, for
// someone catching up quickly. threadContext is the thread rendered as plain
// text (oldest message first). The reply is plain text — a few "- " bullets.
func (a *Assistant) SummarizeThread(ctx context.Context, threadContext string) (<-chan Chunk, error) {
	logging.Trace("ai: summarize thread", "op", "SummarizeThread", "provider", a.provider().Name(),
		"bytes", len(threadContext), "context", logging.Body(threadContext))
	system := "You are an email assistant. Summarize the following email thread for someone catching up " +
		"quickly. Reply with 2-5 short bullet points, one per line, each starting with '- ', covering the key " +
		"points, decisions, and any open questions or action items awaiting a response. Be concise and " +
		"factual. Always write the summary in English, even when the thread is in another language. Output " +
		"only the bullet points — no heading, no preamble such as 'Here is', and no code fences."
	user := "Email thread to summarize:\n\n" + threadContext
	return a.stream(ctx, system, []Msg{{Role: RoleUser, Content: user}})
}

// DraftNew streams a brand-new email body from an instruction (what the user
// wants to say); subject is an optional hint. There is no thread to reply to,
// so this is used when composing from scratch.
func (a *Assistant) DraftNew(ctx context.Context, subject, instruction string, omitSignature bool) (<-chan Chunk, error) {
	system := "You are an email assistant. Write a clear, concise, professional email body from the user's " +
		"instruction. Output only the body — no subject line and no preamble such as 'Here is'. Match the " +
		"language of the instruction."
	if omitSignature {
		system += " Do not add a closing sign-off or signature — one is appended automatically."
	}
	user := strings.TrimSpace(instruction)
	if user == "" {
		user = "Write a brief, friendly email."
	}
	if s := strings.TrimSpace(subject); s != "" {
		user = "The email subject is: " + s + "\n\n" + user
	}
	logging.Trace("ai: draft new", "op", "DraftNew", "provider", a.provider().Name(),
		"omitSignature", omitSignature, "subject", subject, "prompt", logging.Body(user))
	return a.stream(ctx, system, []Msg{{Role: RoleUser, Content: user}})
}

// GenerateSubject returns a concise subject line for the given email body. The
// model is told to reply with only the subject; cleanSubject defends against a
// stray "Subject:" prefix, surrounding quotes, or extra lines.
func (a *Assistant) GenerateSubject(ctx context.Context, body string) (string, error) {
	start := time.Now()
	logging.Trace("ai: generate subject", "op", "GenerateSubject", "provider", a.provider().Name(),
		"bytes", len(body), "body", logging.Body(body))
	system := "You write a concise, specific email subject line for the email body the user provides. " +
		"Reply with ONLY the subject line: a short noun phrase (ideally under 8 words), in the body's " +
		"language, with no surrounding quotes, no 'Subject:' prefix, and no commentary."
	ch, err := a.stream(ctx, system, []Msg{{Role: RoleUser, Content: body}})
	if err != nil {
		logging.Trace("ai: generate subject failed", "op", "GenerateSubject", "err", err)
		return "", err
	}
	var b strings.Builder
	for c := range ch {
		if c.Err != nil {
			logging.Trace("ai: generate subject failed", "op", "GenerateSubject", "err", c.Err)
			return "", c.Err
		}
		b.WriteString(c.Text)
	}
	subject := cleanSubject(b.String())
	logging.Trace("ai: generate subject done", "op", "GenerateSubject",
		"subject", subject, "dur", time.Since(start))
	return subject, nil
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
func (a *Assistant) DraftReply(ctx context.Context, threadContext, instruction string, omitSignature bool) (<-chan Chunk, error) {
	system := "You are an email assistant. Draft a concise, professional reply in the same language as the " +
		"email thread. Output only the reply body — no subject line and no preamble such as 'Here is'."
	if omitSignature {
		system += " Do not add a closing sign-off or signature (e.g. \"Best regards, <name>\") — one is appended automatically."
	}
	user := "Email thread to reply to:\n\n" + threadContext
	if instruction != "" {
		user += "\n\nAdditional instruction: " + instruction
	}
	logging.Trace("ai: draft reply", "op", "DraftReply", "provider", a.provider().Name(),
		"omitSignature", omitSignature, "instruction", instruction, "prompt", logging.Body(user))
	return a.stream(ctx, system, []Msg{{Role: RoleUser, Content: user}})
}

// SnoozeSuggestion is one AI-proposed wake time with its rationale.
type SnoozeSuggestion struct {
	At     time.Time
	Reason string
}

// SuggestSnooze proposes when a snoozed email should return, based on what the
// email says. An email with a concrete event time yields several useful
// moments (an hour before AND the day before); a deadline yields the day
// before. now anchors "today" for the model and validation. An empty slice
// with nil error means the email suggests nothing usable.
func (a *Assistant) SuggestSnooze(ctx context.Context, now time.Time, emailContext string) ([]SnoozeSuggestion, error) {
	start := time.Now()
	logging.Trace("ai: suggest snooze", "op", "SuggestSnooze", "provider", a.provider().Name(), "bytes", len(emailContext))
	system := "The user snoozes an email to deal with it at the right moment. Today is " +
		now.Format("Monday, 2 January 2006, 15:04") + " (local time). " +
		"If the email implies good times to resurface it, reply with one to three lines, most useful " +
		"first, each EXACTLY in the form YYYY-MM-DD HH:MM|reason (reason under 8 words, in the email's " +
		"language). For an event or meeting with a known time, offer BOTH one hour before AND the day " +
		"before at 09:00. For a deadline, the day before at 09:00. For a delivery or travel date, that " +
		"morning. Every time must be in the future. If the email suggests no particular time, reply " +
		"exactly: none"
	ch, err := a.stream(ctx, system, []Msg{{Role: RoleUser, Content: emailContext}})
	if err != nil {
		logging.Trace("ai: suggest snooze failed", "op", "SuggestSnooze", "err", err)
		return nil, err
	}
	var b strings.Builder
	for c := range ch {
		if c.Err != nil {
			logging.Trace("ai: suggest snooze failed", "op", "SuggestSnooze", "err", c.Err)
			return nil, c.Err
		}
		b.WriteString(c.Text)
	}
	suggestions := parseSnoozeSuggestions(b.String(), now)
	logging.Trace("ai: suggest snooze done", "op", "SuggestSnooze", "count", len(suggestions),
		"raw", logging.Body(b.String()), "dur", time.Since(start))
	return suggestions, nil
}

// parseSnoozeSuggestions parses up to three "YYYY-MM-DD HH:MM|reason" lines in
// local time. Malformed, past, or duplicate lines are skipped — the caller
// just offers fewer suggestions, never an error the user has to see.
func parseSnoozeSuggestions(s string, now time.Time) []SnoozeSuggestion {
	var out []SnoozeSuggestion
	seen := map[int64]bool{}
	for _, line := range strings.Split(s, "\n") {
		stamp, reason, _ := strings.Cut(strings.TrimSpace(line), "|")
		t, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(stamp), now.Location())
		if err != nil || !t.After(now) || seen[t.Unix()] {
			continue
		}
		seen[t.Unix()] = true
		out = append(out, SnoozeSuggestion{At: t, Reason: strings.TrimSpace(reason)})
		if len(out) == 3 {
			break
		}
	}
	return out
}

// BriefSummary condenses an email into one very short line for a desktop
// notification. Hard-capped client-side so a rambling model can't blow up the
// notification bubble.
func (a *Assistant) BriefSummary(ctx context.Context, emailContext string) (string, error) {
	start := time.Now()
	logging.Trace("ai: brief summary", "op", "BriefSummary", "provider", a.provider().Name(), "bytes", len(emailContext))
	system := "Summarize this email in ONE very short sentence, at most 12 words, in the email's " +
		"language. State the gist (what they want / what happened), not that it is an email. " +
		"Reply with only that sentence — no preamble, no quotes."
	ch, err := a.stream(ctx, system, []Msg{{Role: RoleUser, Content: emailContext}})
	if err != nil {
		logging.Trace("ai: brief summary failed", "op", "BriefSummary", "err", err)
		return "", err
	}
	var b strings.Builder
	for c := range ch {
		if c.Err != nil {
			logging.Trace("ai: brief summary failed", "op", "BriefSummary", "err", c.Err)
			return "", c.Err
		}
		b.WriteString(c.Text)
	}
	gist := strings.Join(strings.Fields(b.String()), " ") // collapse newlines/runs
	gist = strings.Trim(gist, `"'`)
	if r := []rune(gist); len(r) > 140 {
		gist = string(r[:139]) + "…"
	}
	logging.Trace("ai: brief summary done", "op", "BriefSummary", "gist", gist, "dur", time.Since(start))
	return gist, nil
}
