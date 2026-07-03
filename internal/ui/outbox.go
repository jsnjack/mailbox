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

// refreshOutbox reveals the banner when any account has messages waiting to
// send (queued or failed), and hides it otherwise. A send stuck on a non-active
// account must be just as visible as one on the active account. The counts are
// gathered off the main thread.
func (w *window) refreshOutbox() {
	accounts := append([]AccountInfo(nil), w.deps.Accounts...)
	go func() {
		ctx := context.Background()
		total := 0
		for _, a := range accounts {
			n, err := w.deps.Store.CountPendingOutbox(ctx, a.ID)
			if err != nil {
				slog.Warn("ui: count outbox", "account", a.ID, "err", err)
				continue
			}
			total += n
		}
		logging.Trace("ui: refresh outbox", "accounts", len(accounts), "pending", total)
		dispatch.Main(func() {
			if total == 0 {
				w.outboxBanner.SetRevealed(false)
				return
			}
			noun := "message"
			if total != 1 {
				noun = "messages"
			}
			w.outboxBanner.SetTitle(fmt.Sprintf("%d %s waiting to send", total, noun))
			w.outboxBanner.SetRevealed(true)
		})
	}()
}

// openOutbox shows a dialog listing every account's queued/failed sends, with
// per-item retry/discard and a "Send now" action that sweeps all outboxes. The
// account set is captured at open, so the actions keep targeting the accounts
// the rows were listed for even if the active account changes while it's open.
func (w *window) openOutbox() {
	accounts := append([]AccountInfo(nil), w.deps.Accounts...)
	logging.Trace("ui: open outbox dialog", "accounts", len(accounts))
	listBox := gtk.NewListBox()
	listBox.AddCSSClass("boxed-list")
	listBox.SetSelectionMode(gtk.SelectionNone)

	scroller := gtk.NewScrolledWindow()
	scroller.SetVExpand(true)
	scroller.SetHExpand(true)
	scroller.SetChild(listBox)
	setMargins(scroller, 12, 12, 12, 12)

	showAccount := len(accounts) > 1
	var rebuild func()
	rebuild = func() {
		for child := listBox.FirstChild(); child != nil; child = listBox.FirstChild() {
			listBox.Remove(child)
		}
		total := 0
		for _, a := range accounts {
			items, err := w.deps.Store.ListPendingOutbox(context.Background(), a.ID)
			if err != nil {
				slog.Warn("ui: list outbox", "account", a.ID, "err", err)
				continue
			}
			for _, it := range items {
				listBox.Append(w.outboxRow(a, it, showAccount, rebuild))
				total++
			}
		}
		logging.Trace("ui: outbox dialog listed", "accounts", len(accounts), "items", total)
		if total == 0 {
			empty := gtk.NewLabel("The outbox is empty.")
			empty.AddCSSClass("dim-label")
			setMargins(empty, 12, 12, 18, 18)
			listBox.Append(empty)
		}
	}
	rebuild()

	hb := adw.NewHeaderBar()
	if w.deps.SweepOutbox != nil {
		sendNow := gtk.NewButtonWithLabel("Send now")
		sendNow.AddCSSClass("suggested-action")
		sendNow.ConnectClicked(func() {
			logging.Trace("ui: outbox send now", "accounts", len(accounts))
			go func() {
				var firstErr error
				for _, a := range accounts {
					if err := w.deps.SweepOutbox(context.Background(), a.ID); err != nil {
						slog.Warn("ui: sweep outbox", "account", a.ID, "err", err)
						if firstErr == nil {
							firstErr = err
						}
					}
				}
				logging.Trace("ui: outbox send now done", "accounts", len(accounts), "err", firstErr)
				dispatch.Main(func() {
					if firstErr != nil {
						w.toast("Couldn't send — messages stay queued")
					}
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

// outboxRow renders one queued/failed message with retry and discard actions,
// bound to the account it was listed for (acct — not whatever account is active
// when the button is clicked). showAccount adds the account email, so multi-
// account users can tell whose outbox an item sits in. rebuild refreshes the
// list after an action completes.
func (w *window) outboxRow(acct AccountInfo, it model.OutboxItem, showAccount bool, rebuild func()) *gtk.Box {
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

	toText := "To " + to
	if showAccount {
		toText += " — from " + acct.Email
	}
	toLbl := gtk.NewLabel(toText)
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
	acctID := acct.ID // the account these rows were listed for
	if w.deps.RetryOutbox != nil {
		retry := gtk.NewButtonFromIconName("view-refresh-symbolic")
		retry.SetTooltipText("Retry now")
		a11yLabel(retry, "Retry now")
		retry.SetVAlign(gtk.AlignCenter)
		retry.AddCSSClass("flat")
		retry.ConnectClicked(func() {
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
		a11yLabel(discard, "Discard")
		discard.SetVAlign(gtk.AlignCenter)
		discard.AddCSSClass("flat")
		discard.ConnectClicked(func() {
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
