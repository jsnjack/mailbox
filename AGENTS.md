# AGENTS.md

> See [AGENTS.universal.md](./AGENTS.universal.md) and [AGENTS.go.md](./AGENTS.go.md) for universal conventions.
> Refresh: `make standards`

---

## Overview

`mailbox` is a native, fast desktop email client for Linux/GNOME, written in Go +
GTK4 (`gotk4` + `libadwaita`). It targets Gmail via the Gmail REST API, presents a
modern 3-pane layout (accounts/labels | thread list | message body), and includes
AI features (draft reply, translate email). Multi-account; Linux-only for now.

---

## Architecture

One-way data flow: the local SQLite cache is the single source of truth. Background
goroutines sync Gmail into SQLite and publish ID-only change events; the GTK UI reads
SQLite and re-queries on each event. GTK is single-threaded — every async→UI update
is marshalled through `internal/dispatch.Main` (wraps `glib.IdleAdd`).

```
Gmail REST API → sync engine (goroutines) → SQLite (source of truth)
                       │ notify.Hub (IDs only)        ▲ read-only queries
                       ▼                              │
                dispatch.Main (glib.IdleAdd) → GTK UI (main thread)
```

```
cmd/mailbox/         thin cobra entry point (flags, logger wiring, launches the app)
internal/
  model/             plain domain structs (Account, Thread, Message, Label) — no GTK
  config/            XDG paths + config.toml load/save
  dispatch/          THE main-thread bridge: Main(fn) → glib.IdleAdd
  store/             SQLite layer (schema, FTS5, queries) — modernc.org/sqlite
  auth/              OAuth2 installed-app loopback + keyring-backed token source (auto-refresh, rotated-token write-back, IsAuthError detects a revoked/expired refresh token)
  backend/           the provider-agnostic Backend interface the engine drives (domain-typed: model.Message/Label, opaque sync cursor) + BuildMIME (RFC 5322). No protocol specifics — Gmail today, IMAP planned.
  gmailapi/          wrapper over google.golang.org/api/gmail/v1 (semaphore, per-attempt quota budget, backoff honoring Retry-After; network errors retried for idempotent calls but not sends)
  gmailbackend/      implements backend.Backend over gmailapi.Client (owns the Gmail↔domain conversions + the history-walk → upsert/delete id set)
  imapbackend/       implements backend.Backend over IMAP (emersion/go-imap v2): connect/LOGIN, LIST folders→labels (special-use mapped), multi-folder backfill (skips \All/\Flagged/\Important virtuals), FETCH envelope/flags + body (go-message), and incremental sync. Message id = "imap:<uidvalidity>:<uid>:<mailbox>". Incremental (`Changes`) diffs a per-folder UID-set cursor (JSON in sync_cursor): new = current\stored, vanished = stored\current, UIDVALIDITY change re-syncs the folder; CONDSTORE `CHANGEDSINCE` adds flag-change detection when the server supports it (QRESYNC isn't exposed in go-imap beta.8, so deletions use the UID-set diff). Profile seeds the initial cursor. Mutations, SMTP send, XOAUTH2, threading, and a connection pool (currently one mutex-guarded conn) are still TODO.
  sync/              per-account sync workers (backfill ↔ incremental) + notify.Hub; the engine takes a backend.Backend, never a concrete client
  ai/                provider abstraction (OpenAI-compatible + Anthropic), streaming
  activity/          headless pub/sub of transient "what is the app doing" events (status bar)
  ui/                all GTK/adw/webkit widget code (3-pane shell, list, reader, actions)
```

Multi-account: the launcher builds one Gmail client per connected account and
syncs each; the UI tracks an active account (a switcher appears in the sidebar
when more than one is connected) and routes every operation by account id. New
accounts are added via `mailbox sync --account`. Each switcher row shows the
account's display name (email as a caption when named) and an unread-INBOX
count pill;
names are user-assigned in Preferences → Accounts (`config.{Load,Save}AccountName`,
stored in `<data>/accounts.json` keyed by email — when set, the name is primary
and the email becomes a caption). Per-account pills and the window title come
from one batched `store.UnreadCountByLabelForAccounts` query (`refreshAccountUnread`
/ `applyAccountUnread`), refreshed on any sidebar reload and whenever a non-active
account syncs. `loadLabels` only rebuilds the sidebar widgets when its structure or
the inbox badge actually changed (`sidebarSignature`), so an idle 60s sync touches
nothing.

Colour follows GNOME's HIG (monochrome symbolic icons, one accent reserved for
state): a small application stylesheet (`internal/ui/theme.go`, registered on
the default display in `build`) tints only three things, all from the system
`@accent_color` family so they track the accent and light/dark — count pills
(`countBadge`/`.badge-pill`), the small unread dot on a conversation, and the
soft accent-tinted AI summary card. Folder icons stay the theme foreground and
the account switcher is plain text (no avatars).

UI state (implemented): 3-pane shell renders the cached account live; clicking a
message lazily fetches + sanitizes + renders its body (WebKit; remote images off
behind a toggle). The reader sanitizes with an email-tuned bluemonday policy
(`emailPolicy`, keeps inline styles + tables so HTML mail isn't broken) and
`wrapHTML` injects a fit-to-width script that scales over-wide email to the pane
(no horizontal scrollbar, no cropping). JavaScript is enabled **only** for that
script: a strict per-render CSP (`script-src` pinned to a nonce, `default-src
'none'`) plus the sanitizer mean no email-supplied script can run or reach the
network. Remote images load by default, but tracking pixels are stripped before
render (`cleanEmailHTML`: 1x1/tiny imgs, 1px-styled imgs, and known open-tracker
URL patterns) and the count is surfaced as a "🛡 N trackers blocked" indicator.
A plain-text body (and the snippet fallback) is HTML-escaped into a `<pre>` with
bare http(s) URLs auto-linkified (`linkifyText` — explicit-scheme match only, so
no false positives or non-http schemes), so links in text-only mail (CI/cron/
monitoring notifications) are clickable and open externally like any other link.
A sender-authentication badge shows Gmail's SPF/DKIM/DMARC verdict
(`ToBody` captures `Authentication-Results` into `raw_headers`; `parseAuthResults`
→ green verified / amber partial / red possible-spoof), plus deterministic
anti-phishing heuristics (`phishing.go`: display-name spoofing and deceptive
link text, compared at the registrable-domain level — no AI/network) surfaced as
an amber caution line; the reader overflow's "Check for phishing" additionally
runs an on-demand AI phishing analysis (`AnalyzeEmail` — verdict + reasons, fed
the auth/heuristic signals, shown in the shared AI card), **persisted** per message
(`store.{SetAnalysis,Analysis}`, `message_analyses` table — the message + its
signals are immutable, so it's reused on re-open without re-running the AI). A thread is rendered newest-message-first, with quoted reply history collapsed
behind a native <details> "Show quoted text" toggle (`cleanEmailHTML`, same single HTML pass as tracker stripping, no JS). An AI-summary button reveals a card
pinned above the conversation that streams a bullet summary (`SummarizeThread`),
cached by the thread's message-id fingerprint (`summaryKey`) so reopening is
instant and a new reply auto-invalidates it; the summary is also **persisted**
keyed by thread id + that fingerprint (`store.{SetThreadSummary,ThreadSummary}`,
`thread_summaries` table), so an unchanged thread isn't re-summarized after a
restart. Message headers show the sender's
full address ("Name <addr>"), not just the display name. Reader actions archive /
mark-unread / star / move-to-inbox / report-spam (or not-spam in the Spam
folder), plus "Delete forever" in Trash/Spam and an "Empty now" banner that empties
the whole folder (`DeletePermanently`/`EmptyLabel` → `messages.batchDelete`), via
optimistic `ModifyLabels` + Gmail mirror; opening an unread message marks it read;
Ctrl +/-/0 zoom the message view (`WebView.SetZoomLevel`, persisted);
a 60s background incremental sync updates label counts through `dispatch`→`Hub`,
and new inbox mail (arriving after launch) raises a desktop notification via
`gio.Notification`. The background sync self-heals an expired history watermark
(an account offline past Gmail's history window) by re-backfilling and resetting
the watermark (`engine.Resync` on `ErrHistoryExpired`); a revoked/expired refresh
token (`auth.IsAuthError`) instead publishes `AuthExpired`, which reveals a
"reconnect" banner (it can't recover without re-login). (GNOME routes a notification only when it can resolve the
GApplication app-id `com.jsnjack.mailbox` to an installed `*.desktop` entry; the
RPM ships `com.jsnjack.mailbox.desktop` under `/usr/share/applications`, and for a
binary run from `bin/` `ensureDesktopFile` self-installs a user-level entry —
pointed at the running binary — into `~/.local/share/applications` at startup,
skipping it when a system or user entry already exists so it never shadows a
real install.)
The desktop entry registers `MimeType=x-scheme-handler/mailto;` with `Exec=mailbox
%u`, so the app appears under GNOME's Default Apps → Mail and can be set as the
default mail client. The GApplication uses `ApplicationHandlesOpen`: a clicked
`mailto:` URI is delivered to the `open` handler (and routed to an
already-running instance), which opens a prefilled compose (`parseMailto` →
`composeFromMailto`, handling both the plain `mailto:` and GIO's normalised
`mailto:///` forms). `main` strips the `mailto:` arg before cobra parses (the
root command has subcommands) — `SetArgs` with a non-nil empty slice, since
`SetArgs(nil)` falls back to `os.Args`. The RPM's `%post` runs
`update-desktop-database` so the registration takes effect on install; the dev
self-install runs it too.
Reply / reply-all / forward / new compose in a separate window (text/plain via
`gmailapi.BuildMIME` + `messages.send`, threading headers + threadId on replies;
replies target the sender's `Reply-To` header when present — captured into
`model.Message.ReplyTo` / the `reply_to` column — else the From address, via
`replyTarget`);
To/Cc/Bcc autocomplete from past correspondents (`store.Contacts` ranks
addresses seen in cached mail by frequency+recency) plus the user's own
registered accounts (`withOwnAccounts`, listed first so you can address another
of your accounts); a `GtkEntryCompletion` completes the last comma-separated
token. A sparkle button next to Subject generates it from the body
(`Assistant.GenerateSubject`). The compose window has an AI-draft button (streams a drafted reply via
`DraftReply` for a reply/forward, or a from-scratch body via `DraftNew` for a
new message — both prompted by `askAIIntent`, which also offers an on-demand "Suggest quick replies" button for a reply via `SmartReplies`), an AI grammar-check button
(`Proofread`), and a Save-draft button
(`users.drafts.create`). Send runs pre-send guards (`preSendWarning`: empty
subject, "attachment" mentioned but none attached → confirm), and closing an
unsent message offers Save-as-draft alongside Discard. A configurable signature
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
Clicking a conversation in the Drafts
folder resumes editing it in compose (`openDraftForEdit`) instead of rendering
it read-only: the body/recipients are prefilled and the draft's Gmail resource
id is resolved (`Client.FindDraftID`) so Save updates that draft
(`Drafts.Update`) and Send sends then removes it (`Drafts.Delete`) — never a
duplicate.
Translate (`onTranslate`) renders an English translation of the whole open
conversation in place (markup preserved, "Show original" reverts): every message
is translated concurrently and cached per message id in `translationCache`
(`showTranslatedConversation` rebuilds the stacked sections from the cache), so
reverting, re-opening, or re-translating reuses cached results and an
already-translated message isn't redone. Translations are also **persisted** per
message (`store.{SetTranslation,Translations}`, `message_translations` table,
keyed by gmail id + target lang — a body is immutable so they never go stale), so
`onTranslate` seeds from the cache for free before calling the AI for the rest.
(Both AI caches, like categories, are dropped for a message when it is deleted —
see `deleteMessageTx`.) Draft-reply streams into a compose
window via the `ai` provider. Incoming
attachments are extracted on body fetch (`ReplaceAttachments`) and shown as chips
in the reader; clicking one downloads it (content-addressed under the cache dir)
and opens it with `xdg-open`.

Inbox mail is auto-categorized by AI into action tags (Needs reply / Calendar /
Travel / Receipt / Finance / Security / Discount / Newsletter / Notification; no
match = no tag) shown on rows — category definitions live in the prompt —
batched and **persisted per email** keyed by the latest message's id
(`store.{SetMessageCategory,MessageCategories}`, `message_categories` table), so
`categorizeInbox` seeds tags from the cache for free on launch and only calls the
AI for still-uncategorized threads (capped per pass) — each email is classified
once, not every launch. Gated by a Preferences toggle (`ai.Categorize` /
`categorizeInbox`). Because results are cached, a category-prompt change won't
re-classify existing mail on its own; the thread-list overflow menu's
"Re-categorize inbox" (`onRecategorize` → `store.ClearCategories` + a fresh pass,
inbox-only) forces a re-run.
The list is grouped by conversation: a virtualized `gtk.ListView` over a
`gtk.StringList` of thread ids (looked up in a `threadByID` map of
`model.ThreadSummary`); rows show the newest message + a count. Refreshes are
incremental (`diffThreadModel`): an unchanged list (the common idle-sync case)
does no work, an in-place change (mark-read, star, a new AI category tag) re-
binds only the affected rows via a per-row signature (`renderSig`), and only a
changed set/order triggers a full splice — so the list keeps its scroll position
instead of rebuilding on every event. `store`
provides `ListThreadsByLabel`/`ListThreadMessages`/`GetThreadSummaries`. Opening a
thread renders all its messages stacked in the reader (bodies fetched lazily,
each a sanitized section); archive/trash apply to the whole thread, reply/star
to its newest message. A search entry runs instant local FTS5 search
(`store.Search`, sanitized into a quoted prefix MATCH) whose hits are grouped
into threads; clearing it returns to the current label. Every list populate (a
label switch, a search, the 60s sync refresh) runs its store query off the main
thread via `loadThreads`, guarded by a `refreshGen` counter so a slow query
can't overwrite fresher results (last request wins); search hits are turned into
thread summaries with one batched `store.GetThreadSummaries` (and server-search
ids mapped via `store.ThreadIDsForMessages`) rather than a query per hit. When a search has no
local matches, "Search all mail" runs a Gmail server-side search
(`SearchServer` → `ListMessageIDs` with `q=`), caching matches beyond the local
cache; a reader action "Find emails from sender" runs the same server search
with a `from:` query. Server-search results persist a `serverSearch`/`serverQuery`
mode so the debounced search-changed signal and 60s background refreshes don't
clobber them with an empty local FTS pass. A selection-mode toggle
turns rows into checkboxes with a bulk-action bar (Archive / Trash / Mark read),
applying the change to every selected conversation in one batched `ModifyLabels`
call (`bulkApply`). Label changes from the bulk bar and the right-click row menu
(`threadModifyAll`) show the same reversing "Undo" toast (`showUndoToast`) as the
reader's archive/trash, so every archive/trash is recoverable. The AI-draft dialog offers on-demand quick replies
(`SmartReplies`, behind a "Suggest quick replies" button).
Compose supports attachments (a file picker adds them; `BuildMIME` emits
multipart/mixed with base64 parts). Sending uses Undo Send: the compose closes
and the message is held ~5s behind an "Undo" toast (`deferSend`) before it goes
out; a failed send is queued to the `outbox` table and retried by a background
sweeper (`SweepOutbox`, ~45s); pending/failed sends are surfaced by an
`adw.Banner` over the thread list and an Outbox dialog (per-item retry/discard
plus "send now"). A bottom status bar shows what the app is doing — the current
operation with a spinner/progress bar (left) and live cumulative metrics (right:
bytes transferred, Gmail API requests + quota units, DB size, cached-message
count) — plus an expandable timestamped activity log. Background layers report
transient activity to a headless `activity.Hub` (`deps.Activity`) that the bar
drains via `dispatch`; the AI ops the UI brackets directly; metrics come from
per-account `gmailapi.Stats` (a byte-counting transport + request/quota counters)
aggregated by `deps.Stats`. The window collapses responsively via
`adw.Breakpoint` (3 panes → list+reader below ~860sp → single pane below ~520sp),
with `SetShowContent` driving navigation when collapsed. Single-key shortcuts
(bubble-phase key controller, so text entries keep their input): j/k navigate
threads, r reply, a archive, c compose, / focus search. Test hooks:
`MAILBOX_OPEN_FIRST=1` opens the newest message on launch; `MAILBOX_WIN_SIZE=WxH`
overrides the initial window size; `MAILBOX_APP_ID` overrides the GApplication id
so a sandbox instance runs alongside a real one instead of activating it.

Verify GUI changes in a throwaway sandbox rather than launching the installed
app: copy the live DB + config into a temp dir, point `XDG_{DATA,CONFIG,CACHE}_HOME`
there, and run under Xvfb (`GDK_BACKEND=x11 GSK_RENDERER=cairo`) with a distinct
`MAILBOX_APP_ID`. It shares the real session bus (so the keyring still resolves
OAuth tokens + the AI key) but starts a fresh instance, so it never disturbs a
running app. `MAILBOX_DEMO=1` hides the "read-only" banner for clean screenshots.

Dependency rule: `store`/`backend`/`gmailapi`/`gmailbackend`/`imapbackend`/`sync`/`auth`/`ai`
MUST NOT import any GTK package (they are headless and unit-testable without a
display). `ui` MUST NOT import `sync`/`gmailapi`/`ai` directly — inject interfaces
and communicate via channels + `dispatch`. The sync engine MUST depend only on
`backend.Backend`, never a concrete provider client, so new providers (IMAP) drop
in without touching the engine.

---

## Build & Run

```
make check    # fmt → vet → build → test → lint (gate after every change)
make build    # compile the dynamically-linked binary into bin/
make run      # build and launch the GTK app
make rpm      # build the RPM (packaging/mailbox.spec)
```

Headless sync (no GTK; validates the read path end-to-end):

```
mailbox sync --account <email> --credentials <client_secret.json> [--limit N]
```

It connects (keyring refresh token if present, else interactive loopback login),
syncs labels, backfills the newest N messages into `~/.local/share/mailbox/mailbox.db`,
runs one incremental pass, and prints cache stats.

Requires GTK4/WebKit dev libraries for the GUI — see "Gotchas". The first cgo
build of the gotk4 bindings is heavy (~10-15 min) but cached in `GOCACHE`
afterward. The `sync` command and the headless packages build without GTK.

---

## Configuration

- Config file: `~/.config/mailbox/config.toml`, `[ai]` table: `provider` (`openai`|`litellm`|`anthropic`), `endpoint` (base URL incl. `/v1`), `model`. Editable in-app via the Preferences dialog (`ai.SaveConfig`; applies on next launch). Env overrides: `MAILBOX_AI_{PROVIDER,ENDPOINT,MODEL,KEY}`.
- AI API key: keyring service `mailbox-ai` (user = provider) or `MAILBOX_AI_KEY`; never in the config file. Store it with `printf '%s' "$KEY" | mailbox set-ai-key`.
- Persistent state (SQLite DB): `~/.local/share/mailbox/mailbox.db`. Preferences → Storage can clear the attachment cache (`config.ClearAttachmentsCache`) and compact the DB (`store.Vacuum` — `VACUUM` + WAL-truncate, reclaiming pages freed by deleted mail; WAL keeps that space otherwise).
- Account display names: `~/.local/share/mailbox/accounts.json` (email → name).
- Default signature: `~/.config/mailbox/signature.txt` (plain text, may be empty); per-account overrides in `~/.config/mailbox/signatures.json` (email → signature).
- View state (last folder, unread filter, reader zoom): `~/.local/share/mailbox/view.json`.
- General prefs (e.g. block remote images by default): `~/.config/mailbox/prefs.json`.
- Attachment cache: `~/.cache/mailbox/attachments/` (content-addressed by sha256).
- Secrets (OAuth refresh tokens, AI API keys): OS keyring via Secret Service.
- Trace log: `/tmp/mailbox.log` (truncated each start; enabled with `--trace`).

---

## Design Decisions

- Gmail REST API, not IMAP — native labels/threads, `history.list` incremental sync, server-side search.
- Local SQLite + FTS5 as source of truth; metadata separate from bodies; bodies lazy-loaded.
- Desktop polls `history.list` (no public endpoint for Pub/Sub push). Backfill fetches metadata concurrently (`backfillWorkers`) but commits in `store.UpsertMessages` batches of `backfillBatch` (200) — one transaction/fsync and FTS reindex per batch, not per message; incremental sync likewise batches its deletes (`store.DeleteMessages`) and upserts into one transaction each, then publishes a per-id `MessageUpserted` so new-mail notifications still fire.
- HTML email rendered in a locked-down WebKitGTK view: email-tuned sanitizer (keeps styling), remote images blocked by default, links open externally. JavaScript is enabled only to run a trusted fit-to-width script, fenced off by a per-render nonce CSP with `default-src 'none'` so no email script can run or phone home. AI translate renders in place (markup-preserving) with a revert toggle.
- AI provider is user-configurable behind one `Provider` interface (OpenAI-compatible covers the LiteLLM proxy + OpenAI; Anthropic direct).
- Distribution is RPM (GTK4/WebKit cannot be statically linked into a single binary).

---

## Gotchas

- GTK4 is single-threaded: never touch a widget off the main loop — route through `dispatch.Main`.
- Build needs system dev packages: `webkitgtk6.0-devel libsoup3-devel libsecret-devel` (plus `gtk4-devel`, `libadwaita-devel`).
- The Makefile is Linux-only (`CGO_ENABLED=1`); the standards cross-compile targets are intentionally dropped.
- FTS5 is written explicitly by the store (not via triggers) because searchable text spans two tables.
- Validated GTK binding pins (confirmed to compile + run against system GTK4 4.22.4 / libadwaita 1.9.1 / WebKitGTK 2.52.4 — re-pin these exactly in Phase 2): gotk4 `pkg v0.3.2-0.20250703063411-16654385f59a`, gotk4-adwaita `pkg v0.0.0-20250703085337-e94555b846b6`, gotk4-webkitgtk `pkg v0.0.0-20240108031600-dee1973cf440`. The WebView method is `LoadHtml` (gotk4 lowercases acronyms), not `LoadHTML`.
