package ui

import (
	"strings"
	"testing"
)

func TestRecipientValidation(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		count      int
		bad        bool
	}{
		{name: "empty"},
		{name: "single", text: "a@example.com", count: 1},
		{name: "quoted comma", text: `"Doe, Jane" <jane@example.com>, other@example.com, `, count: 2},
		{name: "incomplete", text: "name@", bad: true},
		{name: "missing domain", text: "name", bad: true},
		{name: "invalid second", text: "valid@example.com, not an address", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRecipients(tc.text)
			if (err != nil) != tc.bad || len(got) != tc.count {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}

func TestRecipientSummary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields []string
		want   string
	}{
		{"bcc only", []string{"", "", "secret@example.com"}, "1 recipient"},
		{"deduplicated", []string{"a@example.com", "a@example.com, b@example.com"}, "2 recipients"},
		{"invalid", []string{"bad address"}, "Check recipient addresses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recipientSummary(tc.fields...); got != tc.want {
				t.Fatalf("got %q; want %q", got, tc.want)
			}
		})
	}
}

func TestShortcutConflict(t *testing.T) {
	for _, tc := range []struct {
		name, id, keys string
		overrides      map[string]string
		want           string
	}{
		{name: "own binding", id: "next", keys: "j"},
		{name: "conflict", id: "prev", keys: "j", want: "Next conversation"},
		{name: "reserved", id: "next", keys: "?", want: "reserved"},
		{name: "disabled", id: "prev", keys: "j", overrides: map[string]string{"next": ""}},
		{name: "custom conflict", id: "prev", keys: "z", overrides: map[string]string{"next": "z"}, want: "Next conversation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := shortcutConflict(tc.overrides, tc.id, tc.keys)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("conflict = %q", got)
			}
		})
	}
}

func TestSearchWithFilters(t *testing.T) {
	for _, tc := range []struct {
		name, q, from, after, before string
		unread, attachment           bool
		want                         string
		bad                          bool
	}{
		{name: "combined", q: "budget", from: "Jane Doe", after: "2026-01-01", before: "2026-02-01", unread: true, attachment: true, want: `budget from:"Jane Doe" after:2026-01-01 before:2026-02-01 is:unread has:attachment`},
		{name: "replace existing", q: `budget from:"Old Sender" is:unread`, from: "new@example.com", want: `budget from:"new@example.com"`},
		{name: "preserve quoted phrase", q: `"from:someone is:unread" subject:report`, want: `"from:someone is:unread" subject:report`},
		{name: "invalid date", after: "2026-02-30", bad: true},
		{name: "reversed dates", after: "2026-02-01", before: "2026-01-01", bad: true},
		{name: "injection", from: `a" is:unread`, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := searchWithFilters(tc.q, tc.from, tc.after, tc.before, tc.unread, tc.attachment)
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestValidateRecipientFields(t *testing.T) {
	for _, tc := range []struct {
		name, to, cc, bcc string
		bad               bool
	}{
		{name: "bcc only", bcc: "private@example.com"},
		{name: "cc only", cc: "cc@example.com"},
		{name: "empty", bad: true},
		{name: "invalid hidden recipient", to: "valid@example.com", bcc: "bad", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateRecipientFields(tc.to, tc.cc, tc.bcc); (got != "") != tc.bad {
				t.Fatalf("validation = %q", got)
			}
		})
	}
}
