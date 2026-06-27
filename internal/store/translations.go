package store

import (
	"context"
	"fmt"
)

// SetTranslation persists a message's translation into lang (the translated,
// markup-preserving body HTML). A message body is immutable, so the translation
// is keyed by the message's Gmail id and never needs invalidation.
func (s *Store) SetTranslation(ctx context.Context, accountID int64, gmailID, lang, text string) error {
	_, err := s.writer.ExecContext(ctx,
		`INSERT INTO message_translations (account_id, gmail_id, lang, text)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(account_id, gmail_id, lang) DO UPDATE SET text = excluded.text`,
		accountID, gmailID, lang, text)
	if err != nil {
		return fmt.Errorf("set translation: %w", err)
	}
	return nil
}

// Translations returns cached translations into lang for the given message ids,
// as a gmail_id → text map. Ids with no stored translation are absent. An empty
// input returns an empty map without querying.
func (s *Store) Translations(ctx context.Context, accountID int64, gmailIDs []string, lang string) (map[string]string, error) {
	out := make(map[string]string, len(gmailIDs))
	const chunk = 500 // stay well under SQLite's bound-variable limit
	for start := 0; start < len(gmailIDs); start += chunk {
		end := start + chunk
		if end > len(gmailIDs) {
			end = len(gmailIDs)
		}
		ids := gmailIDs[start:end]
		args := make([]any, 0, len(ids)+2)
		args = append(args, accountID, lang)
		for _, id := range ids {
			args = append(args, id)
		}
		rows, err := s.reader.QueryContext(ctx,
			`SELECT gmail_id, text FROM message_translations
			   WHERE account_id = ? AND lang = ? AND gmail_id IN (`+placeholders(len(ids))+`)`,
			args...)
		if err != nil {
			return nil, fmt.Errorf("query translations: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id, text string
				if err := rows.Scan(&id, &text); err != nil {
					return err
				}
				out[id] = text
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
