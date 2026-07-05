# Mailbox

A fast, native mail client for GNOME.

![Mailbox](screenshots/main.png)

Mailbox is a GTK4 email client written in Go. Mail is stored in a local
database, so search is instant and reading works offline. Gmail connects
through its own API; Yahoo, iCloud, Fastmail, Outlook, and any other provider
connect over IMAP. If you give it an AI model — any OpenAI-compatible or
Anthropic endpoint — it will sort your inbox, summarize threads, draft
replies, and translate mail. The AI is optional; everything else works
without it.

It is also careful with your mail: tracking pixels are stripped before a
message is rendered, email can never run scripts, senders that fail
authentication are flagged, and passwords and tokens are kept in the system
keyring.

## Features

**Triage**

- The inbox is sorted into action tags: Needs reply, Receipt, Travel,
  Newsletter, and so on.
- Threads can be snoozed with a preset, a date from a calendar, or a moment
  the AI suggests after reading the mail — the day before a deadline, an hour
  before a meeting.
- Unsubscribe works in one click, and a subscriptions dialog lists every
  mailing list you're on, sorted by how much each sender mails you.
- Every action has a keyboard shortcut, and all of them can be rebound.

**Reading and writing**

- A conversation renders as a single thread, with quoted history folded away.
- Meeting invites appear as a card with Accept, Maybe, and Decline buttons.
- Thread summaries, drafted replies, translation, and proofreading are a
  click away.
- A sent message can be undone for a few seconds; failed sends are queued and
  retried until they go out.
- New-mail notifications carry a one-line gist of the message.

**Accounts**

- Gmail uses its native API, so labels, server-side search, and sync behave
  the way Gmail expects.
- IMAP accounts get new mail pushed as it arrives when the server supports
  it, and are polled otherwise.
- Any number of accounts can run side by side, each with its own name and
  signature.

## Get started

```bash
sudo dnf install golang gtk4-devel libadwaita-devel webkitgtk6.0-devel libsoup3-devel libsecret-devel
make build
./bin/mailbox
```

The app opens to a welcome screen, and **Add account…** takes it from there.
IMAP providers need only an app password; Gmail and Outlook need a one-time
client setup. **[docs/SETUP.md](docs/SETUP.md)** walks through every option,
including turning on the AI.

## License

[MIT](LICENSE)
