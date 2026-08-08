package imapbackend

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/logging"
)

const maxSearchSessions = 8

type imapSearchSession struct {
	query   string
	ids     []string
	offset  int
	touched time.Time
}

// SearchIDsPage searches once, globally orders matches by message date, and
// keeps the remaining ids behind an opaque in-memory cursor. Later pages do not
// repeat SEARCH over every folder.
func (b *Backend) SearchIDsPage(ctx context.Context, query, pageToken string, limit int) (backend.SearchPage, error) {
	if limit <= 0 {
		return backend.SearchPage{}, fmt.Errorf("imap search page limit must be positive")
	}
	if pageToken != "" {
		return b.nextSearchPage(query, pageToken, limit)
	}
	ids, err := b.searchIDsByDate(ctx, query)
	if err != nil {
		return backend.SearchPage{}, err
	}
	if len(ids) <= limit {
		return backend.SearchPage{IDs: ids}, nil
	}
	token := fmt.Sprintf("imap-search:%d", b.searchSeq.Add(1))
	b.searchMu.Lock()
	b.evictSearchSessionLocked()
	b.searchSessions[token] = imapSearchSession{query: query, ids: ids, offset: limit, touched: time.Now()}
	b.searchMu.Unlock()
	return backend.SearchPage{IDs: ids[:limit], Next: token}, nil
}

func (b *Backend) nextSearchPage(query, token string, limit int) (backend.SearchPage, error) {
	b.searchMu.Lock()
	defer b.searchMu.Unlock()
	s, ok := b.searchSessions[token]
	if !ok || s.query != query {
		return backend.SearchPage{}, fmt.Errorf("invalid or expired IMAP search page token")
	}
	end := min(s.offset+limit, len(s.ids))
	page := backend.SearchPage{IDs: append([]string(nil), s.ids[s.offset:end]...)}
	if end == len(s.ids) {
		delete(b.searchSessions, token)
		return page, nil
	}
	s.offset = end
	s.touched = time.Now()
	b.searchSessions[token] = s
	page.Next = token
	return page, nil
}

func (b *Backend) evictSearchSessionLocked() {
	if len(b.searchSessions) < maxSearchSessions {
		return
	}
	oldestToken := ""
	var oldest time.Time
	for token, s := range b.searchSessions {
		if oldestToken == "" || s.touched.Before(oldest) {
			oldestToken, oldest = token, s.touched
		}
	}
	delete(b.searchSessions, oldestToken)
}

type datedSearchID struct {
	id   string
	date time.Time
}

func (b *Backend) searchIDsByDate(ctx context.Context, query string) ([]string, error) {
	q, err := parseSearchQuery(query)
	if err != nil {
		return nil, err
	}
	var hits []datedSearchID
	err = b.withConn(ctx, func(c *conn) error {
		folders, err := b.folders(c)
		if err != nil {
			return err
		}
		if q.label != "" {
			folders, err = b.foldersForLabel(q.label)
			if err != nil {
				return err
			}
		}
		for _, folder := range folders {
			sel, err := c.selectMailbox(folder, false)
			if err != nil {
				return err
			}
			criteria := q.criteria
			result, err := c.cl.UIDSearch(&criteria, nil).Wait()
			if err != nil {
				return fmt.Errorf("imap uid search %q: %w", folder, err)
			}
			uids := result.AllUIDs()
			for start := 0; start < len(uids); start += metadataFetchChunk {
				end := min(start+metadataFetchChunk, len(uids))
				bufs, err := c.cl.Fetch(uidSetOf(uids[start:end]), &imap.FetchOptions{
					UID: true, InternalDate: true,
				}).Collect()
				if err != nil {
					return fmt.Errorf("imap fetch search dates %q: %w", folder, err)
				}
				for _, buf := range bufs {
					hits = append(hits, datedSearchID{
						id: msgID(folder, sel.UIDValidity, buf.UID), date: buf.InternalDate,
					})
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].date.Equal(hits[j].date) {
			return hits[i].id > hits[j].id
		}
		return hits[i].date.After(hits[j].date)
	})
	ids := make([]string, len(hits))
	for i, hit := range hits {
		ids[i] = hit.id
	}
	logging.TraceContext(ctx, "imapbackend: paged search ready", "account", b.cfg.Email, "query", query, "hits", len(ids))
	return ids, nil
}
