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
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/model"
)

// openCompose opens a compose window prefilled from init. aiContext, when
// non-empty and an assistant is configured, enables an "AI draft" button that
// streams a drafted reply into the body; autoDraft starts that draft
// immediately on open. title labels the window.
func (w *window) openCompose(init model.OutgoingMessage, aiContext, title string, autoDraft bool) {
	if w.deps.Send == nil {
		return
	}

	// Append the configured default signature: below the cursor area for a new
	// message, between the reply area and the quoted history for a reply/forward.
	// A draft already contains whatever the user saved (signature included), so
	// it is left untouched.
	if init.DraftID == "" {
		init.Body = composeBodyWithSignature(init.Body, w.signature)
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

	// Cc/Bcc are hidden until needed (revealed by the toggle, or automatically
	// when a reply/forward prefilled them).
	ccBccBtn := gtk.NewButtonWithLabel("Cc/Bcc")
	ccBccBtn.AddCSSClass("flat")
	ccBccBtn.SetVAlign(gtk.AlignCenter)
	toRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
	toRow.Append(toEntry)
	toRow.Append(ccBccBtn)

	ccEntry.SetVisible(false)
	bccEntry.SetVisible(false)
	showCcBcc := func() {
		ccEntry.SetVisible(true)
		bccEntry.SetVisible(true)
		ccBccBtn.SetVisible(false)
	}
	ccBccBtn.ConnectClicked(func() { showCcBcc() })
	if strings.TrimSpace(init.Cc) != "" || strings.TrimSpace(init.Bcc) != "" {
		showCcBcc()
	}

	box := gtk.NewBox(gtk.OrientationVertical, 6)
	setMargins(box, 12, 12, 12, 12)
	box.Append(toRow)
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
	send.SetTooltipText("Send (Ctrl+Enter)")
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
			DraftID:     init.DraftID,
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

	doSend := func() {
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
	}
	// preSendWarning returns the first reason to double-check before sending, or
	// "" when the message looks ready.
	preSendWarning := func() string {
		if strings.TrimSpace(subjEntry.Text()) == "" {
			return "This message has no subject line."
		}
		if len(attachments) == 0 && mentionsAttachment(bodyText(buf)) {
			return "You mention an attachment, but none is attached."
		}
		return ""
	}
	send.ConnectClicked(func() {
		if warn := preSendWarning(); warn != "" {
			confirm := adw.NewAlertDialog("Send anyway?", warn)
			confirm.AddResponse("cancel", "Cancel")
			confirm.AddResponse("send", "Send anyway")
			confirm.SetResponseAppearance("send", adw.ResponseSuggested)
			confirm.SetDefaultResponse("cancel")
			confirm.SetCloseResponse("cancel")
			confirm.ConnectResponse(func(r string) {
				if r == "send" {
					doSend()
				}
			})
			confirm.Present(win)
			return
		}
		doSend()
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

	var startAIDraft func()
	if w.deps.Assistant != nil {
		// A reply/forward has thread context; a new message is drafted from the
		// user's instruction (and the subject) alone.
		isReply := aiContext != ""
		aiBtn := gtk.NewButtonWithLabel("AI draft")
		if isReply {
			aiBtn.SetTooltipText("Draft this reply with AI")
		} else {
			aiBtn.SetTooltipText("Draft this email with AI")
		}
		// runDraft streams a draft guided by instruction (may be empty) into the
		// body, above whatever was already there (quote/signature), which is kept.
		runDraft := func(instruction string) {
			aiBtn.SetSensitive(false)
			quote := init.Body
			subject := strings.TrimSpace(subjEntry.Text())
			buf.SetText(quote)
			go func() {
				var ch <-chan ai.Chunk
				var err error
				if isReply {
					ch, err = w.deps.Assistant.DraftReply(aiCtx, aiContext, instruction)
				} else {
					ch, err = w.deps.Assistant.DraftNew(aiCtx, subject, instruction)
				}
				if err != nil {
					msg := err.Error()
					dispatch.Main(func() {
						buf.SetText("AI error: " + msg + "\n" + quote)
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
							buf.SetText(acc.String() + quote)
						}
					})
				}
				dispatch.Main(func() { aiBtn.SetSensitive(true) })
			}()
		}
		// The button (and auto-draft) first ask what the message should say.
		startAIDraft = func() { w.askAIIntent(win, isReply, runDraft) }
		aiBtn.ConnectClicked(func() { startAIDraft() })
		hb.PackEnd(aiBtn)
	}

	// Ctrl+Enter sends, from anywhere in the window. Capture phase so it fires
	// before the body TextView would treat Return as a newline; plain Return
	// (no Ctrl) falls through untouched.
	keyCtl := gtk.NewEventControllerKey()
	keyCtl.SetPropagationPhase(gtk.PhaseCapture)
	keyCtl.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		if state&gdk.ControlMask != 0 && (keyval == gdk.KEY_Return || keyval == gdk.KEY_KP_Enter || keyval == gdk.KEY_ISO_Enter) {
			send.Activate()
			return true
		}
		return false
	})
	win.AddController(keyCtl)

	win.SetVisible(true)

	switch {
	case autoDraft && startAIDraft != nil:
		// For an AI-initiated reply, ask for intent once the window is up.
		startAIDraft()
	case strings.TrimSpace(init.To) != "":
		bodyView.GrabFocus() // reply: cursor in the body, above the quote
	default:
		toEntry.GrabFocus() // new message / forward: pick the recipient first
	}
}

// askAIIntent presents quick presets and a free-text field, then calls onChosen
// with the instruction to guide the AI draft (empty = neutral). The presets and
// wording differ for a reply (tones) versus a new message (intents).
func (w *window) askAIIntent(parent gtk.Widgetter, isReply bool, onChosen func(string)) {
	dialog := adw.NewDialog()
	dialog.SetContentWidth(420)
	dialog.SetFollowsContentSize(true)

	presets := []struct{ label, instruction string }{
		{"Accept / agree", "Accept and agree."},
		{"Politely decline", "Politely decline."},
		{"Thank them", "Thank the sender."},
		{"Ask for more details", "Ask for more details or clarification."},
		{"I'll follow up later", "Say I will follow up later."},
		{"Confirm availability", "Confirm that I'm available."},
	}
	title := "Draft reply with AI"
	hintText := "Pick a tone or describe what to say; the AI drafts the reply."
	if !isReply {
		presets = []struct{ label, instruction string }{
			{"Request a meeting", "Request a meeting and propose a couple of times."},
			{"Introduce myself", "Introduce myself and explain why I'm reaching out."},
			{"Follow up", "Write a polite follow-up."},
			{"Make a request", "Politely ask for something."},
		}
		title = "Draft email with AI"
		hintText = "Describe what the email should say; the AI writes it."
	}
	dialog.SetTitle(title)

	box := gtk.NewBox(gtk.OrientationVertical, 8)
	setMargins(box, 16, 16, 16, 16)

	hint := gtk.NewLabel(hintText)
	hint.SetXAlign(0)
	hint.SetWrap(true)
	hint.AddCSSClass("dim-label")
	box.Append(hint)

	choose := func(instruction string) {
		dialog.Close()
		onChosen(instruction)
	}

	for _, q := range presets {
		instr := q.instruction
		b := gtk.NewButton()
		l := gtk.NewLabel(q.label)
		l.SetXAlign(0)
		l.SetHExpand(true)
		b.SetChild(l)
		b.AddCSSClass("flat")
		b.ConnectClicked(func() { choose(instr) })
		box.Append(b)
	}

	box.Append(gtk.NewSeparator(gtk.OrientationHorizontal))

	entry := gtk.NewEntry()
	entry.SetPlaceholderText("Or describe what to say…")
	entry.SetHExpand(true)
	box.Append(entry)

	gen := gtk.NewButtonWithLabel("Generate")
	gen.AddCSSClass("suggested-action")
	gen.SetHAlign(gtk.AlignEnd)
	gen.ConnectClicked(func() { choose(strings.TrimSpace(entry.Text())) })
	box.Append(gen)

	dialog.SetChild(box)
	dialog.Present(parent)
}

// composeBodyWithSignature inserts the default signature into a compose body.
// quote is the prefilled content (empty for a new message, the quoted history
// for a reply/forward). The signature is placed below the cursor area and above
// any quote, using the RFC 3676 "-- " delimiter. Empty signature → unchanged.
func composeBodyWithSignature(quote, sig string) string {
	sig = strings.TrimRight(sig, " \t\r\n")
	if sig == "" {
		return quote
	}
	block := "\n\n-- \n" + sig
	if quote == "" {
		return block
	}
	return block + "\n\n" + quote
}

// mentionsAttachment reports whether the body text suggests the user meant to
// attach a file. Quoted lines (a reply's history) are ignored so a quoted
// "attached" from the original message doesn't trigger a false warning.
func mentionsAttachment(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, ">") {
			continue
		}
		low := strings.ToLower(l)
		for _, kw := range []string{"attach", "enclosed", "see attached"} {
			if strings.Contains(low, kw) {
				return true
			}
		}
	}
	return false
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
