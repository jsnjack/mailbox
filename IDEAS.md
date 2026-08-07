# IDEAS

Focused work needed to move Mailbox toward 9/10. Move an item into implementation
and remove it here when it ships.

## Incremental pagination

Replace the fixed `threadListCap` (5,000) and `serverSearchCap` (500) with lazy,
incremental page loading:

- Page label, All Mail, and search results as the user scrolls.
- Preserve selection, scroll position, date grouping, and incremental row updates.
- Continue provider pagination without caching thousands of messages up front.
- Show loading, completion, and retry states; never present a capped result as complete.
- Keep SQLite as the source of truth and bound the in-memory summary/page cache.

## Encrypted backup and diagnostics

Add safe recovery and support tooling:

- Export and restore the database, configuration, signatures, and optional caches.
- Encrypt backups with a user-supplied passphrase; never silently export keyring secrets.
- Verify backup integrity and schema compatibility before replacing local data.
- Generate a redacted diagnostics bundle containing versions, provider types, sync state,
  schema/DB health, metrics, and recent logs.
- Strip message content, addresses, tokens, filesystem paths, and AI keys by default, and
  show a preview of every file before the bundle is saved.
