package ui

import (
	"testing"
	"time"
)

func TestSignInAgePhrase(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		when time.Time
		want string
	}{
		{"same day", now.Add(-3 * time.Hour), "today"},
		{"one day", now.Add(-30 * time.Hour), "yesterday"},
		{"a week", now.Add(-7 * 24 * time.Hour), "7 days ago"},
		{"future clock skew", now.Add(2 * time.Hour), "today"},
	}
	for _, c := range cases {
		if got := signInAgePhrase(c.when, now); got != c.want {
			t.Errorf("%s: signInAgePhrase = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestSummaryLanguages guards the table the Summary language row is built from.
// The row preselects by matching the stored pref against these values, so a
// duplicate or a missing default would leave the dialog showing a language the
// app is not using.
func TestSummaryLanguages(t *testing.T) {
	if len(summaryLanguages) < 2 {
		t.Fatalf("summaryLanguages = %d entries, want the default plus at least one choice", len(summaryLanguages))
	}
	// The default (English) is the pref's zero value and leads the list.
	if summaryLanguages[0].label != "English" || summaryLanguages[0].value != "" {
		t.Fatalf("first entry = %+v, want {English, \"\"}", summaryLanguages[0])
	}
	seen := map[string]bool{}
	labels := map[string]bool{}
	for _, l := range summaryLanguages {
		if l.label == "" {
			t.Fatalf("entry %+v has no label", l)
		}
		if seen[l.value] {
			t.Fatalf("duplicate value %q — the row would preselect the wrong entry", l.value)
		}
		if labels[l.label] {
			t.Fatalf("duplicate label %q", l.label)
		}
		seen[l.value], labels[l.label] = true, true
	}
	// "Same as the email" must carry the sentinel the ai package understands,
	// not a language name a model would try to write in.
	if !seen["match"] {
		t.Fatalf("summaryLanguages offers no %q entry", "match")
	}
}
