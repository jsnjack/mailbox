package imapbackend

import (
	"context"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
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
	for ctx.Err() == nil {
		if err := b.idleOnce(ctx, onChange); err != nil && ctx.Err() == nil {
			if err == errNoIdle {
				return // server can't IDLE — leave it to the poll loop
			}
			select {
			case <-ctx.Done():
			case <-time.After(idleRetry):
			}
		}
	}
}

// errNoIdle signals the server doesn't advertise the IDLE capability.
var errNoIdle = errorString("imap: server does not support IDLE")

type errorString string

func (e errorString) Error() string { return string(e) }

// idleOnce dials, selects INBOX, and IDLEs in refresh cycles until ctx is done or
// an error occurs.
func (b *Backend) idleOnce(ctx context.Context, onChange func()) error {
	handler := &imapclient.UnilateralDataHandler{
		Mailbox: func(*imapclient.UnilateralDataMailbox) { onChange() }, // EXISTS changed (new mail)
		Expunge: func(uint32) { onChange() },                            // a message was removed
	}
	cl, err := b.dial(handler)
	if err != nil {
		return err
	}
	defer func() { _ = cl.Close() }()
	if !cl.Caps().Has(imap.CapIdle) {
		return errNoIdle
	}
	if _, err := cl.Select("INBOX", nil).Wait(); err != nil {
		return err
	}
	for {
		idle, err := cl.Idle()
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			_ = idle.Close()
			return ctx.Err()
		case <-time.After(idleRefresh):
		}
		if err := idle.Close(); err != nil {
			return err
		}
		if err := idle.Wait(); err != nil {
			return err
		}
	}
}
