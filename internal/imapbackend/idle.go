package imapbackend

import (
	"context"
	"errors"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jsnjack/mailbox/internal/logging"
)

// idleRefresh is how often the IDLE is torn down and re-armed. RFC 2177 says a
// server may drop IDLE after ~30 min, so refresh comfortably under that.
const idleRefresh = 25 * time.Minute

// idleRetry is the backoff before reconnecting after an IDLE connection error.
const idleRetry = 30 * time.Second

// Watch holds a dedicated connection in IDLE on the INBOX and calls onChange when
// the server reports new or expunged mail, so the app syncs promptly instead of
// waiting for the poll. It reconnects on error and returns when ctx is cancelled
// or the server lacks IDLE (the poll loop then covers changes). The IDLE
// connection is separate from the request pool.
func (b *Backend) Watch(ctx context.Context, onChange func()) {
	logging.TraceContext(ctx, "imapbackend: watch start", "account", b.cfg.Email)
	for ctx.Err() == nil {
		if err := b.idleOnce(ctx, onChange); err != nil && ctx.Err() == nil {
			if errors.Is(err, errNoIdle) {
				logging.TraceContext(ctx, "imapbackend: watch fallback to poll (no IDLE)", "account", b.cfg.Email)
				return // server can't IDLE — leave it to the poll loop
			}
			logging.TraceContext(ctx, "imapbackend: watch idle error, retrying", "account", b.cfg.Email, "retry", idleRetry, "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(idleRetry):
			}
		}
	}
	logging.TraceContext(ctx, "imapbackend: watch stop", "account", b.cfg.Email, "err", ctx.Err())
}

// errNoIdle signals the server doesn't advertise the IDLE capability.
var errNoIdle = errorString("imap: server does not support IDLE")

type errorString string

func (e errorString) Error() string { return string(e) }

// idleOnce dials, selects INBOX, and IDLEs in refresh cycles until ctx is done or
// an error occurs.
func (b *Backend) idleOnce(ctx context.Context, onChange func()) error {
	handler := &imapclient.UnilateralDataHandler{
		Mailbox: func(*imapclient.UnilateralDataMailbox) { // EXISTS changed (new mail)
			logging.Trace("imapbackend: idle nudge (exists changed)", "account", b.cfg.Email, "mailbox", "INBOX")
			onChange()
		},
		Expunge: func(uint32) { // a message was removed
			logging.Trace("imapbackend: idle nudge (expunge)", "account", b.cfg.Email, "mailbox", "INBOX")
			onChange()
		},
	}
	cl, err := b.dial(handler)
	if err != nil {
		return err
	}
	defer func() { _ = cl.Close() }()
	if !cl.Caps().Has(imap.CapIdle) {
		logging.Trace("imapbackend: idle unsupported", "account", b.cfg.Email)
		return errNoIdle
	}
	if _, err := cl.Select("INBOX", nil).Wait(); err != nil {
		logging.Trace("imapbackend: idle select INBOX failed", "account", b.cfg.Email, "err", err)
		return err
	}
	for {
		logging.Trace("imapbackend: idle start", "account", b.cfg.Email, "mailbox", "INBOX", "refresh", idleRefresh)
		idle, err := cl.Idle()
		if err != nil {
			logging.Trace("imapbackend: idle command failed", "account", b.cfg.Email, "err", err)
			return err
		}
		select {
		case <-ctx.Done():
			_ = idle.Close()
			logging.TraceContext(ctx, "imapbackend: idle stop (context done)", "account", b.cfg.Email)
			return ctx.Err()
		case <-time.After(idleRefresh):
		}
		logging.Trace("imapbackend: idle refresh", "account", b.cfg.Email)
		if err := idle.Close(); err != nil {
			return err
		}
		if err := idle.Wait(); err != nil {
			return err
		}
	}
}
