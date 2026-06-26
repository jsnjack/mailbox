package ui

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

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
