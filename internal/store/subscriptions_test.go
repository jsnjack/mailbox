package store

import (
	"context"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

// Subscriptions must group by sender, count ALL of the sender's cached mail,
// take the unsubscribe header from the newest carrying message, and order by
// volume.
func TestSubscriptions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	seed := func(id, from, name, unsub string, oneClick bool, date int64) {
		t.Helper()
		if _, err := s.UpsertMessage(ctx, model.Message{
			AccountID: acc, GmailID: id, ThreadID: id,
			FromAddr: from, FromName: name, InternalDate: time.Unix(date, 0),
			ListUnsubscribe: unsub, ListUnsubOneClick: oneClick,
		}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	// news@ has 3 messages: an old one with a stale token, a newer one with the
	// current token (one-click), and one without the header at all.
	seed("n1", "news@example.com", "The News", "<https://ex.com/old>", false, 100)
	seed("n2", "news@example.com", "The News", "<https://ex.com/new>", true, 300)
	seed("n3", "news@example.com", "The News", "", false, 200)
	// One personal sender: never listed.
	seed("p1", "friend@example.com", "Friend", "", false, 400)
	// A smaller list.
	seed("s1", "shop@example.com", "", "<mailto:leave@shop.example.com>", false, 150)

	subs, err := s.Subscriptions(ctx, acc)
	if err != nil {
		t.Fatalf("Subscriptions: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("got %d subscriptions, want 2: %+v", len(subs), subs)
	}
	news := subs[0]
	if news.FromAddr != "news@example.com" || news.Count != 3 {
		t.Fatalf("first = %+v, want news@example.com with count 3", news)
	}
	if news.ListUnsubscribe != "<https://ex.com/new>" || !news.OneClick {
		t.Fatalf("news header = %q oneclick=%v, want the newest header", news.ListUnsubscribe, news.OneClick)
	}
	if news.FromName != "The News" || news.LastSeen != 300 {
		t.Fatalf("news name/last = %q/%d", news.FromName, news.LastSeen)
	}
	shop := subs[1]
	if shop.FromAddr != "shop@example.com" || shop.Count != 1 || shop.FromName != "shop@example.com" {
		t.Fatalf("second = %+v", shop)
	}
}
