package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// MessageLabels is a message's complete label set as the provider reported it.
type MessageLabels struct {
	GmailID string
	Labels  []string
}

// LabelSetResult reports what ApplyLabelSets did. Ids that are not cached
// appear in none of its lists: the caller has to fetch those.
type LabelSetResult struct {
	// Changed lists the cached messages whose labels changed (GmailID, ThreadID
	// and the new Labels set).
	Changed []model.Message
	// Unchanged lists cached messages that already carried exactly these labels.
	Unchanged []string
	// Pending lists messages left alone because an optimistic local label change
	// is still waiting to reach the provider; the provider's older state would
	// otherwise undo what the user just did.
	Pending []string
}

// ApplyLabelSets replaces the labels of every cached message in sets, in one
// transaction, and recomputes their unread/starred flags. A provider message
// never changes after delivery, so for a cached message its labels are the
// whole of an update — this is what lets a sync skip fetching it again.
func (s *Store) ApplyLabelSets(ctx context.Context, accountID int64, sets []MessageLabels) (LabelSetResult, error) {
	var res LabelSetResult
	if len(sets) == 0 {
		return res, nil
	}
	start := time.Now()
	logging.TraceContext(ctx, "store: apply label sets", "account", accountID, "n", len(sets))
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res = LabelSetResult{}
		// Read inside the transaction: the writer connection serializes it with
		// ModifyLabelsBatchAndEnqueue, so an action taken while this runs is seen.
		pending, err := pendingLabelIDsTx(ctx, tx, accountID)
		if err != nil {
			return err
		}
		for _, set := range sets {
			if pending[set.GmailID] {
				res.Pending = append(res.Pending, set.GmailID)
				continue
			}
			rowID, threadID, err := lookupMessageTx(ctx, tx, accountID, set.GmailID)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			changed, err := setLabelsTx(ctx, tx, accountID, rowID, set.Labels)
			if err != nil {
				return fmt.Errorf("set labels %q: %w", set.GmailID, err)
			}
			if !changed {
				res.Unchanged = append(res.Unchanged, set.GmailID)
				continue
			}
			res.Changed = append(res.Changed, model.Message{AccountID: accountID, GmailID: set.GmailID, ThreadID: threadID, Labels: set.Labels})
		}
		return nil
	})
	if err != nil {
		logging.TraceContext(ctx, "store: apply label sets", "account", accountID, "n", len(sets), "err", err)
		return LabelSetResult{}, err
	}
	logging.TraceContext(ctx, "store: apply label sets done", "account", accountID, "n", len(sets),
		"changed", len(res.Changed), "unchanged", len(res.Unchanged), "pending", len(res.Pending),
		"uncached", len(sets)-len(res.Changed)-len(res.Unchanged)-len(res.Pending), "dur", time.Since(start))
	return res, nil
}

// InboxState is the provider's inbox at one moment (see backend.InboxSnapshot).
type InboxState struct {
	IDs      []string // newest first
	Unread   []string
	Complete bool // IDs and Unread are the whole inbox, not a capped prefix
}

// InboxReconcile reports what ReconcileInbox changed.
type InboxReconcile struct {
	// Changed lists cached messages whose inbox membership or read state moved.
	Changed []model.Message
	// Missing lists inbox ids that are not cached, newest first: the caller
	// fetches them.
	Missing []string
}

// ReconcileInbox makes the cached inbox match the provider's: messages in it
// gain INBOX, unread ones UNREAD, and — when the listing is complete — cached
// inbox messages missing from it lose INBOX, and read ones lose UNREAD. Messages
// with an optimistic local change still waiting to reach the provider are left
// alone, as in ApplyLabelSets. One transaction.
func (s *Store) ReconcileInbox(ctx context.Context, accountID int64, st InboxState) (InboxReconcile, error) {
	start := time.Now()
	logging.TraceContext(ctx, "store: reconcile inbox", "account", accountID, "inbox", len(st.IDs), "unread", len(st.Unread), "complete", st.Complete)
	var res InboxReconcile
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res = InboxReconcile{}
		pending, err := pendingLabelIDsTx(ctx, tx, accountID)
		if err != nil {
			return err
		}
		local, err := cachedInboxTx(ctx, tx, accountID)
		if err != nil {
			return err
		}
		unread := make(map[string]bool, len(st.Unread))
		for _, id := range st.Unread {
			unread[id] = true
		}
		inServer := make(map[string]bool, len(st.IDs))
		apply := func(id string, add, remove []string) (bool, error) {
			rowID, threadID, err := lookupMessageTx(ctx, tx, accountID, id)
			if err != nil {
				return false, err
			}
			if err := modifyLabelsRowTx(ctx, tx, accountID, rowID, add, remove); err != nil {
				return false, err
			}
			res.Changed = append(res.Changed, model.Message{AccountID: accountID, GmailID: id, ThreadID: threadID})
			return true, nil
		}
		for _, id := range st.IDs {
			inServer[id] = true
			if pending[id] {
				continue
			}
			var add, remove []string
			wasUnread, inLocal := local[id]
			if !inLocal {
				add = append(add, model.LabelInbox)
			}
			switch {
			case unread[id] && (!inLocal || !wasUnread):
				add = append(add, model.LabelUnread)
			case !unread[id] && st.Complete && (!inLocal || wasUnread):
				remove = append(remove, model.LabelUnread)
			}
			if len(add) == 0 && len(remove) == 0 {
				continue
			}
			if _, err := apply(id, add, remove); err != nil {
				if errors.Is(err, ErrNotFound) {
					res.Missing = append(res.Missing, id)
					continue
				}
				return err
			}
		}
		if st.Complete {
			for id := range local {
				if inServer[id] || pending[id] {
					continue
				}
				if _, err := apply(id, nil, []string{model.LabelInbox}); err != nil && !errors.Is(err, ErrNotFound) {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		logging.TraceContext(ctx, "store: reconcile inbox", "account", accountID, "err", err)
		return InboxReconcile{}, err
	}
	logging.TraceContext(ctx, "store: reconcile inbox done", "account", accountID, "changed", len(res.Changed), "missing", len(res.Missing), "dur", time.Since(start))
	return res, nil
}

// cachedInboxTx maps every cached inbox message's id to its unread flag.
func cachedInboxTx(ctx context.Context, tx *sql.Tx, accountID int64) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT m.gmail_id, m.is_unread FROM messages m
		JOIN message_labels ml ON ml.message_rowid = m.rowid AND ml.label_id = ?
		WHERE m.account_id = ?`, model.LabelInbox, accountID)
	if err != nil {
		return nil, fmt.Errorf("list cached inbox: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]bool)
	for rows.Next() {
		var id string
		var unread bool
		if err := rows.Scan(&id, &unread); err != nil {
			return nil, fmt.Errorf("scan cached inbox: %w", err)
		}
		out[id] = unread
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list cached inbox: %w", err)
	}
	return out, nil
}

// pendingLabelIDsTx returns the ids of messages with a queued provider mirror.
func pendingLabelIDsTx(ctx context.Context, tx *sql.Tx, accountID int64) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT message_ids FROM pending_label_ops WHERE account_id = ?`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list pending label ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]bool)
	for rows.Next() {
		var idsJSON string
		if err := rows.Scan(&idsJSON); err != nil {
			return nil, fmt.Errorf("scan pending label ids: %w", err)
		}
		var ids []string
		if err := json.Unmarshal([]byte(idsJSON), &ids); err != nil {
			return nil, fmt.Errorf("decode pending label ids: %w", err)
		}
		for _, id := range ids {
			out[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending label ids: %w", err)
	}
	return out, nil
}

// lookupMessageTx returns a cached message's rowid and thread, or ErrNotFound.
func lookupMessageTx(ctx context.Context, tx *sql.Tx, accountID int64, gmailID string) (int64, string, error) {
	var rowID int64
	var threadID string
	err := tx.QueryRowContext(ctx,
		`SELECT rowid, thread_id FROM messages WHERE account_id = ? AND gmail_id = ?`,
		accountID, gmailID).Scan(&rowID, &threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	if err != nil {
		return 0, "", fmt.Errorf("find message: %w", err)
	}
	return rowID, threadID, nil
}

// setLabelsTx replaces one message's label set and recomputes its flags. It
// reports whether anything changed, writing nothing when the set is the same.
func setLabelsTx(ctx context.Context, tx *sql.Tx, accountID, rowID int64, labels []string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT label_id FROM message_labels WHERE message_rowid = ?`, rowID)
	if err != nil {
		return false, fmt.Errorf("read labels: %w", err)
	}
	var current []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("scan label: %w", err)
		}
		current = append(current, l)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("read labels: %w", err)
	}
	want := slices.Clone(labels)
	slices.Sort(want)
	want = slices.Compact(want)
	slices.Sort(current)
	if slices.Equal(current, want) {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_labels WHERE message_rowid = ?`, rowID); err != nil {
		return false, fmt.Errorf("clear labels: %w", err)
	}
	for _, l := range want {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO message_labels (message_rowid, account_id, label_id) VALUES (?,?,?)`,
			rowID, accountID, l); err != nil {
			return false, fmt.Errorf("insert label %q: %w", l, err)
		}
	}
	if err := recomputeFlagsTx(ctx, tx, rowID); err != nil {
		return false, err
	}
	return true, nil
}
