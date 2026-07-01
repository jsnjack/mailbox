package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// ReplaceAttachments sets the attachment metadata for a message, replacing any
// existing rows. Bytes are not stored here — they are downloaded on demand and
// recorded via SetAttachmentDownloaded.
func (s *Store) ReplaceAttachments(ctx context.Context, messageRowID int64, atts []model.Attachment) error {
	start := time.Now()
	logging.TraceContext(ctx, "store: replace attachments", "rowid", messageRowID, "count", len(atts))
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE message_rowid = ?`, messageRowID); err != nil {
			return fmt.Errorf("clear attachments: %w", err)
		}
		for _, a := range atts {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO attachments (message_rowid, gmail_att_id, filename, mime_type, size_bytes, content_id)
				VALUES (?,?,?,?,?,?)`,
				messageRowID, a.GmailAttID, a.Filename, a.MimeType, a.SizeBytes, a.ContentID); err != nil {
				return fmt.Errorf("insert attachment %q: %w", a.Filename, err)
			}
		}
		return nil
	})
	if err != nil {
		logging.TraceContext(ctx, "store: replace attachments", "rowid", messageRowID, "err", err)
		return err
	}
	logging.TraceContext(ctx, "store: replace attachments done", "rowid", messageRowID, "count", len(atts), "dur", time.Since(start))
	return nil
}

// ListAttachments returns a message's attachments, ordered by id.
func (s *Store) ListAttachments(ctx context.Context, messageRowID int64) ([]model.Attachment, error) {
	logging.TraceContext(ctx, "store: list attachments", "rowid", messageRowID)
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, message_rowid, gmail_att_id, filename, mime_type, size_bytes, sha256, disk_path, content_id
		FROM attachments WHERE message_rowid = ? ORDER BY id`, messageRowID)
	if err != nil {
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.Attachment
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	logging.TraceContext(ctx, "store: list attachments", "rowid", messageRowID, "count", len(out))
	return out, nil
}

// GetAttachmentByID returns a single attachment row.
func (s *Store) GetAttachmentByID(ctx context.Context, id int64) (model.Attachment, error) {
	logging.TraceContext(ctx, "store: get attachment", "id", id)
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, message_rowid, gmail_att_id, filename, mime_type, size_bytes, sha256, disk_path, content_id
		FROM attachments WHERE id = ?`, id)
	a, err := scanAttachment(row)
	if errors.Is(err, sql.ErrNoRows) {
		logging.TraceContext(ctx, "store: get attachment", "id", id, "found", false)
		return model.Attachment{}, ErrNotFound
	}
	if err != nil {
		logging.TraceContext(ctx, "store: get attachment", "id", id, "err", err)
		return model.Attachment{}, fmt.Errorf("get attachment %d: %w", id, err)
	}
	return a, nil
}

// SetAttachmentDownloaded records the content hash and on-disk path after the
// attachment bytes have been fetched and written.
func (s *Store) SetAttachmentDownloaded(ctx context.Context, id int64, sha256, diskPath string) error {
	logging.TraceContext(ctx, "store: set attachment downloaded", "id", id, "sha256", sha256, "path", diskPath)
	if _, err := s.writer.ExecContext(ctx,
		`UPDATE attachments SET sha256 = ?, disk_path = ? WHERE id = ?`, sha256, diskPath, id); err != nil {
		logging.TraceContext(ctx, "store: set attachment downloaded", "id", id, "err", err)
		return fmt.Errorf("mark attachment downloaded: %w", err)
	}
	return nil
}

func scanAttachment(sc rowScanner) (model.Attachment, error) {
	var (
		a                              model.Attachment
		filename, mime, sha, disk, cid sql.NullString
		size                           sql.NullInt64
	)
	if err := sc.Scan(&a.ID, &a.MessageRowID, &a.GmailAttID, &filename, &mime, &size, &sha, &disk, &cid); err != nil {
		return model.Attachment{}, err
	}
	a.Filename = filename.String
	a.MimeType = mime.String
	a.SizeBytes = size.Int64
	a.SHA256 = sha.String
	a.DiskPath = disk.String
	a.ContentID = cid.String
	return a, nil
}
