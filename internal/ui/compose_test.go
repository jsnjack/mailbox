package ui

import (
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestEditableBoundary(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // the editable prefix (body[:boundary])
	}{
		{"no markers", "Hello there, how are you?", "Hello there, how are you?"},
		{
			// A bare "-- " (e.g. a user types it in their own signature) still marks
			// the boundary defensively.
			"explicit delimiter then quote",
			"Hi Sam,\n\nThanks!\n\n-- \nYauhen\n\nOn Jan 2, 2026, X wrote:\n> old\n",
			"Hi Sam,\n\nThanks!",
		},
		{
			// The plain sign-off composeBodyWithSignature now inserts has no
			// delimiter, so it stays in the editable region; only the quote is
			// preserved.
			"plain sign-off then quote",
			"Thanks!\n\nBest,\nYauhen\n\nOn Jan 2, 2026, X wrote:\n> old\n",
			"Thanks!\n\nBest,\nYauhen",
		},
		{
			"quote, no signature",
			"My reply here.\n\nOn Jan 2, 2026, X wrote:\n> quoted\n> more\n",
			"My reply here.",
		},
		{
			"bare quoted lines",
			"See below.\n> quoted bit\n",
			"See below.",
		},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.body[:editableBoundary(tt.body)]
			if got != tt.want {
				t.Fatalf("editable prefix = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMentionsAttachment(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"plain mention", "Hi, please find the report attached.", true},
		{"attachment word", "See the attachment for details.", true},
		{"enclosed", "The invoice is enclosed.", true},
		{"none", "Thanks, talk soon!", false},
		{"only in quote", "Sure.\n\n> Please find attached the file", false},
		{"mention outside quote wins", "Here it is, attached.\n\n> earlier text", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mentionsAttachment(tt.body); got != tt.want {
				t.Fatalf("mentionsAttachment(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestComposeBodyWithSignature(t *testing.T) {
	if got := composeBodyWithSignature("", ""); got != "" {
		t.Fatalf("no sig, empty body = %q", got)
	}
	if got := composeBodyWithSignature("quote", ""); got != "quote" {
		t.Fatalf("no sig keeps quote = %q", got)
	}
	if got := composeBodyWithSignature("", "Best,\nYauhen"); got != "\n\nBest,\nYauhen" {
		t.Fatalf("new message sig = %q", got)
	}
	if got := composeBodyWithSignature("> quoted", "Best,\nYauhen"); got != "\n\nBest,\nYauhen\n\n> quoted" {
		t.Fatalf("reply sig placement = %q", got)
	}
}

func TestWithOwnAccounts(t *testing.T) {
	w := &window{
		deps: Deps{Accounts: []AccountInfo{
			{ID: 1, Email: "me@gmail.com"},
			{ID: 2, Email: "me@work.com"},
		}},
		accountNames: map[string]string{"me@work.com": "Work"},
	}
	past := []model.Contact{
		{Address: "friend@x.com", Name: "Friend"},
		{Address: "ME@work.com"}, // same as account 2 (case-insensitive) → deduped
	}
	got := w.withOwnAccounts(past)

	if len(got) != 3 {
		t.Fatalf("got %d contacts, want 3: %+v", len(got), got)
	}
	// Own accounts come first, in registration order.
	if got[0].Address != "me@gmail.com" || got[1].Address != "me@work.com" {
		t.Fatalf("own accounts not first: %+v", got)
	}
	// The assigned display name is used.
	if got[1].Name != "Work" {
		t.Errorf("account 2 name = %q, want Work", got[1].Name)
	}
	// The past correspondent survives; the dup of an own account does not.
	if got[2].Address != "friend@x.com" {
		t.Errorf("third = %q, want friend@x.com", got[2].Address)
	}
}
