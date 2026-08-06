package httpclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// PublicTransport returns the standard resilient transport with a DNS-pinned
// dial path that rejects private, loopback, link-local, multicast, carrier-NAT,
// and documentation addresses. It is for URLs sourced from untrusted email
// content (external images and one-click unsubscribe), preventing SSRF and DNS
// rebinding. Proxies are disabled because a proxy could otherwise fetch a
// private redirect on the application's behalf.
func PublicTransport(connectTimeout time.Duration) *http.Transport {
	tr := BaseTransport()
	tr.Proxy = nil
	dialer := Dialer(connectTimeout)
	resolver := goResolver
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("host resolved to no addresses")
		}
		for _, candidate := range ips {
			if IsPrivateDestination(candidate.IP) {
				return nil, fmt.Errorf("destination resolves to a private address")
			}
		}
		var dialErr error
		for _, candidate := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			dialErr = errors.Join(dialErr, err)
		}
		return nil, dialErr
	}
	return tr
}

// IsPrivateDestination reports whether an IP is unsuitable for an untrusted
// outbound URL.
func IsPrivateDestination(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	for _, cidr := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"2001:2::/48", "2001:10::/28", "2001:db8::/32",
	} {
		_, network, _ := net.ParseCIDR(cidr)
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
