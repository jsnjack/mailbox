package ui

import "testing"

func TestEditableBoundary(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // the editable prefix (body[:boundary])
	}{
		{"no markers", "Hello there, how are you?", "Hello there, how are you?"},
		{
			"signature then quote",
			"Hi Sam,\n\nThanks!\n\n-- \nYauhen\n\nOn Jan 2, 2026, X wrote:\n> old\n",
			"Hi Sam,\n\nThanks!",
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
	if got := composeBodyWithSignature("", "Yauhen"); got != "\n\n-- \nYauhen" {
		t.Fatalf("new message sig = %q", got)
	}
	if got := composeBodyWithSignature("> quoted", "Yauhen"); got != "\n\n-- \nYauhen\n\n> quoted" {
		t.Fatalf("reply sig placement = %q", got)
	}
}
