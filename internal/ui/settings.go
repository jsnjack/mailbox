package ui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"github.com/jsnjack/mailbox/internal/config"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/logging"
)

// humanBytes formats a byte count as B/KB/MB/GB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// openSettings shows a preferences window for the AI provider config. Values are
// saved to config.toml when the window is closed; they take effect on next launch.
func (w *window) openSettings() {
	if w.deps.AISettings == nil {
		logging.Trace("ui: open settings skipped", "reason", "no AI settings")
		return
	}
	provider, endpoint, model := w.deps.AISettings()
	logging.Trace("ui: open settings", "provider", provider, "endpoint", endpoint, "model", model,
		"accounts", len(w.deps.Accounts), "categorize", w.inboxCategories, "block_images", w.blockImages)

	providerRow := adw.NewEntryRow()
	providerRow.SetTitle("Provider (openai / litellm / anthropic)")
	providerRow.SetText(provider)

	endpointRow := adw.NewEntryRow()
	endpointRow.SetTitle("Endpoint (base URL incl. /v1)")
	endpointRow.SetText(endpoint)

	modelRow := adw.NewEntryRow()
	modelRow.SetTitle("Model")
	modelRow.SetText(model)

	group := adw.NewPreferencesGroup()
	group.SetTitle("AI")
	group.SetDescription("Changes take effect after restarting Mailbox. The API key is stored separately (mailbox set-ai-key).")
	group.Add(providerRow)
	group.Add(endpointRow)
	group.Add(modelRow)

	// A "Test connection" button validates the entered settings with a tiny live
	// request; the result shows on the button itself (success/error styling, full
	// error in the tooltip).
	if w.deps.TestAISettings != nil {
		testBtn := gtk.NewButtonWithLabel("Test connection")
		testBtn.SetVAlign(gtk.AlignCenter)
		testBtn.ConnectClicked(func() {
			provider, endpoint, model := providerRow.Text(), endpointRow.Text(), modelRow.Text()
			logging.Trace("ui: settings test AI connection", "provider", provider, "endpoint", endpoint, "model", model)
			testBtn.SetSensitive(false)
			testBtn.SetLabel("Testing…")
			testBtn.RemoveCSSClass("success")
			testBtn.RemoveCSSClass("error")
			testBtn.SetTooltipText("")
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				err := w.deps.TestAISettings(ctx, provider, endpoint, model)
				dispatch.Main(func() {
					testBtn.SetSensitive(true)
					if err != nil {
						logging.Trace("ui: settings test AI connection failed", "err", err)
						testBtn.SetLabel("Test failed")
						testBtn.AddCSSClass("error")
						testBtn.SetTooltipText(err.Error())
					} else {
						logging.Trace("ui: settings test AI connection ok")
						testBtn.SetLabel("Connected ✓")
						testBtn.AddCSSClass("success")
					}
				})
			}()
		})
		group.SetHeaderSuffix(testBtn)
	}

	// One naming field per connected account ("Home", "Work", …), each with a
	// Remove button (when account management is wired).
	nameRows := map[string]*adw.EntryRow{}
	acctGroup := adw.NewPreferencesGroup()
	acctGroup.SetTitle("Accounts")
	acctGroup.SetDescription("Give each account a name (e.g. Home, Work) — shown in the sidebar. Leave blank to use the email.")
	for _, a := range w.deps.Accounts {
		a := a
		r := adw.NewEntryRow()
		r.SetTitle(a.Email)
		r.SetText(w.accountNames[a.Email])
		if w.deps.RemoveAccount != nil {
			rm := gtk.NewButtonFromIconName("user-trash-symbolic")
			rm.SetTooltipText("Remove account")
			rm.AddCSSClass("flat")
			rm.SetVAlign(gtk.AlignCenter)
			rm.ConnectClicked(func() {
				w.confirmRemoveAccount(a, func() {
					acctGroup.Remove(r)
					delete(nameRows, a.Email)
					delete(w.accountNames, a.Email)
				})
			})
			r.AddSuffix(rm)
		}
		acctGroup.Add(r)
		nameRows[a.Email] = r
	}

	// Signatures: always an editable global default (key ""), plus a per-account
	// override editor (key = email) for each account when there's more than one.
	// A blank override falls back to the default. With a single account the
	// default is all you need, so no override editors are shown.
	multiAcct := len(w.deps.Accounts) > 1
	sigGroup := adw.NewPreferencesGroup()
	sigGroup.SetTitle("Signature")
	if multiAcct {
		sigGroup.SetDescription("A default for all accounts, plus optional per-account overrides. Leave an override blank to use the default.")
	} else {
		sigGroup.SetDescription("Appended to new messages and replies (below your text, above any quote). Leave blank for none.")
	}
	sigViews := map[string]*gtk.TextView{}
	sigSeeded := map[string]string{}
	addSigEditor := func(key, heading, seeded string) {
		if heading != "" {
			lbl := gtk.NewLabel(heading)
			lbl.SetXAlign(0)
			lbl.AddCSSClass("heading")
			lbl.SetMarginTop(6)
			sigGroup.Add(lbl)
		}
		sigView := gtk.NewTextView()
		sigView.SetWrapMode(gtk.WrapWordChar)
		sigView.SetTopMargin(6)
		sigView.SetBottomMargin(6)
		sigView.SetLeftMargin(8)
		sigView.SetRightMargin(8)
		sigView.Buffer().SetText(seeded)
		sigScroll := gtk.NewScrolledWindow()
		sigScroll.SetMinContentHeight(90)
		sigScroll.SetChild(sigView)
		sigScroll.AddCSSClass("card")
		sigGroup.Add(sigScroll)
		sigViews[key] = sigView
		sigSeeded[key] = strings.TrimSpace(seeded)
	}

	globalSig, _ := config.LoadSignature()
	defaultHeading := ""
	if multiAcct {
		defaultHeading = "Default (all accounts)"
	}
	addSigEditor("", defaultHeading, globalSig)
	if multiAcct {
		overrides, _ := config.LoadAccountSignatures()
		for _, a := range w.deps.Accounts {
			addSigEditor(a.Email, a.Email, overrides[a.Email])
		}
	}

	// Load-modify-save so the two prefs toggles don't clobber each other's field.
	savePref := func(mut func(*config.Prefs)) {
		p, _ := config.LoadPrefs()
		mut(&p)
		if err := config.SavePrefs(p); err != nil {
			slog.Warn("ui: save prefs", "err", err)
		}
	}

	// Privacy: a global default for loading remote images (tracking pixels are
	// always stripped regardless).
	imgRow := adw.NewSwitchRow()
	imgRow.SetTitle("Load remote images")
	imgRow.SetSubtitle("Tracking pixels are always blocked. Turn off to block all remote images by default.")
	imgRow.SetActive(!w.blockImages)
	imgRow.Connect("notify::active", func() {
		load := imgRow.Active()
		logging.Trace("ui: setting changed", "pref", "load_remote_images", "old", !w.blockImages, "new", load)
		w.blockImages = !load
		savePref(func(p *config.Prefs) { p.BlockRemoteImages = !load })
		w.setImagesEnabled(load)
	})
	privacyGroup := adw.NewPreferencesGroup()
	privacyGroup.SetTitle("Privacy")
	privacyGroup.Add(imgRow)

	// AI categorization of the inbox (sends mail to the AI provider).
	if w.deps.Assistant != nil {
		catRow := adw.NewSwitchRow()
		catRow.SetTitle("Categorize inbox with AI")
		catRow.SetSubtitle("Tags inbox mail (Needs reply, Receipt, …). Sends message details to the AI provider.")
		catRow.SetActive(w.inboxCategories)
		catRow.Connect("notify::active", func() {
			on := catRow.Active()
			logging.Trace("ui: setting changed", "pref", "categorize_inbox", "old", w.inboxCategories, "new", on)
			w.inboxCategories = on
			savePref(func(p *config.Prefs) { p.DisableInboxCategories = !on })
			if on {
				w.categorizeInbox()
			}
		})
		privacyGroup.Add(catRow)
	}

	// Storage: clear the (re-downloadable) attachment cache.
	clearRow := adw.NewActionRow()
	clearRow.SetTitle("Cached attachments")
	clearRow.SetSubtitle("Downloaded attachments are kept on disk for quick reopening.")
	clearBtn := gtk.NewButtonWithLabel("Clear")
	clearBtn.SetVAlign(gtk.AlignCenter)
	clearBtn.ConnectClicked(func() {
		logging.Trace("ui: settings clear attachment cache")
		freed, err := config.ClearAttachmentsCache()
		if err != nil {
			slog.Warn("ui: clear attachments cache", "err", err)
			clearRow.SetSubtitle("Couldn't clear the cache.")
			return
		}
		logging.Trace("ui: settings cleared attachment cache", "freed", freed)
		clearRow.SetSubtitle(fmt.Sprintf("Cleared — freed %s.", humanBytes(freed)))
		clearBtn.SetSensitive(false)
	})
	clearRow.AddSuffix(clearBtn)

	// Storage: compact the database, reclaiming space left by deleted mail.
	dbRow := adw.NewActionRow()
	dbRow.SetTitle("Database")
	if sz, err := config.DBSize(); err == nil && sz > 0 {
		dbRow.SetSubtitle(fmt.Sprintf("%s on disk. Compact to reclaim space from deleted mail.", humanBytes(sz)))
	} else {
		dbRow.SetSubtitle("Compact to reclaim space left by deleted mail.")
	}
	compactBtn := gtk.NewButtonWithLabel("Compact")
	compactBtn.SetVAlign(gtk.AlignCenter)
	compactBtn.ConnectClicked(func() {
		compactBtn.SetSensitive(false)
		compactBtn.SetLabel("Compacting…")
		before, _ := config.DBSize()
		logging.Trace("ui: settings compact database", "before", before)
		go func() {
			err := w.deps.Store.Vacuum(context.Background())
			after, _ := config.DBSize()
			dispatch.Main(func() {
				compactBtn.SetLabel("Compact")
				logging.Trace("ui: settings compact database done", "before", before, "after", after, "err", err)
				if err != nil {
					slog.Warn("ui: compact database", "err", err)
					dbRow.SetSubtitle("Couldn't compact the database.")
					compactBtn.SetSensitive(true)
					return
				}
				if freed := before - after; freed > 0 {
					dbRow.SetSubtitle(fmt.Sprintf("Compacted — freed %s (now %s).", humanBytes(freed), humanBytes(after)))
				} else {
					dbRow.SetSubtitle(fmt.Sprintf("Already compact (%s).", humanBytes(after)))
				}
			})
		}()
	})
	dbRow.AddSuffix(compactBtn)

	storageGroup := adw.NewPreferencesGroup()
	storageGroup.SetTitle("Storage")
	storageGroup.Add(clearRow)
	storageGroup.Add(dbRow)

	page := adw.NewPreferencesPage()
	page.Add(group)
	if len(w.deps.Accounts) > 0 {
		page.Add(acctGroup)
	}
	page.Add(sigGroup)
	page.Add(privacyGroup)
	page.Add(storageGroup)

	dialog := adw.NewPreferencesDialog()
	dialog.SetContentWidth(520)
	dialog.SetContentHeight(360)
	dialog.Add(page)
	dialog.ConnectClosed(func() {
		logging.Trace("ui: settings dialog closed, saving")
		if w.deps.SaveAISettings != nil {
			np, ne, nm := providerRow.Text(), endpointRow.Text(), modelRow.Text()
			logging.Trace("ui: save AI settings", "provider", np, "endpoint", ne, "model", nm)
			if err := w.deps.SaveAISettings(np, ne, nm); err != nil {
				slog.Warn("ui: save settings", "err", err)
			}
		}
		w.applyAccountNames(nameRows)
		for key, view := range sigViews { // key "" = global default; else account email
			newSig := strings.TrimSpace(bodyText(view.Buffer()))
			if newSig == sigSeeded[key] {
				continue // unchanged → keep its override / global default
			}
			var err error
			if key == "" {
				err = config.SaveSignature(newSig) // global default
			} else {
				// Blank removes the per-account override (falls back to default).
				err = config.SaveAccountSignature(key, newSig)
			}
			logging.Trace("ui: save signature", "account", key, "old_len", len(sigSeeded[key]), "new_len", len(newSig), "err", err)
			if err != nil {
				slog.Warn("ui: save signature", "err", err, "account", key)
			}
		}
		// Re-resolve the active account's effective signature (default and/or its
		// override may have changed).
		w.signature = w.signatureForActive()
	})
	dialog.Present(w.win)
}

// applyAccountNames persists any changed account display names and, when
// something changed, re-renders the switcher so the new names/avatars show
// without a restart.
func (w *window) applyAccountNames(rows map[string]*adw.EntryRow) {
	changed := false
	for email, r := range rows {
		newName := strings.TrimSpace(r.Text())
		if newName == strings.TrimSpace(w.accountNames[email]) {
			continue
		}
		logging.Trace("ui: rename account", "email", email, "old", w.accountNames[email], "new", newName)
		if err := config.SaveAccountName(email, newName); err != nil {
			slog.Warn("ui: save account name", "email", email, "err", err)
			continue
		}
		if newName == "" {
			delete(w.accountNames, email)
		} else {
			w.accountNames[email] = newName
		}
		changed = true
	}
	if changed {
		w.rebuildAccountSwitcher()
	}
}

// confirmRemoveAccount asks for confirmation, then removes the account (stopping
// its sync and deleting its local cache + secret) and updates the UI. onRemoved
// runs on success to drop the account's row from the open Preferences dialog.
func (w *window) confirmRemoveAccount(a AccountInfo, onRemoved func()) {
	if w.deps.RemoveAccount == nil {
		return
	}
	body := "Remove " + a.Email + " from Mailbox? Its cached mail will be deleted " +
		"from this device and it will stop syncing. Mail on the server is not affected."
	dialog := adw.NewAlertDialog("Remove account?", body)
	dialog.AddResponse("cancel", "Cancel")
	dialog.AddResponse("remove", "Remove")
	dialog.SetResponseAppearance("remove", adw.ResponseDestructive)
	dialog.SetDefaultResponse("cancel")
	dialog.SetCloseResponse("cancel")
	dialog.ConnectResponse(func(resp string) {
		if resp != "remove" {
			logging.Trace("ui: remove account cancelled", "id", a.ID, "email", a.Email)
			return
		}
		logging.Trace("ui: remove account confirmed", "id", a.ID, "email", a.Email)
		go func() {
			err := w.deps.RemoveAccount(context.Background(), a.ID)
			dispatch.Main(func() {
				if err != nil {
					logging.Trace("ui: remove account failed", "id", a.ID, "err", err)
					w.toast("Couldn't remove account: " + err.Error())
					return
				}
				logging.Trace("ui: remove account done", "id", a.ID, "email", a.Email)
				w.removeAccountFromUI(a.ID)
				if onRemoved != nil {
					onRemoved()
				}
				w.toast("Removed " + a.Email)
			})
		}()
	})
	dialog.Present(w.win)
}
