package model

import (
	"net/url"
	"regexp"
	"strings"
)

// cidRefRe finds the cid: URLs a message body points at.
var cidRefRe = regexp.MustCompile(`(?i)cid:([^"'\s>)]+)`)

// ReferencedCIDs collects the Content-IDs a message body actually references,
// so a part carrying one can be told apart from an image the body displays in
// place. Carrying a Content-ID is not enough on its own to decide: forwarding a
// message makes Gmail stamp one on every part, so a forwarded PDF arrives with
// a Content-ID nothing points at — and treating that as inline hides the
// attachment, which the provider's own web UI lists.
//
// Values are lower-cased for comparison and lose their angle brackets, matching
// how a Content-ID header is stored; percent-decoding is tolerated because the
// reference travels through a URL.
func ReferencedCIDs(html string) map[string]bool {
	out := map[string]bool{}
	for _, m := range cidRefRe.FindAllStringSubmatch(html, -1) {
		v := m[1]
		if dec, err := url.QueryUnescape(v); err == nil {
			v = dec
		}
		if v = strings.Trim(v, "<>"); v != "" {
			out[strings.ToLower(v)] = true
		}
	}
	return out
}

// IsInlineAttachment reports whether a is an image the body shows in place
// rather than a file offered for download.
func IsInlineAttachment(a Attachment, referenced map[string]bool) bool {
	return a.ContentID != "" && referenced[strings.ToLower(a.ContentID)]
}
