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

## Where the details are

This file holds what every session needs: the data flow, the package map, the
rules that cross packages, the commands, and the traps. The full design of
each area is in `docs/`. Read the file for the area before changing it:

| Working on | Read |
|---|---|
| a package or file you have not seen before; the design decisions in full | [docs/architecture.md](docs/architecture.md) |
| accounts and providers, `cmd/mailbox/launch.go`, `internal/syncer`, `internal/snooze`, `internal/auth`, the backends, notifications, `mailto:` | [docs/accounts.md](docs/accounts.md) |
| the reader: body rendering, the sanitizer and email CSS, the WebKit shell page, remote images, links, phishing, invites, unsubscribe, zoom | [docs/reader.md](docs/reader.md) |
| the thread list, search, paging, bulk actions, Undo toasts | [docs/threadlist.md](docs/threadlist.md) |
| compose, drafts, signatures, the AI-reply popover, Undo Send, the outbox | [docs/compose.md](docs/compose.md) |
| `internal/ai`, `internal/aiwork`: summaries, the AI language, translation, categories | [docs/ai.md](docs/ai.md) |
| colour, the status bar and activity log, the responsive layout, shortcuts, test hooks | [docs/window.md](docs/window.md) |
| `config.toml`, the AI chain and keys, where every file lives | [docs/configuration.md](docs/configuration.md) |
| building and connecting an account, as a user does it | [docs/SETUP.md](docs/SETUP.md) |
| choosing a local AI model, `bench/` | [docs/BENCHMARK.md](docs/BENCHMARK.md) |

This file stays under 30,000 bytes, and `make check` fails when it is over (AGENTS.universal.md, "AGENTS.md size"). When a change alters a flow, update that flow's file in `docs/`, not this one.

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

One line per package; [docs/architecture.md](docs/architecture.md) lists
every file.

```
cmd/mailbox/             cobra entry point: flags, logger wiring, the GTK app (launch.go starts each
                         account's backend and sync loops), the headless `sync` and `set-ai-key` commands
internal/model/          plain domain structs (Account, Thread, Message, Label); no GTK
internal/config/         XDG paths, config.toml, and the small state files (prefs, view, signatures,
                         shortcuts, account names, IMAP accounts)
internal/dispatch/       THE main-thread bridge: Main(fn) → glib.IdleAdd
internal/store/          SQLite (modernc.org/sqlite): schema, FTS5, queries, outbox, local drafts, the
                         AI caches (summaries, gists, analyses, categories, translations)
internal/auth/           OAuth2 loopback login, the keyring-backed token source, IMAP credentials
internal/backend/        the provider-agnostic Backend interface the sync engine drives, and BuildMIME
internal/gmailapi/       Gmail REST client: quota budget and pacing, retries, request/byte counters
internal/gmailbackend/   backend.Backend over gmailapi
internal/imapbackend/    backend.Backend over IMAP + SMTP: connection pool, IDLE, UID-set sync cursor
internal/syncer/         the per-account sync engine, `sync` in the dependency rule below: backfill,
                         incremental, body prefetch, outbox, label mirroring; notify.Hub's ID-only events
internal/snooze/         snoozes mirrored to provider labels, the wake sweeper, the reconciler
internal/ai/             AI providers (OpenAI-compatible, Anthropic), the failover chain, the
                         Assistant and its prompts
internal/aiwork/         headless worker that categorizes every account's inbox
internal/activity/       headless pub/sub of "what is the app doing" events for the status bar
internal/ics/            minimal iCalendar reader and iTIP REPLY writer for invite cards
internal/remotecache/    email images downloaded through a privacy-safe client, content-addressed
internal/httpclient/     the User-Agent and resilient dialer of every outbound request;
                         PublicTransport, which refuses private addresses, for URLs from email content
internal/logging/        slog setup and the Trace helpers (see "Tracing")
internal/ui/             all GTK/adw/webkit code: window.go (shell, sidebar, most actions),
                         threadlist.go (middle pane), reader.go (conversation render), compose.go,
                         settings.go (Preferences)
scripts/headless-gui.sh  the private X server, bus and runtime directory every GTK test runs under
packaging/               RPM spec, desktop entry (the `mailto:` handler) and icon
bench/                   the local-model benchmark (docs/BENCHMARK.md)
```

Dependency rule: `store`/`backend`/`gmailapi`/`gmailbackend`/`imapbackend`/`sync`/`auth`/`ai`
MUST NOT import any GTK package (they are headless and unit-testable without a
display). `ui` MUST NOT import `sync`/`gmailapi`/`ai` directly — inject interfaces
and communicate via channels + `dispatch`. The sync engine MUST depend only on
`backend.Backend`, never a concrete provider client, so new providers (IMAP) drop
in without touching the engine.

---

## Key Flows

The short form. The file named in each item has the whole text.

- **Accounts and sync** (docs/accounts.md). `accounts.account_type`
  ('gmail'|'imap') picks each account's backend. The launcher keeps a cancel
  function per account, so adding, removing or reconnecting an account never
  needs a restart. A 60s incremental sync (and IMAP IDLE where the server has
  it) writes SQLite and publishes ID-only events. After a gap (launch, or no
  pass completed for 10 minutes) a pass corrects the inbox before it reads the
  change log, and fetches new inbox mail before anything else. Gmail requests
  wait for quota most urgent first by `backend.PriorityOf(ctx)`, and a context
  without a priority counts as a user action, so background work must mark its
  context with `backend.WithPriority`. An expired history
  watermark triggers a resync; a revoked credential shows the Reconnect
  banner. Provider message and thread ids are not globally unique, so UI
  caches key on `uiCacheKey{accountID,id}`. Snoozes are mirrored to Gmail
  labels, so they hold and wake on every machine; an IMAP account's snoozes
  stay on the device.
- **The reader** (docs/reader.md). Opening a conversation renders what is
  cached at once and fills in the rest. Every email-CSS parse goes through
  `parseStylesheet`, the one hardened door that keeps a malformed email from
  hanging the app. The WebView loads one persistent shell page and swaps
  conversations in by script, never by navigation. JavaScript runs only for
  that shell script, behind a nonce CSP with `default-src 'none'`. Tracking
  pixels are stripped, and no remote image is downloaded before the
  conversation is on screen.
- **The thread list** (docs/threadlist.md). A virtualized `gtk.ListView` of
  thread ids, refreshed by diffing (`diffThreadModel`), so an idle sync does
  no work and the scroll position survives. Every page query runs off the GTK
  thread under `refreshGen`, so the last request wins. Search is local FTS5
  first, then paged provider search.
- **Compose and sending** (docs/compose.md). Compose is plain text, but mail
  goes out as multipart/alternative, with the original's HTML quoted while
  the quote is unedited. AI Draft and Refine show a preview that changes the
  editor only on Apply. Drafts are found by `IsDraft`, never by "newest".
  Send holds a message about 5s behind an Undo toast; a failed send goes to
  the `outbox` table and is retried.
- **AI** (docs/ai.md). Everything the AI says about mail follows one AI
  language setting; whatever it writes as the user stays in the
  correspondent's language. Changing the language clears the cached
  summaries, gists and analyses, and every AI-note writer persists through
  `store.SaveAINote` with the generation it captured, so an older request
  cannot restore a cleared note. Translation sends only text, pooled across
  the conversation, never a tag. A headless worker (`internal/aiwork`)
  categorizes every account's inbox.
- **The window** (docs/window.md). Colour follows GNOME's HIG: one accent,
  used only for state. The status bar shows the current operation and live
  metrics, its activity log keeps one line per operation, and every AI call
  is bracketed into it via `aiActivity`. Single-key shortcuts are
  user-rebindable from one `shortcutDefs` table.

---

## Build & Run

```
make check    # fmt → vet → build → test → lint (gate after every change)
make test-ui  # the GTK tests on a private virtual screen (scripts/headless-gui.sh)
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

Verify GUI changes in a throwaway sandbox rather than launching the installed
app: copy the live DB + config into a temp dir, point `XDG_{DATA,CONFIG,CACHE}_HOME`
there, and run under Xvfb (`GDK_BACKEND=x11 GSK_RENDERER=cairo`) with a distinct
`MAILBOX_APP_ID`. It shares the real session bus (so the keyring still resolves
OAuth tokens + the AI key) but starts a fresh instance, so it never disturbs a
running app. `MAILBOX_DEMO=1` hides the "read-only" banner for clean screenshots.
This sandbox is the only GUI run allowed on the desktop's bus, and it starts no
bus of its own. Run GTK tests with `make test-ui` and every other GUI command
through `scripts/headless-gui.sh`, never a hand-written `dbus-run-session`: a
bus that starts the document portal while sharing `/run/user/$UID` unmounts the
desktop's `doc` filesystem, and Flatpak apps then fail to start.

The test hooks (`MAILBOX_OPEN_FIRST`, `MAILBOX_OPEN_PREFS`, `MAILBOX_WIN_SIZE`,
`MAILBOX_APP_ID`) are listed in [docs/window.md](docs/window.md), and the
`verify` skill (`.claude/skills/verify/SKILL.md`) has the whole sandbox recipe.

---

## Configuration

Every file and setting is in [docs/configuration.md](docs/configuration.md).

- `~/.config/mailbox/config.toml`, `[ai]` table: `provider`
  (`openai`|`litellm`|`anthropic`), `endpoint`, and the failover chain, either
  `models` or `[[ai.chain]]` blocks. The first entry is primary; the rest take
  over when it fails before producing content. Preferences → AI edits it and
  swaps the rebuilt provider into the running app (`SetProvider`).
- Env overrides: `MAILBOX_AI_{PROVIDER,ENDPOINT,MODEL,KEY}` (`MAILBOX_AI_MODEL`
  pins a single model, no fallbacks).
- Secrets (OAuth refresh tokens, AI API keys) live in the OS keyring via
  Secret Service, AI keys under service `mailbox-ai`, one entry per endpoint;
  never in the config file.
- The SQLite DB is `~/.local/share/mailbox/mailbox.db`; the trace log is
  `/tmp/mailbox.log` (truncated each start; enabled with `--trace`).

---

## Design Decisions

The full text of each is in [docs/architecture.md](docs/architecture.md#design-decisions).

- Gmail REST API, not IMAP — native labels/threads, `history.list` incremental sync, server-side search.
- Local SQLite + FTS5 as source of truth; metadata separate from bodies; bodies lazy-loaded.
- Desktop polls `history.list` (no public endpoint for Pub/Sub push); backfill and incremental sync commit in batches, one transaction per batch rather than per message.
- HTML email rendered in a locked-down WebKitGTK view: one persistent shell page, conversations swapped in by script, never by navigation; JavaScript only for the trusted shell script, behind a nonce CSP.
- AI provider is user-configurable behind one `Provider` interface (OpenAI-compatible covers the LiteLLM proxy + OpenAI; Anthropic direct).
- Distribution is RPM (GTK4/WebKit cannot be statically linked into a single binary).

---

## Tracing

Tracing exists so an agent (or a human) can reconstruct exactly what the app did
from `/tmp/mailbox.log` alone — enough to debug an issue and produce a fix
without reproducing it live. Run with `--trace`; the file is truncated each
start. **Every meaningful action and code-path branch must emit a trace log.**

- **One helper.** `logging.Trace(msg, args...)` (or `logging.TraceContext(ctx,
  …)`) — slog has no `Trace` method. `Info`/`Warn`/`Error` stay for
  user-visible/terminal-worthy events; everything finer is `Trace`.
- **Message format** `"pkg: action"` (e.g. `"store: upsert messages"`,
  `"gmailapi: messages.get"`), then structured key/value pairs — never format
  into the message string. Standard keys: `account`, `id`, `label`, `query`,
  `n`/`count`, `bytes`, `dur`, and `err` on failures.
- **What to trace, per code path:** entry of every exported function with its key
  args; the outcome (result count, rows affected, bytes) and `dur` for anything
  doing I/O; **which branch was taken and why** at each non-trivial decision
  (cache hit vs. miss, retry, fallback, early return, skip); every external
  boundary crossing (HTTP request/response status, SQL mutation, IMAP command,
  AI request/response, keyring access, file read/write).
- **Content policy:** this is a single-user local desktop client and the trace
  file is local + per-run-truncated, so trace logs record full content to
  maximise debuggability — full addresses, subjects, headers, query strings, and
  bodies. Cap any unbounded/large value with `logging.Body(s)` (truncates to
  ~2KB with a `…(+N more bytes)` marker). Never trace secrets: OAuth tokens, AI
  API keys, and passwords are logged only as presence/length, never verbatim.
- **Cost:** cheap args (ids, counts, an in-hand string via `logging.Body`) need
  no guard — slog skips disabled levels. Guard only genuinely expensive arg
  construction (hashing, re-serialising) behind `if logging.Enabled() { … }`.
- **Headless rule still holds:** `logging` imports no GTK, so every headless
  package (`store`, `gmailapi`, `imapbackend`, `ai`, …) may and should import it.

---

## Gotchas

- GTK4 is single-threaded: never touch a widget off the main loop — route through `dispatch.Main`.
- Build needs system dev packages: `webkitgtk6.0-devel libsoup3-devel libsecret-devel` (plus `gtk4-devel`, `libadwaita-devel`).
- The Makefile adds `-DENABLE_POINTER_LOCK=1` to `CGO_CPPFLAGS`: WebKitGTK 2.54's umbrella header hides the pointer-lock declarations behind that private macro, although the GTK library still exports the API used by the pinned Go bindings. Direct `go` commands need the same flag.
- The Makefile is Linux-only (`CGO_ENABLED=1`); the standards cross-compile targets are intentionally dropped.
- FTS5 is written explicitly by the store (not via triggers) because searchable text spans two tables.
- Validated GTK binding pins (confirmed to compile + run against system GTK4 4.22.4 / libadwaita 1.9.1 / WebKitGTK 2.52.4 — re-pin these exactly in Phase 2): gotk4 `pkg v0.3.2-0.20250703063411-16654385f59a`, gotk4-adwaita `pkg v0.0.0-20250703085337-e94555b846b6`, gotk4-webkitgtk `pkg v0.0.0-20240108031600-dee1973cf440`. The WebView method is `LoadHtml` (gotk4 lowercases acronyms), not `LoadHTML`.
