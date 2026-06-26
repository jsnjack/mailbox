package store

import (
	"context"
	"database/sql"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

// contactScanCap bounds how many recent messages are scanned to build the
// contact index — plenty for ranking without walking a huge mailbox.
const contactScanCap = 20000

// contactInput is one message's address-bearing headers, fed to buildContacts.
type contactInput struct {
	FromName, FromAddr, To, Cc string
	When                       time.Time
}

// Contacts returns correspondents seen in the account's cached mail, ranked by
// how often and how recently they appear (best first), excluding the account's
// own address. limit caps the result (<=0 means no cap).
func (s *Store) Contacts(ctx context.Context, accountID int64, self string, limit int) ([]model.Contact, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT from_name, from_addr, to_addrs, cc_addrs, internal_date
		   FROM messages WHERE account_id = ? ORDER BY internal_date DESC LIMIT ?`,
		accountID, contactScanCap)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var in []contactInput
	for rows.Next() {
		var fromName, fromAddr, to, cc sql.NullString
		var idate sql.NullInt64
		if err := rows.Scan(&fromName, &fromAddr, &to, &cc, &idate); err != nil {
			return nil, err
		}
		in = append(in, contactInput{
			FromName: fromName.String, FromAddr: fromAddr.String,
			To: to.String, Cc: cc.String, When: time.Unix(idate.Int64, 0),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return buildContacts(in, self, limit), nil
}

// buildContacts aggregates address-bearing headers into a ranked contact list.
// It is pure (no DB) so it can be unit-tested directly.
func buildContacts(in []contactInput, self string, limit int) []model.Contact {
	self = strings.ToLower(strings.TrimSpace(self))
	byAddr := map[string]*model.Contact{}

	add := func(name, addr string, when time.Time) {
		addr = strings.ToLower(strings.TrimSpace(addr))
		if addr == "" || !strings.Contains(addr, "@") || addr == self {
			return
		}
		c := byAddr[addr]
		if c == nil {
			c = &model.Contact{Address: addr}
			byAddr[addr] = c
		}
		c.Count++
		if name = strings.TrimSpace(name); name != "" && (c.Name == "" || strings.EqualFold(c.Name, c.Address)) {
			c.Name = name
		}
		if when.After(c.LastSeen) {
			c.LastSeen = when
		}
	}

	for _, m := range in {
		add(m.FromName, m.FromAddr, m.When)
		for _, a := range parseAddressList(m.To) {
			add(a.Name, a.Address, m.When)
		}
		for _, a := range parseAddressList(m.Cc) {
			add(a.Name, a.Address, m.When)
		}
	}

	out := make([]model.Contact, 0, len(byAddr))
	for _, c := range byAddr {
		out = append(out, *c)
	}
	// Most-used first; ties broken by most-recent, then address for stability.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].Address < out[j].Address
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// parseAddressList tolerantly parses a raw To/Cc header into addresses, ignoring
// malformed input.
func parseAddressList(raw string) []*mail.Address {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	addrs, err := mail.ParseAddressList(raw)
	if err != nil {
		return nil
	}
	return addrs
}
