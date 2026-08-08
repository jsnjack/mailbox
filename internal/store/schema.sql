-- Schema for the local Gmail cache. Everything is namespaced by account_id and
-- cascades on account deletion. Message metadata is kept separate from bodies so
-- the 3-pane list view never reads multi-KB HTML. The FTS5 index is contentless
-- and written explicitly by the store (not via triggers), because the searchable
-- text spans messages + message_bodies and bodies arrive later than metadata.

CREATE TABLE IF NOT EXISTS accounts (
  id              INTEGER PRIMARY KEY,
  email           TEXT NOT NULL UNIQUE,
  display_name    TEXT,
  account_type    TEXT NOT NULL DEFAULT 'gmail', -- backend: 'gmail' | 'imap'
  token_expiry    INTEGER,            -- unix seconds of the current access token
  scopes          TEXT,              -- space-joined granted scopes
  sync_cursor     TEXT,              -- opaque incremental-sync cursor (Gmail historyId; IMAP per-folder state)
  backfilled_at   INTEGER,           -- unix seconds; NULL until initial backfill done
  created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);

-- Removing an account crosses SQLite, the OS keyring, and several config
-- files. The account row and this cleanup intent are committed atomically;
-- external artifacts are then removed and retried on later launches until all
-- have succeeded. It deliberately has no account foreign key because it must
-- outlive the account row.
CREATE TABLE IF NOT EXISTS pending_account_cleanups (
  email           TEXT PRIMARY KEY,
  account_type    TEXT NOT NULL,
  created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS labels (
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  gmail_id        TEXT NOT NULL,     -- e.g. "INBOX", "Label_42"
  name            TEXT NOT NULL,
  type            TEXT NOT NULL,     -- "system" | "user"
  color_bg        TEXT,
  unread_total    INTEGER,
  PRIMARY KEY (account_id, gmail_id)
);

CREATE TABLE IF NOT EXISTS threads (
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  gmail_id        TEXT NOT NULL,     -- Gmail threadId
  last_message_at INTEGER,          -- unix seconds of the newest message
  subject         TEXT,
  snippet         TEXT,
  msg_count       INTEGER NOT NULL DEFAULT 0,
  unread_count    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, gmail_id)
);

CREATE TABLE IF NOT EXISTS messages (
  rowid           INTEGER PRIMARY KEY,  -- explicit so messages_fts can reference it
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  gmail_id        TEXT NOT NULL,
  thread_id       TEXT NOT NULL,
  internal_date   INTEGER,          -- unix seconds from Gmail internalDate
  from_name       TEXT,
  from_addr       TEXT,
  reply_to        TEXT,             -- Reply-To header; replies target this over From
  to_addrs        TEXT,
  cc_addrs        TEXT,
  subject         TEXT,
  snippet         TEXT,
  rfc822_msgid    TEXT,             -- Message-ID header (for threading replies)
  in_reply_to     TEXT,
  references_hdr  TEXT,
  is_unread       INTEGER NOT NULL DEFAULT 0,
  is_starred      INTEGER NOT NULL DEFAULT 0,
  has_attachments INTEGER NOT NULL DEFAULT 0,
  size_estimate   INTEGER,
  -- Fetch-version marker (not a bool): 0 = body not fetched, 1 = fetched by a
  -- build before externalized-HTML support, 2 = fetched with it. Read as "fetched"
  -- via != 0 everywhere; the difference only drives the one-time HTML backfill.
  body_fetched    INTEGER NOT NULL DEFAULT 0,
  list_unsubscribe TEXT NOT NULL DEFAULT '',   -- List-Unsubscribe header value ('' = none)
  list_unsub_post  INTEGER NOT NULL DEFAULT 0, -- 1 when List-Unsubscribe-Post offers RFC 8058 one-click
  bcc_addrs        TEXT NOT NULL DEFAULT '',   -- only ever set on own sent/draft copies
  UNIQUE (account_id, gmail_id)
);

CREATE TABLE IF NOT EXISTS message_labels (
  message_rowid   INTEGER NOT NULL REFERENCES messages(rowid) ON DELETE CASCADE,
  account_id      INTEGER NOT NULL,
  label_id        TEXT NOT NULL,
  PRIMARY KEY (message_rowid, label_id)
);

CREATE TABLE IF NOT EXISTS message_bodies (
  message_rowid   INTEGER PRIMARY KEY REFERENCES messages(rowid) ON DELETE CASCADE,
  body_text       TEXT,
  body_html       TEXT,
  raw_headers     TEXT
);

CREATE TABLE IF NOT EXISTS attachments (
  id              INTEGER PRIMARY KEY,
  message_rowid   INTEGER NOT NULL REFERENCES messages(rowid) ON DELETE CASCADE,
  gmail_att_id    TEXT NOT NULL,
  filename        TEXT,
  mime_type       TEXT,
  size_bytes      INTEGER,
  sha256          TEXT,             -- content hash; NULL until downloaded
  disk_path       TEXT,
  content_id      TEXT NOT NULL DEFAULT ''  -- Content-ID for an inline (cid:) image; '' for a normal attachment
);

CREATE TABLE IF NOT EXISTS outbox (
  id              INTEGER PRIMARY KEY,
  local_uuid      TEXT NOT NULL UNIQUE,
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  thread_id       TEXT,
  rfc822          BLOB NOT NULL,
  state           TEXT NOT NULL DEFAULT 'queued',  -- queued|sending|failed|uncertain
  attempts        INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT,
  draft_id        TEXT,                            -- source draft to delete after a successful send
  local_draft_id  TEXT,                            -- local compose snapshot removed after delivery
  not_before      INTEGER NOT NULL DEFAULT 0,      -- unix seconds; a send held for its undo window is invisible to the sweeper until now >= not_before (0 = send ASAP)
  created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);

-- Local-first drafts. payload is a JSON model.OutgoingMessage (including
-- attachment bytes); a small synthetic DRAFT message supplies the normal list
-- UI while this row remains the authoritative editable copy. revision prevents
-- an older in-flight provider save from marking newer edits as synchronized.
CREATE TABLE IF NOT EXISTS local_drafts (
  local_id             TEXT PRIMARY KEY,
  account_id           INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  source_message_id    TEXT NOT NULL DEFAULT '',
  provider_draft_id    TEXT NOT NULL DEFAULT '',
  provider_message_id  TEXT NOT NULL DEFAULT '',
  payload              BLOB NOT NULL,
  state                TEXT NOT NULL DEFAULT 'queued', -- queued|synced|failed|deleting
  revision             INTEGER NOT NULL DEFAULT 1,
  attempts             INTEGER NOT NULL DEFAULT 0,
  last_error           TEXT NOT NULL DEFAULT '',
  updated_at           INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_local_drafts_account_state
  ON local_drafts(account_id, state, updated_at);

-- Ordered, durable provider mirrors for optimistic local label changes. The UI
-- commits the local message state and this row in one transaction, then the
-- sync engine drains rows oldest-first. A network failure therefore survives a
-- restart instead of silently losing an offline archive/star/read operation.
CREATE TABLE IF NOT EXISTS pending_label_ops (
  id              INTEGER PRIMARY KEY,
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  message_ids     TEXT NOT NULL,       -- JSON []string
  add_labels      TEXT NOT NULL,       -- JSON []string
  remove_labels   TEXT NOT NULL,       -- JSON []string
  attempts        INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_pending_label_ops_account
  ON pending_label_ops(account_id, id);

-- AI-assigned inbox category per message, keyed by the message's Gmail id. This
-- is UI-derived (not Gmail) data, persisted so categorization runs once per
-- email instead of every launch. category is one of the known buckets, or '' to
-- record "classified, no tag" (so it isn't re-classified). Keyed by gmail_id, so
-- a new message in a thread naturally has no row yet and gets classified once.
-- status distinguishes a settled classification ('ok', category may still be
-- '' for "no tag") from an AI attempt that errored ('failed', category always
-- ''): a failed row is excluded from "already done" (MessageCategories), so the
-- thread stays a retry candidate instead of looking identical to a genuine
-- no-match.
CREATE TABLE IF NOT EXISTS message_categories (
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  gmail_id        TEXT NOT NULL,
  category        TEXT NOT NULL,     -- a known category, or '' for "no tag"
  manual          INTEGER NOT NULL DEFAULT 0, -- 1 = user picked it (overrides "Replied")
  status          TEXT NOT NULL DEFAULT 'ok', -- 'ok' or 'failed' (AI attempt errored)
  PRIMARY KEY (account_id, gmail_id)
);

-- AI translation per message, keyed by the message's Gmail id (+ target lang).
-- A message body is immutable, so a translation never goes stale; persisted so
-- it isn't re-requested from the AI on every open/revert. text is the
-- translated, markup-preserving body HTML.
CREATE TABLE IF NOT EXISTS message_translations (
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  gmail_id        TEXT NOT NULL,
  lang            TEXT NOT NULL,     -- target language, e.g. 'English'
  text            TEXT NOT NULL,
  PRIMARY KEY (account_id, gmail_id, lang)
);

-- AI thread summary, keyed by thread id with the fingerprint it was computed for
-- (the thread's message-id set). A fingerprint mismatch means the thread gained
-- a message, so the summary is stale and regenerated. Persisted so an unchanged
-- thread's summary survives restarts instead of being re-summarized.
CREATE TABLE IF NOT EXISTS thread_summaries (
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  thread_id       TEXT NOT NULL,
  fingerprint     TEXT NOT NULL,
  summary         TEXT NOT NULL,
  PRIMARY KEY (account_id, thread_id)
);

-- Records conversations whose complete server-side membership has been fetched.
-- A capped initial backfill can contain a reply without the older message it
-- answers; the reader hydrates that thread once, then this marker avoids a
-- provider request on every subsequent open.
CREATE TABLE IF NOT EXISTS thread_hydrations (
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  thread_id       TEXT NOT NULL,
  PRIMARY KEY (account_id, thread_id)
);

-- AI security analysis (phishing/scam verdict + reasons) per message, keyed by
-- the message's Gmail id. The message and its auth/heuristic signals are
-- immutable, so the analysis never goes stale; persisted so re-opening a message
-- doesn't re-run the AI.
CREATE TABLE IF NOT EXISTS message_analyses (
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  gmail_id        TEXT NOT NULL,
  analysis        TEXT NOT NULL,
  PRIMARY KEY (account_id, gmail_id)
);

-- AI one-line gist per message — the same brief summary the desktop
-- notification shows, reused as a summary card in the reader. Keyed by the
-- message's Gmail id; a message's content is immutable, so a stored gist never
-- goes stale. Derived from the snippet (not the body), so it survives body
-- retention pruning.
CREATE TABLE IF NOT EXISTS message_gists (
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  gmail_id        TEXT NOT NULL,
  gist            TEXT NOT NULL,
  PRIMARY KEY (account_id, gmail_id)
);

-- Snoozed conversations: hidden from the inbox until `until`, then woken by a
-- background sweeper. Each snooze is mirrored to the provider as label state
-- (−INBOX, +Snoozed, +Snoozed/<wake time>) so it holds on other clients and
-- machines (internal/snooze); mirrored records whether that push has been
-- handed to the provider — the reconciler pushes a never-mirrored row out even
-- though its thread still has INBOX, while INBOX on a mirrored row means
-- "unsnoozed elsewhere". The row is not deleted on wake (only on an explicit
-- Unsnooze or a fresh re-snooze) so the list can show a "Snoozed" tag on a
-- thread that recently returned; notified distinguishes an announced wake from
-- a still-pending one.
CREATE TABLE IF NOT EXISTS snoozes (
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  thread_id       TEXT NOT NULL,
  until           INTEGER NOT NULL,  -- unix seconds
  notified        INTEGER NOT NULL DEFAULT 0,
  mirrored        INTEGER NOT NULL DEFAULT 0,
  latest_message_rowid INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, thread_id)
);

-- FTS5 index over message text, keyed by messages.rowid. Rows are written
-- explicitly by the store (not via triggers) because the searchable text spans
-- messages + message_bodies and bodies arrive later than metadata. Updates are
-- DELETE-by-rowid then INSERT.
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
  subject, from_name, from_addr, snippet, body_text,
  tokenize = 'unicode61 remove_diacritics 2'
);

CREATE INDEX IF NOT EXISTS idx_msg_label   ON message_labels(account_id, label_id, message_rowid);
CREATE INDEX IF NOT EXISTS idx_msg_date    ON messages(account_id, internal_date DESC);
CREATE INDEX IF NOT EXISTS idx_msg_thread  ON messages(account_id, thread_id, internal_date);
CREATE INDEX IF NOT EXISTS idx_thread_date ON threads(account_id, last_message_at DESC);
CREATE INDEX IF NOT EXISTS idx_msg_unread  ON messages(account_id, is_unread) WHERE is_unread = 1;
CREATE INDEX IF NOT EXISTS idx_msg_starred ON messages(account_id, is_starred) WHERE is_starred = 1;
CREATE INDEX IF NOT EXISTS idx_outbox_state ON outbox(state) WHERE state IN ('queued', 'failed');
