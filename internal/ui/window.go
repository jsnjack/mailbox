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
	"regexp"
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
	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/config"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/syncer"
	"github.com/microcosm-cc/bluemonday"
)

// window owns the widget tree and the currently displayed selection.
type window struct {
	app  *adw.Application
	deps Deps

	win          *adw.ApplicationWindow
	toastOverlay *adw.ToastOverlay
	outerSplit   *adw.NavigationSplitView
	innerSplit   *adw.NavigationSplitView

	// Bottom status bar: activity-first (spinner + current op + live elapsed on
	// the left); cumulative session stats + the log live in a popover.
	statusSpinner    *adw.Spinner
	statusLabel      *gtk.Label
	statusStatsLabel *gtk.Label           // session stats inside the popover
	statusLogBox     *gtk.Box             // log lines (newest first) inside the popover
	statusLogBtn     *gtk.MenuButton      // opens the activity-log popover
	statusActive     []string             // labels of in-flight operations, most recent last
	statusStarted    map[string]time.Time // op label → start time (elapsed + duration)
	statusProgText   map[string]string    // op label → bounded "N/M" progress text
	statusLogLines   int                  // current number of log rows (capped)
	activityTimer    glib.SourceHandle
	lastSyncLabel    string // idle text once a sync has completed
	accountBox       *gtk.ListBox
	// accountHeader wraps the switcher list-box and its separator; it is hidden
	// when no account is connected (zero-account first run) and revealed once the
	// first account is added, so the switcher appears without a restart.
	accountHeader *gtk.Box
	// accountNames maps account email → user-assigned display name ("Home",
	// "Work"); accountBadges maps account id → its unread-inbox count pill in the
	// switcher, so badges can refresh in place when any account syncs.
	accountNames  map[string]string
	accountBadges map[int64]*gtk.Label
	// signature is the default text appended to composed messages (configurable
	// in Preferences); empty means none.
	signature    string
	labelBox     *gtk.ListBox
	newBtn       *gtk.Button // "New message" — gated on having a connected account
	refreshBtn   *gtk.Button
	syncSpinner  *adw.Spinner             // shown in place of refreshBtn during a manual sync
	sidebar      []sidebarItem            // one entry per row in labelBox (incl. headings)
	sidebarSig   string                   // signature of the rendered sidebar, to skip no-op rebuilds
	sectionCache map[string]cachedSection // rendered message sections, reused across thread re-opens
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
	threadModel *gtk.StringList
	threadSel   *gtk.SingleSelection
	threadView  *gtk.ListView
	threadStack *gtk.Stack      // "list" vs "empty" placeholder
	emptyPage   *adw.StatusPage // the "empty" placeholder (text set per context)
	readerStack *gtk.Stack      // "message" vs "empty" placeholder
	readerCover *gtk.Box        // opaque cover over the webview during a load (hides the swap flash)
	listMenuBtn *gtk.MenuButton // thread-list overflow (unread-only filter + mark-all-read)
	unreadOnly  bool
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
	authBanner        *adw.Banner // revealed when an account's sign-in expired/was revoked
	authExpiredID     int64       // the account the auth banner's Reconnect targets (0 = none/unknown)
	searchEntry       *gtk.SearchEntry
	suppressSearch    bool   // guards SetText from firing a search during label switch
	serverSearch      bool   // current search is a Gmail server-side search, not local FTS
	serverQuery       string // the active server-search query (guards the debounced change signal)
	threadByID        map[string]model.ThreadSummary
	threadIDs         []string          // displayed thread ids, in order (for incremental diffing)
	rowSig            map[string]string // last-rendered signature per row, to detect in-place changes

	// coalesce refreshes triggered by bursts of sync change events.
	refreshPending       bool
	refreshListPending   bool
	refreshThreadPending bool // re-render the open conversation on the next refresh
	// refreshGen increments on every list query; an async query whose result
	// arrives after a newer one was issued is discarded (last request wins).
	refreshGen uint64
	// afterPopulate runs once after the next list populate, then clears. Used by
	// launch hooks that must act on the loaded list (now that loads are async).
	afterPopulate func()

	header       *gtk.Label
	attachBox    *gtk.FlowBox // chips for the open message's attachments (wraps, never forces width)
	trackerLabel *gtk.Label   // "N trackers blocked" indicator
	authLabel    *gtk.Label   // sender authentication (SPF/DKIM/DMARC) badge
	cautionLabel *gtk.Label   // anti-phishing heuristic warnings
	webview      *webkit.WebView
	readerZoom   float64 // reader message zoom (Ctrl +/-/0), persisted
	sanitizer    *bluemonday.Policy

	// reader: the open conversation. openMsg is its newest message (used for
	// reply/forward/star/unread); openThreadMsgs is all of them (oldest first).
	openThreadID   string
	openThreadMsgs []model.Message
	openMsg        model.Message
	replyAllBtn    *adw.SplitButton // primary action; dropdown has Reply/Forward
	aiReplyBtn     *gtk.MenuButton  // AI reply: popover of suggestions + intents
	archiveBtn     *gtk.Button
	translateBtn   *gtk.Button
	overflowBtn    *gtk.MenuButton   // star/unread/trash/images live here (native menu model)
	starAction     *gio.SimpleAction // stateful: the open message's Starred toggle
	imagesAction   *gio.SimpleAction // stateful: the reader's remote-images toggle
	unreadAction   *gio.SimpleAction // stateful: the thread-list "show unread only" filter
	imagesEnabled  bool              // whether remote images are loaded in the reader
	blockImages    bool              // global default: block remote images (Preferences)

	// AI inbox categorization: per-thread category cache (thread id → category),
	// computed in the background for the inbox. inboxCategories gates it.
	// categorizedMsg records the latest message id each thread's category was
	// computed for, so a thread is re-categorized when a new message arrives (e.g.
	// a "Needs reply" thread that gets a discount reply becomes "Discount").
	categories      map[string]string
	categorizedMsg  map[string]string
	categorizing    bool
	inboxCategories bool

	// AI thread summary: a button reveals a card that streams a summary in.
	// summaryCache memoizes by the thread's message fingerprint, so reopening is
	// instant and a new reply (different fingerprint) re-generates automatically.
	summaryBtn      *gtk.Button
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
		categories:       map[string]string{},
		categorizedMsg:   map[string]string{},
	}
	w.accountNames, _ = config.LoadAccountNames()
	if p, err := config.LoadPrefs(); err == nil {
		w.blockImages = p.BlockRemoteImages
		w.inboxCategories = !p.DisableInboxCategories
	}
	if len(deps.Accounts) > 0 {
		w.activeID = deps.Accounts[0].ID
		w.activeEmail = deps.Accounts[0].Email
	}
	// Signature the compose window appends for the active account.
	w.signature = w.signatureForActive()
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

// registerAppMenuActions wires the primary-menu actions: Preferences (gated on
// whether settings are wired) and About. (Keyboard Shortcuts is registered in
// addShortcuts as win.show-help-overlay.)
func (w *window) registerAppMenuActions() {
	pref := gio.NewSimpleAction("preferences", nil)
	pref.ConnectActivate(func(*glib.Variant) { w.openSettings() })
	pref.SetEnabled(w.deps.AISettings != nil)
	w.win.AddAction(pref)

	about := gio.NewSimpleAction("about", nil)
	about.ConnectActivate(func(*glib.Variant) { w.showAbout() })
	w.win.AddAction(about)

	addAcct := gio.NewSimpleAction("add-account", nil)
	addAcct.ConnectActivate(func(*glib.Variant) { w.openAddAccount(nil) })
	addAcct.SetEnabled(w.deps.AddIMAPAccount != nil)
	w.win.AddAction(addAcct)
}

// showAbout presents the standard Adwaita About dialog (app identity, version,
// links). The icon name matches the app id so a real install shows the icon.
func (w *window) showAbout() {
	about := adw.NewAboutDialog()
	about.SetApplicationName("Mailbox")
	about.SetApplicationIcon(appID)
	about.SetDeveloperName("Yauhen Shulitski")
	if v := w.deps.Version; v != "" {
		about.SetVersion(v)
	}
	about.SetComments("A native, fast Gmail client for Linux/GNOME.")
	about.SetWebsite("https://github.com/jsnjack/mailbox")
	about.SetIssueURL("https://github.com/jsnjack/mailbox/issues")
	about.SetLicenseType(gtk.LicenseMITX11)
	about.Present(w.win)
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
	w.toastOverlay.SetVExpand(true)

	root := gtk.NewBox(gtk.OrientationVertical, 0)
	root.Append(w.toastOverlay)
	root.Append(w.buildStatusBar())
	w.win.SetContent(root)
	w.subscribeActivity()
	w.addBreakpoints()
	w.addShortcuts()
}

// addShortcuts wires single-key navigation/actions. The controller runs in the
// capture phase so the shortcut fires even when focus is inside the message
// WebView or the thread list (which would otherwise swallow the key); it bails
// out when a text field is focused so typing in search still works. Keyvals for
// printable keys equal their ASCII rune.
func (w *window) addShortcuts() {
	// The cheat sheet is reachable three ways: the conventional
	// win.show-help-overlay action (used by the primary menu), the GNOME-standard
	// <Ctrl>? accelerator, and a bare "?" matching the app's single-key scheme.
	// (GtkShortcutsWindow, the old standard surface, is deprecated since GTK 4.18,
	// and AdwShortcutsDialog isn't in the pinned binding — so this stays a custom
	// AdwDialog, which is the current recommendation.)
	help := gio.NewSimpleAction("show-help-overlay", nil)
	help.ConnectActivate(func(*glib.Variant) { w.showShortcuts() })
	w.win.AddAction(help)
	w.app.SetAccelsForAction("win.show-help-overlay", []string{"<Control>question"})

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
				w.openCompose(model.OutgoingMessage{}, "", "New message")
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

// toggleStar flips the star on the open conversation. No-op when nothing is open.
func (w *window) toggleStar() {
	if w.openMsg.GmailID == "" {
		return
	}
	w.setStarred(!w.openMsg.IsStarred)
}

// setStarred adds or removes the star across the whole open conversation
// (optimistic), keeping openMsg's flag in sync so the overflow checkbox and the
// 's' shortcut agree. It stars the entire thread, not just the newest message,
// so unstarring actually removes the conversation from the Starred folder (which
// lists any thread with any starred message) rather than leaving older replies
// starred.
func (w *window) setStarred(star bool) {
	if w.openMsg.GmailID == "" {
		return
	}
	w.openMsg.IsStarred = star
	msgs := w.openThreadMsgs
	if len(msgs) == 0 {
		msgs = []model.Message{w.openMsg}
	}
	if star {
		w.applyLabels(msgs, []string{model.LabelStarred}, nil, nil)
	} else {
		w.applyLabels(msgs, nil, []string{model.LabelStarred}, nil)
	}
}

// goBack collapses the reader back to the thread list — meaningful when the
// window is narrow enough that the panes are stacked.
func (w *window) goBack() {
	w.innerSplit.SetShowContent(false)
}

// showConnectHelp explains how to enable live features when the app is running
// read-only (no Gmail client could be built).
// onReconnect re-authenticates the account whose sign-in expired by reopening the
// add-account dialog prefilled for it (same email → cache preserved). When the
// expired account can't be identified it falls back to the plain Add account
// dialog, or to read-only guidance if account management isn't available.
func (w *window) onReconnect() {
	if w.deps.AddIMAPAccount == nil {
		w.showConnectHelp()
		return
	}
	var target AccountInfo
	for _, a := range w.deps.Accounts {
		if a.ID == w.authExpiredID {
			target = a
			break
		}
	}
	if target.ID == 0 {
		if len(w.deps.Accounts) == 1 {
			target = w.deps.Accounts[0] // unambiguous
		} else {
			w.openAddAccount(nil)
			return
		}
	}
	w.reconnectAccount(target)
}

// showConnectHelp explains how to restore a read-only account when in-app
// reconnect isn't available (no provider credentials configured).
func (w *window) showConnectHelp() {
	body := "Mailbox can't reach this account's mail server, so it's showing the " +
		"local cache read-only.\n\nReconnect it from the main menu → Add account…, " +
		"using the same email address (your cached mail is kept). For Gmail you'll " +
		"sign in again; for other providers, re-enter your app password."
	dialog := adw.NewAlertDialog("Not connected", body)
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
		// The live handler is debounced; apply directly so a paired
		// MAILBOX_OPEN_FIRST selects from the search results, not the inbox.
		w.suppressSearch = true
		w.searchEntry.SetText(q)
		w.suppressSearch = false
		w.refreshList(q)
	}
	if os.Getenv("MAILBOX_OPEN_FIRST") == "1" {
		// List loads are async; select the newest thread once it has populated.
		w.afterPopulate = func() {
			if w.threadModel.NItems() > 0 {
				w.threadSel.SetSelected(0)
			}
		}
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
	// Always build the account list-box (even for zero or one account) so the
	// switcher can be populated and revealed in place when an account is added at
	// runtime. accountHeader is hidden until at least one account is connected.
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
	w.accountHeader = gtk.NewBox(gtk.OrientationVertical, 0)
	w.accountHeader.Append(w.accountBox)
	w.accountHeader.Append(gtk.NewSeparator(gtk.OrientationHorizontal))
	w.accountHeader.SetVisible(len(w.deps.Accounts) >= 1)
	box.Append(w.accountHeader)
	box.Append(scroller)

	hb := adw.NewHeaderBar()
	w.newBtn = gtk.NewButtonFromIconName("mail-message-new-symbolic")
	w.newBtn.SetTooltipText("New message")
	w.newBtn.SetSensitive(w.deps.Send != nil && len(w.deps.Accounts) > 0)
	w.newBtn.ConnectClicked(func() {
		w.openCompose(model.OutgoingMessage{}, "", "New message")
	})
	hb.PackStart(w.newBtn)

	w.refreshBtn = gtk.NewButtonFromIconName("view-refresh-symbolic")
	w.refreshBtn.SetTooltipText("Sync now")
	w.refreshBtn.SetSensitive(w.deps.Sync != nil && len(w.deps.Accounts) > 0)
	w.refreshBtn.ConnectClicked(w.onRefresh)

	w.syncSpinner = adw.NewSpinner()
	w.syncSpinner.SetTooltipText("Syncing…")
	w.syncSpinner.SetVisible(false)

	// Primary (hamburger) menu — the GNOME-standard home for Preferences,
	// Keyboard Shortcuts and About, consolidating what used to be a lone gear.
	w.registerAppMenuActions()
	menu := gio.NewMenu()
	acct := gio.NewMenu()
	acct.Append("Add account…", "win.add-account")
	menu.AppendSection("", acct)
	pref := gio.NewMenu()
	pref.Append("Preferences", "win.preferences")
	menu.AppendSection("", pref)
	about := gio.NewMenu()
	about.Append("Keyboard Shortcuts", "win.show-help-overlay")
	about.Append("About Mailbox", "win.about")
	menu.AppendSection("", about)

	primaryBtn := gtk.NewMenuButton()
	primaryBtn.SetIconName("open-menu-symbolic")
	primaryBtn.SetTooltipText("Main menu")
	primaryBtn.SetMenuModel(menu)
	// PackEnd is right-to-left: the primary menu sits at the trailing edge (GNOME
	// convention), with refresh — or its sync spinner — to its left.
	hb.PackEnd(primaryBtn)
	hb.PackEnd(w.refreshBtn)
	hb.PackEnd(w.syncSpinner)

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(box)
	return adw.NewNavigationPage(tv, "Mailbox")
}

// accountSwitcherRow builds a sidebar account entry: the display name (custom
// name if set, else the email) with the email as a caption when a custom name
// replaces it, and an unread-inbox count pill. The badge is recorded in
// accountBadges so applyAccountUnread can update it in place.
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
	if w.accountHeader != nil {
		w.accountHeader.SetVisible(len(w.deps.Accounts) >= 1)
	}
	w.refreshAccountUnread()
}

// addAccount registers a just-added account in the switcher live — it's already
// syncing (the launcher started it), so it shows up and is selectable without a
// restart. Main-thread only.
func (w *window) addAccount(a AccountInfo) {
	if a.ID == w.authExpiredID {
		// This was a reconnect of the expired account — it's syncing again.
		w.authExpiredID = 0
		w.authBanner.SetRevealed(false)
	}
	for _, e := range w.deps.Accounts {
		if e.ID == a.ID {
			return // already present (a reconnect re-adds the same id)
		}
	}
	first := len(w.deps.Accounts) == 0
	w.deps.Accounts = append(w.deps.Accounts, a)
	if w.accountBox != nil {
		w.rebuildAccountSwitcher()
	}
	if first {
		// Coming from a zero-account first run: enable compose/sync and switch the
		// (until-now empty) UI to the new account so its mail loads. setActiveAccount
		// no-ops if a.ID already matches, so force it from the sentinel id.
		if w.newBtn != nil {
			w.newBtn.SetSensitive(w.deps.Send != nil)
		}
		if w.refreshBtn != nil {
			w.refreshBtn.SetSensitive(w.deps.Sync != nil)
		}
		w.activeID = 0
		w.setActiveAccount(a)
		if r := w.accountBox.RowAtIndex(0); r != nil {
			w.accountBox.SelectRow(r)
		}
	}
}

// removeAccountFromUI drops a just-removed account from the switcher and, when it
// was the active one, switches to another account (or the zero-account welcome
// state if it was the last). Main-thread only; the backend teardown + data delete
// already happened in deps.RemoveAccount.
func (w *window) removeAccountFromUI(id int64) {
	idx := -1
	for i, a := range w.deps.Accounts {
		if a.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	wasActive := w.activeID == id
	w.deps.Accounts = append(w.deps.Accounts[:idx], w.deps.Accounts[idx+1:]...)
	w.rebuildAccountSwitcher() // re-renders rows + hides the header when none remain
	if id == w.authExpiredID {
		w.authExpiredID = 0
		w.authBanner.SetRevealed(false)
	}
	if len(w.deps.Accounts) == 0 {
		// Back to a clean first-run state.
		w.activeID, w.activeEmail = 0, ""
		if w.newBtn != nil {
			w.newBtn.SetSensitive(false)
		}
		if w.refreshBtn != nil {
			w.refreshBtn.SetSensitive(false)
		}
		w.clearReader()
		w.loadLabels()
		w.selectLabel(model.LabelInbox)
		return
	}
	if wasActive {
		// Switch to the first remaining account (setActiveAccount no-ops when the id
		// already matches, so clear it first).
		w.activeID = 0
		w.setActiveAccount(w.deps.Accounts[0])
		if r := w.accountBox.RowAtIndex(0); r != nil {
			w.accountBox.SelectRow(r)
		}
	}
}

func (w *window) buildThreadList() *adw.NavigationPage {
	w.registerListActions()
	w.threadByID = make(map[string]model.ThreadSummary)
	w.rowSig = make(map[string]string)
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
		// Keep the signature cache in step with what is actually on screen, so a
		// scroll-recycled row never looks "unchanged" to the next diff.
		w.rowSig[id] = w.renderSig(id)
		row := threadRow(w.threadByID[id], outgoing, w.categories[id])
		// Right-click a row for quick actions (archive/star/read/trash) without
		// opening it. A fresh row+gesture is created each bind, so the captured id
		// always matches what's shown.
		if !w.selectMode && w.deps.ModifyLabels != nil {
			rc := gtk.NewGestureClick()
			rc.SetButton(3) // secondary (right) button
			rc.ConnectPressed(func(_ int, x, y float64) {
				w.showRowMenu(row, id, x, y)
			})
			row.AddController(rc)
		}
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
	// leaving the actions silently inert. MAILBOX_DEMO hides it for screenshots
	// taken against a synthetic cache that has no Gmail client by design.
	w.readOnlyBanner = adw.NewBanner("Read-only — not connected to Gmail")
	w.readOnlyBanner.SetButtonLabel("How to connect")
	w.readOnlyBanner.ConnectButtonClicked(w.showConnectHelp)
	w.readOnlyBanner.SetRevealed(w.deps.ModifyLabels == nil && os.Getenv("MAILBOX_DEMO") == "")

	w.buildSelectionBar()

	w.emptyFolderBanner = adw.NewBanner("")
	w.emptyFolderBanner.SetButtonLabel("Empty now")
	w.emptyFolderBanner.SetRevealed(false)
	w.emptyFolderBanner.ConnectButtonClicked(w.onEmptyFolder)

	// Revealed when an account's refresh token is revoked/expired (a sync hit
	// invalid_grant): the account can't recover without re-login, so say so
	// instead of silently failing to sync.
	w.authBanner = adw.NewBanner("")
	w.authBanner.SetButtonLabel("Reconnect")
	w.authBanner.SetRevealed(false)
	w.authBanner.ConnectButtonClicked(w.onReconnect)

	content := gtk.NewBox(gtk.OrientationVertical, 0)
	content.Append(w.readOnlyBanner)
	content.Append(w.authBanner)
	content.Append(w.outboxBanner)
	content.Append(w.emptyFolderBanner)
	content.Append(w.searchEntry)
	content.Append(w.selectionBar)
	content.Append(w.threadStack)

	hb := adw.NewHeaderBar()
	hb.SetShowTitle(false) // "Messages" is redundant — the pane is self-evident

	// Infrequent list-scope actions (unread-only filter, mark-all-read) live in a
	// small overflow menu rather than cluttering the header. Rebuilt per open so
	// it reflects the current filter state and folder.
	w.listMenuBtn = gtk.NewMenuButton()
	w.listMenuBtn.SetIconName("view-more-symbolic")
	w.listMenuBtn.SetTooltipText("View options")
	// Native menu model: a check item for the unread filter, mark-all-read where
	// it applies. Rebuilt per open (the folder gates mark-all-read), with the
	// toggle state synced first.
	w.listMenuBtn.SetCreatePopupFunc(func(btn *gtk.MenuButton) {
		w.unreadAction.SetState(glib.NewVariantBoolean(w.unreadOnly))
		btn.SetPopover(gtk.NewPopoverMenuFromModel(w.buildListMenuModel()))
	})
	hb.PackEnd(w.listMenuBtn)

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

// registerListActions registers the win.* actions backing the thread-list
// overflow menu and the per-row right-click menu, so both render as native
// GMenu models. The row actions take the clicked row's thread id as a string
// target, since one action serves whichever row was right-clicked.
func (w *window) registerListActions() {
	// Overflow: unread-only is a stateful toggle (native checkmark); mark-all-read
	// is a plain action.
	w.unreadAction = gio.NewSimpleActionStateful("list-unread-only", nil, glib.NewVariantBoolean(w.unreadOnly))
	w.unreadAction.ConnectChangeState(func(v *glib.Variant) {
		w.unreadAction.SetState(v)
		w.unreadOnly = v.Boolean()
		w.refreshList(w.searchEntry.Text())
		w.saveViewState()
	})
	w.win.AddAction(w.unreadAction)

	markAll := gio.NewSimpleAction("list-mark-all-read", nil)
	markAll.ConnectActivate(func(*glib.Variant) { w.onMarkAllRead() })
	w.win.AddAction(markAll)

	recat := gio.NewSimpleAction("list-recategorize", nil)
	recat.ConnectActivate(func(*glib.Variant) { w.onRecategorize() })
	w.win.AddAction(recat)

	// Per-row context actions; each carries the row's thread id.
	row := func(name string, fn func(threadID string)) {
		act := gio.NewSimpleAction(name, glib.NewVariantType("s"))
		act.ConnectActivate(func(p *glib.Variant) {
			if p != nil {
				fn(p.String())
			}
		})
		w.win.AddAction(act)
	}
	row("row-archive", func(id string) { w.threadModifyAll(id, "Archived", nil, []string{model.LabelInbox}) })
	row("row-move-inbox", func(id string) {
		w.threadModifyAll(id, "Moved to Inbox", []string{model.LabelInbox}, []string{model.LabelTrash, model.LabelSpam})
	})
	row("row-trash", func(id string) {
		w.threadModifyAll(id, "Moved to Trash", []string{model.LabelTrash}, []string{model.LabelInbox})
	})
	row("row-mark-read", func(id string) { w.threadModifyAll(id, "Marked as read", nil, []string{model.LabelUnread}) })
	// Star/unstar the whole thread (no toast — trivially reversible), so unstarring
	// clears it from the Starred folder instead of leaving older replies starred.
	row("row-star", func(id string) { w.threadModifyAll(id, "", []string{model.LabelStarred}, nil) })
	row("row-unstar", func(id string) { w.threadModifyAll(id, "", nil, []string{model.LabelStarred}) })
	row("row-mark-unread", func(id string) {
		w.rowLatest(id, func(m model.Message) { w.applyLabels([]model.Message{m}, []string{model.LabelUnread}, nil, nil) })
	})
	row("row-recategorize", func(id string) { w.recategorizeThread(id) })
}

// recategorizeThread re-runs AI categorization for a single conversation: it
// drops the thread's cached tag (in memory + the persisted entry for its latest
// message) so the next pass re-classifies it, then triggers that pass.
func (w *window) recategorizeThread(threadID string) {
	t, ok := w.threadByID[threadID]
	if !ok || w.deps.Assistant == nil {
		return
	}
	msgID := t.Latest.GmailID
	delete(w.categories, threadID)
	delete(w.categorizedMsg, threadID)
	acctID := w.activeID
	go func() {
		if err := w.deps.Store.ClearMessageCategory(context.Background(), acctID, msgID); err != nil {
			slog.Warn("ui: clear message category", "id", msgID, "err", err)
		}
		dispatch.Main(func() {
			if w.activeID != acctID {
				return
			}
			w.refreshList(w.searchEntry.Text()) // drop the stale tag, then re-classify
			w.categorizeInbox()
		})
	}()
}

// rowLatest runs fn with the newest message of the given thread, if it is still
// known in the list model (the row may have been recycled).
func (w *window) rowLatest(threadID string, fn func(model.Message)) {
	if t, ok := w.threadByID[threadID]; ok {
		fn(t.Latest)
	}
}

// buildListMenuModel is the thread-list overflow menu model: the unread-only
// filter (a native check item) and, where it applies, mark-all-read. Rebuilt
// per open so it reflects the current folder.
func (w *window) buildListMenuModel() *gio.Menu {
	menu := gio.NewMenu()
	menu.Append("Show unread only", "win.list-unread-only")
	// "Mark all read" is meaningful per folder, but not for the All Mail view
	// (it spans every label and Gmail offers no such bulk op there).
	if w.deps.MarkAllRead != nil && w.current != allMailID {
		sec := gio.NewMenu()
		sec.Append("Mark all as read", "win.list-mark-all-read")
		menu.AppendSection("", sec)
	}
	// Re-classify the inbox from scratch (clears the cached categories so a prompt
	// change or a fresh judgment takes effect). Only where categorization runs.
	if w.current == model.LabelInbox && w.deps.Assistant != nil && w.inboxCategories {
		sec := gio.NewMenu()
		sec.Append("Re-categorize inbox", "win.list-recategorize")
		menu.AppendSection("", sec)
	}
	return menu
}

// onRecategorize clears the active account's cached inbox categories (in memory
// and persisted) and re-runs categorization, so a category-prompt change or a
// fresh judgment is reflected — categories are otherwise classified once and
// cached. It re-bills the AI for the inbox, so it is a deliberate menu action.
func (w *window) onRecategorize() {
	if w.deps.Assistant == nil {
		return
	}
	acctID := w.activeID
	go func() {
		if err := w.deps.Store.ClearCategories(context.Background(), acctID); err != nil {
			slog.Warn("ui: clear categories", "err", err)
		}
		dispatch.Main(func() {
			if w.activeID != acctID {
				return // account switched while clearing
			}
			w.categories = map[string]string{}
			w.categorizedMsg = map[string]string{}
			// Re-populating the list runs categorizeInbox afresh (no cache to skip).
			w.refreshList(w.searchEntry.Text())
		})
	}()
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

	archive := gtk.NewButtonFromIconName("mail-archive-symbolic")
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
	w.showUndoToast(fmt.Sprintf("%s %d conversations", verb, n), msgs, add, remove)
}

// onSearchAllMail runs a Gmail server-side search for the current query, caches
// the matches, and shows them — finding mail beyond the local cache.
func (w *window) onSearchAllMail() {
	w.serverSearch = true // stay in server-search mode across refreshes
	w.runServerSearch(strings.TrimSpace(w.searchEntry.Text()))
}

// runServerSearch executes the Gmail server-side search for q and shows the
// results. refreshList calls this (instead of local FTS) while serverSearch is on.
func (w *window) runServerSearch(q string) {
	if q == "" || w.deps.SearchServer == nil {
		return
	}
	w.serverQuery = q
	w.emptyPage.SetChild(nil)
	w.emptyPage.SetIconName("edit-find-symbolic")
	w.emptyPage.SetTitle("Searching all mail…")
	w.emptyPage.SetDescription("")
	acctID := w.activeID
	w.refreshGen++
	gen := w.refreshGen
	go func() {
		ctx := context.Background()
		sums, err := w.serverSearchThreads(ctx, acctID, q)
		dispatch.Main(func() {
			if gen != w.refreshGen || !w.serverSearch ||
				strings.TrimSpace(w.searchEntry.Text()) != q || w.activeID != acctID {
				return // mode/query/account changed while searching
			}
			if err != nil {
				slog.Warn("ui: search all mail", "err", err)
				w.toast("Couldn't search all mail")
				w.showThreads(nil)
				return
			}
			w.showThreads(sums)
			if len(sums) == 0 {
				w.toast("No messages found")
			}
		})
	}()
}

// serverSearchThreads runs the Gmail server-side search and groups the matched
// message ids into thread summaries, newest-first. The id→thread mapping and the
// summaries are each fetched in one batched query rather than per matched id.
func (w *window) serverSearchThreads(ctx context.Context, acctID int64, q string) ([]model.ThreadSummary, error) {
	ids, err := w.deps.SearchServer(ctx, acctID, q, 50)
	if err != nil {
		return nil, err
	}
	idToThread, err := w.deps.Store.ThreadIDsForMessages(ctx, acctID, ids)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(ids))
	tids := make([]string, 0, len(ids))
	for _, id := range ids { // preserve the server's relevance order
		t, ok := idToThread[id]
		if !ok || seen[t] {
			continue
		}
		seen[t] = true
		tids = append(tids, t)
	}
	sums, err := w.deps.Store.GetThreadSummaries(ctx, acctID, tids)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(sums, func(i, j int) bool {
		return sums[i].Latest.InternalDate.After(sums[j].Latest.InternalDate)
	})
	return sums, nil
}

func (w *window) onSearchChanged() {
	if w.suppressSearch {
		return
	}
	// The search-changed signal is debounced, so a programmatic SetText (e.g.
	// "Find emails from sender") arrives here after suppressSearch was cleared.
	// Only a genuinely different query exits server-search mode back to local.
	if strings.TrimSpace(w.searchEntry.Text()) != w.serverQuery {
		w.serverSearch = false
	}
	w.refreshList(w.searchEntry.Text())
}

// refreshList populates the thread list from either the current label (blank
// query) or a full-text search (whose message hits are grouped into threads).
// The query runs off the main thread so typing in the search box and the 60s
// background sync never stall the UI.
func (w *window) refreshList(query string) { w.loadThreadsFor(query) }

// refreshListThen repopulates the list and runs done once the new contents are
// actually rendered. The model is repopulated asynchronously (loadThreads runs
// the store query off the main thread), so done must wait for the populate —
// running it right after refreshList returns would act on the stale model (e.g.
// advancing the selection before the archived thread is spliced out).
func (w *window) refreshListThen(query string, done func()) {
	w.afterPopulate = done
	w.refreshList(query)
}

// loadThreadsFor decides what to list — current folder (blank query) or a
// search — and runs it asynchronously.
func (w *window) loadThreadsFor(query string) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		w.serverSearch, w.serverQuery = false, "" // no query → not server-searching
		label, acct := w.current, w.activeID
		w.loadThreads(func(ctx context.Context) ([]model.ThreadSummary, error) {
			if label == allMailID {
				return w.deps.Store.ListAllThreads(ctx, acct, threadListCap, 0)
			}
			return w.deps.Store.ListThreadsByLabel(ctx, acct, label, threadListCap, 0)
		})
		return
	}

	// A server-side search stays a server search across refreshes (e.g. a
	// background sync) instead of reverting to local FTS of the same query.
	if w.serverSearch && w.deps.SearchServer != nil {
		w.runServerSearch(trimmed)
		return
	}

	acct := w.activeID
	w.loadThreads(func(ctx context.Context) ([]model.ThreadSummary, error) {
		return w.searchThreads(ctx, acct, query)
	})
}

// loadThreads runs query off the main thread and renders the result, discarding
// it when a newer refresh has since been issued (last request wins) so a slow
// query can't overwrite fresher results.
func (w *window) loadThreads(query func(context.Context) ([]model.ThreadSummary, error)) {
	w.refreshGen++
	gen := w.refreshGen
	go func() {
		start := time.Now()
		sums, err := query(context.Background())
		slog.Debug("ui: loadThreads", "n", len(sums), "dur", time.Since(start))
		dispatch.Main(func() {
			if gen != w.refreshGen {
				return // superseded by a newer refresh
			}
			if err != nil {
				slog.Error("ui: load threads", "err", err)
				return
			}
			w.showThreads(sums)
		})
	}()
}

// searchThreads runs a local FTS search and groups the hits into thread
// summaries (newest-first, like the folder views), fetching all summaries in one
// batched query rather than one per hit thread.
func (w *window) searchThreads(ctx context.Context, acct int64, query string) ([]model.ThreadSummary, error) {
	msgs, err := w.deps.Store.Search(ctx, acct, query, threadListCap)
	if err != nil {
		return nil, err
	}
	sums, err := w.deps.Store.GetThreadSummaries(ctx, acct, uniqueThreadIDs(msgs))
	if err != nil {
		return nil, err
	}
	sort.SliceStable(sums, func(i, j int) bool {
		return sums[i].Latest.InternalDate.After(sums[j].Latest.InternalDate)
	})
	return sums, nil
}

// uniqueThreadIDs returns the thread ids of msgs, de-duplicated, in first-seen
// order.
func uniqueThreadIDs(msgs []model.Message) []string {
	seen := make(map[string]bool, len(msgs))
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if seen[m.ThreadID] {
			continue
		}
		seen[m.ThreadID] = true
		ids = append(ids, m.ThreadID)
	}
	return ids
}

// liveRefreshList updates the thread list in response to a background change
// (new mail, label edits) while keeping the open conversation selected, so the
// reader is not disturbed.
func (w *window) liveRefreshList() {
	w.loadThreadsFor(w.searchEntry.Text())
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

// showThreads updates the thread list to sums, applying the minimal set of
// changes to the model so an unchanged refresh (the common 60s-sync case) does
// no work at all and an in-place change (mark-read, a new category tag) re-binds
// only the affected rows — preserving scroll position instead of rebuilding the
// whole list on every event.
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

	newByID := make(map[string]model.ThreadSummary, len(sums))
	ids := make([]string, len(sums))
	for i, s := range sums {
		ids[i] = s.ThreadID
		newByID[s.ThreadID] = s
	}
	// Publish the new data before touching the model so any (re)bind reads it.
	oldIDs := w.threadIDs
	w.threadByID = newByID
	w.diffThreadModel(oldIDs, ids)
	w.threadIDs = ids

	if len(sums) == 0 {
		w.emptyPage.SetChild(nil)
		switch {
		case len(w.deps.Accounts) == 0:
			w.emptyPage.SetIconName("mail-send-symbolic")
			w.emptyPage.SetTitle("Welcome to Mailbox")
			w.emptyPage.SetDescription("Connect an account to get started.")
			if w.deps.AddIMAPAccount != nil {
				btn := gtk.NewButtonWithLabel("Add account…")
				btn.AddCSSClass("pill")
				btn.AddCSSClass("suggested-action")
				btn.SetHAlign(gtk.AlignCenter)
				btn.ConnectClicked(func() { w.openAddAccount(nil) })
				w.emptyPage.SetChild(btn)
			}
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
			w.emptyPage.SetIconName("face-smile-symbolic")
			w.emptyPage.SetTitle("All caught up")
			w.emptyPage.SetDescription("No unread messages here — nice.")
		case w.current == model.LabelInbox:
			w.emptyPage.SetIconName("palm-tree-symbolic")
			w.emptyPage.SetTitle("All clear")
			w.emptyPage.SetDescription("Your inbox is empty — go enjoy the sunshine.")
		default:
			w.emptyPage.SetIconName("mail-unread-symbolic")
			w.emptyPage.SetTitle("No messages")
			w.emptyPage.SetDescription("This folder has no messages in the local cache.")
		}
		w.threadStack.SetVisibleChildName("empty")
	} else {
		w.threadStack.SetVisibleChildName("list")
	}
	// Restore the open conversation's selection after any in-place splice (no-op
	// when it isn't in the list, e.g. after a label switch). onThreadSelected
	// short-circuits when the id is already open, so this never re-renders.
	if !w.selectMode {
		w.reselectOpenThread()
	}
	if fn := w.afterPopulate; fn != nil {
		w.afterPopulate = nil
		fn()
	}
	w.categorizeInbox()
}

// diffThreadModel mutates the StringList from oldIDs to newIDs with the fewest
// changes: nothing when identical, a 1-for-1 re-splice of only the rows whose
// rendered content changed when the order is unchanged, and a full replace when
// the set/order differs. rowSig caches each row's last rendered signature so an
// in-place content change (read/unread, star, count, category tag, snippet) is
// detected without rebuilding the list.
func (w *window) diffThreadModel(oldIDs, newIDs []string) {
	sameOrder := len(oldIDs) == len(newIDs)
	if sameOrder {
		for i := range newIDs {
			if oldIDs[i] != newIDs[i] {
				sameOrder = false
				break
			}
		}
	}

	if sameOrder {
		for i, id := range newIDs {
			sig := w.renderSig(id)
			if w.rowSig[id] != sig {
				w.rowSig[id] = sig
				w.threadModel.Splice(uint(i), 1, []string{id}) // remove+add same id → re-bind row i
			}
		}
		return
	}

	// Structural change: replace the whole model and rebuild the signature cache.
	w.threadModel.Splice(0, w.threadModel.NItems(), newIDs)
	w.rowSig = make(map[string]string, len(newIDs))
	for _, id := range newIDs {
		w.rowSig[id] = w.renderSig(id)
	}
}

// renderSig captures everything threadRow renders for id (summary fields, AI
// category, and the select-mode checkbox state), so a change in any of them
// triggers a re-bind of just that row and nothing else does.
func (w *window) renderSig(id string) string {
	t := w.threadByID[id]
	m := t.Latest
	who := m.FromName + "\x1f" + m.FromAddr
	if w.current == model.LabelSent || w.current == model.LabelDraft {
		who = "to:" + m.ToAddrs
	}
	sel := "" // not in selection mode
	if w.selectMode {
		if w.selected[id] {
			sel = "S"
		} else {
			sel = "s"
		}
	}
	return fmt.Sprintf("%s\x1f%d\x1f%d\x1f%s\x1f%s\x1f%s\x1f%d\x1f%t\x1f%t\x1f%t\x1f%s",
		sel, t.UnreadCount, t.Count, w.categories[id], who, m.Subject,
		m.InternalDate.Unix(), m.HasAttachments, m.IsStarred, t.RepliedByMe, m.Snippet)
}

// maxCategorize bounds how many of the newest inbox threads are auto-categorized,
// so a huge inbox can't trigger a flood of AI calls.
const maxCategorize = 40

// categoryCand is one thread to (maybe) categorize: its thread id, the gmail id
// of its latest message (what the category is keyed/persisted by), and the
// "From / Subject / Snippet" context fed to the AI.
type categoryCand struct {
	threadID, msgID, ctx string
}

// categorizeInbox shows inbox category tags with minimal AI cost. It first seeds
// from the persisted per-email cache (store.MessageCategories — no AI call), then
// classifies only the still-uncategorized threads with the AI (batched, capped),
// persisting each result so it survives restarts. Gated by the inboxCategories
// preference + an assistant.
func (w *window) categorizeInbox() {
	if !w.inboxCategories || w.deps.Assistant == nil || w.categorizing || w.current != model.LabelInbox {
		return
	}
	// Candidates: inbox threads not yet categorized in memory this session. Built
	// on the main thread (reads threadByID/categories); the rest runs in the
	// background and marshals UI updates through dispatch.
	var cands []categoryCand
	for id, t := range w.threadByID {
		m := t.Latest
		// Skip only when the category was computed for the current latest message;
		// a newer message (a reply, a follow-up) makes the thread a candidate again
		// so its tag reflects the latest content.
		if w.categorizedMsg[id] == m.GmailID {
			continue
		}
		cands = append(cands, categoryCand{
			threadID: id,
			msgID:    m.GmailID,
			ctx:      fmt.Sprintf("From: %s / Subject: %s / %s", displayFrom(m), m.Subject, m.Snippet),
		})
	}
	if len(cands) == 0 {
		return
	}
	w.categorizing = true
	acctID := w.activeID
	go func() {
		ctx := context.Background()

		// 1) Seed from the persisted cache — free, covers everything classified on
		// a prior run.
		msgIDs := make([]string, len(cands))
		for i, c := range cands {
			msgIDs[i] = c.msgID
		}
		cached, err := w.deps.Store.MessageCategories(ctx, acctID, msgIDs)
		if err != nil {
			slog.Warn("ui: load cached categories", "err", err)
			cached = map[string]string{}
		}
		var todo []categoryCand
		for _, c := range cands {
			if _, ok := cached[c.msgID]; !ok {
				todo = append(todo, c)
			}
		}
		dispatch.Main(func() {
			if w.activeID != acctID {
				return // switched accounts; these tags belong to the other account
			}
			for _, c := range cands {
				if cat, ok := cached[c.msgID]; ok {
					w.categories[c.threadID] = cat
					w.categorizedMsg[c.threadID] = c.msgID
				}
			}
			w.refreshList(w.searchEntry.Text()) // show seeded tags immediately
		})

		// 2) Classify the remainder with the AI (capped to bound cost; the rest
		// finishes on subsequent passes), persisting each result per email.
		if len(todo) > maxCategorize {
			todo = todo[:maxCategorize]
		}
		var firstErr error
		if len(todo) > 0 {
			done := w.aiActivity(fmt.Sprintf("Categorizing %d threads", len(todo)))
			for start := 0; start < len(todo); start += 20 {
				end := start + 20
				if end > len(todo) {
					end = len(todo)
				}
				chunk := todo[start:end]
				ctxs := make([]string, len(chunk))
				for i, c := range chunk {
					ctxs[i] = c.ctx
				}
				cats, err := w.deps.Assistant.Categorize(ctx, ctxs)
				if err != nil {
					firstErr = err
					slog.Warn("ui: categorize inbox", "err", err)
					break
				}
				results := make(map[string]string, len(chunk)) // threadID → category
				forMsg := make(map[string]string, len(chunk))  // threadID → categorized msg id
				for i, c := range chunk {
					if i >= len(cats) {
						break
					}
					cat := normalizeCategory(cats[i])
					if err := w.deps.Store.SetMessageCategory(ctx, acctID, c.msgID, cat); err != nil {
						slog.Warn("ui: persist category", "err", err)
					}
					results[c.threadID] = cat
					forMsg[c.threadID] = c.msgID
				}
				dispatch.Main(func() {
					if w.activeID != acctID {
						return // switched accounts; don't write its tags into the other's map
					}
					for id, cat := range results {
						w.categories[id] = cat
						w.categorizedMsg[id] = forMsg[id]
					}
				})
			}
			dispatch.Main(func() { done(doneErr(firstErr)) })
		}
		dispatch.Main(func() {
			w.categorizing = false
			w.refreshList(w.searchEntry.Text()) // re-bind rows to show the tags
		})
	}()
}

// normalizeCategory maps a model's reply to one of the known categories, or ""
// (no tag) for anything that doesn't match — there is no catch-all category.
func normalizeCategory(s string) string {
	s = strings.TrimSpace(s)
	for _, c := range ai.EmailCategories {
		if strings.EqualFold(c, s) {
			return c
		}
	}
	return ""
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
	w.syncSpinner.SetVisible(on) // AdwSpinner animates whenever it is visible
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
	w.registerReaderActions()
	w.webview = webkit.NewWebView()
	w.sectionCache = make(map[string]cachedSection)
	// Paint an opaque white page background (matches email content + the light
	// wrapper). The cover (below) hides the widget-level swap flash.
	white := gdk.NewRGBA(1, 1, 1, 1)
	w.webview.SetBackgroundColor(&white)
	// While a page loads, WebKit's content swap can flash black at the widget
	// level (the page background-color above doesn't cover it). Keep the WebView
	// mapped (so it keeps rendering) and mask the swap with an opaque white cover
	// shown during the load and hidden once it finishes painting.
	w.webview.ConnectLoadChanged(func(e webkit.LoadEvent) {
		if e == webkit.LoadFinished && w.readerCover != nil {
			w.readerCover.SetVisible(false)
		}
	})
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
	// Let the user select & copy the subject and sender address from the header
	// (the message body is in the WebView, which is already selectable).
	w.header.SetSelectable(true)
	setMargins(w.header, 12, 12, 8, 8)

	// A FlowBox wraps chips to additional rows instead of a single horizontal row,
	// whose summed width could otherwise force the reader pane — and the whole
	// window — wider than the screen (long attachment filenames pushed the window
	// controls off-screen). Each chip's label also ellipsizes (see attachmentChip).
	w.attachBox = gtk.NewFlowBox()
	w.attachBox.SetSelectionMode(gtk.SelectionNone)
	w.attachBox.SetColumnSpacing(6)
	w.attachBox.SetRowSpacing(6)
	w.attachBox.SetHomogeneous(false)
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

	// The WebView sits under an opaque cover (shown during loads) so a content
	// swap never flashes black; the WebView stays mapped so it keeps rendering.
	w.readerCover = gtk.NewBox(gtk.OrientationVertical, 0)
	w.readerCover.AddCSSClass("reader-cover")
	w.readerCover.SetHAlign(gtk.AlignFill)
	w.readerCover.SetVAlign(gtk.AlignFill)
	w.readerCover.SetCanTarget(false) // never intercept input
	w.readerCover.SetVisible(false)
	overlay := gtk.NewOverlay()
	overlay.SetVExpand(true)
	overlay.SetHExpand(true)
	overlay.SetChild(w.webview)
	overlay.AddOverlay(w.readerCover)
	box.Append(overlay)

	// The reader's empty state is just a centered, dimmed envelope — no text.
	empty := gtk.NewImageFromIconName("mail-unread-symbolic")
	empty.SetPixelSize(96)
	empty.AddCSSClass("dim-label")
	empty.SetHAlign(gtk.AlignCenter)
	empty.SetVAlign(gtk.AlignCenter)
	empty.SetHExpand(true)
	empty.SetVExpand(true)

	w.readerStack = gtk.NewStack()
	w.readerStack.AddNamed(empty, "empty")
	w.readerStack.AddNamed(box, "message")
	w.readerStack.SetVisibleChildName("empty")

	hb := adw.NewHeaderBar()
	hb.SetShowTitle(false) // "Reader" is redundant — drop it for a cleaner header

	// Reply-all is the primary action; its dropdown offers Reply and Forward as a
	// native menu model (so the items show their accelerators and read normally).
	replyMenu := gio.NewMenu()
	replyMenu.Append("Reply", "win.reader-reply")
	replyMenu.Append("Forward", "win.reader-forward")

	w.replyAllBtn = adw.NewSplitButton()
	w.replyAllBtn.SetIconName("mail-reply-all-symbolic")
	w.replyAllBtn.SetTooltipText("Reply all (dropdown: Reply, Forward)")
	w.replyAllBtn.ConnectClicked(w.onReplyAll)
	w.replyAllBtn.SetMenuModel(replyMenu)

	w.archiveBtn = gtk.NewButtonFromIconName("mail-archive-symbolic")
	w.archiveBtn.SetTooltipText("Archive (a)")
	w.archiveBtn.ConnectClicked(w.onArchive)

	// AI actions (only useful when an assistant is configured).
	w.translateBtn = gtk.NewButtonFromIconName("translate-symbolic")
	w.translateBtn.SetTooltipText("Translate to English (t)")
	w.translateBtn.ConnectClicked(w.onTranslate)

	w.summaryBtn = gtk.NewButtonFromIconName("view-list-bullet-symbolic")
	w.summaryBtn.SetTooltipText("Summarize thread with AI")
	w.summaryBtn.ConnectClicked(w.onSummarize)

	// AI reply: a popover of AI-suggested quick replies plus reply intents. The
	// popover is rebuilt per open (fresh suggestions for the current message).
	w.aiReplyBtn = gtk.NewMenuButton()
	w.aiReplyBtn.SetIconName("sparkle-symbolic")
	w.aiReplyBtn.SetTooltipText("AI reply")
	w.aiReplyBtn.SetCreatePopupFunc(func(btn *gtk.MenuButton) {
		btn.SetPopover(w.buildAIReplyPopover())
	})

	// Secondary actions (phishing analysis, star, mark-unread, trash, images) live
	// in the overflow — analysis is on-demand and rare, so it doesn't earn a slot.
	w.overflowBtn = gtk.NewMenuButton()
	w.overflowBtn.SetIconName("view-more-symbolic")
	w.overflowBtn.SetTooltipText("More actions")
	// A native menu model (standard GTK4): normal-weight rows, native checkmarks
	// for the toggles, automatic separators. Rebuilt on each open so the dynamic
	// items (spam/not-spam, delete-forever, find-from-sender) match the context,
	// with the toggle states synced first.
	w.overflowBtn.SetCreatePopupFunc(func(btn *gtk.MenuButton) {
		w.starAction.SetState(glib.NewVariantBoolean(w.openMsg.IsStarred))
		w.imagesAction.SetState(glib.NewVariantBoolean(w.imagesEnabled))
		btn.SetPopover(gtk.NewPopoverMenuFromModel(w.buildReaderMenuModel()))
	})

	hb.PackStart(w.replyAllBtn)
	if w.deps.Assistant != nil {
		hb.PackStart(w.aiReplyBtn)
	}
	hb.PackStart(w.archiveBtn)
	hb.PackEnd(w.overflowBtn)
	if w.deps.Assistant != nil {
		hb.PackEnd(w.translateBtn)
		hb.PackEnd(w.summaryBtn)
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
	w.replyAllBtn.SetSensitive(on && w.deps.Send != nil)
	canAI := on && w.deps.Assistant != nil
	w.translateBtn.SetSensitive(canAI)
	if w.summaryBtn != nil {
		w.summaryBtn.SetSensitive(canAI)
	}
	if w.aiReplyBtn != nil {
		w.aiReplyBtn.SetSensitive(canAI && w.deps.Send != nil)
	}
	// The overflow menu builds its own items conditionally; enable it whenever a
	// message is open.
	w.overflowBtn.SetSensitive(on)
}

// replyTarget is the address(es) a reply should go to: the Reply-To header when
// the sender set one (some senders route replies elsewhere — lists, no-reply
// aliases), otherwise the From address.
func replyTarget(m model.Message) string {
	if rt := strings.TrimSpace(m.ReplyTo); rt != "" {
		return rt
	}
	return m.FromAddr
}

// replyInit builds the prefilled compose for a reply to m (To, Re: subject,
// quoted body, threading headers).
func (w *window) replyInit(m model.Message) model.OutgoingMessage {
	return model.OutgoingMessage{
		To:         replyTarget(m),
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
	w.openCompose(w.replyInit(m), w.threadContextFor(m), "Reply")
}

func (w *window) onReplyAll() {
	init, aiContext, ok := w.replyAllInit()
	if !ok {
		return
	}
	w.openCompose(init, aiContext, "Reply all")
}

// replyAllInit builds the reply-all prefill (recipients, subject, quoted body,
// threading headers) and the AI thread context for the open message. ok is false
// when no message is open. Shared by onReplyAll and the AI-reply popover.
func (w *window) replyAllInit() (init model.OutgoingMessage, aiContext string, ok bool) {
	m := w.openMsg
	if m.GmailID == "" {
		return model.OutgoingMessage{}, "", false
	}
	to, cc := replyAllRecipients(m, w.activeEmail)
	return model.OutgoingMessage{
		To:         to,
		Cc:         cc,
		Subject:    ensureRePrefix(m.Subject),
		Body:       quoteOriginal(m, w.bodyTextFor(m)),
		InReplyTo:  m.RFC822MsgID,
		References: strings.TrimSpace(m.References + " " + m.RFC822MsgID),
		ThreadID:   m.ThreadID,
	}, w.threadContextFor(m), true
}

// aiReply opens a reply compose for the open message with an AI action applied
// (a chosen quick reply, an intent to auto-draft, or the AI-draft dialog).
func (w *window) aiReply(auto composeAutoAI) {
	init, aiContext, ok := w.replyAllInit()
	if !ok {
		return
	}
	w.openCompose(init, aiContext, "Reply", auto)
}

// buildAIReplyPopover builds the reader's AI-reply popover: AI-suggested quick
// replies at the top (fetched async; tap → compose prefilled with that reply) and
// reply intents below (tap → AI drafts a full reply in that direction). Rebuilt
// on each open so suggestions match the current message.
func (w *window) buildAIReplyPopover() *gtk.Popover {
	pop := gtk.NewPopover()
	box := gtk.NewBox(gtk.OrientationVertical, 4)
	box.SetSizeRequest(300, -1)
	setMargins(box, 8, 8, 8, 8)

	_, threadContext, ok := w.replyAllInit()
	if !ok || w.deps.Assistant == nil {
		box.Append(aiPopLabel("Open a message to reply."))
		pop.SetChild(box)
		return pop
	}

	// AI-suggested quick replies (one call per open; results stream in).
	box.Append(aiPopLabel("Suggested replies"))
	sug := gtk.NewBox(gtk.OrientationVertical, 4)
	box.Append(sug)
	spinner := adw.NewSpinner()
	spinner.SetHAlign(gtk.AlignStart)
	spinner.SetSizeRequest(20, 20)
	sug.Append(spinner)
	done := w.aiActivity("Suggesting replies")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		replies, err := w.deps.Assistant.SmartReplies(ctx, threadContext)
		dispatch.Main(func() {
			done(doneErr(err))
			for c := sug.FirstChild(); c != nil; c = sug.FirstChild() {
				sug.Remove(c)
			}
			if err != nil {
				slog.Warn("ui: ai-reply suggestions", "err", err)
			}
			if err != nil || len(replies) == 0 {
				sug.Append(aiPopLabel("No suggestions"))
				return
			}
			for _, r := range replies {
				text := strings.TrimSpace(r)
				if text == "" {
					continue
				}
				row := aiPopRow(text, true)
				row.ConnectClicked(func() {
					pop.Popdown()
					w.aiReply(composeAutoAI{quickReply: text})
				})
				sug.Append(row)
			}
		})
	}()

	box.Append(gtk.NewSeparator(gtk.OrientationHorizontal))
	box.Append(aiPopLabel("Write a reply that…"))
	for _, p := range replyPresets() {
		instr := p.instruction
		row := aiPopRow("↳ "+p.label, false)
		row.ConnectClicked(func() {
			pop.Popdown()
			w.aiReply(composeAutoAI{instruction: instr})
		})
		box.Append(row)
	}
	custom := aiPopRow("✎ Custom instruction…", false)
	custom.ConnectClicked(func() {
		pop.Popdown()
		w.aiReply(composeAutoAI{openDialog: true})
	})
	box.Append(custom)

	pop.SetChild(box)
	return pop
}

// aiPopLabel is a dim caption used as a section heading in the AI-reply popover.
func aiPopLabel(text string) *gtk.Label {
	l := gtk.NewLabel(text)
	l.SetXAlign(0)
	l.AddCSSClass("dim-label")
	l.AddCSSClass("caption")
	return l
}

// aiPopRow is a flat, left-aligned popover row button; wrap shows long text over
// multiple lines (suggestions), else it ellipsizes (intents).
func aiPopRow(text string, wrap bool) *gtk.Button {
	l := gtk.NewLabel(text)
	l.SetXAlign(0)
	l.SetHExpand(true)
	if wrap {
		l.SetWrap(true)
		l.SetWrapMode(pango.WrapWordChar)
	} else {
		l.SetEllipsize(pango.EllipsizeEnd)
	}
	b := gtk.NewButton()
	b.SetChild(l)
	b.AddCSSClass("flat")
	return b
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
	toList := append(collect(replyTarget(m)), collect(m.ToAddrs)...)
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
	// A forward carries the original's attachments. Gather them off the main thread
	// (a download may be needed), then open the compose; forwardAttachments returns
	// nil fast when there are none, so an attachment-less forward still opens
	// promptly. (We consult the attachments table directly — the has_attachments
	// metadata flag isn't reliably set.)
	if w.deps.OpenAttach == nil {
		w.openCompose(init, "", "Forward")
		return
	}
	go func() {
		atts := w.forwardAttachments(context.Background(), m)
		dispatch.Main(func() {
			init.Attachments = atts
			w.openCompose(init, "", "Forward")
		})
	}()
}

// forwardAttachments downloads (caching) the original message's attachments and
// returns them as outgoing parts, de-duplicated — the same file is often carried
// by several messages in a chain, and a single message can list a part twice;
// matching on content hash (else name+size) attaches each only once.
func (w *window) forwardAttachments(ctx context.Context, m model.Message) []model.OutgoingAttachment {
	atts, err := w.deps.Store.ListAttachments(ctx, m.RowID)
	if err != nil {
		slog.Warn("ui: forward list attachments", "id", m.GmailID, "err", err)
		return nil
	}
	var out []model.OutgoingAttachment
	seen := make(map[string]bool)
	for _, a := range atts {
		key := a.SHA256
		if key == "" {
			key = fmt.Sprintf("%s\x00%d", a.Filename, a.SizeBytes)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		path, err := w.deps.OpenAttach(ctx, m.AccountID, m.GmailID, a.ID)
		if err != nil {
			slog.Warn("ui: forward fetch attachment", "att", a.Filename, "err", err)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("ui: forward read attachment", "path", path, "err", err)
			continue
		}
		out = append(out, model.OutgoingAttachment{Filename: a.Filename, MimeType: a.MimeType, Data: data})
	}
	return out
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

	// Per-account unread-inbox counts in one query (feeds the inbox badge, the
	// account pills, and the title).
	counts := w.accountUnreadInbox(ctx)
	inboxCount := counts[w.activeID]

	// Rebuild the sidebar widgets only when its structure or the inbox badge
	// actually changed — an idle 60s sync (no new mail) leaves it untouched,
	// avoiding widget churn and a selection flicker every cycle.
	sig := w.sidebarSignature(labels, have, inboxCount)
	if sig != w.sidebarSig {
		w.sidebarSig = sig
		w.labelBox.RemoveAll()
		w.sidebar = w.sidebar[:0]

		// Only the Inbox carries an unread-count badge — that's where new mail
		// matters; badges on every folder/label read as noise.
		for _, f := range systemFolders {
			if f.id == allMailID {
				w.appendFolder(f.id, f.icon, f.name, 0)
				continue
			}
			if !have[f.id] {
				continue
			}
			count := 0
			if f.id == model.LabelInbox {
				count = inboxCount
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
	}

	w.applyAccountUnread(counts) // pills + title from the same counts
}

// sidebarSignature captures everything the label sidebar renders — the active
// account, the visible folders/labels, and the inbox badge count — so loadLabels
// can skip the widget rebuild when none of it changed.
func (w *window) sidebarSignature(labels []model.Label, have map[string]bool, inboxUnread int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "a=%d;inbox=%d;", w.activeID, inboxUnread)
	for _, f := range systemFolders {
		if f.id == allMailID || have[f.id] {
			b.WriteString("f:" + f.id + ";")
		}
	}
	for _, l := range labels {
		if l.Type == model.LabelUser {
			b.WriteString("u:" + l.GmailID + "=" + l.Name + ";")
		}
	}
	return b.String()
}

// accountUnreadInbox returns each account's unread-inbox count (one query).
func (w *window) accountUnreadInbox(ctx context.Context) map[int64]int {
	ids := make([]int64, 0, len(w.deps.Accounts))
	for _, a := range w.deps.Accounts {
		ids = append(ids, a.ID)
	}
	counts, err := w.deps.Store.UnreadCountByLabelForAccounts(ctx, ids, model.LabelInbox)
	if err != nil {
		slog.Warn("ui: account unread counts", "err", err)
		return map[int64]int{}
	}
	return counts
}

// applyAccountUnread updates the per-account pills and the window title from a
// precomputed per-account unread-inbox map (no queries).
func (w *window) applyAccountUnread(counts map[int64]int) {
	total := 0
	for _, a := range w.deps.Accounts {
		n := counts[a.ID]
		total += n
		if badge := w.accountBadges[a.ID]; badge != nil {
			if n > 0 {
				badge.SetText(fmt.Sprintf("%d", n))
				badge.SetVisible(true)
			} else {
				badge.SetVisible(false)
			}
		}
	}
	if total > 0 {
		w.win.SetTitle(fmt.Sprintf("Mailbox — %d unread", total))
	} else {
		w.win.SetTitle("Mailbox")
	}
}

// refreshAccountUnread fetches the per-account unread-inbox counts and applies
// them to the pills and title. Used when only sibling-account counts changed
// (so the active account's sidebar needn't reload).
func (w *window) refreshAccountUnread() {
	w.applyAccountUnread(w.accountUnreadInbox(context.Background()))
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

// signatureForActive returns the signature composes should append for the active
// account: the global default when only one account is connected (per-account
// overrides only matter with several), otherwise the active account's signature
// (its own override, or the global default as fallback).
func (w *window) signatureForActive() string {
	if len(w.deps.Accounts) <= 1 {
		sig, _ := config.LoadSignature()
		return sig
	}
	sig, _ := config.SignatureFor(w.activeEmail)
	return sig
}

// setActiveAccount switches the displayed account, reloading its labels and inbox.
func (w *window) setActiveAccount(a AccountInfo) {
	if a.ID == w.activeID {
		return
	}
	w.activeID = a.ID
	w.activeEmail = a.Email
	w.signature = w.signatureForActive() // signature the next compose appends
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
			}, "", "Edit draft")
		})
	}()
}

// renderConversation fetches each message's body (lazily) and renders the whole
// thread as stacked sections in the reader.
func (w *window) renderConversation(msgs []model.Message) {
	latest := msgs[len(msgs)-1]
	// The pinned header is the thread title: subject plus a message count for a
	// real conversation. Each message's sender/date/recipients live in its own
	// section below (conversationSection), so the header no longer repeats the
	// latest message's sender+date — that duplicated the newest section header.
	subject := strings.TrimSpace(latest.Subject)
	if subject == "" {
		subject = "(no subject)"
	}
	title := "<b>" + html.EscapeString(subject) + "</b>"
	if len(msgs) > 1 {
		title += fmt.Sprintf("\n<span size=\"small\">%d messages</span>", len(msgs))
	}
	w.header.SetMarkup(title)
	// No "Loading…" placeholder: the previous message stays put while bodies are
	// fetched (the status bar reports progress), then loadReaderHTML swaps to the
	// rendered thread behind the cover — so there's no blank/black flash.

	threadID := w.openThreadID // guard against a newer thread being opened mid-render
	// Snapshot already-rendered sections on the main thread; the goroutine reuses
	// these and only sanitizes the misses, so re-opening a thread is near-instant.
	cached := w.cachedSectionsFor(msgs)
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
		fresh := map[string]cachedSection{} // newly-rendered sections to cache
		// Newest message first (msgs is oldest-first from the store).
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			// The latest message always needs its body read for the auth/phishing
			// signals, even when its section is cached.
			if m.RowID == latest.RowID {
				body, _ := w.deps.Store.GetBody(ctx, m.RowID)
				latestAuth = body.RawHeaders
				latestHTML = body.HTML
				if cs, ok := cached[m.GmailID]; ok {
					b.WriteString(cs.html)
					blocked += cs.trackers
					continue
				}
				sec, n := conversationSection(m, body, w.cleanHTML)
				fresh[m.GmailID] = cachedSection{html: sec, trackers: n}
				b.WriteString(sec)
				blocked += n
				continue
			}
			if cs, ok := cached[m.GmailID]; ok {
				b.WriteString(cs.html)
				blocked += cs.trackers
				continue
			}
			body, _ := w.deps.Store.GetBody(ctx, m.RowID)
			sec, n := conversationSection(m, body, w.cleanHTML)
			fresh[m.GmailID] = cachedSection{html: sec, trackers: n}
			b.WriteString(sec)
			blocked += n
		}
		out := b.String()
		verdict := parseAuthResults(latestAuth)
		warnings := phishingWarnings(latest, latestHTML)
		// Gather attachment rows here (off the main thread); the main thread only
		// builds the chip widgets.
		atts := w.threadAttachments(ctx, msgs)
		slog.Debug("ui: renderConversation", "msgs", len(msgs), "fetched", fetched,
			"trackers", blocked, "auth", verdict.level, "fetch", fetchDur, "sanitize", time.Since(sanitizeStart))
		dispatch.Main(func() {
			w.mergeSectionCache(fresh) // cache newly-rendered sections (main thread)
			if w.openThreadID != threadID {
				return // user switched to another conversation while this rendered
			}
			w.setTrackerCount(blocked)
			w.setAuthBadge(verdict)
			w.setCaution(warnings)
			w.loadReaderHTML(wrapHTML(out))
			w.showThreadAttachments(atts)
		})
	}()
}

// loadReaderHTML loads fully-wrapped HTML into the reader, raising the opaque
// cover first so WebKit's content swap doesn't flash black; the cover is dropped
// when the load finishes (see the load-changed handler in buildReader).
func (w *window) loadReaderHTML(full string) {
	if w.readerCover != nil {
		w.readerCover.SetVisible(true)
	}
	w.webview.LoadHtml(full, "about:blank")
}

// cachedSection is a message's rendered (sanitized, de-tracked, quote-collapsed)
// section HTML plus its blocked-tracker count. Sections are immutable once a
// message's body is fetched, so they can be reused across thread re-opens.
type cachedSection struct {
	html     string
	trackers int
}

// sectionCacheCap bounds how many rendered sections are kept in memory.
const sectionCacheCap = 400

// cachedSectionsFor returns the cached sections for the given messages (main
// thread); the result is handed to the render goroutine, which reuses hits and
// sanitizes only the misses.
func (w *window) cachedSectionsFor(msgs []model.Message) map[string]cachedSection {
	out := make(map[string]cachedSection, len(msgs))
	for _, m := range msgs {
		if cs, ok := w.sectionCache[m.GmailID]; ok {
			out[m.GmailID] = cs
		}
	}
	return out
}

// mergeSectionCache stores newly-rendered sections, evicting arbitrary entries
// when over the cap (sections are immutable, so an eviction is just a future
// cache miss). Main-thread only.
func (w *window) mergeSectionCache(fresh map[string]cachedSection) {
	for k, v := range fresh {
		w.sectionCache[k] = v
	}
	for len(w.sectionCache) > sectionCacheCap {
		for k := range w.sectionCache {
			delete(w.sectionCache, k)
			break
		}
	}
}

// invalidateSection drops a message's cached section, so a re-synced message
// (changed metadata/body) re-renders. Main-thread only.
func (w *window) invalidateSection(gmailID string) {
	if gmailID != "" {
		delete(w.sectionCache, gmailID)
	}
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
		return header + "<pre style=\"white-space:pre-wrap\">" + linkifyText(body.Text) + "</pre>", 0
	default:
		return header + "<p>" + linkifyText(m.Snippet) + "</p>", 0
	}
}

// urlPattern matches an explicit http/https URL. Deliberately narrow (a scheme is
// required, and the URL stops at whitespace or a char that would break out of an
// attribute) so linkifyText never fabricates a non-http link or turns an ordinary
// word into one — fewer false positives than a www./bare-domain matcher.
var urlPattern = regexp.MustCompile(`https?://[^\s<>"]+`)

// linkifyText renders a plain-text email body (or snippet) as safe HTML: every
// segment is HTML-escaped, and bare http(s) URLs become <a> links. The reader's
// navigation policy opens those externally (xdg-open), so no extra plumbing is
// needed. Escaping both the href and the link text — and matching a scheme that
// cannot contain a quote — means email text can't inject markup or a bad scheme.
func linkifyText(text string) string {
	var b strings.Builder
	last := 0
	for _, loc := range urlPattern.FindAllStringIndex(text, -1) {
		b.WriteString(html.EscapeString(text[last:loc[0]]))
		raw := trimURLTrailing(text[loc[0]:loc[1]])
		esc := html.EscapeString(raw)
		fmt.Fprintf(&b, `<a href="%s">%s</a>`, esc, esc)
		last = loc[0] + len(raw) // any trimmed tail falls back into plain text
	}
	b.WriteString(html.EscapeString(text[last:]))
	return b.String()
}

// trimURLTrailing strips punctuation that commonly abuts a URL in prose but isn't
// part of it — a sentence's ".,;:!?'", and a closing ) or ] only when it isn't
// balanced inside the URL (so "(see https://x/a)" drops the ")" while
// ".../Foo_(bar)" keeps it).
func trimURLTrailing(u string) string {
	for {
		t := strings.TrimRight(u, ".,;:!?'")
		if strings.HasSuffix(t, ")") && strings.Count(t, "(") < strings.Count(t, ")") {
			t = t[:len(t)-1]
		} else if strings.HasSuffix(t, "]") && strings.Count(t, "[") < strings.Count(t, "]") {
			t = t[:len(t)-1]
		}
		if t == u {
			return t
		}
		u = t
	}
}

// populateThreadAttachments shows chips for all attachments across the thread,
// each opening via its own message.
// threadAttachment is one attachment plus the message it belongs to, gathered
// off the main thread so widget construction is the only main-thread work.
type threadAttachment struct {
	att       model.Attachment
	accountID int64
	gmailID   string
}

// threadAttachments collects every attachment across the thread's messages. It
// runs off the main thread (one DB query per message) and returns nil when
// attachments can't be opened.
func (w *window) threadAttachments(ctx context.Context, msgs []model.Message) []threadAttachment {
	if w.deps.OpenAttach == nil {
		return nil
	}
	var out []threadAttachment
	seen := make(map[string]bool)
	for _, m := range msgs {
		atts, err := w.deps.Store.ListAttachments(ctx, m.RowID)
		if err != nil {
			slog.Warn("ui: list attachments", "id", m.GmailID, "err", err)
			continue
		}
		for _, a := range atts {
			// The same file is usually carried by every message in a reply chain;
			// show it once. Key on content hash when known, else name+size.
			key := a.SHA256
			if key == "" {
				key = fmt.Sprintf("%s\x00%d", a.Filename, a.SizeBytes)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, threadAttachment{att: a, accountID: m.AccountID, gmailID: m.GmailID})
		}
	}
	return out
}

// showThreadAttachments rebuilds the attachment chip row from pre-gathered data.
// Main-thread only (it touches widgets); it does no I/O.
func (w *window) showThreadAttachments(atts []threadAttachment) {
	for child := w.attachBox.FirstChild(); child != nil; child = w.attachBox.FirstChild() {
		w.attachBox.Remove(child)
	}
	for _, ta := range atts {
		ta := ta
		btn := gtk.NewButton()
		btn.SetChild(attachmentChip(ta.att))
		btn.SetTooltipText(ta.att.MimeType)
		btn.ConnectClicked(func() { w.openAttachment(ta.accountID, ta.gmailID, ta.att.ID) })
		w.attachBox.Append(btn)
	}
	w.attachBox.SetVisible(len(atts) > 0)
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
	name := gtk.NewLabel(a.Filename)
	// Ellipsize in the middle so the extension stays visible, and bound the width
	// so one long filename can't blow out the chip (and the reader pane).
	name.SetEllipsize(pango.EllipsizeMiddle)
	name.SetMaxWidthChars(28)
	box.Append(name)
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

// vacuumAfterEmpty is the message count above which emptying a folder triggers a
// background VACUUM — small empties aren't worth a full database rebuild.
const vacuumAfterEmpty = 50

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
			// A big empty frees a lot of pages WAL would otherwise keep; reclaim
			// them in the background (after the UI feedback above), but only when
			// it's worth the full-rebuild cost.
			if err == nil && n >= vacuumAfterEmpty {
				if verr := w.deps.Store.Vacuum(context.Background()); verr != nil {
					slog.Warn("ui: vacuum after empty", "err", verr)
				}
			}
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
				w.refreshListThen(w.searchEntry.Text(), func() { w.advanceSelection(pos) })
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

// showLabelsDialog opens the label chooser (buildLabelsMenu) as a dialog. Labels
// moved out of the reader header into the overflow menu (it's used rarely).
func (w *window) showLabelsDialog() {
	scroller := gtk.NewScrolledWindow()
	scroller.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroller.SetChild(w.buildLabelsMenu())
	scroller.SetVExpand(true)

	tv := adw.NewToolbarView()
	tv.AddTopBar(adw.NewHeaderBar())
	tv.SetContent(scroller)

	dialog := adw.NewDialog()
	dialog.SetTitle("Labels")
	dialog.SetContentWidth(320)
	dialog.SetContentHeight(400)
	dialog.SetChild(tv)
	dialog.Present(w.win)
}

// registerReaderActions registers the win.* actions backing the overflow menu,
// so the menu can be a native GMenu model (standard GTK4 rendering) rather than
// hand-built buttons. The non-toggle actions just call the existing handlers;
// the two toggles are stateful booleans so the menu shows native checkmarks.
func (w *window) registerReaderActions() {
	add := func(name string, fn func()) {
		act := gio.NewSimpleAction(name, nil)
		act.ConnectActivate(func(*glib.Variant) { fn() })
		w.win.AddAction(act)
	}
	add("reader-reply", w.onReply)
	add("reader-forward", w.onForward)
	add("reader-unread", w.onMarkUnread)
	add("reader-move-inbox", w.onMoveToInbox)
	add("reader-report-spam", w.onReportSpam)
	add("reader-not-spam", w.onNotSpam)
	add("reader-trash", w.onTrash)
	add("reader-delete-forever", w.onDeleteForever)
	add("reader-labels", w.showLabelsDialog)
	add("reader-analyze", w.onAnalyze)
	add("reader-find-from", func() { w.searchFrom(w.openMsg.FromAddr) })

	w.starAction = gio.NewSimpleActionStateful("reader-star", nil, glib.NewVariantBoolean(false))
	w.starAction.ConnectChangeState(func(v *glib.Variant) {
		w.starAction.SetState(v)
		w.setStarred(v.Boolean())
	})
	w.win.AddAction(w.starAction)

	w.imagesAction = gio.NewSimpleActionStateful("reader-images", nil, glib.NewVariantBoolean(true))
	w.imagesAction.ConnectChangeState(func(v *glib.Variant) {
		w.imagesAction.SetState(v)
		w.setImagesEnabled(v.Boolean())
	})
	w.win.AddAction(w.imagesAction)
}

// buildReaderMenuModel builds the overflow menu for the current context: star,
// mark-unread, move/spam/trash, labels, optionally find-from-sender, and the
// remote-images toggle. (Reply all, Reply, Forward, Archive, Translate and
// Draft reply are dedicated header controls.) Unlabeled sections render as
// native separators.
func (w *window) buildReaderMenuModel() *gio.Menu {
	menu := gio.NewMenu()
	if w.deps.ModifyLabels != nil {
		sec := gio.NewMenu()
		sec.Append("Starred", "win.reader-star")
		sec.Append("Mark as unread", "win.reader-unread")
		sec.Append("Move to Inbox", "win.reader-move-inbox")
		if w.current == model.LabelSpam {
			sec.Append("Not spam", "win.reader-not-spam")
		} else {
			sec.Append("Report spam", "win.reader-report-spam")
		}
		sec.Append("Move to Trash", "win.reader-trash")
		if w.deps.DeleteForever != nil && (w.current == model.LabelTrash || w.current == model.LabelSpam) {
			sec.Append("Delete forever", "win.reader-delete-forever")
		}
		menu.AppendSection("", sec)

		lbl := gio.NewMenu()
		lbl.Append("Labels…", "win.reader-labels")
		menu.AppendSection("", lbl)
	}
	// On-demand AI phishing/scam analysis (rare, so it lives here rather than the
	// header). Streams its verdict into the shared AI card.
	if w.deps.Assistant != nil {
		sec := gio.NewMenu()
		sec.Append("Check for phishing", "win.reader-analyze")
		menu.AppendSection("", sec)
	}
	// Find all mail from this sender (Gmail server-side search understands from:).
	if w.deps.SearchServer != nil && strings.TrimSpace(w.openMsg.FromAddr) != "" {
		sec := gio.NewMenu()
		sec.Append("Find emails from sender", "win.reader-find-from")
		menu.AppendSection("", sec)
	}
	img := gio.NewMenu()
	img.Append("Show remote images", "win.reader-images")
	menu.AppendSection("", img)
	return menu
}

// searchFrom shows all mail from an address using a Gmail server-side search
// ("from:addr"), so it finds messages beyond the local cache too.
func (w *window) searchFrom(addr string) {
	q := "from:" + strings.TrimSpace(addr)
	w.suppressSearch = true
	w.searchEntry.SetText(q)
	w.suppressSearch = false
	if w.deps.SearchServer != nil {
		w.onSearchAllMail()
	} else {
		w.refreshList(q)
	}
}

// cleanHTML sanitizes email body HTML then strips tracking pixels and collapses
// quoted history in one pass, returning the cleaned HTML and how many trackers
// were removed.
func (w *window) cleanHTML(h string) (string, int) {
	clean, n := cleanEmailHTML(w.sanitizer.Sanitize(h))
	// The sanitizer strips <style>; re-add it scoped to a unique wrapper so an
	// email's class-based layout renders (with its own cascade intact) without
	// bleeding onto other messages in the thread or the reader chrome.
	css := extractStyleCSS(h)
	if strings.TrimSpace(css) == "" {
		return clean, n
	}
	scope := "mbx-" + randNonce()[:12]
	scoped := scopeCSS(css, "."+scope)
	if scoped == "" {
		return clean, n
	}
	return `<div class="` + scope + `"><style>` + scoped + `</style>` + clean + `</div>`, n
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

// onTranslate shows an English translation of the whole open conversation in
// place, preserving each message's markup. Every message is translated and
// cached per message id, so re-opening, reverting, or re-translating reuses the
// cached result (and an already-translated message in the thread isn't redone).
func (w *window) onTranslate() {
	if w.deps.Assistant == nil || len(w.openThreadMsgs) == 0 {
		return
	}
	if w.translateCancel != nil {
		w.translateCancel()
		w.translateCancel = nil
	}
	msgs := append([]model.Message(nil), w.openThreadMsgs...) // snapshot (oldest first)
	threadID := w.openThreadID
	acctID := w.activeID

	// Which messages still need translating? (in-memory cache read on the main
	// thread; the persisted cache is consulted in the goroutine before any AI).
	var todo []model.Message
	for _, m := range msgs {
		if _, ok := w.translationCache[m.GmailID]; !ok {
			todo = append(todo, m)
		}
	}
	if len(todo) == 0 { // whole thread already translated → show instantly
		w.showTranslatedConversation(msgs)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.translateCancel = cancel
	w.translationBanner.SetTitle("Translating…")
	w.translationBanner.SetRevealed(true)
	// Keep the original showing while translating (the banner says "Translating…");
	// loadReaderHTML swaps to the translation behind the cover when it's ready.
	done := w.aiActivity("Translating")

	go func() {
		// 1) Seed from the persisted per-message cache (no AI cost). A message body
		// is immutable, so a stored English translation is always valid.
		ids := make([]string, len(todo))
		for i, m := range todo {
			ids[i] = m.GmailID
		}
		seeded, err := w.deps.Store.Translations(ctx, acctID, ids, translateLang)
		if err != nil {
			slog.Warn("ui: load cached translations", "err", err)
			seeded = map[string]string{}
		}
		var remaining []model.Message
		for _, m := range todo {
			if _, ok := seeded[m.GmailID]; !ok {
				remaining = append(remaining, m)
			}
		}

		// 2) Translate the remainder concurrently (bounded), writing each result
		// through to the store. Sources are read + sanitized here (off the main
		// thread); bluemonday + the store are safe for concurrent use.
		results := make(map[string]string, len(remaining))
		var mu sync.Mutex
		var firstErr error
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		for _, m := range remaining {
			wg.Add(1)
			sem <- struct{}{}
			go func(m model.Message) {
				defer wg.Done()
				defer func() { <-sem }()
				out, err := translateHTMLText(w.bodyHTMLFor(m), func(segs []string) ([]string, error) {
					return w.deps.Assistant.TranslateSegments(ctx, segs, translateLang)
				})
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				text := stripCodeFence(out)
				if serr := w.deps.Store.SetTranslation(ctx, acctID, m.GmailID, translateLang, text); serr != nil {
					slog.Warn("ui: persist translation", "err", serr)
				}
				mu.Lock()
				results[m.GmailID] = text
				mu.Unlock()
			}(m)
		}
		wg.Wait()

		dispatch.Main(func() {
			done(doneErr(firstErr))
			if w.openThreadID != threadID || ctx.Err() != nil {
				return // user switched conversations or reverted
			}
			if firstErr != nil {
				w.loadReaderHTML(wrapHTML("<p>Translation failed: " + html.EscapeString(firstErr.Error()) + "</p>"))
				return
			}
			for id, out := range seeded {
				w.translationCache[id] = out
			}
			for id, out := range results {
				w.translationCache[id] = out
			}
			w.showTranslatedConversation(msgs)
		})
	}()
}

// translateLang is the single target language the Translate action uses; also
// the key under which translations are cached/persisted.
const translateLang = "English"

// showTranslatedConversation renders the thread (newest first) from each
// message's cached translation, like renderConversation but with translated
// bodies. Main thread only.
func (w *window) showTranslatedConversation(msgs []model.Message) {
	w.translationBanner.SetTitle("Showing translation")
	w.translationBanner.SetRevealed(true)
	var b strings.Builder
	blocked := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		body := model.MessageBody{HTML: w.translationCache[m.GmailID]}
		sec, n := conversationSection(m, body, w.cleanHTML)
		b.WriteString(sec)
		blocked += n
	}
	w.setTrackerCount(blocked)
	w.loadReaderHTML(wrapHTML(b.String()))
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
	// Persisted summary for this exact message set (no AI cost). The stored
	// fingerprint is the same key, so a thread that gained a reply misses and is
	// re-summarized. A single indexed lookup, fine on the main thread.
	if fp, sum, ok, err := w.deps.Store.ThreadSummary(context.Background(), w.activeID, w.openThreadID); err == nil && ok && fp == key {
		w.summaryCache[key] = sum
		w.summaryLabel.SetText(sum)
		return
	}

	w.summaryLabel.SetText("Summarizing…")
	ctx, cancel := context.WithCancel(context.Background())
	w.summaryCancel = cancel
	threadID := w.openThreadID
	acctID := w.activeID
	contextText := w.threadContextAll()
	done := w.aiActivity("Summarizing thread")

	go func() {
		ch, err := w.deps.Assistant.SummarizeThread(ctx, contextText)
		if err != nil {
			msg := err.Error()
			dispatch.Main(func() {
				done(doneErr(err))
				if w.openThreadID == threadID && ctx.Err() == nil {
					w.summaryLabel.SetText("Summary failed: " + msg)
				}
			})
			return
		}
		text, serr := streamCoalesced(ch, func(text string) {
			if w.openThreadID != threadID || ctx.Err() != nil {
				return
			}
			w.summaryLabel.SetText(bulletize(text))
		})
		// Finalize + persist off the main thread, so an unchanged thread's summary
		// survives restarts.
		final := ""
		if serr == nil {
			final = bulletize(strings.TrimSpace(text))
			if final != "" {
				if perr := w.deps.Store.SetThreadSummary(context.Background(), acctID, threadID, key, final); perr != nil {
					slog.Warn("ui: persist summary", "err", perr)
				}
			}
		}
		dispatch.Main(func() {
			done(doneErr(serr))
			if w.openThreadID != threadID || ctx.Err() != nil {
				return
			}
			if serr != nil {
				w.summaryLabel.SetText("Summary failed: " + serr.Error())
				return
			}
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
	// Persisted analysis for this message (no AI cost). The message + its signals
	// are immutable, so a stored analysis is always valid. A single indexed
	// lookup, fine on the main thread.
	if a, ok, err := w.deps.Store.Analysis(context.Background(), w.activeID, m.GmailID); err == nil && ok {
		w.summaryCache[key] = a
		w.summaryLabel.SetText(a)
		return
	}

	w.summaryLabel.SetText("Analyzing…")
	ctx, cancel := context.WithCancel(context.Background())
	w.summaryCancel = cancel
	threadID := w.openThreadID
	acctID := w.activeID
	gmailID := m.GmailID
	emailCtx := w.analysisContextFor(m)
	done := w.aiActivity("Analyzing email")

	go func() {
		ch, err := w.deps.Assistant.AnalyzeEmail(ctx, emailCtx)
		if err != nil {
			msg := err.Error()
			dispatch.Main(func() {
				done(doneErr(err))
				if w.openThreadID == threadID && ctx.Err() == nil {
					w.summaryLabel.SetText("Analysis failed: " + msg)
				}
			})
			return
		}
		text, serr := streamCoalesced(ch, func(text string) {
			if w.openThreadID != threadID || ctx.Err() != nil {
				return
			}
			w.summaryLabel.SetText(bulletize(text))
		})
		// Finalize + persist off the main thread, so re-opening the message reuses
		// the analysis instead of re-running the AI.
		final := ""
		if serr == nil {
			final = bulletize(strings.TrimSpace(text))
			if final != "" {
				if perr := w.deps.Store.SetAnalysis(context.Background(), acctID, gmailID, final); perr != nil {
					slog.Warn("ui: persist analysis", "err", perr)
				}
			}
		}
		dispatch.Main(func() {
			done(doneErr(serr))
			if w.openThreadID != threadID || ctx.Err() != nil {
				return
			}
			if serr != nil {
				w.summaryLabel.SetText("Analysis failed: " + serr.Error())
				return
			}
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
			if after != nil {
				// after (e.g. advanceSelection) must run once the list has been
				// respliced, not before — see refreshListThen.
				w.refreshListThen(w.searchEntry.Text(), after)
			} else {
				w.refreshList(w.searchEntry.Text())
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
		w.openComposeOpts(msg, "", "Message", false)
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
				return
			}
			w.toast("Message sent")
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
		w.invalidateSection(c.GmailID) // a re-synced message must re-render
		if c.AccountID == w.activeID {
			// A change to the open conversation (a reply you sent, or a synced
			// message) re-renders it so the new message shows without re-opening.
			if c.ThreadID != "" && c.ThreadID == w.openThreadID {
				w.refreshThreadPending = true
			}
			w.scheduleRefresh(true) // loadLabels (inside) refreshes pills + title
		} else {
			w.refreshAccountUnread() // a sibling account's unread count changed
		}
	case syncer.LabelsSynced:
		if c.AccountID == w.activeID {
			w.scheduleRefresh(false)
		} else {
			w.refreshAccountUnread()
		}
	case syncer.SendStateChanged:
		if c.AccountID == w.activeID {
			w.refreshOutbox()
		}
	case syncer.AuthExpired:
		// The account's sign-in expired/was revoked; surface it (it won't recover
		// without re-login) and name the account so multi-account users know which.
		email := ""
		for _, a := range w.deps.Accounts {
			if a.ID == c.AccountID {
				email = a.Email
				break
			}
		}
		w.authExpiredID = c.AccountID // the Reconnect button re-authenticates this one
		if email != "" {
			w.authBanner.SetTitle("Sign-in expired for " + email + " — reconnect to keep syncing")
		} else {
			w.authBanner.SetTitle("An account's sign-in expired — reconnect to keep syncing")
		}
		w.authBanner.SetRevealed(true)
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
		thread := w.refreshThreadPending
		w.refreshListPending = false
		w.refreshThreadPending = false
		w.loadLabels()
		if list {
			w.liveRefreshList()
		}
		if thread {
			w.refreshOpenThread()
		}
	})
}

// refreshOpenThread re-queries the open conversation and re-renders it, so a
// newly stored message — a reply you just sent, or one pulled in by sync —
// appears without re-opening the thread. A no-op when nothing is open. Unlike
// showThread it doesn't mark-read, reset translation, or change navigation.
func (w *window) refreshOpenThread() {
	if w.openThreadID == "" {
		return
	}
	msgs, err := w.deps.Store.ListThreadMessages(context.Background(), w.activeID, w.openThreadID)
	if err != nil || len(msgs) == 0 {
		return
	}
	w.openThreadMsgs = msgs
	w.openMsg = msgs[len(msgs)-1] // newest, for reply/forward/star/unread
	w.renderConversation(msgs)
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

func threadRow(t model.ThreadSummary, outgoing bool, category string) *gtk.Box {
	m := t.Latest
	unread := t.UnreadCount > 0
	// "Needs reply" means it needs YOUR reply; once you've had the last word the
	// reply marker says it's handled, so drop the now-stale tag. Other tags
	// (Discount, Receipt, …) are informational and stay.
	if category == "Needs reply" && t.RepliedByMe && !outgoing {
		category = ""
	}

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
	// You had the last word — a reply marker signals the conversation is handled
	// and needs no further reply. Redundant in Sent/Drafts, where it's always you.
	if t.RepliedByMe && !outgoing {
		rep := gtk.NewImageFromIconName("mail-replied-symbolic")
		rep.AddCSSClass("dim-label")
		rep.SetTooltipText("You replied")
		top.Append(rep)
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
	subj.SetHExpand(true)
	subj.SetEllipsize(pango.EllipsizeEnd)
	if !unread {
		subj.AddCSSClass("dim-label")
	}
	// An AI category tag (e.g. "Needs reply") sits before the subject;
	// uncategorized mail shows nothing.
	if category != "" {
		tag := gtk.NewLabel(category)
		tag.AddCSSClass("cat-tag")
		if category == "Needs reply" {
			tag.AddCSSClass("cat-needsreply")
		}
		tag.SetVAlign(gtk.AlignCenter)
		subjRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
		subjRow.Append(tag)
		subjRow.Append(subj)
		box.Append(subjRow)
	} else {
		box.Append(subj)
	}

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
