# Setup

How to build Mailbox, connect a Gmail account, and turn on the AI features.

## Build dependencies (Fedora)

```bash
sudo dnf install golang gtk4-devel libadwaita-devel webkitgtk6.0-devel libsoup3-devel libsecret-devel
```

## Build

```bash
make build    # compiles to bin/mailbox
```

The first build compiles the GTK4/WebKit cgo bindings and is slow (~10–15 min); subsequent builds are cached.

On Fedora you can instead build and install an RPM (which pulls the runtime libraries automatically):

```bash
make rpm
sudo dnf install ./rpmbuild/RPMS/x86_64/mailbox-*.rpm
```

## Connect a Gmail account

### 1. Set up a Google OAuth client

You can reuse an existing Google Cloud project — you don't need a new one. In the
[Google Cloud Console](https://console.cloud.google.com), make sure that project has:

- **The Gmail API enabled** (APIs & Services → Library → Gmail API → Enable).
- An **OAuth consent screen** configured, with your Gmail address listed under **Test users**.
- An **OAuth client ID** of type **Desktop app** (reuse one if you have it, otherwise add one to
  the same project). Download its JSON to `~/.config/mailbox/credentials.json`.

> **Why your own OAuth client?** Mailbox uses Gmail's *restricted* scopes (read/modify/send).
> Google only lets an app request those from arbitrary users after a paid annual security
> assessment (CASA), which this hobby project doesn't have. So instead of one published app for
> everyone, you run your own OAuth client. A client in **Testing** mode allows up to **100 Test
> users** — enough to share one client among yourself, family, or a small team: add each address
> under the consent screen's *Test users*, and everyone authorizes their own Google account against
> it. Each person's token is theirs and stays in their own keyring.

### 2. Add your account

```bash
./bin/mailbox sync --account your@gmail.com --credentials ~/.config/mailbox/credentials.json
```

This opens a browser for OAuth login, then syncs your mail. (`--credentials` is optional once the
file is at the default path above.)

### 3. Launch

```bash
./bin/mailbox
```

After the first sync you can just run `./bin/mailbox` — the refresh token is stored in the OS
keyring and the config is persisted.

## Enable the AI features (optional)

The AI features stay dormant until a provider is configured. Set the provider/endpoint/model in
**Preferences → AI** (or via the `MAILBOX_AI_*` env vars), then store the key:

```bash
printf '%s' "$YOUR_API_KEY" | ./bin/mailbox set-ai-key
```

Any OpenAI-compatible endpoint (OpenAI, a LiteLLM proxy, etc.) or Anthropic works; the key lives
only in the OS keyring.

## Configuration

| What | Where |
|---|---|
| Config | `~/.config/mailbox/config.toml` |
| Database | `~/.local/share/mailbox/mailbox.db` |
| OAuth credentials | `~/.config/mailbox/credentials.json` |
| AI key | OS keyring (`mailbox set-ai-key`) |
| Signature | `~/.config/mailbox/signature.txt` |

AI provider can also be set via env vars: `MAILBOX_AI_PROVIDER`, `MAILBOX_AI_ENDPOINT`,
`MAILBOX_AI_MODEL`, `MAILBOX_AI_KEY`.
