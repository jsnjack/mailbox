package ui

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"os"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	webkit "github.com/diamondburned/gotk4-webkitgtk/pkg/webkit/v6"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/microcosm-cc/bluemonday"
)

func newAdwApplication() *adw.Application {
	return adw.NewApplication(appID, gio.ApplicationFlagsNone)
}

// window owns the widget tree and the currently displayed selection.
type window struct {
	app  *adw.Application
	deps Deps

	win       *adw.ApplicationWindow
	labelBox  *gtk.ListBox
	labels    []model.Label
	threadBox *gtk.ListBox
	msgs      []model.Message
	current   string
	header    *gtk.Label
	webview   *webkit.WebView
	sanitizer *bluemonday.Policy

	// reader actions
	openMsg      model.Message // the message currently shown in the reader
	replyBtn     *gtk.Button
	forwardBtn   *gtk.Button
	archiveBtn   *gtk.Button
	unreadBtn    *gtk.Button
	starBtn      *gtk.ToggleButton
	imagesBtn    *gtk.ToggleButton
	translateBtn *gtk.Button
	draftBtn     *gtk.Button
	updatingStar bool // guards programmatic star-toggle from firing the handler
}

func newWindow(app *adw.Application, deps Deps) *window {
	w := &window{
		app:       app,
		deps:      deps,
		current:   model.LabelInbox,
		sanitizer: bluemonday.UGCPolicy(),
	}
	w.build()
	return w
}

func (w *window) build() {
	w.win = adw.NewApplicationWindow(&w.app.Application)
	w.win.SetTitle("Mailbox")
	w.win.SetDefaultSize(1200, 760)

	inner := adw.NewNavigationSplitView()
	inner.SetMinSidebarWidth(340)
	inner.SetMaxSidebarWidth(520)
	inner.SetSidebar(w.buildThreadList())
	inner.SetContent(w.buildReader())

	outer := adw.NewNavigationSplitView()
	outer.SetMinSidebarWidth(220)
	outer.SetMaxSidebarWidth(300)
	outer.SetSidebar(w.buildSidebar())
	outer.SetContent(adw.NewNavigationPage(inner, "Mail"))

	w.win.SetContent(outer)
}

func (w *window) present() {
	w.win.SetVisible(true)
	w.loadLabels()
	w.subscribe()
	w.selectLabel(w.current)

	// Optionally open the newest message on launch (off by default).
	if os.Getenv("MAILBOX_OPEN_FIRST") == "1" {
		if row := w.threadBox.RowAtIndex(0); row != nil {
			w.threadBox.SelectRow(row)
		}
	}
}

func (w *window) buildSidebar() *adw.NavigationPage {
	w.labelBox = gtk.NewListBox()
	w.labelBox.AddCSSClass("navigation-sidebar")
	w.labelBox.ConnectRowSelected(func(row *gtk.ListBoxRow) {
		if row == nil {
			return
		}
		if i := row.Index(); i >= 0 && i < len(w.labels) {
			w.selectLabel(w.labels[i].GmailID)
		}
	})

	scroller := gtk.NewScrolledWindow()
	scroller.SetVExpand(true)
	scroller.SetChild(w.labelBox)

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	acct := gtk.NewLabel(w.deps.AccountEmail)
	acct.AddCSSClass("heading")
	acct.SetXAlign(0)
	setMargins(acct, 12, 12, 12, 6)
	box.Append(acct)
	box.Append(scroller)

	hb := adw.NewHeaderBar()
	newBtn := gtk.NewButtonFromIconName("mail-message-new-symbolic")
	newBtn.SetTooltipText("New message")
	newBtn.SetSensitive(w.deps.Send != nil)
	newBtn.ConnectClicked(func() {
		w.openCompose(model.OutgoingMessage{}, "", "New message")
	})
	hb.PackStart(newBtn)

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(box)
	return adw.NewNavigationPage(tv, "Mailbox")
}

func (w *window) buildThreadList() *adw.NavigationPage {
	w.threadBox = gtk.NewListBox()
	w.threadBox.ConnectRowSelected(func(row *gtk.ListBoxRow) {
		if row == nil {
			return
		}
		if i := row.Index(); i >= 0 && i < len(w.msgs) {
			w.showMessage(w.msgs[i])
		}
	})

	scroller := gtk.NewScrolledWindow()
	scroller.SetVExpand(true)
	scroller.SetHExpand(true)
	scroller.SetChild(w.threadBox)

	tv := adw.NewToolbarView()
	tv.AddTopBar(adw.NewHeaderBar())
	tv.SetContent(scroller)
	return adw.NewNavigationPage(tv, "Messages")
}

func (w *window) buildReader() *adw.NavigationPage {
	w.webview = webkit.NewWebView()
	settings := w.webview.Settings()
	settings.SetEnableJavascript(false)
	settings.SetAutoLoadImages(false)
	w.webview.SetVExpand(true)
	w.webview.SetHExpand(true)

	w.header = gtk.NewLabel("")
	w.header.SetXAlign(0)
	w.header.SetWrap(true)
	setMargins(w.header, 12, 12, 8, 8)

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.Append(w.header)
	box.Append(w.webview)

	hb := adw.NewHeaderBar()

	w.replyBtn = gtk.NewButtonFromIconName("mail-reply-sender-symbolic")
	w.replyBtn.SetTooltipText("Reply")
	w.replyBtn.ConnectClicked(w.onReply)

	w.forwardBtn = gtk.NewButtonFromIconName("mail-forward-symbolic")
	w.forwardBtn.SetTooltipText("Forward")
	w.forwardBtn.ConnectClicked(w.onForward)

	w.archiveBtn = gtk.NewButtonFromIconName("mail-archive-symbolic")
	w.archiveBtn.SetTooltipText("Archive")
	w.archiveBtn.ConnectClicked(w.onArchive)

	w.unreadBtn = gtk.NewButtonFromIconName("mail-mark-unread-symbolic")
	w.unreadBtn.SetTooltipText("Mark unread")
	w.unreadBtn.ConnectClicked(w.onMarkUnread)

	w.starBtn = gtk.NewToggleButton()
	w.starBtn.SetIconName("starred-symbolic")
	w.starBtn.SetTooltipText("Star")
	w.starBtn.ConnectToggled(w.onToggleStar)

	w.imagesBtn = gtk.NewToggleButton()
	w.imagesBtn.SetIconName("image-x-generic-symbolic")
	w.imagesBtn.SetTooltipText("Show remote images")
	w.imagesBtn.ConnectToggled(w.onToggleImages)

	w.translateBtn = gtk.NewButtonWithLabel("Translate")
	w.translateBtn.SetTooltipText("Translate this email to English")
	w.translateBtn.ConnectClicked(w.onTranslate)

	w.draftBtn = gtk.NewButtonWithLabel("Draft reply")
	w.draftBtn.SetTooltipText("Draft a reply with AI")
	w.draftBtn.ConnectClicked(w.onDraftReply)

	hb.PackStart(w.replyBtn)
	hb.PackStart(w.forwardBtn)
	hb.PackStart(w.archiveBtn)
	hb.PackStart(w.unreadBtn)
	hb.PackStart(w.starBtn)
	hb.PackEnd(w.imagesBtn)
	hb.PackEnd(w.draftBtn)
	hb.PackEnd(w.translateBtn)
	w.setActionsSensitive(false)

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(box)
	return adw.NewNavigationPage(tv, "Reader")
}

func (w *window) setActionsSensitive(on bool) {
	canModify := on && w.deps.ModifyLabels != nil
	w.archiveBtn.SetSensitive(canModify)
	w.unreadBtn.SetSensitive(canModify)
	w.starBtn.SetSensitive(canModify)
	canSend := on && w.deps.Send != nil
	w.replyBtn.SetSensitive(canSend)
	w.forwardBtn.SetSensitive(canSend)
	canAI := on && w.deps.Assistant != nil
	w.translateBtn.SetSensitive(canAI)
	w.draftBtn.SetSensitive(canAI)
}

func (w *window) onReply() {
	m := w.openMsg
	if m.GmailID == "" {
		return
	}
	init := model.OutgoingMessage{
		To:         m.FromAddr,
		Subject:    ensureRePrefix(m.Subject),
		InReplyTo:  m.RFC822MsgID,
		References: strings.TrimSpace(m.References + " " + m.RFC822MsgID),
		ThreadID:   m.ThreadID,
	}
	w.openCompose(init, w.threadContextFor(m), "Reply")
}

func (w *window) onForward() {
	m := w.openMsg
	if m.GmailID == "" {
		return
	}
	init := model.OutgoingMessage{
		Subject: ensureFwdPrefix(m.Subject),
		Body:    quoteOriginal(m, w.bodyTextFor(m)),
	}
	w.openCompose(init, "", "Forward")
}

func (w *window) loadLabels() {
	ctx := context.Background()
	labels, err := w.deps.Store.ListLabels(ctx, w.deps.AccountID)
	if err != nil {
		slog.Error("ui: load labels", "err", err)
		return
	}
	w.labels = labels
	w.labelBox.RemoveAll()
	for _, l := range labels {
		n, _ := w.deps.Store.CountByLabel(ctx, w.deps.AccountID, l.GmailID)
		w.labelBox.Append(labelRow(l.Name, n))
	}
}

func (w *window) selectLabel(labelID string) {
	w.current = labelID
	msgs, err := w.deps.Store.ListByLabel(context.Background(), w.deps.AccountID, labelID, 500, 0)
	if err != nil {
		slog.Error("ui: list by label", "label", labelID, "err", err)
		return
	}
	w.msgs = msgs
	w.threadBox.RemoveAll()
	for _, m := range msgs {
		w.threadBox.Append(threadRow(m))
	}
}

func (w *window) showMessage(m model.Message) {
	w.openMsg = m
	w.setActionsSensitive(true)

	// Reflect the starred state without re-triggering the toggle handler.
	w.updatingStar = true
	w.starBtn.SetActive(m.IsStarred)
	w.updatingStar = false

	w.renderBody(m)

	// Opening an unread message marks it read (standard mail behaviour).
	if m.IsUnread && w.deps.ModifyLabels != nil {
		go func() {
			if err := w.deps.ModifyLabels(context.Background(), m.AccountID, m.GmailID, nil, []string{model.LabelUnread}); err != nil {
				slog.Warn("ui: mark read", "id", m.GmailID, "err", err)
				return
			}
			dispatch.Main(w.loadLabels)
		}()
	}
}

// renderBody shows the header and body for m, fetching the body on demand.
func (w *window) renderBody(m model.Message) {
	w.header.SetMarkup(fmt.Sprintf("<b>%s</b>\n%s",
		html.EscapeString(m.Subject), html.EscapeString(displayFrom(m))))

	if body, err := w.deps.Store.GetBody(context.Background(), m.RowID); err == nil && (body.HTML != "" || body.Text != "") {
		w.loadBody(body)
		return
	}

	if w.deps.FetchBody == nil {
		w.webview.LoadHtml(wrapHTML("<p>"+html.EscapeString(m.Snippet)+"</p>"), "about:blank")
		return
	}

	w.webview.LoadHtml(wrapHTML("<p><i>Loading…</i></p>"), "about:blank")
	go func() {
		err := w.deps.FetchBody(context.Background(), m.AccountID, m.GmailID)
		dispatch.Main(func() {
			if err != nil {
				slog.Warn("ui: fetch body", "id", m.GmailID, "err", err)
				w.webview.LoadHtml(wrapHTML("<p>Could not load this message.</p>"), "about:blank")
				return
			}
			body, _ := w.deps.Store.GetBody(context.Background(), m.RowID)
			w.loadBody(body)
		})
	}()
}

func (w *window) onArchive() {
	if w.openMsg.GmailID != "" {
		w.runAction(w.openMsg, nil, []string{model.LabelInbox})
	}
}

func (w *window) onMarkUnread() {
	if w.openMsg.GmailID != "" {
		w.runAction(w.openMsg, []string{model.LabelUnread}, nil)
	}
}

func (w *window) onToggleStar() {
	if w.updatingStar || w.openMsg.GmailID == "" {
		return
	}
	if w.starBtn.Active() {
		w.runAction(w.openMsg, []string{model.LabelStarred}, nil)
	} else {
		w.runAction(w.openMsg, nil, []string{model.LabelStarred})
	}
}

func (w *window) onToggleImages() {
	w.webview.Settings().SetAutoLoadImages(w.imagesBtn.Active())
	if w.openMsg.GmailID != "" {
		w.renderBody(w.openMsg)
	}
}

func (w *window) onTranslate() {
	m := w.openMsg
	if m.GmailID == "" || w.deps.Assistant == nil {
		return
	}
	body := w.bodyTextFor(m)
	w.showAIStream("Translate to English", func(ctx context.Context) (<-chan ai.Chunk, error) {
		return w.deps.Assistant.Translate(ctx, body, "English")
	})
}

func (w *window) onDraftReply() {
	m := w.openMsg
	if m.GmailID == "" || w.deps.Assistant == nil {
		return
	}
	thread := w.threadContextFor(m)
	w.showAIStream("Draft reply", func(ctx context.Context) (<-chan ai.Chunk, error) {
		return w.deps.Assistant.DraftReply(ctx, thread, "")
	})
}

// bodyTextFor returns the best plain-text representation of a message for AI input.
func (w *window) bodyTextFor(m model.Message) string {
	if b, err := w.deps.Store.GetBody(context.Background(), m.RowID); err == nil && b.Text != "" {
		return b.Text
	}
	return m.Snippet
}

func (w *window) threadContextFor(m model.Message) string {
	return fmt.Sprintf("From: %s\nSubject: %s\n\n%s", displayFrom(m), m.Subject, w.bodyTextFor(m))
}

// showAIStream opens a window that streams an AI response token-by-token into an
// editable text view. Stop or closing the window cancels the request.
func (w *window) showAIStream(title string, start func(ctx context.Context) (<-chan ai.Chunk, error)) {
	ctx, cancel := context.WithCancel(context.Background())

	tv := gtk.NewTextView()
	tv.SetWrapMode(gtk.WrapWord)
	tv.SetEditable(true)
	setMargins(tv, 12, 12, 12, 12)
	buf := tv.Buffer()
	buf.SetText("…")

	scroller := gtk.NewScrolledWindow()
	scroller.SetVExpand(true)
	scroller.SetHExpand(true)
	scroller.SetChild(tv)

	hb := adw.NewHeaderBar()
	stop := gtk.NewButtonWithLabel("Stop")
	stop.ConnectClicked(func() { cancel() })
	hb.PackEnd(stop)

	tvw := adw.NewToolbarView()
	tvw.AddTopBar(hb)
	tvw.SetContent(scroller)

	win := adw.NewWindow()
	win.SetTitle(title)
	win.SetDefaultSize(560, 480)
	win.SetContent(tvw)
	win.ConnectCloseRequest(func() bool {
		cancel()
		return false
	})
	win.SetVisible(true)

	go func() {
		ch, err := start(ctx)
		if err != nil {
			msg := err.Error()
			dispatch.Main(func() { buf.SetText("Error: " + msg) })
			return
		}
		var acc strings.Builder
		first := true
		for c := range ch {
			cc := c
			dispatch.Main(func() {
				if first {
					acc.Reset()
					first = false
				}
				if cc.Err != nil {
					acc.WriteString("\n[error: " + cc.Err.Error() + "]")
				} else {
					acc.WriteString(cc.Text)
				}
				buf.SetText(acc.String())
			})
		}
	}()
}

// runAction applies a label change in the background, then refreshes the label
// counts and the current message list.
func (w *window) runAction(m model.Message, add, remove []string) {
	if w.deps.ModifyLabels == nil {
		return
	}
	go func() {
		err := w.deps.ModifyLabels(context.Background(), m.AccountID, m.GmailID, add, remove)
		dispatch.Main(func() {
			if err != nil {
				slog.Warn("ui: action", "id", m.GmailID, "err", err)
			}
			w.loadLabels()
			w.selectLabel(w.current)
		})
	}()
}

func (w *window) loadBody(b model.MessageBody) {
	if b.HTML != "" {
		w.webview.LoadHtml(wrapHTML(w.sanitizer.Sanitize(b.HTML)), "about:blank")
		return
	}
	w.webview.LoadHtml(wrapHTML("<pre style=\"white-space:pre-wrap\">"+html.EscapeString(b.Text)+"</pre>"), "about:blank")
}

// subscribe refreshes label counts when the sync engine reports changes. The
// thread list is left intact so an open message isn't disrupted; re-selecting a
// label reloads it.
func (w *window) subscribe() {
	if w.deps.Hub == nil {
		return
	}
	ch, _ := w.deps.Hub.Subscribe()
	go func() {
		for range ch {
			dispatch.Main(w.loadLabels)
		}
	}()
}

func labelRow(name string, count int) *gtk.Box {
	box := gtk.NewBox(gtk.OrientationHorizontal, 6)
	setMargins(box, 12, 12, 4, 4)
	n := gtk.NewLabel(name)
	n.SetXAlign(0)
	n.SetHExpand(true)
	box.Append(n)
	if count > 0 {
		c := gtk.NewLabel(fmt.Sprintf("%d", count))
		c.AddCSSClass("dim-label")
		box.Append(c)
	}
	return box
}

func threadRow(m model.Message) *gtk.Box {
	box := gtk.NewBox(gtk.OrientationVertical, 2)
	setMargins(box, 12, 12, 6, 6)

	top := gtk.NewBox(gtk.OrientationHorizontal, 6)
	from := gtk.NewLabel(displayFrom(m))
	from.SetXAlign(0)
	from.SetHExpand(true)
	if m.IsUnread {
		from.AddCSSClass("heading")
	}
	top.Append(from)
	if !m.InternalDate.IsZero() {
		date := gtk.NewLabel(m.InternalDate.Format("Jan 2"))
		date.AddCSSClass("dim-label")
		top.Append(date)
	}
	box.Append(top)

	subj := gtk.NewLabel(m.Subject)
	subj.SetXAlign(0)
	if !m.IsUnread {
		subj.AddCSSClass("dim-label")
	}
	box.Append(subj)
	return box
}

func displayFrom(m model.Message) string {
	if m.FromName != "" {
		return m.FromName
	}
	return m.FromAddr
}

func setMargins(w gtk.Widgetter, start, end, top, bottom int) {
	base := gtk.BaseWidget(w)
	base.SetMarginStart(start)
	base.SetMarginEnd(end)
	base.SetMarginTop(top)
	base.SetMarginBottom(bottom)
}

func wrapHTML(inner string) string {
	return `<!doctype html><html><head><meta charset="utf-8">` +
		`<style>body{font-family:sans-serif;margin:16px;color:#222;line-height:1.4}` +
		`pre{font-family:monospace}</style></head><body>` + inner + `</body></html>`
}
