# Mailbox

A native, fast email client for Linux/GNOME, with AI built in.
Gmail through its API (fastest), plus IMAP for Yahoo, iCloud, Fastmail, Outlook,
and any other IMAP server. Written in Go with GTK4 + libadwaita.

![Mailbox](screenshots/main.png)

## Why

GNOME lacks a modern, fast, native email client — Geary is buggy, Thunderbird is heavy, and the rest are Electron or web wrappers. Mailbox is a small native GTK4 app that feels at home on GNOME, with AI as a genuine productivity layer. Gmail is first-class via the Gmail API; everyone else connects over IMAP/SMTP.

## Features

**AI** — inbox auto-categorization (Needs reply, Travel, Receipt, …), one-click thread summaries, contextual draft replies with quick-reply suggestions, subject generation, grammar check, in-place translation, and on-demand phishing analysis. Bring your own provider (OpenAI-compatible or Anthropic); the key stays in the OS keyring.

**Security** — per-message SPF/DKIM/DMARC verdicts, display-name/deceptive-link detection, tracking-pixel stripping, and sandboxed rendering (no email-supplied JavaScript runs).

**Accounts** — Gmail via its native API (incremental `history` sync, server threads & search) and any IMAP provider (Yahoo, iCloud, Fastmail, Outlook/Office 365, self-hosted) over IMAP + SMTP, with CONDSTORE incremental sync, IDLE push for near-real-time mail, and client-side conversation threading. App-password or OAuth (Google/Microsoft XOAUTH2) per account. Add accounts from a provider-preset dialog.

**Core** — multiple accounts with per-account unread counts, instant FTS5 search (with a server-side fallback), conversation view, selection-mode bulk actions, Undo Send with a retrying outbox, attachments, resumable drafts, a configurable signature, recipient autocomplete, and keyboard-first navigation (`j`/`k`, `r`, `a`, `c`, `/`). Responsive three-pane layout that collapses on narrow windows.

## Install & setup

Needs Linux with GTK4, libadwaita, WebKitGTK 6.0, and libsecret.

```bash
make build        # → bin/mailbox
```

See **[docs/SETUP.md](docs/SETUP.md)** for build dependencies, connecting accounts (Gmail and IMAP), and enabling the AI features.

## License

[MIT](LICENSE)
