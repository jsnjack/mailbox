package gmailapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// FindDraftID paginates the drafts list; each page is a separate API request and
// must be charged as such. Before the fix the whole (multi-page) scan was charged
// as one request, under-counting quota/stats.
func TestFindDraftIDChargesPerPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "" {
			// Page 1: no match, points to page 2.
			_, _ = io.WriteString(w, `{"drafts":[{"id":"d1","message":{"id":"m1"}}],"nextPageToken":"p2"}`)
			return
		}
		// Page 2: the match.
		_, _ = io.WriteString(w, `{"drafts":[{"id":"d2","message":{"id":"target"}}]}`)
	}))
	defer srv.Close()

	gsrv, err := gmail.NewService(context.Background(),
		option.WithHTTPClient(srv.Client()), option.WithEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("gmail.NewService: %v", err)
	}
	stats := &Stats{}
	c := NewClientStats(gsrv, stats)

	id, err := c.FindDraftID(context.Background(), "target")
	if err != nil {
		t.Fatalf("FindDraftID: %v", err)
	}
	if id != "d2" {
		t.Fatalf("draft id = %q, want d2", id)
	}
	snap := stats.Snapshot()
	if snap.Requests != 2 {
		t.Fatalf("charged %d requests, want 2 (one per page)", snap.Requests)
	}
	if snap.QuotaUnits != int64(2*costMessageList) {
		t.Fatalf("charged %d quota units, want %d", snap.QuotaUnits, 2*costMessageList)
	}
}
