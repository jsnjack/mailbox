# Setup

How to build Mailbox, connect accounts (Gmail and IMAP), and turn on the AI features.

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

## Connect your first account (Gmail)

Mailbox needs one account to launch; connect it from the command line as below.
Once the app is running, add any others — Gmail or IMAP — from **Add account…**
in the main (☰) menu (see [Add more accounts](#add-more-accounts)).

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

## Add more accounts

With Mailbox running, open the main (☰) menu → **Add account…**. Pick a provider, fill in the
form, and click **Test & Add** — the account starts syncing right away (no restart) and appears
in the sidebar switcher. The connection secret (app password or OAuth refresh token) goes to the
OS keyring; per-account server settings are stored in `~/.local/share/mailbox/imap-accounts.json`.

| Provider | How it connects |
|---|---|
| **Gmail** | Sign in with Google (native Gmail API — same as the CLI flow above). Needs `credentials.json`. |
| **Gmail (IMAP)** | Sign in with Google; connects over IMAP/SMTP with the full-mailbox scope instead of the API. |
| **Outlook / Office 365** | Sign in with Microsoft (OAuth). Requires an Azure app client id — see below. |
| **Yahoo, iCloud, Fastmail** | Enter your email and an **app password** (not your normal password); the dialog links to where each provider creates one. |
| **Other (IMAP)** | Enter the email, password, and IMAP/SMTP host/port/security under **Advanced**. |

Most providers require an **app-specific password** rather than your account password (and may need
IMAP enabled in their settings first):

- **Yahoo** — [account security → app passwords](https://login.yahoo.com/account/security/app-passwords)
- **iCloud** — [account.apple.com](https://account.apple.com/account/manage) → App-Specific Passwords
- **Fastmail** — [Settings → Privacy & Security → app passwords](https://www.fastmail.com/settings/security/devicekeys)

### Outlook / Office 365 (Azure app id)

Outlook OAuth needs a public client id from an [Azure app registration](https://portal.azure.com)
(Microsoft Entra ID → App registrations → New registration). Under **Authentication**, add a
**Mobile and desktop applications** platform with a loopback redirect — Mailbox redirects to
`http://127.0.0.1/callback` on a per-login port — and grant the delegated Graph/IMAP permissions
`IMAP.AccessAsUser.All`, `SMTP.Send`, and `offline_access`. Then point Mailbox at the client id:

```bash
export MAILBOX_MS_CLIENT_ID=<your-application-client-id>
./bin/mailbox
```

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
| Gmail OAuth client | `~/.config/mailbox/credentials.json` |
| IMAP account servers | `~/.local/share/mailbox/imap-accounts.json` |
| Account secrets | OS keyring — Gmail tokens under `mailbox`, IMAP app passwords / OAuth tokens under `mailbox-imap` |
| AI key | OS keyring (`mailbox set-ai-key`) |
| Signature | `~/.config/mailbox/signature.txt` |

Env vars: AI provider — `MAILBOX_AI_PROVIDER`, `MAILBOX_AI_ENDPOINT`, `MAILBOX_AI_MODEL`,
`MAILBOX_AI_KEY`; Outlook OAuth — `MAILBOX_MS_CLIENT_ID`.
