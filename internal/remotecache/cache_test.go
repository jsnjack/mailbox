package remotecache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestCacheDownloadsOnceThenWorksOffline(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nimage"))
	}))
	defer srv.Close()
	c := NewWithClient(t.TempDir(), srv.Client())
	first, ok, err := c.Get(context.Background(), srv.URL+"/hero.png#fragment", true)
	if err != nil || !ok {
		t.Fatalf("first Get = %+v, %v, %v", first, ok, err)
	}
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	second, ok, err := c.Get(context.Background(), srv.URL+"/hero.png", false)
	if err != nil || !ok || second.Key != first.Key || requests != 1 {
		t.Fatalf("offline Get = %+v, %v, %v requests=%d", second, ok, err, requests)
	}
}

func TestCacheRejectsHTMLDisguisedAsImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("<!doctype html><script>alert(1)</script>"))
	}))
	defer srv.Close()
	c := NewWithClient(t.TempDir(), srv.Client())
	if _, ok, err := c.Get(context.Background(), srv.URL, true); err == nil || ok {
		t.Fatalf("disguised HTML accepted: ok=%v err=%v", ok, err)
	}
}

// A caller names an image by key before the cache holds it, so Key must agree
// with Get about where a URL lands — including the normalization Get applies.
// If these ever drift, every image resolves to a key nothing was stored under.
func TestKeyMatchesWhereGetStoresTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nimage"))
	}))
	defer srv.Close()
	c := NewWithClient(t.TempDir(), srv.Client())

	key, err := Key(srv.URL + "/hero.png#fragment")
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if _, ok := c.Open(key); ok {
		t.Fatal("key resolved before anything was fetched")
	}
	entry, ok, err := c.Get(context.Background(), srv.URL+"/hero.png", true)
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	if entry.Key != key {
		t.Fatalf("Get stored under %q, Key named %q", entry.Key, key)
	}
	if _, ok := c.Open(key); !ok {
		t.Fatal("named key does not resolve after the fetch")
	}
}

func TestKeyRejectsURLsThatCouldNotBeFetched(t *testing.T) {
	for _, raw := range []string{"", "not a url", "ftp://example.com/x.png", "https://user:pw@example.com/x.png", "/relative.png"} {
		if _, err := Key(raw); err == nil {
			t.Fatalf("Key(%q) succeeded; an unfetchable URL must not be named", raw)
		}
	}
}
