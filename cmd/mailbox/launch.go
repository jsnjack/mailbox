package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jsnjack/mailbox/internal/activity"
	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/auth"
	"github.com/jsnjack/mailbox/internal/config"
	"github.com/jsnjack/mailbox/internal/gmailapi"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
	"github.com/jsnjack/mailbox/internal/syncer"
	"github.com/jsnjack/mailbox/internal/ui"
	"github.com/zalando/go-keyring"
)

// aiKeyringService is the keyring collection for the AI provider API key.
const aiKeyringService = "mailbox-ai"

// syncInterval is how often the background incremental sync runs while the GUI
// is open.
const syncInterval = 60 * time.Second

// resyncBackfillLimit bounds how many newest messages a recovery re-backfills
// when an expired history watermark forces a resync (see engine.Resync).
const resyncBackfillLimit = 500

// launchUI opens the store, picks the first connected account, optionally builds
// a live Gmail client (when credentials are available), starts a background
// incremental sync, and runs the GTK application.
func launchUI() error {
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
	if len(accounts) == 0 {
		return fmt.Errorf("no account connected yet; run: mailbox sync --account <email> --credentials <client_secret.json>")
	}

	deps := ui.Deps{Store: st, Version: Version}
	for _, a := range accounts {
		deps.Accounts = append(deps.Accounts, ui.AccountInfo{ID: a.ID, Email: a.Email})
	}

	// Activity hub feeds the status bar (Stats, below, feeds its metrics).
	act := activity.NewHub()
	deps.Activity = act

	// AI settings are editable regardless of account/client state.
	if cfgPath, err := config.ConfigFilePath(); err == nil {
		deps.AISettings = func() (string, string, string) {
			c, err := ai.LoadConfig(cfgPath)
			if err != nil {
				slog.Warn("load ai config", "err", err)
			}
			return c.Provider, c.Endpoint, c.Model
		}
		deps.SaveAISettings = func(provider, endpoint, model string) error {
			return ai.SaveConfig(cfgPath, ai.Config{Provider: provider, Endpoint: endpoint, Model: model})
		}
		deps.TestAISettings = func(ctx context.Context, provider, endpoint, model string) error {
			cfg := ai.Config{Provider: provider, Endpoint: endpoint, Model: model}
			if !cfg.Configured() {
				return fmt.Errorf("provider, endpoint, and model are required")
			}
			key := os.Getenv("MAILBOX_AI_KEY")
			if key == "" {
				key, _ = keyring.Get(aiKeyringService, provider)
			}
			p, err := ai.NewProvider(cfg, key)
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
	clients := make(map[int64]*gmailapi.Client)
	for _, a := range accounts {
		client, err := buildClientForAccount(ctx, a.Email)
		if err != nil {
			fmt.Fprintf(os.Stderr, "live features disabled for %s (%v)\n", a.Email, err)
			continue
		}
		clients[a.ID] = client
		go backgroundSync(ctx, engine, act, client, a.ID, a.Email)
		go backgroundSweep(ctx, engine, client, a.ID)
	}

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
		for _, c := range clients {
			cs := c.Stats()
			s.Requests += cs.Requests
			s.QuotaUnits += cs.QuotaUnits
			s.BytesIn += cs.BytesIn
			s.BytesOut += cs.BytesOut
		}
		// Include AI provider traffic so "data transferred" reflects all network
		// activity, not just Gmail.
		if deps.Assistant != nil {
			in, out := deps.Assistant.Transferred()
			s.BytesIn += in
			s.BytesOut += out
		}
		return s
	}

	if len(clients) > 0 {
		deps.Hub = hub
		clientFor := func(accountID int64) (*gmailapi.Client, error) {
			if c := clients[accountID]; c != nil {
				return c, nil
			}
			return nil, fmt.Errorf("account %d has no connected client", accountID)
		}
		deps.FetchBody = func(ctx context.Context, accountID int64, gmailID string) error {
			c, err := clientFor(accountID)
			if err != nil {
				return err
			}
			done := act.Begin("fetch", "Fetching message")
			err = engine.FetchBody(ctx, c, accountID, gmailID)
			done(doneNote(err))
			return err
		}
		deps.ModifyLabels = func(ctx context.Context, accountID int64, gmailIDs []string, add, remove []string) error {
			c, err := clientFor(accountID)
			if err != nil {
				return err
			}
			return engine.ModifyLabelsBatch(ctx, c, accountID, gmailIDs, add, remove)
		}
		deps.Send = func(ctx context.Context, accountID int64, msg model.OutgoingMessage) error {
			c, err := clientFor(accountID)
			if err != nil {
				return err
			}
			done := act.Begin("send", "Sending message")
			err = engine.Send(ctx, c, accountID, msg)
			done(doneNote(err))
			return err
		}
		deps.SaveDraft = func(ctx context.Context, accountID int64, msg model.OutgoingMessage) error {
			c, err := clientFor(accountID)
			if err != nil {
				return err
			}
			return engine.SaveDraft(ctx, c, accountID, msg)
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
			return engine.OpenAttachment(ctx, c, gmailID, attID)
		}
		deps.Sync = func(ctx context.Context, accountID int64) error {
			c, err := clientFor(accountID)
			if err != nil {
				return err
			}
			_, err = engine.Incremental(ctx, c, accountID)
			if errors.Is(err, syncer.ErrHistoryExpired) {
				_, err = engine.Resync(ctx, c, accountID, resyncBackfillLimit)
			}
			return err
		}
		deps.SearchServer = func(ctx context.Context, accountID int64, query string, max int) ([]string, error) {
			c, err := clientFor(accountID)
			if err != nil {
				return nil, err
			}
			done := act.Begin("search", "Searching all mail")
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
			return engine.MarkLabelRead(ctx, c, accountID, labelID)
		}
		deps.SweepOutbox = func(ctx context.Context, accountID int64) error {
			c, err := clientFor(accountID)
			if err != nil {
				return err
			}
			_, err = engine.SweepOutbox(ctx, c, accountID)
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
		deps.DiscardOutbox = func(ctx context.Context, accountID, id int64) error {
			return engine.DiscardOutbox(ctx, accountID, id)
		}
		deps.DeleteForever = func(ctx context.Context, accountID int64, gmailIDs []string) error {
			c, err := clientFor(accountID)
			if err != nil {
				return err
			}
			return engine.DeletePermanently(ctx, c, accountID, gmailIDs)
		}
		deps.EmptyFolder = func(ctx context.Context, accountID int64, labelID string) (int, error) {
			c, err := clientFor(accountID)
			if err != nil {
				return 0, err
			}
			return engine.EmptyLabel(ctx, c, accountID, labelID)
		}
	}

	if asst, err := buildAssistant(); err != nil {
		fmt.Fprintf(os.Stderr, "AI features disabled (%v)\n", err)
	} else if asst != nil {
		deps.Assistant = asst
	}

	return ui.Run(deps)
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
	key := os.Getenv("MAILBOX_AI_KEY")
	if key == "" {
		key, _ = keyring.Get(aiKeyringService, cfg.Provider) // empty is fine for keyless proxies
	}
	p, err := ai.NewProvider(cfg, key)
	if err != nil {
		return nil, err
	}
	return ai.NewAssistant(p), nil
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

// backgroundSweep retries queued outbox messages on a timer.
func backgroundSweep(ctx context.Context, engine *syncer.Engine, client *gmailapi.Client, accountID int64) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		if _, err := engine.SweepOutbox(ctx, client, accountID); err != nil {
			fmt.Fprintf(os.Stderr, "outbox sweep: %v\n", err)
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
func backgroundSync(ctx context.Context, engine *syncer.Engine, act *activity.Hub, client *gmailapi.Client, accountID int64, email string) {
	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()
	for {
		done := act.Begin("sync", "Syncing "+email)
		n, err := engine.Incremental(ctx, client, accountID)
		if errors.Is(err, syncer.ErrHistoryExpired) {
			// Watermark too old (offline past Gmail's history window). Recover by
			// re-backfilling and resetting it, else incremental fails forever.
			fmt.Fprintf(os.Stderr, "background sync: history expired for %s, resyncing\n", email)
			n, err = engine.Resync(ctx, client, accountID, resyncBackfillLimit)
		}
		if err != nil {
			if auth.IsAuthError(err) {
				// Revoked/expired refresh token — can't recover without re-login;
				// tell the UI so it can prompt the user to reconnect.
				engine.NotifyAuthExpired(accountID)
			}
			done("error: " + err.Error())
			fmt.Fprintf(os.Stderr, "background sync: %v\n", err)
		} else if n > 0 {
			done(fmt.Sprintf("%d change(s)", n))
		} else {
			done("up to date")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
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
