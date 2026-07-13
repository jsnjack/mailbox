// Package aiwork runs AI enrichment that must not wait for the user to look at
// an account: inbox categorization for every connected account, both as mail
// arrives (sync events) and as a catch-up sweep at launch. It is headless —
// results are persisted to the store and announced over the sync Hub as
// AIUpdated changes; the UI seeds its tags from the cache. (One-line gists are
// generated on arrival by the notification path, which already covers all
// accounts.)
package aiwork

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jsnjack/mailbox/internal/activity"
	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
	"github.com/jsnjack/mailbox/internal/syncer"
)

const (
	// debounce lets a sync burst (a backfill batch, a busy incremental pass)
	// settle so one pass handles the whole batch. Events arriving faster than
	// this keep pushing the pass back, so categorization never competes with an
	// active sync for the AI/DB.
	debounce = 2 * time.Second
	// passCap bounds one pass's AI calls per account; the rest chains onto the
	// next tick, so a large backlog is a background trickle, not a stampede.
	passCap = 40
	// passWorkers is how many categorize requests run concurrently within a pass.
	passWorkers = 3
	// passTimeout bounds one pass end to end, so a hung provider can't pin the
	// worker loop for the HTTP client's full timeout per item.
	passTimeout = 45 * time.Second
	// failCooldown pauses categorization after a failing pass so a down provider
	// isn't re-hammered on every sync event; a Trigger lifts it ("try now").
	failCooldown = time.Minute
	// inboxScan is how many newest inbox threads a pass considers — the same
	// window the thread list shows (threadListCap in the UI).
	inboxScan = 5000
)

// Worker is the background categorizer. Create with New, run with Run.
type Worker struct {
	st      *store.Store
	asst    *ai.Assistant
	hub     *syncer.Hub
	act     *activity.Hub
	enabled func() bool // the "Categorize inbox with AI" preference, read per pass

	kick chan int64
}

// New assembles a Worker. act may be nil (no status-bar reporting); enabled may
// be nil (always on).
func New(st *store.Store, asst *ai.Assistant, hub *syncer.Hub, act *activity.Hub, enabled func() bool) *Worker {
	return &Worker{st: st, asst: asst, hub: hub, act: act, enabled: enabled, kick: make(chan int64, 16)}
}

// Trigger queues an immediate pass for the account (0 = every connected
// account) and lifts any failure cooldown — an explicit user action
// ("Re-categorize inbox", enabling the preference) beats backoff. Non-blocking.
func (w *Worker) Trigger(accountID int64) {
	select {
	case w.kick <- accountID:
	default: // a kick is already pending; coalesce
	}
}

// Run subscribes to sync changes and processes accounts until ctx ends. It
// starts with a catch-up pass over every account, so mail that arrived while
// the app was closed (or on another machine) is categorized without waiting for
// the user to open each account.
func (w *Worker) Run(ctx context.Context) {
	ch, unsub := w.hub.Subscribe()
	defer unsub()

	pending := map[int64]bool{}
	w.markAll(ctx, pending) // launch catch-up
	var failUntil time.Time

	for {
		// Only arm the timer while there is work; a failure cooldown stretches it.
		var wait <-chan time.Time
		if len(pending) > 0 {
			d := debounce
			if until := time.Until(failUntil); until > d {
				d = until
			}
			wait = time.After(d)
		}
		select {
		case <-ctx.Done():
			return
		case c, ok := <-ch:
			if !ok {
				return
			}
			if c.Kind == syncer.MessageUpserted && c.AccountID != 0 {
				pending[c.AccountID] = true
			}
		case id := <-w.kick:
			failUntil = time.Time{}
			logging.Trace("aiwork: triggered", "account", id)
			if id == 0 {
				w.markAll(ctx, pending)
			} else {
				pending[id] = true
			}
		case <-wait:
			ids := make([]int64, 0, len(pending))
			for id := range pending {
				ids = append(ids, id)
			}
			clear(pending)
			for _, id := range ids {
				remaining, err := w.pass(ctx, id)
				if err != nil {
					// The provider (whole failover chain) is struggling; retry this
					// account after the cooldown rather than on the next event.
					failUntil = time.Now().Add(failCooldown)
					pending[id] = true
					continue
				}
				if remaining > 0 {
					pending[id] = true // capped pass: chain the rest onto the next tick
				}
			}
		}
	}
}

// markAll queues every connected account.
func (w *Worker) markAll(ctx context.Context, pending map[int64]bool) {
	accounts, err := w.st.ListAccounts(ctx)
	if err != nil {
		logging.Trace("aiwork: list accounts", "err", err)
		return
	}
	for _, a := range accounts {
		pending[a.ID] = true
	}
}

// pass categorizes up to passCap of the account's uncategorized inbox threads
// (newest first), persisting each result, and returns how many candidates
// remain for a follow-up pass. Already-categorized threads cost two indexed
// queries and no AI. A pass that stored or failed anything publishes an
// AIUpdated change so an open UI refreshes its tags.
func (w *Worker) pass(ctx context.Context, accountID int64) (remaining int, err error) {
	if w.enabled != nil && !w.enabled() {
		logging.Trace("aiwork: pass skipped", "account", accountID, "reason", "categorization disabled")
		return 0, nil
	}
	begin := time.Now()
	threads, err := w.st.ListThreadsByLabel(ctx, accountID, model.LabelInbox, inboxScan, 0)
	if err != nil {
		return 0, fmt.Errorf("list inbox threads: %w", err)
	}
	ids := make([]string, len(threads))
	for i, t := range threads {
		ids[i] = t.Latest.GmailID
	}
	cached, err := w.st.MessageCategories(ctx, accountID, ids)
	if err != nil {
		return 0, fmt.Errorf("load cached categories: %w", err)
	}
	type cand struct {
		msgID  string
		prompt string
	}
	var todo []cand
	for _, t := range threads {
		m := t.Latest
		if _, ok := cached[m.GmailID]; ok {
			continue
		}
		todo = append(todo, cand{
			msgID:  m.GmailID,
			prompt: fmt.Sprintf("From: %s / Subject: %s / %s", displayFrom(m), m.Subject, ai.CleanContext(m.Snippet)),
		})
	}
	if len(todo) == 0 {
		logging.Trace("aiwork: pass up to date", "account", accountID, "threads", len(threads), "dur", time.Since(begin))
		return 0, nil
	}
	if len(todo) > passCap {
		remaining = len(todo) - passCap
		todo = todo[:passCap]
	}
	logging.Trace("aiwork: categorize pass", "account", accountID, "todo", len(todo), "remaining", remaining)

	var done func(string)
	if w.act != nil {
		email := ""
		if acc, aerr := w.st.GetAccountByID(ctx, accountID); aerr == nil {
			email = acc.Email
		}
		done = w.act.Begin("ai", email, fmt.Sprintf("categorize %d", len(todo)))
	}
	aiCtx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()

	type result struct {
		c   cand
		cat string
		err error
	}
	results := make(chan result, len(todo))
	sem := make(chan struct{}, passWorkers)
	var wg sync.WaitGroup
	for _, c := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func(c cand) {
			defer wg.Done()
			defer func() { <-sem }()
			cats, cerr := w.asst.Categorize(aiCtx, []string{c.prompt})
			cat := ""
			if cerr == nil {
				if len(cats) > 0 {
					cat = ai.MatchCategory(cats[0])
				} else {
					cerr = fmt.Errorf("categorize %q: empty reply", c.msgID)
				}
			}
			results <- result{c: c, cat: cat, err: cerr}
		}(c)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var firstErr error
	assigned, failed := 0, 0
	for r := range results {
		if r.err != nil {
			failed++
			if firstErr == nil {
				firstErr = r.err
			}
			logging.Trace("aiwork: categorize failed", "account", accountID, "id", r.c.msgID, "err", r.err)
			if serr := w.st.SetMessageCategoryFailed(ctx, accountID, r.c.msgID); serr != nil {
				logging.Trace("aiwork: persist failed category", "id", r.c.msgID, "err", serr)
			}
			continue
		}
		if serr := w.st.SetMessageCategory(ctx, accountID, r.c.msgID, r.cat); serr != nil {
			logging.Trace("aiwork: persist category", "id", r.c.msgID, "err", serr)
			continue
		}
		assigned++
	}
	logging.Trace("aiwork: categorize pass done", "account", accountID,
		"assigned", assigned, "failed", failed, "remaining", remaining, "dur", time.Since(begin), "err", firstErr)
	if done != nil {
		if firstErr != nil {
			done("error: " + firstErr.Error())
		} else {
			note := fmt.Sprintf("%d tagged", assigned)
			if m := ai.ShortModel(w.asst.ActiveModel()); m != "" {
				note += " · " + m
			}
			done(note)
		}
	}
	if assigned > 0 || failed > 0 {
		w.hub.Publish(syncer.Change{Kind: syncer.AIUpdated, AccountID: accountID, Count: assigned})
	}
	return remaining, firstErr
}

// displayFrom labels the sender the same way the thread list does.
func displayFrom(m model.Message) string {
	if strings.TrimSpace(m.FromName) != "" {
		return m.FromName
	}
	return m.FromAddr
}
