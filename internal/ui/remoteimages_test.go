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

// The render must name every external image without fetching any of it — that
// is what keeps the download off the path to the first paint — and the key it
// names has to be the key the cache stores the image under, or the image would
// never resolve once serveRemoteImage has fetched it.
func TestResolveRemoteImagesNamesCacheKeysWithoutFetching(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nimage"))
	}))
	defer srv.Close()
	w := &window{remoteImages: remotecache.NewWithClient(t.TempDir(), srv.Client())}
	imageURL := srv.URL + "/hero.png"
	source := `<img src="` + imageURL + `" alt="hero"><div style="background:url('` + imageURL + `')">x</div>`

	got, stats, pending := w.resolveRemoteImages(source, true, false)
	if requests.Load() != 0 {
		t.Fatalf("resolving made %d requests; the fetch belongs to the scheme handler", requests.Load())
	}
	if stats.Total != 1 || stats.Cached != 0 || stats.Unavailable != 0 || stats.Blocked != 0 || stats.Deferred != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if strings.Contains(got, srv.URL) || strings.Count(got, "mbcache:") != 2 {
		t.Fatalf("references not rewritten: %s", got)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %v, want the one URL the handler fetches", pending)
	}
	var namedKey string
	for key, raw := range pending {
		namedKey = key
		if raw != imageURL {
			t.Fatalf("pending[%q] = %q, want %q", key, raw, imageURL)
		}
	}
	if !strings.Contains(got, "mbcache:"+namedKey) {
		t.Fatalf("document names a different key than the handler fetches: %s", got)
	}

	// Fetch it the way serveRemoteImage would, then resolve again: the same key
	// must now be on disk, so the next open serves it without a request.
	entry, ok, err := w.remoteImages.Get(context.Background(), imageURL, true)
	if err != nil || !ok {
		t.Fatalf("fetch: ok=%v err=%v", ok, err)
	}
	if entry.Key != namedKey {
		t.Fatalf("fetched key %q, document named %q", entry.Key, namedKey)
	}
	if _, stats, _ = w.resolveRemoteImages(source, true, false); stats.Cached != 1 {
		t.Fatalf("cached image not recognised: %+v", stats)
	}
}

func TestResolveRemoteImagesNeverNamesNonImageCSSResources(t *testing.T) {
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

	got, stats, pending := w.resolveRemoteImages(source, true, false)
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("resolving fetched %v", requests)
	}
	if stats.Total != 1 || len(pending) != 1 {
		t.Fatalf("stats=%+v pending=%v, want only the background image", stats, pending)
	}
	for _, path := range []string{"/import.css", "/font.woff2", "/cursor.png"} {
		if strings.Contains(got, srv.URL+path) {
			t.Fatalf("non-image CSS resource %s survived: %s", path, got)
		}
		for _, raw := range pending {
			if strings.HasSuffix(raw, path) {
				t.Fatalf("non-image CSS resource %s was named as fetchable", path)
			}
		}
	}
}

func TestResolveRemoteImagesRemovesBlockedURLs(t *testing.T) {
	w := &window{remoteImages: remotecache.NewWithClient(t.TempDir(), http.DefaultClient)}
	source := `<img src="https://tracking.example/pixel.png"><div style="background:url(https://tracking.example/bg.png)">x</div>`

	got, stats, pending := w.resolveRemoteImages(source, false, false)
	if stats.Cached != 0 || stats.Blocked != 2 || len(pending) != 0 {
		t.Fatalf("stats=%+v pending=%v", stats, pending)
	}
	if strings.Contains(got, "http") || strings.Contains(got, "src=") || strings.Contains(got, "mbcache:") {
		t.Fatalf("blocked external URLs survived: %s", got)
	}
}

func TestResolveRemoteImagesRequiresApprovalForLargeSets(t *testing.T) {
	w := &window{remoteImages: remotecache.NewWithClient(t.TempDir(), http.DefaultClient)}
	var source strings.Builder
	for i := range remoteImagePromptThreshold + 1 {
		fmt.Fprintf(&source, `<img src="https://images.example/%d.png">`, i)
	}

	got, stats, pending := w.resolveRemoteImages(source.String(), true, false)
	if stats.Total != remoteImagePromptThreshold+1 || stats.Deferred != stats.Total || len(pending) != 0 || strings.Contains(got, "mbcache:") {
		t.Fatalf("unapproved pass stats=%+v pending=%d: %s", stats, len(pending), got)
	}

	got, stats, pending = w.resolveRemoteImages(source.String(), true, true)
	if stats.Deferred != 0 || stats.Unavailable != 0 || len(pending) != stats.Total || strings.Count(got, "mbcache:") != stats.Total {
		t.Fatalf("approved pass stats=%+v pending=%d: %s", stats, len(pending), got)
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
