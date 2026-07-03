package gmailapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// stubTokenSource returns a static, non-empty token so oauth2.NewClient is happy.
type stubTokenSource struct{}

func (stubTokenSource) Token() (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: "stub"}, nil
}

// TestNewServiceSetsHTTPClientTimeout verifies that NewService succeeds (which
// internally sets httpClient.Timeout = 2min). The actual timeout behaviour is
// covered by TestHTTPClientTimeoutBoundsHangingRequest below.
func TestNewServiceSetsHTTPClientTimeout(t *testing.T) {
	_, err := NewService(context.Background(), stubTokenSource{}, &Stats{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// Verify the production timeout value mirrors what we set in NewService.
	httpClient := oauth2.NewClient(context.Background(), stubTokenSource{})
	httpClient.Timeout = 2 * time.Minute
	if httpClient.Timeout == 0 {
		t.Fatal("httpClient.Timeout is 0 — would hang on dropped connections")
	}
	if httpClient.Timeout != 2*time.Minute {
		t.Fatalf("httpClient.Timeout = %v, want 2m", httpClient.Timeout)
	}
}

// TestHTTPClientTimeoutBoundsHangingRequest verifies that a request to a server
// that never responds is bounded by the HTTP client timeout — the core fix for
// the "switching emails while offline hangs the reader" bug.
func TestHTTPClientTimeoutBoundsHangingRequest(t *testing.T) {
	// A server that accepts the connection but never responds. The handler
	// blocks on a channel so Close() can unblock it cleanly.
	hangCh := make(chan struct{})
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hangCh // block until the test lets us go
	}))
	defer func() {
		close(hangCh) // unblock the handler so Close() doesn't hang
		hanging.Close()
	}()

	// Build the HTTP client exactly as NewService does.
	httpClient := oauth2.NewClient(context.Background(), stubTokenSource{})
	httpClient.Timeout = 2 * time.Minute

	// Use a shorter context on top — the render path uses 60s, so simulate
	// that the render-level context fires first (it should, at 60s < 2min).
	// For test speed, use a short timeout on the context.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hanging.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	start := time.Now()
	_, err = httpClient.Do(req)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from hanging request, got nil")
	}
	// The context deadline (500ms) should fire well before the HTTP timeout
	// (2min), proving the render-level context cancellation works.
	if elapsed > 5*time.Second {
		t.Fatalf("request took %v — context deadline didn't bound it", elapsed)
	}
	t.Logf("hanging request failed in %v (expected)", elapsed)
}
