package ui

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
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
// saved when the window is closed and apply to the running app immediately.
func (w *window) openSettings() {
	if w.deps.AISettings == nil {
		logging.Trace("ui: open settings skipped", "reason", "no AI settings")
		return
	}
	seeded := w.deps.AISettings()
	logging.Trace("ui: open settings", "aiModels", len(seeded),
		"accounts", len(w.deps.Accounts), "categorize", w.inboxCategories, "block_images", w.blockImages)

	group := adw.NewPreferencesGroup()
	group.SetTitle("AI")
	desc := "Models are tried in order: the first is the primary, the next takes over while it is unreachable. Changes apply immediately."
	if w.deps.Assistant == nil {
		desc += " Enabling AI for the first time takes effect after a restart."
	}
	group.SetDescription(desc)

	// The chain editor: one expander per model, in priority order. Reordering
	// re-adds the rows (PreferencesGroup has no move API), so each expander is
	// kept in entryRows alongside its field widgets.
	type aiEntryUI struct {
		row      *adw.ExpanderRow
		model    *adw.EntryRow
		provider *adw.EntryRow
		endpoint *adw.EntryRow
		key      *adw.PasswordEntryRow
		up       *gtk.Button
	}
	var entryRows []*aiEntryUI
	var inGroup []*adw.ExpanderRow // rows currently added to the group
	relayout := func() {
		for _, r := range inGroup {
			group.Remove(r)
		}
		inGroup = inGroup[:0]
		for i, u := range entryRows {
			group.Add(u.row)
			inGroup = append(inGroup, u.row)
			u.up.SetVisible(i > 0)
		}
	}
	entryTitle := func(u *aiEntryUI) {
		title := strings.TrimSpace(u.model.Text())
		if title == "" {
			title = "New model"
		}
		u.row.SetTitle(title)
		u.row.SetSubtitle(strings.TrimSpace(u.endpoint.Text()))
	}
	addEntry := func(e AIModelEntry, expand bool) {
		u := &aiEntryUI{row: adw.NewExpanderRow()}
		u.model = adw.NewEntryRow()
		u.model.SetTitle("Model")
		u.model.SetText(e.Model)
		u.provider = adw.NewEntryRow()
		u.provider.SetTitle("Provider (openai / litellm / anthropic)")
		u.provider.SetText(e.Provider)
		u.endpoint = adw.NewEntryRow()
		u.endpoint.SetTitle("Endpoint (base URL incl. /v1)")
		u.endpoint.SetText(e.Endpoint)
		u.key = adw.NewPasswordEntryRow()
		u.key.SetTitle("API key (stored in the system keyring)")
		u.key.SetText(e.Key)
		u.row.AddRow(u.model)
		u.row.AddRow(u.provider)
		u.row.AddRow(u.endpoint)
		u.row.AddRow(u.key)

		// Each row tests its own settings with a tiny live request — a single
		// shared button couldn't say which chain entry a result belongs to. The
		// verdict shows on the button and resets when the row is edited.
		resetTest := func() {}
		if w.deps.TestAISettings != nil {
			const testTip = "Check this model with a tiny live request"
			test := gtk.NewButtonWithLabel("Test")
			test.SetTooltipText(testTip)
			test.SetVAlign(gtk.AlignCenter)
			resetTest = func() {
				test.SetLabel("Test")
				test.RemoveCSSClass("success")
				test.RemoveCSSClass("error")
				test.SetTooltipText(testTip)
			}
			test.ConnectClicked(func() {
				e := AIModelEntry{
					Provider: strings.TrimSpace(u.provider.Text()),
					Endpoint: strings.TrimSpace(u.endpoint.Text()),
					Model:    strings.TrimSpace(u.model.Text()),
					Key:      u.key.Text(),
				}
				logging.Trace("ui: settings test AI model", "model", e.Model, "endpoint", e.Endpoint)
				test.SetSensitive(false)
				test.SetLabel("Testing…")
				test.RemoveCSSClass("success")
				test.RemoveCSSClass("error")
				test.SetTooltipText(testTip)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					err := w.deps.TestAISettings(ctx, []AIModelEntry{e})
					dispatch.Main(func() {
						test.SetSensitive(true)
						if err != nil {
							logging.Trace("ui: settings test AI model failed", "model", e.Model, "err", err)
							test.SetLabel("Failed")
							test.AddCSSClass("error")
							test.SetTooltipText(err.Error())
						} else {
							logging.Trace("ui: settings test AI model ok", "model", e.Model)
							test.SetLabel("Connected ✓")
							test.AddCSSClass("success")
						}
					})
				}()
			})
			u.row.AddSuffix(test)
		}
		u.model.Connect("changed", func() { entryTitle(u); resetTest() })
		u.provider.Connect("changed", resetTest)
		u.endpoint.Connect("changed", func() { entryTitle(u); resetTest() })
		u.key.Connect("changed", resetTest)
		entryTitle(u)

		u.up = gtk.NewButtonFromIconName("go-up-symbolic")
		u.up.SetTooltipText("Try earlier (higher priority)")
		u.up.AddCSSClass("flat")
		u.up.SetVAlign(gtk.AlignCenter)
		u.up.ConnectClicked(func() {
			for i, cand := range entryRows {
				if cand == u && i > 0 {
					entryRows[i-1], entryRows[i] = entryRows[i], entryRows[i-1]
					break
				}
			}
			relayout()
		})
		rm := gtk.NewButtonFromIconName("user-trash-symbolic")
		rm.SetTooltipText("Remove model")
		rm.AddCSSClass("flat")
		rm.SetVAlign(gtk.AlignCenter)
		rm.ConnectClicked(func() {
			for i, cand := range entryRows {
				if cand == u {
					entryRows = append(entryRows[:i], entryRows[i+1:]...)
					break
				}
			}
			relayout()
		})
		u.row.AddSuffix(u.up)
		u.row.AddSuffix(rm)

		entryRows = append(entryRows, u)
		relayout()
		if expand {
			u.row.SetExpanded(true)
		}
	}
	for _, e := range seeded {
		addEntry(e, false)
	}

	// collectAIEntries reads the editor state in priority order, skipping rows
	// left entirely blank (an added-then-abandoned entry).
	collectAIEntries := func() []AIModelEntry {
		var out []AIModelEntry
		for _, u := range entryRows {
			e := AIModelEntry{
				Provider: strings.TrimSpace(u.provider.Text()),
				Endpoint: strings.TrimSpace(u.endpoint.Text()),
				Model:    strings.TrimSpace(u.model.Text()),
				Key:      u.key.Text(),
			}
			if e.Provider == "" && e.Endpoint == "" && e.Model == "" {
				continue
			}
			out = append(out, e)
		}
		return out
	}

	// Header button: add a model (pre-filled from the last entry, so a fallback
	// on the same proxy only needs its model name). Connection testing lives on
	// each row, not here — per model, never ambiguous about which one it tested.
	headerBox := gtk.NewBox(gtk.OrientationHorizontal, 6)
	addBtn := gtk.NewButtonWithLabel("Add model")
	addBtn.SetVAlign(gtk.AlignCenter)
	addBtn.ConnectClicked(func() {
		e := AIModelEntry{}
		if n := len(entryRows); n > 0 {
			last := entryRows[n-1]
			e.Provider = strings.TrimSpace(last.provider.Text())
			e.Endpoint = strings.TrimSpace(last.endpoint.Text())
			e.Key = last.key.Text()
		}
		logging.Trace("ui: settings add AI model", "entries", len(entryRows)+1)
		addEntry(e, true)
	})
	headerBox.Append(addBtn)
	group.SetHeaderSuffix(headerBox)

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

	// AI Features: every AI-powered feature gets its own on/off switch, each
	// hiding the button/menu item it drives immediately (see the
	// w.refreshAIVisibility calls below and each feature's own gate at its
	// UI call site). All of them send message content to the configured AI
	// provider, so the subtitles say so.
	var aiFeaturesGroup *adw.PreferencesGroup
	if w.deps.Assistant != nil {
		aiFeaturesGroup = adw.NewPreferencesGroup()
		aiFeaturesGroup.SetTitle("AI Features")
		aiFeaturesGroup.SetDescription("Each feature sends related message content to the AI provider. Turning one off also hides its button or menu item.")

		aiToggle := func(title, subtitle string, active bool, onChange func(on bool)) {
			row := adw.NewSwitchRow()
			row.SetTitle(title)
			row.SetSubtitle(subtitle)
			row.SetActive(active)
			row.Connect("notify::active", func() {
				on := row.Active()
				logging.Trace("ui: setting changed", "pref", title, "new", on)
				onChange(on)
			})
			aiFeaturesGroup.Add(row)
		}

		aiToggle("Categorize inbox with AI", "Tags inbox mail (Needs reply, Receipt, …).",
			w.inboxCategories, func(on bool) {
				w.inboxCategories = on
				savePref(func(p *config.Prefs) { p.DisableInboxCategories = !on })
				if on {
					w.categorizeInbox() // seed what's already cached
					if w.deps.RecategorizeInbox != nil {
						w.deps.RecategorizeInbox(0) // classify the rest, all accounts
					}
				}
			})
		aiToggle("Message summaries", "A one-line AI gist shown on messages and in new-mail notifications.",
			w.aiGist, func(on bool) {
				w.aiGist = on
				savePref(func(p *config.Prefs) { p.DisableGist = !on })
			})
		aiToggle("AI draft replies", "The \"AI draft\" button in compose and the reader's AI-reply popover.",
			w.aiDraft, func(on bool) {
				w.aiDraft = on
				savePref(func(p *config.Prefs) { p.DisableAIDraft = !on })
				w.refreshAIVisibility()
			})
		aiToggle("Smart quick replies", "One-tap suggested replies in the reader and compose.",
			w.aiSmartReplies, func(on bool) {
				w.aiSmartReplies = on
				savePref(func(p *config.Prefs) { p.DisableSmartReplies = !on })
				w.refreshAIVisibility()
			})
		aiToggle("Grammar and spelling check", "The grammar-check button in compose.",
			w.aiProofread, func(on bool) {
				w.aiProofread = on
				savePref(func(p *config.Prefs) { p.DisableProofread = !on })
			})
		aiToggle("Refine with AI", "The compose button that rewrites your text (or a selection) to an instruction.",
			w.aiRefine, func(on bool) {
				w.aiRefine = on
				savePref(func(p *config.Prefs) { p.DisableRefine = !on })
			})
		aiToggle("Subject suggestions", "The sparkle button next to Subject in compose.",
			w.aiGenerateSubject, func(on bool) {
				w.aiGenerateSubject = on
				savePref(func(p *config.Prefs) { p.DisableGenerateSubject = !on })
			})
		aiToggle("Thread summaries", "The \"Summarize thread\" button in the reader.",
			w.aiSummarize, func(on bool) {
				w.aiSummarize = on
				savePref(func(p *config.Prefs) { p.DisableSummarize = !on })
				w.refreshAIVisibility()
			})
		aiToggle("Translate", "The \"Translate to English\" button in the reader.",
			w.aiTranslate, func(on bool) {
				w.aiTranslate = on
				savePref(func(p *config.Prefs) { p.DisableTranslate = !on })
				w.refreshAIVisibility()
			})
		aiToggle("Phishing analysis", "The reader overflow's \"Check for phishing\".",
			w.aiPhishing, func(on bool) {
				w.aiPhishing = on
				savePref(func(p *config.Prefs) { p.DisablePhishingAnalysis = !on })
			})
		aiToggle("Snooze suggestions", "AI-suggested wake times in the Snooze flyout and dialog.",
			w.aiSnoozeSuggestions, func(on bool) {
				w.aiSnoozeSuggestions = on
				savePref(func(p *config.Prefs) { p.DisableSnoozeSuggestions = !on })
			})
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

	// Storage: body retention — prune cached bodies of old mail (metadata and
	// header search stay; a pruned message re-fetches its body on open). The
	// options map to days; index 0 keeps everything forever (the default).
	// The explanation lives in the group description (full-width), not a row
	// subtitle — a long subtitle's natural width squeezes the combo dropdown
	// until its selected value ellipsizes.
	retentionDays := []int{0, 30, 91, 182, 365, 2 * 365, 5 * 365}
	retentionRow := adw.NewComboRow()
	retentionRow.SetTitle("Keep message bodies")
	retentionRow.SetModel(gtk.NewStringList([]string{"Forever", "1 month", "3 months", "6 months", "1 year", "2 years", "5 years"}))
	prefs, _ := config.LoadPrefs()
	for i, d := range retentionDays {
		if d == prefs.BodyRetentionDays {
			retentionRow.SetSelected(uint(i))
		}
	}
	retentionRow.Connect("notify::selected", func() {
		sel := int(retentionRow.Selected())
		if sel < 0 || sel >= len(retentionDays) {
			return
		}
		days := retentionDays[sel]
		logging.Trace("ui: setting changed", "pref", "body_retention_days", "new", days)
		savePref(func(p *config.Prefs) { p.BodyRetentionDays = days })
		if days > 0 {
			// Apply right away (the daily background pass also picks it up):
			// prune in the background so the dialog stays responsive.
			cutoff := time.Now().AddDate(0, 0, -days).Unix()
			go func() {
				n, err := w.deps.Store.PruneBodies(context.Background(), cutoff)
				if err != nil {
					slog.Warn("ui: prune bodies", "err", err)
					return
				}
				logging.Trace("ui: prune bodies done", "count", n, "days", days)
			}()
		}
	})

	// Sending: how long the Undo toast holds a message before it goes out.
	undoSecs := []int{5, 10, 20, 30}
	undoRow := adw.NewComboRow()
	undoRow.SetTitle("Undo send window")
	undoRow.SetSubtitle("How long a sent message can still be taken back.")
	undoRow.SetModel(gtk.NewStringList([]string{"5 seconds", "10 seconds", "20 seconds", "30 seconds"}))
	for i, v := range undoSecs {
		if v == w.sendUndoSecs {
			undoRow.SetSelected(uint(i))
		}
	}
	undoRow.Connect("notify::selected", func() {
		sel := int(undoRow.Selected())
		if sel < 0 || sel >= len(undoSecs) {
			return
		}
		secs := undoSecs[sel]
		logging.Trace("ui: setting changed", "pref", "send_undo_seconds", "new", secs)
		w.sendUndoSecs = secs
		savePref(func(p *config.Prefs) { p.SendUndoSeconds = secs })
	})
	sendGroup := adw.NewPreferencesGroup()
	sendGroup.SetTitle("Sending")
	sendGroup.Add(undoRow)

	storageGroup := adw.NewPreferencesGroup()
	storageGroup.SetTitle("Storage")
	storageGroup.SetDescription("Bodies older than the retention window are removed from the cache and re-downloaded when opened. Headers and search by sender or subject always stay.")
	storageGroup.Add(retentionRow)
	storageGroup.Add(clearRow)
	storageGroup.Add(dbRow)

	page := adw.NewPreferencesPage()
	page.Add(group)
	if len(w.deps.Accounts) > 0 {
		page.Add(acctGroup)
	}
	page.Add(sigGroup)
	page.Add(privacyGroup)
	if aiFeaturesGroup != nil {
		page.Add(aiFeaturesGroup)
	}
	page.Add(sendGroup)
	page.Add(storageGroup)

	dialog := adw.NewPreferencesDialog()
	dialog.SetContentWidth(720)
	dialog.SetContentHeight(760)
	dialog.Add(page)
	dialog.ConnectClosed(func() {
		logging.Trace("ui: settings dialog closed, saving")
		if w.deps.SaveAISettings != nil {
			// Save only when something changed: a no-op save would still rebuild
			// and swap the live provider, resetting the failover chain's
			// committed entry and breaker state (so the status bar would claim
			// the primary serves when a fallback does), and re-write the keyring.
			entries := collectAIEntries()
			if slices.Equal(entries, seeded) {
				logging.Trace("ui: AI settings unchanged, save skipped")
			} else {
				logging.Trace("ui: save AI settings", "entries", len(entries))
				if err := w.deps.SaveAISettings(entries); err != nil {
					slog.Warn("ui: save settings", "err", err)
				}
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
