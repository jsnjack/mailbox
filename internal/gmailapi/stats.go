package gmailapi

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
	"golang.org/x/oauth2"
	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// Stats accumulates per-account API usage for the status bar: request count,
// quota units spent, and bytes transferred. All fields are updated atomically
// from concurrent workers and read via Snapshot.
type Stats struct {
	requests   atomic.Int64
	quotaUnits atomic.Int64
	bytesIn    atomic.Int64
	bytesOut   atomic.Int64
}

// StatsSnapshot is a plain-value copy of the counters at a point in time.
type StatsSnapshot struct {
	Requests   int64
	QuotaUnits int64
	BytesIn    int64
	BytesOut   int64
}

// Snapshot reads the current counters.
func (s *Stats) Snapshot() StatsSnapshot {
	return StatsSnapshot{
		Requests:   s.requests.Load(),
		QuotaUnits: s.quotaUnits.Load(),
		BytesIn:    s.bytesIn.Load(),
		BytesOut:   s.bytesOut.Load(),
	}
}

// Stats returns a snapshot of this client's cumulative API usage.
func (c *Client) Stats() StatsSnapshot { return c.stats.Snapshot() }

// NewService builds a Gmail service authenticated by ts whose HTTP transport
// counts bytes in/out into stats. Pair it with NewClientStats(srv, stats) so the
// request/quota counters land in the same Stats.
func NewService(ctx context.Context, ts oauth2.TokenSource, stats *Stats) (*gmail.Service, error) {
	logging.TraceContext(ctx, "gmailapi: newService", "token_source", ts != nil)
	httpClient := oauth2.NewClient(ctx, ts) // an *http.Client whose Transport adds auth
	// Safety net: cap every individual HTTP request so a dropped connection (no
	// FIN/RST received) can't block a caller indefinitely. A normal Gmail API
	// call finishes in seconds; 2 minutes is generous yet bounded. When the
	// deadline fires the error wraps context.DeadlineExceeded, which isRetryable
	// treats as non-retryable — the call fails immediately instead of looping.
	httpClient.Timeout = 2 * time.Minute
	httpClient.Transport = &countingTransport{base: httpClient.Transport, stats: stats}
	srv, err := gmail.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		logging.TraceContext(ctx, "gmailapi: newService failed", "err", err)
		return nil, err
	}
	logging.TraceContext(ctx, "gmailapi: newService done")
	return srv, nil
}

// countingTransport tallies request and response bytes into Stats.
type countingTransport struct {
	base  http.RoundTripper
	stats *Stats
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.ContentLength > 0 {
		t.stats.bytesOut.Add(req.ContentLength)
	}
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp != nil && resp.Body != nil {
		resp.Body = &countingBody{rc: resp.Body, n: &t.stats.bytesIn}
	}
	return resp, err
}

// countingBody adds bytes read from a response body into a counter.
type countingBody struct {
	rc io.ReadCloser
	n  *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.n.Add(int64(n))
	}
	return n, err
}

func (b *countingBody) Close() error { return b.rc.Close() }
