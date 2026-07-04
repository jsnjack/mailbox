package ui

import (
	"context"
	"time"

	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// showRowMenu opens a right-click context menu for a thread row at (x,y) within
// row, with quick label actions that operate on that thread without opening it
// or entering selection mode.
//
// It is hand-built from flat GtkButtons with direct "clicked" handlers rather
// than a GMenu model of win.*/row.* actions. A GtkPopoverMenu manually parented
// to a recycled GtkListView row does not activate its items' GActions — clicking
// only closes the popover, with no GTK warning, regardless of how the action is
// registered or targeted (verified in a sandbox). Direct clicked handlers on
// buttons owned by a plain GtkPopover sidestep the action machinery entirely.
func (w *window) showRowMenu(row gtk.Widgetter, threadID string, x, y float64) {
	t, ok := w.threadByID[threadID]
	if !ok || w.deps.ModifyLabels == nil {
		logging.Trace("ui: row menu skipped", "id", threadID, "found", ok, "can_modify", w.deps.ModifyLabels != nil)
		return
	}
	// Capture the account this row belongs to now, so an action still targets the
	// right account (and the right thread's messages) if the user switches
	// accounts while the menu is open — otherwise the action would query the newly
	// active account for a thread it doesn't have and silently do nothing.
	acct := w.activeID
	logging.Trace("ui: show row menu", "id", threadID, "account", acct, "label", w.current, "starred", t.Latest.IsStarred, "unread", t.UnreadCount)

	pop := gtk.NewPopover()
	pop.SetParent(row)
	pop.SetHasArrow(false)
	pop.SetPosition(gtk.PosBottom)
	rect := gdk.NewRectangle(int(x), int(y), 1, 1)
	pop.SetPointingTo(&rect)
	// Detach from the row when dismissed so the recycled row isn't left parenting
	// a stale popover.
	pop.ConnectClosed(func() {
		logging.Trace("ui: row menu closed", "id", threadID)
		pop.Unparent()
	})

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.AddCSSClass("menu")
	box.AddCSSClass("rowmenu")

	// item appends a flat, left-aligned button that pops the menu down and runs fn
	// (traced so the log shows exactly which row-menu item fired).
	item := func(parent *gtk.Box, label string, fn func()) {
		lbl := gtk.NewLabel(label)
		lbl.SetXAlign(0)
		lbl.SetHExpand(true)
		b := gtk.NewButton()
		b.SetChild(lbl)
		b.AddCSSClass("flat")
		b.ConnectClicked(func() {
			logging.Trace("ui: row menu action", "action", label, "id", threadID)
			pop.Popdown()
			fn()
		})
		parent.Append(b)
	}
	sep := func() { box.Append(gtk.NewSeparator(gtk.OrientationHorizontal)) }

	if w.current == model.LabelTrash || w.current == model.LabelSpam {
		item(box, "Move to Inbox", func() {
			w.threadModifyAll(acct, threadID, "Moved to Inbox", []string{model.LabelInbox}, []string{model.LabelTrash, model.LabelSpam})
		})
	} else {
		item(box, "Archive", func() { w.threadModifyAll(acct, threadID, "Archived", nil, []string{model.LabelInbox}) })
	}
	// File this conversation into a user label (Move to… relocates it out of the
	// current location; Label… toggles labels without moving it).
	item(box, "Move to…", func() {
		w.showMoveToDialog(acct, func(labelID, name string) {
			w.threadModifyAll(acct, threadID, "Moved to "+name, []string{labelID}, moveLocationRemovals)
		})
	})
	item(box, "Label…", func() { w.showThreadLabelsDialog(acct, threadID) })
	// Snooze: hide the conversation until a quick wake time (nested popover of
	// presets, like "Categorize as"); in the Snoozed view offer the reverse.
	if w.current == snoozedID {
		item(box, "Unsnooze", func() { w.unsnooze(acct, threadID) })
	} else {
		snPop := gtk.NewPopover()
		snPop.SetHasArrow(false)
		snPop.SetPosition(gtk.PosRight)
		snBox := gtk.NewBox(gtk.OrientationVertical, 0)
		snBox.AddCSSClass("menu")
		snBox.AddCSSClass("rowmenu")
		for _, p := range snoozePresets(time.Now()) {
			p := p
			item(snBox, p.label+" ("+p.t.Format("Mon 15:04")+")", func() {
				snPop.Popdown()
				w.snoozeUntil(acct, threadID, p.t)
			})
		}
		item(snBox, "Pick date…", func() {
			snPop.Popdown()
			w.openSnoozeDialog(acct, threadID)
		})
		snPop.SetChild(snBox)
		lbl := gtk.NewLabel("Snooze")
		lbl.SetXAlign(0)
		lbl.SetHExpand(true)
		snBtn := gtk.NewButton()
		snBtn.SetChild(lbl)
		snBtn.AddCSSClass("flat")
		snBtn.ConnectClicked(func() {
			snPop.SetParent(snBtn)
			snPop.Popup()
		})
		snPop.ConnectClosed(func() { snPop.Unparent() })
		box.Append(snBtn)
	}
	sep()

	if t.Latest.IsStarred {
		item(box, "Unstar", func() { w.threadModifyAll(acct, threadID, "", nil, []string{model.LabelStarred}) })
	} else {
		item(box, "Star", func() { w.threadModifyAll(acct, threadID, "", []string{model.LabelStarred}, nil) })
	}
	if t.UnreadCount > 0 {
		item(box, "Mark as read", func() { w.threadModifyAll(acct, threadID, "Marked as read", nil, []string{model.LabelUnread}) })
	} else {
		// Use the message captured when the menu opened (not a live map lookup),
		// so a mid-menu account switch can't turn this into a no-op.
		latest := t.Latest
		item(box, "Mark as unread", func() {
			w.applyLabels([]model.Message{latest}, []string{model.LabelUnread}, nil, nil)
		})
	}

	// Categorize this one conversation — where categories apply (the inbox, with
	// the toggle on). "Categorize as" opens a nested popover of choices (works
	// without the AI, a fallback when the provider is down); "Re-categorize with
	// AI" needs an assistant.
	if w.inboxCategories && w.current == model.LabelInbox {
		sep()
		catPop := gtk.NewPopover()
		catPop.SetHasArrow(false)
		catPop.SetPosition(gtk.PosRight)
		catBox := gtk.NewBox(gtk.OrientationVertical, 0)
		catBox.AddCSSClass("menu")
		catBox.AddCSSClass("rowmenu")
		setCat := func(label, cat string) {
			item(catBox, label, func() { catPop.Popdown(); w.setThreadCategory(threadID, cat) })
		}
		for _, c := range ai.EmailCategories {
			setCat(c, c)
		}
		setCat("None", "")
		catPop.SetChild(catBox)

		lbl := gtk.NewLabel("Categorize as")
		lbl.SetXAlign(0)
		lbl.SetHExpand(true)
		catBtn := gtk.NewButton()
		catBtn.SetChild(lbl)
		catBtn.AddCSSClass("flat")
		catBtn.ConnectClicked(func() {
			catPop.SetParent(catBtn)
			catPop.Popup()
		})
		catPop.ConnectClosed(func() { catPop.Unparent() })
		box.Append(catBtn)

		if w.deps.Assistant != nil {
			item(box, "Re-categorize with AI", func() { w.recategorizeThread(threadID) })
		}
		// When this thread carries a category the user set by hand, offer a
		// one-click way to drop it (reverting to the AI / "Replied" tag) rather
		// than burying it under Categorize as → None.
		if w.manualCat[threadID] {
			item(box, "Clear category", func() { w.setThreadCategory(threadID, "") })
		}
	}

	sep()
	item(box, "Move to Trash", func() {
		w.threadModifyAll(acct, threadID, "Moved to Trash", []string{model.LabelTrash}, []string{model.LabelInbox})
	})

	pop.SetChild(box)
	pop.Popup()
}

// threadModifyAll applies a label delta to every message in a thread (loaded
// from the store for acctID — captured when the row menu opened, so it stays
// correct across an account switch), then shows an undo toast (reversing the
// change) when verb is non-empty — so a right-click archive/trash is as
// recoverable as the reader's.
func (w *window) threadModifyAll(acctID int64, threadID, verb string, add, remove []string) {
	msgs, err := w.deps.Store.ListThreadMessages(context.Background(), acctID, threadID)
	if err != nil || len(msgs) == 0 {
		logging.Trace("ui: thread modify all skipped", "id", threadID, "account", acctID, "verb", verb, "n", len(msgs), "err", err)
		return
	}
	logging.Trace("ui: thread modify all", "id", threadID, "account", acctID, "verb", verb, "n", len(msgs), "add", add, "remove", remove)
	w.applyLabels(msgs, add, remove, nil)
	if verb != "" {
		w.showUndoToast(verb, msgs, add, remove)
	}
}
