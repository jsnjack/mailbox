package imapbackend

import (
	"fmt"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"github.com/jsnjack/mailbox/internal/logging"
	"golang.org/x/oauth2"
)

// Credential authenticates the IMAP and SMTP connections — either a password
// (IMAP LOGIN / SASL PLAIN) or an auto-refreshing OAuth token (SASL XOAUTH2).
// The caller picks the right one per account (password for Yahoo/iCloud/Fastmail
// app passwords; OAuth for Gmail-over-IMAP and Outlook/Office 365).
type Credential interface {
	imapLogin(cl *imapclient.Client) error
	smtpSASL() (sasl.Client, error)
}

// passwordCred authenticates with a username + password (or app password).
type passwordCred struct{ username, password string }

// PasswordAuth builds a password credential.
func PasswordAuth(username, password string) Credential {
	return passwordCred{username: username, password: password}
}

func (c passwordCred) imapLogin(cl *imapclient.Client) error {
	logging.Trace("imapbackend: imap login", "account", c.username, "cred", "password", "password_len", len(c.password))
	err := cl.Login(c.username, c.password).Wait()
	if err != nil {
		logging.Trace("imapbackend: imap login failed", "account", c.username, "cred", "password", "err", err)
		return err
	}
	logging.Trace("imapbackend: imap login ok", "account", c.username, "cred", "password")
	return nil
}

func (c passwordCred) smtpSASL() (sasl.Client, error) {
	return sasl.NewPlainClient("", c.username, c.password), nil
}

// oauthCred authenticates with SASL XOAUTH2 using a fresh access token. The token
// source auto-refreshes, so a long-lived backend keeps working as tokens rotate.
type oauthCred struct {
	username string
	ts       oauth2.TokenSource
}

// OAuthAuth builds an OAuth (XOAUTH2) credential from an auto-refreshing token
// source (see auth.OAuthTokenSourceFor).
func OAuthAuth(username string, ts oauth2.TokenSource) Credential {
	return &oauthCred{username: username, ts: ts}
}

func (c *oauthCred) accessToken() (string, error) {
	tok, err := c.ts.Token()
	if err != nil {
		logging.Trace("imapbackend: oauth token refresh failed", "account", c.username, "err", err)
		return "", fmt.Errorf("oauth token: %w", err)
	}
	logging.Trace("imapbackend: oauth token obtained", "account", c.username, "token_len", len(tok.AccessToken))
	return tok.AccessToken, nil
}

func (c *oauthCred) imapLogin(cl *imapclient.Client) error {
	logging.Trace("imapbackend: imap authenticate", "account", c.username, "cred", "xoauth2")
	at, err := c.accessToken()
	if err != nil {
		return err
	}
	err = cl.Authenticate(xoauth2Client(c.username, at))
	if err != nil {
		logging.Trace("imapbackend: imap authenticate failed", "account", c.username, "cred", "xoauth2", "err", err)
		return err
	}
	logging.Trace("imapbackend: imap authenticate ok", "account", c.username, "cred", "xoauth2")
	return nil
}

func (c *oauthCred) smtpSASL() (sasl.Client, error) {
	at, err := c.accessToken()
	if err != nil {
		return nil, err
	}
	return xoauth2Client(c.username, at), nil
}

// xoauth2 is a minimal SASL XOAUTH2 client — the OAuth mechanism Gmail and
// Outlook use for IMAP/SMTP. go-sasl ships PLAIN and OAUTHBEARER but not XOAUTH2,
// which is the one those providers actually require, so implement it: the single
// initial response is "user=<user>^Aauth=Bearer <token>^A^A".
type xoauth2 struct{ username, token string }

func xoauth2Client(username, token string) sasl.Client { return &xoauth2{username, token} }

func (x *xoauth2) Start() (string, []byte, error) {
	ir := []byte(fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", x.username, x.token))
	return "XOAUTH2", ir, nil
}

func (x *xoauth2) Next(challenge []byte) ([]byte, error) {
	// A server challenge here is the base64 error payload sent when the token is
	// rejected — there's nothing to answer with, so abort.
	return nil, fmt.Errorf("xoauth2: authentication failed: %s", challenge)
}
