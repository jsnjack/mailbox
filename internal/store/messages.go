package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// msgCols is the messages column list, aliased to m, for SELECTs that may join
// message_labels (where account_id would otherwise be ambiguous).
const msgCols = `m.rowid, m.account_id, m.gmail_id, m.thread_id, m.internal_date, ` +
	`m.from_name, m.from_addr, m.reply_to, m.to_addrs, m.cc_addrs, m.subject, m.snippet, ` +
	`m.rfc822_msgid, m.in_reply_to, m.references_hdr, m.is_unread, m.is_starred, ` +
	`m.has_attachments, m.size_estimate, m.body_fetched, m.list_unsubscribe, m.list_unsub_post`

// UpsertMessage inserts or updates a message's metadata, replaces its label set,
// and refreshes its full-text index entry. It returns the message's local rowid.
func (s *Store) UpsertMessage(ctx context.Context, m model.Message) (int64, error) {
	start := time.Now()
	logging.TraceContext(ctx, "store: upsert message", "account", m.AccountID, "id", m.GmailID, "thread", m.ThreadID, "subject", m.Subject)
	var rowid int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		rowid, err = upsertMessageTx(ctx, tx, m)
		return err
	})
	if err != nil {
		logging.TraceContext(ctx, "store: upsert message", "account", m.AccountID, "id", m.GmailID, "err", err)
		return rowid, err
	}
	logging.TraceContext(ctx, "store: upsert message done", "account", m.AccountID, "id", m.GmailID, "rowid", rowid, "dur", time.Since(start))
	return rowid, nil
}

// UpsertMessages upserts many messages in a single transaction. Backfill and
// incremental sync use this so a run that touches N messages costs one
// commit/fsync instead of N — the dominant cost when catching up a mailbox.
func (s *Store) UpsertMessages(ctx context.Context, msgs []model.Message) error {
	if len(msgs) == 0 {
		logging.TraceContext(ctx, "store: upsert messages", "n", 0)
		return nil
	}
	start := time.Now()
	logging.TraceContext(ctx, "store: upsert messages", "n", len(msgs))
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		for _, m := range msgs {
			if _, err := upsertMessageTx(ctx, tx, m); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		logging.TraceContext(ctx, "store: upsert messages", "n", len(msgs), "err", err)
		return err
	}
	logging.TraceContext(ctx, "store: upsert messages done", "n", len(msgs), "dur", time.Since(start))
	return nil
}

// upsertMessageTx upserts one message's metadata, replaces its label set, and
// refreshes its FTS entry within tx, returning the message's rowid. It is the
// shared body of UpsertMessage and UpsertMessages.
func upsertMessageTx(ctx context.Context, tx *sql.Tx, m model.Message) (int64, error) {
	var idate any
	if !m.InternalDate.IsZero() {
		idate = m.InternalDate.Unix()
	}
	// Read the current FTS-relevant columns (if the row exists) before the upsert
	// so we can skip re-tokenizing when only labels/flags changed — the common case
	// for a mark-read/archive/star synced from another device. A label-only
	// re-upsert would otherwise re-index the full body every time.
	var (
		oldSubject, oldFromName, oldFromAddr, oldSnippet string
		existed                                          bool
	)
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(subject,''), COALESCE(from_name,''), COALESCE(from_addr,''), COALESCE(snippet,'')
		FROM messages WHERE account_id = ? AND gmail_id = ?`,
		m.AccountID, m.GmailID).Scan(&oldSubject, &oldFromName, &oldFromAddr, &oldSnippet)
	switch {
	case err == nil:
		existed = true
	case errors.Is(err, sql.ErrNoRows):
		existed = false
	default:
		return 0, fmt.Errorf("read prior fts columns %q: %w", m.GmailID, err)
	}

	var rowid int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO messages (
			account_id, gmail_id, thread_id, internal_date, from_name, from_addr,
			reply_to, to_addrs, cc_addrs, subject, snippet, rfc822_msgid, in_reply_to,
			references_hdr, is_unread, is_starred, has_attachments, size_estimate,
			list_unsubscribe, list_unsub_post)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id, gmail_id) DO UPDATE SET
			thread_id=excluded.thread_id, internal_date=excluded.internal_date,
			from_name=excluded.from_name, from_addr=excluded.from_addr,
			reply_to=excluded.reply_to, to_addrs=excluded.to_addrs, cc_addrs=excluded.cc_addrs,
			subject=excluded.subject, snippet=excluded.snippet,
			rfc822_msgid=excluded.rfc822_msgid, in_reply_to=excluded.in_reply_to,
			references_hdr=excluded.references_hdr, is_unread=excluded.is_unread,
			is_starred=excluded.is_starred, has_attachments=excluded.has_attachments,
			size_estimate=excluded.size_estimate,
			list_unsubscribe=excluded.list_unsubscribe, list_unsub_post=excluded.list_unsub_post
		RETURNING rowid`,
		m.AccountID, m.GmailID, m.ThreadID, idate, m.FromName, m.FromAddr,
		m.ReplyTo, m.ToAddrs, m.CcAddrs, m.Subject, m.Snippet, m.RFC822MsgID, m.InReplyTo,
		m.References, b2i(m.IsUnread), b2i(m.IsStarred), b2i(m.HasAttachments), m.SizeEstimate,
		m.ListUnsubscribe, b2i(m.ListUnsubOneClick),
	).Scan(&rowid)
	if err != nil {
		return 0, fmt.Errorf("upsert message %q: %w", m.GmailID, err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM message_labels WHERE message_rowid = ?`, rowid); err != nil {
		return 0, fmt.Errorf("clear labels: %w", err)
	}
	for _, lbl := range m.Labels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO message_labels (message_rowid, account_id, label_id) VALUES (?,?,?)`,
			rowid, m.AccountID, lbl); err != nil {
			return 0, fmt.Errorf("insert label %q: %w", lbl, err)
		}
	}
	// Only re-index when a searchable column actually changed (or the message is
	// new). The body isn't touched here, so if subject/from/snippet are unchanged
	// the existing FTS row is already correct — skipping the DELETE+INSERT avoids
	// re-tokenizing the full body on a label-only re-upsert. UpsertBody still
	// reindexes unconditionally when body text arrives.
	ftsChanged := !existed ||
		oldSubject != m.Subject || oldFromName != m.FromName ||
		oldFromAddr != m.FromAddr || oldSnippet != m.Snippet
	if ftsChanged {
		if err := reindexFTS(ctx, tx, rowid); err != nil {
			return 0, err
		}
	}
	return rowid, nil
}

// UpsertBody stores a message's body parts, marks the message body-fetched, and
// refreshes the full-text index so the body becomes searchable.
func (s *Store) UpsertBody(ctx context.Context, b model.MessageBody) error {
	start := time.Now()
	logging.TraceContext(ctx, "store: upsert body", "rowid", b.MessageRowID, "text_bytes", len(b.Text), "html_bytes", len(b.HTML))
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO message_bodies (message_rowid, body_text, body_html, raw_headers)
			VALUES (?,?,?,?)
			ON CONFLICT(message_rowid) DO UPDATE SET
				body_text=excluded.body_text, body_html=excluded.body_html,
				raw_headers=excluded.raw_headers`,
			b.MessageRowID, b.Text, b.HTML, b.RawHeaders); err != nil {
			return fmt.Errorf("upsert body: %w", err)
		}
		// body_fetched is a fetch-version marker, not just a bool: 0 = never
		// fetched, 1 = fetched by a build before externalized-HTML support, 2 =
		// fetched with it. Stamping 2 here lets the one-time HTML backfill
		// (MessagesMissingHTML) find and re-fetch the version-1 text-only bodies
		// once, then never again. Everything else still reads it as "fetched" (!= 0).
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET body_fetched = 2 WHERE rowid = ?`, b.MessageRowID); err != nil {
			return fmt.Errorf("mark body fetched: %w", err)
		}
		return reindexFTS(ctx, tx, b.MessageRowID)
	})
	if err != nil {
		logging.TraceContext(ctx, "store: upsert body", "rowid", b.MessageRowID, "err", err)
		return err
	}
	logging.TraceContext(ctx, "store: upsert body done", "rowid", b.MessageRowID, "dur", time.Since(start))
	return nil
}

// DeleteMessage removes a message and its dependent rows (labels, body,
// attachments cascade) plus its FTS entry. It is a no-op if the message is absent.
func (s *Store) DeleteMessage(ctx context.Context, accountID int64, gmailID string) error {
	logging.TraceContext(ctx, "store: delete message", "account", accountID, "id", gmailID)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		return deleteMessageTx(ctx, tx, accountID, gmailID)
	})
	if err != nil {
		logging.TraceContext(ctx, "store: delete message", "account", accountID, "id", gmailID, "err", err)
	}
	return err
}

// deleteChunkSize bounds how many messages one delete transaction covers, so a
// bulk delete yields the single writer connection to concurrent writes between
// chunks.
const deleteChunkSize = 500

// DeleteMessages removes many messages (and their FTS rows), batched into
// chunked transactions; missing ids are skipped. Chunked, not one transaction:
// a bulk delete ("Empty Trash" can be tens of thousands) in a single
// transaction would hold the sole writer connection — and with it every
// concurrent sync/outbox write — for its whole duration. Deletes are
// idempotent and re-derived from the server, so all-or-nothing atomicity buys
// nothing here.
func (s *Store) DeleteMessages(ctx context.Context, accountID int64, gmailIDs []string) error {
	if len(gmailIDs) == 0 {
		logging.TraceContext(ctx, "store: delete messages", "account", accountID, "n", 0)
		return nil
	}
	start := time.Now()
	logging.TraceContext(ctx, "store: delete messages", "account", accountID, "n", len(gmailIDs))
	for cs := 0; cs < len(gmailIDs); cs += deleteChunkSize {
		ce := cs + deleteChunkSize
		if ce > len(gmailIDs) {
			ce = len(gmailIDs)
		}
		chunk := gmailIDs[cs:ce]
		err := s.withTx(ctx, func(tx *sql.Tx) error {
			for _, id := range chunk {
				if err := deleteMessageTx(ctx, tx, accountID, id); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			logging.TraceContext(ctx, "store: delete messages", "account", accountID, "n", len(gmailIDs), "deleted", cs, "err", err)
			return err
		}
	}
	logging.TraceContext(ctx, "store: delete messages done", "account", accountID, "n", len(gmailIDs), "dur", time.Since(start))
	return nil
}

// deleteMessageTx deletes one message and its FTS row within tx; absent ids are
// a no-op. Shared by DeleteMessage and DeleteMessages.
func deleteMessageTx(ctx context.Context, tx *sql.Tx, accountID int64, gmailID string) error {
	var rowID int64
	var threadID string
	err := tx.QueryRowContext(ctx,
		`SELECT rowid, thread_id FROM messages WHERE account_id = ? AND gmail_id = ?`,
		accountID, gmailID).Scan(&rowID, &threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find message to delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages_fts WHERE rowid = ?`, rowID); err != nil {
		return fmt.Errorf("delete fts row: %w", err)
	}
	// The AI caches (categories, translations, summaries) are keyed by gmail_id /
	// thread_id with their FK on accounts, so they don't cascade on message
	// deletion — clean them up explicitly so a deleted email leaves no orphans.
	// Deleting any message changes the thread's fingerprint, so dropping its
	// summary is both tidy and correct (it would be stale anyway).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM message_categories WHERE account_id = ? AND gmail_id = ?`, accountID, gmailID); err != nil {
		return fmt.Errorf("delete message category: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM message_translations WHERE account_id = ? AND gmail_id = ?`, accountID, gmailID); err != nil {
		return fmt.Errorf("delete message translations: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM message_analyses WHERE account_id = ? AND gmail_id = ?`, accountID, gmailID); err != nil {
		return fmt.Errorf("delete message analysis: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM thread_summaries WHERE account_id = ? AND thread_id = ?`, accountID, threadID); err != nil {
		return fmt.Errorf("delete thread summary: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE rowid = ?`, rowID); err != nil {
		return fmt.Errorf("delete message: %w", err)
	}
	return nil
}

// ModifyLabels applies a label delta to a message (adding and removing label
// ids) and recomputes the derived unread/starred flags. It is used for optimistic
// local updates before the change is mirrored to Gmail.
func (s *Store) ModifyLabels(ctx context.Context, accountID int64, gmailID string, add, remove []string) error {
	logging.TraceContext(ctx, "store: modify labels", "account", accountID, "id", gmailID, "add", add, "remove", remove)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		return modifyLabelsTx(ctx, tx, accountID, gmailID, add, remove)
	})
	if err != nil {
		logging.TraceContext(ctx, "store: modify labels", "account", accountID, "id", gmailID, "err", err)
	}
	return err
}

// ModifyLabelsBatch applies the same label delta to many messages in a single
// transaction — one commit/fsync instead of one per message, the dominant cost
// when a whole conversation or a bulk selection is archived/trashed/marked-read.
// A missing id is skipped (not an error): a bulk action shouldn't abort because
// one message was deleted meanwhile. Single-message callers use ModifyLabels.
func (s *Store) ModifyLabelsBatch(ctx context.Context, accountID int64, gmailIDs []string, add, remove []string) error {
	if len(gmailIDs) == 0 {
		return nil
	}
	start := time.Now()
	logging.TraceContext(ctx, "store: modify labels batch", "account", accountID, "n", len(gmailIDs), "add", add, "remove", remove)
	skipped := 0
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		for _, id := range gmailIDs {
			if err := modifyLabelsTx(ctx, tx, accountID, id, add, remove); err != nil {
				if errors.Is(err, ErrNotFound) {
					skipped++
					continue
				}
				return err
			}
		}
		return nil
	})
	if err != nil {
		logging.TraceContext(ctx, "store: modify labels batch", "account", accountID, "n", len(gmailIDs), "err", err)
		return err
	}
	logging.TraceContext(ctx, "store: modify labels batch done", "account", accountID, "n", len(gmailIDs), "skipped", skipped, "dur", time.Since(start))
	return nil
}

// modifyLabelsTx applies a label delta to one message within tx and recomputes
// its derived unread/starred flags. It returns ErrNotFound when the message isn't
// cached. Shared by ModifyLabels and ModifyLabelsBatch.
func modifyLabelsTx(ctx context.Context, tx *sql.Tx, accountID int64, gmailID string, add, remove []string) error {
	var rowID int64
	err := tx.QueryRowContext(ctx,
		`SELECT rowid FROM messages WHERE account_id = ? AND gmail_id = ?`,
		accountID, gmailID).Scan(&rowID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("find message: %w", err)
	}
	for _, l := range remove {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM message_labels WHERE message_rowid = ? AND label_id = ?`, rowID, l); err != nil {
			return fmt.Errorf("remove label %q: %w", l, err)
		}
	}
	for _, l := range add {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO message_labels (message_rowid, account_id, label_id) VALUES (?,?,?)`,
			rowID, accountID, l); err != nil {
			return fmt.Errorf("add label %q: %w", l, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE messages SET
			is_unread  = (SELECT COUNT(*) FROM message_labels WHERE message_rowid = ? AND label_id = ?),
			is_starred = (SELECT COUNT(*) FROM message_labels WHERE message_rowid = ? AND label_id = ?)
		WHERE rowid = ?`,
		rowID, model.LabelUnread, rowID, model.LabelStarred, rowID); err != nil {
		return fmt.Errorf("recompute flags: %w", err)
	}
	return nil
}

// UnreadIDsByLabel returns the Gmail ids of unread messages carrying labelID.
func (s *Store) UnreadIDsByLabel(ctx context.Context, accountID int64, labelID string) ([]string, error) {
	logging.TraceContext(ctx, "store: unread ids by label", "account", accountID, "label", labelID)
	rows, err := s.reader.QueryContext(ctx, `
		SELECT m.gmail_id FROM messages m
		JOIN message_labels ml ON ml.message_rowid = m.rowid AND ml.label_id = ?
		WHERE m.account_id = ? AND m.is_unread = 1`, labelID, accountID)
	if err != nil {
		logging.TraceContext(ctx, "store: unread ids by label", "account", accountID, "label", labelID, "err", err)
		return nil, fmt.Errorf("unread ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan unread id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: unread ids by label", "account", accountID, "label", labelID, "count", len(out))
	return out, nil
}

// MarkLabelReadLocal clears the unread flag and removes the UNREAD label from
// every message in a label (optimistic local mirror of a bulk mark-read).
func (s *Store) MarkLabelReadLocal(ctx context.Context, accountID int64, labelID string) error {
	logging.TraceContext(ctx, "store: mark label read local", "account", accountID, "label", labelID)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE messages SET is_unread = 0
			WHERE account_id = ? AND rowid IN (
				SELECT message_rowid FROM message_labels WHERE account_id = ? AND label_id = ?)`,
			accountID, accountID, labelID); err != nil {
			return fmt.Errorf("clear unread flags: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM message_labels WHERE label_id = ? AND message_rowid IN (
				SELECT message_rowid FROM message_labels WHERE account_id = ? AND label_id = ?)`,
			model.LabelUnread, accountID, labelID); err != nil {
			return fmt.Errorf("remove unread labels: %w", err)
		}
		return nil
	})
	if err != nil {
		logging.TraceContext(ctx, "store: mark label read local", "account", accountID, "label", labelID, "err", err)
	}
	return err
}

// UnreadCountByLabelForAccounts returns, per account, the number of unread
// messages carrying labelID — in one query, so the sidebar badges and window
// title don't issue a count per account.
func (s *Store) UnreadCountByLabelForAccounts(ctx context.Context, accountIDs []int64, labelID string) (map[int64]int, error) {
	out := make(map[int64]int, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	logging.TraceContext(ctx, "store: unread count by label for accounts", "label", labelID, "n", len(accountIDs))
	args := make([]any, 0, len(accountIDs)+1)
	args = append(args, labelID)
	for _, id := range accountIDs {
		args = append(args, id)
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT m.account_id, COUNT(*) FROM messages m
		JOIN message_labels ml ON ml.message_rowid = m.rowid AND ml.label_id = ?
		WHERE m.is_unread = 1 AND m.account_id IN (`+placeholders(len(accountIDs))+`)
		GROUP BY m.account_id`, args...)
	if err != nil {
		logging.TraceContext(ctx, "store: unread count by label for accounts", "label", labelID, "err", err)
		return nil, fmt.Errorf("unread counts by accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("scan unread count: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// Count returns the total number of cached messages across all accounts.
func (s *Store) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := s.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		logging.TraceContext(ctx, "store: count messages", "err", err)
		return 0, fmt.Errorf("count messages: %w", err)
	}
	logging.TraceContext(ctx, "store: count messages", "count", n)
	return n, nil
}

// CountByLabel returns the number of messages carrying the given label.
func (s *Store) CountByLabel(ctx context.Context, accountID int64, labelID string) (int, error) {
	var n int
	if err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM message_labels WHERE account_id = ? AND label_id = ?`,
		accountID, labelID).Scan(&n); err != nil {
		logging.TraceContext(ctx, "store: count by label", "account", accountID, "label", labelID, "err", err)
		return 0, fmt.Errorf("count by label: %w", err)
	}
	logging.TraceContext(ctx, "store: count by label", "account", accountID, "label", labelID, "count", n)
	return n, nil
}

// CountUnreadByLabel returns the number of unread messages carrying the given
// label — used for the sidebar's unread badges.
func (s *Store) CountUnreadByLabel(ctx context.Context, accountID int64, labelID string) (int, error) {
	var n int
	if err := s.reader.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM messages m
		JOIN message_labels ml ON ml.message_rowid = m.rowid AND ml.label_id = ?
		WHERE m.account_id = ? AND m.is_unread = 1`,
		labelID, accountID).Scan(&n); err != nil {
		logging.TraceContext(ctx, "store: count unread by label", "account", accountID, "label", labelID, "err", err)
		return 0, fmt.Errorf("count unread by label: %w", err)
	}
	logging.TraceContext(ctx, "store: count unread by label", "account", accountID, "label", labelID, "count", n)
	return n, nil
}

// ListByLabel returns a page of messages carrying labelID, newest first. Labels
// are not populated on list rows (the list view needs only headers and flags).
func (s *Store) ListByLabel(ctx context.Context, accountID int64, labelID string, limit, offset int) ([]model.Message, error) {
	start := time.Now()
	logging.TraceContext(ctx, "store: list by label", "account", accountID, "label", labelID, "limit", limit, "offset", offset)
	rows, err := s.reader.QueryContext(ctx, `
		SELECT `+msgCols+`
		FROM messages m JOIN message_labels ml ON ml.message_rowid = m.rowid
		WHERE m.account_id = ? AND ml.label_id = ?
		ORDER BY m.internal_date DESC
		LIMIT ? OFFSET ?`, accountID, labelID, limit, offset)
	if err != nil {
		logging.TraceContext(ctx, "store: list by label", "account", accountID, "label", labelID, "err", err)
		return nil, fmt.Errorf("list by label: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: list by label done", "account", accountID, "label", labelID, "count", len(out), "dur", time.Since(start))
	return out, nil
}

// Search runs a full-text query scoped to an account, best matches first. The
// raw user query is turned into a safe FTS5 expression (each whitespace token is
// quoted and made a prefix match), so arbitrary input cannot break the syntax.
func (s *Store) Search(ctx context.Context, accountID int64, query string, limit int) ([]model.Message, error) {
	start := time.Now()
	filter := parseSearch(query)
	match := ftsQuery(filter.freeText)
	preds, predArgs := filter.buildFilterPredicates()
	logging.TraceContext(ctx, "store: search", "account", accountID, "query", query, "match", match, "operators", len(preds), "limit", limit)

	// Nothing to match on (blank, or only unmatchable free text like "*"): return
	// no results rather than every message.
	if match == "" && len(preds) == 0 {
		logging.TraceContext(ctx, "store: search", "account", accountID, "query", query, "count", 0, "reason", "blank match")
		return nil, nil
	}

	var (
		sqlText string
		args    []any
	)
	predSQL := ""
	if len(preds) > 0 {
		predSQL = " AND " + strings.Join(preds, " AND ")
	}
	if match != "" {
		// Free text present: rank by FTS relevance, AND-ed with any field operators.
		args = append(args, match, accountID)
		args = append(args, predArgs...)
		sqlText = `SELECT ` + msgCols + `
			FROM messages_fts JOIN messages m ON m.rowid = messages_fts.rowid
			WHERE messages_fts MATCH ? AND m.account_id = ?` + predSQL + `
			ORDER BY rank
			LIMIT ?`
	} else {
		// Operators only (e.g. "from:alice has:attachment"): query messages
		// directly, newest first, since there is no FTS rank to order by.
		args = append(args, accountID)
		args = append(args, predArgs...)
		sqlText = `SELECT ` + msgCols + `
			FROM messages m
			WHERE m.account_id = ?` + predSQL + `
			ORDER BY m.internal_date DESC, m.rowid DESC
			LIMIT ?`
	}
	args = append(args, limit)

	rows, err := s.reader.QueryContext(ctx, sqlText, args...)
	if err != nil {
		logging.TraceContext(ctx, "store: search", "account", accountID, "query", query, "err", err)
		return nil, fmt.Errorf("search: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: search done", "account", accountID, "query", query, "count", len(out), "dur", time.Since(start))
	return out, nil
}

// ThreadIDsForMessages maps each given Gmail message id to its thread id,
// omitting ids with no cached message. It does this in one query per chunk
// rather than a GetMessage per id (which would also needlessly load labels).
func (s *Store) ThreadIDsForMessages(ctx context.Context, accountID int64, gmailIDs []string) (map[string]string, error) {
	logging.TraceContext(ctx, "store: thread ids for messages", "account", accountID, "n", len(gmailIDs))
	out := make(map[string]string, len(gmailIDs))
	const chunk = 500
	for start := 0; start < len(gmailIDs); start += chunk {
		end := start + chunk
		if end > len(gmailIDs) {
			end = len(gmailIDs)
		}
		ids := gmailIDs[start:end]
		args := make([]any, 0, len(ids)+1)
		args = append(args, accountID)
		for _, id := range ids {
			args = append(args, id)
		}
		rows, err := s.reader.QueryContext(ctx,
			`SELECT gmail_id, thread_id FROM messages WHERE account_id = ? AND gmail_id IN (`+placeholders(len(ids))+`)`,
			args...)
		if err != nil {
			logging.TraceContext(ctx, "store: thread ids for messages", "account", accountID, "err", err)
			return nil, fmt.Errorf("thread ids for messages: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var g, t string
				if err := rows.Scan(&g, &t); err != nil {
					return fmt.Errorf("scan thread id: %w", err)
				}
				out[g] = t
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	logging.TraceContext(ctx, "store: thread ids for messages done", "account", accountID, "n", len(gmailIDs), "count", len(out))
	return out, nil
}

// ExistingMessageIDs returns which of the given provider ids are cached locally,
// in one IN-query per chunk instead of a GetMessage per id.
func (s *Store) ExistingMessageIDs(ctx context.Context, accountID int64, gmailIDs []string) (map[string]bool, error) {
	logging.TraceContext(ctx, "store: existing message ids", "account", accountID, "n", len(gmailIDs))
	out := make(map[string]bool, len(gmailIDs))
	err := s.scanIDsIn(ctx, gmailIDs, out, func(ids []string) (string, []any) {
		args := make([]any, 0, len(ids)+1)
		args = append(args, accountID)
		for _, id := range ids {
			args = append(args, id)
		}
		return `SELECT gmail_id FROM messages WHERE account_id = ? AND gmail_id IN (` + placeholders(len(ids)) + `)`, args
	})
	if err != nil {
		logging.TraceContext(ctx, "store: existing message ids", "account", accountID, "err", err)
		return nil, fmt.Errorf("existing message ids: %w", err)
	}
	logging.TraceContext(ctx, "store: existing message ids done", "account", accountID, "n", len(gmailIDs), "found", len(out))
	return out, nil
}

// MessageIDsWithLabel returns which of the given provider ids carry labelID
// locally. Used as a guard before destructive bulk operations (empty Trash/Spam)
// so a backend that returned out-of-scope ids can't wipe unrelated mail.
func (s *Store) MessageIDsWithLabel(ctx context.Context, accountID int64, labelID string, gmailIDs []string) (map[string]bool, error) {
	logging.TraceContext(ctx, "store: message ids with label", "account", accountID, "label", labelID, "n", len(gmailIDs))
	out := make(map[string]bool, len(gmailIDs))
	err := s.scanIDsIn(ctx, gmailIDs, out, func(ids []string) (string, []any) {
		args := make([]any, 0, len(ids)+2)
		args = append(args, labelID, accountID)
		for _, id := range ids {
			args = append(args, id)
		}
		return `SELECT m.gmail_id FROM messages m
			JOIN message_labels ml ON ml.message_rowid = m.rowid AND ml.label_id = ?
			WHERE m.account_id = ? AND m.gmail_id IN (` + placeholders(len(ids)) + `)`, args
	})
	if err != nil {
		logging.TraceContext(ctx, "store: message ids with label", "account", accountID, "label", labelID, "err", err)
		return nil, fmt.Errorf("message ids with label: %w", err)
	}
	logging.TraceContext(ctx, "store: message ids with label done", "account", accountID, "label", labelID, "n", len(gmailIDs), "found", len(out))
	return out, nil
}

// scanIDsIn runs a chunked single-string-column query over ids (build returns
// the SQL + args for one chunk) and collects the returned ids into out.
func (s *Store) scanIDsIn(ctx context.Context, ids []string, out map[string]bool, build func(chunkIDs []string) (string, []any)) error {
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		q, args := build(ids[start:end])
		rows, err := s.reader.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				out[id] = true
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// ftsQuery converts free-text input into an FTS5 MATCH expression: each token is
// double-quoted (escaping embedded quotes) and given a trailing prefix wildcard,
// joined by implicit AND. Returns "" for blank input.
func ftsQuery(raw string) string {
	var terms []string
	for _, f := range strings.Fields(raw) {
		// Strip double quotes entirely (they can't be meaningfully matched and a
		// quote-only token would otherwise produce an invalid FTS5 expression),
		// then quote the token and make it a prefix match.
		t := strings.ReplaceAll(f, `"`, "")
		if t == "" {
			continue
		}
		terms = append(terms, `"`+t+`"*`)
	}
	return strings.Join(terms, " ")
}

// GetMessage returns a single message (with its labels) by Gmail id.
func (s *Store) GetMessage(ctx context.Context, accountID int64, gmailID string) (model.Message, error) {
	logging.TraceContext(ctx, "store: get message", "account", accountID, "id", gmailID)
	row := s.reader.QueryRowContext(ctx,
		`SELECT `+msgCols+` FROM messages m WHERE m.account_id = ? AND m.gmail_id = ?`,
		accountID, gmailID)
	m, err := scanMessage(row)
	if err == sql.ErrNoRows {
		logging.TraceContext(ctx, "store: get message", "account", accountID, "id", gmailID, "found", false)
		return model.Message{}, ErrNotFound
	}
	if err != nil {
		logging.TraceContext(ctx, "store: get message", "account", accountID, "id", gmailID, "err", err)
		return model.Message{}, fmt.Errorf("get message %q: %w", gmailID, err)
	}
	labels, err := s.loadLabels(ctx, m.RowID)
	if err != nil {
		return model.Message{}, err
	}
	m.Labels = labels
	return m, nil
}

// MessagesMissingHTML returns the gmail ids of body-fetched messages that have
// no HTML body and were fetched before externalized-HTML support (body_fetched =
// 1), newest first. These may be messages whose large HTML part Gmail served via
// an attachment id, which an older build dropped — re-fetching them recovers the
// HTML. A re-fetch stamps body_fetched = 2 (see UpsertBody), so a genuinely
// text-only message is re-checked exactly once and never re-selected here.
func (s *Store) MessagesMissingHTML(ctx context.Context, accountID int64, limit int) ([]string, error) {
	logging.TraceContext(ctx, "store: messages missing html", "account", accountID, "limit", limit)
	rows, err := s.reader.QueryContext(ctx, `
		SELECT m.gmail_id
		FROM messages m JOIN message_bodies b ON b.message_rowid = m.rowid
		WHERE m.account_id = ? AND m.body_fetched = 1
		  AND (b.body_html IS NULL OR b.body_html = '')
		ORDER BY m.internal_date DESC
		LIMIT ?`, accountID, limit)
	if err != nil {
		logging.TraceContext(ctx, "store: messages missing html", "account", accountID, "err", err)
		return nil, fmt.Errorf("query missing-html messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan gmail id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: messages missing html done", "account", accountID, "count", len(ids))
	return ids, nil
}

// GetBody returns the stored body parts for a message rowid.
func (s *Store) GetBody(ctx context.Context, messageRowID int64) (model.MessageBody, error) {
	logging.TraceContext(ctx, "store: get body", "rowid", messageRowID)
	var b model.MessageBody
	b.MessageRowID = messageRowID
	var text, html, raw sql.NullString
	err := s.reader.QueryRowContext(ctx,
		`SELECT body_text, body_html, raw_headers FROM message_bodies WHERE message_rowid = ?`,
		messageRowID).Scan(&text, &html, &raw)
	if err == sql.ErrNoRows {
		logging.TraceContext(ctx, "store: get body", "rowid", messageRowID, "found", false)
		return model.MessageBody{}, ErrNotFound
	}
	if err != nil {
		logging.TraceContext(ctx, "store: get body", "rowid", messageRowID, "err", err)
		return model.MessageBody{}, fmt.Errorf("get body: %w", err)
	}
	b.Text, b.HTML, b.RawHeaders = text.String, html.String, raw.String
	logging.TraceContext(ctx, "store: get body done", "rowid", messageRowID, "text_bytes", len(b.Text), "html_bytes", len(b.HTML))
	return b, nil
}

func (s *Store) loadLabels(ctx context.Context, rowID int64) ([]string, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT label_id FROM message_labels WHERE message_rowid = ?`, rowID)
	if err != nil {
		return nil, fmt.Errorf("load labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan label id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// reindexFTSHook, when non-nil, is called with each rowid reindexFTS runs for.
// It is a test-only seam (nil in production, so a plain nil check per call) that
// lets tests assert a label-only re-upsert does not re-tokenize the body.
var reindexFTSHook func(rowID int64)

// reindexFTS rebuilds the FTS row for a message from the current messages +
// message_bodies state, so it stays correct whether metadata or body changed.
func reindexFTS(ctx context.Context, tx *sql.Tx, rowID int64) error {
	if reindexFTSHook != nil {
		reindexFTSHook(rowID)
	}
	var subject, fromName, fromAddr, snippet, body sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT m.subject, m.from_name, m.from_addr, m.snippet, COALESCE(b.body_text, '')
		FROM messages m LEFT JOIN message_bodies b ON b.message_rowid = m.rowid
		WHERE m.rowid = ?`, rowID).Scan(&subject, &fromName, &fromAddr, &snippet, &body)
	if err != nil {
		return fmt.Errorf("read fts source: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages_fts WHERE rowid = ?`, rowID); err != nil {
		return fmt.Errorf("delete fts row: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages_fts (rowid, subject, from_name, from_addr, snippet, body_text)
		VALUES (?,?,?,?,?,?)`,
		rowID, subject.String, fromName.String, fromAddr.String, snippet.String, body.String); err != nil {
		return fmt.Errorf("insert fts row: %w", err)
	}
	return nil
}

// scanMessagesAndClose scans all rows into messages and closes the rows.
func scanMessagesAndClose(rows *sql.Rows) ([]model.Message, error) {
	defer func() { _ = rows.Close() }()
	return scanMessages(rows)
}

func scanMessages(rows *sql.Rows) ([]model.Message, error) {
	var out []model.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanMessage(sc rowScanner) (model.Message, error) {
	var (
		m       model.Message
		idate   sql.NullInt64
		size    sql.NullInt64
		unread  int
		starred int
		hasAtt  int
		fetched int
		unsub   sql.NullString
		unsubP  int
		strs    = make([]sql.NullString, 10) // from_name..references_hdr text columns
	)
	if err := sc.Scan(
		&m.RowID, &m.AccountID, &m.GmailID, &m.ThreadID, &idate,
		&strs[0], &strs[1], &strs[2], &strs[3], &strs[4], &strs[5],
		&strs[6], &strs[7], &strs[8], &strs[9],
		&unread, &starred, &hasAtt, &size, &fetched, &unsub, &unsubP,
	); err != nil {
		return model.Message{}, err
	}
	m.ListUnsubscribe, m.ListUnsubOneClick = unsub.String, unsubP != 0
	m.FromName, m.FromAddr, m.ReplyTo = strs[0].String, strs[1].String, strs[2].String
	m.ToAddrs, m.CcAddrs = strs[3].String, strs[4].String
	m.Subject, m.Snippet = strs[5].String, strs[6].String
	m.RFC822MsgID, m.InReplyTo, m.References = strs[7].String, strs[8].String, strs[9].String
	if idate.Valid {
		m.InternalDate = time.Unix(idate.Int64, 0)
	}
	m.SizeEstimate = size.Int64
	m.IsUnread, m.IsStarred, m.HasAttachments = unread != 0, starred != 0, hasAtt != 0
	m.BodyFetched = fetched != 0
	return m, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
