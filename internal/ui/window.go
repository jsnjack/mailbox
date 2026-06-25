package ui

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net/mail"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	webkit "github.com/diamondburned/gotk4-webkitgtk/pkg/webkit/v6"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/syncer"
	"github.com/microcosm-cc/bluemonday"
)

func newAdwApplication() *adw.Application {
	return adw.NewApplication(appID, gio.ApplicationFlagsNone)
}

// window owns the widget tree and the currently displayed selection.
type window struct {
	app  *adw.Application
	deps Deps

	win        *adw.ApplicationWindow
	outerSplit *adw.NavigationSplitView
	innerSplit *adw.NavigationSplitView
	labelBox   *gtk.ListBox
	labels     []model.Label
	current    string
	startTime  time.Time // only mail arriving after this triggers notifications

	// virtualized thread list: a StringList of gmail ids drives a ListView; the
	// factory builds row widgets only for visible items, looked up in msgByID.
	threadModel    *gtk.StringList
	threadSel      *gtk.SingleSelection
	threadView     *gtk.ListView
	threadStack    *gtk.Stack // "list" vs "empty" placeholder
	readerStack    *gtk.Stack // "message" vs "empty" placeholder
	searchEntry    *gtk.SearchEntry
	suppressSearch bool // guards SetText from firing a search during label switch
	msgByID        map[string]model.Message

	header    *gtk.Label
	attachBox *gtk.Box // chips for the open message's attachments
	webview   *webkit.WebView
	sanitizer *bluemonday.Policy

	// reader actions
	openMsg      model.Message // the message currently shown in the reader
	replyBtn     *gtk.Button
	replyAllBtn  *gtk.Button
	forwardBtn   *gtk.Button
	archiveBtn   *gtk.Button
	trashBtn     *gtk.Button
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
		startTime: time.Now(),
		sanitizer: bluemonday.UGCPolicy(),
	}
	w.build()
	return w
}

func (w *window) build() {
	w.win = adw.NewApplicationWindow(&w.app.Application)
	w.win.SetTitle("Mailbox")
	winW, winH := 1200, 760
	if s := os.Getenv("MAILBOX_WIN_SIZE"); s != "" {
		if _, err := fmt.Sscanf(s, "%dx%d", &winW, &winH); err != nil {
			winW, winH = 1200, 760
		}
	}
	w.win.SetDefaultSize(winW, winH)

	w.innerSplit = adw.NewNavigationSplitView()
	w.innerSplit.SetMinSidebarWidth(340)
	w.innerSplit.SetMaxSidebarWidth(520)
	w.innerSplit.SetSidebar(w.buildThreadList())
	w.innerSplit.SetContent(w.buildReader())

	w.outerSplit = adw.NewNavigationSplitView()
	w.outerSplit.SetMinSidebarWidth(220)
	w.outerSplit.SetMaxSidebarWidth(300)
	w.outerSplit.SetSidebar(w.buildSidebar())
	w.outerSplit.SetContent(adw.NewNavigationPage(w.innerSplit, "Mail"))

	w.win.SetContent(w.outerSplit)
	w.addBreakpoints()
}

// addBreakpoints collapses the panes as the window narrows: below ~860sp the
// accounts sidebar collapses (list + reader), and below ~520sp the thread list
// collapses too (single pane with back navigation).
func (w *window) addBreakpoints() {
	medium := adw.NewBreakpoint(adw.NewBreakpointConditionLength(
		adw.BreakpointConditionMaxWidth, 860, adw.LengthUnitSp))
	medium.AddSetter(w.outerSplit, "collapsed", coreglib.NewValue(true))
	w.win.AddBreakpoint(medium)

	narrow := adw.NewBreakpoint(adw.NewBreakpointConditionLength(
		adw.BreakpointConditionMaxWidth, 520, adw.LengthUnitSp))
	narrow.AddSetter(w.outerSplit, "collapsed", coreglib.NewValue(true))
	narrow.AddSetter(w.innerSplit, "collapsed", coreglib.NewValue(true))
	w.win.AddBreakpoint(narrow)
}

func (w *window) present() {
	w.win.SetVisible(true)
	w.loadLabels()
	w.subscribe()
	w.selectLabel(w.current)

	// Test hooks (off by default).
	if q := os.Getenv("MAILBOX_SEARCH"); q != "" {
		w.searchEntry.SetText(q) // fires the search-changed handler
	}
	if os.Getenv("MAILBOX_OPEN_FIRST") == "1" && w.threadModel.NItems() > 0 {
		w.threadSel.SetSelected(0)
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

	refreshBtn := gtk.NewButtonFromIconName("view-refresh-symbolic")
	refreshBtn.SetTooltipText("Sync now")
	refreshBtn.SetSensitive(w.deps.Sync != nil)
	refreshBtn.ConnectClicked(w.onRefresh)
	hb.PackEnd(refreshBtn)

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(box)
	return adw.NewNavigationPage(tv, "Mailbox")
}

func (w *window) buildThreadList() *adw.NavigationPage {
	w.msgByID = make(map[string]model.Message)
	w.threadModel = gtk.NewStringList(nil)
	w.threadSel = gtk.NewSingleSelection(w.threadModel)
	w.threadSel.SetAutoselect(false)
	w.threadSel.SetCanUnselect(true)
	w.threadSel.ConnectSelectionChanged(func(position, nItems uint) {
		w.onThreadSelected()
	})

	factory := gtk.NewSignalListItemFactory()
	factory.ConnectBind(func(obj *coreglib.Object) {
		li, ok := obj.Cast().(*gtk.ListItem)
		if !ok {
			return
		}
		so, ok := li.Item().Cast().(*gtk.StringObject)
		if !ok {
			return
		}
		li.SetChild(threadRow(w.msgByID[so.String()]))
	})

	w.threadView = gtk.NewListView(w.threadSel, &factory.ListItemFactory)
	w.threadView.SetVExpand(true)
	w.threadView.SetHExpand(true)

	scroller := gtk.NewScrolledWindow()
	scroller.SetVExpand(true)
	scroller.SetHExpand(true)
	scroller.SetChild(w.threadView)

	empty := adw.NewStatusPage()
	empty.SetIconName("mail-archive-symbolic")
	empty.SetTitle("No messages")
	empty.SetDescription("This label has no messages in the local cache.")

	w.threadStack = gtk.NewStack()
	w.threadStack.SetVExpand(true)
	w.threadStack.AddNamed(scroller, "list")
	w.threadStack.AddNamed(empty, "empty")
	w.threadStack.SetVisibleChildName("list")

	w.searchEntry = gtk.NewSearchEntry()
	w.searchEntry.SetPlaceholderText("Search cached messages")
	setMargins(w.searchEntry, 6, 6, 6, 6)
	w.searchEntry.ConnectSearchChanged(w.onSearchChanged)

	content := gtk.NewBox(gtk.OrientationVertical, 0)
	content.Append(w.searchEntry)
	content.Append(w.threadStack)

	tv := adw.NewToolbarView()
	tv.AddTopBar(adw.NewHeaderBar())
	tv.SetContent(content)
	return adw.NewNavigationPage(tv, "Messages")
}

func (w *window) onSearchChanged() {
	if w.suppressSearch {
		return
	}
	w.refreshList(w.searchEntry.Text())
}

// refreshList populates the thread list from either the current label (blank
// query) or a full-text search.
func (w *window) refreshList(query string) {
	ctx := context.Background()
	var (
		msgs []model.Message
		err  error
	)
	if strings.TrimSpace(query) == "" {
		msgs, err = w.deps.Store.ListByLabel(ctx, w.deps.AccountID, w.current, threadListCap, 0)
	} else {
		msgs, err = w.deps.Store.Search(ctx, w.deps.AccountID, query, threadListCap)
	}
	if err != nil {
		slog.Error("ui: refresh list", "query", query, "err", err)
		return
	}
	w.showMessages(msgs)
}

// showMessages replaces the thread list contents.
func (w *window) showMessages(msgs []model.Message) {
	w.msgByID = make(map[string]model.Message, len(msgs))
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.GmailID
		w.msgByID[m.GmailID] = m
	}
	w.threadModel.Splice(0, w.threadModel.NItems(), ids)
	if len(msgs) == 0 {
		w.threadStack.SetVisibleChildName("empty")
	} else {
		w.threadStack.SetVisibleChildName("list")
	}
}

func (w *window) onRefresh() {
	if w.deps.Sync == nil {
		return
	}
	go func() {
		if err := w.deps.Sync(context.Background()); err != nil {
			slog.Warn("ui: sync now", "err", err)
			return
		}
		dispatch.Main(func() {
			w.loadLabels()
			w.refreshList(w.searchEntry.Text())
		})
	}()
}

func (w *window) onThreadSelected() {
	item := w.threadSel.SelectedItem()
	if item == nil {
		return
	}
	so, ok := item.Cast().(*gtk.StringObject)
	if !ok {
		return
	}
	if m, ok := w.msgByID[so.String()]; ok {
		w.showMessage(m)
	}
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

	w.attachBox = gtk.NewBox(gtk.OrientationHorizontal, 6)
	setMargins(w.attachBox, 12, 12, 0, 8)
	w.attachBox.SetVisible(false)

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.Append(w.header)
	box.Append(w.attachBox)
	box.Append(w.webview)

	empty := adw.NewStatusPage()
	empty.SetIconName("mail-unread-symbolic")
	empty.SetTitle("No message selected")
	empty.SetDescription("Choose a message from the list to read it here.")

	w.readerStack = gtk.NewStack()
	w.readerStack.AddNamed(empty, "empty")
	w.readerStack.AddNamed(box, "message")
	w.readerStack.SetVisibleChildName("empty")

	hb := adw.NewHeaderBar()

	w.replyBtn = gtk.NewButtonFromIconName("mail-reply-sender-symbolic")
	w.replyBtn.SetTooltipText("Reply")
	w.replyBtn.ConnectClicked(w.onReply)

	w.replyAllBtn = gtk.NewButtonFromIconName("mail-reply-all-symbolic")
	w.replyAllBtn.SetTooltipText("Reply all")
	w.replyAllBtn.ConnectClicked(w.onReplyAll)

	w.forwardBtn = gtk.NewButtonFromIconName("mail-forward-symbolic")
	w.forwardBtn.SetTooltipText("Forward")
	w.forwardBtn.ConnectClicked(w.onForward)

	w.archiveBtn = gtk.NewButtonFromIconName("mail-archive-symbolic")
	w.archiveBtn.SetTooltipText("Archive")
	w.archiveBtn.ConnectClicked(w.onArchive)

	w.trashBtn = gtk.NewButtonFromIconName("user-trash-symbolic")
	w.trashBtn.SetTooltipText("Move to Trash")
	w.trashBtn.ConnectClicked(w.onTrash)

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
	hb.PackStart(w.replyAllBtn)
	hb.PackStart(w.forwardBtn)
	hb.PackStart(w.archiveBtn)
	hb.PackStart(w.trashBtn)
	hb.PackStart(w.unreadBtn)
	hb.PackStart(w.starBtn)
	hb.PackEnd(w.imagesBtn)
	hb.PackEnd(w.draftBtn)
	hb.PackEnd(w.translateBtn)
	w.setActionsSensitive(false)

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(w.readerStack)
	return adw.NewNavigationPage(tv, "Reader")
}

func (w *window) setActionsSensitive(on bool) {
	canModify := on && w.deps.ModifyLabels != nil
	w.archiveBtn.SetSensitive(canModify)
	w.trashBtn.SetSensitive(canModify)
	w.unreadBtn.SetSensitive(canModify)
	w.starBtn.SetSensitive(canModify)
	canSend := on && w.deps.Send != nil
	w.replyBtn.SetSensitive(canSend)
	w.replyAllBtn.SetSensitive(canSend)
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
		Body:       quoteOriginal(m, w.bodyTextFor(m)),
		InReplyTo:  m.RFC822MsgID,
		References: strings.TrimSpace(m.References + " " + m.RFC822MsgID),
		ThreadID:   m.ThreadID,
	}
	w.openCompose(init, w.threadContextFor(m), "Reply")
}

func (w *window) onReplyAll() {
	m := w.openMsg
	if m.GmailID == "" {
		return
	}
	to, cc := replyAllRecipients(m, w.deps.AccountEmail)
	init := model.OutgoingMessage{
		To:         to,
		Cc:         cc,
		Subject:    ensureRePrefix(m.Subject),
		Body:       quoteOriginal(m, w.bodyTextFor(m)),
		InReplyTo:  m.RFC822MsgID,
		References: strings.TrimSpace(m.References + " " + m.RFC822MsgID),
		ThreadID:   m.ThreadID,
	}
	w.openCompose(init, w.threadContextFor(m), "Reply all")
}

// replyAllRecipients computes To (original sender + original To) and Cc (original
// Cc), excluding the account's own address and de-duplicating.
func replyAllRecipients(m model.Message, self string) (to, cc string) {
	seen := map[string]bool{strings.ToLower(strings.TrimSpace(self)): true}
	collect := func(raw string) []string {
		addrs, err := mail.ParseAddressList(raw)
		if err != nil {
			return nil
		}
		var out []string
		for _, a := range addrs {
			key := strings.ToLower(a.Address)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, a.Address)
		}
		return out
	}
	toList := append(collect(m.FromAddr), collect(m.ToAddrs)...)
	ccList := collect(m.CcAddrs)
	return strings.Join(toList, ", "), strings.Join(ccList, ", ")
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

// threadListCap bounds how many messages a label loads at once. The ListView
// virtualizes row widgets, so this only bounds metadata held in memory; a truly
// windowed model (paging on scroll) is a further optimization.
const threadListCap = 5000

func (w *window) selectLabel(labelID string) {
	w.current = labelID
	// Switching label clears any active search without re-triggering it.
	w.suppressSearch = true
	w.searchEntry.SetText("")
	w.suppressSearch = false
	w.refreshList("")
	// When collapsed, reveal the thread list for the chosen label.
	w.outerSplit.SetShowContent(true)
}

func (w *window) showMessage(m model.Message) {
	w.openMsg = m
	w.setActionsSensitive(true)
	w.readerStack.SetVisibleChildName("message")
	// When collapsed, navigate to the reader.
	w.innerSplit.SetShowContent(true)

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
		w.populateAttachments(m)
		return
	}

	if w.deps.FetchBody == nil {
		w.webview.LoadHtml(wrapHTML("<p>"+html.EscapeString(m.Snippet)+"</p>"), "about:blank")
		w.populateAttachments(m)
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
			w.populateAttachments(m)
		})
	}()
}

// populateAttachments rebuilds the attachment chip bar for the open message.
func (w *window) populateAttachments(m model.Message) {
	for child := w.attachBox.FirstChild(); child != nil; child = w.attachBox.FirstChild() {
		w.attachBox.Remove(child)
	}
	atts, err := w.deps.Store.ListAttachments(context.Background(), m.RowID)
	if err != nil {
		slog.Warn("ui: list attachments", "id", m.GmailID, "err", err)
	}
	if len(atts) == 0 || w.deps.OpenAttach == nil {
		w.attachBox.SetVisible(false)
		return
	}
	for _, a := range atts {
		att := a
		btn := gtk.NewButton()
		btn.SetChild(attachmentChip(att))
		btn.SetTooltipText(att.MimeType)
		btn.ConnectClicked(func() { w.openAttachment(m.GmailID, att.ID) })
		w.attachBox.Append(btn)
	}
	w.attachBox.SetVisible(true)
}

func (w *window) openAttachment(gmailID string, attID int64) {
	if w.deps.OpenAttach == nil {
		return
	}
	go func() {
		path, err := w.deps.OpenAttach(context.Background(), gmailID, attID)
		if err != nil {
			slog.Warn("ui: open attachment", "id", gmailID, "err", err)
			return
		}
		if err := exec.Command("xdg-open", path).Start(); err != nil {
			slog.Warn("ui: xdg-open", "path", path, "err", err)
		}
	}()
}

func attachmentChip(a model.Attachment) *gtk.Box {
	box := gtk.NewBox(gtk.OrientationHorizontal, 4)
	box.Append(gtk.NewImageFromIconName("mail-attachment-symbolic"))
	box.Append(gtk.NewLabel(a.Filename))
	return box
}

func (w *window) onArchive() {
	if w.openMsg.GmailID != "" {
		w.runAction(w.openMsg, nil, []string{model.LabelInbox})
	}
}

func (w *window) onTrash() {
	if w.openMsg.GmailID != "" {
		w.runAction(w.openMsg, []string{model.LabelTrash}, []string{model.LabelInbox})
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
		for c := range ch {
			c := c
			dispatch.Main(func() { w.onChange(c) })
		}
	}()
}

// onChange refreshes label counts and notifies for genuinely new inbox mail.
func (w *window) onChange(c syncer.Change) {
	w.loadLabels()
	if c.Kind != syncer.MessageUpserted || c.GmailID == "" {
		return
	}
	m, err := w.deps.Store.GetMessage(context.Background(), c.AccountID, c.GmailID)
	if err != nil || !m.IsUnread || !m.InternalDate.After(w.startTime) {
		return
	}
	for _, l := range m.Labels {
		if l == model.LabelInbox {
			w.notifyNewMail(m)
			return
		}
	}
}

func (w *window) notifyNewMail(m model.Message) {
	n := gio.NewNotification("New mail")
	body := displayFrom(m)
	if m.Subject != "" {
		body += " — " + m.Subject
	}
	n.SetBody(body)
	w.app.SendNotification("mailbox-new-mail", n)
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
