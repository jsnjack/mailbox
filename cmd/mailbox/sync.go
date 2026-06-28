package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jsnjack/mailbox/internal/auth"
	"github.com/jsnjack/mailbox/internal/config"
	"github.com/jsnjack/mailbox/internal/gmailapi"
	"github.com/jsnjack/mailbox/internal/gmailbackend"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
	"github.com/jsnjack/mailbox/internal/syncer"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
	gmailv1 "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

var (
	syncAccount string
	syncLimit   int
)

var syncCmd = &cobra.Command{
	Use:           "sync",
	Short:         "Sync a Gmail account into the local cache (headless)",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runSync,
}

func init() {
	f := syncCmd.Flags()
	f.StringVar(&syncAccount, "account", "", "Account email (uses the keyring refresh token if present; otherwise logs in).")
	f.IntVar(&syncLimit, "limit", 300, "Maximum number of newest messages to backfill (0 = all).")
	rootCmd.AddCommand(syncCmd)
}

func runSync(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	cc, err := auth.LoadClientConfig(credentialsPath())
	if err != nil {
		return err
	}

	srv, email, err := buildGmailService(ctx, cc, syncAccount)
	if err != nil {
		return err
	}
	client := gmailapi.NewClient(srv)

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

	// Capture the history watermark BEFORE backfill so messages arriving during
	// it aren't missed by the first incremental sync.
	prof, err := client.GetProfile(ctx)
	if err != nil {
		return err
	}
	accID, err := st.UpsertAccount(ctx, model.Account{
		Email:         email,
		LastHistoryID: fmt.Sprintf("%d", prof.HistoryId),
	})
	if err != nil {
		return err
	}
	fmt.Printf("Account %s (id=%d) — server total=%d, historyId=%d\n", email, accID, prof.MessagesTotal, prof.HistoryId)

	engine := syncer.NewEngine(st, nil)
	b := gmailbackend.New(client, accID)

	nLabels, err := engine.SyncLabels(ctx, b, accID)
	if err != nil {
		return fmt.Errorf("sync labels: %w", err)
	}
	fmt.Printf("Synced %d labels\n", nLabels)

	fmt.Printf("Backfilling newest %d messages...\n", syncLimit)
	start := time.Now()
	n, err := engine.Backfill(ctx, b, accID, "", syncLimit)
	if err != nil {
		return fmt.Errorf("backfill: %w", err)
	}
	if err := st.SetBackfilledAt(ctx, accID, time.Now()); err != nil {
		return err
	}
	fmt.Printf("Stored %d messages in %s\n", n, time.Since(start).Round(time.Millisecond))

	inbox, _ := st.CountByLabel(ctx, accID, model.LabelInbox)
	unread, _ := st.CountByLabel(ctx, accID, model.LabelUnread)
	fmt.Printf("Local cache: INBOX=%d  UNREAD=%d\n", inbox, unread)

	msgs, err := st.ListByLabel(ctx, accID, model.LabelInbox, 5, 0)
	if err != nil {
		return err
	}
	fmt.Println("Newest INBOX messages from the local cache:")
	for _, m := range msgs {
		flag := " "
		if m.IsUnread {
			flag = "•"
		}
		fmt.Printf("  %s %-32s | %s\n", flag, truncate(m.FromAddr, 32), truncate(m.Subject, 60))
	}

	changed, err := engine.Incremental(ctx, b, accID)
	if err != nil && !errors.Is(err, syncer.ErrHistoryExpired) {
		return fmt.Errorf("incremental: %w", err)
	}
	fmt.Printf("Incremental sync applied %d change(s)\n", changed)
	return nil
}

// buildGmailService returns a Gmail service for account. If a refresh token is
// in the keyring it is used silently; otherwise an interactive login runs and
// the new refresh token is saved under the profile's email.
func buildGmailService(ctx context.Context, cc auth.ClientConfig, account string) (*gmailv1.Service, string, error) {
	if account != "" {
		if _, err := auth.LoadRefreshToken(account); err == nil {
			ts, err := auth.TokenSource(ctx, cc, account, time.Time{})
			if err != nil {
				return nil, "", err
			}
			srv, err := gmailv1.NewService(ctx, option.WithTokenSource(ts))
			return srv, account, err
		}
	}

	tok, err := auth.Login(ctx, cc)
	if err != nil {
		return nil, "", fmt.Errorf("login: %w", err)
	}
	srv, err := gmailv1.NewService(ctx, option.WithTokenSource(oauth2.StaticTokenSource(tok)))
	if err != nil {
		return nil, "", err
	}
	prof, err := srv.Users.GetProfile("me").Do()
	if err != nil {
		return nil, "", fmt.Errorf("get profile: %w", err)
	}
	if tok.RefreshToken != "" {
		if err := auth.SaveRefreshToken(prof.EmailAddress, tok.RefreshToken); err != nil {
			return nil, "", err
		}
	}
	return srv, prof.EmailAddress, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
