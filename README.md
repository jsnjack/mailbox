# Mailbox

A native, fast Gmail client for Linux/GNOME with AI built in.

Written in Go with GTK4 + libadwaita.

## Why This Exists

GNOME doesn't have a good modern, fast email client. Evolution is bloated. Geary is unmaintained. The alternatives are Electron apps or web wrappers.

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

- **SPF/DKIM/DMARC badge** — sender authentication verdict shown per message
- **Deception detection** — deterministic checks for display-name spoofing and deceptive link text
- **Tracker blocking** — tracking pixels stripped before render, count surfaced per message
- **Sanitized rendering** — email-tuned HTML sanitizer with strict per-render CSP (no email-supplied JavaScript)

## Requirements

- Linux (GNOME recommended)
- GTK4, libadwaita, WebKitGTK 6.0, libsecret

## Build

```bash
make build    # compiles to bin/mailbox
make run      # build and launch
make check    # fmt → vet → build → test → lint
make rpm      # build RPM package
```

Headless sync (no GUI required):

```bash
mailbox sync --account <email> --credentials <client_secret.json>
```

## Configuration

- **Config:** `~/.config/mailbox/config.toml`
- **Database:** `~/.local/share/mailbox/mailbox.db`
- **AI key:** stored in OS keyring (`printf '%s' "$KEY" | mailbox set-ai-key`)
- **Signature:** `~/.config/mailbox/signature.txt`

AI provider can be configured in the Preferences dialog or via env vars: `MAILBOX_AI_PROVIDER`, `MAILBOX_AI_ENDPOINT`, `MAILBOX_AI_MODEL`, `MAILBOX_AI_KEY`.

## Architecture

Single SQLite cache as source of truth. Background goroutines sync Gmail into SQLite and publish ID-only change events. GTK UI reads SQLite and re-queries on each event.

```
Gmail REST API → sync engine → SQLite → dispatch.Main → GTK UI
```

## License

MIT
