package remotecache

import (
	"context"
	"net"
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

func TestBlockedIP(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.1.2.3", "169.254.1.1", "::1", "100.64.0.1", "192.0.2.1"} {
		if !blockedIP(net.ParseIP(raw)) {
			t.Errorf("%s was not blocked", raw)
		}
	}
	if blockedIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("public address was blocked")
	}
}
