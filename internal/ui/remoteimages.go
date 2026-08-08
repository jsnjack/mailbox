package ui

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"

	"github.com/aymerick/douceur/css"
	"github.com/aymerick/douceur/parser"
	"github.com/diamondburned/gotk4-webkitgtk/pkg/webkit/v6"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/remotecache"
)

const (
	remoteImagePromptThreshold = 20
	remoteImageWorkers         = 6
)

// imageFetchSlots caps how many images (external or inline) download at once.
// WebKit asks for every image in the page at once and our fetches bypass its
// own connection limits, so an image-heavy newsletter would otherwise open a
// connection per image.
var imageFetchSlots = make(chan struct{}, remoteImageWorkers)

type remoteImageStats struct {
	Total       int
	Cached      int
	Unavailable int
	Blocked     int
	Deferred    int
}

var (
	cssRemoteURLRe = regexp.MustCompile(`(?i)url\(\s*(['"]?)(https?://[^'")\s]+)['"]?\s*\)`)
	cssImportRe    = regexp.MustCompile(`(?is)@import\s+[^;]+;`)
	srcsetRemoteRe = regexp.MustCompile(`(?i)https?://[^\s,]+`)
)

// resolveRemoteImages replaces every external image reference with a custom URI
// backed by the content-addressed cache, without touching the network: the key
// is derived from the URL, so the document can name an image the cache does not
// hold yet and serveRemoteImage downloads it when WebKit asks. That keeps the
// download off the render's critical path — the conversation is readable as soon
// as its text is sanitized, and images fill in as they arrive, the way a browser
// loads a page. References that must not be requested at all (the privacy
// opt-out, or an image-heavy message whose one-time prompt is unconfirmed) lose
// their attribute here, so nothing can reach the network behind that decision.
// It returns the rewritten HTML, what the banner has to say, and the key → URL
// map the scheme handler fetches against.
func (w *window) resolveRemoteImages(source string, allowNetwork, largeSetApproved bool) (string, remoteImageStats, map[string]string) {
	doc, err := xhtml.Parse(strings.NewReader(source))
	if err != nil {
		return source, remoteImageStats{}, nil
	}
	urls := collectRemoteImageURLs(doc)
	stats := remoteImageStats{Total: len(urls)}
	entries := make(map[string]remotecache.Entry, len(urls))
	pending := make(map[string]string, len(urls))
	largeSetBlocked := len(urls) > remoteImagePromptThreshold && !largeSetApproved
	for _, rawURL := range urls {
		switch {
		case !allowNetwork:
			stats.Blocked++
			continue
		case largeSetBlocked:
			stats.Deferred++
			continue
		}
		key, err := remotecache.Key(rawURL)
		if err != nil {
			// Not addressable (no host, credentials in the URL, wrong scheme):
			// it could never have been fetched, so report it as unavailable
			// rather than naming a resource the handler would reject.
			logging.Trace("ui: external image not addressable", "err", err)
			stats.Unavailable++
			continue
		}
		entries[rawURL] = remotecache.Entry{Key: key}
		pending[key] = rawURL
		if w.remoteImages != nil {
			if _, ok := w.remoteImages.Open(key); ok {
				stats.Cached++ // already on disk: served without a request
			}
		}
	}
	changed := rewriteRemoteImageURLs(doc, entries)
	if !changed {
		return source, stats, pending
	}
	body := findBody(doc)
	if body == nil {
		return source, stats, pending
	}
	var b strings.Builder
	for child := body.FirstChild; child != nil; child = child.NextSibling {
		if err := xhtml.Render(&b, child); err != nil {
			return source, stats, pending
		}
	}
	return b.String(), stats, pending
}

func collectRemoteImageURLs(root *xhtml.Node) []string {
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		raw = html.UnescapeString(strings.TrimSpace(raw))
		if !isRemoteHTTP(raw) || seen[raw] {
			return
		}
		seen[raw] = true
		out = append(out, raw)
	}
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			for _, a := range n.Attr {
				switch strings.ToLower(a.Key) {
				case "src", "background", "poster":
					add(a.Val)
				case "srcset":
					for _, raw := range srcsetRemoteRe.FindAllString(a.Val, -1) {
						add(raw)
					}
				case "style":
					collectInlineCSSImageURLs(a.Val, add)
				}
			}
		}
		if n.Type == xhtml.TextNode && n.Parent != nil && n.Parent.Type == xhtml.ElementNode && n.Parent.Data == "style" {
			collectStylesheetImageURLs(n.Data, add)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return out
}

func collectInlineCSSImageURLs(cssText string, add func(string)) {
	// Inline email CSS frequently omits the final semicolon, which douceur's
	// declaration parser rejects. Split the simple declaration list here; a URL
	// containing a semicolon is left unfetched rather than guessed at.
	for _, declaration := range strings.Split(cssText, ";") {
		property, value, ok := strings.Cut(declaration, ":")
		if ok && cssPropertyDisplaysImage(property) {
			collectCSSURLs(value, add)
		}
	}
}

func collectStylesheetImageURLs(cssText string, add func(string)) {
	stylesheet, err := parser.Parse(cssText)
	if err != nil {
		return
	}
	var walk func([]*css.Rule)
	walk = func(rules []*css.Rule) {
		for _, rule := range rules {
			collectCSSDeclarationURLs(rule.Declarations, add)
			walk(rule.Rules)
		}
	}
	walk(stylesheet.Rules)
}

func collectCSSDeclarationURLs(declarations []*css.Declaration, add func(string)) {
	for _, declaration := range declarations {
		if !cssPropertyDisplaysImage(declaration.Property) {
			continue
		}
		collectCSSURLs(declaration.Value, add)
	}
}

func cssPropertyDisplaysImage(property string) bool {
	switch strings.ToLower(strings.TrimSpace(property)) {
	case "background", "background-image", "border-image", "border-image-source",
		"content", "list-style", "list-style-image", "mask", "mask-image",
		"-webkit-mask", "-webkit-mask-image":
		return true
	default:
		return false
	}
}

func collectCSSURLs(cssText string, add func(string)) {
	for _, match := range cssRemoteURLRe.FindAllStringSubmatch(cssText, -1) {
		if len(match) > 2 {
			add(match[2])
		}
	}
}

func rewriteRemoteImageURLs(root *xhtml.Node, entries map[string]remotecache.Entry) bool {
	changed := false
	rewrite := func(raw string) (string, bool) {
		raw = html.UnescapeString(strings.TrimSpace(raw))
		if !isRemoteHTTP(raw) {
			return raw, false
		}
		if entry, ok := entries[raw]; ok {
			return "mbcache:" + entry.Key, true
		}
		return "", true // uncached/blocked: remove the network-bearing attribute
	}
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			attrs := n.Attr[:0]
			for _, a := range n.Attr {
				keep := true
				switch strings.ToLower(a.Key) {
				case "src", "background", "poster":
					if value, did := rewrite(a.Val); did {
						changed = true
						a.Val = value
						keep = value != ""
					}
				case "srcset":
					value, did := rewriteSrcset(a.Val, entries)
					changed = changed || did
					a.Val = value
					keep = strings.TrimSpace(value) != ""
				case "style":
					a.Val = rewriteCSSURLs(a.Val, entries, &changed)
				}
				if keep {
					attrs = append(attrs, a)
				}
			}
			n.Attr = attrs
		}
		if n.Type == xhtml.TextNode && n.Parent != nil && n.Parent.Type == xhtml.ElementNode && n.Parent.Data == "style" {
			n.Data = rewriteCSSURLs(n.Data, entries, &changed)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return changed
}

func rewriteSrcset(value string, entries map[string]remotecache.Entry) (string, bool) {
	parts := strings.Split(value, ",")
	out := parts[:0]
	changed := false
	for _, part := range parts {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 0 || !isRemoteHTTP(fields[0]) {
			out = append(out, part)
			continue
		}
		changed = true
		raw := html.UnescapeString(fields[0])
		entry, ok := entries[raw]
		if !ok {
			continue // remove the whole candidate, including its width descriptor
		}
		fields[0] = "mbcache:" + entry.Key
		out = append(out, strings.Join(fields, " "))
	}
	return strings.Join(out, ", "), changed
}

func rewriteCSSURLs(cssText string, entries map[string]remotecache.Entry, changed *bool) string {
	withoutImports := cssImportRe.ReplaceAllStringFunc(cssText, func(string) string {
		*changed = true
		return ""
	})
	return cssRemoteURLRe.ReplaceAllStringFunc(withoutImports, func(match string) string {
		parts := cssRemoteURLRe.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		raw := html.UnescapeString(parts[2])
		*changed = true
		if entry, ok := entries[raw]; ok {
			return `url("mbcache:` + entry.Key + `")`
		}
		return "none"
	})
}

func isRemoteHTTP(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")
}

func imageNoun(n int) string {
	if n == 1 {
		return "image"
	}
	return "images"
}

func remoteImageBannerCopy(stats remoteImageStats) (title, button string, loadAll bool) {
	if stats.Blocked > 0 {
		return fmt.Sprintf("%d external %s blocked for privacy", stats.Blocked, imageNoun(stats.Blocked)), "Show images", false
	}
	if stats.Deferred > 0 {
		return fmt.Sprintf("This message contains %d external images", stats.Total), "Load images", true
	}
	if stats.Unavailable > 0 {
		return fmt.Sprintf("%d external %s unavailable", stats.Unavailable, imageNoun(stats.Unavailable)), "Retry", false
	}
	return "", "", false
}

// remoteImageFetchTimeout bounds one on-demand image download. The cache's HTTP
// client has its own per-request timeout; this also covers a server that dribbles
// bytes forever, so a hung image can never hold a request object open.
const remoteImageFetchTimeout = 20 * time.Second

// serveRemoteImage answers a mbcache: request: from disk when the image is
// already cached, otherwise by downloading it and finishing the request when it
// lands. WebKit allows a scheme request to be completed later, which is what
// keeps image loading off the render path — the handler must never block, since
// it runs on the main thread.
func (w *window) serveRemoteImage(req *webkit.URISchemeRequest) {
	if w.remoteImages == nil {
		finishBlankImage(req)
		return
	}
	key := strings.TrimPrefix(req.URI(), "mbcache:")
	key = strings.TrimPrefix(key, "//")
	if i := strings.IndexAny(key, "?#/"); i >= 0 {
		key = key[:i]
	}
	if entry, ok := w.remoteImages.Open(key); ok {
		w.finishImageRequest(req, entry.Path, entry.MIME)
		return
	}
	rawURL, ok := w.remoteImageURLs[key]
	if !ok {
		// Not part of the open conversation (a stale request from a page being
		// replaced, or a key we never named): nothing to fetch.
		logging.Trace("ui: external image not in the open conversation", "key", key)
		finishBlankImage(req)
		return
	}
	gen := w.renderGen
	logging.Trace("ui: external image fetch on demand", "key", key)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), remoteImageFetchTimeout)
		defer cancel()
		select {
		case imageFetchSlots <- struct{}{}:
			defer func() { <-imageFetchSlots }()
		case <-ctx.Done():
			dispatch.Main(func() { req.FinishError(ctx.Err()) })
			return
		}
		entry, ok, err := w.remoteImages.Get(ctx, rawURL, true)
		dispatch.Main(func() {
			if err != nil || !ok {
				logging.Trace("ui: external image unavailable", "key", key, "err", err)
				finishBlankImage(req)
				w.noteRemoteImageUnavailable(gen)
				return
			}
			w.finishImageRequest(req, entry.Path, entry.MIME)
		})
	}()
}

// blankPixel is a 1×1 fully transparent PNG. An image reference that can't be
// satisfied is answered with this rather than an error: failing the request
// makes WebKit draw its broken-image glyph, and a dead campaign URL in a
// year-old newsletter is not something the reader should render as damage. The
// element keeps whatever box its width/height give it and shows nothing.
var blankPixel = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00,
	0x1f, 0x15, 0xc4, 0x89,
	0x00, 0x00, 0x00, 0x0a, 'I', 'D', 'A', 'T',
	0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01,
	0x0d, 0x0a, 0x2d, 0xb4,
	0x00, 0x00, 0x00, 0x00, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

// finishBlankImage answers a scheme request with the transparent pixel.
// Main thread only.
func finishBlankImage(req *webkit.URISchemeRequest) {
	stream := gio.NewMemoryInputStreamFromBytes(glib.NewBytes(blankPixel))
	req.Finish(stream, int64(len(blankPixel)), "image/png")
}

// finishImageRequest streams a local file back to WebKit as the answer to a
// custom-scheme request. Main thread only.
func (w *window) finishImageRequest(req *webkit.URISchemeRequest, path, mime string) {
	stream, err := gio.NewFileForPath(path).Read(context.Background())
	if err != nil {
		slog.Warn("ui: open cached image", "path", path, "err", err)
		finishBlankImage(req)
		return
	}
	var size int64 = -1
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
	}
	req.Finish(stream, size, mime)
}

// noteRemoteImageUnavailable folds a failed on-demand download into the banner,
// so "N external images unavailable" appears as failures happen rather than
// after a blocking prefetch. Stale renders are ignored: their images belong to a
// conversation that is no longer on screen. Main thread only.
func (w *window) noteRemoteImageUnavailable(gen uint64) {
	if gen != w.renderGen {
		return
	}
	w.remoteStats.Unavailable++
	w.applyRemoteImageBanner(w.remoteStats)
}

// applyRemoteImageBanner reveals (or hides) the remote-image banner for stats.
// Main thread only.
func (w *window) applyRemoteImageBanner(stats remoteImageStats) {
	title, button, loadAll := remoteImageBannerCopy(stats)
	w.remoteImageTotal = stats.Total
	w.remoteImageLoadAll = loadAll
	if title == "" {
		w.remoteImageBanner.SetRevealed(false)
		return
	}
	w.remoteImageBanner.SetTitle(title)
	w.remoteImageBanner.SetButtonLabel(button)
	w.remoteImageBanner.SetRevealed(true)
}
