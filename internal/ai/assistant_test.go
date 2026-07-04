package ai

import (
	"testing"
	"time"
)

// parseSnoozeSuggestion must accept the documented one-line form, reject the
// past, and treat "none"/garbage as no suggestion.
func TestParseSnoozeSuggestion(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.Local)
	if got, reason := parseSnoozeSuggestion("2026-07-09 09:00|day before the deadline", now); got.IsZero() ||
		got != time.Date(2026, 7, 9, 9, 0, 0, 0, time.Local) || reason != "day before the deadline" {
		t.Fatalf("valid suggestion = %v %q", got, reason)
	}
	// Extra model chatter after the first line is ignored.
	if got, _ := parseSnoozeSuggestion("2026-07-09 09:00|deadline\nSome explanation", now); got.IsZero() {
		t.Fatal("first-line parse failed")
	}
	for _, bad := range []string{"none", "", "tomorrow-ish", "2026-07-01 09:00|already past"} {
		if got, _ := parseSnoozeSuggestion(bad, now); !got.IsZero() {
			t.Fatalf("%q parsed to %v, want zero", bad, got)
		}
	}
}
