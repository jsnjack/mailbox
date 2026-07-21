package httpclient

import (
	"net"
	"net/http"
	"time"
)

// goResolver forces Go's pure-Go DNS resolver everywhere this package's
// dialers are used, instead of letting Go fall back to cgo (glibc's
// getaddrinfo). See Dialer and BaseTransport for why this matters.
var goResolver = &net.Resolver{PreferGo: true}

// tcpKeepAlive detects a half-open TCP connection — the failure mode of a
// VPN/Wi-Fi/LAN interface flap: the peer vanishes with no FIN/RST, so the
// socket looks fine until something tries to use it — far faster than the
// OS/Go default (idle ~15s, then ~9 unanswered probes: over two minutes). An
// unanswered probe every 10s, three misses, drops the connection in well
// under a minute so a blocked read/write fails fast instead of hanging until
// an application-level timeout notices.
var tcpKeepAlive = net.KeepAliveConfig{Enable: true, Idle: 10 * time.Second, Interval: 10 * time.Second, Count: 3}

// Dialer returns a *net.Dialer pinned to Go's own DNS resolver and an
// aggressive TCP keepalive (see BaseTransport and tcpKeepAlive), with the
// given connect timeout (zero for Go's default).
//
// On Linux, Go's net package silently switches to the cgo resolver whenever
// cgo is available and /etc/nsswitch.conf's "hosts:" line names an NSS module
// the pure-Go resolver can't emulate — e.g. Fedora/systemd-resolved ship
// "mdns4_minimal" and "resolve" by default. The cgo path blocks an OS thread
// inside glibc, outside the Go scheduler, and does not honor context
// cancellation: a lookup wedged by a network change (VPN up/down,
// suspend/resume, interface flap) makes the caller's context time out and
// return "context deadline exceeded" as usual, but the underlying OS thread
// and glibc/NSS resolver state are never freed — so later lookups can wedge
// too, with no application-level retry able to recover it short of a process
// restart. PreferGo keeps DNS resolution entirely inside the Go runtime,
// where a context deadline actually aborts the lookup.
func Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Resolver: goResolver, KeepAliveConfig: tcpKeepAlive}
}

// BaseTransport returns a clone of http.DefaultTransport whose dialer is
// pinned to Go's own DNS resolver and a fast-detecting keepalive (see
// Dialer) — the safe base for every outbound HTTP transport this app builds
// (Gmail API, OAuth refresh, AI providers, unsubscribe POSTs).
//
// It also force-enables HTTP/2 with a ping-based idle health check
// (SendPingTimeout): a pooled HTTP/2 connection left half-open by a network
// change is caught by an unanswered PING and evicted before it's handed to a
// new request, instead of that request hanging against a dead peer. A
// non-nil DialContext otherwise conservatively disables HTTP/2 on a Transport
// (see http.Transport.ForceAttemptHTTP2), which would silently drop this
// protection along with the resolver/keepalive fix above.
func BaseTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = Dialer(0).DialContext
	tr.ForceAttemptHTTP2 = true
	tr.HTTP2 = &http.HTTP2Config{SendPingTimeout: 10 * time.Second}
	return tr
}

// sharedBase is Transport's fallback when Base is nil — one safe, pooled
// transport reused across every such caller, computed once rather than per
// request (a fresh BaseTransport() per RoundTrip would defeat connection
// pooling entirely).
var sharedBase = BaseTransport()
