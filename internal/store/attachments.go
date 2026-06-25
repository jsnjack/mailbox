package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jsnjack/mailbox/internal/model"
)

// ReplaceAttachments sets the attachment metadata for a message, replacing any
// existing rows. Bytes are not stored here — they are downloaded on demand and
// recorded via SetAttachmentDownloaded.
func (s *Store) ReplaceAttachments(ctx context.Context, messageRowID int64, atts []model.Attachment) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE message_rowid = ?`, messageRowID); err != nil {
			return fmt.Errorf("clear attachments: %w", err)
		}
		for _, a := range atts {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO attachments (message_rowid, gmail_att_id, filename, mime_type, size_bytes)
				VALUES (?,?,?,?,?)`,
				messageRowID, a.GmailAttID, a.Filename, a.MimeType, a.SizeBytes); err != nil {
				return fmt.Errorf("insert attachment %q: %w", a.Filename, err)
			}
		}
		return nil
	})
}

// ListAttachments returns a message's attachments, ordered by id.
func (s *Store) ListAttachments(ctx context.Context, messageRowID int64) ([]model.Attachment, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, message_rowid, gmail_att_id, filename, mime_type, size_bytes, sha256, disk_path
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
	return out, rows.Err()
}

// GetAttachmentByID returns a single attachment row.
func (s *Store) GetAttachmentByID(ctx context.Context, id int64) (model.Attachment, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, message_rowid, gmail_att_id, filename, mime_type, size_bytes, sha256, disk_path
		FROM attachments WHERE id = ?`, id)
	a, err := scanAttachment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Attachment{}, ErrNotFound
	}
	if err != nil {
		return model.Attachment{}, fmt.Errorf("get attachment %d: %w", id, err)
	}
	return a, nil
}

// SetAttachmentDownloaded records the content hash and on-disk path after the
// attachment bytes have been fetched and written.
func (s *Store) SetAttachmentDownloaded(ctx context.Context, id int64, sha256, diskPath string) error {
	if _, err := s.writer.ExecContext(ctx,
		`UPDATE attachments SET sha256 = ?, disk_path = ? WHERE id = ?`, sha256, diskPath, id); err != nil {
		return fmt.Errorf("mark attachment downloaded: %w", err)
	}
	return nil
}

func scanAttachment(sc rowScanner) (model.Attachment, error) {
	var (
		a                         model.Attachment
		filename, mime, sha, disk sql.NullString
		size                      sql.NullInt64
	)
	if err := sc.Scan(&a.ID, &a.MessageRowID, &a.GmailAttID, &filename, &mime, &size, &sha, &disk); err != nil {
		return model.Attachment{}, err
	}
	a.Filename = filename.String
	a.MimeType = mime.String
	a.SizeBytes = size.Int64
	a.SHA256 = sha.String
	a.DiskPath = disk.String
	return a, nil
}
