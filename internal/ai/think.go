package ai

import "strings"

// Reasoning models expose their chain-of-thought one of two ways: a separate
// response field, or inline in the content between <think> tags. The separate
// field is already dropped — every extractFunc reads only the content delta —
// but inline tags would stream straight into whatever the user is looking at: a
// drafted reply appearing in the compose window, or a thread summary card. So
// the text is filtered on the way out of streamSSE, for every provider.
//
// Stripping it after the fact is not enough. The stream is consumed
// incrementally and rendered as it arrives, so by the time "</think>" shows up
// the reasoning is already on screen. The filter therefore has to withhold text
// it cannot yet classify, which also means a tag split across SSE chunks
// ("<thi" then "nk>") must not leak.
type thinkFilter struct {
	inThink bool
	held    strings.Builder // text withheld because it may start a tag
}

var thinkOpen = []string{"<think>", "<thinking>", "<reasoning>"}
var thinkClose = []string{"</think>", "</thinking>", "</reasoning>"}

// couldStartTag reports whether s is a proper prefix of any tag in tags, i.e.
// whether more input could still turn it into one.
func couldStartTag(s string, tags []string) bool {
	for _, t := range tags {
		if len(s) < len(t) && strings.HasPrefix(t, s) {
			return true
		}
	}
	return false
}

// firstTag returns the earliest occurrence of any tag in tags, or -1.
func firstTag(s string, tags []string) (idx int, tag string) {
	idx = -1
	for _, t := range tags {
		if i := strings.Index(s, t); i >= 0 && (idx < 0 || i < idx) {
			idx, tag = i, t
		}
	}
	return idx, tag
}

// feed returns the portion of s that is safe to show the user now. Text inside
// a think block is dropped; a partial tag at the end is held back until the next
// chunk resolves it.
func (f *thinkFilter) feed(s string) string {
	f.held.WriteString(s)
	buf := f.held.String()
	f.held.Reset()

	var out strings.Builder
	for buf != "" {
		if f.inThink {
			i, tag := firstTag(buf, thinkClose)
			if i < 0 {
				// Still thinking. Keep only a possible partial closing tag; the
				// rest is reasoning and is discarded.
				if tail := partialTail(buf, thinkClose); tail != "" {
					f.held.WriteString(tail)
				}
				break
			}
			buf = buf[i+len(tag):]
			f.inThink = false
			continue
		}
		i, tag := firstTag(buf, thinkOpen)
		if i < 0 {
			// No opening tag. Emit everything except a suffix that could still
			// become one.
			if tail := partialTail(buf, thinkOpen); tail != "" {
				out.WriteString(buf[:len(buf)-len(tail)])
				f.held.WriteString(tail)
			} else {
				out.WriteString(buf)
			}
			break
		}
		out.WriteString(buf[:i])
		buf = buf[i+len(tag):]
		f.inThink = true
	}
	return out.String()
}

// partialTail returns the longest suffix of s that is a proper prefix of one of
// tags, or "" when no suffix could grow into a tag.
func partialTail(s string, tags []string) string {
	max := 0
	for _, t := range tags {
		if len(t)-1 > max {
			max = len(t) - 1
		}
	}
	if max > len(s) {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if couldStartTag(s[len(s)-n:], tags) {
			return s[len(s)-n:]
		}
	}
	return ""
}

// flush returns any withheld text at end of stream. Text held because it looked
// like the start of a tag was never reasoning if the tag never completed, so it
// is released; anything still inside an unterminated think block is dropped,
// since a model that opened <think> and never closed it produced no answer.
func (f *thinkFilter) flush() string {
	s := f.held.String()
	f.held.Reset()
	if f.inThink {
		return ""
	}
	return s
}
