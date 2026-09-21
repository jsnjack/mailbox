package ui

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestMoveComposeDraft(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		oldID                  string
		saveFails, deleteFails bool
		wantCalls              []string
		wantID                 string
	}{
		{name: "saved draft", oldID: "old", wantCalls: []string{"save", "delete"}, wantID: "new"},
		{name: "unsaved message", wantCalls: []string{"save"}, wantID: "new"},
		{name: "save failure preserves source", oldID: "old", saveFails: true, wantCalls: []string{"save"}},
		{name: "cleanup failure retains destination", oldID: "old", deleteFails: true, wantCalls: []string{"save", "delete"}, wantID: "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := model.OutgoingMessage{From: "other@example.com", To: "recipient@example.com", Subject: "Subject", Body: "Unsent writing", HTMLBody: "<p>Unsent writing</p>", ThreadID: "source-thread", DraftID: "provider-draft", SourceMessageID: "source-message", LocalDraftID: tc.oldID, InReplyTo: "<original@example.com>", References: "<root@example.com>"}
			var calls []string
			save := func(_ context.Context, accountID int64, msg model.OutgoingMessage) (string, error) {
				calls = append(calls, "save")
				if accountID != 2 {
					t.Errorf("destination = %d", accountID)
				}
				want := original
				want.ThreadID = ""
				want.DraftID = ""
				want.SourceMessageID = ""
				want.LocalDraftID = ""
				if !reflect.DeepEqual(msg, want) {
					t.Errorf("saved payload = %#v; want %#v", msg, want)
				}
				if tc.saveFails {
					return "", errors.New("disk full")
				}
				return "new", nil
			}
			remove := func(_ context.Context, accountID int64, id string) error {
				calls = append(calls, "delete")
				if accountID != 1 || id != tc.oldID {
					t.Errorf("deleted %d/%s", accountID, id)
				}
				if tc.deleteFails {
					return errors.New("cleanup failed")
				}
				return nil
			}
			got, err := moveComposeDraft(context.Background(), save, remove, 1, 2, tc.oldID, original)
			if (err != nil) != (tc.saveFails || tc.deleteFails) {
				t.Errorf("error = %v", err)
			}
			if got.LocalDraftID != tc.wantID {
				t.Errorf("draft ID = %q; want %q", got.LocalDraftID, tc.wantID)
			}
			if !reflect.DeepEqual(calls, tc.wantCalls) {
				t.Errorf("calls = %v; want %v", calls, tc.wantCalls)
			}
		})
	}
}
