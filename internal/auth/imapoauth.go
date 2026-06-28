package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/zalando/go-keyring"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/microsoft"
)

// ScopeMailGoogle is Gmail's full-mailbox scope, required for IMAP/SMTP access
// (the REST scopes don't grant it). Connecting Gmail over IMAP re-consents here.
const ScopeMailGoogle = "https://mail.google.com/"

// MicrosoftScopes are the Outlook/Office 365 IMAP+SMTP OAuth scopes;
// offline_access yields a refresh token.
var MicrosoftScopes = []string{
	"https://outlook.office.com/IMAP.AccessAsUser.All",
	"https://outlook.office.com/SMTP.Send",
	"offline_access",
}

// IMAPKeyringService is the keyring collection for IMAP-OAuth refresh tokens,
// kept separate from the Gmail-REST tokens under "mailbox" (a Gmail address may
// hold both a REST token and a full-mail IMAP token).
const IMAPKeyringService = "mailbox-imap"

// googleMailConfig builds a Google OAuth config scoped for IMAP/SMTP.
func googleMailConfig(cc ClientConfig) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     cc.ClientID,
		ClientSecret: cc.ClientSecret,
		Scopes:       []string{ScopeMailGoogle},
		Endpoint:     google.Endpoint,
	}
}

// microsoftConfig builds a Microsoft OAuth config for the multi-tenant "common"
// authority. Outlook desktop OAuth uses a public client (no secret); PKCE is the
// protection. clientID is an Azure app registration's public client id.
func microsoftConfig(clientID string) *oauth2.Config {
	return &oauth2.Config{
		ClientID: clientID,
		Scopes:   MicrosoftScopes,
		Endpoint: microsoft.AzureADEndpoint("common"),
	}
}

// LoginGoogleMail runs the loopback OAuth flow for Gmail-over-IMAP (full-mail
// scope) and returns a token whose refresh token the caller stores under
// IMAPKeyringService.
func LoginGoogleMail(ctx context.Context, cc ClientConfig) (*oauth2.Token, error) {
	return loginWithConfig(ctx, googleMailConfig(cc),
		oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
}

// LoginMicrosoft runs the loopback OAuth flow for Outlook/Office 365.
func LoginMicrosoft(ctx context.Context, clientID string) (*oauth2.Token, error) {
	return loginWithConfig(ctx, microsoftConfig(clientID))
}

// OAuthTokenSourceFor returns an auto-refreshing token source for an IMAP-OAuth
// account whose refresh token lives in the keyring under service. Rotated tokens
// are written back. Used to build an imapbackend OAuth credential.
func OAuthTokenSourceFor(ctx context.Context, conf *oauth2.Config, service, email string, expiry time.Time) (oauth2.TokenSource, error) {
	rt, err := keyring.Get(service, email)
	if err != nil {
		return nil, fmt.Errorf("load oauth refresh token for %q: %w", email, err)
	}
	seed := &oauth2.Token{RefreshToken: rt, Expiry: expiry}
	base := &persistingTokenSource{service: service, email: email, last: rt, src: conf.TokenSource(ctx, seed)}
	return oauth2.ReuseTokenSource(seed, base), nil
}

// GoogleMailTokenSource is OAuthTokenSourceFor for a Gmail-over-IMAP account.
func GoogleMailTokenSource(ctx context.Context, cc ClientConfig, email string, expiry time.Time) (oauth2.TokenSource, error) {
	return OAuthTokenSourceFor(ctx, googleMailConfig(cc), IMAPKeyringService, email, expiry)
}

// MicrosoftTokenSource is OAuthTokenSourceFor for an Outlook/Office 365 account.
func MicrosoftTokenSource(ctx context.Context, clientID, email string, expiry time.Time) (oauth2.TokenSource, error) {
	return OAuthTokenSourceFor(ctx, microsoftConfig(clientID), IMAPKeyringService, email, expiry)
}

// SaveIMAPSecret stores an IMAP account's secret (an app password, or an OAuth
// refresh token) in the keyring under IMAPKeyringService, keyed by email.
func SaveIMAPSecret(email, secret string) error {
	if err := keyring.Set(IMAPKeyringService, email, secret); err != nil {
		return fmt.Errorf("save imap secret for %q: %w", email, err)
	}
	return nil
}

// LoadIMAPSecret reads an IMAP account's secret from the keyring.
func LoadIMAPSecret(email string) (string, error) {
	s, err := keyring.Get(IMAPKeyringService, email)
	if err != nil {
		return "", fmt.Errorf("load imap secret for %q: %w", email, err)
	}
	return s, nil
}

// DeleteIMAPSecret removes an IMAP account's secret from the keyring.
func DeleteIMAPSecret(email string) error {
	if err := keyring.Delete(IMAPKeyringService, email); err != nil {
		return fmt.Errorf("delete imap secret for %q: %w", email, err)
	}
	return nil
}
