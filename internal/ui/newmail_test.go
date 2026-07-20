package ui

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

// TestCheckNewMailSelfSentUnread covers the self-sent branch of checkNewMail:
// the user's own outgoing message that came back UNREAD (a recipient alias
// forwarding into this mailbox merges as +UNREAD on the sent copy) gets its
// unread state cleared — but not when the user explicitly marked it unread,
// and not for mail that merely spoofs the user's From address (no SENT label).
func TestCheckNewMailSelfSentUnread(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	acc, err := st.UpsertAccount(ctx, model.Account{Email: "me@example.com"})
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}

	now := time.Now()
	seed := func(gmailID, from string, labels []string) {
		t.Helper()
		if _, err := st.UpsertMessage(ctx, model.Message{
			AccountID:    acc,
			GmailID:      gmailID,
			ThreadID:     gmailID,
			InternalDate: now,
			FromAddr:     from,
			IsUnread:     true,
			Labels:       labels,
		}); err != nil {
			t.Fatalf("UpsertMessage %s: %v", gmailID, err)
		}
	}
	seed("self-loop", "me@example.com", []string{model.LabelSent, model.LabelUnread})
	seed("self-marked", "me@example.com", []string{model.LabelSent, model.LabelUnread})
	seed("spoof", "me@example.com", []string{model.LabelInbox, model.LabelUnread})
	// Someone else's unread mail without INBOX: neither cleared nor notified.
	seed("other", "alice@example.com", []string{model.LabelUnread})

	type call struct {
		accountID   int64
		ids         []string
		add, remove []string
	}
	var calls []call
	w := &window{
		deps: Deps{
			Store:    st,
			Accounts: []AccountInfo{{ID: acc, Email: "me@example.com"}},
			ModifyLabels: func(ctx context.Context, accountID int64, gmailIDs []string, add, remove []string) error {
				calls = append(calls, call{accountID: accountID, ids: gmailIDs, add: add, remove: remove})
				return nil
			},
		},
		startTime:  now.Add(-time.Hour),
		userUnread: map[string]bool{},
	}

	w.checkNewMail([]notifyCandidate{
		{accountID: acc, gmailID: "self-loop"},
		{accountID: acc, gmailID: "self-marked", userMarked: true},
		{accountID: acc, gmailID: "spoof"},
		{accountID: acc, gmailID: "other"},
	})

	if len(calls) != 1 {
		t.Fatalf("ModifyLabels calls = %d, want 1 (%+v)", len(calls), calls)
	}
	c := calls[0]
	if c.accountID != acc || len(c.ids) != 1 || c.ids[0] != "self-loop" {
		t.Fatalf("cleared %+v, want just self-loop on account %d", c, acc)
	}
	if len(c.add) != 0 || len(c.remove) != 1 || c.remove[0] != model.LabelUnread {
		t.Fatalf("label delta = add %v remove %v, want remove UNREAD only", c.add, c.remove)
	}
}
