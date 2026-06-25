// Package ui is the GTK4/libadwaita presentation layer. It is the only package
// that imports GTK; it reads the store and renders, and never holds the
// canonical data. Background work reaches the UI exclusively through
// internal/dispatch (the GTK main-thread bridge).
package ui

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
	"github.com/jsnjack/mailbox/internal/syncer"
)

// appID is the GTK/D-Bus application identifier.
const appID = "com.surfly.mailbox"

// BodyFetcher lazily downloads and caches a message body when it is opened.
type BodyFetcher func(ctx context.Context, accountID int64, gmailID string) error

// LabelModifier applies a label delta to a message (optimistic local update plus
// the mirrored Gmail call).
type LabelModifier func(ctx context.Context, accountID int64, gmailID string, add, remove []string) error

// Sender transmits an outgoing message.
type Sender func(ctx context.Context, msg model.OutgoingMessage) error

// AttachmentOpener ensures an attachment is cached locally and returns its path.
type AttachmentOpener func(ctx context.Context, gmailID string, attID int64) (string, error)

// SyncNow runs an immediate incremental sync.
type SyncNow func(ctx context.Context) error

// Deps are the dependencies the UI needs. FetchBody, ModifyLabels and Hub may be
// nil (the UI then renders the cache read-only without live updates, on-demand
// bodies, or message actions).
type Deps struct {
	Store        *store.Store
	Hub          *syncer.Hub
	AccountID    int64
	AccountEmail string
	FetchBody    BodyFetcher
	ModifyLabels LabelModifier
	Send         Sender
	SaveDraft    Sender
	OpenAttach   AttachmentOpener
	Sync         SyncNow
	Assistant    *ai.Assistant
}

// Run launches the GTK application and blocks until the window is closed.
func Run(deps Deps) error {
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
