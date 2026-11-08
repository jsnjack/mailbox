# AI features

What the AI does with mail: the conversation summary, the one AI language
setting and why changing it drops the cached notes, translating a whole
conversation, draft replies, how markdown in AI answers is rendered, inbox
categories and the background worker that assigns them, and the automatic
Replied tag beside them. Read it before changing `internal/ai`,
`internal/aiwork`, or the AI parts of `internal/ui`. The AI in compose and the
AI-reply popover are in [compose.md](compose.md); provider and model-chain
settings are in [configuration.md](configuration.md).

Other docs, all in `docs/`: [architecture.md](architecture.md) (every file),
[accounts.md](accounts.md) (accounts, sync, snooze), [reader.md](reader.md)
(the reader), [threadlist.md](threadlist.md) (list and search),
[compose.md](compose.md) (compose, drafts, outbox), [ai.md](ai.md) (AI
features), [window.md](window.md) (colour, status bar, shortcuts) and
[configuration.md](configuration.md); [SETUP.md](SETUP.md) and
[BENCHMARK.md](BENCHMARK.md) are for people setting up the app or choosing a
model. [AGENTS.md](../AGENTS.md) says which to read before which change.

## Conversation summary

An AI-summary button reveals the **Conversation summary** card
pinned above the conversation that streams a bullet summary (`SummarizeThread`),
cached by the thread's message-id fingerprint (`summaryKey`) so reopening is
instant and a new reply auto-invalidates it; the summary is also **persisted**
keyed by thread id + that fingerprint (`store.{SetThreadSummary,ThreadSummary}`,
`thread_summaries` table), so an unchanged thread isn't re-summarized after a
restart.

## AI language

**One AI language** (Preferences → AI Features → **Language**, default
English, `config.Prefs.AILanguage` → `Assistant.SetLanguage`, applied live like a
provider swap) governs everything the AI says *about* the mail: the conversation
summary, the per-message gist, the phishing verdict and its reasons, a snooze
suggestion's reason, and the Translate action's target (`window.translateTarget`,
also the translations' cache key). "Same as the email" (`ai.LanguageMatch`) keeps
the older follow-the-mail behaviour, and is the one value Translate can't honour
— translating a mail into its own language is a no-op — so it falls back to
`ai.DefaultLanguage` there. The prompts share `Assistant.languageClause` (a
sentence) and `languagePhrase` (a parenthetical, where a strict output format
leaves no room). Nothing that writes mail *as* the user follows it — draft,
refine, proofread, subject, quick replies stay in the correspondent's language,
the only right answer there — and category tags stay fixed ids. Because the
summary, gist and analysis caches are permanent by design (a body never changes),
changing the language drops all three (`store.ClearAINotes` + the in-memory maps,
`dropAINotes`) and they are re-earned on open, on demand, and by the worker's
next pass: otherwise the setting would only reach mail that hasn't arrived yet,
and the inbox that prompted the change would keep its old-language notes forever.
Every AI-note writer captures `store.AINotesGeneration` before requesting AI and
persists through `SaveAINote`, which serializes the generation check and write
with `ClearAINotes`; an older request cannot restore a cleared note. UI callbacks
and rendered sections are also guarded by a note generation. The reset clears
all section caches and explicitly wakes the background worker for every account.
Translations are exempt from deletion: both the memory and database caches are
keyed by account, message and target language, so a change misses and translates
afresh while switching back reuses the earlier translation.

## Translation

Translate (`onTranslate`) renders a translation of the whole open conversation
into the configured AI language (English by default) in place (markup preserved, "Show original" reverts): every message
is translated concurrently and cached per account, message id and target language in `translationCache`
(`showTranslatedConversation` rebuilds the stacked sections from the cache), so
reverting, re-opening, or re-translating reuses cached results and an
already-translated message isn't redone. Translations are also **persisted** per
message (`store.{SetTranslation,Translations}`, `message_translations` table,
keyed by gmail id + target lang — a body is immutable so they never go stale), so
`onTranslate` seeds from the cache for free before calling the AI for the rest.
Only the text travels, and the conversation's text travels **pooled**:
`planTranslation` extracts each body's visible text into a `translationPlan`,
`poolTranslations` translates the union of every body's segments, and
`plan.render` writes the answers back into the original markup, so the model
never sees a tag. Extraction and reassembly are separate phases precisely so the
pool can span messages — each message quotes the ones before it, so the same
paragraph appears in several bodies (314 segments, 85 distinct, on a real
five-message thread), and pooling asks for it once and renders every copy
identically instead of re-wording it per message. The pool goes out in bounded
batches (`translateBatch` 40, `translateWorkers` 4): one request for a whole
thread cannot overlap with itself and measured 67s where parallel batches of the
same text measured 7.6s. Preformatted text is segmented **per paragraph**, not
per node: a plain-text body is one `<pre>`, so the whole email is a single text
node, and a whole email offered as one snippet reads to the model like several —
it answers with one element per paragraph, and only the first would be kept
(which silently dropped everything after the greeting). Flowed HTML is never
split, where a blank line is insignificant whitespace inside a sentence.
Translations are looked up by source text, so a segment the model skipped keeps
its own original wording and nothing after it moves; a reply with *more* elements
than segments still can't be paired up positionally, so a single segment re-joins
them (`zipTranslations`) and anything else fails the batch rather than mixing one
paragraph's translation into another's. A failed translation keeps the
conversation on screen (the translation is only swapped in once ready) and says
so in a toast, and persists nothing, so translating again retries.
(Both AI caches, like categories, are dropped for a message when it is deleted —
see `deleteMessageTx`.)

Draft-reply streams into a compose window via the `ai` provider.

## Markdown in AI answers

Every AI answer is written in markdown whether or not it was asked for, so it is
rendered rather than shown raw (`internal/ui/markdown.go`: bold, inline code,
italics, bullets and headings, into Pango markup for GTK labels and HTML for the
reader). Only balanced markers produce tags — a half-streamed `**` stays literal
instead of yielding markup Pango would refuse, which blanks a label. ✦ marks
AI-written text everywhere (the card's title, the reader's inline note), and the
words carry the scope: the panel is the "Conversation summary" with the message
count beside it, each message's own line is "In short".

## Inbox categories

Inbox mail is auto-categorized by AI into action tags (Needs reply / Calendar /
Travel / Receipt / Finance / Security / Discount / Newsletter / Notification; no
match = no tag) shown on rows — category definitions live in the prompt — and
**persisted per email** keyed by the latest message's id
(`store.{SetMessageCategory,MessageCategories}`, `message_categories` table).
Classification runs in a **headless background worker** (`internal/aiwork`) for
**every connected account** — not just the active one — so tags are ready before
an account is switched to: the worker does a catch-up sweep at launch, then
reacts to `MessageUpserted` sync events (debounced so it never competes with an
active sync), one request per email through a small pool, capped per pass with
the rest chained, and a failing provider pauses it for a cooldown. Each stored
pass publishes an `AIUpdated` hub change; the UI's `categorizeInbox` is
seed-only (two batched cache queries, re-binding rows only when a tag actually
changed) — each email is classified once, not every launch. Gated by a
Preferences toggle (read per pass). Because results are cached, a
category-prompt change won't re-classify existing mail on its own; the
thread-list overflow menu's "Re-categorize inbox" (`onRecategorize` →
`store.ClearCategories` + `Worker.Trigger`, which also lifts the cooldown)
forces a re-run.

## The Replied tag

The automatic "Replied" tag requires the newest non-draft message to carry
SENT and an earlier message in the same account and thread without SENT or
DRAFT. A single message labelled both INBOX and SENT, such as an appointment
notification sent from the account's own address, does not qualify. Drafts
neither establish received history nor change which delivered message is newest.
