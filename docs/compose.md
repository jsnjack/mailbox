# Compose, drafts and sending

The compose window: replies and forwards that quote the original's HTML,
recipients and autocomplete, the AI Draft and Refine actions, saving and
discarding, switching the From account, signatures, resuming a draft and
drafts inside conversations, the reader's AI-reply popover, attachments, Undo
Send and the outbox. Read it before changing `internal/ui/compose*.go`,
`replydialog.go`, `recipients.go`, `draftundo.go`, `outbox.go`, or the draft
and outbox code in `internal/syncer` and `internal/store`.

Other docs, all in `docs/`: [architecture.md](architecture.md) (every file),
[accounts.md](accounts.md) (accounts, sync, snooze), [reader.md](reader.md)
(the reader), [threadlist.md](threadlist.md) (list and search),
[compose.md](compose.md) (compose, drafts, outbox), [ai.md](ai.md) (AI
features), [window.md](window.md) (colour, status bar, shortcuts) and
[configuration.md](configuration.md); [SETUP.md](SETUP.md) and
[BENCHMARK.md](BENCHMARK.md) are for people setting up the app or choosing a
model. [AGENTS.md](../AGENTS.md) says which to read before which change.

## Compose window, quoting and sending

Reply / reply-all / forward / new compose in a separate window. Compose is a
plain-text editor, but outgoing mail is multipart/alternative (text +
quoted-printable HTML via `backend.BuildMIME`; nested in multipart/mixed with
attachments): `buildHTMLBody` renders the user's text escaped+linkified, and —
while the prefilled quote region is unedited (`quoteBoundary` vs the snapshot)
— embeds the original's sanitized HTML (`bodyHTMLFor` → `QuoteHTML`) as a
left-rule blockquote for replies or unquoted under the header block for
forwards, so recipients see the original's real formatting; an edited quote
falls back to rendering the whole text so the alternatives never disagree.
Replies quote with the conventional "On <date>, Name <addr> wrote:" attribution
+ "> " markers; forwards use the standard "---------- Forwarded message
---------" From/Date/Subject/To block with the body unquoted. `htmlToText`
(quote/AI text source) walks the HTML tree and preserves link targets as
"text (url)". (Send: `messages.send`, threading headers + threadId on replies;
replies target the sender's `Reply-To` header when present — captured into
`model.Message.ReplyTo` / the `reply_to` column — else the From address, via
`replyTarget`);

## Recipients and subject

To/Cc/Bcc autocomplete from past correspondents (`store.Contacts` ranks
addresses seen in cached mail by frequency+recency) plus the user's own
registered accounts (`withOwnAccounts`, listed first so you can address another
of your accounts); a `GtkEntryCompletion` completes the last comma-separated
token. A sparkle button next to Subject generates it from the body
(`Assistant.GenerateSubject`).

## AI in compose

Visible AI Draft and Refine buttons provide direct access to compose AI. Refine
opens a compact panel beneath the toolbar with an instruction field and Fix grammar,
Shorten, and More natural shortcuts. The panel closes on submission or Escape.
These actions use `previewComposeAI`:
generation leaves the editor unchanged, a complete successful result opens for
review with Apply/Cancel, and Apply is one GTK undo action. An intervening edit
invalidates the preview. Cancel or compose close cancels the request; partial
stream failures never become outgoing text. `askAIIntent` collects drafting
instructions and offers suggested replies. Refine and Proofread use the selected
span, or the writing above the quote when nothing is selected.

## Saving and discarding

Compose keeps Save draft and Discard available. A fixed bottom strip shows
Saving/Saved/Save failed and a deduplicated recipient count. To/Cc/Bcc use
compact single-line text fields with autocomplete and inline validation. Long
recipient lists scroll within the entry instead of increasing its height; empty
Cc/Bcc fields stay hidden until requested. Bcc-only
and Cc-only messages are accepted. The reader's primary action is explicitly
labelled Reply all, with its audience count in the tooltip. Closing saves dirty content and
closes after the local write succeeds; failures keep the editor open. Discard
serializes against pending saves, deletes the exact local draft, and offers Undo
that restores the captured content as a new draft. Send also waits for any active
save and invalidates stale queued snapshots.

## Switching the From account

From remains selectable in fresh and resumed drafts. Switching saves a new draft
under the destination account before deleting the source, clears provider IDs,
and replaces only an unchanged trailing account signature. Edited signatures are
preserved. Provider-only drafts first gain a local identity so cleanup targets the
exact draft. The store and outbox reject cross-account draft IDs.

## Pre-send guards and signatures

Send runs pre-send guards (`preSendWarning`: empty subject or a mentioned but
missing attachment requires confirmation). A configurable signature
is appended to every composed body below the cursor area and above any quote
(`composeBodyWithSignature`) as a plain sign-off — no RFC 3676 "-- " delimiter
(Gmail/Outlook don't honor it, so for a short sign-off it just shows a stray
"--" line); it is not re-added when editing an existing draft. Signatures have a **global default**
(`config.{Load,Save}Signature`, `<config>/signature.txt`) plus optional
**per-account overrides** (`signatures.json` keyed by email,
`config.{Load,Save}AccountSignature`; a blank override is removed so the account
falls back to the default — "blank means use the default"). `signatureForActive`
resolves which a compose appends: the global default for a single account, else
`config.SignatureFor(activeEmail)` (override-or-default); `w.signature` is
refreshed on account switch. Preferences always shows an editable Default
signature, plus one override editor per account when more than one is connected.

## Resuming a draft

Clicking a conversation in the Drafts folder resumes editing it in compose (`openDraftForEdit`) instead of rendering
it read-only: the body/recipients are prefilled and the draft's Gmail resource
id is resolved (`Client.FindDraftID`) so Save updates that draft
(`Drafts.Update`) and Send sends then removes it (`Drafts.Delete`) — never a
duplicate; a resumed draft never re-appends the signature, even when the
resource id couldn't be resolved offline.

## Drafts inside conversations

**Drafts inside conversations**: a draft reply is autosaved by Gmail into the real thread, so without a mark it
renders as delivered mail. `model.Message.IsDraft` is hydrated on every store
read (an indexed EXISTS on the DRAFT label in `msgCols` — cheaper than label
slices, which only writers populate) and `ThreadSummary.HasDraft`
(`markHasDraft`) flags threads holding one: the list row shows a red "Draft"
tag (`.cat-draft`, skipped in Sent/Drafts where it's redundant), and the
reader's draft section carries a red "Draft" chip on its sender line plus an
"Edit draft" link (`mbaction:editdraft` → `openDraftForEditMsg`, the
message-targeted resume). A draft's ⋯ menu offers Edit/Discard instead of
Reply/Forward, draft sections are excluded from the immutable-section cache
(their bodies mutate between saves), and the header bar's
reply/forward/star/unread target the newest *delivered* message
(`newestActionable`) so answering a thread can't quote your own unsent draft.
`openDraftForEdit` and the engine's `DiscardDraft` select the draft by
`IsDraft`, never "newest" — after an incoming reply the newest message is
someone else's mail.

## The AI-reply popover

The reader's AI-reply popover contains only fixed presets and **Custom reply…**;
it never starts suggestion requests or resizes as results arrive. Custom reply
opens `askAIIntent` before creating a compose. Its fixed-size dialog has a
header with Cancel/Create draft, an instruction editor, and
generated replies directly underneath it, without tabs or preset buttons.
Suggestions load when the dialog opens, use actionable native boxed rows with
forward arrows in a reserved scrollable area, and
open an editable compose immediately when selected, closing the dialog.
Create draft applies only to the custom instruction. Try again regenerates; closing cancels pending requests. A bounded
session cache keyed by the full conversation context hash reuses suggestions on
reopen and misses when the conversation changes. Smart-replies-only settings
show only the generated replies. `TestReplyDialogGTK` verifies sizing and the
asynchronous interaction; `make test-ui` runs it with the other GTK tests.

## Attachments, Undo Send and the outbox

Compose supports attachments (the file picker accepts multiple files and shows
them as wrapping, width-bounded chips; decoded payloads are capped at 18 MiB total before reading, and `BuildMIME` emits multipart/mixed with streaming line-wrapped base64 encoding). Sending uses Undo Send: the compose closes
and the message is held ~5s behind an "Undo" toast (`deferSend`) before it goes
out; a failed send is queued to the `outbox` table and retried by a background
sweeper (`SweepOutbox`, ~45s); pending/failed sends are surfaced by an
`adw.Banner` over the thread list and an Outbox dialog (retry, Edit as draft,
discard with Undo, and Send now). `RecoverOutboxDraft` parses the queued MIME and
atomically cancels an unclaimed send and saves its editable local draft; failure
rolls back both. Restoring a discarded outbox item preserves its UUID, attempt
count, and uncertain-delivery state, so Undo does not turn uncertainty into an
automatic retry. Undo Send opens compose only after successful cancellation; its queries run off the GTK thread and actions disable while showing progress.
