# IDEAS

Discoveries and deferred work we want to keep for later — not committed to yet.
Move an item into a commit (and delete it here) when it's done.

## Performance

- **Fully-paged thread list.** The list is a `gtk.ListView` over a `gtk.StringList`
  of thread ids, capped at `threadListCap` (5000), refreshed incrementally
  (`diffThreadModel`). The original plan called for a windowed `gio.ListModel`
  (`GetNItems` = COUNT(*), `GetItem` pages SQL with an LRU). We deferred it: at the
  5000 cap the incremental diff already removes the per-sync churn and the memory
  (~1.5 MB of summaries) is fine, so a custom GListModel GObject subclass isn't worth
  the gotk4 complexity / regression risk. **Revisit if** users routinely have a
  single label / All-Mail far beyond 5000 threads and want them all scrollable, or
  the summary map's memory becomes a problem. Build it behind the existing
  `showThreads`/`diffThreadModel` surface.

- **Prepared-statement reuse.** The store re-parses each SQL string per call. Caching
  prepared statements for the hot queries would shave parse overhead. Low impact per
  the audit; do only if profiling shows it.

## Features (noticed while working, not requested yet)

_(none currently — recently shipped: per-account signatures, Reply-To handling.)_

## Feature ideas — modern email clients, 2026 scan

What the current crop (Superhuman, Hey, Notion Mail, Shortwave, Missive, Proton)
ships that we don't, filtered to what fits a native GNOME client with a local
SQLite source of truth and a pluggable AI provider. Roughly ordered by
value-for-effort.

### Table stakes we're missing

- **Snooze / remind-me.** Hide a thread until a chosen time, then resurface it in
  the inbox (optionally with a notification). Gmail has no snooze API, so model it
  locally: a `snoozed_until` column, the list query excludes snoozed threads, a
  sweeper (like `SweepOutbox`) re-inboxes them. A "Snoozed" virtual folder falls
  out of the same query.
- **Scheduled send ("send later").** The outbox-first queue already holds a
  message before it goes out; add a `not_before` timestamp and a picker in the
  compose send-button dropdown. The `SweepOutbox` sweeper is the scheduler.
- **Unified inbox.** A virtual "All inboxes" entry above the account switcher —
  one query across accounts (store is already multi-account), rows badged with
  the account color. Compose from it picks the From account by context.
- **Follow-up nudges ("waiting on reply").** Track sent messages with a question
  or ask; if no reply lands on the thread in N days, resurface it with a
  "still waiting" pill. Detection is a cheap AI classification at send time
  (reuse the categorizer plumbing + a `followups` table); the check is a local
  query, no network.
- **One-click unsubscribe + subscription dashboard.** Honor `List-Unsubscribe`
  (RFC 8058 one-click POST, else mailto/URL) as a button on newsletter rows —
  we already classify Newsletter. A Preferences-style dialog groups senders by
  volume ("47 emails last month") with unsubscribe / auto-archive per sender.
- **Thread mute.** A `muted` flag per thread: incoming mail on it skips the inbox
  and the desktop notification (Gmail even has a native MUTE label to mirror).

### AI, beyond what we have

- **"Catch me up" digest.** One card summarizing what arrived since you last
  looked (or overnight): what needs a reply, what's just FYI, what was
  auto-filed. We already have per-thread summaries and categories persisted —
  this is a second-level summary over those rows, so it's cheap and mostly
  offline.
- **Natural-language search.** Free-text box ("pdf from Anna about the offsite,
  last spring") that the AI compiles into our now-existing local search
  operators / FTS query (and the Gmail `q=` syntax for server search). One
  prompt, no new index; show the compiled query so it's auditable.
- **Ask-your-mailbox (local RAG).** Embeddings over cached bodies in a
  `sqlite-vec` table, filled during backfill; a chat panel answers "what was the
  wifi password from the hotel?" with links to the source messages. The only
  new infra is an embedding endpoint on the existing provider abstraction.
- **Natural-language rules.** "Archive GitHub notifications unless I'm
  mentioned" → AI compiles once into a deterministic local rule (JSON:
  conditions on sender/subject/category → actions we already have: label,
  archive, mute). Rules run in the sync path with zero per-message AI cost, and
  are inspectable/editable in Preferences.
- **Structured extraction on top of categories.** We already tag Travel /
  Receipt / Calendar; extract the payload once at categorization time (flight +
  PNR, order + tracking number, event + time) into a details card with actions
  (add to calendar, track package, total spent per month).

### Calendar & contacts gravity

- **ICS invite handling.** Detect `text/calendar` parts, render an event card
  with Accept / Tentative / Decline that sends the iTIP reply — invites are
  email; no calendar backend needed for MVP. (Full agenda sidebar via CalDAV /
  Google Calendar API is the phase 2.)
- **Sender context pane.** Click a sender → recent threads with them, attachment
  history, first-seen date, mail volume — all answerable from SQLite today; no
  network, no CRM.
- **Attachment browser.** A virtual "Attachments" view (name/type/sender/date,
  thumbnails for images) over an index we already have at sync time. Finding
  "that PDF" is a top-3 email task and pure local win.

### Differentiators worth considering

- **Screener for first-time senders (Hey-style).** First mail from an unknown
  address lands in a Screener view: Approve (inbox, remembered) / Screen out
  (auto-archive forever). `store.Contacts` already knows who's known. Opt-in.
- **Split inbox tabs.** Since categories are already persisted per message,
  offer category tabs over the inbox (Primary / Newsletters / Notifications /
  Receipts) instead of only row pills — the triage payoff of categorization.
- **Email aliases for signups.** Generate/manage plus-addresses (`user+shop@`)
  when composing to a new service; pairs with the subscription dashboard for
  one-click kill of a leaked alias. (Gmail plus-addressing needs no server
  support at all.)
- **E2E encryption (OpenPGP/Autocrypt).** Table stakes for a subset of the
  Linux audience specifically. Big lift (key mgmt UX, sequoia/gopenpgp,
  encrypted-body FTS questions) — decide deliberately, not by default.
- **JMAP backend.** A third `backend.Backend` implementation (Fastmail, Stalwart
  self-hosters). The engine is already provider-agnostic; JMAP's sync model
  (`Email/changes`) maps to our cursor better than IMAP does.
- **Notification quick actions.** Archive / Mark read / Reply buttons on the
  GNOME notification via `gio.Notification` action support — we already raise
  the notification; wiring actions to existing handlers is small.
