# Mailbox

A fast, native mail client for GNOME, with useful AI.

![Mailbox](screenshots/main.png)

Mailbox is a small GTK4 app for people who live on Linux and want their mail
quick, local, and private. Gmail connects through its own API; everything else
speaks IMAP. An AI layer takes over the busywork — sorting, summarizing,
drafting, translating — using whatever model you point it at.

## How it's different

- **Native and light.** Go + GTK4/libadwaita, at home on GNOME. Not a web page
  in a frame.
- **Local first.** Your mail lives in a local database: search answers as you
  type, reading works offline, and the interface never waits for a server.
- **AI on your terms.** Any OpenAI-compatible or Anthropic endpoint works.
  Every result is cached, so nothing is analyzed or paid for twice. Without a
  configured provider the app is simply a complete mail client; nothing nags.
- **Private by default.** Tracking pixels are stripped before rendering, email
  can never run scripts, forged senders are flagged, and every secret stays in
  the system keyring.

## Features

**Triage**

- Inbox auto-sorted into action tags: Needs reply, Receipt, Travel, Newsletter, …
- Snooze — quick presets, a calendar, or let the AI read the mail and suggest
  the moment ("day before the deadline")
- One-click unsubscribe, plus a dashboard of every list you're on, sorted by
  how much they send
- Keyboard-driven throughout; shortcuts are rebindable

**Reading & writing**

- Conversation view that folds older messages out of the way
- Meeting invites become a card: Accept / Maybe / Decline without leaving mail
- Thread summaries, drafted replies, translation, and proofreading on demand
- Undo send, backed by an outbox that survives crashes and retries failures
- New-mail notifications carry a one-line AI gist and quick actions

**Accounts**

- Gmail through its native API: real labels, server search, instant incremental sync
- Yahoo, iCloud, Fastmail, Outlook, and any IMAP server — with live push where
  the server supports it
- Any number of accounts side by side, each with its own identity and signature

## Get started

```bash
sudo dnf install golang gtk4-devel libadwaita-devel webkitgtk6.0-devel libsoup3-devel libsecret-devel
make build
./bin/mailbox
```

The app opens to a welcome screen and **Add account…** takes it from there.
IMAP providers need only an app password; Gmail's API and Outlook need a
one-time client setup. **[docs/SETUP.md](docs/SETUP.md)** walks through every
option, including turning on the AI.

## License

[MIT](LICENSE)
