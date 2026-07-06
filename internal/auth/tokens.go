package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/zalando/go-keyring"
	"golang.org/x/oauth2"
)

// IsAuthError reports whether err is a permanent OAuth failure — the refresh
// token was revoked or expired ("invalid_grant") — meaning the account needs
// interactive re-authentication and won't recover on its own. Transient network
// or 5xx errors are not auth errors. It looks through wrapping (the token error
// is wrapped by the token source and again by the API client), with a string
// fallback for layers that don't wrap with %w.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
		logging.Trace("auth: IsAuthError classified revoked/expired", "via", "RetrieveError", "code", re.ErrorCode)
		return true
	}
	if strings.Contains(err.Error(), "invalid_grant") {
		logging.Trace("auth: IsAuthError classified revoked/expired", "via", "string")
		return true
	}
	return false
}

// keyringService is the Secret Service collection key under which refresh tokens
// are stored, one item per account email.
const keyringService = "mailbox"

// refreshTimeout bounds the whole token-refresh POST. Every API request passes
// through oauth2.Transport.RoundTrip → Source.Token(), which takes no context —
// neither the per-request context nor the transport stall watchdog can cancel
// it, and oauth2.ReuseTokenSource serializes all callers behind one mutex. An
// unbounded refresh on a half-open connection (suspend/resume, network switch)
// therefore wedges every request for the account indefinitely. The refresh is a
// tiny POST, so a whole-request timeout is safe here (unlike the API transport,
// where it would kill large attachment transfers — see gmailapi.NewService).
// A var, not a const, so tests can shrink it.
var refreshTimeout = 30 * time.Second

// refreshContext returns ctx with a dedicated bounded HTTP client for the
// oauth2 token refresh (oauth2 uses http.DefaultClient — no timeout — otherwise).
func refreshContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: refreshTimeout})
}

// SaveRefreshToken stores the account's refresh token in the OS keyring.
func SaveRefreshToken(email, refreshToken string) error {
	if err := keyring.Set(keyringService, email, refreshToken); err != nil {
		logging.Trace("auth: keyring save refresh token failed", "service", keyringService, "email", email, "err", err)
		return fmt.Errorf("save refresh token for %q: %w", email, err)
	}
	logging.Trace("auth: keyring save refresh token", "service", keyringService, "email", email, "tokenLen", len(refreshToken))
	return nil
}

// LoadRefreshToken reads the account's refresh token from the OS keyring.
func LoadRefreshToken(email string) (string, error) {
	rt, err := keyring.Get(keyringService, email)
	if err != nil {
		logging.Trace("auth: keyring load refresh token not found", "service", keyringService, "email", email, "err", err)
		return "", fmt.Errorf("load refresh token for %q: %w", email, err)
	}
	logging.Trace("auth: keyring load refresh token", "service", keyringService, "email", email, "tokenLen", len(rt))
	return rt, nil
}

// DeleteRefreshToken removes the account's refresh token from the OS keyring.
func DeleteRefreshToken(email string) error {
	if err := keyring.Delete(keyringService, email); err != nil {
		logging.Trace("auth: keyring delete refresh token failed", "service", keyringService, "email", email, "err", err)
		return fmt.Errorf("delete refresh token for %q: %w", email, err)
	}
	logging.Trace("auth: keyring delete refresh token", "service", keyringService, "email", email)
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
	base := &persistingTokenSource{service: keyringService, email: email, last: rt, src: conf.TokenSource(refreshContext(ctx), seed)}
	logging.Trace("auth: token source built", "service", keyringService, "email", email, "seedExpiry", expiry)
	return oauth2.ReuseTokenSource(seed, base), nil
}

// persistingTokenSource writes a rotated refresh token back to the keyring
// (under service, keyed by email) so the new value survives restarts. Google and
// Microsoft both rotate refresh tokens.
type persistingTokenSource struct {
	service string
	email   string
	last    string
	src     oauth2.TokenSource
}

// Token delegates to the wrapped source and persists any changed refresh token.
func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err != nil {
		logging.Trace("auth: token refresh failed", "service", p.service, "email", p.email, "authError", IsAuthError(err), "err", err)
		return nil, fmt.Errorf("refresh token for %q: %w", p.email, err)
	}
	rotated := tok.RefreshToken != "" && tok.RefreshToken != p.last
	logging.Trace("auth: token refreshed", "service", p.service, "email", p.email, "expiry", tok.Expiry, "rotated", rotated)
	if rotated {
		if err := keyring.Set(p.service, p.email, tok.RefreshToken); err != nil {
			logging.Trace("auth: rotated token write-back failed", "service", p.service, "email", p.email, "err", err)
			return nil, fmt.Errorf("persist rotated token for %q: %w", p.email, err)
		}
		logging.Trace("auth: rotated token written back", "service", p.service, "email", p.email, "tokenLen", len(tok.RefreshToken))
		p.last = tok.RefreshToken
	}
	return tok, nil
}
