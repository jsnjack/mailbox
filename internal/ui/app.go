// Package ui is the GTK4/libadwaita presentation layer. It is the only package
// that imports GTK; it reads the store and renders, and never holds the
// canonical data. Background work reaches the UI exclusively through
// internal/dispatch (the GTK main-thread bridge).
package ui

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/jsnjack/mailbox/internal/activity"
	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/config"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
	"github.com/jsnjack/mailbox/internal/syncer"
)

// appID is the GTK/D-Bus application identifier.
const appID = "com.jsnjack.mailbox"

// applicationID is the id the GApplication registers under. A test sandbox can
// override it via MAILBOX_APP_ID so it runs as a distinct instance alongside a
// real one — same session bus (so the OS keyring still resolves OAuth tokens and
// the AI key), just a different bus name, so launching it doesn't merely activate
// the running app.
func applicationID() string {
	if id := os.Getenv("MAILBOX_APP_ID"); id != "" {
		return id
	}
	return appID
}

// AccountInfo identifies a connected account for the switcher.
type AccountInfo struct {
	ID    int64
	Email string
	// Type is the account_type ("gmail" or "imap"); it lets the UI preselect the
	// right provider when reconnecting and word the remove confirmation.
	Type string
}

// All operations are account-routed: the caller passes the account id so the
// dependency can dispatch to that account's Gmail client.

// BodyFetcher lazily downloads and caches a message body when it is opened.
type BodyFetcher func(ctx context.Context, accountID int64, gmailID string) error

// LabelModifier applies a label delta to many messages at once (optimistic local
// update plus a single mirrored Gmail BatchModify) — so acting on a whole
// conversation is one round-trip, not one per message.
type LabelModifier func(ctx context.Context, accountID int64, gmailIDs []string, add, remove []string) error

// Sender transmits an outgoing message from the given account.
type Sender func(ctx context.Context, accountID int64, msg model.OutgoingMessage) error

// AttachmentOpener ensures an attachment is cached locally and returns its path.
type AttachmentOpener func(ctx context.Context, accountID int64, gmailID string, attID int64) (string, error)

// DraftFinder resolves the Gmail draft resource id backing a stored draft
// message (Gmail tracks drafts by an id separate from the message id).
type DraftFinder func(ctx context.Context, accountID int64, gmailID string) (string, error)

// SyncNow runs an immediate incremental sync for an account.
type SyncNow func(ctx context.Context, accountID int64) error

// ServerSearcher runs a Gmail server-side search, caches the matches, and
// returns the matching message ids.
type ServerSearcher func(ctx context.Context, accountID int64, query string, max int) ([]string, error)

// LabelReader marks every unread message in a label as read.
type LabelReader func(ctx context.Context, accountID int64, labelID string) error

// OutboxSweeper attempts to send all of an account's queued messages now.
type OutboxSweeper func(ctx context.Context, accountID int64) error

// OutboxAction acts on a single outbox item (retry or discard) by id.
type OutboxAction func(ctx context.Context, accountID, id int64) error

// PermanentDeleter permanently deletes messages (bypassing Trash).
type PermanentDeleter func(ctx context.Context, accountID int64, gmailIDs []string) error

// FolderEmptier permanently deletes every message in a folder (Trash/Spam),
// returning the count removed.
type FolderEmptier func(ctx context.Context, accountID int64, labelID string) (int, error)

// StatusStats is a snapshot of cumulative counters shown in the status bar.
type StatusStats struct {
	Requests   int64 // Gmail API requests issued this session
	QuotaUnits int64 // Gmail API quota units spent this session
	BytesIn    int64 // bytes received from the Gmail API
	BytesOut   int64 // bytes sent to the Gmail API
	DBBytes    int64 // size of the SQLite cache on disk
	CacheBytes int64 // size of the attachment cache on disk
	Messages   int64 // cached message count
}

// Deps are the dependencies the UI needs. FetchBody, ModifyLabels and Hub may be
// nil (the UI then renders the cache read-only without live updates, on-demand
// bodies, or message actions).
type Deps struct {
	Version       string // app version string, shown in the About dialog
	Store         *store.Store
	Hub           *syncer.Hub
	Accounts      []AccountInfo
	FetchBody     BodyFetcher
	ModifyLabels  LabelModifier // batch: applies to a slice of message ids
	Send          Sender
	SaveDraft     Sender
	FindDraftID   DraftFinder
	OpenAttach    AttachmentOpener
	Sync          SyncNow
	SearchServer  ServerSearcher
	MarkAllRead   LabelReader
	SweepOutbox   OutboxSweeper
	RetryOutbox   OutboxAction
	DiscardOutbox OutboxAction
	DeleteForever PermanentDeleter
	EmptyFolder   FolderEmptier
	Assistant     *ai.Assistant

	// Activity carries transient "what the app is doing" events for the status
	// bar (sync, AI, search, fetch). May be nil. Stats, if set, returns a
	// snapshot of cumulative API/cache counters for the status bar's metrics.
	Activity *activity.Hub
	Stats    func() StatusStats

	// AISettings/SaveAISettings read and persist the [ai] config (provider,
	// endpoint, model). Always wired, independent of whether an account exists.
	AISettings     func() (provider, endpoint, model string)
	SaveAISettings func(provider, endpoint, model string) error
	// TestAISettings validates the given AI settings (plus the stored key) with a
	// tiny live request; nil result means the connection works.
	TestAISettings func(ctx context.Context, provider, endpoint, model string) error

	// IMAP account management, for the add-account dialog. TestIMAPAccount
	// validates a connection (login + folder list) with the given settings and
	// password. AddIMAPAccount persists the account (config + keyring secret +
	// account row); it begins syncing on the next launch. OAuthConnect runs the
	// browser OAuth flow for an OAuth provider and returns the refresh token to
	// store as the secret, plus the verified account email when the provider can
	// supply it (Gmail, from the profile) — empty otherwise, so the caller keeps
	// the typed address. Nil when no Gmail credentials are configured.
	TestIMAPAccount func(ctx context.Context, acct config.IMAPAccount, password string) error
	// AddIMAPAccount persists and immediately starts syncing the account, returning
	// its id so the UI can add it to the switcher (no restart needed). Re-adding an
	// existing account (same email) reconnects it in place, preserving its cache.
	AddIMAPAccount func(ctx context.Context, acct config.IMAPAccount, secret string) (accountID int64, err error)
	OAuthConnect   func(ctx context.Context, kind config.AuthKind) (email, refreshToken string, err error)
	// RemoveAccount stops the account's sync, deletes its cached data, and clears
	// its stored secret + per-account config. Nil when account management is off.
	RemoveAccount func(ctx context.Context, accountID int64) error
}

// Run launches the GTK application and blocks until the window is closed. mailto,
// when set, is a mailto: URI the app was invoked with (it's the default mail
// handler) — it opens a prefilled compose. GApplication routes it to an
// already-running instance, so clicking a mailto link reuses the open window.
func Run(deps Deps, mailto string) error {
	logging.Trace("ui: run", "app_id", applicationID(), "accounts", len(deps.Accounts), "mailto", mailto != "")
	// Notifications are routed by the desktop environment via the app id's
	// installed desktop entry; make sure one exists before any can fire.
	ensureDesktopFile()
	// HandlesOpen so a mailto: URI passed on the command line is delivered to the
	// "open" handler (and forwarded to the primary instance when one is running).
	app := adw.NewApplication(applicationID(), gio.ApplicationHandlesOpen)
	var win *window
	ensureWindow := func() *window {
		if win == nil {
			win = newWindow(app, deps)
			win.present()
			slog.Debug("ui: window presented")
			logging.Trace("ui: window presented")
		}
		return win
	}
	app.ConnectActivate(func() {
		slog.Debug("ui: activate")
		logging.Trace("ui: activate")
		ensureWindow()
	})
	app.ConnectOpen(func(files []gio.Filer, hint string) {
		slog.Debug("ui: open", "n", len(files))
		logging.Trace("ui: open handler", "n", len(files), "hint", hint)
		w := ensureWindow()
		for _, f := range files {
			uri := f.URI()
			logging.Trace("ui: mailto routed", "uri", uri)
			w.composeFromMailto(uri)
		}
	})
	argv := []string{"mailbox"}
	if mailto != "" {
		argv = append(argv, mailto)
	}
	code := app.Run(argv)
	slog.Debug("ui: app.Run returned", "code", code)
	logging.Trace("ui: app.Run returned", "code", code)
	if code != 0 {
		return fmt.Errorf("gtk application exited with code %d", code)
	}
	return nil
}
