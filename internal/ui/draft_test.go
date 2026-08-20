package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

// A Gmail-autosaved draft reply syncs into the real conversation as an ordinary
// message; without a mark it reads as delivered mail. The section must say
// "Draft" and offer the way back into compose.
func TestConversationSectionMarksDraft(t *testing.T) {
	w := &window{sanitizer: emailPolicy()}
	draft := model.Message{
		GmailID:      "d1",
		FromAddr:     "me@example.com",
		ToAddrs:      "you@example.com",
		Subject:      "re: ping",
		InternalDate: time.Unix(1000, 0),
		IsDraft:      true,
	}
	head, _, _ := w.conversationSection(draft, model.MessageBody{Text: "half-written reply"}, w.cleanHTML, false, "")
	if !strings.Contains(head, `<span class="mbdraft">Draft</span>`) {
		t.Errorf("draft section header lacks the Draft chip: %q", head)
	}
	if !strings.Contains(head, `mbaction:editdraft/d1`) {
		t.Errorf("draft section header lacks the Edit draft affordance: %q", head)
	}

	sent := draft
	sent.IsDraft = false
	head, _, _ = w.conversationSection(sent, model.MessageBody{Text: "delivered"}, w.cleanHTML, false, "")
	if strings.Contains(head, "mbdraft") || strings.Contains(head, "editdraft") {
		t.Errorf("delivered message header carries draft markings: %q", head)
	}
}

// The header bar's reply/forward/star target the newest delivered message —
// answering a thread must not quote your own unsent draft.
func TestNewestActionableSkipsDrafts(t *testing.T) {
	msgs := []model.Message{
		{GmailID: "m1"},
		{GmailID: "m2"},
		{GmailID: "d1", IsDraft: true},
	}
	if got := newestActionable(msgs); got.GmailID != "m2" {
		t.Errorf("newestActionable = %s, want m2 (newest non-draft)", got.GmailID)
	}
	onlyDrafts := []model.Message{{GmailID: "d1", IsDraft: true}, {GmailID: "d2", IsDraft: true}}
	if got := newestActionable(onlyDrafts); got.GmailID != "d2" {
		t.Errorf("newestActionable all-drafts fallback = %s, want d2", got.GmailID)
	}
}
