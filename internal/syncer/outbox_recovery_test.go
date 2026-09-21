package syncer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

func TestRecoverAndRestoreOutbox(t *testing.T) {
	for _, tc := range []struct {
		name   string
		broken bool
	}{{name: "editable snapshot"}, {name: "invalid MIME leaves send queued", broken: true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := st.Close(); err != nil {
					t.Error(err)
				}
			}()
			acct, err := st.UpsertAccount(ctx, model.Account{Email: "sender@example.com", Type: model.AccountGmail})
			if err != nil {
				t.Fatal(err)
			}
			engine := &Engine{Store: st}
			original := model.OutgoingMessage{From: "sender@example.com", To: "recipient@example.com", Body: "New reply\n\n> Original", QuoteHTML: "<p>Original</p>", SkipSignature: true}
			localID, err := engine.SaveDraftLocal(ctx, acct, original)
			if err != nil {
				t.Fatal(err)
			}
			original.LocalDraftID = localID
			if tc.broken {
				_, err = st.EnqueueOutbox(ctx, acct, "", "", []byte("invalid MIME"), 0)
			} else {
				_, err = engine.EnqueueSend(ctx, acct, original, 0)
			}
			if err != nil {
				t.Fatal(err)
			}
			items, err := st.ListPendingOutbox(ctx, acct, time.Now().Unix())
			if err != nil {
				t.Fatal(err)
			}
			msg, ok, err := engine.RecoverOutboxDraft(ctx, items[0])
			if tc.broken {
				if err == nil || ok {
					t.Fatal("invalid MIME recovered")
				}
				remaining, err := st.ListPendingOutbox(ctx, acct, time.Now().Unix())
				if err != nil || len(remaining) != 1 {
					t.Fatalf("original lost: %v %v", remaining, err)
				}
				return
			}
			if err != nil || !ok {
				t.Fatalf("recovery failed: %v %v", ok, err)
			}
			if msg.QuoteHTML != original.QuoteHTML || !msg.SkipSignature || strings.ReplaceAll(msg.Body, "\r\n", "\n") != original.Body {
				t.Fatalf("compose metadata changed: %#v", msg)
			}
			be := &countingBackend{}
			if n, err := engine.SweepOutbox(ctx, be, acct); err != nil || n != 0 || be.sends.Load() != 0 {
				t.Fatalf("recovered message still sent: %d %v", n, err)
			}
			if err := engine.RestoreOutbox(ctx, items[0]); err != nil {
				t.Fatal(err)
			}
			remaining, err := st.ListPendingOutbox(ctx, acct, time.Now().Unix())
			if err != nil || len(remaining) != 1 {
				t.Fatalf("restore failed: %v %v", remaining, err)
			}
		})
	}
}
