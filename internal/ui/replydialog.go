package ui

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/logging"
)

const replySuggestionsCap = 32

// askAIIntent shows instructions and generated replies in one bounded dialog.
// Choosing a suggestion opens the compose immediately.
func (w *window) askAIIntent(parent gtk.Widgetter, isReply bool, threadContext string, onInstruction, onQuickReply func(string)) *adw.Dialog {
	logging.Trace("ui: ai intent dialog", "is_reply", isReply)
	dialog := adw.NewDialog()
	dialog.SetContentWidth(520)
	dialog.SetContentHeight(460)
	dialog.SetFollowsContentSize(false)
	title := "Draft reply"
	if !isReply {
		title = "Draft email"
	}
	dialog.SetTitle(title)
	ctx, cancel := context.WithCancel(context.Background())
	dialog.ConnectClosed(cancel)

	toolbar := adw.NewToolbarView()
	header := adw.NewHeaderBar()
	header.SetShowStartTitleButtons(false)
	header.SetShowEndTitleButtons(false)
	header.SetTitleWidget(adw.NewWindowTitle(title, ""))
	cancelBtn := gtk.NewButtonWithLabel("Cancel")
	cancelBtn.ConnectClicked(func() { cancel(); dialog.Close() })
	header.PackStart(cancelBtn)
	action := gtk.NewButtonWithLabel("Create draft")
	action.AddCSSClass("suggested-action")
	action.SetSensitive(false)
	action.SetVisible(onInstruction != nil)
	header.PackEnd(action)
	toolbar.AddTopBar(header)

	content := gtk.NewBox(gtk.OrientationVertical, 20)
	setMargins(content, 20, 20, 12, 20)
	canSuggest := isReply && strings.TrimSpace(threadContext) != "" && onQuickReply != nil && w.deps.Assistant != nil && w.aiSmartReplies
	entry := gtk.NewTextView()
	entry.SetWrapMode(gtk.WrapWordChar)
	entry.SetAcceptsTab(false)
	entry.SetLeftMargin(12)
	entry.SetRightMargin(12)
	entry.SetTopMargin(12)
	entry.SetBottomMargin(12)
	a11yLabel(entry, "What would you like to say?")
	updateAction := func() {
		action.SetSensitive(strings.TrimSpace(bodyText(entry.Buffer())) != "")
	}
	entry.Buffer().ConnectChanged(updateAction)
	action.ConnectClicked(func() {
		instruction := strings.TrimSpace(bodyText(entry.Buffer()))
		if instruction == "" || onInstruction == nil || ctx.Err() != nil {
			return
		}
		cancel()
		dialog.Close()
		onInstruction(instruction)
	})

	if onInstruction != nil {
		form := gtk.NewBox(gtk.OrientationVertical, 12)
		label := gtk.NewLabel("What would you like to say?")
		label.SetXAlign(0)
		label.AddCSSClass("heading")
		form.Append(label)
		hint := gtk.NewLabel("For example: decline politely and offer next Tuesday.")
		if !isReply {
			hint.SetText("For example: ask the team for feedback by Friday.")
		}
		hint.SetXAlign(0)
		hint.SetWrap(true)
		hint.AddCSSClass("dim-label")
		form.Append(hint)
		scroller := gtk.NewScrolledWindow()
		scroller.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
		scroller.SetVExpand(!canSuggest)
		if canSuggest {
			scroller.SetSizeRequest(-1, 96)
		}
		scroller.SetChild(entry)
		scroller.AddCSSClass("card")
		form.Append(scroller)
		form.SetVExpand(!canSuggest)
		content.Append(form)
	}

	if canSuggest {
		page := gtk.NewBox(gtk.OrientationVertical, 12)
		page.SetVExpand(true)
		heading := gtk.NewBox(gtk.OrientationHorizontal, 12)
		label := gtk.NewLabel("Suggested replies")
		label.SetXAlign(0)
		label.SetHExpand(true)
		label.AddCSSClass("heading")
		heading.Append(label)
		retry := gtk.NewButtonWithLabel("Try again")
		retry.AddCSSClass("flat")
		heading.Append(retry)
		page.Append(heading)
		scroller := gtk.NewScrolledWindow()
		scroller.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
		scroller.SetVExpand(true)
		quick := gtk.NewBox(gtk.OrientationVertical, 10)
		scroller.SetChild(quick)
		page.Append(scroller)
		content.Append(page)
		key := sha256.Sum256([]byte(threadContext))
		clearQuick := func() {
			for child := quick.FirstChild(); child != nil; child = quick.FirstChild() {
				quick.Remove(child)
			}
		}
		showReplies := func(replies []string) {
			clearQuick()
			list := gtk.NewListBox()
			list.SetSelectionMode(gtk.SelectionNone)
			list.AddCSSClass("boxed-list")
			list.SetVAlign(gtk.AlignStart)
			quick.Append(list)
			for _, reply := range replies {
				row := adw.NewActionRow()
				row.SetTitle(glib.MarkupEscapeText(reply))
				row.SetTitleLines(0)
				row.SetActivatable(true)
				row.AddSuffix(gtk.NewImageFromIconName("go-next-symbolic"))
				row.ConnectActivated(func() {
					if ctx.Err() != nil {
						return
					}
					cancel()
					dialog.Close()
					onQuickReply(reply)
				})
				list.Append(row)
			}
		}
		load := func(refresh bool) {
			if replies := w.replySuggestions[key]; !refresh && len(replies) > 0 {
				logging.Trace("ui: smart replies cache hit")
				showReplies(replies)
				return
			}
			clearQuick()
			retry.SetSensitive(false)
			busy := gtk.NewBox(gtk.OrientationVertical, 12)
			busy.SetVExpand(true)
			busy.SetVAlign(gtk.AlignCenter)
			spinner := adw.NewSpinner()
			spinner.SetSizeRequest(28, 28)
			busy.Append(spinner)
			busy.Append(gtk.NewLabel("Suggesting replies…"))
			quick.Append(busy)
			done := w.aiActivity("Suggesting replies")
			logging.Trace("ui: ai smart replies begin", "refresh", refresh)
			go func() {
				requestCtx, stop := context.WithTimeout(ctx, 30*time.Second)
				defer stop()
				replies, err := w.deps.Assistant.SmartReplies(requestCtx, threadContext)
				dispatch.Main(func() {
					done(doneErrCtx(ctx, err))
					if ctx.Err() != nil {
						return
					}
					retry.SetSensitive(true)
					clearQuick()
					replies = cleanQuickReplies(replies)
					logging.Trace("ui: ai smart replies done", "n", len(replies), "err", err)
					if err != nil || len(replies) == 0 {
						if err != nil {
							slog.Warn("ui: smart replies", "err", err)
						}
						message := gtk.NewLabel("Couldn't suggest replies. Try again.")
						message.SetWrap(true)
						message.AddCSSClass("dim-label")
						quick.Append(message)
						return
					}
					if w.replySuggestions == nil || len(w.replySuggestions) >= replySuggestionsCap {
						w.replySuggestions = make(map[[32]byte][]string)
					}
					w.replySuggestions[key] = replies
					showReplies(replies)
				})
			}()
		}
		retry.ConnectClicked(func() { load(true) })
		load(false)
	}
	toolbar.SetContent(content)
	dialog.SetChild(toolbar)
	if onInstruction != nil {
		dialog.SetFocus(entry)
	}
	updateAction()
	dialog.Present(parent)
	return dialog
}

func cleanQuickReplies(replies []string) []string {
	var out []string
	for _, reply := range replies {
		if text := strings.TrimSpace(reply); text != "" {
			out = append(out, text)
		}
	}
	return out
}
