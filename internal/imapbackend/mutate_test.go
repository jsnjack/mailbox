package imapbackend

import (
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/jsnjack/mailbox/internal/model"
)

// Ids for the same mailbox under different UIDVALIDITY epochs must land in
// separate groups — merging them under one epoch would let a stale UID be
// flag-stored/moved/expunged against whatever message holds that number now.
func TestGroupByFolderSeparatesEpochs(t *testing.T) {
	ids := []string{
		msgID("INBOX", 1, 10),
		msgID("INBOX", 1, 11),
		msgID("INBOX", 2, 10), // same mailbox, new epoch
		msgID("Work", 7, 3),
		"not-an-imap-id", // skipped, not grouped anywhere
	}
	groups := groupByFolder(ids)
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3: %v", len(groups), groups)
	}
	if got := groups[folderKey{"INBOX", 1}]; len(got) != 2 {
		t.Errorf("INBOX epoch 1: %v, want 2 uids", got)
	}
	if got := groups[folderKey{"INBOX", 2}]; len(got) != 1 || got[0] != imap.UID(10) {
		t.Errorf("INBOX epoch 2: %v, want [10]", got)
	}
	if got := groups[folderKey{"Work", 7}]; len(got) != 1 || got[0] != imap.UID(3) {
		t.Errorf("Work epoch 7: %v, want [3]", got)
	}
}

func TestMoveDest(t *testing.T) {
	b := &Backend{
		labelToFolder: map[string]string{
			model.LabelInbox: "INBOX",
			model.LabelTrash: "Trash",
			"Projects":       "Projects",
		},
		archiveFolder: "Archive",
	}
	tests := []struct {
		name        string
		add, remove []string
		want        string
	}{
		{name: "user folder beats archive", add: []string{"Projects"}, remove: []string{model.LabelInbox}, want: "Projects"},
		{name: "user folder from trash", add: []string{"Projects"}, remove: []string{model.LabelTrash}, want: "Projects"},
		{name: "archive", remove: []string{model.LabelInbox}, want: "Archive"},
		{name: "trash", add: []string{model.LabelTrash}, remove: []string{model.LabelInbox}, want: "Trash"},
		{name: "flags only", add: []string{model.LabelStarred}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := b.moveDest(tt.add, tt.remove); got != tt.want {
				t.Fatalf("moveDest(%v, %v) = %q, want %q", tt.add, tt.remove, got, tt.want)
			}
		})
	}
}

func TestNextSearchPageUsesOpaqueSession(t *testing.T) {
	b := &Backend{searchSessions: map[string]imapSearchSession{
		"token": {query: "invoice", ids: []string{"a", "b", "c"}, offset: 1},
	}}
	page, err := b.nextSearchPage("invoice", "token", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.IDs) != 1 || page.IDs[0] != "b" || page.Next != "token" {
		t.Fatalf("first continuation = %+v", page)
	}
	page, err = b.nextSearchPage("invoice", "token", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.IDs) != 1 || page.IDs[0] != "c" || page.Next != "" {
		t.Fatalf("last continuation = %+v", page)
	}
	if _, err := b.nextSearchPage("invoice", "token", 1); err == nil {
		t.Fatal("exhausted token remained valid")
	}
}
