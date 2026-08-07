package ui

import (
	"errors"
	"testing"

	"github.com/jsnjack/mailbox/internal/config"
)

func TestFormError(t *testing.T) {
	if got := formError(errors.New("enter a login username")); got != "Enter a login username." {
		t.Fatalf("formError = %q", got)
	}
}

func TestValidateAccountSettings(t *testing.T) {
	valid := config.IMAPAccount{
		Email: "me@example.com", Username: "mailbox-login",
		IMAPHost: "imap.example.com", IMAPPort: 993, IMAPSecurity: "tls",
		SMTPHost: "smtp.example.com", SMTPPort: 587, SMTPSecurity: "starttls",
		Auth: config.AuthPassword,
	}
	if err := validateAccountSettings(valid); err != nil {
		t.Fatalf("valid custom account: %v", err)
	}

	tests := []struct {
		name string
		edit func(*config.IMAPAccount)
	}{
		{"bad email", func(a *config.IMAPAccount) { a.Email = "not an email" }},
		{"display name is not an account address", func(a *config.IMAPAccount) { a.Email = "Me <me@example.com>" }},
		{"missing username", func(a *config.IMAPAccount) { a.Username = "" }},
		{"missing IMAP host", func(a *config.IMAPAccount) { a.IMAPHost = "" }},
		{"invalid IMAP port", func(a *config.IMAPAccount) { a.IMAPPort = 70000 }},
		{"missing SMTP host", func(a *config.IMAPAccount) { a.SMTPHost = "" }},
		{"invalid SMTP port", func(a *config.IMAPAccount) { a.SMTPPort = 0 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := valid
			tc.edit(&a)
			if err := validateAccountSettings(a); err == nil {
				t.Fatal("validateAccountSettings returned nil")
			}
		})
	}
}

func TestSecuritySelection(t *testing.T) {
	if got := securityIndex("starttls"); got != 1 {
		t.Fatalf("securityIndex(starttls) = %d, want 1", got)
	}
	if got := securityValue(1); got != "starttls" {
		t.Fatalf("securityValue(1) = %q, want starttls", got)
	}
	if got := securityValue(securityIndex("tls")); got != "tls" {
		t.Fatalf("TLS round trip = %q", got)
	}
	if got := securityValue(securityIndex("none")); got != "none" {
		t.Fatalf("none round trip = %q", got)
	}
}
