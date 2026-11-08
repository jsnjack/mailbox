# Accounts, sync and the desktop

Several accounts on two kinds of backend (Gmail REST and IMAP): adding,
removing and reconnecting them without a restart, the account switcher and its
unread counts, background sync and new-mail notifications, snooze and how it
is mirrored to the provider so that it holds on every machine, and the desktop
entry that makes mailbox the `mailto:` handler. Read it before changing
`cmd/mailbox/launch.go`, `internal/syncer`, `internal/snooze`,
`internal/auth`, a backend, or the account parts of `internal/ui`.

Other docs, all in `docs/`: [architecture.md](architecture.md) (every file),
[accounts.md](accounts.md) (accounts, sync, snooze), [reader.md](reader.md)
(the reader), [threadlist.md](threadlist.md) (list and search),
[compose.md](compose.md) (compose, drafts, outbox), [ai.md](ai.md) (AI
features), [window.md](window.md) (colour, status bar, shortcuts) and
[configuration.md](configuration.md); [SETUP.md](SETUP.md) and
[BENCHMARK.md](BENCHMARK.md) are for people setting up the app or choosing a
model. [AGENTS.md](../AGENTS.md) says which to read before which change.

## Accounts and providers

Multi-account, multi-provider: `accounts.account_type` ('gmail'|'imap') selects
the backend; the launcher's `buildBackendForAccount` builds the Gmail REST
backend (first-class — `history.list`, server threads/search) or an IMAP backend
(`imapbackend` over emersion/go-imap, with a password or XOAUTH2 credential) per
account, and the engine drives whichever via `backend.Backend`. The UI tracks an
active account (a switcher appears in the sidebar when more than one is
connected) and routes every operation by account id. The app launches even with
**zero accounts** — it opens to a welcome empty state whose **Add account…**
button connects the first one. Accounts are added via the
primary menu's **Add account…** dialog (`openAddAccount`: provider presets →
email + app password or OAuth sign-in → Test & Add; persists via
`config.{SaveIMAPAccount}` + `auth.SaveIMAPSecret` + `store.UpsertAccount`) and,
for Gmail REST, via `mailbox sync --account`. Add/remove/reconnect are all live
— no restart: the launcher keeps a per-account `context.CancelFunc`, and
`startAccount` (used at launch, add, and reconnect) stops any existing runtime
for that id before starting, so a backend can be torn down or swapped cleanly. A
freshly added account has no sync cursor, so `backgroundSync` does an initial
labels+backfill (via `engine.Resync`) immediately before switching to
incremental. An account is **removed** from Preferences → Accounts
(`deps.RemoveAccount` → `stopAccount` + `store.DeleteAccount`, which clears the
FTS rows then drops the account row so its messages/labels/threads cascade, +
keyring secret + per-account config cleanup); the switcher collapses live and the
active account falls back to a survivor or the welcome state. A revoked/expired
credential (Gmail OAuth `invalid_grant`, or a rejected IMAP password classified
as `backend.ErrAuth`) reveals the auth banner whose **Reconnect** button reopens
the dialog prefilled for that account (`reconnectAccount`) — re-adding the same
email keeps its cursor (cache preserved) and restarts its sync. Each switcher row shows the
account's display name (email as a caption when named) and an unread-INBOX
count pill;
names are user-assigned in Preferences → Accounts (`config.{Load,Save}AccountName`,
stored in `<data>/accounts.json` keyed by email — when set, the name is primary
and the email becomes a caption). Per-account pills and the window title come
from one batched `store.UnreadCountByLabelForAccounts` query (`refreshAccountUnread`
/ `applyAccountUnread`), refreshed on any sidebar reload and whenever a non-active
account syncs. `loadLabels` only rebuilds the sidebar widgets when its structure or
the inbox badge actually changed (`sidebarSignature`), so an idle 60s sync touches
nothing. UI memory caches use `uiCacheKey{accountID,id}` for rendered sections,
categories, gists, analyses, and summaries; translations wrap that key with their
target language. Provider message and thread ids are never treated as globally unique.

## Snooze

**Snooze**: a conversation can be hidden until a wake time. The local
`snoozes` table drives this machine's UI (instant hide, the Snoozed virtual
folder, the inbox-query exclusion), and every snooze is **mirrored to the
provider as label state** (`internal/snooze`): −INBOX (other clients — the
Gmail phone app included — stop showing it), +`Snoozed` (stable membership,
browsable elsewhere), and a hidden `Snoozed/<local time + offset>` child
(`labelListVisibility=labelHide`) carrying the exact wake moment — so a snooze
made on one machine holds and **wakes everywhere**. `Manager.Reconcile` (after
every sync pass) converges rows ↔ labels both ways: adopts snoozes made
elsewhere (exact time parsed from the stamp), cancels a snooze whose thread is
back in INBOX (woken/unsnoozed on another machine, dragged to inbox on the
phone, or re-inboxed by a new reply — matching Gmail's own wake-on-reply),
retimes on a remote re-snooze, and pushes rows the provider doesn't know
(pre-mirror snoozes migrate themselves). The row menu's Snooze flyout offers
presets, AI-suggested moments read from the email itself
(`Assistant.SuggestSnooze` — up to three "YYYY-MM-DD HH:MM|reason" lines: an
hour before a meeting, the day before a deadline), and a "Pick date…" calendar
dialog (`openSnoozeDialog`); a Snoozed virtual folder (`snoozedID`, neutral
grey count pill) lists pending rows; the per-minute sweeper
(`backgroundSnoozeWake` → `Manager.WakeDue`) marks a due row notified,
restores INBOX and strips the snooze labels through the engine's ordered
mirror queue, deletes an emptied wake-time label (`DeleteLabelIfUnused`, via
`Engine.MirrorOp` so it can't overtake the modify), and publishes `SnoozeWoke`
(list refresh + "Reminder" notification). Mirror labels are hidden from the
sidebar and label pickers (`model.IsSnoozeLabel`); an account whose backend
lacks `backend.LabelManager` (IMAP) keeps explicitly device-only snoozes and cancels one when a message newer than its snooze snapshot enters the thread.

## Background sync, notifications and the desktop entry

A 60s background incremental sync (`syncPass`, shared with the manual refresh; each pass under a `syncPassTimeout` backstop; consecutive failures back off exponentially to `syncBackoffCap` instead of re-running a heavy resync every tick) updates label counts through `dispatch`→`Hub`,
and new inbox mail (arriving after launch) raises an immediate desktop notification via
`gio.Notification`. Notifications are account-qualified and deduplicated for the session, require a real SENT label before suppressing own-address mail, and ignore body-hydration events. A persisted AI gist is used when already available but AI never delays delivery. The background sync self-heals an expired history watermark
(an account offline past Gmail's history window) by re-backfilling and resetting
the watermark (`engine.Resync` on `ErrHistoryExpired`); a revoked/expired refresh
token (`auth.IsAuthError`) instead publishes `AuthExpired`, which reveals a
"reconnect" banner (it can't recover without re-login). (GNOME routes a notification only when it can resolve the
GApplication app-id `com.jsnjack.mailbox` to an installed `*.desktop` entry; the
RPM ships `com.jsnjack.mailbox.desktop` under `/usr/share/applications`.
Development binaries do not install a user-level entry because it would shadow
the packaged application.)

The desktop entry registers `MimeType=x-scheme-handler/mailto;` with
`Exec=/usr/bin/mailbox %u`, so it always launches the RPM-owned binary rather
than another `mailbox` earlier on `PATH`. The app appears under GNOME's Default
Apps → Mail and can be set as the default mail client. The GApplication uses
`ApplicationHandlesOpen`: a clicked
`mailto:` URI is delivered to the `open` handler (and routed to an
already-running instance), which opens a prefilled compose (`parseMailto` →
`composeFromMailto`, handling both the plain `mailto:` and GIO's normalised
`mailto:///` forms). `main` strips the `mailto:` arg before cobra parses (the
root command has subcommands) — `SetArgs` with a non-nil empty slice, since
`SetArgs(nil)` falls back to `os.Args`. The RPM's `%post` runs
`update-desktop-database` so the registration takes effect on install.
