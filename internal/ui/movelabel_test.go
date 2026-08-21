package ui

import (
	"slices"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

// Every Move-to surface relocates through moveRemovals: the location set minus
// the target, so "Move to Inbox" never adds and removes INBOX in one call.
func TestMoveRemovals(t *testing.T) {
	if got := moveRemovals(model.LabelInbox); !slices.Equal(got, []string{model.LabelTrash, model.LabelSpam}) {
		t.Errorf("moveRemovals(INBOX) = %v, want [TRASH SPAM]", got)
	}
	if got := moveRemovals("Label_42"); !slices.Equal(got, moveLocationRemovals) {
		t.Errorf("moveRemovals(user label) = %v, want the full location set %v", got, moveLocationRemovals)
	}
}
