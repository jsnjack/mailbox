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
	"github.com/jsnjack/mailbox/internal/logging"
)

// activityRow is the app's one activity surface: an inset card mounted in a
// window's bottom bar (adw.ToolbarView.AddBottomBar), following Nautilus'
// operations row and its timings. The main window keeps one permanently at the
// bottom of the sidebar, fed from the activity hub; a dialog mounts its own,
// driven directly by the work that dialog started, so the window you are
// looking at is the one that reports.
//
// The row shows exactly one of three things:
//
//	●  resting     — the health dot and a quiet line ("Up to date · 11:55")
//	◐  running     — spinner, the gerund phrase, live elapsed, optional bar
//	✓  finished    — the past-tense result, flashed once, held for 3 seconds
//
// Nautilus' behaviour is followed where it fits: the accent flash on finishing,
// the spinner→checkmark crossfade, and a linger that keeps re-arming while
// nobody could have seen the result. Reveal-on-demand is used only by dialogs;
// the main window's row is permanent, because "when did this last sync" is
// worth answering even when nothing is happening.
type activityRow struct {
	bar      *gtk.Box // mount this via ToolbarView.AddBottomBar
	button   *gtk.Button
	icons    *gtk.Stack
	dot      *gtk.Label
	spinner  *adw.Spinner
	doneIcon *gtk.Image
	label    *gtk.Label
	detail   *gtk.Label
	progress *gtk.ProgressBar

	// reveal is ToolbarView.SetRevealBottomBars for a transient row (dialogs);
	// nil for the main window's permanent row.
	reveal    func(bool)
	transient bool

	// active holds in-flight operations, most recent last — the newest owns the
	// visible phrase, and finishing it falls back to the one underneath.
	active  []*activityOp
	finish  *finishState
	phase   rowPhase
	resting func() string // the resting line, supplied by the owner

	tick    glib.SourceHandle
	linger  glib.SourceHandle
	flashID glib.SourceHandle

	// health tints the resting dot: an amber light while a provider is failing
	// is the whole of that state's UI, so no separate warning icon is needed.
	health rowHealth
	// popupVisible reports whether an attached log panel is open, so the linger
	// can wait for the user to look away. nil when there is no panel.
	popupVisible func() bool
	// windowActive reports whether the row's window has focus; a result that
	// lands while the user is elsewhere waits for them.
	windowActive func() bool
}

// activityOp is one running operation owned by a row.
type activityOp struct {
	label string // the phrase shown while it runs, with the account it is for
	// plain is the phrase without the account: the result is announced without
	// it, because "Mail checked · Home · 2 changes" does not fit a sidebar and
	// the account is what matters least once the work is over — the log keeps
	// its own chip for that.
	plain   string
	op      string // hub category ("sync", "ai", …); "" for a directly-driven op
	started time.Time
	prog    float64 // determinate fraction, or <0 while indeterminate
	done    bool
}

// finishState is a settled result waiting to be shown (and then to expire).
type finishState struct {
	text  string
	ok    bool
	flash bool
}

type rowPhase int

const (
	phaseResting rowPhase = iota
	phaseRunning
	phaseFinished
)

type rowHealth int

const (
	healthOK rowHealth = iota
	healthWarning
)

// Nautilus' timings (nautilus-progress-indicator.c), reused so the two apps
// behave identically.
const (
	rowAttentionMS   = 2000 // needs-attention animation
	rowLingerSeconds = 3    // how long a finished result is held
	rowMorphMS       = 500  // spinner → checkmark crossfade
	rowTickMS        = 120  // live elapsed repaint
	lingerMaxWaits   = 10   // how many times the linger waits for a viewer
	// flashMinDuration is how long an operation must have run before its result
	// is worth flashing the accent for. See worthFlashing.
	flashMinDuration = 800 * time.Millisecond
)

// newActivityRow builds the row. resting supplies the line shown when nothing
// is running (it is re-read on every repaint, so it can age); a transient row
// hides itself instead and may pass nil.
func newActivityRow(resting func() string) *activityRow {
	r := &activityRow{resting: resting}

	r.dot = gtk.NewLabel("●")
	r.dot.AddCSSClass("status-dot")
	r.spinner = adw.NewSpinner()
	r.spinner.SetSizeRequest(14, 14)
	r.doneIcon = gtk.NewImageFromIconName("object-select-symbolic")
	r.doneIcon.SetPixelSize(14)

	// A stack, not a shown/hidden spinner: a hidden GTK widget takes no space,
	// so swapping icons in place is what keeps the phrase from jumping sideways
	// every time a sync starts.
	r.icons = gtk.NewStack()
	r.icons.AddNamed(r.dot, "rest")
	r.icons.AddNamed(r.spinner, "run")
	r.icons.AddNamed(r.doneIcon, "done")
	r.icons.SetTransitionType(gtk.StackTransitionTypeCrossfade)
	r.icons.SetTransitionDuration(rowMorphMS)
	r.icons.SetSizeRequest(15, 15)
	r.icons.SetVAlign(gtk.AlignCenter)

	r.label = gtk.NewLabel("")
	r.label.SetXAlign(0)
	r.label.SetHExpand(true)
	r.label.SetEllipsize(pango.EllipsizeEnd)
	tooltipWhenTruncated(r.label)

	r.detail = gtk.NewLabel("")
	r.detail.AddCSSClass("status-detail")
	r.detail.SetVisible(false)

	line := gtk.NewBox(gtk.OrientationHorizontal, 8)
	line.Append(r.icons)
	line.Append(r.label)
	line.Append(r.detail)

	r.progress = gtk.NewProgressBar()
	r.progress.AddCSSClass("status-progress")
	r.progress.SetVisible(false)

	body := gtk.NewBox(gtk.OrientationVertical, 3)
	body.SetHExpand(true)
	body.Append(line)
	body.Append(r.progress)

	// A plain button, not a GtkMenuButton: a menu button with neither popover
	// nor menu model makes its own internal button insensitive, which renders a
	// dialog's panel-less row in disabled grey.
	r.button = gtk.NewButton()
	r.button.SetChild(body)
	r.button.AddCSSClass("flat")
	r.button.AddCSSClass("status-row")
	// The card spans the pane rather than hugging its phrase: a width that
	// changed with every state would make the corner of the window restless,
	// and it gives the label somewhere to ellipsize instead of widening the
	// sidebar to fit "Categorizing 12 conversations".
	r.button.SetHExpand(true)
	a11yLabel(r.button, "Activity")

	r.bar = gtk.NewBox(gtk.OrientationHorizontal, 0)
	r.bar.Append(r.button)

	r.repaint()
	return r
}

// mount attaches the row as tv's bottom bar. A transient row starts hidden and
// reveals itself only while it has something to say.
func (r *activityRow) mount(tv *adw.ToolbarView, transient bool) {
	tv.AddBottomBar(r.bar)
	r.transient = transient
	if transient {
		tv.SetRevealBottomBars(false)
		r.reveal = tv.SetRevealBottomBars
		// Without a log to open, the row is a readout rather than a control: it
		// takes no clicks and no focus. Insensitive would be wrong — that dims
		// the text, and there is nothing disabled about a status line.
		r.button.SetCanTarget(false)
		r.button.SetCanFocus(false)
	}
}

// setPanel gives the row a popover (the activity log + session stats) opened by
// clicking it.
func (r *activityRow) setPanel(pop *gtk.Popover) {
	pop.SetPosition(gtk.PosTop) // the row is at the bottom of its pane
	pop.SetParent(r.button)
	r.button.ConnectClicked(pop.Popup)
	r.popupVisible = pop.IsVisible
	a11yLabel(r.button, "Activity log and session statistics")
	pop.ConnectClosed(func() {
		// The result has now been seen; give it the usual grace and go.
		if r.phase == phaseFinished && r.linger == 0 {
			r.scheduleHide()
		}
	})
}

// setHealth tints the resting dot. Returns true when the state changed.
func (r *activityRow) setHealth(h rowHealth) bool {
	if r.health == h {
		return false
	}
	r.health = h
	r.repaint()
	return true
}

// begin starts a directly-driven operation (a dialog reporting its own work)
// and returns the function that ends it, taking the same result note the
// activity hub uses.
func (r *activityRow) begin(label string) func(note string) {
	op := r.start("", label, label)
	return func(note string) { r.stop(op, "", note) }
}

// feed applies one activity-hub event. display is the phrase for the running
// state (the operation and the account it runs for). Main thread only.
func (r *activityRow) feed(e activity.Event, display string) {
	switch e.Phase {
	case activity.Start:
		r.start(e.Op, display, e.Label)
	case activity.Progress:
		if e.Total > 0 {
			if op := r.find(display); op != nil {
				op.prog = float64(e.Done) / float64(e.Total)
				r.repaint()
			}
		}
	case activity.Done:
		r.stop(r.find(display), e.Op, e.Note)
	}
}

// start records a new operation and shows it.
func (r *activityRow) start(op, label, plain string) *activityOp {
	a := &activityOp{label: label, plain: plain, op: op, started: time.Now(), prog: -1}
	r.active = append(r.active, a)
	r.finish = nil // superseded: new work outranks an old result
	r.unschedule()
	r.repaint()
	return a
}

// find returns the oldest unfinished operation with this label, so concurrent
// identical operations finish in the order they started.
func (r *activityRow) find(label string) *activityOp {
	for _, a := range r.active {
		if a.label == label && !a.done {
			return a
		}
	}
	return nil
}

// stop settles an operation. A nil op (a Report — an instant, already-finished
// operation, or one whose Start predates the row) still produces a result.
func (r *activityRow) stop(a *activityOp, op, note string) {
	label := ""
	var ran time.Duration
	if a != nil {
		a.done = true
		label = a.plain
		op = a.op
		ran = time.Since(a.started)
		for i, x := range r.active {
			if x == a {
				r.active = append(r.active[:i], r.active[i+1:]...)
				break
			}
		}
	}
	if notableFinish(op, note) {
		ok := !strings.HasPrefix(note, "error:")
		r.finish = &finishState{
			text:  finishText(label, note),
			ok:    ok,
			flash: worthFlashing(ok, a != nil, ran),
		}
	}
	r.repaint()
}

// reportDone shows a completed operation that never had a running phase.
func (r *activityRow) reportDone(op, label, note string) {
	if !notableFinish(op, note) {
		return
	}
	// An instant report is something that happened to the app rather than work
	// it was watched doing — a snooze waking, a queued message going out — so it
	// always earns the flash.
	r.finish = &finishState{
		text:  finishText(label, note),
		ok:    !strings.HasPrefix(note, "error:"),
		flash: true,
	}
	r.repaint()
}

// worthFlashing decides whether a finished operation gets the accent flash. A
// failure always does. Otherwise the flash is spent only on work that ran long
// enough to have been noticed running: archiving a conversation settles in a
// fraction of a second and already has its own undo toast, so triaging an inbox
// with the "a" key would otherwise strobe the corner of the window.
func worthFlashing(ok, hadRunningPhase bool, ran time.Duration) bool {
	if !ok || !hadRunningPhase {
		return true
	}
	return ran >= flashMinDuration
}

// repaint renders whichever of the three states currently applies, and starts
// or stops the machinery each one needs.
func (r *activityRow) repaint() {
	switch {
	case len(r.active) > 0:
		r.enterRunning()
	case r.finish != nil:
		r.enterFinished()
	default:
		r.enterResting()
	}
}

func (r *activityRow) enterRunning() {
	a := r.active[len(r.active)-1]
	r.icons.SetVisibleChildName("run")
	text := a.label
	if extra := len(r.active) - 1; extra > 0 {
		text += fmt.Sprintf("  (+%d more)", extra)
	}
	r.label.SetText(text)
	// Elapsed rides in its own label so the ticking digits never reflow the
	// phrase beside them.
	r.detail.SetVisible(true)
	r.detail.SetText(humanDuration(time.Since(a.started)))
	r.progress.SetVisible(a.prog >= 0)
	if a.prog >= 0 {
		r.progress.SetFraction(a.prog)
	}
	if r.phase != phaseRunning {
		r.phase = phaseRunning
		r.show(true)
	}
	if r.tick == 0 {
		r.tick = glib.TimeoutAdd(rowTickMS, r.onTick)
	}
}

func (r *activityRow) enterFinished() {
	r.stopTick()
	r.progress.SetVisible(false)
	r.detail.SetVisible(false)
	icon := "object-select-symbolic"
	if !r.finish.ok {
		icon = "process-stop-symbolic"
	}
	r.doneIcon.SetFromIconName(icon)
	if r.finish.ok {
		r.doneIcon.RemoveCSSClass("error")
	} else {
		r.doneIcon.AddCSSClass("error")
	}
	r.icons.SetVisibleChildName("done")
	r.label.SetText(r.finish.text)
	if r.phase != phaseFinished {
		r.phase = phaseFinished
		r.show(true)
		if r.finish.flash {
			r.flash()
		}
		r.scheduleHide()
	}
}

func (r *activityRow) enterResting() {
	r.stopTick()
	r.unschedule()
	r.progress.SetVisible(false)
	r.detail.SetVisible(false)
	r.icons.SetVisibleChildName("rest")
	if r.health == healthWarning {
		r.dot.AddCSSClass("warning")
	} else {
		r.dot.RemoveCSSClass("warning")
	}
	if r.resting != nil {
		r.label.SetText(r.resting())
	} else {
		r.label.SetText("")
	}
	r.phase = phaseResting
	r.show(!r.transient)
}

// refreshResting repaints the resting line so a relative time stays honest
// while nothing is happening.
func (r *activityRow) refreshResting() {
	if r.phase == phaseResting {
		r.enterResting()
	}
}

func (r *activityRow) show(on bool) {
	if r.reveal != nil {
		r.reveal(on)
	}
}

func (r *activityRow) onTick() bool {
	if len(r.active) == 0 {
		r.tick = 0
		return false
	}
	a := r.active[len(r.active)-1]
	r.detail.SetText(humanDuration(time.Since(a.started)))
	return true
}

func (r *activityRow) stopTick() {
	if r.tick != 0 {
		glib.SourceRemove(r.tick)
		r.tick = 0
	}
}

// scheduleHide arms the linger. The timer re-arms while the window is
// unfocused or the log panel is open (Nautilus' has-viewers rule), so a result
// that arrived while the user was elsewhere is still there when they look —
// but only for lingerMaxWaits. Waiting for a viewer who never comes is how a
// stale result pins the row forever, which is exactly what happens wherever
// focus is never reported (a bare X server, some tiling window managers).
func (r *activityRow) scheduleHide() {
	r.unschedule()
	waits := 0
	r.linger = glib.TimeoutSecondsAdd(rowLingerSeconds, func() bool {
		if waits < lingerMaxWaits && r.watched() {
			waits++
			return true
		}
		r.linger = 0
		r.finish = nil
		r.repaint()
		return false
	})
}

// watched reports whether the result still needs to wait to be seen.
func (r *activityRow) watched() bool {
	if r.popupVisible != nil && r.popupVisible() {
		return true
	}
	return r.windowActive != nil && !r.windowActive()
}

func (r *activityRow) unschedule() {
	if r.linger != 0 {
		glib.SourceRemove(r.linger)
		r.linger = 0
	}
}

// flash replays Nautilus' needs-attention animation: re-add the class, then
// drop it once the animation has run so the next finish can restart it.
func (r *activityRow) flash() {
	if r.flashID != 0 {
		glib.SourceRemove(r.flashID)
	}
	r.button.RemoveCSSClass("needs-attention")
	r.button.AddCSSClass("needs-attention")
	r.flashID = glib.TimeoutAdd(rowAttentionMS, func() bool {
		r.flashID = 0
		r.button.RemoveCSSClass("needs-attention")
		return false
	})
}

// destroy releases the row's timers. A dialog's row outlives its window
// otherwise, and its callbacks would touch disposed widgets.
func (r *activityRow) destroy() {
	r.stopTick()
	r.unschedule()
	if r.flashID != 0 {
		glib.SourceRemove(r.flashID)
		r.flashID = 0
	}
	logging.Trace("ui: activity row destroyed")
}

// notableFinish reports whether a finished operation deserves the row's
// finished state — the checkmark, the accent flash and the three-second hold —
// rather than dropping straight back to the resting line. The minute-by-minute
// mail check that found nothing is the app breathing, not an event, and
// flashing for it would spend the one moment of colour the row has on nothing.
// A cancelled operation is silent for the same reason: the user caused it.
func notableFinish(op, note string) bool {
	switch {
	case note == noteCancelled:
		return false
	case strings.HasPrefix(note, "error:"):
		return true
	case op == "sync" && (note == "" || note == activity.NoteUpToDate):
		return false
	default:
		return true
	}
}

// finishText renders a finished operation for the row. Success reads in past
// tense with its result appended; a failure keeps the original phrase and says
// so, because "Thread summarized · connection refused" would claim work that
// did not happen.
func finishText(label, note string) string {
	if reason, bad := strings.CutPrefix(note, "error: "); bad {
		if label == "" {
			return reason
		}
		return label + " failed · " + reason
	}
	past := activity.PastTense(label)
	switch {
	case past == "":
		return note
	case note == "":
		return past
	default:
		return past + " · " + note
	}
}

// restingText renders the row's line when nothing is running: what the app's
// state is, then when it was established. State first, so the line reads as a
// condition rather than as an event scrolling away — and so it stops changing
// width as the minutes pass.
func restingText(last, now time.Time, failing bool) string {
	if failing {
		return "AI provider unavailable"
	}
	if last.IsZero() {
		return "Not synced yet"
	}
	if d := now.Sub(last); d < 90*time.Second {
		return "Up to date · just now"
	}
	return "Up to date · " + last.Format("15:04")
}
