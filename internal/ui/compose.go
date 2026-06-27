package ui

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/config"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/model"
)

// openCompose opens a compose window prefilled from init. aiContext, when
// non-empty and an assistant is configured, enables an "AI draft" button that
// streams a drafted reply into the body; autoDraft starts that draft
// immediately on open. title labels the window.
func (w *window) openCompose(init model.OutgoingMessage, aiContext, title string, autoDraft bool) {
	// Fresh composes/replies/forwards get the default signature; an existing
	// draft or a reopened (undone) message already contains its body verbatim.
	w.openComposeOpts(init, aiContext, title, autoDraft, init.DraftID == "")
}

func (w *window) openComposeOpts(init model.OutgoingMessage, aiContext, title string, autoDraft, addSignature bool) {
	if w.deps.Send == nil {
		return
	}

	// Append the configured default signature: below the cursor area for a new
	// message, between the reply area and the quoted history for a reply/forward.
	if addSignature {
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

	// One AI context for the window; cancelled when it closes (see below).
	aiCtx, cancelAI := context.WithCancel(context.Background())

	var attachments []model.OutgoingAttachment
	attachRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
	attachRow.SetVisible(false)
	addChip := func(name string) {
		chip := gtk.NewBox(gtk.OrientationHorizontal, 4)
		chip.Append(gtk.NewImageFromIconName("mail-attachment-symbolic"))
		chip.Append(gtk.NewLabel(name))
		attachRow.Append(chip)
		attachRow.SetVisible(true)
	}
	// Carry over any attachments from init (e.g. a reopened/undone message).
	attachments = append(attachments, init.Attachments...)
	for _, a := range init.Attachments {
		addChip(a.Filename)
	}

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
	// Subject row: the entry, plus an AI button that fills the subject in from the
	// body (the user's own text, not the signature/quote).
	if w.deps.Assistant != nil {
		subjEntry.SetHExpand(true)
		subjBtn := gtk.NewButtonFromIconName("sparkle-symbolic")
		subjBtn.SetTooltipText("Generate a subject from the email (AI)")
		subjBtn.AddCSSClass("flat")
		subjBtn.SetVAlign(gtk.AlignCenter)
		subjBtn.ConnectClicked(func() {
			full := bodyText(buf)
			body := strings.TrimSpace(full[:editableBoundary(full)])
			if body == "" {
				status.SetVisible(true)
				status.SetText("Write the email first, then generate a subject")
				return
			}
			subjBtn.SetSensitive(false)
			status.SetVisible(true)
			status.SetText("Generating subject…")
			done := w.aiActivity("Generating subject")
			go func() {
				subject, err := w.deps.Assistant.GenerateSubject(aiCtx, body)
				dispatch.Main(func() {
					done(doneErr(err))
					subjBtn.SetSensitive(true)
					if err != nil || subject == "" {
						status.SetText("Couldn't generate a subject")
						return
					}
					subjEntry.SetText(subject)
					status.SetVisible(false)
				})
			}()
		})
		subjRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
		subjRow.Append(subjEntry)
		subjRow.Append(subjBtn)
		box.Append(subjRow)
	} else {
		box.Append(subjEntry)
	}
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
	cw, ch := 640, 560
	if vs, err := config.LoadViewState(); err == nil && vs.ComposeWidth >= 400 && vs.ComposeHeight >= 300 {
		cw, ch = vs.ComposeWidth, vs.ComposeHeight
	}
	win.SetDefaultSize(cw, ch)
	saveComposeSize := func() {
		if win.IsMaximized() {
			return
		}
		vs, _ := config.LoadViewState()
		vs.ComposeWidth, vs.ComposeHeight = win.Width(), win.Height()
		if err := config.SaveViewState(vs); err != nil {
			slog.Warn("ui: save compose size", "err", err)
		}
	}
	win.SetContent(tv)

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
			saveComposeSize()
			return false // allow the close
		}
		confirm := adw.NewAlertDialog("Discard message?", "This message has not been sent.")
		confirm.AddResponse("cancel", "Cancel")
		if w.deps.SaveDraft != nil {
			confirm.AddResponse("save", "Save as draft")
			confirm.SetResponseAppearance("save", adw.ResponseSuggested)
		}
		confirm.AddResponse("discard", "Discard")
		confirm.SetResponseAppearance("discard", adw.ResponseDestructive)
		confirm.SetDefaultResponse("cancel")
		confirm.SetCloseResponse("cancel")
		confirm.ConnectResponse(func(response string) {
			switch response {
			case "discard":
				sent = true // bypass the guard on the programmatic close below
				cancelAI()
				win.Close()
			case "save":
				msg := gather()
				acctID := selectedAccount().ID
				sent = true
				cancelAI()
				go func() {
					if err := w.deps.SaveDraft(context.Background(), acctID, msg); err != nil {
						slog.Warn("ui: save draft on close", "err", err)
					}
				}()
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
					addChip(name)
				})
			}()
		})
	})

	doSend := func() {
		msg := gather() // reads the selected account on the main thread
		acctID := selectedAccount().ID
		// Close immediately and hand off to the delayed-send queue, which shows an
		// "Undo" toast for a few seconds before the message actually goes out.
		sent = true
		w.deferSend(acctID, msg)
		win.Close()
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
			done := w.aiActivity("Drafting reply")
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
						done(doneErr(err))
						buf.SetText("AI error: " + msg + "\n" + quote)
						aiBtn.SetSensitive(true)
					})
					return
				}
				final, _ := streamCoalesced(ch, func(text string) {
					buf.SetText(text + quote)
				})
				dispatch.Main(func() {
					done("")
					buf.SetText(final + quote)
					aiBtn.SetSensitive(true)
				})
			}()
		}
		// The button (and auto-draft) open the AI dialog: quick replies + presets.
		// A quick reply is used as-is (above the signature/quote); a preset/free
		// text generates a full draft.
		startAIDraft = func() {
			w.askAIIntent(win, isReply, aiContext, runDraft, func(text string) {
				buf.SetText(text + init.Body)
				bodyView.GrabFocus()
			})
		}
		aiBtn.ConnectClicked(func() { startAIDraft() })
		hb.PackEnd(aiBtn)

		// AI grammar/spell check: rewrite the body with spelling and grammar fixed
		// (preserving quotes and signature).
		grammarBtn := gtk.NewButtonFromIconName("tools-check-spelling-symbolic")
		grammarBtn.SetTooltipText("Check spelling & grammar with AI")
		grammarBtn.ConnectClicked(func() {
			// Proofread only the user's own writing: the selection if there is one,
			// otherwise the text above the signature/quote. The signature and the
			// quoted history are never sent to the AI or altered.
			startIter, endIter := grammarRange(buf)
			original := buf.Text(startIter, endIter, false)
			if strings.TrimSpace(original) == "" {
				return
			}
			// Anchor the range with marks so the exact span is replaced even if it
			// streams back after an edit (left/right gravity keeps it bracketing the
			// text). Other text — signature, quote — is left untouched.
			startMark := buf.CreateMark("", startIter, true)
			endMark := buf.CreateMark("", endIter, false)
			grammarBtn.SetSensitive(false)
			status.SetVisible(true)
			status.SetText("Checking grammar…")
			done := w.aiActivity("Checking grammar")
			go func() {
				ch, err := w.deps.Assistant.Proofread(aiCtx, original)
				var acc strings.Builder
				if err == nil {
					for c := range ch {
						if c.Err == nil {
							acc.WriteString(c.Text)
						}
					}
				}
				corrected := strings.TrimRight(acc.String(), " \t\r\n")
				dispatch.Main(func() {
					done(doneErr(err))
					grammarBtn.SetSensitive(true)
					defer func() { buf.DeleteMark(startMark); buf.DeleteMark(endMark) }()
					if err != nil || strings.TrimSpace(corrected) == "" {
						status.SetText("Grammar check failed")
						return
					}
					si := buf.IterAtMark(startMark)
					ei := buf.IterAtMark(endMark)
					buf.Delete(si, ei)
					buf.Insert(buf.IterAtMark(startMark), corrected)
					status.SetVisible(false)
				})
			}()
		})
		hb.PackEnd(grammarBtn)
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
		// Esc closes the window (through the unsaved-changes guard), unless the
		// focus is a field that wants Esc itself (e.g. dismissing autocomplete).
		if keyval == gdk.KEY_Escape {
			if _, ok := win.Focus().(*gtk.Text); ok {
				return false
			}
			win.Close()
			return true
		}
		return false
	})
	win.AddController(keyCtl)

	// Recipient autocomplete from past correspondents (built off the main thread,
	// then attached to the To/Cc/Bcc fields).
	go func() {
		contacts, err := w.deps.Store.Contacts(context.Background(), w.activeID, w.activeEmail, 1500)
		if err != nil {
			slog.Warn("ui: load contacts", "err", err)
		}
		dispatch.Main(func() {
			contacts = w.withOwnAccounts(contacts)
			if len(contacts) == 0 {
				return
			}
			st := buildContactStore(contacts)
			attachRecipientCompletion(toEntry, st)
			attachRecipientCompletion(ccEntry, st)
			attachRecipientCompletion(bccEntry, st)
		})
	}()

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

// askAIIntent presents AI reply assistance in one place: ready-to-send quick
// replies (for a reply, loaded from the thread), tone presets, and a free-text
// field. Picking a quick reply calls onQuickReply with its text (used directly);
// a preset or free text calls onInstruction to generate a full draft.
func (w *window) askAIIntent(parent gtk.Widgetter, isReply bool, threadContext string, onInstruction, onQuickReply func(string)) {
	dialog := adw.NewDialog()
	dialog.SetContentWidth(440)
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
		onInstruction(instruction)
	}

	// Ready-to-send quick replies (for a reply) — gated behind a button so they
	// are only generated (and tokens spent) when the user asks.
	if isReply && strings.TrimSpace(threadContext) != "" && onQuickReply != nil && w.deps.Assistant != nil {
		quick := gtk.NewBox(gtk.OrientationVertical, 4)
		box.Append(quick)
		clearQuick := func() {
			for c := quick.FirstChild(); c != nil; c = quick.FirstChild() {
				quick.Remove(c)
			}
		}
		var showSuggestButton func()
		showSuggestButton = func() {
			clearQuick()
			bl := gtk.NewLabel("Suggest quick replies")
			bl.SetXAlign(0)
			bl.SetHExpand(true)
			btn := gtk.NewButton()
			btn.SetChild(bl)
			btn.AddCSSClass("flat")
			btn.ConnectClicked(func() {
				clearQuick()
				busy := gtk.NewLabel("Loading quick replies…")
				busy.SetXAlign(0)
				busy.AddCSSClass("dim-label")
				busy.AddCSSClass("caption")
				quick.Append(busy)
				done := w.aiActivity("Suggesting quick replies")
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					replies, err := w.deps.Assistant.SmartReplies(ctx, threadContext)
					dispatch.Main(func() {
						done(doneErr(err))
						clearQuick()
						if err != nil || len(replies) == 0 {
							if err != nil {
								slog.Warn("ui: smart replies", "err", err)
							}
							showSuggestButton() // restore so the user can retry
							return
						}
						for _, r := range replies {
							text := strings.TrimSpace(r)
							if text == "" {
								continue
							}
							rl := gtk.NewLabel(text)
							rl.SetXAlign(0)
							rl.SetWrap(true)
							rl.SetHExpand(true)
							rb := gtk.NewButton()
							rb.SetChild(rl)
							rb.AddCSSClass("flat")
							rb.ConnectClicked(func() {
								dialog.Close()
								onQuickReply(text)
							})
							quick.Append(rb)
						}
						quick.Append(gtk.NewSeparator(gtk.OrientationHorizontal))
					})
				}()
			})
			quick.Append(btn)
			quick.Append(gtk.NewSeparator(gtk.OrientationHorizontal))
		}
		showSuggestButton()
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

// formatContact renders a contact as an RFC-5322-ish recipient token.
func formatContact(c model.Contact) string {
	if c.Name != "" && !strings.EqualFold(c.Name, c.Address) {
		return fmt.Sprintf("%s <%s>", c.Name, c.Address)
	}
	return c.Address
}

// withOwnAccounts prepends the user's registered accounts to the contact list
// (so you can address your other account), de-duplicated against the past
// correspondents by address. Own accounts come first so they rank at the top.
func (w *window) withOwnAccounts(past []model.Contact) []model.Contact {
	seen := make(map[string]bool, len(w.deps.Accounts))
	out := make([]model.Contact, 0, len(w.deps.Accounts)+len(past))
	for _, a := range w.deps.Accounts {
		addr := strings.ToLower(strings.TrimSpace(a.Email))
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, model.Contact{Name: w.accountDisplayName(a), Address: a.Email})
	}
	for _, c := range past {
		if seen[strings.ToLower(strings.TrimSpace(c.Address))] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// buildContactStore builds a single-column list model of recipient tokens to
// back the autocompletion on the To/Cc/Bcc fields. GtkListStore/TreeModel are
// deprecated in GTK4 but are still required to back a GtkEntryCompletion
// (gio.ListStore is not a GtkTreeModel).
//
//nolint:staticcheck // GtkListStore/EntryCompletion are deprecated; no replacement.
func buildContactStore(contacts []model.Contact) *gtk.ListStore {
	st := gtk.NewListStore([]coreglib.Type{coreglib.TypeString})
	for _, c := range contacts {
		st.SetValue(st.Append(), 0, coreglib.NewValue(formatContact(c)))
	}
	return st
}

// attachRecipientCompletion wires past-correspondent autocompletion onto a
// recipient entry. Matching and insertion operate on the last comma-separated
// token, so it works for multi-recipient fields.
//
// practical way to complete a text entry; the list-model widgets don't replace it.
//
//nolint:staticcheck // GtkEntryCompletion is deprecated in GTK4 but is still the
func attachRecipientCompletion(entry *gtk.Entry, st *gtk.ListStore) {
	lastToken := func() (prefix, token string) {
		t := entry.Text()
		if i := strings.LastIndexByte(t, ','); i >= 0 {
			return t[:i+1] + " ", strings.TrimSpace(t[i+1:])
		}
		return "", strings.TrimSpace(t)
	}

	comp := gtk.NewEntryCompletion()
	comp.SetModel(st)
	comp.SetTextColumn(0)
	comp.SetMinimumKeyLength(1)
	comp.SetPopupCompletion(true)
	comp.SetMatchFunc(func(_ *gtk.EntryCompletion, _ string, iter *gtk.TreeIter) bool {
		_, token := lastToken()
		if token == "" {
			return false
		}
		val := st.Value(iter, 0)
		return strings.Contains(strings.ToLower(val.String()), strings.ToLower(token))
	})
	comp.ConnectMatchSelected(func(_ gtk.TreeModeller, iter *gtk.TreeIter) bool {
		prefix, _ := lastToken()
		val := st.Value(iter, 0)
		entry.SetText(prefix + val.String() + ", ")
		entry.SetPosition(-1)
		return true
	})
	entry.SetCompletion(comp)
}

// bodyText returns the full text content of a text buffer.
func bodyText(buf *gtk.TextBuffer) string {
	start, end := buf.Bounds()
	return buf.Text(start, end, false)
}

// grammarRange returns the iters bounding the text grammar-check should correct:
// the current selection if any, otherwise from the start of the buffer to the
// signature/quote boundary (so the signature and quoted reply history are left
// out). The returned iters are valid until the buffer is mutated.
func grammarRange(buf *gtk.TextBuffer) (*gtk.TextIter, *gtk.TextIter) {
	if start, end, ok := buf.SelectionBounds(); ok {
		return start, end
	}
	start, _ := buf.Bounds()
	full := bodyText(buf)
	boundary := utf8.RuneCountInString(full[:editableBoundary(full)]) // bytes → chars
	return start, buf.IterAtOffset(boundary)
}

// editableBoundary returns the byte offset in a composed body where the
// non-editable region (signature, then quoted history) begins — i.e. the
// earliest of the RFC 3676 "-- " signature delimiter line, a quote attribution
// ("On … wrote:") line, or the first ">"-quoted line. It returns len(text) when
// the body is all the user's own writing. The markers are reliable because the
// compose path generates them (composeBodyWithSignature, quoteOriginal).
func editableBoundary(text string) int {
	boundary := len(text)
	mark := func(off int) {
		if off < boundary {
			boundary = off
		}
	}
	off := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimRight(line, "\n")
		switch {
		case trimmed == "-- ": // signature delimiter
			mark(off)
		case strings.HasPrefix(trimmed, ">"): // quoted line
			mark(off)
		case strings.HasPrefix(trimmed, "On ") && strings.HasSuffix(strings.TrimRight(trimmed, " "), "wrote:"):
			mark(off) // quote attribution
		}
		off += len(line)
	}
	// Back up over the blank-line separator before the marker so it stays with the
	// preserved region — otherwise the corrected text (trailing newlines trimmed)
	// would be glued directly onto the signature/quote.
	if boundary < len(text) {
		for boundary > 0 && text[boundary-1] == '\n' {
			boundary--
		}
	}
	return boundary
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
