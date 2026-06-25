package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jsnjack/mailbox/internal/model"
)

// UpsertLabel inserts or updates a label for an account.
func (s *Store) UpsertLabel(ctx context.Context, l model.Label) error {
	_, err := s.writer.ExecContext(ctx, `
		INSERT INTO labels (account_id, gmail_id, name, type, color_bg, unread_total)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, gmail_id) DO UPDATE SET
			name         = excluded.name,
			type         = excluded.type,
			color_bg     = excluded.color_bg,
			unread_total = excluded.unread_total`,
		l.AccountID, l.GmailID, l.Name, string(l.Type), l.ColorBg, l.UnreadTotal)
	if err != nil {
		return fmt.Errorf("upsert label %q: %w", l.GmailID, err)
	}
	return nil
}

// ListLabels returns all labels for an account, ordered by name.
func (s *Store) ListLabels(ctx context.Context, accountID int64) ([]model.Label, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT account_id, gmail_id, name, type, color_bg, unread_total
		FROM labels WHERE account_id = ? ORDER BY name`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.Label
	for rows.Next() {
		var (
			l      model.Label
			typ    string
			color  sql.NullString
			unread sql.NullInt64
		)
		if err := rows.Scan(&l.AccountID, &l.GmailID, &l.Name, &typ, &color, &unread); err != nil {
			return nil, fmt.Errorf("scan label: %w", err)
		}
		l.Type = model.LabelType(typ)
		l.ColorBg = color.String
		l.UnreadTotal = int(unread.Int64)
		out = append(out, l)
	}
	return out, rows.Err()
}
