package ui

import (
	"context"

	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	glib "github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/model"
)

// showRowMenu opens a right-click context menu for a thread row at (x,y) within
// row, with quick label actions that operate on that thread without opening it
// or entering selection mode. It is a native GMenu model whose items target the
// win.row-* actions (registered in registerListActions), carrying the thread id.
func (w *window) showRowMenu(row gtk.Widgetter, threadID string, x, y float64) {
	t, ok := w.threadByID[threadID]
	if !ok || w.deps.ModifyLabels == nil {
		return
	}
	tv := glib.NewVariantString(threadID)
	menu := gio.NewMenu()

	move := gio.NewMenu()
	if w.current == model.LabelTrash || w.current == model.LabelSpam {
		move.AppendItem(rowItem("Move to Inbox", "win.row-move-inbox", tv))
	} else {
		move.AppendItem(rowItem("Archive", "win.row-archive", tv))
	}
	menu.AppendSection("", move)

	flags := gio.NewMenu()
	if t.Latest.IsStarred {
		flags.AppendItem(rowItem("Unstar", "win.row-unstar", tv))
	} else {
		flags.AppendItem(rowItem("Star", "win.row-star", tv))
	}
	if t.UnreadCount > 0 {
		flags.AppendItem(rowItem("Mark as read", "win.row-mark-read", tv))
	} else {
		flags.AppendItem(rowItem("Mark as unread", "win.row-mark-unread", tv))
	}
	menu.AppendSection("", flags)

	// Categorize this one conversation — where categories apply (the inbox, with
	// the toggle on). Manual "Categorize as" works without the AI (a fallback when
	// the provider is down); "Re-categorize with AI" needs an assistant.
	if w.inboxCategories && w.current == model.LabelInbox {
		cat := gio.NewMenu()
		choices := gio.NewMenu()
		for _, c := range ai.EmailCategories {
			choices.AppendItem(rowItem(c, "win.row-setcat", glib.NewVariantString(threadID+"\x1f"+c)))
		}
		choices.AppendItem(rowItem("None", "win.row-setcat", glib.NewVariantString(threadID+"\x1f")))
		cat.AppendSubmenu("Categorize as", choices)
		if w.deps.Assistant != nil {
			cat.AppendItem(rowItem("Re-categorize with AI", "win.row-recategorize", tv))
		}
		menu.AppendSection("", cat)
	}

	del := gio.NewMenu()
	del.AppendItem(rowItem("Move to Trash", "win.row-trash", tv))
	menu.AppendSection("", del)

	pop := gtk.NewPopoverMenuFromModel(menu)
	pop.SetParent(row)
	pop.SetHasArrow(false)
	rect := gdk.NewRectangle(int(x), int(y), 1, 1)
	pop.SetPointingTo(&rect)
	// Detach from the row when dismissed so the recycled row isn't left parenting
	// a stale popover.
	pop.ConnectClosed(func() { pop.Unparent() })
	pop.Popup()
}

// rowItem builds a menu item bound to a win.row-* action carrying the thread id.
func rowItem(label, action string, target *glib.Variant) *gio.MenuItem {
	item := gio.NewMenuItem(label, "")
	item.SetActionAndTargetValue(action, target)
	return item
}

// threadModifyAll applies a label delta to every message in a thread (loaded
// from the store), then shows an undo toast (reversing the change) when verb is
// non-empty — so a right-click archive/trash is as recoverable as the reader's.
func (w *window) threadModifyAll(threadID, verb string, add, remove []string) {
	msgs, err := w.deps.Store.ListThreadMessages(context.Background(), w.activeID, threadID)
	if err != nil || len(msgs) == 0 {
		return
	}
	w.applyLabels(msgs, add, remove, nil)
	if verb != "" {
		w.showUndoToast(verb, msgs, add, remove)
	}
}
