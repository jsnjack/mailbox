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
  auth/              OAuth2 installed-app loopback + keyring-backed token source
  gmailapi/          wrapper over google.golang.org/api/gmail/v1 (semaphore, budget, backoff, MIME)
  sync/              per-account sync workers (backfill ↔ incremental) + notify.Hub
  ai/                provider abstraction (OpenAI-compatible + Anthropic), streaming
  ui/                all GTK/adw/webkit widget code (3-pane shell, list, reader, actions)
```

UI state (implemented): 3-pane shell renders the cached account live; clicking a
message lazily fetches + sanitizes + renders its body (WebKit, JS off, remote
images off behind a toggle); reader actions archive / mark-unread / star via
optimistic `ModifyLabels` + Gmail mirror; opening an unread message marks it read;
a 60s background incremental sync updates label counts through `dispatch`→`Hub`,
and new inbox mail (arriving after launch) raises a desktop notification via
`gio.Notification`. (The GApplication app-id `com.surfly.mailbox` matches the
installed `com.surfly.mailbox.desktop`, which GNOME requires for notifications.)
Reply / reply-all / forward / new compose in a separate window (text/plain via
`gmailapi.BuildMIME` + `messages.send`, threading headers + threadId on replies);
the compose window has an AI-draft button that streams a reply into the body, and
a Save-draft button (`users.drafts.create`).
Translate / draft-reply stream into a window via the `ai` provider. Incoming
attachments are extracted on body fetch (`ReplaceAttachments`) and shown as chips
in the reader; clicking one downloads it (content-addressed under the cache dir)
and opens it with `xdg-open`.

The list is grouped by conversation: a virtualized `gtk.ListView` over a
`gtk.StringList` of thread ids (looked up in a `threadByID` map of
`model.ThreadSummary`); rows show the newest message + a count. `store`
provides `ListThreadsByLabel`/`ListThreadMessages`/`GetThreadSummary`. Opening a
thread renders all its messages stacked in the reader (bodies fetched lazily,
each a sanitized section); archive/trash apply to the whole thread, reply/star
to its newest message. A search entry runs instant local FTS5 search
(`store.Search`, sanitized into a quoted prefix MATCH) whose hits are grouped
into threads; clearing it returns to the current label.
Compose supports attachments (a file picker adds them; `BuildMIME` emits
multipart/mixed with base64 parts). Send is synchronous for compose feedback,
but a failed send is queued to the `outbox` table and retried by a background
sweeper (`SweepOutbox`, ~45s). The window collapses responsively via
`adw.Breakpoint` (3 panes → list+reader below ~860sp → single pane below ~520sp),
with `SetShowContent` driving navigation when collapsed. Single-key shortcuts
(bubble-phase key controller, so text entries keep their input): j/k navigate
threads, r reply, a archive, c compose, / focus search. Test hooks:
`MAILBOX_OPEN_FIRST=1` opens the newest message on launch; `MAILBOX_WIN_SIZE=WxH`
overrides the initial window size.

Dependency rule: `store`/`gmailapi`/`sync`/`auth`/`ai` MUST NOT import any GTK
package (they are headless and unit-testable without a display). `ui` MUST NOT
import `sync`/`gmailapi`/`ai` directly — inject interfaces and communicate via
channels + `dispatch`.

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
- Persistent state (SQLite DB): `~/.local/share/mailbox/mailbox.db`.
- Attachment cache: `~/.cache/mailbox/attachments/` (content-addressed by sha256).
- Secrets (OAuth refresh tokens, AI API keys): OS keyring via Secret Service.
- Trace log: `/tmp/mailbox.log` (truncated each start; enabled with `--trace`).

---

## Design Decisions

- Gmail REST API, not IMAP — native labels/threads, `history.list` incremental sync, server-side search.
- Local SQLite + FTS5 as source of truth; metadata separate from bodies; bodies lazy-loaded.
- Desktop polls `history.list` (no public endpoint for Pub/Sub push).
- HTML email rendered in a locked-down WebKitGTK view (JS off, remote images blocked, links open externally).
- AI provider is user-configurable behind one `Provider` interface (OpenAI-compatible covers the LiteLLM proxy + OpenAI; Anthropic direct).
- Distribution is RPM (GTK4/WebKit cannot be statically linked into a single binary).

---

## Gotchas

- GTK4 is single-threaded: never touch a widget off the main loop — route through `dispatch.Main`.
- Build needs system dev packages: `webkitgtk6.0-devel libsoup3-devel libsecret-devel` (plus `gtk4-devel`, `libadwaita-devel`).
- The Makefile is Linux-only (`CGO_ENABLED=1`); the standards cross-compile targets are intentionally dropped.
- FTS5 is written explicitly by the store (not via triggers) because searchable text spans two tables.
- Validated GTK binding pins (confirmed to compile + run against system GTK4 4.22.4 / libadwaita 1.9.1 / WebKitGTK 2.52.4 — re-pin these exactly in Phase 2): gotk4 `pkg v0.3.2-0.20250703063411-16654385f59a`, gotk4-adwaita `pkg v0.0.0-20250703085337-e94555b846b6`, gotk4-webkitgtk `pkg v0.0.0-20240108031600-dee1973cf440`. The WebView method is `LoadHtml` (gotk4 lowercases acronyms), not `LoadHTML`.
