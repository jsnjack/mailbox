package httpclient

import (
	"net/http"
	"testing"
	"time"
)

// TestDialerPrefersGoResolver guards against silently regressing to the cgo
// resolver (see dial.go's doc comment for why that matters).
func TestDialerPrefersGoResolver(t *testing.T) {
	d := Dialer(5 * time.Second)
	if d.Resolver == nil || !d.Resolver.PreferGo {
		t.Fatalf("Dialer: Resolver.PreferGo = %v, want true", d.Resolver)
	}
	if d.Timeout != 5*time.Second {
		t.Fatalf("Dialer: Timeout=%v, want 5s", d.Timeout)
	}
	if !d.KeepAliveConfig.Enable || d.KeepAliveConfig.Idle != 10*time.Second || d.KeepAliveConfig.Count != 3 {
		t.Fatalf("Dialer: KeepAliveConfig = %+v, want an enabled fast-detecting config", d.KeepAliveConfig)
	}
}

func TestBaseTransportUsesGoResolverDialer(t *testing.T) {
	tr := BaseTransport()
	if tr.DialContext == nil {
		t.Fatal("BaseTransport: DialContext is nil")
	}
	if tr == http.DefaultTransport {
		t.Fatal("BaseTransport must return its own clone, not the shared http.DefaultTransport")
	}
	// A non-nil DialContext otherwise conservatively disables HTTP/2 (see
	// http.Transport.ForceAttemptHTTP2) — without this, the ping health check
	// below would silently never run.
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("BaseTransport: ForceAttemptHTTP2 = false, want true (else HTTP/2 — and its ping health check — never activates alongside a custom DialContext)")
	}
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout == 0 {
		t.Fatal("BaseTransport: HTTP2.SendPingTimeout not set, want a proactive idle-connection health check")
	}
}

// TestTransportFallsBackToSafeSharedBase ensures a Transport built with no
// explicit Base (the common case at every call site) doesn't silently fall
// through to the raw http.DefaultTransport.
func TestTransportFallsBackToSafeSharedBase(t *testing.T) {
	if sharedBase == nil || sharedBase == http.DefaultTransport {
		t.Fatalf("sharedBase must be our own safe clone, not the raw http.DefaultTransport")
	}
	if sharedBase.DialContext == nil {
		t.Fatal("sharedBase: DialContext is nil")
	}
}
