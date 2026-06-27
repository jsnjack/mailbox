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

	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
	"github.com/jsnjack/mailbox/internal/syncer"
)

// appID is the GTK/D-Bus application identifier.
const appID = "com.surfly.mailbox"

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

// Deps are the dependencies the UI needs. FetchBody, ModifyLabels and Hub may be
// nil (the UI then renders the cache read-only without live updates, on-demand
// bodies, or message actions).
type Deps struct {
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

	// AISettings/SaveAISettings read and persist the [ai] config (provider,
	// endpoint, model). Always wired, independent of whether an account exists.
	AISettings     func() (provider, endpoint, model string)
	SaveAISettings func(provider, endpoint, model string) error
	// TestAISettings validates the given AI settings (plus the stored key) with a
	// tiny live request; nil result means the connection works.
	TestAISettings func(ctx context.Context, provider, endpoint, model string) error
}

// Run launches the GTK application and blocks until the window is closed.
func Run(deps Deps) error {
	// Notifications are routed by the desktop environment via the app id's
	// installed desktop entry; make sure one exists before any can fire.
	ensureDesktopFile()
	app := newAdwApplication()
	app.ConnectActivate(func() {
		slog.Debug("ui: activate")
		w := newWindow(app, deps)
		w.present()
		slog.Debug("ui: window presented")
	})
	code := app.Run([]string{"mailbox"})
	slog.Debug("ui: app.Run returned", "code", code)
	if code != 0 {
		return fmt.Errorf("gtk application exited with code %d", code)
	}
	return nil
}
