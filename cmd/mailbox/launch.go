package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jsnjack/mailbox/internal/activity"
	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/aiwork"
	"github.com/jsnjack/mailbox/internal/auth"
	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/config"
	"github.com/jsnjack/mailbox/internal/gmailapi"
	"github.com/jsnjack/mailbox/internal/gmailbackend"
	"github.com/jsnjack/mailbox/internal/imapbackend"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/snooze"
	"github.com/jsnjack/mailbox/internal/store"
	"github.com/jsnjack/mailbox/internal/syncer"
	"github.com/jsnjack/mailbox/internal/ui"
	"github.com/zalando/go-keyring"
	"golang.org/x/oauth2"
)

// aiKeyringService is the keyring collection for the AI provider API key.
const aiKeyringService = "mailbox-ai"

// aiKeyFor resolves the API key for one AI chain entry: the MAILBOX_AI_KEY env
// override, else a key stored per endpoint (chain entries on different
// endpoints need different keys), else the legacy per-provider entry that
// `mailbox set-ai-key` and older builds write. Empty is fine — local proxies
// are often keyless.
func aiKeyFor(provider, endpoint string) string {
	if v := os.Getenv("MAILBOX_AI_KEY"); v != "" {
		return v
	}
	if k, _ := keyring.Get(aiKeyringService, endpoint); k != "" {
		return k
	}
	k, _ := keyring.Get(aiKeyringService, provider)
	return k
}

// syncInterval is how often the background incremental sync runs while the GUI
// is open.
const syncInterval = 60 * time.Second

// resyncBackfillLimit bounds how many newest messages a recovery re-backfills
// when an expired history watermark forces a resync (see engine.Resync).
const resyncBackfillLimit = 500

// stopGrace bounds how long stopAccount waits for an account's background
// goroutines to exit before proceeding, so one wedged on a deadline-less network
// read can't hang a remove/reconnect indefinitely.
const stopGrace = 5 * time.Second

// syncPassTimeout bounds one background sync pass end to end. Every layer below
// carries its own bound (Gmail's stall watchdog, the IMAP op watchdog, the
// bounded token refresh), so this backstop should never fire — it exists so no
// single pass, present or future, can pin the sync loop indefinitely. Generous:
// an initial 500-message backfill on a slow link takes minutes.
const syncPassTimeout = 10 * time.Minute

// syncBackoffCap is the longest wait between sync attempts for an account whose
// passes keep failing. Failures back off exponentially from syncInterval up to
// this cap, so a persistently broken account (bad cursor, provider outage)
// doesn't re-run a heavy resync every tick — and every IDLE nudge — forever.
const syncBackoffCap = 15 * time.Minute

// syncBackoff returns how long to wait after n consecutive failed passes.
func syncBackoff(n int) time.Duration {
	d := syncInterval
	for i := 1; i < n && d < syncBackoffCap; i++ {
		d *= 2
	}
	if d > syncBackoffCap {
		d = syncBackoffCap
	}
	return d
}

// acctRuntime tracks a live account's background goroutines (sync, outbox sweep,
// IMAP IDLE watch) so stopAccount can cancel them and wait for them to exit
// before the account is torn down.
type acctRuntime struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// launchUI opens the store, starts a background sync for each connected account
// (building a live Gmail/IMAP backend when credentials are available), and runs
// the GTK application. Zero accounts is fine — the app opens to a welcome empty
// state from which the first account is connected via the Add account dialog.
func launchUI(mailto string) error {
	if err := config.EnsureDirs(); err != nil {
		return err
	}
	dbPath, err := config.DBPath()
	if err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	accounts, err := st.ListAccounts(ctx)
	if err != nil {
		return err
	}
	// Zero accounts is fine: the app opens to an empty state and the user connects
	// their first account from the Add account dialog (Gmail or IMAP).
	deps := ui.Deps{Store: st, Version: Version}
	for _, a := range accounts {
		deps.Accounts = append(deps.Accounts, ui.AccountInfo{ID: a.ID, Email: a.Email, Type: a.Type})
	}

	// Activity hub feeds the status bar (Stats, below, feeds its metrics).
	act := activity.NewHub()
	deps.Activity = act

	// AI settings are editable regardless of account/client state.
	if cfgPath, err := config.ConfigFilePath(); err == nil {
		// chainConfig turns the dialog's entries back into a Config; the first
		// entry doubles as the top-level defaults, so SaveConfig can collapse a
		// single-endpoint chain to the terse legacy form.
		chainConfig := func(entries []ui.AIModelEntry) ai.Config {
			var cfg ai.Config
			for _, e := range entries {
				cfg.Chain = append(cfg.Chain, ai.ModelConfig{Model: e.Model, Provider: e.Provider, Endpoint: e.Endpoint})
			}
			if len(entries) > 0 {
				cfg.Provider = entries[0].Provider
				cfg.Endpoint = entries[0].Endpoint
			}
			return cfg
		}
		// typedKeys resolves keys as entered in the dialog (per endpoint), with
		// the env override as a fallback — matching what a live swap would use.
		typedKeys := func(entries []ui.AIModelEntry) ai.KeyFunc {
			byEndpoint := map[string]string{}
			for _, e := range entries {
				if e.Key != "" {
					byEndpoint[e.Endpoint] = e.Key
				}
			}
			return func(_, endpoint string) string {
				if k := byEndpoint[endpoint]; k != "" {
					return k
				}
				return os.Getenv("MAILBOX_AI_KEY")
			}
		}
		deps.AISettings = func() []ui.AIModelEntry {
			c, err := ai.LoadConfig(cfgPath)
			if err != nil {
				slog.Warn("load ai config", "err", err)
			}
			var out []ui.AIModelEntry
			for _, e := range c.ResolvedChain() {
				key, _ := keyring.Get(aiKeyringService, e.Endpoint)
				if key == "" {
					key, _ = keyring.Get(aiKeyringService, e.Provider) // legacy per-provider entry
				}
				out = append(out, ui.AIModelEntry{Provider: e.Provider, Endpoint: e.Endpoint, Model: e.Model, Key: key})
			}
			return out
		}
		deps.SaveAISettings = func(entries []ui.AIModelEntry) error {
			cfg := chainConfig(entries)
			if err := ai.SaveConfig(cfgPath, cfg); err != nil {
				return err
			}
			// Each entry's key row mirrors the keyring, keyed by endpoint: typed →
			// stored, cleared → removed (keyless local proxies). A cleared key also
			// removes the legacy per-provider entry, else the fallback lookup would
			// resurrect it.
			for _, e := range entries {
				if e.Key != "" {
					if err := keyring.Set(aiKeyringService, e.Endpoint, e.Key); err != nil {
						return fmt.Errorf("store AI key: %w", err)
					}
					continue
				}
				for _, user := range []string{e.Endpoint, e.Provider} {
					if err := keyring.Delete(aiKeyringService, user); err != nil && !errors.Is(err, keyring.ErrNotFound) {
						logging.Trace("launch: delete ai key", "user", user, "err", err)
					}
				}
			}
			// Swap the new provider into the live assistant so the change applies
			// now. Going from unconfigured to configured still needs a restart (the
			// AI widgets aren't built); broken new settings keep the old provider.
			if deps.Assistant == nil || !cfg.Configured() {
				logging.Trace("launch: ai settings saved without live swap",
					"assistant", deps.Assistant != nil, "configured", cfg.Configured())
				return nil
			}
			p, err := ai.NewProvider(cfg, aiKeyFor)
			if err != nil {
				return err
			}
			deps.Assistant.SetProvider(p)
			return nil
		}
		deps.TestAISettings = func(ctx context.Context, entries []ui.AIModelEntry) error {
			cfg := chainConfig(entries)
			if !cfg.Configured() {
				return fmt.Errorf("every model needs a provider, endpoint, and model name")
			}
			p, err := ai.NewProvider(cfg, typedKeys(entries))
			if err != nil {
				return err
			}
			return ai.NewAssistant(p).Ping(ctx)
		}
	}

	// Build a Gmail client per account (those without a usable token are
	// rendered read-only). Operations are routed by account id.
	hub := syncer.NewHub()
	engine := syncer.NewEngine(st, hub)
	// clients keeps the raw Gmail clients for their API stats; backends holds the
	// provider-agnostic adapters the engine drives (Gmail today; IMAP later).
	clients := make(map[int64]*gmailapi.Client)
	backends := make(map[int64]backend.Backend)
	// running tracks each live account's sync goroutines so stopAccount can cancel
	// them AND wait for them to actually exit before tearing the account down (so a
	// follow-up DeleteAccount can't race an in-flight store write).
	running := make(map[int64]*acctRuntime)
	// accountsMu guards the three maps: startAccount/stopAccount write them from
	// the add-account dialog's goroutine while clientFor/Stats read them from the
	// UI thread.
	var accountsMu sync.Mutex

	// stopAccount tears down a running account's sync loop (+ IDLE watch), closes
	// its backend, and waits (bounded) for the goroutines to exit. Safe to call for
	// an unknown id.
	stopAccount := func(id int64) {
		accountsMu.Lock()
		rt := running[id]
		b := backends[id]
		delete(running, id)
		delete(backends, id)
		delete(clients, id)
		accountsMu.Unlock()
		if rt != nil {
			rt.cancel()
		}
		// Close the engine's mirror queue and give already-queued label mirrors a
		// bounded window to reach the provider before the backend goes away —
		// otherwise an archive/star done just before a remove/reconnect is
		// silently dropped server-side (and the drain goroutine leaks).
		select {
		case <-engine.StopAccount(id):
		case <-time.After(stopGrace):
		}
		if c, ok := b.(interface{ Close() }); ok {
			c.Close() // releases the IMAP connection pool + idle conns; no-op for Gmail
		}
		if rt != nil {
			// Wait for sync/sweep/watch to return so the caller (remove/reconnect)
			// sees a quiesced account. Bounded, in case a goroutine is wedged on a
			// network read with no deadline — then we proceed and accept the small
			// residual window rather than hang.
			done := make(chan struct{})
			go func() { rt.wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(stopGrace):
			}
		}
	}

	// emailOf resolves an account id to its email for activity reporting (a
	// point query on the local DB; "" for a removed/unknown account).
	emailOf := func(accountID int64) string {
		if a, err := st.GetAccountByID(context.Background(), accountID); err == nil {
			return a.Email
		}
		return ""
	}

	// Snooze manager: the local snoozes table plus its provider label mirror,
	// so a snooze holds (and wakes) on every machine and client. Accounts whose
	// backend can't manage labels keep local-only snoozes.
	snoozeMgr := &snooze.Manager{
		St: st, Engine: engine, Hub: hub, Act: act,
		BackendFor: func(accountID int64) backend.Backend {
			accountsMu.Lock()
			defer accountsMu.Unlock()
			return backends[accountID]
		},
		EmailOf: emailOf,
	}

	// startAccount builds an account's backend, registers it, and starts its
	// background sync/sweep (+ IMAP IDLE watch). Used at launch, when the dialog
	// adds an account (so it syncs immediately — no restart), and on reconnect. Any
	// existing runtime for the same id is stopped first, so a reconnect swaps in the
	// fresh credential without leaking the old sync loop.
	startAccount := func(a model.Account) error {
		b, client, err := buildBackendForAccount(ctx, a)
		if err != nil {
			return err // leave any existing runtime untouched on a failed rebuild
		}
		stopAccount(a.ID)
		actx, cancel := context.WithCancel(ctx)
		rt := &acctRuntime{cancel: cancel}
		accountsMu.Lock()
		if client != nil { // Gmail REST exposes API stats; IMAP reports bytes via the backend
			clients[a.ID] = client
		}
		backends[a.ID] = b
		running[a.ID] = rt
		accountsMu.Unlock()
		// A wake channel lets a push notification (IMAP IDLE) trigger an immediate
		// sync instead of waiting for the poll tick. All Add()s happen here, before
		// any goroutine can finish, so stopAccount's Wait can't race them.
		wake := make(chan struct{}, 1)
		if watcher, ok := b.(backend.Watcher); ok {
			rt.wg.Add(1)
			go func() {
				defer rt.wg.Done()
				watcher.Watch(actx, func() {
					select {
					case wake <- struct{}{}:
					default: // a wake is already pending; coalesce
					}
				})
			}()
		}
		rt.wg.Add(2)
		reconcileSnoozes := func(pctx context.Context) {
			changed, err := snoozeMgr.Reconcile(pctx, a.ID)
			if err != nil {
				logging.Trace("launch: snooze reconcile failed", "account", a.ID, "err", err)
				return
			}
			if changed {
				hub.Publish(syncer.Change{Kind: syncer.MessageUpserted, AccountID: a.ID})
			}
		}
		go func() {
			defer rt.wg.Done()
			backgroundSync(actx, engine, act, b, a.ID, a.Email, wake, reconcileSnoozes)
		}()
		go func() { defer rt.wg.Done(); backgroundSweep(actx, engine, act, b, a.ID, a.Email) }()
		// One-time recovery: re-fetch Gmail messages cached text-only by an older
		// build that dropped externalized (attachment-id-served) HTML bodies. IMAP
		// fetches whole bodies, so its text-only mail is genuinely text-only — skip it.
		if a.Type == model.AccountGmail || a.Type == "" {
			rt.wg.Add(1)
			go func() { defer rt.wg.Done(); backgroundBackfillHTML(actx, engine, act, b, a.ID, a.Email) }()
		}
		return nil
	}
	for _, a := range accounts {
		if err := startAccount(a); err != nil {
			fmt.Fprintf(os.Stderr, "live features disabled for %s (%v)\n", a.Email, err)
		}
	}

	// Body retention: when configured (Preferences → Storage), prune cached
	// bodies older than the window shortly after launch and then daily.
	go backgroundRetention(ctx, st, act)

	// Snooze wake: return due snoozed conversations to the inbox — locally and,
	// via the label mirror, on every other client.
	go backgroundSnoozeWake(ctx, snoozeMgr, act)

	// Cumulative metrics for the status bar: cache sizes + per-account API stats.
	deps.Stats = func() ui.StatusStats {
		s := ui.StatusStats{}
		if fi, err := os.Stat(dbPath); err == nil {
			s.DBBytes = fi.Size()
		}
		s.CacheBytes = dirSize(cacheDir())
		if n, err := st.Count(context.Background()); err == nil {
			s.Messages = n
		}
		accountsMu.Lock()
		for _, c := range clients {
			cs := c.Stats()
			s.Requests += cs.Requests
			s.QuotaUnits += cs.QuotaUnits
			s.BytesIn += cs.BytesIn
			s.BytesOut += cs.BytesOut
		}
		// IMAP backends report wire bytes (no Gmail-style request/quota counters).
		for _, b := range backends {
			if r, ok := b.(interface{ Transferred() (int64, int64) }); ok {
				in, out := r.Transferred()
				s.BytesIn += in
				s.BytesOut += out
			}
		}
		accountsMu.Unlock()
		// AI traffic gets its own counters (and status-bar line) rather than
		// being folded invisibly into the mail-API numbers.
		if deps.Assistant != nil {
			s.AIRequests = deps.Assistant.Requests()
			s.AIBytesIn, s.AIBytesOut = deps.Assistant.Transferred()
		}
		return s
	}

	// Add-account dialog hooks — wired unconditionally so an account can be added
	// even from a zero-account first run. A newly added IMAP account is picked up
	// (and backfilled) on the next launch.
	deps.TestIMAPAccount = func(ctx context.Context, acct config.IMAPAccount, password string) error {
		b := imapbackend.New(imapConfigOf(acct), 0, imapbackend.PasswordAuth(usernameOf(acct), password))
		defer b.Close()
		_, err := b.Profile(ctx) // connects, logs in, lists folders
		return err
	}
	deps.AddIMAPAccount = func(ctx context.Context, acct config.IMAPAccount, secret string) (int64, error) {
		if secret == "" {
			return 0, fmt.Errorf("no credentials to save")
		}
		atype := model.AccountIMAP
		if acct.Auth == config.AuthGmailREST {
			// Gmail REST: native backend — token under the Gmail keyring, a
			// gmail-type account, no IMAP server config.
			if err := auth.SaveRefreshToken(acct.Email, secret); err != nil {
				return 0, err
			}
			atype = model.AccountGmail
		} else {
			if err := auth.SaveIMAPSecret(acct.Email, secret); err != nil {
				return 0, err
			}
			if err := config.SaveIMAPAccount(acct); err != nil {
				return 0, err
			}
		}
		if err := upsertAccountKeepingCursor(ctx, st, acct.Email, atype); err != nil {
			return 0, err
		}
		saved, err := st.GetAccountByEmail(ctx, acct.Email)
		if err != nil {
			return 0, err
		}
		// Start syncing it now — no restart needed.
		if err := startAccount(saved); err != nil {
			return saved.ID, fmt.Errorf("account saved but could not start syncing: %w", err)
		}
		return saved.ID, nil
	}
	deps.RemoveAccount = func(ctx context.Context, accountID int64) error {
		acc, err := st.GetAccountByID(ctx, accountID)
		if err != nil {
			return fmt.Errorf("look up account: %w", err)
		}
		stopAccount(accountID)
		if err := st.DeleteAccount(ctx, accountID); err != nil {
			return fmt.Errorf("delete cached data: %w", err)
		}
		// Best-effort secret + per-account config cleanup; a missing entry is fine.
		if acc.Type == model.AccountGmail {
			_ = auth.DeleteRefreshToken(acc.Email)
		} else {
			_ = auth.DeleteIMAPSecret(acc.Email)
			_ = config.DeleteIMAPAccount(acc.Email)
		}
		_ = config.SaveAccountName(acc.Email, "")      // blank removes the name entry
		_ = config.SaveAccountSignature(acc.Email, "") // blank removes the override
		return nil
	}
	deps.OAuthConnect = func(ctx context.Context, kind config.AuthKind) (string, string, error) {
		switch kind {
		case config.AuthGmailREST:
			cc, err := auth.LoadClientConfig(credentialsPath())
			if err != nil {
				return "", "", err
			}
			tok, err := auth.Login(ctx, cc) // Gmail REST scopes
			if err != nil {
				return "", "", err
			}
			return gmailVerifiedEmail(ctx, tok), tok.RefreshToken, nil
		case config.AuthGoogle:
			cc, err := auth.LoadClientConfig(credentialsPath())
			if err != nil {
				return "", "", err
			}
			tok, err := auth.LoginGoogleMail(ctx, cc)
			if err != nil {
				return "", "", err
			}
			return gmailVerifiedEmail(ctx, tok), tok.RefreshToken, nil
		case config.AuthMicrosoft:
			id := microsoftClientID()
			if id == "" {
				return "", "", fmt.Errorf("set MAILBOX_MS_CLIENT_ID to connect Outlook")
			}
			tok, err := auth.LoginMicrosoft(ctx, id)
			if err != nil {
				return "", "", err
			}
			return "", tok.RefreshToken, nil // Microsoft: keep the typed address
		default:
			return "", "", fmt.Errorf("provider does not use OAuth")
		}
	}

	// Operation hooks + change events are wired unconditionally — even with zero
	// accounts — so the add-account dialog can connect the first account live.
	// Each hook routes through clientFor, which errors gracefully until a backend
	// exists for the account.
	deps.Hub = hub
	clientFor := func(accountID int64) (backend.Backend, error) {
		accountsMu.Lock()
		b := backends[accountID]
		accountsMu.Unlock()
		if b != nil {
			return b, nil
		}
		return nil, fmt.Errorf("account %d has no connected client", accountID)
	}
	deps.FetchBody = func(ctx context.Context, accountID int64, gmailID string) error {
		c, err := clientFor(accountID)
		if err != nil {
			return err
		}
		done := act.Begin("fetch", emailOf(accountID), "body")
		err = engine.FetchBody(ctx, c, accountID, gmailID)
		done(doneNote(err))
		return err
	}
	deps.ModifyLabels = func(ctx context.Context, accountID int64, gmailIDs []string, add, remove []string) error {
		c, err := clientFor(accountID)
		if err != nil {
			return err
		}
		err = engine.ModifyLabelsBatch(ctx, c, accountID, gmailIDs, add, remove)
		// Instant local change + async mirror — log it as a completed op.
		act.Report("mail", emailOf(accountID), labelChangeSummary(add, remove, len(gmailIDs)), doneNote(err))
		return err
	}
	// Snooze/Unsnooze route through the manager so every snooze is mirrored to
	// the provider (wake-anywhere) — the UI never touches the snoozes table
	// directly when these are wired.
	deps.Snooze = func(ctx context.Context, accountID int64, threadID string, until time.Time) error {
		return snoozeMgr.Snooze(ctx, accountID, threadID, until)
	}
	deps.Unsnooze = func(ctx context.Context, accountID int64, threadID string) error {
		return snoozeMgr.Unsnooze(ctx, accountID, threadID)
	}
	deps.Send = func(ctx context.Context, accountID int64, msg model.OutgoingMessage) error {
		c, err := clientFor(accountID)
		if err != nil {
			return err
		}
		done := act.Begin("send", emailOf(accountID), "message")
		err = engine.Send(ctx, c, accountID, msg)
		done(doneNote(err))
		return err
	}
	// EnqueueSend persists the message to the outbox immediately (durable across a
	// quit) with a not_before undo window; the background sweeper delivers it once
	// the window elapses. It doesn't need a backend, so it works even for an
	// account whose backend failed to build — the sweeper picks it up once the
	// account reconnects, rather than the send being lost.
	deps.EnqueueSend = func(ctx context.Context, accountID int64, msg model.OutgoingMessage, notBefore int64) (int64, error) {
		return engine.EnqueueSend(ctx, accountID, msg, notBefore)
	}
	deps.SaveDraft = func(ctx context.Context, accountID int64, msg model.OutgoingMessage) error {
		c, err := clientFor(accountID)
		if err != nil {
			return err
		}
		done := act.Begin("draft", emailOf(accountID), "save")
		err = engine.SaveDraft(ctx, c, accountID, msg)
		done(doneNote(err))
		return err
	}
	deps.FindDraftID = func(ctx context.Context, accountID int64, gmailID string) (string, error) {
		c, err := clientFor(accountID)
		if err != nil {
			return "", err
		}
		return c.FindDraftID(ctx, gmailID)
	}
	deps.OpenAttach = func(ctx context.Context, accountID int64, gmailID string, attID int64) (string, error) {
		c, err := clientFor(accountID)
		if err != nil {
			return "", err
		}
		done := act.Begin("attach", emailOf(accountID), "download")
		path, err := engine.OpenAttachment(ctx, c, gmailID, attID)
		done(doneNote(err))
		return path, err
	}
	deps.Sync = func(ctx context.Context, accountID int64) error {
		c, err := clientFor(accountID)
		if err != nil {
			return err
		}
		done := act.Begin("sync", emailOf(accountID), "now")
		n, err := engine.Incremental(ctx, c, accountID)
		if errors.Is(err, syncer.ErrHistoryExpired) {
			n, err = engine.Resync(ctx, c, accountID, resyncBackfillLimit)
		}
		if err != nil {
			done(doneNote(err))
		} else {
			done(fmt.Sprintf("%d change(s)", n))
		}
		return err
	}
	deps.SearchServer = func(ctx context.Context, accountID int64, query string, max int) ([]string, error) {
		c, err := clientFor(accountID)
		if err != nil {
			return nil, err
		}
		done := act.Begin("search", emailOf(accountID), "all mail")
		ids, err := engine.SearchServer(ctx, c, accountID, query, max)
		if err != nil {
			done(doneNote(err))
		} else {
			done(fmt.Sprintf("%d result(s)", len(ids)))
		}
		return ids, err
	}
	deps.MarkAllRead = func(ctx context.Context, accountID int64, labelID string) error {
		c, err := clientFor(accountID)
		if err != nil {
			return err
		}
		done := act.Begin("mail", emailOf(accountID), "Mark "+labelID+" read")
		err = engine.MarkLabelRead(ctx, c, accountID, labelID)
		done(doneNote(err))
		return err
	}
	deps.SweepOutbox = func(ctx context.Context, accountID int64) error {
		c, err := clientFor(accountID)
		if err != nil {
			return err
		}
		done := act.Begin("send", emailOf(accountID), "outbox")
		n, err := engine.SweepOutbox(ctx, c, accountID)
		if err != nil {
			done(doneNote(err))
		} else {
			done(fmt.Sprintf("%d sent", n))
		}
		return err
	}
	deps.RetryOutbox = func(ctx context.Context, accountID, id int64) error {
		c, err := clientFor(accountID)
		if err != nil {
			return err
		}
		return engine.RetryOutbox(ctx, c, accountID, id)
	}
	// Discarding needs no Gmail client, so a stuck send can be cleared even
	// when the account currently has no working connection.
	deps.DiscardOutbox = func(ctx context.Context, accountID, id int64) (bool, error) {
		return engine.DiscardOutbox(ctx, accountID, id)
	}
	deps.DeleteForever = func(ctx context.Context, accountID int64, gmailIDs []string) error {
		c, err := clientFor(accountID)
		if err != nil {
			return err
		}
		done := act.Begin("mail", emailOf(accountID), fmt.Sprintf("Delete %d forever", len(gmailIDs)))
		err = engine.DeletePermanently(ctx, c, accountID, gmailIDs)
		done(doneNote(err))
		return err
	}
	deps.EmptyFolder = func(ctx context.Context, accountID int64, labelID string) (int, error) {
		c, err := clientFor(accountID)
		if err != nil {
			return 0, err
		}
		done := act.Begin("mail", emailOf(accountID), "Empty "+labelID)
		n, err := engine.EmptyLabel(ctx, c, accountID, labelID)
		if err != nil {
			done(doneNote(err))
		} else {
			done(fmt.Sprintf("%d deleted", n))
		}
		return n, err
	}

	if asst, err := buildAssistant(); err != nil {
		fmt.Fprintf(os.Stderr, "AI features disabled (%v)\n", err)
	} else if asst != nil {
		deps.Assistant = asst
		// Any change in the model serving requests — a failover to a backup, a
		// recovery to the primary, a settings swap — drops its own row into the
		// activity log, so the current model is visible beyond per-op notes.
		asst.SetOnModelChange(func(prev, cur string) {
			note := cur
			if prev != "" {
				note = cur + " (was " + prev + ")"
			}
			act.Report("ai", "", "model", note)
		})
		// Background categorization for every connected account — new mail is
		// tagged as it arrives (plus a catch-up sweep at launch), so switching
		// accounts shows ready tags instead of kicking off classification.
		worker := aiwork.New(st, asst, hub, act, func() bool {
			p, _ := config.LoadPrefs()
			return !p.DisableInboxCategories
		})
		go worker.Run(ctx)
		deps.RecategorizeInbox = worker.Trigger
	}

	return ui.Run(deps, mailto)
}

// labelChangeSummary renders a label mutation for the activity log, e.g.
// "+TRASH −INBOX · 3 msgs".
func labelChangeSummary(add, remove []string, n int) string {
	var parts []string
	for _, l := range add {
		parts = append(parts, "+"+l)
	}
	for _, l := range remove {
		parts = append(parts, "−"+l)
	}
	if len(parts) == 0 {
		parts = append(parts, "labels")
	}
	return fmt.Sprintf("%s · %d msg(s)", strings.Join(parts, " "), n)
}

// buildAssistant constructs the AI assistant from the config file + key (keyring
// or MAILBOX_AI_KEY). Returns (nil, nil) when AI is not configured.
func buildAssistant() (*ai.Assistant, error) {
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		return nil, err
	}
	cfg, err := ai.LoadConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	if !cfg.Configured() {
		return nil, nil
	}
	p, err := ai.NewProvider(cfg, aiKeyFor)
	if err != nil {
		return nil, err
	}
	return ai.NewAssistant(p), nil
}

// buildBackendForAccount builds the right provider backend for an account based
// on its type: the Gmail REST backend (also returning the raw client, for API
// stats) or an IMAP backend (client is nil — IMAP reports no Gmail stats).
func buildBackendForAccount(ctx context.Context, a model.Account) (backend.Backend, *gmailapi.Client, error) {
	if a.Type == model.AccountIMAP {
		b, err := buildIMAPBackend(ctx, a)
		return b, nil, err
	}
	client, err := buildClientForAccount(ctx, a.Email)
	if err != nil {
		return nil, nil, err
	}
	return gmailbackend.New(client, a.ID), client, nil
}

// buildIMAPBackend assembles an IMAP backend from the stored connection config
// and a credential (app password or OAuth token source).
func buildIMAPBackend(ctx context.Context, a model.Account) (backend.Backend, error) {
	cfg, ok, err := config.LoadIMAPAccount(a.Email)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no IMAP config for %s", a.Email)
	}
	cred, err := buildIMAPCredential(ctx, a.Email, usernameOf(cfg), cfg)
	if err != nil {
		return nil, err
	}
	return imapbackend.New(imapConfigOf(cfg), a.ID, cred), nil
}

// upsertAccountKeepingCursor creates or updates an account by email without
// blanking an existing one's sync cursor / backfill timestamp — re-adding an
// account (e.g. to fix a password) must not trigger a needless full re-sync.
func upsertAccountKeepingCursor(ctx context.Context, st *store.Store, email, accountType string) error {
	acct := model.Account{Email: email, Type: accountType}
	if existing, err := st.GetAccountByEmail(ctx, email); err == nil {
		acct = existing // preserve cursor, backfilled_at, scopes, display name
		acct.Type = accountType
	}
	_, err := st.UpsertAccount(ctx, acct)
	return err
}

// usernameOf is the IMAP/SMTP login username (the email unless overridden).
func usernameOf(a config.IMAPAccount) string {
	if a.Username != "" {
		return a.Username
	}
	return a.Email
}

// imapConfigOf maps the persisted account config to the backend's Config.
func imapConfigOf(a config.IMAPAccount) imapbackend.Config {
	return imapbackend.Config{
		Host: a.IMAPHost, Port: a.IMAPPort, Security: imapbackend.Security(a.IMAPSecurity),
		Username: usernameOf(a), Email: a.Email,
		SMTPHost: a.SMTPHost, SMTPPort: a.SMTPPort, SMTPSecurity: imapbackend.Security(a.SMTPSecurity),
	}
}

// buildIMAPCredential picks the credential flow recorded for the account: a
// keyring app password, or an auto-refreshing OAuth token source (Gmail-mail or
// Microsoft).
func buildIMAPCredential(ctx context.Context, email, username string, cfg config.IMAPAccount) (imapbackend.Credential, error) {
	switch cfg.Auth {
	case config.AuthPassword:
		pw, err := auth.LoadIMAPSecret(email)
		if err != nil {
			return nil, err
		}
		return imapbackend.PasswordAuth(username, pw), nil
	case config.AuthGoogle:
		cc, err := auth.LoadClientConfig(credentialsPath())
		if err != nil {
			return nil, err
		}
		ts, err := auth.GoogleMailTokenSource(ctx, cc, email, time.Time{})
		if err != nil {
			return nil, err
		}
		return imapbackend.OAuthAuth(username, ts), nil
	case config.AuthMicrosoft:
		clientID := microsoftClientID()
		if clientID == "" {
			return nil, fmt.Errorf("no Microsoft OAuth client id (set MAILBOX_MS_CLIENT_ID)")
		}
		ts, err := auth.MicrosoftTokenSource(ctx, clientID, email, time.Time{})
		if err != nil {
			return nil, err
		}
		return imapbackend.OAuthAuth(username, ts), nil
	default:
		return nil, fmt.Errorf("unknown auth kind %q for %s", cfg.Auth, email)
	}
}

// microsoftClientID is the Azure app registration's public client id used for
// Outlook/Office 365 OAuth (a SETUP step, like the Google credentials).
func microsoftClientID() string { return os.Getenv("MAILBOX_MS_CLIENT_ID") }

// gmailVerifiedEmail reads the signed-in Gmail account's address from its profile
// so the account is keyed by the address the user actually authenticated as, not
// whatever was typed. Best-effort: returns "" on failure (the caller then keeps
// the typed address).
func gmailVerifiedEmail(ctx context.Context, tok *oauth2.Token) string {
	srv, err := gmailapi.NewService(ctx, oauth2.StaticTokenSource(tok), &gmailapi.Stats{})
	if err != nil {
		slog.Warn("verify gmail email: service", "err", err)
		return ""
	}
	prof, err := gmailapi.NewClient(srv).GetProfile(ctx)
	if err != nil {
		slog.Warn("verify gmail email: profile", "err", err)
		return ""
	}
	return prof.EmailAddress
}

// buildClientForAccount builds a Gmail client from the keyring refresh token and
// the OAuth client credentials. It never opens a browser; an account must have
// been connected via `mailbox sync` first.
func buildClientForAccount(ctx context.Context, email string) (*gmailapi.Client, error) {
	credPath := credentialsPath()
	cc, err := auth.LoadClientConfig(credPath)
	if err != nil {
		return nil, fmt.Errorf("load credentials from %s: %w", credPath, err)
	}
	if _, err := auth.LoadRefreshToken(email); err != nil {
		return nil, fmt.Errorf("no stored token for %s: %w", email, err)
	}
	ts, err := auth.TokenSource(ctx, cc, email, time.Time{})
	if err != nil {
		return nil, err
	}
	// A byte-counting service + a client sharing its Stats, so the status bar can
	// report requests, quota units, and bytes transferred.
	stats := &gmailapi.Stats{}
	srv, err := gmailapi.NewService(ctx, ts, stats)
	if err != nil {
		return nil, err
	}
	return gmailapi.NewClientStats(srv, stats), nil
}

// sweepInterval is how often the outbox is retried while the GUI is open.
const sweepInterval = 45 * time.Second

// backgroundSweep retries queued outbox messages on a timer. A quiet tick (an
// empty outbox) is not logged — only sweeps that delivered something or failed.
func backgroundSweep(ctx context.Context, engine *syncer.Engine, act *activity.Hub, b backend.Backend, accountID int64, email string) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		n, err := engine.SweepOutbox(ctx, b, accountID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "outbox sweep: %v\n", err)
			act.Report("send", email, "outbox", doneNote(err))
		} else if n > 0 {
			act.Report("send", email, "outbox", fmt.Sprintf("%d sent", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// retentionDelay defers the first retention pass past launch so it never
// competes with startup sync for the write lock; retentionInterval re-runs it
// daily for a long-lived session. The pref is re-read every pass, so a changed
// setting applies without a restart (Preferences also triggers an immediate
// pass on change).
const (
	retentionDelay    = 2 * time.Minute
	retentionInterval = 24 * time.Hour
	// retentionVacuumMin is the pruned-message count from which a pass is worth
	// a follow-up Vacuum — below it the freed pages aren't worth rewriting the
	// whole DB file for; they'll be reused by new mail anyway.
	retentionVacuumMin = 200
)

// backgroundRetention applies the body-retention preference (see
// store.PruneBodies): old message bodies are cleared, metadata kept.
func backgroundRetention(ctx context.Context, st *store.Store, act *activity.Hub) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(retentionDelay):
	}
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()
	for {
		runRetentionPass(ctx, st, act)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runRetentionPass prunes bodies older than the configured window (no-op when
// retention is off) and compacts the DB when the pass freed enough to matter.
// A pass that pruned something (or failed) is reported to the activity log; a
// quiet no-op pass is not.
func runRetentionPass(ctx context.Context, st *store.Store, act *activity.Hub) {
	prefs, err := config.LoadPrefs()
	if err != nil || prefs.BodyRetentionDays <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -prefs.BodyRetentionDays).Unix()
	n, err := st.PruneBodies(ctx, cutoff)
	if err != nil {
		slog.Warn("body retention: prune", "err", err)
		act.Report("mail", "", "Body retention", doneNote(err))
		return
	}
	if n == 0 {
		return
	}
	slog.Info("body retention: pruned old message bodies", "count", n, "days", prefs.BodyRetentionDays)
	act.Report("mail", "", "Body retention", fmt.Sprintf("%d bodies (>%dd)", n, prefs.BodyRetentionDays))
	if n >= retentionVacuumMin {
		if err := st.Vacuum(ctx); err != nil {
			slog.Warn("body retention: vacuum", "err", err)
		}
	}
}

// snoozeWakeInterval is how often due snoozes are checked while the GUI is
// open. A snooze that comes due while the app is closed wakes on next launch.
const snoozeWakeInterval = time.Minute

// backgroundSnoozeWake returns conversations whose snooze elapsed to the
// inbox: locally (the row is marked notified and lingers for the list's
// "Snoozed" tag; SnoozeWoke drives the refresh + reminder notification) and on
// every other client via the label mirror (+INBOX, −Snoozed labels).
func backgroundSnoozeWake(ctx context.Context, mgr *snooze.Manager, act *activity.Hub) {
	ticker := time.NewTicker(snoozeWakeInterval)
	defer ticker.Stop()
	for {
		if woke := mgr.WakeDue(ctx, time.Now()); woke > 0 {
			act.Report("mail", "", "Snooze woke", fmt.Sprintf("%d", woke))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// backgroundSync runs an incremental sync immediately and then on a timer,
// reporting each pass to the activity hub for the status bar.
// reconcile, when non-nil, runs after every successful pass — it converges
// snooze rows with the label state the pass just pulled in.
func backgroundSync(ctx context.Context, engine *syncer.Engine, act *activity.Hub, b backend.Backend, accountID int64, email string, wake <-chan struct{}, reconcile func(context.Context)) {
	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()
	consecFails := 0
	for {
		done := act.Begin("sync", email, "")
		var (
			n   int
			err error
		)
		// Per-pass backstop deadline — see syncPassTimeout.
		passCtx, cancelPass := context.WithTimeout(ctx, syncPassTimeout)
		if acc, aerr := engine.Store.GetAccountByID(passCtx, accountID); aerr == nil && acc.SyncCursor == "" {
			// Never backfilled (a freshly added IMAP account, or a Gmail account
			// connected without the headless `sync`): do the initial labels +
			// backfill, which seeds the cursor.
			if _, lerr := engine.SyncLabels(passCtx, b, accountID); lerr != nil {
				fmt.Fprintf(os.Stderr, "initial label sync for %s: %v\n", email, lerr)
			}
			n, err = engine.Resync(passCtx, b, accountID, resyncBackfillLimit)
			if err == nil {
				_ = engine.Store.SetBackfilledAt(passCtx, accountID, time.Now())
			}
		} else {
			n, err = engine.Incremental(passCtx, b, accountID)
			if errors.Is(err, syncer.ErrHistoryExpired) {
				// Cursor too old (offline past the provider's change window). Recover
				// by re-backfilling and resetting it, else incremental fails forever.
				fmt.Fprintf(os.Stderr, "background sync: cursor expired for %s, resyncing\n", email)
				n, err = engine.Resync(passCtx, b, accountID, resyncBackfillLimit)
			}
		}
		cancelPass()
		if err != nil {
			if auth.IsAuthError(err) || errors.Is(err, backend.ErrAuth) {
				// Revoked/expired OAuth token, or a rejected IMAP password — can't
				// recover without re-auth; tell the UI to prompt a reconnect.
				engine.NotifyAuthExpired(accountID)
			}
			done("error: " + err.Error())
			fmt.Fprintf(os.Stderr, "background sync: %v\n", err)
			// Back off on repeated failures instead of re-running a possibly heavy
			// pass every tick; IDLE nudges are ignored while backing off (the wake
			// channel holds at most one, consumed by the next healthy select).
			consecFails++
			wait := syncBackoff(consecFails)
			logging.Trace("launch: sync backing off", "account", accountID, "fails", consecFails, "wait", wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		consecFails = 0
		if reconcile != nil {
			reconcile(ctx)
		}
		if n > 0 {
			done(fmt.Sprintf("%d change(s)", n))
		} else {
			done("up to date")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake: // a push notification (IMAP IDLE) — sync now
		}
	}
}

// htmlBackfillCap bounds how many text-only messages one launch re-fetches for
// HTML recovery. Generous enough to clear a typical cache in one run, bounded so
// a huge cache (or a flaky network) stays a gentle background trickle; whatever
// is left — including any that failed to fetch — is retried on the next launch.
const htmlBackfillCap = 2000

// htmlBackfillDelay holds the recovery pass back briefly so it never competes
// with the initial sync and UI startup for network/CPU.
const htmlBackfillDelay = 25 * time.Second

// backgroundBackfillHTML runs the one-time HTML re-fetch (see
// engine.BackfillHTMLBodies) once, after a short startup delay. A no-op once the
// account's bodies are all at the current fetch version.
func backgroundBackfillHTML(ctx context.Context, engine *syncer.Engine, act *activity.Hub, b backend.Backend, accountID int64, email string) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(htmlBackfillDelay):
	}
	done := act.Begin("sync", email, "HTML")
	n, err := engine.BackfillHTMLBodies(ctx, b, accountID, htmlBackfillCap)
	switch {
	case err != nil:
		done("error: " + err.Error())
		fmt.Fprintf(os.Stderr, "html backfill for %s: %v\n", email, err)
	case n > 0:
		done(fmt.Sprintf("recovered %d", n))
	default:
		done("")
	}
}

// doneNote summarizes an operation's result for the activity log.
func doneNote(err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return ""
}

// cacheDir is the app's cache directory (attachments etc.).
func cacheDir() string {
	c, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(c, "mailbox")
}

// dirSize sums the sizes of all regular files under dir (0 if missing).
func dirSize(dir string) int64 {
	if dir == "" {
		return 0
	}
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, e := d.Info(); e == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}
