package ui

import (
	"log/slog"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
)

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

	page := adw.NewPreferencesPage()
	page.Add(group)

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
