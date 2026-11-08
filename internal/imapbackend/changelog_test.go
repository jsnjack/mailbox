package imapbackend

import (
	"testing"
)

func TestInboxFirst(t *testing.T) {
	ids := []string{
		msgID("Archive", 1, 50),
		msgID("INBOX", 1, 7),
		msgID("Lists", 1, 9),
		msgID("INBOX", 1, 12),
	}
	got := inboxFirst(ids)
	want := []struct {
		id    string
		inbox bool
	}{
		{msgID("INBOX", 1, 12), true},
		{msgID("INBOX", 1, 7), true},
		{msgID("Archive", 1, 50), false},
		{msgID("Lists", 1, 9), false},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		if got[i].ID != w.id || got[i].Inbox != w.inbox || got[i].Labels != nil {
			t.Fatalf("got[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}
