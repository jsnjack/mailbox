package ai

import (
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jsnjack/mailbox/internal/httpclient"
)

// aiDialTimeout bounds the TCP connect to an AI endpoint. A VPN-only endpoint
// reached while off VPN can blackhole the SYN instead of refusing it; without a
// short dial bound every request would hang for the OS connect timeout (tens of
// seconds) before the failover chain could move to the next model.
const aiDialTimeout = 5 * time.Second

// transferCounter tallies HTTP bytes in/out for an AI provider, so the status
// bar can report data transferred for AI calls alongside Gmail traffic.
type transferCounter struct {
	in  atomic.Int64
	out atomic.Int64
}

func (c *transferCounter) snapshot() (in, out int64) { return c.in.Load(), c.out.Load() }

// countingClient returns an HTTP client whose transport tallies bytes into c
// and identifies the app via User-Agent, with a short dial bound (see
// aiDialTimeout).
func countingClient(timeout time.Duration, c *transferCounter) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: aiDialTimeout, KeepAlive: 30 * time.Second}).DialContext
	return &http.Client{
		Timeout:   timeout,
		Transport: &countingTransport{base: &httpclient.Transport{Base: tr}, c: c},
	}
}

type countingTransport struct {
	base http.RoundTripper
	c    *transferCounter
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.ContentLength > 0 {
		t.c.out.Add(req.ContentLength)
	}
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp != nil && resp.Body != nil {
		resp.Body = &countingBody{rc: resp.Body, n: &t.c.in}
	}
	return resp, err
}

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

// Transferred returns the bytes received/sent by AI providers this session:
// the current provider's counters plus the baseline rolled over from providers
// swapped out by a live settings change.
func (a *Assistant) Transferred() (in, out int64) {
	in, out = a.baseIn.Load(), a.baseOut.Load()
	if r, ok := a.provider().(interface{ transfer() (int64, int64) }); ok {
		i, o := r.transfer()
		in += i
		out += o
	}
	return in, out
}
