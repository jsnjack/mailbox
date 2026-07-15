package ui

import (
	"log/slog"
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
	{"reply", "Reply all", "r", (*window).onReplyAll},
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

// showShortcuts presents the keyboard-shortcuts dialog — cheat sheet and
// editor in one (Preferences deliberately has no second copy). The single-key
// actions are editable in place; chords and structural keys are fixed.
func (w *window) showShortcuts() {
	logging.Trace("ui: show shortcuts dialog")
	box := gtk.NewBox(gtk.OrientationVertical, 18)
	setMargins(box, 18, 18, 14, 18)

	hint := gtk.NewLabel("Click a key field to rebind (several characters act as aliases, e.g. \"ae\"; empty disables). Press Enter to apply.")
	hint.SetXAlign(0)
	hint.SetWrap(true)
	hint.AddCSSClass("dim-label")
	hint.AddCSSClass("caption")
	box.Append(hint)

	overrides, _ := config.LoadShortcuts()
	defByID := map[string]shortcutDef{}
	for _, def := range shortcutDefs {
		defByID[def.id] = def
	}

	var grid *gtk.Grid
	row := 0
	newGroup := func(title string) {
		h := gtk.NewLabel(title)
		h.SetXAlign(0)
		h.AddCSSClass("heading")
		box.Append(h)
		grid = gtk.NewGrid()
		grid.SetRowSpacing(6)
		grid.SetColumnSpacing(20)
		box.Append(grid)
		row = 0
	}
	fixed := func(keys, label string) {
		k := gtk.NewLabel(keys)
		k.SetXAlign(1)
		k.SetHAlign(gtk.AlignEnd)
		k.AddCSSClass("dim-label")
		l := gtk.NewLabel(label)
		l.SetXAlign(0)
		l.SetHExpand(true)
		grid.Attach(k, 0, row, 1, 1)
		grid.Attach(l, 1, row, 1, 1)
		row++
	}
	// editable puts the action's keys in a small entry; Enter saves the
	// override and rebuilds the live keymap immediately. suffix names any
	// fixed alias (Del, Ctrl F, …) that always applies on top.
	editable := func(id, suffix string) {
		def := defByID[id]
		e := gtk.NewEntry()
		e.SetText(effectiveKeys(overrides, def))
		e.SetWidthChars(5)
		e.SetMaxWidthChars(5)
		e.SetAlignment(1)
		e.SetPlaceholderText("—")
		e.SetTooltipText("Type keys, press Enter")
		e.ConnectActivate(func() {
			keys := sanitizeKeys(e.Text())
			e.SetText(keys)
			m, _ := config.LoadShortcuts()
			m[def.id] = keys
			if err := config.SaveShortcuts(m); err != nil {
				slog.Warn("ui: save shortcuts", "err", err)
			}
			logging.Trace("ui: shortcut rebound", "action", def.id, "keys", keys)
			w.rebuildKeymap()
			w.toast("Shortcut updated")
		})
		grid.Attach(e, 0, row, 1, 1)
		label := def.label
		if suffix != "" {
			label += "  (also " + suffix + ")"
		}
		l := gtk.NewLabel(label)
		l.SetXAlign(0)
		l.SetHExpand(true)
		grid.Attach(l, 1, row, 1, 1)
		row++
	}

	newGroup("Navigation")
	editable("next", "")
	editable("prev", "")
	editable("search", "Ctrl F")
	fixed("Esc", "Back / exit selection")

	newGroup("Message")
	editable("reply", "")
	editable("forward", "")
	editable("archive", "")
	editable("trash", "Del")
	editable("star", "")
	editable("unread", "")
	editable("spam", "")
	editable("translate", "")

	newGroup("Compose")
	editable("compose", "Ctrl N")
	fixed("Ctrl + Enter", "Send")

	newGroup("View")
	fixed("Ctrl +  /  −", "Zoom in / out")
	fixed("Ctrl 0", "Reset zoom")
	fixed("?", "This shortcuts list")

	newGroup("Application")
	fixed("Ctrl ,", "Preferences")
	fixed("Ctrl W", "Close window")

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
