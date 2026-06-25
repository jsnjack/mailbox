package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestRelativeDate(t *testing.T) {
	now := time.Date(2026, time.June, 25, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		when time.Time
		want string
	}{
		{"today shows time", time.Date(2026, time.June, 25, 9, 30, 0, 0, time.UTC), "09:30"},
		{"earlier this week shows weekday", time.Date(2026, time.June, 22, 9, 0, 0, 0, time.UTC), "Mon"},
		{"this year shows month/day", time.Date(2026, time.January, 2, 9, 0, 0, 0, time.UTC), "Jan 2"},
		{"older shows year", time.Date(2024, time.December, 31, 9, 0, 0, 0, time.UTC), "Dec 31, 2024"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := relativeDate(c.when, now); got != c.want {
				t.Fatalf("relativeDate(%v) = %q, want %q", c.when, got, c.want)
			}
		})
	}
	if got := relativeDate(time.Time{}, now); got != "" {
		t.Fatalf("zero time should render empty, got %q", got)
	}
}

func TestReplyAllRecipients(t *testing.T) {
	m := model.Message{
		FromAddr: "alice@x.com",
		ToAddrs:  "Me <me@self.com>, Bob <bob@x.com>",
		CcAddrs:  "carol@x.com, me@self.com",
	}
	to, cc := replyAllRecipients(m, "me@self.com")

	if !strings.Contains(to, "alice@x.com") || !strings.Contains(to, "bob@x.com") {
		t.Fatalf("To missing expected recipients: %q", to)
	}
	if strings.Contains(to, "me@self.com") || strings.Contains(cc, "me@self.com") {
		t.Fatalf("self address should be excluded: to=%q cc=%q", to, cc)
	}
	if cc != "carol@x.com" {
		t.Fatalf("Cc = %q, want carol@x.com", cc)
	}
}

func TestEnsurePrefixes(t *testing.T) {
	if got := ensureRePrefix("Hello"); got != "Re: Hello" {
		t.Fatalf("ensureRePrefix = %q", got)
	}
	if got := ensureRePrefix("Re: Hello"); got != "Re: Hello" {
		t.Fatalf("ensureRePrefix double-prefixed: %q", got)
	}
	if got := ensureFwdPrefix("Hello"); got != "Fwd: Hello" {
		t.Fatalf("ensureFwdPrefix = %q", got)
	}
}
