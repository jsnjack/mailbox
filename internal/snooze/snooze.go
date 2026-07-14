// Package snooze mirrors snoozed conversations to the mail provider as label
// state, so a snooze made on one machine holds everywhere: the thread leaves
// the inbox on every client (including the Gmail phone app), a stable
// "Snoozed" label keeps it findable there, and a hidden "Snoozed/<wake time>"
// child label carries the exact wake moment so ANY machine running this app —
// not just the one that snoozed — wakes it on schedule.
//
// The local snoozes table stays the source of truth for this machine's UI
// (instant hide/show); the labels are its provider mirror. Reconcile, run
// after every sync pass, converges the two in both directions:
//
//	labels → rows  adopt snoozes made elsewhere (exact wake time from the
//	               label); a thread back in INBOX (woken elsewhere, unsnoozed
//	               from the phone, or re-inboxed by a new reply) cancels the
//	               local snooze and strips leftover labels.
//	rows → labels  push snoozes the provider doesn't know about (rows from
//	               before mirroring existed, or a mirror lost to an offline
//	               window).
//
// Backends without label management (backend.LabelManager) keep the old
// local-only behavior. All provider writes go through the engine's ordered
// per-account mirror queue, so a snooze and its undo can't race.
package snooze

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jsnjack/mailbox/internal/activity"
	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
	"github.com/jsnjack/mailbox/internal/syncer"
)

// stampLayout encodes a wake time in a label name: local time with a numeric
// zone offset, so it reads naturally in other clients ("Snoozed/2026-07-18
// 14:30 +0200") while parsing back to the exact instant on any machine in any
// timezone.
const stampLayout = "2006-01-02 15:04 -0700"

// Stamp renders a wake time as its mirror label name.
func Stamp(t time.Time) string {
	return model.SnoozeLabelPrefix + t.Format(stampLayout)
}

// parseStamp extracts the wake time from a mirror label name. ok is false for
// the root label and anything foreign that merely shares the prefix.
func parseStamp(name string) (time.Time, bool) {
	if len(name) <= len(model.SnoozeLabelPrefix) || name[:len(model.SnoozeLabelPrefix)] != model.SnoozeLabelPrefix {
		return time.Time{}, false
	}
	t, err := time.Parse(stampLayout, name[len(model.SnoozeLabelPrefix):])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Manager owns snooze state: local rows plus their provider label mirror.
type Manager struct {
	St     *store.Store
	Engine *syncer.Engine
	Hub    *syncer.Hub
	Act    *activity.Hub
	// BackendFor returns the live backend for an account (nil when it isn't
	// running); EmailOf resolves an account id for activity rows.
	BackendFor func(accountID int64) backend.Backend
	EmailOf    func(accountID int64) string

	// labelCache remembers ensured label ids per "account|name" so a bulk
	// snooze doesn't re-list the account's labels once per thread.
	labelMu    sync.Mutex
	labelCache map[string]model.Label
}

// labelManagerFor returns the account's backend and its label-management
// capability (nil, nil when the account can't mirror — not running, or IMAP).
func (m *Manager) labelManagerFor(accountID int64) (backend.Backend, backend.LabelManager) {
	b := m.BackendFor(accountID)
	if b == nil {
		return nil, nil
	}
	lm, ok := b.(backend.LabelManager)
	if !ok {
		return b, nil
	}
	return b, lm
}

// ensureLabel resolves name to the provider's label, creating it when absent,
// and shadows it into the local labels table so reconciliation and the mirror
// queries see it before the next full label sync.
func (m *Manager) ensureLabel(ctx context.Context, accountID int64, lm backend.LabelManager, name string, hidden bool) (model.Label, error) {
	key := fmt.Sprintf("%d|%s", accountID, name)
	m.labelMu.Lock()
	if l, ok := m.labelCache[key]; ok {
		m.labelMu.Unlock()
		return l, nil
	}
	m.labelMu.Unlock()
	l, err := lm.EnsureLabel(ctx, name, hidden)
	if err != nil {
		return model.Label{}, err
	}
	if err := m.St.UpsertLabel(ctx, l); err != nil {
		return model.Label{}, err
	}
	m.labelMu.Lock()
	if m.labelCache == nil {
		m.labelCache = map[string]model.Label{}
	}
	m.labelCache[key] = l
	m.labelMu.Unlock()
	return l, nil
}

// forgetLabel drops a deleted label from the ensure cache.
func (m *Manager) forgetLabel(accountID int64, name string) {
	m.labelMu.Lock()
	delete(m.labelCache, fmt.Sprintf("%d|%s", accountID, name))
	m.labelMu.Unlock()
}

// threadMessageIDs resolves a thread to its cached provider message ids.
func (m *Manager) threadMessageIDs(ctx context.Context, accountID int64, threadID string) ([]string, error) {
	msgs, err := m.St.ListThreadMessages(ctx, accountID, threadID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(msgs))
	for i, msg := range msgs {
		ids[i] = msg.GmailID
	}
	return ids, nil
}

// Snooze hides the thread until t everywhere: the local row hides it here
// instantly, then the provider mirror (−INBOX, +Snoozed, +Snoozed/<stamp>)
// hides it on every other client and carries the wake time to other machines.
// A mirror that can't run now (offline, no label support) is not an error —
// Reconcile pushes it out later.
func (m *Manager) Snooze(ctx context.Context, accountID int64, threadID string, t time.Time) error {
	// Minute precision: the stamp label encodes minutes (and the sweeper ticks
	// per minute), so truncating keeps the local row and the mirror label
	// naming the identical instant — reconciliation then never sees a
	// sub-minute mismatch as a remote re-snooze.
	t = t.Truncate(time.Minute)
	logging.TraceContext(ctx, "snooze: snooze", "account", accountID, "thread", threadID, "until", t.Unix())
	if err := m.St.SnoozeThread(ctx, accountID, threadID, t.Unix()); err != nil {
		return err
	}
	note := ""
	if err := m.mirrorSnooze(ctx, accountID, threadID, t); err != nil {
		// The local snooze holds either way. An account that can't mirror
		// (IMAP, read-only) is local-only by design — no note; a transient
		// mirror failure self-heals on a later Reconcile and says so.
		logging.TraceContext(ctx, "snooze: mirror deferred", "account", accountID, "thread", threadID, "err", err)
		if !errors.Is(err, errNoMirror) {
			note = "local only — will sync: " + err.Error()
		}
	} else if err := m.St.MarkSnoozeMirrored(ctx, accountID, threadID); err != nil {
		return err
	}
	m.Act.Report("mail", m.EmailOf(accountID), "Snoozed until "+t.Format("Jan 2 15:04"), note)
	return nil
}

// errNoMirror marks an account that cannot mirror snoozes at all (its backend
// lacks label management), as opposed to a transient mirror failure.
var errNoMirror = errors.New("account can't mirror labels")

// mirrorSnooze pushes one snooze to the provider: ensures the mirror labels
// and applies (+root +stamp, −INBOX, −any previous stamp) to the thread.
func (m *Manager) mirrorSnooze(ctx context.Context, accountID int64, threadID string, t time.Time) error {
	b, lm := m.labelManagerFor(accountID)
	if lm == nil {
		return errNoMirror
	}
	ids, err := m.threadMessageIDs(ctx, accountID, threadID)
	if err != nil || len(ids) == 0 {
		return fmt.Errorf("resolve thread messages: %w", err)
	}
	root, err := m.ensureLabel(ctx, accountID, lm, model.SnoozeLabelRoot, false)
	if err != nil {
		return err
	}
	stamp, err := m.ensureLabel(ctx, accountID, lm, Stamp(t), true)
	if err != nil {
		return err
	}
	add := []string{root.GmailID, stamp.GmailID}
	remove := []string{model.LabelInbox}
	// A re-snooze replaces the previous wake-time label in the same change.
	prev, err := m.St.ThreadSnoozeLabels(ctx, accountID, threadID)
	if err == nil {
		for _, l := range prev {
			if l.GmailID != root.GmailID && l.GmailID != stamp.GmailID {
				remove = append(remove, l.GmailID)
			}
		}
	}
	if err := m.Engine.ModifyLabelsBatch(ctx, b, accountID, ids, add, remove); err != nil {
		return err
	}
	m.cleanupStamps(accountID, lm, prev, stamp.GmailID)
	return nil
}

// Unsnooze returns the thread to the inbox everywhere, now: the row is
// dropped and the mirror restores INBOX and strips the snooze labels.
func (m *Manager) Unsnooze(ctx context.Context, accountID int64, threadID string) error {
	logging.TraceContext(ctx, "snooze: unsnooze", "account", accountID, "thread", threadID)
	if err := m.St.UnsnoozeThread(ctx, accountID, threadID); err != nil {
		return err
	}
	m.restoreThread(ctx, accountID, threadID)
	m.Act.Report("mail", m.EmailOf(accountID), "Unsnoozed", "")
	return nil
}

// restoreThread mirrors a thread's return to the inbox (+INBOX, −snooze
// labels) and cleans up wake-time labels that no thread uses anymore.
// Mirror-less accounts no-op (the local row change was the whole story).
func (m *Manager) restoreThread(ctx context.Context, accountID int64, threadID string) {
	b, lm := m.labelManagerFor(accountID)
	if lm == nil {
		return
	}
	ids, err := m.threadMessageIDs(ctx, accountID, threadID)
	if err != nil || len(ids) == 0 {
		return
	}
	labels, err := m.St.ThreadSnoozeLabels(ctx, accountID, threadID)
	if err != nil {
		logging.TraceContext(ctx, "snooze: restore labels lookup failed", "account", accountID, "thread", threadID, "err", err)
	}
	remove := make([]string, 0, len(labels))
	for _, l := range labels {
		remove = append(remove, l.GmailID)
	}
	if err := m.Engine.ModifyLabelsBatch(ctx, b, accountID, ids, []string{model.LabelInbox}, remove); err != nil {
		logging.TraceContext(ctx, "snooze: restore mirror failed", "account", accountID, "thread", threadID, "err", err)
		return
	}
	m.cleanupStamps(accountID, lm, labels, "")
}

// cleanupStamps deletes wake-time labels (never the root) that no longer tag
// any message, keeping the label list tidy in other clients. It runs on the
// account's ordered mirror queue so the check happens only after the modify
// that removed the label from its last thread has reached the provider; keep
// (still shared by another thread) and already-gone are both fine. keepID
// exempts the stamp just applied by a re-snooze.
func (m *Manager) cleanupStamps(accountID int64, lm backend.LabelManager, labels []model.Label, keepID string) {
	for _, l := range labels {
		if l.GmailID == keepID || l.Name == model.SnoozeLabelRoot {
			continue
		}
		l := l
		m.Engine.MirrorOp(accountID, func() {
			ctx, cancel := syncer.MirrorCtx()
			defer cancel()
			if err := lm.DeleteLabelIfUnused(ctx, l.GmailID); err != nil {
				logging.Trace("snooze: stamp cleanup failed", "account", accountID, "label", l.Name, "err", err)
				return
			}
			m.forgetLabel(accountID, l.Name)
			// Best-effort local shadow; a stale row is harmless (hidden from
			// the sidebar) and the next label sync settles it.
			if err := m.St.DeleteLabel(context.Background(), accountID, l.GmailID); err != nil {
				logging.Trace("snooze: stamp local delete failed", "account", accountID, "label", l.Name, "err", err)
			}
		})
	}
}

// WakeDue wakes every snooze that has come due: the row is marked notified
// (keeping the lingering "Snoozed" tag), the thread returns to the inbox
// everywhere via the mirror, and a SnoozeWoke change drives the UI refresh
// and reminder notification. Racing another machine is safe — label adds and
// removes are idempotent and both reminders firing is what a reminder is for.
func (m *Manager) WakeDue(ctx context.Context, now time.Time) int {
	due, err := m.St.DueSnoozes(ctx, now.Unix())
	if err != nil {
		logging.TraceContext(ctx, "snooze: list due failed", "err", err)
		return 0
	}
	woke := 0
	for _, sn := range due {
		if err := m.St.MarkSnoozeNotified(ctx, sn.AccountID, sn.ThreadID); err != nil {
			logging.TraceContext(ctx, "snooze: mark notified failed", "thread", sn.ThreadID, "err", err)
			continue
		}
		m.restoreThread(ctx, sn.AccountID, sn.ThreadID)
		m.Hub.Publish(syncer.Change{Kind: syncer.SnoozeWoke, AccountID: sn.AccountID, ThreadID: sn.ThreadID})
		woke++
	}
	if woke > 0 {
		logging.TraceContext(ctx, "snooze: woke", "count", woke)
	}
	return woke
}

// Reconcile converges an account's local snooze rows with the provider label
// state after a sync pass. It returns whether anything changed (the caller
// publishes one refresh).
func (m *Manager) Reconcile(ctx context.Context, accountID int64) (bool, error) {
	rows, err := m.St.ListSnoozes(ctx, accountID)
	if err != nil {
		return false, err
	}
	labeled, err := m.St.SnoozeLabelState(ctx, accountID)
	if err != nil {
		return false, err
	}
	byThread := make(map[string]store.SnoozeState, len(rows))
	threads := make([]string, 0, len(rows)+len(labeled))
	for _, r := range rows {
		byThread[r.ThreadID] = r
		threads = append(threads, r.ThreadID)
	}
	for tid := range labeled {
		if _, ok := byThread[tid]; !ok {
			threads = append(threads, tid)
		}
	}
	if len(threads) == 0 {
		return false, nil
	}
	inbox, err := m.St.ThreadsWithInbox(ctx, accountID, threads)
	if err != nil {
		return false, err
	}
	_, lm := m.labelManagerFor(accountID)

	changed := false
	for _, tid := range threads {
		row, hasRow := byThread[tid]
		pending := hasRow && !row.Notified
		labels := labeled[tid]
		wake, wakeOK := newestStamp(labels)

		switch {
		case pending && !row.Mirrored:
			// The provider has never seen this snooze (a row from before
			// mirroring existed, or a push lost offline). Its thread still
			// having INBOX is expected — removing it is the mirror's job — so
			// this must outrank the back-in-inbox rule, which on a MIRRORED
			// row means the user unsnoozed elsewhere.
			if lm == nil {
				continue // local-only account, nothing to push
			}
			logging.TraceContext(ctx, "snooze: reconcile push", "account", accountID, "thread", tid, "until", row.Until)
			if err := m.mirrorSnooze(ctx, accountID, tid, time.Unix(row.Until, 0)); err != nil {
				logging.TraceContext(ctx, "snooze: reconcile push failed", "account", accountID, "thread", tid, "err", err)
			} else if err := m.St.MarkSnoozeMirrored(ctx, accountID, tid); err != nil {
				return changed, err
			}
		case inbox[tid]:
			// Back in the inbox — woken elsewhere, unsnoozed from another
			// client, or re-inboxed by a new reply. The snooze is over:
			// cancel a pending row and strip leftover labels.
			if pending {
				logging.TraceContext(ctx, "snooze: reconcile cancel (thread back in inbox)", "account", accountID, "thread", tid)
				if err := m.St.UnsnoozeThread(ctx, accountID, tid); err != nil {
					return changed, err
				}
				changed = true
			}
			if len(labels) > 0 && lm != nil {
				m.restoreThread(ctx, accountID, tid)
			}
		case !pending && wakeOK:
			// Snoozed (or re-snoozed after waking) on another machine — adopt
			// its exact wake time. Skip our own just-woken thread whose label
			// removal is still queued, or the adoption would re-snooze it.
			if hasRow && row.Notified && row.Until == wake.Unix() {
				continue
			}
			logging.TraceContext(ctx, "snooze: reconcile adopt", "account", accountID, "thread", tid, "until", wake.Unix())
			if err := m.adoptSnooze(ctx, accountID, tid, wake); err != nil {
				return changed, err
			}
			changed = true
		case pending && wakeOK && wake.Unix() != row.Until:
			// Re-snoozed elsewhere to a different time: the label is provider
			// truth (our own changes update the local labels optimistically,
			// so a mismatch after a sync means a remote edit).
			logging.TraceContext(ctx, "snooze: reconcile retime", "account", accountID, "thread", tid, "old", row.Until, "new", wake.Unix())
			if err := m.adoptSnooze(ctx, accountID, tid, wake); err != nil {
				return changed, err
			}
			changed = true
		case pending && !wakeOK && lm != nil:
			// Mirrored, but the wake-time label vanished without the thread
			// returning to the inbox (stripped by hand in another client):
			// the local snooze still owns the thread — push the labels back.
			logging.TraceContext(ctx, "snooze: reconcile re-push", "account", accountID, "thread", tid, "until", row.Until)
			if err := m.mirrorSnooze(ctx, accountID, tid, time.Unix(row.Until, 0)); err != nil {
				logging.TraceContext(ctx, "snooze: reconcile re-push failed", "account", accountID, "thread", tid, "err", err)
			}
		}
	}
	return changed, nil
}

// adoptSnooze upserts a row for a snooze whose label state came from another
// machine — by definition already mirrored.
func (m *Manager) adoptSnooze(ctx context.Context, accountID int64, threadID string, wake time.Time) error {
	if err := m.St.SnoozeThread(ctx, accountID, threadID, wake.Unix()); err != nil {
		return err
	}
	return m.St.MarkSnoozeMirrored(ctx, accountID, threadID)
}

// newestStamp returns the latest wake time among a thread's stamp labels (a
// re-snooze can transiently leave two; the later one is the user's newest
// intent). ok is false when none parse — a bare root label is left alone, it
// may be a foreign label that just shares the name.
func newestStamp(labels []model.Label) (time.Time, bool) {
	var (
		best time.Time
		ok   bool
	)
	for _, l := range labels {
		if t, parsed := parseStamp(l.Name); parsed && (!ok || t.After(best)) {
			best, ok = t, true
		}
	}
	return best, ok
}
