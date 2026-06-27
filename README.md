# Mailbox

A native, fast Gmail client for Linux/GNOME, with AI built in.
Written in Go with GTK4 + libadwaita.

![Mailbox](screenshots/main.png)

## Why

GNOME lacks a modern, fast, native email client — Geary is buggy, Thunderbird is heavy, and the rest are Electron or web wrappers. Mailbox is a small native GTK4 app that feels at home on GNOME, backed by Gmail, with AI as a genuine productivity layer.

## Features

**AI** — inbox auto-categorization (Needs reply, Travel, Receipt, …), one-click thread summaries, contextual draft replies with quick-reply suggestions, subject generation, grammar check, in-place translation, and on-demand phishing analysis. Bring your own provider (OpenAI-compatible or Anthropic); the key stays in the OS keyring.

**Security** — per-message SPF/DKIM/DMARC verdicts, display-name/deceptive-link detection, tracking-pixel stripping, and sandboxed rendering (no email-supplied JavaScript runs).

**Core** — multiple accounts with per-account unread counts, instant FTS5 search (with a Gmail server-side fallback), conversation view, selection-mode bulk actions, Undo Send with a retrying outbox, attachments, resumable drafts, a configurable signature, recipient autocomplete, and keyboard-first navigation (`j`/`k`, `r`, `a`, `c`, `/`). Responsive three-pane layout that collapses on narrow windows.

## Install & setup

Needs Linux with GTK4, libadwaita, WebKitGTK 6.0, and libsecret.

```bash
make build        # → bin/mailbox
```

See **[docs/SETUP.md](docs/SETUP.md)** for build dependencies, connecting a Gmail account (Google OAuth), and enabling the AI features.

## License

[MIT](LICENSE)
