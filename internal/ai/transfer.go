package ai

import (
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// transferCounter tallies HTTP bytes in/out for an AI provider, so the status
// bar can report data transferred for AI calls alongside Gmail traffic.
type transferCounter struct {
	in  atomic.Int64
	out atomic.Int64
}

func (c *transferCounter) snapshot() (in, out int64) { return c.in.Load(), c.out.Load() }

// countingClient returns an HTTP client whose transport tallies bytes into c.
func countingClient(timeout time.Duration, c *transferCounter) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &countingTransport{base: http.DefaultTransport, c: c},
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

// Transferred returns the bytes received/sent by the assistant's provider so
// far (0,0 when the provider doesn't track it).
func (a *Assistant) Transferred() (in, out int64) {
	if r, ok := a.provider().(interface{ transfer() (int64, int64) }); ok {
		return r.transfer()
	}
	return 0, 0
}
