package ui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/config"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// addAccountPrefill seeds the add-account dialog for a reconnect: the existing
// account's email and the provider preset to preselect.
type addAccountPrefill struct {
	email     string
	presetID  string // config preset id to preselect ("" = leave default)
	reconnect bool   // re-authenticating an existing account (vs. adding a new one)
}

// openAddAccount presents the add-account dialog: pick a provider, fill in
// credentials (a password/app password, or an OAuth sign-in), optionally tweak
// the servers under Advanced, then Test & Add. The account begins syncing
// immediately. When prefill is non-nil the dialog is seeded for reconnecting an
// existing account (same email → its cache is preserved).
func (w *window) openAddAccount(prefill *addAccountPrefill) {
	if w.deps.AddIMAPAccount == nil {
		logging.Trace("ui: add account unavailable")
		w.toast("Adding accounts isn't available")
		return
	}
	if prefill != nil {
		logging.Trace("ui: open add account", "reconnect", prefill.reconnect, "email", prefill.email, "preset", prefill.presetID)
	} else {
		logging.Trace("ui: open add account", "reconnect", false)
	}

	dialog := adw.NewPreferencesDialog()
	dialog.SetTitle("Add account")
	if prefill != nil && prefill.reconnect {
		dialog.SetTitle("Reconnect account")
	}
	page := adw.NewPreferencesPage()

	// --- provider + identity ---
	idGroup := adw.NewPreferencesGroup()
	idGroup.SetTitle("Account")

	providerRow := adw.NewComboRow()
	providerRow.SetTitle("Provider")
	names := make([]string, len(config.Presets))
	for i, p := range config.Presets {
		names[i] = p.Name
	}
	providerRow.SetModel(gtk.NewStringList(names))

	emailRow := adw.NewEntryRow()
	emailRow.SetTitle("Email address")

	passwordRow := adw.NewPasswordEntryRow()
	passwordRow.SetTitle("Password")

	idGroup.Add(providerRow)
	idGroup.Add(emailRow)
	idGroup.Add(passwordRow)

	hint := gtk.NewLabel("")
	hint.SetXAlign(0)
	hint.SetWrap(true)
	hint.AddCSSClass("dim-label")
	hint.AddCSSClass("caption")
	setMargins(hint, 12, 12, 0, 6)
	idGroup.Add(hint)

	// --- advanced server settings (prefilled per provider) ---
	advGroup := adw.NewPreferencesGroup()
	advGroup.SetTitle("Advanced")
	imapHost := entryRow("IMAP server")
	imapPort := entryRow("IMAP port")
	smtpHost := entryRow("SMTP server")
	smtpPort := entryRow("SMTP port")
	for _, r := range []*adw.EntryRow{imapHost, imapPort, smtpHost, smtpPort} {
		advGroup.Add(r)
	}

	page.Add(idGroup)
	page.Add(advGroup)
	dialog.Add(page)

	// OAuth state: for Gmail/Outlook the password field is replaced by a sign-in
	// step that yields a refresh token.
	var (
		oauthToken string
		oauthEmail string // verified address from sign-in (Gmail), if any
		oauthDone  bool
	)
	current := func() config.Preset { return config.Presets[providerRow.Selected()] }

	applyPreset := func() {
		p := current()
		imapHost.SetText(p.IMAPHost)
		imapPort.SetText(itoa(p.IMAPPort))
		smtpHost.SetText(p.SMTPHost)
		smtpPort.SetText(itoa(p.SMTPPort))
		oauthToken, oauthEmail, oauthDone = "", "", false
		isOAuth := p.Auth == config.AuthGoogle || p.Auth == config.AuthMicrosoft || p.Auth == config.AuthGmailREST
		passwordRow.SetVisible(!isOAuth)
		// Gmail's native REST backend uses no IMAP/SMTP servers, so the Advanced
		// section is meaningless for it.
		advGroup.SetVisible(p.Auth != config.AuthGmailREST)
		h := p.Hint
		if p.URL != "" {
			h += "  " + p.URL
		}
		hint.SetText(h)
		logging.Trace("ui: add account preset chosen", "preset", p.ID, "auth", p.Auth, "imap_host", p.IMAPHost, "smtp_host", p.SMTPHost, "oauth", isOAuth)
	}
	if prefill != nil {
		emailRow.SetText(prefill.email)
		for i, p := range config.Presets {
			if p.ID == prefill.presetID {
				providerRow.SetSelected(uint(i))
				break
			}
		}
	}
	providerRow.Connect("notify::selected", applyPreset)
	applyPreset()

	// --- footer: status + Test & Add ---
	footer := gtk.NewBox(gtk.OrientationHorizontal, 8)
	setMargins(footer, 12, 12, 6, 12)
	status := gtk.NewLabel("")
	status.SetXAlign(0)
	status.SetHExpand(true)
	status.SetWrap(true)
	addBtn := gtk.NewButtonWithLabel("Test & Add")
	addBtn.AddCSSClass("suggested-action")
	footer.Append(status)
	footer.Append(addBtn)
	footerGroup := adw.NewPreferencesGroup()
	footerGroup.Add(footer)
	page.Add(footerGroup)

	// gather builds the account config from the form.
	gather := func() (config.IMAPAccount, config.Preset) {
		p := current()
		ip, _ := strconv.Atoi(strings.TrimSpace(imapPort.Text()))
		sp, _ := strconv.Atoi(strings.TrimSpace(smtpPort.Text()))
		email := strings.TrimSpace(emailRow.Text())
		return config.IMAPAccount{
			Email: email, Username: email,
			IMAPHost: strings.TrimSpace(imapHost.Text()), IMAPPort: ip, IMAPSecurity: p.IMAPSecurity,
			SMTPHost: strings.TrimSpace(smtpHost.Text()), SMTPPort: sp, SMTPSecurity: p.SMTPSecurity,
			Auth: p.Auth,
		}, p
	}

	// finish persists + starts the account, registers it in the switcher, and closes.
	finish := func(acct config.IMAPAccount, secret string) {
		logging.Trace("ui: add account persist", "email", acct.Email, "auth", acct.Auth,
			"imap_host", acct.IMAPHost, "smtp_host", acct.SMTPHost, "secret_len", len(secret))
		go func() {
			id, err := w.deps.AddIMAPAccount(context.Background(), acct, secret)
			dispatch.Main(func() {
				if err != nil {
					logging.Trace("ui: add account persist failed", "email", acct.Email, "err", err)
					status.SetText("Couldn't add account: " + friendlyConnError(err))
					addBtn.SetSensitive(true)
					return
				}
				logging.Trace("ui: add account persisted", "email", acct.Email, "id", id)
				dialog.Close()
				atype := model.AccountIMAP
				if acct.Auth == config.AuthGmailREST {
					atype = model.AccountGmail
				}
				w.addAccount(AccountInfo{ID: id, Email: acct.Email, Type: atype})
				w.toast("Account added — syncing " + acct.Email)
			})
		}()
	}

	addBtn.ConnectClicked(func() {
		acct, p := gather()
		if acct.Email == "" {
			status.SetText("Enter your email address.")
			return
		}
		logging.Trace("ui: add account test&add clicked", "email", acct.Email, "auth", p.Auth)
		addBtn.SetSensitive(false)

		// OAuth providers (Gmail REST, Gmail-IMAP, Outlook): sign in via the
		// browser to obtain a refresh token, then add.
		if p.Auth == config.AuthGmailREST || p.Auth == config.AuthGoogle || p.Auth == config.AuthMicrosoft {
			if oauthDone {
				logging.Trace("ui: add account oauth reuse token", "email", acct.Email, "token_present", oauthToken != "")
				if oauthEmail != "" {
					acct.Email, acct.Username = oauthEmail, oauthEmail
				}
				finish(acct, oauthToken)
				return
			}
			status.SetText("Opening your browser to sign in…")
			logging.Trace("ui: add account oauth begin", "auth", p.Auth, "email", acct.Email)
			go func() {
				email, tok, err := w.deps.OAuthConnect(context.Background(), p.Auth)
				dispatch.Main(func() {
					if err != nil {
						logging.Trace("ui: add account oauth failed", "auth", p.Auth, "err", err)
						status.SetText("Sign-in failed: " + err.Error())
						addBtn.SetSensitive(true)
						return
					}
					logging.Trace("ui: add account oauth ok", "auth", p.Auth, "verified_email", email, "token_present", tok != "", "token_len", len(tok))
					// Prefer the address actually signed in (Gmail reports it via the
					// profile) over whatever was typed.
					if email != "" {
						acct.Email, acct.Username = email, email
					}
					oauthToken, oauthEmail, oauthDone = tok, email, true
					finish(acct, tok)
				})
			}()
			return
		}

		// Password providers: validate the connection, then add.
		if strings.TrimSpace(passwordRow.Text()) == "" {
			status.SetText("Enter your password (or app password).")
			addBtn.SetSensitive(true)
			return
		}
		pw := passwordRow.Text()
		status.SetText("Testing connection…")
		logging.Trace("ui: add account test connection", "email", acct.Email, "imap_host", acct.IMAPHost, "password_len", len(pw))
		go func() {
			err := w.deps.TestIMAPAccount(context.Background(), acct, pw)
			dispatch.Main(func() {
				if err != nil {
					logging.Trace("ui: add account test failed", "email", acct.Email, "err", err)
					status.SetText("Connection failed: " + friendlyConnError(err))
					addBtn.SetSensitive(true)
					return
				}
				logging.Trace("ui: add account test ok", "email", acct.Email)
				finish(acct, pw)
			})
		}()
	})

	dialog.Present(w.win)
}

// friendlyConnError turns a raw connection error into a plain-language message
// for the add-account dialog. It recognizes the common failure modes (bad
// credentials, wrong host, refused/timed-out connection, TLS) and otherwise
// falls back to the underlying error.
func friendlyConnError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, backend.ErrAuth) {
		return "The username or password was rejected. Most providers require an app password, not your normal account password — see the link above."
	}
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "no such host"), strings.Contains(low, "server misbehaving"):
		return "Couldn't find that server — check the IMAP/SMTP host name."
	case strings.Contains(low, "connection refused"):
		return "The server refused the connection — check the host and port."
	case strings.Contains(low, "timeout"), strings.Contains(low, "i/o timeout"), strings.Contains(low, "deadline exceeded"):
		return "The connection timed out — check the host, port, and your network."
	case strings.Contains(low, "certificate"), strings.Contains(low, "x509"), strings.Contains(low, "tls"):
		return "Couldn't establish a secure connection — the server's TLS certificate didn't verify."
	default:
		return err.Error()
	}
}

// reconnectAccount reopens the add-account dialog seeded for re-authenticating an
// existing account: its email and provider are prefilled, so the user only signs
// in again (OAuth) or re-enters the app password. Re-adding the same email
// preserves the account's cached mail (the sync cursor is kept).
func (w *window) reconnectAccount(a AccountInfo) {
	logging.Trace("ui: reconnect account", "id", a.ID, "email", a.Email, "type", a.Type)
	w.openAddAccount(&addAccountPrefill{
		email:     a.Email,
		presetID:  presetForReconnect(a),
		reconnect: true,
	})
}

// presetForReconnect picks the provider preset to preselect when reconnecting an
// account. Gmail (REST) is unambiguous; for IMAP it reads the stored auth kind
// and host to land on the right preset (falling back to Other).
func presetForReconnect(a AccountInfo) string {
	if a.Type != model.AccountIMAP {
		return "gmail"
	}
	acct, ok, _ := config.LoadIMAPAccount(a.Email)
	if !ok {
		return ""
	}
	switch acct.Auth {
	case config.AuthGoogle:
		return "gmail-imap"
	case config.AuthMicrosoft:
		return "outlook"
	}
	host := strings.ToLower(acct.IMAPHost)
	switch {
	case strings.Contains(host, "yahoo"):
		return "yahoo"
	case strings.Contains(host, "icloud"), strings.Contains(host, "me.com"):
		return "icloud"
	case strings.Contains(host, "fastmail"):
		return "fastmail"
	default:
		return "other"
	}
}

func entryRow(title string) *adw.EntryRow {
	r := adw.NewEntryRow()
	r.SetTitle(title)
	return r
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
}
