package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jsnjack/mailbox/internal/model"
)

const (
	LocalDraftPrefix       = "localdraft:"
	LocalDraftQueued       = "queued"
	LocalDraftSynced       = "synced"
	LocalDraftFailed       = "failed"
	LocalDraftDeleting     = "deleting"
	localDraftSnippetRunes = 180
)

// ErrDraftAccountMismatch means a draft id was used with an account other than
// the one that owns it.
var ErrDraftAccountMismatch = errors.New("draft belongs to another account")

// IsLocalDraftID reports whether id names the synthetic message backing an
// offline-editable local draft.
func IsLocalDraftID(id string) bool { return strings.HasPrefix(id, LocalDraftPrefix) }

// SaveLocalDraft persists the compose payload before any provider I/O and
// upserts a lightweight synthetic DRAFT message so the normal thread list can
// display and reopen it offline. It returns the stable local id.
func (s *Store) SaveLocalDraft(ctx context.Context, accountID int64, msg model.OutgoingMessage) (string, error) {
	localID := msg.LocalDraftID
	if localID == "" {
		uuid, err := randomUUID()
		if err != nil {
			return "", err
		}
		localID = LocalDraftPrefix + uuid
	}
	if !IsLocalDraftID(localID) {
		return "", fmt.Errorf("invalid local draft id %q", localID)
	}
	msg.LocalDraftID = localID
	payload, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("encode local draft: %w", err)
	}
	now := time.Now()
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO local_drafts (
				local_id, account_id, source_message_id, provider_draft_id,
				payload, state, revision, attempts, last_error, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, 0, '', ?)
			ON CONFLICT(local_id) DO UPDATE SET
				source_message_id=CASE WHEN excluded.source_message_id <> '' THEN excluded.source_message_id ELSE local_drafts.source_message_id END,
				provider_draft_id=CASE WHEN excluded.provider_draft_id <> '' THEN excluded.provider_draft_id ELSE local_drafts.provider_draft_id END,
				payload=excluded.payload, state=?, revision=local_drafts.revision+1,
				attempts=0, last_error='', updated_at=excluded.updated_at
			WHERE local_drafts.account_id = excluded.account_id`,
			localID, accountID, msg.SourceMessageID, msg.DraftID, payload,
			LocalDraftQueued, now.Unix(), LocalDraftQueued)
		if err != nil {
			return fmt.Errorf("save local draft: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("save local draft: affected rows: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("save local draft %q: %w", localID, ErrDraftAccountMismatch)
		}

		synthetic := model.Message{
			AccountID:      accountID,
			GmailID:        localID,
			ThreadID:       localID,
			InternalDate:   now,
			FromAddr:       msg.From,
			ToAddrs:        msg.To,
			CcAddrs:        msg.Cc,
			BccAddrs:       msg.Bcc,
			Subject:        msg.Subject,
			Snippet:        localDraftSnippet(msg.Body),
			InReplyTo:      msg.InReplyTo,
			References:     msg.References,
			HasAttachments: len(msg.Attachments) > 0,
			Labels:         []string{model.LabelDraft},
		}
		rowID, err := upsertMessageTx(ctx, tx, synthetic)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO message_bodies (message_rowid, body_text, body_html, raw_headers)
			VALUES (?, ?, ?, '')
			ON CONFLICT(message_rowid) DO UPDATE SET
				body_text=excluded.body_text, body_html=excluded.body_html`,
			rowID, msg.Body, msg.HTMLBody); err != nil {
			return fmt.Errorf("save local draft body: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET body_fetched = 2 WHERE rowid = ?`, rowID); err != nil {
			return fmt.Errorf("mark local draft body fetched: %w", err)
		}
		return reindexFTS(ctx, tx, rowID)
	})
	if err != nil {
		return "", err
	}
	return localID, nil
}

func localDraftSnippet(body string) string {
	text := strings.Join(strings.Fields(body), " ")
	if utf8.RuneCountInString(text) <= localDraftSnippetRunes {
		return text
	}
	r := []rune(text)
	return string(r[:localDraftSnippetRunes]) + "…"
}

// LocalDraft returns one durable compose snapshot.
func (s *Store) LocalDraft(ctx context.Context, localID string) (model.LocalDraft, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT local_id, account_id, source_message_id, provider_draft_id,
			provider_message_id, payload, state, revision, attempts, last_error, updated_at
		FROM local_drafts WHERE local_id = ?`, localID)
	d, err := scanLocalDraft(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.LocalDraft{}, ErrNotFound
	}
	if err != nil {
		return model.LocalDraft{}, fmt.Errorf("get local draft: %w", err)
	}
	return d, nil
}

// PendingLocalDrafts returns edits/deletions waiting to reach the provider,
// oldest first. Synced drafts remain local for reliable offline reopen.
func (s *Store) PendingLocalDrafts(ctx context.Context, accountID int64, limit int) ([]model.LocalDraft, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT local_id, account_id, source_message_id, provider_draft_id,
			provider_message_id, payload, state, revision, attempts, last_error, updated_at
		FROM local_drafts
		WHERE account_id = ? AND state IN (?, ?, ?)
		ORDER BY updated_at, local_id LIMIT ?`,
		accountID, LocalDraftQueued, LocalDraftFailed, LocalDraftDeleting, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending local drafts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.LocalDraft
	for rows.Next() {
		d, err := scanLocalDraft(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pending local draft: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type localDraftScanner interface{ Scan(...any) error }

func scanLocalDraft(sc localDraftScanner) (model.LocalDraft, error) {
	var d model.LocalDraft
	var payload []byte
	var updated int64
	if err := sc.Scan(&d.LocalID, &d.AccountID, &d.SourceMessageID,
		&d.ProviderDraftID, &d.ProviderMessageID, &payload, &d.State,
		&d.Revision, &d.Attempts, &d.LastError, &updated); err != nil {
		return model.LocalDraft{}, err
	}
	if err := json.Unmarshal(payload, &d.Message); err != nil {
		return model.LocalDraft{}, fmt.Errorf("decode payload for %s: %w", d.LocalID, err)
	}
	d.Message.LocalDraftID = d.LocalID
	d.Message.SourceMessageID = d.SourceMessageID
	if d.ProviderDraftID != "" {
		d.Message.DraftID = d.ProviderDraftID
	}
	d.UpdatedAt = time.Unix(updated, 0)
	return d, nil
}

// RecordLocalDraftSynced records provider ids even when revision has advanced
// during the network call. Only an unchanged revision becomes synced; a newer
// edit stays queued and will update the just-recorded provider draft next.
func (s *Store) RecordLocalDraftSynced(ctx context.Context, localID string, revision int64, ref model.DraftRef) error {
	res, err := s.writer.ExecContext(ctx, `
		UPDATE local_drafts SET
			provider_draft_id=CASE WHEN ? <> '' THEN ? ELSE provider_draft_id END,
			provider_message_id=CASE WHEN ? <> '' THEN ? ELSE provider_message_id END,
			state=CASE WHEN state = ? THEN ? WHEN revision = ? THEN ? ELSE ? END,
			attempts=CASE WHEN revision = ? THEN 0 ELSE attempts END,
			last_error=CASE WHEN revision = ? THEN '' ELSE last_error END
		WHERE local_id = ?`,
		ref.DraftID, ref.DraftID, ref.MessageID, ref.MessageID,
		LocalDraftDeleting, LocalDraftDeleting, revision, LocalDraftSynced, LocalDraftQueued,
		revision, revision, localID)
	if err != nil {
		return fmt.Errorf("record local draft sync: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// FailLocalDraft records a retryable provider error only if this is still the
// revision that failed. A newer autosave has already reset it to queued.
func (s *Store) FailLocalDraft(ctx context.Context, localID string, revision int64, cause error) error {
	if cause == nil {
		return nil
	}
	_, err := s.writer.ExecContext(ctx, `
		UPDATE local_drafts SET state=?, attempts=attempts+1, last_error=?
		WHERE local_id=? AND revision=? AND state <> ?`,
		LocalDraftFailed, cause.Error(), localID, revision, LocalDraftDeleting)
	if err != nil {
		return fmt.Errorf("fail local draft: %w", err)
	}
	return nil
}

// MarkLocalDraftDeleting hides a local draft and its cached provider copies
// immediately while retaining a tombstone until DeleteDraft succeeds online.
func (s *Store) MarkLocalDraftDeleting(ctx context.Context, localID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var accountID int64
		var sourceID, providerMessageID string
		if err := tx.QueryRowContext(ctx, `
			SELECT account_id, source_message_id, provider_message_id
			FROM local_drafts WHERE local_id = ?`, localID).Scan(&accountID, &sourceID, &providerMessageID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE local_drafts SET state=?, revision=revision+1, updated_at=unixepoch() WHERE local_id=?`, LocalDraftDeleting, localID); err != nil {
			return err
		}
		for _, id := range []string{localID, sourceID, providerMessageID} {
			if id != "" {
				if err := deleteMessageTx(ctx, tx, accountID, id); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// QueueProviderDraftDelete creates a deletion tombstone for a provider draft
// that has not previously been edited locally and removes its cached message.
func (s *Store) QueueProviderDraftDelete(ctx context.Context, accountID int64, sourceMessageID, draftID, threadID string) (string, error) {
	uuid, err := randomUUID()
	if err != nil {
		return "", err
	}
	localID := LocalDraftPrefix + uuid
	msg := model.OutgoingMessage{LocalDraftID: localID, SourceMessageID: sourceMessageID, DraftID: draftID, ThreadID: threadID}
	payload, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO local_drafts (local_id, account_id, source_message_id, provider_draft_id, payload, state)
			VALUES (?, ?, ?, ?, ?, ?)`, localID, accountID, sourceMessageID, draftID, payload, LocalDraftDeleting); err != nil {
			return fmt.Errorf("queue provider draft delete: %w", err)
		}
		return deleteMessageTx(ctx, tx, accountID, sourceMessageID)
	})
	return localID, err
}

// CompleteLocalDraft removes the local record and every cached representation
// of it. It is used after provider deletion or successful send.
func (s *Store) CompleteLocalDraft(ctx context.Context, localID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var accountID int64
		var sourceID, providerMessageID string
		err := tx.QueryRowContext(ctx, `
			SELECT account_id, source_message_id, provider_message_id
			FROM local_drafts WHERE local_id = ?`, localID).Scan(&accountID, &sourceID, &providerMessageID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, id := range []string{localID, sourceID, providerMessageID} {
			if id != "" {
				if err := deleteMessageTx(ctx, tx, accountID, id); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM local_drafts WHERE local_id = ?`, localID); err != nil {
			return fmt.Errorf("complete local draft: %w", err)
		}
		return nil
	})
}
