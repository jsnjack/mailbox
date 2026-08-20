// Thread list: the middle pane. Loading a page of conversations (label, all
// mail, snoozed, local search, provider search), keeping exactly one query in
// flight, turning the result into rows with the fewest possible splices, and
// the list-level actions (selection mode, bulk apply, category tags).
package ui

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

const threadPageSize = 100

type threadPageMode uint8

const (
	threadPageLabel threadPageMode = iota + 1
	threadPageAll
	threadPageSnoozed
	threadPageLocalSearch
	threadPageServerSearch
)

type threadPageKey struct {
	mode      threadPageMode
	accountID int64
	label     string
	query     string
	newest    bool
}

// threadPageState is the conversation page the list currently shows: the query
// it answers, its rows, and what a continuation would need to ask for next.
type threadPageState struct {
	key          threadPageKey
	sums         []model.ThreadSummary
	cursor       *store.ThreadPageCursor
	searchOffset int
	serverToken  string
	serverIDs    []string
	hasMore      bool
}

// threadLoad is the one list query in flight; the zero value means the list is
// idle. Holding the running query in a single value is what makes its rules
// legible: exactly one runs at a time, startThreadLoad cancels whatever it
// replaces, and a refresh for the query already running sets reload rather than
// starting a second one or cancelling the first (see coalesceThreadLoad).
type threadLoad struct {
	key    threadPageKey
	gen    uint64             // stamp; a result whose gen is no longer current is dropped
	more   bool               // continuation of the loaded page rather than a refresh of it
	cancel context.CancelFunc // non-nil exactly while the query is running
	reload bool               // a refresh arrived mid-flight: re-run once this lands
}

// threadLoadFailure is the last query that failed, which is what the in-place
// Retry footer offers to re-issue. Nil once anything succeeds.
type threadLoadFailure struct {
	more  bool
	retry func()
}

type threadPageResult struct {
	sums         []model.ThreadSummary
	cursor       *store.ThreadPageCursor
	searchOffset int
	serverToken  string
	serverIDs    []string
	hasMore      bool
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
		if isDateHeader(id) {
			w.rowSig[id] = id
			li.SetChild(dateHeaderRow(strings.TrimPrefix(id, dateHdrPrefix)))
			li.SetSelectable(false)
			li.SetActivatable(false)
			return
		}
		// Recycled items may have been a header last bind; restore defaults.
		li.SetSelectable(true)
		li.SetActivatable(true)
		outgoing := w.current == model.LabelSent || w.current == model.LabelDraft
		// Keep the signature cache in step with what is actually on screen, so a
		// scroll-recycled row never looks "unchanged" to the next diff.
		w.rowSig[id] = w.renderSig(id)
		cat := w.threadCats[w.activeCacheKey(id)]
		row := threadRow(w.threadByID[id], outgoing, cat.tag, cat.manual, cat.failed)
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

	w.threadScroll = gtk.NewScrolledWindow()
	w.threadScroll.SetVExpand(true)
	w.threadScroll.SetHExpand(true)
	w.threadScroll.SetChild(w.threadView)
	w.threadScroll.VAdjustment().ConnectValueChanged(w.maybeLoadNextThreadPage)

	w.emptyPage = adw.NewStatusPage()
	w.emptyPage.SetIconName("mail-unread-symbolic")
	w.emptyPage.SetTitle("No messages")
	w.emptyPage.SetDescription("This folder has no messages in the local cache.")

	w.threadStack = gtk.NewStack()
	w.threadStack.SetVExpand(true)
	w.threadStack.AddNamed(w.threadScroll, "list")
	w.threadStack.AddNamed(w.emptyPage, "empty")
	w.threadStack.SetVisibleChildName("list")

	w.pageSpinner = adw.NewSpinner()
	w.pageSpinner.SetVisible(false)
	w.pageLabel = gtk.NewLabel("")
	w.pageLabel.AddCSSClass("dim-label")
	w.pageRetry = gtk.NewButtonWithLabel("Retry")
	w.pageRetry.AddCSSClass("flat")
	w.pageRetry.SetVisible(false)
	w.pageRetry.ConnectClicked(func() {
		if f := w.threadFailed; f != nil && f.retry != nil {
			f.retry()
		}
	})
	w.pageStatus = gtk.NewBox(gtk.OrientationHorizontal, 8)
	w.pageStatus.SetHAlign(gtk.AlignCenter)
	setMargins(w.pageStatus, 6, 6, 6, 6)
	w.pageStatus.Append(w.pageSpinner)
	w.pageStatus.Append(w.pageLabel)
	w.pageStatus.Append(w.pageRetry)
	w.pageStatus.SetVisible(false)

	w.searchEntry = gtk.NewSearchEntry()
	w.searchEntry.SetPlaceholderText("Search")
	w.searchEntry.SetHExpand(true)
	w.searchEntry.ConnectSearchChanged(w.onSearchChanged)
	// Esc in the search entry (stop-search) clears it, returning the list to the
	// current label (the cleared text fires search-changed → refreshList("")).
	w.searchEntry.ConnectStopSearch(func() {
		if w.searchEntry.Text() == "" {
			return
		}
		logging.Trace("ui: search cleared via Esc")
		w.searchEntry.SetText("")
	})
	w.searchSort = gtk.NewDropDownFromStrings([]string{"Relevant", "Newest"})
	w.searchSort.SetTooltipText("Search result order")
	w.searchSort.SetVisible(false)
	w.searchSort.Connect("notify::selected", func() {
		if strings.TrimSpace(w.searchEntry.Text()) != "" {
			w.loadThreadsFor(w.searchEntry.Text())
		}
	})
	w.searchAllBtn = gtk.NewButtonFromIconName("edit-find-symbolic")
	w.searchAllBtn.SetTooltipText("Search all mail on the provider")
	a11yLabel(w.searchAllBtn, "Search all mail on the provider")
	w.searchAllBtn.AddCSSClass("flat")
	w.searchAllBtn.SetVisible(false)
	w.searchAllBtn.ConnectClicked(w.onSearchAllMail)
	searchBar := gtk.NewBox(gtk.OrientationHorizontal, 4)
	setMargins(searchBar, 6, 6, 6, 6)
	searchBar.Append(w.searchEntry)
	searchBar.Append(w.searchSort)
	searchBar.Append(w.searchAllBtn)

	// Banner titles name folders and accounts, which can contain "&" (a label
	// called "R&D"): parsed as markup they would render empty. See newToast.
	w.outboxBanner = adw.NewBanner("")
	w.outboxBanner.SetUseMarkup(false)
	w.outboxBanner.SetButtonLabel("Outbox")
	w.outboxBanner.SetRevealed(false)
	w.outboxBanner.ConnectButtonClicked(w.openOutbox)

	// When no provider backend could be built the UI is read-only; say so instead of
	// leaving the actions silently inert. MAILBOX_DEMO hides it for screenshots
	// taken against a synthetic cache that has no provider backend by design.
	w.readOnlyBanner = adw.NewBanner("Read-only — no mail provider is connected")
	w.readOnlyBanner.SetUseMarkup(false)
	w.readOnlyBanner.SetButtonLabel("How to connect")
	w.readOnlyBanner.ConnectButtonClicked(w.showConnectHelp)
	w.readOnlyBanner.SetRevealed(w.deps.ModifyLabels == nil && os.Getenv("MAILBOX_DEMO") == "")

	w.buildSelectionBar()

	w.emptyFolderBanner = adw.NewBanner("")
	w.emptyFolderBanner.SetUseMarkup(false)
	w.emptyFolderBanner.SetButtonLabel("Empty now")
	w.emptyFolderBanner.SetRevealed(false)
	w.emptyFolderBanner.ConnectButtonClicked(w.onEmptyFolder)

	// Revealed when an account's refresh token is revoked/expired (a sync hit
	// invalid_grant): the account can't recover without re-login, so say so
	// instead of silently failing to sync.
	w.authBanner = adw.NewBanner("")
	w.authBanner.SetUseMarkup(false)
	w.authBanner.SetButtonLabel("Reconnect")
	w.authBanner.SetRevealed(false)
	w.authBanner.ConnectButtonClicked(w.onReconnect)

	content := gtk.NewBox(gtk.OrientationVertical, 0)
	content.Append(w.readOnlyBanner)
	content.Append(w.authBanner)
	content.Append(w.outboxBanner)
	content.Append(w.emptyFolderBanner)
	content.Append(searchBar)
	content.Append(w.selectionBar)
	content.Append(w.threadStack)
	content.Append(w.pageStatus)

	hb := adw.NewHeaderBar()
	hb.SetShowTitle(false) // "Messages" is redundant — the pane is self-evident

	// Infrequent list-scope actions (unread-only filter, mark-all-read) live in a
	// small overflow menu rather than cluttering the header. Rebuilt per open so
	// it reflects the current filter state and folder.
	w.listMenuBtn = gtk.NewMenuButton()
	w.listMenuBtn.SetIconName("view-more-symbolic")
	w.listMenuBtn.SetTooltipText("View options")
	a11yLabel(w.listMenuBtn, "View options")
	// Native menu model: a check item for the unread filter, mark-all-read where
	// it applies. Rebuilt per open (the folder gates mark-all-read), with the
	// toggle state synced first.
	w.listMenuBtn.SetCreatePopupFunc(func(btn *gtk.MenuButton) {
		w.unreadAction.SetState(glib.NewVariantBoolean(w.unreadOnly))
		menu := gtk.NewPopoverMenuFromModel(w.buildListMenuModel())
		btn.SetPopover(&menu.Popover)
	})
	hb.PackEnd(w.listMenuBtn)

	// Multi-select triage (only when label changes are possible).
	if w.deps.ModifyLabels != nil {
		w.selectBtn = gtk.NewToggleButton()
		w.selectBtn.SetIconName("selection-mode-symbolic")
		w.selectBtn.SetTooltipText("Select multiple")
		a11yLabel(w.selectBtn, "Select multiple")
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

	subs := gio.NewSimpleAction("list-subscriptions", nil)
	subs.ConnectActivate(func(*glib.Variant) { w.openSubscriptions() })
	w.win.AddAction(subs)
	// The per-row context-menu actions live in a dedicated group built per popup
	// in showRowMenu (parameter-less closures), not here — see the comment there.
}

// setThreadCategory manually assigns (or clears, when cat is empty) a
// conversation's category. It persists the choice keyed by the thread's latest
// message and pins categorizedMsg so the auto-categorizer won't override it —
// the manual fallback when the AI is unavailable, or to correct a misfire.
func (w *window) setThreadCategory(threadID, cat string) {
	t, ok := w.threadByID[threadID]
	if !ok {
		logging.Trace("ui: set thread category skipped", "thread", threadID, "reason", "thread not in map", "category", cat)
		return
	}
	msgID := t.Latest.GmailID
	acctID := w.activeID
	key := cacheKey(acctID, threadID)
	logging.Trace("ui: set thread category", "thread", threadID, "id", msgID, "category", cat, "account", acctID)
	// A manual decision resolves any pending "failed" state, and pins the thread
	// (categorizedMsg) so categorizeInbox leaves the choice alone. "None" clears
	// the override entirely, reverting to the default — which, for a thread you
	// replied to last, is the "Replied" tag.
	w.threadCats[key] = threadCategory{
		tag: cat, categorizedMsg: msgID, manual: cat != "", // a hand-picked category outranks "Replied"
	}
	w.refreshList(w.searchEntry.Text())
	go func() {
		var err error
		if cat == "" {
			err = w.deps.Store.ClearMessageCategory(context.Background(), acctID, msgID)
		} else {
			err = w.deps.Store.SetManualCategory(context.Background(), acctID, msgID, cat)
		}
		if err != nil {
			slog.Warn("ui: set thread category", "err", err)
		}
	}()
}

// recategorizeThread re-runs AI categorization for a single conversation: it
// drops the thread's cached tag (in memory + the persisted entry for its latest
// message) so the next pass re-classifies it, then triggers that pass.
func (w *window) recategorizeThread(threadID string) {
	t, ok := w.threadByID[threadID]
	if !ok || w.deps.Assistant == nil {
		logging.Trace("ui: recategorize thread skipped", "thread", threadID, "known", ok, "assistant", w.deps.Assistant != nil)
		return
	}
	msgID := t.Latest.GmailID
	logging.Trace("ui: recategorize thread", "thread", threadID, "id", msgID, "account", w.activeID)
	key := w.activeCacheKey(threadID)
	delete(w.threadCats, key) // re-running AI drops the tag and any manual override
	acctID := w.activeID
	go func() {
		if err := w.deps.Store.ClearMessageCategory(context.Background(), acctID, msgID); err != nil {
			slog.Warn("ui: clear message category", "id", msgID, "err", err)
		}
		// The persisted tag is gone; kick the background worker now — an
		// explicit user action lifts its post-failure cooldown ("try now"
		// beats backoff).
		if w.deps.RecategorizeInbox != nil {
			w.deps.RecategorizeInbox(acctID)
		}
		dispatch.Main(func() {
			if w.activeID != acctID {
				return
			}
			w.refreshList(w.searchEntry.Text()) // drop the stale tag until the worker re-tags
		})
	}()
}

// buildListMenuModel is the thread-list overflow menu model: the unread-only
// filter (a native check item) and, where it applies, mark-all-read. Rebuilt
// per open so it reflects the current folder.
func (w *window) buildListMenuModel() *gio.Menu {
	menu := gio.NewMenu()
	menu.Append("Show unread only", "win.list-unread-only")
	// "Mark all read" is meaningful per folder, but not for the All Mail view
	// (it spans every label and Gmail offers no such bulk op there).
	if w.deps.MarkAllRead != nil && w.current != allMailID && w.current != snoozedID {
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
	// Mailing-list housekeeping: every unsubscribe-capable sender in one place.
	subsSec := gio.NewMenu()
	subsSec.Append("Subscriptions…", "win.list-subscriptions")
	menu.AppendSection("", subsSec)
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
	logging.Trace("ui: recategorize inbox", "account", acctID)
	go func() {
		if err := w.deps.Store.ClearCategories(context.Background(), acctID); err != nil {
			slog.Warn("ui: clear categories", "err", err)
		}
		// Kick the background worker for a fresh classification pass — an
		// explicit user action lifts its post-failure cooldown ("try now"
		// beats backoff).
		if w.deps.RecategorizeInbox != nil {
			w.deps.RecategorizeInbox(acctID)
		}
		dispatch.Main(func() {
			if w.activeID != acctID {
				return // account switched while clearing
			}
			clearAccountCache(w.threadCats, acctID)
			// Drop the stale tags; the worker's AIUpdated events reveal the new
			// ones as they are classified.
			w.refreshList(w.searchEntry.Text())
		})
	}()
}

func (w *window) onMarkAllRead() {
	if w.deps.MarkAllRead == nil || w.current == allMailID || w.current == snoozedID {
		return
	}
	label := w.current
	acctID := w.activeID
	logging.Trace("ui: mark all read", "label", label, "account", acctID)
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
// mode: a count, select-all, the two headline actions (archive / trash), a ⋯
// menu holding the rest (snooze / mark read / move to), and cancel. The bar
// lives inside the thread-list pane, whose NavigationSplitView width cap
// (280–360px) loses to any child's minimum width — so the bar keeps few enough
// buttons (and an ellipsizing label) that its minimum always fits the pane
// instead of forcing it wider and shrinking the reader.
func (w *window) buildSelectionBar() {
	w.selectionLabel = gtk.NewLabel("0 selected")
	w.selectionLabel.SetXAlign(0)
	w.selectionLabel.SetHExpand(true)
	w.selectionLabel.SetEllipsize(pango.EllipsizeEnd)
	setMargins(w.selectionLabel, 10, 6, 0, 0)

	archive := gtk.NewButtonFromIconName("mail-archive-symbolic")
	archive.SetTooltipText("Archive selected")
	a11yLabel(archive, "Archive selected")
	archive.ConnectClicked(func() { w.bulkApply("Archived", nil, []string{model.LabelInbox}) })

	trash := gtk.NewButtonFromIconName("user-trash-symbolic")
	trash.SetTooltipText("Move selected to Trash")
	a11yLabel(trash, "Move selected to Trash")
	trash.ConnectClicked(func() { w.bulkApply("Trashed", []string{model.LabelTrash}, []string{model.LabelInbox}) })

	// The remaining bulk actions live behind a ⋯ menu (rowmenu pattern — see
	// showRowMenu for why hand-built flat buttons, not a GMenu model), so the
	// bar's minimum width stays under the pane's.
	more := gtk.NewButtonFromIconName("view-more-symbolic")
	more.SetTooltipText("More actions")
	a11yLabel(more, "More bulk actions")
	more.ConnectClicked(func() { w.showBulkMoreMenu(more) })

	cancel := gtk.NewButtonFromIconName("window-close-symbolic")
	cancel.AddCSSClass("flat")
	cancel.SetTooltipText("Cancel")
	a11yLabel(cancel, "Cancel selection")
	cancel.ConnectClicked(func() { w.selectBtn.SetActive(false) })

	w.selectionBar = gtk.NewBox(gtk.OrientationHorizontal, 6)
	w.selectionBar.AddCSSClass("toolbar")
	setMargins(w.selectionBar, 6, 6, 4, 4)
	w.selectionBar.Append(w.selectionLabel)
	w.selectionBar.Append(archive)
	w.selectionBar.Append(trash)
	w.selectionBar.Append(more)
	w.selectionBar.Append(cancel)
	w.selectionBar.SetVisible(false)
}

// showBulkMoreMenu opens the selection bar's ⋯ menu: the bulk actions that
// don't earn a top-level button — select all/none, snooze presets (a nested
// flyout), mark read, and move-to-label.
func (w *window) showBulkMoreMenu(parent gtk.Widgetter) {
	logging.Trace("ui: show bulk more menu", "selected", len(w.selected))
	pop := gtk.NewPopover()
	pop.SetParent(parent)
	pop.ConnectClosed(func() { pop.Unparent() })

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.AddCSSClass("menu")
	box.AddCSSClass("rowmenu")

	item := func(parent *gtk.Box, label string, fn func()) {
		lbl := gtk.NewLabel(label)
		lbl.SetXAlign(0)
		lbl.SetHExpand(true)
		b := gtk.NewButton()
		b.SetChild(lbl)
		b.AddCSSClass("flat")
		b.ConnectClicked(func() {
			logging.Trace("ui: bulk more menu action", "action", label, "selected", len(w.selected))
			pop.Popdown()
			fn()
		})
		parent.Append(b)
	}

	item(box, "Select all / none", func() {
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
	box.Append(gtk.NewSeparator(gtk.OrientationHorizontal))

	// Bulk snooze: a nested flyout of the quick presets, applied to every
	// selected conversation (Monday-morning triage in one sweep).
	snPop := gtk.NewPopover()
	snPop.SetHasArrow(false)
	snPop.SetPosition(gtk.PosRight)
	snBox := gtk.NewBox(gtk.OrientationVertical, 0)
	snBox.AddCSSClass("menu")
	snBox.AddCSSClass("rowmenu")
	for _, p := range snoozePresets(time.Now()) {
		p := p
		item(snBox, p.label+" ("+p.t.Format("Mon 15:04")+")", func() {
			snPop.Popdown()
			w.bulkSnooze(p.t)
		})
	}
	snPop.SetChild(snBox)
	snLbl := gtk.NewLabel("Snooze")
	snLbl.SetXAlign(0)
	snLbl.SetHExpand(true)
	snRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
	snRow.Append(snLbl)
	snRow.Append(gtk.NewImageFromIconName("pan-end-symbolic"))
	snBtn := gtk.NewButton()
	snBtn.SetChild(snRow)
	snBtn.AddCSSClass("flat")
	snBtn.ConnectClicked(func() {
		snPop.SetParent(snBtn)
		snPop.Popup()
	})
	snPop.ConnectClosed(func() { snPop.Unparent() })
	box.Append(snBtn)

	item(box, "Mark read", func() { w.bulkApply("Marked read", nil, []string{model.LabelUnread}) })
	item(box, "Move to…", func() {
		if len(w.selected) == 0 {
			return
		}
		// Capture the account owning the selection now: a mid-dialog account
		// switch must not offer another account's labels (their ids are
		// per-account and wouldn't exist on the selected messages' account).
		w.showMoveToDialog(w.activeID, func(labelID, name string) {
			w.bulkApply("Moved to "+name, []string{labelID}, moveLocationRemovals)
		})
	})

	pop.SetChild(box)
	pop.Popup()
}

// setSelectMode enters/leaves multi-select triage, re-binding the list so rows
// show or hide their checkboxes.
func (w *window) setSelectMode(on bool) {
	if w.selectMode == on {
		return
	}
	logging.Trace("ui: select mode", "on", on)
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

// bulkSnooze hides every selected conversation until t (mirrored to the
// provider like any snooze), then exits selection mode and refreshes.
func (w *window) bulkSnooze(t time.Time) {
	if len(w.selected) == 0 {
		return
	}
	ids := make([]string, 0, len(w.selected))
	for id := range w.selected {
		ids = append(ids, id)
	}
	acctID := w.activeID
	logging.Trace("ui: bulk snooze", "n", len(ids), "until", t.Unix(), "account", acctID)
	w.selectBtn.SetActive(false) // exits select mode (clears selection, refreshes)
	go func() {
		ctx := context.Background()
		for _, id := range ids {
			if err := w.snoozeThread(ctx, acctID, id, t); err != nil {
				slog.Warn("ui: bulk snooze", "thread", id, "err", err)
			}
		}
		dispatch.Main(func() {
			toast := newToast(fmt.Sprintf("Snoozed %d conversations until %s", len(ids), formatWakeTime(t, time.Now())))
			toast.SetButtonLabel("Undo")
			toast.SetTimeout(6)
			toast.ConnectButtonClicked(func() {
				go func() {
					for _, id := range ids {
						if err := w.unsnoozeThread(context.Background(), acctID, id); err != nil {
							slog.Warn("ui: bulk unsnooze", "thread", id, "err", err)
						}
					}
					dispatch.Main(func() {
						w.refreshList(w.searchEntry.Text())
						w.loadLabels()
					})
				}()
			})
			w.toastOverlay.AddToast(toast)
			w.refreshList(w.searchEntry.Text())
			w.loadLabels()
		})
	}()
}

// bulkApply applies a label change to every selected conversation in one batch,
// then leaves selection mode. The per-thread message resolution (one query per
// selected thread) runs off the main thread; the label change and the undo
// toast dispatch back once resolved.
func (w *window) bulkApply(verb string, add, remove []string) {
	if len(w.selected) == 0 {
		return
	}
	logging.Trace("ui: bulk apply", "verb", verb, "selected", len(w.selected), "add", add, "remove", remove, "account", w.activeID)
	ids := make([]string, 0, len(w.selected))
	for id := range w.selected {
		ids = append(ids, id)
	}
	acctID := w.activeID
	w.selectBtn.SetActive(false) // exits select mode (clears selection, refreshes)
	go func() {
		ctx := context.Background()
		var msgs []model.Message
		n := 0
		for _, id := range ids {
			if tm, err := w.deps.Store.ListThreadMessages(ctx, acctID, id); err == nil && len(tm) > 0 {
				msgs = append(msgs, tm...)
				n++
			}
		}
		logging.Trace("ui: bulk apply resolved", "verb", verb, "threads", n, "messages", len(msgs))
		if len(msgs) == 0 {
			return
		}
		dispatch.Main(func() {
			w.applyLabels(msgs, add, remove, nil)
			w.showUndoToast(verb, msgs, add, remove)
		})
	}()
}

// onSearchAllMail runs a provider-side search for the current query, caches the
// matches, and shows them — finding mail beyond the local cache.
func (w *window) onSearchAllMail() {
	q := strings.TrimSpace(w.searchEntry.Text())
	if q == "" || !w.canSearchServer() {
		return
	}
	logging.Trace("ui: search all mail", "query", q, "account", w.activeID)
	w.serverSearch = true // stay in server-search mode across refreshes
	w.runServerSearch(q)
}

func (w *window) canSearchServer() bool {
	return w.deps.SearchPage != nil || w.deps.SearchServer != nil
}

// runServerSearch starts a fresh provider-side search. Further pages are loaded
// only as the list approaches its bottom; a live sync refreshes the summaries
// already present without restarting the provider query.
func (w *window) runServerSearch(q string) {
	if q == "" || !w.canSearchServer() {
		logging.Trace("ui: run server search skipped", "query", q, "hasSearch", w.canSearchServer())
		return
	}
	logging.Trace("ui: run server search", "query", q, "account", w.activeID)
	w.serverQuery = q
	w.emptyPage.SetChild(nil)
	w.emptyPage.SetIconName("edit-find-symbolic")
	w.emptyPage.SetTitle("Searching all mail…")
	w.emptyPage.SetDescription("")
	w.searchAllBtn.SetSensitive(false)
	acctID := w.activeID
	key := threadPageKey{mode: threadPageServerSearch, accountID: acctID, query: q, newest: w.searchSort.Selected() == 1}
	w.startThreadLoad(key, false, func() { w.runServerSearch(q) }, func(ctx context.Context) (threadPageResult, error) {
		ids, next, err := w.fetchServerSearchPage(ctx, acctID, q, "")
		if err != nil {
			return threadPageResult{}, err
		}
		sums, err := w.serverSearchSummaries(ctx, acctID, ids, key.newest)
		return threadPageResult{sums: sums, serverToken: next, serverIDs: ids, hasMore: next != ""}, err
	}, func(err error) {
		w.searchAllBtn.SetSensitive(true)
		slog.Warn("ui: search all mail", "err", err)
		w.toast("Couldn't search all mail — showing cached results")
		w.serverSearch, w.serverQuery = false, ""
		w.refreshList(q)
	})
}

func (w *window) fetchServerSearchPage(ctx context.Context, acctID int64, q, token string) ([]string, string, error) {
	if w.deps.SearchPage != nil {
		page, err := w.deps.SearchPage(ctx, acctID, q, token, threadPageSize)
		return page.IDs, page.Next, err
	}
	if token != "" || w.deps.SearchServer == nil {
		return nil, "", nil
	}
	ids, err := w.deps.SearchServer(ctx, acctID, q, threadPageSize)
	return ids, "", err
}

// serverSearchSummaries maps provider message ids to conversations with two
// batched store reads. Sorting by newest message preserves the list's existing
// search behavior while the provider cursor remains independent and opaque.
func (w *window) serverSearchSummaries(ctx context.Context, acctID int64, ids []string, newest bool) ([]model.ThreadSummary, error) {
	idToThread, err := w.deps.Store.ThreadIDsForMessages(ctx, acctID, ids)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(ids))
	tids := make([]string, 0, len(ids))
	for _, id := range ids { // de-duplicate provider hits before one batched summary read
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
	if newest {
		sortThreadSummariesNewest(sums)
	}
	return sums, nil
}

func (w *window) onSearchChanged() {
	if w.suppressSearch {
		return
	}
	q := strings.TrimSpace(w.searchEntry.Text())
	logging.Trace("ui: search changed", "query", q, "serverQuery", w.serverQuery)
	w.searchAllBtn.SetVisible(q != "" && w.canSearchServer())
	w.searchSort.SetVisible(q != "")
	// The search-changed signal is debounced, so a programmatic SetText (e.g.
	// "Find emails from sender") arrives here after suppressSearch was cleared.
	// Only a genuinely different query exits server-search mode back to local.
	if q != w.serverQuery {
		w.searchAllBtn.SetSensitive(true)
		w.serverSearch = false
	}
	// Cancelling the in-flight load here would be a guess about what the refresh
	// below decides to do: startThreadLoad cancels the previous query when it
	// actually starts a replacement, and when the refresh coalesces into that
	// same query instead, killing it would abandon the list mid-load.
	w.refreshList(q)
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
		mode := threadPageLabel
		switch label {
		case allMailID:
			mode = threadPageAll
		case snoozedID:
			mode = threadPageSnoozed
		}
		key := threadPageKey{mode: mode, accountID: acct, label: label}
		if w.coalesceThreadLoad(key) {
			return
		}
		target := threadPageSize
		if w.threadPage.key == key && len(w.threadPage.sums) > target {
			target = len(w.threadPage.sums)
		}
		logging.Trace("ui: load threads (label)", "label", label, "account", acct, "unreadOnly", w.unreadOnly)
		w.startThreadLoad(key, false, func() { w.loadThreadsFor(query) }, func(ctx context.Context) (threadPageResult, error) {
			switch mode {
			case threadPageAll:
				page, err := w.deps.Store.ListAllThreadsPage(ctx, acct, target, nil)
				return threadPageResult{sums: page.Threads, cursor: page.Next, hasMore: page.Next != nil}, err
			case threadPageSnoozed:
				sums, err := w.snoozedSummaries(ctx, acct)
				return threadPageResult{sums: sums}, err
			default:
				page, err := w.deps.Store.ListThreadsByLabelPage(ctx, acct, label, target, nil)
				return threadPageResult{sums: page.Threads, cursor: page.Next, hasMore: page.Next != nil}, err
			}
		}, nil)
		return
	}

	// A server-side search stays a server search across refreshes (e.g. a
	// background sync) without restarting its provider cursor.
	if w.serverSearch && w.canSearchServer() {
		logging.Trace("ui: load threads (server search)", "query", trimmed, "account", w.activeID)
		key := threadPageKey{mode: threadPageServerSearch, accountID: w.activeID, query: trimmed, newest: w.searchSort.Selected() == 1}
		if w.coalesceThreadLoad(key) {
			return
		}
		if w.threadPage.key != key {
			w.runServerSearch(trimmed)
			return
		}
		state := w.threadPage
		w.startThreadLoad(key, false, func() { w.loadThreadsFor(query) }, func(ctx context.Context) (threadPageResult, error) {
			sums, err := w.serverSearchSummaries(ctx, key.accountID, state.serverIDs, key.newest)
			return threadPageResult{
				sums: sums, serverToken: state.serverToken, serverIDs: state.serverIDs,
				hasMore: state.hasMore,
			}, err
		}, nil)
		return
	}

	acct := w.activeID
	key := threadPageKey{mode: threadPageLocalSearch, accountID: acct, query: trimmed, newest: w.searchSort.Selected() == 1}
	if w.coalesceThreadLoad(key) {
		return
	}
	target := threadPageSize
	if w.threadPage.key == key && w.threadPage.searchOffset > target {
		target = w.threadPage.searchOffset
	}
	logging.Trace("ui: load threads (local search)", "query", query, "account", acct)
	w.startThreadLoad(key, false, func() { w.loadThreadsFor(query) }, func(ctx context.Context) (threadPageResult, error) {
		sums, consumed, more, err := w.searchThreadPage(ctx, acct, query, target, 0, key.newest)
		return threadPageResult{sums: sums, searchOffset: consumed, hasMore: more}, err
	}, nil)
}

// coalesceThreadLoad folds a refresh into the identical query already in flight,
// reporting whether the caller should stand down. The in-flight result can't
// simply be accepted as this refresh's answer: it was queried before whatever
// asked for the refresh happened (an archive's optimistic label change, a sync
// that just stored mail), so it lists the conversation the user just archived
// and would run afterPopulate — advanceSelection — against that stale model.
// Instead the load is re-run once the in-flight one lands, which keeps exactly
// one query in flight without ever dropping a refresh. Cancelling the in-flight
// query here would be worse still: nothing would then repopulate the list, and
// the abandoned query surfaces as "Couldn't load messages".
func (w *window) coalesceThreadLoad(key threadPageKey) bool {
	if w.threadLoad.cancel == nil || w.threadLoad.key != key {
		return false
	}
	logging.Trace("ui: load threads coalesced into in-flight query",
		"mode", key.mode, "account", key.accountID, "label", key.label, "query", key.query)
	w.threadLoad.reload = true
	return true
}

// startThreadLoad runs one initial, refresh, or continuation query off the GTK
// thread. It commits page state only after success, so a failed continuation
// keeps every already-visible conversation and offers an in-place retry.
func (w *window) startThreadLoad(key threadPageKey, loadingMore bool, retry func(), query func(context.Context) (threadPageResult, error), onError func(error)) {
	if w.threadLoad.cancel != nil {
		w.threadLoad.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.loadGen++
	gen := w.loadGen
	// Replacing the value wholesale also drops any coalesced reload: this query
	// is the re-run that refresh was waiting for.
	w.threadLoad = threadLoad{key: key, gen: gen, more: loadingMore, cancel: cancel}
	w.threadFailed = nil
	w.updateThreadPageStatus()
	go func() {
		defer cancel()
		start := time.Now()
		result, err := query(ctx)
		slog.Debug("ui: loadThreads", "n", len(result.sums), "dur", time.Since(start))
		logging.Trace("ui: load threads result", "n", len(result.sums), "more", result.hasMore, "dur", time.Since(start), "err", err)
		dispatch.Main(func() {
			if gen != w.threadLoad.gen {
				logging.Trace("ui: load threads discarded", "gen", gen, "cur", w.threadLoad.gen, "n", len(result.sums))
				return // superseded by a newer refresh
			}
			reload := w.threadLoad.reload
			w.threadLoad = threadLoad{} // nothing running any more
			if errors.Is(err, context.Canceled) {
				// Abandoned, not broken: whoever cancelled owns what comes next.
				// Keep the loaded conversations on screen rather than replacing
				// them with a failure the user can't act on.
				logging.Trace("ui: load threads cancelled", "gen", gen)
				w.updateThreadPageStatus()
				if reload {
					w.rerunCoalescedLoad()
				}
				return
			}
			if err != nil {
				slog.Error("ui: load threads", "err", err)
				w.threadFailed = &threadLoadFailure{more: loadingMore, retry: retry}
				w.updateThreadPageStatus()
				if onError != nil {
					onError(err)
				} else if !loadingMore {
					w.toast("Couldn't load messages")
				}
				if reload {
					w.rerunCoalescedLoad()
				}
				return
			}
			w.threadPage = threadPageState{
				key: key, sums: result.sums, cursor: result.cursor,
				searchOffset: result.searchOffset, serverToken: result.serverToken,
				serverIDs: result.serverIDs, hasMore: result.hasMore,
			}
			w.updateThreadPageStatus()
			w.searchAllBtn.SetSensitive(true)
			if reload {
				// These rows predate the refresh that coalesced into this query,
				// so afterPopulate — advanceSelection past an archived
				// conversation — waits for the re-run's real ones.
				held := w.afterPopulate
				w.afterPopulate = nil
				w.showThreads(result.sums)
				w.afterPopulate = held
				w.rerunCoalescedLoad()
				return
			}
			w.showThreads(result.sums)
			dispatch.Main(w.maybeLoadNextThreadPage)
		})
	}()
}

// rerunCoalescedLoad re-issues the refresh that coalesceThreadLoad folded into
// the query that just finished.
func (w *window) rerunCoalescedLoad() {
	logging.Trace("ui: re-running coalesced thread load")
	w.loadThreadsFor(w.searchEntry.Text())
}

// searchThreadPage loads one bounded slice of FTS message hits and groups it
// into thread summaries. consumed is the number of raw hits advanced: multiple
// matching messages can belong to one visible conversation.
func (w *window) searchThreadPage(ctx context.Context, acct int64, query string, limit, offset int, newest bool) ([]model.ThreadSummary, int, bool, error) {
	msgs, err := w.deps.Store.SearchPage(ctx, acct, query, limit+1, offset)
	if err != nil {
		return nil, 0, false, err
	}
	more := len(msgs) > limit
	if more {
		msgs = msgs[:limit]
	}
	sums, err := w.deps.Store.GetThreadSummaries(ctx, acct, uniqueThreadIDs(msgs))
	if err != nil {
		return nil, 0, false, err
	}
	if newest {
		sortThreadSummariesNewest(sums)
	}
	return sums, len(msgs), more, nil
}

func (w *window) updateThreadPageStatus() {
	if w.pageStatus == nil {
		return
	}
	switch {
	case w.threadLoad.cancel != nil:
		w.pageSpinner.SetVisible(true)
		w.pageRetry.SetVisible(false)
		if w.threadLoad.more {
			w.pageLabel.SetText("Loading more messages…")
		} else {
			w.pageLabel.SetText("Loading messages…")
		}
		w.pageStatus.SetVisible(true)
	case w.threadFailed != nil:
		w.pageSpinner.SetVisible(false)
		if w.threadFailed.more {
			w.pageLabel.SetText("Couldn't load more messages")
		} else {
			w.pageLabel.SetText("Couldn't load messages")
		}
		w.pageRetry.SetVisible(w.threadFailed.retry != nil)
		w.pageStatus.SetVisible(true)
	default:
		w.pageSpinner.SetVisible(false)
		w.pageRetry.SetVisible(false)
		w.pageStatus.SetVisible(false)
	}
}

func (w *window) maybeLoadNextThreadPage() {
	if w.threadScroll == nil || w.threadLoad.cancel != nil || w.threadFailed != nil || !w.threadPage.hasMore {
		return
	}
	adj := w.threadScroll.VAdjustment()
	if adj.Upper()-adj.Value()-adj.PageSize() <= 600 {
		w.loadNextThreadPage()
	}
}

func (w *window) loadNextThreadPage() {
	state := w.threadPage
	if w.threadLoad.cancel != nil || w.threadFailed != nil || !state.hasMore {
		return
	}
	key := state.key
	logging.Trace("ui: load next thread page", "mode", key.mode, "account", key.accountID, "loaded", len(state.sums))
	w.startThreadLoad(key, true, w.loadNextThreadPage, func(ctx context.Context) (threadPageResult, error) {
		switch key.mode {
		case threadPageAll:
			page, err := w.deps.Store.ListAllThreadsPage(ctx, key.accountID, threadPageSize, state.cursor)
			return threadPageResult{sums: mergeThreadSummaries(state.sums, page.Threads), cursor: page.Next, hasMore: page.Next != nil}, err
		case threadPageLabel:
			page, err := w.deps.Store.ListThreadsByLabelPage(ctx, key.accountID, key.label, threadPageSize, state.cursor)
			return threadPageResult{sums: mergeThreadSummaries(state.sums, page.Threads), cursor: page.Next, hasMore: page.Next != nil}, err
		case threadPageLocalSearch:
			sums, consumed, more, err := w.searchThreadPage(ctx, key.accountID, key.query, threadPageSize, state.searchOffset, key.newest)
			return threadPageResult{
				sums:         mergeSearchThreadSummaries(state.sums, sums, key.newest),
				searchOffset: state.searchOffset + consumed, hasMore: more,
			}, err
		case threadPageServerSearch:
			ids, next, err := w.fetchServerSearchPage(ctx, key.accountID, key.query, state.serverToken)
			if err != nil {
				return threadPageResult{}, err
			}
			allIDs := appendUniqueStrings(state.serverIDs, ids)
			sums, err := w.serverSearchSummaries(ctx, key.accountID, allIDs, key.newest)
			return threadPageResult{
				sums: sums, serverToken: next, serverIDs: allIDs, hasMore: next != "",
			}, err
		default:
			return threadPageResult{sums: state.sums}, nil
		}
	}, nil)
}

func mergeThreadSummaries(existing, added []model.ThreadSummary) []model.ThreadSummary {
	byID := make(map[string]model.ThreadSummary, len(existing)+len(added))
	for _, sum := range existing {
		byID[sum.ThreadID] = sum
	}
	for _, sum := range added {
		byID[sum.ThreadID] = sum
	}
	out := make([]model.ThreadSummary, 0, len(byID))
	for _, sum := range byID {
		out = append(out, sum)
	}
	sortThreadSummariesNewest(out)
	return out
}

func sortThreadSummariesNewest(sums []model.ThreadSummary) {
	sort.SliceStable(sums, func(i, j int) bool {
		if sums[i].Latest.InternalDate.Equal(sums[j].Latest.InternalDate) {
			if sums[i].Latest.RowID == sums[j].Latest.RowID {
				return sums[i].ThreadID < sums[j].ThreadID
			}
			return sums[i].Latest.RowID > sums[j].Latest.RowID
		}
		return sums[i].Latest.InternalDate.After(sums[j].Latest.InternalDate)
	})
}

func mergeSearchThreadSummaries(existing, added []model.ThreadSummary, newest bool) []model.ThreadSummary {
	positions := make(map[string]int, len(existing)+len(added))
	out := append([]model.ThreadSummary(nil), existing...)
	for i, sum := range out {
		positions[sum.ThreadID] = i
	}
	for _, sum := range added {
		if i, ok := positions[sum.ThreadID]; ok {
			out[i] = sum
			continue
		}
		positions[sum.ThreadID] = len(out)
		out = append(out, sum)
	}
	if newest {
		sortThreadSummariesNewest(out)
	}
	return out
}

func appendUniqueStrings(existing, added []string) []string {
	seen := make(map[string]bool, len(existing)+len(added))
	out := make([]string, 0, len(existing)+len(added))
	for _, id := range append(append([]string(nil), existing...), added...) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
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

func isDateHeader(id string) bool { return strings.HasPrefix(id, dateHdrPrefix) }

// dateBucket names the date group a message time falls into, relative to now.
func dateBucket(t, now time.Time) string {
	if t.IsZero() {
		return "Older"
	}
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	switch {
	case !t.Before(today):
		return "Today"
	case !t.Before(today.AddDate(0, 0, -1)):
		return "Yesterday"
	case !t.Before(today.AddDate(0, 0, -6)):
		return "Last 7 days"
	case !t.Before(today.AddDate(0, 0, -29)):
		return "Last 30 days"
	default:
		return "Older"
	}
}

// dateHeaderRow renders a date group header row.
func dateHeaderRow(label string) *gtk.Box {
	box := gtk.NewBox(gtk.OrientationHorizontal, 0)
	l := gtk.NewLabel(label)
	l.SetXAlign(0)
	l.AddCSSClass("dim-label")
	l.AddCSSClass("caption-heading")
	setMargins(l, 12, 12, 10, 2)
	box.Append(l)
	return box
}

func (w *window) showThreads(sums []model.ThreadSummary) {
	if !w.switchStart.IsZero() {
		logging.Trace("ui: switch account content visible", "account", w.activeID, "n", len(sums), "dur", time.Since(w.switchStart))
		w.switchStart = time.Time{}
	}
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

	// Date group headers ("Today", …) are woven in as synthetic rows — the
	// label views are date-ordered so buckets appear once each. Search results
	// are relevance-shaped and the Snoozed view is ordered by wake time, so
	// both stay header-free.
	withHeaders := strings.TrimSpace(w.searchEntry.Text()) == "" && w.current != snoozedID
	now := time.Now()
	newByID := make(map[string]model.ThreadSummary, len(sums))
	ids := make([]string, 0, len(sums)+6)
	lastBucket := ""
	for _, s := range sums {
		if withHeaders {
			if b := dateBucket(s.Latest.InternalDate, now); b != lastBucket {
				ids = append(ids, dateHdrPrefix+b)
				lastBucket = b
			}
		}
		ids = append(ids, s.ThreadID)
		newByID[s.ThreadID] = s
	}
	// Publish the new data before touching the model so any (re)bind reads it.
	oldIDs := w.threadIDs
	w.threadByID = newByID
	w.diffThreadModel(oldIDs, ids)
	w.threadIDs = ids
	w.updateEmptyFolderBanner(len(sums))

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
			if w.serverSearch {
				w.emptyPage.SetDescription(fmt.Sprintf("No messages in this account match %q.", q))
			} else {
				w.emptyPage.SetDescription(fmt.Sprintf("No cached messages match %q.", q))
			}
			// Keep the empty-state action as a larger, discoverable counterpart to
			// the search-bar button when only the local cache has been searched.
			if w.canSearchServer() && !w.serverSearch {
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
		rebound := 0
		for i, id := range newIDs {
			sig := w.renderSig(id)
			if w.rowSig[id] != sig {
				w.rowSig[id] = sig
				w.threadModel.Splice(uint(i), 1, []string{id}) // remove+add same id → re-bind row i
				rebound++
			}
		}
		if rebound == 0 {
			logging.Trace("ui: diff threads no-op", "n", len(newIDs))
		} else {
			logging.Trace("ui: diff threads rebind", "n", len(newIDs), "rebound", rebound)
		}
		return
	}

	// Paging normally extends a date-ordered folder without disturbing its
	// existing prefix. Append only the new tail so GTK keeps the current scroll
	// anchor and realized row widgets instead of replacing the entire model.
	if len(newIDs) > len(oldIDs) && slices.Equal(oldIDs, newIDs[:len(oldIDs)]) {
		rebound := 0
		for i, id := range oldIDs {
			sig := w.renderSig(id)
			if w.rowSig[id] != sig {
				w.rowSig[id] = sig
				w.threadModel.Splice(uint(i), 1, []string{id})
				rebound++
			}
		}
		tail := newIDs[len(oldIDs):]
		w.threadModel.Splice(uint(len(oldIDs)), 0, tail)
		for _, id := range tail {
			w.rowSig[id] = w.renderSig(id)
		}
		logging.Trace("ui: diff threads append", "old", len(oldIDs), "added", len(tail), "rebound", rebound)
		return
	}

	// Structural change: replace the whole model and rebuild the signature cache.
	logging.Trace("ui: diff threads splice", "old", len(oldIDs), "new", len(newIDs))
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
	if isDateHeader(id) {
		return id // header rows never change appearance
	}
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
	cat := w.threadCats[w.activeCacheKey(id)]
	return fmt.Sprintf("%s\x1f%d\x1f%d\x1f%s\x1f%t\x1f%t\x1f%s\x1f%s\x1f%d\x1f%t\x1f%t\x1f%t\x1f%t\x1f%d\x1f%s",
		sel, t.UnreadCount, t.Count, cat.tag, cat.manual, cat.failed, who, m.Subject,
		m.InternalDate.Unix(), m.HasAttachments, m.IsStarred, t.RepliedByMe, t.HasDraft, t.SnoozedUntil, m.Snippet)
}

// categoryCand is one thread whose tag to look up: its thread id and the gmail
// id of its latest message (what the category is keyed/persisted by).
type categoryCand struct {
	threadID, msgID string
}

// categorizeInbox seeds inbox category tags from the persisted per-email cache
// (store.MessageCategories) — no AI calls from the UI: classification itself
// runs in the background worker (internal/aiwork) for every account, announced
// back via AIUpdated events. Seeding is two batched indexed queries, so it runs
// on every inbox populate; the list is re-bound only when a tag actually
// changed. Gated by the inboxCategories preference + an assistant.
func (w *window) categorizeInbox() {
	if !w.inboxCategories || w.deps.Assistant == nil || w.current != model.LabelInbox {
		return
	}
	// A search fills threadByID with hits from anywhere (local FTS or a server
	// search), not the inbox — tagging those would repeat on every refresh.
	// Only the real inbox listing seeds tags.
	if w.serverSearch || strings.TrimSpace(w.searchEntry.Text()) != "" {
		logging.Trace("ui: categorize inbox skipped", "reason", "search active", "serverSearch", w.serverSearch)
		return
	}
	// Candidates: inbox threads whose tag wasn't yet applied for their current
	// latest message (a reply re-keys the thread, so its tag refreshes). Built on
	// the main thread (reads threadByID); the store lookups run in the background
	// and marshal back through dispatch.
	var cands []categoryCand
	for id, t := range w.threadByID {
		if w.threadCats[w.activeCacheKey(id)].categorizedMsg == t.Latest.GmailID {
			continue
		}
		cands = append(cands, categoryCand{threadID: id, msgID: t.Latest.GmailID})
	}
	if len(cands) == 0 {
		return
	}
	acctID := w.activeID
	go func() {
		ctx := context.Background()
		msgIDs := make([]string, len(cands))
		for i, c := range cands {
			msgIDs[i] = c.msgID
		}
		cached, err := w.deps.Store.MessageCategories(ctx, acctID, msgIDs)
		if err != nil {
			slog.Warn("ui: load cached categories", "err", err)
			cached = map[string]string{}
		}
		// Which of those were set by hand — so a manual pick still outranks the
		// "Replied" tag after a restart, not just in the session it was made.
		manual, err := w.deps.Store.ManualCategoryIDs(ctx, acctID, msgIDs)
		if err != nil {
			slog.Warn("ui: load manual categories", "err", err)
			manual = map[string]bool{}
		}
		// Ids whose last AI attempt errored — the worker retries them; meanwhile
		// they show a distinct "failed" tag rather than looking uncategorized.
		failedIDs, err := w.deps.Store.FailedCategoryIDs(ctx, acctID, msgIDs)
		if err != nil {
			slog.Warn("ui: load failed categories", "err", err)
			failedIDs = map[string]bool{}
		}
		logging.Trace("ui: categorize seeded from cache", "candidates", len(cands), "cached", len(cached), "manual", len(manual), "failed", len(failedIDs), "account", acctID)
		dispatch.Main(func() {
			if w.activeID != acctID {
				return // switched accounts; these tags belong to the other account
			}
			changed := false
			for _, c := range cands {
				key := cacheKey(acctID, c.threadID)
				switch cat, ok := cached[c.msgID]; {
				case ok:
					// A stored tag settles the thread: it is no longer failed, and
					// a manual pick recorded by the store stays a manual pick.
					next := threadCategory{
						tag: cat, categorizedMsg: c.msgID,
						manual: manual[c.msgID] || w.threadCats[key].manual,
					}
					if w.threadCats[key] != next {
						w.threadCats[key] = next
						changed = true
					}
				case failedIDs[c.msgID] && !w.threadCats[key].failed:
					cur := w.threadCats[key]
					cur.failed = true
					w.threadCats[key] = cur
					changed = true
				}
			}
			// Re-bind only when a tag actually changed: showThreads re-enters this
			// seeder on every refresh, so an unconditional refresh would loop.
			if changed {
				w.refreshList(w.searchEntry.Text())
			}
		})
	}()
}

func threadRow(t model.ThreadSummary, outgoing bool, category string, manualCat bool, categorizeFailed bool) *gtk.Box {
	m := t.Latest
	unread := t.UnreadCount > 0
	// Once you've had the last word the conversation is handled, so show a
	// "Replied" tag in place of the content category (Needs reply / Discount / …).
	// Skipped in Sent/Drafts, where the last message is always yours, and when the
	// user picked the category by hand (a deliberate choice outranks "Replied").
	switch {
	case t.RepliedByMe && !outgoing && !manualCat:
		category = "Replied"
	case t.WokeFromSnooze && !outgoing && !manualCat:
		// A thread that returned to the inbox from Snooze — "Replied" still wins
		// if both apply (it's the more current, actionable fact). The tag stays
		// until the user re-snoozes it or picks a category by hand.
		category = "Snoozed"
	case category == "" && categorizeFailed:
		// The AI attempt errored rather than legitimately finding no category —
		// show that distinctly instead of silently looking uncategorized. A
		// retry happens automatically on a later pass.
		category = "Categorize failed"
	}

	box := gtk.NewBox(gtk.OrientationVertical, 2)
	setMargins(box, 12, 12, 6, 6)

	top := gtk.NewBox(gtk.OrientationHorizontal, 6)
	if unread {
		// A small accent dot marks an unread conversation at a glance. The "●"
		// glyph is meaningless to a screen reader, so give it a proper name.
		dot := gtk.NewLabel("●")
		dot.AddCSSClass("unread-dot")
		dot.SetVAlign(gtk.AlignCenter)
		a11yLabel(dot, "Unread")
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
	stamp := ""
	if t.SnoozedUntil > 0 {
		stamp = "until " + formatWakeTime(time.Unix(t.SnoozedUntil, 0), time.Now())
	} else {
		stamp = relativeDate(m.InternalDate, time.Now())
	}
	if stamp != "" {
		date := gtk.NewLabel(stamp)
		date.AddCSSClass("dim-label")
		date.AddCSSClass("caption")
		top.Append(date)
	}
	box.Append(top)

	subjText := singleLineText(m.Subject)
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
	// An unsent draft waits in this thread — marked in red (Gmail's convention)
	// so the row isn't read as settled correspondence. Redundant in the Drafts
	// folder itself, where every row is a draft.
	hasDraft := t.HasDraft && !outgoing
	// An AI category tag (e.g. "Needs reply") sits before the subject;
	// uncategorized mail shows nothing.
	if category != "" || hasDraft {
		subjRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
		if hasDraft {
			d := gtk.NewLabel("Draft")
			d.AddCSSClass("cat-tag")
			d.AddCSSClass("cat-draft")
			d.SetVAlign(gtk.AlignCenter)
			subjRow.Append(d)
		}
		if category != "" {
			tag := gtk.NewLabel(category)
			tag.AddCSSClass("cat-tag")
			switch category {
			case "Needs reply":
				tag.AddCSSClass("cat-needsreply")
			case "Replied":
				tag.AddCSSClass("cat-replied")
			case "Discount":
				tag.AddCSSClass("cat-discount")
			case "Snoozed":
				tag.AddCSSClass("cat-snoozed")
			case "Categorize failed":
				tag.AddCSSClass("cat-failed")
			}
			tag.SetVAlign(gtk.AlignCenter)
			subjRow.Append(tag)
		}
		subjRow.Append(subj)
		box.Append(subjRow)
	} else {
		box.Append(subj)
	}

	if m.Snippet != "" {
		// Decode any HTML entities in older cached snippets (new ones arrive
		// already decoded); harmless on plain text.
		snip := gtk.NewLabel(singleLineText(html.UnescapeString(m.Snippet)))
		snip.SetXAlign(0)
		snip.SetEllipsize(pango.EllipsizeEnd)
		snip.AddCSSClass("dim-label")
		snip.AddCSSClass("caption")
		box.Append(snip)
	}
	return box
}

func singleLineText(s string) string {
	return strings.Join(strings.Fields(s), " ")
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
