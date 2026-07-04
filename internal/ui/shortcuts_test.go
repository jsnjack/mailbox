package ui

import "testing"

func TestSanitizeKeys(t *testing.T) {
	for in, want := range map[string]string{
		"ae":      "ae",
		"A E":     "ae",
		"aaa":     "a",
		"abcd":    "abc", // capped at three
		"й":       "",    // non-ASCII can't match keyvals
		" ":       "",
		"#":       "#",
		"j\tk\nl": "jkl",
	} {
		if got := sanitizeKeys(in); got != want {
			t.Errorf("sanitizeKeys(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEffectiveKeys(t *testing.T) {
	def := shortcutDef{id: "archive", defaultKeys: "ae"}
	if got := effectiveKeys(map[string]string{}, def); got != "ae" {
		t.Errorf("no override = %q, want default", got)
	}
	if got := effectiveKeys(map[string]string{"archive": "x"}, def); got != "x" {
		t.Errorf("override = %q, want x", got)
	}
	// An explicit empty override disables the keys (≠ missing).
	if got := effectiveKeys(map[string]string{"archive": ""}, def); got != "" {
		t.Errorf("disabled = %q, want empty", got)
	}
}
