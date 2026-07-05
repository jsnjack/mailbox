# Setup

Build the app, connect your mail, and (optionally) turn on the AI.

## 1. Build

Dependencies (Fedora):

```bash
sudo dnf install golang gtk4-devel libadwaita-devel webkitgtk6.0-devel libsoup3-devel libsecret-devel
```

```bash
make build    # compiles to bin/mailbox
```

The first build compiles the GTK4/WebKit bindings and takes a while (~10–15
minutes); every build after that is cached and fast.

On Fedora you can instead build and install an RPM:

```bash
make rpm
sudo dnf install ./rpmbuild/RPMS/x86_64/mailbox-*.rpm
```

## 2. First launch

```bash
./bin/mailbox
```

The app opens to a welcome screen. Everything else happens from the main (☰)
menu → **Add account…**: pick a provider, fill in the form, and press
**Test & Add**. The account starts syncing immediately, without a restart,
and further accounts are added the same way.

## 3. Connect an account

| You have | Easiest path | You'll need |
|---|---|---|
| Yahoo, iCloud, Fastmail, or any IMAP server | **App password** (below) | An app password from your provider |
| Gmail | **Gmail, native API** (below) | A one-time Google OAuth client |
| Outlook / Office 365 | **Outlook** (below) | A one-time Azure app registration |

### App password — Yahoo, iCloud, Fastmail, any IMAP server

This path needs no client setup. Create an **app password** with
your provider (your normal password won't work, and some providers also want
IMAP switched on in their settings):

- **Yahoo** — [account security → app passwords](https://login.yahoo.com/account/security/app-passwords)
- **iCloud** — [account.apple.com](https://account.apple.com/account/manage) → App-Specific Passwords
- **Fastmail** — [Settings → Privacy & Security → app passwords](https://www.fastmail.com/settings/security/devicekeys)

Then **Add account…** → pick the provider → email + app password → **Test &
Add**. For a self-hosted or unlisted server, choose **Other (IMAP)** and enter
the IMAP/SMTP host, port, and security under **Advanced**.

### Gmail — native API

The native API gives Gmail its real labels, server-side search, and fast
sync. It needs a one-time setup of your own Google OAuth client:

1. In the [Google Cloud Console](https://console.cloud.google.com) (any
   project, new or existing):
   - enable the **Gmail API** (APIs & Services → Library),
   - configure the **OAuth consent screen** and add your Gmail address under
     **Test users**,
   - create an **OAuth client ID** of type **Desktop app** and download its
     JSON to `~/.config/mailbox/credentials.json`.
2. **Add account…** → **Gmail** → sign in with Google.

> **Why your own client?** Mailbox asks for Gmail's restricted scopes
> (read/modify/send). Google only grants those to published apps after a paid
> annual security review, which this project doesn't have — so you run your
> own client instead. A client in Testing mode allows up to 100 test users, so
> one client can serve your family or team: list each address under *Test
> users*, and each person signs in with their own Google account. Tokens stay
> in each person's own keyring.

The same flow also works from the terminal, and does the first sync in one
go:

```bash
./bin/mailbox sync --account your@gmail.com --credentials ~/.config/mailbox/credentials.json
```

### Gmail — over IMAP

**Add account…** also offers **Gmail (IMAP)**: the same Google sign-in and the
same `credentials.json`, but mail flows over IMAP/SMTP instead of the API.
Pick it if you specifically want the IMAP behavior; otherwise the native API
above is faster and more capable.

### Outlook / Office 365

Microsoft sign-in needs a public client id from a one-time
[Azure app registration](https://portal.azure.com) (Microsoft Entra ID → App
registrations → New registration):

1. Under **Authentication**, add a **Mobile and desktop applications**
   platform with a loopback redirect — Mailbox uses
   `http://127.0.0.1/callback` on a per-login port.
2. Grant the delegated permissions `IMAP.AccessAsUser.All`, `SMTP.Send`, and
   `offline_access`.
3. Point Mailbox at the client id and add the account:

```bash
export MAILBOX_MS_CLIENT_ID=<your-application-client-id>
./bin/mailbox
```

## 4. Turn on the AI (optional)

Set the provider, endpoint, and model in **Preferences → AI** (or the
`MAILBOX_AI_*` environment variables), then store the key:

```bash
printf '%s' "$YOUR_API_KEY" | ./bin/mailbox set-ai-key
```

Any OpenAI-compatible endpoint works, as does Anthropic directly. The key is
stored in the OS keyring, not in the config file.

## Where things live

| What | Where |
|---|---|
| Config | `~/.config/mailbox/config.toml` |
| Database (mail cache) | `~/.local/share/mailbox/mailbox.db` |
| Gmail OAuth client | `~/.config/mailbox/credentials.json` |
| IMAP account servers | `~/.local/share/mailbox/imap-accounts.json` |
| Account secrets | OS keyring — Gmail tokens under `mailbox`, IMAP passwords/tokens under `mailbox-imap` |
| AI key | OS keyring (`mailbox set-ai-key`) |
| Signature | `~/.config/mailbox/signature.txt` |
| Keyboard shortcuts | `~/.config/mailbox/shortcuts.json` |

Environment variables: `MAILBOX_AI_PROVIDER`, `MAILBOX_AI_ENDPOINT`,
`MAILBOX_AI_MODEL`, `MAILBOX_AI_KEY` (AI); `MAILBOX_MS_CLIENT_ID` (Outlook).
