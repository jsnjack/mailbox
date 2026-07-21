// Package httpclient gives every outbound HTTP request this app makes (Gmail
// API, OAuth token refresh/exchange, AI providers, unsubscribe POSTs) a common
// User-Agent identity, so a server operator can tell mailbox's traffic apart
// from a generic Go HTTP client.
package httpclient

import "net/http"

// UserAgent is set once at startup (cmd/mailbox, from the build-time version)
// before any outbound request fires. Left as a bare "mailbox" in tests and any
// other context that never sets it.
var UserAgent = "mailbox"

// Transport sets User-Agent on every request that doesn't already carry one (a
// caller-set header always wins), then delegates to Base (a shared BaseTransport
// if nil — see dial.go for why that, and not the raw http.DefaultTransport, is
// the safe default).
type Transport struct {
	Base http.RoundTripper
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", UserAgent)
	}
	base := t.Base
	if base == nil {
		base = sharedBase
	}
	return base.RoundTrip(req)
}
