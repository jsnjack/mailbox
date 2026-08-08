// Package auth implements the OAuth2 installed-app loopback flow for connecting
// Gmail accounts and stores refresh tokens in the OS keyring. It imports no GTK
// code. The Google Cloud "Desktop app" OAuth client ID/secret are supplied by
// the caller (ClientConfig); for an installed app the secret is not truly
// secret — PKCE provides the real protection.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Gmail OAuth scopes requested at login: read+modify (label/archive/star/delete)
// and send. Together these cover the full v1 client.
const (
	ScopeModify = "https://www.googleapis.com/auth/gmail.modify"
	ScopeSend   = "https://www.googleapis.com/auth/gmail.send"
)

// DefaultScopes are the scopes requested during Login. The OIDC scopes let the
// app read the account's email address and display name.
var DefaultScopes = []string{ScopeModify, ScopeSend, "openid", "email", "profile"}

// ClientConfig holds the Google Cloud "Desktop app" OAuth client credentials.
type ClientConfig struct {
	ClientID     string
	ClientSecret string
}

// oauthConfig builds an oauth2.Config for the given loopback redirect URL.
func oauthConfig(cc ClientConfig, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     cc.ClientID,
		ClientSecret: cc.ClientSecret,
		Scopes:       DefaultScopes,
		Endpoint:     google.Endpoint,
		RedirectURL:  redirectURL,
	}
}

// randomState returns a URL-safe random string for CSRF protection.
func randomState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Login runs the installed-app loopback OAuth flow for the Gmail REST scopes:
// it starts a local server on a random loopback port, opens the system browser
// to Google's consent page, captures the authorization code on the callback, and
// exchanges it (with PKCE) for a token. AccessTypeOffline + consent prompt
// guarantee a refresh token.
func Login(ctx context.Context, cc ClientConfig) (*oauth2.Token, error) {
	return LoginWithBrowserFallback(ctx, cc, nil)
}

// LoginWithBrowserFallback is Login with a callback that receives the consent
// URL when the system browser cannot be launched.
func LoginWithBrowserFallback(ctx context.Context, cc ClientConfig, fallback func(string)) (*oauth2.Token, error) {
	return loginWithConfig(ctx, oauthConfig(cc, ""), fallback,
		oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
}

// loginWithConfig runs the generic loopback+PKCE flow for any provider's
// oauth2.Config (Google REST/IMAP, Microsoft). The RedirectURL is filled in with
// the chosen loopback port. authOpts are extra AuthCodeURL options (offline
// access, consent prompt).
func loginWithConfig(ctx context.Context, conf *oauth2.Config, fallback func(string), authOpts ...oauth2.AuthCodeOption) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on loopback: %w", err)
	}
	defer func() { _ = ln.Close() }()

	port := ln.Addr().(*net.TCPAddr).Port
	conf.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	state, err := randomState()
	if err != nil {
		return nil, err
	}
	verifier := oauth2.GenerateVerifier()
	opts := append([]oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)}, authOpts...)
	authURL := conf.AuthCodeURL(state, opts...)
	logging.TraceContext(ctx, "auth: oauth loopback start", "redirect", conf.RedirectURL, "scopes", conf.Scopes)

	type result struct {
		code string
		err  error
	}
	resCh := make(chan result, 1)
	publish := func(res result) {
		// Browsers can retry or prefetch a callback. Never let a duplicate request
		// pin an HTTP handler (and therefore Server.Shutdown) after the first result
		// has already won or the dialog was closed.
		select {
		case resCh <- res:
		case <-ctx.Done():
		default:
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		logging.TraceContext(ctx, "auth: oauth callback received")
		if e := q.Get("error"); e != "" {
			http.Error(w, "Authorization failed. You can close this tab.", http.StatusBadRequest)
			logging.TraceContext(ctx, "auth: oauth authorization denied", "reason", e)
			publish(result{err: fmt.Errorf("authorization denied: %s", e)})
			return
		}
		if q.Get("state") != state {
			http.Error(w, "State mismatch. You can close this tab.", http.StatusBadRequest)
			logging.TraceContext(ctx, "auth: oauth state mismatch")
			publish(result{err: errors.New("state mismatch (possible CSRF)")})
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "Authorization code missing. You can close this tab.", http.StatusBadRequest)
			publish(result{err: errors.New("authorization callback contained no code")})
			return
		}
		logging.TraceContext(ctx, "auth: oauth state validated", "hasCode", true)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		_, _ = w.Write([]byte("<html><body>Signed in. You can close this tab and return to mailbox.</body></html>"))
		publish(result{code: code})
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.TraceContext(ctx, "auth: loopback server", "err", err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	go func() {
		if err := openBrowser(authURL); err != nil {
			// Not fatal — the loopback server keeps waiting while the user opens
			// the URL manually.
			slog.Default().Warn("could not open browser automatically; open this URL to sign in", "url", authURL)
			if fallback != nil {
				fallback(authURL)
			}
		}
	}()

	select {
	case <-ctx.Done():
		logging.TraceContext(ctx, "auth: oauth loopback canceled", "err", ctx.Err())
		return nil, ctx.Err()
	case res := <-resCh:
		if res.err != nil {
			return nil, res.err
		}
		tok, err := conf.Exchange(refreshContext(ctx), res.code, oauth2.VerifierOption(verifier))
		if err != nil {
			logging.TraceContext(ctx, "auth: oauth code exchange failed", "err", err)
			return nil, fmt.Errorf("exchange authorization code: %w", err)
		}
		logging.TraceContext(ctx, "auth: oauth code exchange ok", "expiry", tok.Expiry, "tokenType", tok.TokenType, "hasRefresh", tok.RefreshToken != "")
		return tok, nil
	}
}

// openBrowser launches the system browser at url via xdg-open.
func openBrowser(url string) error {
	if err := exec.Command("xdg-open", url).Run(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	return nil
}
