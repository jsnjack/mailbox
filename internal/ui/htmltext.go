package ui

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// translationPlan is one email body prepared for translation: its parsed
// document, the text worth translating, and where each translation goes back.
// Extraction and reassembly are separate phases so a whole conversation can pool
// its text into one set of requests — the same paragraph quoted in four replies
// is then translated once instead of four times (measured at two thirds of the
// work on a real five-message thread), and every copy of it reads identically.
type translationPlan struct {
	doc      *html.Node
	nodes    []*html.Node
	pieces   [][]textPiece
	segments []string // in document order; may repeat within one body
}

// textPiece is one run of a text node: seg is the trimmed text to translate, or
// "" for a separator kept verbatim. A node's pieces concatenate back to its
// original text.
type textPiece struct {
	text string
	seg  string
}

// planTranslation parses htmlStr and collects the visible text worth translating.
//
// Preformatted text is collected one segment per paragraph instead of one segment
// per node: a plain-text body is rendered as a single <pre>, so the whole email is
// one text node, and a whole email in one snippet reads to the model like several
// snippets — it answers with one element per paragraph, and the reply no longer
// lines up with what was asked for.
func planTranslation(htmlStr string) (*translationPlan, error) {
	doc, err := html.Parse(strings.NewReader(htmlStr))
	if err != nil {
		logging.Trace("ui: translate html parse failed", "err", err)
		return nil, err
	}
	p := &translationPlan{doc: doc}
	var walk func(n *html.Node, inPre bool)
	walk = func(n *html.Node, inPre bool) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "head", "title":
				return // non-visible content
			case "pre":
				inPre = true
			}
		}
		if n.Type == html.TextNode && hasLetters(n.Data) {
			var ps []textPiece
			for _, chunk := range splitParagraphs(n.Data, inPre) {
				var seg string
				if hasLetters(chunk) {
					seg = strings.TrimSpace(chunk)
					p.segments = append(p.segments, seg)
				}
				ps = append(ps, textPiece{text: chunk, seg: seg})
			}
			p.nodes = append(p.nodes, n)
			p.pieces = append(p.pieces, ps)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inPre)
		}
	}
	walk(doc, false)
	logging.Trace("ui: translate html segments", "n", len(p.segments))
	return p, nil
}

// render writes the translations back into the plan's markup and returns the
// re-rendered HTML. Translations are looked up by source text, so a segment the
// translator left out (or answered empty) simply keeps its original wording —
// only that segment, never the ones after it. The markup is preserved verbatim.
func (p *translationPlan) render(byText map[string]string) (string, error) {
	for i, n := range p.nodes {
		var b strings.Builder
		for _, piece := range p.pieces[i] {
			tr := byText[piece.seg]
			if piece.seg == "" || strings.TrimSpace(tr) == "" {
				b.WriteString(piece.text) // separator, or nothing came back
				continue
			}
			b.WriteString(preserveSpacing(piece.text, tr))
		}
		n.Data = b.String()
	}
	var b strings.Builder
	if err := html.Render(&b, p.doc); err != nil {
		return "", err
	}
	return b.String(), nil
}

// How a conversation's pooled text is split into requests: batches of
// translateBatch segments, translateWorkers of them in flight. Measured on real
// threads: a long thread's text in one request took 67s, where the same text in
// parallel batches took 13s, and a batch this size answers in a few seconds.
const (
	translateBatch   = 40
	translateWorkers = 4
)

// poolTranslations translates the text of several bodies as one set and returns
// the translation of every unique segment, keyed by source text.
//
// Pooling is what makes a conversation cheap to translate: every message quotes
// the ones before it, so the same paragraph turns up in several bodies — two
// thirds of the segments on a real five-message thread — and pooling asks for it
// once. It also makes the result consistent, since a quoted paragraph is no
// longer re-worded differently in each message that quotes it.
func poolTranslations(plans []*translationPlan, translate func([]string) ([]string, error)) (map[string]string, error) {
	var unique []string
	seen := map[string]bool{}
	total := 0
	for _, p := range plans {
		total += len(p.segments)
		for _, seg := range p.segments {
			if !seen[seg] {
				seen[seg] = true
				unique = append(unique, seg)
			}
		}
	}
	logging.Trace("ui: translate pooled segments", "bodies", len(plans), "segments", total, "unique", len(unique))

	byText := make(map[string]string, len(unique))
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, translateWorkers)
	var wg sync.WaitGroup
	for start := 0; start < len(unique); start += translateBatch {
		batch := unique[start:min(start+translateBatch, len(unique))]
		wg.Add(1)
		sem <- struct{}{}
		go func(batch []string) {
			defer wg.Done()
			defer func() { <-sem }()
			out, err := translate(batch)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				var got map[string]string
				if got, err = zipTranslations(batch, out); err == nil {
					for seg, tr := range got {
						byText[seg] = tr
					}
				}
			}
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}(batch)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return byText, nil
}

// translateHTMLText translates one body end to end: extract, translate, reassemble.
// translate must return one translation per segment, in order.
func translateHTMLText(htmlStr string, translate func([]string) ([]string, error)) (string, error) {
	plan, err := planTranslation(htmlStr)
	if err != nil {
		return "", err
	}
	if len(plan.segments) == 0 {
		logging.Trace("ui: translate html no text segments")
		return htmlStr, nil
	}
	translated, err := translate(plan.segments)
	if err != nil {
		logging.Trace("ui: translate html failed", "err", err, "segments", len(plan.segments))
		return "", err
	}
	byText, err := zipTranslations(plan.segments, translated)
	if err != nil {
		return "", err
	}
	logging.Trace("ui: translate html done", "segments", len(translated))
	return plan.render(byText)
}

// zipTranslations pairs each segment with its translation by position, keyed by
// source text so callers can look one up wherever it appears.
//
// More elements back than were asked for means the model split a snippet on its
// own, and the reply can no longer be paired up by position. For a single snippet
// the extra elements are the rest of its translation and re-join; otherwise this
// refuses, because pairing element i with segment i would then put one
// paragraph's translation on another paragraph's text. (Keeping only the elements
// the segments had room for is what dropped everything after the greeting of a
// plain-text email.) Fewer elements is safe: the tail keeps its original text.
func zipTranslations(segments, translated []string) (map[string]string, error) {
	if len(translated) > len(segments) {
		if len(segments) != 1 {
			logging.Trace("ui: translate segment mismatch", "want", len(segments), "got", len(translated))
			return nil, fmt.Errorf("translator returned %d segments for %d", len(translated), len(segments))
		}
		translated = []string{joinSegments(translated)}
	}
	byText := make(map[string]string, len(segments))
	for i, seg := range segments {
		if i >= len(translated) || strings.TrimSpace(translated[i]) == "" {
			continue
		}
		if _, ok := byText[seg]; !ok { // a repeated segment keeps the first answer
			byText[seg] = translated[i]
		}
	}
	return byText, nil
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

// paragraphBreak matches a run of two or more line breaks — the paragraph
// separator of a plain-text email body.
var paragraphBreak = regexp.MustCompile(`\r?\n[ \t]*(?:\r?\n[ \t]*)+`)

// splitParagraphs breaks a text node's data into alternating text and separator
// chunks that concatenate back to the original string. Only preformatted text is
// split: in flowed HTML a blank line is insignificant whitespace inside a
// paragraph, so splitting there would hand the translator half a sentence.
func splitParagraphs(s string, inPre bool) []string {
	if !inPre {
		return []string{s}
	}
	breaks := paragraphBreak.FindAllStringIndex(s, -1)
	if len(breaks) == 0 {
		return []string{s}
	}
	out := make([]string, 0, 2*len(breaks)+1)
	last := 0
	for _, b := range breaks {
		out = append(out, s[last:b[0]], s[b[0]:b[1]])
		last = b[1]
	}
	return append(out, s[last:])
}

// joinSegments reassembles a translation the model returned in several parts for
// one requested snippet. Such a reply usually keeps each part's own trailing
// line breaks, so the parts concatenate; when they were trimmed, the paragraph
// breaks are restored, and parts with no line breaks at all (one paragraph split
// by sentence) are joined with a space.
func joinSegments(parts []string) string {
	selfSeparating, multiline := true, false
	for i, p := range parts {
		if strings.ContainsAny(p, "\r\n") {
			multiline = true
		}
		if i < len(parts)-1 && strings.TrimRight(p, " \t\r\n") == p {
			selfSeparating = false
		}
	}
	switch {
	case selfSeparating:
		return strings.Join(parts, "")
	case multiline:
		return strings.Join(parts, "\n\n")
	default:
		return strings.Join(parts, " ")
	}
}
