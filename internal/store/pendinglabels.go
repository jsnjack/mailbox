package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/jsnjack/mailbox/internal/logging"
)

// PendingLabelOp is one optimistic local label change waiting to reach the
// provider. Rows are applied in ID order so an action and its Undo cannot race.
type PendingLabelOp struct {
	ID         int64
	AccountID  int64
	MessageIDs []string
	Add        []string
	Remove     []string
	Attempts   int
	LastError  string
}

// ModifyLabelsBatchAndEnqueue atomically updates the local cache and records the
// provider mirror. If either half fails neither is committed, preserving the
// store's role as the source of truth for pending offline work.
func (s *Store) ModifyLabelsBatchAndEnqueue(ctx context.Context, accountID int64, gmailIDs, add, remove []string) error {
	if len(gmailIDs) == 0 {
		return nil
	}
	idsJSON, err := json.Marshal(gmailIDs)
	if err != nil {
		return fmt.Errorf("encode pending label ids: %w", err)
	}
	addJSON, err := json.Marshal(add)
	if err != nil {
		return fmt.Errorf("encode pending add labels: %w", err)
	}
	removeJSON, err := json.Marshal(remove)
	if err != nil {
		return fmt.Errorf("encode pending remove labels: %w", err)
	}
	logging.TraceContext(ctx, "store: modify labels and enqueue", "account", accountID, "n", len(gmailIDs), "add", add, "remove", remove)
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, id := range gmailIDs {
			if err := modifyLabelsTx(ctx, tx, accountID, id, add, remove); err != nil {
				if err == ErrNotFound {
					continue
				}
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pending_label_ops (account_id, message_ids, add_labels, remove_labels)
			VALUES (?, ?, ?, ?)`, accountID, string(idsJSON), string(addJSON), string(removeJSON)); err != nil {
			return fmt.Errorf("enqueue pending label operation: %w", err)
		}
		return nil
	})
}

// PendingLabelOps returns the oldest provider mirrors for accountID.
func (s *Store) PendingLabelOps(ctx context.Context, accountID int64, limit int) ([]PendingLabelOp, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, account_id, message_ids, add_labels, remove_labels, attempts, last_error
		FROM pending_label_ops WHERE account_id = ? ORDER BY id LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending label operations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PendingLabelOp
	for rows.Next() {
		var op PendingLabelOp
		var idsJSON, addJSON, removeJSON string
		if err := rows.Scan(&op.ID, &op.AccountID, &idsJSON, &addJSON, &removeJSON, &op.Attempts, &op.LastError); err != nil {
			return nil, fmt.Errorf("scan pending label operation: %w", err)
		}
		if err := json.Unmarshal([]byte(idsJSON), &op.MessageIDs); err != nil {
			return nil, fmt.Errorf("decode pending label ids %d: %w", op.ID, err)
		}
		if err := json.Unmarshal([]byte(addJSON), &op.Add); err != nil {
			return nil, fmt.Errorf("decode pending add labels %d: %w", op.ID, err)
		}
		if err := json.Unmarshal([]byte(removeJSON), &op.Remove); err != nil {
			return nil, fmt.Errorf("decode pending remove labels %d: %w", op.ID, err)
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending label operations: %w", err)
	}
	return out, nil
}

// CompletePendingLabelOp removes an operation after the provider accepted it.
func (s *Store) CompletePendingLabelOp(ctx context.Context, id int64) error {
	if _, err := s.writer.ExecContext(ctx, `DELETE FROM pending_label_ops WHERE id = ?`, id); err != nil {
		return fmt.Errorf("complete pending label operation: %w", err)
	}
	return nil
}

// FailPendingLabelOp records diagnostics while leaving the row retryable.
func (s *Store) FailPendingLabelOp(ctx context.Context, id int64, cause error) error {
	if cause == nil {
		return nil
	}
	if _, err := s.writer.ExecContext(ctx, `
		UPDATE pending_label_ops SET attempts = attempts + 1, last_error = ? WHERE id = ?`, cause.Error(), id); err != nil {
		return fmt.Errorf("fail pending label operation: %w", err)
	}
	return nil
}
