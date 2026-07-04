package ui

import (
	"context"
	"fmt"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/logging"
)

// openSnoozeDialog offers every way to pick a wake time for a conversation:
// the quick presets, an AI suggestion read from the email itself (a deadline →
// the day before, an event → that morning), and a free calendar + time picker.
func (w *window) openSnoozeDialog(acctID int64, threadID string) {
	logging.Trace("ui: open snooze dialog", "account", acctID, "thread", threadID)

	dialog := adw.NewDialog()
	dialog.SetTitle("Snooze until…")
	dialog.SetContentWidth(360)

	apply := func(t time.Time) {
		dialog.Close()
		w.snoozeUntil(acctID, threadID, t)
	}

	box := gtk.NewBox(gtk.OrientationVertical, 10)
	setMargins(box, 14, 14, 12, 14)

	// Quick presets, same times as the row-menu flyout.
	for _, p := range snoozePresets(time.Now()) {
		p := p
		b := gtk.NewButtonWithLabel(p.label + " (" + p.t.Format("Mon 15:04") + ")")
		b.ConnectClicked(func() { apply(p.t) })
		box.Append(b)
	}

	// AI suggestion: read the conversation and propose a moment (a deadline →
	// the day before). Loads in the background; the row only appears when the
	// email actually implies a time.
	if w.deps.Assistant != nil {
		aiRow := gtk.NewButtonWithLabel("Reading the email…")
		aiRow.SetSensitive(false)
		box.Append(aiRow)
		go func() {
			t, reason, err := w.suggestSnoozeFor(acctID, threadID)
			dispatch.Main(func() {
				if err != nil || t.IsZero() {
					aiRow.SetVisible(false) // nothing useful — no dead row
					return
				}
				label := "✨ " + t.Format("Mon, Jan 2 15:04")
				if reason != "" {
					label += " — " + reason
				}
				aiRow.SetLabel(label)
				aiRow.SetSensitive(true)
				aiRow.ConnectClicked(func() { apply(t) })
			})
		}()
	}

	box.Append(gtk.NewSeparator(gtk.OrientationHorizontal))

	// Custom date + time.
	cal := gtk.NewCalendar()
	box.Append(cal)

	timeRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
	timeRow.SetHAlign(gtk.AlignCenter)
	hour := gtk.NewSpinButtonWithRange(0, 23, 1)
	hour.SetValue(9)
	hour.SetOrientation(gtk.OrientationVertical)
	minute := gtk.NewSpinButtonWithRange(0, 55, 5)
	minute.SetValue(0)
	minute.SetOrientation(gtk.OrientationVertical)
	timeRow.Append(hour)
	timeRow.Append(gtk.NewLabel(":"))
	timeRow.Append(minute)
	box.Append(timeRow)

	confirm := gtk.NewButtonWithLabel("Snooze")
	confirm.AddCSSClass("suggested-action")
	confirm.ConnectClicked(func() {
		d := cal.Date()
		t := time.Date(d.Year(), time.Month(d.Month()), d.DayOfMonth(),
			int(hour.Value()), int(minute.Value()), 0, 0, time.Local)
		if !t.After(time.Now()) {
			w.toast("Pick a time in the future")
			return
		}
		apply(t)
	})
	box.Append(confirm)

	scroller := gtk.NewScrolledWindow()
	scroller.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroller.SetChild(box)
	scroller.SetVExpand(true)

	tv := adw.NewToolbarView()
	tv.AddTopBar(adw.NewHeaderBar())
	tv.SetContent(scroller)
	dialog.SetChild(tv)
	dialog.Present(w.win)
}

// suggestSnoozeFor builds the newest message's context off the main thread and
// asks the AI for a wake time. Zero time = no usable suggestion.
func (w *window) suggestSnoozeFor(acctID int64, threadID string) (time.Time, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	msgs, err := w.deps.Store.ListThreadMessages(ctx, acctID, threadID)
	if err != nil || len(msgs) == 0 {
		return time.Time{}, "", err
	}
	m := msgs[len(msgs)-1]
	emailContext := fmt.Sprintf("From: %s\nDate: %s\nSubject: %s\n\n%s",
		displayFrom(m), m.InternalDate.Format("Mon, 2 Jan 2006 15:04"), m.Subject, w.bodyTextFor(m))
	done := w.aiActivity("Suggesting snooze time")
	t, reason, err := w.deps.Assistant.SuggestSnooze(ctx, time.Now(), emailContext)
	dispatch.Main(func() { done(doneErrCtx(ctx, err)) })
	return t, reason, err
}
