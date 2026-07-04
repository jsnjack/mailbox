package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
)

// Subscription summarizes one mailing-list sender for the subscriptions
// dashboard: who, how much mail, and how to leave.
type Subscription struct {
	FromAddr        string
	FromName        string
	Count           int   // all cached mail from this sender
	LastSeen        int64 // unix seconds of the newest message
	ListUnsubscribe string
	OneClick        bool
}

// Subscriptions groups an account's cached mail by senders that carry a
// List-Unsubscribe header, most mail first. The unsubscribe header comes from
// the sender's newest carrying message (lists rotate tokens; old ones expire).
func (s *Store) Subscriptions(ctx context.Context, accountID int64) ([]Subscription, error) {
	start := time.Now()
	rows, err := s.reader.QueryContext(ctx, `
		SELECT u.from_addr,
		       COALESCE((SELECT m3.from_name FROM messages m3
		                 WHERE m3.account_id = ? AND m3.from_addr = u.from_addr AND m3.from_name != ''
		                 ORDER BY m3.internal_date DESC LIMIT 1), u.from_addr),
		       (SELECT COUNT(*) FROM messages mc WHERE mc.account_id = ? AND mc.from_addr = u.from_addr),
		       (SELECT COALESCE(MAX(m4.internal_date),0) FROM messages m4 WHERE m4.account_id = ? AND m4.from_addr = u.from_addr),
		       u.list_unsubscribe, u.list_unsub_post
		FROM (
			SELECT m.from_addr, m.list_unsubscribe, m.list_unsub_post,
			       ROW_NUMBER() OVER (PARTITION BY m.from_addr ORDER BY m.internal_date DESC) rn
			FROM messages m
			WHERE m.account_id = ? AND m.list_unsubscribe != '' AND m.from_addr != ''
		) u
		WHERE u.rn = 1
		ORDER BY 3 DESC`, accountID, accountID, accountID, accountID)
	if err != nil {
		return nil, fmt.Errorf("subscriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Subscription
	for rows.Next() {
		var sub Subscription
		var oneClick int
		if err := rows.Scan(&sub.FromAddr, &sub.FromName, &sub.Count, &sub.LastSeen, &sub.ListUnsubscribe, &oneClick); err != nil {
			return nil, fmt.Errorf("scan subscription: %w", err)
		}
		sub.OneClick = oneClick != 0
		out = append(out, sub)
	}
	logging.TraceContext(ctx, "store: subscriptions", "account", accountID, "count", len(out), "dur", time.Since(start))
	return out, rows.Err()
}
