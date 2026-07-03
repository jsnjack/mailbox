package ui

import (
	"context"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// userLabels returns the active account's user labels (system labels excluded),
// for the move/label pickers. Nil on error (already logged).
func (w *window) userLabels() []model.Label {
	labels, err := w.deps.Store.ListLabels(context.Background(), w.activeID)
	if err != nil {
		logging.Trace("ui: user labels", "account", w.activeID, "err", err)
		return nil
	}
	out := labels[:0:0]
	for _, l := range labels {
		if l.Type == model.LabelUser {
			out = append(out, l)
		}
	}
	return out
}

// showMoveToDialog presents the account's user labels; picking one calls onPick
// with that label's id and name. Filing (add label + remove the current
// location) is done by the caller so it can use the right batch path (a single
// thread vs. a bulk selection) and show the matching undo toast.
func (w *window) showMoveToDialog(onPick func(labelID, name string)) {
	labels := w.userLabels()
	logging.Trace("ui: move-to dialog", "account", w.activeID, "labels", len(labels))

	listBox := gtk.NewListBox()
	listBox.AddCSSClass("boxed-list")
	listBox.SetSelectionMode(gtk.SelectionNone)

	dialog := adw.NewDialog()

	if len(labels) == 0 {
		empty := gtk.NewLabel("No labels to move to.\nCreate labels in Gmail to file mail here.")
		empty.AddCSSClass("dim-label")
		empty.SetJustify(gtk.JustifyCenter)
		setMargins(empty, 18, 18, 18, 18)
		listBox.Append(empty)
	}
	for _, l := range labels {
		labelID, name := l.GmailID, l.Name
		row := gtk.NewButton()
		lbl := gtk.NewLabel(name)
		lbl.SetXAlign(0)
		lbl.SetHExpand(true)
		row.SetChild(lbl)
		row.AddCSSClass("flat")
		row.ConnectClicked(func() {
			logging.Trace("ui: move-to pick", "label", labelID, "name", name)
			dialog.Close()
			onPick(labelID, name)
		})
		listBox.Append(row)
	}

	scroller := gtk.NewScrolledWindow()
	scroller.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroller.SetChild(listBox)
	scroller.SetVExpand(true)
	setMargins(scroller, 6, 6, 6, 6)

	tv := adw.NewToolbarView()
	tv.AddTopBar(adw.NewHeaderBar())
	tv.SetContent(scroller)

	dialog.SetTitle("Move to")
	dialog.SetContentWidth(320)
	dialog.SetContentHeight(400)
	dialog.SetChild(tv)
	dialog.Present(w.win)
}

// labelToggleBox builds the user-label checklist for a thread: each user label
// is a checkbox reflecting whether it's applied to the thread, and toggling it
// adds/removes that label across all of the thread's messages.
func (w *window) labelToggleBox(threadID string, msgs []model.Message) gtk.Widgetter {
	box := gtk.NewBox(gtk.OrientationVertical, 2)
	setMargins(box, 8, 8, 8, 8)

	labels := w.userLabels()
	applied, err := w.deps.Store.ThreadLabels(context.Background(), w.activeID, threadID)
	if err != nil {
		logging.Trace("ui: thread labels", "id", threadID, "err", err)
		applied = map[string]bool{}
	}
	any := false
	for _, l := range labels {
		any = true
		labelID := l.GmailID
		cb := gtk.NewCheckButtonWithLabel(l.Name)
		cb.SetActive(applied[labelID]) // set before connecting so it doesn't self-fire
		cb.ConnectToggled(func() {
			if cb.Active() {
				w.applyLabels(msgs, []string{labelID}, nil, nil)
			} else {
				w.applyLabels(msgs, nil, []string{labelID}, nil)
			}
		})
		box.Append(cb)
	}
	if !any {
		box.Append(gtk.NewLabel("No labels"))
	}
	return box
}

// showThreadLabelsDialog opens the label-toggle checklist for a single thread
// (used from the row context menu), loading the thread's messages first.
func (w *window) showThreadLabelsDialog(acctID int64, threadID string) {
	msgs, err := w.deps.Store.ListThreadMessages(context.Background(), acctID, threadID)
	if err != nil || len(msgs) == 0 {
		logging.Trace("ui: thread labels dialog skipped", "id", threadID, "n", len(msgs), "err", err)
		return
	}
	scroller := gtk.NewScrolledWindow()
	scroller.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroller.SetChild(w.labelToggleBox(threadID, msgs))
	scroller.SetVExpand(true)

	tv := adw.NewToolbarView()
	tv.AddTopBar(adw.NewHeaderBar())
	tv.SetContent(scroller)

	dialog := adw.NewDialog()
	dialog.SetTitle("Labels")
	dialog.SetContentWidth(320)
	dialog.SetContentHeight(400)
	dialog.SetChild(tv)
	dialog.Present(w.win)
}

// moveLocationRemovals is the set of "location" labels a Move-to strips, so
// filing a thread into a label reliably relocates it out of Inbox/Trash/Spam.
var moveLocationRemovals = []string{model.LabelInbox, model.LabelTrash, model.LabelSpam}
