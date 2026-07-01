package ui

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// refreshOutbox reveals the banner when the active account has messages waiting
// to send (queued or failed), and hides it otherwise.
func (w *window) refreshOutbox() {
	n, err := w.deps.Store.CountPendingOutbox(context.Background(), w.activeID)
	if err != nil {
		slog.Warn("ui: count outbox", "err", err)
		return
	}
	logging.Trace("ui: refresh outbox", "account", w.activeID, "pending", n)
	if n == 0 {
		w.outboxBanner.SetRevealed(false)
		return
	}
	noun := "message"
	if n != 1 {
		noun = "messages"
	}
	w.outboxBanner.SetTitle(fmt.Sprintf("%d %s waiting to send", n, noun))
	w.outboxBanner.SetRevealed(true)
}

// openOutbox shows a dialog listing the account's queued/failed sends, with
// per-item retry/discard and a "Send now" action that sweeps the whole outbox.
func (w *window) openOutbox() {
	logging.Trace("ui: open outbox dialog", "account", w.activeID)
	listBox := gtk.NewListBox()
	listBox.AddCSSClass("boxed-list")
	listBox.SetSelectionMode(gtk.SelectionNone)

	scroller := gtk.NewScrolledWindow()
	scroller.SetVExpand(true)
	scroller.SetHExpand(true)
	scroller.SetChild(listBox)
	setMargins(scroller, 12, 12, 12, 12)

	var rebuild func()
	rebuild = func() {
		for child := listBox.FirstChild(); child != nil; child = listBox.FirstChild() {
			listBox.Remove(child)
		}
		items, err := w.deps.Store.ListPendingOutbox(context.Background(), w.activeID)
		if err != nil {
			slog.Warn("ui: list outbox", "err", err)
		}
		if len(items) == 0 {
			empty := gtk.NewLabel("The outbox is empty.")
			empty.AddCSSClass("dim-label")
			setMargins(empty, 12, 12, 18, 18)
			listBox.Append(empty)
			return
		}
		for _, it := range items {
			listBox.Append(w.outboxRow(it, rebuild))
		}
	}
	rebuild()

	hb := adw.NewHeaderBar()
	if w.deps.SweepOutbox != nil {
		sendNow := gtk.NewButtonWithLabel("Send now")
		sendNow.AddCSSClass("suggested-action")
		sendNow.ConnectClicked(func() {
			acctID := w.activeID
			logging.Trace("ui: outbox send now", "account", acctID)
			go func() {
				err := w.deps.SweepOutbox(context.Background(), acctID)
				dispatch.Main(func() {
					if err != nil {
						slog.Warn("ui: sweep outbox", "err", err)
						w.toast("Couldn't send — messages stay queued")
					}
					logging.Trace("ui: outbox send now done", "account", acctID, "err", err)
					rebuild()
					w.refreshOutbox()
				})
			}()
		})
		hb.PackStart(sendNow)
	}

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(scroller)

	dialog := adw.NewDialog()
	dialog.SetTitle("Outbox")
	dialog.SetContentWidth(560)
	dialog.SetContentHeight(440)
	dialog.SetChild(tv)
	dialog.Present(w.win)
}

// outboxRow renders one queued/failed message with retry and discard actions.
// rebuild refreshes the list after an action completes.
func (w *window) outboxRow(it model.OutboxItem, rebuild func()) *gtk.Box {
	to, subject := outboxHeaders(it.RFC822)
	if subject == "" {
		subject = "(no subject)"
	}
	if to == "" {
		to = "(no recipient)"
	}

	info := gtk.NewBox(gtk.OrientationVertical, 2)
	info.SetHExpand(true)

	title := gtk.NewLabel(subject)
	title.SetXAlign(0)
	title.AddCSSClass("heading")
	title.SetEllipsize(pango.EllipsizeEnd)
	info.Append(title)

	toLbl := gtk.NewLabel("To " + to)
	toLbl.SetXAlign(0)
	toLbl.AddCSSClass("dim-label")
	toLbl.AddCSSClass("caption")
	toLbl.SetEllipsize(pango.EllipsizeEnd)
	info.Append(toLbl)

	status := gtk.NewLabel(outboxStatus(it))
	status.SetXAlign(0)
	status.SetWrap(true)
	status.AddCSSClass("caption")
	if it.State == "failed" {
		status.AddCSSClass("error")
	} else {
		status.AddCSSClass("dim-label")
	}
	info.Append(status)

	row := gtk.NewBox(gtk.OrientationHorizontal, 8)
	setMargins(row, 12, 12, 8, 8)
	row.Append(info)

	id := it.ID
	if w.deps.RetryOutbox != nil {
		retry := gtk.NewButtonFromIconName("view-refresh-symbolic")
		retry.SetTooltipText("Retry now")
		retry.SetVAlign(gtk.AlignCenter)
		retry.AddCSSClass("flat")
		retry.ConnectClicked(func() {
			acctID := w.activeID
			logging.Trace("ui: outbox retry item", "account", acctID, "id", id)
			go func() {
				err := w.deps.RetryOutbox(context.Background(), acctID, id)
				dispatch.Main(func() {
					if err != nil {
						slog.Warn("ui: retry outbox", "err", err)
						w.toast("Retry failed — message stays queued")
					}
					logging.Trace("ui: outbox retry item done", "account", acctID, "id", id, "err", err)
					rebuild()
					w.refreshOutbox()
				})
			}()
		})
		row.Append(retry)
	}
	if w.deps.DiscardOutbox != nil {
		discard := gtk.NewButtonFromIconName("user-trash-symbolic")
		discard.SetTooltipText("Discard")
		discard.SetVAlign(gtk.AlignCenter)
		discard.AddCSSClass("flat")
		discard.ConnectClicked(func() {
			acctID := w.activeID
			logging.Trace("ui: outbox discard item", "account", acctID, "id", id)
			go func() {
				err := w.deps.DiscardOutbox(context.Background(), acctID, id)
				dispatch.Main(func() {
					if err != nil {
						slog.Warn("ui: discard outbox", "err", err)
						w.toast("Couldn't discard message")
					}
					logging.Trace("ui: outbox discard item done", "account", acctID, "id", id, "err", err)
					rebuild()
					w.refreshOutbox()
				})
			}()
		})
		row.Append(discard)
	}
	return row
}

// outboxHeaders parses the recipient and subject from a queued RFC 5322 message.
func outboxHeaders(raw []byte) (to, subject string) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", ""
	}
	return strings.TrimSpace(msg.Header.Get("To")), strings.TrimSpace(msg.Header.Get("Subject"))
}

// outboxStatus describes an item's send state for display.
func outboxStatus(it model.OutboxItem) string {
	if it.State == "failed" {
		if it.LastError != "" {
			return fmt.Sprintf("Failed (attempt %d): %s", it.Attempts, it.LastError)
		}
		return fmt.Sprintf("Failed (attempt %d)", it.Attempts)
	}
	return "Queued"
}
