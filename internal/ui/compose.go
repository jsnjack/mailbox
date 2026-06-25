package ui

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/model"
)

// openCompose opens a compose window prefilled from init. aiContext, when
// non-empty and an assistant is configured, enables an "AI draft" button that
// streams a drafted reply into the body. title labels the window.
func (w *window) openCompose(init model.OutgoingMessage, aiContext, title string) {
	if w.deps.Send == nil {
		return
	}

	// With more than one account connected, the user picks which to send from;
	// otherwise the message goes from the active account.
	accounts := w.deps.Accounts
	var accountDD *gtk.DropDown
	selectedAccount := func() AccountInfo {
		if accountDD != nil {
			if i := int(accountDD.Selected()); i >= 0 && i < len(accounts) {
				return accounts[i]
			}
		}
		return AccountInfo{ID: w.activeID, Email: w.activeEmail}
	}

	toEntry := gtk.NewEntry()
	toEntry.SetPlaceholderText("To")
	toEntry.SetText(init.To)
	toEntry.SetHExpand(true)

	ccEntry := gtk.NewEntry()
	ccEntry.SetPlaceholderText("Cc")
	ccEntry.SetText(init.Cc)

	bccEntry := gtk.NewEntry()
	bccEntry.SetPlaceholderText("Bcc")
	bccEntry.SetText(init.Bcc)

	subjEntry := gtk.NewEntry()
	subjEntry.SetPlaceholderText("Subject")
	subjEntry.SetText(init.Subject)

	bodyView := gtk.NewTextView()
	bodyView.SetWrapMode(gtk.WrapWordChar)
	bodyView.SetVExpand(true)
	bodyView.SetLeftMargin(8)
	bodyView.SetTopMargin(8)
	buf := bodyView.Buffer()
	buf.SetText(init.Body)

	scroller := gtk.NewScrolledWindow()
	scroller.SetVExpand(true)
	scroller.SetHExpand(true)
	scroller.SetChild(bodyView)

	status := gtk.NewLabel("")
	status.SetXAlign(0)
	status.SetVisible(false)

	var attachments []model.OutgoingAttachment
	attachRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
	attachRow.SetVisible(false)

	box := gtk.NewBox(gtk.OrientationVertical, 6)
	setMargins(box, 12, 12, 12, 12)
	box.Append(toEntry)
	box.Append(ccEntry)
	box.Append(bccEntry)
	box.Append(subjEntry)
	box.Append(attachRow)
	box.Append(scroller)
	box.Append(status)

	if len(accounts) > 1 {
		emails := make([]string, len(accounts))
		active := 0
		for i, a := range accounts {
			emails[i] = a.Email
			if a.ID == w.activeID {
				active = i
			}
		}
		accountDD = gtk.NewDropDownFromStrings(emails)
		accountDD.SetSelected(uint(active))
		accountDD.SetHExpand(true)
		fromRow := gtk.NewBox(gtk.OrientationHorizontal, 8)
		fromRow.Append(gtk.NewLabel("From"))
		fromRow.Append(accountDD)
		box.Prepend(fromRow)
	}

	hb := adw.NewHeaderBar()
	send := gtk.NewButtonWithLabel("Send")
	send.AddCSSClass("suggested-action")
	hb.PackStart(send)

	var draftBtn *gtk.Button
	if w.deps.SaveDraft != nil {
		draftBtn = gtk.NewButtonWithLabel("Save draft")
		hb.PackStart(draftBtn)
	}

	attachBtn := gtk.NewButtonFromIconName("mail-attachment-symbolic")
	attachBtn.SetTooltipText("Attach a file")
	hb.PackEnd(attachBtn)

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(box)

	win := adw.NewWindow()
	win.SetTitle(title)
	win.SetDefaultSize(640, 560)
	win.SetContent(tv)

	aiCtx, cancelAI := context.WithCancel(context.Background())
	// sent becomes true once the message is sent, saved as a draft, or the user
	// confirms discarding it — so the close-request guard lets the window close.
	sent := false

	gather := func() model.OutgoingMessage {
		return model.OutgoingMessage{
			From:        selectedAccount().Email,
			To:          strings.TrimSpace(toEntry.Text()),
			Cc:          strings.TrimSpace(ccEntry.Text()),
			Bcc:         strings.TrimSpace(bccEntry.Text()),
			Subject:     subjEntry.Text(),
			Body:        bodyText(buf),
			InReplyTo:   init.InReplyTo,
			References:  init.References,
			ThreadID:    init.ThreadID,
			Attachments: attachments,
		}
	}

	// dirty reports whether the user has changed anything from the initial draft
	// (a reply/forward starts with prefilled, unedited content).
	dirty := func() bool {
		c := gather()
		return strings.TrimSpace(c.To) != strings.TrimSpace(init.To) ||
			strings.TrimSpace(c.Cc) != strings.TrimSpace(init.Cc) ||
			strings.TrimSpace(c.Bcc) != strings.TrimSpace(init.Bcc) ||
			c.Subject != init.Subject ||
			c.Body != init.Body ||
			len(c.Attachments) > 0
	}

	win.ConnectCloseRequest(func() bool {
		if sent || !dirty() {
			cancelAI()
			return false // allow the close
		}
		confirm := adw.NewAlertDialog("Discard message?", "This message has not been sent.")
		confirm.AddResponse("cancel", "Cancel")
		confirm.AddResponse("discard", "Discard")
		confirm.SetResponseAppearance("discard", adw.ResponseDestructive)
		confirm.SetDefaultResponse("cancel")
		confirm.SetCloseResponse("cancel")
		confirm.ConnectResponse(func(response string) {
			if response == "discard" {
				sent = true // bypass the guard on the programmatic close below
				cancelAI()
				win.Close()
			}
		})
		confirm.Present(win)
		return true // block this close; the dialog drives the actual close
	})

	attachBtn.ConnectClicked(func() {
		dialog := gtk.NewFileDialog()
		dialog.SetTitle("Attach a file")
		dialog.Open(context.Background(), &win.Window, func(res gio.AsyncResulter) {
			file, err := dialog.OpenFinish(res)
			if err != nil || file == nil {
				return // cancelled
			}
			path := file.Path()
			if path == "" {
				return
			}
			go func() {
				data, err := os.ReadFile(path)
				if err != nil {
					slog.Warn("ui: read attachment", "path", path, "err", err)
					return
				}
				name := filepath.Base(path)
				mtype := mime.TypeByExtension(filepath.Ext(name))
				if mtype == "" {
					mtype = "application/octet-stream"
				}
				dispatch.Main(func() {
					attachments = append(attachments, model.OutgoingAttachment{Filename: name, MimeType: mtype, Data: data})
					chip := gtk.NewBox(gtk.OrientationHorizontal, 4)
					chip.Append(gtk.NewImageFromIconName("mail-attachment-symbolic"))
					chip.Append(gtk.NewLabel(name))
					attachRow.Append(chip)
					attachRow.SetVisible(true)
				})
			}()
		})
	})

	send.ConnectClicked(func() {
		msg := gather() // reads the selected account on the main thread
		acctID := selectedAccount().ID
		send.SetSensitive(false)
		status.SetVisible(true)
		status.SetText("Sending…")
		go func() {
			err := w.deps.Send(context.Background(), acctID, msg)
			dispatch.Main(func() {
				if err != nil {
					slog.Warn("ui: send", "err", err)
					status.SetText("Send failed: " + err.Error())
					send.SetSensitive(true)
					return
				}
				sent = true
				win.Close()
			})
		}()
	})

	if draftBtn != nil {
		draftBtn.ConnectClicked(func() {
			msg := gather()
			acctID := selectedAccount().ID
			draftBtn.SetSensitive(false)
			status.SetVisible(true)
			status.SetText("Saving draft…")
			go func() {
				err := w.deps.SaveDraft(context.Background(), acctID, msg)
				dispatch.Main(func() {
					if err != nil {
						slog.Warn("ui: save draft", "err", err)
						status.SetText("Could not save draft: " + err.Error())
						draftBtn.SetSensitive(true)
						return
					}
					sent = true
					win.Close()
				})
			}()
		})
	}

	if w.deps.Assistant != nil && aiContext != "" {
		aiBtn := gtk.NewButtonWithLabel("AI draft")
		aiBtn.SetTooltipText("Draft this reply with AI")
		aiBtn.ConnectClicked(func() {
			aiBtn.SetSensitive(false)
			buf.SetText("")
			go func() {
				ch, err := w.deps.Assistant.DraftReply(aiCtx, aiContext, "")
				if err != nil {
					msg := err.Error()
					dispatch.Main(func() {
						buf.SetText("AI error: " + msg)
						aiBtn.SetSensitive(true)
					})
					return
				}
				var acc strings.Builder
				for c := range ch {
					cc := c
					dispatch.Main(func() {
						if cc.Err == nil {
							acc.WriteString(cc.Text)
							buf.SetText(acc.String())
						}
					})
				}
				dispatch.Main(func() { aiBtn.SetSensitive(true) })
			}()
		})
		hb.PackEnd(aiBtn)
	}

	win.SetVisible(true)
}

// bodyText returns the full text content of a text buffer.
func bodyText(buf *gtk.TextBuffer) string {
	start, end := buf.Bounds()
	return buf.Text(start, end, false)
}

// ensureRePrefix prefixes "Re: " unless the subject already has one.
func ensureRePrefix(subject string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(subject)), "re:") {
		return subject
	}
	return "Re: " + subject
}

// ensureFwdPrefix prefixes "Fwd: " unless the subject already has one.
func ensureFwdPrefix(subject string) string {
	low := strings.ToLower(strings.TrimSpace(subject))
	if strings.HasPrefix(low, "fwd:") || strings.HasPrefix(low, "fw:") {
		return subject
	}
	return "Fwd: " + subject
}

// quoteOriginal renders a simple quoted block of the original message.
func quoteOriginal(m model.Message, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nOn %s, %s wrote:\n", m.InternalDate.Format("Jan 2, 2006 15:04"), displayFrom(m))
	for _, line := range strings.Split(body, "\n") {
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
