package ui

import (
	"context"
	"fmt"
	"html"
	"io"

	"log/slog"
	"net/mail"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	xhtml "golang.org/x/net/html"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4-webkitgtk/pkg/javascriptcore/v6"
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
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/remotecache"
	"github.com/jsnjack/mailbox/internal/store"
	"github.com/jsnjack/mailbox/internal/syncer"
	"github.com/microcosm-cc/bluemonday"
)

// threadCategory is everything the UI knows about one conversation's AI tag:
// the tag, the message it was computed for (so a new reply re-categorizes the
// thread — a "Needs reply" thread that gets a discount reply becomes
// "Discount"), whether the user picked it by hand (a manual pick outranks the
// automatic "Replied" tag), and whether the last attempt errored. A failure is
// distinct from a settled "no category": it stays an AI retry candidate and
// renders a subtle "failed" tag instead of silently looking uncategorized.
type threadCategory struct {
	tag            string
	categorizedMsg string
	manual         bool
	failed         bool
}

// uiCacheKey keeps provider-scoped identifiers isolated between accounts.
// Gmail and IMAP ids are unique only within their owning account.
type uiCacheKey struct {
	accountID int64
	id        string
}

func cacheKey(accountID int64, id string) uiCacheKey {
	return uiCacheKey{accountID: accountID, id: id}
}

func (w *window) activeCacheKey(id string) uiCacheKey {
	return cacheKey(w.activeID, id)
}

// window owns the widget tree and the currently displayed selection.
type window struct {
	app  *adw.Application
	deps Deps

	win          *adw.ApplicationWindow
	toastOverlay *adw.ToastOverlay
	outerSplit   *adw.NavigationSplitView
	innerSplit   *adw.NavigationSplitView

	// The activity row at the bottom of the sidebar, and the log panel it opens
	// (the app's one status surface — see activityRow).
	activity        *activityRow
	statusLogBox    *gtk.Box             // log lines (newest first) inside the panel
	statusLogEmpty  *gtk.Label           // "No activity yet", hidden once a row exists
	statusLogRows   map[string][]*logRow // op+label → in-flight log rows (FIFO), finished in place
	statusQuietRows map[string]*gtk.Box  // op+account+label → last quiet mail-check row (deduped)
	statusLogLines  int                  // current number of log rows (capped)
	// The panel's session grid: cumulative counters, filled on open.
	statGrid        *gtk.Grid
	statNet         *statCell
	statAPI         *statCell
	statAI          *statCell
	statCache       *statCell
	statModelBox    *gtk.Box
	statModel       *gtk.Label
	statModelChip   *gtk.Label
	markReadTimer   glib.SourceHandle // pending "mark thread read" (cancelled if the user navigates away first)
	pendingMarkRead func()            // the deferred mark-read body, so an explicit action can flush it early
	lastSyncAt      time.Time         // when the last sync finished (resting status text)
	accountBox      *gtk.ListBox
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
	sidebar      []sidebarItem                // one entry per row in labelBox (incl. headings)
	sidebarSig   string                       // signature of the rendered sidebar, to skip no-op rebuilds
	sectionCache map[uiCacheKey]cachedSection // rendered message sections, reused across thread re-opens
	remoteImages *remotecache.Cache           // allowed external images, persisted for offline rendering
	current      string
	activeID     int64 // the account currently shown
	activeEmail  string
	// suppressLabelSelect guards the row-selected handler while loadLabels
	// restores the visual highlight, so a background refresh doesn't reset the
	// list or clear an active search.
	suppressLabelSelect bool
	// suppressAccountSelect guards the account switcher's row-selected handler
	// during programmatic selection (rebuilds, removals), so restoring the
	// highlight never re-routes the UI to whatever account occupies that index.
	suppressAccountSelect bool
	startTime             time.Time // only mail arriving after this triggers notifications

	// virtualized list grouped by conversation: a StringList of thread ids drives
	// a ListView; the factory builds visible rows from threadByID.
	threadModel  *gtk.StringList
	threadSel    *gtk.SingleSelection
	threadView   *gtk.ListView
	threadScroll *gtk.ScrolledWindow
	threadStack  *gtk.Stack      // "list" vs "empty" placeholder
	emptyPage    *adw.StatusPage // the "empty" placeholder (text set per context)
	pageStatus   *gtk.Box
	pageSpinner  *adw.Spinner
	pageLabel    *gtk.Label
	pageRetry    *gtk.Button
	threadPage   threadPageState
	threadLoad   threadLoad
	threadFailed *threadLoadFailure
	// loadGen stamps each list query so a slow result cannot overwrite a newer
	// account/folder/query (last request wins).
	loadGen     uint64
	readerStack *gtk.Stack // "message" vs "empty" placeholder
	// readerReady flips when the shell page reports __mbSet is installed;
	// content set before that is parked in pendingReaderHTML (latest wins) and
	// flushed by the shell-ready handler in buildReader.
	readerReady       bool
	pendingReaderHTML *string
	listMenuBtn       *gtk.MenuButton // thread-list overflow (unread-only filter + mark-all-read)
	unreadOnly        bool
	// multi-select triage: a selection mode with per-row checkboxes and a bulk
	// action bar.
	selectBtn         *gtk.ToggleButton
	selectMode        bool
	selected          map[string]bool // selected thread ids
	selectionBar      *gtk.Box
	selectionLabel    *gtk.Label
	readOnlyBanner    *adw.Banner    // revealed when no provider backend (live features off)
	outboxBanner      *adw.Banner    // revealed when sends are queued/failed
	emptyFolderBanner *adw.Banner    // revealed in Trash/Spam to empty them permanently
	authBanner        *adw.Banner    // revealed when an account's sign-in expired/was revoked
	authExpiredID     int64          // the account the auth banner's Reconnect targets (0 = none/unknown)
	authReported      map[int64]bool // accounts whose expiry already got an activity-log row (AuthExpired repeats every failed sync pass)
	searchEntry       *gtk.SearchEntry
	searchSort        *gtk.DropDown // Relevant (provider/FTS rank) or Newest
	searchAllBtn      *gtk.Button   // explicit provider search, available even when local FTS has hits
	suppressSearch    bool          // guards SetText from firing a search during label switch
	serverSearch      bool          // current search is a provider-side search, not local FTS
	serverQuery       string        // the active server-search query (guards the debounced change signal)
	threadByID        map[string]model.ThreadSummary
	threadIDs         []string          // displayed thread ids, in order (for incremental diffing)
	rowSig            map[string]string // last-rendered signature per row, to detect in-place changes

	// coalesce refreshes triggered by bursts of sync change events.
	refreshPending       bool
	refreshListPending   bool
	refreshThreadPending bool // re-render the open conversation on the next refresh
	// labelsGen is the same last-request-wins guard for loadLabels, whose store
	// queries run off the main thread.
	labelsGen uint64
	// notifyQueue coalesces new-mail notification checks from a burst of
	// MessageUpserted events: ids collect here (main thread) and are looked up in
	// one background pass, instead of one main-thread GetMessage per event.
	notifyQueue     []notifyCandidate
	notifyScheduled bool
	// notified remembers desktop notifications already delivered this session.
	// Metadata refreshes and repeated provider events must not pop the same mail
	// again; the account-qualified key also keeps equal provider ids isolated.
	notified map[uiCacheKey]bool
	// userUnread records messages the user explicitly marked unread (reader
	// toggle, row menu, or undoing a mark-read), keyed account/gmailID. The
	// self-sent auto-clear in checkNewMail must never fight an explicit mark
	// when the change echoes back from the provider. Main thread only.
	userUnread map[string]bool
	// unreadRefreshPending coalesces per-account unread-pill refreshes triggered
	// by bursts of sibling-account sync events (the query runs off-thread).
	unreadRefreshPending bool
	// afterPopulate runs once after the next list populate, then clears. Used by
	// launch hooks that must act on the loaded list (now that loads are async).
	afterPopulate func()

	header       *gtk.Label
	attachBox    *gtk.FlowBox // chips for the open message's attachments (wraps, never forces width)
	inviteCard   *gtk.Box     // meeting-invite card (Accept/Maybe/Decline) above the conversation
	trackerLabel *gtk.Label   // "N trackers blocked" indicator
	authIcon     *gtk.Image   // compact sender-auth (SPF/DKIM/DMARC) status; details on hover
	cautionLabel *gtk.Label   // anti-phishing heuristic warnings
	webview      *webkit.WebView
	readerZoom   float64 // reader message zoom (Ctrl +/-/0), persisted
	sanitizer    *bluemonday.Policy
	// Last pointer position over the WebView (widget coords), tracked by a
	// motion controller: an in-page click reaches Go as a navigation with no
	// coordinates, and the per-message ⋯ menu anchors its popover here.
	readerPtrX, readerPtrY float64

	// reader: the open conversation. openMsg is its newest message (used for
	// reply/forward/star/unread); openThreadMsgs is all of them (oldest first).
	openThreadID   string
	openThreadMsgs []model.Message
	openMsg        model.Message
	// openGen guards showThread's off-thread read: each open bumps it, and a
	// completed read only applies if its generation is still current (last
	// click wins). Main-thread only.
	openGen uint64
	// Pending undo toast and the burst of same-kind label changes it reverses
	// (rapid triage coalesces into one toast — see showUndoToast). Main-thread
	// only.
	undoToast  *adw.Toast
	undoVerb   string
	undoMsgs   []model.Message
	undoAdd    []string
	undoRemove []string
	// switchStart stamps an account switch so the first thread-list render for
	// the new account logs the end-to-end click→content latency (diagnosing
	// "switching feels slow" from a trace). Zero when no switch is pending.
	switchStart time.Time
	// renderCancel cancels an in-flight render goroutine when the user opens
	// another thread or backs out — so a hung body fetch can't pin a stale
	// goroutine for the full fetch timeout. Main-thread only (set/cancelled
	// in renderConversation and clearReader, which both run on the main thread).
	renderCancel context.CancelFunc
	// renderFetching holds the message ids the in-flight render owns the body
	// fetches for, and renderGen stamps that render so a finished-but-superseded
	// one can't clear a newer render's set. Every body fetch published as
	// MessageBodyFetched comes from a render (the engine's HTML backfill stores
	// quietly), so an event for one of these ids is this render's own echo — the
	// render reads the body itself and must not be restarted by it. Main-thread
	// only.
	renderFetching  map[uiCacheKey]bool
	renderGen       uint64
	lastFetchFailed bool             // true if the last render had fetch failures (for retry menu item)
	replyBtn        *adw.SplitButton // primary action (Reply all); dropdown has Reply/Forward
	aiReplyBtn      *gtk.MenuButton  // AI reply: popover of suggestions + intents
	archiveBtn      *gtk.Button
	translateBtn    *gtk.Button
	overflowBtn     *gtk.MenuButton   // secondary reader actions (native menu model)
	starAction      *gio.SimpleAction // stateful: the open message's Starred toggle
	unreadAction    *gio.SimpleAction // stateful: the thread-list "show unread only" filter
	imagesEnabled   bool              // whether remote images are loaded in the reader
	blockImages     bool              // global remote-image opt-out (Preferences)

	// AI inbox categorization, keyed by thread. The four facts about a thread's
	// tag are always read and written together, so they live in one value: a
	// half-updated tag (a manual pick without its pin, a retry that keeps its
	// old failure) is the bug this shape prevents. inboxCategories gates it.
	threadCats map[uiCacheKey]threadCategory
	// inlineRefetched guards the one-time re-fetch of a message whose body
	// references inline (cid:) images that older extraction didn't capture.
	inlineRefetched map[uiCacheKey]bool
	// gistRequested guards per-message gist generation (the one-line AI summary
	// card): a message is scheduled at most once per session — a failure clears
	// its mark so a later open retries, a success is persisted and never re-runs.
	// The reader marks it before starting so overlapping renders never generate
	// the same message twice concurrently.
	gistRequested map[uiCacheKey]bool
	// appliedGists holds gists already revealed in the live reader, re-asserted
	// after every conversation swap: a re-render in flight when a gist persists
	// (the mark-read refresh 1.5s after opening an unread thread) queried the
	// store and snapshotted the section cache before the persist, so its swap
	// would otherwise replace the revealed card with the hidden placeholder —
	// the card would blink. Entries drop once a render's store query has them.
	appliedGists map[uiCacheKey]string
	// inlineByCID maps the open thread's inline-image Content-IDs to the
	// attachment behind each one, served (and downloaded on first request) by the
	// cid: URI-scheme handler — so a big inline image loads as a streamed
	// resource, not a multi-MB base64 blob inflating the HTML.
	inlineByCID map[string]inlineImage
	// remoteImageURLs maps the open conversation's mbcache: keys back to the URL
	// serveRemoteImage downloads on a cache miss. Main thread only.
	remoteImageURLs map[string]string
	// AI health: aiFailedAt is when the last AI request failed; aiFailing drives
	// the status-bar warning. Used to back off auto-categorization when the LLM is
	// unreachable so it doesn't retry on every inbox refresh.
	aiFailing       bool
	aiFailedAt      time.Time
	inboxCategories bool
	// Per-feature AI toggles (Preferences → AI Features), each mirroring a
	// config.Prefs.DisableXxx field (inverted: true = feature on). Gate both
	// whether a feature's UI is shown and whether it runs.
	aiGist              bool
	aiDraft             bool
	aiSmartReplies      bool
	aiProofread         bool
	aiRefine            bool
	aiGenerateSubject   bool
	aiSummarize         bool
	aiTranslate         bool
	aiPhishing          bool
	aiSnoozeSuggestions bool
	sendUndoSecs        int             // undo-send window in seconds (0 = default 5)
	keymap              map[uint]func() // single-key shortcuts (configurable; see shortcuts.go)
	readerCatTag        *gtk.Label      // thread category pill in the reader header

	// AI thread summary: a button reveals a card that streams a summary in.
	// summaryCache memoizes by the thread's message fingerprint, so reopening is
	// instant and a new reply (different fingerprint) re-generates automatically.
	summaryBtn      *gtk.Button
	summaryRevealer *gtk.Revealer
	summaryLabel    *gtk.Label
	cardIcon        *gtk.Image // card icon (set per action: summary vs analysis)
	cardTitle       *gtk.Label // card title (set per action)
	summaryCancel   context.CancelFunc
	summaryCache    map[uiCacheKey]string

	// in-place translation: a banner offers reverting to the original; the cancel
	// func aborts an in-flight translation when the user reverts or switches mail;
	// translationCache memoizes results per message id so re-showing is instant.
	// translationShown records that the reader currently displays translated
	// bodies, so a background re-render (a synced reply, the images toggle)
	// re-applies the translation instead of silently reverting to the original
	// under a still-revealed "Showing translation" banner.
	translationBanner *adw.Banner
	remoteImageBanner *adw.Banner // explains blocked/expired images instead of showing unexplained broken glyphs
	translateCancel   context.CancelFunc
	translationCache  map[uiCacheKey]string
	translationShown  bool
}

func newWindow(app *adw.Application, deps Deps) *window {
	w := &window{
		app:              app,
		deps:             deps,
		current:          model.LabelInbox,
		startTime:        time.Now(),
		sanitizer:        emailPolicy(),
		translationCache: map[uiCacheKey]string{},
		summaryCache:     map[uiCacheKey]string{},
		accountBadges:    map[int64]*gtk.Label{},
		readerZoom:       1.0,
		selected:         map[string]bool{},
		authReported:     map[int64]bool{},
		threadCats:       map[uiCacheKey]threadCategory{},
		inlineRefetched:  map[uiCacheKey]bool{},
		gistRequested:    map[uiCacheKey]bool{},
		appliedGists:     map[uiCacheKey]string{},
		notified:         map[uiCacheKey]bool{},
		userUnread:       map[string]bool{},
	}
	w.accountNames, _ = config.LoadAccountNames()
	if dir, err := config.RemoteImagesDir(); err == nil {
		w.remoteImages = remotecache.New(dir)
	}
	w.rebuildKeymap()
	if p, err := config.LoadPrefs(); err == nil {
		w.blockImages = p.BlockRemoteImages
		w.inboxCategories = !p.DisableInboxCategories
		w.aiGist = !p.DisableGist
		w.aiDraft = !p.DisableAIDraft
		w.aiSmartReplies = !p.DisableSmartReplies
		w.aiProofread = !p.DisableProofread
		w.aiRefine = !p.DisableRefine
		w.aiGenerateSubject = !p.DisableGenerateSubject
		w.aiSummarize = !p.DisableSummarize
		w.aiTranslate = !p.DisableTranslate
		w.aiPhishing = !p.DisablePhishingAnalysis
		w.aiSnoozeSuggestions = !p.DisableSnoozeSuggestions
		w.sendUndoSecs = p.SendUndoSeconds
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
	// All three carry "<accountID>|<gmailID>" so a notification (which may target a
	// non-active account) can act on the right message.
	act := gio.NewSimpleAction("open-message", glib.NewVariantType("s"))
	act.ConnectActivate(func(p *glib.Variant) {
		if p != nil {
			w.openFromNotification(p.String())
		}
	})
	w.app.AddAction(act)

	arch := gio.NewSimpleAction("notify-archive", glib.NewVariantType("s"))
	arch.ConnectActivate(func(p *glib.Variant) {
		if p != nil {
			w.archiveFromNotification(p.String())
		}
	})
	w.app.AddAction(arch)

	rep := gio.NewSimpleAction("notify-reply", glib.NewVariantType("s"))
	rep.ConnectActivate(func(p *glib.Variant) {
		if p != nil {
			w.replyFromNotification(p.String())
		}
	})
	w.app.AddAction(rep)
}

// parseNotifyTarget splits a notification action target "<accountID>|<gmailID>".
func parseNotifyTarget(target string) (accountID int64, gmailID string, ok bool) {
	parts := strings.SplitN(target, "|", 2)
	if len(parts) != 2 {
		return 0, "", false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", false
	}
	return id, parts[1], true
}

// archiveFromNotification archives a message straight from its new-mail
// notification (no window focus needed), then dismisses the notification.
func (w *window) archiveFromNotification(target string) {
	acctID, gmailID, ok := parseNotifyTarget(target)
	logging.Trace("ui: notification archive", "target", target, "account", acctID, "id", gmailID, "ok", ok)
	if !ok || w.deps.ModifyLabels == nil {
		return
	}
	w.app.WithdrawNotification(fmt.Sprintf("mailbox-mail-%d-%s", acctID, gmailID))
	go func() {
		if err := w.deps.ModifyLabels(context.Background(), acctID, []string{gmailID}, nil, []string{model.LabelInbox}); err != nil {
			slog.Warn("ui: notification archive", "id", gmailID, "err", err)
		}
	}()
}

// replyFromNotification opens a reply to a message from its notification,
// focusing the window and switching to the message's account first.
func (w *window) replyFromNotification(target string) {
	acctID, gmailID, ok := parseNotifyTarget(target)
	logging.Trace("ui: notification reply", "target", target, "account", acctID, "id", gmailID, "ok", ok)
	if !ok || w.deps.Send == nil {
		return
	}
	w.win.Present()
	if acctID != w.activeID {
		for _, a := range w.deps.Accounts {
			if a.ID == acctID {
				w.setActiveAccount(a)
				break
			}
		}
	}
	m, err := w.deps.Store.GetMessage(context.Background(), acctID, gmailID)
	if err != nil {
		slog.Warn("ui: notification reply", "id", gmailID, "err", err)
		return
	}
	w.openCompose(w.replyInit(m), w.threadContextFor(m), "Reply")
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
	about.SetComments("A native, fast email client for Linux/GNOME.")
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
	logging.Trace("ui: open from notification", "target", target, "account", acctID, "id", gmailID)
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
	// Open the conversation where it actually lives: archived (or moved) between
	// the notification and the click, it is no longer in the Inbox list — the
	// reader would show a conversation the list doesn't contain, which reads as
	// detached. All Mail holds everything except Trash/Spam, which are followed
	// to their own folder.
	label := model.LabelInbox
	switch {
	case hasLabel(m, model.LabelInbox):
	case hasLabel(m, model.LabelTrash):
		label = model.LabelTrash
	case hasLabel(m, model.LabelSpam):
		label = model.LabelSpam
	default:
		label = allMailID
	}
	if label != model.LabelInbox {
		logging.Trace("ui: notification message left the inbox; following", "id", gmailID, "label", label)
	}
	w.selectLabel(label)
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

	// No full-width strip under the panes: the activity row is mounted inside
	// the sidebar (buildSidebar), the way Nautilus places its operations row.
	w.win.SetContent(w.toastOverlay)
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

	// Standard GNOME menu accelerators. The single-key scheme below stays intact;
	// these add the modifier chords a keyboard-first user expects.
	newMsg := gio.NewSimpleAction("new-message", nil)
	newMsg.ConnectActivate(func(*glib.Variant) {
		// Guarded exactly like the New button — no compose without a place to send.
		if w.newBtn == nil || !w.newBtn.Sensitive() {
			logging.Trace("ui: new message accel ignored (no account)")
			return
		}
		logging.Trace("ui: new message (accel)", "account", w.activeID)
		w.openCompose(model.OutgoingMessage{}, "", "New message")
	})
	w.win.AddAction(newMsg)
	w.app.SetAccelsForAction("win.new-message", []string{"<Control>n"})

	focusSearch := gio.NewSimpleAction("focus-search", nil)
	focusSearch.ConnectActivate(func(*glib.Variant) {
		logging.Trace("ui: focus search (accel)")
		w.searchEntry.GrabFocus()
	})
	w.win.AddAction(focusSearch)
	w.app.SetAccelsForAction("win.focus-search", []string{"<Control>f"})

	closeWin := gio.NewSimpleAction("close", nil)
	closeWin.ConnectActivate(func(*glib.Variant) {
		logging.Trace("ui: close window (accel)")
		w.win.Close()
	})
	w.win.AddAction(closeWin)
	w.app.SetAccelsForAction("win.close", []string{"<Control>w"})

	// Preferences action is registered in registerAppMenuActions; bind its chord.
	w.app.SetAccelsForAction("win.preferences", []string{"<Control>comma"})

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
		// The customizable single-key actions (Preferences → Keyboard).
		if fn, ok := w.keymap[gdk.KeyvalToLower(keyval)]; ok {
			fn()
			return true
		}
		switch keyval {
		case gdk.KEY_Delete:
			w.onTrash()
		case gdk.KEY_Escape:
			// Esc unwinds one layer at a time: selection mode first, then the
			// collapsed-pane back navigation.
			if w.selectMode && w.selectBtn != nil {
				w.selectBtn.SetActive(false)
			} else {
				w.goBack()
			}
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
	step := 1
	if delta < 0 {
		step = -1
	}
	if cur := w.threadSel.Selected(); cur != invalidPos {
		next = int(cur) + delta
	}
	if next < 0 {
		next, step = 0, 1 // clamped at the top: only forward makes sense
	}
	if next >= n {
		next, step = n-1, -1
	}
	// Date group headers aren't conversations; keep moving past them.
	for next >= 0 && next < n && isDateHeader(w.threadModel.String(uint(next))) {
		next += step
	}
	if next < 0 || next >= n {
		return // nothing but headers in that direction
	}
	w.threadSel.SetSelected(uint(next))
}

// anyStarred reports whether any message of the conversation is starred — the
// predicate the Starred folder uses to list a thread, and what unstar undoes.
func anyStarred(msgs []model.Message) bool {
	for _, m := range msgs {
		if m.IsStarred {
			return true
		}
	}
	return false
}

// threadStarred reports the open conversation's star state: starred as soon as
// any of its messages is (an older starred reply keeps the thread in the
// Starred folder even when the newest message isn't starred).
func (w *window) threadStarred() bool {
	return anyStarred(w.openThreadMsgs) || w.openMsg.IsStarred
}

// toggleStar flips the star on the open conversation. No-op when nothing is open.
func (w *window) toggleStar() {
	if w.openMsg.GmailID == "" {
		return
	}
	w.setStarred(!w.threadStarred())
}

// setStarred adds or removes the star across the whole open conversation
// (optimistic), keeping the cached message flags in sync so the overflow
// checkbox and the 's' shortcut agree. It stars the entire thread, not just
// the newest message, so unstarring actually removes the conversation from the
// Starred folder (which lists any thread with any starred message) rather than
// leaving older replies starred.
func (w *window) setStarred(star bool) {
	if w.openMsg.GmailID == "" {
		return
	}
	logging.Trace("ui: set starred", "thread", w.openThreadID, "id", w.openMsg.GmailID, "star", star, "account", w.activeID)
	w.openMsg.IsStarred = star
	for i := range w.openThreadMsgs {
		w.openThreadMsgs[i].IsStarred = star
	}
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

// onReconnect re-authenticates the account whose sign-in expired by reopening the
// add-account dialog prefilled for it (same email → cache preserved). When the
// expired account can't be identified it falls back to the plain Add account
// dialog, or to read-only guidance if account management isn't available.
func (w *window) onReconnect() {
	logging.Trace("ui: reconnect", "account", w.authExpiredID)
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

// addBreakpoints collapses the panes as the window narrows, before a pane's
// minimum width would be clipped. The thresholds track the split views' actual
// minimums: the thread-list + reader pair needs ~709px (280 sidebar + ~429
// reader header, whose min is dominated by the action buttons + window
// controls), so the thread list collapses below 720sp; adding the accounts
// sidebar (200) needs ~909px, so it collapses below 960sp. Collapsing any later
// leaves a band where the panes are shown side-by-side but overflow the window
// (GtkBox "exceeds AdwApplicationWindow width" warnings + clipped content).
func (w *window) addBreakpoints() {
	medium := adw.NewBreakpoint(adw.NewBreakpointConditionLength(
		adw.BreakpointConditionMaxWidth, 960, adw.LengthUnitSp))
	medium.AddSetter(w.outerSplit, "collapsed", coreglib.NewValue(true))
	w.win.AddBreakpoint(medium)

	narrow := adw.NewBreakpoint(adw.NewBreakpointConditionLength(
		adw.BreakpointConditionMaxWidth, 720, adw.LengthUnitSp))
	narrow.AddSetter(w.outerSplit, "collapsed", coreglib.NewValue(true))
	narrow.AddSetter(w.innerSplit, "collapsed", coreglib.NewValue(true))
	w.win.AddBreakpoint(narrow)
}

func (w *window) present() {
	w.win.SetVisible(true)
	w.loadLabels()
	w.subscribe()
	// Reopen where the user left off (folder + unread filter + open thread).
	vs, vsErr := config.LoadViewState()
	if vsErr == nil {
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
	if vsErr == nil && vs.OpenThread != "" {
		// Re-open the conversation that was open at last save, once the list has
		// populated (loads are async). Selecting the row opens it, so keyboard
		// navigation continues from there. A vanished thread is just skipped.
		tid := vs.OpenThread
		w.afterPopulate = func() {
			for i := uint(0); i < uint(w.threadModel.NItems()); i++ {
				if w.threadModel.String(i) == tid {
					logging.Trace("ui: restore open thread", "thread", tid, "row", i)
					w.threadSel.SetSelected(i)
					return
				}
			}
			logging.Trace("ui: restore open thread not in list", "thread", tid)
		}
	}

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
			for i := uint(0); i < w.threadModel.NItems(); i++ {
				if !isDateHeader(w.threadModel.String(i)) {
					w.threadSel.SetSelected(i)
					return
				}
			}
		}
	}
	if os.Getenv("MAILBOX_OPEN_PREFS") == "1" {
		w.openSettings() // sandbox verification of the Preferences dialog
	}
}

// allMailID is the sentinel "folder" id for the All Mail view, which lists every
// cached thread regardless of label (it is not a real Gmail label).
const allMailID = "__all_mail__"

// snoozedID is the sentinel "folder" id for the Snoozed view: conversations
// hidden from the inbox until their wake time. The local snoozes table drives
// this view; the provider "Snoozed" labels are its cross-client mirror.
const snoozedID = "__snoozed__"

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
	{snoozedID, "Snoozed", "alarm-symbolic"},
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
		if row == nil || w.suppressAccountSelect {
			return
		}
		if i := row.Index(); i >= 0 && i < len(w.deps.Accounts) {
			w.setActiveAccount(w.deps.Accounts[i])
		}
	})
	w.selectAccountRow(w.activeID)
	w.accountHeader = gtk.NewBox(gtk.OrientationVertical, 0)
	w.accountHeader.Append(w.accountBox)
	w.accountHeader.Append(gtk.NewSeparator(gtk.OrientationHorizontal))
	w.accountHeader.SetVisible(len(w.deps.Accounts) >= 1)
	box.Append(w.accountHeader)
	box.Append(scroller)

	hb := adw.NewHeaderBar()
	w.newBtn = gtk.NewButtonFromIconName("mail-message-new-symbolic")
	w.newBtn.SetTooltipText("New message")
	a11yLabel(w.newBtn, "New message")
	w.newBtn.SetSensitive(w.deps.Send != nil && len(w.deps.Accounts) > 0)
	w.newBtn.ConnectClicked(func() {
		logging.Trace("ui: new message", "account", w.activeID)
		w.openCompose(model.OutgoingMessage{}, "", "New message")
	})
	hb.PackStart(w.newBtn)

	w.refreshBtn = gtk.NewButtonFromIconName("view-refresh-symbolic")
	w.refreshBtn.SetTooltipText("Sync now")
	a11yLabel(w.refreshBtn, "Sync now")
	w.refreshBtn.SetSensitive(w.deps.Sync != nil && len(w.deps.Accounts) > 0)
	w.refreshBtn.ConnectClicked(w.onRefresh)

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
	a11yLabel(primaryBtn, "Main menu")
	primaryBtn.SetMenuModel(menu)
	// PackEnd is right-to-left: the primary menu sits at the trailing edge (GNOME
	// convention), with refresh to its left.
	hb.PackEnd(primaryBtn)
	hb.PackEnd(w.refreshBtn)

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(box)
	// The permanent activity row sits under the folder list, in the pane rather
	// than across the window. Collapsed layouts show the sidebar as its own
	// page, where the row simply comes with it.
	tv.AddBottomBar(w.buildActivityRow().bar)
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
// rename, add, or removal), restoring the selection to the active account by
// its id — never by row index, which shifts when the list changes. The rebuild
// and re-selection are programmatic, so the row-selected handler is suppressed
// (it would otherwise route the UI to whatever account landed on that index).
func (w *window) rebuildAccountSwitcher() {
	if w.accountBox == nil {
		return
	}
	w.suppressAccountSelect = true
	w.accountBox.RemoveAll()
	w.accountBadges = map[int64]*gtk.Label{}
	for _, a := range w.deps.Accounts {
		w.accountBox.Append(w.accountSwitcherRow(a))
	}
	w.suppressAccountSelect = false
	w.selectAccountRow(w.activeID)
	if w.accountHeader != nil {
		w.accountHeader.SetVisible(len(w.deps.Accounts) >= 1)
	}
	w.refreshAccountUnread()
}

// selectAccountRow highlights the switcher row for the given account id without
// firing the row-selected handler (programmatic selection must not re-route the
// UI). A no-op when the id isn't in the switcher.
func (w *window) selectAccountRow(id int64) {
	if w.accountBox == nil {
		return
	}
	for i, a := range w.deps.Accounts {
		if a.ID != id {
			continue
		}
		if r := w.accountBox.RowAtIndex(i); r != nil {
			w.suppressAccountSelect = true
			w.accountBox.SelectRow(r)
			w.suppressAccountSelect = false
		}
		return
	}
	logging.Trace("ui: select account row not found", "account", id)
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
	delete(w.authReported, a.ID) // a future expiry earns a fresh activity row
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
		w.selectAccountRow(a.ID)
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
	w.rebuildAccountSwitcher() // re-renders rows; restores the highlight by account id
	if id == w.authExpiredID {
		w.authExpiredID = 0
		w.authBanner.SetRevealed(false)
	}
	delete(w.authReported, id)
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
		w.selectAccountRow(w.deps.Accounts[0].ID)
	}
}

// showThreads updates the thread list to sums, applying the minimal set of
// changes to the model so an unchanged refresh (the common 60s-sync case) does
// no work at all and an in-place change (mark-read, a new category tag) re-binds
// only the affected rows — preserving scroll position instead of rebuilding the
// whole list on every event.
// dateHdrPrefix marks a synthetic thread-list row that renders as a date group
// header ("Today", …) instead of a conversation. The unit separator survives
// GTK's C strings (NUL would truncate) and never appears in provider ids.
const dateHdrPrefix = "\x1fhdr:"

// cleanAIContext strips invisible preheader padding and collapses whitespace
// before a snippet is fed to the AI \u2014 see ai.CleanContext (shared with the
// background categorization worker).
func cleanAIContext(s string) string {
	return ai.CleanContext(s)
}

func (w *window) onRefresh() {
	if w.deps.Sync == nil {
		return
	}
	acctID := w.activeID
	logging.Trace("ui: sync now", "account", acctID)
	w.setSyncing(true)
	go func() {
		start := time.Now()
		err := w.deps.Sync(context.Background(), acctID)
		logging.Trace("ui: sync now done", "account", acctID, "dur", time.Since(start), "err", err)
		dispatch.Main(func() {
			w.setSyncing(false)
			if err != nil {
				// The activity row already reports the failure with its reason,
				// and the log keeps it; a toast saying the same thing would be
				// the second answer to one click.
				slog.Warn("ui: sync now", "err", err)
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
// A NewWindowAction (WebKit's native "Open Image/Link in New Window" context-menu
// action) never gets a real window — the app has no secondary WebView to host
// one — so its target is resolved and handed to the same external-open path.
func (w *window) onDecidePolicy(decision webkit.PolicyDecisioner, dtype webkit.PolicyDecisionType) bool {
	if dtype != webkit.PolicyDecisionTypeNavigationAction && dtype != webkit.PolicyDecisionTypeNewWindowAction {
		return false // resource loads (images/css) use default handling
	}
	nav, ok := decision.(*webkit.NavigationPolicyDecision)
	if !ok {
		return false
	}
	uri := nav.NavigationAction().Request().URI()
	if dtype == webkit.PolicyDecisionTypeNewWindowAction {
		w.openNewWindowTarget(uri)
		nav.Ignore()
		return true
	}
	if uri == "" || strings.HasPrefix(uri, "about:") || strings.HasPrefix(uri, "data:") || strings.HasPrefix(uri, "blob:") {
		return false // our own rendered content — show it in place
	}
	// Our own in-page affordances (the sender link and the per-message ⋯ menu
	// in a message header).
	if act, id, ok := parseMBAction(uri); ok {
		switch act {
		case "sender":
			w.showSenderActions(id)
		case "rcpt":
			w.showRecipientActions(id)
		case "menu":
			w.showMessageMenu(id)
		default:
			slog.Debug("ui: unknown mbaction", "uri", uri)
		}
		nav.Ignore()
		return true
	}
	if allowedExternalLink(uri) {
		w.openReaderLink(uri, "click")
	} else {
		slog.Debug("ui: blocked navigation to unsupported scheme", "uri", uri)
	}
	nav.Ignore()
	return true
}

// openNewWindowTarget resolves a WebKit "new window" navigation target (from
// the native "Open Image/Link in New Window" context-menu action) to
// something xdg-open can actually show. cid: and mbcache: resources resolve to
// their cached files; ordinary links are handed over as-is.
func (w *window) openNewWindowTarget(uri string) {
	if cid, ok := strings.CutPrefix(uri, "cid:"); ok {
		if dec, err := url.PathUnescape(cid); err == nil {
			cid = dec
		}
		cid = strings.Trim(cid, "<>")
		img, ok := w.inlineByCID[cid]
		if !ok {
			slog.Debug("ui: open in new window: unknown cid", "cid", cid)
			return
		}
		if img.path == "" {
			// The image is displayed but its download hasn't finished (or the
			// user asked before it started) — fetch it, then hand over the file.
			logging.Trace("ui: open inline image externally, fetching first", "cid", cid)
			go func() {
				path, err := w.fetchInlineImage(img)
				if err != nil {
					slog.Warn("ui: open inline image externally", "cid", cid, "err", err)
					return
				}
				dispatch.Main(func() { openExternal(path) })
			}()
			return
		}
		logging.Trace("ui: open inline image externally", "cid", cid, "path", img.path)
		openExternal(img.path)
		return
	}
	if key, ok := strings.CutPrefix(uri, "mbcache:"); ok {
		key = strings.TrimPrefix(key, "//")
		if w.remoteImages == nil {
			return
		}
		entry, ok := w.remoteImages.Open(key)
		if !ok {
			slog.Debug("ui: open in new window: unknown cached image")
			return
		}
		logging.Trace("ui: open cached external image", "path", entry.Path)
		openExternal(entry.Path)
		return
	}
	if uri == "" || strings.HasPrefix(uri, "about:") || strings.HasPrefix(uri, "data:") || strings.HasPrefix(uri, "blob:") {
		return // our own rendered content, nothing external to show
	}
	if allowedExternalLink(uri) {
		w.openReaderLink(uri, "new-window")
	} else {
		slog.Debug("ui: blocked new-window target with unsupported scheme", "uri", uri)
	}
}

// openReaderLink applies network-free click-tracking protection before handing
// a message link to the user's default browser. Other openExternal callers
// (attachments and unsubscribe actions) deliberately bypass this: their query
// parameters may be required by the provider rather than being email content.
func (w *window) openReaderLink(uri, source string) {
	target, stats := cleanExternalLink(uri)
	logging.Trace("ui: open external link", "source", source,
		"from_host", hostOfURL(uri), "to_host", hostOfURL(target),
		"unwrapped", stats.Unwrapped, "stripped_params", stats.StrippedParams)
	openExternal(target)
}

func allowedExternalLink(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "mailto", "ftp", "ftps":
		return true
	default:
		return false
	}
}

// onContextMenu runs before WebKit shows its native context menu. It swaps out
// the "Open Image in New Window" / "Open Link in New Window" stock items —
// each normally spawns a new window via the "create" signal, which the app
// never handles (no secondary WebView to give it), so left alone they select
// but silently do nothing — for a custom action that resolves the same
// target through openNewWindowTarget instead. Per WebKit's own docs, tapping
// the existing item's GAction can observe activation but can't stop the
// broken stock behavior, so the item itself must be replaced.
func (w *window) onContextMenu(menu *webkit.ContextMenu, hit *webkit.HitTestResult) bool {
	for i, item := range menu.Items() {
		var label, uri string
		switch item.StockAction() {
		case webkit.ContextMenuActionOpenImageInNewWindow:
			label, uri = "Open Image in New Window", hit.ImageURI()
		case webkit.ContextMenuActionOpenLinkInNewWindow:
			label, uri = "Open Link in New Window", hit.LinkURI()
		default:
			continue
		}
		logging.Trace("ui: patched context menu action", "action", item.StockAction().String(), "uri", uri)
		act := gio.NewSimpleAction("mb-open-new-window", nil)
		act.ConnectActivate(func(*glib.Variant) { w.openNewWindowTarget(uri) })
		menu.Remove(item)
		menu.Insert(webkit.NewContextMenuItemFromGaction(act, label, nil), i)
	}
	return false // show the (patched) menu
}

// parseMBAction splits an in-page affordance URI ("mbaction:<act>/<escaped
// message id>") into its action and decoded message id.
func parseMBAction(uri string) (act, id string, ok bool) {
	rest, ok := strings.CutPrefix(uri, "mbaction:")
	if !ok {
		return "", "", false
	}
	act, enc, found := strings.Cut(rest, "/")
	if !found {
		return "", "", false
	}
	id, err := url.QueryUnescape(enc)
	if err != nil {
		return "", "", false
	}
	return act, id, true
}

// openExternal hands a URI or path to the user's default handler via xdg-open,
// never loading it inside the app.
func openExternal(target string) {
	if err := exec.Command("xdg-open", target).Start(); err != nil {
		slog.Warn("ui: open external", "target", target, "err", err)
	}
}

// setSyncing marks a manual sync in flight. The activity row narrates it —
// spinner, phrase, elapsed — so the header only has to stop offering a second
// sync; swapping the button for another spinner would put the same fact in two
// places, and swapping widgets in a header bar makes it twitch.
func (w *window) setSyncing(on bool) {
	w.refreshBtn.SetSensitive(!on)
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
	if isDateHeader(id) {
		return // a date group header is not a conversation
	}
	if id == w.openThreadID {
		logging.Trace("ui: thread selected (already open)", "thread", id)
		return // already shown; avoids a re-render when the list refreshes live
	}
	logging.Trace("ui: thread selected", "thread", id, "account", w.activeID)
	w.showThread(id)
}

func (w *window) buildReader() *adw.NavigationPage {
	w.registerReaderActions()
	w.webview = webkit.NewWebView()
	// Serve inline (cid:) images from the cache as streamed resources rather than
	// embedding them in the HTML — a big inline image (e.g. a 15 MB banner) would
	// otherwise inflate the page to tens of MB and stall WebKit's parse.
	w.webview.Context().RegisterURIScheme("cid", w.serveCID)
	w.webview.Context().RegisterURIScheme("mbcache", w.serveRemoteImage)
	// Track the pointer over the reader (capture phase — WebKit handles input
	// internally) so showMessageMenu can anchor its popover at the clicked ⋯.
	motion := gtk.NewEventControllerMotion()
	motion.SetPropagationPhase(gtk.PhaseCapture)
	motion.ConnectMotion(func(x, y float64) { w.readerPtrX, w.readerPtrY = x, y })
	w.webview.AddController(motion)
	w.sectionCache = make(map[uiCacheKey]cachedSection)
	// The view background is what WebKit paints where no content is (resize
	// gutters, overscroll, the instant before the shell's first paint). Pin it
	// to white so those areas always match email content, regardless of theme.
	white := gdk.NewRGBA(1, 1, 1, 1)
	w.webview.SetBackgroundColor(&white)
	// The reader never navigates after this one shell load. A LoadHtml per
	// conversation made WebKit tear down the page's composited layer tree and
	// hand GTK an empty (black) GPU buffer until the new page's first frame
	// arrived — the flicker two earlier fixes (a white view background, then a
	// white cover over the swap) could only partially mask, because the black
	// frame sits below every background color and no reveal signal is exactly
	// first-paint. Instead the shell page loads once, and each conversation is
	// swapped into it via script (setReaderHTML → __mbSet, an innerHTML swap):
	// no navigation, no surface teardown, nothing to flash. The shell reports
	// readiness on the script-message channel; content set before that is
	// queued and flushed here.
	ucm := w.webview.UserContentManager()
	ucm.RegisterScriptMessageHandler(shellReadyHandler, "")
	ucm.ConnectScriptMessageReceived(func(*javascriptcore.Value) {
		logging.Trace("ui: reader shell ready", "pending", w.pendingReaderHTML != nil)
		w.readerReady = true
		if w.pendingReaderHTML != nil {
			inner := *w.pendingReaderHTML
			w.pendingReaderHTML = nil
			w.setReaderHTML(inner)
		}
	})
	settings := w.webview.Settings()
	// JavaScript is enabled only so the injected fit-to-width script can run.
	// Defense in depth keeps it safe: bodies are sanitized (no email scripts
	// survive), and the shell page (readerShellHTML) sets a strict CSP — script-src is locked to our
	// per-render nonce and default-src 'none' blocks all network (no fetch/XHR
	// exfiltration, no iframes), so only our own script ever executes.
	settings.SetEnableJavascript(true)
	// External images are fetched by the hardened cache client and rendered only
	// via mbcache:, never directly from an email-controlled origin. The global
	// privacy preference can opt out.
	w.imagesEnabled = !w.blockImages
	settings.SetAutoLoadImages(w.imagesEnabled)
	w.webview.SetVExpand(true)
	w.webview.SetHExpand(true)
	// Keep the reader a viewer: clicked links open in the default browser, never
	// inside the WebView.
	w.webview.ConnectDecidePolicy(w.onDecidePolicy)
	// WebKit's native "Open Image/Link in New Window" context-menu actions go
	// straight to the "create" signal (never through decide-policy) to spawn the
	// new window; patch them before the menu shows since the app has nothing to
	// give "create" to host one.
	w.webview.ConnectContextMenu(w.onContextMenu)

	w.header = gtk.NewLabel("")
	w.header.SetXAlign(0)
	w.header.SetHExpand(true)
	w.header.SetWrap(true)
	// WordChar, not the default word-only wrap: a subject with one long
	// unbreakable token (a CI test id, a URL) would otherwise set the label's
	// minimum width and force the reader pane — and the window — wider,
	// clipping the header's right edge.
	w.header.SetWrapMode(pango.WrapWordChar)
	// Let the user select & copy the subject and sender address from the header
	// (the message body is in the WebView, which is already selectable).
	w.header.SetSelectable(true)

	// Compact sender-auth status next to the subject (Gmail-style): a small shield
	// whose colour/icon conveys the SPF/DKIM/DMARC verdict; the full detail is on
	// hover (setAuthBadge sets the tooltip).
	w.authIcon = gtk.NewImageFromIconName("security-high-symbolic")
	w.authIcon.SetVAlign(gtk.AlignCenter)
	w.authIcon.SetVisible(false)
	// The thread's AI category carries over from the list (already-computed
	// context; no extra AI cost).
	w.readerCatTag = gtk.NewLabel("")
	w.readerCatTag.AddCSSClass("cat-tag")
	w.readerCatTag.SetVAlign(gtk.AlignCenter)
	w.readerCatTag.SetVisible(false)
	// Tracker count sits quietly at the end of the subject row instead of
	// claiming a row of its own.
	w.trackerLabel = gtk.NewLabel("")
	w.trackerLabel.AddCSSClass("dim-label")
	w.trackerLabel.AddCSSClass("caption")
	w.trackerLabel.SetVAlign(gtk.AlignCenter)
	w.trackerLabel.SetTooltipText("Likely tracking resources removed before loading")
	w.trackerLabel.SetVisible(false)
	headerRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
	setMargins(headerRow, 16, 16, 8, 8)
	headerRow.Append(w.authIcon)
	headerRow.Append(w.header)
	headerRow.Append(w.trackerLabel)
	headerRow.Append(w.readerCatTag)

	// A FlowBox wraps chips to additional rows instead of a single horizontal row,
	// whose summed width could otherwise force the reader pane — and the whole
	// window — wider than the screen (long attachment filenames pushed the window
	// controls off-screen). Each chip's label also ellipsizes (see attachmentChip).
	w.attachBox = gtk.NewFlowBox()
	w.attachBox.SetSelectionMode(gtk.SelectionNone)
	w.attachBox.SetColumnSpacing(6)
	w.attachBox.SetRowSpacing(6)
	w.attachBox.SetHomogeneous(false)
	setMargins(w.attachBox, 16, 16, 0, 8)
	w.attachBox.SetVisible(false)

	w.cautionLabel = gtk.NewLabel("")
	w.cautionLabel.SetXAlign(0)
	w.cautionLabel.SetWrap(true)
	// Same as w.header: a caution line quoting a long URL must break mid-token
	// rather than widen the pane.
	w.cautionLabel.SetWrapMode(pango.WrapWordChar)
	w.cautionLabel.AddCSSClass("caption")
	w.cautionLabel.AddCSSClass("warning")
	setMargins(w.cautionLabel, 16, 16, 0, 6)
	w.cautionLabel.SetVisible(false)

	// Revealed while an in-place translation is shown; reverts to the original.
	w.translationBanner = adw.NewBanner("Showing translation")
	w.translationBanner.SetUseMarkup(false)
	w.translationBanner.SetButtonLabel("Show original")
	w.translationBanner.SetRevealed(false)
	w.translationBanner.ConnectButtonClicked(w.showOriginal)
	w.remoteImageBanner = adw.NewBanner("")
	w.remoteImageBanner.SetUseMarkup(false)
	w.remoteImageBanner.SetRevealed(false)
	// The banner only appears when the privacy opt-out withheld images, so its
	// action is always "load them now".
	w.remoteImageBanner.ConnectButtonClicked(func() { w.setImagesEnabled(true) })

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.Append(w.translationBanner)
	box.Append(headerRow)
	box.Append(w.buildSummaryCard())
	box.Append(w.buildInviteCard())
	box.Append(w.attachBox)
	box.Append(w.cautionLabel)
	box.Append(w.remoteImageBanner)

	box.Append(w.webview)

	// The one and only navigation: load the empty shell page that every
	// conversation is swapped into (see setReaderHTML).
	w.webview.LoadHtml(readerShellHTML(), "about:blank")

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

	// Reply all is the primary one-click action — it never drops participants
	// from a group thread, and on a 1:1 email it degrades to a plain reply
	// (replyAllRecipients dedups down to just the sender). Reply (sender only)
	// and Forward live in the SplitButton's dropdown as a native menu model.
	replyMenu := gio.NewMenu()
	replyMenu.Append("Reply", "win.reader-reply")
	replyMenu.Append("Forward", "win.reader-forward")

	w.replyBtn = adw.NewSplitButton()
	w.replyBtn.SetIconName("mail-reply-all-symbolic")
	w.replyBtn.SetTooltipText("Reply all to conversation (r) — dropdown: Reply, Forward")
	a11yLabel(w.replyBtn, "Reply all to conversation")
	w.replyBtn.ConnectClicked(w.onReplyAll)
	w.replyBtn.SetMenuModel(replyMenu)

	w.archiveBtn = gtk.NewButtonFromIconName("mail-archive-symbolic")
	w.archiveBtn.SetTooltipText("Archive conversation (a)")
	a11yLabel(w.archiveBtn, "Archive conversation")
	w.archiveBtn.ConnectClicked(w.onArchive)

	// AI actions (only useful when an assistant is configured).
	w.translateBtn = gtk.NewButtonFromIconName("translate-symbolic")
	w.translateBtn.SetTooltipText("Translate conversation to English (t)")
	a11yLabel(w.translateBtn, "Translate conversation to English")
	w.translateBtn.ConnectClicked(w.onTranslate)

	w.summaryBtn = gtk.NewButtonFromIconName("summarize-symbolic")
	w.summaryBtn.SetTooltipText("Summarize conversation with AI")
	a11yLabel(w.summaryBtn, "Summarize conversation with AI")
	w.summaryBtn.ConnectClicked(w.onSummarize)

	// AI reply: a popover of AI-suggested quick replies plus reply intents. The
	// popover is rebuilt per open (fresh suggestions for the current message).
	w.aiReplyBtn = gtk.NewMenuButton()
	w.aiReplyBtn.SetIconName("sparkle-symbolic")
	w.aiReplyBtn.SetTooltipText("AI reply")
	a11yLabel(w.aiReplyBtn, "AI reply")
	w.aiReplyBtn.SetCreatePopupFunc(func(btn *gtk.MenuButton) {
		btn.SetPopover(w.buildAIReplyPopover())
	})

	// Secondary actions (phishing analysis, star, mark-unread, trash) live
	// in the overflow — analysis is on-demand and rare, so it doesn't earn a slot.
	w.overflowBtn = gtk.NewMenuButton()
	w.overflowBtn.SetIconName("view-more-symbolic")
	w.overflowBtn.SetTooltipText("More actions")
	a11yLabel(w.overflowBtn, "More actions")
	// A native menu model (standard GTK4): normal-weight rows, native checkmarks
	// for toggles, automatic separators. Rebuilt on each open so the dynamic
	// items (spam/not-spam, delete-forever, find-from-sender) match the context,
	// with the star state synced first.
	w.overflowBtn.SetCreatePopupFunc(func(btn *gtk.MenuButton) {
		w.starAction.SetState(glib.NewVariantBoolean(w.threadStarred()))
		menu := gtk.NewPopoverMenuFromModel(w.buildReaderMenuModel())
		btn.SetPopover(&menu.Popover)
	})

	hb.PackStart(w.replyBtn)
	hb.PackStart(w.aiReplyBtn)
	hb.PackStart(w.archiveBtn)
	hb.PackEnd(w.overflowBtn)
	hb.PackEnd(w.translateBtn)
	hb.PackEnd(w.summaryBtn)
	w.refreshAIVisibility()
	w.setActionsSensitive(false)

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(w.readerStack)
	return adw.NewNavigationPage(tv, "Reader")
}

// refreshAIVisibility shows/hides the reader header's AI buttons per the
// current per-feature toggles (Preferences → AI Features). Unlike the rest of
// the AI UI — compose windows, menus, dialogs — which are rebuilt fresh on
// every open and so pick up the current flags automatically, these three
// buttons are built once in w.build() and persist for the window's lifetime,
// so a toggle flipped in Preferences must call this to apply live.
func (w *window) refreshAIVisibility() {
	show := w.deps.Assistant != nil
	w.translateBtn.SetVisible(show && w.aiTranslate)
	w.summaryBtn.SetVisible(show && w.aiSummarize)
	w.aiReplyBtn.SetVisible(show && (w.aiSmartReplies || w.aiDraft))
}

func (w *window) setActionsSensitive(on bool) {
	canModify := on && w.deps.ModifyLabels != nil
	w.archiveBtn.SetSensitive(canModify)
	w.replyBtn.SetSensitive(on && w.deps.Send != nil)
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
// aliases), otherwise the From address with its display name (like Gmail's
// compose prefill).
func replyTarget(m model.Message) string {
	if rt := strings.TrimSpace(m.ReplyTo); rt != "" {
		return rt
	}
	return addressToken(m.FromName, m.FromAddr)
}

// addressToken renders "Name <addr>" safely for a comma-separated recipient
// line: a plain name stays unquoted (like Gmail's compose), one with specials
// is quoted via net/mail so a comma in it can't split the list, and one that
// would need RFC 2047 encoding is dropped — the encoded form is unreadable in
// a compose entry — leaving the bare address.
func addressToken(name, addr string) string {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return addr
	case plainName(name):
		return name + " <" + addr + ">"
	}
	s := (&mail.Address{Name: name, Address: addr}).String()
	if strings.Contains(s, "=?") {
		return addr
	}
	return s
}

// plainName reports whether a display name needs no quoting or encoding in a
// recipient line (ASCII letters/digits/space and -/' only).
func plainName(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ' ', r == '-', r == '\'':
		default:
			return false
		}
	}
	return true
}

// replyToLine is the To line for a sender-only reply to m. Replying to your
// own message continues it to its original recipients (Gmail behavior) rather
// than addressing yourself.
func replyToLine(m model.Message, own bool) string {
	if own && strings.TrimSpace(m.ToAddrs) != "" {
		return m.ToAddrs
	}
	return replyTarget(m)
}

// isGitHubNotification reports whether m is a GitHub notification email
// (From notifications@github.com, or a Reply-To on reply.github.com — the
// address GitHub's reply-by-email uses). GitHub's own reply parsing only
// looks at the new text above its own marker, so quoting the original for it
// — diff hunks, the prior comment thread, GitHub's UI chrome, the "Reply to
// this email directly, or view it on GitHub" footer — is pure noise with no
// upside, unlike a normal human correspondent.
func isGitHubNotification(m model.Message) bool {
	return addrDomain(m.FromAddr) == "github.com" || addrDomain(m.ReplyTo) == "reply.github.com"
}

// addrDomain returns the lowercased domain of an email address, accepting
// either a bare address or a "Name <addr>" form ("" if it doesn't parse).
func addrDomain(addr string) string {
	a, err := mail.ParseAddress(addr)
	if err != nil {
		return ""
	}
	at := strings.LastIndex(a.Address, "@")
	if at < 0 {
		return ""
	}
	return strings.ToLower(a.Address[at+1:])
}

// replyQuote returns the reply body/HTML quote for m — empty for a GitHub
// notification (see isGitHubNotification), the full original otherwise.
func (w *window) replyQuote(m model.Message) (body, quoteHTML string) {
	if isGitHubNotification(m) {
		return "", ""
	}
	return quoteOriginal(m, w.bodyTextFor(m)), w.bodyHTMLFor(m)
}

// replyInit builds the prefilled compose for a reply to m (To, Re: subject,
// quoted body, threading headers).
func (w *window) replyInit(m model.Message) model.OutgoingMessage {
	body, quoteHTML := w.replyQuote(m)
	return model.OutgoingMessage{
		To:            replyToLine(m, w.isOwnAddress(m.FromAddr)),
		Subject:       ensureRePrefix(m.Subject),
		Body:          body,
		QuoteHTML:     quoteHTML,
		SkipSignature: isGitHubNotification(m),
		InReplyTo:     m.RFC822MsgID,
		References:    strings.TrimSpace(m.References + " " + m.RFC822MsgID),
		ThreadID:      m.ThreadID,
	}
}

func (w *window) onReply() {
	m := w.openMsg
	if m.GmailID == "" {
		return
	}
	logging.Trace("ui: reply", "id", m.GmailID, "thread", w.openThreadID, "to", replyTarget(m), "account", w.activeID)
	w.flushMarkRead() // explicit engagement — mark read now, don't wait out the timer
	w.openCompose(w.replyInit(m), w.threadContextFor(m), "Reply")
}

func (w *window) onReplyAll() {
	init, aiContext, ok := w.replyAllInit()
	if !ok {
		return
	}
	logging.Trace("ui: reply all", "id", w.openMsg.GmailID, "thread", w.openThreadID, "to", init.To, "cc", init.Cc, "account", w.activeID)
	w.flushMarkRead()
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
	return w.replyAllInitFor(m), w.threadContextFor(m), true
}

// replyAllInitFor builds the reply-all prefill for a specific message — the
// open one for the header-bar action, or any message of the open thread for
// the in-page per-message reply links.
func (w *window) replyAllInitFor(m model.Message) model.OutgoingMessage {
	to, cc := replyAllRecipients(m, w.activeEmail)
	body, quoteHTML := w.replyQuote(m)
	return model.OutgoingMessage{
		To:            to,
		Cc:            cc,
		Subject:       ensureRePrefix(m.Subject),
		Body:          body,
		QuoteHTML:     quoteHTML,
		SkipSignature: isGitHubNotification(m),
		InReplyTo:     m.RFC822MsgID,
		References:    strings.TrimSpace(m.References + " " + m.RFC822MsgID),
		ThreadID:      m.ThreadID,
	}
}

// threadMessageByID returns the open conversation's message with the given id
// — the per-message header icons name their target this way.
func (w *window) threadMessageByID(gmailID string) (model.Message, bool) {
	for _, m := range w.openThreadMsgs {
		if m.GmailID == gmailID {
			return m, true
		}
	}
	logging.Trace("ui: message not in open thread", "id", gmailID, "thread", w.openThreadID)
	return model.Message{}, false
}

// replyToMessage opens a reply compose targeting a specific message of the
// open conversation (all: reply-all vs sender-only) — the per-message header
// icons let the user answer any message in a thread, not just the newest.
func (w *window) replyToMessage(gmailID string, all bool) {
	if w.deps.Send == nil {
		return
	}
	m, ok := w.threadMessageByID(gmailID)
	if !ok {
		return
	}
	logging.Trace("ui: reply to message", "id", m.GmailID, "thread", w.openThreadID, "all", all, "account", w.activeID)
	w.flushMarkRead()
	if all {
		w.openCompose(w.replyAllInitFor(m), w.threadContextFor(m), "Reply all")
	} else {
		w.openCompose(w.replyInit(m), w.threadContextFor(m), "Reply")
	}
}

// forwardMessage forwards a specific message of the open conversation — the
// per-message header icons' forward action.
func (w *window) forwardMessage(gmailID string) {
	if w.deps.Send == nil {
		return
	}
	if m, ok := w.threadMessageByID(gmailID); ok {
		w.forwardMsg(m)
	}
}

// aiReply opens a reply compose for the open message with an AI action applied
// (a chosen quick reply, an intent to auto-draft, or the AI-draft dialog).
func (w *window) aiReply(auto composeAutoAI) {
	init, aiContext, ok := w.replyAllInit()
	if !ok {
		return
	}
	logging.Trace("ui: ai reply", "id", w.openMsg.GmailID, "thread", w.openThreadID,
		"quickReply", auto.quickReply != "", "instruction", logging.Body(auto.instruction), "openDialog", auto.openDialog, "account", w.activeID)
	w.openCompose(init, aiContext, "Reply", auto)
}

// buildAIReplyPopover builds the reader's AI-reply popover: reply intents
// first (tap → AI drafts a full reply in that direction), then AI-suggested
// quick replies below (fetched async; tap → compose prefilled with that
// reply). The async section trails the fixed intents so its streaming-in
// results grow the popover downward instead of shoving the intents down
// while the user is picking one. Rebuilt on each open so suggestions match
// the current message.
func (w *window) buildAIReplyPopover() *gtk.Popover {
	pop := gtk.NewPopover()
	box := gtk.NewBox(gtk.OrientationVertical, 4)
	box.SetSizeRequest(300, -1)
	setMargins(box, 8, 8, 8, 8)

	_, threadContext, ok := w.replyAllInit()
	if !ok || w.deps.Assistant == nil || (!w.aiSmartReplies && !w.aiDraft) {
		box.Append(aiPopLabel("Open a message to reply."))
		pop.SetChild(box)
		return pop
	}

	if w.aiDraft {
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
	}

	if w.aiSmartReplies {
		if w.aiDraft {
			box.Append(gtk.NewSeparator(gtk.OrientationHorizontal))
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
		logging.Trace("ui: suggest quick replies", "thread", w.openThreadID, "account", w.activeID)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			replies, err := w.deps.Assistant.SmartReplies(ctx, threadContext)
			logging.Trace("ui: suggest quick replies result", "n", len(replies), "err", err)
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
	}

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

// replyAllRecipients computes To (reply target + original To) and Cc (original
// Cc), excluding the account's own address and de-duplicating across both
// lines. Display names are preserved (Gmail-like, via addressToken). Bcc is
// never carried into a reply, matching Gmail. When every candidate was you (a
// note to yourself), To falls back to the reply target so the compose never
// opens with no recipient.
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
			out = append(out, addressToken(a.Name, a.Address))
		}
		return out
	}
	toList := append(collect(replyTarget(m)), collect(m.ToAddrs)...)
	ccList := collect(m.CcAddrs)
	if len(toList) == 0 && len(ccList) == 0 {
		return replyTarget(m), ""
	}
	return strings.Join(toList, ", "), strings.Join(ccList, ", ")
}

func (w *window) onForward() {
	m := w.openMsg
	if m.GmailID == "" {
		return
	}
	w.forwardMsg(m)
}

// forwardMsg opens a forward compose for m — the newest message via the header
// bar's dropdown, or any message of the open thread via its header icons.
func (w *window) forwardMsg(m model.Message) {
	logging.Trace("ui: forward", "id", m.GmailID, "thread", w.openThreadID, "account", w.activeID)
	w.flushMarkRead()
	init := model.OutgoingMessage{
		Subject:   ensureFwdPrefix(m.Subject),
		Body:      forwardOriginal(m, w.bodyTextFor(m)),
		QuoteHTML: w.bodyHTMLFor(m),
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
		if a.ContentID != "" {
			continue // inline body image, not a real attachment to carry over
		}
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
// Gmail system labels that aren't folders are omitted. The store queries run off
// the main thread (this fires on every coalesced sync refresh, and main-thread
// SQL stalls scale with mailbox size); the widget rebuild is dispatched back,
// guarded by labelsGen so overlapping reloads apply last-request-wins.
func (w *window) loadLabels() {
	w.labelsGen++
	gen := w.labelsGen
	acct := w.activeID
	ids := make([]int64, 0, len(w.deps.Accounts))
	for _, a := range w.deps.Accounts {
		ids = append(ids, a.ID)
	}
	go func() {
		start := time.Now()
		ctx := context.Background()
		labels, err := w.deps.Store.ListLabels(ctx, acct)
		if err != nil {
			slog.Error("ui: load labels", "err", err)
			return
		}
		// Per-account unread-inbox counts in one query (feeds the inbox badge,
		// the account pills, and the title).
		counts, err := w.deps.Store.UnreadCountByLabelForAccounts(ctx, ids, model.LabelInbox)
		if err != nil {
			slog.Warn("ui: account unread counts", "err", err)
			counts = map[int64]int{}
		}
		snoozed, err := w.deps.Store.SnoozedCount(ctx, acct)
		if err != nil {
			slog.Warn("ui: snoozed count", "err", err)
		}
		dispatch.Main(func() {
			slog.Debug("ui: loadLabels", "dur", time.Since(start))
			if gen != w.labelsGen || acct != w.activeID {
				logging.Trace("ui: load labels superseded", "gen", gen, "account", acct)
				return // a newer reload (or an account switch) owns the sidebar
			}
			w.applySidebar(labels, counts, snoozed)
		})
	}()
}

// applySidebar renders loadLabels' query results into the sidebar widgets.
// Main thread only.
func (w *window) applySidebar(labels []model.Label, counts map[int64]int, snoozed int) {
	have := make(map[string]bool, len(labels))
	for _, l := range labels {
		have[l.GmailID] = true
	}
	inboxCount := counts[w.activeID]

	// Rebuild the sidebar widgets only when its structure or the inbox badge
	// actually changed — an idle 60s sync (no new mail) leaves it untouched,
	// avoiding widget churn and a selection flicker every cycle.
	sig := w.sidebarSignature(labels, have, inboxCount, snoozed)
	if sig != w.sidebarSig {
		w.sidebarSig = sig
		w.labelBox.RemoveAll()
		w.sidebar = w.sidebar[:0]

		// Only the Inbox carries an unread-count badge — that's where new mail
		// matters; badges on every folder/label read as noise.
		for _, f := range systemFolders {
			if f.id == allMailID || f.id == snoozedID {
				count := 0
				if f.id == snoozedID {
					count = snoozed // neutral badge — see appendFolder
				}
				w.appendFolder(f.id, f.icon, f.name, count) // virtual views, no gmail label
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
		// The snooze mirror's labels are bookkeeping, not places to browse — the
		// Snoozed virtual folder above is their UI.
		firstUser := true
		for _, l := range labels {
			if l.Type != model.LabelUser || model.IsSnoozeLabel(l.Name) {
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
func (w *window) sidebarSignature(labels []model.Label, have map[string]bool, inboxUnread, snoozed int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "a=%d;inbox=%d;snoozed=%d;", w.activeID, inboxUnread, snoozed)
	for _, f := range systemFolders {
		if f.id == allMailID || f.id == snoozedID || have[f.id] {
			b.WriteString("f:" + f.id + ";")
		}
	}
	for _, l := range labels {
		if l.Type == model.LabelUser && !model.IsSnoozeLabel(l.Name) {
			b.WriteString("u:" + l.GmailID + "=" + l.Name + ";")
		}
	}
	return b.String()
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

// accountUnreadCoalesceMS is how long pill refreshes wait for a burst of
// sibling-account sync events to settle before one off-thread query serves
// them all.
const accountUnreadCoalesceMS = 300

// refreshAccountUnread fetches the per-account unread-inbox counts and applies
// them to the pills and title. Used when only sibling-account counts changed
// (so the active account's sidebar needn't reload). Calls are coalesced — a
// sync burst on a non-active account fires this per event — and the count query
// runs off the main thread.
func (w *window) refreshAccountUnread() {
	if w.unreadRefreshPending {
		return
	}
	w.unreadRefreshPending = true
	glib.TimeoutAdd(accountUnreadCoalesceMS, func() bool {
		w.unreadRefreshPending = false
		ids := make([]int64, 0, len(w.deps.Accounts))
		for _, a := range w.deps.Accounts {
			ids = append(ids, a.ID)
		}
		go func() {
			counts, err := w.deps.Store.UnreadCountByLabelForAccounts(context.Background(), ids, model.LabelInbox)
			if err != nil {
				slog.Warn("ui: account unread counts", "err", err)
				return
			}
			logging.Trace("ui: account unread refreshed", "accounts", len(ids))
			dispatch.Main(func() { w.applyAccountUnread(counts) })
		}()
		return false
	})
}

// appendFolder adds a selectable folder/label row mapped to id.
func (w *window) appendFolder(id, icon, name string, count int) {
	w.labelBox.Append(folderRow(icon, name, count, id == snoozedID))
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
	logging.Trace("ui: switch account", "from", w.activeID, "to", a.ID, "email", a.Email)
	w.switchStart = time.Now()
	w.activeID = a.ID
	w.activeEmail = a.Email
	w.signature = w.signatureForActive() // signature the next compose appends
	w.current = model.LabelInbox
	w.clearReader()
	// Blank the list NOW: its reload is async (last-request-wins), so without
	// this the old account's threads stay on screen until the new account's
	// query lands — instant when the store is idle, but seconds during heavy
	// churn (a triage session's mirror queue + syncs), which reads as a laggy
	// switch. An empty list is honest immediate feedback; rows follow.
	w.threadByID = map[string]model.ThreadSummary{}
	w.threadModel.Splice(0, w.threadModel.NItems(), nil)
	w.threadIDs = nil
	w.rowSig = map[string]string{}
	w.threadPage = threadPageState{}
	w.threadFailed = nil
	w.updateThreadPageStatus()
	w.loadLabels()
	w.selectLabel(model.LabelInbox)
	w.refreshOutbox()
}

// clearReader returns the reader to its empty state and forgets the open
// conversation, so stale actions can't target a thread from another account.
func (w *window) clearReader() {
	w.cancelMarkRead() // no thread open → nothing to mark read
	if w.renderCancel != nil {
		w.renderCancel()
		w.renderCancel = nil
	}
	w.renderFetching = nil // the cancelled render owns no fetch any more
	w.openThreadID = ""
	w.openThreadMsgs = nil
	w.openMsg = model.Message{}
	w.remoteImageBanner.SetRevealed(false)
	w.resetTranslation()
	w.hideSummary()
	w.showInviteCard(0, nil)
	w.setReaderCategory("", false)
	w.setActionsSensitive(false)
	w.readerStack.SetVisibleChildName("empty")
}

// updateEmptyFolderBanner reveals the "Empty now" banner only when the current
// folder is Trash or Spam and actually holds messages — a destructive CTA over
// an empty folder is noise. A live search replaces the folder view, so the
// banner (which empties the whole folder, not the hits) stays hidden then.
func (w *window) updateEmptyFolderBanner(visible int) {
	show := w.deps.EmptyFolder != nil &&
		(w.current == model.LabelTrash || w.current == model.LabelSpam) &&
		visible > 0 &&
		w.searchEntry.Text() == ""
	w.emptyFolderBanner.SetRevealed(show)
}

func (w *window) selectLabel(labelID string) {
	logging.Trace("ui: select label", "label", labelID, "account", w.activeID)
	w.current = labelID
	// The "empty folder" banner appears only in Trash/Spam. Hide it here; the
	// destructive "Empty now" CTA is revealed only once we know the folder is
	// non-empty (updateEmptyFolderBanner, from showThreads).
	if w.deps.EmptyFolder != nil && (labelID == model.LabelTrash || labelID == model.LabelSpam) {
		name := "Trash"
		if labelID == model.LabelSpam {
			name = "Spam"
		}
		w.emptyFolderBanner.SetTitle(name + " — messages here can be permanently deleted")
	}
	w.emptyFolderBanner.SetRevealed(false)
	// Switching label clears any active search without re-triggering it.
	w.suppressSearch = true
	w.searchEntry.SetText("")
	w.suppressSearch = false
	w.refreshList("")
	// Keep the sidebar highlight on the chosen folder: a programmatic switch
	// (opening an archived conversation from a notification) has no row click
	// to move it. A no-op when the row is already selected.
	w.restoreSidebarSelection()
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
	vs.OpenThread = w.openThreadID // "" when nothing is open
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
	logging.Trace("ui: set zoom", "zoom", z)
	w.readerZoom = z
	w.webview.SetZoomLevel(z)
	evalJS(w.webview, readerRefitScript)
	w.saveViewState()
}

// showThread opens a conversation: it loads all its messages, renders them
// stacked in the reader, and marks any unread ones read.
func (w *window) showThread(threadID string) {
	// In the Drafts folder, a click resumes editing the draft in compose rather
	// than rendering it read-only.
	if w.current == model.LabelDraft && w.deps.Send != nil {
		logging.Trace("ui: show thread → edit draft", "thread", threadID, "account", w.activeID)
		w.openDraftForEdit(threadID)
		return
	}
	// The thread read runs off the main thread: it is normally instant, but a
	// background VACUUM (compact, retention, empty-folder cleanup) holds an
	// exclusive lock, and a synchronous read here would freeze the whole UI
	// behind it for up to the store's busy timeout. openGen makes the last
	// click win when reads complete out of order.
	w.openGen++
	gen := w.openGen
	acctID := w.activeID
	go func() {
		msgs, err := w.readThreadMessages(acctID, threadID)
		// Hydration completes a conversation the backfill capped, which is a
		// provider round trip. Waiting for it here would put the network in front
		// of mail that is already cached, so it runs after the render below —
		// except when there is nothing cached to render, where it is the only way
		// to open the conversation at all.
		hydrated := false
		if len(msgs) == 0 && err == nil && w.deps.HydrateThread != nil {
			w.hydrateThread(acctID, threadID)
			hydrated = true
			msgs, err = w.readThreadMessages(acctID, threadID)
		}
		dispatch.Main(func() {
			if gen != w.openGen {
				logging.Trace("ui: show thread superseded", "thread", threadID)
				return
			}
			if err != nil || len(msgs) == 0 {
				if err != nil {
					slog.Warn("ui: load thread", "thread", threadID, "err", err)
					w.toast("Couldn't open this conversation")
				}
				logging.Trace("ui: show thread empty", "thread", threadID, "n", len(msgs), "err", err)
				return
			}
			w.showThreadMsgs(threadID, msgs)
		})
		if !hydrated && len(msgs) > 0 && w.deps.HydrateThread != nil {
			w.hydrateThread(acctID, threadID)
		}
	}()
}

// readThreadMessages reads one conversation from the local cache. Off the main
// thread: it is normally instant, but a background VACUUM holds an exclusive
// lock, and a synchronous read would freeze the UI behind it.
func (w *window) readThreadMessages(accountID int64, threadID string) ([]model.Message, error) {
	ctx, cancel := context.WithTimeout(context.Background(), openThreadTimeout)
	defer cancel()
	return w.deps.Store.ListThreadMessages(ctx, accountID, threadID)
}

// hydrateThread fetches a conversation's complete server-side message membership
// once (the result is persisted, so it is a no-op on every later open). When it
// adds messages the engine publishes MessageUpserted for the thread, which
// re-renders it if it is the one on screen — so a capped backfill's missing
// older messages appear a moment after the cached ones, rather than the cached
// ones waiting on the network. Off the main thread.
func (w *window) hydrateThread(accountID int64, threadID string) {
	ctx, cancel := context.WithTimeout(context.Background(), threadHydrateTimeout)
	defer cancel()
	added, err := w.deps.HydrateThread(ctx, accountID, threadID)
	if err != nil {
		// Opening mail must remain useful offline: hydration repairs a partial
		// cache when possible, but never hides what is already local.
		logging.Trace("ui: hydrate thread failed; using cache", "thread", threadID, "err", err)
		return
	}
	logging.Trace("ui: hydrate thread done", "thread", threadID, "added", added)
}

// openThreadTimeout bounds the store read behind opening a conversation. The
// SQLite busy timeout (5s) caps a lock wait, but a reader-pool checkout has no
// bound of its own — this keeps a click from waiting forever on a saturated
// pool.
const openThreadTimeout = 10 * time.Second

// threadHydrateTimeout bounds the one-time provider lookup that completes a
// conversation omitted by a capped backfill. Failure falls back to local mail.
const threadHydrateTimeout = 15 * time.Second

// showThreadMsgs is showThread's main-thread continuation once the thread's
// messages have been read.
func (w *window) showThreadMsgs(threadID string, msgs []model.Message) {
	logging.Trace("ui: show thread", "thread", threadID, "n", len(msgs), "account", w.activeID)
	w.openThreadID = threadID
	w.openThreadMsgs = msgs
	w.openMsg = msgs[len(msgs)-1] // newest, for reply/forward/star/unread
	w.resetTranslation()          // a freshly opened thread shows the original
	w.hideSummary()               // collapse any summary from the previous thread
	// Re-apply the global policy after a one-conversation "Show images" override.
	if want := !w.blockImages; want != w.imagesEnabled {
		w.imagesEnabled = want
		w.webview.Settings().SetAutoLoadImages(want)
	}
	w.setActionsSensitive(true)
	w.readerStack.SetVisibleChildName("message")
	w.innerSplit.SetShowContent(true)

	w.renderConversation(msgs)

	// Mark the thread read only after it stays open a beat, so j/k skimming past
	// unread mail doesn't destroy its unread state. The timer is cancelled if the
	// user navigates to another thread or backs out first.
	w.scheduleMarkRead(threadID, msgs)
}

// markReadDelay is how long a thread must stay open before it is marked read, so
// quick j/k navigation past it leaves the unread state intact.
const markReadDelay = 1500 // ms

// cancelMarkRead drops any pending "mark thread read" timer (the user navigated
// away before it fired).
func (w *window) cancelMarkRead() {
	if w.markReadTimer != 0 {
		glib.SourceRemove(w.markReadTimer)
		w.markReadTimer = 0
	}
	w.pendingMarkRead = nil
}

// flushMarkRead marks the open thread read immediately, if a deferred mark-read
// is pending. Explicit engagement (reply, forward) shouldn't wait out the timer.
func (w *window) flushMarkRead() {
	if w.pendingMarkRead == nil {
		return
	}
	fn := w.pendingMarkRead
	w.cancelMarkRead()
	fn()
}

// scheduleMarkRead arms a timer to mark the thread's unread messages read after
// markReadDelay, cancelling any previous pending one. No-op when nothing is
// unread or label edits aren't available.
func (w *window) scheduleMarkRead(threadID string, msgs []model.Message) {
	w.cancelMarkRead()
	if w.deps.ModifyLabels == nil {
		return
	}
	var ids []string
	for _, m := range msgs {
		if m.IsUnread {
			ids = append(ids, m.GmailID)
		}
	}
	if len(ids) == 0 {
		return
	}
	acctID := w.activeID
	w.pendingMarkRead = func() {
		logging.Trace("ui: mark thread read", "thread", threadID, "n", len(ids), "account", acctID)
		go func() {
			if err := w.deps.ModifyLabels(context.Background(), acctID, ids, nil, []string{model.LabelUnread}); err != nil {
				slog.Warn("ui: mark read", "n", len(ids), "err", err)
			}
			dispatch.Main(w.loadLabels)
		}()
	}
	logging.Trace("ui: schedule mark thread read", "thread", threadID, "n", len(ids), "account", acctID, "delay_ms", markReadDelay)
	w.markReadTimer = glib.TimeoutAdd(markReadDelay, func() bool {
		w.markReadTimer = 0
		if w.pendingMarkRead != nil {
			fn := w.pendingMarkRead
			w.pendingMarkRead = nil
			fn()
		}
		return false // one-shot
	})
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
	logging.Trace("ui: open draft for edit", "thread", threadID, "account", w.activeID)
	acctID := w.activeID
	// Progress belongs to the activity row, not a toast: a toast acknowledges a
	// finished action, and one that says "…" leaves a notice that never
	// resolves. Resolving a provider draft can take two network round trips.
	endOpen := w.opActivity("draft", w.emailForAccount(acctID), "Opening the draft")
	go func() {
		note := ""
		defer func() { dispatch.Main(func() { endOpen(note) }) }()
		ctx := context.Background()
		// Local-first drafts contain the complete compose payload (including
		// attachments), so reopening them never needs the network.
		if store.IsLocalDraftID(threadID) {
			d, err := w.deps.Store.LocalDraft(ctx, threadID)
			if err != nil {
				slog.Warn("ui: load local draft", "id", threadID, "err", err)
				note = doneErr(err)
				dispatch.Main(func() { w.toast("Couldn't open this draft") })
				return
			}
			dispatch.Main(func() { w.openCompose(d.Message, "", "Edit draft") })
			return
		}
		// Read off the main thread (a background VACUUM would block a
		// synchronous read here — see showThread) and bounded like any other
		// store read behind a click.
		listCtx, cancelList := context.WithTimeout(ctx, openThreadTimeout)
		msgs, err := w.deps.Store.ListThreadMessages(listCtx, acctID, threadID)
		cancelList()
		if err != nil || len(msgs) == 0 {
			if err != nil {
				slog.Warn("ui: load draft thread", "thread", threadID, "err", err)
				note = doneErr(err)
			} else {
				note = "error: the draft is no longer in the cache"
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
		if !dm.BodyFetched && w.deps.FetchBody != nil {
			fetchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			if err := w.deps.FetchBody(fetchCtx, dm.AccountID, dm.GmailID); err != nil {
				slog.Warn("ui: fetch draft body", "id", dm.GmailID, "err", err)
			} else {
				dm.BodyFetched = true
			}
			cancel()
		}
		if !dm.BodyFetched {
			// Never replace an unfetched provider draft with its short list snippet:
			// saving that offline would destroy most of the original body.
			note = "error: the full body isn't available offline yet"
			dispatch.Main(func() { w.toast("This draft's full body isn't available offline yet") })
			return
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
			findCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if id, err := w.deps.FindDraftID(findCtx, acctID, dm.GmailID); err != nil {
				slog.Warn("ui: find draft id", "id", dm.GmailID, "err", err)
			} else {
				draftID = id
			}
			cancel()
		}
		dispatch.Main(func() {
			w.openCompose(model.OutgoingMessage{
				To:              strings.TrimSpace(dm.ToAddrs),
				Cc:              strings.TrimSpace(dm.CcAddrs),
				Bcc:             strings.TrimSpace(dm.BccAddrs),
				Subject:         dm.Subject,
				Body:            body,
				InReplyTo:       dm.InReplyTo,
				References:      dm.References,
				ThreadID:        dm.ThreadID,
				DraftID:         draftID,
				SourceMessageID: dm.GmailID,
			}, "", "Edit draft")
		})
	}()
}

// loadingInner is the reader content shown while message bodies are being
// fetched — swapped into the shell like any conversation, so no navigation.
const loadingInner = `<div style="display:flex;align-items:center;justify-content:center;height:80vh;color:#888;font-size:14px">Loading message…</div>`

// cachedSection is a message's rendered (sanitized, de-tracked, quote-collapsed)
// section HTML plus its blocked-tracker count. Sections are immutable once a
// message's body is fetched, so they can be reused across thread re-opens.
type cachedSection struct {
	head     string // the message's header, which doubles as its <details> summary
	body     string // gist card + rendered body, shown only while it is open
	trackers int
}

// sectionCacheCap bounds how many rendered sections are kept in memory.
const sectionCacheCap = 400

// aiCacheCap bounds the session AI caches (per-message translations, per-thread
// summaries/analyses) the same way sectionCacheCap bounds rendered sections —
// they hold full bodies, so an unbounded session would grow without limit. An
// eviction is just a future cache miss (both are also persisted in the store).
const aiCacheCap = 400

func clearAccountCache[V any](m map[uiCacheKey]V, accountID int64) {
	for k := range m {
		if k.accountID == accountID {
			delete(m, k)
		}
	}
}

// rcptShown is how many recipients a To/Cc line shows before collapsing the
// rest behind "+N more" (expanded in place by the shell script — no re-render,
// so the reader's scroll position survives).
const rcptShown = 3

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
			// Inline images (cid:) are rendered in the body, not offered as
			// downloadable chips.
			if a.ContentID != "" {
				continue
			}
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
		// A linked pair reads as one chip: the name opens the file, the trailing
		// button saves it somewhere.
		row := gtk.NewBox(gtk.OrientationHorizontal, 0)
		row.AddCSSClass("linked")

		open := gtk.NewButton()
		open.SetChild(attachmentChip(ta.att))
		open.SetTooltipText(ta.att.MimeType)
		open.ConnectClicked(func() { w.openAttachment(ta.accountID, ta.gmailID, ta.att.ID) })
		row.Append(open)

		save := gtk.NewButtonFromIconName("document-save-symbolic")
		save.SetTooltipText("Save as…")
		save.ConnectClicked(func() { w.saveAttachment(ta.accountID, ta.gmailID, ta.att) })
		row.Append(save)

		w.attachBox.Append(row)
	}
	w.attachBox.SetVisible(len(atts) > 0)
}

func (w *window) openAttachment(accountID int64, gmailID string, attID int64) {
	if w.deps.OpenAttach == nil {
		return
	}
	logging.Trace("ui: open attachment", "account", accountID, "id", gmailID, "attID", attID)
	go func() {
		path, err := w.deps.OpenAttach(context.Background(), accountID, gmailID, attID)
		if err != nil {
			slog.Warn("ui: open attachment", "id", gmailID, "err", err)
			dispatch.Main(func() { w.toast("Couldn't download attachment") })
			return
		}
		logging.Trace("ui: open attachment ready", "id", gmailID, "path", path)
		openExternal(path)
	}()
}

// saveAttachment downloads the attachment into the cache (reusing it when
// already fetched) then prompts for a destination and copies it there.
func (w *window) saveAttachment(accountID int64, gmailID string, att model.Attachment) {
	if w.deps.OpenAttach == nil {
		return
	}
	logging.Trace("ui: save attachment", "account", accountID, "id", gmailID, "attID", att.ID)
	go func() {
		src, err := w.deps.OpenAttach(context.Background(), accountID, gmailID, att.ID)
		if err != nil {
			slog.Warn("ui: save attachment: download", "id", gmailID, "err", err)
			dispatch.Main(func() { w.toast("Couldn't download attachment") })
			return
		}
		dispatch.Main(func() {
			dialog := gtk.NewFileDialog()
			dialog.SetTitle("Save attachment")
			if att.Filename != "" {
				dialog.SetInitialName(att.Filename)
			}
			dialog.Save(context.Background(), &w.win.Window, func(res gio.AsyncResulter) {
				file, err := dialog.SaveFinish(res)
				if err != nil || file == nil {
					return // cancelled
				}
				dst := file.Path()
				go func() {
					if err := copyFile(src, dst); err != nil {
						slog.Warn("ui: save attachment: copy", "dst", dst, "err", err)
						dispatch.Main(func() { w.toast("Couldn't save attachment") })
						return
					}
					logging.Trace("ui: save attachment done", "id", gmailID, "dst", dst)
					dispatch.Main(func() { w.toast("Saved " + filepath.Base(dst)) })
				}()
			})
		})
	}()
}

// copyFile streams src to dst, overwriting dst if it exists.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
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
	if a.SizeBytes > 0 {
		size := gtk.NewLabel(humanBytes(a.SizeBytes))
		size.AddCSSClass("dim-label")
		size.AddCSSClass("caption")
		box.Append(size)
	}
	return box
}

func (w *window) onArchive() {
	logging.Trace("ui: archive", "thread", w.openThreadID, "account", w.activeID)
	w.removeFromList("Archived", nil, []string{model.LabelInbox})
}

func (w *window) onTrash() {
	logging.Trace("ui: trash", "thread", w.openThreadID, "account", w.activeID)
	w.removeFromList("Moved to Trash", []string{model.LabelTrash}, []string{model.LabelInbox})
}

// onMoveToInbox restores the open conversation to the inbox (adding INBOX and
// clearing TRASH) — for un-archiving or recovering from Trash.
func (w *window) onMoveToInbox() {
	if len(w.openThreadMsgs) == 0 {
		return
	}
	logging.Trace("ui: move to inbox", "thread", w.openThreadID, "account", w.activeID)
	w.applyLabels(w.openThreadMsgs, []string{model.LabelInbox}, []string{model.LabelTrash}, nil)
	w.toast("Moved to Inbox")
}

// onReportSpam moves the open conversation to Spam (and out of the inbox).
func (w *window) onReportSpam() {
	logging.Trace("ui: report spam", "thread", w.openThreadID, "account", w.activeID)
	w.removeFromList("Reported spam", []string{model.LabelSpam}, []string{model.LabelInbox})
}

// onNotSpam takes the open conversation out of Spam and back to the inbox.
func (w *window) onNotSpam() {
	logging.Trace("ui: not spam", "thread", w.openThreadID, "account", w.activeID)
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
	logging.Trace("ui: empty folder requested", "label", label, "account", w.activeID)
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
			logging.Trace("ui: empty folder cancelled", "label", label)
			return
		}
		logging.Trace("ui: empty folder confirmed", "label", label, "account", acctID)
		go func() {
			n, err := w.deps.EmptyFolder(context.Background(), acctID, label)
			logging.Trace("ui: empty folder done", "label", label, "deleted", n, "err", err)
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
	logging.Trace("ui: delete forever requested", "thread", w.openThreadID, "n", len(w.openThreadMsgs), "account", w.activeID)
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
			logging.Trace("ui: delete forever cancelled", "thread", w.openThreadID)
			return
		}
		ids := make([]string, len(msgs))
		for i, m := range msgs {
			ids[i] = m.GmailID
		}
		acctID := w.activeID
		logging.Trace("ui: delete forever confirmed", "n", len(ids), "account", acctID)
		go func() {
			err := w.deps.DeleteForever(context.Background(), acctID, ids)
			logging.Trace("ui: delete forever done", "n", len(ids), "err", err)
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

// onMarkUnread marks the conversation unread by marking only its newest
// message: one unread message flips the thread's unread state in the list
// (same as the row menu's Mark as unread), while marking every message would
// inflate the unread counts.
func (w *window) onMarkUnread() {
	if w.openMsg.GmailID != "" {
		logging.Trace("ui: mark unread", "id", w.openMsg.GmailID, "thread", w.openThreadID, "account", w.activeID)
		w.applyLabels([]model.Message{w.openMsg}, []string{model.LabelUnread}, nil, nil)
	}
}

// showLabelsDialog opens the label chooser for the current conversation.
func (w *window) showLabelsDialog() {
	if w.openThreadID == "" {
		return
	}
	acct := w.activeID
	if len(w.openThreadMsgs) > 0 {
		acct = w.openThreadMsgs[0].AccountID
	}
	w.showThreadLabelsDialog(acct, w.openThreadID)
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
	add("reader-reply-all", w.onReplyAll)
	add("reader-forward", w.onForward)
	add("reader-unread", w.onMarkUnread)
	add("reader-move-inbox", w.onMoveToInbox)
	add("reader-report-spam", w.onReportSpam)
	add("reader-not-spam", w.onNotSpam)
	add("reader-trash", w.onTrash)
	add("reader-delete-forever", w.onDeleteForever)
	add("reader-labels", w.showLabelsDialog)
	add("reader-unsubscribe", w.onUnsubscribe)
	add("reader-print", w.onPrint)
	add("reader-retry", w.onRetryLoading)

	w.starAction = gio.NewSimpleActionStateful("reader-star", nil, glib.NewVariantBoolean(false))
	w.starAction.ConnectChangeState(func(v *glib.Variant) {
		w.starAction.SetState(v)
		w.setStarred(v.Boolean())
	})
	w.win.AddAction(w.starAction)

}

// buildReaderMenuModel builds the overflow menu — conversation-scoped actions
// only: star/unread/move/spam/trash, labels, unsubscribe, print, and retry.
// (Reply all, Reply, Forward, Archive, Translate and
// Draft reply are dedicated header controls; message-scoped actions live in
// each message's ⋯ menu — showMessageMenu — and sender actions in the
// sender-name dialog — showSenderActions.) Unlabeled sections render as native
// separators.
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

		if !w.isIMAPAccount(w.activeID) {
			lbl := gio.NewMenu()
			lbl.Append("Labels…", "win.reader-labels")
			menu.AppendSection("", lbl)
		}
	}
	// Unsubscribe is about the conversation's mailing list, so it stays here
	// (Gmail keeps it similarly prominent); the sender utilities live in the
	// sender-name dialog (showSenderActions).
	if w.openMsg.ListUnsubscribe != "" {
		sec := gio.NewMenu()
		sec.Append("Unsubscribe", "win.reader-unsubscribe")
		menu.AppendSection("", sec)
	}
	util := gio.NewMenu()
	util.Append("Print…", "win.reader-print")
	menu.AppendSection("", util)
	if w.lastFetchFailed {
		retry := gio.NewMenu()
		retry.Append("Retry loading", "win.reader-retry")
		menu.AppendSection("", retry)
	}

	return menu
}

// searchFrom shows all mail from an address using a Gmail server-side search
// ("from:addr"), so it finds messages beyond the local cache too.
func (w *window) searchFrom(addr string) {
	q := "from:" + strings.TrimSpace(addr)
	logging.Trace("ui: find from sender", "query", q, "account", w.activeID)
	w.suppressSearch = true
	w.searchEntry.SetText(q)
	w.suppressSearch = false
	if w.canSearchServer() {
		w.onSearchAllMail()
	} else {
		w.refreshList(q)
	}
}

// formatRawHeaders pretty-prints the stored header blob for the dialog. The
// Gmail path stores the bare Authentication-Results value (no header name, with
// the wire's whitespace runs), which reads best labeled with one verdict clause
// per line; proper "Name: value" header lines are unfolded and kept, with
// whitespace runs collapsed.
func formatRawHeaders(raw string) string {
	raw = strings.TrimSpace(raw)
	first, _, _ := strings.Cut(raw, "\n")
	if name, _, ok := strings.Cut(first, ":"); ok && !strings.ContainsAny(strings.TrimSpace(name), " \t(") {
		// Named header lines: join folded continuations, collapse runs.
		var out []string
		for _, l := range strings.Split(raw, "\n") {
			folded := strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")
			l = strings.Join(strings.Fields(l), " ")
			if folded && len(out) > 0 {
				out[len(out)-1] += " " + l
				continue
			}
			out = append(out, l)
		}
		return strings.Join(out, "\n")
	}
	// Bare Authentication-Results value: label it, one ";" clause per line.
	var clauses []string
	for _, cl := range strings.Split(raw, ";") {
		if cl = strings.Join(strings.Fields(cl), " "); cl != "" {
			clauses = append(clauses, "  "+cl)
		}
	}
	return "Authentication-Results:\n" + strings.Join(clauses, ";\n")
}

// ensureBody returns m's stored body, fetching it on demand (bounded) when the
// cache has none — a message of a long thread may never have been opened, so
// View headers / phishing analysis can't rely on a cached body row. Blocking;
// run off the main thread.
func (w *window) ensureBody(ctx context.Context, m model.Message) model.MessageBody {
	body, err := w.deps.Store.GetBody(ctx, m.RowID)
	if err == nil && (body.RawHeaders != "" || body.HTML != "" || body.Text != "") {
		return body
	}
	if w.deps.FetchBody == nil {
		return body
	}
	logging.Trace("ui: ensure body fetch", "id", m.GmailID, "account", m.AccountID)
	fetchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := w.deps.FetchBody(fetchCtx, m.AccountID, m.GmailID); err != nil {
		slog.Warn("ui: ensure body fetch", "id", m.GmailID, "err", err)
		return body
	}
	body, _ = w.deps.Store.GetBody(ctx, m.RowID)
	return body
}

// viewMessageHeaders shows a message's raw stored headers — any message of the
// open thread, via its per-message ⋯ menu. The store read (and possible
// on-demand body fetch) runs off the main thread.
func (w *window) viewMessageHeaders(m model.Message) {
	if m.GmailID == "" {
		return
	}
	logging.Trace("ui: view headers", "id", m.GmailID, "account", m.AccountID)
	threadID := w.openThreadID
	go func() {
		body := w.ensureBody(context.Background(), m)
		headers := strings.TrimSpace(body.RawHeaders)
		if headers == "" && w.deps.FetchBody != nil {
			// A body cached before header capture has no stored headers — one
			// refetch picks them up (the same self-heal the inline-image path
			// uses).
			logging.Trace("ui: view headers refetch", "id", m.GmailID)
			fetchCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			if err := w.deps.FetchBody(fetchCtx, m.AccountID, m.GmailID); err != nil {
				slog.Warn("ui: view headers refetch", "id", m.GmailID, "err", err)
			} else if b2, err := w.deps.Store.GetBody(context.Background(), m.RowID); err == nil {
				headers = strings.TrimSpace(b2.RawHeaders)
			}
			cancel()
		}
		dispatch.Main(func() {
			if w.openThreadID != threadID {
				logging.Trace("ui: view headers discarded", "id", m.GmailID, "openThread", w.openThreadID)
				return // the user moved on — don't pop a dialog over another thread
			}
			if headers == "" {
				// The bounded fetch may have failed offline, or a malformed provider
				// response may not contain a usable header block.
				logging.Trace("ui: view headers empty", "id", m.GmailID)
				w.toast("No headers are available for this message")
				return
			}
			w.showHeadersDialog(headers)
		})
	}()
}

// showHeadersDialog shows raw headers in a scrollable monospace dialog.
// Main thread only.
func (w *window) showHeadersDialog(headers string) {
	logging.Trace("ui: show headers dialog", "bytes", len(headers))

	tv := gtk.NewTextView()
	tv.SetEditable(false)
	tv.SetCursorVisible(false)
	tv.SetMonospace(true)
	tv.SetWrapMode(gtk.WrapWordChar)
	setMargins(tv, 12, 12, 12, 12)
	tv.Buffer().SetText(formatRawHeaders(headers))

	scroller := gtk.NewScrolledWindow()
	scroller.SetPolicy(gtk.PolicyAutomatic, gtk.PolicyAutomatic)
	scroller.SetChild(tv)
	scroller.SetVExpand(true)

	toolbar := adw.NewToolbarView()
	toolbar.AddTopBar(adw.NewHeaderBar())
	toolbar.SetContent(scroller)

	dialog := adw.NewDialog()
	dialog.SetTitle("Message Headers")
	dialog.SetContentWidth(640)
	dialog.SetContentHeight(560)
	dialog.SetChild(toolbar)
	dialog.Present(w.win)
}

// onPrint runs a WebKit print operation on the reader webview (its native print
// dialog), so the open conversation prints exactly as rendered.
func (w *window) onPrint() {
	if w.webview == nil {
		return
	}
	logging.Trace("ui: print", "thread", w.openThreadID, "account", w.activeID)
	op := webkit.NewPrintOperation(w.webview)
	op.RunDialog(&w.win.Window)
}

// showSenderActions presents the sender utilities for the message with this
// provider id in the open conversation — reached by clicking the sender in a
// message header.
func (w *window) showSenderActions(gmailID string) {
	var m model.Message
	for _, om := range w.openThreadMsgs {
		if om.GmailID == gmailID {
			m = om
			break
		}
	}
	if m.GmailID == "" {
		return
	}
	addr := strings.TrimSpace(m.FromAddr)
	logging.Trace("ui: sender actions", "id", gmailID, "addr", addr)

	w.addressActionsDialog(addr, func(item func(label string, fn func())) {
		if w.canSearchServer() {
			item("Find emails from "+displayFrom(m), func() { w.searchFrom(addr) })
		}
	})
}

// showRecipientActions presents the address card for a recipient in a message
// header (To/Cc) — the same surface the sender name opens. token is the RFC 5322 form
// carried by the mbaction:rcpt link ("Name <addr>" or a bare address).
func (w *window) showRecipientActions(token string) {
	addr, name := strings.TrimSpace(token), ""
	if p, err := mail.ParseAddress(token); err == nil {
		addr, name = strings.TrimSpace(p.Address), p.Name
	}
	if addr == "" {
		return
	}
	logging.Trace("ui: recipient actions", "addr", addr, "name", name)

	w.addressActionsDialog(addr, func(item func(label string, fn func())) {
		who := name
		if who == "" {
			who = addr
		}
		if w.canSearchServer() {
			item("Find emails from "+who, func() { w.searchFrom(addr) })
		}
		if w.deps.Send != nil {
			item("New message to this address", func() {
				w.openCompose(model.OutgoingMessage{To: formatContact(model.Contact{Name: name, Address: addr})}, "", "New message")
			})
		}
	})
}

// addressActionsDialog is the shared scaffold of the sender/recipient address
// cards: a small boxed-list dialog titled with the address, opening with a
// "Copy address" row; build appends the surface-specific rows via item (each
// row closes the dialog before acting).
func (w *window) addressActionsDialog(addr string, build func(item func(label string, fn func()))) {
	list := gtk.NewListBox()
	list.AddCSSClass("boxed-list")
	list.SetSelectionMode(gtk.SelectionNone)
	dialog := adw.NewDialog()
	item := func(label string, fn func()) {
		lbl := gtk.NewLabel(label)
		lbl.SetXAlign(0)
		lbl.SetHExpand(true)
		b := gtk.NewButton()
		b.SetChild(lbl)
		b.AddCSSClass("flat")
		b.ConnectClicked(func() {
			dialog.Close()
			fn()
		})
		list.Append(b)
	}
	item("Copy address", func() {
		if disp := gdk.DisplayGetDefault(); disp != nil {
			disp.Clipboard().SetText(addr)
			w.toast("Copied " + addr)
		}
	})
	build(item)

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	setMargins(box, 12, 12, 6, 12)
	box.Append(list)
	tv := adw.NewToolbarView()
	tv.AddTopBar(adw.NewHeaderBar())
	tv.SetContent(box)
	dialog.SetTitle(addr)
	dialog.SetContentWidth(420)
	dialog.SetChild(tv)
	dialog.Present(w.win)
}

// onTranslate shows an English translation of the whole open conversation in
// place, preserving each message's markup. Every message is translated and
// cached per message id, so re-opening, reverting, or re-translating reuses the
// cached result (and an already-translated message in the thread isn't redone).
func (w *window) onTranslate() {
	if w.deps.Assistant == nil || !w.aiTranslate || len(w.openThreadMsgs) == 0 {
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
		if _, ok := w.translationCache[cacheKey(m.AccountID, m.GmailID)]; !ok {
			todo = append(todo, m)
		}
	}
	logging.Trace("ui: translate", "thread", threadID, "msgs", len(msgs), "todo", len(todo), "account", acctID)
	if len(todo) == 0 { // whole thread already translated → show instantly
		logging.Trace("ui: translate cache hit (memory)", "thread", threadID)
		w.showTranslatedConversation(msgs)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.translateCancel = cancel
	w.translationBanner.SetTitle("Translating…")
	w.translationBanner.SetRevealed(true)
	// Keep the original showing while translating (the banner says "Translating…");
	// setReaderHTML swaps to the translation in place when it's ready.
	done := w.aiActivity("Translating conversation")

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
		logging.Trace("ui: translate seeded from cache", "seeded", len(seeded), "remaining", len(remaining), "account", acctID)

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

		logging.Trace("ui: translate done", "thread", threadID, "translated", len(results), "err", firstErr)
		dispatch.Main(func() {
			done(doneErrCtx(ctx, firstErr)) // a user cancel is neutral for AI health
			if w.openThreadID != threadID || ctx.Err() != nil {
				logging.Trace("ui: translate discarded", "thread", threadID, "openThread", w.openThreadID, "cancelled", ctx.Err() != nil)
				return // user switched conversations or reverted
			}
			if firstErr != nil {
				w.setReaderHTML("<p>Translation failed: " + html.EscapeString(firstErr.Error()) + "</p>")
				return
			}
			for id, out := range seeded {
				w.translationCache[cacheKey(acctID, id)] = out
			}
			for id, out := range results {
				w.translationCache[cacheKey(acctID, id)] = out
			}
			capCache(w.translationCache, aiCacheCap)
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
	w.translationShown = true
	var b strings.Builder
	blocked := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		body := model.MessageBody{HTML: w.translationCache[cacheKey(m.AccountID, m.GmailID)]}
		// No gist card here: the gist is in the email's original language, which
		// would clash with the translated bodies this view exists to show.
		head, rest, n := w.conversationSection(m, body, w.cleanHTML, false, "")
		// The translated view is a flat stack: every message is shown in full,
		// so none of them folds.
		b.WriteString(composeSection(head, rest, false))
		blocked += n
	}
	w.setTrackerCount(blocked)
	// cid: images resolve via w.inlineByCID, already populated by the original
	// render of this thread (serveCID).
	w.setReaderHTML(b.String())
}

// bodyHTMLFor returns the message's HTML body (sanitized), falling back to its
// text or snippet wrapped as HTML. Used for translation and as the QuoteHTML a
// reply/forward compose embeds in the outgoing HTML alternative.
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
	logging.Trace("ui: show original", "thread", w.openThreadID)
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
	w.translationShown = false
	w.translationBanner.SetRevealed(false)
}

// buildSummaryCard creates the (initially hidden) AI thread-summary card shown
// at the top of the reader: a title row with a close button and the streamed
// summary below. Returns the revealer wrapping it.
func (w *window) buildSummaryCard() *gtk.Revealer {
	w.cardIcon = gtk.NewImageFromIconName("summarize-symbolic")
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
	a11yLabel(closeBtn, "Hide summary")
	closeBtn.ConnectClicked(w.hideSummary)

	titleRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
	titleRow.Append(w.cardIcon)
	titleRow.Append(w.cardTitle)
	titleRow.Append(closeBtn)

	w.summaryLabel = gtk.NewLabel("")
	w.summaryLabel.SetXAlign(0)
	w.summaryLabel.SetWrap(true)
	w.summaryLabel.SetWrapMode(pango.WrapWordChar)
	w.summaryLabel.SetSelectable(true)

	card := gtk.NewBox(gtk.OrientationVertical, 6)
	card.AddCSSClass("summary-card")
	setMargins(card, 6, 6, 6, 6)
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
	if len(w.openThreadMsgs) == 0 || w.deps.Assistant == nil || !w.aiSummarize {
		return
	}
	if w.summaryCancel != nil { // cancel a summary still streaming
		w.summaryCancel()
		w.summaryCancel = nil
	}
	w.cardIcon.SetFromIconName("view-list-bullet-symbolic")
	w.cardTitle.SetText("Summary")
	key := w.summaryKey()
	memoryKey := w.activeCacheKey(key)
	logging.Trace("ui: summarize", "thread", w.openThreadID, "account", w.activeID)
	w.summaryRevealer.SetRevealChild(true)
	if cached, ok := w.summaryCache[memoryKey]; ok {
		logging.Trace("ui: summarize cache hit (memory)", "thread", w.openThreadID)
		w.summaryLabel.SetText(cached)
		return
	}
	// Persisted summary for this exact message set (no AI cost). The stored
	// fingerprint is the same key, so a thread that gained a reply misses and is
	// re-summarized. A single indexed lookup, fine on the main thread.
	if fp, sum, ok, err := w.deps.Store.ThreadSummary(context.Background(), w.activeID, w.openThreadID); err == nil && ok && fp == key {
		logging.Trace("ui: summarize cache hit (persisted)", "thread", w.openThreadID)
		w.summaryCache[memoryKey] = sum
		capCache(w.summaryCache, aiCacheCap)
		w.summaryLabel.SetText(sum)
		return
	}
	logging.Trace("ui: summarize cache miss → AI", "thread", w.openThreadID)

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
				done(doneErrCtx(ctx, err)) // a user cancel is neutral for AI health
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
			done(doneErrCtx(ctx, serr)) // a user cancel is neutral for AI health
			if w.openThreadID != threadID || ctx.Err() != nil {
				return
			}
			if serr != nil {
				w.summaryLabel.SetText("Summary failed: " + serr.Error())
				return
			}
			if final != "" {
				w.summaryCache[cacheKey(acctID, key)] = final
				capCache(w.summaryCache, aiCacheCap)
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

// analyzeMessage runs an on-demand AI phishing/scam analysis of one message —
// any message of the open thread, via its per-message ⋯ menu — and streams the
// verdict + reasons into the shared card. It feeds the AI the deterministic
// signals (auth result, heuristic warnings) alongside the content, and caches
// by message id so re-running is instant.
func (w *window) analyzeMessage(m model.Message) {
	if m.GmailID == "" || w.deps.Assistant == nil || !w.aiPhishing {
		return
	}
	if w.summaryCancel != nil {
		w.summaryCancel()
		w.summaryCancel = nil
	}
	w.cardIcon.SetFromIconName("security-high-symbolic")
	// The card is shared with the thread summary — name the target when it
	// isn't the newest message, so it's clear which message was analyzed.
	title := "Security analysis"
	if m.GmailID != w.openMsg.GmailID {
		title += " — " + displayFrom(m)
	}
	w.cardTitle.SetText(title)
	logging.Trace("ui: analyze phishing", "id", m.GmailID, "thread", w.openThreadID, "account", w.activeID)
	w.summaryRevealer.SetRevealChild(true)
	key := cacheKey(m.AccountID, "analyze:"+m.GmailID)
	if cached, ok := w.summaryCache[key]; ok {
		logging.Trace("ui: analyze cache hit (memory)", "id", m.GmailID)
		w.summaryLabel.SetText(cached)
		return
	}
	// Persisted analysis for this message (no AI cost). The message + its signals
	// are immutable, so a stored analysis is always valid. A single indexed
	// lookup, fine on the main thread.
	if a, ok, err := w.deps.Store.Analysis(context.Background(), w.activeID, m.GmailID); err == nil && ok {
		logging.Trace("ui: analyze cache hit (persisted)", "id", m.GmailID)
		w.summaryCache[key] = a
		capCache(w.summaryCache, aiCacheCap)
		w.summaryLabel.SetText(a)
		return
	}
	logging.Trace("ui: analyze cache miss → AI", "id", m.GmailID)

	w.summaryLabel.SetText("Analyzing…")
	ctx, cancel := context.WithCancel(context.Background())
	w.summaryCancel = cancel
	threadID := w.openThreadID
	acctID := w.activeID
	gmailID := m.GmailID
	done := w.aiActivity("Checking for phishing")

	go func() {
		// Assemble the context off the main thread: it reads the stored body and
		// may fetch it on demand for a message that was never opened.
		emailCtx := w.analysisContextFor(ctx, m)
		ch, err := w.deps.Assistant.AnalyzeEmail(ctx, emailCtx)
		if err != nil {
			msg := err.Error()
			dispatch.Main(func() {
				done(doneErrCtx(ctx, err)) // a user cancel is neutral for AI health
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
			done(doneErrCtx(ctx, serr)) // a user cancel is neutral for AI health
			if w.openThreadID != threadID || ctx.Err() != nil {
				return
			}
			if serr != nil {
				w.summaryLabel.SetText("Analysis failed: " + serr.Error())
				return
			}
			if final != "" {
				w.summaryCache[key] = final
				capCache(w.summaryCache, aiCacheCap)
				w.summaryLabel.SetText(final)
			}
		})
	}()
}

// analysisContextFor assembles the email plus deterministic signals (auth
// verdict, heuristic warnings) as plain text for the AI analyzer. Blocking
// (may fetch the body on demand); run off the main thread.
func (w *window) analysisContextFor(ctx context.Context, m model.Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From name: %s\nFrom address: %s\nSubject: %s\n", m.FromName, m.FromAddr, m.Subject)
	body := w.ensureBody(ctx, m)
	if v := parseAuthResults(body.RawHeaders); v.level != authUnknown {
		fmt.Fprintf(&b, "Mail-server authentication check: %s (%s)\n", authLevelWord(v.level), v.detail)
	}
	for _, warn := range phishingWarnings(m, body.HTML) {
		fmt.Fprintf(&b, "Automated warning: %s\n", warn)
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

// htmlToText renders HTML as readable plain text: tags stripped with block
// boundaries becoming newlines, entities decoded, and — unlike a bare tag
// strip — link targets preserved as "text (url)", so a quoted link still leads
// somewhere. Safe on input that is already plain text.
func htmlToText(s string) string {
	doc, err := xhtml.Parse(strings.NewReader(s))
	if err != nil {
		// Fallback: strip tags without structure (the historical behavior).
		return collapseBlank(html.UnescapeString(bluemonday.StrictPolicy().Sanitize(s)))
	}
	var b strings.Builder
	renderText(&b, doc)
	return collapseBlank(b.String())
}

// blockTags are elements whose end implies a line break in a text rendering.
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "tr": true, "li": true, "ul": true,
	"ol": true, "table": true, "blockquote": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "pre": true, "hr": true,
}

// renderText walks a parsed HTML tree appending its visible text, newlines at
// block boundaries, and "(url)" after links whose text isn't the URL itself.
func renderText(b *strings.Builder, n *xhtml.Node) {
	switch n.Type {
	case xhtml.TextNode:
		b.WriteString(n.Data)
		return
	case xhtml.ElementNode:
		switch n.Data {
		case "script", "style", "head", "title":
			return
		case "br", "hr":
			b.WriteString("\n")
			return
		}
	}
	start := b.Len()
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderText(b, c)
	}
	if n.Type != xhtml.ElementNode {
		return
	}
	if n.Data == "a" {
		if href := attrVal(n, "href"); strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
			if text := strings.TrimSpace(b.String()[start:]); text != "" && text != href {
				fmt.Fprintf(b, " (%s)", href)
			}
		}
	}
	if blockTags[n.Data] {
		b.WriteString("\n")
	}
}

// attrVal returns the value of the node's named attribute, or "".
func attrVal(n *xhtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// collapseBlank trims the text and collapses runs of blank lines (which tag
// removal tends to leave behind) to one.
func collapseBlank(text string) string {
	// Whitespace-only lines count as blank for collapsing.
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = ""
		}
	}
	text = strings.Join(lines, "\n")
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
	if slices.Contains(add, model.LabelUnread) {
		// An explicit mark-unread (reader toggle, row menu, or undoing a
		// mark-read): remember it so the self-sent auto-clear in checkNewMail
		// doesn't revert it when the change echoes back from the provider.
		for _, m := range msgs {
			w.userUnread[selfUnreadKey(m.AccountID, m.GmailID)] = true
		}
	}
	accountID := msgs[0].AccountID
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.GmailID
	}
	logging.Trace("ui: apply labels", "n", len(ids), "add", add, "remove", remove, "account", accountID)
	go func() {
		start := time.Now()
		mirrorErr := w.deps.ModifyLabels(context.Background(), accountID, ids, add, remove)
		if mirrorErr != nil {
			slog.Warn("ui: apply labels", "n", len(ids), "err", mirrorErr)
		}
		slog.Debug("ui: applyLabels", "n", len(ids), "dur", time.Since(start))
		dispatch.Main(func() {
			t := time.Now()
			// The local change already applied optimistically; only warn (briefly,
			// non-alarming) that the provider mirror may not have gone through.
			if mirrorErr != nil {
				w.toast("Change may not have synced")
			}
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
func (w *window) removeFromList(verb string, add, remove []string) {
	msgs := w.openThreadMsgs
	if w.deps.ModifyLabels == nil || len(msgs) == 0 {
		return
	}
	pos := w.threadSel.Selected()
	w.applyLabels(msgs, add, remove, func() { w.advanceSelection(pos) })
	w.showUndoToast(verb, msgs, add, remove)
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
	// Never land on a date group header: prefer the next conversation below,
	// else the nearest above.
	p := int(pos)
	for p < int(n) && isDateHeader(w.threadModel.String(uint(p))) {
		p++
	}
	if p >= int(n) {
		p = int(pos)
		for p >= 0 && isDateHeader(w.threadModel.String(uint(p))) {
			p--
		}
	}
	if p < 0 {
		w.clearReader()
		return
	}
	// The list was just spliced, so the selection is currently invalid; setting it
	// fires the selection-changed handler, which opens the conversation.
	w.threadSel.SetSelected(uint(p))
}

// toast shows a transient message over the window. Safe to call from the main
// thread only (like all widget access).
func (w *window) toast(msg string) {
	if w.toastOverlay == nil {
		return
	}
	w.toastOverlay.AddToast(newToast(msg))
}

// newToast builds a toast whose title is literal text. libadwaita parses a
// toast title as Pango markup by default, so a subject, sender or filename
// containing "&" — "Summit & more", "Ben & Jerry's" — fails to parse and the
// toast appears with no text at all, just its buttons. Nothing the app puts in
// a toast is ever markup, so the parse is switched off rather than every call
// site being made to escape.
func newToast(text string) *adw.Toast {
	t := adw.NewToast(text)
	t.SetUseMarkup(false)
	return t
}

// defaultSendUndoDelay is how long a sent message is held (with an Undo toast)
// before it actually goes out, unless Preferences says otherwise.
const defaultSendUndoDelay = 5 * time.Second

// sendUndoDelay returns the configured undo-send window (Preferences →
// Sending), falling back to the default.
func (w *window) sendUndoDelay() time.Duration {
	if w.sendUndoSecs > 0 {
		return time.Duration(w.sendUndoSecs) * time.Second
	}
	return defaultSendUndoDelay
}

// deferSend makes a send crash-safe: it persists the message to the outbox
// immediately with a not_before watermark one undo-window ahead (so a quit or
// crash within the window can no longer lose it), shows an "Undo" toast, and —
// if the user doesn't undo — sweeps the outbox once the window elapses so the
// message goes out promptly. Undo deletes the queued row and reopens the message
// in compose. A recipient/MIME/DB error at enqueue time is surfaced with the
// content reopened intact, never silently dropped. The compose window has
// already closed, so this runs at the main-window level.
func (w *window) deferSend(accountID int64, msg model.OutgoingMessage) {
	logging.Trace("ui: defer send", "account", accountID, "to", msg.To, "cc", msg.Cc, "subject", msg.Subject)
	notBefore := time.Now().Add(w.sendUndoDelay()).Unix()
	go func() {
		id, err := w.deps.EnqueueSend(context.Background(), accountID, msg, notBefore)
		logging.Trace("ui: enqueue send done", "account", accountID, "id", id, "err", err)
		dispatch.Main(func() {
			if err != nil {
				w.reopenUnsent(accountID, msg, err)
				return
			}
			w.showSendUndoToast(accountID, id, msg)
		})
	}()
}

// showSendUndoToast presents the "Sending…"/Undo toast for a message already
// queued in the outbox (id). If the undo window elapses without an undo, it
// sweeps the outbox to deliver the message. Undo discards the queued row and
// reopens the message in compose so it isn't lost.
func (w *window) showSendUndoToast(accountID, outboxID int64, msg model.OutgoingMessage) {
	cancelled := false
	toast := newToast("Sending…")
	toast.SetButtonLabel("Undo")
	toast.SetTimeout(0) // we control the lifetime via the timer below
	toast.ConnectButtonClicked(func() {
		logging.Trace("ui: undo send", "account", accountID, "id", outboxID, "subject", msg.Subject)
		cancelled = true
		toast.Dismiss()
		go func() {
			ok, err := w.deps.DiscardOutbox(context.Background(), accountID, outboxID)
			if err != nil {
				slog.Warn("ui: undo send discard", "id", outboxID, "err", err)
			}
			dispatch.Main(func() {
				// A sweep (the 45s background ticker, or an Outbox "send now")
				// can claim the row while the toast is still up. When the cancel
				// lost that race, say so — reopening the compose would present
				// delivered content as unsent and invite a duplicate send.
				if !ok && err == nil {
					logging.Trace("ui: undo send too late", "account", accountID, "id", outboxID)
					w.toast("Too late to undo — the message was already sent")
					return
				}
				// Reopen the message exactly as it was (no second signature),
				// from the account it was being sent from, and already "dirty" —
				// its content is user-authored, so closing it must prompt rather
				// than silently discard. On a discard error the row is still
				// queued; reopening keeps the content in front of the user.
				w.openComposeOpts(msg, "", "Message", composeOpts{fromAccountID: accountID, startDirty: true})
			})
		}()
	})
	w.toastOverlay.AddToast(toast)

	go func() {
		// A small margin past not_before guarantees the item is sweepable.
		time.Sleep(w.sendUndoDelay() + 250*time.Millisecond)
		dispatch.Main(func() {
			if cancelled {
				return
			}
			toast.Dismiss()
			if w.deps.SweepOutbox == nil {
				return
			}
			go func() {
				if err := w.deps.SweepOutbox(context.Background(), accountID); err != nil {
					slog.Warn("ui: send sweep", "account", accountID, "err", err)
				}
				dispatch.Main(w.refreshOutbox)
			}()
		})
	}()
}

// reopenUnsent puts a message that could not be queued back in front of the user
// (its content is user-authored and was stored nowhere) and explains why, so it
// is never silently dropped.
func (w *window) reopenUnsent(accountID int64, msg model.OutgoingMessage, cause error) {
	slog.Warn("ui: send not queued", "err", cause)
	logging.Trace("ui: send enqueue failed, reopening compose", "account", accountID, "subject", msg.Subject, "err", cause)
	w.openComposeOpts(msg, "", "Message", composeOpts{fromAccountID: accountID, startDirty: true})
	alert := adw.NewAlertDialog("Message not sent",
		"Sending failed: "+cause.Error()+"\n\nThe message could not be queued, so it has been "+
			"reopened — try again or save it as a draft.")
	alert.AddResponse("ok", "OK")
	alert.SetDefaultResponse("ok")
	alert.SetCloseResponse("ok")
	alert.Present(w.win)
}

// showUndoToast presents an undo toast that reverses the add/remove applied to
// msgs (re-adding what was removed and vice versa).
func (w *window) showUndoToast(verb string, msgs []model.Message, add, remove []string) {
	if w.toastOverlay == nil {
		return
	}
	// Coalesce a burst of same-kind changes (rapid j/e triage) into ONE toast
	// whose Undo reverses the whole burst. ToastOverlay queues toasts one at a
	// time, so 80 individual archives would otherwise drain one 6-second
	// "Archived" toast at a time for minutes — each reversing only its own,
	// unidentified conversation. A different-kind change starts a fresh burst.
	if w.undoToast != nil && w.undoVerb == verb &&
		slices.Equal(w.undoAdd, add) && slices.Equal(w.undoRemove, remove) {
		w.undoMsgs = append(w.undoMsgs, msgs...)
		old := w.undoToast
		w.undoToast = nil // detach before Dismiss so its dismissed-handler keeps the burst
		old.Dismiss()
	} else {
		w.undoMsgs = append([]model.Message(nil), msgs...)
		w.undoVerb, w.undoAdd, w.undoRemove = verb, add, remove
	}
	burst := w.undoMsgs
	t := newToast(undoTitle(verb, burst))
	t.SetButtonLabel("Undo")
	t.SetTimeout(6)
	t.ConnectButtonClicked(func() {
		logging.Trace("ui: undo", "verb", verb, "n", len(burst), "add", add, "remove", remove)
		w.applyLabels(burst, remove, add, nil) // swap to reverse the change
	})
	t.ConnectDismissed(func() {
		// The burst ends when its toast leaves the screen (timeout, close, or
		// undo) — not when a coalescing replacement dismisses the old toast
		// (w.undoToast already points elsewhere then).
		if w.undoToast == t {
			w.undoToast, w.undoMsgs = nil, nil
		}
	})
	w.undoToast = t
	w.toastOverlay.AddToast(t)
}

// undoTitle names an undo toast: one conversation is identified by its subject
// ("don't know which email the Undo applies to" is worse than a long toast), a
// burst by its conversation count.
func undoTitle(verb string, msgs []model.Message) string {
	threads := map[string]bool{}
	for _, m := range msgs {
		threads[m.ThreadID] = true
	}
	if len(threads) > 1 {
		return fmt.Sprintf("%s %d conversations", verb, len(threads))
	}
	subject := strings.TrimSpace(msgs[len(msgs)-1].Subject)
	if subject == "" {
		return verb
	}
	if r := []rune(subject); len(r) > 45 {
		subject = string(r[:44]) + "…"
	}
	return verb + " “" + subject + "”"
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
	logging.Trace("ui: sync change", "kind", c.Kind, "account", c.AccountID, "id", c.GmailID, "thread", c.ThreadID, "active", w.activeID)
	switch c.Kind {
	case syncer.MessageUpserted, syncer.MessageBodyFetched, syncer.MessageDeleted:
		w.invalidateSection(c.AccountID, c.GmailID) // a re-synced message must re-render
		if w.ownBodyFetch(c) {
			// The render that asked for this body reads it directly, and the row
			// it belongs to is unchanged (bodies aren't listed). Acting on the
			// echo would cancel that render and reload the list for nothing.
			logging.Trace("ui: own body fetch echo ignored", "id", c.GmailID, "thread", c.ThreadID)
			return
		}
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
	case syncer.AIUpdated:
		// The background worker persisted new inbox categories. For the active
		// account, re-seed the visible tags from the cache; another account's
		// tags are simply ready when it is switched to.
		if c.AccountID == w.activeID {
			w.categorizeInbox()
		}
	case syncer.SendStateChanged:
		w.refreshOutbox() // the banner counts every account's outbox
	case syncer.SnoozeWoke:
		// A snoozed conversation returned to the inbox: refresh the list (its
		// absence from snoozes un-hides it) and raise a reminder notification.
		if c.AccountID == w.activeID {
			w.scheduleRefresh(true)
		} else {
			w.refreshAccountUnread()
		}
		w.notifySnoozeWoke(c.AccountID, c.ThreadID)
	case syncer.AuthExpired:
		// The account's sign-in expired/was revoked; surface it (it won't recover
		// without re-login) and name the account so multi-account users know which.
		email, acctType := "", ""
		for _, a := range w.deps.Accounts {
			if a.ID == c.AccountID {
				email, acctType = a.Email, a.Type
				break
			}
		}
		// How old was the credential? Recorded at sign-in (connected.json), so an
		// expiry names its age instead of looking arbitrary — a Google OAuth app
		// left in "Testing" publishing status expires sign-ins after 7 days, and
		// the age is what makes that pattern recognizable.
		age, days := "", -1
		if email != "" {
			if t, ok := loadConnectedTime(email); ok {
				days = int(time.Since(t).Hours() / 24)
				age = signInAgePhrase(t, time.Now())
			}
		}
		logging.Trace("ui: auth expired banner", "account", c.AccountID, "email", email, "type", acctType, "signInDays", days)
		w.authExpiredID = c.AccountID // the Reconnect button re-authenticates this one
		switch {
		case email != "" && age != "":
			w.authBanner.SetTitle("Sign-in expired for " + email + " (signed in " + age + ") — reconnect to keep syncing")
		case email != "":
			w.authBanner.SetTitle("Sign-in expired for " + email + " — reconnect to keep syncing")
		default:
			w.authBanner.SetTitle("An account's sign-in expired — reconnect to keep syncing")
		}
		w.authBanner.SetRevealed(true)
		// One activity-log row per expiry (the sync loop republishes AuthExpired
		// on every failed pass), carrying the age and — when the age fits the
		// weekly pattern on a Google account — the likely cause.
		if !w.authReported[c.AccountID] {
			w.authReported[c.AccountID] = true
			note := ""
			if age != "" {
				note = "signed in " + age
				if acctType == model.AccountGmail && days >= 6 && days <= 8 {
					note += " — Google apps in Testing mode expire sign-ins after 7 days; publish the app to Production to stop this"
				}
			}
			w.deps.Activity.Report("mail", email, "Sign-in expired", note)
		}
	}
	if c.Kind != syncer.MessageUpserted || c.GmailID == "" {
		return
	}
	w.queueNewMailCheck(c.AccountID, c.GmailID)
}

// ownBodyFetch reports whether c is the echo of a body fetch the in-flight
// conversation render started itself (see renderFetching). Main thread only.
func (w *window) ownBodyFetch(c syncer.Change) bool {
	return c.Kind == syncer.MessageBodyFetched && c.GmailID != "" &&
		w.renderFetching[cacheKey(c.AccountID, c.GmailID)]
}

// notifyCandidate is one upserted message queued for the new-mail
// notification check.
type notifyCandidate struct {
	accountID int64
	gmailID   string
	// userMarked snapshots userUnread on the main thread before the check runs
	// off it: the user explicitly marked this message unread, so the self-sent
	// auto-clear must leave it alone.
	userMarked bool
}

// selfUnreadKey keys userUnread entries across accounts.
func selfUnreadKey(accountID int64, gmailID string) string {
	return fmt.Sprintf("%d/%s", accountID, gmailID)
}

// notifyCoalesceMS is how long queued notification checks wait for a burst of
// sync events to settle before one background lookup handles them all.
const notifyCoalesceMS = 300

// queueNewMailCheck collects a MessageUpserted id for the new-mail notification
// decision. A sync burst upserts many messages; instead of a per-event
// GetMessage on the GTK loop, ids collect for notifyCoalesceMS and one
// goroutine looks the batch up. Main thread only.
func (w *window) queueNewMailCheck(accountID int64, gmailID string) {
	w.notifyQueue = append(w.notifyQueue, notifyCandidate{accountID: accountID, gmailID: gmailID})
	if w.notifyScheduled {
		return
	}
	w.notifyScheduled = true
	glib.TimeoutAdd(notifyCoalesceMS, func() bool {
		batch := w.notifyQueue
		w.notifyQueue = nil
		w.notifyScheduled = false
		for i := range batch {
			batch[i].userMarked = w.userUnread[selfUnreadKey(batch[i].accountID, batch[i].gmailID)]
		}
		logging.Trace("ui: new-mail check batch", "n", len(batch))
		go w.checkNewMail(batch)
		return false
	})
}

// checkNewMail looks up the batched message ids off the main thread and raises
// a desktop notification for each genuinely new unread inbox message (arrived
// after launch). Only the notification dispatch touches the main thread.
func (w *window) checkNewMail(batch []notifyCandidate) {
	type hit struct {
		accountID int64
		msg       model.Message
	}
	ctx := context.Background()
	var hits []hit
	selfUnread := map[int64][]string{}
	for _, c := range batch {
		m, err := w.deps.Store.GetMessage(ctx, c.accountID, c.gmailID)
		if err != nil {
			continue
		}
		notify, clearUnread := newMailDisposition(m, w.startTime, w.isOwnAddress(m.FromAddr), c.userMarked)
		if clearUnread {
			// A message this app sent (a reply, or self-addressed mail) —
			// Gmail can legitimately label its own sent copy INBOX+UNREAD, but
			// it isn't new mail to look at, so don't notify. When the copy is
			// genuinely the user's outgoing message (SENT — not spoofed From),
			// the unread state came from the provider merging a looped-back
			// copy (e.g. a recipient alias forwarding into this mailbox), so
			// clear it too — unless the user explicitly marked it unread.
			logging.Trace("ui: new-mail check skip self-sent", "id", m.GmailID, "from", m.FromAddr, "user_marked", c.userMarked)
			selfUnread[c.accountID] = append(selfUnread[c.accountID], m.GmailID)
		}
		if notify {
			hits = append(hits, hit{accountID: c.accountID, msg: m})
		}
	}
	if w.deps.ModifyLabels != nil {
		for accountID, ids := range selfUnread {
			// Optimistic local change + provider mirror, so the thread stops
			// rendering bold here, on other machines, and on the phone.
			logging.Trace("ui: clear self-sent unread", "account", accountID, "n", len(ids))
			if err := w.deps.ModifyLabels(ctx, accountID, ids, nil, []string{model.LabelUnread}); err != nil {
				slog.Warn("ui: clear self-sent unread", "n", len(ids), "err", err)
			}
		}
	}
	logging.Trace("ui: new-mail check done", "batch", len(batch), "notify", len(hits), "self_unread", len(selfUnread))
	if len(hits) == 0 {
		return
	}
	// A gist may already be persisted (e.g. inbox categorization ran first) —
	// look it up now, off the main thread, so the very first notification can
	// show the same AI summary the reader would, instead of the raw snippet.
	gists := map[uiCacheKey]string{}
	if w.aiGist {
		byAccount := map[int64][]string{}
		for _, h := range hits {
			byAccount[h.accountID] = append(byAccount[h.accountID], h.msg.GmailID)
		}
		for accountID, ids := range byAccount {
			if g, err := w.deps.Store.MessageGists(ctx, accountID, ids); err == nil {
				for id, gist := range g {
					gists[cacheKey(accountID, id)] = gist
				}
			}
		}
	}
	dispatch.Main(func() {
		for _, h := range hits {
			key := cacheKey(h.accountID, h.msg.GmailID)
			if !w.claimNotification(key) {
				continue
			}
			w.notifyNewMail(h.accountID, h.msg, gists[key])
		}
	})
}

func newMailDisposition(m model.Message, started time.Time, ownAddress, userMarked bool) (notify, clearUnread bool) {
	if !m.IsUnread || !m.InternalDate.After(started) {
		return false, false
	}
	if ownAddress && hasLabel(m, model.LabelSent) {
		return false, !userMarked
	}
	return hasLabel(m, model.LabelInbox), false
}

func (w *window) claimNotification(key uiCacheKey) bool {
	if w.notified == nil {
		w.notified = map[uiCacheKey]bool{}
	}
	if w.notified[key] {
		return false
	}
	w.notified[key] = true
	return true
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
		logging.Trace("ui: schedule refresh coalesced", "withList", withList)
		return
	}
	w.refreshPending = true
	dispatch.Main(func() {
		w.refreshPending = false
		list := w.refreshListPending
		thread := w.refreshThreadPending
		w.refreshListPending = false
		w.refreshThreadPending = false
		logging.Trace("ui: refresh (coalesced)", "list", list, "thread", thread, "account", w.activeID)
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
	logging.Trace("ui: refresh open thread", "thread", w.openThreadID, "account", w.activeID)
	msgs, err := w.deps.Store.ListThreadMessages(context.Background(), w.activeID, w.openThreadID)
	if err != nil || len(msgs) == 0 {
		return
	}
	w.openThreadMsgs = msgs
	w.openMsg = msgs[len(msgs)-1] // newest, for reply/forward/star/unread
	w.rerenderOpenThread()        // keeps a shown translation shown
}

func (w *window) notifyNewMail(accountID int64, m model.Message, gist string) {
	logging.Trace("ui: notify new mail", "account", accountID, "id", m.GmailID, "from", m.FromAddr, "subject", m.Subject, "has_gist", gist != "")
	// Unique id per message so concurrent accounts' notifications don't replace
	// one another.
	id := fmt.Sprintf("mailbox-mail-%d-%s", accountID, m.GmailID)
	if w.app == nil {
		return
	}

	// Delivery is immediate. A persisted gist can enrich it for free; generating
	// one never delays the only notification the user receives.
	w.app.SendNotification(id, w.mailNotification(accountID, m, gist))
}

// mailNotification builds the new-mail notification. Sender and subject form
// the title; the body is the snippet until the AI's one-sentence gist replaces
// it. The gist must be the FIRST body line: GNOME's collapsed banner shows only
// the title plus one body line, so a summary on a second line is invisible
// until the notification is expanded in the tray — which is exactly where a
// glanceable gist is useless.
func (w *window) mailNotification(accountID int64, m model.Message, gist string) *gio.Notification {
	title := displayFrom(m)
	if m.Subject != "" {
		title += " — " + m.Subject
	}
	n := gio.NewNotification(title)
	body := strings.TrimSpace(m.Snippet)
	if gist != "" {
		body = gist
	}
	if body == "" {
		body = "New mail"
	}
	n.SetBody(body)
	// Clicking the notification opens this message; the buttons act on it without
	// opening (see registerActions).
	target := glib.NewVariantString(fmt.Sprintf("%d|%s", accountID, m.GmailID))
	n.SetDefaultAction(gio.ActionPrintDetailedName("app.open-message", target))
	if w.deps.ModifyLabels != nil {
		n.AddButton("Archive", gio.ActionPrintDetailedName("app.notify-archive", target))
	}
	if w.deps.Send != nil {
		n.AddButton("Reply", gio.ActionPrintDetailedName("app.notify-reply", target))
	}
	return n
}

// snoozedSummaries lists the account's snoozed conversations (soonest wake
// first) as thread summaries — the Snoozed virtual folder's content.
func (w *window) snoozedSummaries(ctx context.Context, acct int64) ([]model.ThreadSummary, error) {
	sns, err := w.deps.Store.SnoozedThreads(ctx, acct)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(sns))
	until := make(map[string]int64, len(sns))
	for i, sn := range sns {
		ids[i] = sn.ThreadID
		until[sn.ThreadID] = sn.Until
	}
	sums, err := w.deps.Store.GetThreadSummaries(ctx, acct, ids)
	if err != nil {
		return nil, err
	}
	// Rows in the Snoozed view show when the thread comes back, not when its
	// last message arrived.
	for i := range sums {
		sums[i].SnoozedUntil = until[sums[i].ThreadID]
	}
	return sums, nil
}

// snoozePreset is one quick snooze choice offered by the row menu.
type snoozePreset struct {
	label string
	t     time.Time
}

// snoozePresets returns the quick wake times: later today, tomorrow morning,
// and next week's Monday morning.
func snoozePresets(now time.Time) []snoozePreset {
	tomorrow := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	daysToMonday := (int(time.Monday) - int(now.Weekday()) + 7) % 7
	if daysToMonday == 0 {
		daysToMonday = 7
	}
	monday := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, now.Location()).AddDate(0, 0, daysToMonday)
	return []snoozePreset{
		{"Later today", now.Add(4 * time.Hour)},
		{"Tomorrow morning", tomorrow},
		{"Next week", monday},
	}
}

// formatWakeTime renders a snooze wake time at the precision its distance
// needs: weekday+time within the week, date+time within the year, full date
// beyond — "until Tue 09:00" would be a lie for a September snooze.
func formatWakeTime(t, now time.Time) string {
	switch {
	case t.Sub(now) < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	case t.Year() == now.Year():
		return t.Format("Mon, Jan 2 15:04")
	default:
		return t.Format("Jan 2, 2006 15:04")
	}
}

// snoozeThread hides a conversation until t: through the mirror-aware hook
// when wired (the provider labels carry the snooze to other clients and
// machines), else the local-only row (read-only cache mode).
func (w *window) snoozeThread(ctx context.Context, acctID int64, threadID string, t time.Time) error {
	if w.deps.Snooze != nil {
		return w.deps.Snooze(ctx, acctID, threadID, t)
	}
	return w.deps.Store.SnoozeThread(ctx, acctID, threadID, t.Unix())
}

// unsnoozeThread is snoozeThread's inverse — same routing.
func (w *window) unsnoozeThread(ctx context.Context, acctID int64, threadID string) error {
	if w.deps.Unsnooze != nil {
		return w.deps.Unsnooze(ctx, acctID, threadID)
	}
	return w.deps.Store.UnsnoozeThread(ctx, acctID, threadID)
}

// snoozeUntil hides a conversation until t — here instantly, and everywhere
// else via the provider label mirror (any machine wakes it on schedule).
func (w *window) snoozeUntil(acctID int64, threadID string, t time.Time) {
	logging.Trace("ui: snooze", "account", acctID, "thread", threadID, "until", t.Unix())
	go func() {
		if err := w.snoozeThread(context.Background(), acctID, threadID, t); err != nil {
			slog.Warn("ui: snooze thread", "thread", threadID, "err", err)
			return
		}
		dispatch.Main(func() {
			title := "Snoozed until " + formatWakeTime(t, time.Now())
			if w.isIMAPAccount(acctID) {
				title = "Snoozed on this device until " + formatWakeTime(t, time.Now())
			}
			toast := newToast(title)
			toast.SetButtonLabel("Undo")
			toast.SetTimeout(6)
			toast.ConnectButtonClicked(func() { w.unsnooze(acctID, threadID) })
			w.toastOverlay.AddToast(toast)
			w.refreshList(w.searchEntry.Text())
			w.loadLabels() // the Snoozed badge counts one more
		})
	}()
}

// unsnooze wakes a conversation now, everywhere.
func (w *window) unsnooze(acctID int64, threadID string) {
	logging.Trace("ui: unsnooze", "account", acctID, "thread", threadID)
	go func() {
		if err := w.unsnoozeThread(context.Background(), acctID, threadID); err != nil {
			slog.Warn("ui: unsnooze thread", "thread", threadID, "err", err)
			return
		}
		dispatch.Main(func() {
			w.toast("Snooze removed")
			w.refreshList(w.searchEntry.Text())
			w.loadLabels() // the Snoozed badge counts one fewer
		})
	}()
}

// notifySnoozeWoke raises the reminder notification for a woken conversation.
func (w *window) notifySnoozeWoke(accountID int64, threadID string) {
	go func() {
		sum, err := w.deps.Store.GetThreadSummary(context.Background(), accountID, threadID)
		if err != nil {
			logging.Trace("ui: notify snooze woke skipped", "thread", threadID, "err", err)
			return
		}
		dispatch.Main(func() {
			logging.Trace("ui: notify snooze woke", "account", accountID, "thread", threadID, "subject", sum.Latest.Subject)
			n := gio.NewNotification("Reminder")
			body := displayFrom(sum.Latest)
			if sum.Latest.Subject != "" {
				body += " — " + sum.Latest.Subject
			}
			n.SetBody(body)
			target := glib.NewVariantString(fmt.Sprintf("%d|%s", accountID, sum.Latest.GmailID))
			n.SetDefaultAction(gio.ActionPrintDetailedName("app.open-message", target))
			w.app.SendNotification("mailbox-snooze-"+threadID, n)
		})
	}()
}

// folderRow builds a sidebar row: a leading symbolic icon, the folder name, and
// an unread-count badge. When there are unread messages the name is emphasized,
// like a standard mail client.
func folderRow(icon, name string, unread int, neutral bool) *gtk.Box {
	box := gtk.NewBox(gtk.OrientationHorizontal, 8)
	setMargins(box, 12, 12, 6, 6)
	if icon != "" {
		box.Append(gtk.NewImageFromIconName(icon))
	}
	n := gtk.NewLabel(name)
	n.SetXAlign(0)
	n.SetHExpand(true)
	n.SetEllipsize(pango.EllipsizeEnd)
	if unread > 0 && !neutral {
		n.AddCSSClass("heading") // unread demands attention; a neutral count doesn't
	}
	box.Append(n)
	if unread > 0 {
		badge := countBadge(unread)
		if neutral {
			badge.AddCSSClass("neutral")
		}
		box.Append(badge)
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

// a11yLabel gives an icon-only control an accessible name so assistive
// technologies announce its purpose (a symbolic icon carries no text of its
// own; tooltips are not reliably exposed as names).
func a11yLabel(w gtk.Widgetter, name string) {
	gtk.BaseWidget(w).UpdateProperty(
		[]gtk.AccessibleProperty{gtk.AccessiblePropertyLabel},
		[]coreglib.Value{*coreglib.NewValue(name)},
	)
}
