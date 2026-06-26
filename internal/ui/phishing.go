package ui

import (
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/publicsuffix"

	"github.com/jsnjack/mailbox/internal/model"
)

// emailInName matches an email address embedded in a display name.
var emailInName = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// bareHost matches a token that looks like a hostname (label.label…TLD).
var bareHost = regexp.MustCompile(`^[A-Za-z0-9\-]+(\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}$`)

// phishingWarnings runs cheap, deterministic anti-phishing heuristics over a
// message and its HTML body, returning human-readable cautions. These complement
// (don't replace) the SPF/DKIM/DMARC verdict — they catch deception that can pass
// authentication.
func phishingWarnings(m model.Message, htmlBody string) []string {
	var out []string
	if senderNameSpoofed(m.FromName, m.FromAddr) {
		out = append(out, "The sender's name looks like a different address than who actually sent it.")
	}
	if hasDeceptiveLink(htmlBody) {
		out = append(out, "A link's text shows a different site than where it actually goes.")
	}
	return out
}

// senderNameSpoofed reports whether the display name embeds an email address
// whose domain differs from the real sender's — the classic
// `From: "security@paypal.com" <attacker@evil.example>` trick.
func senderNameSpoofed(fromName, fromAddr string) bool {
	embedded := emailInName.FindString(fromName)
	if embedded == "" {
		return false
	}
	nameDom := domainOf(embedded)
	addrDom := domainOf(fromAddr)
	if nameDom == "" || addrDom == "" {
		return false
	}
	return !sameRegistrableDomain(nameDom, addrDom)
}

// hasDeceptiveLink reports whether any anchor's visible text presents a hostname
// that resolves to a different registrable domain than its href.
func hasDeceptiveLink(htmlBody string) bool {
	if strings.TrimSpace(htmlBody) == "" {
		return false
	}
	doc, err := html.Parse(strings.NewReader(htmlBody))
	if err != nil {
		return false
	}
	found := false
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if found {
			return
		}
		if n.Type == html.ElementNode && n.Data == "a" {
			if linkTextMismatch(textContent(n), attrValue(n, "href")) {
				found = true
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}

// linkTextMismatch reports whether the visible link text looks like a URL/host
// that differs (by registrable domain) from where the href actually points. Only
// fires when the text itself looks like a destination, keeping false positives
// low (plain "click here" text never triggers it).
func linkTextMismatch(text, href string) bool {
	hrefHost := hostOfURL(href)
	if hrefHost == "" {
		return false // non-navigable href (mailto:, anchor, …)
	}
	textHost := hostFromText(text)
	if textHost == "" {
		return false // the text isn't presenting a destination
	}
	return !sameRegistrableDomain(textHost, hrefHost)
}

// hostFromText extracts a hostname when the visible text *is* a URL or bare
// domain; otherwise returns "".
func hostFromText(text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return ""
	}
	if strings.Contains(t, "://") {
		return hostOfURL(t)
	}
	// Bare domain like "paypal.com" or "paypal.com/login": take the first token.
	if i := strings.IndexAny(t, "/ \t\r\n"); i >= 0 {
		t = t[:i]
	}
	t = strings.ToLower(strings.TrimPrefix(t, "www."))
	if bareHost.MatchString(t) {
		return t
	}
	return ""
}

// hostOfURL returns the lowercase host of an http(s) URL, or "".
func hostOfURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(u.Hostname(), "www."))
}

// domainOf returns the lowercase domain part of an email address.
func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return strings.ToLower(strings.TrimSpace(addr[i+1:]))
	}
	return ""
}

// sameRegistrableDomain compares two hosts at the registrable-domain level
// (so mail.example.com and links.example.com count as the same site).
func sameRegistrableDomain(a, b string) bool {
	ra, rb := registrableDomain(a), registrableDomain(b)
	return ra != "" && ra == rb
}

func registrableDomain(host string) string {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "www."), "."))
	if host == "" {
		return ""
	}
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	return host
}

// textContent returns the concatenated visible text of a node's subtree.
func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// attrValue returns the value of an element's attribute, or "".
func attrValue(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}
