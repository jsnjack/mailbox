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
	return loginWithConfig(ctx, oauthConfig(cc, ""),
		oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
}

// loginWithConfig runs the generic loopback+PKCE flow for any provider's
// oauth2.Config (Google REST/IMAP, Microsoft). The RedirectURL is filled in with
// the chosen loopback port. authOpts are extra AuthCodeURL options (offline
// access, consent prompt).
func loginWithConfig(ctx context.Context, conf *oauth2.Config, authOpts ...oauth2.AuthCodeOption) (*oauth2.Token, error) {
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

	type result struct {
		code string
		err  error
	}
	resCh := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			http.Error(w, "Authorization failed. You can close this tab.", http.StatusBadRequest)
			resCh <- result{err: fmt.Errorf("authorization denied: %s", e)}
			return
		}
		if q.Get("state") != state {
			http.Error(w, "State mismatch. You can close this tab.", http.StatusBadRequest)
			resCh <- result{err: errors.New("state mismatch (possible CSRF)")}
			return
		}
		_, _ = w.Write([]byte("<html><body>Signed in. You can close this tab and return to mailbox.</body></html>"))
		resCh <- result{code: q.Get("code")}
	})

	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Log(ctx, logging.LevelTrace, "loopback server", "err", err)
		}
	}()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	if err := openBrowser(authURL); err != nil {
		// Not fatal — the user can open the URL manually.
		slog.Default().Warn("could not open browser automatically; open this URL to sign in", "url", authURL)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-resCh:
		if res.err != nil {
			return nil, res.err
		}
		tok, err := conf.Exchange(ctx, res.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("exchange authorization code: %w", err)
		}
		return tok, nil
	}
}

// openBrowser launches the system browser at url via xdg-open.
func openBrowser(url string) error {
	if err := exec.Command("xdg-open", url).Start(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	return nil
}
