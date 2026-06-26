package ui

import (
	"log/slog"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// shortcutList is the single source of truth for the keyboard shortcuts, shown
// in the Preferences dialog.
func shortcutList() [][2]string {
	return [][2]string{
		{"j / k", "Next / previous conversation"},
		{"r", "Reply"},
		{"f", "Forward"},
		{"a / e", "Archive"},
		{"# / Delete", "Move to Trash"},
		{"s", "Star / unstar"},
		{"t", "Translate to English"},
		{"c", "Compose"},
		{"/", "Search"},
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
	})
	dialog.Present(w.win)
}
