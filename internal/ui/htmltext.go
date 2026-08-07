package ui

import (
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/html"

	"github.com/jsnjack/mailbox/internal/logging"
)

// trackerSrcPatterns are URL substrings of well-known email open-tracking
// endpoints. Structural checks catch hidden and tiny resources; this list adds
// clear offenders that disguise themselves as visible content.
var trackerSrcPatterns = []string{
	"/wf/open",   // SendGrid
	"__ptq.gif",  // HubSpot
	"/open.aspx", // common ESP open pixel
	"/track/open", "/trackopen", "/openpixel", "open-pixel", "/o/open",
	"emltrk.com", // Litmus
	"/decode_serialized_blob", "/imp.gif", "/oo.gif",
	"/email/open", "/email/track", "/tracking/open", "/tracking/pixel",
	"/email-pixel", "/email_pixel", "/read-receipt", "/read_receipt", "/readreceipt",
}

// cleanEmailHTML performs the two structural passes a rendered email needs, in a
// single parse + serialize of (already-sanitized) HTML:
//
//   - strips likely tracking resources — tiny or concealed images/backgrounds
//     and URLs matching known open/read patterns — before they can be fetched; and
//   - wraps each top-level <blockquote> (a quoted reply history) in a native
//     <details> disclosure so long quote chains collapse behind a "Show quoted
//     text" toggle.
//
// It returns the body's inner HTML and the number of trackers removed. If neither
// pass changed anything the input is returned verbatim, so a miss never alters
// rendering and no re-serialization cost is paid. (Previously these were two
// separate parse/walk/render passes; folding them halves the per-message cost.)
func cleanEmailHTML(htmlStr string) (string, int) {
	doc, err := html.Parse(strings.NewReader(htmlStr))
	if err != nil {
		logging.Trace("ui: clean email html parse failed", "err", err, "bytes", len(htmlStr))
		return htmlStr, 0
	}
	removed := 0
	var quotes []*html.Node
	var walk func(n *html.Node, inQuote bool)
	walk = func(n *html.Node, inQuote bool) {
		var next *html.Node
		for c := n.FirstChild; c != nil; c = next {
			next = c.NextSibling
			if c.Type == html.ElementNode && c.Data == "img" && isTrackerImg(c) {
				n.RemoveChild(c)
				removed++
				continue
			}
			if c.Type == html.ElementNode {
				removed += stripTrackerResources(c)
			}
			isBlockquote := c.Type == html.ElementNode && c.Data == "blockquote"
			if isBlockquote && !inQuote {
				quotes = append(quotes, c) // top-level only; nested are left inside
			}
			walk(c, inQuote || isBlockquote)
		}
	}
	walk(doc, false)

	if removed == 0 && len(quotes) == 0 {
		logging.Trace("ui: clean email html unchanged", "bytes", len(htmlStr))
		return htmlStr, 0 // unchanged; avoid re-serializing
	}
	logging.Trace("ui: clean email html", "trackers", removed, "quoted_blocks", len(quotes))
	for _, bq := range quotes {
		parent := bq.Parent
		if parent == nil {
			continue
		}
		details := &html.Node{Type: html.ElementNode, Data: "details"}
		summary := &html.Node{Type: html.ElementNode, Data: "summary",
			Attr: []html.Attribute{{Key: "style", Val: "cursor:pointer;color:#888;font-size:90%;margin:4px 0"}}}
		summary.AppendChild(&html.Node{Type: html.TextNode, Data: "Show quoted text"})
		parent.InsertBefore(details, bq)
		parent.RemoveChild(bq)
		details.AppendChild(summary)
		details.AppendChild(bq)
	}

	body := findBody(doc)
	if body == nil {
		return htmlStr, removed
	}
	var b strings.Builder
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&b, c); err != nil {
			return htmlStr, removed
		}
	}
	return b.String(), removed
}

// isTrackerImg reports whether an <img> node looks like a tracking pixel.
func isTrackerImg(n *html.Node) bool {
	if resourceNodeConcealed(n) {
		return true
	}
	var sources []string
	for _, a := range n.Attr {
		switch strings.ToLower(a.Key) {
		case "src":
			sources = append(sources, a.Val)
		case "srcset":
			for _, candidate := range strings.Split(a.Val, ",") {
				if fields := strings.Fields(candidate); len(fields) > 0 {
					sources = append(sources, fields[0])
				}
			}
		}
	}
	for _, src := range sources {
		if isTrackerURL(src) {
			return true
		}
	}
	return false
}

func stripTrackerResources(n *html.Node) int {
	concealed := resourceNodeConcealed(n)
	removed := 0
	attrs := n.Attr[:0]
	for _, a := range n.Attr {
		keep := true
		switch strings.ToLower(a.Key) {
		case "background":
			if isRemoteHTTP(a.Val) && (concealed || isTrackerURL(a.Val)) {
				keep = false
				removed++
			}
		case "style":
			var n int
			a.Val, n = stripTrackerCSSURLs(a.Val, concealed)
			removed += n
		}
		if keep {
			attrs = append(attrs, a)
		}
	}
	n.Attr = attrs
	return removed
}

func stripTrackerCSSURLs(cssText string, blockAll bool) (string, int) {
	removed := 0
	out := cssRemoteURLRe.ReplaceAllStringFunc(cssText, func(match string) string {
		parts := cssRemoteURLRe.FindStringSubmatch(match)
		if len(parts) < 3 || (!blockAll && !isTrackerURL(parts[2])) {
			return match
		}
		removed++
		return "none"
	})
	return out, removed
}

func resourceNodeConcealed(n *html.Node) bool {
	for cur := n; cur != nil && cur.Type != html.DocumentNode; cur = cur.Parent {
		for _, a := range cur.Attr {
			switch strings.ToLower(a.Key) {
			case "hidden":
				return true
			case "width", "height":
				if tinyDim(a.Val) {
					return true
				}
			case "style":
				if styleConcealsResource(a.Val) {
					return true
				}
			}
		}
	}
	return false
}

func styleConcealsResource(style string) bool {
	for _, decl := range strings.Split(style, ";") {
		property, value, ok := strings.Cut(decl, ":")
		if ok && cssDeclarationConcealsResource(property, value) {
			return true
		}
	}
	return false
}

func cssDeclarationConcealsResource(property, value string) bool {
	property = strings.ToLower(strings.TrimSpace(property))
	value = strings.ToLower(strings.TrimSpace(value))
	switch property {
	case "display":
		return value == "none"
	case "visibility":
		return value == "hidden" || value == "collapse"
	case "content-visibility":
		return value == "hidden"
	case "opacity":
		percent := strings.HasSuffix(value, "%")
		n, err := strconv.ParseFloat(strings.TrimSuffix(value, "%"), 64)
		if percent {
			n /= 100
		}
		return err == nil && n <= 0.01
	case "width", "height", "max-width", "max-height":
		return tinyDim(value)
	case "mso-hide":
		return value == "all"
	case "filter":
		compact := strings.ReplaceAll(value, " ", "")
		return strings.Contains(compact, "opacity(0)") || strings.Contains(compact, "opacity(0%)")
	case "transform":
		return strings.Contains(strings.ReplaceAll(value, " ", ""), "scale(0)")
	}
	return false
}

func isTrackerURL(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	for _, pattern := range trackerSrcPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	u, err := url.Parse(lower)
	if err != nil {
		return false
	}
	path := strings.TrimSuffix(u.Path, "/")
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		path = path[i+1:]
	}
	if i := strings.LastIndexByte(path, '.'); i > 0 {
		path = path[:i]
	}
	name := strings.NewReplacer("-", "", "_", "").Replace(path)
	switch name {
	case "pixel", "beacon", "openpixel", "trackingpixel", "emailpixel", "readreceipt", "spypixel":
		return true
	case "open", "read", "track":
		if u.RawQuery != "" {
			return true
		}
	}
	for key, values := range u.Query() {
		key = strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(key))
		if key == "openpixel" || key == "trackingpixel" || key == "readreceipt" {
			return true
		}
		if key != "event" && key != "action" && key != "type" {
			continue
		}
		for _, value := range values {
			value = strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(value))
			switch value {
			case "open", "opened", "emailopen", "read", "viewed", "impression":
				return true
			}
		}
	}
	return false
}

// tinyDim reports whether a width/height attribute is present and ≤ 2 px.
func tinyDim(v string) bool {
	v = strings.TrimSpace(v)
	// Read the leading numeric run so any unit (px, em, %, pt, or none) is
	// ignored — "1", "1.0", "1px", "0.5em" all count. strconv.Atoi missed the
	// decimal/relative forms, letting 1x1 trackers evade detection.
	i := 0
	for i < len(v) && ((v[i] >= '0' && v[i] <= '9') || v[i] == '.') {
		i++
	}
	if i == 0 {
		return false
	}
	n, err := strconv.ParseFloat(v[:i], 64)
	return err == nil && n <= 2
}

// findBody returns the <body> element of a parsed document.
func findBody(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && n.Data == "body" {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if b := findBody(c); b != nil {
			return b
		}
	}
	return nil
}

// translateHTMLText extracts the visible text from htmlStr, passes the segments
// to translate (which must return one translation per segment, in order), writes
// the results back into the original markup, and returns the re-rendered HTML.
// The markup is preserved verbatim — only text changes — so the translator only
// ever handles plain text, never tags.
func translateHTMLText(htmlStr string, translate func([]string) ([]string, error)) (string, error) {
	doc, err := html.Parse(strings.NewReader(htmlStr))
	if err != nil {
		logging.Trace("ui: translate html parse failed", "err", err)
		return "", err
	}

	var nodes []*html.Node
	var texts []string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "head", "title":
				return // non-visible content
			}
		}
		if n.Type == html.TextNode && hasLetters(n.Data) {
			nodes = append(nodes, n)
			texts = append(texts, strings.TrimSpace(n.Data))
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if len(texts) == 0 {
		logging.Trace("ui: translate html no text segments")
		return htmlStr, nil
	}
	logging.Trace("ui: translate html segments", "n", len(texts))
	translated, err := translate(texts)
	if err != nil {
		logging.Trace("ui: translate html failed", "err", err, "segments", len(texts))
		return "", err
	}
	logging.Trace("ui: translate html done", "segments", len(translated))
	for i, n := range nodes {
		if i >= len(translated) || strings.TrimSpace(translated[i]) == "" {
			continue // length mismatch or empty → keep the original text
		}
		n.Data = preserveSpacing(n.Data, translated[i])
	}

	var b strings.Builder
	if err := html.Render(&b, doc); err != nil {
		return "", err
	}
	return b.String(), nil
}

// hasLetters reports whether s contains a letter — i.e. is worth translating
// (skips pure whitespace, numbers, punctuation, and URLs of symbols).
func hasLetters(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// preserveSpacing re-wraps translated with the leading/trailing whitespace of
// orig, so spacing between inline elements in the markup is kept.
func preserveSpacing(orig, translated string) string {
	lead := orig[:len(orig)-len(strings.TrimLeft(orig, " \t\r\n"))]
	trail := orig[len(strings.TrimRight(orig, " \t\r\n")):]
	return lead + strings.TrimSpace(translated) + trail
}
