package ui

import (
	"context"
	"fmt"
	"strings"

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

// switchComposeSignature replaces only the exact sign-off still at the end of
// the user's writing; a manually edited sign-off is left alone.
func switchComposeSignature(body, previous, next string) string {
	previous = strings.TrimSpace(previous)
	next = strings.TrimSpace(next)
	if previous == next {
		return body
	}
	boundary := quoteBoundary(body)
	own, quote := body[:boundary], body[boundary:]
	trimmed := strings.TrimRight(own, " \t\r\n")
	if previous != "" {
		if !strings.HasSuffix(trimmed, previous) {
			return body
		}
		at := len(trimmed) - len(previous)
		if at > 0 && trimmed[at-1] != '\n' {
			return body
		}
		own = strings.TrimRight(trimmed[:at], " \t\r\n")
	} else {
		own = trimmed
	}
	if next != "" {
		own += "\n\n" + next
	}
	if quote != "" {
		own += "\n\n" + quote
	}
	return own
}
