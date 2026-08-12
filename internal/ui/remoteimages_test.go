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

	got, stats, pending := w.resolveRemoteImages(source, true)
	if requests.Load() != 0 {
		t.Fatalf("resolving made %d requests; the fetch belongs to the scheme handler", requests.Load())
	}
	if stats.Total != 1 || stats.Blocked != 0 {
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

	// Fetch it the way serveRemoteImage would: it must land under exactly the
	// key the document named, or the image would never resolve.
	entry, ok, err := w.remoteImages.Get(context.Background(), imageURL, true)
	if err != nil || !ok {
		t.Fatalf("fetch: ok=%v err=%v", ok, err)
	}
	if entry.Key != namedKey {
		t.Fatalf("fetched key %q, document named %q", entry.Key, namedKey)
	}
	if _, ok := w.remoteImages.Open(namedKey); !ok {
		t.Fatal("named key does not resolve after the fetch")
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

	got, stats, pending := w.resolveRemoteImages(source, true)
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

	got, stats, pending := w.resolveRemoteImages(source, false)
	if stats.Blocked != 2 || len(pending) != 0 {
		t.Fatalf("stats=%+v pending=%v", stats, pending)
	}
	if strings.Contains(got, "http") || strings.Contains(got, "src=") || strings.Contains(got, "mbcache:") {
		t.Fatalf("blocked external URLs survived: %s", got)
	}
}

// An image-heavy newsletter is named in full and loads like any other: there is
// no count at which the reader stops asking for pictures, because the sender
// already knows the message was opened from the first one.
func TestResolveRemoteImagesNamesEveryImageOfALargeSet(t *testing.T) {
	w := &window{remoteImages: remotecache.NewWithClient(t.TempDir(), http.DefaultClient)}
	const many = 60
	var source strings.Builder
	for i := range many {
		fmt.Fprintf(&source, `<img src="https://images.example/%d.png">`, i)
	}

	got, stats, pending := w.resolveRemoteImages(source.String(), true)
	if stats.Total != many || stats.Blocked != 0 {
		t.Fatalf("stats=%+v, want %d total and none blocked", stats, many)
	}
	if len(pending) != many || strings.Count(got, "mbcache:") != many {
		t.Fatalf("pending=%d, mbcache refs=%d; want %d of each", len(pending), strings.Count(got, "mbcache:"), many)
	}
}

func TestRemoteImageBannerCopy(t *testing.T) {
	tests := []struct {
		name   string
		stats  remoteImageStats
		title  string
		button string
	}{
		{name: "blocked", stats: remoteImageStats{Blocked: 2}, title: "2 external images blocked for privacy", button: "Show images"},
		// A large set is not withheld any more, so it has nothing to say.
		{name: "large set", stats: remoteImageStats{Total: 60}},
		{name: "none", stats: remoteImageStats{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, button := remoteImageBannerCopy(tt.stats)
			if title != tt.title || button != tt.button {
				t.Fatalf("got %q, %q; want %q, %q", title, button, tt.title, tt.button)
			}
		})
	}
}
