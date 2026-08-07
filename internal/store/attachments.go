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
// rows no longer reported by the provider. Download metadata is retained when
// the provider attachment id still exists: a body re-fetch must not make an
// already-cached attachment unavailable offline or leak its orphaned file.
func (s *Store) ReplaceAttachments(ctx context.Context, messageRowID int64, atts []model.Attachment) error {
	start := time.Now()
	logging.TraceContext(ctx, "store: replace attachments", "rowid", messageRowID, "count", len(atts))
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		type cachedAttachment struct {
			filename, mimeType, contentID, sha256, diskPath string
			sizeBytes                                       int64
		}
		cached := make(map[string]cachedAttachment)
		rows, err := tx.QueryContext(ctx, `
			SELECT gmail_att_id, COALESCE(filename, ''), COALESCE(mime_type, ''),
				COALESCE(size_bytes, 0), content_id, COALESCE(sha256, ''), COALESCE(disk_path, '')
			FROM attachments WHERE message_rowid = ?`, messageRowID)
		if err != nil {
			return fmt.Errorf("read cached attachments: %w", err)
		}
		for rows.Next() {
			var providerID string
			var prior cachedAttachment
			if err := rows.Scan(&providerID, &prior.filename, &prior.mimeType, &prior.sizeBytes,
				&prior.contentID, &prior.sha256, &prior.diskPath); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan cached attachment: %w", err)
			}
			cached[providerID] = prior
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close cached attachments: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read cached attachments: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE message_rowid = ?`, messageRowID); err != nil {
			return fmt.Errorf("clear attachments: %w", err)
		}
		for _, a := range atts {
			prior := cached[a.GmailAttID]
			// Provider ids are stable for Gmail. IMAP uses a MIME-part ordinal,
			// which can shift when parser support improves; require the immutable
			// metadata to match as well so an old PDF can never be served as a newly
			// discovered inline image that inherited its ordinal.
			if prior.filename != a.Filename || prior.mimeType != a.MimeType ||
				prior.sizeBytes != a.SizeBytes || prior.contentID != a.ContentID {
				prior.sha256, prior.diskPath = "", ""
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO attachments
					(message_rowid, gmail_att_id, filename, mime_type, size_bytes, sha256, disk_path, content_id)
				VALUES (?,?,?,?,?,?,?,?)`,
				messageRowID, a.GmailAttID, a.Filename, a.MimeType, a.SizeBytes,
				prior.sha256, prior.diskPath, a.ContentID); err != nil {
				return fmt.Errorf("insert attachment %q: %w", a.Filename, err)
			}
		}
		hasDownloadable := false
		for _, a := range atts {
			if a.ContentID == "" {
				hasDownloadable = true
				break
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE messages SET has_attachments = ? WHERE rowid = ?`, b2i(hasDownloadable), messageRowID); err != nil {
			return fmt.Errorf("update attachment marker: %w", err)
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
