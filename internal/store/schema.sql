-- Schema for the local Gmail cache. Everything is namespaced by account_id and
-- cascades on account deletion. Message metadata is kept separate from bodies so
-- the 3-pane list view never reads multi-KB HTML. The FTS5 index is contentless
-- and written explicitly by the store (not via triggers), because the searchable
-- text spans messages + message_bodies and bodies arrive later than metadata.

CREATE TABLE IF NOT EXISTS accounts (
  id              INTEGER PRIMARY KEY,
  email           TEXT NOT NULL UNIQUE,
  display_name    TEXT,
  token_expiry    INTEGER,            -- unix seconds of the current access token
  scopes          TEXT,              -- space-joined granted scopes
  last_history_id TEXT,              -- Gmail historyId watermark for incremental sync
  backfilled_at   INTEGER,           -- unix seconds; NULL until initial backfill done
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
  body_fetched    INTEGER NOT NULL DEFAULT 0,
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
  disk_path       TEXT
);

CREATE TABLE IF NOT EXISTS outbox (
  id              INTEGER PRIMARY KEY,
  local_uuid      TEXT NOT NULL UNIQUE,
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  thread_id       TEXT,
  rfc822          BLOB NOT NULL,
  state           TEXT NOT NULL DEFAULT 'queued',  -- queued|sending|sent|failed
  attempts        INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT,
  created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);

-- AI-assigned inbox category per message, keyed by the message's Gmail id. This
-- is UI-derived (not Gmail) data, persisted so categorization runs once per
-- email instead of every launch. category is one of the known buckets, or '' to
-- record "classified, no tag" (so it isn't re-classified). Keyed by gmail_id, so
-- a new message in a thread naturally has no row yet and gets classified once.
CREATE TABLE IF NOT EXISTS message_categories (
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  gmail_id        TEXT NOT NULL,
  category        TEXT NOT NULL,     -- a known category, or '' for "no tag"
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
