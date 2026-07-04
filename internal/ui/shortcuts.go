package ui

import (
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"github.com/jsnjack/mailbox/internal/config"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// shortcutDef is one customizable single-key action: its stable id (the
// shortcuts.json key), the label shown in Preferences and the cheat sheet, the
// default keys (each rune an alias), and what it runs.
type shortcutDef struct {
	id          string
	label       string
	defaultKeys string
	run         func(*window)
}

// shortcutDefs is the customizable-action table, in the order Preferences and
// the cheat sheet list them. Ctrl-chords, Escape, Delete, and "?" stay fixed.
var shortcutDefs = []shortcutDef{
	{"next", "Next conversation", "j", func(w *window) { w.selectAdjacent(1) }},
	{"prev", "Previous conversation", "k", func(w *window) { w.selectAdjacent(-1) }},
	{"reply", "Reply", "r", (*window).onReply},
	{"forward", "Forward", "f", (*window).onForward},
	{"archive", "Archive", "ae", (*window).onArchive},
	{"trash", "Move to Trash", "#", (*window).onTrash},
	{"spam", "Report spam / Not spam", "!", func(w *window) {
		if w.current == model.LabelSpam {
			w.onNotSpam()
		} else {
			w.onReportSpam()
		}
	}},
	{"star", "Star / unstar", "s", (*window).toggleStar},
	{"unread", "Mark unread", "u", (*window).onMarkUnread},
	{"translate", "Translate", "t", (*window).onTranslate},
	{"compose", "New message", "c", func(w *window) {
		if w.deps.Send != nil && len(w.deps.Accounts) > 0 {
			w.openCompose(model.OutgoingMessage{}, "", "New message")
		}
	}},
	{"search", "Focus search", "/", func(w *window) { w.searchEntry.GrabFocus() }},
}

// sanitizeKeys reduces a user-entered binding to at most three distinct
// printable ASCII keys (the keyval-equals-rune range the handler matches).
func sanitizeKeys(s string) string {
	var out []rune
	for _, r := range strings.ToLower(s) {
		if r <= ' ' || r > '~' || strings.ContainsRune(string(out), r) {
			continue
		}
		out = append(out, r)
		if len(out) == 3 {
			break
		}
	}
	return string(out)
}

// effectiveKeys returns the keys bound to an action: the user override when
// shortcuts.json has one (possibly "", meaning disabled), else the default.
func effectiveKeys(overrides map[string]string, def shortcutDef) string {
	if v, ok := overrides[def.id]; ok {
		return sanitizeKeys(v)
	}
	return def.defaultKeys
}

// rebuildKeymap compiles the single-key shortcut table (user overrides over
// defaults) into the keyval→action map the key handler consults. On a key
// conflict the first action in shortcutDefs order wins, so a bad override can
// never silently shadow everything.
func (w *window) rebuildKeymap() {
	overrides, _ := config.LoadShortcuts()
	w.keymap = make(map[uint]func(), 16)
	for _, def := range shortcutDefs {
		def := def
		for _, r := range effectiveKeys(overrides, def) {
			kv := uint(r)
			if _, taken := w.keymap[kv]; taken {
				logging.Trace("ui: shortcut key conflict", "key", string(r), "action", def.id)
				continue
			}
			w.keymap[kv] = func() { def.run(w) }
		}
	}
	logging.Trace("ui: keymap rebuilt", "keys", len(w.keymap))
}

// showShortcuts presents a keyboard-shortcuts cheat sheet (the single-key
// actions are otherwise undiscoverable). Bound to "?".
func (w *window) showShortcuts() {
	logging.Trace("ui: show shortcuts dialog")
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
	overrides, _ := config.LoadShortcuts()
	keysFor := func(id string) string {
		for _, def := range shortcutDefs {
			if def.id != id {
				continue
			}
			keys := effectiveKeys(overrides, def)
			if keys == "" {
				return "—"
			}
			parts := make([]string, 0, len(keys))
			for _, r := range keys {
				parts = append(parts, string(r))
			}
			return strings.Join(parts, "  /  ")
		}
		return ""
	}
	group("Navigation", [][2]string{
		{keysFor("next") + "  /  " + keysFor("prev"), "Next / previous conversation"},
		{keysFor("search") + "  /  Ctrl F", "Search"},
		{"Esc", "Back / exit selection"},
	})
	group("Message", [][2]string{
		{keysFor("reply"), "Reply"},
		{keysFor("forward"), "Forward"},
		{keysFor("archive"), "Archive"},
		{keysFor("trash") + "  /  Del", "Move to Trash"},
		{keysFor("star"), "Star"},
		{keysFor("unread"), "Mark unread"},
		{keysFor("spam"), "Report spam / Not spam"},
		{keysFor("translate"), "Translate"},
	})
	group("Compose", [][2]string{
		{keysFor("compose") + "  /  Ctrl N", "New message"},
		{"Ctrl + Enter", "Send"},
	})
	group("View", [][2]string{
		{"Ctrl +  /  −", "Zoom in / out"},
		{"Ctrl 0", "Reset zoom"},
		{"?  /  Ctrl ?", "This shortcuts list"},
	})
	group("Application", [][2]string{
		{"Ctrl ,", "Preferences"},
		{"Ctrl W", "Close window"},
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
