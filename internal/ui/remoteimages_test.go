package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	got, stats := w.cacheRemoteImages(context.Background(), source, true, false)
	if stats.Cached != 1 || stats.Unavailable != 0 || stats.Deferred != 0 || requests != 1 || strings.Contains(got, srv.URL) || strings.Count(got, "mbcache:") != 2 {
		t.Fatalf("online rewrite stats=%+v requests=%d: %s", stats, requests, got)
	}
	srv.Close()
	w = &window{remoteImages: remotecache.NewWithClient(dir, srv.Client())}
	got, stats = w.cacheRemoteImages(context.Background(), source, false, false)
	if stats.Cached != 1 || stats.Blocked != 0 || strings.Contains(got, srv.URL) || !strings.Contains(got, "mbcache:") {
		t.Fatalf("offline rewrite stats=%+v: %s", stats, got)
	}
}

func TestCacheRemoteImagesNeverFetchesNonImageCSSResources(t *testing.T) {
	var mu sync.Mutex
	requests := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nimage"))
	}))
	defer srv.Close()
	w := &window{remoteImages: remotecache.NewWithClient(t.TempDir(), srv.Client())}
	source := `<style>` +
		`@import "` + srv.URL + `/import.css";` +
		`@font-face{font-family:x;src:url(` + srv.URL + `/font.woff2)}` +
		`.hero{background-image:url(` + srv.URL + `/hero.png)}` +
		`.pointer{cursor:url(` + srv.URL + `/cursor.png),auto}` +
		`</style><div class="hero">Hello</div>`
	got, stats := w.cacheRemoteImages(context.Background(), source, true, false)
	mu.Lock()
	defer mu.Unlock()
	if stats.Cached != 1 || stats.Unavailable != 0 || requests["/hero.png"] != 1 {
		t.Fatalf("stats=%+v requests=%v: %s", stats, requests, got)
	}
	for _, path := range []string{"/import.css", "/font.woff2", "/cursor.png"} {
		if requests[path] != 0 || strings.Contains(got, srv.URL+path) {
			t.Fatalf("non-image CSS resource %s survived or was requested: requests=%v html=%s", path, requests, got)
		}
	}
}

func TestCacheRemoteImagesRemovesUnapprovedNetworkURLs(t *testing.T) {
	w := &window{remoteImages: remotecache.NewWithClient(t.TempDir(), http.DefaultClient)}
	source := `<img src="https://tracking.example/pixel.png"><div style="background:url(https://tracking.example/bg.png)">x</div>`
	got, stats := w.cacheRemoteImages(context.Background(), source, false, false)
	if stats.Cached != 0 || stats.Blocked != 2 || strings.Contains(got, "http") || strings.Contains(got, "src=") {
		t.Fatalf("stats=%+v, blocked external URLs survived: %s", stats, got)
	}
}

func TestCacheRemoteImagesRequiresApprovalForLargeSets(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nimage"))
	}))
	defer srv.Close()
	w := &window{remoteImages: remotecache.NewWithClient(t.TempDir(), srv.Client())}
	var source strings.Builder
	for i := range remoteImagePromptThreshold + 1 {
		fmt.Fprintf(&source, `<img src="%s/%d.png">`, srv.URL, i)
	}
	got, stats := w.cacheRemoteImages(context.Background(), source.String(), true, false)
	if stats.Total != remoteImagePromptThreshold+1 || stats.Cached != 0 || stats.Deferred != stats.Total || requests.Load() != 0 || strings.Contains(got, "mbcache:") {
		t.Fatalf("unapproved pass stats=%+v requests=%d: %s", stats, requests.Load(), got)
	}
	got, stats = w.cacheRemoteImages(context.Background(), source.String(), true, true)
	if stats.Cached != stats.Total || stats.Deferred != 0 || stats.Unavailable != 0 || int(requests.Load()) != stats.Total || strings.Count(got, "mbcache:") != stats.Total {
		t.Fatalf("approved pass stats=%+v requests=%d: %s", stats, requests.Load(), got)
	}
}

func TestRemoteImageBannerCopy(t *testing.T) {
	tests := []struct {
		name    string
		stats   remoteImageStats
		title   string
		button  string
		loadAll bool
	}{
		{name: "blocked", stats: remoteImageStats{Blocked: 2}, title: "2 external images blocked for privacy", button: "Show images"},
		{name: "unavailable", stats: remoteImageStats{Unavailable: 1}, title: "1 external image unavailable", button: "Retry"},
		{name: "large set", stats: remoteImageStats{Total: 21, Deferred: 21}, title: "This message contains 21 external images", button: "Load images", loadAll: true},
		{name: "none", stats: remoteImageStats{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, button, loadAll := remoteImageBannerCopy(tt.stats)
			if title != tt.title || button != tt.button || loadAll != tt.loadAll {
				t.Fatalf("got %q, %q, %v; want %q, %q, %v", title, button, loadAll, tt.title, tt.button, tt.loadAll)
			}
		})
	}
}
