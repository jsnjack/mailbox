package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
)

// SetMessageGist persists a message's AI one-line gist (keyed by its Gmail id).
// A message's content is immutable, so the gist is written once and reused —
// by the reader's summary card and the desktop notification alike.
func (s *Store) SetMessageGist(ctx context.Context, accountID int64, gmailID, gist string) error {
	logging.TraceContext(ctx, "store: set message gist", "account", accountID, "id", gmailID, "gist", gist)
	_, err := s.writer.ExecContext(ctx,
		`INSERT INTO message_gists (account_id, gmail_id, gist)
		 VALUES (?, ?, ?)
		 ON CONFLICT(account_id, gmail_id) DO UPDATE SET gist = excluded.gist`,
		accountID, gmailID, gist)
	if err != nil {
		logging.TraceContext(ctx, "store: set message gist", "account", accountID, "id", gmailID, "err", err)
		return fmt.Errorf("set message gist: %w", err)
	}
	return nil
}

// MessageGists returns the cached gists for the given message ids, as a
// gmail_id → gist map. Ids with no stored gist are absent from the map. An
// empty input returns an empty map without querying.
func (s *Store) MessageGists(ctx context.Context, accountID int64, gmailIDs []string) (map[string]string, error) {
	begin := time.Now()
	logging.TraceContext(ctx, "store: message gists", "account", accountID, "n", len(gmailIDs))
	out := make(map[string]string, len(gmailIDs))
	const chunk = 500 // stay well under SQLite's bound-variable limit
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
			`SELECT gmail_id, gist FROM message_gists
			   WHERE account_id = ? AND gmail_id IN (`+placeholders(len(ids))+`)`,
			args...)
		if err != nil {
			return nil, fmt.Errorf("query message gists: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id, g string
				if err := rows.Scan(&id, &g); err != nil {
					return err
				}
				out[id] = g
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	logging.TraceContext(ctx, "store: message gists done", "account", accountID, "n", len(gmailIDs), "count", len(out), "dur", time.Since(begin))
	return out, nil
}
