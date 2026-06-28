package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/config"
	"github.com/jsnjack/mailbox/internal/dispatch"
)

// openAddAccount presents the add-account dialog: pick a provider, fill in
// credentials (a password/app password, or an OAuth sign-in), optionally tweak
// the servers under Advanced, then Test & Add. A new account begins syncing on
// the next launch.
func (w *window) openAddAccount() {
	if w.deps.AddIMAPAccount == nil {
		w.toast("Adding accounts isn't available")
		return
	}

	dialog := adw.NewPreferencesDialog()
	dialog.SetTitle("Add account")
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
		h := p.Hint
		if p.URL != "" {
			h += "  " + p.URL
		}
		hint.SetText(h)
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

	// finish persists the account and closes.
	finish := func(acct config.IMAPAccount, secret string) {
		go func() {
			err := w.deps.AddIMAPAccount(context.Background(), acct, secret)
			dispatch.Main(func() {
				if err != nil {
					status.SetText("Couldn't add account: " + err.Error())
					addBtn.SetSensitive(true)
					return
				}
				dialog.Close()
				w.toast("Account added — restart mailbox to start syncing it")
			})
		}()
	}

	addBtn.ConnectClicked(func() {
		acct, p := gather()
		if acct.Email == "" {
			status.SetText("Enter your email address.")
			return
		}
		addBtn.SetSensitive(false)

		// OAuth providers (Gmail REST, Gmail-IMAP, Outlook): sign in via the
		// browser to obtain a refresh token, then add.
		if p.Auth == config.AuthGmailREST || p.Auth == config.AuthGoogle || p.Auth == config.AuthMicrosoft {
			if oauthDone {
				if oauthEmail != "" {
					acct.Email, acct.Username = oauthEmail, oauthEmail
				}
				finish(acct, oauthToken)
				return
			}
			status.SetText("Opening your browser to sign in…")
			go func() {
				email, tok, err := w.deps.OAuthConnect(context.Background(), p.Auth)
				dispatch.Main(func() {
					if err != nil {
						status.SetText("Sign-in failed: " + err.Error())
						addBtn.SetSensitive(true)
						return
					}
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
		go func() {
			err := w.deps.TestIMAPAccount(context.Background(), acct, pw)
			dispatch.Main(func() {
				if err != nil {
					status.SetText("Connection failed: " + err.Error())
					addBtn.SetSensitive(true)
					return
				}
				finish(acct, pw)
			})
		}()
	})

	dialog.Present(w.win)
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
