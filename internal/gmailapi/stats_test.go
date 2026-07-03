package gmailapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// doVia issues a GET to url through a countingTransport with the given stall
// bound and returns the response error and, when the request succeeded, the
// body-read result.
func doVia(t *testing.T, url string, stall time.Duration) ([]byte, error) {
	t.Helper()
	client := &http.Client{Transport: &countingTransport{base: http.DefaultTransport, stats: &Stats{}, stall: stall}}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

// A server that never responds must fail in bounded time with a cancellation
// (non-retryable, like the old whole-request deadline) — the "switching emails
// while offline hangs the reader" case.
func TestStallWatchdogCancelsSilentServer(t *testing.T) {
	hangCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hangCh // accept the connection, never respond
	}))
	// Unblock the handler BEFORE srv.Close() (defers run LIFO) — Close waits
	// for in-flight handlers, so the reverse order deadlocks the test.
	defer srv.Close()
	defer close(hangCh)

	start := time.Now()
	_, err := doVia(t, srv.URL, 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected the stall watchdog to fail the hanging request, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a context.Canceled wrap (non-retryable)", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("request took %v — watchdog didn't bound it", elapsed)
	}
}

// A response that stalls mid-body (headers + one chunk, then silence) must be
// cancelled too — progress before the stall doesn't excuse a dead connection.
func TestStallWatchdogCancelsMidBodyStall(t *testing.T) {
	hangCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first chunk"))
		w.(http.Flusher).Flush()
		<-hangCh // then go silent
	}))
	// Unblock the handler before Close — see TestStallWatchdogCancelsSilentServer.
	defer srv.Close()
	defer close(hangCh)

	_, err := doVia(t, srv.URL, 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected the stall watchdog to fail the mid-body stall, got nil")
	}
}

// A slow transfer that keeps moving bytes must NOT be cut off, even when it
// takes far longer than the stall bound — the whole point of replacing the
// whole-response timeout (which killed large attachments on slow links).
func TestStallWatchdogAllowsSlowProgress(t *testing.T) {
	const chunks = 8
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for i := 0; i < chunks; i++ {
			_, _ = w.Write([]byte("chunk"))
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond) // total 800ms ≫ 300ms stall bound
		}
	}))
	defer srv.Close()

	body, err := doVia(t, srv.URL, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("slow-but-progressing transfer was cut off: %v", err)
	}
	if len(body) != chunks*len("chunk") {
		t.Fatalf("read %d bytes, want %d", len(body), chunks*len("chunk"))
	}
}

// The transport still counts response bytes into Stats.
func TestCountingTransportCountsBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("0123456789"))
	}))
	defer srv.Close()

	stats := &Stats{}
	client := &http.Client{Transport: &countingTransport{base: http.DefaultTransport, stats: stats}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if len(b) != 10 {
		t.Fatalf("read %d bytes, want 10", len(b))
	}
	if got := stats.Snapshot().BytesIn; got != 10 {
		t.Fatalf("BytesIn = %d, want 10", got)
	}
}
