package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/zalando/go-keyring"
	"golang.org/x/oauth2"
)

// keyringService is the Secret Service collection key under which refresh tokens
// are stored, one item per account email.
const keyringService = "mailbox"

// SaveRefreshToken stores the account's refresh token in the OS keyring.
func SaveRefreshToken(email, refreshToken string) error {
	if err := keyring.Set(keyringService, email, refreshToken); err != nil {
		return fmt.Errorf("save refresh token for %q: %w", email, err)
	}
	return nil
}

// LoadRefreshToken reads the account's refresh token from the OS keyring.
func LoadRefreshToken(email string) (string, error) {
	rt, err := keyring.Get(keyringService, email)
	if err != nil {
		return "", fmt.Errorf("load refresh token for %q: %w", email, err)
	}
	return rt, nil
}

// DeleteRefreshToken removes the account's refresh token from the OS keyring.
func DeleteRefreshToken(email string) error {
	if err := keyring.Delete(keyringService, email); err != nil {
		return fmt.Errorf("delete refresh token for %q: %w", email, err)
	}
	return nil
}

// TokenSource returns an auto-refreshing token source for an account whose
// refresh token lives in the keyring. expiry primes the source with the stored
// access-token expiry so a still-valid token isn't refreshed unnecessarily.
// Rotated refresh tokens are written back to the keyring.
func TokenSource(ctx context.Context, cc ClientConfig, email string, expiry time.Time) (oauth2.TokenSource, error) {
	rt, err := LoadRefreshToken(email)
	if err != nil {
		return nil, err
	}
	conf := oauthConfig(cc, "") // redirect is unused for refresh
	seed := &oauth2.Token{RefreshToken: rt, Expiry: expiry}
	base := &persistingTokenSource{email: email, last: rt, src: conf.TokenSource(ctx, seed)}
	return oauth2.ReuseTokenSource(seed, base), nil
}

// persistingTokenSource writes a rotated refresh token back to the keyring so
// the new value survives restarts. Google occasionally rotates refresh tokens.
type persistingTokenSource struct {
	email string
	last  string
	src   oauth2.TokenSource
}

// Token delegates to the wrapped source and persists any changed refresh token.
func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err != nil {
		return nil, fmt.Errorf("refresh token for %q: %w", p.email, err)
	}
	if tok.RefreshToken != "" && tok.RefreshToken != p.last {
		if err := SaveRefreshToken(p.email, tok.RefreshToken); err != nil {
			return nil, err
		}
		p.last = tok.RefreshToken
	}
	return tok, nil
}
