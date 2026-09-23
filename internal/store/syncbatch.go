package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jsnjack/mailbox/internal/model"
)

// SyncBatch records a fixed provider change range and its unfinished work.
type SyncBatch struct {
	Cursor  string
	Next    string
	Upserts []string
	Deletes []string
}

// LoadSyncBatch returns a saved change range, or nil when none is pending.
func (s *Store) LoadSyncBatch(ctx context.Context, accountID int64) (*SyncBatch, error) {
	var payload string
	if err := s.reader.QueryRowContext(ctx, `SELECT payload FROM sync_batches WHERE account_id=?`, accountID).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load sync batch: %w", err)
	}
	var batch SyncBatch
	if err := json.Unmarshal([]byte(payload), &batch); err != nil {
		return nil, fmt.Errorf("decode sync batch: %w", err)
	}
	return &batch, nil
}

// SaveSyncBatch commits downloaded metadata and the remaining work atomically.
func (s *Store) SaveSyncBatch(ctx context.Context, accountID int64, batch SyncBatch, msgs []model.Message) error {
	payload, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("encode sync batch: %w", err)
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var cursor string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sync_cursor,'') FROM accounts WHERE id=?`, accountID).Scan(&cursor); err != nil {
			return fmt.Errorf("read sync batch cursor: %w", err)
		}
		if cursor != batch.Cursor {
			return fmt.Errorf("sync batch cursor changed for account %d", accountID)
		}
		for _, m := range msgs {
			if m.AccountID != accountID {
				return fmt.Errorf("sync batch message belongs to account %d, expected %d", m.AccountID, accountID)
			}
			if _, err := upsertMessageTx(ctx, tx, m); err != nil {
				return fmt.Errorf("save sync batch message: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sync_batches(account_id,payload) VALUES (?,?)
			ON CONFLICT(account_id) DO UPDATE SET payload=excluded.payload`, accountID, string(payload)); err != nil {
			return fmt.Errorf("save sync batch: %w", err)
		}
		return nil
	})
}
