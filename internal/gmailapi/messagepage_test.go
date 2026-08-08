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

func TestListMessageIDsPagePassesOpaqueCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gmail/v1/users/me/messages" {
			t.Errorf("path = %q", r.URL.Path)
		}
		query := r.URL.Query()
		if got := query.Get("q"); got != "from:sender@example.com" {
			t.Errorf("q = %q", got)
		}
		if got := query.Get("pageToken"); got != "opaque-current" {
			t.Errorf("pageToken = %q", got)
		}
		if got := query.Get("maxResults"); got != "37" {
			t.Errorf("maxResults = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messages":[{"id":"m1"},{"id":"m2"}],"nextPageToken":"opaque-next"}`)
	}))
	defer srv.Close()

	gsrv, err := gmail.NewService(context.Background(),
		option.WithHTTPClient(srv.Client()), option.WithEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("gmail.NewService: %v", err)
	}
	stats := &Stats{}
	ids, next, err := NewClientStats(gsrv, stats).ListMessageIDsPage(
		context.Background(), "from:sender@example.com", "opaque-current", 37,
	)
	if err != nil {
		t.Fatalf("ListMessageIDsPage: %v", err)
	}
	if len(ids) != 2 || ids[0] != "m1" || ids[1] != "m2" {
		t.Fatalf("ids = %v", ids)
	}
	if next != "opaque-next" {
		t.Fatalf("next = %q", next)
	}
	if got := stats.Snapshot().QuotaUnits; got != costMessageList {
		t.Fatalf("quota units = %d, want %d", got, costMessageList)
	}
}
