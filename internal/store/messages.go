package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

// msgCols is the messages column list, aliased to m, for SELECTs that may join
// message_labels (where account_id would otherwise be ambiguous).
const msgCols = `m.rowid, m.account_id, m.gmail_id, m.thread_id, m.internal_date, ` +
	`m.from_name, m.from_addr, m.to_addrs, m.cc_addrs, m.subject, m.snippet, ` +
	`m.rfc822_msgid, m.in_reply_to, m.references_hdr, m.is_unread, m.is_starred, ` +
	`m.has_attachments, m.size_estimate, m.body_fetched`

// UpsertMessage inserts or updates a message's metadata, replaces its label set,
// and refreshes its full-text index entry. It returns the message's local rowid.
func (s *Store) UpsertMessage(ctx context.Context, m model.Message) (int64, error) {
	var rowid int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var idate any
		if !m.InternalDate.IsZero() {
			idate = m.InternalDate.Unix()
		}
		err := tx.QueryRowContext(ctx, `
			INSERT INTO messages (
				account_id, gmail_id, thread_id, internal_date, from_name, from_addr,
				to_addrs, cc_addrs, subject, snippet, rfc822_msgid, in_reply_to,
				references_hdr, is_unread, is_starred, has_attachments, size_estimate)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(account_id, gmail_id) DO UPDATE SET
				thread_id=excluded.thread_id, internal_date=excluded.internal_date,
				from_name=excluded.from_name, from_addr=excluded.from_addr,
				to_addrs=excluded.to_addrs, cc_addrs=excluded.cc_addrs,
				subject=excluded.subject, snippet=excluded.snippet,
				rfc822_msgid=excluded.rfc822_msgid, in_reply_to=excluded.in_reply_to,
				references_hdr=excluded.references_hdr, is_unread=excluded.is_unread,
				is_starred=excluded.is_starred, has_attachments=excluded.has_attachments,
				size_estimate=excluded.size_estimate
			RETURNING rowid`,
			m.AccountID, m.GmailID, m.ThreadID, idate, m.FromName, m.FromAddr,
			m.ToAddrs, m.CcAddrs, m.Subject, m.Snippet, m.RFC822MsgID, m.InReplyTo,
			m.References, b2i(m.IsUnread), b2i(m.IsStarred), b2i(m.HasAttachments), m.SizeEstimate,
		).Scan(&rowid)
		if err != nil {
			return fmt.Errorf("upsert message %q: %w", m.GmailID, err)
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM message_labels WHERE message_rowid = ?`, rowid); err != nil {
			return fmt.Errorf("clear labels: %w", err)
		}
		for _, lbl := range m.Labels {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO message_labels (message_rowid, account_id, label_id) VALUES (?,?,?)`,
				rowid, m.AccountID, lbl); err != nil {
				return fmt.Errorf("insert label %q: %w", lbl, err)
			}
		}
		return reindexFTS(ctx, tx, rowid)
	})
	return rowid, err
}

// UpsertBody stores a message's body parts, marks the message body-fetched, and
// refreshes the full-text index so the body becomes searchable.
func (s *Store) UpsertBody(ctx context.Context, b model.MessageBody) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO message_bodies (message_rowid, body_text, body_html, raw_headers)
			VALUES (?,?,?,?)
			ON CONFLICT(message_rowid) DO UPDATE SET
				body_text=excluded.body_text, body_html=excluded.body_html,
				raw_headers=excluded.raw_headers`,
			b.MessageRowID, b.Text, b.HTML, b.RawHeaders); err != nil {
			return fmt.Errorf("upsert body: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET body_fetched = 1 WHERE rowid = ?`, b.MessageRowID); err != nil {
			return fmt.Errorf("mark body fetched: %w", err)
		}
		return reindexFTS(ctx, tx, b.MessageRowID)
	})
}

// DeleteMessage removes a message and its dependent rows (labels, body,
// attachments cascade) plus its FTS entry. It is a no-op if the message is absent.
func (s *Store) DeleteMessage(ctx context.Context, accountID int64, gmailID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var rowID int64
		err := tx.QueryRowContext(ctx,
			`SELECT rowid FROM messages WHERE account_id = ? AND gmail_id = ?`,
			accountID, gmailID).Scan(&rowID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find message to delete: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages_fts WHERE rowid = ?`, rowID); err != nil {
			return fmt.Errorf("delete fts row: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE rowid = ?`, rowID); err != nil {
			return fmt.Errorf("delete message: %w", err)
		}
		return nil
	})
}

// ModifyLabels applies a label delta to a message (adding and removing label
// ids) and recomputes the derived unread/starred flags. It is used for optimistic
// local updates before the change is mirrored to Gmail.
func (s *Store) ModifyLabels(ctx context.Context, accountID int64, gmailID string, add, remove []string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
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
	})
}

// CountByLabel returns the number of messages carrying the given label.
func (s *Store) CountByLabel(ctx context.Context, accountID int64, labelID string) (int, error) {
	var n int
	if err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM message_labels WHERE account_id = ? AND label_id = ?`,
		accountID, labelID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count by label: %w", err)
	}
	return n, nil
}

// ListByLabel returns a page of messages carrying labelID, newest first. Labels
// are not populated on list rows (the list view needs only headers and flags).
func (s *Store) ListByLabel(ctx context.Context, accountID int64, labelID string, limit, offset int) ([]model.Message, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT `+msgCols+`
		FROM messages m JOIN message_labels ml ON ml.message_rowid = m.rowid
		WHERE m.account_id = ? AND ml.label_id = ?
		ORDER BY m.internal_date DESC
		LIMIT ? OFFSET ?`, accountID, labelID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list by label: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMessages(rows)
}

// Search runs a full-text query scoped to an account, best matches first. The
// raw user query is turned into a safe FTS5 expression (each whitespace token is
// quoted and made a prefix match), so arbitrary input cannot break the syntax.
func (s *Store) Search(ctx context.Context, accountID int64, query string, limit int) ([]model.Message, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT `+msgCols+`
		FROM messages_fts JOIN messages m ON m.rowid = messages_fts.rowid
		WHERE messages_fts MATCH ? AND m.account_id = ?
		ORDER BY rank
		LIMIT ?`, match, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMessages(rows)
}

// ftsQuery converts free-text input into an FTS5 MATCH expression: each token is
// double-quoted (escaping embedded quotes) and given a trailing prefix wildcard,
// joined by implicit AND. Returns "" for blank input.
func ftsQuery(raw string) string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return ""
	}
	var b strings.Builder
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(`"` + strings.ReplaceAll(f, `"`, `""`) + `"*`)
	}
	return b.String()
}

// GetMessage returns a single message (with its labels) by Gmail id.
func (s *Store) GetMessage(ctx context.Context, accountID int64, gmailID string) (model.Message, error) {
	row := s.reader.QueryRowContext(ctx,
		`SELECT `+msgCols+` FROM messages m WHERE m.account_id = ? AND m.gmail_id = ?`,
		accountID, gmailID)
	m, err := scanMessage(row)
	if err == sql.ErrNoRows {
		return model.Message{}, ErrNotFound
	}
	if err != nil {
		return model.Message{}, fmt.Errorf("get message %q: %w", gmailID, err)
	}
	labels, err := s.loadLabels(ctx, m.RowID)
	if err != nil {
		return model.Message{}, err
	}
	m.Labels = labels
	return m, nil
}

// GetBody returns the stored body parts for a message rowid.
func (s *Store) GetBody(ctx context.Context, messageRowID int64) (model.MessageBody, error) {
	var b model.MessageBody
	b.MessageRowID = messageRowID
	var text, html, raw sql.NullString
	err := s.reader.QueryRowContext(ctx,
		`SELECT body_text, body_html, raw_headers FROM message_bodies WHERE message_rowid = ?`,
		messageRowID).Scan(&text, &html, &raw)
	if err == sql.ErrNoRows {
		return model.MessageBody{}, ErrNotFound
	}
	if err != nil {
		return model.MessageBody{}, fmt.Errorf("get body: %w", err)
	}
	b.Text, b.HTML, b.RawHeaders = text.String, html.String, raw.String
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

// reindexFTS rebuilds the FTS row for a message from the current messages +
// message_bodies state, so it stays correct whether metadata or body changed.
func reindexFTS(ctx context.Context, tx *sql.Tx, rowID int64) error {
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
		strs    = make([]sql.NullString, 9) // from_name..references_hdr text columns
	)
	if err := sc.Scan(
		&m.RowID, &m.AccountID, &m.GmailID, &m.ThreadID, &idate,
		&strs[0], &strs[1], &strs[2], &strs[3], &strs[4], &strs[5],
		&strs[6], &strs[7], &strs[8],
		&unread, &starred, &hasAtt, &size, &fetched,
	); err != nil {
		return model.Message{}, err
	}
	m.FromName, m.FromAddr = strs[0].String, strs[1].String
	m.ToAddrs, m.CcAddrs = strs[2].String, strs[3].String
	m.Subject, m.Snippet = strs[4].String, strs[5].String
	m.RFC822MsgID, m.InReplyTo, m.References = strs[6].String, strs[7].String, strs[8].String
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
