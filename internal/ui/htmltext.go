package ui

import (
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// trackerSrcPatterns are URL substrings of well-known email open-tracking
// endpoints. Conservative on purpose — the 1x1-pixel heuristic catches most
// trackers; this only adds clear offenders that may declare a larger size.
var trackerSrcPatterns = []string{
	"/wf/open",   // SendGrid
	"__ptq.gif",  // HubSpot
	"/open.aspx", // common ESP open pixel
	"/track/open", "/trackopen", "/openpixel", "open-pixel", "/o/open",
	"emltrk.com", // Litmus
	"/decode_serialized_blob", "/imp.gif", "/oo.gif",
}

// stripTrackers removes likely tracking pixels from (already-sanitized) email
// HTML: <img> elements that are 1x1/tiny or whose src matches a known tracker
// pattern. Real, visible images are kept, so images can load by default without
// leaking that the message was opened. Returns the body's inner HTML and the
// number of trackers removed.
func stripTrackers(htmlStr string) (string, int) {
	doc, err := html.Parse(strings.NewReader(htmlStr))
	if err != nil {
		return htmlStr, 0
	}
	removed := 0
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		var next *html.Node
		for c := n.FirstChild; c != nil; c = next {
			next = c.NextSibling
			if c.Type == html.ElementNode && c.Data == "img" && isTrackerImg(c) {
				n.RemoveChild(c)
				removed++
				continue
			}
			walk(c)
		}
	}
	walk(doc)
	if removed == 0 {
		return htmlStr, 0 // unchanged; avoid re-serializing
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
	var src, width, height, style string
	for _, a := range n.Attr {
		switch strings.ToLower(a.Key) {
		case "src":
			src = strings.ToLower(a.Val)
		case "width":
			width = a.Val
		case "height":
			height = a.Val
		case "style":
			style = strings.ToLower(a.Val)
		}
	}
	if tinyDim(width) && tinyDim(height) {
		return true
	}
	if strings.Contains(style, "width:1px") || strings.Contains(style, "width: 1px") ||
		strings.Contains(style, "height:1px") || strings.Contains(style, "height: 1px") {
		return true
	}
	for _, p := range trackerSrcPatterns {
		if strings.Contains(src, p) {
			return true
		}
	}
	return false
}

// tinyDim reports whether a width/height attribute is present and ≤ 2 px.
func tinyDim(v string) bool {
	v = strings.TrimSuffix(strings.TrimSpace(v), "px")
	if v == "" {
		return false
	}
	n, err := strconv.Atoi(v)
	return err == nil && n <= 2
}

// collapseQuotes wraps each top-level <blockquote> (a quoted reply history) in a
// native <details> disclosure, so long quote chains collapse behind a "Show
// quoted text" toggle. It runs on already-sanitized HTML and adds only trusted
// markup. If there are no blockquotes the input is returned unchanged — so a
// miss never alters rendering. Nested blockquotes are left inside their parent.
func collapseQuotes(htmlStr string) string {
	doc, err := html.Parse(strings.NewReader(htmlStr))
	if err != nil {
		return htmlStr
	}
	var quotes []*html.Node
	var walk func(n *html.Node, inQuote bool)
	walk = func(n *html.Node, inQuote bool) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			isBlockquote := c.Type == html.ElementNode && c.Data == "blockquote"
			if isBlockquote && !inQuote {
				quotes = append(quotes, c)
			}
			walk(c, inQuote || isBlockquote)
		}
	}
	walk(doc, false)
	if len(quotes) == 0 {
		return htmlStr // unchanged; avoid re-serializing
	}

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
		return htmlStr
	}
	var b strings.Builder
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&b, c); err != nil {
			return htmlStr
		}
	}
	return b.String()
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
		return htmlStr, nil
	}
	translated, err := translate(texts)
	if err != nil {
		return "", err
	}
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
