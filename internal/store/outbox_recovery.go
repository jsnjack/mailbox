package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

// RecoverOutboxDraft atomically cancels an unclaimed send and saves its editable
// snapshot. A failed save rolls back cancellation; a claimed send stays untouched.
func (s *Store) RecoverOutboxDraft(ctx context.Context, item model.OutboxItem, msg model.OutgoingMessage) (string, bool, error) {
	localID := item.LocalDraftID
	if localID == "" {
		localID = LocalDraftPrefix + item.LocalUUID
	}
	if !IsLocalDraftID(localID) {
		return "", false, fmt.Errorf("recover outbox: invalid draft id")
	}
	msg.LocalDraftID = localID
	msg.DraftID = item.DraftID
	msg.ThreadID = item.ThreadID
	payload, err := json.Marshal(msg)
	if err != nil {
		return "", false, fmt.Errorf("encode recovered draft: %w", err)
	}
	cancelled := false
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE id=? AND account_id=? AND local_uuid=? AND state IN ('queued','failed','uncertain')`, item.ID, item.AccountID, item.LocalUUID)
		if err != nil {
			return fmt.Errorf("cancel outbox for editing: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("check outbox cancellation: %w", err)
		}
		if n == 0 {
			return nil
		}
		if err := saveLocalDraftTx(ctx, tx, item.AccountID, msg, localID, payload, time.Now()); err != nil {
			return err
		}
		cancelled = true
		return nil
	})
	if err != nil {
		return "", false, fmt.Errorf("recover outbox draft: %w", err)
	}
	return localID, cancelled, nil
}

// RestoreOutbox restores an explicitly discarded snapshot with its original
// delivery state, including uncertain delivery. Repeated Undo cannot duplicate it.
func (s *Store) RestoreOutbox(ctx context.Context, item model.OutboxItem) error {
	switch item.State {
	case "queued", "failed", "uncertain":
	default:
		return fmt.Errorf("cannot restore outbox state %q", item.State)
	}
	_, err := s.writer.ExecContext(ctx, `INSERT INTO outbox (local_uuid,account_id,thread_id,draft_id,local_draft_id,rfc822,state,attempts,last_error,not_before)
 VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(local_uuid) DO NOTHING`, item.LocalUUID, item.AccountID, item.ThreadID, item.DraftID, item.LocalDraftID, item.RFC822, item.State, item.Attempts, item.LastError, item.NotBefore)
	if err != nil {
		return fmt.Errorf("restore discarded outbox: %w", err)
	}
	return nil
}
