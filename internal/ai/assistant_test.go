package ai

import (
	"testing"
	"time"
)

// parseSnoozeSuggestions must accept up to three documented lines, skip the
// past, duplicates, and garbage, and treat "none" as no suggestions.
func TestParseSnoozeSuggestions(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.Local)

	got := parseSnoozeSuggestions(
		"2026-07-10 13:00|an hour before the meeting\n"+
			"2026-07-09 09:00|the day before\n"+
			"2026-07-09 09:00|duplicate time\n"+
			"2026-07-01 09:00|already past\n"+
			"gibberish\n"+
			"2026-07-11 09:00|third\n"+
			"2026-07-12 09:00|would be fourth", now)
	if len(got) != 3 {
		t.Fatalf("got %d suggestions, want 3: %+v", len(got), got)
	}
	if got[0].At != time.Date(2026, 7, 10, 13, 0, 0, 0, time.Local) || got[0].Reason != "an hour before the meeting" {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1].Reason != "the day before" || got[2].Reason != "third" {
		t.Fatalf("order/dedup wrong: %+v", got)
	}

	for _, bad := range []string{"none", "", "tomorrow-ish"} {
		if got := parseSnoozeSuggestions(bad, now); len(got) != 0 {
			t.Fatalf("%q parsed to %+v, want none", bad, got)
		}
	}
}
