package activity

import (
	"strings"
	"testing"
)

func TestHubPublishSubscribe(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	defer cancel()

	done := h.Begin("ai", "a@example.com", "translate")
	if got := <-ch; got.Op != "ai" || got.Phase != Start || got.Account != "a@example.com" || got.Label != "translate" {
		t.Fatalf("start event wrong: %+v", got)
	}
	done("240 tok")
	if got := <-ch; got.Phase != Done || got.Note != "240 tok" {
		t.Fatalf("done event wrong: %+v", got)
	}
}

func TestHubDropsWhenFull(t *testing.T) {
	h := NewHub()
	_, cancel := h.Subscribe() // never drained
	defer cancel()
	// Far more than the buffer; Publish must not block.
	for i := 0; i < 1000; i++ {
		h.Publish(Event{Op: "sync", Phase: Progress, Done: i, Total: 1000})
	}
}

func TestPastTense(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Checking mail", "Mail checked"},
		{"Checking grammar", "Grammar checked"},
		{"Categorizing 12 conversations", "Categorized 12 conversations"},
		{"Marking Inbox read", "Marked Inbox read"},
		{"Marking as spam", "Marked as spam"},
		{"Moving to Trash", "Moved to Trash"},
		{"Filing to Receipts", "Filed to Receipts"},
		{"Deleting 3 messages forever", "Deleted 3 messages forever"},
		{"Archiving", "Archived"},
		{"Sending queued mail", "Queued mail sent"},
		{"Sending message", "Message sent"},
		// Already past tense (every Report publishes one) — left alone.
		{"Snoozed until Jul 20 09:00", "Snoozed until Jul 20 09:00"},
		{"Returned from snooze", "Returned from snooze"},
		// Unknown wording keeps itself rather than becoming a generic "Done".
		{"Reticulating splines", "Reticulating splines"},
		{"", ""},
	}
	for _, c := range cases {
		if got := PastTense(c.in); got != c.want {
			t.Errorf("PastTense(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// "Marking as spam" must not be swallowed by the "Marking " prefix entry, which
// would render it "Marked as spam" only by luck of ordering — assert the table
// stays ordered longest-first for every overlapping pair.
func TestPastTenseOverlappingPrefixes(t *testing.T) {
	for i, e := range pastLabels {
		for j := 0; j < i; j++ {
			if strings.HasPrefix(e.gerund, pastLabels[j].gerund) {
				t.Errorf("%q is shadowed by the earlier entry %q", e.gerund, pastLabels[j].gerund)
			}
		}
	}
}

// Every gerund label the app publishes must have a past-tense pairing, or a
// finished operation reads as if it were still running.
func TestEveryPublishedLabelHasAPast(t *testing.T) {
	labels := []string{
		"Adding the account", "Categorizing 3 conversations", "Checking for phishing",
		"Checking grammar", "Checking mail", "Clearing cached mail files",
		"Compacting the database", "Deleting 2 messages forever", "Discarding draft",
		"Discarding queued message", "Downloading attachment", "Drafting message",
		"Emptying Trash", "Fetching message", "Marking Inbox read",
		"Re-fetching HTML bodies", "Refining text", "Removing the account",
		"Retrying queued message", "Saving draft locally", "Searching all mail",
		"Sending message", "Sending queued mail", "Suggesting a subject",
		"Suggesting replies", "Suggesting snooze times", "Summarizing thread",
		"Summarizing message", "Summarizing 3 messages", "Translating conversation",
		"Unsubscribing from Bol", "Testing granite-4.1", "Testing the AI connection",
		"Archiving", "Moving to Trash", "Marking as spam", "Starring", "Unstarring",
		"Filing to Receipts", "Changing labels", "Marking unread", "Moving to Inbox",
	}
	for _, l := range labels {
		if got := PastTense(l); got == l {
			t.Errorf("no past-tense pairing for %q", l)
		}
	}
}

func TestNilHubIsNoop(t *testing.T) {
	var h *Hub
	h.Publish(Event{Op: "x"}) // must not panic
	done := h.Begin("x", "", "y")
	done("z")
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	cancel()
	h.Publish(Event{Op: "sync"})
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after cancel")
	}
}
