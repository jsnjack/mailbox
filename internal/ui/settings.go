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

// shortcutList is the single source of truth for the keyboard shortcuts, shown
// in the Preferences dialog.
func shortcutList() [][2]string {
	return [][2]string{
		{"j / k", "Next / previous conversation"},
		{"r", "Reply"},
		{"f", "Forward"},
		{"a / e", "Archive"},
		{"# / Delete", "Move to Trash"},
		{"!", "Report spam / not spam"},
		{"s", "Star / unstar"},
		{"u", "Mark as unread"},
		{"t", "Translate to English"},
		{"c", "Compose"},
		{"/", "Search"},
		{"Ctrl + / Ctrl − / Ctrl 0", "Zoom message in / out / reset"},
		{"Esc", "Back to list"},
		{"?", "Open preferences"},
	}
}

// openSettings shows a preferences window for the AI provider config. Values are
// saved to config.toml when the window is closed; they take effect on next launch.
func (w *window) openSettings() {
	if w.deps.AISettings == nil {
		return
	}
	provider, endpoint, model := w.deps.AISettings()

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
						testBtn.SetLabel("Test failed")
						testBtn.AddCSSClass("error")
						testBtn.SetTooltipText(err.Error())
					} else {
						testBtn.SetLabel("Connected ✓")
						testBtn.AddCSSClass("success")
					}
				})
			}()
		})
		group.SetHeaderSuffix(testBtn)
	}

	// One naming field per connected account ("Home", "Work", …).
	nameRows := map[string]*adw.EntryRow{}
	acctGroup := adw.NewPreferencesGroup()
	acctGroup.SetTitle("Accounts")
	acctGroup.SetDescription("Give each account a name (e.g. Home, Work) — shown in the sidebar. Leave blank to use the email.")
	for _, a := range w.deps.Accounts {
		r := adw.NewEntryRow()
		r.SetTitle(a.Email)
		r.SetText(w.accountNames[a.Email])
		acctGroup.Add(r)
		nameRows[a.Email] = r
	}

	// Default signature, appended to composed messages.
	sigView := gtk.NewTextView()
	sigView.SetWrapMode(gtk.WrapWordChar)
	sigView.SetTopMargin(6)
	sigView.SetBottomMargin(6)
	sigView.SetLeftMargin(8)
	sigView.SetRightMargin(8)
	sigView.Buffer().SetText(w.signature)
	sigScroll := gtk.NewScrolledWindow()
	sigScroll.SetMinContentHeight(90)
	sigScroll.SetChild(sigView)
	sigScroll.AddCSSClass("card")
	sigGroup := adw.NewPreferencesGroup()
	sigGroup.SetTitle("Signature")
	sigGroup.SetDescription("Appended to new messages and replies (below your text, above any quote). Leave blank for none.")
	sigGroup.Add(sigScroll)

	// Privacy: a global default for loading remote images (tracking pixels are
	// always stripped regardless).
	imgRow := adw.NewSwitchRow()
	imgRow.SetTitle("Load remote images")
	imgRow.SetSubtitle("Tracking pixels are always blocked. Turn off to block all remote images by default.")
	imgRow.SetActive(!w.blockImages)
	imgRow.Connect("notify::active", func() {
		load := imgRow.Active()
		w.blockImages = !load
		if err := config.SavePrefs(config.Prefs{BlockRemoteImages: !load}); err != nil {
			slog.Warn("ui: save prefs", "err", err)
		}
		w.setImagesEnabled(load)
	})
	privacyGroup := adw.NewPreferencesGroup()
	privacyGroup.SetTitle("Privacy")
	privacyGroup.Add(imgRow)

	// Storage: clear the (re-downloadable) attachment cache.
	clearRow := adw.NewActionRow()
	clearRow.SetTitle("Cached attachments")
	clearRow.SetSubtitle("Downloaded attachments are kept on disk for quick reopening.")
	clearBtn := gtk.NewButtonWithLabel("Clear")
	clearBtn.SetVAlign(gtk.AlignCenter)
	clearBtn.ConnectClicked(func() {
		freed, err := config.ClearAttachmentsCache()
		if err != nil {
			slog.Warn("ui: clear attachments cache", "err", err)
			clearRow.SetSubtitle("Couldn't clear the cache.")
			return
		}
		clearRow.SetSubtitle(fmt.Sprintf("Cleared — freed %s.", humanBytes(freed)))
		clearBtn.SetSensitive(false)
	})
	clearRow.AddSuffix(clearBtn)
	storageGroup := adw.NewPreferencesGroup()
	storageGroup.SetTitle("Storage")
	storageGroup.Add(clearRow)

	scGroup := adw.NewPreferencesGroup()
	scGroup.SetTitle("Keyboard Shortcuts")
	scGroup.SetDescription("Single keys work while reading; they're ignored while typing in a field.")
	for _, s := range shortcutList() {
		row := adw.NewActionRow()
		row.SetTitle(s[1])
		key := gtk.NewLabel(s[0])
		key.AddCSSClass("dim-label")
		key.AddCSSClass("numeric")
		row.AddSuffix(key)
		scGroup.Add(row)
	}

	page := adw.NewPreferencesPage()
	page.Add(group)
	if len(w.deps.Accounts) > 0 {
		page.Add(acctGroup)
	}
	page.Add(sigGroup)
	page.Add(privacyGroup)
	page.Add(storageGroup)
	page.Add(scGroup)

	dialog := adw.NewPreferencesDialog()
	dialog.SetContentWidth(520)
	dialog.SetContentHeight(360)
	dialog.Add(page)
	dialog.ConnectClosed(func() {
		if w.deps.SaveAISettings != nil {
			if err := w.deps.SaveAISettings(providerRow.Text(), endpointRow.Text(), modelRow.Text()); err != nil {
				slog.Warn("ui: save settings", "err", err)
			}
		}
		w.applyAccountNames(nameRows)
		if newSig := strings.TrimSpace(bodyText(sigView.Buffer())); newSig != w.signature {
			if err := config.SaveSignature(newSig); err != nil {
				slog.Warn("ui: save signature", "err", err)
			} else {
				w.signature = newSig
			}
		}
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
