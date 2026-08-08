package syncer

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

type pagedSearchBackend struct {
	countingBackend
	accountID int64
	pages     map[string]backend.SearchPage
	tokens    []string
	mu        sync.Mutex
	fetched   []string
}

func (b *pagedSearchBackend) SearchIDsPage(_ context.Context, _ string, token string, _ int) (backend.SearchPage, error) {
	b.tokens = append(b.tokens, token)
	return b.pages[token], nil
}

func (b *pagedSearchBackend) FetchMetadata(_ context.Context, id string) (model.Message, error) {
	b.mu.Lock()
	b.fetched = append(b.fetched, id)
	b.mu.Unlock()
	return model.Message{
		AccountID:    b.accountID,
		GmailID:      id,
		ThreadID:     "thread-" + id,
		Subject:      id,
		InternalDate: time.Unix(100, 0),
	}, nil
}

func TestSearchServerPageUsesOpaqueCursorAndCachesOnlyPage(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	acct, err := s.UpsertAccount(ctx, model.Account{Email: "a@example.com", Type: model.AccountGmail})
	if err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	be := &pagedSearchBackend{
		accountID: acct,
		pages: map[string]backend.SearchPage{
			"opaque-current": {IDs: []string{"m1", "m2"}, Next: "opaque-next"},
		},
	}
	e := &Engine{Store: s}
	page, err := e.SearchServerPage(ctx, be, acct, "from:a@example.com", "opaque-current", 2)
	if err != nil {
		t.Fatalf("SearchServerPage: %v", err)
	}
	if !reflect.DeepEqual(page, backend.SearchPage{IDs: []string{"m1", "m2"}, Next: "opaque-next"}) {
		t.Fatalf("page = %+v", page)
	}
	if !reflect.DeepEqual(be.tokens, []string{"opaque-current"}) {
		t.Fatalf("tokens = %v", be.tokens)
	}
	be.mu.Lock()
	fetched := append([]string(nil), be.fetched...)
	be.mu.Unlock()
	sort.Strings(fetched)
	if !reflect.DeepEqual(fetched, []string{"m1", "m2"}) {
		t.Fatalf("fetched = %v", fetched)
	}
	existing, err := s.ExistingMessageIDs(ctx, acct, []string{"m1", "m2", "m3"})
	if err != nil {
		t.Fatalf("ExistingMessageIDs: %v", err)
	}
	if !existing["m1"] || !existing["m2"] || existing["m3"] {
		t.Fatalf("existing = %v", existing)
	}
}

type offsetSearchBackend struct {
	countingBackend
	ids  []string
	maxs []int
}

func (b *offsetSearchBackend) SearchIDs(_ context.Context, _ string, max int) ([]string, error) {
	b.maxs = append(b.maxs, max)
	if max > len(b.ids) {
		max = len(b.ids)
	}
	return append([]string(nil), b.ids[:max]...), nil
}

func TestSearchIDsOffsetPageKeepsCompatibilityForUnpagedBackend(t *testing.T) {
	be := &offsetSearchBackend{ids: []string{"m1", "m2", "m3", "m4"}}
	first, err := searchIDsOffsetPage(context.Background(), be, "query", "", 2)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	second, err := searchIDsOffsetPage(context.Background(), be, "query", first.Next, 2)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if !reflect.DeepEqual(first, backend.SearchPage{IDs: []string{"m1", "m2"}, Next: "offset:2"}) {
		t.Fatalf("first = %+v", first)
	}
	if !reflect.DeepEqual(second, backend.SearchPage{IDs: []string{"m3", "m4"}}) {
		t.Fatalf("second = %+v", second)
	}
	if !reflect.DeepEqual(be.maxs, []int{3, 5}) {
		t.Fatalf("SearchIDs max args = %v", be.maxs)
	}
}

func TestSearchIDsOffsetPageRejectsForeignToken(t *testing.T) {
	be := &offsetSearchBackend{ids: []string{"m1"}}
	if _, err := searchIDsOffsetPage(context.Background(), be, "query", "provider-token", 2); err == nil {
		t.Fatal("foreign token error = nil")
	}
}
