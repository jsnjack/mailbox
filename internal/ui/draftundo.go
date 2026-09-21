package ui

import (
	"context"

	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/model"
)

func (w *window) offerDraftUndo(account int64, msg model.OutgoingMessage) {
	msg.LocalDraftID = ""
	msg.DraftID = ""
	msg.SourceMessageID = ""
	toast := newToast("Draft discarded")
	toast.SetButtonLabel("Undo")
	restored := false
	toast.ConnectButtonClicked(func() {
		if restored {
			return
		}
		restored = true
		go func() {
			id, err := w.deps.SaveDraft(context.Background(), account, msg)
			dispatch.Main(func() {
				if err != nil {
					w.toast("Could not restore draft: " + err.Error())
				} else {
					msg.LocalDraftID = id
				}
				// Keep the snapshot editable even if restoring its durable copy failed.
				w.openComposeOpts(msg, "", "Restored draft", composeOpts{fromAccountID: account, startDirty: err != nil})
			})
		}()
	})
	w.toastOverlay.AddToast(toast)
}
