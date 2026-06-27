package ui

import (
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// showShortcuts presents a keyboard-shortcuts cheat sheet (the single-key
// actions are otherwise undiscoverable). Bound to "?".
func (w *window) showShortcuts() {
	box := gtk.NewBox(gtk.OrientationVertical, 18)
	setMargins(box, 18, 18, 14, 18)

	group := func(title string, rows [][2]string) {
		h := gtk.NewLabel(title)
		h.SetXAlign(0)
		h.AddCSSClass("heading")
		box.Append(h)
		grid := gtk.NewGrid()
		grid.SetRowSpacing(6)
		grid.SetColumnSpacing(20)
		for i, r := range rows {
			key := gtk.NewLabel(r[0])
			key.SetXAlign(1)
			key.SetHAlign(gtk.AlignEnd)
			key.AddCSSClass("dim-label")
			act := gtk.NewLabel(r[1])
			act.SetXAlign(0)
			act.SetHExpand(true)
			grid.Attach(key, 0, i, 1, 1)
			grid.Attach(act, 1, i, 1, 1)
		}
		box.Append(grid)
	}
	group("Navigation", [][2]string{
		{"j  /  k", "Next / previous conversation"},
		{"/", "Search"},
		{"Esc", "Back"},
	})
	group("Message", [][2]string{
		{"r", "Reply"},
		{"f", "Forward"},
		{"a  /  e", "Archive"},
		{"#  /  Del", "Move to Trash"},
		{"s", "Star"},
		{"u", "Mark unread"},
		{"!", "Report spam / Not spam"},
		{"t", "Translate"},
	})
	group("Compose", [][2]string{
		{"c", "New message"},
		{"Ctrl + Enter", "Send"},
	})
	group("View", [][2]string{
		{"Ctrl +  /  −", "Zoom in / out"},
		{"Ctrl 0", "Reset zoom"},
		{"?  /  Ctrl ?", "This shortcuts list"},
	})

	scroller := gtk.NewScrolledWindow()
	scroller.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroller.SetChild(box)
	scroller.SetVExpand(true)

	tv := adw.NewToolbarView()
	tv.AddTopBar(adw.NewHeaderBar())
	tv.SetContent(scroller)

	dialog := adw.NewDialog()
	dialog.SetTitle("Keyboard Shortcuts")
	dialog.SetContentWidth(440)
	dialog.SetContentHeight(560)
	dialog.SetChild(tv)
	dialog.Present(w.win)
}
