package auth

import (
	"slices"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestRefreshTokenRoundTrip(t *testing.T) {
	keyring.MockInit() // in-memory keyring; no Secret Service needed

	const email = "user@example.com"
	if err := SaveRefreshToken(email, "rt-123"); err != nil {
		t.Fatalf("SaveRefreshToken: %v", err)
	}
	got, err := LoadRefreshToken(email)
	if err != nil {
		t.Fatalf("LoadRefreshToken: %v", err)
	}
	if got != "rt-123" {
		t.Fatalf("got %q, want %q", got, "rt-123")
	}
	if err := DeleteRefreshToken(email); err != nil {
		t.Fatalf("DeleteRefreshToken: %v", err)
	}
	if _, err := LoadRefreshToken(email); err == nil {
		t.Fatal("expected error loading deleted token")
	}
}

func TestRandomState(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		s, err := randomState()
		if err != nil {
			t.Fatalf("randomState: %v", err)
		}
		if s == "" {
			t.Fatal("empty state")
		}
		if seen[s] {
			t.Fatalf("duplicate state %q", s)
		}
		seen[s] = true
	}
}

func TestOAuthConfig(t *testing.T) {
	cc := ClientConfig{ClientID: "id", ClientSecret: "secret"}
	conf := oauthConfig(cc, "http://127.0.0.1:1234/callback")

	if conf.ClientID != "id" || conf.ClientSecret != "secret" {
		t.Fatalf("credentials not set: %+v", conf)
	}
	if conf.RedirectURL != "http://127.0.0.1:1234/callback" {
		t.Fatalf("redirect: %q", conf.RedirectURL)
	}
	for _, want := range []string{ScopeModify, ScopeSend} {
		if !slices.Contains(conf.Scopes, want) {
			t.Fatalf("scopes %v missing %q", conf.Scopes, want)
		}
	}
}
