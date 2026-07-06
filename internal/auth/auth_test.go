package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
	"golang.org/x/oauth2"
)

func TestIsAuthError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"generic", errors.New("connection refused"), false},
		{"transient retrieve", &oauth2.RetrieveError{ErrorCode: "temporarily_unavailable"}, false},
		{"invalid_grant typed", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}, true},
		{"invalid_grant wrapped", fmt.Errorf("refresh token: %w", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}), true},
		{"invalid_grant string only", errors.New(`oauth2: "invalid_grant" Token has been expired or revoked.`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAuthError(tc.err); got != tc.want {
				t.Fatalf("IsAuthError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

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

// TestRefreshTimeout verifies the token-refresh POST is bounded: a refresh
// endpoint that never answers must surface an error instead of hanging Token()
// (and, through ReuseTokenSource's mutex, every API request) forever.
func TestRefreshTimeout(t *testing.T) {
	keyring.MockInit()

	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang // never respond
	}))
	defer srv.Close()
	defer close(hang)

	old := refreshTimeout
	refreshTimeout = 200 * time.Millisecond
	defer func() { refreshTimeout = old }()

	// Build the same chain TokenSource builds, with the token endpoint pointed
	// at the hanging server (TokenSource hardcodes Google's endpoint).
	const email = "user@example.com"
	conf := oauthConfig(ClientConfig{ClientID: "id", ClientSecret: "secret"}, "")
	conf.Endpoint = oauth2.Endpoint{TokenURL: srv.URL}
	seed := &oauth2.Token{RefreshToken: "rt-123"} // no expiry → forces a refresh
	base := &persistingTokenSource{service: keyringService, email: email, last: "rt-123", src: conf.TokenSource(refreshContext(context.Background()), seed)}
	ts := oauth2.ReuseTokenSource(seed, base)

	done := make(chan error, 1)
	go func() {
		_, err := ts.Token()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a timeout error from a hanging refresh endpoint")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Token() hung past the refresh timeout — refresh POST is unbounded")
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
