package httpclient

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestIsPrivateDestination(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.1.2.3", "169.254.1.1", "::1", "100.64.0.1", "192.0.2.1"} {
		if !IsPrivateDestination(net.ParseIP(raw)) {
			t.Errorf("%s was not blocked", raw)
		}
	}
	if IsPrivateDestination(net.ParseIP("8.8.8.8")) {
		t.Fatal("public address was blocked")
	}
}

func TestPublicTransportRejectsLoopbackBeforeDial(t *testing.T) {
	tr := PublicTransport(time.Second)
	_, err := tr.DialContext(context.Background(), "tcp", "127.0.0.1:443")
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("loopback dial error = %v", err)
	}
}

func TestPublicTransportDisablesProxy(t *testing.T) {
	tr := PublicTransport(3 * time.Second)
	if tr.Proxy != nil || tr.DialContext == nil {
		t.Fatalf("public transport Proxy nil=%t DialContext nil=%t", tr.Proxy == nil, tr.DialContext == nil)
	}
}
