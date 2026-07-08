package gmailapi

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jsnjack/mailbox/internal/httpclient"
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
	// No httpClient.Timeout: that caps the WHOLE response (upload + headers +
	// body), which deterministically kills large attachment transfers on slow
	// links. Dropped connections are instead bounded by the transport's
	// no-progress watchdog (netStallTimeout) — see countingTransport.RoundTrip.
	// option.WithUserAgent is incompatible with option.WithHTTPClient (used
	// below), so the header is added via RoundTripper middleware instead.
	oauthTransport := httpClient.Transport
	httpClient.Transport = &countingTransport{
		base:  &httpclient.Transport{Base: oauthTransport},
		stats: stats, stall: netStallTimeout,
	}
	srv, err := gmail.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		logging.TraceContext(ctx, "gmailapi: newService failed", "err", err)
		return nil, err
	}
	logging.TraceContext(ctx, "gmailapi: newService done")
	return srv, nil
}

// netStallTimeout bounds how long a request may go with NO progress — no
// request-body bytes sent, no response headers, no response-body bytes — before
// it is cancelled. Unlike a whole-response timeout, any progress resets the
// clock, so a 25 MB attachment on a slow link transfers fine while a dropped
// connection (no FIN/RST received) still fails in bounded time. The resulting
// error wraps context.Canceled, which isRetryable treats as non-retryable —
// the call fails immediately instead of looping.
const netStallTimeout = 2 * time.Minute

// countingTransport tallies request and response bytes into Stats and bounds
// each request with a no-progress watchdog (see netStallTimeout).
type countingTransport struct {
	base  http.RoundTripper
	stats *Stats
	stall time.Duration // no-progress bound; tests shrink it
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.ContentLength > 0 {
		t.stats.bytesOut.Add(req.ContentLength)
	}
	stall := t.stall
	if stall <= 0 {
		stall = netStallTimeout
	}
	// The watchdog cancels the request when the stall timer fires; every unit of
	// progress (an upload chunk read from req.Body, headers arriving, a body
	// read) pushes it back.
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	watchdog := time.AfterFunc(stall, cancel)
	if req.Body != nil {
		req.Body = &progressBody{rc: req.Body, watchdog: watchdog, stall: stall}
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		watchdog.Stop()
		cancel()
		return nil, err
	}
	watchdog.Reset(stall) // headers arrived; re-arm for the body phase
	resp.Body = &countingBody{rc: resp.Body, n: &t.stats.bytesIn, watchdog: watchdog, stall: stall, cancel: cancel}
	return resp, nil
}

// progressBody re-arms the stall watchdog as the transport reads the request
// body, so a long upload that is moving bytes never trips it.
type progressBody struct {
	rc       io.ReadCloser
	watchdog *time.Timer
	stall    time.Duration
}

func (b *progressBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.watchdog.Reset(b.stall)
	}
	return n, err
}

func (b *progressBody) Close() error { return b.rc.Close() }

// countingBody adds bytes read from a response body into a counter, re-arms the
// stall watchdog on every read, and releases the watchdog + request context on
// Close (cancel after the body is done is harmless — the connection was already
// handed back to the pool at EOF).
type countingBody struct {
	rc       io.ReadCloser
	n        *atomic.Int64
	watchdog *time.Timer
	stall    time.Duration
	cancel   context.CancelFunc
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.n.Add(int64(n))
		b.watchdog.Reset(b.stall)
	}
	return n, err
}

func (b *countingBody) Close() error {
	b.watchdog.Stop()
	b.cancel()
	return b.rc.Close()
}
