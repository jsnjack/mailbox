// Package backend defines the provider-agnostic interface the sync engine drives.
// A Backend abstracts one account's mail provider (Gmail REST today; IMAP next)
// behind domain types (model.*), so the engine and the rest of the app never
// depend on a specific protocol. Implementations are per-account and may hold a
// long-lived client or connection.
package backend

import (
	"context"
	"errors"

	"github.com/jsnjack/mailbox/internal/model"
)

// ErrCursorExpired means the incremental-sync cursor is too old to use — Gmail's
// historyId fell out of the retention window, or an IMAP folder's UIDVALIDITY
// changed. The caller recovers with a full resync.
var ErrCursorExpired = errors.New("sync cursor expired; full resync required")

// ErrAuth means the account's credentials were rejected (a revoked/expired
// token, or a wrong IMAP password) — sync can't recover without the user
// re-authenticating. Backends wrap their auth failures with it so the launcher
// can surface a "reconnect" prompt instead of retrying forever. (Gmail's OAuth
// invalid_grant is detected separately by auth.IsAuthError.)
var ErrAuth = errors.New("authentication failed; reconnect required")

// ErrNotFound means a specific message no longer exists at the provider (e.g. it
// was deleted between a history record naming it and the fetch). Backends wrap a
// 404/vanished-message error with it so the sync engine can tell a genuinely
// gone message (safe to skip) from a transient fetch failure (must be retried,
// not skipped past). See FetchMetadata.
var ErrNotFound = errors.New("message not found")

// Profile identifies the connected account and seeds incremental sync.
type Profile struct {
	Email  string
	Cursor string // opaque incremental-sync watermark (Gmail: historyId)
}

// Watcher is an optional Backend capability: a provider that can push change
// notifications (IMAP IDLE) implements it so the app reacts in near-real-time
// instead of waiting for the next poll. Providers without it (Gmail REST) are
// driven purely by the poll loop.
type Watcher interface {
	// Watch blocks until ctx is cancelled, calling onChange whenever the server
	// signals new or changed mail. onChange must be cheap and non-blocking (it
	// just nudges the sync loop). It returns when ctx is done or the provider
	// can't watch (the caller then relies on polling).
	Watch(ctx context.Context, onChange func())
}

// Backend is one account's mail provider. Every id is a provider message id
// (a Gmail message id today). Implementations must be safe for concurrent use —
// the engine fans out fetches across goroutines.
type Backend interface {
	// Profile returns the account's address and current sync cursor.
	Profile(ctx context.Context) (Profile, error)

	// Labels returns the account's labels/folders as domain labels.
	Labels(ctx context.Context) ([]model.Label, error)

	// SearchIDs returns provider message ids matching query (provider search
	// syntax; empty = all), newest-first, capped to max (0 = no cap).
	SearchIDs(ctx context.Context, query string, max int) ([]string, error)

	// FetchMetadata fetches one message's headers, flags, and labels (no body).
	FetchMetadata(ctx context.Context, id string) (model.Message, error)

	// FetchBody fetches a message's rendered body and its attachment metadata.
	FetchBody(ctx context.Context, id string) (model.MessageBody, []model.Attachment, error)

	// FetchAttachment returns the decoded bytes of one attachment.
	FetchAttachment(ctx context.Context, msgID, attID string) ([]byte, error)

	// ApplyLabels adds and removes labels/flags across the given messages.
	ApplyLabels(ctx context.Context, ids []string, add, remove []string) error

	// Delete permanently removes the given messages (bypassing Trash).
	Delete(ctx context.Context, ids []string) error

	// Changes returns the message ids upserted and deleted since cursor, plus the
	// next cursor to persist. It returns ErrCursorExpired when cursor is too old.
	Changes(ctx context.Context, cursor string) (upserts, deletes []string, next string, err error)

	// Send transmits a raw RFC 5322 message. threadID, when set and supported,
	// files it into an existing conversation. Returns the new provider message id.
	Send(ctx context.Context, raw []byte, threadID string) (string, error)

	// SaveDraft stores raw as a new draft and returns its provider draft id.
	SaveDraft(ctx context.Context, raw []byte, threadID string) (string, error)

	// UpdateDraft replaces an existing draft's content, returning its draft id.
	UpdateDraft(ctx context.Context, draftID string, raw []byte, threadID string) (string, error)

	// DeleteDraft removes a draft by its provider draft id.
	DeleteDraft(ctx context.Context, draftID string) error

	// FindDraftID resolves the provider draft id backing a draft message (some
	// providers track drafts by an id distinct from the message id). Returns an
	// empty string when the message isn't a draft.
	FindDraftID(ctx context.Context, msgID string) (string, error)
}
