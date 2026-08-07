package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jsnjack/mailbox/internal/remotecache"
)

func TestCacheRemoteImagesRewritesAndReusesOffline(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nimage"))
	}))
	dir := t.TempDir()
	w := &window{remoteImages: remotecache.NewWithClient(dir, srv.Client())}
	source := `<img src="` + srv.URL + `/hero.png" alt="hero"><div style="background:url('` + srv.URL + `/hero.png')">x</div>`
	got, cached, missing := w.cacheRemoteImages(context.Background(), source, true)
	if cached != 1 || missing != 0 || requests != 1 || strings.Contains(got, srv.URL) || strings.Count(got, "mbcache:") != 2 {
		t.Fatalf("online rewrite cached=%d missing=%d requests=%d: %s", cached, missing, requests, got)
	}
	srv.Close()
	w = &window{remoteImages: remotecache.NewWithClient(dir, srv.Client())}
	got, cached, missing = w.cacheRemoteImages(context.Background(), source, false)
	if cached != 1 || missing != 0 || strings.Contains(got, srv.URL) || !strings.Contains(got, "mbcache:") {
		t.Fatalf("offline rewrite cached=%d missing=%d: %s", cached, missing, got)
	}
}

func TestCacheRemoteImagesRemovesUnapprovedNetworkURLs(t *testing.T) {
	w := &window{remoteImages: remotecache.NewWithClient(t.TempDir(), http.DefaultClient)}
	source := `<img src="https://tracking.example/pixel.png"><div style="background:url(https://tracking.example/bg.png)">x</div>`
	got, cached, missing := w.cacheRemoteImages(context.Background(), source, false)
	if cached != 0 || missing != 2 || strings.Contains(got, "http") || strings.Contains(got, "src=") {
		t.Fatalf("blocked external URLs survived: %s", got)
	}
}
