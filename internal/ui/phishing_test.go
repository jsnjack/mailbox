package ui

import "testing"

func TestSenderNameSpoofed(t *testing.T) {
	tests := []struct {
		name, fromName, fromAddr string
		want                     bool
	}{
		{"clean name", "Alice Smith", "alice@x.com", false},
		{"matching embedded address", "billing@shop.com", "noreply@shop.com", false},
		{"spoofed brand in name", "security@paypal.com", "attacker@evil.example", true},
		{"subdomain still same site", "no-reply@mail.bank.com", "alerts@bank.com", false},
		{"no email in name", "Support Team", "x@evil.example", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := senderNameSpoofed(tt.fromName, tt.fromAddr); got != tt.want {
				t.Fatalf("senderNameSpoofed(%q,%q)=%v want %v", tt.fromName, tt.fromAddr, got, tt.want)
			}
		})
	}
}

func TestLinkTextMismatch(t *testing.T) {
	tests := []struct {
		name, text, href string
		want             bool
	}{
		{"plain text link", "Click here", "https://paypal.com/login", false},
		{"matching domain", "paypal.com", "https://paypal.com/login", false},
		{"matching subdomain", "paypal.com", "https://secure.paypal.com/", false},
		{"deceptive", "paypal.com", "https://evil.example/login", true},
		{"deceptive full url", "https://paypal.com", "http://evil.example", true},
		{"mailto href", "write us", "mailto:a@x.com", false},
		{"text not a url", "Open your account", "https://evil.example", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := linkTextMismatch(tt.text, tt.href); got != tt.want {
				t.Fatalf("linkTextMismatch(%q,%q)=%v want %v", tt.text, tt.href, got, tt.want)
			}
		})
	}
}

func TestHasDeceptiveLink(t *testing.T) {
	clean := `<p>Hi</p><a href="https://example.com/x">example.com</a>`
	if hasDeceptiveLink(clean) {
		t.Fatal("clean body flagged")
	}
	bad := `<p>Account alert</p><a href="https://evil.example/login">paypal.com</a>`
	if !hasDeceptiveLink(bad) {
		t.Fatal("deceptive link not flagged")
	}
}
