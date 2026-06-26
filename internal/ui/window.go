package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"log/slog"
	"net/mail"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	webkit "github.com/diamondburned/gotk4-webkitgtk/pkg/webkit/v6"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/jsnjack/mailbox/internal/config"
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

	win          *adw.ApplicationWindow
	toastOverlay *adw.ToastOverlay
	outerSplit   *adw.NavigationSplitView
	innerSplit   *adw.NavigationSplitView
	accountBox   *gtk.ListBox
	labelBox     *gtk.ListBox
	refreshBtn   *gtk.Button
	syncSpinner  *gtk.Spinner  // shown in place of refreshBtn during a manual sync
	sidebar      []sidebarItem // one entry per row in labelBox (incl. headings)
	current      string
	activeID     int64 // the account currently shown
	activeEmail  string
	// suppressLabelSelect guards the row-selected handler while loadLabels
	// restores the visual highlight, so a background refresh doesn't reset the
	// list or clear an active search.
	suppressLabelSelect bool
	startTime           time.Time // only mail arriving after this triggers notifications

	// virtualized list grouped by conversation: a StringList of thread ids drives
	// a ListView; the factory builds visible rows from threadByID.
	threadModel    *gtk.StringList
	threadSel      *gtk.SingleSelection
	threadView     *gtk.ListView
	threadStack    *gtk.Stack // "list" vs "empty" placeholder
	readerStack    *gtk.Stack // "message" vs "empty" placeholder
	markReadBtn    *gtk.Button
	readOnlyBanner *adw.Banner // revealed when no Gmail client (live features off)
	outboxBanner   *adw.Banner // revealed when sends are queued/failed
	searchEntry    *gtk.SearchEntry
	suppressSearch bool // guards SetText from firing a search during label switch
	threadByID     map[string]model.ThreadSummary

	header    *gtk.Label
	attachBox *gtk.Box // chips for the open message's attachments
	webview   *webkit.WebView
	sanitizer *bluemonday.Policy

	// reader: the open conversation. openMsg is its newest message (used for
	// reply/forward/star/unread); openThreadMsgs is all of them (oldest first).
	openThreadID   string
	openThreadMsgs []model.Message
	openMsg        model.Message
	replyBtn       *gtk.Button
	replyAllBtn    *gtk.Button
	forwardBtn     *gtk.Button
	archiveBtn     *gtk.Button
	trashBtn       *gtk.Button
	labelsBtn      *gtk.MenuButton
	starBtn        *gtk.ToggleButton
	translateBtn   *gtk.Button
	draftBtn       *gtk.Button
	overflowBtn    *gtk.MenuButton // "more actions" menu in the reader header
	readerMenuPop  *gtk.Popover    // overflow menu content (built lazily)
	imagesEnabled  bool            // whether remote images are loaded in the reader

	// in-place translation: a banner offers reverting to the original; the cancel
	// func aborts an in-flight translation when the user reverts or switches mail.
	translationBanner *adw.Banner
	translateCancel   context.CancelFunc

	updatingStar bool // guards programmatic star-toggle from firing the handler
}

func newWindow(app *adw.Application, deps Deps) *window {
	w := &window{
		app:       app,
		deps:      deps,
		current:   model.LabelInbox,
		startTime: time.Now(),
		sanitizer: emailPolicy(),
	}
	if len(deps.Accounts) > 0 {
		w.activeID = deps.Accounts[0].ID
		w.activeEmail = deps.Accounts[0].Email
	}
	w.build()
	return w
}

func (w *window) build() {
	w.win = adw.NewApplicationWindow(&w.app.Application)
	w.win.SetTitle("Mailbox")
	// Size precedence: env override (test hook) > last-remembered size > default.
	winW, winH := 1280, 800
	if st, err := config.LoadWindowState(); err == nil && st.Width >= 400 && st.Height >= 300 {
		winW, winH = st.Width, st.Height
	}
	if s := os.Getenv("MAILBOX_WIN_SIZE"); s != "" {
		var ew, eh int
		if _, err := fmt.Sscanf(s, "%dx%d", &ew, &eh); err == nil {
			winW, winH = ew, eh
		}
	}
	w.win.SetDefaultSize(winW, winH)
	// Remember the size on close (skip while maximized so we keep the windowed
	// dimensions rather than the full-screen ones).
	w.win.ConnectCloseRequest(func() bool {
		if !w.win.IsMaximized() {
			if err := config.SaveWindowState(config.WindowState{Width: w.win.Width(), Height: w.win.Height()}); err != nil {
				slog.Warn("ui: save window state", "err", err)
			}
		}
		return false
	})

	// Keep the two sidebars compact so the reader gets the majority of the width
	// (HTML email is typically laid out for ~600px). NavigationSplitView sizes a
	// sidebar as fraction*total clamped to [min,max]; capping the maxes low is
	// what actually widens the reader on a roomy window.
	w.innerSplit = adw.NewNavigationSplitView()
	w.innerSplit.SetMinSidebarWidth(280)
	w.innerSplit.SetMaxSidebarWidth(360)
	w.innerSplit.SetSidebar(w.buildThreadList())
	w.innerSplit.SetContent(w.buildReader())

	w.outerSplit = adw.NewNavigationSplitView()
	w.outerSplit.SetMinSidebarWidth(200)
	w.outerSplit.SetMaxSidebarWidth(240)
	w.outerSplit.SetSidebar(w.buildSidebar())
	w.outerSplit.SetContent(adw.NewNavigationPage(w.innerSplit, "Mail"))

	w.toastOverlay = adw.NewToastOverlay()
	w.toastOverlay.SetChild(w.outerSplit)
	w.win.SetContent(w.toastOverlay)
	w.addBreakpoints()
	w.addShortcuts()
}

// addShortcuts wires single-key navigation/actions. The controller runs in the
// capture phase so the shortcut fires even when focus is inside the message
// WebView or the thread list (which would otherwise swallow the key); it bails
// out when a text field is focused so typing in search still works. Keyvals for
// printable keys equal their ASCII rune.
func (w *window) addShortcuts() {
	ec := gtk.NewEventControllerKey()
	ec.SetPropagationPhase(gtk.PhaseCapture)
	ec.ConnectKeyPressed(func(keyval, keycode uint, state gdk.ModifierType) bool {
		if state&(gdk.ControlMask|gdk.AltMask|gdk.SuperMask) != 0 {
			return false
		}
		switch w.win.Focus().(type) {
		case *gtk.Text, *gtk.TextView:
			return false // user is typing in a field; don't hijack the key
		}
		switch keyval {
		case 'j':
			w.selectAdjacent(1)
		case 'k':
			w.selectAdjacent(-1)
		case 'r':
			w.onReply()
		case 'f':
			w.onForward()
		case 'a', 'e':
			w.onArchive()
		case '#', gdk.KEY_Delete:
			w.onTrash()
		case 's':
			w.toggleStar()
		case 'c':
			if w.deps.Send != nil {
				w.openCompose(model.OutgoingMessage{}, "", "New message", false)
			}
		case '/':
			w.searchEntry.GrabFocus()
		case gdk.KEY_Escape:
			w.goBack()
		case '?':
			w.showShortcuts()
		default:
			return false
		}
		return true
	})
	w.win.AddController(ec)
}

// selectAdjacent moves the thread selection by delta, clamped to the list.
func (w *window) selectAdjacent(delta int) {
	n := int(w.threadModel.NItems())
	if n == 0 {
		return
	}
	const invalidPos = 0xffffffff // GTK_INVALID_LIST_POSITION
	next := 0
	if cur := w.threadSel.Selected(); cur != invalidPos {
		next = int(cur) + delta
	}
	if next < 0 {
		next = 0
	}
	if next >= n {
		next = n - 1
	}
	w.threadSel.SetSelected(uint(next))
}

// toggleStar flips the star on the open message via the toolbar button (which
// runs the optimistic label change). No-op when nothing is open.
func (w *window) toggleStar() {
	if w.openMsg.GmailID == "" {
		return
	}
	w.starBtn.SetActive(!w.starBtn.Active())
}

// goBack collapses the reader back to the thread list — meaningful when the
// window is narrow enough that the panes are stacked.
func (w *window) goBack() {
	w.innerSplit.SetShowContent(false)
}

// showConnectHelp explains how to enable live features when the app is running
// read-only (no Gmail client could be built).
func (w *window) showConnectHelp() {
	body := "Mailbox couldn't connect to Gmail, so it's showing the local cache " +
		"read-only.\n\n" +
		"1. Put your Google OAuth client secret at:\n" +
		"   ~/.config/mailbox/credentials.json\n\n" +
		"2. Connect the account (opens a browser to sign in):\n" +
		"   mailbox sync --account you@gmail.com\n\n" +
		"3. Restart Mailbox."
	dialog := adw.NewAlertDialog("Not connected to Gmail", body)
	dialog.AddResponse("ok", "Got it")
	dialog.SetDefaultResponse("ok")
	dialog.SetCloseResponse("ok")
	dialog.Present(w.win)
}

// showShortcuts presents a dialog listing the keyboard shortcuts.
func (w *window) showShortcuts() {
	rows := [][2]string{
		{"j / k", "Next / previous conversation"},
		{"r", "Reply"},
		{"f", "Forward"},
		{"a / e", "Archive"},
		{"# / Delete", "Move to Trash"},
		{"s", "Star / unstar"},
		{"c", "Compose"},
		{"/", "Search"},
		{"Esc", "Back to list"},
		{"?", "Keyboard shortcuts"},
	}
	grid := gtk.NewGrid()
	grid.SetRowSpacing(10)
	grid.SetColumnSpacing(24)
	setMargins(grid, 18, 18, 18, 18)
	for i, r := range rows {
		key := gtk.NewLabel(r[0])
		key.SetXAlign(1)
		key.AddCSSClass("heading")
		desc := gtk.NewLabel(r[1])
		desc.SetXAlign(0)
		desc.SetHExpand(true)
		grid.Attach(key, 0, i, 1, 1)
		grid.Attach(desc, 1, i, 1, 1)
	}

	tv := adw.NewToolbarView()
	tv.AddTopBar(adw.NewHeaderBar())
	tv.SetContent(grid)

	dialog := adw.NewDialog()
	dialog.SetTitle("Keyboard Shortcuts")
	dialog.SetChild(tv)
	dialog.SetContentWidth(380)
	dialog.SetFollowsContentSize(true)
	dialog.Present(w.win)
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
	w.refreshOutbox()

	// Test hooks (off by default).
	if q := os.Getenv("MAILBOX_SEARCH"); q != "" {
		// Apply synchronously (the live handler is debounced) so a paired
		// MAILBOX_OPEN_FIRST selects from the search results, not the inbox.
		w.suppressSearch = true
		w.searchEntry.SetText(q)
		w.suppressSearch = false
		w.refreshList(q)
	}
	if os.Getenv("MAILBOX_OPEN_FIRST") == "1" && w.threadModel.NItems() > 0 {
		w.threadSel.SetSelected(0)
	}
}

// allMailID is the sentinel "folder" id for the All Mail view, which lists every
// cached thread regardless of label (it is not a real Gmail label).
const allMailID = "__all_mail__"

// sidebarItem records what a row in the sidebar list maps to. Heading rows are
// non-selectable and carry an empty id.
type sidebarItem struct {
	id         string
	selectable bool
}

// folderDef is a curated system "folder" presented in the sidebar, in display
// order, with a friendly name and a (libadwaita-available) symbolic icon.
type folderDef struct {
	id, name, icon string
}

// systemFolders are the standard mailboxes shown at the top of the sidebar, in
// order. Raw Gmail system labels not listed here (UNREAD, CHAT, CATEGORY_*, …)
// are intentionally hidden — they are not navigable folders.
var systemFolders = []folderDef{
	{model.LabelInbox, "Inbox", "mail-unread-symbolic"},
	{model.LabelStarred, "Starred", "starred-symbolic"},
	{model.LabelImportant, "Important", "mail-mark-important-symbolic"},
	{model.LabelSent, "Sent", "mail-send-symbolic"},
	{model.LabelDraft, "Drafts", "document-edit-symbolic"},
	{model.LabelSpam, "Spam", "mail-mark-junk-symbolic"},
	{model.LabelTrash, "Trash", "user-trash-symbolic"},
	{allMailID, "All Mail", "folder-symbolic"},
}

func (w *window) buildSidebar() *adw.NavigationPage {
	w.labelBox = gtk.NewListBox()
	w.labelBox.AddCSSClass("navigation-sidebar")
	w.labelBox.ConnectRowSelected(func(row *gtk.ListBoxRow) {
		if row == nil || w.suppressLabelSelect {
			return
		}
		if i := row.Index(); i >= 0 && i < len(w.sidebar) {
			if it := w.sidebar[i]; it.selectable {
				w.selectLabel(it.id)
			}
		}
	})

	scroller := gtk.NewScrolledWindow()
	scroller.SetVExpand(true)
	scroller.SetChild(w.labelBox)

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	if len(w.deps.Accounts) > 1 {
		w.accountBox = gtk.NewListBox()
		w.accountBox.AddCSSClass("navigation-sidebar")
		for _, a := range w.deps.Accounts {
			row := gtk.NewLabel(a.Email)
			row.SetXAlign(0)
			setMargins(row, 12, 12, 4, 4)
			w.accountBox.Append(row)
		}
		w.accountBox.ConnectRowSelected(func(row *gtk.ListBoxRow) {
			if row == nil {
				return
			}
			if i := row.Index(); i >= 0 && i < len(w.deps.Accounts) {
				w.setActiveAccount(w.deps.Accounts[i])
			}
		})
		if r := w.accountBox.RowAtIndex(0); r != nil {
			w.accountBox.SelectRow(r)
		}
		box.Append(w.accountBox)
		box.Append(gtk.NewSeparator(gtk.OrientationHorizontal))
	} else {
		acct := gtk.NewLabel(w.activeEmail)
		acct.AddCSSClass("heading")
		acct.SetXAlign(0)
		setMargins(acct, 12, 12, 12, 6)
		box.Append(acct)
	}
	box.Append(scroller)

	hb := adw.NewHeaderBar()
	newBtn := gtk.NewButtonFromIconName("mail-message-new-symbolic")
	newBtn.SetTooltipText("New message")
	newBtn.SetSensitive(w.deps.Send != nil)
	newBtn.ConnectClicked(func() {
		w.openCompose(model.OutgoingMessage{}, "", "New message", false)
	})
	hb.PackStart(newBtn)

	w.refreshBtn = gtk.NewButtonFromIconName("view-refresh-symbolic")
	w.refreshBtn.SetTooltipText("Sync now")
	w.refreshBtn.SetSensitive(w.deps.Sync != nil)
	w.refreshBtn.ConnectClicked(w.onRefresh)
	hb.PackEnd(w.refreshBtn)

	w.syncSpinner = gtk.NewSpinner()
	w.syncSpinner.SetTooltipText("Syncing…")
	w.syncSpinner.SetVisible(false)
	hb.PackEnd(w.syncSpinner)

	prefsBtn := gtk.NewButtonFromIconName("emblem-system-symbolic")
	prefsBtn.SetTooltipText("Preferences")
	prefsBtn.SetSensitive(w.deps.AISettings != nil)
	prefsBtn.ConnectClicked(w.openSettings)
	hb.PackEnd(prefsBtn)

	shortcutsBtn := gtk.NewButtonFromIconName("preferences-desktop-keyboard-shortcuts-symbolic")
	shortcutsBtn.SetTooltipText("Keyboard shortcuts (?)")
	shortcutsBtn.ConnectClicked(w.showShortcuts)
	hb.PackEnd(shortcutsBtn)

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(box)
	return adw.NewNavigationPage(tv, "Mailbox")
}

func (w *window) buildThreadList() *adw.NavigationPage {
	w.threadByID = make(map[string]model.ThreadSummary)
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
		li.SetChild(threadRow(w.threadByID[so.String()]))
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

	w.outboxBanner = adw.NewBanner("")
	w.outboxBanner.SetButtonLabel("Outbox")
	w.outboxBanner.SetRevealed(false)
	w.outboxBanner.ConnectButtonClicked(w.openOutbox)

	// When no Gmail client could be built the UI is read-only; say so instead of
	// leaving the actions silently inert.
	w.readOnlyBanner = adw.NewBanner("Read-only — not connected to Gmail")
	w.readOnlyBanner.SetButtonLabel("How to connect")
	w.readOnlyBanner.ConnectButtonClicked(w.showConnectHelp)
	w.readOnlyBanner.SetRevealed(w.deps.ModifyLabels == nil)

	content := gtk.NewBox(gtk.OrientationVertical, 0)
	content.Append(w.readOnlyBanner)
	content.Append(w.outboxBanner)
	content.Append(w.searchEntry)
	content.Append(w.threadStack)

	hb := adw.NewHeaderBar()
	w.markReadBtn = gtk.NewButtonFromIconName("mail-mark-read-symbolic")
	w.markReadBtn.SetTooltipText("Mark all as read")
	w.markReadBtn.SetSensitive(w.deps.MarkAllRead != nil)
	w.markReadBtn.ConnectClicked(w.onMarkAllRead)
	hb.PackEnd(w.markReadBtn)

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(content)
	return adw.NewNavigationPage(tv, "Messages")
}

func (w *window) onMarkAllRead() {
	if w.deps.MarkAllRead == nil || w.current == allMailID {
		return
	}
	label := w.current
	acctID := w.activeID
	go func() {
		if err := w.deps.MarkAllRead(context.Background(), acctID, label); err != nil {
			slog.Warn("ui: mark all read", "label", label, "err", err)
		}
		dispatch.Main(func() {
			w.loadLabels()
			w.refreshList(w.searchEntry.Text())
		})
	}()
}

func (w *window) onSearchChanged() {
	if w.suppressSearch {
		return
	}
	w.refreshList(w.searchEntry.Text())
}

// refreshList populates the thread list from either the current label (blank
// query) or a full-text search (whose message hits are grouped into threads).
func (w *window) refreshList(query string) {
	ctx := context.Background()
	if strings.TrimSpace(query) == "" {
		sums, err := w.threadsForCurrent(ctx)
		if err != nil {
			slog.Error("ui: list threads", "label", w.current, "err", err)
			return
		}
		w.showThreads(sums)
		return
	}

	msgs, err := w.deps.Store.Search(ctx, w.activeID, query, threadListCap)
	if err != nil {
		slog.Error("ui: search", "query", query, "err", err)
		return
	}
	seen := make(map[string]bool)
	var sums []model.ThreadSummary
	for _, m := range msgs {
		if seen[m.ThreadID] {
			continue
		}
		seen[m.ThreadID] = true
		sum, err := w.deps.Store.GetThreadSummary(ctx, w.activeID, m.ThreadID)
		if err != nil {
			continue
		}
		sums = append(sums, sum)
	}
	w.showThreads(sums)
}

// threadsForCurrent lists the threads for the selected folder: the All Mail
// pseudo-folder spans every label, others are label-scoped.
func (w *window) threadsForCurrent(ctx context.Context) ([]model.ThreadSummary, error) {
	if w.current == allMailID {
		return w.deps.Store.ListAllThreads(ctx, w.activeID, threadListCap, 0)
	}
	return w.deps.Store.ListThreadsByLabel(ctx, w.activeID, w.current, threadListCap, 0)
}

// liveRefreshList updates the thread list in response to a background change
// (new mail, label edits) while keeping the open conversation selected, so the
// reader is not disturbed.
func (w *window) liveRefreshList() {
	w.refreshList(w.searchEntry.Text())
	w.reselectOpenThread()
}

// reselectOpenThread restores the list selection to the open conversation after
// the model was respliced. onThreadSelected no-ops when the selection already
// matches the open thread, so this does not trigger a re-render.
func (w *window) reselectOpenThread() {
	if w.openThreadID == "" {
		return
	}
	n := w.threadModel.NItems()
	for i := uint(0); i < n; i++ {
		if w.threadModel.String(i) == w.openThreadID {
			w.threadSel.SetSelected(i)
			return
		}
	}
}

// showThreads replaces the thread list contents.
func (w *window) showThreads(sums []model.ThreadSummary) {
	w.threadByID = make(map[string]model.ThreadSummary, len(sums))
	ids := make([]string, len(sums))
	for i, s := range sums {
		ids[i] = s.ThreadID
		w.threadByID[s.ThreadID] = s
	}
	w.threadModel.Splice(0, w.threadModel.NItems(), ids)
	if len(sums) == 0 {
		w.threadStack.SetVisibleChildName("empty")
	} else {
		w.threadStack.SetVisibleChildName("list")
	}
}

func (w *window) onRefresh() {
	if w.deps.Sync == nil {
		return
	}
	acctID := w.activeID
	w.setSyncing(true)
	go func() {
		err := w.deps.Sync(context.Background(), acctID)
		dispatch.Main(func() {
			w.setSyncing(false)
			if err != nil {
				slog.Warn("ui: sync now", "err", err)
				w.toast("Sync failed — will retry automatically")
				return
			}
			w.loadLabels()
			w.refreshList(w.searchEntry.Text())
		})
	}()
}

// setSyncing swaps the refresh button for a running spinner while a manual sync
// is in flight (and back when it finishes), giving the user visible feedback.
func (w *window) setSyncing(on bool) {
	w.refreshBtn.SetVisible(!on)
	w.syncSpinner.SetVisible(on)
	if on {
		w.syncSpinner.Start()
	} else {
		w.syncSpinner.Stop()
	}
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
	id := so.String()
	if id == w.openThreadID {
		return // already shown; avoids a re-render when the list refreshes live
	}
	w.showThread(id)
}

func (w *window) buildReader() *adw.NavigationPage {
	w.webview = webkit.NewWebView()
	settings := w.webview.Settings()
	// JavaScript is enabled only so the injected fit-to-width script can run.
	// Defense in depth keeps it safe: bodies are sanitized (no email scripts
	// survive), and wrapHTML sets a strict CSP — script-src is locked to our
	// per-render nonce and default-src 'none' blocks all network (no fetch/XHR
	// exfiltration, no iframes), so only our own script ever executes.
	settings.SetEnableJavascript(true)
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

	// Revealed while an in-place translation is shown; reverts to the original.
	w.translationBanner = adw.NewBanner("Showing translation")
	w.translationBanner.SetButtonLabel("Show original")
	w.translationBanner.SetRevealed(false)
	w.translationBanner.ConnectButtonClicked(w.showOriginal)

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.Append(w.translationBanner)
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

	// Primary triage actions, icon-only.
	w.replyBtn = gtk.NewButtonFromIconName("mail-reply-sender-symbolic")
	w.replyBtn.SetTooltipText("Reply (r)")
	w.replyBtn.ConnectClicked(w.onReply)

	w.replyAllBtn = gtk.NewButtonFromIconName("mail-reply-all-symbolic")
	w.replyAllBtn.SetTooltipText("Reply all")
	w.replyAllBtn.ConnectClicked(w.onReplyAll)

	w.forwardBtn = gtk.NewButtonFromIconName("mail-forward-symbolic")
	w.forwardBtn.SetTooltipText("Forward (f)")
	w.forwardBtn.ConnectClicked(w.onForward)

	w.archiveBtn = gtk.NewButtonFromIconName("folder-download-symbolic")
	w.archiveBtn.SetTooltipText("Archive (a)")
	w.archiveBtn.ConnectClicked(w.onArchive)

	w.trashBtn = gtk.NewButtonFromIconName("user-trash-symbolic")
	w.trashBtn.SetTooltipText("Move to Trash")
	w.trashBtn.ConnectClicked(w.onTrash)

	w.starBtn = gtk.NewToggleButton()
	w.starBtn.SetIconName("starred-symbolic")
	w.starBtn.SetTooltipText("Star")
	w.starBtn.ConnectToggled(w.onToggleStar)

	w.labelsBtn = gtk.NewMenuButton()
	w.labelsBtn.SetIconName("user-bookmarks-symbolic")
	w.labelsBtn.SetTooltipText("Labels")
	labelsPop := gtk.NewPopover()
	w.labelsBtn.SetPopover(labelsPop)
	w.labelsBtn.SetCreatePopupFunc(func(*gtk.MenuButton) {
		labelsPop.SetChild(w.buildLabelsMenu())
	})

	// AI actions (only useful when an assistant is configured).
	w.translateBtn = gtk.NewButtonFromIconName("accessories-dictionary-symbolic")
	w.translateBtn.SetTooltipText("Translate to English")
	w.translateBtn.ConnectClicked(w.onTranslate)

	w.draftBtn = gtk.NewButtonFromIconName("document-edit-symbolic")
	w.draftBtn.SetTooltipText("Draft a reply with AI")
	w.draftBtn.ConnectClicked(w.onDraftReply)

	// Secondary actions live in an overflow menu so the bar stays uncluttered.
	w.overflowBtn = gtk.NewMenuButton()
	w.overflowBtn.SetIconName("view-more-symbolic")
	w.overflowBtn.SetTooltipText("More actions")
	w.readerMenuPop = gtk.NewPopover()
	w.overflowBtn.SetPopover(w.readerMenuPop)
	w.overflowBtn.SetCreatePopupFunc(func(*gtk.MenuButton) {
		w.readerMenuPop.SetChild(w.buildReaderMenu())
	})

	hb.PackStart(w.replyBtn)
	hb.PackStart(w.replyAllBtn)
	hb.PackStart(w.forwardBtn)
	hb.PackStart(w.archiveBtn)
	hb.PackStart(w.trashBtn)
	hb.PackEnd(w.overflowBtn)
	hb.PackEnd(w.labelsBtn)
	hb.PackEnd(w.starBtn)
	if w.deps.Assistant != nil {
		hb.PackEnd(w.draftBtn)
		hb.PackEnd(w.translateBtn)
	}
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
	w.labelsBtn.SetSensitive(canModify)
	w.starBtn.SetSensitive(canModify)
	canSend := on && w.deps.Send != nil
	w.replyBtn.SetSensitive(canSend)
	w.replyAllBtn.SetSensitive(canSend)
	w.forwardBtn.SetSensitive(canSend)
	canAI := on && w.deps.Assistant != nil
	w.translateBtn.SetSensitive(canAI)
	w.draftBtn.SetSensitive(canAI)
	// The overflow menu builds its own items conditionally; enable it whenever a
	// message is open.
	w.overflowBtn.SetSensitive(on)
}

// replyInit builds the prefilled compose for a reply to m (To, Re: subject,
// quoted body, threading headers).
func (w *window) replyInit(m model.Message) model.OutgoingMessage {
	return model.OutgoingMessage{
		To:         m.FromAddr,
		Subject:    ensureRePrefix(m.Subject),
		Body:       quoteOriginal(m, w.bodyTextFor(m)),
		InReplyTo:  m.RFC822MsgID,
		References: strings.TrimSpace(m.References + " " + m.RFC822MsgID),
		ThreadID:   m.ThreadID,
	}
}

func (w *window) onReply() {
	m := w.openMsg
	if m.GmailID == "" {
		return
	}
	w.openCompose(w.replyInit(m), w.threadContextFor(m), "Reply", false)
}

func (w *window) onReplyAll() {
	m := w.openMsg
	if m.GmailID == "" {
		return
	}
	to, cc := replyAllRecipients(m, w.activeEmail)
	init := model.OutgoingMessage{
		To:         to,
		Cc:         cc,
		Subject:    ensureRePrefix(m.Subject),
		Body:       quoteOriginal(m, w.bodyTextFor(m)),
		InReplyTo:  m.RFC822MsgID,
		References: strings.TrimSpace(m.References + " " + m.RFC822MsgID),
		ThreadID:   m.ThreadID,
	}
	w.openCompose(init, w.threadContextFor(m), "Reply all", false)
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
	w.openCompose(init, "", "Forward", false)
}

// loadLabels rebuilds the sidebar: the curated standard folders first (only those
// the account actually has), then the user's own labels under a heading. Raw
// Gmail system labels that aren't folders are omitted.
func (w *window) loadLabels() {
	ctx := context.Background()
	labels, err := w.deps.Store.ListLabels(ctx, w.activeID)
	if err != nil {
		slog.Error("ui: load labels", "err", err)
		return
	}
	have := make(map[string]bool, len(labels))
	for _, l := range labels {
		have[l.GmailID] = true
	}

	w.labelBox.RemoveAll()
	w.sidebar = w.sidebar[:0]

	// Badges show unread counts (like a standard mail client). All Mail spans
	// every label, so it carries no badge — matching Gmail.
	for _, f := range systemFolders {
		if f.id == allMailID {
			w.appendFolder(f.id, f.icon, f.name, 0)
			continue
		}
		if !have[f.id] {
			continue
		}
		count, _ := w.deps.Store.CountUnreadByLabel(ctx, w.activeID, f.id)
		w.appendFolder(f.id, f.icon, f.name, count)
	}

	// User-created labels, alphabetical (ListLabels already orders by name).
	firstUser := true
	for _, l := range labels {
		if l.Type != model.LabelUser {
			continue
		}
		if firstUser {
			w.appendHeading("Labels")
			firstUser = false
		}
		n, _ := w.deps.Store.CountUnreadByLabel(ctx, w.activeID, l.GmailID)
		w.appendFolder(l.GmailID, "user-bookmarks-symbolic", l.Name, n)
	}

	w.restoreSidebarSelection()
}

// appendFolder adds a selectable folder/label row mapped to id.
func (w *window) appendFolder(id, icon, name string, count int) {
	w.labelBox.Append(folderRow(icon, name, count))
	w.sidebar = append(w.sidebar, sidebarItem{id: id, selectable: true})
}

// appendHeading adds a non-selectable section heading row.
func (w *window) appendHeading(text string) {
	lbl := gtk.NewLabel(text)
	lbl.AddCSSClass("dim-label")
	lbl.SetXAlign(0)
	setMargins(lbl, 12, 12, 10, 4)
	row := gtk.NewListBoxRow()
	row.SetChild(lbl)
	row.SetSelectable(false)
	row.SetActivatable(false)
	w.labelBox.Append(row)
	w.sidebar = append(w.sidebar, sidebarItem{selectable: false})
}

// restoreSidebarSelection re-highlights the row for the current folder after a
// rebuild, without firing the selection handler (so it doesn't reset the list or
// clear an active search on a background refresh).
func (w *window) restoreSidebarSelection() {
	for i, it := range w.sidebar {
		if it.selectable && it.id == w.current {
			w.suppressLabelSelect = true
			if r := w.labelBox.RowAtIndex(i); r != nil {
				w.labelBox.SelectRow(r)
			}
			w.suppressLabelSelect = false
			return
		}
	}
}

// threadListCap bounds how many messages a label loads at once. The ListView
// virtualizes row widgets, so this only bounds metadata held in memory; a truly
// windowed model (paging on scroll) is a further optimization.
const threadListCap = 5000

// setActiveAccount switches the displayed account, reloading its labels and inbox.
func (w *window) setActiveAccount(a AccountInfo) {
	if a.ID == w.activeID {
		return
	}
	w.activeID = a.ID
	w.activeEmail = a.Email
	w.current = model.LabelInbox
	w.clearReader()
	w.loadLabels()
	w.selectLabel(model.LabelInbox)
	w.refreshOutbox()
}

// clearReader returns the reader to its empty state and forgets the open
// conversation, so stale actions can't target a thread from another account.
func (w *window) clearReader() {
	w.openThreadID = ""
	w.openThreadMsgs = nil
	w.openMsg = model.Message{}
	w.resetTranslation()
	w.setActionsSensitive(false)
	w.readerStack.SetVisibleChildName("empty")
}

func (w *window) selectLabel(labelID string) {
	w.current = labelID
	// "Mark all read" is meaningful per folder, but not for the All Mail view
	// (it spans every label and Gmail offers no such bulk op there).
	w.markReadBtn.SetSensitive(w.deps.MarkAllRead != nil && labelID != allMailID)
	// Switching label clears any active search without re-triggering it.
	w.suppressSearch = true
	w.searchEntry.SetText("")
	w.suppressSearch = false
	w.refreshList("")
	// When collapsed, reveal the thread list for the chosen label.
	w.outerSplit.SetShowContent(true)
}

// showThread opens a conversation: it loads all its messages, renders them
// stacked in the reader, and marks any unread ones read.
func (w *window) showThread(threadID string) {
	msgs, err := w.deps.Store.ListThreadMessages(context.Background(), w.activeID, threadID)
	if err != nil || len(msgs) == 0 {
		if err != nil {
			slog.Warn("ui: load thread", "thread", threadID, "err", err)
		}
		return
	}
	w.openThreadID = threadID
	w.openThreadMsgs = msgs
	w.openMsg = msgs[len(msgs)-1] // newest, for reply/forward/star/unread
	w.resetTranslation()          // a freshly opened thread shows the original
	w.setActionsSensitive(true)
	w.readerStack.SetVisibleChildName("message")
	w.innerSplit.SetShowContent(true)

	w.updatingStar = true
	w.starBtn.SetActive(w.openMsg.IsStarred)
	w.updatingStar = false

	w.renderConversation(msgs)

	// Mark unread messages in the thread read.
	if w.deps.ModifyLabels != nil {
		var unread []model.Message
		for _, m := range msgs {
			if m.IsUnread {
				unread = append(unread, m)
			}
		}
		if len(unread) > 0 {
			go func() {
				for _, m := range unread {
					if err := w.deps.ModifyLabels(context.Background(), m.AccountID, m.GmailID, nil, []string{model.LabelUnread}); err != nil {
						slog.Warn("ui: mark read", "id", m.GmailID, "err", err)
					}
				}
				dispatch.Main(w.loadLabels)
			}()
		}
	}
}

// renderConversation fetches each message's body (lazily) and renders the whole
// thread as stacked sections in the reader.
func (w *window) renderConversation(msgs []model.Message) {
	latest := msgs[len(msgs)-1]
	w.header.SetMarkup(fmt.Sprintf("<b>%s</b>\n%d message(s)",
		html.EscapeString(latest.Subject), len(msgs)))
	w.webview.LoadHtml(wrapHTML("<p><i>Loading…</i></p>"), "about:blank")

	threadID := w.openThreadID // guard against a newer thread being opened mid-render
	go func() {
		ctx := context.Background()
		// Fetch missing bodies concurrently (bounded); the Gmail client also caps
		// in-flight requests and quota use.
		if w.deps.FetchBody != nil {
			sem := make(chan struct{}, 6)
			var wg sync.WaitGroup
			for _, m := range msgs {
				if m.BodyFetched {
					continue
				}
				wg.Add(1)
				sem <- struct{}{}
				go func(m model.Message) {
					defer wg.Done()
					defer func() { <-sem }()
					if err := w.deps.FetchBody(ctx, m.AccountID, m.GmailID); err != nil {
						slog.Warn("ui: fetch body", "id", m.GmailID, "err", err)
					}
				}(m)
			}
			wg.Wait()
		}
		var b strings.Builder
		for _, m := range msgs {
			body, _ := w.deps.Store.GetBody(ctx, m.RowID)
			b.WriteString(conversationSection(m, body, w.sanitizer.Sanitize))
		}
		out := b.String()
		dispatch.Main(func() {
			if w.openThreadID != threadID {
				return // user switched to another conversation while this rendered
			}
			w.webview.LoadHtml(wrapHTML(out), "about:blank")
			w.populateThreadAttachments(msgs)
		})
	}()
}

func conversationSection(m model.Message, body model.MessageBody, sanitize func(string) string) string {
	var hb strings.Builder
	hb.WriteString(`<div style="border-top:1px solid #ddd;margin-top:18px;padding-top:8px;color:#555;font-size:90%">`)
	fmt.Fprintf(&hb, `<b>%s</b> · %s`,
		html.EscapeString(displayFrom(m)), m.InternalDate.Format("Jan 2, 2006 15:04"))
	if to := strings.TrimSpace(m.ToAddrs); to != "" {
		fmt.Fprintf(&hb, `<br><span style="color:#888">to %s</span>`, html.EscapeString(to))
	}
	if cc := strings.TrimSpace(m.CcAddrs); cc != "" {
		fmt.Fprintf(&hb, `<br><span style="color:#888">cc %s</span>`, html.EscapeString(cc))
	}
	hb.WriteString(`</div>`)
	header := hb.String()
	switch {
	case body.HTML != "":
		return header + sanitize(body.HTML)
	case body.Text != "":
		return header + "<pre style=\"white-space:pre-wrap\">" + html.EscapeString(body.Text) + "</pre>"
	default:
		return header + "<p>" + html.EscapeString(m.Snippet) + "</p>"
	}
}

// populateThreadAttachments shows chips for all attachments across the thread,
// each opening via its own message.
func (w *window) populateThreadAttachments(msgs []model.Message) {
	for child := w.attachBox.FirstChild(); child != nil; child = w.attachBox.FirstChild() {
		w.attachBox.Remove(child)
	}
	if w.deps.OpenAttach == nil {
		w.attachBox.SetVisible(false)
		return
	}
	any := false
	for _, m := range msgs {
		atts, err := w.deps.Store.ListAttachments(context.Background(), m.RowID)
		if err != nil {
			slog.Warn("ui: list attachments", "id", m.GmailID, "err", err)
			continue
		}
		gmailID := m.GmailID
		accountID := m.AccountID
		for _, a := range atts {
			att := a
			btn := gtk.NewButton()
			btn.SetChild(attachmentChip(att))
			btn.SetTooltipText(att.MimeType)
			btn.ConnectClicked(func() { w.openAttachment(accountID, gmailID, att.ID) })
			w.attachBox.Append(btn)
			any = true
		}
	}
	w.attachBox.SetVisible(any)
}

func (w *window) openAttachment(accountID int64, gmailID string, attID int64) {
	if w.deps.OpenAttach == nil {
		return
	}
	go func() {
		path, err := w.deps.OpenAttach(context.Background(), accountID, gmailID, attID)
		if err != nil {
			slog.Warn("ui: open attachment", "id", gmailID, "err", err)
			dispatch.Main(func() { w.toast("Couldn't download attachment") })
			return
		}
		if err := exec.Command("xdg-open", path).Start(); err != nil {
			slog.Warn("ui: xdg-open", "path", path, "err", err)
			dispatch.Main(func() { w.toast("Couldn't open attachment") })
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
	w.removeFromList("Archived", nil, []string{model.LabelInbox})
}

func (w *window) onTrash() {
	w.removeFromList("Moved to Trash", []string{model.LabelTrash}, []string{model.LabelInbox})
}

func (w *window) onMarkUnread() {
	if w.openMsg.GmailID != "" {
		w.applyLabels([]model.Message{w.openMsg}, []string{model.LabelUnread}, nil, nil)
	}
}

func (w *window) onToggleStar() {
	if w.updatingStar || w.openMsg.GmailID == "" {
		return
	}
	if w.starBtn.Active() {
		w.applyLabels([]model.Message{w.openMsg}, []string{model.LabelStarred}, nil, nil)
	} else {
		w.applyLabels([]model.Message{w.openMsg}, nil, []string{model.LabelStarred}, nil)
	}
}

// buildLabelsMenu builds the popover content for the labels button: a checkbox
// per user label, ticked when the open thread already carries it. Toggling
// applies or removes that label across the whole conversation.
func (w *window) buildLabelsMenu() gtk.Widgetter {
	box := gtk.NewBox(gtk.OrientationVertical, 2)
	setMargins(box, 8, 8, 8, 8)
	if w.openThreadID == "" {
		box.Append(gtk.NewLabel("No conversation open"))
		return box
	}
	ctx := context.Background()
	labels, err := w.deps.Store.ListLabels(ctx, w.activeID)
	if err != nil {
		slog.Warn("ui: labels menu", "err", err)
		box.Append(gtk.NewLabel("Could not load labels"))
		return box
	}
	applied, err := w.deps.Store.ThreadLabels(ctx, w.activeID, w.openThreadID)
	if err != nil {
		slog.Warn("ui: thread labels", "err", err)
		applied = map[string]bool{}
	}
	msgs := w.openThreadMsgs
	any := false
	for _, l := range labels {
		if l.Type != model.LabelUser {
			continue
		}
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

// buildReaderMenu is the overflow popover for less-common reader actions.
// (Reply all, Translate and Draft reply are dedicated header buttons.)
func (w *window) buildReaderMenu() gtk.Widgetter {
	box := gtk.NewBox(gtk.OrientationVertical, 2)
	setMargins(box, 6, 6, 6, 6)
	box.SetSizeRequest(200, -1)

	if w.deps.ModifyLabels != nil {
		box.Append(w.readerMenuItem("Mark as unread", w.onMarkUnread))
	}

	img := gtk.NewCheckButtonWithLabel("Show remote images")
	img.SetActive(w.imagesEnabled)
	setMargins(img, 8, 8, 6, 6)
	img.ConnectToggled(func() {
		w.readerMenuPop.Popdown()
		w.setImagesEnabled(img.Active())
	})
	box.Append(img)
	return box
}

// readerMenuItem returns a flat, full-width, left-aligned button styled like a
// menu row; clicking it closes the overflow popover and runs fn.
func (w *window) readerMenuItem(label string, fn func()) *gtk.Button {
	b := gtk.NewButton()
	l := gtk.NewLabel(label)
	l.SetXAlign(0)
	l.SetHExpand(true)
	b.SetChild(l)
	b.AddCSSClass("flat")
	b.ConnectClicked(func() {
		w.readerMenuPop.Popdown()
		fn()
	})
	return b
}

// setImagesEnabled toggles remote-image loading and re-renders the open thread.
func (w *window) setImagesEnabled(on bool) {
	w.imagesEnabled = on
	w.webview.Settings().SetAutoLoadImages(on)
	if len(w.openThreadMsgs) > 0 {
		w.renderConversation(w.openThreadMsgs)
	}
}

// onTranslate translates the open message into English in place, preserving the
// email's markup (so styling is kept), and streams the result as it arrives. A
// banner reverts to the original.
func (w *window) onTranslate() {
	m := w.openMsg
	if m.GmailID == "" || w.deps.Assistant == nil {
		return
	}
	if w.translateCancel != nil {
		w.translateCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.translateCancel = cancel
	threadID := w.openThreadID
	source := w.bodyHTMLFor(m)

	w.translationBanner.SetTitle("Translating…")
	w.translationBanner.SetRevealed(true)
	w.webview.LoadHtml(wrapHTML("<p><i>Translating…</i></p>"), "about:blank")

	render := func(htmlBody string, final bool) {
		safe := w.sanitizer.Sanitize(stripCodeFence(htmlBody))
		dispatch.Main(func() {
			if w.openThreadID != threadID {
				return // user switched conversations while translating
			}
			if final {
				w.translationBanner.SetTitle("Showing translation")
			}
			w.webview.LoadHtml(wrapHTML(safe), "about:blank")
		})
	}

	go func() {
		ch, err := w.deps.Assistant.TranslateHTML(ctx, source, "English")
		if err != nil {
			msg := err.Error()
			dispatch.Main(func() {
				if w.openThreadID == threadID {
					w.webview.LoadHtml(wrapHTML("<p>Translation failed: "+html.EscapeString(msg)+"</p>"), "about:blank")
				}
			})
			return
		}
		var acc strings.Builder
		var last time.Time
		for c := range ch {
			if c.Err != nil {
				continue
			}
			acc.WriteString(c.Text)
			// Throttle live re-renders so streaming stays smooth, not flickery.
			if time.Since(last) > 350*time.Millisecond {
				last = time.Now()
				render(acc.String(), false)
			}
		}
		render(acc.String(), true)
	}()
}

// bodyHTMLFor returns the open message's HTML body for translation (sanitized),
// falling back to its text or snippet wrapped as HTML.
func (w *window) bodyHTMLFor(m model.Message) string {
	if b, err := w.deps.Store.GetBody(context.Background(), m.RowID); err == nil {
		if strings.TrimSpace(b.HTML) != "" {
			return w.sanitizer.Sanitize(b.HTML)
		}
		if strings.TrimSpace(b.Text) != "" {
			return "<pre style=\"white-space:pre-wrap\">" + html.EscapeString(b.Text) + "</pre>"
		}
	}
	return "<p>" + html.EscapeString(m.Snippet) + "</p>"
}

// stripCodeFence removes a leading/trailing Markdown code fence the model may
// wrap HTML output in despite instructions.
func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return s
	}
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:]
	}
	t = strings.TrimSuffix(strings.TrimRight(t, " \n\r\t"), "```")
	return t
}

// showOriginal cancels any translation and restores the original message view.
func (w *window) showOriginal() {
	w.resetTranslation()
	if len(w.openThreadMsgs) > 0 {
		w.renderConversation(w.openThreadMsgs)
	}
}

// resetTranslation hides the translation banner and aborts any in-flight
// translation — used when reverting or when a different conversation is opened.
func (w *window) resetTranslation() {
	if w.translateCancel != nil {
		w.translateCancel()
		w.translateCancel = nil
	}
	w.translationBanner.SetRevealed(false)
}

// onDraftReply opens a reply compose window and streams an AI-drafted reply
// straight into its body, so the user can edit and send it.
func (w *window) onDraftReply() {
	m := w.openMsg
	if m.GmailID == "" || w.deps.Assistant == nil {
		return
	}
	w.openCompose(w.replyInit(m), w.threadContextFor(m), "Reply", true)
}

// bodyTextFor returns the best plain-text representation of a message for AI
// input: the text/plain part when present, otherwise the HTML reduced to text,
// otherwise the snippet. HTML tags and entities are always stripped so the AI
// never sees raw markup.
func (w *window) bodyTextFor(m model.Message) string {
	raw := m.Snippet
	if b, err := w.deps.Store.GetBody(context.Background(), m.RowID); err == nil {
		switch {
		case strings.TrimSpace(b.Text) != "":
			raw = b.Text
		case strings.TrimSpace(b.HTML) != "":
			raw = b.HTML
		}
	}
	return htmlToText(raw)
}

// htmlToText strips any HTML tags and decodes entities, yielding readable plain
// text. Safe on input that is already plain text.
func htmlToText(s string) string {
	stripped := bluemonday.StrictPolicy().Sanitize(s)
	text := html.UnescapeString(stripped)
	// Collapse the runs of blank lines that tag removal tends to leave behind.
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(text)
}

func (w *window) threadContextFor(m model.Message) string {
	return fmt.Sprintf("From: %s\nSubject: %s\n\n%s", displayFrom(m), m.Subject, w.bodyTextFor(m))
}

// applyLabels applies a label change to the given messages in the background,
// then refreshes the label counts and the current list (preserving any search).
// If after is non-nil it runs on the main thread once the list has refreshed.
func (w *window) applyLabels(msgs []model.Message, add, remove []string, after func()) {
	if w.deps.ModifyLabels == nil || len(msgs) == 0 {
		return
	}
	go func() {
		for _, m := range msgs {
			if err := w.deps.ModifyLabels(context.Background(), m.AccountID, m.GmailID, add, remove); err != nil {
				slog.Warn("ui: apply labels", "id", m.GmailID, "err", err)
			}
		}
		dispatch.Main(func() {
			w.loadLabels()
			w.refreshList(w.searchEntry.Text())
			if after != nil {
				after()
			}
		})
	}()
}

// removeFromList applies a destructive label change to the whole open thread
// (archive or trash), advances the selection to the next conversation, and shows
// an undo toast that reverses the change.
func (w *window) removeFromList(toastTitle string, add, remove []string) {
	msgs := w.openThreadMsgs
	if w.deps.ModifyLabels == nil || len(msgs) == 0 {
		return
	}
	pos := w.threadSel.Selected()
	w.applyLabels(msgs, add, remove, func() { w.advanceSelection(pos) })
	w.showUndoToast(toastTitle, msgs, add, remove)
}

// advanceSelection selects the conversation that now occupies pos (the one after
// the removed thread), clamped to the list, or clears the reader if empty.
func (w *window) advanceSelection(pos uint) {
	const invalidPos = 0xffffffff // GTK_INVALID_LIST_POSITION
	n := w.threadModel.NItems()
	if n == 0 {
		w.clearReader()
		return
	}
	if pos == invalidPos || pos >= n {
		pos = n - 1
	}
	// The list was just spliced, so the selection is currently invalid; setting it
	// fires the selection-changed handler, which opens the conversation.
	w.threadSel.SetSelected(pos)
}

// toast shows a transient message over the window. Safe to call from the main
// thread only (like all widget access).
func (w *window) toast(msg string) {
	if w.toastOverlay == nil {
		return
	}
	w.toastOverlay.AddToast(adw.NewToast(msg))
}

// showUndoToast presents an undo toast that reverses the add/remove applied to
// msgs (re-adding what was removed and vice versa).
func (w *window) showUndoToast(title string, msgs []model.Message, add, remove []string) {
	if w.toastOverlay == nil {
		return
	}
	t := adw.NewToast(title)
	t.SetButtonLabel("Undo")
	t.SetTimeout(6)
	t.ConnectButtonClicked(func() {
		w.applyLabels(msgs, remove, add, nil) // swap to reverse the change
	})
	w.toastOverlay.AddToast(t)
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
			dispatch.Main(func() { w.onChange(c) })
		}
	}()
}

// onChange reacts to a background sync change: it refreshes the active account's
// label counts and thread list (keeping the open conversation in place) and
// notifies for genuinely new inbox mail on any account.
func (w *window) onChange(c syncer.Change) {
	switch c.Kind {
	case syncer.MessageUpserted, syncer.MessageDeleted:
		if c.AccountID == w.activeID {
			w.loadLabels()
			w.liveRefreshList()
		}
	case syncer.LabelsSynced:
		if c.AccountID == w.activeID {
			w.loadLabels()
		}
	case syncer.SendStateChanged:
		if c.AccountID == w.activeID {
			w.refreshOutbox()
		}
	}
	if c.Kind != syncer.MessageUpserted || c.GmailID == "" {
		return
	}
	m, err := w.deps.Store.GetMessage(context.Background(), c.AccountID, c.GmailID)
	if err != nil || !m.IsUnread || !m.InternalDate.After(w.startTime) {
		return
	}
	for _, l := range m.Labels {
		if l == model.LabelInbox {
			w.notifyNewMail(c.AccountID, m)
			return
		}
	}
}

func (w *window) notifyNewMail(accountID int64, m model.Message) {
	n := gio.NewNotification("New mail")
	body := displayFrom(m)
	if m.Subject != "" {
		body += " — " + m.Subject
	}
	n.SetBody(body)
	// Unique id per message so concurrent accounts' notifications don't replace
	// one another.
	w.app.SendNotification(fmt.Sprintf("mailbox-mail-%d-%s", accountID, m.GmailID), n)
}

// folderRow builds a sidebar row: a leading symbolic icon, the folder name, and
// an unread-count badge. When there are unread messages the name is emphasized,
// like a standard mail client.
func folderRow(icon, name string, unread int) *gtk.Box {
	box := gtk.NewBox(gtk.OrientationHorizontal, 8)
	setMargins(box, 12, 12, 6, 6)
	if icon != "" {
		box.Append(gtk.NewImageFromIconName(icon))
	}
	n := gtk.NewLabel(name)
	n.SetXAlign(0)
	n.SetHExpand(true)
	n.SetEllipsize(pango.EllipsizeEnd)
	if unread > 0 {
		n.AddCSSClass("heading")
	}
	box.Append(n)
	if unread > 0 {
		c := gtk.NewLabel(fmt.Sprintf("%d", unread))
		c.AddCSSClass("numeric")
		c.AddCSSClass("dim-label")
		box.Append(c)
	}
	return box
}

func threadRow(t model.ThreadSummary) *gtk.Box {
	m := t.Latest
	unread := t.UnreadCount > 0

	box := gtk.NewBox(gtk.OrientationVertical, 2)
	setMargins(box, 12, 12, 6, 6)

	top := gtk.NewBox(gtk.OrientationHorizontal, 6)
	fromText := displayFrom(m)
	if t.Count > 1 {
		fromText += fmt.Sprintf("  (%d)", t.Count)
	}
	from := gtk.NewLabel(fromText)
	from.SetXAlign(0)
	from.SetHExpand(true)
	from.SetEllipsize(pango.EllipsizeEnd)
	if unread {
		from.AddCSSClass("heading")
	}
	top.Append(from)
	if m.HasAttachments {
		clip := gtk.NewImageFromIconName("mail-attachment-symbolic")
		clip.AddCSSClass("dim-label")
		top.Append(clip)
	}
	if d := relativeDate(m.InternalDate, time.Now()); d != "" {
		date := gtk.NewLabel(d)
		date.AddCSSClass("dim-label")
		date.AddCSSClass("caption")
		top.Append(date)
	}
	box.Append(top)

	subjText := m.Subject
	if strings.TrimSpace(subjText) == "" {
		subjText = "(no subject)"
	}
	subj := gtk.NewLabel(subjText)
	subj.SetXAlign(0)
	subj.SetEllipsize(pango.EllipsizeEnd)
	if !unread {
		subj.AddCSSClass("dim-label")
	}
	box.Append(subj)

	if m.Snippet != "" {
		// Decode any HTML entities in older cached snippets (new ones arrive
		// already decoded); harmless on plain text.
		snip := gtk.NewLabel(html.UnescapeString(m.Snippet))
		snip.SetXAlign(0)
		snip.SetEllipsize(pango.EllipsizeEnd)
		snip.AddCSSClass("dim-label")
		snip.AddCSSClass("caption")
		box.Append(snip)
	}
	return box
}

// relativeDate renders a compact timestamp relative to now: a clock time for
// today, the weekday within the past week, "Jan 2" within the current year, and
// "Jan 2, 2006" beyond that. It returns "" for a zero time.
func relativeDate(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	t = t.In(now.Location())
	y, mo, d := now.Date()
	startOfToday := time.Date(y, mo, d, 0, 0, 0, 0, now.Location())
	switch {
	case !t.Before(startOfToday):
		return t.Format("15:04")
	case !t.Before(startOfToday.AddDate(0, 0, -6)):
		return t.Format("Mon")
	case t.Year() == now.Year():
		return t.Format("Jan 2")
	default:
		return t.Format("Jan 2, 2006")
	}
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
	// CSS keeps the common overflow culprits in check (images capped to the
	// width, long URLs wrapped); the script then scales down anything still too
	// wide — chiefly fixed-width newsletter tables that CSS cannot shrink below
	// their min-content — so email fits the reader with neither a horizontal
	// scrollbar nor cropping.
	const style = `
body{font-family:sans-serif;margin:16px;color:#222;line-height:1.4;overflow-wrap:anywhere}
img,video{max-width:100%!important;height:auto!important}
pre{font-family:monospace;white-space:pre-wrap}`

	// Scale wide content down to fit the reader. WebKitGTK ignores CSS `zoom`, so
	// we wrap the body in a div and apply transform:scale (origin top-left).
	// Because transform doesn't shrink the layout box, the wrapper is pinned to
	// its natural width, the body height is collapsed to the scaled height (no
	// trailing gap), and overflow-x is clipped. Measured before scaling so it
	// never feeds back on itself; re-runs on load and resize.
	nonce := randNonce()
	script := `<script nonce="` + nonce + `">(function(){var wrap;function fit(){var b=document.body;if(!b||!wrap)return;` +
		`wrap.style.transform='none';wrap.style.width='auto';var avail=b.clientWidth,natural=wrap.scrollWidth;` +
		`if(natural>avail+1&&natural>0){var s=avail/natural;wrap.style.width=natural+'px';wrap.style.transformOrigin='top left';wrap.style.transform='scale('+s+')';b.style.height=(wrap.offsetHeight*s)+'px';}else{b.style.height='';}}` +
		`function setup(){var b=document.body;if(!b)return;wrap=document.createElement('div');while(b.firstChild){wrap.appendChild(b.firstChild);}b.appendChild(wrap);b.style.overflowX='hidden';fit();window.addEventListener('resize',fit);}` +
		`if(document.readyState!=='loading'){setup();}else{document.addEventListener('DOMContentLoaded',setup);}window.addEventListener('load',fit);})();</script>`

	csp := "default-src 'none'; img-src http: https: data: cid:; media-src http: https: data:; " +
		"style-src 'unsafe-inline'; script-src 'nonce-" + nonce + "'; font-src http: https: data:"

	return `<!doctype html><html><head><meta charset="utf-8">` +
		`<meta http-equiv="Content-Security-Policy" content="` + csp + `">` +
		`<style>` + style + `</style></head><body>` + inner + script + `</body></html>`
}

// randNonce returns a random CSP nonce so only our injected script may run.
func randNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "mailboxfit" // non-secret fallback; CSP still restricts to this value
	}
	return hex.EncodeToString(b[:])
}
