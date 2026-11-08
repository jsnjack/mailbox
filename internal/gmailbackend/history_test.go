package gmailbackend

import (
	"slices"
	"testing"

	"github.com/jsnjack/mailbox/internal/backend"
	gmail "google.golang.org/api/gmail/v1"
)

func msg(id string, labels ...string) *gmail.Message {
	return &gmail.Message{Id: id, LabelIds: labels}
}

// History is folded into one change per message, most recently changed first,
// with the labels of the message's last record; a deleted message is a delete.
func TestHistoryChanges(t *testing.T) {
	records := []*gmail.History{
		{MessagesAdded: []*gmail.HistoryMessageAdded{{Message: msg("a", "INBOX", "UNREAD")}}},
		{MessagesAdded: []*gmail.HistoryMessageAdded{{Message: msg("b", "INBOX")}}},
		{LabelsRemoved: []*gmail.HistoryLabelRemoved{{Message: msg("a", "CATEGORY_UPDATES"), LabelIds: []string{"INBOX", "UNREAD"}}}},
		{MessagesAdded: []*gmail.HistoryMessageAdded{{Message: msg("c")}}},
		{MessagesAdded: []*gmail.HistoryMessageAdded{{Message: msg("d", "INBOX")}}},
		{MessagesDeleted: []*gmail.HistoryMessageDeleted{{Message: msg("d")}}},
	}
	changes, deletes := historyChanges(records)
	want := []backend.MessageChange{
		{ID: "c"},
		{ID: "a", Labels: []string{"CATEGORY_UPDATES"}},
		{ID: "b", Labels: []string{"INBOX"}, Inbox: true},
	}
	if len(changes) != len(want) {
		t.Fatalf("changes = %+v, want %+v", changes, want)
	}
	for i := range want {
		if changes[i].ID != want[i].ID || !slices.Equal(changes[i].Labels, want[i].Labels) || changes[i].Inbox != want[i].Inbox {
			t.Fatalf("changes[%d] = %+v, want %+v", i, changes[i], want[i])
		}
	}
	if !slices.Equal(deletes, []string{"d"}) {
		t.Fatalf("deletes = %v, want [d]", deletes)
	}
}
