package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/auth"
	"github.com/jsnjack/mailbox/internal/config"
	"github.com/jsnjack/mailbox/internal/gmailapi"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
	"github.com/jsnjack/mailbox/internal/syncer"
	"github.com/jsnjack/mailbox/internal/ui"
	"github.com/zalando/go-keyring"
	gmailv1 "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// aiKeyringService is the keyring collection for the AI provider API key.
const aiKeyringService = "mailbox-ai"

// syncInterval is how often the background incremental sync runs while the GUI
// is open.
const syncInterval = 60 * time.Second

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
	acc := accounts[0]

	deps := ui.Deps{
		Store:        st,
		AccountID:    acc.ID,
		AccountEmail: acc.Email,
	}

	// If credentials are available, wire a live client for lazy body fetch and a
	// background incremental sync. Otherwise the UI renders the cache read-only.
	if client, err := buildClientForAccount(ctx, acc.Email); err != nil {
		fmt.Fprintf(os.Stderr, "live sync disabled (%v); rendering cached mail read-only\n", err)
	} else {
		hub := syncer.NewHub()
		engine := syncer.NewEngine(st, hub)
		deps.Hub = hub
		deps.FetchBody = func(ctx context.Context, accountID int64, gmailID string) error {
			return engine.FetchBody(ctx, client, accountID, gmailID)
		}
		deps.ModifyLabels = func(ctx context.Context, accountID int64, gmailID string, add, remove []string) error {
			return engine.ModifyLabels(ctx, client, accountID, gmailID, add, remove)
		}
		deps.Send = func(ctx context.Context, msg model.OutgoingMessage) error {
			return engine.Send(ctx, client, acc.ID, msg)
		}
		deps.OpenAttach = func(ctx context.Context, gmailID string, attID int64) (string, error) {
			return engine.OpenAttachment(ctx, client, gmailID, attID)
		}
		go backgroundSync(ctx, engine, client, acc.ID)
		go backgroundSweep(ctx, engine, client, acc.ID)
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
	srv, err := gmailv1.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, err
	}
	return gmailapi.NewClient(srv), nil
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

// backgroundSync runs an incremental sync immediately and then on a timer.
func backgroundSync(ctx context.Context, engine *syncer.Engine, client *gmailapi.Client, accountID int64) {
	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()
	for {
		if _, err := engine.Incremental(ctx, client, accountID); err != nil {
			fmt.Fprintf(os.Stderr, "background sync: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
