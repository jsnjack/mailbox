package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/jsnjack/mailbox/internal/activity"
	"github.com/jsnjack/mailbox/internal/dispatch"
)

// statusLogCap bounds how many recent activity lines the log popover keeps.
const statusLogCap = 200

// buildStatusBar constructs the bottom status bar. It is activity-first: the
// left shows what the app is doing right now (spinner + label + elapsed time,
// with a progress bar that pulses for indeterminate work); cumulative session
// stats live behind the activity-log button so they don't masquerade as live
// state.
func (w *window) buildStatusBar() gtk.Widgetter {
	w.statusStarted = make(map[string]time.Time)
	w.statusProgText = make(map[string]string)

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
	scroller.SetSizeRequest(400, 240)

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

// doneErr summarizes an operation result for the activity log note.
func doneErr(err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return ""
}

// aiActivity reports an AI operation to the status bar; the returned function
// ends it (pass a note, e.g. a token count or "" ). It also records AI health so
// the status bar can flag a failing provider. Safe when no hub is wired.
func (w *window) aiActivity(label string) func(note string) {
	var end func(string)
	if w.deps.Activity != nil {
		end = w.deps.Activity.Begin("ai", label)
	}
	return func(note string) {
		if end != nil {
			end(note)
		}
		w.noteAIResult(note)
	}
}

// noteAIResult reads an AI op's completion note (doneErr yields "error: …" on
// failure) and toggles the status-bar warning, recording the failure time so
// auto-categorization can back off (see categorizeInbox).
func (w *window) noteAIResult(note string) {
	failed := strings.HasPrefix(note, "error:")
	dispatch.Main(func() {
		if failed {
			w.aiFailedAt = time.Now()
		}
		if w.aiFailing != failed {
			w.aiFailing = failed
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
	switch e.Phase {
	case activity.Start:
		w.statusActive = append(w.statusActive, e.Label)
		w.statusStarted[e.Label] = time.Now()
		w.appendLogLine("▸ " + e.Label)
	case activity.Progress:
		if e.Total > 0 {
			w.statusProgText[e.Label] = fmt.Sprintf("%d/%d", e.Done, e.Total)
		}
	case activity.Done:
		w.statusActive = removeFirst(w.statusActive, e.Label)
		// "✓ Label — 1.2s · note" (duration only when we saw the matching Start;
		// a Start published before the UI subscribed would otherwise read bogus).
		line := e.Label
		if t, ok := w.statusStarted[e.Label]; ok {
			line += " — " + humanDuration(time.Since(t))
		}
		if e.Note != "" {
			line += " · " + e.Note
		}
		w.appendLogLine("✓ " + line)
		delete(w.statusStarted, e.Label)
		delete(w.statusProgText, e.Label)
		if e.Op == "sync" {
			w.lastSyncLabel = "Synced " + time.Now().Format("15:04")
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
	if w.lastSyncLabel != "" {
		w.statusLabel.SetText(w.lastSyncLabel)
	} else {
		w.statusLabel.SetText("Idle")
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
func (w *window) refreshStatusStats() {
	if w.deps.Stats == nil {
		w.statusStatsLabel.SetText("No connected account.")
		return
	}
	s := w.deps.Stats()
	var lines []string
	if s.Requests > 0 {
		lines = append(lines,
			fmt.Sprintf("API: %d requests · %d quota units (of 6000/min)", s.Requests, s.QuotaUnits),
			fmt.Sprintf("Transferred: ↓ %s · ↑ %s", humanBytes(s.BytesIn), humanBytes(s.BytesOut)))
	}
	lines = append(lines, fmt.Sprintf("Cache: %s messages · DB %s", humanCount(s.Messages), humanBytes(s.DBBytes)))
	if s.CacheBytes > 0 {
		lines = append(lines, fmt.Sprintf("Attachments: %s", humanBytes(s.CacheBytes)))
	}
	w.statusStatsLabel.SetText(strings.Join(lines, "\n"))
}

// appendLogLine prepends a timestamped line to the activity log, capping the count.
func (w *window) appendLogLine(text string) {
	row := gtk.NewLabel(time.Now().Format("15:04:05") + "  " + text)
	row.SetXAlign(0)
	row.SetEllipsize(pango.EllipsizeEnd)
	row.AddCSSClass("caption")
	w.statusLogBox.Prepend(row)
	w.statusLogLines++
	for w.statusLogLines > statusLogCap {
		if last := w.statusLogBox.LastChild(); last != nil {
			w.statusLogBox.Remove(last)
			w.statusLogLines--
		} else {
			break
		}
	}
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
