# Mailbox

A native, fast Gmail client for Linux/GNOME with AI built in.

Written in Go with GTK4 + libadwaita.

## Why This Exists

GNOME doesn't have a good modern, fast email client. Geary is buggy, Thunderbird is slow and heavy, and the rest are Electron apps or web wrappers.

Mailbox fills that gap — a native GTK4 app that's fast, small, and feels at home on GNOME, with Gmail as the backend and AI as a genuine productivity layer.

## AI Features

- **Inbox categorization** — incoming mail auto-tagged by AI (Needs reply, Calendar, Receipt, Finance, Security, etc.) so you can prioritize what matters
- **Thread summarization** — one-click bullet summary of long conversations, cached per thread so reopening is instant
- **Draft replies** — AI drafts a contextual reply you review before sending, with smart quick-reply suggestions
- **Grammar check** — proofread compose bodies before sending
- **Translate** — translate message bodies in place
- **Phishing analysis** — on-demand AI security review fed by sender auth signals (SPF/DKIM/DMARC) and deterministic deception detection

AI provider is user-configurable: OpenAI-compatible endpoints (including LiteLLM proxies) or Anthropic. Your API key stays in the OS keyring.

## Security

- **Sender verification** — SPF/DKIM/DMARC results shown per message
- **Deception detection** — catches display-name spoofing and deceptive links
- **Tracker blocking** — tracking pixels stripped before render
- **Sanitized rendering** — no email-supplied JavaScript can run

## Requirements

- Linux (GNOME recommended)
- GTK4, libadwaita, WebKitGTK 6.0, libsecret

## Screenshot

![Mailbox](screenshots/main.png)

## Install

From source:

```bash
make build    # compiles to bin/mailbox
```

On Fedora, you can build an RPM:

```bash
make rpm
sudo dnf install ./rpmbuild/RPMS/x86_64/mailbox-*.rpm
```

Build dependencies (Fedora):

```bash
sudo dnf install gtk4-devel libadwaita-devel webkitgtk6.0-devel libsecret-devel
```

## Quick Start

1. **Create a Google OAuth credential** — go to [Google Cloud Console](https://console.cloud.google.com), create an OAuth 2.0 client ID (type: Desktop app), and download the JSON as `credentials.json` into `~/.config/mailbox/`.
2. **Add your account** — run `mailbox sync --account your@gmail.com --credentials ~/.config/mailbox/credentials.json`. This opens a browser for OAuth login, then syncs your mail.
3. **Launch** — run `mailbox` to start the GUI.

After the first sync you can just run `mailbox` — tokens are stored in the OS keyring and the config is persisted.

## Configuration

- **Config:** `~/.config/mailbox/config.toml`
- **Database:** `~/.local/share/mailbox/mailbox.db`
- **AI key:** stored in OS keyring (`printf '%s' "$KEY" | mailbox set-ai-key`)
- **Signature:** `~/.config/mailbox/signature.txt`

AI provider can be configured in the Preferences dialog or via env vars: `MAILBOX_AI_PROVIDER`, `MAILBOX_AI_ENDPOINT`, `MAILBOX_AI_MODEL`, `MAILBOX_AI_KEY`.

## License

MIT
