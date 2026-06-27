package ui

import (
	"context"

	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/model"
)

// showRowMenu opens a right-click context menu for a thread row at (x,y) within
// row, with quick label actions that operate on that thread without opening it
// or entering selection mode.
func (w *window) showRowMenu(row gtk.Widgetter, threadID string, x, y float64) {
	t, ok := w.threadByID[threadID]
	if !ok || w.deps.ModifyLabels == nil {
		return
	}

	pop := gtk.NewPopover()
	pop.SetParent(row)
	pop.SetHasArrow(false)
	rect := gdk.NewRectangle(int(x), int(y), 1, 1)
	pop.SetPointingTo(&rect)
	// Detach from the row when dismissed so the recycled row isn't left parenting
	// a stale popover.
	pop.ConnectClosed(func() { pop.Unparent() })

	box := gtk.NewBox(gtk.OrientationVertical, 2)
	setMargins(box, 6, 6, 6, 6)
	box.SetSizeRequest(180, -1)
	add := func(label string, fn func()) { box.Append(menuItemButton(pop, label, fn)) }

	if w.current == model.LabelTrash || w.current == model.LabelSpam {
		add("Move to Inbox", func() {
			w.threadModifyAll(threadID, "Moved to Inbox", []string{model.LabelInbox}, []string{model.LabelTrash, model.LabelSpam})
		})
	} else {
		add("Archive", func() {
			w.threadModifyAll(threadID, "Archived", nil, []string{model.LabelInbox})
		})
	}
	if t.Latest.IsStarred {
		add("Unstar", func() { w.applyLabels([]model.Message{t.Latest}, nil, []string{model.LabelStarred}, nil) })
	} else {
		add("Star", func() { w.applyLabels([]model.Message{t.Latest}, []string{model.LabelStarred}, nil, nil) })
	}
	if t.UnreadCount > 0 {
		add("Mark as read", func() { w.threadModifyAll(threadID, "Marked as read", nil, []string{model.LabelUnread}) })
	} else {
		add("Mark as unread", func() { w.applyLabels([]model.Message{t.Latest}, []string{model.LabelUnread}, nil, nil) })
	}
	add("Move to Trash", func() {
		w.threadModifyAll(threadID, "Moved to Trash", []string{model.LabelTrash}, []string{model.LabelInbox})
	})

	pop.SetChild(box)
	pop.Popup()
}

// threadModifyAll applies a label delta to every message in a thread (loaded
// from the store), then toasts verb when non-empty.
func (w *window) threadModifyAll(threadID, verb string, add, remove []string) {
	msgs, err := w.deps.Store.ListThreadMessages(context.Background(), w.activeID, threadID)
	if err != nil || len(msgs) == 0 {
		return
	}
	w.applyLabels(msgs, add, remove, nil)
	if verb != "" {
		w.toast(verb)
	}
}
