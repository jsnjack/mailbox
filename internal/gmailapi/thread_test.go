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

func TestGetThreadMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gmail/v1/users/me/threads/t1" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("format"); got != "metadata" {
			t.Errorf("format = %q, want metadata", got)
		}
		if len(r.URL.Query()["metadataHeaders"]) == 0 {
			t.Error("metadataHeaders missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"t1","messages":[{"id":"m1","threadId":"t1"},{"id":"m2","threadId":"t1"}]}`)
	}))
	defer srv.Close()

	gsrv, err := gmail.NewService(context.Background(),
		option.WithHTTPClient(srv.Client()), option.WithEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("gmail.NewService: %v", err)
	}
	stats := &Stats{}
	thread, err := NewClientStats(gsrv, stats).GetThreadMetadata(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetThreadMetadata: %v", err)
	}
	if len(thread.Messages) != 2 || thread.Messages[0].Id != "m1" || thread.Messages[1].Id != "m2" {
		t.Fatalf("messages = %+v", thread.Messages)
	}
	if got := stats.Snapshot().QuotaUnits; got != costThreadGet {
		t.Fatalf("quota units = %d, want %d", got, costThreadGet)
	}
}
