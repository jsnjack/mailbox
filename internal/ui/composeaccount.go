package ui

import (
	"context"
	"fmt"

	"github.com/jsnjack/mailbox/internal/model"
)

// moveComposeDraft saves the destination before deleting the source. A nonempty
// returned draft ID remains authoritative even if source cleanup fails.
func moveComposeDraft(ctx context.Context, save DraftSaver, remove DraftDeleter, source, destination int64, oldID string, msg model.OutgoingMessage) (model.OutgoingMessage, error) {
	msg.LocalDraftID = ""
	msg.DraftID = ""
	msg.SourceMessageID = ""
	msg.ThreadID = "" // Provider thread IDs belong to the source account.
	if oldID != "" && (save == nil || remove == nil) {
		return msg, fmt.Errorf("could not change sender: draft storage is unavailable")
	}
	if save == nil {
		return msg, nil
	}
	id, err := save(ctx, destination, msg)
	if err != nil {
		return msg, fmt.Errorf("could not save draft under the selected sender: %w", err)
	}
	msg.LocalDraftID = id
	if oldID != "" {
		if err := remove(ctx, source, oldID); err != nil {
			return msg, fmt.Errorf("sender changed, but could not remove the previous account's draft: %w", err)
		}
	}
	return msg, nil
}
