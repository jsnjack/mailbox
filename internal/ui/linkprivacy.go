package ui

import (
	"net/url"
	"strings"
)

const maxLinkUnwraps = 3

// linkCleanStats describes the privacy-only changes made before handing an
// email link to the user's browser.
type linkCleanStats struct {
	Unwrapped      int
	StrippedParams int
}

// cleanExternalLink removes well-known, non-functional campaign identifiers
// and bypasses obvious redirect wrappers when their destination is present in
// the URL itself. It never contacts the wrapper: an opaque or ambiguous link is
// returned unchanged apart from safe parameter stripping.
func cleanExternalLink(raw string) (string, linkCleanStats) {
	original := raw
	current := strings.TrimSpace(raw)
	var stats linkCleanStats

	for stats.Unwrapped < maxLinkUnwraps {
		wrapper, ok := parseHTTPLink(current)
		if !ok {
			return original, linkCleanStats{}
		}
		destination, ok := explicitLinkDestination(wrapper)
		if !ok || !likelyRedirectWrapper(wrapper, destination) {
			break
		}
		if destination.String() == wrapper.String() {
			break
		}
		current = destination.String()
		stats.Unwrapped++
	}

	target, ok := parseHTTPLink(current)
	if !ok {
		return original, linkCleanStats{}
	}
	stats.StrippedParams = stripTrackingParams(target)
	if stats.Unwrapped == 0 && stats.StrippedParams == 0 {
		return original, stats
	}
	return target.String(), stats
}

func parseHTTPLink(raw string) (*url.URL, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, false
	}
	return u, true
}

// explicitLinkDestination returns the one unambiguous absolute HTTP(S)
// destination carried by a common redirect parameter. Different destinations
// in the same wrapper are deliberately treated as ambiguous.
func explicitLinkDestination(wrapper *url.URL) (*url.URL, bool) {
	values, err := url.ParseQuery(wrapper.RawQuery)
	if err != nil {
		return nil, false
	}
	var found *url.URL
	for key, candidates := range values {
		if !isDestinationParam(key) {
			continue
		}
		for _, candidate := range candidates {
			destination, ok := decodeHTTPDestination(candidate)
			if !ok {
				continue
			}
			if found != nil && found.String() != destination.String() {
				return nil, false
			}
			found = destination
		}
	}
	return found, found != nil
}

func decodeHTTPDestination(raw string) (*url.URL, bool) {
	candidate := strings.TrimSpace(raw)
	for range 3 {
		if destination, ok := parseHTTPLink(candidate); ok {
			return destination, true
		}
		decoded, err := url.QueryUnescape(candidate)
		if err != nil || decoded == candidate {
			break
		}
		candidate = strings.TrimSpace(decoded)
	}
	return nil, false
}

func isDestinationParam(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "url", "u", "q", "target", "dest", "destination", "redirect",
		"redirect_url", "redirecturl", "redirect_to", "redirectto", "out", "to":
		return true
	default:
		return false
	}
}

// likelyRedirectWrapper keeps automatic unwrapping conservative. A URL-shaped
// query value alone is not enough: login and OAuth endpoints commonly carry
// callback URLs. The wrapper must also advertise link/redirect behavior in its
// host or path. Same-site redirects require the stronger host signal so a
// normal /login/redirect flow is not bypassed.
func likelyRedirectWrapper(wrapper, destination *url.URL) bool {
	hostSignal := redirectHostSignal(wrapper.Hostname())
	if sameRegistrableDomain(wrapper.Hostname(), destination.Hostname()) {
		return hostSignal
	}
	return hostSignal || redirectPathSignal(wrapper.Path)
}

func redirectHostSignal(host string) bool {
	for _, label := range strings.Split(strings.ToLower(host), ".") {
		switch label {
		case "click", "clicks", "link", "links", "redirect", "redirects",
			"track", "tracking", "safelink", "safelinks", "urldefense":
			return true
		}
	}
	return false
}

func redirectPathSignal(path string) bool {
	for _, segment := range strings.Split(strings.ToLower(path), "/") {
		segment = strings.TrimSuffix(segment, ".php")
		segment = strings.TrimSuffix(segment, ".aspx")
		switch segment {
		case "click", "link", "redirect", "redir", "track", "out", "away", "url", "l":
			return true
		}
	}
	return false
}

func stripTrackingParams(target *url.URL) int {
	if target.RawQuery == "" {
		return 0
	}
	values, err := url.ParseQuery(target.RawQuery)
	if err != nil {
		return 0 // preserve malformed or unusual application queries verbatim
	}
	removed := 0
	for key, entries := range values {
		if !isTrackingParam(key) {
			continue
		}
		if len(entries) == 0 {
			removed++
		} else {
			removed += len(entries)
		}
		delete(values, key)
	}
	if removed > 0 {
		target.RawQuery = values.Encode()
	}
	return removed
}

func isTrackingParam(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if strings.HasPrefix(key, "utm_") {
		return true
	}
	switch key {
	case "fbclid", "gclid", "dclid", "gbraid", "wbraid", "msclkid",
		"mc_cid", "mc_eid", "mkt_tok", "_hsenc", "_hsmi":
		return true
	default:
		return false
	}
}
