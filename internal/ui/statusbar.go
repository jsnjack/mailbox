package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/jsnjack/mailbox/internal/activity"
	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// statusLogCap bounds how many recent operations the log panel keeps.
const statusLogCap = 200

// logRow is one operation's row in the activity log. An operation gets a
// single row for its whole lifecycle — inserted at Start (with a running
// glyph), updated with bounded progress, and finished in place with its
// duration and result — instead of separate start/done lines that interleave
// under concurrency.
type logRow struct {
	box     *gtk.Box   // the whole row, so a superseded row can be removed
	status  *gtk.Label // ▸ running · ✓ ok · ✗ error · – cancelled
	lbl     *gtk.Label // the operation, re-worded to past tense once it is done
	dur     *gtk.Label // live progress ("3/10") while running, duration when done
	note    *gtk.Label // result note (counts, errors); empty until Done
	started time.Time
}

// statCell is one label/value pair in the panel's session grid.
type statCell struct {
	box   *gtk.Box
	value *gtk.Label
	unit  *gtk.Label
}

// buildActivityRow builds the main window's activity row — the permanent one at
// the bottom of the sidebar — together with the log panel it opens. See
// activityRow for the surface's behaviour.
func (w *window) buildActivityRow() *activityRow {
	w.statusLogRows = make(map[string][]*logRow)
	w.statusQuietRows = make(map[string]*gtk.Box)

	w.activity = newActivityRow(func() string {
		return restingText(w.lastSyncAt, time.Now(), w.aiFailing)
	})
	w.activity.windowActive = func() bool { return w.win != nil && w.win.IsActive() }
	w.activity.setPanel(w.buildActivityPanel())

	// Keep the resting line honest ("Up to date · just now" becomes a clock
	// time) while nothing is happening; a cheap label repaint twice a minute.
	glib.TimeoutSecondsAdd(30, func() bool {
		w.activity.refreshResting()
		return true
	})
	return w.activity
}

// buildActivityPanel returns the popover the activity row opens: this session's
// operations, then what the app has spent.
func (w *window) buildActivityPanel() *gtk.Popover {
	w.statusLogBox = gtk.NewBox(gtk.OrientationVertical, 1)
	w.statusLogEmpty = gtk.NewLabel("No activity yet")
	w.statusLogEmpty.AddCSSClass("dim-label")
	w.statusLogEmpty.SetXAlign(0)
	setMargins(w.statusLogEmpty, 2, 2, 6, 6)
	w.statusLogBox.Append(w.statusLogEmpty)

	scroller := gtk.NewScrolledWindow()
	scroller.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroller.SetChild(w.statusLogBox)
	// Natural height, capped: four rows make a four-row panel rather than a
	// mostly-empty box of a fixed size.
	scroller.SetMaxContentHeight(300)
	scroller.SetPropagateNaturalHeight(true)

	content := gtk.NewBox(gtk.OrientationVertical, 4)
	setMargins(content, 8, 8, 8, 8)
	content.SetSizeRequest(470, -1)
	content.Append(heading("Activity"))
	content.Append(scroller)
	content.Append(gtk.NewSeparator(gtk.OrientationHorizontal))
	content.Append(heading("Session"))
	content.Append(w.buildSessionGrid())

	pop := gtk.NewPopover()
	pop.SetChild(content)
	pop.ConnectShow(w.refreshStatusStats) // stats are read on demand, not polled
	return pop
}

// buildSessionGrid lays the cumulative counters out as label/value pairs: the
// number carries the weight, its units are demoted beside it, and the caption
// above says what it counts. As one wrapped paragraph of "key: value" lines,
// none of it was scannable.
func (w *window) buildSessionGrid() *gtk.Widget {
	grid := gtk.NewGrid()
	grid.SetColumnSpacing(18)
	grid.SetRowSpacing(10)
	grid.SetColumnHomogeneous(true)
	setMargins(grid, 2, 2, 4, 2)

	w.statGrid = grid
	w.statNet = newStatCell("Transferred")
	w.statAPI = newStatCell("Gmail API")
	w.statAI = newStatCell("AI")
	w.statCache = newStatCell("Cache")
	w.layoutStats()

	w.statModel = gtk.NewLabel("")
	w.statModel.SetXAlign(0)
	w.statModel.SetEllipsize(pango.EllipsizeEnd)
	w.statModel.AddCSSClass("stat-model")
	w.statModelChip = gtk.NewLabel("fallback")
	w.statModelChip.AddCSSClass("stat-chip")
	w.statModelChip.SetVisible(false)

	w.statModelBox = gtk.NewBox(gtk.OrientationHorizontal, 8)
	setMargins(w.statModelBox, 2, 2, 2, 0)
	serving := gtk.NewLabel("Serving")
	serving.AddCSSClass("stat-key")
	w.statModelBox.Append(serving)
	w.statModelBox.Append(w.statModel)
	w.statModelBox.Append(w.statModelChip)
	w.statModelBox.SetVisible(false)

	box := gtk.NewBox(gtk.OrientationVertical, 8)
	box.Append(grid)
	box.Append(w.statModelBox)
	return &box.Widget
}

// layoutStats places the visible counters in reading order across two columns.
// A counter is hidden when the app hasn't done that kind of work at all (no AI
// requests this session), and leaving its cell in place would put a hole in the
// middle of the grid rather than closing the gap.
func (w *window) layoutStats() {
	slot := 0
	for _, c := range []*statCell{w.statNet, w.statAPI, w.statAI, w.statCache} {
		if c.box.Parent() != nil {
			w.statGrid.Remove(c.box)
		}
		if !c.box.Visible() {
			continue
		}
		w.statGrid.Attach(c.box, slot%2, slot/2, 1, 1)
		slot++
	}
}

// newStatCell builds one caption-over-value pair for the session grid.
func newStatCell(key string) *statCell {
	k := gtk.NewLabel(key)
	k.SetXAlign(0)
	k.AddCSSClass("stat-key")

	c := &statCell{
		value: gtk.NewLabel("—"),
		unit:  gtk.NewLabel(""),
	}
	c.value.AddCSSClass("stat-value")
	c.unit.SetXAlign(0)
	c.unit.AddCSSClass("stat-unit")
	c.unit.SetEllipsize(pango.EllipsizeEnd)

	line := gtk.NewBox(gtk.OrientationHorizontal, 5)
	line.Append(c.value)
	line.Append(c.unit)

	c.box = gtk.NewBox(gtk.OrientationVertical, 1)
	c.box.Append(k)
	c.box.Append(line)
	return c
}

// set fills a cell and reveals it; an absent counter hides the cell instead of
// showing a zero that means "we never did this".
func (c *statCell) set(show bool, value, unit string) {
	c.box.SetVisible(show)
	if !show {
		return
	}
	c.value.SetText(value)
	c.unit.SetText(unit)
}

// heading is a small bold section label for the panel.
func heading(text string) *gtk.Label {
	l := gtk.NewLabel(text)
	l.SetXAlign(0)
	l.AddCSSClass("heading")
	return l
}

// noteCancelled is the activity note for a user-cancelled operation. It is
// neutral for AI health: a cancel (switching threads, reverting a translation)
// says nothing about whether the provider works, so noteAIResult ignores it —
// neither flagging a failure (which would pause categorization for the
// cooldown) nor claiming a success.
const noteCancelled = "cancelled"

// doneErr summarizes an operation result for the activity log note.
func doneErr(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return noteCancelled
	default:
		return "error: " + err.Error()
	}
}

// aiActivity reports an AI operation for the active account to the activity
// hub; the returned function ends it (pass a note, e.g. a token count or "" ).
// The log note is suffixed with the model that served the request
// (failover-aware), so the log shows which chain entry answered. It also
// records AI health so the row's dot can flag a failing provider. Safe when no
// hub is wired.
func (w *window) aiActivity(label string) func(note string) {
	return w.aiActivityFor(w.activeEmail, label)
}

// aiActivityFor is aiActivity for an explicit account (the new-mail gist can
// run for a non-active account).
func (w *window) aiActivityFor(email, label string) func(note string) {
	var end func(string)
	if w.deps.Activity != nil {
		end = w.deps.Activity.Begin("ai", email, label)
	}
	return func(note string) {
		if end != nil {
			end(w.withAIModel(note))
		}
		w.noteAIResult(note)
	}
}

// aiActivityIn is aiActivity for work started from a dialog with its own
// activity row (compose): the operation is reported to the app-wide hub, so it
// still reaches the log, and to that window's row, so the window the user is
// looking at is the one that reports its own progress.
func (w *window) aiActivityIn(row *activityRow, label string) func(note string) {
	end := w.aiActivity(label)
	if row == nil {
		return end
	}
	rowEnd := row.begin(label)
	return func(note string) {
		// The row gets the bare result; which chain entry served it is a detail
		// for the log, and the model name would crowd out the result itself.
		rowEnd(note)
		end(note)
	}
}

// opActivity brackets an operation the UI performs itself — a dialog action, a
// maintenance task — into the activity hub, so the log is a complete record of
// what the app did rather than only the work the background layers happen to
// publish. Pass "" for account for app-wide work. The returned function ends
// it with a result note, exactly like activity.Hub.Begin.
func (w *window) opActivity(op, email, label string) func(note string) {
	if w.deps.Activity == nil {
		return func(string) {}
	}
	return w.deps.Activity.Begin(op, email, label)
}

// reportActivity logs an operation that is already over — one with no waiting
// to narrate. Its label is written in past tense, like every other Report.
func (w *window) reportActivity(op, email, label, note string) {
	if w.deps.Activity != nil {
		w.deps.Activity.Report(op, email, label, note)
	}
}

// emailForAccount resolves an account id to the address activity events are
// keyed by; unknown ids report as app-wide rather than inventing a name.
func (w *window) emailForAccount(id int64) string {
	for _, a := range w.deps.Accounts {
		if a.ID == id {
			return a.Email
		}
	}
	return ""
}

func (w *window) isIMAPAccount(id int64) bool {
	for _, a := range w.deps.Accounts {
		if a.ID == id {
			return a.Type == model.AccountIMAP
		}
	}
	return false
}

// accountTag maps an activity event's account email to its short display form:
// the user-assigned name ("Work") when set, else the email; "" stays "".
func (w *window) accountTag(email string) string {
	if email == "" {
		return ""
	}
	if n := strings.TrimSpace(w.accountNames[email]); n != "" {
		return n
	}
	return email
}

// withAIModel appends the serving model to a successful AI note ("2.1 KB ·
// granite-4.…"), shortened to stay glanceable (ai.ShortModel). Errors and
// cancels pass through — no model served those.
func (w *window) withAIModel(note string) string {
	if w.deps.Assistant == nil || note == noteCancelled || strings.HasPrefix(note, "error:") {
		return note
	}
	m := ai.ShortModel(w.deps.Assistant.ActiveModel())
	switch {
	case m == "":
		return note
	case note == "":
		return m
	default:
		return note + " · " + m
	}
}

// doneErrCtx is doneErr for an operation whose context the user can cancel
// (switching threads, reverting a translation): once ctx is cancelled the
// result is reported neutral regardless of how the provider surfaced the abort
// (not every HTTP/stream error chain wraps context.Canceled).
func doneErrCtx(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return noteCancelled
	}
	return doneErr(err)
}

// noteAIResult reads an AI op's completion note (doneErr yields "error: …" on
// failure) and tints the activity row's health dot, recording the failure time
// so auto-categorization can back off (see categorizeInbox). A user-cancelled
// op is neutral — it neither marks the provider failing nor healthy.
func (w *window) noteAIResult(note string) {
	if note == noteCancelled {
		logging.Trace("ui: ai result cancelled — health unchanged")
		return
	}
	failed := strings.HasPrefix(note, "error:")
	dispatch.Main(func() {
		if failed {
			w.aiFailedAt = time.Now()
		}
		if w.aiFailing != failed {
			w.aiFailing = failed
			logging.Trace("ui: ai provider health changed", "failing", failed, "note", note)
			if w.activity != nil {
				health := healthOK
				if failed {
					health = healthWarning
				}
				w.activity.setHealth(health)
			}
		}
	})
}

// subscribeActivity drains the activity hub into the row and its log (on the
// main thread). No-op when no hub is wired (read-only/no-account mode).
func (w *window) subscribeActivity() {
	if w.deps.Activity == nil {
		return
	}
	ch, _ := w.deps.Activity.Subscribe()
	go func() {
		for e := range ch {
			e := e
			dispatch.Main(func() { w.onActivity(e) })
		}
	}()
}

// onActivity updates the row (and log) for one event. Main thread only.
func (w *window) onActivity(e activity.Event) {
	key := e.Op + "\x00" + e.Account + "\x00" + e.Label
	tag := w.accountTag(e.Account)
	disp := barText(tag, e.Label)
	switch e.Phase {
	case activity.Start:
		// Concurrent identical ops queue up; Done finishes the oldest (FIFO), so
		// no row is ever orphaned in the running state.
		w.statusLogRows[key] = append(w.statusLogRows[key], w.newLogRow(e.Op, tag, e.Label))
		logging.Trace("ui: activity start", "op", e.Op, "label", e.Label)
	case activity.Progress:
		if e.Total > 0 {
			p := fmt.Sprintf("%d/%d", e.Done, e.Total)
			if rows := w.statusLogRows[key]; len(rows) > 0 {
				rows[0].dur.SetText(p)
			}
		}
	case activity.Done:
		var row *logRow
		if rows := w.statusLogRows[key]; len(rows) > 0 {
			row = rows[0]
			if len(rows) == 1 {
				delete(w.statusLogRows, key)
			} else {
				w.statusLogRows[key] = rows[1:]
			}
		} else {
			// A Report (instant, completed operation) — or a Start published
			// before the UI subscribed. One row, no duration.
			row = w.newLogRow(e.Op, tag, e.Label)
			row.started = time.Time{}
			// The row never ran, so the row widget never saw a Start either.
			w.activity.reportDone(e.Op, e.Label, e.Note)
		}
		var dur time.Duration
		if !row.started.IsZero() {
			dur = time.Since(row.started)
			row.dur.SetText(humanDuration(dur))
		} else {
			row.dur.SetText("")
		}
		switch {
		case e.Note == noteCancelled:
			row.status.SetText("–")
		case strings.HasPrefix(e.Note, "error:"):
			row.status.SetText("✗")
			row.status.AddCSSClass("log-error")
			row.note.AddCSSClass("log-error")
		default:
			row.status.SetText("✓")
			row.status.AddCSSClass("log-ok")
			// A finished operation reads in past tense, the same way the row
			// above it does.
			row.lbl.SetText(activity.PastTense(e.Label))
		}
		if e.Note != "" {
			row.note.SetText("· " + e.Note)
		}
		// A quiet mail check repeats every minute per account — keep only the
		// newest such row so the log records real events, not wallpaper.
		if e.Op == "sync" && (e.Note == activity.NoteUpToDate || e.Note == "") {
			if old := w.statusQuietRows[key]; old != nil && old != row.box && old.Parent() != nil {
				w.statusLogBox.Remove(old)
				w.statusLogLines--
			}
			w.statusQuietRows[key] = row.box
		}
		logging.Trace("ui: activity done", "op", e.Op, "label", e.Label, "dur", dur, "note", e.Note)
		if e.Op == "sync" {
			w.lastSyncAt = time.Now()
		}
	}
	// The row shows the account alongside the phrase; the log has its own chip
	// for it, so the two surfaces stay differently shaped on purpose.
	w.activity.feed(e, disp)
}

// refreshStatusStats fills the panel's session grid from deps.Stats. The
// gathering (a COUNT(*), an os.Stat, a recursive attachment-dir walk) runs off
// the main thread; only the label updates dispatch back.
func (w *window) refreshStatusStats() {
	if w.deps.Stats == nil {
		w.statNet.set(false, "", "")
		w.statAPI.set(false, "", "")
		w.statAI.set(false, "", "")
		w.statCache.set(true, "—", "no connected account")
		w.layoutStats()
		w.statModelBox.SetVisible(false)
		return
	}
	go func() {
		s := w.deps.Stats()
		var model string
		var fallback bool
		if w.deps.Assistant != nil {
			model, fallback = w.deps.Assistant.ModelStatus()
		}
		logging.Trace("ui: refresh session stats", "requests", s.Requests, "quota", s.QuotaUnits,
			"bytes_in", s.BytesIn, "bytes_out", s.BytesOut,
			"ai_requests", s.AIRequests, "ai_bytes_in", s.AIBytesIn, "ai_bytes_out", s.AIBytesOut,
			"messages", s.Messages, "db_bytes", s.DBBytes, "cache_bytes", s.CacheBytes)
		dispatch.Main(func() {
			w.statNet.set(s.Requests > 0 || s.BytesIn > 0,
				humanBytes(s.BytesIn), "in · "+humanBytes(s.BytesOut)+" out")
			w.statAPI.set(s.Requests > 0, fmt.Sprintf("%d", s.Requests),
				plural(s.Requests, "request", "requests")+" · "+
					activity.Plural(int(s.QuotaUnits), "quota unit", "quota units"))
			w.statAI.set(s.AIRequests > 0, fmt.Sprintf("%d", s.AIRequests),
				plural(s.AIRequests, "request", "requests")+" · "+humanBytes(s.AIBytesIn+s.AIBytesOut))
			cache := humanBytes(s.DBBytes)
			if s.CacheBytes > 0 {
				cache += " · " + humanBytes(s.CacheBytes) + " files"
			}
			w.statCache.set(true, humanCount(s.Messages), "messages · "+cache)
			w.layoutStats()
			w.statModelBox.SetVisible(model != "")
			w.statModel.SetText(model)
			w.statModelChip.SetVisible(fallback)
		})
	}()
}

// newLogRow prepends one operation's row to the activity log (newest on top,
// capped at statusLogCap) and returns it for in-place updates. A single dense
// line per operation, the account as its own chip after the kind:
//
//	15:04:05 SYNC Work ✓ Mail checked · up to date              1.2s
//
// The note rides inline after the label, dim (error-tinted on failure), with
// the full text in a tooltip only when ellipsized.
func (w *window) newLogRow(op, account, label string) *logRow {
	r := &logRow{started: time.Now()}

	tim := gtk.NewLabel(time.Now().Format("15:04:05"))
	tim.AddCSSClass("log-time")

	chip := gtk.NewLabel(strings.ToUpper(op))
	chip.AddCSSClass("log-chip")

	var acct *gtk.Label
	if account != "" {
		acct = gtk.NewLabel(account)
		acct.AddCSSClass("log-chip")
		acct.AddCSSClass("log-account") // dimmer: context, not the op kind
	}

	r.status = gtk.NewLabel("▸")
	r.status.AddCSSClass("log-time")
	r.status.SetWidthChars(1)

	r.lbl = gtk.NewLabel(label)
	r.lbl.SetXAlign(0)
	r.lbl.SetEllipsize(pango.EllipsizeEnd)
	tooltipWhenTruncated(r.lbl)

	// Always visible (even empty) — it is the row's only hexpand child, so
	// hiding it would let the duration collapse inward off the right edge.
	r.note = gtk.NewLabel("")
	r.note.SetXAlign(0)
	r.note.SetHExpand(true)
	r.note.SetEllipsize(pango.EllipsizeEnd)
	r.note.AddCSSClass("log-note")
	tooltipWhenTruncated(r.note)

	r.dur = gtk.NewLabel("")
	r.dur.AddCSSClass("log-time")
	r.dur.SetXAlign(1)

	box := gtk.NewBox(gtk.OrientationHorizontal, 6)
	box.AddCSSClass("caption")
	box.Append(tim)
	box.Append(chip)
	if acct != nil {
		box.Append(acct)
	}
	box.Append(r.status)
	box.Append(r.lbl)
	box.Append(r.note)
	box.Append(r.dur)
	r.box = box

	w.statusLogEmpty.SetVisible(false)
	w.statusLogBox.Prepend(box)
	w.statusLogLines++
	for w.statusLogLines > statusLogCap {
		if last := w.statusLogBox.LastChild(); last != nil && last != &w.statusLogEmpty.Widget {
			w.statusLogBox.Remove(last)
			w.statusLogLines--
		} else {
			break
		}
	}
	return r
}

// tooltipWhenTruncated gives a label a tooltip only while its text is actually
// ellipsized (a long error note, a long subject) — then it shows the hidden
// full text. A tooltip that just repeats visible text is noise, so a label
// that fits shows none.
func tooltipWhenTruncated(l *gtk.Label) {
	l.SetHasTooltip(true)
	l.ConnectQueryTooltip(func(_, _ int, _ bool, tip *gtk.Tooltip) bool {
		if !l.Layout().IsEllipsized() {
			return false
		}
		tip.SetText(strings.TrimPrefix(l.Text(), "· "))
		return true
	})
}

// barText renders an in-flight operation for the activity row, where there are
// no chips. Labels are self-describing phrases ("Checking mail", "Summarizing
// thread"), so the row just adds which account it's for.
func barText(account, label string) string {
	if account == "" {
		return label
	}
	return label + " · " + account
}

// humanDuration formats an elapsed duration compactly: "0.4s", "12.3s", "2m05s".
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// plural picks the noun for n without repeating the count: in the session grid
// the number is the value label sitting right beside it, so activity.Plural
// would print it twice ("1  1 request").
func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// humanCount formats a count compactly (1.2k, 3.4M).
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}
