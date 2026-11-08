package syncer

import (
	"sync"
	"time"

	"github.com/jsnjack/mailbox/internal/activity"
	"github.com/jsnjack/mailbox/internal/logging"
)

// Activity labels a sync pass publishes. A pass is "Checking mail" for as long
// as it is quick; a long catch-up moves its bulk work onto its own row.
const (
	LabelCheckingMail = "Checking mail"
	// LabelSyncingOther is the mail outside the inbox that a catch-up fetches
	// after the inbox is current.
	LabelSyncingOther = "Syncing other mail"
	// LabelSyncingRecent is the newest mail a full resync re-downloads.
	LabelSyncingRecent = "Syncing recent mail"
)

// progressEvery throttles progress updates: a backfill reports per message,
// and the status bar needs a few repaints a second, not hundreds.
const progressEvery = 250 * time.Millisecond

// progressRowMin is the smallest fetch that shows a done/total bar. A minute's
// worth of new mail finishes before a bar would mean anything, so ordinary
// passes stay a plain spinner.
const progressRowMin = 20

// Progress shows one sync pass in the activity hub. Exactly one of its rows is
// open at a time, so the status bar names what the pass is doing now — reading
// the change log, fetching the inbox, then the bulk of a long catch-up — and the
// log keeps one line per phase. A nil *Progress reports nothing, which is what
// tests and the headless sync command pass.
type Progress struct {
	act     *activity.Hub
	account string

	mu      sync.Mutex
	label   string
	end     func(note string)
	note    string
	lastPub time.Time
}

// NewProgress opens a pass's "Checking mail" row for account.
func NewProgress(act *activity.Hub, account string) *Progress {
	p := &Progress{act: act, account: account}
	p.label = LabelCheckingMail
	p.end = act.Begin("sync", account, LabelCheckingMail)
	return p
}

// Phase ends the open row with closeNote and opens label in its place.
func (p *Progress) Phase(label, closeNote string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.label == label {
		return
	}
	logging.Trace("syncer: progress phase", "account", p.account, "from", p.label, "to", label, "note", closeNote)
	if p.end != nil {
		p.end(closeNote)
	}
	p.label = label
	p.note = ""
	p.lastPub = time.Time{}
	p.end = p.act.Begin("sync", p.account, label)
}

// Step reports the open row's position: done of total units (see
// activity.Event.Unit). Updates are throttled; the final one always shows.
func (p *Progress) Step(done, total int, unit string) {
	if p == nil || total <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if done < total && now.Sub(p.lastPub) < progressEvery {
		return
	}
	p.lastPub = now
	p.act.Publish(activity.Event{Op: "sync", Phase: activity.Progress, Account: p.account, Label: p.label, Done: done, Total: total, Unit: unit})
}

// Note sets what the open row ends with when the pass succeeds.
func (p *Progress) Note(note string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.note = note
}

// Finish ends the open row: with the error when err is set, else with the note
// the row was given, else with fallback. It is safe to call more than once.
func (p *Progress) Finish(err error, fallback string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.end == nil {
		return
	}
	note := fallback
	switch {
	case err != nil:
		note = "error: " + err.Error()
	case p.note != "":
		note = p.note
	}
	p.end(note)
	p.end = nil
}
