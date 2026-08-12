package ui

import (
	"html"
	"strings"
)

// The AI writes markdown whether or not it is asked to, so its answers are
// rendered rather than shown raw — a summary full of "**Proposed change**" and
// backticks reads like a debug dump. Only the subset a model actually reaches
// for in prose is supported: bold, inline code, italics, bullets and headings.
//
// Two targets need it: GTK labels, which speak Pango markup, and the reader's
// WebView, which speaks HTML. One walker serves both — the difference is the
// tags it emits and how it ends a line.
type mdTarget struct {
	bold   [2]string
	italic [2]string
	code   [2]string
	br     string
}

var (
	// Pango has no <code>; <tt> is its monospace tag.
	pangoTarget = mdTarget{
		bold:   [2]string{"<b>", "</b>"},
		italic: [2]string{"<i>", "</i>"},
		code:   [2]string{"<tt>", "</tt>"},
		br:     "\n",
	}
	htmlTarget = mdTarget{
		bold:   [2]string{"<b>", "</b>"},
		italic: [2]string{"<i>", "</i>"},
		code:   [2]string{"<code>", "</code>"},
		br:     "<br>",
	}
)

// markdownToPango renders AI markdown for a GTK label (SetMarkup). Everything
// literal is escaped, and only balanced tags are ever emitted — an unbalanced
// marker in a half-streamed answer stays literal rather than producing markup
// Pango refuses to parse, which would blank the label.
func markdownToPango(s string) string { return renderMarkdown(s, pangoTarget) }

// markdownToHTML renders AI markdown for the reader.
func markdownToHTML(s string) string { return renderMarkdown(s, htmlTarget) }

// renderMarkdown converts a block of markdown line by line.
func renderMarkdown(s string, t mdTarget) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		out = append(out, renderMarkdownLine(ln, t))
	}
	return strings.Join(out, t.br)
}

// renderMarkdownLine handles the block-level markers that can start a line —
// a heading or a bullet — then renders the rest inline.
func renderMarkdownLine(ln string, t mdTarget) string {
	trimmed := strings.TrimLeft(ln, " \t")
	indent := ln[:len(ln)-len(trimmed)]

	// "### Heading" — a summary has no room for real heading levels, so every
	// depth becomes a bold line.
	if h := strings.TrimLeft(trimmed, "#"); h != trimmed && strings.HasPrefix(h, " ") {
		return indent + t.bold[0] + renderInlineMarkdown(strings.TrimSpace(h), t) + t.bold[1]
	}
	// "- item" / "* item" / an already-bulleted line from an older stored
	// summary. Two spaces after the bullet keep wrapped lines readable.
	for _, marker := range []string{"- ", "* ", "•  ", "• "} { // longest first
		if strings.HasPrefix(trimmed, marker) {
			return indent + "•  " + renderInlineMarkdown(strings.TrimPrefix(trimmed, marker), t)
		}
	}
	return indent + renderInlineMarkdown(trimmed, t)
}

// renderInlineMarkdown emits one line, escaping every literal run and closing
// each marker it opens. A marker with no partner is left as written: text is
// half-formed while an answer streams in, and an unclosed "**" must not swallow
// the rest of the line.
func renderInlineMarkdown(s string, t mdTarget) string {
	var out, lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			out.WriteString(html.EscapeString(lit.String()))
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch {
		case s[i] == '`':
			// Code spans win over everything inside them, so a path like
			// `ss-*` cannot start an italic run.
			if j := strings.IndexByte(s[i+1:], '`'); j > 0 {
				flush()
				out.WriteString(t.code[0] + html.EscapeString(s[i+1:i+1+j]) + t.code[1])
				i += j + 2
				continue
			}
		case strings.HasPrefix(s[i:], "**"):
			if j := strings.Index(s[i+2:], "**"); j > 0 {
				flush()
				out.WriteString(t.bold[0] + renderInlineMarkdown(s[i+2:i+2+j], t) + t.bold[1])
				i += j + 4
				continue
			}
		case s[i] == '*':
			// Single asterisks are italics only when they wrap something: a
			// lone "*" (a glob, a footnote mark) stays literal. Underscores are
			// deliberately not italics — snake_case names are far more common
			// in this mail than emphasis.
			if j := strings.IndexByte(s[i+1:], '*'); j > 0 && strings.TrimSpace(s[i+1:i+1+j]) != "" {
				flush()
				out.WriteString(t.italic[0] + renderInlineMarkdown(s[i+1:i+1+j], t) + t.italic[1])
				i += j + 2
				continue
			}
		}
		lit.WriteByte(s[i])
		i++
	}
	flush()
	return out.String()
}
