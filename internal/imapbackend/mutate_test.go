package imapbackend

import (
	"testing"

	"github.com/emersion/go-imap/v2"
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
