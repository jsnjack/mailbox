# Configuration

Every file and setting mailbox reads or writes, where it lives, and what edits
it. [AGENTS.md](../AGENTS.md) has the short version; read this before adding a
setting or changing where state is stored.

Other docs, all in `docs/`: [architecture.md](architecture.md) (every file),
[accounts.md](accounts.md) (accounts, sync, snooze), [reader.md](reader.md)
(the reader), [threadlist.md](threadlist.md) (list and search),
[compose.md](compose.md) (compose, drafts, outbox), [ai.md](ai.md) (AI
features), [window.md](window.md) (colour, status bar, shortcuts) and
[configuration.md](configuration.md); [SETUP.md](SETUP.md) and
[BENCHMARK.md](BENCHMARK.md) are for people setting up the app or choosing a
model. [AGENTS.md](../AGENTS.md) says which to read before which change.

- Config file: `~/.config/mailbox/config.toml`, `[ai]` table: `provider` (`openai`|`litellm`|`anthropic`), `endpoint` (base URL incl. `/v1`), and the failover chain — either `models` (priority list on the shared endpoint) or `[[ai.chain]]` blocks (each `model` optionally with its own `provider`/`endpoint`, inheriting the top level when blank — the fullest form, for a VPN-only proxy chained to a local model). The first entry is primary; the rest take over when it fails before producing content. Legacy single `model` still reads; on save the primary is mirrored into the top-level fields for downgrade compatibility and an all-same-endpoint chain collapses back to `models`. Editable in-app via Preferences → AI (an expander per model with add/remove/reorder and a per-row Test button that live-checks just that entry — a chain-wide test couldn't say which model answered), which applies changes to the running app by swapping the rebuilt provider into the live Assistant (`SetProvider`); only enabling AI for the very first time needs a restart (the AI widgets aren't built). Env overrides: `MAILBOX_AI_{PROVIDER,ENDPOINT,MODEL,KEY}` (`MAILBOX_AI_MODEL` pins a single model, no fallbacks).
- AI API key: keyring service `mailbox-ai`, one entry per endpoint (user = endpoint; a legacy user = provider entry — what `mailbox set-ai-key` writes — is read as fallback) or `MAILBOX_AI_KEY`; never in the config file. Editable per chain entry in Preferences → AI (each key row mirrors the keyring: typed → stored, cleared → removed).
- Persistent state (SQLite DB): `~/.local/share/mailbox/mailbox.db`. Preferences → Storage can clear the attachment cache (`config.ClearAttachmentsCache`) and compact the DB (`store.Vacuum` — `VACUUM` + WAL-truncate, reclaiming pages freed by deleted mail; WAL keeps that space otherwise).
- Account display names: `~/.local/share/mailbox/accounts.json` (email → name).
- Default signature: `~/.config/mailbox/signature.txt` (plain text, may be empty); per-account overrides in `~/.config/mailbox/signatures.json` (email → signature).
- Keyboard shortcut overrides: `~/.config/mailbox/shortcuts.json` (action id → keys).
- View state (last folder, unread filter, reader zoom): `~/.local/share/mailbox/view.json`.
- General prefs (automatic remote-image loading, per-feature AI toggles, AI language, body retention window): `~/.config/mailbox/prefs.json`. Body retention (Preferences → Storage, default off) prunes cached bodies older than N days (`store.PruneBodies` — metadata/header search kept, body re-fetched on open, body-derived AI caches + attachment rows pruned too); applied on change and by a daily background pass (`backgroundRetention`), with an auto-`Vacuum` after a large prune.
- Attachment cache: `~/.cache/mailbox/attachments/` (content-addressed by sha256).
- Secrets (OAuth refresh tokens, AI API keys): OS keyring via Secret Service.
- Trace log: `/tmp/mailbox.log` (truncated each start; enabled with `--trace`).
