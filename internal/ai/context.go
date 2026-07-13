package ai

import "strings"

// CleanContext strips the invisible preheader padding marketing mail packs
// into snippets (LinkedIn pads with dozens of U+034F+NBSP pairs to suppress
// preview text) and collapses whitespace. The junk wastes prompt tokens and
// measurably changes a small model's classification. Shared by every caller
// that feeds snippets to the AI (the UI and the background worker).
func CleanContext(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case 0x034f, 0x200b, 0x200c, 0x200d, 0xfeff: // grapheme joiner, zero-widths, BOM
			return -1
		case 0x00a0: // NBSP
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
