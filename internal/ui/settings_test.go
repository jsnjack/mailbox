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
