package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/jsnjack/mailbox/internal/activity"
	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/logging"
)

// statusLogCap bounds how many recent operations the log popover keeps.
const statusLogCap = 200

// logRow is one operation's row in the activity log. An operation gets a
// single row for its whole lifecycle — inserted at Start (with a running
// glyph), updated with bounded progress, and finished in place with its
// duration and result — instead of separate start/done lines that interleave
// under concurrency.
type logRow struct {
	status  *gtk.Label // ▸ running · ✓ ok · ✗ error · – cancelled
	dur     *gtk.Label // live progress ("3/10") while running, duration when done
	note    *gtk.Label // result note (counts, errors); hidden while empty
	started time.Time
}

// buildStatusBar constructs the bottom status bar. It is activity-first: the
// left shows what the app is doing right now (spinner + label + elapsed time,
// with a progress bar that pulses for indeterminate work); cumulative session
// stats live behind the activity-log button so they don't masquerade as live
// state.
func (w *window) buildStatusBar() gtk.Widgetter {
	w.statusStarted = make(map[string]time.Time)
	w.statusProgText = make(map[string]string)
	w.statusLogRows = make(map[string][]*logRow)
	// Keep the relative idle text ("Synced 2 min ago") honest while nothing is
	// happening; a cheap label repaint twice a minute.
	glib.TimeoutSecondsAdd(30, func() bool {
		if len(w.statusActive) == 0 && !w.lastSyncAt.IsZero() {
			w.refreshStatusLabel()
		}
		return true
	})

	bar := gtk.NewBox(gtk.OrientationHorizontal, 8)
	bar.AddCSSClass("status-bar")
	setMargins(bar, 10, 8, 2, 2)

	w.statusSpinner = adw.NewSpinner()
	w.statusSpinner.SetVisible(false)

	w.statusLabel = gtk.NewLabel("Idle")
	w.statusLabel.SetXAlign(0)
	w.statusLabel.SetEllipsize(pango.EllipsizeEnd)
	w.statusLabel.AddCSSClass("dim-label")

	left := gtk.NewBox(gtk.OrientationHorizontal, 6)
	left.SetHExpand(true)
	left.Append(w.statusSpinner)
	left.Append(w.statusLabel)

	// Shown when AI requests are failing (e.g. the provider is unreachable), so
	// the user knows AI features are degraded; hidden again on the next success.
	w.aiWarnIcon = gtk.NewImageFromIconName("dialog-warning-symbolic")
	w.aiWarnIcon.AddCSSClass("warning")
	w.aiWarnIcon.SetTooltipText("AI provider unavailable — the last request failed")
	w.aiWarnIcon.SetVisible(false)

	bar.Append(left)
	bar.Append(w.aiWarnIcon)
	bar.Append(w.buildActivityLogButton())
	return bar
}

// buildActivityLogButton returns the "activity log" button and its popover,
// which holds the recent-operation log and a session-stats section.
func (w *window) buildActivityLogButton() gtk.Widgetter {
	w.statusLogBox = gtk.NewBox(gtk.OrientationVertical, 1)
	scroller := gtk.NewScrolledWindow()
	scroller.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroller.SetChild(w.statusLogBox)
	scroller.SetSizeRequest(440, 220)

	w.statusStatsLabel = gtk.NewLabel("")
	w.statusStatsLabel.SetXAlign(0)
	w.statusStatsLabel.SetWrap(true)
	w.statusStatsLabel.AddCSSClass("caption")
	w.statusStatsLabel.AddCSSClass("dim-label")

	content := gtk.NewBox(gtk.OrientationVertical, 4)
	setMargins(content, 8, 8, 8, 8)
	content.Append(heading("Activity"))
	content.Append(scroller)
	content.Append(gtk.NewSeparator(gtk.OrientationHorizontal))
	content.Append(heading("Session"))
	content.Append(w.statusStatsLabel)

	pop := gtk.NewPopover()
	pop.SetChild(content)
	pop.ConnectShow(w.refreshStatusStats) // stats are read on demand, not polled

	btn := gtk.NewMenuButton()
	btn.SetIconName("view-list-symbolic")
	btn.SetTooltipText("Activity log & stats")
	a11yLabel(btn, "Activity log and stats")
	btn.AddCSSClass("flat")
	btn.SetPopover(pop)
	w.statusLogBtn = btn
	return btn
}

// heading is a small bold section label for the popover.
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

// aiActivity reports an AI operation for the active account to the status bar;
// the returned function ends it (pass a note, e.g. a token count or "" ). The
// log note is suffixed with the model that served the request (failover-aware),
// so the log shows which chain entry answered. It also records AI health so the
// status bar can flag a failing provider. Safe when no hub is wired.
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

// emailByID resolves an account id to its email ("" when unknown).
func (w *window) emailByID(id int64) string {
	for _, a := range w.deps.Accounts {
		if a.ID == id {
			return a.Email
		}
	}
	return ""
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
// failure) and toggles the status-bar warning, recording the failure time so
// auto-categorization can back off (see categorizeInbox). A user-cancelled op
// is neutral — it neither marks the provider failing nor healthy.
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
			if w.aiWarnIcon != nil {
				w.aiWarnIcon.SetVisible(failed)
			}
		}
	})
}

// subscribeActivity drains the activity hub into the status bar (on the main
// thread). No-op when no hub is wired (read-only/no-account mode).
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

// onActivity updates the bar (and log) for one event. Main thread only.
func (w *window) onActivity(e activity.Event) {
	key := e.Op + "\x00" + e.Account + "\x00" + e.Label
	tag := w.accountTag(e.Account)
	disp := barText(e.Op, tag, e.Label)
	switch e.Phase {
	case activity.Start:
		w.statusActive = append(w.statusActive, disp)
		w.statusStarted[disp] = time.Now()
		// Concurrent identical ops queue up; Done finishes the oldest (FIFO), so
		// no row is ever orphaned in the running state.
		w.statusLogRows[key] = append(w.statusLogRows[key], w.newLogRow(e.Op, tag, e.Label))
		logging.Trace("ui: activity start", "op", e.Op, "label", e.Label, "active", len(w.statusActive))
	case activity.Progress:
		if e.Total > 0 {
			p := fmt.Sprintf("%d/%d", e.Done, e.Total)
			w.statusProgText[disp] = p
			if rows := w.statusLogRows[key]; len(rows) > 0 {
				rows[0].dur.SetText(p)
			}
		}
	case activity.Done:
		w.statusActive = removeFirst(w.statusActive, disp)
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
		}
		if e.Note != "" {
			row.note.SetText("· " + e.Note)
			row.note.SetVisible(true)
		}
		logging.Trace("ui: activity done", "op", e.Op, "label", e.Label, "dur", dur, "note", e.Note)
		delete(w.statusStarted, disp)
		delete(w.statusProgText, disp)
		if e.Op == "sync" {
			w.lastSyncAt = time.Now()
		}
	}
	w.refreshStatusLabel()
}

// refreshStatusLabel starts/stops the live-activity ticker and shows either the
// current operation or an idle line.
func (w *window) refreshStatusLabel() {
	if len(w.statusActive) > 0 {
		if w.activityTimer == 0 {
			w.statusSpinner.SetVisible(true) // AdwSpinner animates while visible
			w.activityTimer = glib.TimeoutAdd(120, w.tickActivity)
		}
		w.paintActivity()
		return
	}
	if w.activityTimer != 0 {
		glib.SourceRemove(w.activityTimer)
		w.activityTimer = 0
	}
	w.statusSpinner.SetVisible(false)
	if !w.lastSyncAt.IsZero() {
		w.statusLabel.SetText(syncedAgo(w.lastSyncAt, time.Now()))
	} else {
		w.statusLabel.SetText("Idle")
	}
}

// syncedAgo renders the idle status as a relative time ("Synced 00:15" read as
// a clock time; "2 min ago" doesn't). Older than an hour falls back to the
// absolute clock, which is more useful than "247 min ago".
func syncedAgo(t, now time.Time) string {
	switch d := now.Sub(t); {
	case d < time.Minute:
		return "Synced just now"
	case d < time.Hour:
		return fmt.Sprintf("Synced %d min ago", int(d.Minutes()))
	default:
		return "Synced at " + t.Format("15:04")
	}
}

// tickActivity repaints the live label while work is in flight; it returns false
// (removing itself) once everything is idle.
func (w *window) tickActivity() bool {
	if len(w.statusActive) == 0 {
		w.activityTimer = 0
		w.refreshStatusLabel()
		return false
	}
	w.paintActivity()
	return true
}

// paintActivity renders the current operation, any bounded progress, and the
// live elapsed time (the spinner conveys that it's ongoing).
func (w *window) paintActivity() {
	label := w.statusActive[len(w.statusActive)-1]
	text := label
	if p := w.statusProgText[label]; p != "" {
		text += " " + p
	}
	text += fmt.Sprintf("… %s", humanDuration(time.Since(w.statusStarted[label])))
	if extra := len(w.statusActive) - 1; extra > 0 {
		text += fmt.Sprintf("  (+%d more)", extra)
	}
	w.statusLabel.SetText(text)
}

// humanDuration formats an elapsed duration compactly: "0.4s", "12.3s", "2m05s".
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// refreshStatusStats fills the popover's session-stats section from deps.Stats.
// The gathering (a COUNT(*), an os.Stat, a recursive attachment-dir walk) runs
// off the main thread; only the label update dispatches back.
func (w *window) refreshStatusStats() {
	if w.deps.Stats == nil {
		w.statusStatsLabel.SetText("No connected account.")
		return
	}
	go func() {
		s := w.deps.Stats()
		var lines []string
		if s.Requests > 0 {
			lines = append(lines,
				fmt.Sprintf("API: %d requests · %d quota units (of 6000/min)", s.Requests, s.QuotaUnits),
				fmt.Sprintf("Transferred: ↓ %s · ↑ %s", humanBytes(s.BytesIn), humanBytes(s.BytesOut)))
		}
		if s.AIRequests > 0 {
			lines = append(lines,
				fmt.Sprintf("AI: %d requests · ↓ %s · ↑ %s", s.AIRequests, humanBytes(s.AIBytesIn), humanBytes(s.AIBytesOut)))
		}
		lines = append(lines, fmt.Sprintf("Cache: %s messages · DB %s", humanCount(s.Messages), humanBytes(s.DBBytes)))
		if s.CacheBytes > 0 {
			lines = append(lines, fmt.Sprintf("Attachments: %s", humanBytes(s.CacheBytes)))
		}
		logging.Trace("ui: refresh session stats", "requests", s.Requests, "quota", s.QuotaUnits,
			"bytes_in", s.BytesIn, "bytes_out", s.BytesOut,
			"ai_requests", s.AIRequests, "ai_bytes_in", s.AIBytesIn, "ai_bytes_out", s.AIBytesOut,
			"messages", s.Messages, "db_bytes", s.DBBytes, "cache_bytes", s.CacheBytes)
		text := strings.Join(lines, "\n")
		dispatch.Main(func() { w.statusStatsLabel.SetText(text) })
	}()
}

// newLogRow prepends one operation's row to the activity log (newest on top,
// capped at statusLogCap) and returns it for in-place updates. A single dense
// line per operation, the account as its own chip after the kind:
//
//	15:04:05 SYNC Work ✓ · 1 change(s)              1.2s
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
	}

	r.status = gtk.NewLabel("▸")
	r.status.AddCSSClass("log-time")
	r.status.SetWidthChars(1)

	lbl := gtk.NewLabel(label)
	lbl.SetXAlign(0)
	lbl.SetEllipsize(pango.EllipsizeEnd)
	tooltipWhenTruncated(lbl)

	r.note = gtk.NewLabel("")
	r.note.SetXAlign(0)
	r.note.SetHExpand(true)
	r.note.SetEllipsize(pango.EllipsizeEnd)
	r.note.AddCSSClass("log-note")
	r.note.SetVisible(false)
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
	box.Append(lbl)
	box.Append(r.note)
	box.Append(r.dur)

	w.statusLogBox.Prepend(box)
	w.statusLogLines++
	for w.statusLogLines > statusLogCap {
		if last := w.statusLogBox.LastChild(); last != nil {
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

// barText renders an in-flight operation for the bottom bar's label, where
// there are no chips: the op word, the account tag, and the terse label
// composed into one line ("Sync Work", "AI Work translate"). "mail" labels
// already read standalone ("Mark INBOX read"), so they get no op word.
func barText(op, account, label string) string {
	prefix := map[string]string{
		"sync": "Sync", "ai": "AI", "fetch": "Fetch", "send": "Send",
		"search": "Search", "attach": "Attachment", "draft": "Draft",
	}[op]
	return strings.Join(strings.Fields(prefix+" "+account+" "+label), " ")
}

// removeFirst removes the first occurrence of s from xs (order preserved).
func removeFirst(xs []string, s string) []string {
	for i, x := range xs {
		if x == s {
			return append(xs[:i], xs[i+1:]...)
		}
	}
	return xs
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
