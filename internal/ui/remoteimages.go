package ui

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	xhtml "golang.org/x/net/html"

	"github.com/aymerick/douceur/css"
	"github.com/aymerick/douceur/parser"
	"github.com/diamondburned/gotk4-webkitgtk/pkg/webkit/v6"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/remotecache"
)

const (
	remoteImagePromptThreshold = 20
	remoteImageWorkers         = 6
)

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

// cacheRemoteImages replaces every external image reference with a custom URI
// backed by the content-addressed cache. With network disabled it still serves
// images cached during an earlier trusted view; uncached URLs are removed so
// WebKit can never bypass the cache/client privacy policy.
func (w *window) cacheRemoteImages(ctx context.Context, source string, allowNetwork, largeSetApproved bool) (string, remoteImageStats) {
	doc, err := xhtml.Parse(strings.NewReader(source))
	if err != nil {
		return source, remoteImageStats{}
	}
	urls := collectRemoteImageURLs(doc)
	stats := remoteImageStats{Total: len(urls)}
	entries := make(map[string]remotecache.Entry, len(urls))
	if w.remoteImages != nil {
		for _, rawURL := range urls {
			entry, ok, err := w.remoteImages.Get(ctx, rawURL, false)
			if err == nil && ok {
				entries[rawURL] = entry
			}
		}
	}
	var networkURLs []string
	largeSetBlocked := len(urls) > remoteImagePromptThreshold && !largeSetApproved
	for _, rawURL := range urls {
		if _, ok := entries[rawURL]; ok {
			continue
		}
		if !allowNetwork {
			stats.Blocked++
		} else if largeSetBlocked {
			stats.Deferred++
		} else {
			networkURLs = append(networkURLs, rawURL)
		}
	}
	cachedBefore := len(entries)
	if w.remoteImages != nil && len(networkURLs) > 0 {
		fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		jobs := make(chan string)
		var wg sync.WaitGroup
		var mu sync.Mutex
		workers := remoteImageWorkers
		if len(networkURLs) < workers {
			workers = len(networkURLs)
		}
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for rawURL := range jobs {
					entry, ok, err := w.remoteImages.Get(fetchCtx, rawURL, allowNetwork)
					if err != nil {
						logging.TraceContext(fetchCtx, "ui: external image unavailable", "err_type", fmt.Sprintf("%T", err))
						continue
					}
					if ok {
						mu.Lock()
						entries[rawURL] = entry
						mu.Unlock()
					}
				}
			}()
		}
	sendJobs:
		for _, rawURL := range networkURLs {
			select {
			case jobs <- rawURL:
			case <-fetchCtx.Done():
				break sendJobs
			}
		}
		close(jobs)
		wg.Wait()
	}
	stats.Cached = len(entries)
	stats.Unavailable = len(networkURLs) - (len(entries) - cachedBefore)
	changed := rewriteRemoteImageURLs(doc, entries)
	if !changed {
		return source, stats
	}
	body := findBody(doc)
	if body == nil {
		return source, stats
	}
	var b strings.Builder
	for child := body.FirstChild; child != nil; child = child.NextSibling {
		if err := xhtml.Render(&b, child); err != nil {
			return source, stats
		}
	}
	return b.String(), stats
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

// serveRemoteImage streams a previously validated cached image to WebKit.
func (w *window) serveRemoteImage(req *webkit.URISchemeRequest) {
	if w.remoteImages == nil {
		req.FinishError(fmt.Errorf("external image cache unavailable"))
		return
	}
	key := strings.TrimPrefix(req.URI(), "mbcache:")
	key = strings.TrimPrefix(key, "//")
	if i := strings.IndexAny(key, "?#/"); i >= 0 {
		key = key[:i]
	}
	entry, ok := w.remoteImages.Open(key)
	if !ok {
		req.FinishError(fmt.Errorf("cached external image not found"))
		return
	}
	stream, err := gio.NewFileForPath(entry.Path).Read(context.Background())
	if err != nil {
		slog.Warn("ui: open cached external image", "key", key, "err", err)
		req.FinishError(err)
		return
	}
	var size int64 = -1
	if fi, err := os.Stat(entry.Path); err == nil {
		size = fi.Size()
	}
	req.Finish(stream, size, entry.MIME)
}
