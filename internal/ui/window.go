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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	webkit "github.com/diamondburned/gotk4-webkitgtk/pkg/webkit/v6"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	glib "github.com/diamondburned/gotk4/pkg/glib/v2"
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
	// accountNames maps account email → user-assigned display name ("Home",
	// "Work"); accountBadges maps account id → its unread-inbox count pill in the
	// switcher, so badges can refresh in place when any account syncs.
	accountNames  map[string]string
	accountBadges map[int64]*gtk.Label
	// signature is the default text appended to composed messages (configurable
	// in Preferences); empty means none.
	signature   string
	labelBox    *gtk.ListBox
	refreshBtn  *gtk.Button
	syncSpinner *gtk.Spinner  // shown in place of refreshBtn during a manual sync
	sidebar     []sidebarItem // one entry per row in labelBox (incl. headings)
	current     string
	activeID    int64 // the account currently shown
	activeEmail string
	// suppressLabelSelect guards the row-selected handler while loadLabels
	// restores the visual highlight, so a background refresh doesn't reset the
	// list or clear an active search.
	suppressLabelSelect bool
	startTime           time.Time // only mail arriving after this triggers notifications

	// virtualized list grouped by conversation: a StringList of thread ids drives
	// a ListView; the factory builds visible rows from threadByID.
	threadModel  *gtk.StringList
	threadSel    *gtk.SingleSelection
	threadView   *gtk.ListView
	threadStack  *gtk.Stack      // "list" vs "empty" placeholder
	emptyPage    *adw.StatusPage // the "empty" placeholder (text set per context)
	readerStack  *gtk.Stack      // "message" vs "empty" placeholder
	markReadBtn  *gtk.Button
	unreadToggle *gtk.ToggleButton // "show unread only" filter for the current view
	unreadOnly   bool
	// multi-select triage: a selection mode with per-row checkboxes and a bulk
	// action bar.
	selectBtn         *gtk.ToggleButton
	selectMode        bool
	selected          map[string]bool // selected thread ids
	selectionBar      *gtk.Box
	selectionLabel    *gtk.Label
	readOnlyBanner    *adw.Banner // revealed when no Gmail client (live features off)
	outboxBanner      *adw.Banner // revealed when sends are queued/failed
	emptyFolderBanner *adw.Banner // revealed in Trash/Spam to empty them permanently
	searchEntry       *gtk.SearchEntry
	suppressSearch    bool // guards SetText from firing a search during label switch
	threadByID        map[string]model.ThreadSummary

	// coalesce refreshes triggered by bursts of sync change events.
	refreshPending     bool
	refreshListPending bool

	header       *gtk.Label
	attachBox    *gtk.Box   // chips for the open message's attachments
	trackerLabel *gtk.Label // "N trackers blocked" indicator
	authLabel    *gtk.Label // sender authentication (SPF/DKIM/DMARC) badge
	cautionLabel *gtk.Label // anti-phishing heuristic warnings
	webview      *webkit.WebView
	readerZoom   float64 // reader message zoom (Ctrl +/-/0), persisted
	sanitizer    *bluemonday.Policy

	// reader: the open conversation. openMsg is its newest message (used for
	// reply/forward/star/unread); openThreadMsgs is all of them (oldest first).
	openThreadID   string
	openThreadMsgs []model.Message
	openMsg        model.Message
	replyAllBtn    *adw.SplitButton // primary action; dropdown has Reply/Forward
	archiveBtn     *gtk.Button
	labelsBtn      *gtk.MenuButton
	translateBtn   *gtk.Button
	draftBtn       *gtk.Button
	overflowBtn    *gtk.MenuButton // star/unread/trash/images live here
	readerMenuPop  *gtk.Popover    // overflow menu content (built lazily)
	imagesEnabled  bool            // whether remote images are loaded in the reader
	blockImages    bool            // global default: block remote images (Preferences)

	// AI thread summary: a button reveals a card that streams a summary in.
	// summaryCache memoizes by the thread's message fingerprint, so reopening is
	// instant and a new reply (different fingerprint) re-generates automatically.
	summaryBtn      *gtk.Button
	analyzeBtn      *gtk.Button // on-demand AI phishing/scam analysis
	summaryRevealer *gtk.Revealer
	summaryLabel    *gtk.Label
	cardIcon        *gtk.Image // card icon (set per action: summary vs analysis)
	cardTitle       *gtk.Label // card title (set per action)
	summaryCancel   context.CancelFunc
	summaryCache    map[string]string

	// in-place translation: a banner offers reverting to the original; the cancel
	// func aborts an in-flight translation when the user reverts or switches mail;
	// translationCache memoizes results per message id so re-showing is instant.
	translationBanner *adw.Banner
	translateCancel   context.CancelFunc
	translationCache  map[string]string
}

func newWindow(app *adw.Application, deps Deps) *window {
	w := &window{
		app:              app,
		deps:             deps,
		current:          model.LabelInbox,
		startTime:        time.Now(),
		sanitizer:        emailPolicy(),
		translationCache: map[string]string{},
		summaryCache:     map[string]string{},
		accountBadges:    map[int64]*gtk.Label{},
		readerZoom:       1.0,
		selected:         map[string]bool{},
	}
	w.accountNames, _ = config.LoadAccountNames()
	w.signature, _ = config.LoadSignature()
	if p, err := config.LoadPrefs(); err == nil {
		w.blockImages = p.BlockRemoteImages
	}
	if len(deps.Accounts) > 0 {
		w.activeID = deps.Accounts[0].ID
		w.activeEmail = deps.Accounts[0].Email
	}
	w.build()
	w.registerActions()
	return w
}

// registerActions wires GApplication actions invoked from outside the widget
// tree — currently "open-message", fired when a new-mail notification is
// clicked, carrying "<accountID>|<gmailID>" as its string target.
func (w *window) registerActions() {
	act := gio.NewSimpleAction("open-message", glib.NewVariantType("s"))
	act.ConnectActivate(func(parameter *glib.Variant) {
		if parameter != nil {
			w.openFromNotification(parameter.String())
		}
	})
	w.app.AddAction(act)
}

// openFromNotification focuses the window and opens the conversation containing
// the message identified by target ("<accountID>|<gmailID>").
func (w *window) openFromNotification(target string) {
	parts := strings.SplitN(target, "|", 2)
	if len(parts) != 2 {
		return
	}
	acctID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return
	}
	gmailID := parts[1]
	if acctID != w.activeID {
		for _, a := range w.deps.Accounts {
			if a.ID == acctID {
				w.setActiveAccount(a)
				break
			}
		}
	}
	w.win.Present()
	m, err := w.deps.Store.GetMessage(context.Background(), acctID, gmailID)
	if err != nil {
		slog.Warn("ui: open from notification", "id", gmailID, "err", err)
		return
	}
	w.selectLabel(model.LabelInbox)
	w.showThread(m.ThreadID)
}

func (w *window) build() {
	loadAppCSS() // register the colour stylesheet before any widgets are built
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
		// Ctrl +/-/0 zoom the message view (works while reading, incl. focus in
		// the WebView), like a browser.
		if state&gdk.ControlMask != 0 {
			switch keyval {
			case gdk.KEY_plus, gdk.KEY_equal, gdk.KEY_KP_Add:
				w.adjustZoom(0.1)
				return true
			case gdk.KEY_minus, gdk.KEY_KP_Subtract:
				w.adjustZoom(-0.1)
				return true
			case gdk.KEY_0, gdk.KEY_KP_0:
				w.setZoom(1.0)
				return true
			}
		}
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
		case '!':
			if w.current == model.LabelSpam {
				w.onNotSpam()
			} else {
				w.onReportSpam()
			}
		case 's':
			w.toggleStar()
		case 'u':
			w.onMarkUnread()
		case 't':
			w.onTranslate()
		case 'c':
			if w.deps.Send != nil {
				w.openCompose(model.OutgoingMessage{}, "", "New message", false)
			}
		case '/':
			w.searchEntry.GrabFocus()
		case gdk.KEY_Escape:
			w.goBack()
		case '?':
			w.openSettings()
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

// toggleStar flips the star on the open message. No-op when nothing is open.
func (w *window) toggleStar() {
	if w.openMsg.GmailID == "" {
		return
	}
	w.setStarred(!w.openMsg.IsStarred)
}

// setStarred adds or removes the star on the open message (optimistic), keeping
// openMsg's flag in sync so the overflow checkbox and the 's' shortcut agree.
func (w *window) setStarred(star bool) {
	if w.openMsg.GmailID == "" {
		return
	}
	w.openMsg.IsStarred = star
	if star {
		w.applyLabels([]model.Message{w.openMsg}, []string{model.LabelStarred}, nil, nil)
	} else {
		w.applyLabels([]model.Message{w.openMsg}, nil, []string{model.LabelStarred}, nil)
	}
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
	// Reopen where the user left off (folder + unread filter).
	if vs, err := config.LoadViewState(); err == nil {
		if vs.Folder != "" {
			w.current = vs.Folder
		}
		if vs.UnreadOnly {
			w.unreadOnly = true
			w.unreadToggle.SetActive(true)
		}
		if vs.Zoom >= 0.5 && vs.Zoom <= 3.0 {
			w.readerZoom = vs.Zoom
		}
	}
	w.webview.SetZoomLevel(w.readerZoom)
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
// order, with a friendly name, a (libadwaita-available) symbolic icon, and a CSS
// class that tints that icon (see appCSS).
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
			w.accountBox.Append(w.accountSwitcherRow(a))
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
	} else if len(w.deps.Accounts) == 1 {
		a := w.deps.Accounts[0]
		row := w.accountSwitcherRow(a)
		row.SetMarginTop(6)
		box.Append(row)
		box.Append(gtk.NewSeparator(gtk.OrientationHorizontal))
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

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(box)
	return adw.NewNavigationPage(tv, "Mailbox")
}

// accountSwitcherRow builds a sidebar account entry: the display name (custom
// name if set, else the email) with the email as a caption when a custom name
// replaces it, and an unread-inbox count pill. The badge is recorded in
// accountBadges so refreshAccountBadges can update it in place.
func (w *window) accountSwitcherRow(a AccountInfo) *gtk.Box {
	name := w.accountDisplayName(a)

	row := gtk.NewBox(gtk.OrientationHorizontal, 10)
	setMargins(row, 12, 12, 6, 6)

	primary := gtk.NewLabel(name)
	primary.SetXAlign(0)
	primary.AddCSSClass("heading")
	primary.SetEllipsize(pango.EllipsizeEnd)

	textCol := gtk.NewBox(gtk.OrientationVertical, 0)
	textCol.SetHExpand(true)
	textCol.SetVAlign(gtk.AlignCenter)
	textCol.Append(primary)
	if w.hasCustomName(a.Email) {
		email := gtk.NewLabel(a.Email)
		email.SetXAlign(0)
		email.AddCSSClass("caption")
		email.AddCSSClass("dim-label")
		email.SetEllipsize(pango.EllipsizeEnd)
		textCol.Append(email)
	}
	row.Append(textCol)

	badge := countBadge(0)
	badge.SetVisible(false)
	w.accountBadges[a.ID] = badge
	row.Append(badge)
	return row
}

// accountDisplayName returns the account's user-assigned name, or its email when
// none is set.
func (w *window) accountDisplayName(a AccountInfo) string {
	if n := strings.TrimSpace(w.accountNames[a.Email]); n != "" {
		return n
	}
	return a.Email
}

// hasCustomName reports whether the user assigned a display name to email.
func (w *window) hasCustomName(email string) bool {
	return strings.TrimSpace(w.accountNames[email]) != ""
}

// refreshAccountBadges recomputes each account's unread-inbox count and updates
// its switcher pill (hidden at zero). Cheap — one indexed COUNT per account.
func (w *window) refreshAccountBadges() {
	if len(w.accountBadges) == 0 {
		return
	}
	ctx := context.Background()
	for _, a := range w.deps.Accounts {
		badge := w.accountBadges[a.ID]
		if badge == nil {
			continue
		}
		n, _ := w.deps.Store.CountUnreadByLabel(ctx, a.ID, model.LabelInbox)
		if n > 0 {
			badge.SetText(fmt.Sprintf("%d", n))
			badge.SetVisible(true)
		} else {
			badge.SetVisible(false)
		}
	}
}

// rebuildAccountSwitcher re-renders the multi-account switcher rows (after a
// rename), preserving the current selection. Single-account naming applies on
// next launch.
func (w *window) rebuildAccountSwitcher() {
	if w.accountBox == nil {
		return
	}
	selIdx := -1
	if r := w.accountBox.SelectedRow(); r != nil {
		selIdx = r.Index()
	}
	w.accountBox.RemoveAll()
	w.accountBadges = map[int64]*gtk.Label{}
	for _, a := range w.deps.Accounts {
		w.accountBox.Append(w.accountSwitcherRow(a))
	}
	if selIdx >= 0 {
		if r := w.accountBox.RowAtIndex(selIdx); r != nil {
			w.accountBox.SelectRow(r)
		}
	}
	w.refreshAccountBadges()
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
		id := so.String()
		outgoing := w.current == model.LabelSent || w.current == model.LabelDraft
		row := threadRow(w.threadByID[id], outgoing)
		if !w.selectMode {
			li.SetChild(row)
			return
		}
		// Selection mode: prepend a checkbox; the row body still shows.
		check := gtk.NewCheckButton()
		check.SetVAlign(gtk.AlignCenter)
		check.SetActive(w.selected[id]) // set before connecting, so this doesn't fire
		check.ConnectToggled(func() {
			if check.Active() {
				w.selected[id] = true
			} else {
				delete(w.selected, id)
			}
			w.updateSelectionBar()
		})
		row.SetHExpand(true)
		wrap := gtk.NewBox(gtk.OrientationHorizontal, 6)
		setMargins(wrap, 6, 0, 0, 0)
		wrap.Append(check)
		wrap.Append(row)
		li.SetChild(wrap)
	})

	w.threadView = gtk.NewListView(w.threadSel, &factory.ListItemFactory)
	w.threadView.SetVExpand(true)
	w.threadView.SetHExpand(true)

	scroller := gtk.NewScrolledWindow()
	scroller.SetVExpand(true)
	scroller.SetHExpand(true)
	scroller.SetChild(w.threadView)

	w.emptyPage = adw.NewStatusPage()
	w.emptyPage.SetIconName("mail-unread-symbolic")
	w.emptyPage.SetTitle("No messages")
	w.emptyPage.SetDescription("This folder has no messages in the local cache.")

	w.threadStack = gtk.NewStack()
	w.threadStack.SetVExpand(true)
	w.threadStack.AddNamed(scroller, "list")
	w.threadStack.AddNamed(w.emptyPage, "empty")
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

	w.buildSelectionBar()

	w.emptyFolderBanner = adw.NewBanner("")
	w.emptyFolderBanner.SetButtonLabel("Empty now")
	w.emptyFolderBanner.SetRevealed(false)
	w.emptyFolderBanner.ConnectButtonClicked(w.onEmptyFolder)

	content := gtk.NewBox(gtk.OrientationVertical, 0)
	content.Append(w.readOnlyBanner)
	content.Append(w.outboxBanner)
	content.Append(w.emptyFolderBanner)
	content.Append(w.searchEntry)
	content.Append(w.selectionBar)
	content.Append(w.threadStack)

	hb := adw.NewHeaderBar()

	w.unreadToggle = gtk.NewToggleButton()
	w.unreadToggle.SetIconName("mail-unread-symbolic")
	w.unreadToggle.SetTooltipText("Show unread only")
	w.unreadToggle.ConnectToggled(func() {
		w.unreadOnly = w.unreadToggle.Active()
		w.refreshList(w.searchEntry.Text())
		w.saveViewState()
	})
	hb.PackStart(w.unreadToggle)

	w.markReadBtn = gtk.NewButtonFromIconName("mail-read-symbolic")
	w.markReadBtn.SetTooltipText("Mark all as read")
	w.markReadBtn.SetSensitive(w.deps.MarkAllRead != nil)
	w.markReadBtn.ConnectClicked(w.onMarkAllRead)
	hb.PackEnd(w.markReadBtn)

	// Multi-select triage (only when label changes are possible).
	if w.deps.ModifyLabels != nil {
		w.selectBtn = gtk.NewToggleButton()
		w.selectBtn.SetIconName("selection-mode-symbolic")
		w.selectBtn.SetTooltipText("Select multiple")
		w.selectBtn.ConnectToggled(func() { w.setSelectMode(w.selectBtn.Active()) })
		hb.PackEnd(w.selectBtn)
	}

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

// buildSelectionBar constructs the (hidden) bulk-action bar shown in selection
// mode: a count plus archive / trash / mark-read / cancel.
func (w *window) buildSelectionBar() {
	w.selectionLabel = gtk.NewLabel("0 selected")
	w.selectionLabel.SetXAlign(0)
	w.selectionLabel.SetHExpand(true)
	setMargins(w.selectionLabel, 10, 6, 0, 0)

	selectAll := gtk.NewButtonFromIconName("edit-select-all-symbolic")
	selectAll.SetTooltipText("Select all / none")
	selectAll.ConnectClicked(func() {
		allSelected := len(w.threadByID) > 0 && len(w.selected) >= len(w.threadByID)
		w.selected = map[string]bool{}
		if !allSelected {
			for id := range w.threadByID {
				w.selected[id] = true
			}
		}
		w.updateSelectionBar()
		w.refreshList(w.searchEntry.Text()) // re-bind checkboxes
	})

	archive := gtk.NewButtonFromIconName("folder-download-symbolic")
	archive.SetTooltipText("Archive selected")
	archive.ConnectClicked(func() { w.bulkApply("Archived", nil, []string{model.LabelInbox}) })

	trash := gtk.NewButtonFromIconName("user-trash-symbolic")
	trash.SetTooltipText("Move selected to Trash")
	trash.ConnectClicked(func() { w.bulkApply("Trashed", []string{model.LabelTrash}, []string{model.LabelInbox}) })

	read := gtk.NewButtonFromIconName("mail-read-symbolic")
	read.SetTooltipText("Mark selected as read")
	read.ConnectClicked(func() { w.bulkApply("Marked read", nil, []string{model.LabelUnread}) })

	cancel := gtk.NewButtonFromIconName("window-close-symbolic")
	cancel.AddCSSClass("flat")
	cancel.SetTooltipText("Cancel")
	cancel.ConnectClicked(func() { w.selectBtn.SetActive(false) })

	w.selectionBar = gtk.NewBox(gtk.OrientationHorizontal, 6)
	w.selectionBar.AddCSSClass("toolbar")
	setMargins(w.selectionBar, 6, 6, 4, 4)
	w.selectionBar.Append(w.selectionLabel)
	w.selectionBar.Append(selectAll)
	w.selectionBar.Append(archive)
	w.selectionBar.Append(trash)
	w.selectionBar.Append(read)
	w.selectionBar.Append(cancel)
	w.selectionBar.SetVisible(false)
}

// setSelectMode enters/leaves multi-select triage, re-binding the list so rows
// show or hide their checkboxes.
func (w *window) setSelectMode(on bool) {
	if w.selectMode == on {
		return
	}
	w.selectMode = on
	if !on {
		w.selected = map[string]bool{}
	}
	w.selectionBar.SetVisible(on)
	w.updateSelectionBar()
	w.refreshList(w.searchEntry.Text())
}

// updateSelectionBar refreshes the "N selected" count.
func (w *window) updateSelectionBar() {
	w.selectionLabel.SetText(fmt.Sprintf("%d selected", len(w.selected)))
}

// bulkApply applies a label change to every selected conversation in one batch,
// then leaves selection mode.
func (w *window) bulkApply(verb string, add, remove []string) {
	if len(w.selected) == 0 {
		return
	}
	ctx := context.Background()
	var msgs []model.Message
	n := 0
	for id := range w.selected {
		if tm, err := w.deps.Store.ListThreadMessages(ctx, w.activeID, id); err == nil {
			msgs = append(msgs, tm...)
			n++
		}
	}
	w.selectBtn.SetActive(false) // exits select mode (clears selection, refreshes)
	if len(msgs) == 0 {
		return
	}
	w.applyLabels(msgs, add, remove, nil)
	w.toast(fmt.Sprintf("%s %d conversations", verb, n))
}

// onSearchAllMail runs a Gmail server-side search for the current query, caches
// the matches, and shows them — finding mail beyond the local cache.
func (w *window) onSearchAllMail() {
	q := strings.TrimSpace(w.searchEntry.Text())
	if q == "" || w.deps.SearchServer == nil {
		return
	}
	w.emptyPage.SetChild(nil)
	w.emptyPage.SetIconName("edit-find-symbolic")
	w.emptyPage.SetTitle("Searching all mail…")
	w.emptyPage.SetDescription("")
	acctID := w.activeID
	go func() {
		ids, err := w.deps.SearchServer(context.Background(), acctID, q, 50)
		dispatch.Main(func() {
			if strings.TrimSpace(w.searchEntry.Text()) != q || w.activeID != acctID {
				return // the query changed while searching
			}
			if err != nil {
				slog.Warn("ui: search all mail", "err", err)
				w.toast("Couldn't search all mail")
				w.showThreads(nil)
				return
			}
			ctx := context.Background()
			seen := make(map[string]bool)
			var sums []model.ThreadSummary
			for _, id := range ids {
				m, err := w.deps.Store.GetMessage(ctx, acctID, id)
				if err != nil || seen[m.ThreadID] {
					continue
				}
				seen[m.ThreadID] = true
				if sum, err := w.deps.Store.GetThreadSummary(ctx, acctID, m.ThreadID); err == nil {
					sums = append(sums, sum)
				}
			}
			sort.SliceStable(sums, func(i, j int) bool {
				return sums[i].Latest.InternalDate.After(sums[j].Latest.InternalDate)
			})
			w.showThreads(sums)
			if len(sums) == 0 {
				w.toast("No messages found")
			}
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
	defer func(start time.Time) {
		slog.Debug("ui: refreshList", "label", w.current, "search", query != "", "dur", time.Since(start))
	}(time.Now())
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
	// Search hits come back ranked by relevance; show them newest-first like the
	// folder views.
	sort.SliceStable(sums, func(i, j int) bool {
		return sums[i].Latest.InternalDate.After(sums[j].Latest.InternalDate)
	})
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
	// The "unread only" toggle filters whatever the current view produced.
	if w.unreadOnly {
		var filtered []model.ThreadSummary
		for _, s := range sums {
			if s.UnreadCount > 0 {
				filtered = append(filtered, s)
			}
		}
		sums = filtered
	}

	w.threadByID = make(map[string]model.ThreadSummary, len(sums))
	ids := make([]string, len(sums))
	for i, s := range sums {
		ids[i] = s.ThreadID
		w.threadByID[s.ThreadID] = s
	}
	w.threadModel.Splice(0, w.threadModel.NItems(), ids)
	if len(sums) == 0 {
		w.emptyPage.SetChild(nil)
		switch {
		case strings.TrimSpace(w.searchEntry.Text()) != "":
			q := strings.TrimSpace(w.searchEntry.Text())
			w.emptyPage.SetIconName("edit-find-symbolic")
			w.emptyPage.SetTitle("No matches")
			w.emptyPage.SetDescription(fmt.Sprintf("No cached messages match %q.", q))
			// Offer to look beyond the local cache.
			if w.deps.SearchServer != nil {
				btn := gtk.NewButtonWithLabel("Search all mail")
				btn.AddCSSClass("pill")
				btn.AddCSSClass("suggested-action")
				btn.SetHAlign(gtk.AlignCenter)
				btn.ConnectClicked(w.onSearchAllMail)
				w.emptyPage.SetChild(btn)
			}
		case w.unreadOnly:
			w.emptyPage.SetIconName("mail-read-symbolic")
			w.emptyPage.SetTitle("No unread messages")
			w.emptyPage.SetDescription("You're all caught up in this view.")
		default:
			w.emptyPage.SetIconName("mail-unread-symbolic")
			w.emptyPage.SetTitle("No messages")
			w.emptyPage.SetDescription("This folder has no messages in the local cache.")
		}
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

// onDecidePolicy keeps the reader a viewer: our own injected content
// (about:/data:/blob:) loads in place, but a link the user clicks opens in their
// default handler (browser, mail client) instead of navigating inside the
// WebView. Unsupported schemes (file:, javascript:, …) are blocked outright.
func (w *window) onDecidePolicy(decision webkit.PolicyDecisioner, dtype webkit.PolicyDecisionType) bool {
	if dtype != webkit.PolicyDecisionTypeNavigationAction && dtype != webkit.PolicyDecisionTypeNewWindowAction {
		return false // resource loads (images/css) use default handling
	}
	nav, ok := decision.(*webkit.NavigationPolicyDecision)
	if !ok {
		return false
	}
	uri := nav.NavigationAction().Request().URI()
	if uri == "" || strings.HasPrefix(uri, "about:") || strings.HasPrefix(uri, "data:") || strings.HasPrefix(uri, "blob:") {
		return false // our own rendered content — show it in place
	}
	switch {
	case strings.HasPrefix(uri, "http://"), strings.HasPrefix(uri, "https://"),
		strings.HasPrefix(uri, "mailto:"), strings.HasPrefix(uri, "ftp://"), strings.HasPrefix(uri, "ftps://"):
		openExternal(uri)
	default:
		slog.Debug("ui: blocked navigation to unsupported scheme", "uri", uri)
	}
	nav.Ignore()
	return true
}

// openExternal hands a URI or path to the user's default handler via xdg-open,
// never loading it inside the app.
func openExternal(target string) {
	if err := exec.Command("xdg-open", target).Start(); err != nil {
		slog.Warn("ui: open external", "target", target, "err", err)
	}
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
	if w.selectMode {
		return // in selection mode, rows are picked via their checkboxes
	}
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
	// Images load by default (unless blocked globally in Preferences); tracking
	// pixels are stripped from the HTML before rendering (stripTrackers), so opens
	// aren't leaked. The overflow toggle can still block remote images per message.
	w.imagesEnabled = !w.blockImages
	settings.SetAutoLoadImages(w.imagesEnabled)
	w.webview.SetVExpand(true)
	w.webview.SetHExpand(true)
	// Keep the reader a viewer: clicked links open in the default browser, never
	// inside the WebView.
	w.webview.ConnectDecidePolicy(w.onDecidePolicy)

	w.header = gtk.NewLabel("")
	w.header.SetXAlign(0)
	w.header.SetWrap(true)
	setMargins(w.header, 12, 12, 8, 8)

	w.attachBox = gtk.NewBox(gtk.OrientationHorizontal, 6)
	setMargins(w.attachBox, 12, 12, 0, 8)
	w.attachBox.SetVisible(false)

	w.trackerLabel = gtk.NewLabel("")
	w.trackerLabel.SetXAlign(0)
	w.trackerLabel.AddCSSClass("dim-label")
	w.trackerLabel.AddCSSClass("caption")
	setMargins(w.trackerLabel, 12, 12, 0, 6)
	w.trackerLabel.SetVisible(false)

	w.authLabel = gtk.NewLabel("")
	w.authLabel.SetXAlign(0)
	w.authLabel.SetWrap(true)
	w.authLabel.AddCSSClass("caption")
	setMargins(w.authLabel, 12, 12, 0, 6)
	w.authLabel.SetVisible(false)

	w.cautionLabel = gtk.NewLabel("")
	w.cautionLabel.SetXAlign(0)
	w.cautionLabel.SetWrap(true)
	w.cautionLabel.AddCSSClass("caption")
	w.cautionLabel.AddCSSClass("warning")
	setMargins(w.cautionLabel, 12, 12, 0, 6)
	w.cautionLabel.SetVisible(false)

	// Revealed while an in-place translation is shown; reverts to the original.
	w.translationBanner = adw.NewBanner("Showing translation")
	w.translationBanner.SetButtonLabel("Show original")
	w.translationBanner.SetRevealed(false)
	w.translationBanner.ConnectButtonClicked(w.showOriginal)

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.Append(w.translationBanner)
	box.Append(w.header)
	box.Append(w.buildSummaryCard())
	box.Append(w.attachBox)
	box.Append(w.authLabel)
	box.Append(w.cautionLabel)
	box.Append(w.trackerLabel)
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

	// Reply-all is the primary action; its dropdown offers Reply and Forward.
	replyPop := gtk.NewPopover()
	replyMenu := gtk.NewBox(gtk.OrientationVertical, 2)
	setMargins(replyMenu, 6, 6, 6, 6)
	replyMenu.SetSizeRequest(160, -1)
	replyMenu.Append(menuItemButton(replyPop, "Reply", w.onReply))
	replyMenu.Append(menuItemButton(replyPop, "Forward", w.onForward))
	replyPop.SetChild(replyMenu)

	w.replyAllBtn = adw.NewSplitButton()
	w.replyAllBtn.SetIconName("mail-reply-all-symbolic")
	w.replyAllBtn.SetTooltipText("Reply all (dropdown: Reply, Forward)")
	w.replyAllBtn.ConnectClicked(w.onReplyAll)
	w.replyAllBtn.SetPopover(replyPop)

	w.archiveBtn = gtk.NewButtonFromIconName("folder-download-symbolic")
	w.archiveBtn.SetTooltipText("Archive (a)")
	w.archiveBtn.ConnectClicked(w.onArchive)

	w.labelsBtn = gtk.NewMenuButton()
	w.labelsBtn.SetIconName("user-bookmarks-symbolic")
	w.labelsBtn.SetTooltipText("Labels")
	labelsPop := gtk.NewPopover()
	w.labelsBtn.SetPopover(labelsPop)
	w.labelsBtn.SetCreatePopupFunc(func(*gtk.MenuButton) {
		labelsPop.SetChild(w.buildLabelsMenu())
	})

	// AI actions (only useful when an assistant is configured).
	w.translateBtn = gtk.NewButtonFromIconName("accessories-character-map-symbolic")
	w.translateBtn.SetTooltipText("Translate to English (t)")
	w.translateBtn.ConnectClicked(w.onTranslate)

	w.draftBtn = gtk.NewButtonFromIconName("document-edit-symbolic")
	w.draftBtn.SetTooltipText("Draft a reply with AI")
	w.draftBtn.ConnectClicked(w.onDraftReply)

	w.summaryBtn = gtk.NewButtonFromIconName("view-list-bullet-symbolic")
	w.summaryBtn.SetTooltipText("Summarize thread with AI")
	w.summaryBtn.ConnectClicked(w.onSummarize)

	w.analyzeBtn = gtk.NewButtonFromIconName("security-high-symbolic")
	w.analyzeBtn.SetTooltipText("Analyze this email for phishing (AI)")
	w.analyzeBtn.ConnectClicked(w.onAnalyze)

	// Secondary actions (star, mark-unread, trash, images) live in the overflow.
	w.overflowBtn = gtk.NewMenuButton()
	w.overflowBtn.SetIconName("view-more-symbolic")
	w.overflowBtn.SetTooltipText("More actions")
	w.readerMenuPop = gtk.NewPopover()
	w.overflowBtn.SetPopover(w.readerMenuPop)
	w.overflowBtn.SetCreatePopupFunc(func(*gtk.MenuButton) {
		w.readerMenuPop.SetChild(w.buildReaderMenu())
	})

	hb.PackStart(w.replyAllBtn)
	hb.PackStart(w.archiveBtn)
	hb.PackEnd(w.overflowBtn)
	hb.PackEnd(w.labelsBtn)
	if w.deps.Assistant != nil {
		hb.PackEnd(w.draftBtn)
		hb.PackEnd(w.translateBtn)
		hb.PackEnd(w.summaryBtn)
		hb.PackEnd(w.analyzeBtn)
	}
	w.setActionsSensitive(false)

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(w.readerStack)
	return adw.NewNavigationPage(tv, "Reader")
}

// menuItemButton returns a flat, full-width, left-aligned button styled like a
// menu row; clicking it closes pop and runs fn.
func menuItemButton(pop *gtk.Popover, label string, fn func()) *gtk.Button {
	b := gtk.NewButton()
	l := gtk.NewLabel(label)
	l.SetXAlign(0)
	l.SetHExpand(true)
	b.SetChild(l)
	b.AddCSSClass("flat")
	b.ConnectClicked(func() {
		pop.Popdown()
		fn()
	})
	return b
}

func (w *window) setActionsSensitive(on bool) {
	canModify := on && w.deps.ModifyLabels != nil
	w.archiveBtn.SetSensitive(canModify)
	w.labelsBtn.SetSensitive(canModify)
	w.replyAllBtn.SetSensitive(on && w.deps.Send != nil)
	canAI := on && w.deps.Assistant != nil
	w.translateBtn.SetSensitive(canAI)
	w.draftBtn.SetSensitive(canAI)
	if w.summaryBtn != nil {
		w.summaryBtn.SetSensitive(canAI)
	}
	if w.analyzeBtn != nil {
		w.analyzeBtn.SetSensitive(canAI)
	}
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
	defer func(start time.Time) { slog.Debug("ui: loadLabels", "dur", time.Since(start)) }(time.Now())
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

	// Only the Inbox carries an unread-count badge — that's where new mail
	// matters; badges on every folder/label read as noise.
	for _, f := range systemFolders {
		count := 0
		if f.id == model.LabelInbox {
			count, _ = w.deps.Store.CountUnreadByLabel(ctx, w.activeID, f.id)
		}
		if f.id == allMailID {
			w.appendFolder(f.id, f.icon, f.name, 0)
			continue
		}
		if !have[f.id] {
			continue
		}
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
		w.appendFolder(l.GmailID, "user-bookmarks-symbolic", l.Name, 0)
	}

	w.restoreSidebarSelection()
	w.refreshAccountBadges() // keep the per-account unread pills current
	w.updateTitle()
}

// updateTitle reflects the total unread-inbox count (across all accounts) in the
// window title, so it shows in the taskbar / overview.
func (w *window) updateTitle() {
	total := 0
	ctx := context.Background()
	for _, a := range w.deps.Accounts {
		n, _ := w.deps.Store.CountUnreadByLabel(ctx, a.ID, model.LabelInbox)
		total += n
	}
	if total > 0 {
		w.win.SetTitle(fmt.Sprintf("Mailbox — %d unread", total))
	} else {
		w.win.SetTitle("Mailbox")
	}
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
	w.hideSummary()
	w.setActionsSensitive(false)
	w.readerStack.SetVisibleChildName("empty")
}

func (w *window) selectLabel(labelID string) {
	w.current = labelID
	// "Mark all read" is meaningful per folder, but not for the All Mail view
	// (it spans every label and Gmail offers no such bulk op there).
	w.markReadBtn.SetSensitive(w.deps.MarkAllRead != nil && labelID != allMailID)
	// The "empty folder" banner appears only in Trash/Spam.
	if w.deps.EmptyFolder != nil && (labelID == model.LabelTrash || labelID == model.LabelSpam) {
		name := "Trash"
		if labelID == model.LabelSpam {
			name = "Spam"
		}
		w.emptyFolderBanner.SetTitle(name + " — messages here can be permanently deleted")
		w.emptyFolderBanner.SetRevealed(true)
	} else {
		w.emptyFolderBanner.SetRevealed(false)
	}
	// Switching label clears any active search without re-triggering it.
	w.suppressSearch = true
	w.searchEntry.SetText("")
	w.suppressSearch = false
	w.refreshList("")
	// When collapsed, reveal the thread list for the chosen label.
	w.outerSplit.SetShowContent(true)
	w.saveViewState()
}

// saveViewState persists the current folder and unread filter so the next
// launch reopens here.
func (w *window) saveViewState() {
	// Load-modify-save so we preserve fields written elsewhere (compose size).
	vs, _ := config.LoadViewState()
	vs.Folder, vs.UnreadOnly, vs.Zoom = w.current, w.unreadOnly, w.readerZoom
	if err := config.SaveViewState(vs); err != nil {
		slog.Warn("ui: save view state", "err", err)
	}
}

// adjustZoom changes the reader zoom by delta; setZoom clamps to a sane range,
// applies it to the message view, and remembers it.
func (w *window) adjustZoom(delta float64) { w.setZoom(w.readerZoom + delta) }

func (w *window) setZoom(z float64) {
	switch {
	case z < 0.5:
		z = 0.5
	case z > 3.0:
		z = 3.0
	}
	w.readerZoom = z
	w.webview.SetZoomLevel(z)
	w.saveViewState()
}

// showThread opens a conversation: it loads all its messages, renders them
// stacked in the reader, and marks any unread ones read.
func (w *window) showThread(threadID string) {
	// In the Drafts folder, a click resumes editing the draft in compose rather
	// than rendering it read-only.
	if w.current == model.LabelDraft && w.deps.Send != nil {
		w.openDraftForEdit(threadID)
		return
	}
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
	w.hideSummary()               // collapse any summary from the previous thread
	w.setActionsSensitive(true)
	w.readerStack.SetVisibleChildName("message")
	w.innerSplit.SetShowContent(true)

	w.renderConversation(msgs)

	// Mark unread messages in the thread read — in one batch call.
	if w.deps.ModifyLabels != nil {
		var ids []string
		for _, m := range msgs {
			if m.IsUnread {
				ids = append(ids, m.GmailID)
			}
		}
		if len(ids) > 0 {
			acctID := w.activeID
			go func() {
				if err := w.deps.ModifyLabels(context.Background(), acctID, ids, nil, []string{model.LabelUnread}); err != nil {
					slog.Warn("ui: mark read", "n", len(ids), "err", err)
				}
				dispatch.Main(w.loadLabels)
			}()
		}
	}
}

// hasLabel reports whether message m carries the given label id.
func hasLabel(m model.Message, label string) bool {
	for _, l := range m.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// openDraftForEdit resumes editing the draft in the given thread: it fetches the
// draft body and resolves its Gmail draft id (so sending/saving replaces the
// draft rather than duplicating it), then opens a compose window prefilled with
// the draft's recipients, subject, and body.
func (w *window) openDraftForEdit(threadID string) {
	msgs, err := w.deps.Store.ListThreadMessages(context.Background(), w.activeID, threadID)
	if err != nil || len(msgs) == 0 {
		if err != nil {
			slog.Warn("ui: load draft thread", "thread", threadID, "err", err)
		}
		return
	}
	// The draft is the message carrying the DRAFT label (fall back to newest).
	dm := msgs[len(msgs)-1]
	for _, m := range msgs {
		if hasLabel(m, model.LabelDraft) {
			dm = m
			break
		}
	}
	acctID := w.activeID
	w.toast("Opening draft…")
	go func() {
		ctx := context.Background()
		if !dm.BodyFetched && w.deps.FetchBody != nil {
			if err := w.deps.FetchBody(ctx, dm.AccountID, dm.GmailID); err != nil {
				slog.Warn("ui: fetch draft body", "id", dm.GmailID, "err", err)
			}
		}
		// Our drafts are text/plain — use the text verbatim so re-editing is
		// lossless; fall back to HTML-reduced-to-text or the snippet.
		body := dm.Snippet
		if b, err := w.deps.Store.GetBody(ctx, dm.RowID); err == nil {
			switch {
			case strings.TrimSpace(b.Text) != "":
				body = b.Text
			case strings.TrimSpace(b.HTML) != "":
				body = htmlToText(b.HTML)
			}
		}
		draftID := ""
		if w.deps.FindDraftID != nil {
			if id, err := w.deps.FindDraftID(ctx, acctID, dm.GmailID); err != nil {
				slog.Warn("ui: find draft id", "id", dm.GmailID, "err", err)
			} else {
				draftID = id
			}
		}
		dispatch.Main(func() {
			w.openCompose(model.OutgoingMessage{
				To:         strings.TrimSpace(dm.ToAddrs),
				Cc:         strings.TrimSpace(dm.CcAddrs),
				Subject:    dm.Subject,
				Body:       body,
				InReplyTo:  dm.InReplyTo,
				References: dm.References,
				ThreadID:   dm.ThreadID,
				DraftID:    draftID,
			}, "", "Edit draft", false)
		})
	}()
}

// renderConversation fetches each message's body (lazily) and renders the whole
// thread as stacked sections in the reader.
func (w *window) renderConversation(msgs []model.Message) {
	latest := msgs[len(msgs)-1]
	sender := html.EscapeString(displayFrom(latest))
	if addr := strings.TrimSpace(latest.FromAddr); addr != "" && !strings.EqualFold(addr, displayFrom(latest)) {
		sender += " &lt;" + html.EscapeString(addr) + "&gt;"
	}
	meta := sender + " · " + latest.InternalDate.Format("Jan 2, 2006 15:04")
	if len(msgs) > 1 {
		meta += fmt.Sprintf(" · %d messages", len(msgs))
	}
	w.header.SetMarkup(fmt.Sprintf("<b>%s</b>\n<span size=\"small\">%s</span>",
		html.EscapeString(latest.Subject), meta))
	w.webview.LoadHtml(wrapHTML("<p><i>Loading…</i></p>"), "about:blank")

	threadID := w.openThreadID // guard against a newer thread being opened mid-render
	go func() {
		start := time.Now()
		ctx := context.Background()
		// Fetch missing bodies concurrently (bounded); the Gmail client also caps
		// in-flight requests and quota use.
		fetched := 0
		if w.deps.FetchBody != nil {
			sem := make(chan struct{}, 6)
			var wg sync.WaitGroup
			for _, m := range msgs {
				if m.BodyFetched {
					continue
				}
				fetched++
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
		fetchDur := time.Since(start)
		sanitizeStart := time.Now()
		var b strings.Builder
		blocked := 0
		latestAuth, latestHTML := "", ""
		// Newest message first (msgs is oldest-first from the store).
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			body, _ := w.deps.Store.GetBody(ctx, m.RowID)
			if m.RowID == latest.RowID {
				latestAuth = body.RawHeaders // Authentication-Results of the newest message
				latestHTML = body.HTML
			}
			sec, n := conversationSection(m, body, w.cleanHTML)
			b.WriteString(sec)
			blocked += n
		}
		out := b.String()
		verdict := parseAuthResults(latestAuth)
		warnings := phishingWarnings(latest, latestHTML)
		slog.Debug("ui: renderConversation", "msgs", len(msgs), "fetched", fetched,
			"trackers", blocked, "auth", verdict.level, "fetch", fetchDur, "sanitize", time.Since(sanitizeStart))
		dispatch.Main(func() {
			if w.openThreadID != threadID {
				return // user switched to another conversation while this rendered
			}
			w.setTrackerCount(blocked)
			w.setAuthBadge(verdict)
			w.setCaution(warnings)
			w.webview.LoadHtml(wrapHTML(out), "about:blank")
			w.populateThreadAttachments(msgs)
		})
	}()
}

// conversationSection renders one message's header + body and returns the HTML
// plus how many trackers were stripped from it. clean sanitizes+de-tracks HTML.
func conversationSection(m model.Message, body model.MessageBody, clean func(string) (string, int)) (string, int) {
	var hb strings.Builder
	hb.WriteString(`<div style="border-top:1px solid #ddd;margin-top:18px;padding-top:8px;color:#555;font-size:90%">`)
	fmt.Fprintf(&hb, `<b>%s</b>`, html.EscapeString(displayFrom(m)))
	// Always show the actual address, not just the display name.
	if addr := strings.TrimSpace(m.FromAddr); addr != "" && !strings.EqualFold(addr, displayFrom(m)) {
		fmt.Fprintf(&hb, ` <span style="color:#888">&lt;%s&gt;</span>`, html.EscapeString(addr))
	}
	fmt.Fprintf(&hb, ` · %s`, m.InternalDate.Format("Jan 2, 2006 15:04"))
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
		cleaned, blocked := clean(body.HTML)
		return header + cleaned, blocked
	case body.Text != "":
		return header + "<pre style=\"white-space:pre-wrap\">" + html.EscapeString(body.Text) + "</pre>", 0
	default:
		return header + "<p>" + html.EscapeString(m.Snippet) + "</p>", 0
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
		openExternal(path)
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

// onMoveToInbox restores the open conversation to the inbox (adding INBOX and
// clearing TRASH) — for un-archiving or recovering from Trash.
func (w *window) onMoveToInbox() {
	if len(w.openThreadMsgs) == 0 {
		return
	}
	w.applyLabels(w.openThreadMsgs, []string{model.LabelInbox}, []string{model.LabelTrash}, nil)
	w.toast("Moved to Inbox")
}

// onReportSpam moves the open conversation to Spam (and out of the inbox).
func (w *window) onReportSpam() {
	w.removeFromList("Reported spam", []string{model.LabelSpam}, []string{model.LabelInbox})
}

// onNotSpam takes the open conversation out of Spam and back to the inbox.
func (w *window) onNotSpam() {
	w.removeFromList("Marked not spam", []string{model.LabelInbox}, []string{model.LabelSpam})
}

// onEmptyFolder permanently deletes every message in the current folder
// (Trash/Spam) after a destructive confirmation.
func (w *window) onEmptyFolder() {
	label := w.current
	if w.deps.EmptyFolder == nil || (label != model.LabelTrash && label != model.LabelSpam) {
		return
	}
	name := "Trash"
	if label == model.LabelSpam {
		name = "Spam"
	}
	confirm := adw.NewAlertDialog("Empty "+name+"?", "This permanently deletes every message in "+name+". This can't be undone.")
	confirm.AddResponse("cancel", "Cancel")
	confirm.AddResponse("empty", "Empty "+name)
	confirm.SetResponseAppearance("empty", adw.ResponseDestructive)
	confirm.SetDefaultResponse("cancel")
	confirm.SetCloseResponse("cancel")
	acctID := w.activeID
	confirm.ConnectResponse(func(response string) {
		if response != "empty" {
			return
		}
		go func() {
			n, err := w.deps.EmptyFolder(context.Background(), acctID, label)
			dispatch.Main(func() {
				if err != nil {
					slog.Warn("ui: empty folder", "label", label, "err", err)
					w.toast("Couldn't empty " + name)
					return
				}
				w.loadLabels()
				w.refreshList(w.searchEntry.Text())
				w.toast(fmt.Sprintf("Permanently deleted %d messages", n))
			})
		}()
	})
	confirm.Present(w.win)
}

// onDeleteForever permanently deletes the open conversation (Trash/Spam only),
// after a confirmation, since it cannot be undone.
func (w *window) onDeleteForever() {
	if w.deps.DeleteForever == nil || len(w.openThreadMsgs) == 0 {
		return
	}
	msgs := w.openThreadMsgs
	pos := w.threadSel.Selected()
	confirm := adw.NewAlertDialog("Delete forever?", "This permanently deletes the conversation. This can't be undone.")
	confirm.AddResponse("cancel", "Cancel")
	confirm.AddResponse("delete", "Delete forever")
	confirm.SetResponseAppearance("delete", adw.ResponseDestructive)
	confirm.SetDefaultResponse("cancel")
	confirm.SetCloseResponse("cancel")
	confirm.ConnectResponse(func(response string) {
		if response != "delete" {
			return
		}
		ids := make([]string, len(msgs))
		for i, m := range msgs {
			ids[i] = m.GmailID
		}
		acctID := w.activeID
		go func() {
			err := w.deps.DeleteForever(context.Background(), acctID, ids)
			dispatch.Main(func() {
				if err != nil {
					slog.Warn("ui: delete forever", "err", err)
					w.toast("Couldn't delete the conversation")
					return
				}
				w.loadLabels()
				w.refreshList(w.searchEntry.Text())
				w.advanceSelection(pos)
				w.toast("Deleted forever")
			})
		}()
	})
	confirm.Present(w.win)
}

func (w *window) onMarkUnread() {
	if w.openMsg.GmailID != "" {
		w.applyLabels([]model.Message{w.openMsg}, []string{model.LabelUnread}, nil, nil)
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

// buildReaderMenu is the overflow popover for auxiliary reader actions: star,
// mark-unread, trash, and the remote-images toggle. (Reply all, Reply, Forward,
// Archive, Labels, Translate and Draft reply are dedicated header controls.)
func (w *window) buildReaderMenu() gtk.Widgetter {
	box := gtk.NewBox(gtk.OrientationVertical, 2)
	setMargins(box, 6, 6, 6, 6)
	box.SetSizeRequest(200, -1)

	if w.deps.ModifyLabels != nil {
		star := gtk.NewCheckButtonWithLabel("Starred")
		star.SetActive(w.openMsg.IsStarred)
		setMargins(star, 8, 8, 6, 6)
		star.ConnectToggled(func() {
			w.readerMenuPop.Popdown()
			w.setStarred(star.Active())
		})
		box.Append(star)
		box.Append(w.readerMenuItem("Mark as unread", w.onMarkUnread))
		box.Append(w.readerMenuItem("Move to Inbox", w.onMoveToInbox))
		if w.current == model.LabelSpam {
			box.Append(w.readerMenuItem("Not spam", w.onNotSpam))
		} else {
			box.Append(w.readerMenuItem("Report spam", w.onReportSpam))
		}
		box.Append(w.readerMenuItem("Move to Trash", w.onTrash))
		if w.deps.DeleteForever != nil && (w.current == model.LabelTrash || w.current == model.LabelSpam) {
			box.Append(w.readerMenuItem("Delete forever", w.onDeleteForever))
		}
		box.Append(gtk.NewSeparator(gtk.OrientationHorizontal))
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

// readerMenuItem returns a flat menu-style row that closes the overflow popover
// and runs fn when clicked.
func (w *window) readerMenuItem(label string, fn func()) *gtk.Button {
	return menuItemButton(w.readerMenuPop, label, fn)
}

// cleanHTML sanitizes email body HTML and strips tracking pixels for rendering,
// returning the cleaned HTML and how many trackers were removed.
func (w *window) cleanHTML(h string) (string, int) {
	return stripTrackers(w.sanitizer.Sanitize(h))
}

// setTrackerCount shows "N trackers blocked" in the reader (hidden when none).
func (w *window) setTrackerCount(n int) {
	if n <= 0 {
		w.trackerLabel.SetVisible(false)
		return
	}
	noun := "tracker"
	if n != 1 {
		noun = "trackers"
	}
	w.trackerLabel.SetText(fmt.Sprintf("🛡 %d %s blocked", n, noun))
	w.trackerLabel.SetVisible(true)
}

// setAuthBadge shows the sender-authentication verdict (SPF/DKIM/DMARC, as
// computed by Gmail) with semantic colour; an inconclusive verdict hides it.
func (w *window) setAuthBadge(v authVerdict) {
	w.authLabel.RemoveCSSClass("success")
	w.authLabel.RemoveCSSClass("warning")
	w.authLabel.RemoveCSSClass("error")
	switch v.level {
	case authPass:
		w.authLabel.SetText("✓ Verified sender · " + v.detail)
		w.authLabel.AddCSSClass("success")
		w.authLabel.SetVisible(true)
	case authPartial:
		w.authLabel.SetText("Partially verified · " + v.detail)
		w.authLabel.AddCSSClass("warning")
		w.authLabel.SetVisible(true)
	case authFail:
		w.authLabel.SetText("⚠ Authentication failed — sender may be spoofed (" + v.detail + ")")
		w.authLabel.AddCSSClass("error")
		w.authLabel.SetVisible(true)
	default:
		w.authLabel.SetVisible(false)
	}
}

// setCaution shows anti-phishing heuristic warnings (hidden when there are none).
func (w *window) setCaution(warnings []string) {
	if len(warnings) == 0 {
		w.cautionLabel.SetVisible(false)
		return
	}
	w.cautionLabel.SetText("⚠ " + strings.Join(warnings, " "))
	w.cautionLabel.SetVisible(true)
}

// setImagesEnabled toggles remote-image loading and re-renders the open thread.
func (w *window) setImagesEnabled(on bool) {
	w.imagesEnabled = on
	w.webview.Settings().SetAutoLoadImages(on)
	if len(w.openThreadMsgs) > 0 {
		w.renderConversation(w.openThreadMsgs) // re-render only; keep summary as-is
	}
}

// onTranslate shows an English translation of the open message in place,
// preserving the email's markup (so styling is kept). The result is cached per
// message, so toggling back to it (or re-translating) is instant.
func (w *window) onTranslate() {
	m := w.openMsg
	if m.GmailID == "" || w.deps.Assistant == nil {
		return
	}
	if w.translateCancel != nil {
		w.translateCancel()
		w.translateCancel = nil
	}
	gmailID := m.GmailID

	show := func(translatedHTML string) {
		w.translationBanner.SetTitle("Showing translation")
		w.translationBanner.SetRevealed(true)
		cleaned, blocked := w.cleanHTML(stripCodeFence(translatedHTML))
		w.setTrackerCount(blocked)
		w.webview.LoadHtml(wrapHTML(cleaned), "about:blank")
	}

	if cached, ok := w.translationCache[gmailID]; ok {
		show(cached)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.translateCancel = cancel
	threadID := w.openThreadID
	source := w.bodyHTMLFor(m)

	w.translationBanner.SetTitle("Translating…")
	w.translationBanner.SetRevealed(true)
	w.webview.LoadHtml(wrapHTML("<p><i>Translating…</i></p>"), "about:blank")

	go func() {
		// Translate only the text segments (cheap) and reinsert them into the
		// original markup locally, so the model never regenerates the HTML.
		out, err := translateHTMLText(source, func(segs []string) ([]string, error) {
			return w.deps.Assistant.TranslateSegments(ctx, segs, "English")
		})
		dispatch.Main(func() {
			// Skip if the user switched conversations or reverted (cancels ctx).
			if w.openThreadID != threadID || ctx.Err() != nil {
				return
			}
			if err != nil {
				w.webview.LoadHtml(wrapHTML("<p>Translation failed: "+html.EscapeString(err.Error())+"</p>"), "about:blank")
				return
			}
			w.translationCache[gmailID] = out
			show(out)
		})
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
		w.renderConversation(w.openThreadMsgs) // re-render only; keep summary as-is
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

// buildSummaryCard creates the (initially hidden) AI thread-summary card shown
// at the top of the reader: a title row with a close button and the streamed
// summary below. Returns the revealer wrapping it.
func (w *window) buildSummaryCard() *gtk.Revealer {
	w.cardIcon = gtk.NewImageFromIconName("view-list-bullet-symbolic")
	w.cardIcon.AddCSSClass("summary-title")

	w.cardTitle = gtk.NewLabel("Summary")
	w.cardTitle.AddCSSClass("summary-title")
	w.cardTitle.AddCSSClass("heading")
	w.cardTitle.SetXAlign(0)
	w.cardTitle.SetHExpand(true)

	closeBtn := gtk.NewButtonFromIconName("window-close-symbolic")
	closeBtn.AddCSSClass("flat")
	closeBtn.AddCSSClass("circular")
	closeBtn.SetTooltipText("Hide")
	closeBtn.ConnectClicked(w.hideSummary)

	titleRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
	titleRow.Append(w.cardIcon)
	titleRow.Append(w.cardTitle)
	titleRow.Append(closeBtn)

	w.summaryLabel = gtk.NewLabel("")
	w.summaryLabel.SetXAlign(0)
	w.summaryLabel.SetWrap(true)
	w.summaryLabel.SetSelectable(true)

	card := gtk.NewBox(gtk.OrientationVertical, 6)
	card.AddCSSClass("summary-card")
	setMargins(card, 12, 12, 6, 6)
	card.Append(titleRow)
	card.Append(w.summaryLabel)

	w.summaryRevealer = gtk.NewRevealer()
	w.summaryRevealer.SetTransitionType(gtk.RevealerTransitionTypeSlideDown)
	w.summaryRevealer.SetChild(card)
	w.summaryRevealer.SetRevealChild(false)
	return w.summaryRevealer
}

// onSummarize reveals the summary card and streams an AI summary of the open
// thread into it. A summary cached for this exact set of messages shows
// instantly; once the thread gains a reply its fingerprint changes, so the
// cache misses and a fresh summary is generated.
func (w *window) onSummarize() {
	if len(w.openThreadMsgs) == 0 || w.deps.Assistant == nil {
		return
	}
	if w.summaryCancel != nil { // cancel a summary still streaming
		w.summaryCancel()
		w.summaryCancel = nil
	}
	w.cardIcon.SetFromIconName("view-list-bullet-symbolic")
	w.cardTitle.SetText("Summary")
	key := w.summaryKey()
	w.summaryRevealer.SetRevealChild(true)
	if cached, ok := w.summaryCache[key]; ok {
		w.summaryLabel.SetText(cached)
		return
	}

	w.summaryLabel.SetText("Summarizing…")
	ctx, cancel := context.WithCancel(context.Background())
	w.summaryCancel = cancel
	threadID := w.openThreadID
	contextText := w.threadContextAll()

	go func() {
		ch, err := w.deps.Assistant.SummarizeThread(ctx, contextText)
		if err != nil {
			msg := err.Error()
			dispatch.Main(func() {
				if w.openThreadID == threadID && ctx.Err() == nil {
					w.summaryLabel.SetText("Summary failed: " + msg)
				}
			})
			return
		}
		// acc is only ever touched inside dispatch.Main (the main thread), so the
		// builder is never accessed concurrently with the streaming goroutine.
		var acc strings.Builder
		for c := range ch {
			cc := c
			dispatch.Main(func() {
				if w.openThreadID != threadID || ctx.Err() != nil {
					return
				}
				if cc.Err != nil {
					w.summaryLabel.SetText("Summary failed: " + cc.Err.Error())
					return
				}
				acc.WriteString(cc.Text)
				w.summaryLabel.SetText(bulletize(acc.String()))
			})
		}
		dispatch.Main(func() {
			if w.openThreadID != threadID || ctx.Err() != nil {
				return
			}
			final := bulletize(strings.TrimSpace(acc.String()))
			if final != "" {
				w.summaryCache[key] = final
				w.summaryLabel.SetText(final)
			}
		})
	}()
}

// hideSummary collapses the summary card and aborts any in-flight summary.
func (w *window) hideSummary() {
	if w.summaryCancel != nil {
		w.summaryCancel()
		w.summaryCancel = nil
	}
	if w.summaryRevealer != nil {
		w.summaryRevealer.SetRevealChild(false)
	}
}

// onAnalyze runs an on-demand AI phishing/scam analysis of the open message and
// streams the verdict + reasons into the shared card. It feeds the AI the
// deterministic signals (auth result, heuristic warnings) alongside the content,
// and caches by message id so re-running is instant.
func (w *window) onAnalyze() {
	m := w.openMsg
	if m.GmailID == "" || w.deps.Assistant == nil {
		return
	}
	if w.summaryCancel != nil {
		w.summaryCancel()
		w.summaryCancel = nil
	}
	w.cardIcon.SetFromIconName("security-high-symbolic")
	w.cardTitle.SetText("Security analysis")
	w.summaryRevealer.SetRevealChild(true)
	key := "analyze:" + m.GmailID
	if cached, ok := w.summaryCache[key]; ok {
		w.summaryLabel.SetText(cached)
		return
	}

	w.summaryLabel.SetText("Analyzing…")
	ctx, cancel := context.WithCancel(context.Background())
	w.summaryCancel = cancel
	threadID := w.openThreadID
	emailCtx := w.analysisContextFor(m)

	go func() {
		ch, err := w.deps.Assistant.AnalyzeEmail(ctx, emailCtx)
		if err != nil {
			msg := err.Error()
			dispatch.Main(func() {
				if w.openThreadID == threadID && ctx.Err() == nil {
					w.summaryLabel.SetText("Analysis failed: " + msg)
				}
			})
			return
		}
		var acc strings.Builder
		for c := range ch {
			cc := c
			dispatch.Main(func() {
				if w.openThreadID != threadID || ctx.Err() != nil {
					return
				}
				if cc.Err != nil {
					w.summaryLabel.SetText("Analysis failed: " + cc.Err.Error())
					return
				}
				acc.WriteString(cc.Text)
				w.summaryLabel.SetText(bulletize(acc.String()))
			})
		}
		dispatch.Main(func() {
			if w.openThreadID != threadID || ctx.Err() != nil {
				return
			}
			final := bulletize(strings.TrimSpace(acc.String()))
			if final != "" {
				w.summaryCache[key] = final
				w.summaryLabel.SetText(final)
			}
		})
	}()
}

// analysisContextFor assembles the email plus deterministic signals (auth
// verdict, heuristic warnings) as plain text for the AI analyzer.
func (w *window) analysisContextFor(m model.Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From name: %s\nFrom address: %s\nSubject: %s\n", m.FromName, m.FromAddr, m.Subject)
	body, err := w.deps.Store.GetBody(context.Background(), m.RowID)
	if err == nil {
		if v := parseAuthResults(body.RawHeaders); v.level != authUnknown {
			fmt.Fprintf(&b, "Mail-server authentication check: %s (%s)\n", authLevelWord(v.level), v.detail)
		}
		for _, warn := range phishingWarnings(m, body.HTML) {
			fmt.Fprintf(&b, "Automated warning: %s\n", warn)
		}
	}
	text := w.bodyTextFor(m)
	const cap = 6000
	if len(text) > cap {
		text = text[:cap] + "…"
	}
	b.WriteString("\nBody:\n" + text)
	return b.String()
}

// authLevelWord describes an auth level in words for the analysis prompt.
func authLevelWord(l authLevel) string {
	switch l {
	case authPass:
		return "passed"
	case authPartial:
		return "partially passed"
	case authFail:
		return "FAILED"
	default:
		return "unknown"
	}
}

// summaryKey fingerprints the open thread by its message ids, so the cached
// summary is reused only while the conversation is unchanged.
func (w *window) summaryKey() string {
	var b strings.Builder
	b.WriteString(w.openThreadID)
	for _, m := range w.openThreadMsgs {
		b.WriteByte('|')
		b.WriteString(m.GmailID)
	}
	return b.String()
}

// threadContextAll renders the whole open thread as plain text (oldest first)
// for summarization, capping each body so very long threads stay within a
// reasonable token budget.
func (w *window) threadContextAll() string {
	const maxPerMsg = 4000
	var b strings.Builder
	for _, m := range w.openThreadMsgs {
		fmt.Fprintf(&b, "From: %s\nDate: %s\nSubject: %s\n\n",
			displayFrom(m), m.InternalDate.Format("Jan 2, 2006 15:04"), m.Subject)
		body := w.bodyTextFor(m)
		if len(body) > maxPerMsg {
			body = body[:maxPerMsg] + "…"
		}
		b.WriteString(body)
		b.WriteString("\n\n---\n\n")
	}
	return b.String()
}

// bulletize rewrites Markdown-style "- "/"* " line prefixes as "•  " bullets so
// the model's plain-text summary reads cleanly in the card.
func bulletize(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		t := strings.TrimLeft(ln, " \t")
		switch {
		case strings.HasPrefix(t, "- "):
			lines[i] = "•  " + t[2:]
		case strings.HasPrefix(t, "* "):
			lines[i] = "•  " + t[2:]
		}
	}
	return strings.Join(lines, "\n")
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

// applyLabels applies a label change to the given messages in one batch (one
// Gmail round-trip, one UI refresh), then refreshes the label counts and the
// current list (preserving any search). If after is non-nil it runs on the main
// thread once the list has refreshed.
func (w *window) applyLabels(msgs []model.Message, add, remove []string, after func()) {
	if w.deps.ModifyLabels == nil || len(msgs) == 0 {
		return
	}
	accountID := msgs[0].AccountID
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.GmailID
	}
	go func() {
		start := time.Now()
		if err := w.deps.ModifyLabels(context.Background(), accountID, ids, add, remove); err != nil {
			slog.Warn("ui: apply labels", "n", len(ids), "err", err)
		}
		slog.Debug("ui: applyLabels", "n", len(ids), "dur", time.Since(start))
		dispatch.Main(func() {
			t := time.Now()
			w.loadLabels()
			w.refreshList(w.searchEntry.Text())
			if after != nil {
				after()
			}
			slog.Debug("ui: applyLabels refresh", "dur", time.Since(t))
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

// sendUndoDelay is how long a sent message is held (with an Undo toast) before
// it actually goes out.
const sendUndoDelay = 5 * time.Second

// deferSend holds an outgoing message for sendUndoDelay, showing an "Undo" toast;
// if the user doesn't undo, it sends. Undo reopens the message in compose. The
// compose window has already closed, so this runs at the main-window level.
// (Caveat: quitting within the window drops the unsent message.)
func (w *window) deferSend(accountID int64, msg model.OutgoingMessage) {
	cancelled := false
	toast := adw.NewToast("Sending…")
	toast.SetButtonLabel("Undo")
	toast.SetTimeout(0) // we control the lifetime via the timer below
	toast.ConnectButtonClicked(func() {
		cancelled = true
		toast.Dismiss()
		// Reopen the message exactly as it was (no second signature).
		w.openComposeOpts(msg, "", "Message", false, false)
	})
	w.toastOverlay.AddToast(toast)

	go func() {
		time.Sleep(sendUndoDelay)
		dispatch.Main(func() {
			if cancelled {
				return
			}
			toast.Dismiss()
			w.reallySend(accountID, msg)
		})
	}()
}

// reallySend performs the actual send (after the undo window elapsed). On
// failure engine.Send queues it to the outbox, surfaced via the outbox banner.
func (w *window) reallySend(accountID int64, msg model.OutgoingMessage) {
	go func() {
		err := w.deps.Send(context.Background(), accountID, msg)
		dispatch.Main(func() {
			if err != nil {
				slog.Warn("ui: send", "err", err)
				w.toast("Send failed — kept in Outbox")
				w.refreshOutbox()
			}
		})
	}()
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
// notifies for genuinely new inbox mail on any account. The refresh is coalesced
// so a burst of per-message events from a sync triggers one refresh, not N.
func (w *window) onChange(c syncer.Change) {
	switch c.Kind {
	case syncer.MessageUpserted, syncer.MessageDeleted:
		if c.AccountID == w.activeID {
			w.scheduleRefresh(true) // loadLabels (inside) refreshes badges + title
		} else {
			w.refreshAccountBadges() // a sibling account's unread count changed
			w.updateTitle()
		}
	case syncer.LabelsSynced:
		if c.AccountID == w.activeID {
			w.scheduleRefresh(false)
		} else {
			w.refreshAccountBadges()
			w.updateTitle()
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

// scheduleRefresh coalesces refreshes from a burst of change events: the first
// call schedules a single loadLabels (+ thread list when withList) on the idle
// queue; further calls before it runs are folded into that one refresh. This
// keeps a sync that upserts many messages from rebuilding the UI N times.
func (w *window) scheduleRefresh(withList bool) {
	if withList {
		w.refreshListPending = true
	}
	if w.refreshPending {
		return
	}
	w.refreshPending = true
	dispatch.Main(func() {
		w.refreshPending = false
		list := w.refreshListPending
		w.refreshListPending = false
		w.loadLabels()
		if list {
			w.liveRefreshList()
		}
	})
}

func (w *window) notifyNewMail(accountID int64, m model.Message) {
	n := gio.NewNotification("New mail")
	body := displayFrom(m)
	if m.Subject != "" {
		body += " — " + m.Subject
	}
	n.SetBody(body)
	// Clicking the notification opens this message (see registerActions).
	target := glib.NewVariantString(fmt.Sprintf("%d|%s", accountID, m.GmailID))
	n.SetDefaultAction(gio.ActionPrintDetailedName("app.open-message", target))
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
		box.Append(countBadge(unread))
	}
	return box
}

// countBadge returns an accent-pill label showing n (used for folder unread
// counts and per-account unread totals).
func countBadge(n int) *gtk.Label {
	c := gtk.NewLabel(fmt.Sprintf("%d", n))
	c.AddCSSClass("badge-pill")
	c.AddCSSClass("numeric")
	c.SetVAlign(gtk.AlignCenter)
	return c
}

func threadRow(t model.ThreadSummary, outgoing bool) *gtk.Box {
	m := t.Latest
	unread := t.UnreadCount > 0

	box := gtk.NewBox(gtk.OrientationVertical, 2)
	setMargins(box, 12, 12, 6, 6)

	top := gtk.NewBox(gtk.OrientationHorizontal, 6)
	if unread {
		// A small accent dot marks an unread conversation at a glance.
		dot := gtk.NewLabel("●")
		dot.AddCSSClass("unread-dot")
		dot.SetVAlign(gtk.AlignCenter)
		top.Append(dot)
	}
	// In Sent/Drafts the sender is always you, so show the recipient instead.
	fromText := displayFrom(m)
	if outgoing {
		fromText = "To: " + displayTo(m)
	}
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
	if m.IsStarred {
		top.Append(gtk.NewImageFromIconName("starred-symbolic"))
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

// displayTo returns a concise recipient label for outgoing mail: the first
// address in the To header, with "+N" when there are more.
func displayTo(m model.Message) string {
	to := strings.TrimSpace(m.ToAddrs)
	if to == "" {
		return "(no recipient)"
	}
	if addrs, err := mail.ParseAddressList(to); err == nil && len(addrs) > 0 {
		first := addrs[0].Name
		if first == "" {
			first = addrs[0].Address
		}
		if len(addrs) > 1 {
			return fmt.Sprintf("%s +%d", first, len(addrs)-1)
		}
		return first
	}
	return to
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
