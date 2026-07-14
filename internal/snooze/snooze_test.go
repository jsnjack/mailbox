package snooze

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
	"github.com/jsnjack/mailbox/internal/syncer"
)

// The stamp label must parse back to the exact instant on a machine in any
// timezone — that is the whole cross-machine wake contract.
func TestStampRoundTrip(t *testing.T) {
	amsterdam, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skip("no tzdata")
	}
	orig := time.Date(2026, 7, 18, 14, 30, 0, 0, amsterdam)
	name := Stamp(orig)
	if name != model.SnoozeLabelPrefix+"2026-07-18 14:30 +0200" {
		t.Fatalf("stamp = %q", name)
	}
	got, ok := parseStamp(name)
	if !ok || !got.Equal(orig) {
		t.Fatalf("parse(%q) = %v, %v; want %v", name, got, ok, orig)
	}
	if _, ok := parseStamp(model.SnoozeLabelRoot); ok {
		t.Fatal("root label must not parse as a stamp")
	}
	if _, ok := parseStamp(model.SnoozeLabelPrefix + "someday"); ok {
		t.Fatal("foreign sublabel must not parse as a stamp")
	}
}

type applyCall struct{ ids, add, remove []string }

// fakeBackend records label operations; unimplemented Backend methods panic
// via the embedded nil interface (nothing else may be called in these tests).
type fakeBackend struct {
	backend.Backend
	acct    int64
	mu      sync.Mutex
	applied []applyCall
	labels  map[string]model.Label
	deleted []string
}

func (f *fakeBackend) ApplyLabels(_ context.Context, ids []string, add, remove []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, applyCall{ids, add, remove})
	return nil
}

func (f *fakeBackend) EnsureLabel(_ context.Context, name string, _ bool) (model.Label, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.labels[name]; ok {
		return l, nil
	}
	l := model.Label{AccountID: f.acct, GmailID: "L_" + name, Name: name, Type: model.LabelUser}
	if f.labels == nil {
		f.labels = map[string]model.Label{}
	}
	f.labels[name] = l
	return l, nil
}

func (f *fakeBackend) DeleteLabelIfUnused(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func testManager(t *testing.T) (*Manager, *fakeBackend, *store.Store, int64, *syncer.Engine) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	acct, err := s.UpsertAccount(context.Background(), model.Account{Email: "a@example.com"})
	if err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	hub := syncer.NewHub()
	engine := syncer.NewEngine(s, hub)
	fb := &fakeBackend{acct: acct}
	m := &Manager{
		St: s, Engine: engine, Hub: hub,
		BackendFor: func(int64) backend.Backend { return fb },
		EmailOf:    func(int64) string { return "a@example.com" },
	}
	return m, fb, s, acct, engine
}

// Snoozing mirrors −INBOX plus the root and wake-time labels to the provider
// and hides the thread locally at once.
func TestSnoozeMirrorsLabels(t *testing.T) {
	m, fb, s, acct, engine := testManager(t)
	ctx := context.Background()
	if err := s.UpsertMessages(ctx, []model.Message{
		{AccountID: acct, GmailID: "g1", ThreadID: "t1", Subject: "s", Labels: []string{model.LabelInbox}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	until := time.Now().Add(2 * time.Hour).Truncate(time.Minute)
	if err := m.Snooze(ctx, acct, "t1", until); err != nil {
		t.Fatalf("snooze: %v", err)
	}
	<-engine.StopAccount(acct) // drain the mirror queue

	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.applied) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(fb.applied))
	}
	call := fb.applied[0]
	if len(call.ids) != 1 || call.ids[0] != "g1" {
		t.Fatalf("applied to %v", call.ids)
	}
	wantAdd := map[string]bool{"L_" + model.SnoozeLabelRoot: true, "L_" + Stamp(until): true}
	for _, a := range call.add {
		delete(wantAdd, a)
	}
	if len(wantAdd) != 0 || len(call.remove) != 1 || call.remove[0] != model.LabelInbox {
		t.Fatalf("mirror delta = +%v −%v", call.add, call.remove)
	}
	snoozed, err := s.SnoozedThreads(ctx, acct)
	if err != nil || len(snoozed) != 1 || snoozed[0].Until != until.Unix() {
		t.Fatalf("local rows = %v, %v", snoozed, err)
	}
	rows, err := s.ListSnoozes(ctx, acct)
	if err != nil || len(rows) != 1 || !rows[0].Mirrored {
		t.Fatalf("row must be marked mirrored: %+v, %v", rows, err)
	}
}

// A thread carrying a wake-time label with no local row was snoozed on another
// machine: Reconcile adopts it with the exact wake time.
func TestReconcileAdoptsRemoteSnooze(t *testing.T) {
	m, _, s, acct, _ := testManager(t)
	ctx := context.Background()
	until := time.Now().Add(3 * time.Hour).Truncate(time.Minute)
	stampName := Stamp(until)
	for _, l := range []model.Label{
		{AccountID: acct, GmailID: "L_root", Name: model.SnoozeLabelRoot, Type: model.LabelUser},
		{AccountID: acct, GmailID: "L_stamp", Name: stampName, Type: model.LabelUser},
	} {
		if err := s.UpsertLabel(ctx, l); err != nil {
			t.Fatalf("seed label: %v", err)
		}
	}
	if err := s.UpsertMessages(ctx, []model.Message{
		{AccountID: acct, GmailID: "g2", ThreadID: "t2", Subject: "s", Labels: []string{"L_root", "L_stamp"}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changed, err := m.Reconcile(ctx, acct)
	if err != nil || !changed {
		t.Fatalf("reconcile = %v, %v", changed, err)
	}
	snoozed, err := s.SnoozedThreads(ctx, acct)
	if err != nil || len(snoozed) != 1 || snoozed[0].ThreadID != "t2" || snoozed[0].Until != until.Unix() {
		t.Fatalf("adopted rows = %v, %v", snoozed, err)
	}
	// Idempotent: a second pass changes nothing.
	if changed, _ = m.Reconcile(ctx, acct); changed {
		t.Fatal("second reconcile must be a no-op")
	}
}

// A pending snooze whose thread is back in INBOX (woken/unsnoozed elsewhere,
// or re-inboxed by a new reply) is cancelled and its labels stripped.
func TestReconcileCancelsOnInbox(t *testing.T) {
	m, fb, s, acct, engine := testManager(t)
	ctx := context.Background()
	if err := s.UpsertLabel(ctx, model.Label{AccountID: acct, GmailID: "L_root", Name: model.SnoozeLabelRoot, Type: model.LabelUser}); err != nil {
		t.Fatalf("seed label: %v", err)
	}
	if err := s.UpsertMessages(ctx, []model.Message{
		{AccountID: acct, GmailID: "g3", ThreadID: "t3", Subject: "s", Labels: []string{model.LabelInbox, "L_root"}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.SnoozeThread(ctx, acct, "t3", time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("seed snooze: %v", err)
	}
	// The row has been mirrored — INBOX reappearing therefore means the user
	// unsnoozed it elsewhere (a never-mirrored row would be pushed instead).
	if err := s.MarkSnoozeMirrored(ctx, acct, "t3"); err != nil {
		t.Fatalf("seed mirrored: %v", err)
	}
	changed, err := m.Reconcile(ctx, acct)
	if err != nil || !changed {
		t.Fatalf("reconcile = %v, %v", changed, err)
	}
	if n, _ := s.SnoozedCount(ctx, acct); n != 0 {
		t.Fatalf("snoozed count after cancel = %d", n)
	}
	<-engine.StopAccount(acct)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.applied) != 1 || fb.applied[0].add[0] != model.LabelInbox || fb.applied[0].remove[0] != "L_root" {
		t.Fatalf("strip mirror = %+v", fb.applied)
	}
}

// A pending row the provider doesn't know about (created before mirroring, or
// pushed while offline) is mirrored out by Reconcile — even though its thread
// still has INBOX (nothing ever removed it server-side, so INBOX here must not
// read as "unsnoozed elsewhere").
func TestReconcilePushesLocalSnooze(t *testing.T) {
	m, fb, s, acct, engine := testManager(t)
	ctx := context.Background()
	if err := s.UpsertMessages(ctx, []model.Message{
		{AccountID: acct, GmailID: "g4", ThreadID: "t4", Subject: "s", Labels: []string{model.LabelInbox}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	until := time.Now().Add(4 * time.Hour).Truncate(time.Minute)
	if err := s.SnoozeThread(ctx, acct, "t4", until.Unix()); err != nil {
		t.Fatalf("seed snooze: %v", err)
	}
	if _, err := m.Reconcile(ctx, acct); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n, _ := s.SnoozedCount(ctx, acct); n != 1 {
		t.Fatal("pre-mirror snooze must survive reconciliation, not be cancelled")
	}
	rows, _ := s.ListSnoozes(ctx, acct)
	if len(rows) != 1 || !rows[0].Mirrored {
		t.Fatalf("pushed row must be marked mirrored: %+v", rows)
	}
	<-engine.StopAccount(acct)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.applied) != 1 {
		t.Fatalf("push mirror calls = %d, want 1", len(fb.applied))
	}
	found := false
	for _, a := range fb.applied[0].add {
		if a == "L_"+Stamp(until) {
			found = true
		}
	}
	if !found || fb.applied[0].remove[0] != model.LabelInbox {
		t.Fatalf("push delta = +%v −%v", fb.applied[0].add, fb.applied[0].remove)
	}
}

// WakeDue returns a due thread to the inbox everywhere: row marked notified,
// mirror restores INBOX and strips the labels, the stamp label is cleaned up,
// and SnoozeWoke is published.
func TestWakeDueRestoresEverywhere(t *testing.T) {
	m, fb, s, acct, engine := testManager(t)
	ctx := context.Background()
	until := time.Now().Add(-time.Minute).Truncate(time.Minute)
	stampName := Stamp(until)
	for _, l := range []model.Label{
		{AccountID: acct, GmailID: "L_root", Name: model.SnoozeLabelRoot, Type: model.LabelUser},
		{AccountID: acct, GmailID: "L_stamp", Name: stampName, Type: model.LabelUser},
	} {
		if err := s.UpsertLabel(ctx, l); err != nil {
			t.Fatalf("seed label: %v", err)
		}
	}
	if err := s.UpsertMessages(ctx, []model.Message{
		{AccountID: acct, GmailID: "g5", ThreadID: "t5", Subject: "s", Labels: []string{"L_root", "L_stamp"}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.SnoozeThread(ctx, acct, "t5", until.Unix()); err != nil {
		t.Fatalf("seed snooze: %v", err)
	}
	events, unsub := m.Hub.Subscribe()
	defer unsub()

	if woke := m.WakeDue(ctx, time.Now()); woke != 1 {
		t.Fatalf("woke = %d, want 1", woke)
	}
	due, _ := s.DueSnoozes(ctx, time.Now().Unix())
	if len(due) != 0 {
		t.Fatal("row must be marked notified")
	}
	<-engine.StopAccount(acct)
	fb.mu.Lock()
	if len(fb.applied) != 1 || fb.applied[0].add[0] != model.LabelInbox || len(fb.applied[0].remove) != 2 {
		fb.mu.Unlock()
		t.Fatalf("wake mirror = %+v", fb.applied)
	}
	deleted := append([]string(nil), fb.deleted...)
	fb.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "L_stamp" {
		t.Fatalf("cleanup deleted %v, want only the stamp label", deleted)
	}
	woke := false
	for !woke {
		select {
		case c := <-events:
			if c.Kind == syncer.SnoozeWoke && c.ThreadID == "t5" {
				woke = true
			}
		default:
			t.Fatal("no SnoozeWoke published")
		}
	}
}
