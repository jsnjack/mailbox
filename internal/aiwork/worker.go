// Package aiwork runs AI enrichment that must not wait for the user to look at
// an account: for every connected account it assigns inbox categories and
// writes each thread's one-line gist, both as mail arrives (sync events) and as
// a catch-up sweep at launch. It is headless — results are persisted to the
// store and announced over the sync Hub as AIUpdated changes; the UI seeds its
// tags and summary cards from the cache.
//
// Gists live here rather than only in the reader because a new-mail
// notification shows one, and the reader only produces a gist for a
// conversation someone has opened — never the message a notification is about.
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

// Worker is the background AI pass over inbox mail: it assigns categories and
// writes each thread's one-line gist. Create with New, run with Run.
type Worker struct {
	st          *store.Store
	asst        *ai.Assistant
	hub         *syncer.Hub
	act         *activity.Hub
	enabled     func() bool // "Categorize inbox with AI", read per pass
	gistEnabled func() bool // "one-line AI summary", read per pass

	// Gists have no persisted failure marker (categories do), so a message the
	// model cannot summarize would be retried on every pass forever. Remember
	// the failures for this session instead; a restart retries them.
	mu         sync.Mutex
	gistFailed map[string]bool

	kick chan int64
}

// New assembles a Worker. act may be nil (no status-bar reporting); either
// enabled func may be nil (that half always on).
func New(st *store.Store, asst *ai.Assistant, hub *syncer.Hub, act *activity.Hub,
	enabled, gistEnabled func() bool) *Worker {
	return &Worker{st: st, asst: asst, hub: hub, act: act, enabled: enabled,
		gistEnabled: gistEnabled, gistFailed: map[string]bool{},
		kick: make(chan int64, 16)}
}

func (w *Worker) markGistFailed(accountID int64, gmailID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gistFailed[fmt.Sprintf("%d|%s", accountID, gmailID)] = true
}

func (w *Worker) gistHasFailed(accountID int64, gmailID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gistFailed[fmt.Sprintf("%d|%s", accountID, gmailID)]
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

// pass brings one account up to date: categories for threads that lack one,
// gists for threads that lack one. Each half has its own preference gate and
// its own passCap, and returns how many candidates remain for a follow-up
// pass, so a large backlog trickles instead of stampeding. A half that stored
// or failed anything publishes an AIUpdated change so an open UI refreshes.
func (w *Worker) pass(ctx context.Context, accountID int64) (remaining int, err error) {
	wantCats := w.enabled == nil || w.enabled()
	wantGists := w.gistEnabled == nil || w.gistEnabled()
	if !wantCats && !wantGists {
		logging.Trace("aiwork: pass skipped", "account", accountID, "reason", "both AI passes disabled")
		return 0, nil
	}
	begin := time.Now()
	// One listing serves both halves; an up-to-date account costs this query
	// plus one indexed lookup each and no AI.
	threads, err := w.st.ListThreadsByLabel(ctx, accountID, model.LabelInbox, inboxScan, 0)
	if err != nil {
		return 0, fmt.Errorf("list inbox threads: %w", err)
	}
	var firstErr error
	if wantCats {
		left, cerr := w.categorize(ctx, accountID, threads)
		remaining += left
		firstErr = cerr
	}
	if wantGists {
		left, gerr := w.gists(ctx, accountID, threads)
		remaining += left
		if firstErr == nil {
			firstErr = gerr
		}
	}
	logging.Trace("aiwork: pass done", "account", accountID, "threads", len(threads),
		"remaining", remaining, "dur", time.Since(begin), "err", firstErr)
	return remaining, firstErr
}

// gists writes the one-line AI gist for inbox threads that lack one.
//
// This exists because the gist is what a new-mail desktop notification shows,
// and it used to be produced ONLY by the reader when a conversation was opened.
// A message that had never been opened therefore never had a gist — which is
// every message a new-mail notification is about, so the notification always
// fell back to the raw snippet. Producing gists here makes that path reachable.
//
// Same shape as categorize: newest first, capped per pass, concurrent, each
// result persisted as it lands.
func (w *Worker) gists(ctx context.Context, accountID int64, threads []model.ThreadSummary) (remaining int, err error) {
	begin := time.Now()
	ids := make([]string, len(threads))
	for i, t := range threads {
		ids[i] = t.Latest.GmailID
	}
	cached, err := w.st.MessageGists(ctx, accountID, ids)
	if err != nil {
		return 0, fmt.Errorf("load cached gists: %w", err)
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
		if w.gistHasFailed(accountID, m.GmailID) {
			continue
		}
		todo = append(todo, cand{
			msgID: m.GmailID,
			// Byte-for-byte the context the reader builds (ui.gistContext), so a
			// gist written here is indistinguishable from one written on open.
			prompt: fmt.Sprintf("From: %s\nSubject: %s\n\n%s",
				displayFrom(m), m.Subject, ai.CleanContext(m.Snippet)),
		})
	}
	if len(todo) == 0 {
		logging.Trace("aiwork: gists up to date", "account", accountID, "threads", len(threads))
		return 0, nil
	}
	if len(todo) > passCap {
		remaining = len(todo) - passCap
		todo = todo[:passCap]
	}
	logging.Trace("aiwork: gist pass", "account", accountID, "todo", len(todo), "remaining", remaining)

	var done func(string)
	if w.act != nil {
		email := ""
		if acc, aerr := w.st.GetAccountByID(ctx, accountID); aerr == nil {
			email = acc.Email
		}
		done = w.act.Begin("ai", email, "Summarizing "+activity.Plural(len(todo), "message", "messages"))
	}
	aiCtx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()

	type result struct {
		c    cand
		gist string
		err  error
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
			gist, gerr := w.asst.BriefSummary(aiCtx, c.prompt)
			if gerr == nil && strings.TrimSpace(gist) == "" {
				gerr = fmt.Errorf("gist %q: empty reply", c.msgID)
			}
			results <- result{c: c, gist: strings.TrimSpace(gist), err: gerr}
		}(c)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var firstErr error
	written, failed := 0, 0
	for r := range results {
		if r.err != nil {
			failed++
			if firstErr == nil {
				firstErr = r.err
			}
			logging.Trace("aiwork: gist failed", "account", accountID, "id", r.c.msgID, "err", r.err)
			w.markGistFailed(accountID, r.c.msgID)
			continue
		}
		if serr := w.st.SetMessageGist(ctx, accountID, r.c.msgID, r.gist); serr != nil {
			logging.Trace("aiwork: persist gist", "id", r.c.msgID, "err", serr)
			continue
		}
		written++
	}
	logging.Trace("aiwork: gist pass done", "account", accountID, "written", written,
		"failed", failed, "remaining", remaining, "dur", time.Since(begin), "err", firstErr)
	if done != nil {
		if firstErr != nil {
			done("error: " + firstErr.Error())
		} else {
			note := fmt.Sprintf("%d summarized", written)
			if m := ai.ShortModel(w.asst.ActiveModel()); m != "" {
				note += " · " + m
			}
			done(note)
		}
	}
	if written > 0 {
		// Wakes an open reader's summary card and, more importantly, the
		// new-mail notification that is waiting for this gist.
		w.hub.Publish(syncer.Change{Kind: syncer.AIUpdated, AccountID: accountID, Count: written})
	}
	return remaining, firstErr
}

// categorize assigns a category to inbox threads that lack one.
func (w *Worker) categorize(ctx context.Context, accountID int64, threads []model.ThreadSummary) (remaining int, err error) {
	begin := time.Now()
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
		logging.Trace("aiwork: categories up to date", "account", accountID, "threads", len(threads))
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
		done = w.act.Begin("ai", email, "Categorizing "+activity.Plural(len(todo), "conversation", "conversations"))
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
